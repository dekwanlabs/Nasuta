package contract

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/dekwanlabs/nasuta/internal/semantic"
)

// Memory is a process-local reference implementation of semantic.Store used to
// exercise Run without a live backend. It mirrors the scoring semantics of the
// real adapters (cosine dense, dot-product sparse, RRF fusion) closely enough
// to validate contract logic; it is not a production store.
type Memory struct {
	mu     sync.Mutex
	points map[string]semantic.Record
}

func NewMemory() *Memory { return &Memory{points: map[string]semantic.Record{}} }

func (*Memory) Ensure(context.Context, semantic.Schema) error { return nil }
func (*Memory) Capabilities() semantic.Capabilities           { return semantic.RequiredCapabilities() }
func (*Memory) Close() error                                  { return nil }
func (m *Memory) Count(_ context.Context, f semantic.Filter) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, rec := range m.points {
		if matchesFilter(rec.Metadata, f) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) Upsert(_ context.Context, records []semantic.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range records {
		m.points[rec.ID] = cloneRecord(rec)
	}
	return nil
}

func (m *Memory) Search(_ context.Context, query semantic.Query) ([]semantic.Hit, error) {
	if query.Limit <= 0 {
		return nil, fmt.Errorf("memory: search limit must be positive")
	}
	m.mu.Lock()
	candidates := make([]semantic.Record, 0, len(m.points))
	for _, rec := range m.points {
		if matchesFilter(rec.Metadata, query.Filter) {
			candidates = append(candidates, rec)
		}
	}
	m.mu.Unlock()

	if query.SparseVector != nil {
		return hybridSearch(candidates, query), nil
	}
	return denseSearch(candidates, query), nil
}

func (m *Memory) Delete(_ context.Context, query semantic.DeleteQuery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(query.IDs) > 0 {
		for _, id := range query.IDs {
			delete(m.points, id)
		}
		return nil
	}
	filter := query.Filter
	filter.Keywords = copyKeywords(filter.Keywords)
	if query.Repository != "" {
		filter.Keywords["repo"] = query.Repository
	}
	if query.DocumentID != "" {
		filter.Keywords["doc_id"] = query.DocumentID
	}
	exceptActive := len(query.Except.Keywords) > 0 || len(query.Except.AnyInteger) > 0
	for id, rec := range m.points {
		if !matchesFilter(rec.Metadata, filter) {
			continue
		}
		if exceptActive && matchesFilter(rec.Metadata, query.Except) {
			continue
		}
		delete(m.points, id)
	}
	return nil
}

func denseSearch(candidates []semantic.Record, query semantic.Query) []semantic.Hit {
	type entry struct {
		rec   semantic.Record
		score float32
	}
	items := make([]entry, 0, len(candidates))
	for _, rec := range candidates {
		items = append(items, entry{rec: rec, score: cosine(query.DenseVector, rec.DenseVector)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
	hits := make([]semantic.Hit, 0, len(items))
	for _, e := range items {
		hits = append(hits, semantic.Hit{
			ID: e.rec.ID, Score: e.score, ScoreKind: semantic.ScoreDense,
			DenseScore: e.score, Metadata: copyMeta(e.rec.Metadata),
		})
	}
	return limitOrGroup(hits, query)
}

func hybridSearch(candidates []semantic.Record, query semantic.Query) []semantic.Hit {
	type entry struct {
		rec         semantic.Record
		denseScore  float32
		sparseScore float32
	}
	items := make([]entry, 0, len(candidates))
	for _, rec := range candidates {
		items = append(items, entry{
			rec:         rec,
			denseScore:  cosine(query.DenseVector, rec.DenseVector),
			sparseScore: sparseDot(query.SparseVector, rec.SparseVector),
		})
	}
	const rrfK = 60.0
	byDense := append([]entry(nil), items...)
	sort.SliceStable(byDense, func(i, j int) bool { return byDense[i].denseScore > byDense[j].denseScore })
	bySparse := append([]entry(nil), items...)
	sort.SliceStable(bySparse, func(i, j int) bool { return bySparse[i].sparseScore > bySparse[j].sparseScore })
	denseRank := make(map[string]int, len(items))
	for i, e := range byDense {
		denseRank[e.rec.ID] = i
	}
	sparseRank := make(map[string]int, len(items))
	for i, e := range bySparse {
		sparseRank[e.rec.ID] = i
	}
	type fused struct {
		rec semantic.Record
		rrf float32
	}
	fusedHits := make([]fused, 0, len(items))
	for _, e := range items {
		rrf := float32(1.0/(rrfK+float64(denseRank[e.rec.ID])) + 1.0/(rrfK+float64(sparseRank[e.rec.ID])))
		fusedHits = append(fusedHits, fused{rec: e.rec, rrf: rrf})
	}
	sort.SliceStable(fusedHits, func(i, j int) bool { return fusedHits[i].rrf > fusedHits[j].rrf })
	hits := make([]semantic.Hit, 0, len(fusedHits))
	for _, f := range fusedHits {
		hits = append(hits, semantic.Hit{
			ID: f.rec.ID, Score: f.rrf, ScoreKind: semantic.ScoreFusion,
			FusionScore: f.rrf, Metadata: copyMeta(f.rec.Metadata),
		})
	}
	return limitOrGroup(hits, query)
}

func limitOrGroup(hits []semantic.Hit, query semantic.Query) []semantic.Hit {
	if query.GroupBy != "" {
		seen := map[string]struct{}{}
		out := make([]semantic.Hit, 0, len(hits))
		for _, h := range hits {
			group := fmt.Sprint(h.Metadata[query.GroupBy])
			if _, ok := seen[group]; ok {
				continue
			}
			seen[group] = struct{}{}
			out = append(out, h)
			if len(out) == query.Limit {
				break
			}
		}
		return out
	}
	if len(hits) > query.Limit {
		hits = hits[:query.Limit]
	}
	return hits
}

func matchesFilter(meta map[string]any, f semantic.Filter) bool {
	for k, want := range f.Keywords {
		if fmt.Sprint(meta[k]) != want {
			return false
		}
	}
	for k, values := range f.AnyInteger {
		got := toInt64(meta[k])
		found := false
		for _, v := range values {
			if got == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func cosine(a, b []float32) float32 {
	var dot, na, nb float32
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(na))*math.Sqrt(float64(nb)))
}

func sparseDot(query, candidate *semantic.SparseVector) float32 {
	if query == nil || candidate == nil || len(query.Indices) == 0 || len(candidate.Indices) == 0 {
		return 0
	}
	lookup := make(map[uint32]float32, len(candidate.Indices))
	for i, idx := range candidate.Indices {
		lookup[idx] = candidate.Values[i]
	}
	var sum float32
	for i, idx := range query.Indices {
		if v, ok := lookup[idx]; ok {
			sum += query.Values[i] * v
		}
	}
	return sum
}

func toInt64(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case int32:
		return int64(v)
	default:
		return 0
	}
}

func cloneRecord(rec semantic.Record) semantic.Record {
	out := rec
	out.DenseVector = append([]float32(nil), rec.DenseVector...)
	if rec.SparseVector != nil {
		out.SparseVector = &semantic.SparseVector{
			Indices: append([]uint32(nil), rec.SparseVector.Indices...),
			Values:  append([]float32(nil), rec.SparseVector.Values...),
		}
	}
	out.Metadata = copyMeta(rec.Metadata)
	return out
}

func copyMeta(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

func copyKeywords(source map[string]string) map[string]string {
	out := make(map[string]string, len(source)+2)
	for k, v := range source {
		out[k] = v
	}
	return out
}
