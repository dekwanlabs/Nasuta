// Package contract defines the backend-neutral behavior every semantic.Store
// implementation must satisfy. Run drives a constructed store through the
// scenarios listed in docs/design/nasuta-semantic-provider-proposal.zh-CN.md §11.
//
// The scenarios use only fields in the Milvus scalar whitelist
// (kind/repo/service_name/doc_id/path/lang/status/index_generation/user_id)
// so the same suite is meaningful on both Qdrant and Milvus. Point IDs are
// UUID-shaped because Qdrant only accepts UUID or uint64 point ids.
package contract

import (
	"context"
	"fmt"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/semantic"
)

const denseDim = 8

// Run exercises a constructed semantic.Store against the shared contract.
// The store must already be bound to a collection (Ensure is called here with an
// empty Collection so the configured collection is reused). Each subtest is
// self-contained: it purges prior contract points, seeds its own, and cleans up.
func Run(t *testing.T, store semantic.Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("ensure_idempotent", func(t *testing.T) {
		if err := store.Ensure(ctx, semantic.Schema{DenseDim: denseDim}); err != nil {
			t.Fatalf("first Ensure: %v", err)
		}
		if err := store.Ensure(ctx, semantic.Schema{DenseDim: denseDim}); err != nil {
			t.Fatalf("second Ensure: %v", err)
		}
	})

	t.Run("dense_search", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecA, Filter: keyword("kind", "runbook"), Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(hits) == 0 || hits[0].Metadata["repo"] != repoA {
			t.Fatalf("top hit = %+v, want repo %q", firstMeta(hits), repoA)
		}
		if hits[0].ID != id("a1") {
			t.Fatalf("top hit id = %q, want %q", hits[0].ID, id("a1"))
		}
	})

	t.Run("hybrid_search", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecB, SparseVector: sparseB, Filter: keyword("kind", "runbook"), Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(hits) == 0 || hits[0].ID != id("a2") {
			t.Fatalf("hybrid top hit = %+v, want id %q", firstMeta(hits), id("a2"))
		}
		if hits[0].ScoreKind != semantic.ScoreFusion {
			t.Fatalf("hybrid score kind = %q, want %q", hits[0].ScoreKind, semantic.ScoreFusion)
		}
	})

	t.Run("keyword_filter", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecC, Filter: keyword("repo", repoB), Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(hits) == 0 || len(hits) > 2 {
			t.Fatalf("keyword filter returned %d hits, want 1-2", len(hits))
		}
		for _, h := range hits {
			if h.Metadata["repo"] != repoB {
				t.Fatalf("filter leaked repo %q into results", h.Metadata["repo"])
			}
		}
		if hits[0].ID != id("b1") {
			t.Fatalf("keyword filter top = %q, want %q", hits[0].ID, id("b1"))
		}
	})

	t.Run("integer_filter", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecA, Filter: semantic.Filter{AnyInteger: map[string][]int64{"user_id": {11}}}, Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(hits) != 1 || hits[0].ID != id("a1") {
			t.Fatalf("integer filter hits = %+v, want only %q", firstMeta(hits), id("a1"))
		}
	})

	t.Run("group_by", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecA, GroupBy: "repo", Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		seen := map[string]struct{}{}
		for _, h := range hits {
			seen[fmt.Sprint(h.Metadata["repo"])] = struct{}{}
		}
		if len(hits) != 2 {
			t.Fatalf("group_by returned %d hits, want exactly 2 (one per repo)", len(hits))
		}
		if len(seen) != 2 {
			t.Fatalf("group_by returned duplicate repos: %v", seen)
		}
	})

	t.Run("delete_by_id", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.Delete(ctx, semantic.DeleteQuery{IDs: []string{id("a1")}}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		hits, err := store.Search(ctx, semantic.Query{
			DenseVector: vecA, Filter: keyword("kind", "runbook"), Limit: 5,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, h := range hits {
			if h.ID == id("a1") {
				t.Fatalf("deleted id %q still present", id("a1"))
			}
		}
		if len(hits) == 0 {
			t.Fatalf("Delete removed too much; expected remaining runbook points")
		}
	})

	t.Run("delete_by_repo", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.Delete(ctx, semantic.DeleteQuery{Repository: repoA}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n, err := store.Count(ctx, keyword("repo", repoA)); err != nil || n != 0 {
			t.Fatalf("count(repo=%q) = (%d, %v), want 0", repoA, n, err)
		}
		if n, err := store.Count(ctx, keyword("repo", repoB)); err != nil || n != 2 {
			t.Fatalf("count(repo=%q) = (%d, %v), want 2 (delete must be scoped)", repoB, n, err)
		}
	})

	t.Run("delete_by_doc_id", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.Delete(ctx, semantic.DeleteQuery{Filter: keyword("doc_id", "doc3")}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n, err := store.Count(ctx, keyword("doc_id", "doc3")); err != nil || n != 0 {
			t.Fatalf("count(doc_id=doc3) = (%d, %v), want 0", n, err)
		}
		if n, err := store.Count(ctx, keyword("repo", repoB)); err != nil || n != 1 {
			t.Fatalf("count(repo=%q) = (%d, %v), want 1", repoB, n, err)
		}
	})

	t.Run("delete_repo_except_generation", func(t *testing.T) {
		purge(t, ctx, store)
		pts := testPoints()
		pts = append(pts, semantic.Record{
			ID: id("b3"), DenseVector: vecA, SparseVector: sparseA,
			Metadata: meta("code_chunk", repoB, "svcB", "doc5", "c2.py", "py", "g1", 23),
		})
		if err := store.Upsert(ctx, pts); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.Delete(ctx, semantic.DeleteQuery{
			Repository: repoB,
			Except:     keyword("index_generation", "g2"),
		}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n, err := store.Count(ctx, mergeKeywords(keyword("repo", repoB), keyword("index_generation", "g1"))); err != nil || n != 0 {
			t.Fatalf("count(repo=%q,gen=g1) = (%d, %v), want 0 (stale generation pruned)", repoB, n, err)
		}
		if n, err := store.Count(ctx, mergeKeywords(keyword("repo", repoB), keyword("index_generation", "g2"))); err != nil || n != 2 {
			t.Fatalf("count(repo=%q,gen=g2) = (%d, %v), want 2 (active generation kept)", repoB, n, err)
		}
	})

	t.Run("count_filtered", func(t *testing.T) {
		purge(t, ctx, store)
		if err := store.Upsert(ctx, testPoints()); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if n, err := store.Count(ctx, keyword("kind", "runbook")); err != nil || n != 2 {
			t.Fatalf("count(kind=runbook) = (%d, %v), want 2", n, err)
		}
		if n, err := store.Count(ctx, keyword("kind", "code_chunk")); err != nil || n != 2 {
			t.Fatalf("count(kind=code_chunk) = (%d, %v), want 2", n, err)
		}
		if err := store.Delete(ctx, semantic.DeleteQuery{Repository: repoA}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if n, err := store.Count(ctx, keyword("kind", "runbook")); err != nil || n != 0 {
			t.Fatalf("count(kind=runbook) after delete = (%d, %v), want 0", n, err)
		}
	})
}

// purge removes any leftover contract points so each subtest starts clean and
// repeated runs are idempotent. Both backends treat an empty result delete as
// a no-op since Repository keeps the predicate bounded.
func purge(t *testing.T, ctx context.Context, store semantic.Store) {
	t.Helper()
	for _, repo := range []string{repoA, repoB} {
		if err := store.Delete(ctx, semantic.DeleteQuery{Repository: repo}); err != nil {
			t.Fatalf("purge repo %q: %v", repo, err)
		}
	}
}

func firstMeta(hits []semantic.Hit) map[string]any {
	if len(hits) == 0 {
		return nil
	}
	return hits[0].Metadata
}

func keyword(k, v string) semantic.Filter {
	return semantic.Filter{Keywords: map[string]string{k: v}}
}

func mergeKeywords(a, b semantic.Filter) semantic.Filter {
	out := semantic.Filter{Keywords: map[string]string{}, AnyInteger: map[string][]int64{}}
	for k, v := range a.Keywords {
		out.Keywords[k] = v
	}
	for k, v := range b.Keywords {
		out.Keywords[k] = v
	}
	for k, v := range a.AnyInteger {
		out.AnyInteger[k] = v
	}
	for k, v := range b.AnyInteger {
		out.AnyInteger[k] = v
	}
	return out
}
