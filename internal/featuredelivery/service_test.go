package featuredelivery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

type generationFailureStore struct {
	Store
	feature      FeatureRequest
	parent       Artifact
	created      GenerationRun
	finished     GenerationRun
	finishErr    error
	finishCalled bool
}

func (store *generationFailureStore) GetFeature(_ context.Context, id string) (*FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *generationFailureStore) GetCurrentLineage(context.Context, string) (Lineage, error) {
	parent := store.parent
	return Lineage{Requirement: &parent}, nil
}

func (store *generationFailureStore) CreateGenerationRun(_ context.Context, run GenerationRun) error {
	store.created = run
	return nil
}

func (store *generationFailureStore) FinishGenerationRun(_ context.Context, id, status string, inputTokens, outputTokens int64, summary string) error {
	store.finishCalled = true
	store.finished = GenerationRun{
		ID: id, Status: status, InputTokens: inputTokens, OutputTokens: outputTokens, ErrorSummary: summary,
	}
	return store.finishErr
}

func TestNormalizeRepository(t *testing.T) {
	valid := map[string]string{
		"team/nasuta":    "team/nasuta",
		" team/nasuta/ ": "team/nasuta",
		"nasuta":         "nasuta",
	}
	for input, want := range valid {
		got, err := NormalizeRepository(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeRepository(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{
		"", "/tmp/repo", `team\nasuta`, "team/../nasuta", "team//nasuta", ".", "team/\x00nasuta",
		string(make([]byte, maxRepository+1)),
	} {
		if got, err := NormalizeRepository(input); err == nil {
			t.Fatalf("NormalizeRepository(%q) = %q, want error", input, got)
		}
	}
}

func TestGenerateArtifactReturnsFailedRunAndPersistenceError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider rejected request", http.StatusBadRequest)
	}))
	defer server.Close()

	persistenceErr := errors.New("generation terminal write failed")
	store := &generationFailureStore{
		feature:   FeatureRequest{ID: "feat-1", Title: "Add feature", CreatedBy: 7},
		parent:    Artifact{ID: "art-requirement", RequestID: "feat-1", Kind: KindRequirement},
		finishErr: persistenceErr,
	}
	client := llm.NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, server.Client())
	service := NewService(store, NewGenerator(nil, client, "openai", "model", 100), time.Second)

	artifact, run, err := service.GenerateArtifact(context.Background(), "feat-1", KindRequirementAnalysis, 7, false)

	if artifact != nil || err == nil || !errors.Is(err, persistenceErr) {
		t.Fatalf("artifact=%v run=%+v err=%v", artifact, run, err)
	}
	if run == nil || run.Status != "failed" || run.EndedAt == nil || run.ErrorSummary == "" {
		t.Fatalf("failed run=%+v", run)
	}
	if store.created.Status != "running" || !store.finishCalled || store.finished.Status != "failed" {
		t.Fatalf("created=%+v finished=%+v called=%t", store.created, store.finished, store.finishCalled)
	}
}
