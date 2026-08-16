package qa

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
)

// Service is the agent-facing runtime facade.
type Service struct {
	// helperLLM handles session maintenance and memory extraction outside Runs.
	helperLLM *llm.LLMClient
	// fastLLM handles cheap structured preparation and falls back to helperLLM.
	fastLLM            *llm.LLMClient
	retriever          contextRetriever
	runtime            agentapi.ManagedRuntime
	runtimeTools       ScenarioToolSource
	phaseEmitter       interface{ EmitPhase(string, string) }
	investigation      InvestigationRunner
	scenarios          ScenarioLifecycle
	coordinator        *Coordinator
	executionEvents    ExecutionEventEmitter
	memory             *memory.MemoryStore
	sessions           *memory.SessionStore
	history            SessionHistory
	writeAvailable     atomic.Bool
	cfg                config.Config
	routerConfidence   float64
	routerMaxTokens    int
	contextWindow      int
	outputReserve      int
	domainKnowledge    string
	toolPruningEnabled bool
	delegationEnabled  bool
	delegationShadow   bool
	workflowEscalation bool
	delegationChildren int
	delegationTokens   int64
	delegationCost     int64
	workflowEscalator  agentapi.WorkflowEscalator
	capabilities       WorkflowCapabilityResolver
	definitions        DefinitionResolver
	agentRef           agentapi.DefinitionRef
	definitionErr      error
	runtimeErr         error
	compactionMu       sync.RWMutex
	compactionStatus   map[string]SessionStatusEvent
}

// New wires retrieval, agent, memory, and write tools together.
func New(d Deps) *Service {
	platformSettings := d.Platform
	ret := retrieval.New(d.Tools, d.Cfg).WithPlatform(platformSettings)
	if d.CodeGraphDB != nil {
		ret.WithCodeGraph(d.CodeGraphDB)
	}
	routerConfidence := platformSettings.RetrievalRouterConfidence
	if routerConfidence == 0 {
		routerConfidence = config.DefaultRetrievalRouterDirectConfidence
	}
	routerMaxTokens := platformSettings.RetrievalRouterMaxTokens
	if routerMaxTokens == 0 {
		routerMaxTokens = config.DefaultRetrievalRouterMaxTokens
	}
	svc := &Service{
		retriever: ret, cfg: d.Cfg,
		routerConfidence: routerConfidence, routerMaxTokens: routerMaxTokens,
		toolPruningEnabled: platformSettings.ToolPruningEnabled,
		delegationEnabled:  platformSettings.DelegationEnabled,
		delegationShadow:   platformSettings.DelegationShadowEnabled,
		workflowEscalation: platformSettings.DelegationWorkflowEscalationEnabled,
		delegationChildren: platformSettings.DelegationMaxChildren,
		delegationTokens:   platformSettings.DelegationMaxTotalTokens,
		delegationCost:     platformSettings.DelegationMaxTotalCostMicros,
		history:            d.History, sessions: d.Sessions, contextWindow: platformSettings.LLMContextWindow,
		outputReserve:   platformSettings.LLMAnswerMaxTokens,
		domainKnowledge: platformSettings.DomainKnowledge,
		definitions:     d.Definitions, agentRef: d.Agent,
		runtime: d.Runtime, runtimeTools: d.RuntimeTools,
		phaseEmitter: d.PhaseEmitter, investigation: d.Investigation,
		scenarios: d.ScenarioLifecycle, coordinator: d.Coordinator,
		executionEvents:   d.ExecutionEvents,
		workflowEscalator: d.WorkflowEscalator,
		capabilities:      d.Capabilities,
		memory:            d.Memory,
		compactionStatus:  make(map[string]SessionStatusEvent),
	}
	svc.writeAvailable.Store(d.WriteAvailable)
	if svc.agentRef.ID == "" {
		svc.agentRef = agentapi.DefinitionRef{ID: "qa.answerer"}
	}
	if svc.definitions == nil {
		schemas := agentapi.NewSchemaRegistry()
		err := schemas.Publish(catalog.DefaultSchemas())
		definitions := catalog.New(schemas)
		if err == nil {
			var definition agentapi.Definition
			definition, err = catalog.DefaultQA(platformSettings)
			if err == nil {
				err = definitions.Publish([]agentapi.Definition{definition})
			}
		}
		svc.definitions = definitions
		svc.definitionErr = err
	}
	useDashScope := platformSettings.RerankProvider == "dashscope" && platformSettings.RerankAPIKey != ""
	log.Infof("[qa] retrieval router: direct_min_confidence=%.2f max_tokens=%d", routerConfidence, routerMaxTokens)
	if useDashScope {
		ret.WithReranker(retrieval.NewDashScopeReranker(platformSettings))
		log.Infof("[qa] reranker: dashscope (%s)", platformSettings.RerankModel)
	}

	if d.Runtime == nil || d.RuntimeTools == nil || d.Models == nil {
		svc.runtimeErr = fmt.Errorf("QA runtime is not configured")
	} else {
		svc.helperLLM = d.Models.Primary()
		svc.fastLLM = d.Models.Fast()
	}

	return svc
}

func (svc *Service) Memory() *memory.MemoryStore { return svc.memory }

// SetWriteAvailable updates write-action availability without replacing the
// service or any of the runtime dependencies it already holds.
func (svc *Service) SetWriteAvailable(available bool) {
	svc.writeAvailable.Store(available)
}

// emitStep pushes a lightweight phase hint to the run hub.
func (svc *Service) emitStep(runID, text string) {
	if svc.phaseEmitter != nil {
		svc.phaseEmitter.EmitPhase(runID, text)
	}
}

func (svc *Service) emitStatus(runID, text, code string, started time.Time) {
	elapsed := int64(0)
	if !started.IsZero() {
		elapsed = time.Since(started).Milliseconds()
	}
	svc.emitStatusElapsed(runID, text, code, elapsed)
}

func (svc *Service) emitStatusElapsed(runID, text, code string, elapsedMS int64) {
	if emitter, ok := svc.phaseEmitter.(interface {
		EmitStatus(string, string, string, int64)
	}); ok {
		emitter.EmitStatus(runID, text, code, elapsedMS)
		return
	}
	svc.emitStep(runID, text)
}

func (svc *Service) emitContextUsage(runID string, event ContextUsageEvent) {
	if emitter, ok := svc.phaseEmitter.(interface {
		EmitContextUsage(string, ContextUsageEvent)
	}); ok {
		emitter.EmitContextUsage(runID, event)
	}
}

func (svc *Service) updateCompaction(runID, sessionID, status, text string, fromTurn, toTurn int) {
	event := SessionStatusEvent{
		Status: status, Text: text, FromTurn: fromTurn, ToTurn: toTurn,
		UpdatedAtMs: time.Now().UnixMilli(),
	}
	svc.compactionMu.Lock()
	svc.compactionStatus[sessionID] = event
	svc.compactionMu.Unlock()
	if emitter, ok := svc.phaseEmitter.(interface {
		EmitSessionStatus(string, SessionStatusEvent)
	}); ok {
		emitter.EmitSessionStatus(runID, event)
	}
}

// CompactionStatus returns the latest transient archive status for one session.
func (svc *Service) CompactionStatus(sessionID string) SessionStatusEvent {
	svc.compactionMu.RLock()
	defer svc.compactionMu.RUnlock()
	return svc.compactionStatus[sessionID]
}

// helperTimeout bounds each pre-retrieval LLM helper. A stuck helper degrades to
// its fallback (clean question / tech terms / original question) instead of
// stalling retrieval until the request deadline. The parent ctx caps it lower.
const helperTimeout = 12 * time.Second

// AskWithHistory starts a run with verbatim recent history and no explicit evidence plan.
func (svc *Service) AskWithHistory(ctx context.Context, question string, history []llm.Message, userID int64, rolePrompt, runID string) (*AskResult, error) {
	return svc.AskWithContext(ctx, question, ConversationContext{Recent: history}, userID, rolePrompt, runID, nil)
}

// AskWithContext preserves bounded session state and recalled history.
func (svc *Service) AskWithContext(ctx context.Context, question string, conversation ConversationContext, userID int64, rolePrompt, runID string, explicitPlan *domain.EvidencePlan) (*AskResult, error) {
	return svc.Ask(ctx, Request{
		Question: question, Conversation: conversation, UserID: userID,
		RolePrompt: rolePrompt, RunID: runID, EvidencePlan: explicitPlan,
	})
}

// Ask starts one QA run with optional trusted scenario context.
func (svc *Service) Ask(ctx context.Context, request Request) (*AskResult, error) {
	if svc.runtimeErr != nil {
		return nil, svc.runtimeErr
	}
	prepared, err := svc.prepare(ctx, request)
	if err != nil {
		return nil, err
	}

	var result *AskResult
	if prepared.execution.Strategy == retrieval.ExecutionMultiAgent {
		result, err = svc.submitInvestigation(prepared)
	} else {
		result, err = svc.prepareSingleRun(prepared)
	}
	if err != nil {
		prepared.closeTrace()
	}
	return result, err
}

func standardRequest(request Request, defaultAgent agentapi.DefinitionRef) bool {
	if request.WorkflowRunID != "" || request.WorkflowNodeID != "" {
		return false
	}
	if request.Agent.ID == "" {
		return true
	}
	return request.Agent.ID == defaultAgent.ID &&
		(request.Agent.Version == 0 || request.Agent.Version == defaultAgent.Version)
}

func (svc *Service) emitEvent(eventType EventType, event ExecutionEvent) {
	if svc.executionEvents != nil {
		svc.executionEvents.EmitEvent(eventType, event)
	}
}
