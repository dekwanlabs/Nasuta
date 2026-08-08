package qa

import (
	"context"
	"fmt"
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

// QA is the agent-facing runtime facade.
type QA struct {
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
	executionEvents    ExecutionEventEmitter
	memory             *memory.MemoryStore
	sessions           *memory.SessionStore
	history            SessionHistory
	writeAvailable     bool
	cfg                config.Config
	routerConfidence   float64
	routerMaxTokens    int
	contextWindow      int
	outputReserve      int
	domainKnowledge    string
	toolPruningEnabled bool
	definitions        DefinitionResolver
	agentRef           agentapi.DefinitionRef
	definitionErr      error
	runtimeErr         error
}

// NewQA wires retrieval, agent, memory, and write tools together.
func NewQA(d QADeps) *QA {
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
	svc := &QA{
		retriever: ret, cfg: d.Cfg,
		routerConfidence: routerConfidence, routerMaxTokens: routerMaxTokens,
		toolPruningEnabled: platformSettings.ToolPruningEnabled,
		history:            d.History, sessions: d.Sessions, contextWindow: platformSettings.LLMContextWindow,
		outputReserve: max(
			platformSettings.LLMAnswerMaxTokens,
			platformSettings.LLMConclusionMaxTokens,
		),
		domainKnowledge: platformSettings.DomainKnowledge,
		definitions:     d.Definitions, agentRef: d.Agent,
		runtime: d.Runtime, runtimeTools: d.RuntimeTools,
		phaseEmitter: d.PhaseEmitter, investigation: d.Investigation,
		scenarios: d.ScenarioLifecycle, executionEvents: d.ExecutionEvents,
		writeAvailable: d.WriteAvailable, memory: d.Memory,
	}
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

func (svc *QA) Memory() *memory.MemoryStore { return svc.memory }

// emitStep pushes a lightweight phase hint to the run hub.
func (svc *QA) emitStep(runID, text string) {
	if svc.phaseEmitter != nil {
		svc.phaseEmitter.EmitPhase(runID, text)
	}
}

// helperTimeout bounds each pre-retrieval LLM helper. A stuck helper degrades to
// its fallback (clean question / tech terms / original question) instead of
// stalling retrieval until the request deadline. The parent ctx caps it lower.
const helperTimeout = 12 * time.Second

// AskAgent starts a run with verbatim recent history and no explicit evidence plan.
func (svc *QA) AskAgent(ctx context.Context, question string, history []llm.Message, userID int64, rolePrompt, runID string) (*AskResult, error) {
	return svc.AskAgentWithContext(ctx, question, ConversationContext{Recent: history}, userID, rolePrompt, runID, nil, false)
}

// AskAgentWithContext preserves bounded session state and recalled history.
func (svc *QA) AskAgentWithContext(ctx context.Context, question string, conversation ConversationContext, userID int64, rolePrompt, runID string, explicitPlan *domain.EvidencePlan, allowWrite bool) (*AskResult, error) {
	return svc.Ask(ctx, QARequest{
		Question: question, Conversation: conversation, UserID: userID,
		RolePrompt: rolePrompt, RunID: runID, EvidencePlan: explicitPlan, AllowWrite: allowWrite,
	})
}

// Ask starts one QA run with optional trusted scenario context.
func (svc *QA) Ask(ctx context.Context, request QARequest) (*AskResult, error) {
	if svc.runtimeErr != nil {
		return nil, svc.runtimeErr
	}
	prepared, err := svc.prepareQA(ctx, request)
	if err != nil {
		return nil, err
	}
	if prepared.execution.Strategy == retrieval.ExecutionMultiAgent {
		return svc.submitInvestigation(
			prepared.ctx, prepared.request, prepared.request.Question,
			prepared.request.Conversation, prepared.request.UserID,
			prepared.request.RunID, prepared.trace, prepared.ownsTrace,
		)
	}
	return svc.prepareSingleAgentRun(prepared)
}

func standardQARequest(request QARequest, defaultAgent agentapi.DefinitionRef) bool {
	if len(request.PreloadedContext) > 0 || len(request.Instructions) > 0 ||
		len(request.ToolPlan.Prefetch) > 0 || request.ParentRunID != "" ||
		request.WorkflowRunID != "" || request.WorkflowNodeID != "" {
		return false
	}
	if request.Agent.ID == "" {
		return true
	}
	return request.Agent.ID == defaultAgent.ID &&
		(request.Agent.Version == 0 || request.Agent.Version == defaultAgent.Version)
}

func (svc *QA) emitExecutionEvent(eventType EventType, event ExecutionEvent) {
	if svc.executionEvents != nil {
		svc.executionEvents.EmitExecutionEvent(eventType, event)
	}
}
