package session

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	sessionRecentTokenRatio     = 0.12
	sessionRecentTokenMinimum   = 8000
	sessionRecentTokenMaximum   = 16000
	summaryItemTokenReserve     = turnSummaryTokenLimit + 64
)

// SessionCompactionUsage contains the observable budget inputs for one decision.
type SessionCompactionUsage struct {
	ContextWindow              int
	PreviousPeakInputTokens    int
	PreviousPeakReservedTokens int
	OutputReserveTokens        int
}

// SessionCompactionResult reports whether the monotonic archive boundary advanced.
type SessionCompactionResult struct {
	Applied               bool
	Stale                 bool
	FromTurn              int
	ToTurn                int
	References            []string
	ProjectedBeforeTokens int
	ProjectedAfterTokens  int
	ArchivedTurnCount     int
	RestartTurnThreshold  int
	CriticalWaterReached  bool
	NewSessionRecommended bool
}

type sessionCompactionPlan struct {
	trigger          string
	targetReduction  int
	incomingTokens   int
	observedOverhead int
	outputReserve    int
	lowWater         int
	criticalWater    int
}

// CompactSessionIfNeeded protects the next run when the projected context reaches high water.
func CompactSessionIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, usage SessionCompactionUsage, incomingText string,
	onStart func(fromTurn, toTurn int), histories ...SessionHistory) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if client == nil || sessions == nil || sessionID == "" || usage.ContextWindow <= 0 {
		return result, nil
	}
	stats, err := sessions.SessionContextStats(sessionID, userID)
	if err != nil {
		return result, fmt.Errorf("measure session context %q: %w", sessionID, err)
	}
	incomingTokens := tooloutput.EstimateTokens(incomingText)
	historyTokens := stats.UncompactedTokens
	observedOverhead := max(0, usage.PreviousPeakInputTokens-historyTokens)
	observedOutputReserve := max(0, usage.PreviousPeakReservedTokens-usage.PreviousPeakInputTokens)
	outputReserve := max(usage.OutputReserveTokens, observedOutputReserve)
	projectedBefore := historyTokens + incomingTokens + observedOverhead + outputReserve
	result.ProjectedBeforeTokens = projectedBefore
	result.RestartTurnThreshold = restartTurnThreshold(usage.ContextWindow)

	highWater := int(float64(usage.ContextWindow) * sessionHighWaterRatio)
	selectionTarget := int(float64(usage.ContextWindow) * sessionSelectionTargetRatio)
	lowWater := int(float64(usage.ContextWindow) * sessionLowWaterRatio)
	criticalWater := int(float64(usage.ContextWindow) * sessionCriticalWaterRatio)
	eligibleTurns := max(0, stats.LatestTurn-sessionRecentTurns-stats.CompactedThroughTurn)
	result.ArchivedTurnCount = stats.CompactedThroughTurn
	result.CriticalWaterReached = projectedBefore >= criticalWater
	result.NewSessionRecommended = shouldRecommendNewSession(
		projectedBefore, criticalWater, result.ArchivedTurnCount, result.RestartTurnThreshold,
	)
	decision := "below_high_water"
	if projectedBefore >= highWater {
		decision = "high_water_reached"
	}
	log.InfofCtx(ctx, "[qa] compaction decision session=%s window=%d high=%d low=%d critical=%d selection_target=%d history_tokens=%d archived_summary_tokens=%d archived_turns=%d restart_turn_threshold=%d uncompacted_tokens=%d incoming_tokens=%d previous_peak_input=%d previous_peak_reserved=%d output_reserve=%d observed_overhead=%d projected=%d eligible_turns=%d new_session_recommended=%t decision=%s",
		sessionID, usage.ContextWindow, highWater, lowWater, criticalWater, selectionTarget,
		historyTokens, stats.ArchivedSummaryTokens, result.ArchivedTurnCount, result.RestartTurnThreshold,
		stats.UncompactedTokens, incomingTokens,
		usage.PreviousPeakInputTokens, usage.PreviousPeakReservedTokens,
		outputReserve, observedOverhead, projectedBefore, eligibleTurns,
		result.NewSessionRecommended, decision)
	if projectedBefore < highWater {
		return result, nil
	}
	return compactSessionTurns(ctx, client, sessions, sessionID, userID, stats, result, sessionCompactionPlan{
		trigger: "high_water", targetReduction: projectedBefore - selectionTarget,
		incomingTokens: incomingTokens, observedOverhead: observedOverhead, outputReserve: outputReserve,
		lowWater: lowWater, criticalWater: criticalWater,
	}, onStart, histories...)
}

// ArchiveSessionHistoryIfNeeded bounds the raw tail after a complete turn has been saved.
func ArchiveSessionHistoryIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, contextWindow int, histories ...SessionHistory) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if client == nil || sessions == nil || sessionID == "" || contextWindow <= 0 {
		return result, nil
	}
	stats, err := sessions.SessionContextStats(sessionID, userID)
	if err != nil {
		return result, fmt.Errorf("measure session context %q after turn: %w", sessionID, err)
	}
	recentBudget := recentHistoryTokenBudget(contextWindow)
	criticalWater := int(float64(contextWindow) * sessionCriticalWaterRatio)
	eligibleTurns := max(0, stats.LatestTurn-sessionRecentTurns-stats.CompactedThroughTurn)
	result = newSessionCompactionResult(stats, contextWindow, stats.UncompactedTokens, criticalWater)
	decision := "within_recent_budget"
	if stats.UncompactedTokens > recentBudget && eligibleTurns > 0 {
		decision = "recent_budget_exceeded"
	}
	log.InfofCtx(ctx, "[qa] post-turn archive decision session=%s recent_budget=%d uncompacted_tokens=%d archived_turns=%d eligible_turns=%d decision=%s",
		sessionID, recentBudget, stats.UncompactedTokens, stats.CompactedThroughTurn, eligibleTurns, decision)
	if stats.UncompactedTokens <= recentBudget || eligibleTurns == 0 {
		if stats.UncompactedTokens > recentBudget {
			log.WarnfCtx(ctx, "[qa] post-turn archive remains above budget session=%s uncompacted_tokens=%d recent_budget=%d reason=minimum_recent_turns_exceed_budget",
				sessionID, stats.UncompactedTokens, recentBudget)
		}
		return result, nil
	}
	return compactSessionTurns(ctx, client, sessions, sessionID, userID, stats, result, sessionCompactionPlan{
		trigger: "recent_budget", targetReduction: stats.UncompactedTokens - recentBudget,
		lowWater: recentBudget, criticalWater: criticalWater,
	}, nil, histories...)
}

func compactSessionTurns(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, stats memory.SessionContextStats, result SessionCompactionResult,
	plan sessionCompactionPlan, onStart func(fromTurn, toTurn int),
	histories ...SessionHistory) (SessionCompactionResult, error) {

	compactionStarted := time.Now()
	candidate, err := sessions.PrepareCompaction(sessionID, userID, memory.CompactionSelection{
		KeepRecentTurns: sessionRecentTurns, TargetReductionTokens: plan.targetReduction,
	})
	if err != nil {
		return result, fmt.Errorf("prepare session compaction %q: %w", sessionID, err)
	}
	if candidate == nil {
		log.WarnfCtx(ctx, "[qa] compaction target unavailable session=%s trigger=%s projected=%d target=%d reason=minimum_recent_turns_prevent_target",
			sessionID, plan.trigger, result.ProjectedBeforeTokens, plan.lowWater)
		return result, nil
	}
	if onStart != nil {
		onStart(candidate.FromTurn, candidate.ToTurn)
	}
	prepareDuration := time.Since(compactionStarted)

	archiveStarted := time.Now()
	records := make([]memory.TurnContextRecord, 0, len(candidate.Turns))
	refs := make([]string, 0, len(candidate.Turns))
	removedTokens := 0
	for _, turn := range candidate.Turns {
		ref := turnCompactionRef(sessionID, turn.TurnNumber)
		detail, err := CompressTurnDetail(turn.TurnNumber, turn.Messages)
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
	archiveDuration := time.Since(archiveStarted)
	summaryStarted := time.Now()
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
		records[i].SummaryTokens = tooloutput.EstimateTokens(summary)
	}
	summaryDuration := time.Since(summaryStarted)
	if len(histories) > 0 && histories[0] != nil {
		histories[0].PrepareRecords(records)
	}
	remainingTokens := max(0, stats.UncompactedTokens-removedTokens)
	projectedAfter := remainingTokens + plan.incomingTokens + plan.observedOverhead + plan.outputReserve
	result.ProjectedAfterTokens = projectedAfter

	applyStarted := time.Now()
	applied, err := sessions.ApplyCompaction(*candidate, records)
	if err != nil {
		return result, fmt.Errorf("apply session compaction %q: %w", sessionID, err)
	}
	result.Applied = applied
	result.Stale = !applied
	result.FromTurn = candidate.FromTurn
	result.ToTurn = candidate.ToTurn
	result.References = refs
	result.ArchivedTurnCount = candidate.ToTurn
	result.CriticalWaterReached = applied && projectedAfter >= plan.criticalWater
	result.NewSessionRecommended = applied && shouldRecommendNewSession(
		projectedAfter, plan.criticalWater, result.ArchivedTurnCount, result.RestartTurnThreshold,
	)
	applyDuration := time.Since(applyStarted)
	status := "stale"
	if applied {
		status = "below_low_water"
		if projectedAfter > plan.lowWater {
			status = "above_low_water"
		}
	}
	log.InfofCtx(ctx, "[qa] compaction result session=%s trigger=%s turns=%d-%d eligible_through=%d estimated_reclaimed=%d removed_tokens=%d archived_turns=%d remaining_tokens=%d projected_before=%d projected_after=%d low=%d critical=%d new_session_recommended=%t total_ms=%d prepare_ms=%d archive_ms=%d summarize_ms=%d apply_ms=%d status=%s",
		sessionID, plan.trigger, candidate.FromTurn, candidate.ToTurn, candidate.EligibleThrough,
		candidate.EstimatedReclaimedTokens, removedTokens, result.ArchivedTurnCount,
		remainingTokens, result.ProjectedBeforeTokens, projectedAfter,
		plan.lowWater, plan.criticalWater, result.NewSessionRecommended,
		time.Since(compactionStarted).Milliseconds(), prepareDuration.Milliseconds(),
		archiveDuration.Milliseconds(), summaryDuration.Milliseconds(), applyDuration.Milliseconds(), status)
	if applied && projectedAfter > plan.lowWater {
		log.WarnfCtx(ctx, "[qa] compaction remains above low water session=%s trigger=%s projected_after=%d low=%d turns=%d-%d eligible_through=%d reason=all_eligible_turns_compacted_minimum_recent_turns_retained",
			sessionID, plan.trigger, projectedAfter, plan.lowWater, candidate.FromTurn, candidate.ToTurn,
			candidate.EligibleThrough)
	}
	return result, nil
}

func newSessionCompactionResult(stats memory.SessionContextStats, contextWindow, projected, criticalWater int) SessionCompactionResult {
	restartThreshold := restartTurnThreshold(contextWindow)
	return SessionCompactionResult{
		ProjectedBeforeTokens: projected,
		ArchivedTurnCount:     stats.CompactedThroughTurn,
		RestartTurnThreshold:  restartThreshold,
		CriticalWaterReached:  projected >= criticalWater,
		NewSessionRecommended: shouldRecommendNewSession(
			projected, criticalWater, stats.CompactedThroughTurn, restartThreshold,
		),
	}
}

func recentHistoryTokenBudget(contextWindow int) int {
	return min(max(int(float64(contextWindow)*sessionRecentTokenRatio), sessionRecentTokenMinimum), sessionRecentTokenMaximum)
}

func restartTurnThreshold(contextWindow int) int {
	targetTokens := int(float64(contextWindow) * sessionRestartSummaryRatio)
	return max(1, (targetTokens+summaryItemTokenReserve-1)/summaryItemTokenReserve)
}

func shouldRecommendNewSession(projectedTokens, criticalWater, archivedTurns, turnThreshold int) bool {
	return projectedTokens >= criticalWater || archivedTurns > turnThreshold
}

func turnCompactionRef(sessionID string, turnNumber int) string {
	return "cmp_" + platform.UUIDFromString(fmt.Sprintf("%s:%d", sessionID, turnNumber))
}
