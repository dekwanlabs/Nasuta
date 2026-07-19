package semantic

import (
	"context"
	"fmt"
)

// Store is the backend-neutral semantic persistence contract.
type Store interface {
	Ensure(context.Context, Schema) error
	Search(context.Context, Query) ([]Hit, error)
	Upsert(context.Context, []Record) error
	Delete(context.Context, DeleteQuery) error
	Count(context.Context, Filter) (int, error)
	Capabilities() Capabilities
	Close() error
}

// Schema describes the logical collection expected by indexing.
type Schema struct {
	Collection string
	DenseDim   int
}

// Query describes dense or hybrid retrieval without provider-specific APIs.
type Query struct {
	DenseVector  []float32
	SparseVector *SparseVector
	Filter       Filter
	Limit        int
	GroupBy      string
}

// SparseVector is the BM25 sparse representation shared by supported backends.
type SparseVector struct {
	Indices []uint32
	Values  []float32
}

// Filter contains the scalar constraints required by Nasuta retrieval.
type Filter struct {
	Keywords   map[string]string
	AnyInteger map[string][]int64
}

// DeleteQuery selects records without exposing backend payload operations.
type DeleteQuery struct {
	IDs        []string
	Filter     Filter
	Except     Filter
	Repository string
	DocumentID string
}

// Record is one dense vector, optional sparse vector, and its metadata.
type Record struct {
	ID           string
	DenseVector  []float32
	SparseVector *SparseVector
	Metadata     map[string]any
}

// Hit is one backend-neutral semantic result.
type Hit struct {
	ID          string
	Score       float32
	ScoreKind   ScoreKind
	DenseScore  float32
	FusionScore float32
	Metadata    map[string]any
}

// ScoreKind distinguishes cosine similarity from rank-fusion scores.
type ScoreKind string

const (
	ScoreDense  ScoreKind = "dense"
	ScoreFusion ScoreKind = "rrf"
)

// Capabilities declares behavior implemented by a provider adapter.
type Capabilities struct {
	Dense   bool `json:"dense"`
	Sparse  bool `json:"sparse"`
	Hybrid  bool `json:"hybrid"`
	Filters bool `json:"filters"`
	GroupBy bool `json:"groupBy"`
	Count   bool `json:"count"`
}

var requiredCapabilities = Capabilities{
	Dense: true, Sparse: true, Hybrid: true, Filters: true, GroupBy: true, Count: true,
}

// RequiredCapabilities returns the behavior used by current indexing and QA paths.
func RequiredCapabilities() Capabilities { return requiredCapabilities }

// ValidateCapabilities rejects providers that would silently reduce retrieval behavior.
func ValidateCapabilities(provider string, actual Capabilities) error {
	required := RequiredCapabilities()
	if actual != required {
		return fmt.Errorf("semantic provider %q capabilities %+v do not satisfy required %+v", provider, actual, required)
	}
	return nil
}
