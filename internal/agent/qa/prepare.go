package qa

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

type preparation struct {
	request            Request
	sourceConversation ConversationContext
	ctx                context.Context
	trace              *runtrace.Scope
	ownsTrace          bool
	toolPolicy         ToolPolicy
	candidateToolSet   ScenarioToolSet
	toolCandidates     []retrieval.ToolRouteCandidate
	planning           evidencePlanningOutput
	analysis           queryAnalysisOutput
	execution          executionRouteDecision
	historyCandidates  *HistoryCandidates
	runLimits          agentapi.RunLimits
	definition         agentapi.Definition
	selection          agentapi.DefinitionSelection
	requestDeadline    time.Time
	requestCancel      context.CancelFunc
}

type preparedEvidence struct {
	retrieved *retrieval.RetrievedContext
	recalled  []memory.MemoryRecord
}

func (prepared *preparation) closeTrace() {
	if prepared.requestCancel != nil {
		prepared.requestCancel()
		prepared.requestCancel = nil
	}
	if !prepared.ownsTrace {
		return
	}
	prepared.trace.Close()
	prepared.ownsTrace = false
}

func (prepared *preparation) failPreparation(
	ctx context.Context,
	run agentapi.ManagedRun,
	err error,
) {
	if finishErr := run.Finish(&agentapi.RunError{
		Code: "preparation_failed", Message: err.Error(),
	}); finishErr != nil {
		log.ErrorfCtx(ctx, "[qa] finish failed preparation %s: %v", prepared.request.RunID, finishErr)
	}
}

func (svc *Service) prepare(ctx context.Context, request Request, requestStartedAt time.Time) (*preparation, error) {
	prepared, err := svc.initializePreparation(ctx, request, requestStartedAt)
	if err != nil {
		return nil, err
	}

	historyDiscovery := startHistoryDiscovery(
		prepared.ctx, svc.history, prepared.request.UserID,
		prepared.request.Conversation, prepared.request.Question,
	)
	if historyDiscovery != nil {
		defer historyDiscovery.cancel()
	}

	anchor, err := svc.planQuestion(prepared)
	if err != nil {
		prepared.closeTrace()
		return nil, err
	}
	if err := svc.analyzeQuestion(prepared, anchor); err != nil {
		prepared.closeTrace()
		return nil, err
	}
	if err := svc.prepareConversation(prepared, historyDiscovery); err != nil {
		prepared.closeTrace()
		return nil, err
	}

	svc.applyTimeConstraint(prepared)
	svc.applyExecutionRoute(prepared)
	return prepared, nil
}

func (svc *Service) initializePreparation(
	ctx context.Context,
	request Request,
	requestStartedAt time.Time,
) (*preparation, error) {
	request, err := normalizeRequest(request)
	if err != nil {
		return nil, err
	}

	trace, ownsTrace := beginExecutionTrace(ctx)
	ctx = runtrace.WithScope(ctx, trace)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: request.RunID, ParentRunID: request.ParentRunID,
	})
	prepared := &preparation{
		request: request, sourceConversation: request.Conversation,
		ctx: ctx, trace: trace, ownsTrace: ownsTrace,
	}
	definition, selection, err := svc.resolveAgentDefinition(prepared)
	if err != nil {
		prepared.closeTrace()
		return nil, err
	}
	prepared.definition = definition
	prepared.selection = selection
	if definition.Budget.Timeout > 0 {
		deadline := requestStartedAt.Add(definition.Budget.Timeout)
		if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
			deadline = existing
		}
		prepared.ctx, prepared.requestCancel = context.WithDeadline(ctx, deadline)
		prepared.requestDeadline = deadline
	}

	prepared.toolPolicy = toolPolicyForRun(
		svc.writeAvailable.Load() && request.WriteAuthorized && request.WriteRequested,
	)
	prepared.candidateToolSet = svc.runtimeTools.ToolsFor(prepared.toolPolicy)
	if request.Conversation.CompactedThroughTurn <= 0 || svc.history == nil {
		prepared.candidateToolSet = withoutHistoryTools(prepared.candidateToolSet)
	}
	prepared.toolCandidates = routingCandidates(prepared.candidateToolSet.Tools())
	return prepared, nil
}

func (svc *Service) planQuestion(prepared *preparation) (time.Time, error) {
	request := prepared.request
	svc.emitStatus(request.RunID, "嗯...让我先琢磨一下你在问什么 ✨", "prepare.planning", time.Time{})
	routeContext := buildHistoryContext(request.Conversation)
	if routeContext == "" {
		routeContext = buildRagCtx(request.Conversation.Recent)
	}

	requestAnchor := time.Now()
	hctx, cancelPlanning := context.WithTimeout(
		llm.WithUsagePhase(prepared.ctx, llm.PhaseRoute), helperTimeout,
	)
	planning, err := svc.planEvidence(hctx, evidencePlanningInput{
		Question: request.Question, RouteContext: routeContext, ExplicitPlan: request.EvidencePlan,
		ToolCandidates: prepared.toolCandidates,
		AvailableTools: scenarioToolIDs(prepared.candidateToolSet.Tools()),
		UserID:         request.UserID,
	})
	cancelPlanning()
	if err != nil {
		return requestAnchor, fmt.Errorf("plan QA evidence: %w", err)
	}
	prepared.planning = planning
	svc.emitStatus(request.RunID, "问题规划完成，正在整理会话上下文", "prepare.history", time.Time{})
	return requestAnchor, nil
}

func (svc *Service) analyzeQuestion(
	prepared *preparation,
	requestAnchor time.Time,
) error {
	request := prepared.request
	analysis, err := analyzeQuery(prepared.ctx, queryAnalysisInput{
		Question: request.Question, CleanQuestion: prepared.planning.CleanQuestion,
		Terms: prepared.planning.Terms, QuerySemantics: prepared.planning.QuerySemantics,
		Time:   prepared.planning.Time,
		Anchor: requestAnchor, RecentTurns: request.Conversation.RecentTurns,
		History: prepared.planning.History, HistoryValid: prepared.planning.HistoryValid,
	})
	if err != nil {
		return fmt.Errorf("analyze QA query: %w", err)
	}
	prepared.analysis = analysis
	requiredFacets := domain.RequiredFacetsFor(analysis.QueryPlan.Kind)
	log.InfofCtx(prepared.ctx,
		"[qa] canonical query plan kind=%s required_facets=%d entities=%d",
		analysis.QueryPlan.Kind, len(requiredFacets), len(analysis.QueryPlan.Entities),
	)
	return nil
}

func (svc *Service) prepareConversation(
	prepared *preparation,
	historyDiscovery *historyDiscoveryTask,
) error {
	historyStarted := time.Now()
	prepared.historyCandidates = resolveCandidates(
		prepared.ctx, historyDiscovery, prepared.analysis.History,
	)
	request := prepared.request
	assembled, err := svc.assembleContext(prepared.ctx, contextAssembleInput{
		Question: request.Question, UserID: request.UserID, Conversation: request.Conversation,
		Relation: prepared.analysis.History, Origin: prepared.analysis.HistoryOrigin,
		Upgrade:    prepared.analysis.HistoryUpdate,
		Candidates: prepared.historyCandidates,
	})
	if err != nil {
		return err
	}
	svc.emitStatus(request.RunID, "上下文整理完成，正在准备检索", "prepare.routing", historyStarted)
	prepared.request.Conversation = assembled.Conversation
	return nil
}

func (svc *Service) applyTimeConstraint(prepared *preparation) {
	if prepared.analysis.TimeError != nil {
		log.WarnfCtx(prepared.ctx, "[qa] resolve relative time degraded: %v", prepared.analysis.TimeError)
		return
	}
	if !prepared.analysis.HasTimeRange {
		return
	}

	prepared.ctx = tool.WithTimeRange(prepared.ctx, prepared.analysis.TimeRange)
	log.InfofCtx(prepared.ctx, "[qa] relative time resolved raw=%q from=%s to=%s",
		prepared.analysis.TimeRange.Raw,
		prepared.analysis.TimeRange.From.Format(time.RFC3339),
		prepared.analysis.TimeRange.To.Format(time.RFC3339),
	)
}

func normalizeRequest(request Request) (Request, error) {
	request.Question = strings.TrimSpace(request.Question)
	if request.Question == "" {
		return Request{}, fmt.Errorf("question is required")
	}

	request.Conversation.Instructions = append(
		[]llm.Message(nil), request.Conversation.Instructions...,
	)
	if request.RunID == "" {
		request.RunID = NewRunID()
	}
	return request, nil
}

func (svc *Service) prepareSingleRun(prepared *preparation) (*AskResult, error) {
	definition, selection := prepared.definition, prepared.selection
	contextWindow, outputReserve := svc.contextLimits(
		definition.Budget.ContextTokens, definition.Model.MaxOutputTokens,
	)
	if contextWindow != svc.contextWindow || outputReserve != svc.outputReserve {
		if err := svc.reassembleConversation(
			prepared.ctx, prepared, contextWindow, outputReserve,
		); err != nil {
			return nil, err
		}
	}
	prepared.runLimits = svc.parentRunLimits(prepared, definition)
	run, err := svc.beginSingleRun(prepared, definition, selection)
	if err != nil {
		return nil, err
	}
	runCtx := run.Context(prepared.ctx)

	conversation := svc.prepareRunConversation(prepared)
	stepRecorder, _ := run.(preparationStepRecorder)
	admitted, err := svc.acquireEvidence(runCtx, prepared, stepRecorder, run)
	if err != nil {
		prepared.failPreparation(runCtx, run, err)
		return nil, err
	}
	conversation = svc.answerContext(
		runCtx, conversation, admitted.Recalled, prepared.request.RolePrompt, admitted.Retrieved,
	)
	conversation, err = svc.compactAnswer(
		runCtx, prepared, conversation, admitted.Retrieved, admitted.Plan,
		contextWindow, outputReserve,
	)
	if err != nil {
		prepared.failPreparation(runCtx, run, err)
		return nil, err
	}
	return svc.submitRun(
		runCtx, run, prepared.request, definition, selection, prepared.request.Question,
		conversation, prepared.request.UserID, admitted.Retrieved,
		prepared.request.RunID, prepared.analysis.QueryPlan, admitted.Plan, prepared.toolPolicy,
		prepared.candidateToolSet, prepared.execution.HighRisk, prepared.runLimits,
		prepared.trace, prepared.ownsTrace, prepared.requestCancel,
	)
}

func (svc *Service) parentRunLimits(
	prepared *preparation,
	definition agentapi.Definition,
) agentapi.RunLimits {
	deadline := prepared.requestDeadline
	if deadline.IsZero() {
		deadline = time.Now().UTC().Add(definition.Budget.Timeout)
	}
	limits := agentapi.RunLimits{
		Deadline:     deadline,
		MaxSteps:     definition.Budget.MaxSteps,
		MaxToolCalls: definition.Budget.MaxToolCalls,
	}
	if svc.delegationEnabled {
		limits.MaxTotalTokens = svc.delegationBudget.MaxTotalTokens
		limits.MaxCostMicros = svc.delegationBudget.MaxCostMicros
		limits.ParentAnswerReserve = svc.delegationBudget.ParentAnswerReserve
	}
	return limits
}

func (svc *Service) reassembleConversation(
	ctx context.Context,
	prepared *preparation,
	contextWindow int,
	outputReserve int,
) error {
	assembled, err := svc.assembleContext(ctx, contextAssembleInput{
		Question:      prepared.request.Question,
		UserID:        prepared.request.UserID,
		Conversation:  prepared.sourceConversation,
		Relation:      prepared.analysis.History,
		Origin:        prepared.analysis.HistoryOrigin,
		Upgrade:       prepared.analysis.HistoryUpdate,
		Candidates:    prepared.historyCandidates,
		ContextWindow: contextWindow,
		OutputReserve: outputReserve,
	})
	if err != nil {
		return fmt.Errorf("reassemble context for agent definition: %w", err)
	}
	prepared.request.Conversation = assembled.Conversation
	return nil
}

func (svc *Service) prepareRunConversation(prepared *preparation) ConversationContext {
	conversation := prepared.request.Conversation
	routedToolIDs := prepared.planning.RoutedToolIDs
	delegation := parentDelegationInstruction(prepared)
	if len(routedToolIDs) > 0 {
		conversation.Instructions = append(conversation.Instructions, llm.Message{
			Role: "system", Content: preferenceInstruction(routedToolIDs, delegation != ""),
		})
	}
	if delegation != "" {
		conversation.Instructions = append(conversation.Instructions, llm.Message{
			Role: "system", Content: delegation,
		})
	}
	// Dry-run pruning still records the potential saving while keeping all tools visible.
	if decidePrune(prepared.planning.PlanningError, prepared.planning.Effective) {
		conversation.PrunedToolIDs = svc.prunedToolIDSet(
			prepared.candidateToolSet.Tools(), routedToolIDs,
		)
		conversation.PruneApplied = svc.toolPruningEnabled
	}
	return conversation
}

func (svc *Service) resolveAgentDefinition(
	prepared *preparation,
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
	if resolver, ok := svc.definitions.(SelectionResolver); ok {
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

func (svc *Service) beginSingleRun(
	prepared *preparation,
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
		Input:          runInput(prepared.request.Question),
		Permissions:    runPermissions(prepared.toolPolicy.AllowWrite),
		ToolScope: agentapi.ToolScope{
			AllowWrite:      prepared.toolPolicy.AllowWrite,
			RestrictVisible: true,
			VisibleToolIDs:  scenarioToolIDs(prepared.candidateToolSet.Tools()),
		},
		Policy: agentapi.RunPolicy{
			// The output contract is derived before Begin and is part of the
			// immutable RunStart/RunRequest boundary. Evidence admission fields
			// are intentionally filled only after preparation completes.
			OutputContract: outputContractForQuery(prepared.analysis.QueryPlan),
		},
		Limits: prepared.runLimits,
		Actor:  agentapi.Actor{UserID: prepared.request.UserID},
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

func (svc *Service) prepareEvidence(
	ctx context.Context,
	prepared *preparation,
	evidencePlan domain.EvidencePlan,
	webUnavailable bool,
	stepRecorder preparationStepRecorder,
) (*preparedEvidence, error) {
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
		return &preparedEvidence{retrieved: rc, recalled: recalled}, nil
	}
	if svc.retriever == nil {
		return nil, fmt.Errorf("retrieve internal evidence: retriever is unavailable")
	}

	retrievalStarted := time.Now()
	svc.emitStatus(prepared.request.RunID, "正在准备查询向量和召回资料", "retrieval.embedding", time.Time{})
	ctx = retrieval.WithProgress(ctx, func(event retrieval.ProgressEvent) {
		svc.emitStatusElapsed(
			prepared.request.RunID, event.Text, event.Code, event.ElapsedMS,
		)
	})
	rc, err := svc.retriever.RetrievePlan(
		ctx, canonicalQuery, prepared.planning.Terms, evidencePlan, prepared.analysis.QueryPlan,
	)
	if err != nil {
		runErr := fmt.Errorf("retrieve internal evidence: %w", err)
		log.ErrorfCtx(ctx, "[qa] agent pre-retrieve error: %v", runErr)
		return nil, runErr
	}
	rc.OriginalQuestion = prepared.request.Question
	svc.emitStatus(prepared.request.RunID, "资料查询完成，正在生成答案", "retrieval.ready", retrievalStarted)
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
	return &preparedEvidence{retrieved: rc, recalled: recalled}, nil
}

func (svc *Service) recallMemory(
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
	memoryResult, _ := runtrace.Invoke(ctx, memoryRecallSpec, memoryInput, func(
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
