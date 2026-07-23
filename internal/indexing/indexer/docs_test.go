package indexer

import (
	"slices"
	"testing"
)

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
