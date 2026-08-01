package indexer

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

type knowledgeDocStoreStub struct {
	docs  []domain.DocRecord
	err   error
	reads int
}

func (store *knowledgeDocStoreStub) ListDocsByKinds([]string) ([]domain.DocRecord, error) {
	store.reads++
	return store.docs, store.err
}

func TestRunbookFrontmatterMapsServiceRelations(t *testing.T) {
	fm := parseFrontmatter(`---
service: orders
depends_on:
  - payments
called_by: gateway
---
# Orders recovery
`)

	if service := fmString(fm.data, "service"); service != "orders" {
		t.Fatalf("service = %q, want orders", service)
	}
	if dependencies := fmStringArray(fm.data, "depends_on"); !slices.Equal(dependencies, []string{"payments"}) {
		t.Fatalf("depends_on = %#v", dependencies)
	}
	if callers := fmStringArray(fm.data, "called_by"); !slices.Equal(callers, []string{"gateway"}) {
		t.Fatalf("called_by = %#v", callers)
	}
}

func TestCanonicalRunbookKeepsNormalizedServiceAtProjectionBoundary(t *testing.T) {
	records := canonicalRunbooks([]domain.RunbookRecord{{
		ID: " recovery ", Repo: "docs", Title: " Recovery ", Path: " ./runbooks/recovery.md ",
		ServiceName: " orders ", Tags: []string{"ops", " ops ", ""},
	}})
	want := []domain.RunbookRecord{{
		ID: "recovery", Repo: "docs", Title: "Recovery", Path: "runbooks/recovery.md",
		ServiceName: "orders", Tags: []string{"ops"},
	}}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("canonical runbooks = %#v", records)
	}
}

func TestLoadKnowledgeBaseUsesOneConsistentDocumentRead(t *testing.T) {
	store := &knowledgeDocStoreStub{docs: []domain.DocRecord{{
		ID: "orders-recovery", Title: "Orders recovery", Filename: "runbooks/orders.md", Kind: domain.DocKindFlow,
		Content: "---\nid: legacy-orders-recovery\nservice: orders\ndepends_on: payments\n---\n# Orders recovery\n",
	}}}
	runbooks, dependencies, err := LoadKnowledgeBase(store)
	if err != nil {
		t.Fatal(err)
	}
	if store.reads != 1 || len(runbooks) != 1 || len(dependencies) != 1 {
		t.Fatalf("reads=%d runbooks=%d dependencies=%d", store.reads, len(runbooks), len(dependencies))
	}
	if runbooks[0].ID != "orders-recovery" {
		t.Fatalf("runbook ID = %q, want document store ID", runbooks[0].ID)
	}
	if runbooks[0].ServiceName != "orders" || dependencies[0].From != "orders" || dependencies[0].To != "payments" {
		t.Fatalf("runbook=%+v dependency=%+v", runbooks[0], dependencies[0])
	}
}

func TestLoadKnowledgeBaseDoesNotTurnReadFailureIntoEmptySnapshot(t *testing.T) {
	store := &knowledgeDocStoreStub{err: errors.New("mysql unavailable")}
	if _, _, err := LoadKnowledgeBase(store); err == nil {
		t.Fatal("knowledge-base read failure was treated as an empty snapshot")
	}
}
