package contract

import "github.com/dekwanlabs/nasuta/internal/semantic"

// Contract repos are stable strings so purge can delete by Repository across
// runs and the same collection can be reused without leftover state.
const (
	repoA = "contract_a"
	repoB = "contract_b"
)

// Orthogonal unit vectors keep cosine scores unambiguous: a query vector matches
// exactly one seeded point at cosine 1 and is orthogonal to the rest (cosine 0).
var (
	vecA = []float32{1, 0, 0, 0, 0, 0, 0, 0}
	vecB = []float32{0, 1, 0, 0, 0, 0, 0, 0}
	vecC = []float32{0, 0, 1, 0, 0, 0, 0, 0}
	vecD = []float32{0, 0, 0, 1, 0, 0, 0, 0}

	sparseA = &semantic.SparseVector{Indices: []uint32{1}, Values: []float32{1}}
	sparseB = &semantic.SparseVector{Indices: []uint32{2}, Values: []float32{1}}
	sparseC = &semantic.SparseVector{Indices: []uint32{3}, Values: []float32{1}}
	sparseD = &semantic.SparseVector{Indices: []uint32{4}, Values: []float32{1}}
)

// id is a deterministic UUID-shaped point id. Qdrant accepts only UUID or uint64
// point ids, so the contract must not use arbitrary strings.
func id(seed string) string {
	return "00000000-0000-5000-8000-0000000000" + seed
}

// meta builds a metadata map using only fields in the Milvus scalar whitelist,
// so the same record is filterable on both backends.
func meta(kind, repo, service, doc, path, lang, gen string, userID int64) map[string]any {
	return map[string]any{
		"kind":             kind,
		"repo":             repo,
		"service_name":     service,
		"doc_id":           doc,
		"path":             path,
		"lang":             lang,
		"status":           "active",
		"index_generation": gen,
		"user_id":          userID,
	}
}

func testPoints() []semantic.Record {
	return []semantic.Record{
		{ID: id("a1"), DenseVector: vecA, SparseVector: sparseA, Metadata: meta("runbook", repoA, "svcA", "doc1", "a.go", "go", "g1", 11)},
		{ID: id("a2"), DenseVector: vecB, SparseVector: sparseB, Metadata: meta("runbook", repoA, "svcA", "doc2", "b.go", "go", "g1", 12)},
		{ID: id("b1"), DenseVector: vecC, SparseVector: sparseC, Metadata: meta("code_chunk", repoB, "svcB", "doc3", "c.py", "py", "g2", 21)},
		{ID: id("b2"), DenseVector: vecD, SparseVector: sparseD, Metadata: meta("code_chunk", repoB, "svcB", "doc4", "d.py", "py", "g2", 22)},
	}
}
