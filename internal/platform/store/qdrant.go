package store

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/astris/config"
	"github.com/dekwanlabs/astris/log"
	"github.com/qdrant/go-client/qdrant"
)

type SemanticHit struct {
	ID          string
	Score       float32
	ScoreKind   SemanticScoreKind
	DenseScore  float32
	FusionScore float32
	Payload     map[string]any
}

// SemanticScoreKind keeps cosine similarity separate from rank-fusion scores.
type SemanticScoreKind string

const (
	SemanticScoreDense  SemanticScoreKind = "dense"
	SemanticScoreFusion SemanticScoreKind = "rrf"
)

type SemanticPoint struct {
	ID            string
	Vector        []float32
	SparseIndices []uint32  // BM25 sparse vector indices
	SparseValues  []float32 // BM25 sparse vector values
	Payload       map[string]any
}

// SemanticFilter preserves typed payload constraints that string filters cannot express.
type SemanticFilter struct {
	Keywords   map[string]string
	AnyInteger map[string][]int64
}

type SemanticStore interface {
	Ensure(ctx context.Context, dim int) error
	// Search runs a dense query. groupKey, when non-empty, makes Qdrant return
	// at most one point per distinct value of that payload field (e.g. "path")
	// so a long doc cannot flood recall with its chunks.
	Search(ctx context.Context, vector []float32, filters map[string]string, limit int, groupKey string) ([]SemanticHit, error)
	SearchFiltered(ctx context.Context, vector []float32, filter SemanticFilter, limit int, groupKey string) ([]SemanticHit, error)
	SearchHybrid(ctx context.Context, vector []float32, sparseIndices []uint32, sparseValues []float32, filters map[string]string, limit int, groupKey string) ([]SemanticHit, error)
	Upsert(ctx context.Context, points []SemanticPoint) error
	DeletePoints(ctx context.Context, ids []string) error
	DeleteByRepo(ctx context.Context, repo string) error
	DeleteByFilterExcept(ctx context.Context, filters, except map[string]string) error
	DeleteRepoExceptGeneration(ctx context.Context, repo, generation string) error
	DeleteByDocID(ctx context.Context, docID string) error
	CountByFilter(ctx context.Context, filters map[string]string) (int, error)
	Enabled() bool
}

type NoopSemantic struct{}

func (NoopSemantic) Ensure(context.Context, int) error { return nil }
func (NoopSemantic) Search(context.Context, []float32, map[string]string, int, string) ([]SemanticHit, error) {
	return nil, fmt.Errorf("semantic search disabled: configure QDRANT_HOST + EMBEDDING_API_KEY")
}
func (NoopSemantic) SearchFiltered(context.Context, []float32, SemanticFilter, int, string) ([]SemanticHit, error) {
	return nil, fmt.Errorf("semantic search disabled: configure QDRANT_HOST + EMBEDDING_API_KEY")
}
func (NoopSemantic) SearchHybrid(context.Context, []float32, []uint32, []float32, map[string]string, int, string) ([]SemanticHit, error) {
	return nil, fmt.Errorf("semantic search disabled")
}
func (NoopSemantic) Upsert(context.Context, []SemanticPoint) error { return nil }
func (NoopSemantic) DeletePoints(context.Context, []string) error  { return nil }
func (NoopSemantic) DeleteByRepo(context.Context, string) error    { return nil }
func (NoopSemantic) DeleteByFilterExcept(context.Context, map[string]string, map[string]string) error {
	return nil
}
func (NoopSemantic) DeleteRepoExceptGeneration(context.Context, string, string) error {
	return nil
}
func (NoopSemantic) DeleteByDocID(context.Context, string) error { return nil }
func (NoopSemantic) CountByFilter(context.Context, map[string]string) (int, error) {
	return 0, nil
}
func (NoopSemantic) Enabled() bool { return false }

type Qdrant struct {
	client     *qdrant.Client
	collection string
}

// Keep the original sparse vector name so existing collections can be
// migrated in place without dropping dense service/document embeddings.
const codeSparseVector = "bm25"

func NewSemantic(c config.Config) (SemanticStore, error) {
	return NewSemanticWithCollection(c, c.QdrantCollection)
}

func NewSemanticWithCollection(c config.Config, collection string) (SemanticStore, error) {
	if c.QdrantHost == "" {
		return NoopSemantic{}, nil
	}
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   c.QdrantHost,
		Port:                   c.QdrantPort,
		APIKey:                 c.QdrantAPIKey,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, err
	}
	return &Qdrant{client: client, collection: collection}, nil
}

func (q *Qdrant) Enabled() bool { return true }

func (q *Qdrant) Ensure(ctx context.Context, dim int) error {
	bm25Params := codeSparseVectorParams()
	exists, err := q.client.CollectionExists(ctx, q.collection)
	if err != nil {
		return err
	}
	if exists {
		return q.client.UpdateCollection(ctx, &qdrant.UpdateCollection{
			CollectionName: q.collection,
			SparseVectorsConfig: &qdrant.SparseVectorConfig{
				Map: map[string]*qdrant.SparseVectorParams{
					codeSparseVector: bm25Params,
				},
			},
		})
	}
	return q.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: q.collection,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dim),
			Distance: qdrant.Distance_Cosine,
		}),
		SparseVectorsConfig: qdrant.NewSparseVectorsConfig(map[string]*qdrant.SparseVectorParams{
			codeSparseVector: bm25Params,
		}),
	})
}

func codeSparseVectorParams() *qdrant.SparseVectorParams {
	return &qdrant.SparseVectorParams{Modifier: qdrant.Modifier_Idf.Enum()}
}

func buildFilter(filters map[string]string) *qdrant.Filter {
	conds := make([]*qdrant.Condition, 0, len(filters))
	hasPath := false
	for k, v := range filters {
		if v == "" {
			continue
		}
		conds = append(conds, qdrant.NewMatch(k, v))
		if k == "kind" && v == "code_chunk" {
			hasPath = true
		}
	}
	f := &qdrant.Filter{Must: conds}
	if hasPath {
		f.MustNot = []*qdrant.Condition{qdrant.NewMatchText("path", ".codeloom")}
	}
	if len(f.Must) == 0 && len(f.MustNot) == 0 {
		return nil
	}
	return f
}

func buildSemanticFilter(filter SemanticFilter) *qdrant.Filter {
	f := buildFilter(filter.Keywords)
	if f == nil {
		f = &qdrant.Filter{}
	}
	for field, values := range filter.AnyInteger {
		if len(values) > 0 {
			f.Must = append(f.Must, qdrant.NewMatchInts(field, values...))
		}
	}
	if len(f.Must) == 0 && len(f.MustNot) == 0 {
		return nil
	}
	return f
}

func pointsToHits(res []*qdrant.ScoredPoint, kind SemanticScoreKind) []SemanticHit {
	hits := make([]SemanticHit, 0, len(res))
	for _, p := range res {
		hit := SemanticHit{
			ID:        p.GetId().GetUuid(),
			Score:     p.GetScore(),
			ScoreKind: kind,
			Payload:   payloadToMap(p.GetPayload()),
		}
		if kind == SemanticScoreFusion {
			hit.FusionScore = hit.Score
		} else {
			hit.DenseScore = hit.Score
		}
		hits = append(hits, hit)
	}
	return hits
}

// groupsToHits flattens grouped query results into a flat hit list. With
// GroupSize=1 each group contributes its single best point; group order is
// best-first so the caller's top-N stays correct.
func groupsToHits(groups []*qdrant.PointGroup, kind SemanticScoreKind) []SemanticHit {
	out := make([]SemanticHit, 0, len(groups))
	for _, g := range groups {
		for _, p := range g.GetHits() {
			hit := SemanticHit{
				ID:        p.GetId().GetUuid(),
				Score:     p.GetScore(),
				ScoreKind: kind,
				Payload:   payloadToMap(p.GetPayload()),
			}
			if kind == SemanticScoreFusion {
				hit.FusionScore = hit.Score
			} else {
				hit.DenseScore = hit.Score
			}
			out = append(out, hit)
		}
	}
	return out
}

func (q *Qdrant) Search(ctx context.Context, vector []float32, filters map[string]string, limit int, groupKey string) ([]SemanticHit, error) {
	return q.search(ctx, vector, buildFilter(filters), limit, groupKey)
}

func (q *Qdrant) SearchFiltered(ctx context.Context, vector []float32, filter SemanticFilter, limit int, groupKey string) ([]SemanticHit, error) {
	return q.search(ctx, vector, buildSemanticFilter(filter), limit, groupKey)
}

func (q *Qdrant) search(ctx context.Context, vector []float32, filter *qdrant.Filter, limit int, groupKey string) ([]SemanticHit, error) {
	limU := uint64(limit)
	if groupKey != "" {
		groupSize := uint64(1)
		groups, err := q.client.QueryGroups(ctx, &qdrant.QueryPointGroups{
			CollectionName: q.collection,
			Query:          qdrant.NewQuery(vector...),
			Filter:         filter,
			Limit:          &limU,
			GroupBy:        groupKey,
			GroupSize:      &groupSize,
			WithPayload:    qdrant.NewWithPayload(true),
		})
		if err != nil {
			log.WarnfCtx(ctx, "[qdrant] query groups (field=%q) failed, falling back to non-grouped: %v", groupKey, err)
		} else {
			return groupsToHits(groups, SemanticScoreDense), nil
		}
	}
	res, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.collection,
		Query:          qdrant.NewQuery(vector...),
		Filter:         filter,
		Limit:          &limU,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, err
	}
	return pointsToHits(res, SemanticScoreDense), nil
}

func (q *Qdrant) CountByFilter(ctx context.Context, filters map[string]string) (int, error) {
	exact := true
	n, err := q.client.Count(ctx, &qdrant.CountPoints{
		CollectionName: q.collection,
		Filter:         buildFilter(filters),
		Exact:          &exact,
	})
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (q *Qdrant) SearchHybrid(ctx context.Context, vector []float32, sparseIndices []uint32, sparseValues []float32, filters map[string]string, limit int, groupKey string) ([]SemanticHit, error) {
	filter := buildFilter(filters)
	limU := uint64(limit)
	denseLim := uint64(limit * 3)
	denseQuery := &qdrant.PrefetchQuery{
		Query: qdrant.NewQueryDense(vector),
		Using: stringPtr(""),
		Limit: &denseLim,
	}
	sparseQuery := &qdrant.PrefetchQuery{
		Query: qdrant.NewQuerySparse(sparseIndices, sparseValues),
		Using: stringPtr(codeSparseVector),
		Limit: &denseLim,
	}
	if filter != nil {
		denseQuery.Filter = filter
		sparseQuery.Filter = filter
	}
	prefetch := []*qdrant.PrefetchQuery{denseQuery, sparseQuery}
	fusion := qdrant.NewQueryFusion(qdrant.Fusion_RRF)
	withPayload := qdrant.NewWithPayload(true)
	if groupKey != "" {
		groupSize := uint64(1)
		groups, err := q.client.QueryGroups(ctx, &qdrant.QueryPointGroups{
			CollectionName: q.collection,
			Prefetch:       prefetch,
			Query:          fusion,
			Limit:          &limU,
			GroupBy:        groupKey,
			GroupSize:      &groupSize,
			WithPayload:    withPayload,
		})
		if err != nil {
			log.WarnfCtx(ctx, "[qdrant] hybrid query groups (field=%q) failed, falling back to non-grouped: %v", groupKey, err)
		} else {
			return groupsToHits(groups, SemanticScoreFusion), nil
		}
	}
	res, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.collection,
		Prefetch:       prefetch,
		Query:          fusion,
		Limit:          &limU,
		WithPayload:    withPayload,
	})
	if err != nil {
		return nil, err
	}
	return pointsToHits(res, SemanticScoreFusion), nil
}

func stringPtr(s string) *string { return &s }

// Upsert writes points into the collection.
func (q *Qdrant) Upsert(ctx context.Context, points []SemanticPoint) error {
	if len(points) == 0 {
		return nil
	}
	qp := make([]*qdrant.PointStruct, 0, len(points))
	for _, p := range points {
		ps := &qdrant.PointStruct{
			Id:      qdrant.NewID(p.ID),
			Payload: qdrant.NewValueMap(p.Payload),
		}
		if len(p.SparseIndices) > 0 && len(p.SparseValues) > 0 {
			ps.Vectors = qdrant.NewVectorsMap(map[string]*qdrant.Vector{
				"":               qdrant.NewVector(p.Vector...),
				codeSparseVector: qdrant.NewVectorSparse(p.SparseIndices, p.SparseValues),
			})
		} else {
			ps.Vectors = qdrant.NewVectors(p.Vector...)
		}
		qp = append(qp, ps)
	}
	_, err := q.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: q.collection,
		Points:         qp,
	})
	return err
}

// DeletePoints removes a bounded set of points by ID.
func (q *Qdrant) DeletePoints(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			pointIDs = append(pointIDs, qdrant.NewID(id))
		}
	}
	if len(pointIDs) == 0 {
		return nil
	}
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Points:         qdrant.NewPointsSelector(pointIDs...),
	})
	return err
}

// DeleteByRepo removes all points whose payload repo matches.
func (q *Qdrant) DeleteByRepo(ctx context.Context, repo string) error {
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatch("repo", repo)},
		}),
	})
	return err
}

// DeleteByFilterExcept removes matching points except the active generation.
func (q *Qdrant) DeleteByFilterExcept(ctx context.Context, filters, except map[string]string) error {
	must := make([]*qdrant.Condition, 0, len(filters))
	mustNot := make([]*qdrant.Condition, 0, len(except))
	for key, value := range filters {
		if value != "" {
			must = append(must, qdrant.NewMatch(key, value))
		}
	}
	for key, value := range except {
		if value != "" {
			mustNot = append(mustNot, qdrant.NewMatch(key, value))
		}
	}
	if len(must) == 0 {
		return nil
	}
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must:    must,
			MustNot: mustNot,
		}),
	})
	return err
}

// DeleteRepoExceptGeneration removes stale points only after a complete new
// repository generation has been written.
func (q *Qdrant) DeleteRepoExceptGeneration(ctx context.Context, repo, generation string) error {
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must:    []*qdrant.Condition{qdrant.NewMatch("repo", repo)},
			MustNot: []*qdrant.Condition{qdrant.NewMatch("index_generation", generation)},
		}),
	})
	return err
}

// DeleteByDocID removes all points whose payload doc_id matches.
func (q *Qdrant) DeleteByDocID(ctx context.Context, docID string) error {
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatch("doc_id", docID)},
		}),
	})
	return err
}

func payloadToMap(p map[string]*qdrant.Value) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		switch v.GetKind().(type) {
		case *qdrant.Value_IntegerValue:
			out[k] = v.GetIntegerValue()
		case *qdrant.Value_DoubleValue:
			out[k] = v.GetDoubleValue()
		case *qdrant.Value_BoolValue:
			out[k] = v.GetBoolValue()
		default:
			out[k] = v.GetStringValue()
		}
	}
	return out
}
