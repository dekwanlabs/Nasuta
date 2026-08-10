package retrieval

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/knowledge"
)

type vectorReuseTools struct {
	servicePathFakeTools
	embedErr     error
	embedCalls   atomic.Int64
	vectorCalls  atomic.Int64
	fallbackCall atomic.Int64
}

func (tools *vectorReuseTools) EmbedQuery(context.Context, string) ([]float32, error) {
	tools.embedCalls.Add(1)
	if tools.embedErr != nil {
		return nil, tools.embedErr
	}
	return []float32{1, 0}, nil
}

func (tools *vectorReuseTools) FindCodeWithVector(
	context.Context, string, string, int, []float32,
) (domain.SearchResult[domain.CodeSearchHit], error) {
	tools.vectorCalls.Add(1)
	return domain.SearchResult[domain.CodeSearchHit]{}, nil
}

func (tools *vectorReuseTools) FindServicesWithVector(
	context.Context, string, int, []float32,
) (domain.SearchResult[domain.ServiceRecord], error) {
	tools.vectorCalls.Add(1)
	return domain.SearchResult[domain.ServiceRecord]{}, nil
}

func (tools *vectorReuseTools) FindRunbooksWithVector(
	context.Context, knowledge.RunbookQuery, []float32,
) (domain.RunbookSearchResult, error) {
	tools.vectorCalls.Add(1)
	return domain.RunbookSearchResult{}, nil
}

func (tools *vectorReuseTools) FindCode(
	context.Context, string, string, int,
) (domain.SearchResult[domain.CodeSearchHit], error) {
	tools.fallbackCall.Add(1)
	return domain.SearchResult[domain.CodeSearchHit]{}, nil
}

func (tools *vectorReuseTools) FindServices(
	context.Context, string, int,
) (domain.SearchResult[domain.ServiceRecord], error) {
	tools.fallbackCall.Add(1)
	return domain.SearchResult[domain.ServiceRecord]{}, nil
}

func (tools *vectorReuseTools) FindRunbooks(
	context.Context, knowledge.RunbookQuery,
) (domain.RunbookSearchResult, error) {
	tools.fallbackCall.Add(1)
	return domain.RunbookSearchResult{}, nil
}

func TestRetrievePlanReusesQueryEmbeddingAcrossSources(t *testing.T) {
	tools := &vectorReuseTools{}
	retrieve := New(tools, config.Config{})
	_, err := retrieve.RetrievePlan(
		t.Context(),
		"checkout timeout",
		QueryTerms{},
		domain.EvidencePlan{Sources: domain.Internal},
		domain.RetrievalIntent{Kind: domain.RetrievalRuntimeDiagnosis},
	)
	if err != nil {
		t.Fatalf("RetrievePlan: %v", err)
	}
	embedCalls := tools.embedCalls.Load()
	vectorCalls := tools.vectorCalls.Load()
	fallbackCalls := tools.fallbackCall.Load()
	if embedCalls != 1 || vectorCalls != 3 || fallbackCalls != 0 {
		t.Fatalf(
			"calls = embed:%d vector:%d fallback:%d, want 1/3/0",
			embedCalls, vectorCalls, fallbackCalls,
		)
	}
}

func TestRetrievePlanDoesNotRetryEmbeddingAcrossSources(t *testing.T) {
	tools := &vectorReuseTools{embedErr: errors.New("embedding unavailable")}
	retrieve := New(tools, config.Config{})
	_, err := retrieve.RetrievePlan(
		t.Context(),
		"checkout timeout",
		QueryTerms{},
		domain.EvidencePlan{Sources: domain.Internal},
		domain.RetrievalIntent{Kind: domain.RetrievalRuntimeDiagnosis},
	)
	if err != nil {
		t.Fatalf("RetrievePlan: %v", err)
	}
	embedCalls := tools.embedCalls.Load()
	vectorCalls := tools.vectorCalls.Load()
	fallbackCalls := tools.fallbackCall.Load()
	if embedCalls != 1 || vectorCalls != 3 || fallbackCalls != 0 {
		t.Fatalf(
			"calls = embed:%d vector:%d fallback:%d, want 1/3/0",
			embedCalls, vectorCalls, fallbackCalls,
		)
	}
}
