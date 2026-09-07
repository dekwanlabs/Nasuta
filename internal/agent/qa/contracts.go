package qa

import (
	"context"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/agent/tools"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

// Deps bundles the services needed by the QA scenario.
type Deps struct {
	Tools       *tools.Service
	Cfg         config.Config
	Platform    *config.PlatformSettings
	CodeGraphDB *codegraph.DB
	History     session.History
	Sessions    *memory.SessionStore
	Memory      *memory.MemoryStore
	Definitions definition.Resolver
	Agent       agentapi.DefinitionRef
	// Runtime accepts any composition that provides both the managed-run
	// lifecycle (RunStarter) and preparation tool snapshot (ScenarioToolSource).
	// Narrower callers may pass a type that only implements one side when they
	// do not need the other.
	Runtime        RuntimePort
	Events         EventSink
	Models         *Models
	WriteAvailable bool
}

type SelectionResolver interface {
	ResolveFor(agentapi.DefinitionRef, string) (
		agentapi.Definition,
		agentapi.DefinitionSelection,
		error,
	)
}

type contextRetriever interface {
	RetrievePlan(context.Context, string, retrieval.QueryTerms, domain.EvidencePlan, domain.QueryPlan) (*retrieval.RetrievedContext, error)
	ContextBudget() int
}

// AskResult identifies the asynchronous run and its pre-retrieved context.
type AskResult struct {
	RunID   string
	Context *retrieval.RetrievedContext
}

// ContextBlock is trusted evidence prepared by an upper-layer scenario.
type ContextBlock struct {
	Source     string
	Title      string
	Content    string
	References []retrieval.Reference
	Evidence   []tool.EvidenceUnit
}

type PlannedToolCall struct {
	ToolID    tool.ToolID
	Arguments tool.Arguments
	Required  bool
}

type ToolPlan struct {
	Prefetch []PlannedToolCall
}

// Request is the stable use-case input for standard and scenario handlers.
type Request struct {
	Question         string
	Conversation     execution.ConversationContext
	PreloadedContext []ContextBlock
	UserID           int64
	RolePrompt       string
	RunID            string
	EvidencePlan     *domain.EvidencePlan
	ToolPlan         ToolPlan
	WriteAuthorized  bool
	WriteRequested   bool
	Agent            agentapi.DefinitionRef
	ParentRunID      string
	WorkflowRunID    string
	WorkflowNodeID   string
}
