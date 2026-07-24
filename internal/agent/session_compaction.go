package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	sessionCompactionRatio = 0.8
	sessionRecentTurns     = 3
)

// SessionCompactionResult reports whether a monotonic summary snapshot advanced.
type SessionCompactionResult struct {
	Applied    bool
	Stale      bool
	FromTurn   int
	ToTurn     int
	References []string
}

// CompactSessionIfNeeded preserves three recent turns after the 80% threshold.
func CompactSessionIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, contextWindow, peakContextTokens int, incomingText string,
	onStart func(fromTurn, toTurn int)) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if client == nil || sessions == nil || sessionID == "" || contextWindow <= 0 {
		return result, nil
	}
	uncompactedTokens, compactedThrough, err := sessions.SessionContextStats(sessionID, userID)
	if err != nil {
		return result, fmt.Errorf("measure session context %q: %w", sessionID, err)
	}
	threshold := int(float64(contextWindow) * sessionCompactionRatio)
	projectedInputTokens := peakContextTokens + tooloutput.EstimateTokens(incomingText)
	if compactedThrough == 0 && projectedInputTokens < threshold && uncompactedTokens < threshold {
		return result, nil
	}
	candidate, err := sessions.PrepareCompaction(sessionID, userID, sessionRecentTurns)
	if err != nil {
		return result, fmt.Errorf("prepare session compaction %q: %w", sessionID, err)
	}
	if candidate == nil {
		return result, nil
	}
	if onStart != nil {
		onStart(candidate.FromTurn, candidate.ToTurn)
	}
	records := make([]memory.TurnContextRecord, 0, len(candidate.Turns))
	refs := make([]string, 0, len(candidate.Turns))
	for _, turn := range candidate.Turns {
		ref := turnCompactionRef(sessionID, turn.TurnNumber)
		detail := compressTurnDetail(turn.TurnNumber, turn.Messages)
		records = append(records, memory.TurnContextRecord{
			Ref: ref, SessionID: sessionID, UserID: userID, RunID: turn.RunID,
			Text: detail, TurnNumber: turn.TurnNumber, SourceTokens: turn.SourceTokens,
			RetainedTokens: tooloutput.EstimateTokens(detail),
		})
		refs = append(refs, ref)
	}
	summaries, err := GenerateTurnCompactionSummaries(ctx, client, records)
	if err != nil {
		return result, fmt.Errorf("summarize session %q turns %d-%d: %w",
			sessionID, candidate.FromTurn, candidate.ToTurn, err)
	}
	for i := range records {
		summary := strings.TrimSpace(summaries[records[i].Ref])
		if summary == "" {
			return result, fmt.Errorf("summarize session %q turn %d: empty summary",
				sessionID, records[i].TurnNumber)
		}
		records[i].SummaryText = tooloutput.Truncate(summary, turnSummaryTokenLimit)
	}
	summary := buildRollingSummary(candidate.PreviousSummary, records)
	applied, err := sessions.ApplyCompaction(*candidate, records, summary)
	if err != nil {
		return result, fmt.Errorf("apply session compaction %q: %w", sessionID, err)
	}
	result = SessionCompactionResult{
		Applied: applied, Stale: !applied, FromTurn: candidate.FromTurn,
		ToTurn: candidate.ToTurn, References: refs,
	}
	return result, nil
}

func turnCompactionRef(sessionID string, turnNumber int) string {
	return "cmp_" + platform.UUIDFromString(fmt.Sprintf("%s:%d", sessionID, turnNumber))
}

func buildRollingSummary(previous string, records []memory.TurnContextRecord) string {
	lines := make([]string, 0, len(records)+8)
	for _, line := range strings.Split(previous, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, rollingSummaryInstructionPrefix) {
			lines = append(lines, line)
		}
	}
	for _, record := range records {
		text := strings.Join(strings.Fields(record.SummaryText), " ")
		lines = append(lines, fmt.Sprintf("ref=%s, text=%s", record.Ref, text))
	}
	lines = append(lines, rollingSummaryInstruction)
	return strings.Join(lines, "\n")
}
