package investigation

import (
	"math"
	"sort"
	"time"
)

// RunMetricsSummary aggregates the bounded per-run dimensions that transport
// consumers need for cost calibration and regression gates. It is intentionally
// deterministic and only reports the dimensions already persisted per run.
type RunMetricsSummary struct {
	Runs                 int
	BudgetInsufficiency  float64
	ComposerFallbackRate float64
	DurationP50          time.Duration
	DurationP95          time.Duration
	InputTokensP50       int64
	InputTokensP95       int64
	OutputTokensP50      int64
	OutputTokensP95      int64
	ToolCallsP50         int
	ToolCallsP95         int
}

// AggregateRunMetrics computes p50/p95 and failure-rate summaries over the
// already persisted per-run metrics and statuses.
func AggregateRunMetrics(runs []InvestigationRun) RunMetricsSummary {
	metrics := make([]RunMetrics, 0, len(runs))
	exhausted := 0
	fallbacks := 0
	for _, run := range runs {
		metrics = append(metrics, run.Metrics)
		if run.Status == RunBudgetExhausted {
			exhausted++
		}
		if run.Metrics.ComposerFallback {
			fallbacks++
		}
	}
	durations := make([]int64, 0, len(metrics))
	inputTokens := make([]int64, 0, len(metrics))
	outputTokens := make([]int64, 0, len(metrics))
	toolCalls := make([]int, 0, len(metrics))
	for _, metric := range metrics {
		durations = append(durations, int64(metric.Duration))
		inputTokens = append(inputTokens, metric.InputTokens)
		outputTokens = append(outputTokens, metric.OutputTokens)
		toolCalls = append(toolCalls, metric.ToolCalls)
	}
	summary := RunMetricsSummary{Runs: len(runs)}
	if len(runs) > 0 {
		summary.BudgetInsufficiency = float64(exhausted) / float64(len(runs))
		summary.ComposerFallbackRate = float64(fallbacks) / float64(len(runs))
	}
	summary.DurationP50 = time.Duration(percentileInt64(durations, 0.50))
	summary.DurationP95 = time.Duration(percentileInt64(durations, 0.95))
	summary.InputTokensP50 = percentileInt64(inputTokens, 0.50)
	summary.InputTokensP95 = percentileInt64(inputTokens, 0.95)
	summary.OutputTokensP50 = percentileInt64(outputTokens, 0.50)
	summary.OutputTokensP95 = percentileInt64(outputTokens, 0.95)
	summary.ToolCallsP50 = percentileInt(toolCalls, 0.50)
	summary.ToolCallsP95 = percentileInt(toolCalls, 0.95)
	return summary
}

func percentileInt64(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func percentileInt(values []int, percentile float64) int {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}
