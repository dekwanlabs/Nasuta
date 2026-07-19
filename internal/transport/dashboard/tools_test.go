package dashboard

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/semantic"
)

type semanticStatusStore struct {
	count int
}

func (semanticStatusStore) Ensure(context.Context, semantic.Schema) error { return nil }
func (semanticStatusStore) Search(context.Context, semantic.Query) ([]semantic.Hit, error) {
	return nil, nil
}
func (semanticStatusStore) Upsert(context.Context, []semantic.Record) error    { return nil }
func (semanticStatusStore) Delete(context.Context, semantic.DeleteQuery) error { return nil }
func (semanticStatusStore) Close() error                                       { return nil }

func (store semanticStatusStore) Count(context.Context, semantic.Filter) (int, error) {
	return store.count, nil
}

func (semanticStatusStore) Capabilities() semantic.Capabilities {
	return semantic.RequiredCapabilities()
}

func TestAPISemanticStatusUsesProviderNeutralResponse(t *testing.T) {
	handler := &Handler{
		semantic: semanticStatusStore{count: 17},
		cfg: config.Config{Semantic: config.SemanticConfig{
			Provider: "milvus", Collection: "knowledge",
		}},
	}
	recorder := httptest.NewRecorder()
	handler.APISemanticStatus(recorder, httptest.NewRequest("GET", "/api/semantic/status", nil))

	body := recorder.Body.String()
	for _, want := range []string{`"provider":"milvus"`, `"collection":"knowledge"`, `"vectorCount":17`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body %s missing %s", body, want)
		}
	}
}
