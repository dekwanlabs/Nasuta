package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	sessionSelectionTargetRatio = 0.60
	sessionLowWaterRatio        = 0.65
	sessionCriticalWaterRatio   = 0.95
	sessionRestartSummaryRatio  = 0.30
	sessionRecentTurns          = 3
	summaryItemTokenReserve     = turnSummaryTokenLimit + 64
)

// SessionCompactionUsage contains the observable budget inputs for one decision.
type SessionCompactionUsage struct {
	ContextWindow int
	// IncomingTokens is the current request prompt outside session-owned history.
	// When unset, the compactor estimates it from incomingText for compatibility.
	IncomingTokens int
	// ProjectedTokens is the complete model request projection, including the
	// selected session history and output reservation. When present, it is the
	// authoritative high-water decision value; persisted history totals remain
	// useful only for choosing which old turns to archive.
	ProjectedTokens     int
	OutputReserveTokens int
}

// SessionCompactionResult reports whether the monotonic archive boundary advanced.
type SessionCompactionResult struct {
	Applied               bool
	Stale                 bool
	Triggered             bool
	FromTurn              int
	ToTurn                int
	References            []string
	ProjectedBeforeTokens int
	ProjectedAfterTokens  int
	ContextWindow         int
	HighWaterTokens       int
	SafetyTokens          int
	SafeLimitTokens       int
	OutputReserveTokens   int
	ArchivedTurnCount     int
	RestartTurnThreshold  int
	CriticalWaterReached  bool
	NewSessionRecommended bool
}

type sessionCompactionPlan struct {
	trigger         string
	targetReduction int
	incomingTokens  int
	outputReserve   int
	lowWater        int
	criticalWater   int
}

// CompactSessionIfNeeded bounds the session-owned part of the next run's context.
func CompactSessionIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, usage SessionCompactionUsage, incomingText string,
	onStart func(fromTurn, toTurn int), histories ...SessionHistory) (SessionCompactionResult, error) {
	return compactSessionIfNeeded(
		ctx, client, sessions, sessionID, userID, usage, incomingText,
		"pre_answer_high_water", onStart, histories...,
	)
}

func compactSessionIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, usage SessionCompactionUsage, incomingText, trigger string,
	onStart func(fromTurn, toTurn int), histories ...SessionHistory) (SessionCompactionResult, error) {
	var result SessionCompactionResult
	if client == nil || sessions == nil || sessionID == "" || usage.ContextWindow <= 0 {
		return result, nil
	}
	stats, err := sessions.SessionContextStats(sessionID, userID)
	if err != nil {
		return result, fmt.Errorf("measure session context %q: %w", sessionID, err)
	}
	incomingTokens := max(0, usage.IncomingTokens)
	if incomingTokens == 0 {
		incomingTokens = tooloutput.EstimateTokens(incomingText)
	}
	historyTokens := stats.UncompactedTokens
	outputReserve := max(0, usage.OutputReserveTokens)
	projectedBefore := historyTokens + incomingTokens + outputReserve
	if usage.ProjectedTokens > 0 {
		projectedBefore = usage.ProjectedTokens
	}
	result.ProjectedBeforeTokens = projectedBefore
	result.ProjectedAfterTokens = projectedBefore
	result.ContextWindow = usage.ContextWindow
	result.HighWaterTokens = run.ContextHighWaterTokens(usage.ContextWindow)
	result.SafetyTokens = run.ContextSafetyTokens(usage.ContextWindow)
	result.SafeLimitTokens = run.ContextSafeLimitTokens(usage.ContextWindow)
	result.OutputReserveTokens = outputReserve
	result.RestartTurnThreshold = restartTurnThreshold(usage.ContextWindow)

	highWater := result.HighWaterTokens
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
	log.InfofCtx(ctx, "[qa] compaction decision session=%s trigger=%s window=%d high=%d low=%d critical=%d selection_target=%d history_tokens=%d archived_summary_tokens=%d archived_turns=%d restart_turn_threshold=%d uncompacted_tokens=%d incoming_tokens=%d output_reserve=%d projected=%d eligible_turns=%d new_session_recommended=%t decision=%s",
		sessionID, trigger, usage.ContextWindow, highWater, lowWater, criticalWater, selectionTarget,
		historyTokens, stats.ArchivedSummaryTokens, result.ArchivedTurnCount, result.RestartTurnThreshold,
		stats.UncompactedTokens, incomingTokens, outputReserve, projectedBefore, eligibleTurns,
		result.NewSessionRecommended, decision)
	if projectedBefore < highWater {
		return result, nil
	}
	result.Triggered = true
	return compactSessionTurns(ctx, client, sessions, sessionID, userID, stats, result, sessionCompactionPlan{
		trigger: trigger, targetReduction: projectedBefore - selectionTarget,
		incomingTokens: incomingTokens, outputReserve: outputReserve,
		lowWater: lowWater, criticalWater: criticalWater,
	}, onStart, histories...)
}

// ArchiveSessionHistoryIfNeeded applies the same high-water policy after a turn is saved.
func ArchiveSessionHistoryIfNeeded(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, contextWindow int, histories ...SessionHistory) (SessionCompactionResult, error) {
	return ArchiveSessionHistoryIfNeededWithStatus(
		ctx, client, sessions, sessionID, userID,
		SessionCompactionUsage{ContextWindow: contextWindow}, nil, histories...,
	)
}

// ArchiveSessionHistoryIfNeededWithStatus performs idle compaction only after the high-water mark is reached.
func ArchiveSessionHistoryIfNeededWithStatus(ctx context.Context, client *llm.LLMClient, sessions *memory.SessionStore,
	sessionID string, userID int64, usage SessionCompactionUsage, onStart func(fromTurn, toTurn int),
	histories ...SessionHistory) (SessionCompactionResult, error) {
	return compactSessionIfNeeded(
		ctx, client, sessions, sessionID, userID, usage, "",
		"post_turn_high_water", onStart, histories...,
	)
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
	projectedAfter := remainingTokens + plan.incomingTokens + plan.outputReserve
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
