package featuredelivery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

type manualArtifactStore struct {
	Store
	feature FeatureRequest
	parent  Artifact
	base    Artifact
	created Artifact
}

type artifactReviewStore struct {
	Store
	feature  FeatureRequest
	parent   Artifact
	artifact Artifact
}

func (store *artifactReviewStore) GetFeature(_ context.Context, id string) (*FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *artifactReviewStore) GetCurrentLineage(context.Context, string) (Lineage, error) {
	parent := store.parent
	return Lineage{Requirement: &parent}, nil
}

func (store *artifactReviewStore) GetArtifact(_ context.Context, id string) (*Artifact, error) {
	if id != store.artifact.ID {
		return nil, ErrNotFound
	}
	artifact := store.artifact
	return &artifact, nil
}

func (store *manualArtifactStore) GetFeature(_ context.Context, id string) (*FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *manualArtifactStore) GetCurrentLineage(context.Context, string) (Lineage, error) {
	parent := store.parent
	return Lineage{RequirementAnalysis: &parent}, nil
}

func (store *manualArtifactStore) GetArtifact(_ context.Context, id string) (*Artifact, error) {
	if id != store.base.ID {
		return nil, ErrNotFound
	}
	base := store.base
	return &base, nil
}

func (store *manualArtifactStore) CreateArtifact(_ context.Context, artifact Artifact) (*Artifact, error) {
	store.created = artifact
	return &artifact, nil
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

func TestAddArtifactInheritsValidatedEvidenceSnapshot(t *testing.T) {
	store := &manualArtifactStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		parent:  Artifact{ID: "analysis-1", RequestID: "feat-1", Kind: KindRequirementAnalysis},
		base: Artifact{
			ID: "proposal-1", RequestID: "feat-1", Kind: KindTechnicalProposal, ParentArtifactID: "analysis-1",
			Evidence: []EvidenceRef{{Kind: "code", Path: "service.go", Summary: "Existing behavior", Hash: "hash-1"}},
		},
	}
	service := NewService(store, nil, time.Second)
	document := []byte(`{
		"current_technical_baseline":[{"statement":"The current path is synchronous","classification":"fact","evidence_ids":[0]}],
		"architecture_drivers":["Reduce latency"],
		"candidate_architectures":[
			{"name":"A","summary":"Keep it","architecture_pattern":"modular monolith","communication_pattern":"calls","data_pattern":"crud","deployment_pattern":"binary","contract_pattern":"api","migration_pattern":"expand-contract","reliability_pattern":"timeouts","observability_pattern":"logs","benefits":[],"costs":[],"risks":[],"reversibility":[]},
			{"name":"B","summary":"Change it","architecture_pattern":"service","communication_pattern":"events","data_pattern":"owned data","deployment_pattern":"container","contract_pattern":"events","migration_pattern":"parallel run","reliability_pattern":"retries","observability_pattern":"traces","benefits":[],"costs":[],"risks":[],"reversibility":[]}
		],
		"technical_decision":{"selected_option":"B","rationale":"Lower latency","accepted_tradeoffs":["More operations"]}
	}`)

	artifact, err := service.AddArtifact(
		context.Background(), "feat-1", KindTechnicalProposal, "analysis-1", "proposal-1", document, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Evidence) != 1 || artifact.Evidence[0].Hash != "hash-1" {
		t.Fatalf("evidence = %+v", artifact.Evidence)
	}
	if store.created.Origin != OriginUser || store.created.ParentArtifactID != "analysis-1" {
		t.Fatalf("created artifact = %+v", store.created)
	}
}

func TestAddArtifactRejectsForeignEvidenceSnapshot(t *testing.T) {
	store := &manualArtifactStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		parent:  Artifact{ID: "analysis-1", RequestID: "feat-1", Kind: KindRequirementAnalysis},
		base: Artifact{
			ID: "proposal-foreign", RequestID: "feat-2", Kind: KindTechnicalProposal, ParentArtifactID: "analysis-1",
		},
	}
	service := NewService(store, nil, time.Second)

	_, err := service.AddArtifact(
		context.Background(), "feat-1", KindTechnicalProposal, "analysis-1", "proposal-foreign", []byte(`{}`), 7, false,
	)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestReviewArtifactExplainsUnresolvedBlockingQuestions(t *testing.T) {
	store := &artifactReviewStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		parent:  Artifact{ID: "requirement-1", RequestID: "feat-1", Kind: KindRequirement},
		artifact: Artifact{
			ID: "analysis-1", RequestID: "feat-1", Kind: KindRequirementAnalysis,
			ParentArtifactID: "requirement-1",
			DocumentJSON: []byte(`{
				"problem_statement":"Customers need export",
				"goals":["Enable export"],
				"functional_requirements":["Customers can request an export"],
				"acceptance_criteria":["A requested export is available"],
				"blocking_questions":["Which languages?","What range?"]
			}`),
		},
	}
	service := NewService(store, nil, time.Second)

	err := service.ReviewArtifact(
		context.Background(), "feat-1", "analysis-1", DecisionApproved, "", ReviewApprovalBinding{}, 1,
	)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	want := "artifact has 2 unresolved blocking questions; revise and clear them before approval"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want message containing %q", err, want)
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
