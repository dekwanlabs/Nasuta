package run

import (
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestPutEvidenceLedgerIsIdempotentAndDetectsConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := Bind(db)
	unit := testEvidenceUnit("retrieval", "doc-a", strings.Repeat("a", 64))
	artifact, err := NewEvidenceLedgerArtifact("run-parent", []tool.EvidenceUnit{unit})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WithArgs(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM agent_run_artifacts").
		WithArgs("run-parent", EvidenceLedgerArtifactKind).
		WillReturnRows(runArtifactRows().AddRow(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
		))

	persisted, err := store.PutEvidenceLedger(
		t.Context(),
		"run-parent",
		[]tool.EvidenceUnit{unit},
	)
	if err != nil {
		t.Fatalf("PutEvidenceLedger: %v", err)
	}
	if !sameRunArtifact(persisted, artifact) {
		t.Fatalf("persisted artifact = %+v, want %+v", persisted, artifact)
	}

	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WillReturnResult(sqlmock.NewResult(0, 0))
	conflicting := artifact
	conflicting.ContentHash = strings.Repeat("f", 64)
	mock.ExpectQuery("FROM agent_run_artifacts").
		WithArgs("run-parent", EvidenceLedgerArtifactKind).
		WillReturnRows(runArtifactRows().AddRow(
			conflicting.ID,
			conflicting.RunID,
			conflicting.Kind,
			conflicting.Schema.ID,
			conflicting.Schema.Version,
			conflicting.ContentHash,
			conflicting.Content,
		))
	_, err = store.PutEvidenceLedger(
		t.Context(),
		"run-parent",
		[]tool.EvidenceUnit{unit},
	)
	if !errors.Is(err, ErrEvidenceLedgerConflict) {
		t.Fatalf("conflicting PutEvidenceLedger error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkflowEscalationEvidenceEnforcesOwnershipAndRequestOrder(
	t *testing.T,
) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := Bind(db)
	parentUnit := testEvidenceUnit(
		"retrieval",
		"doc-a",
		strings.Repeat("a", 64),
	)
	childUnit := testEvidenceUnit(
		"runtime",
		"trace-1",
		strings.Repeat("b", 64),
	)
	parentArtifact := mustEvidenceArtifact(
		t,
		"parent-1",
		[]tool.EvidenceUnit{parentUnit},
	)
	childArtifact := mustEvidenceArtifact(
		t,
		"child-1",
		[]tool.EvidenceUnit{childUnit},
	)

	mock.ExpectQuery("SELECT DISTINCT a.artifact_id").
		WithArgs(
			EvidenceLedgerArtifactKind,
			"parent-1",
			"parent-1",
			"delegation-1",
			"parent-1",
		).
		WillReturnRows(runArtifactRows().
			AddRow(
				parentArtifact.ID,
				parentArtifact.RunID,
				parentArtifact.Kind,
				parentArtifact.Schema.ID,
				parentArtifact.Schema.Version,
				parentArtifact.ContentHash,
				parentArtifact.Content,
			).
			AddRow(
				childArtifact.ID,
				childArtifact.RunID,
				childArtifact.Kind,
				childArtifact.Schema.ID,
				childArtifact.Schema.Version,
				childArtifact.ContentHash,
				childArtifact.Content,
			))

	resolved, err := store.ResolveWorkflowEscalationEvidence(
		t.Context(),
		"parent-1",
		"delegation-1",
		[]string{"runtime:trace-1", "doc-a"},
	)
	if err != nil {
		t.Fatalf("ResolveWorkflowEscalationEvidence: %v", err)
	}
	if len(resolved) != 2 ||
		resolved[0].Ref != "runtime:trace-1" ||
		resolved[0].Unit.ContentHash != childUnit.ContentHash ||
		resolved[1].Ref != "doc-a" ||
		resolved[1].Unit.ContentHash != parentUnit.ContentHash {
		t.Fatalf("resolved evidence = %+v", resolved)
	}

	mock.ExpectQuery("FROM agent_run_artifacts").
		WithArgs("parent-1", EvidenceLedgerArtifactKind).
		WillReturnRows(runArtifactRows().AddRow(
			parentArtifact.ID,
			parentArtifact.RunID,
			parentArtifact.Kind,
			parentArtifact.Schema.ID,
			parentArtifact.Schema.Version,
			parentArtifact.ContentHash,
			parentArtifact.Content,
		))
	_, err = store.ResolveWorkflowEscalationEvidence(
		t.Context(),
		"parent-1",
		"",
		[]string{"trace-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("unauthorized child evidence error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkflowEscalationEvidenceUsesStableCanonicalRefs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := Bind(db)
	contentHash := strings.Repeat("a", 64)
	overview := testEvidenceUnit("retrieval", "doc-a", contentHash)
	overview.Sections = []string{"overview"}
	failure := testEvidenceUnit("retrieval", "doc-a", contentHash)
	failure.Sections = []string{"failure"}
	artifact := mustEvidenceArtifact(
		t,
		"parent-1",
		[]tool.EvidenceUnit{overview, failure},
	)
	mock.ExpectQuery("FROM agent_run_artifacts").
		WithArgs("parent-1", EvidenceLedgerArtifactKind).
		WillReturnRows(runArtifactRows().AddRow(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
		))
	refs := []string{
		EvidenceReferenceID(failure),
		EvidenceReferenceID(overview),
	}
	resolved, err := store.ResolveWorkflowEscalationEvidence(
		t.Context(),
		"parent-1",
		"",
		refs,
	)
	if err != nil {
		t.Fatalf("ResolveWorkflowEscalationEvidence: %v", err)
	}
	if len(resolved) != 2 ||
		resolved[0].Ref != refs[0] ||
		len(resolved[0].Unit.Sections) != 1 ||
		resolved[0].Unit.Sections[0] != "failure" ||
		resolved[1].Ref != refs[1] ||
		len(resolved[1].Unit.Sections) != 1 ||
		resolved[1].Unit.Sections[0] != "overview" {
		t.Fatalf("resolved evidence = %+v", resolved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkflowEscalationEvidenceRejectsTamperingAndAliasConflict(
	t *testing.T,
) {
	t.Run("artifact hash", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		store := Bind(db)
		artifact := mustEvidenceArtifact(t, "parent-1", []tool.EvidenceUnit{
			testEvidenceUnit("retrieval", "doc-a", strings.Repeat("a", 64)),
		})
		artifact.Content = append([]byte(nil), artifact.Content...)
		artifact.Content[len(artifact.Content)-1] ^= 1
		mock.ExpectQuery("FROM agent_run_artifacts").
			WithArgs("parent-1", EvidenceLedgerArtifactKind).
			WillReturnRows(runArtifactRows().AddRow(
				artifact.ID,
				artifact.RunID,
				artifact.Kind,
				artifact.Schema.ID,
				artifact.Schema.Version,
				artifact.ContentHash,
				artifact.Content,
			))
		_, err = store.ResolveWorkflowEscalationEvidence(
			t.Context(),
			"parent-1",
			"",
			[]string{"doc-a"},
		)
		if !errors.Is(err, ErrEvidenceLedgerConflict) {
			t.Fatalf("tampered ledger error = %v", err)
		}
	})

	t.Run("alias collision", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		store := Bind(db)
		parentArtifact := mustEvidenceArtifact(t, "parent-1", []tool.EvidenceUnit{
			testEvidenceUnit("retrieval", "shared", strings.Repeat("a", 64)),
		})
		childArtifact := mustEvidenceArtifact(t, "child-1", []tool.EvidenceUnit{
			testEvidenceUnit("runtime", "shared", strings.Repeat("b", 64)),
		})
		mock.ExpectQuery("SELECT DISTINCT a.artifact_id").
			WithArgs(
				EvidenceLedgerArtifactKind,
				"parent-1",
				"parent-1",
				"delegation-1",
				"parent-1",
			).
			WillReturnRows(runArtifactRows().
				AddRow(
					parentArtifact.ID,
					parentArtifact.RunID,
					parentArtifact.Kind,
					parentArtifact.Schema.ID,
					parentArtifact.Schema.Version,
					parentArtifact.ContentHash,
					parentArtifact.Content,
				).
				AddRow(
					childArtifact.ID,
					childArtifact.RunID,
					childArtifact.Kind,
					childArtifact.Schema.ID,
					childArtifact.Schema.Version,
					childArtifact.ContentHash,
					childArtifact.Content,
				))
		_, err = store.ResolveWorkflowEscalationEvidence(
			t.Context(),
			"parent-1",
			"delegation-1",
			[]string{"shared"},
		)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("alias collision error = %v", err)
		}
	})
}

func TestGetDelegationEvidenceRequiresSettledOwnedTask(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := Bind(db)
	unit := testEvidenceUnit("runtime", "trace-1", strings.Repeat("a", 64))
	artifact := mustEvidenceArtifact(
		t,
		"child-1",
		[]tool.EvidenceUnit{unit},
	)
	mock.ExpectQuery("FROM agent_delegation_tasks t").
		WithArgs(
			EvidenceLedgerArtifactKind,
			"parent-1",
			"delegation-1",
			0,
		).
		WillReturnRows(runArtifactRows().AddRow(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
		))
	units, err := store.GetDelegationEvidence(
		t.Context(),
		"parent-1",
		"delegation-1",
		0,
	)
	if err != nil {
		t.Fatalf("GetDelegationEvidence: %v", err)
	}
	if len(units) != 1 || units[0].ContentHash != unit.ContentHash {
		t.Fatalf("delegation evidence = %+v", units)
	}
}

func mustEvidenceArtifact(
	t *testing.T,
	runID string,
	units []tool.EvidenceUnit,
) RunArtifact {
	t.Helper()
	artifact, err := NewEvidenceLedgerArtifact(runID, units)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func testEvidenceUnit(source, target, contentHash string) tool.EvidenceUnit {
	return tool.EvidenceUnit{
		SourceKind:  source,
		Target:      target,
		ContentHash: contentHash,
		Coverage: tool.EvidenceCoverage{
			Complete: true, Included: 1,
		},
	}
}

func runArtifactRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"artifact_id",
		"run_id",
		"kind",
		"schema_id",
		"schema_version",
		"content_hash",
		"content",
	})
}
