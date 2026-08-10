package qa

import (
	"context"
	"fmt"

	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// compactBeforeAnswer runs after prefetch, memory recall, and evidence retrieval so
// the decision includes the request-scoped prompt that will reach the answer model.
func (svc *QA) compactBeforeAnswer(
	ctx context.Context,
	prepared *qaPreparation,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	contextWindow int,
	outputReserve int,
) (ConversationContext, error) {
	if contextWindow <= 0 {
		return conversation, nil
	}

	tools := sessionCompactionTools(prepared.candidateToolSet, conversation)
	incomingTokens, projectedTokens, err := sessionCompactionTokenProjection(
		prepared.request.Question, conversation, rc, plan, svc.domainKnowledge,
		tools, outputReserve,
	)
	if err != nil {
		return conversation, fmt.Errorf("estimate session compaction context: %w", err)
	}
	result := agentsession.SessionCompactionResult{
		ProjectedBeforeTokens: projectedTokens,
		ProjectedAfterTokens:  projectedTokens,
		ContextWindow:         contextWindow,
		HighWaterTokens:       agentrun.ContextHighWaterTokens(contextWindow),
		SafetyTokens:          agentrun.ContextSafetyTokens(contextWindow),
		SafeLimitTokens:       agentrun.ContextSafeLimitTokens(contextWindow),
		OutputReserveTokens:   outputReserve,
	}
	defer func() {
		svc.emitContextUsage(prepared.request.RunID, contextUsageFromSessionCompaction(result))
	}()
	if projectedTokens < result.HighWaterTokens ||
		svc.sessions == nil || svc.helperLLM == nil || conversation.SessionID == "" {
		return conversation, nil
	}

	started := false
	fromTurn, toTurn := 0, 0
	result, err = agentsession.CompactSessionIfNeeded(
		ctx, svc.helperLLM, svc.sessions, conversation.SessionID, prepared.request.UserID,
		agentsession.SessionCompactionUsage{
			ContextWindow:       contextWindow,
			IncomingTokens:      incomingTokens,
			ProjectedTokens:     projectedTokens,
			OutputReserveTokens: outputReserve,
		},
		"",
		func(from, to int) {
			started = true
			fromTurn, toTurn = from, to
			svc.updateSessionCompaction(
				prepared.request.RunID, conversation.SessionID, "start",
				fmt.Sprintf("正在压缩第 %d–%d 轮历史上下文…", from, to),
				from, to,
			)
		},
		svc.history,
	)
	if err != nil {
		if started {
			svc.updateSessionCompaction(
				prepared.request.RunID, conversation.SessionID, "failed", "历史上下文压缩失败",
				fromTurn, toTurn,
			)
		}
		return conversation, fmt.Errorf("compact session %q after retrieval: %w", conversation.SessionID, err)
	}
	if result.Applied || result.Stale {
		refreshed, refreshErr := svc.refreshCompactedConversation(
			ctx, prepared, conversation, contextWindow, outputReserve,
		)
		if refreshErr != nil {
			if started {
				svc.updateSessionCompaction(
					prepared.request.RunID, conversation.SessionID, "failed", "历史上下文压缩失败",
					fromTurn, toTurn,
				)
			}
			return conversation, refreshErr
		}
		conversation = refreshed
		_, projectedAfter, projectionErr := sessionCompactionTokenProjection(
			prepared.request.Question, conversation, rc, plan, svc.domainKnowledge,
			sessionCompactionTools(prepared.candidateToolSet, conversation), outputReserve,
		)
		if projectionErr != nil {
			return conversation, fmt.Errorf("estimate compacted session context: %w", projectionErr)
		}
		result.ProjectedAfterTokens = projectedAfter
	}
	if result.Applied {
		svc.updateSessionCompaction(
			prepared.request.RunID, conversation.SessionID, "done", "历史上下文压缩完成",
			result.FromTurn, result.ToTurn,
		)
		log.InfofCtx(ctx, "[qa] compacted session %s turns %d-%d after retrieval",
			conversation.SessionID, result.FromTurn, result.ToTurn)
	} else if result.Stale && started {
		svc.updateSessionCompaction(
			prepared.request.RunID, conversation.SessionID, "done", "历史上下文压缩完成",
			result.FromTurn, result.ToTurn,
		)
		log.InfofCtx(ctx, "[qa] ignored stale post-retrieval compaction for session %s through turn %d",
			conversation.SessionID, result.ToTurn)
	}
	return conversation, nil
}

func sessionCompactionTokenProjection(
	question string,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	domainKnowledge string,
	tools []tool.Tool,
	outputReserve int,
) (int, int, error) {
	definitions := agentexecution.ToolDefinitions(tools)
	projectedInput, err := agentexecution.EstimateInputTokens(buildAgentMessages(
		question, conversation, rc, plan, domainKnowledge, 0,
	), definitions)
	if err != nil {
		return 0, 0, err
	}
	withoutSessionHistory := conversation
	withoutSessionHistory.Recent = nil
	withoutSessionHistory.RecentTurns = nil
	withoutSessionHistory.RecentDialogue = nil
	withoutSessionHistory.HistoricalContext = ""
	incomingTokens, err := agentexecution.EstimateInputTokens(buildAgentMessages(
		question, withoutSessionHistory, rc, plan, domainKnowledge, 0,
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

func contextUsageFromSessionCompaction(result agentsession.SessionCompactionResult) ContextUsageEvent {
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

func (svc *QA) refreshCompactedConversation(
	ctx context.Context,
	prepared *qaPreparation,
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
