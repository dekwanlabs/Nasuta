package qa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

type qaPreparation struct {
	request          QARequest
	ctx              context.Context
	trace            *executiontrace.Scope
	ownsTrace        bool
	toolPolicy       ToolPolicy
	candidateToolSet ScenarioToolSet
	toolCandidates   []retrieval.ToolRouteCandidate
	planning         evidencePlanningOutput
	analysis         queryAnalysisOutput
	execution        executionRouteDecision
}

type qaEvidence struct {
	retrieved *retrieval.RetrievedContext
	recalled  []memory.MemoryRecord
}

func (prepared *qaPreparation) closeTrace() {
	if !prepared.ownsTrace {
		return
	}
	prepared.trace.Close()
	prepared.ownsTrace = false
}

func (prepared *qaPreparation) finishPreparationFailure(
	ctx context.Context,
	run agentapi.ManagedRun,
	err error,
) {
	if finishErr := run.Finish(&agentapi.RunError{
		Code: "preparation_failed", Message: err.Error(),
	}); finishErr != nil {
		log.ErrorfCtx(ctx, "[qa] finish failed preparation %s: %v", prepared.request.RunID, finishErr)
	}
	prepared.closeTrace()
}

func (svc *QA) prepareQA(ctx context.Context, request QARequest) (*qaPreparation, error) {
	request, err := normalizeQARequest(request)
	if err != nil {
		return nil, err
	}

	trace, ownsTrace := beginExecutionTrace(ctx)
	ctx = executiontrace.WithScope(ctx, trace)
	ctx = executiontrace.WithCorrelation(ctx, executiontrace.Correlation{
		RunID: request.RunID, ParentRunID: request.ParentRunID,
	})
	prepared := &qaPreparation{
		request: request, ctx: ctx, trace: trace, ownsTrace: ownsTrace,
	}

	prepared.toolPolicy = toolPolicyForRun(svc.writeAvailable && request.AllowWrite)
	prepared.candidateToolSet = svc.runtimeTools.PrepareTools(prepared.toolPolicy)
	if request.Conversation.CompactedThroughTurn <= 0 || svc.history == nil {
		prepared.candidateToolSet = withoutSessionHistoryTools(prepared.candidateToolSet)
	}
	prepared.toolCandidates = routingCandidates(prepared.candidateToolSet.Tools())

	svc.emitStep(request.RunID, "嗯...让我先琢磨一下你在问什么 ✨")
	routeContext := buildHistoryRouteContext(request.Conversation)
	if routeContext == "" {
		routeContext = buildRagCtx(request.Conversation.Recent)
	}

	requestAnchor := time.Now()
	hctx, cancelPlanning := context.WithTimeout(
		llm.WithUsagePhase(ctx, llm.PhaseRoute), helperTimeout,
	)
	prepared.planning, _ = svc.planEvidence(hctx, evidencePlanningInput{
		Question: request.Question, RouteContext: routeContext, ExplicitPlan: request.EvidencePlan,
		ToolCandidates: prepared.toolCandidates,
		AvailableTools: scenarioToolIDs(prepared.candidateToolSet.Tools()),
		UserID:         request.UserID,
	})
	cancelPlanning()

	prepared.analysis, _ = analyzeQuery(ctx, queryAnalysisInput{
		Question: request.Question, CleanQuestion: prepared.planning.CleanQuestion,
		Terms: prepared.planning.Terms, Time: prepared.planning.Time,
		Anchor: requestAnchor, RecentTurns: request.Conversation.RecentTurns,
		History: prepared.planning.History, HistoryValid: prepared.planning.HistoryValid,
	})
	assembled, err := svc.assembleContext(ctx, contextAssembleInput{
		Question: request.Question, UserID: request.UserID, Conversation: request.Conversation,
		Relation: prepared.analysis.History, Origin: prepared.analysis.HistoryOrigin,
		Upgrade: prepared.analysis.HistoryUpdate,
	})
	if err != nil {
		prepared.closeTrace()
		return nil, err
	}
	prepared.request.Conversation = assembled.Conversation
	if prepared.analysis.TimeError != nil {
		prepared.planning.PlanningError = errors.Join(
			prepared.planning.PlanningError,
			fmt.Errorf("resolve relative time: %w", prepared.analysis.TimeError),
		)
	} else if prepared.analysis.HasTimeRange {
		prepared.ctx = tool.WithTimeRange(prepared.ctx, prepared.analysis.TimeRange)
		log.InfofCtx(prepared.ctx, "[qa] relative time resolved raw=%q from=%s to=%s",
			prepared.analysis.TimeRange.Raw,
			prepared.analysis.TimeRange.From.Format(time.RFC3339),
			prepared.analysis.TimeRange.To.Format(time.RFC3339),
		)
	}

	svc.routeQAExecution(prepared)
	return prepared, nil
}

func normalizeQARequest(request QARequest) (QARequest, error) {
	request.Question = strings.TrimSpace(request.Question)
	if request.Question == "" {
		return QARequest{}, fmt.Errorf("question is required")
	}

	request.Conversation.Instructions = append(
		[]llm.Message(nil), request.Conversation.Instructions...,
	)
	if request.RunID == "" {
		request.RunID = NewRunID()
	}
	return request, nil
}

func (svc *QA) routeQAExecution(prepared *qaPreparation) {
	planning := prepared.planning
	decision, effectiveDecision := planning.Decision, planning.Effective
	if decision.Origin == domain.Model &&
		decision.Plan.Direct() && decision.Confidence < svc.routerConfidence {
		log.WarnfCtx(prepared.ctx, "[qa] evidence planner direct confidence %.2f below %.2f; using internal fallback",
			decision.Confidence, svc.routerConfidence,
		)
	}
	if planning.PlanningError != nil {
		log.WarnfCtx(prepared.ctx, "[qa] evidence planning degraded: %v", planning.PlanningError)
		planning.RoutedToolIDs = nil
		prepared.planning.RoutedToolIDs = nil
	}

	policy := ExecutionPolicy{
		AllowMultiAgent: standardQARequest(prepared.request, svc.agentRef),
		MinComplexity:   defaultMultiAgentMinComplexity,
		MinConfidence:   defaultMultiAgentMinConfidence,
	}
	workflowAvailable := false
	if policy.AllowMultiAgent && svc.investigation != nil && svc.scenarios != nil {
		workflowAvailable = svc.investigation.Available()
	}
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion: planning.Execution, Policy: policy,
		EvidencePlan: effectiveDecision.Plan, AllowWrite: prepared.request.AllowWrite,
		WorkflowAvailable: workflowAvailable,
		History:           planning.History, HistoryValid: planning.HistoryValid,
		ToolCandidates: prepared.toolCandidates, RoutedToolIDs: planning.RoutedToolIDs,
	})

	svc.emitExecutionEvent(EventExecutionRouted, ExecutionEvent{
		RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "completed",
		Complexity: planning.Execution.Complexity, Confidence: planning.Execution.Confidence,
	})
	degradedReason := prepared.execution.DowngradeReason
	if degradedReason == "" && planning.PlanningError != nil {
		degradedReason = "route_degraded"
	}
	if degradedReason != "" {
		svc.emitExecutionEvent(EventExecutionDegraded, ExecutionEvent{
			RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "degraded",
			Reason: degradedReason, Complexity: planning.Execution.Complexity,
			Confidence: planning.Execution.Confidence,
		})
	}
}

func (svc *QA) prepareSingleAgentRun(prepared *qaPreparation) (*AskResult, error) {
	definition, selection, err := svc.resolveAgentDefinition(prepared)
	if err != nil {
		prepared.closeTrace()
		return nil, err
	}
	run, err := svc.beginSingleAgentRun(prepared, definition, selection)
	if err != nil {
		prepared.closeTrace()
		return nil, err
	}
	runCtx := run.Context(prepared.ctx)

	conversation := svc.prepareRunConversation(prepared)
	planning := prepared.planning
	log.InfofCtx(runCtx, "[qa] evidence plan proposed=%s proposed_sources=%v confidence=%.2f origin=%s effective=%s effective_sources=%v effective_confidence=%.2f effective_origin=%s",
		planning.Decision.Plan.String(), planning.Decision.Plan.SourceNames(), planning.Decision.Confidence, planning.Decision.Origin,
		planning.Effective.Plan.String(), planning.Effective.Plan.SourceNames(), planning.Effective.Confidence, planning.Effective.Origin,
	)
	evidencePlan := planning.Effective.Plan
	webUnavailable := evidencePlan.Has(domain.Web) && !svc.cfg.WebSearchEnabled
	if webUnavailable {
		log.WarnfCtx(runCtx, "[qa] retrieval source unavailable: web")
	}
	stepRecorder, _ := run.(preparationStepRecorder)
	evidence, err := svc.prepareEvidence(
		runCtx, prepared, evidencePlan, webUnavailable, stepRecorder,
	)
	if err != nil {
		prepared.finishPreparationFailure(runCtx, run, err)
		return nil, err
	}
	conversation = svc.prepareAnswerConversation(
		runCtx, conversation, evidence.recalled, prepared.request.RolePrompt, evidence.retrieved,
	)
	conversation, err = svc.compactBeforeAnswer(
		runCtx, prepared, conversation, evidence.retrieved, evidencePlan,
	)
	if err != nil {
		prepared.finishPreparationFailure(runCtx, run, err)
		return nil, err
	}
	return svc.submitRun(
		runCtx, run, prepared.request, definition, selection, prepared.request.Question,
		conversation, prepared.request.UserID, evidence.retrieved,
		prepared.request.RunID, evidencePlan, prepared.toolPolicy,
		prepared.candidateToolSet, prepared.trace, prepared.ownsTrace,
	)
}

func (svc *QA) prepareRunConversation(prepared *qaPreparation) ConversationContext {
	conversation := prepared.request.Conversation
	routedToolIDs := prepared.planning.RoutedToolIDs
	if len(routedToolIDs) > 0 {
		conversation.Instructions = append(conversation.Instructions, llm.Message{
			Role: "system", Content: preferredToolsInstruction(routedToolIDs),
		})
	}
	conversation.FullInvestigation = routedToolsNeedFullInvestigation(
		prepared.toolCandidates, routedToolIDs,
	)
	// Dry-run pruning still records the potential saving while keeping all tools visible.
	if decidePrune(prepared.planning.PlanningError, prepared.planning.Effective) {
		conversation.PrunedToolIDs = svc.prunedToolIDSet(
			prepared.candidateToolSet.Tools(), routedToolIDs,
		)
		conversation.PruneApplied = svc.toolPruningEnabled
	}
	return conversation
}

func (svc *QA) resolveAgentDefinition(
	prepared *qaPreparation,
) (agentapi.Definition, agentapi.DefinitionSelection, error) {
	agentRef := prepared.request.Agent
	if agentRef.ID == "" {
		agentRef = svc.agentRef
	}
	if svc.definitionErr != nil {
		return agentapi.Definition{}, agentapi.DefinitionSelection{},
			fmt.Errorf("resolve agent definition %q: %w", agentRef.ID, svc.definitionErr)
	}
	if svc.definitions == nil {
		return agentapi.Definition{}, agentapi.DefinitionSelection{}, nil
	}
	if resolver, ok := svc.definitions.(DefinitionSelectionResolver); ok {
		stableKey := catalog.StableSelectionKey(
			prepared.request.UserID, prepared.request.Conversation.SessionID,
		)
		definition, selection, err := resolver.ResolveFor(agentRef, stableKey)
		if err != nil {
			return agentapi.Definition{}, agentapi.DefinitionSelection{},
				fmt.Errorf("resolve agent definition %q: %w", agentRef.ID, err)
		}
		return definition, selection, nil
	}
	definition, err := svc.definitions.Resolve(agentRef)
	if err != nil {
		return agentapi.Definition{}, agentapi.DefinitionSelection{},
			fmt.Errorf("resolve agent definition %q: %w", agentRef.ID, err)
	}
	return definition, agentapi.DefinitionSelection{}, nil
}

func (svc *QA) beginSingleAgentRun(
	prepared *qaPreparation,
	definition agentapi.Definition,
	selection agentapi.DefinitionSelection,
) (agentapi.ManagedRun, error) {
	run, err := svc.runtime.Begin(prepared.ctx, agentapi.RunStart{
		RunID: prepared.request.RunID,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
		DefinitionHash: definition.ContentHash,
		Selection:      selection,
		Input:          qaRunInput(prepared.request.Question),
		Permissions:    qaRunPermissions(prepared.toolPolicy.AllowWrite),
		ToolScope: agentapi.ToolScope{
			AllowWrite:      prepared.toolPolicy.AllowWrite,
			RestrictVisible: true,
			VisibleToolIDs:  scenarioToolIDs(prepared.candidateToolSet.Tools()),
		},
		Actor: agentapi.Actor{UserID: prepared.request.UserID},
		Correlation: agentapi.Correlation{
			SessionID:     prepared.request.Conversation.SessionID,
			ParentRunID:   prepared.request.ParentRunID,
			WorkflowRunID: prepared.request.WorkflowRunID,
			NodeID:        prepared.request.WorkflowNodeID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("begin QA run %q: %w", prepared.request.RunID, err)
	}
	return run, nil
}

func (svc *QA) prepareEvidence(
	ctx context.Context,
	prepared *qaPreparation,
	evidencePlan domain.EvidencePlan,
	webUnavailable bool,
	stepRecorder preparationStepRecorder,
) (*qaEvidence, error) {
	prefetched, err := svc.executePrefetch(
		ctx,
		prepared.request.RunID,
		prepared.candidateToolSet,
		prepared.request.ToolPlan,
		stepRecorder,
	)
	if err != nil {
		return nil, err
	}
	preloadedContext := make([]ContextBlock, 0, len(prefetched)+len(prepared.request.PreloadedContext))
	preloadedContext = append(preloadedContext, prefetched...)
	preloadedContext = append(preloadedContext, prepared.request.PreloadedContext...)

	recalled, memoryUnavailable := svc.recallMemory(
		ctx, prepared.request.UserID, prepared.request.Question, evidencePlan,
	)
	if memoryUnavailable != "" {
		preloadedContext = append(preloadedContext, unavailableToolBlock("memory", memoryUnavailable))
	}

	q := prepared.planning.CleanQuestion
	if q == "" {
		q = prepared.request.Question
	}
	retrievalTerms := strings.TrimSpace(strings.Join(prepared.planning.Terms.DomainTerms, " "))
	canonicalQuery, _ := rewriteQuery(ctx, queryRewriteInput{
		CleanQuestion: q, ContextTerms: retrievalTerms,
	})
	if canonicalQuery != q {
		log.InfofCtx(ctx, "[qa] retrieval query: augmented with grounded terms (%d chars)", len(retrievalTerms))
	}
	if !evidencePlan.Has(domain.Internal) {
		rc, _ := skipRetrieval(ctx, retrievalDispatchInput{
			Question: prepared.request.Question, Plan: evidencePlan, Blocks: preloadedContext,
			Budget: svc.contextBudget(), WebDown: webUnavailable,
		})
		return &qaEvidence{retrieved: rc, recalled: recalled}, nil
	}

	svc.emitStep(prepared.request.RunID, "好嘞，关键词到手了，我去查一下资料~ 📚")
	rc, err := svc.retriever.RetrievePlan(
		ctx, canonicalQuery, prepared.request.Question, prepared.planning.Terms, evidencePlan,
	)
	if err != nil {
		runErr := fmt.Errorf("retrieve internal evidence: %w", err)
		log.ErrorfCtx(ctx, "[qa] agent pre-retrieve error: %v", runErr)
		return nil, runErr
	}
	rc.OriginalQuestion = prepared.request.Question
	mergePreloadedContext(rc, preloadedContext, svc.contextBudget())
	appendUnavailableWeb(rc, webUnavailable)
	log.InfofCtx(ctx, "[qa] agent pre-retrieve done: hitCount=%d contextLen=%d,question=%v",
		rc.HitCount, len(rc.Text), prepared.request.Question,
	)
	if len(rc.References) > 0 {
		refStrs := make([]string, 0, len(rc.References))
		for _, reference := range rc.References {
			refStrs = append(refStrs, fmt.Sprintf("%s:%s", reference.Type, reference.Target))
		}
		log.InfofCtx(ctx, "[qa] pre-retrieve refs: %s",
			platform.TruncateForLog(strings.Join(refStrs, " | "), 800),
		)
	}
	log.InfofCtx(ctx, "[qa] pre-retrieve context:\n%s",
		platform.TruncateForLog(rc.Text, 4000),
	)
	return &qaEvidence{retrieved: rc, recalled: recalled}, nil
}

func (svc *QA) recallMemory(
	ctx context.Context,
	userID int64,
	question string,
	evidencePlan domain.EvidencePlan,
) ([]memory.MemoryRecord, string) {
	if !evidencePlan.Has(domain.Memory) {
		return nil, ""
	}
	memoryInput := &memoryRecallInput{
		Store: svc.memory, UserID: userID, Query: question, Limit: 3,
	}
	memoryResult, _ := executiontrace.Invoke(ctx, memoryRecallSpec, memoryInput, func(
		ctx context.Context, input *memoryRecallInput,
	) (memoryRecallOutput, error) {
		if input.Store == nil || !input.Store.Enabled() || input.UserID == 0 {
			return memoryRecallOutput{
				Status: "unavailable",
				Error:  "memory capability not configured for this user",
			}, nil
		}
		recall, err := input.Store.Recall(ctx, input.UserID, input.Query, input.Limit)
		if err != nil {
			return memoryRecallOutput{Status: "failed", Error: err.Error()}, nil
		}
		input.TemporalIntent = recall.Intent
		return memoryRecallOutput{Result: recall, Status: "completed"}, nil
	})
	switch memoryResult.Status {
	case "unavailable":
		log.WarnfCtx(ctx, "[qa] evidence source unavailable: memory")
		return nil, memoryResult.Error
	case "failed":
		reason := "memory recall failed: " + memoryResult.Error
		log.ErrorfCtx(ctx, "[qa] memory recall error: %s", memoryResult.Error)
		return nil, reason
	default:
		return memoryResult.Result.Records, ""
	}
}
