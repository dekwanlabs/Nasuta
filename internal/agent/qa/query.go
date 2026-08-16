package qa

import (
	"context"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
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
	History       retrieval.HistoryRelation
	HistoryOrigin string
	HistoryUpdate string
	TimeRange     tool.TimeRange
	HasTimeRange  bool
	TimeError     error
	QueryPlan     domain.QueryPlan
}

type queryAnalysisResult struct {
	Analysis         queryAnalysisOutput
	ResolutionOrigin domain.QueryResolutionOrigin
	MatchedRuleKind  domain.QueryKind
}

var queryAnalysisSpec = runtrace.Spec[queryAnalysisInput, queryAnalysisResult]{
	Operation: "agent.query_analysis",
	Node:      "query_analysis",
	Input: func(input queryAnalysisInput) map[string]any {
		return map[string]any{"question": input.Question, "history_candidates": len(input.RecentTurns)}
	},
	Output: func(input queryAnalysisInput, result queryAnalysisResult, _ error) map[string]any {
		output := result.Analysis
		timeFrom, timeTo := "", ""
		if output.HasTimeRange {
			timeFrom = output.TimeRange.From.Format(time.RFC3339)
			timeTo = output.TimeRange.To.Format(time.RFC3339)
		}
		required := domain.RequiredFacetsFor(output.QueryPlan.Kind)
		requiredFacets := make([]string, len(required))
		for index, facet := range required {
			requiredFacets[index] = string(facet)
		}
		return map[string]any{
			"clean_question": input.CleanQuestion, "domain_terms": input.Terms.DomainTerms,
			"identifiers": input.Terms.Identifiers, "query_kind": output.QueryPlan.Kind,
			"query_entities": output.QueryPlan.Entities, "entity_count": len(output.QueryPlan.Entities),
			"required_facets": requiredFacets, "resolution_origin": result.ResolutionOrigin,
			"matched_rule_kind": result.MatchedRuleKind,
			"time_kind":         input.Time.Kind, "time_raw": input.Time.Raw,
			"time_from": timeFrom, "time_to": timeTo,
		}
	},
	Status: func(result queryAnalysisResult, _ error) string {
		if result.Analysis.TimeError != nil {
			return "degraded"
		}
		return ""
	},
}

func analyzeQuery(ctx context.Context, input queryAnalysisInput) (queryAnalysisOutput, error) {
	result, err := runtrace.Invoke(ctx, queryAnalysisSpec, input, func(_ context.Context, input queryAnalysisInput) (queryAnalysisResult, error) {
		history, origin, update := resolveHistoryRelation(input.Question, input.RecentTurns, input.History, input.HistoryValid)
		timeRange, hasTimeRange, timeErr := retrieval.ResolveTime(input.Time, input.Anchor)
		resolution := domain.ResolveQueryPlan(input.Question, domain.QuerySignals{
			Identifiers: input.Terms.Identifiers,
			DomainTerms: input.Terms.DomainTerms,
		})
		return queryAnalysisResult{
			Analysis: queryAnalysisOutput{
				History: history, HistoryOrigin: origin, HistoryUpdate: update,
				TimeRange: timeRange, HasTimeRange: hasTimeRange, TimeError: timeErr,
				QueryPlan: resolution.Plan,
			},
			ResolutionOrigin: resolution.Origin,
			MatchedRuleKind:  resolution.MatchedRuleKind,
		}, nil
	})
	return result.Analysis, err
}

type queryRewriteInput struct {
	CleanQuestion string
	ContextTerms  string
}

var queryRewriteSpec = runtrace.Spec[queryRewriteInput, string]{
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
	return runtrace.Invoke(ctx, queryRewriteSpec, input, func(_ context.Context, input queryRewriteInput) (string, error) {
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

var retrievalDispatchSpec = runtrace.Spec[retrievalDispatchInput, *retrieval.RetrievedContext]{
	Operation: "agent.retrieval_dispatch",
	Node:      "retrieval_dispatch",
	Output: func(input retrievalDispatchInput, _ *retrieval.RetrievedContext, _ error) map[string]any {
		return map[string]any{"skipped": true, "sources": input.Plan.SourceNames()}
	},
}

func skipRetrieval(ctx context.Context, input retrievalDispatchInput) (*retrieval.RetrievedContext, error) {
	return runtrace.Invoke(ctx, retrievalDispatchSpec, input, func(_ context.Context, input retrievalDispatchInput) (*retrieval.RetrievedContext, error) {
		result := &retrieval.RetrievedContext{OriginalQuestion: input.Question}
		mergePreloadedContext(result, input.Blocks, input.Budget)
		appendUnavailableWeb(result, input.WebDown)
		return result, nil
	})
}
