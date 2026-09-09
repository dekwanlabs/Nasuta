package qa

import (
	"context"
	"fmt"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
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
	fastLLM                 *llm.LLMClient
	retriever               contextRetriever
	starter                 RunStarter
	scenarioTools           ScenarioToolSource
	events                  EventSink
	memory                  *memory.MemoryStore
	sessions                *memory.SessionStore
	history                 session.History
	writeAvailable          atomic.Bool
	cfg                     config.Config
	routerConfidence        float64
	routerMaxTokens         int
	contextWindow           int
	outputReserve           int
	domainKnowledge         string
	toolPruningEnabled      bool
	delegationEnabled       bool
	delegationMaxConcurrent int
	delegationBudget        agentapi.RunLimits
	answerReserve           time.Duration
	definitions             definition.Resolver
	agentRef                agentapi.DefinitionRef
	definitionErr           error
	runtimeErr              error
	compactionMu            sync.RWMutex
	compactionStatus        map[string]run.SessionStatusEvent
	compactionStatuses      map[string]time.Time
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
		toolPruningEnabled:      platformSettings.ToolPruningEnabled,
		delegationEnabled:       platformSettings.DelegationEnabled,
		delegationMaxConcurrent: platformSettings.DelegationMaxConcurrent,
		delegationBudget: agentapi.RunLimits{
			MaxTotalTokens:      platformSettings.DelegationMaxTotalTokens,
			MaxCostMicros:       platformSettings.DelegationMaxTotalCostMicros,
			ParentAnswerReserve: platformSettings.DelegationParentAnswerReserve,
		},
		answerReserve: time.Duration(platformSettings.AgentAnswerReserve),
		history:       d.History, sessions: d.Sessions, contextWindow: platformSettings.LLMContextWindow,
		outputReserve:   platformSettings.LLMAnswerMaxTokens,
		domainKnowledge: platformSettings.DomainKnowledge,
		definitions:     d.Definitions, agentRef: d.Agent,
		starter:            d.Starter,
		scenarioTools:      d.ScenarioTools,
		events:             d.Events,
		memory:             d.Memory,
		compactionStatus:   make(map[string]run.SessionStatusEvent),
		compactionStatuses: make(map[string]time.Time),
	}
	svc.writeAvailable.Store(d.WriteAvailable)
	if svc.agentRef.ID == "" {
		svc.agentRef = agentapi.DefinitionRef{ID: "qa.answerer"}
	}
	useDashScope := platformSettings.RerankProvider == "dashscope" && platformSettings.RerankAPIKey != ""
	log.Infof("[qa] retrieval router: direct_min_confidence=%.2f max_tokens=%d", routerConfidence, routerMaxTokens)
	if useDashScope {
		ret.WithReranker(retrieval.NewDashScopeReranker(platformSettings))
		log.Infof("[qa] reranker: dashscope (%s)", platformSettings.RerankModel)
	}

	if d.Starter == nil || d.ScenarioTools == nil || d.Models == nil {
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

func (svc *Service) emitStatus(runID, text, code string, started time.Time) {
	elapsed := int64(0)
	if !started.IsZero() {
		elapsed = time.Since(started).Milliseconds()
	}
	svc.emitStatusElapsed(runID, text, code, elapsed)
}

func (svc *Service) emitStatusElapsed(runID, text, code string, elapsedMS int64) {
	if svc.events != nil {
		svc.events.EmitStatus(runID, text, code, elapsedMS)
	}
}

func (svc *Service) emitContextUsage(runID string, event run.ContextUsageEvent) {
	if svc.events != nil {
		svc.events.EmitContextUsage(runID, event)
	}
}

func (svc *Service) updateCompaction(runID, status, text string, fromTurn, toTurn int) {
	now := time.Now()
	event := run.SessionStatusEvent{
		Status: status, Text: text, FromTurn: fromTurn, ToTurn: toTurn,
		UpdatedAtMs: now.UnixMilli(),
	}
	svc.compactionMu.Lock()
	if svc.compactionStatuses == nil {
		svc.compactionStatuses = make(map[string]time.Time)
	}
	if svc.compactionStatus == nil {
		svc.compactionStatus = make(map[string]run.SessionStatusEvent)
	}
	svc.pruneCompactionStatuses(now)
	svc.compactionStatus[runID] = event
	svc.compactionStatuses[runID] = now
	svc.compactionMu.Unlock()
	if svc.events != nil {
		svc.events.EmitSessionStatus(runID, event)
	}
}

// compactionStatusLifetime bounds how long a transient archive status remains
// queryable before it is garbage-collected on the next write or read.
const compactionStatusLifetime = 2 * time.Minute

// pruneCompactionStatuses removes expired transient archive status entries.
// It is called on reads and writes so the map cannot grow without bound.
func (svc *Service) pruneCompactionStatuses(now time.Time) {
	for runID, updated := range svc.compactionStatuses {
		if now.Sub(updated) > compactionStatusLifetime {
			delete(svc.compactionStatuses, runID)
			delete(svc.compactionStatus, runID)
		}
	}
}

// CompactionStatus returns the latest transient archive status for one run.
func (svc *Service) CompactionStatus(runID string) run.SessionStatusEvent {
	now := time.Now()
	svc.compactionMu.Lock()
	svc.pruneCompactionStatuses(now)
	status := svc.compactionStatus[runID]
	svc.compactionMu.Unlock()
	return status
}

// Ask starts one QA run with optional trusted scenario context.
func (svc *Service) Ask(ctx context.Context, request Request) (*AskResult, error) {
	requestStartedAt := time.Now()
	if svc.runtimeErr != nil {
		return nil, svc.runtimeErr
	}
	prepared, err := svc.prepare(ctx, request, requestStartedAt)
	if err != nil {
		return nil, err
	}

	// QA always executes through the normal agent loop. When enabled, the
	// parent agent can use delegate_investigation to fan out read-only work;
	// QA itself no longer creates or waits on a durable investigation workflow.
	result, err := svc.prepareSingleRun(prepared)
	if err != nil {
		prepared.closeTrace()
	}
	return result, err
}

func (svc *Service) emitEvent(eventType run.EventType, event run.ExecutionEvent) {
	if svc.events != nil {
		svc.events.EmitEvent(eventType, event)
	}
}
