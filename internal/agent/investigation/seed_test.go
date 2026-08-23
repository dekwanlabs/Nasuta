package investigation

import (
	"context"
	"testing"
	"time"
)

func TestCoordinatorAdmitsSeedEvidenceForVerification(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	seed, err := normalizeSeedEvidence("", EvidenceUnit{
		SourceKind: "code", Target: "service-a", Section: "L10-L20", ContentHash: "seed-hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{
			Output: []byte(`{"verified":true}`),
			Claims: []ClaimCandidate{{
				GoalID: "g1", Text: "seed evidence supports the entrypoint", Status: ClaimSupported,
				EvidenceRefs: []EvidenceRef{{
					EvidenceID: seed.ID, SourceKind: seed.SourceKind, Target: seed.Target, ContentHash: seed.ContentHash,
				}},
			}},
		}, nil
	})
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:   catalog,
		Schemas:   testSchemas(),
		Store:     NewMemoryRunStore(),
		Executors: testExecutors(executor),
		Composer: ComposerFunc(func(_ context.Context, _ InvestigationContract, report InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{Text: "verified from seed", Status: DeliverySucceeded, ClaimIDs: []string{report.Claims[0].ID}}, nil
		}),
		BudgetLimit: BudgetVector{},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.SeedEvidence = []EvidenceUnit{seed}
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || len(run.Report.Evidence) != 1 || run.Report.Evidence[0].Content != "" {
		t.Fatalf("run = %#v", run)
	}
	if len(run.Report.Claims) != 1 || run.Report.Claims[0].Status != ClaimSupported {
		t.Fatalf("claims = %#v", run.Report.Claims)
	}
}
