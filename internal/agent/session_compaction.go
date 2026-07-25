package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	sessionHighWaterRatio       = 0.80
	sessionSelectionTargetRatio = 0.60
	sessionLowWaterRatio        = 0.65
	sessionCriticalWaterRatio   = 0.95
	sessionRestartSummaryRatio  = 0.30
	sessionRecentTurns          = 3
	summaryItemTokenReserve     = turnSummaryTokenLimit + 64
)

// SessionCompactionUsage contains the observable budget inputs for one decision.
type SessionCompactionUsage struct {
	ContextWindow              int
	PreviousPeakInputTokens    int
	PreviousPeakReservedTokens int
	OutputReserveTokens        int
}

// SessionCompactionResult reports whether a monotonic summary snapshot advanced.
type SessionCompactionResult struct {
	Applied               bool
	Stale                 bool
	FromTurn              int
	ToTurn                int
	References            []string
	ProjectedBeforeTokens int
	ProjectedAfterTokens  int
	SummaryItemCount      int
	SummaryItemThreshold  int
	CriticalWaterReached  bool
	SummaryLimitReached   bool
	NewSessionRecommended bool
}

// CompactSessionIfNeeded compresses one oldest-first batch only at the high water mark.
func CompactSessionIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, usage SessionCompactionUsage, incomingText string,
	onStart func(fromTurn, toTurn int)) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if client == nil || sessions == nil || sessionID == "" || usage.ContextWindow <= 0 {
		return result, nil
	}
	stats, err := sessions.SessionContextStats(sessionID, userID)
	if err != nil {
		return result, fmt.Errorf("measure session context %q: %w", sessionID, err)
	}
	summaryTokens := tooloutput.EstimateTokens(stats.SummaryJSON)
	incomingTokens := tooloutput.EstimateTokens(incomingText)
	historyTokens := summaryTokens + stats.UncompactedTokens
	observedOverhead := max(0, usage.PreviousPeakInputTokens-historyTokens)
	observedOutputReserve := max(0, usage.PreviousPeakReservedTokens-usage.PreviousPeakInputTokens)
	outputReserve := max(usage.OutputReserveTokens, observedOutputReserve)
	projectedBefore := historyTokens + incomingTokens + observedOverhead + outputReserve
	result.ProjectedBeforeTokens = projectedBefore
	result.SummaryItemThreshold = restartSummaryItemThreshold(usage.ContextWindow)

	highWater := int(float64(usage.ContextWindow) * sessionHighWaterRatio)
	selectionTarget := int(float64(usage.ContextWindow) * sessionSelectionTargetRatio)
	lowWater := int(float64(usage.ContextWindow) * sessionLowWaterRatio)
	criticalWater := int(float64(usage.ContextWindow) * sessionCriticalWaterRatio)
	eligibleTurns := max(0, stats.LatestTurn-sessionRecentTurns-stats.CompactedThroughTurn)
	result.SummaryItemCount = stats.CompactedThroughTurn
	result.CriticalWaterReached = projectedBefore >= criticalWater
	result.SummaryLimitReached = result.SummaryItemCount > result.SummaryItemThreshold
	result.NewSessionRecommended = shouldRecommendNewSession(
		projectedBefore, criticalWater, result.SummaryItemCount, result.SummaryItemThreshold,
	)
	decision := "below_high_water"
	if projectedBefore >= highWater {
		decision = "high_water_reached"
	}
	log.InfofCtx(ctx, "[qa] compaction decision session=%s window=%d high=%d low=%d critical=%d selection_target=%d history_tokens=%d summary_tokens=%d summary_items=%d restart_item_threshold=%d uncompacted_tokens=%d incoming_tokens=%d previous_peak_input=%d previous_peak_reserved=%d output_reserve=%d observed_overhead=%d projected=%d eligible_turns=%d new_session_recommended=%t decision=%s",
		sessionID, usage.ContextWindow, highWater, lowWater, criticalWater, selectionTarget,
		historyTokens, summaryTokens, result.SummaryItemCount, result.SummaryItemThreshold,
		stats.UncompactedTokens, incomingTokens,
		usage.PreviousPeakInputTokens, usage.PreviousPeakReservedTokens,
		outputReserve, observedOverhead, projectedBefore, eligibleTurns,
		result.NewSessionRecommended, decision)
	if projectedBefore < highWater {
		return result, nil
	}

	candidate, err := sessions.PrepareCompaction(sessionID, userID, memory.CompactionSelection{
		KeepRecentTurns: sessionRecentTurns, TargetReductionTokens: projectedBefore - selectionTarget,
		SummaryItemTokens: summaryItemTokenReserve,
	})
	if err != nil {
		return result, fmt.Errorf("prepare session compaction %q: %w", sessionID, err)
	}
	if candidate == nil {
		log.WarnfCtx(ctx, "[qa] compaction target unavailable session=%s projected=%d low=%d eligible_turns=0 reason=minimum_recent_turns_prevent_target",
			sessionID, projectedBefore, lowWater)
		return result, nil
	}
	if onStart != nil {
		onStart(candidate.FromTurn, candidate.ToTurn)
	}

	records := make([]memory.TurnContextRecord, 0, len(candidate.Turns))
	refs := make([]string, 0, len(candidate.Turns))
	removedTokens := 0
	for _, turn := range candidate.Turns {
		ref := turnCompactionRef(sessionID, turn.TurnNumber)
		detail, err := compressTurnDetail(turn.TurnNumber, turn.Messages)
		if err != nil {
			return result, fmt.Errorf("compress session %q turn %d: %w", sessionID, turn.TurnNumber, err)
		}
		records = append(records, memory.TurnContextRecord{
			Ref: ref, SessionID: sessionID, UserID: userID, RunID: turn.RunID,
			DetailJSON: detail, TurnNumber: turn.TurnNumber, SourceTokens: turn.SourceTokens,
			RetainedTokens: tooloutput.EstimateTokens(string(detail)),
		})
		refs = append(refs, ref)
		removedTokens += turn.SourceTokens
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
		records[i].SummaryText = summary
	}
	summaryJSON, err := buildRollingSummary(candidate.PreviousSummary, candidate.PreviousThrough, records)
	if err != nil {
		return result, fmt.Errorf("build session %q rolling summary: %w", sessionID, err)
	}
	remainingTokens := max(0, stats.UncompactedTokens-removedTokens)
	finalSummaryTokens := tooloutput.EstimateTokens(summaryJSON)
	projectedAfter := finalSummaryTokens + remainingTokens + incomingTokens + observedOverhead + outputReserve
	result.ProjectedAfterTokens = projectedAfter

	applied, err := sessions.ApplyCompaction(*candidate, records, summaryJSON)
	if err != nil {
		return result, fmt.Errorf("apply session compaction %q: %w", sessionID, err)
	}
	result.Applied = applied
	result.Stale = !applied
	result.FromTurn = candidate.FromTurn
	result.ToTurn = candidate.ToTurn
	result.References = refs
	result.SummaryItemCount = candidate.ToTurn
	result.CriticalWaterReached = applied && projectedAfter >= criticalWater
	result.SummaryLimitReached = applied && result.SummaryItemCount > result.SummaryItemThreshold
	result.NewSessionRecommended = applied && shouldRecommendNewSession(
		projectedAfter, criticalWater, result.SummaryItemCount, result.SummaryItemThreshold,
	)
	status := "stale"
	if applied {
		status = "below_low_water"
		if projectedAfter > lowWater {
			status = "above_low_water"
		}
	}
	log.InfofCtx(ctx, "[qa] compaction result session=%s turns=%d-%d eligible_through=%d estimated_reclaimed=%d removed_tokens=%d summary_tokens=%d summary_items=%d remaining_tokens=%d projected_before=%d projected_after=%d low=%d critical=%d new_session_recommended=%t status=%s",
		sessionID, candidate.FromTurn, candidate.ToTurn, candidate.EligibleThrough,
		candidate.EstimatedReclaimedTokens, removedTokens, finalSummaryTokens,
		result.SummaryItemCount, remainingTokens, projectedBefore, projectedAfter,
		lowWater, criticalWater, result.NewSessionRecommended, status)
	if applied && projectedAfter > lowWater {
		reason := "summary_estimate_exceeded_target"
		if candidate.ToTurn == candidate.EligibleThrough {
			reason = "all_eligible_turns_compacted_minimum_recent_turns_retained"
		}
		log.WarnfCtx(ctx, "[qa] compaction remains above low water session=%s projected_after=%d low=%d turns=%d-%d eligible_through=%d reason=%s",
			sessionID, projectedAfter, lowWater, candidate.FromTurn, candidate.ToTurn,
			candidate.EligibleThrough, reason)
	}
	return result, nil
}

func restartSummaryItemThreshold(contextWindow int) int {
	targetTokens := int(float64(contextWindow) * sessionRestartSummaryRatio)
	return max(1, (targetTokens+summaryItemTokenReserve-1)/summaryItemTokenReserve)
}

func shouldRecommendNewSession(projectedTokens, criticalWater, summaryItems, itemThreshold int) bool {
	return projectedTokens >= criticalWater || summaryItems > itemThreshold
}

func turnCompactionRef(sessionID string, turnNumber int) string {
	return "cmp_" + platform.UUIDFromString(fmt.Sprintf("%s:%d", sessionID, turnNumber))
}
