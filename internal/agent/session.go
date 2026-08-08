package agent

import (
	"context"
	"encoding/json"

	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

// SessionHistory is the bounded current-session archive capability consumed by QA.
type SessionHistory = agentsession.SessionHistory

// SessionCompactionUsage contains the observable budget inputs for one decision.
type SessionCompactionUsage = agentsession.SessionCompactionUsage

// SessionCompactionResult reports whether the monotonic archive boundary advanced.
type SessionCompactionResult = agentsession.SessionCompactionResult

// CompactSessionIfNeeded preserves the historical agent package API.
func CompactSessionIfNeeded(
	ctx context.Context,
	client *llm.LLMClient,
	sessions *memory.SessionStore,
	sessionID string,
	userID int64,
	usage SessionCompactionUsage,
	incomingText string,
	onStart func(fromTurn, toTurn int),
	histories ...SessionHistory,
) (SessionCompactionResult, error) {
	return agentsession.CompactSessionIfNeeded(
		ctx, client, sessions, sessionID, userID, usage, incomingText, onStart, histories...,
	)
}

// ArchiveSessionHistoryIfNeeded preserves the historical agent package API.
func ArchiveSessionHistoryIfNeeded(
	ctx context.Context,
	client *llm.LLMClient,
	sessions *memory.SessionStore,
	sessionID string,
	userID int64,
	contextWindow int,
	histories ...SessionHistory,
) (SessionCompactionResult, error) {
	return agentsession.ArchiveSessionHistoryIfNeeded(
		ctx, client, sessions, sessionID, userID, contextWindow, histories...,
	)
}

// GenerateTurnCompactionSummaries preserves the historical agent package API.
func GenerateTurnCompactionSummaries(
	ctx context.Context,
	client *llm.LLMClient,
	records []memory.TurnContextRecord,
) (map[string]string, error) {
	return agentsession.GenerateTurnCompactionSummaries(ctx, client, records)
}

func compressTurnDetail(turnNumber int, messages []llm.Message) (json.RawMessage, error) {
	return agentsession.CompressTurnDetail(turnNumber, messages)
}

func withSessionToolScope(
	ctx context.Context,
	conversation ConversationContext,
	userID int64,
) context.Context {
	return agentsession.WithToolScope(
		ctx,
		conversation.SessionID,
		conversation.CompactedThroughTurn,
		userID,
	)
}
