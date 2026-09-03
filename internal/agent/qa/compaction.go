package qa

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// compactAnswer runs after prefetch, memory recall, and evidence retrieval so
// the decision includes the request-scoped prompt that will reach the answer model.
func (svc *Service) compactAnswer(
	ctx context.Context,
	prepared *preparation,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	contextWindow int,
	outputReserve int,
) (ConversationContext, error) {
	if contextWindow <= 0 {
		return conversation, nil
	}
	incomingTokens, projectedTokens, err := svc.compactionProjectionFor(
		prepared, conversation, rc, plan, contextWindow, outputReserve,
	)
	if err != nil {
		return conversation, fmt.Errorf("estimate session compaction context: %w", err)
	}
	result := session.CompactionResult{
		ProjectedBeforeTokens: projectedTokens,
		ProjectedAfterTokens:  projectedTokens,
		ContextWindow:         contextWindow,
		HighWaterTokens:       run.ContextHighWaterTokens(contextWindow),
		SafetyTokens:          run.ContextSafetyTokens(contextWindow),
		SafeLimitTokens:       run.ContextSafeLimitTokens(contextWindow),
		OutputReserveTokens:   outputReserve,
	}
	defer func() {
		svc.emitContextUsage(prepared.request.RunID, usageFromCompaction(result))
	}()
	if projectedTokens < result.HighWaterTokens ||
		svc.sessions == nil || svc.helperLLM == nil || conversation.SessionID == "" {
		return conversation, nil
	}
	result, started, fromTurn, toTurn, err := svc.runAnswerCompaction(
		ctx, prepared, conversation, incomingTokens, projectedTokens, contextWindow, outputReserve,
	)
	if err != nil {
		if started {
			svc.updateCompaction(
				prepared.request.RunID, conversation.SessionID, "failed", "历史上下文压缩失败",
				fromTurn, toTurn,
			)
		}
		return conversation, err
	}
	if result.Applied || result.Stale {
		refreshed, refreshErr := svc.refreshConversation(
			ctx, prepared, conversation, contextWindow, outputReserve,
		)
		if refreshErr != nil {
			if started {
				svc.updateCompaction(
					prepared.request.RunID, conversation.SessionID, "failed", "历史上下文压缩失败",
					fromTurn, toTurn,
				)
			}
			return conversation, refreshErr
		}
		conversation = refreshed
		_, projectedAfter, projectionErr := svc.compactionProjectionFor(
			prepared, conversation, rc, plan, contextWindow, outputReserve,
		)
		if projectionErr != nil {
			return conversation, fmt.Errorf("estimate compacted session context: %w", projectionErr)
		}
		result.ProjectedAfterTokens = projectedAfter
	}
	svc.reportAnswerCompaction(ctx, prepared.request.RunID, conversation.SessionID, result, started)
	return conversation, nil
}

func (svc *Service) compactionProjectionFor(
	prepared *preparation,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	contextWindow int,
	outputReserve int,
) (int, int, error) {
	tools := sessionCompactionTools(prepared.candidateToolSet, conversation)
	return compactionProjection(
		prepared.request.Question, prepared.analysis.QueryPlan, conversation, rc, plan,
		svc.domainKnowledge, tools, outputReserve,
	)
}

func (svc *Service) runAnswerCompaction(
	ctx context.Context,
	prepared *preparation,
	conversation ConversationContext,
	incomingTokens, projectedTokens, contextWindow, outputReserve int,
) (session.CompactionResult, bool, int, int, error) {
	started := false
	fromTurn, toTurn := 0, 0
	result, err := session.CompactIfNeeded(
		ctx, svc.helperLLM, svc.sessions, conversation.SessionID, prepared.request.UserID,
		session.CompactionUsage{
			ContextWindow:       contextWindow,
			IncomingTokens:      incomingTokens,
			ProjectedTokens:     projectedTokens,
			OutputReserveTokens: outputReserve,
		},
		"",
		func(from, to int) {
			started = true
			fromTurn, toTurn = from, to
			svc.updateCompaction(
				prepared.request.RunID, conversation.SessionID, "start",
				fmt.Sprintf("正在压缩第 %d–%d 轮历史上下文…", from, to),
				from, to,
			)
		},
		svc.history,
	)
	if err != nil {
		return result, started, fromTurn, toTurn,
			fmt.Errorf("compact session %q after retrieval: %w", conversation.SessionID, err)
	}
	return result, started, fromTurn, toTurn, nil
}

func (svc *Service) reportAnswerCompaction(
	ctx context.Context,
	runID, sessionID string,
	result session.CompactionResult,
	started bool,
) {
	if result.Applied {
		svc.updateCompaction(
			runID, sessionID, "done", "历史上下文压缩完成",
			result.FromTurn, result.ToTurn,
		)
		log.InfofCtx(ctx, "[qa] compacted session %s turns %d-%d after retrieval",
			sessionID, result.FromTurn, result.ToTurn)
	} else if result.Stale && started {
		svc.updateCompaction(
			runID, sessionID, "done", "历史上下文压缩完成",
			result.FromTurn, result.ToTurn,
		)
		log.InfofCtx(ctx, "[qa] ignored stale post-retrieval compaction for session %s through turn %d",
			sessionID, result.ToTurn)
	}
}

func compactionProjection(
	question string,
	query domain.QueryPlan,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	domainKnowledge string,
	tools []tool.Tool,
	outputReserve int,
) (int, int, error) {
	definitions := execution.ToolDefinitions(tools)
	projectedInput, err := execution.EstimateInputTokens(buildAgentMessages(
		question, query, conversation, rc, plan, domainKnowledge, 0,
	), definitions)
	if err != nil {
		return 0, 0, err
	}
	withoutSessionHistory := conversation
	withoutSessionHistory.Recent = nil
	withoutSessionHistory.RecentTurns = nil
	withoutSessionHistory.RecentDialogue = nil
	withoutSessionHistory.HistoricalContext = ""
	incomingTokens, err := execution.EstimateInputTokens(buildAgentMessages(
		question, query, withoutSessionHistory, rc, plan, domainKnowledge, 0,
	), definitions)
	if err != nil {
		return 0, 0, err
	}
	return incomingTokens, projectedInput + max(0, outputReserve), nil
}

func sessionCompactionTools(prepared ScenarioToolSet, conversation ConversationContext) []tool.Tool {
	if prepared == nil {
		return nil
	}
	tools := prepared.Tools()
	if !conversation.PruneApplied {
		return tools
	}
	selected := make([]tool.Tool, 0, len(conversation.PrunedToolIDs))
	for _, candidate := range tools {
		if _, ok := conversation.PrunedToolIDs[candidate.ID]; ok {
			selected = append(selected, candidate)
		}
	}
	return selected
}

func usageFromCompaction(result session.CompactionResult) ContextUsageEvent {
	return ContextUsageEvent{
		Phase:                 "session_pre_answer",
		ProjectedBeforeTokens: result.ProjectedBeforeTokens,
		ProjectedAfterTokens:  result.ProjectedAfterTokens,
		ContextWindow:         result.ContextWindow,
		HighWaterTokens:       result.HighWaterTokens,
		SafetyTokens:          result.SafetyTokens,
		SafeLimitTokens:       result.SafeLimitTokens,
		OutputReserveTokens:   result.OutputReserveTokens,
		CompactionTriggered:   result.Triggered,
		CompactionApplied:     result.Applied,
	}
}

func (svc *Service) refreshConversation(
	ctx context.Context,
	prepared *preparation,
	conversation ConversationContext,
	contextWindow int,
	outputReserve int,
) (ConversationContext, error) {
	session, err := svc.sessions.GetContextSnapshot(
		conversation.SessionID, prepared.request.UserID,
		memory.RecentTurnMetadataLimit, memory.RecentDialogueTurnLimit,
	)
	if err != nil {
		return ConversationContext{}, fmt.Errorf("reload compacted session %q: %w", conversation.SessionID, err)
	}
	if session == nil {
		return conversation, nil
	}
	conversation.SessionTitle = session.Title
	conversation.CompactedThroughTurn = session.CompactedThroughTurn
	conversation.RecentTurns = session.RecentTurns
	conversation.RecentDialogue = session.RecentDialogue
	conversation.Recent = nil
	conversation.RetrievedHistory = ""
	conversation.HistoricalContext = ""
	assembled, err := svc.assembleContext(ctx, contextAssembleInput{
		Question:      prepared.request.Question,
		UserID:        prepared.request.UserID,
		Conversation:  conversation,
		Relation:      prepared.analysis.History,
		Origin:        prepared.analysis.HistoryOrigin,
		Upgrade:       prepared.analysis.HistoryUpdate,
		ContextWindow: contextWindow,
		OutputReserve: outputReserve,
	})
	if err != nil {
		return ConversationContext{}, fmt.Errorf("assemble compacted session %q: %w", conversation.SessionID, err)
	}
	return assembled.Conversation, nil
}
