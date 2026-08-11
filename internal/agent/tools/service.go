package tools

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/dekwanlabs/nasuta/internal/agent/web"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
)

// Deps bundles the stores and services used by tool handlers.
type Deps struct {
	DB            *store.SQLite
	Semantic      semantic.Store
	Embedder      embed.Embedder
	WorkspaceRoot string
	DocStore      docStore
	CallChain     *callchain.Service
	Ontology      *ontology.Service
}

// Service exposes the retrieval and analysis tools used by the agent.
type Service struct {
	db             *store.SQLite
	semantic       semantic.Store
	embedder       embed.Embedder
	workspaceRoot  string
	docStore       docStore
	callChain      *callchain.Service
	ontology       *ontology.Service
	bm25           atomic.Pointer[retrieval.BM25Builder]
	mergedSvcCache atomic.Pointer[[]domain.ServiceRecord]
	denseWarnOnce  sync.Once
	webOnce        sync.Once
	web            *web.Service
}

func New(deps Deps) *Service {
	return &Service{
		db:            deps.DB,
		semantic:      deps.Semantic,
		embedder:      deps.Embedder,
		workspaceRoot: deps.WorkspaceRoot,
		docStore:      deps.DocStore,
		callChain:     deps.CallChain,
		ontology:      deps.Ontology,
		web:           web.New(),
	}
}

func (srv *Service) SetBM25(builder *retrieval.BM25Builder) {
	srv.bm25.Store(builder)
}

func (srv *Service) BM25View() *retrieval.BM25Builder {
	return srv.bm25.Load()
}

func (srv *Service) InvalidateServices() {
	srv.mergedSvcCache.Store(nil)
}

func (srv *Service) semanticEnabled() bool {
	return srv.semantic != nil &&
		srv.semantic.Capabilities().Dense &&
		srv.embedder != nil &&
		srv.embedder.Enabled()
}

func payloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func payloadInt(value any) int {
	return trustTierFromPayload(value)
}

func trustTierFromPayload(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	case float32:
		return int(number)
	case json.Number:
		integer, _ := number.Int64()
		return int(integer)
	default:
		return 0
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
