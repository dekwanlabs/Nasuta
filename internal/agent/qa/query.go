package qa

import (
	"context"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

type queryAnalysisInput struct {
	Question      string
	CleanQuestion string
	Terms         retrieval.QueryTerms
	Time          retrieval.TimeExpr
	Anchor        time.Time
	RecentTurns   []memory.TurnMetadata
	History       retrieval.HistoryRelation
	HistoryValid  bool
}

type queryAnalysisOutput struct {
	History         retrieval.HistoryRelation
	HistoryOrigin   string
	HistoryUpdate   string
	TimeRange       tool.TimeRange
	HasTimeRange    bool
	TimeError       error
	ResponseMode    domain.ResponseMode
	RetrievalIntent domain.RetrievalIntent
	IntentOrigin    domain.IntentOrigin
}

var queryAnalysisSpec = executiontrace.Spec[queryAnalysisInput, queryAnalysisOutput]{
	Operation: "agent.query_analysis",
	Node:      "query_analysis",
	Input: func(input queryAnalysisInput) map[string]any {
		return map[string]any{"question": input.Question, "history_candidates": len(input.RecentTurns)}
	},
	Output: func(input queryAnalysisInput, output queryAnalysisOutput, _ error) map[string]any {
		timeFrom, timeTo := "", ""
		if output.HasTimeRange {
			timeFrom = output.TimeRange.From.Format(time.RFC3339)
			timeTo = output.TimeRange.To.Format(time.RFC3339)
		}
		return map[string]any{
			"clean_question": input.CleanQuestion, "domain_terms": input.Terms.DomainTerms,
			"identifiers": input.Terms.Identifiers, "response_mode": output.ResponseMode,
			"time_kind": input.Time.Kind, "time_raw": input.Time.Raw,
			"time_from": timeFrom, "time_to": timeTo,
		}
	},
	Status: func(output queryAnalysisOutput, _ error) string {
		if output.TimeError != nil {
			return "degraded"
		}
		return ""
	},
}

func analyzeQuery(ctx context.Context, input queryAnalysisInput) (queryAnalysisOutput, error) {
	return executiontrace.Invoke(ctx, queryAnalysisSpec, input, func(_ context.Context, input queryAnalysisInput) (queryAnalysisOutput, error) {
		history, origin, update := resolveHistoryRelation(input.Question, input.RecentTurns, input.History, input.HistoryValid)
		timeRange, hasTimeRange, timeErr := retrieval.ResolveTime(input.Time, input.Anchor)
		intent := domain.ResolveRetrievalIntent(input.Question, domain.RetrievalIntentSignals{
			Identifiers: input.Terms.Identifiers,
			DomainTerms: input.Terms.DomainTerms,
		})
		return queryAnalysisOutput{
			History: history, HistoryOrigin: origin, HistoryUpdate: update,
			TimeRange: timeRange, HasTimeRange: hasTimeRange, TimeError: timeErr,
			ResponseMode: intent.ResponseMode, RetrievalIntent: intent.Intent, IntentOrigin: intent.Origin,
		}, nil
	})
}

type queryRewriteInput struct {
	CleanQuestion string
	ContextTerms  string
}

var queryRewriteSpec = executiontrace.Spec[queryRewriteInput, string]{
	Operation: "agent.query_rewrite",
	Node:      "query_rewrite",
	Input: func(input queryRewriteInput) map[string]any {
		return map[string]any{"clean_question": input.CleanQuestion}
	},
	Output: func(input queryRewriteInput, output string, _ error) map[string]any {
		return map[string]any{"retrieval_query": output, "context_augmented": output != input.CleanQuestion}
	},
}

func rewriteQuery(ctx context.Context, input queryRewriteInput) (string, error) {
	return executiontrace.Invoke(ctx, queryRewriteSpec, input, func(_ context.Context, input queryRewriteInput) (string, error) {
		return canonicalRetrievalQuery(input.CleanQuestion, input.ContextTerms), nil
	})
}

type retrievalDispatchInput struct {
	Question string
	Plan     domain.EvidencePlan
	Blocks   []ContextBlock
	Budget   int
	WebDown  bool
}

var retrievalDispatchSpec = executiontrace.Spec[retrievalDispatchInput, *retrieval.RetrievedContext]{
	Operation: "agent.retrieval_dispatch",
	Node:      "retrieval_dispatch",
	Output: func(input retrievalDispatchInput, _ *retrieval.RetrievedContext, _ error) map[string]any {
		return map[string]any{"skipped": true, "sources": input.Plan.SourceNames()}
	},
}

func skipRetrieval(ctx context.Context, input retrievalDispatchInput) (*retrieval.RetrievedContext, error) {
	return executiontrace.Invoke(ctx, retrievalDispatchSpec, input, func(_ context.Context, input retrievalDispatchInput) (*retrieval.RetrievedContext, error) {
		result := &retrieval.RetrievedContext{OriginalQuestion: input.Question}
		mergePreloadedContext(result, input.Blocks, input.Budget)
		appendUnavailableWeb(result, input.WebDown)
		return result, nil
	})
}
