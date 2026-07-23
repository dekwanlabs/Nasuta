package agent

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
	_ "github.com/go-sql-driver/mysql"
)

// QADeps bundles the services needed to build the QA runtime.
type QADeps struct {
	Tools          *Service
	Semantic       semantic.Store
	Embedder       embed.Embedder
	WriteAvailable bool
	Cfg            config.Config
	Platform       *config.PlatformSettings
	Registry       *Registry
	CodeGraphDB    *codegraph.DB
	DB             *sql.DB
	RunStore       *RunStore
}

// QA is the agent-facing runtime facade.
type QA struct {
	llm *llm.LLMClient
	// fastLLM handles cheap structured steps and falls back to llm when unset.
	fastLLM          *llm.LLMClient
	retriever        *retrieval.Retriever
	agent            *Agent
	registry         *Registry
	executor         *ToolExecutor
	memory           *memory.MemoryStore
	writeAvailable   bool
	hub              *RunHub
	runStore         *RunStore
	cfg              config.Config
	routerConfidence float64
	routerMaxTokens  int
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
}

type PlannedToolCall struct {
	ToolID    tool.ToolID
	Arguments tool.Arguments
	Required  bool
}

type ToolPlan struct {
	Prefetch []PlannedToolCall
}

// QARequest is the stable use-case input for standard and scenario handlers.
type QARequest struct {
	Question         string
	Conversation     ConversationContext
	PreloadedContext []ContextBlock
	Instructions     []string
	UserID           int64
	RolePrompt       string
	RunID            string
	EvidencePlan     *domain.EvidencePlan
	ToolPlan         ToolPlan
	AllowWrite       bool
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
	}

	useDashScope := platformSettings.RerankProvider == "dashscope" && platformSettings.RerankAPIKey != ""

	svc.llm = llm.NewLLMClientWithHTTPAndProvider(platformSettings.LLMBaseURL, platformSettings.LLMAPIKey, platformSettings.LLMModel, platformSettings.LLMProvider, platformSettings.LLMMaxTokens, nil)
	if platformSettings.FastLLMConfigured() {
		fastProvider := platformSettings.LLMProvider
		if fastProvider == "" {
			fastProvider = "openai"
		}
		svc.fastLLM = llm.NewLLMClientWithHTTPAndProvider(platformSettings.LLMBaseURL, platformSettings.LLMAPIKey, platformSettings.LLMFastModel, fastProvider, platformSettings.LLMMaxTokens, nil)
		log.Infof("[qa] fast model enabled for preprocess/queryterms: %s @ %s (%s)", platformSettings.LLMFastModel, platformSettings.LLMBaseURL, fastProvider)
	} else {
		svc.fastLLM = svc.llm
	}
	log.Infof("[qa] retrieval router: direct_min_confidence=%.2f max_tokens=%d", routerConfidence, routerMaxTokens)
	if useDashScope {
		ret.WithReranker(retrieval.NewDashScopeReranker(platformSettings))
		log.Infof("[qa] reranker: dashscope (%s)", platformSettings.RerankModel)
	}

	svc.runStore = d.RunStore
	svc.hub = NewRunHub(svc.runStore)

	if d.Registry != nil {
		svc.registry = d.Registry
	} else {
		svc.registry = NewRegistry(d.Tools, d.Cfg)
	}
	svc.writeAvailable = d.WriteAvailable
	svc.executor = NewToolExecutor(svc.registry)

	svc.agent = NewAgent(svc.llm, svc.executor, AgentConfig{
		Timeout:             time.Duration(platformSettings.AgentTimeout),
		MaxSteps:            platformSettings.AgentMaxSteps,
		AnswerReserve:       time.Duration(platformSettings.AgentAnswerReserve),
		AnswerMaxTokens:     platformSettings.LLMAnswerMaxTokens,
		ConclusionMaxTokens: platformSettings.LLMConclusionMaxTokens,
		MaxContinueRounds:   platformSettings.LLMMaxContinueRounds,
		DomainKnowledge:     platformSettings.DomainKnowledge,
		HistoryLimit:        6,
	}, svc.hub, svc.hub)
	// Keep the phase hint behind the reasoning stream.
	svc.agent.SetOnFirstAnswerToken(func(runID string) {
		svc.emitStep(runID, "找到啦，我来把答案写出来 ✍️")
	})

	if d.DB != nil && d.Embedder != nil && d.Embedder.Enabled() {
		memorySemanticConfig := d.Cfg.Semantic
		memorySemanticConfig.Collection = "memory"
		memSemantic, err := semanticstore.New(memorySemanticConfig)
		if err != nil {
			log.Warnf("[qa] memory semantic store init failed: %v", err)
		} else if err := memSemantic.Ensure(context.Background(), semantic.Schema{Collection: "memory", DenseDim: d.Embedder.Dim()}); err != nil {
			_ = memSemantic.Close()
			log.Warnf("[qa] memory collection ensure failed: %v", err)
		} else {
			svc.memory = memory.NewMemoryStore(d.DB, memSemantic, d.Embedder, d.Cfg.MemoryWorkContextTTL)
		}
	}

	return svc
}

func (svc *QA) Hub() *RunHub { return svc.hub }

func (svc *QA) RunStore() *RunStore { return svc.runStore }

func (svc *QA) Memory() *memory.MemoryStore { return svc.memory }

func (svc *QA) LLM() *llm.LLMClient { return svc.llm }

// UsageContext associates an auxiliary model call with the Run that triggered it.
func (svc *QA) UsageContext(ctx context.Context, runID, phase string) context.Context {
	if svc == nil || svc.runStore == nil || runID == "" {
		return ctx
	}
	ctx = llm.WithUsageRecorder(ctx, runID, svc.runStore)
	return llm.WithUsagePhase(ctx, phase)
}

// emitStep pushes a lightweight phase hint to the run hub.
func (svc *QA) emitStep(runID, text string) {
	if svc.hub != nil {
		svc.hub.EmitPhase(runID, text)
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

// AskAgentWithContext preserves the canonical session summary without recompressing it.
func (svc *QA) AskAgentWithContext(ctx context.Context, question string, conversation ConversationContext, userID int64, rolePrompt, runID string, explicitPlan *domain.EvidencePlan, allowWrite bool) (*AskResult, error) {
	return svc.Ask(ctx, QARequest{
		Question: question, Conversation: conversation, UserID: userID,
		RolePrompt: rolePrompt, RunID: runID, EvidencePlan: explicitPlan, AllowWrite: allowWrite,
	})
}

// Ask starts one QA run with optional trusted scenario context.
func (svc *QA) Ask(ctx context.Context, request QARequest) (*AskResult, error) {
	question := strings.TrimSpace(request.Question)
	if question == "" {
		return nil, fmt.Errorf("question is required")
	}
	conversation := request.Conversation
	for _, instruction := range request.Instructions {
		if instruction = strings.TrimSpace(instruction); instruction != "" {
			conversation.Instructions = append(conversation.Instructions, llm.Message{Role: "system", Content: instruction})
		}
	}
	userID := request.UserID
	rolePrompt := request.RolePrompt
	runID := request.RunID
	if runID == "" {
		runID = NewRunID()
	}
	if svc.runStore != nil {
		if err := svc.runStore.Create(RunRecord{
			ID: runID, UserID: userID, SessionID: conversation.SessionID,
			Question: question, Mode: "single", MaxSteps: svc.agent.MaxStepsFor(question),
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return nil, fmt.Errorf("create agent run %q: %w", runID, err)
		}
		ctx = llm.WithUsageRecorder(ctx, runID, svc.runStore)
	}
	explicitPlan := request.EvidencePlan
	toolPolicy := ToolPolicyForPlan(domain.DirectPlan(), svc.writeAvailable && request.AllowWrite)
	executor := svc.toolExecutor()
	candidateSnapshot := executor.Snapshot(toolPolicy)
	toolCandidates := routingCandidates(candidateSnapshot)
	traceEnabled := domain.TraceEnabled(ctx)
	emit := func(text string) {
		svc.emitStep(runID, text)
	}

	emit("嗯...让我先琢磨一下你在问什么 ✨")
	retrievalPrefix := buildRagCtx(conversation.Recent)
	continuityBasis := retrievalPrefix
	if continuityBasis == "" {
		continuityBasis = conversation.Summary
	}
	if hasConflictingConversationEntity(question, continuityBasis) {
		log.InfofCtx(ctx, "[qa] conversation context omitted after explicit entity switch")
		conversation.Summary = ""
		conversation.Recent = nil
		retrievalPrefix = ""
	}
	routeContext := buildRouteContext(conversation.Summary, retrievalPrefix)

	cleanQuestion := strings.TrimSpace(question)
	var terms retrieval.QueryTerms
	var timeExpr retrieval.TimeExpr
	decision := domain.InternalFallbackDecision()
	var planningErr error
	var routedToolIDs []string
	var preWg sync.WaitGroup
	requestAnchor := time.Now()
	analysisStarted := requestAnchor
	var planningDuration time.Duration
	preWg.Add(1)
	go func() {
		defer preWg.Done()
		planningStarted := time.Now()
		defer func() { planningDuration = time.Since(planningStarted) }()
		hctx, cancel := context.WithTimeout(llm.WithUsagePhase(ctx, llm.PhaseRoute), helperTimeout)
		defer cancel()
		termsQuestion := strings.TrimSpace(question)
		if explicitPlan != nil {
			analysis, err := retrieval.AnalyzeForPlan(
				hctx, svc.fastLLM, question, routeContext, termsQuestion,
				toolCandidates, svc.routerMaxTokens, *explicitPlan,
			)
			if err != nil {
				planningErr = err
				cleanQuestion = strings.TrimSpace(question)
				decision = domain.PlanDecision{Plan: *explicitPlan, Confidence: 1, Origin: domain.Explicit}
				return
			}
			cleanQuestion, terms, timeExpr, decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			routedToolIDs = analysis.ToolIDs
		} else if shouldShortCircuitMeta(question) {
			cleanQuestion = strings.TrimSpace(question)
			decision = domain.PlanDecision{
				Plan: domain.DirectPlan(), Confidence: 1, Origin: domain.Rule,
			}
		} else {
			analysis, err := retrieval.AnalyzeEvidence(
				hctx, svc.fastLLM, question, routeContext, termsQuestion,
				retrieval.RoutingCapabilities{
					Memory: svc.memory != nil && svc.memory.Enabled() && userID != 0,
					Web:    svc.cfg.WebSearchEnabled,
				},
				toolCandidates,
				svc.routerMaxTokens,
			)
			if err != nil {
				planningErr = err
				cleanQuestion = strings.TrimSpace(question)
				decision = domain.InternalFallbackDecision()
				return
			}
			cleanQuestion, terms, timeExpr, decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			routedToolIDs = analysis.ToolIDs
		}
	}()
	preWg.Wait()
	resolvedTime, hasResolvedTime, timeErr := retrieval.ResolveTime(timeExpr, requestAnchor)
	if timeErr != nil {
		planningErr = errors.Join(planningErr, fmt.Errorf("resolve relative time: %w", timeErr))
	} else if hasResolvedTime {
		ctx = tool.WithTimeRange(ctx, resolvedTime)
		log.InfofCtx(ctx, "[qa] relative time resolved raw=%q from=%s to=%s",
			resolvedTime.Raw, resolvedTime.From.Format(time.RFC3339), resolvedTime.To.Format(time.RFC3339))
	}
	timeFrom, timeTo := "", ""
	if hasResolvedTime {
		timeFrom = resolvedTime.From.Format(time.RFC3339)
		timeTo = resolvedTime.To.Format(time.RFC3339)
	}
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "query_analysis", DurationMS: time.Since(analysisStarted).Milliseconds(),
			Input: map[string]any{"question": question, "history_messages": len(conversation.Recent)},
			Output: map[string]any{
				"clean_question": cleanQuestion, "domain_terms": terms.DomainTerms,
				"identifiers":   terms.Identifiers,
				"response_mode": ClassifyResponseMode(question),
				"time_kind":     timeExpr.Kind, "time_raw": timeExpr.Raw,
				"time_from": timeFrom, "time_to": timeTo,
			},
		})
	}

	effectiveDecision := decision
	if decision.Origin == domain.Model &&
		decision.Plan.Direct() && decision.Confidence < svc.routerConfidence {
		log.WarnfCtx(ctx, "[qa] evidence planner direct confidence %.2f below %.2f; using internal fallback", decision.Confidence, svc.routerConfidence)
		effectiveDecision = domain.InternalFallbackDecision()
	}
	if planningErr != nil {
		log.WarnfCtx(ctx, "[qa] evidence planning degraded: %v", planningErr)
		routedToolIDs = nil
	}
	var retainedContextTools bool
	if planningErr == nil {
		routedToolIDs, retainedContextTools = contextualRoutedToolIDs(
			routedToolIDs, toolCandidates, routeContext, effectiveDecision.Plan,
		)
	}
	if retainedContextTools {
		log.InfofCtx(ctx, "[qa] contextual follow-up retained routed read tools: %v", routedToolIDs)
	}
	toolSnapshot, allowedToolIDs := selectRoutedTools(candidateSnapshot, routedToolIDs)
	toolPolicy.AllowedIDs = allowedToolIDs
	log.InfofCtx(ctx, "[qa] evidence plan proposed=%s proposed_sources=%v confidence=%.2f origin=%s effective=%s effective_sources=%v effective_confidence=%.2f effective_origin=%s",
		decision.Plan.String(), decision.Plan.SourceNames(), decision.Confidence, decision.Origin,
		effectiveDecision.Plan.String(), effectiveDecision.Plan.SourceNames(), effectiveDecision.Confidence, effectiveDecision.Origin)
	if traceEnabled {
		fallbackError := ""
		if planningErr != nil && effectiveDecision.Origin == domain.Fallback {
			fallbackError = planningErr.Error()
		}
		status := "completed"
		planningError := ""
		if planningErr != nil {
			status = "degraded"
			planningError = planningErr.Error()
		}
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "evidence_plan", Status: status, DurationMS: planningDuration.Milliseconds(),
			Output: map[string]any{
				"response_mode": ClassifyResponseMode(question),
				"proposed_plan": decision.Plan.String(), "proposed_sources": decision.Plan.SourceNames(), "proposed_confidence": decision.Confidence,
				"proposed_origin": decision.Origin,
				"effective_plan":  effectiveDecision.Plan.String(), "effective_sources": effectiveDecision.Plan.SourceNames(), "effective_confidence": effectiveDecision.Confidence,
				"effective_origin": effectiveDecision.Origin, "routed_tool_ids": routedToolIDs,
				"planning_error": planningError, "fallback_error": fallbackError,
			},
		})
	}
	webUnavailable := effectiveDecision.Plan.Has(domain.Web) && !svc.cfg.WebSearchEnabled
	if webUnavailable {
		log.WarnfCtx(ctx, "[qa] retrieval source unavailable: web")
	}
	evidencePlan := effectiveDecision.Plan
	prefetched, err := svc.executePrefetch(ctx, toolSnapshot, request.ToolPlan)
	if err != nil {
		if svc.hub != nil {
			svc.hub.Complete(runID, RunOutcome{Status: RunStatusFailed, Err: err})
		}
		return nil, err
	}
	preloadedContext := make([]ContextBlock, 0, len(prefetched)+len(request.PreloadedContext))
	preloadedContext = append(preloadedContext, prefetched...)
	preloadedContext = append(preloadedContext, request.PreloadedContext...)

	var recalled []memory.MemoryRecord
	memoryUnavailable := ""
	if evidencePlan.Has(domain.Memory) {
		memoryStarted := time.Now()
		memoryStatus := "completed"
		memoryError := ""
		if svc.memory == nil || !svc.memory.Enabled() || userID == 0 {
			memoryStatus = "unavailable"
			memoryError = "memory capability not configured for this user"
			memoryUnavailable = memoryError
			log.WarnfCtx(ctx, "[qa] evidence source unavailable: memory")
		} else if recall, err := svc.memory.Recall(ctx, userID, question, 3); err == nil {
			recalled = recall.Records
			if traceEnabled {
				domain.RecordTrace(ctx, domain.EvaluationTrace{
					Node: "memory_recall", Status: memoryStatus, DurationMS: time.Since(memoryStarted).Milliseconds(),
					Input: map[string]any{"user_id": userID, "limit": 3, "temporal_intent": recall.Intent},
					Output: map[string]any{
						"candidates": recall.Stats.Candidates, "invalid_payload": recall.Stats.InvalidPayload,
						"missing_records": recall.Stats.MissingRecords, "unauthorized": recall.Stats.Unauthorized,
						"superseded_filtered": recall.Stats.SupersededFiltered, "expired_filtered": recall.Stats.ExpiredFiltered,
						"episode_filtered": recall.Stats.EpisodeFiltered, "records": recall.Stats.Injected,
					},
				})
			}
		} else {
			memoryStatus = "failed"
			memoryError = err.Error()
			memoryUnavailable = "memory recall failed: " + memoryError
			log.ErrorfCtx(ctx, "[qa] memory recall error: %v", err)
		}
		if traceEnabled && memoryStatus != "completed" {
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "memory_recall", Status: memoryStatus, DurationMS: time.Since(memoryStarted).Milliseconds(),
				Input:  map[string]any{"user_id": userID, "limit": 3},
				Output: map[string]any{"records": len(recalled), "error": memoryError},
			})
		}
	}
	if memoryUnavailable != "" {
		preloadedContext = append(preloadedContext, unavailableToolBlock("memory", memoryUnavailable))
	}
	q := cleanQuestion
	if q == "" {
		q = question
	}
	retrievalTerms := strings.TrimSpace(strings.Join(terms.DomainTerms, " "))
	if retrievalPrefix != "" {
		retrievalTerms = strings.TrimSpace(retrievalPrefix + " " + retrievalTerms)
	}
	canonicalQuery := canonicalRetrievalQuery(q, retrievalTerms)
	if canonicalQuery != q {
		log.InfofCtx(ctx, "[qa] retrieval query: augmented with grounded terms (%d chars)", len(retrievalTerms))
	}
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "query_rewrite", Input: map[string]any{"clean_question": q},
			Output: map[string]any{"retrieval_query": canonicalQuery, "context_augmented": canonicalQuery != q},
		})
	}
	preRetrieve := evidencePlan.Has(domain.Internal)
	if !preRetrieve {
		if traceEnabled {
			domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "retrieval_dispatch", Output: map[string]any{"skipped": true, "sources": evidencePlan.SourceNames()}})
		}
		rc := &retrieval.RetrievedContext{OriginalQuestion: question}
		mergePreloadedContext(rc, preloadedContext, svc.contextBudget())
		appendUnavailableWeb(rc, webUnavailable)
		return svc.runAgentWithSnapshot(ctx, question, conversation, userID, rc, recalled, rolePrompt, runID, effectiveDecision.Plan, toolPolicy, toolSnapshot)
	}

	emit("好嘞，关键词到手了，我去查一下资料~ 📚")
	rc, err := svc.retriever.RetrievePlan(
		ctx, canonicalQuery, question, terms, evidencePlan,
	)
	if err != nil {
		runErr := fmt.Errorf("retrieve internal evidence: %w", err)
		log.ErrorfCtx(ctx, "[qa] agent pre-retrieve error: %v", runErr)
		if svc.hub != nil {
			svc.hub.Complete(runID, RunOutcome{Status: RunStatusFailed, Err: runErr})
		}
		return nil, runErr
	}
	rc.OriginalQuestion = question
	mergePreloadedContext(rc, preloadedContext, svc.contextBudget())
	appendUnavailableWeb(rc, webUnavailable)
	log.InfofCtx(ctx, "[qa] agent pre-retrieve done: hitCount=%d contextLen=%d,question=%v", rc.HitCount, len(rc.Text), question)
	if len(rc.References) > 0 {
		var refStrs []string
		for _, r := range rc.References {
			refStrs = append(refStrs, fmt.Sprintf("%s:%s", r.Type, r.Target))
		}
		log.InfofCtx(ctx, "[qa] pre-retrieve refs: %s", platform.TruncateForLog(strings.Join(refStrs, " | "), 800))
	}
	log.InfofCtx(ctx, "[qa] pre-retrieve context:\n%s", platform.TruncateForLog(rc.Text, 4000))
	return svc.runAgentWithSnapshot(ctx, question, conversation, userID, rc, recalled, rolePrompt, runID, effectiveDecision.Plan, toolPolicy, toolSnapshot)
}

func routingCandidates(snapshot tool.Snapshot) []retrieval.ToolRouteCandidate {
	tools := snapshot.Tools()
	candidates := make([]retrieval.ToolRouteCandidate, 0, len(tools))
	for _, candidate := range tools {
		if candidate.Kind != tool.KindRead || candidate.Routing == nil {
			continue
		}
		candidates = append(candidates, retrieval.ToolRouteCandidate{
			ID: string(candidate.ID), Intent: candidate.Routing.Intent, Temporal: candidate.Routing.Temporal,
		})
	}
	return candidates
}

func allRoutedToolIDs(candidates []retrieval.ToolRouteCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	return ids
}

func contextualRoutedToolIDs(
	selected []string,
	candidates []retrieval.ToolRouteCandidate,
	routeContext string,
	plan domain.EvidencePlan,
) ([]string, bool) {
	if len(selected) > 0 || strings.TrimSpace(routeContext) == "" || !plan.Has(domain.Internal) {
		return selected, false
	}
	ids := allRoutedToolIDs(candidates)
	return ids, len(ids) > 0
}

func selectRoutedTools(snapshot tool.Snapshot, routedIDs []string) (tool.Snapshot, map[tool.ToolID]struct{}) {
	routed := make(map[tool.ToolID]struct{}, len(routedIDs))
	for _, id := range routedIDs {
		routed[tool.ToolID(id)] = struct{}{}
	}
	allowed := make(map[tool.ToolID]struct{})
	for _, candidate := range snapshot.Tools() {
		if candidate.Kind == tool.KindRead && candidate.Routing != nil {
			if _, selected := routed[candidate.ID]; !selected {
				continue
			}
		}
		allowed[candidate.ID] = struct{}{}
	}
	return snapshot.Select(allowed), allowed
}

func (svc *QA) executePrefetch(ctx context.Context, snapshot tool.Snapshot, plan ToolPlan) ([]ContextBlock, error) {
	if len(plan.Prefetch) == 0 {
		return nil, nil
	}
	blocks := make([]ContextBlock, 0, len(plan.Prefetch))
	for _, call := range plan.Prefetch {
		candidate, ok := snapshot.Get(call.ToolID)
		if !ok {
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q is unavailable", call.ToolID)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, "tool is unavailable"))
			continue
		}
		if candidate.Kind != tool.KindRead || candidate.Prefetch == nil {
			return nil, fmt.Errorf("prefetch tool %q is not eligible", call.ToolID)
		}
		result, err := svc.toolExecutor().ExecuteArguments(ctx, snapshot, call.ToolID, call.Arguments)
		if err != nil {
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q: %w", call.ToolID, err)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, err.Error()))
			continue
		}
		references := make([]retrieval.Reference, 0, len(result.References))
		for _, ref := range result.References {
			references = append(references, retrieval.Reference{
				Type: ref.Type, Label: ref.Label, Target: ref.Target,
			})
		}
		blocks = append(blocks, ContextBlock{
			Source: string(call.ToolID), Title: candidate.Description,
			Content: result.Content, References: references,
		})
	}
	return blocks, nil
}

func (svc *QA) toolExecutor() *ToolExecutor {
	if svc.executor != nil {
		return svc.executor
	}
	if svc.agent != nil && svc.agent.executor != nil {
		return svc.agent.executor
	}
	return NewToolExecutor(nil)
}

func unavailableToolBlock(id tool.ToolID, reason string) ContextBlock {
	return ContextBlock{
		Source:  string(id),
		Title:   string(id) + " unavailable",
		Content: "The prefetch could not be completed: " + reason,
	}
}

const preloadedContextBudget = 16000

func (svc *QA) contextBudget() int {
	if svc.retriever != nil {
		return svc.retriever.ContextBudget()
	}
	return 48000
}

func mergePreloadedContext(context *retrieval.RetrievedContext, blocks []ContextBlock, totalBudget int) {
	if context == nil || len(blocks) == 0 {
		return
	}
	if totalBudget <= 0 {
		totalBudget = 48000
	}
	seenContent := make(map[string]struct{}, len(blocks))
	seenRefs := make(map[string]struct{}, len(context.References)+len(blocks))
	for _, ref := range context.References {
		seenRefs[ref.Type+"\x00"+ref.Target] = struct{}{}
	}
	var text strings.Builder
	preloadedLimit := min(preloadedContextBudget, totalBudget)
	budget := preloadedLimit
	for _, block := range blocks {
		content := strings.TrimSpace(block.Content)
		if content == "" {
			continue
		}
		if _, duplicate := seenContent[content]; duplicate {
			continue
		}
		seenContent[content] = struct{}{}
		title := strings.TrimSpace(block.Title)
		if title == "" {
			title = strings.TrimSpace(block.Source)
		}
		if title == "" {
			title = "Preloaded Context"
		}
		section := "## " + title + "\n" + content + "\n"
		runes := []rune(section)
		if len(runes) > budget {
			runes = runes[:budget]
		}
		text.WriteString(string(runes))
		budget -= len(runes)
		for _, ref := range block.References {
			key := ref.Type + "\x00" + ref.Target
			if ref.Target == "" {
				continue
			}
			if _, duplicate := seenRefs[key]; duplicate {
				continue
			}
			seenRefs[key] = struct{}{}
			context.References = append(context.References, ref)
		}
		if budget == 0 {
			break
		}
	}
	if text.Len() == 0 {
		return
	}
	if context.Text != "" {
		remaining := totalBudget - (preloadedLimit - budget)
		if remaining > 0 {
			text.WriteString("\n")
			remaining--
		}
		if remaining > 0 {
			existing := []rune(context.Text)
			if len(existing) > remaining {
				existing = existing[:remaining]
			}
			text.WriteString(string(existing))
		}
	}
	context.Text = text.String()
	context.HitCount = len(context.References)
}

func canonicalRetrievalQuery(cleanQuestion, contextTerms string) string {
	q := strings.TrimSpace(cleanQuestion)
	terms := strings.TrimSpace(contextTerms)
	if terms != "" {
		return q + " " + terms
	}
	return q
}

func appendUnavailableWeb(rc *retrieval.RetrievedContext, unavailable bool) {
	if !unavailable || rc == nil {
		return
	}
	if rc.Text != "" {
		rc.Text += "\n\n"
	}
	rc.Text += "## Evidence Availability\n- Web source unavailable: web search is not configured.\n"
}

func (svc *QA) runAgentWithPlan(ctx context.Context, question string, conversation ConversationContext, userID int64, rc *retrieval.RetrievedContext, recalled []memory.MemoryRecord, rolePrompt, runID string, plan domain.EvidencePlan) (*AskResult, error) {
	policy := ToolPolicyForPlan(plan, false)
	return svc.runAgentWithSnapshot(ctx, question, conversation, userID, rc, recalled, rolePrompt, runID, plan, policy, svc.toolExecutor().Snapshot(policy))
}

func (svc *QA) runAgentWithSnapshot(ctx context.Context, question string, conversation ConversationContext, userID int64, rc *retrieval.RetrievedContext, recalled []memory.MemoryRecord, rolePrompt, runID string, plan domain.EvidencePlan, policy ToolPolicy, snapshot tool.Snapshot) (*AskResult, error) {
	log.InfofCtx(ctx, "[qa] runAgent runID=%s", runID)

	maxSteps := svc.agent.MaxStepsForPlan(question, plan)
	if svc.runStore != nil {
		if err := svc.runStore.SetMaxSteps(runID, maxSteps); err != nil {
			log.ErrorfCtx(ctx, "[qa] update run max steps: %v", err)
		}
	}

	instructions := append([]llm.Message{}, conversation.Instructions...)
	if len(recalled) > 0 {
		memoryStarted := time.Now()
		formatted := memory.FormatMemories(recalled)
		instructions = append(instructions, llm.Message{Role: "system", Content: formatted})
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "memory_inject", DurationMS: time.Since(memoryStarted).Milliseconds(),
			Output: map[string]any{"records": len(recalled), "characters": len([]rune(formatted))},
		})
	}
	conversation.RolePrompt = rolePrompt
	conversation.Instructions = instructions

	go func() {
		res, runErr := svc.agent.RunWithSnapshot(ctx, runID, question, conversation, rc, plan, policy, snapshot)
		outcome := outcomeFor(res, runErr)
		if svc.hub != nil {
			svc.hub.Complete(runID, outcome)
		}

		if outcome.Status == RunStatusDone && res != nil &&
			svc.memory != nil && userID != 0 && res.Answer != "" {
			memCtx := llm.WithUsagePhase(context.WithoutCancel(ctx), llm.PhaseMemoryExtract)
			memCtx, memCancel := context.WithTimeout(memCtx, 60*time.Second)
			extractStarted := time.Now()
			if mems, err := memory.ExtractMemories(memCtx, svc.llm, question, res.Answer); err == nil {
				domain.RecordTrace(memCtx, domain.EvaluationTrace{
					Node: "memory_extract", DurationMS: time.Since(extractStarted).Milliseconds(),
					Output: map[string]any{"records": len(mems)},
				})
				writeStarted := time.Now()
				outcomes := make(map[memory.WriteOutcome]int, 4)
				vectorSynced := 0
				for i := range mems {
					mems[i].UserID = userID
					mems[i].SourceSession = conversation.SessionID
					result, err := svc.memory.Write(memCtx, mems[i])
					if err != nil {
						log.ErrorfCtx(ctx, "[qa] memory write error: %v", err)
						continue
					}
					outcomes[result.Outcome]++
					if result.VectorSynced {
						vectorSynced++
					}
				}
				domain.RecordTrace(memCtx, domain.EvaluationTrace{
					Node: "memory_write", DurationMS: time.Since(writeStarted).Milliseconds(),
					Output: map[string]any{
						"records": len(mems), "inserted": outcomes[memory.WriteInserted],
						"refreshed": outcomes[memory.WriteRefreshed], "superseded": outcomes[memory.WriteSuperseded],
						"rejected": outcomes[memory.WriteRejected], "vector_synced": vectorSynced,
					},
				})
				if len(mems) > 0 {
					log.InfofCtx(ctx, "[qa] extracted %d memories for user %d", len(mems), userID)
				}
			} else {
				log.ErrorfCtx(ctx, "[qa] memory extraction error: %v", err)
				domain.RecordTrace(memCtx, domain.EvaluationTrace{
					Node: "memory_extract", Status: "failed", DurationMS: time.Since(extractStarted).Milliseconds(),
					Output: map[string]any{"error": err.Error()},
				})
			}
			memCancel()
		}
	}()

	return &AskResult{RunID: runID, Context: rc}, nil
}

func buildRagCtx(history []llm.Message) string {
	if len(history) == 0 {
		return ""
	}

	var lastUserQ, lastAssistA string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && lastAssistA == "" {
			lastAssistA = history[i].Content
		}
		if history[i].Role == "user" && lastUserQ == "" {
			lastUserQ = history[i].Content
		}
		if lastUserQ != "" && lastAssistA != "" {
			break
		}
	}
	if lastUserQ == "" && lastAssistA == "" {
		return ""
	}

	var parts []string
	if lastUserQ != "" {
		parts = append(parts, lastUserQ)
	}
	// Carry over only explicit backtick identifiers the assistant deliberately marked -
	// not free-text tech terms, which re-inject tangential names from a possibly-wrong
	// prior answer and drift the next retrieval. Anaphora is ResolveStandaloneQuery's job.
	parts = append(parts, extractBacktick(lastAssistA)...)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func buildRouteContext(summary, recent string) string {
	const summaryLimit = 1500
	runes := []rune(summary)
	if len(runes) > summaryLimit {
		summary = string(runes[:summaryLimit])
	}
	if summary == "" {
		return recent
	}
	if recent == "" {
		return "[summary]: " + summary
	}
	return "[summary]: " + summary + "\n" + recent
}

func extractBacktick(text string) []string {
	var tokens []string
	seen := map[string]bool{}
	for {
		start := strings.Index(text, "`")
		if start < 0 {
			break
		}
		text = text[start+1:]
		end := strings.Index(text, "`")
		if end < 0 {
			break
		}
		tok := strings.ToLower(strings.TrimSpace(text[:end]))
		text = text[end+1:]
		if len(tok) >= 4 && !seen[tok] {
			seen[tok] = true
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

func NewRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Errorf("[agent] crypto/rand.Read failed: %v — falling back to timestamp-based id", err)
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return "run_" + hex.EncodeToString(b[:])
}

type RunStore struct {
	db *sql.DB
}

// NewRunStore binds agent run queries to the platform-owned MySQL pool.
func NewRunStore(db *sql.DB) (*RunStore, error) {
	if db == nil {
		return nil, fmt.Errorf("agent/runstore: database is required")
	}
	runStore := &RunStore{db: db}
	recovered, err := runStore.RecoverInterrupted()
	if err != nil {
		return nil, fmt.Errorf("agent/runstore: recover interrupted runs: %w", err)
	}
	if recovered > 0 {
		log.Warnf("[qa] recovered %d interrupted agent runs as aborted", recovered)
	}
	return runStore, nil
}

// RecoverInterrupted closes process-local Runs left active by a prior process.
func (rs *RunStore) RecoverInterrupted() (int64, error) {
	result, err := rs.db.Exec(
		`UPDATE agent_runs SET status=?,ended_at=? WHERE status IN (?,?)`,
		RunStatusAborted, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)),
		RunStatusRunning, RunStatusPaused,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (rs *RunStore) Create(r RunRecord) error {
	if r.StartedAt == "" {
		r.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if r.Status == "" {
		r.Status = RunStatusRunning
	}
	_, err := rs.db.Exec(
		`INSERT INTO agent_runs(id,user_id,session_id,question,status,mode,max_steps,step_count,token_used,started_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.UserID, r.SessionID, r.Question, r.Status, r.Mode, r.MaxSteps, 0, 0, store.DatabaseTime(r.StartedAt))
	return err
}

var ErrRunNotActive = errors.New("agent: run is missing or already terminal")

// Complete atomically moves an active Run to one terminal state.
func (rs *RunStore) Complete(id string, outcome RunOutcome) error {
	if !outcome.Status.Terminal() {
		return fmt.Errorf("agent: complete run with non-terminal status %q", outcome.Status)
	}
	result, err := rs.db.Exec(
		`UPDATE agent_runs
		 SET status=?,step_count=?,token_used=?,ended_at=?
		 WHERE id=? AND status IN (?,?)`,
		outcome.Status, outcome.StepCount, outcome.TokenUsed,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), id,
		RunStatusRunning, RunStatusPaused,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRunNotActive
	}
	return nil
}

// TransitionControl updates the persisted pause state without changing terminal fields.
func (rs *RunStore) TransitionControl(id string, from, to RunStatus) error {
	if !validControlTransition(from, to) {
		return fmt.Errorf("agent: invalid run control transition %q -> %q", from, to)
	}
	result, err := rs.db.Exec(`UPDATE agent_runs SET status=? WHERE id=? AND status=?`, to, id, from)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRunNotActive
	}
	return nil
}

func (rs *RunStore) AddStep(st StepRow) error {
	if st.CreatedAt == "" {
		st.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := rs.db.Exec(
		`INSERT INTO agent_steps(run_id,step_no,kind,tool,args,result_summary,content,token_delta,reasoning_tokens,duration_ms,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		st.RunID, st.StepNo, st.Kind, st.Tool, st.Args, st.ResultSummary, st.Content, st.TokenDelta, st.ReasoningTokens, st.DurationMs, store.DatabaseTime(st.CreatedAt))
	return err
}

// SetMaxSteps records the plan-specific loop bound resolved after routing.
func (rs *RunStore) SetMaxSteps(id string, maxSteps int) error {
	_, err := rs.db.Exec(`UPDATE agent_runs SET max_steps=? WHERE id=?`, maxSteps, id)
	return err
}

// RecordLLMCall stores one provider call and updates its Run aggregate atomically.
func (rs *RunStore) RecordLLMCall(ctx context.Context, call llm.CallUsage) error {
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var callCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT llm_call_count FROM agent_runs WHERE id=? FOR UPDATE`, call.RunID,
	).Scan(&callCount); err != nil {
		return err
	}
	callSeq := callCount + 1
	usage := call.Usage
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_llm_calls(
			run_id,call_seq,phase,provider,model,input_tokens,cached_input_tokens,
			output_tokens,reasoning_tokens,total_tokens,max_output_tokens,duration_ms,status)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		call.RunID, callSeq, call.Phase, call.Provider, call.Model,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		usage.ReasoningTokens, usage.TotalTokens, call.MaxOutputTokens,
		call.Duration.Milliseconds(), call.Status,
	); err != nil {
		return err
	}
	reservedTokens := 0
	if call.MaxOutputTokens > 0 {
		reservedTokens = usage.InputTokens + call.MaxOutputTokens
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_runs SET
			input_tokens=input_tokens+?,cached_input_tokens=cached_input_tokens+?,
			output_tokens=output_tokens+?,reasoning_tokens=reasoning_tokens+?,
			total_tokens=total_tokens+?,llm_call_count=?,
			peak_input_tokens=GREATEST(peak_input_tokens,?),
			peak_reserved_tokens=GREATEST(peak_reserved_tokens,?)
		 WHERE id=?`,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		usage.ReasoningTokens, usage.TotalTokens, callSeq,
		usage.InputTokens, reservedTokens, call.RunID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (rs *RunStore) List(userID int64, sessionID string, status RunStatus, limit int) ([]RunRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	page, err := rs.ListPage(userID, sessionID, status, 1, limit)
	if err != nil {
		return nil, err
	}
	return page.List, nil
}

func (rs *RunStore) ListPage(userID int64, sessionID string, status RunStatus, page, pageSize int) (*domain.Page[RunRecord], error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}

	q := `SELECT id,user_id,session_id,question,status,mode,max_steps,step_count,token_used,
		input_tokens,cached_input_tokens,output_tokens,reasoning_tokens,total_tokens,llm_call_count,
		peak_input_tokens,peak_reserved_tokens,started_at,ended_at FROM agent_runs`
	countQ := `SELECT COUNT(*) FROM agent_runs`
	var where []string
	var args []any
	if userID != 0 {
		where = append(where, "user_id=?")
		args = append(args, userID)
	}
	if sessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, sessionID)
	}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if len(where) > 0 {
		cond := " WHERE " + strings.Join(where, " AND ")
		q += cond
		countQ += cond
	}

	var total int
	if err := rs.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, err
	}

	q += " ORDER BY started_at DESC LIMIT ? OFFSET ?"
	offset := (page - 1) * pageSize
	args = append(args, pageSize, offset)

	rows, err := rs.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RunRecord, 0, pageSize)
	for rows.Next() {
		r, err := scanRunRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &domain.Page[RunRecord]{Total: total, Page: page, PageSize: pageSize, List: out}, nil
}

func (rs *RunStore) Get(id string) (*RunDetail, error) {
	r, err := scanRunRecord(rs.db.QueryRow(
		`SELECT id,user_id,session_id,question,status,mode,max_steps,step_count,token_used,
			input_tokens,cached_input_tokens,output_tokens,reasoning_tokens,total_tokens,llm_call_count,
			peak_input_tokens,peak_reserved_tokens,started_at,ended_at
		 FROM agent_runs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}

	rows, err := rs.db.Query(
		`SELECT id,run_id,step_no,kind,tool,args,result_summary,content,token_delta,reasoning_tokens,duration_ms,created_at
		 FROM agent_steps WHERE run_id=? ORDER BY step_no, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []StepRow
	for rows.Next() {
		var st StepRow
		var args, summary, content sql.NullString
		var createdAt sql.NullTime
		if err := rows.Scan(&st.ID, &st.RunID, &st.StepNo, &st.Kind, &st.Tool, &args, &summary, &content, &st.TokenDelta, &st.ReasoningTokens, &st.DurationMs, &createdAt); err != nil {
			return nil, err
		}
		st.Args = args.String
		st.ResultSummary = summary.String
		st.Content = content.String
		st.CreatedAt = store.FormatDatabaseTime(createdAt)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	llmCalls, err := rs.listLLMCalls(id, 1000)
	if err != nil {
		return nil, err
	}
	return &RunDetail{RunRecord: r, Steps: steps, LLMCalls: llmCalls}, nil
}

func (rs *RunStore) listLLMCalls(runID string, limit int) ([]LLMCallRow, error) {
	rows, err := rs.db.Query(
		`SELECT id,run_id,call_seq,phase,provider,model,input_tokens,cached_input_tokens,
			output_tokens,reasoning_tokens,total_tokens,max_output_tokens,duration_ms,status,created_at
		 FROM agent_llm_calls WHERE run_id=? ORDER BY call_seq LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := make([]LLMCallRow, 0, min(limit, 16))
	for rows.Next() {
		var call LLMCallRow
		var createdAt sql.NullTime
		if err := rows.Scan(
			&call.ID, &call.RunID, &call.CallSeq, &call.Phase, &call.Provider, &call.Model,
			&call.InputTokens, &call.CachedInputTokens, &call.OutputTokens,
			&call.ReasoningTokens, &call.TotalTokens, &call.MaxOutputTokens,
			&call.DurationMs, &call.Status, &createdAt,
		); err != nil {
			return nil, err
		}
		call.CreatedAt = store.FormatDatabaseTime(createdAt)
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

type rowScanner interface {
	Scan(...any) error
}

func scanRunRecord(row rowScanner) (RunRecord, error) {
	var record RunRecord
	var startedAt, endedAt sql.NullTime
	if err := row.Scan(&record.ID, &record.UserID, &record.SessionID, &record.Question, &record.Status,
		&record.Mode, &record.MaxSteps, &record.StepCount, &record.TokenUsed,
		&record.InputTokens, &record.CachedInputTokens, &record.OutputTokens,
		&record.ReasoningTokens, &record.TotalTokens, &record.LLMCallCount,
		&record.PeakInputTokens, &record.PeakReservedTokens, &startedAt, &endedAt); err != nil {
		return record, err
	}
	record.StartedAt = store.FormatDatabaseTime(startedAt)
	record.EndedAt = store.FormatDatabaseTime(endedAt)
	return record, nil
}

func (rs *RunStore) DeleteBySession(sessionID string, userID int64) error {
	tx, err := rs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE c FROM agent_llm_calls c JOIN agent_runs r ON c.run_id = r.id WHERE r.session_id = ? AND r.user_id=?`,
		sessionID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE s FROM agent_steps s JOIN agent_runs r ON s.run_id = r.id WHERE r.session_id = ? AND r.user_id=?`,
		sessionID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_runs WHERE session_id = ? AND user_id=?`, sessionID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

type RunStatus string

const (
	RunStatusRunning RunStatus = "running"
	RunStatusDone    RunStatus = "done"
	RunStatusFailed  RunStatus = "failed"
	RunStatusAborted RunStatus = "aborted"
	RunStatusPaused  RunStatus = "paused"
)

func (status RunStatus) Terminal() bool {
	switch status {
	case RunStatusDone, RunStatusFailed, RunStatusAborted:
		return true
	default:
		return false
	}
}

func validControlTransition(from, to RunStatus) bool {
	return from == RunStatusRunning && to == RunStatusPaused ||
		from == RunStatusPaused && to == RunStatusRunning
}

var ErrEmptyAnswer = errors.New("agent: completed without a visible answer")

// RunOutcome is the single terminal fact consumed by persistence and streaming.
type RunOutcome struct {
	Status    RunStatus
	StepCount int
	TokenUsed int
	Answer    string
	Err       error
}

func outcomeFor(result *RunResult, runErr error) RunOutcome {
	if result == nil {
		if runErr == nil {
			runErr = errors.New("agent: run returned no result")
		}
		return RunOutcome{Status: RunStatusFailed, Err: runErr}
	}
	outcome := RunOutcome{
		StepCount: result.Steps,
		TokenUsed: len(result.Answer),
		Answer:    result.Answer,
	}
	switch {
	case result.Aborted:
		outcome.Status = RunStatusAborted
		outcome.Err = runErr
	case runErr != nil:
		outcome.Status = RunStatusFailed
		outcome.Err = runErr
	case result.Err != nil:
		outcome.Status = RunStatusFailed
		outcome.Err = result.Err
	case strings.TrimSpace(result.Answer) == "":
		outcome.Status = RunStatusFailed
		outcome.Err = ErrEmptyAnswer
	default:
		outcome.Status = RunStatusDone
	}
	return outcome
}

type RunRecord struct {
	ID                 string    `json:"id"`
	UserID             int64     `json:"user_id"`
	SessionID          string    `json:"session_id"`
	Question           string    `json:"question"`
	Status             RunStatus `json:"status"`
	Mode               string    `json:"mode"`
	MaxSteps           int       `json:"max_steps"`
	StepCount          int       `json:"step_count"`
	TokenUsed          int       `json:"token_used"`
	InputTokens        int64     `json:"input_tokens"`
	CachedInputTokens  int64     `json:"cached_input_tokens"`
	OutputTokens       int64     `json:"output_tokens"`
	ReasoningTokens    int64     `json:"reasoning_tokens"`
	TotalTokens        int64     `json:"total_tokens"`
	LLMCallCount       int       `json:"llm_call_count"`
	PeakInputTokens    int       `json:"peak_input_tokens"`
	PeakReservedTokens int       `json:"peak_reserved_tokens"`
	StartedAt          string    `json:"started_at"`
	EndedAt            string    `json:"ended_at"`
}

type LLMCallRow struct {
	ID                int64  `json:"id"`
	RunID             string `json:"run_id"`
	CallSeq           int    `json:"call_seq"`
	Phase             string `json:"phase"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	InputTokens       int    `json:"input_tokens"`
	CachedInputTokens int    `json:"cached_input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	ReasoningTokens   int    `json:"reasoning_tokens"`
	TotalTokens       int    `json:"total_tokens"`
	MaxOutputTokens   int    `json:"max_output_tokens"`
	DurationMs        int64  `json:"duration_ms"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
}

type StepKind string

const (
	StepKindThink      StepKind = "think"
	StepKindToolCall   StepKind = "tool_call"
	StepKindToolResult StepKind = "tool_result"
	StepKindAnswer     StepKind = "answer"
	StepKindRetrieval  StepKind = "retrieval"
)

type StepRecord struct {
	StepNo          int       `json:"step_no"`
	Kind            StepKind  `json:"kind"`
	Tool            string    `json:"tool,omitempty"`
	Args            string    `json:"args,omitempty"`
	ResultSummary   string    `json:"result_summary,omitempty"`
	Content         string    `json:"content,omitempty"`
	TokenDelta      int       `json:"token_delta"`
	ReasoningTokens int       `json:"reasoning_tokens"`
	DurationMs      int       `json:"duration_ms"`
	CreatedAt       time.Time `json:"created_at"`
}

type StepRow struct {
	ID              int64    `json:"id"`
	RunID           string   `json:"run_id"`
	StepNo          int      `json:"step_no"`
	Kind            StepKind `json:"kind"`
	Tool            string   `json:"tool,omitempty"`
	Args            string   `json:"args,omitempty"`
	ResultSummary   string   `json:"result_summary,omitempty"`
	Content         string   `json:"content,omitempty"`
	TokenDelta      int      `json:"token_delta"`
	ReasoningTokens int      `json:"reasoning_tokens"`
	DurationMs      int      `json:"duration_ms"`
	CreatedAt       string   `json:"created_at"`
}

type RunDetail struct {
	RunRecord
	Steps    []StepRow    `json:"steps"`
	LLMCalls []LLMCallRow `json:"llm_calls"`
}

type ControlKind int

const (
	CtrlNone ControlKind = iota
	CtrlPause
	CtrlAbort
	CtrlNudge
)

type ControlSignal struct {
	Kind    ControlKind
	Message string
}

type RunPage = domain.Page[RunRecord]
