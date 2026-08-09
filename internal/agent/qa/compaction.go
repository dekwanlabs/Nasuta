package qa

import (
	"context"
	"fmt"

	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
)

// compactBeforeAnswer runs after prefetch, memory recall, and evidence retrieval so
// the decision includes the request-scoped prompt that will reach the answer model.
func (svc *QA) compactBeforeAnswer(
	ctx context.Context,
	prepared *qaPreparation,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
) (ConversationContext, error) {
	if svc.sessions == nil || svc.helperLLM == nil || conversation.SessionID == "" || svc.contextWindow <= 0 {
		return conversation, nil
	}

	started := false
	fromTurn, toTurn := 0, 0
	incomingTokens := sessionCompactionIncomingTokens(
		prepared.request.Question, conversation, rc, plan, svc.domainKnowledge,
	)
	result, err := agentsession.CompactSessionIfNeeded(
		ctx, svc.helperLLM, svc.sessions, conversation.SessionID, prepared.request.UserID,
		agentsession.SessionCompactionUsage{
			ContextWindow:       svc.contextWindow,
			IncomingTokens:      incomingTokens,
			OutputReserveTokens: svc.outputReserve,
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
		refreshed, refreshErr := svc.refreshCompactedConversation(ctx, prepared, conversation)
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

func sessionCompactionIncomingTokens(
	question string,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	domainKnowledge string,
) int {
	withoutSessionHistory := conversation
	withoutSessionHistory.Recent = nil
	withoutSessionHistory.RecentTurns = nil
	withoutSessionHistory.RetrievedHistory = ""
	withoutSessionHistory.HistoricalContext = ""
	return estimateMessagesTokens(buildAgentMessages(
		question, withoutSessionHistory, rc, plan, domainKnowledge, 0,
	))
}

func (svc *QA) refreshCompactedConversation(
	ctx context.Context,
	prepared *qaPreparation,
	conversation ConversationContext,
) (ConversationContext, error) {
	session, err := svc.sessions.GetContextMetadata(
		conversation.SessionID, prepared.request.UserID, memory.RecentTurnMetadataLimit,
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
	conversation.Recent = nil
	conversation.RetrievedHistory = ""
	conversation.HistoricalContext = ""
	assembled, err := svc.assembleContext(ctx, contextAssembleInput{
		Question:     prepared.request.Question,
		UserID:       prepared.request.UserID,
		Conversation: conversation,
		Relation:     prepared.analysis.History,
		Origin:       prepared.analysis.HistoryOrigin,
		Upgrade:      prepared.analysis.HistoryUpdate,
	})
	if err != nil {
		return ConversationContext{}, fmt.Errorf("assemble compacted session %q: %w", conversation.SessionID, err)
	}
	return assembled.Conversation, nil
}
