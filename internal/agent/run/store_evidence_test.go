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
