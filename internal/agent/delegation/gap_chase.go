package delegation

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// gapChaseObjectivePrefix keeps the narrowed continuation scope explicit so the
// child investigates only the missing evidence, never a fresh broad sweep.
const gapChaseObjectivePrefix = "Follow up on the unresolved evidence in the prior report. Only investigate the listed missing evidence goals and open hops, and return a schema-valid investigation.report:"

// chaseGaps runs a single bounded child continuation that targets only the
// unresolved evidence goals and open hops left by a partial report. It returns
// the projected continuation report plus the raw evidence units collected by
// that continuation, or ok=false when the chase cannot be admitted (no time, no
// budget, or a bad continuation result). It never retries and never re-emits
// the started/checkpoint/terminal lifecycle: the caller owns the final merged
// report's settlement and terminal event.
func (executor *Executor) chaseGaps(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	report agentapi.DelegationReport,
) (chased agentapi.DelegationReport, evidenceUnits []tool.EvidenceUnit, ok bool) {
	objective, gaps := buildGapChaseObjective(report)
	if objective == "" || len(gaps) == 0 || ctx.Err() != nil {
		return agentapi.DelegationReport{}, nil, false
	}

	limits, err := executor.gapChaseLimits(ctx, parent, task.definition)
	if err != nil {
		return agentapi.DelegationReport{}, nil, false
	}

	chaseTask := task
	chaseTask.childRunID = stableID("run_child_chase", task.childRunID, objective)
	chaseTask.request = agentapi.DelegationTask{
		Capability:  task.request.Capability,
		Objective:   truncateText(objective, maxObjectiveBytes),
		FocusFacets: append([]string(nil), task.request.FocusFacets...),
	}
	chaseTask.limits = limits
	chaseTask.objectiveHash = hashJSON(chaseTask.request)

	runCtx, cancel := context.WithDeadline(ctx, limits.Deadline)
	defer cancel()
	if chaseTask.budget != nil {
		runCtx = agentapi.WithRunBudgetGate(runCtx, chaseTask.budget)
	}
	result, runErr := executor.runtime.Run(runCtx, executor.runRequest(parent, delegationID, chaseTask))
	if runErr != nil || runCtx.Err() != nil || result.Status == agentapi.RunCancelled {
		return agentapi.DelegationReport{}, nil, false
	}

	flowEvidence := AddEvidenceUnits(cloneEvidenceIndex(parent.Evidence), result.EvidenceUnits)
	chased, err = projectReportWithEvidence(
		result, chaseTask.capability.ID, chaseTask.reportID, flowEvidence,
	)
	if err != nil {
		return agentapi.DelegationReport{}, nil, false
	}
	chased = boundReport(chased, chaseTask.reportTokens)
	return chased, cloneEvidenceUnits(result.EvidenceUnits), true
}

func buildGapChaseObjective(report agentapi.DelegationReport) (string, []string) {
	goals := appendUniqueStrings(nil, report.UnresolvedGoals...)
	hops := appendUniqueStrings(nil, report.OpenHops...)
	gaps := append(append([]string(nil), goals...), hops...)
	gaps = appendUniqueStrings(nil, gaps...)
	if len(gaps) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%s %s", gapChaseObjectivePrefix, strings.Join(gaps, "; ")), gaps
}

// mergeGapChase folds a usable continuation report back into the original
// partial report. It preserves the original identity and usage, moves newly
// covered goals out of the unresolved set, deduplicates findings, and extends
// the flow handoff. Open hops always mirror the merged flow's open hops, so a
// hop that the continuation resolved disappears while a still-open hop is kept.
// It never upgrades an inferred/unresolved hop to verified.
func mergeGapChase(
	original agentapi.DelegationReport,
	chased agentapi.DelegationReport,
) agentapi.DelegationReport {
	covered := appendUniqueStrings(nil, original.CoveredGoals...)
	unresolved := appendUniqueStrings(nil, original.UnresolvedGoals...)
	for _, goal := range chased.CoveredGoals {
		if containsString(unresolved, goal) {
			unresolved = removeString(unresolved, goal)
		}
		covered = appendUniqueStrings(covered, goal)
	}
	for _, goal := range chased.UnresolvedGoals {
		unresolved = appendUniqueStrings(unresolved, goal)
	}

	merged := original
	merged.CoveredGoals = covered
	merged.UnresolvedGoals = unresolved
	merged.Findings = mergeFindings(original.Findings, chased.Findings)

	if chased.Flow != nil {
		if merged.Flow == nil {
			merged.Flow = cloneFlowIR(chased.Flow)
		} else {
			merged.Flow = mergeFlowHandoff(cloneFlowIR(merged.Flow), cloneFlowIR(chased.Flow))
		}
	}
	if merged.Flow != nil {
		merged.OpenHops = appendUniqueStrings(nil, merged.Flow.OpenHops...)
	} else {
		merged.OpenHops = appendUniqueStrings(nil, original.OpenHops...)
	}
	return merged
}

func mergeFlowHandoff(base, chased *agentapi.FlowIR) *agentapi.FlowIR {
	if base == nil {
		return chased
	}
	if chased == nil {
		return base
	}
	merged, err := MergeFlowIRs([]agentapi.FlowIR{*base, *chased})
	if err != nil || merged == nil {
		// Fall back to appending chased nodes/edges without touching the
		// already-validated base. Open hops are only carried by the base.
		base.Nodes = append(base.Nodes, chased.Nodes...)
		base.Edges = append(base.Edges, chased.Edges...)
		return base
	}
	return merged
}

func mergeFindings(
	base, extra []agentapi.DelegationFinding,
) []agentapi.DelegationFinding {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]agentapi.DelegationFinding, 0, len(base)+len(extra))
	for _, finding := range append(append([]agentapi.DelegationFinding(nil), base...), extra...) {
		key := finding.ID
		if key == "" {
			key = finding.Statement
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, finding)
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

// gapChaseTimeAvailable reports whether a gap chase still fits inside the
// remaining answer/batch window. It prefers the earlier of BatchDeadline and
// AnswerDeadline, and requires at least one GapChaseTimeout plus the child
// answer safety margin to remain.
func (executor *Executor) gapChaseTimeAvailable(parent ParentContext) bool {
	deadline := parent.BatchDeadline
	if deadline.IsZero() {
		deadline = parent.AnswerDeadline
	} else if !parent.AnswerDeadline.IsZero() && parent.AnswerDeadline.Before(deadline) {
		deadline = parent.AnswerDeadline
	}
	if deadline.IsZero() {
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	// Mirror childDeadlineCap's scaled safety so short test/embedded windows
	// stay usable: for a window shorter than the normal safety margin, reserve
	// at most ten percent instead of rejecting an otherwise-fittable chase.
	safety := childAnswerDeadlineSafety
	if remaining < safety {
		safety = remaining / 10
		if safety < time.Millisecond {
			safety = 0
		}
	}
	return remaining >= executor.policy.GapChaseTimeout+safety
}

// acquireGapChaseSlot reserves one gap-chase continuation for a delegation
// batch. It returns false once the batch has reached MaxGapChasePerBatch, so
// multiple partial children in one batch cannot each pay a full chase.
func (executor *Executor) acquireGapChaseSlot(delegationID string) bool {
	limit := executor.policy.MaxGapChasePerBatch
	if limit <= 0 {
		limit = 1
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.gapChaseCounts == nil {
		executor.gapChaseCounts = make(map[string]int)
	}
	if executor.gapChaseCounts[delegationID] >= limit {
		return false
	}
	executor.gapChaseCounts[delegationID]++
	return true
}

// runGapChase is the single integration point in the owned-attempt settlement
// path. It decides whether a partial report has chasable gaps, runs at most one
// bounded child continuation, folds recovered evidence back in, and stamps the
// report.GapChase status. The parent policy, prompt, and effective tool scope
// are unchanged; gap chasing is executor-side only.
func (executor *Executor) runGapChase(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	report agentapi.DelegationReport,
	result agentapi.RunResult,
) (agentapi.DelegationReport, agentapi.RunResult) {
	chasable := report.Status == agentapi.DelegationPartial ||
		report.Completeness == agentapi.DelegationIncomplete
	gaps := len(report.UnresolvedGoals) + len(report.OpenHops)

	if !chasable || gaps == 0 {
		report.GapChase = agentapi.DelegationGapChaseNone
		return report, result
	}
	if executor.policy.MaxGapChaseRounds <= 0 || ctx.Err() != nil {
		report.GapChase = agentapi.DelegationGapChaseUnavailable
		return report, result
	}

	// Gate 1: time. Never launch a chase that cannot fit inside the remaining
	// answer/batch window plus the child answer safety margin.
	if !executor.gapChaseTimeAvailable(parent) {
		report.GapChase = agentapi.DelegationGapChaseBudgetExhausted
		log.InfofCtx(ctx,
			"[delegation] gap_chase rejected parent=%s delegation=%s task=%d reason=time",
			parent.RunID, delegationID, task.index,
		)
		return report, result
	}
	// Gate 2: value. Only unresolved evidence goals justify a continuation;
	// zero-valued open hops never trigger a chase on their own.
	if len(report.UnresolvedGoals) < executor.policy.MinGapChaseGoals {
		report.GapChase = agentapi.DelegationGapChaseNone
		log.InfofCtx(ctx,
			"[delegation] gap_chase rejected parent=%s delegation=%s task=%d reason=value",
			parent.RunID, delegationID, task.index,
		)
		return report, result
	}
	// Gate 3: per-batch quota. At most MaxGapChasePerBatch continuations per
	// delegation batch, so multiple partial children cannot each pay a chase.
	if !executor.acquireGapChaseSlot(delegationID) {
		report.GapChase = agentapi.DelegationGapChaseBudgetExhausted
		log.InfofCtx(ctx,
			"[delegation] gap_chase rejected parent=%s delegation=%s task=%d reason=quota",
			parent.RunID, delegationID, task.index,
		)
		return report, result
	}

	report.GapChase = agentapi.DelegationGapChaseTriggered
	chased, chasedUnits, ok := executor.chaseGaps(ctx, parent, delegationID, task, report)
	if !ok {
		// Chased but could not retrieve anything usable: the gaps are now known
		// to be unavailable within this budget window.
		report.GapChase = agentapi.DelegationGapChaseUnavailable
		return report, result
	}

	before := len(report.UnresolvedGoals) + len(report.OpenHops)
	merged := mergeGapChase(report, chased)
	merged = boundReport(merged, task.reportTokens)
	after := len(merged.UnresolvedGoals) + len(merged.OpenHops)
	resolvedCount := before - after
	if resolvedCount < 0 {
		resolvedCount = 0
	}

	if resolvedCount > 0 {
		merged.GapChase = agentapi.DelegationGapChaseRetrieved
	} else {
		merged.GapChase = agentapi.DelegationGapChaseUnavailable
	}

	merged.Usage.ToolCalls += chased.Usage.ToolCalls
	merged.Usage.InputTokens += chased.Usage.InputTokens
	merged.Usage.OutputTokens += chased.Usage.OutputTokens
	merged.Usage.ReasoningTokens += chased.Usage.ReasoningTokens
	merged.Usage.TotalTokens += chased.Usage.TotalTokens
	merged.Usage.CostMicros += chased.Usage.CostMicros

	// Propagate the continuation's evidence into the parent-visible ledger so
	// recovered goals and flow hops carry their own evidence units.
	result.EvidenceUnits = append(result.EvidenceUnits, chasedUnits...)

	log.InfofCtx(ctx,
		"[delegation] gap_chase settled parent=%s delegation=%s task=%d child=%s status=%s gaps_resolved=%d gaps_remaining=%d",
		parent.RunID, delegationID, task.index, task.childRunID,
		string(merged.GapChase), resolvedCount, after,
	)
	return merged, result
}
