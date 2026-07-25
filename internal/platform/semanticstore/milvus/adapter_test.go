package milvus

import (
	"context"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/semantic"
)

func TestCompileFilterUsesOnlyDeclaredScalarFields(t *testing.T) {
	expr, err := compileFilter(semantic.Filter{
		Keywords:   map[string]string{"lang": "go", "status": "active"},
		AnyInteger: map[string][]int64{"user_id": {42, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`lang == "go"`, `status == "active"`, "user_id in [42,0]"} {
		if !strings.Contains(expr, want) {
			t.Fatalf("expression %q missing %q", expr, want)
		}
	}
}

func TestCompileFilterSupportsSessionHistoryMetadata(t *testing.T) {
	expr, err := compileFilter(semantic.Filter{
		Keywords:   map[string]string{"session_id": "qa-1"},
		AnyInteger: map[string][]int64{"turn_number": {7, 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`metadata["session_id"] == "qa-1"`, `metadata["turn_number"] in [7,8]`} {
		if !strings.Contains(expr, want) {
			t.Fatalf("expression %q missing %q", expr, want)
		}
	}
}

func TestSearchRejectsInvalidSparseVectorBeforeCallingMilvus(t *testing.T) {
	_, err := (&Adapter{}).Search(context.Background(), semantic.Query{
		DenseVector: []float32{1}, SparseVector: &semantic.SparseVector{}, Limit: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid sparse vector") {
		t.Fatalf("error = %v, want invalid sparse vector", err)
	}
}

func TestCompileDeleteRefusesUnboundedDelete(t *testing.T) {
	if _, err := compileDelete(semantic.DeleteQuery{}); err == nil {
		t.Fatal("unbounded delete unexpectedly accepted")
	}
}

func TestCompileDeleteExcludesGeneration(t *testing.T) {
	expr, err := compileDelete(semantic.DeleteQuery{
		Repository: "team/orders",
		Except:     semantic.Filter{Keywords: map[string]string{"index_generation": "new"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expr, `repo == "team/orders"`) || !strings.Contains(expr, `!(index_generation == "new")`) {
		t.Fatalf("delete expression = %q", expr)
	}
}
