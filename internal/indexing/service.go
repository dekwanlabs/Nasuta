package indexing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// ToolsSink lets indexing refresh agent-side search helpers after rebuilds.
type ToolsSink interface {
	SetBM25(*retrieval.BM25Builder)
	InvalidateServices()
}

// Service owns indexing dependencies and coordinates the specialized pipelines
// implemented by the other files in this package.
type Service struct {
	Cfg       config.Config
	Platform  *config.PlatformSettings
	DB        *store.SQLite
	Semantic  semantic.Store
	Embedder  embed.Embedder
	tools     ToolsSink
	ScanDirs  []string
	publisher ontology.Publisher
	configs   config.Resolver

	docDB       *store.DocStore
	docStoreErr error

	VCS    *indexer.Client
	Syncer *indexer.Syncer

	activeVCSTokenFingerprint string

	bm25 atomic.Pointer[retrieval.BM25Builder]

	indexMu               sync.Mutex
	bm25MigrationRequired atomic.Bool
}

// Build initializes the durable stores and optional semantic backend.
func Build(cfg config.Config, docDB *store.DocStore, docStoreErr error) (*Service, error) {
	db, err := store.Open(cfg.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	semanticBackend, err := semanticstore.New(cfg.Semantic)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("build semantic store: %w", err)
	}
	svc := &Service{
		Cfg: cfg, DB: db, Semantic: semanticBackend, Embedder: embed.New(cfg),
		docDB: docDB, docStoreErr: docStoreErr, Platform: &config.PlatformSettings{},
	}
	// Ensure the configured collection exists up front so read paths (dashboard
	// counts, search) never hit "collection not found" before the first index.
	// The dense dimension comes from config so the collection exists even before
	// an embedding key is set; Ensure is idempotent on a matching collection.
	if err := svc.Semantic.Ensure(context.Background(), semantic.Schema{
		Collection: cfg.Semantic.Collection,
		DenseDim:   cfg.EmbeddingDim,
	}); err != nil {
		log.Warnf("[indexing] ensure semantic collection %q: %v", cfg.Semantic.Collection, err)
	}
	svc.loadBM25()
	svc.ScanDirs, err = svc.LoadScanDirs()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("load scan directories: %w", err)
	}
	return svc, nil
}

func (svc *Service) DocDB() *store.DocStore { return svc.docDB }

// SetTools connects indexing rebuilds to the agent-facing caches.
func (svc *Service) SetTools(tools ToolsSink) {
	svc.tools = tools
	if tools != nil {
		if builder := svc.bm25.Load(); builder != nil {
			tools.SetBM25(builder)
		}
	}
}

// SetOntologyPublisher configures publication of rebuilt structural knowledge.
// A completed index rebuild replaces the shared ontology snapshot atomically.
// Leaving it unset disables publication without disabling indexing.
func (svc *Service) SetOntologyPublisher(publisher ontology.Publisher) {
	svc.publisher = publisher
}

// SetConfigResolver connects application-owned configuration sources.
func (svc *Service) SetConfigResolver(resolver config.Resolver) {
	svc.configs = resolver
}

// SetPlatform swaps in the latest platform-managed settings.
func (svc *Service) SetPlatform(settings *config.PlatformSettings) {
	if settings != nil {
		svc.Platform = settings
	}
}

// Close releases the backends owned by the indexing service.
// Optional backends are closed only when they were configured.
// Individual close failures remain observable through logging.
func (svc *Service) Close() {
	if svc.Semantic != nil {
		if err := svc.Semantic.Close(); err != nil {
			log.Infof("[indexing] semantic close: %v", err)
		}
	}
	if svc.DB != nil {
		if err := svc.DB.Close(); err != nil {
			log.Infof("[indexing] db close: %v", err)
		}
	}
}

func (svc *Service) loadBM25() {
	vocabPath := svc.bm25VocabPath()
	if fileExists(vocabPath) {
		if builder, err := retrieval.LoadVocab(vocabPath); err == nil {
			svc.setBM25(builder)
			log.Infof("[indexing] loaded BM25 vocab from %s (%d tokens)", vocabPath, builder.VocabularySize())
			return
		} else if errors.Is(err, retrieval.ErrLegacyVocabulary) {
			svc.bm25MigrationRequired.Store(true)
			log.Warnf("[indexing] legacy BM25 vocabulary at %s - run the full Embed Code operation once before repository-only embedding", vocabPath)
			return
		} else {
			svc.bm25MigrationRequired.Store(true)
			log.Errorf("[indexing] BM25 vocab at %s failed to load: %v - full code embedding is required before repository-only embedding", vocabPath, err)
			return
		}
	}
	svc.bm25MigrationRequired.Store(true)
	log.Warnf("[indexing] BM25 vocab missing at %s - hybrid search disabled (dense-only). Trigger the \"Embed Code\" platform action to rebuild it before repository-only embedding.", vocabPath)
}

func (svc *Service) setBM25(builder *retrieval.BM25Builder) {
	svc.bm25.Store(builder)
	if svc.tools != nil {
		svc.tools.SetBM25(builder)
	}
}

func (svc *Service) bm25VocabPath() string {
	return filepath.Join(svc.Cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "bm25_vocab.json")
}

func (svc *Service) invalidateToolCaches() {
	if svc.tools != nil {
		svc.tools.InvalidateServices()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
