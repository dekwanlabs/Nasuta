package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/rbac"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/internal/sessionhistory"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

func buildSessionHistory(cfg config.Config, sessions *memory.SessionStore, emb embed.Embedder) *sessionhistory.Service {
	if sessions == nil {
		return nil
	}
	if emb == nil || !emb.Enabled() {
		log.Warnf("[qa] session history dense index disabled; lexical recall remains available")
		return sessionhistory.New(sessions, nil, emb)
	}
	historyConfig := cfg.Semantic
	historyConfig.Collection = "session_history"
	historySemantic, err := semanticstore.New(historyConfig)
	if err != nil {
		log.Errorf("[qa] session history semantic store unavailable; lexical recall remains available: %v", err)
		return sessionhistory.New(sessions, nil, emb)
	}
	if err := historySemantic.Ensure(context.Background(), semantic.Schema{Collection: "session_history", DenseDim: emb.Dim()}); err != nil {
		_ = historySemantic.Close()
		log.Errorf("[qa] session history collection unavailable; lexical recall remains available: %v", err)
		return sessionhistory.New(sessions, nil, emb)
	}
	history := sessionhistory.New(sessions, historySemantic, emb)
	vocabPath := filepath.Join(cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "history_bm25_vocab.json")
	if err := history.EnableBM25(vocabPath); err != nil {
		log.Errorf("[qa] session history BM25 disabled; dense and lexical recall remain available: %v", err)
		return history
	}
	log.Infof("[qa] session history hybrid index enabled (collection=session_history)")
	return history
}

func buildLongTermMemory(cfg config.Config, db *sql.DB, emb embed.Embedder) *memory.MemoryStore {
	if db == nil || emb == nil || !emb.Enabled() {
		return nil
	}
	memoryConfig := cfg.Semantic
	memoryConfig.Collection = "memory"
	memorySemantic, err := semanticstore.New(memoryConfig)
	if err != nil {
		log.Warnf("[qa] memory semantic store init failed: %v", err)
		return nil
	}
	if err := memorySemantic.Ensure(context.Background(), semantic.Schema{
		Collection: "memory",
		DenseDim:   emb.Dim(),
	}); err != nil {
		_ = memorySemantic.Close()
		log.Warnf("[qa] memory collection ensure failed: %v", err)
		return nil
	}
	memoryStore := memory.NewMemoryStore(db, memorySemantic, emb, cfg.MemoryWorkContextTTL)
	vocabPath := filepath.Join(cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "memory_bm25_vocab.json")
	if err := memoryStore.EnableBM25(context.Background(), vocabPath); err != nil {
		log.Warnf("[qa] memory BM25 disabled; dense recall remains available: %v", err)
	} else {
		log.Infof("[qa] memory hybrid index enabled (collection=memory)")
	}
	return memoryStore
}

func openPlatformDB() (*sql.DB, error) {
	dsn := config.LoadMySQLDSN()
	if dsn == "" {
		err := fmt.Errorf("MYSQL_DSN not set")
		log.Warnf("[server] MySQL-backed capabilities disabled (%v)", err)
		return nil, err
	}
	db, err := store.OpenMySQL(dsn)
	if err != nil {
		log.Warnf("[server] MySQL-backed capabilities disabled: %v", err)
		return nil, err
	}
	log.Infof("[server] MySQL platform store enabled")
	return db, nil
}

func buildAuth(cfg config.Config, db *sql.DB) (*auth.DB, *auth.Service) {
	if db == nil {
		log.Warnf("[server] auth disabled (MySQL unavailable)")
		return nil, nil
	}
	authDB := auth.NewDB(db)
	oauth := auth.NewFeishuOAuth(cfg.FeishuAppID, cfg.FeishuAppSecret)
	log.Infof("[server] auth enabled (MySQL: ok, Feishu: %v)", cfg.FeishuConfigured())
	return authDB, auth.NewService(oauth, authDB, cfg.FeishuRedirectURI, cfg.WebBaseURL)
}

func loadPlatformSettings(authDB *auth.DB) *config.PlatformSettings {
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	if authDB == nil {
		return settings
	}
	stored, err := authDB.GetSettings()
	if err != nil {
		log.Warnf("[server] platform settings unavailable: %v", err)
		return settings
	}
	settings.Apply(stored)
	return settings
}

func (p *Platform) initRBAC() {
	if p.db == nil {
		return
	}
	store, err := rbac.NewStore(p.db)
	if err != nil {
		log.Warnf("[server] RBAC store init failed: %v", err)
		p.auth.keyAuth = func(context.Context, string) (bool, error) {
			return false, fmt.Errorf("RBAC store unavailable: %w", err)
		}
		return
	}
	p.auth.rbac = rbac.NewHandler(store)
	p.auth.keyAuth = store.AuthenticateMCPKey
	p.auth.prompt = store.RolePromptFor
	log.Infof("[server] RBAC enabled")
}
