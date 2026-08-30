package run

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestReserveDelegationBatchIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	admission := testDelegationAdmission()
	reservationRaw, err := json.Marshal(admission.Reservations[0])
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	expectDelegationParentBudget(mock, admission.ParentRunID, 100, 20, 2000, 1000)
	mock.ExpectQuery("FROM agent_delegation_tasks WHERE parent_run_id=\\? FOR UPDATE").
		WithArgs(admission.ParentRunID).
		WillReturnRows(emptyDelegationTaskRows())
	mock.ExpectExec("INSERT INTO agent_delegation_tasks").
		WithArgs(
			admission.ParentRunID,
			admission.DelegationID,
			0,
			"child-0",
			"knowledge.code.inspect",
			int64(3),
			strings.Repeat("a", 64),
			strings.Repeat("b", 64),
			true,
			"",
			reservationRaw,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	first, err := store.ReserveDelegationBatch(t.Context(), admission)
	if err != nil {
		t.Fatalf("ReserveDelegationBatch: %v", err)
	}
	if len(first) != 1 || !first[0].Admitted || first[0].ChildRunID != "child-0" {
		t.Fatalf("first admission = %+v", first)
	}

	mock.ExpectBegin()
	expectDelegationParentBudget(mock, admission.ParentRunID, 100, 20, 2000, 1000)
	mock.ExpectQuery("FROM agent_delegation_tasks WHERE parent_run_id=\\? FOR UPDATE").
		WithArgs(admission.ParentRunID).
		WillReturnRows(emptyDelegationTaskRows().AddRow(
			admission.ParentRunID,
			admission.DelegationID,
			0,
			"child-0",
			"knowledge.code.inspect",
			int64(3),
			strings.Repeat("a", 64),
			strings.Repeat("b", 64),
			true,
			"",
			reservationRaw,
			nil,
			"",
		))
	mock.ExpectCommit()

	second, err := store.ReserveDelegationBatch(t.Context(), admission)
	if err != nil {
		t.Fatalf("retry ReserveDelegationBatch: %v", err)
	}
	if len(second) != 1 || second[0].Reservation.ReservedTokens != 500 {
		t.Fatalf("retry admission = %+v", second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveDelegationBatchProtectsParentAnswerReserve(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	admission := testDelegationAdmission()
	admission.MaxTotalTokens = 799
	admission.ParentAnswerReserve = 300

	mock.ExpectBegin()
	expectDelegationParentBudget(mock, admission.ParentRunID, 100, 0, 1000, 0)
	mock.ExpectQuery("FROM agent_delegation_tasks WHERE parent_run_id=\\? FOR UPDATE").
		WithArgs(admission.ParentRunID).
		WillReturnRows(emptyDelegationTaskRows())
	mock.ExpectRollback()

	_, err = store.ReserveDelegationBatch(t.Context(), admission)
	if !errors.Is(err, ErrDelegationBudgetInsufficient) {
		t.Fatalf("error = %v, want budget insufficient", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveDelegationBatchDoesNotChargeParentRetrieveAgainstChildCeiling(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	admission := testDelegationAdmission()
	admission.MaxTotalTokens = 800
	admission.ParentAnswerReserve = 200
	reservationRaw, err := json.Marshal(admission.Reservations[0])
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	expectDelegationParentBudget(mock, admission.ParentRunID, 61679, 0, 512000, 0)
	mock.ExpectQuery("FROM agent_delegation_tasks WHERE parent_run_id=\\? FOR UPDATE").
		WithArgs(admission.ParentRunID).
		WillReturnRows(emptyDelegationTaskRows())
	mock.ExpectExec("INSERT INTO agent_delegation_tasks").
		WithArgs(
			admission.ParentRunID,
			admission.DelegationID,
			0,
			"child-0",
			"knowledge.code.inspect",
			int64(3),
			strings.Repeat("a", 64),
			strings.Repeat("b", 64),
			true,
			"",
			reservationRaw,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	records, err := store.ReserveDelegationBatch(t.Context(), admission)
	if err != nil {
		t.Fatalf("ReserveDelegationBatch: %v", err)
	}
	if len(records) != 1 || !records[0].Admitted {
		t.Fatalf("records = %+v", records)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveDelegationBatchHonorsParentOwnedCeiling(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	admission := testDelegationAdmission()
	admission.MaxTotalTokens = 2000
	admission.ParentAnswerReserve = 200

	mock.ExpectBegin()
	expectDelegationParentBudget(mock, admission.ParentRunID, 900, 0, 1000, 0)
	mock.ExpectQuery("FROM agent_delegation_tasks WHERE parent_run_id=\\? FOR UPDATE").
		WithArgs(admission.ParentRunID).
		WillReturnRows(emptyDelegationTaskRows())
	mock.ExpectRollback()

	_, err = store.ReserveDelegationBatch(t.Context(), admission)
	if !errors.Is(err, ErrDelegationBudgetInsufficient) {
		t.Fatalf("error = %v, want parent ceiling", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSettleDelegationTaskPersistsReportBeforeSettlement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	admission := testDelegationAdmission()
	reservation := admission.Reservations[0]
	reservationRaw, err := json.Marshal(reservation)
	if err != nil {
		t.Fatal(err)
	}
	usage := agentapi.Usage{
		InputTokens: 200, OutputTokens: 50, TotalTokens: 250, CostMicros: 80,
	}
	content := []byte(`{"status":"completed"}`)
	artifact := &DelegationArtifact{
		ID: "report-0", RunID: reservation.ChildRunID,
		Kind:        DelegationReportArtifactKind,
		Schema:      agentapi.SchemaRef{ID: "delegation.report", Version: 1},
		ContentHash: fmt.Sprintf("%x", sha256.Sum256(content)),
		Content:     content,
	}
	evidenceArtifact, err := NewEvidenceLedgerArtifact(
		reservation.ChildRunID,
		[]tool.EvidenceUnit{{
			SourceKind:  "runtime",
			Target:      "trace-1",
			ContentHash: strings.Repeat("d", 64),
			Coverage: tool.EvidenceCoverage{
				Complete: true, Included: 1,
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks.*FOR UPDATE").
		WithArgs(admission.ParentRunID, admission.DelegationID, 0).
		WillReturnRows(emptyDelegationTaskRows().AddRow(
			admission.ParentRunID,
			admission.DelegationID,
			0,
			reservation.ChildRunID,
			reservation.Capability.ID,
			reservation.Capability.Version,
			reservation.CapabilityHash,
			reservation.ObjectiveHash,
			true,
			"",
			reservationRaw,
			nil,
			"",
		))
	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WithArgs(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			content,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WithArgs(
			evidenceArtifact.ID,
			evidenceArtifact.RunID,
			evidenceArtifact.Kind,
			evidenceArtifact.Schema.ID,
			evidenceArtifact.Schema.Version,
			evidenceArtifact.ContentHash,
			evidenceArtifact.Content,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	usageRaw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs(
			usageRaw,
			artifact.ID,
			sqlmock.AnyArg(),
			admission.ParentRunID,
			admission.DelegationID,
			0,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	settled, err := store.SettleDelegationTask(t.Context(), DelegationSettlement{
		ParentRunID:      admission.ParentRunID,
		DelegationID:     admission.DelegationID,
		TaskIndex:        0,
		ChildRunID:       reservation.ChildRunID,
		Usage:            usage,
		Artifact:         artifact,
		EvidenceArtifact: &evidenceArtifact,
	})
	if err != nil {
		t.Fatalf("SettleDelegationTask: %v", err)
	}
	if settled.SettledUsage == nil || *settled.SettledUsage != usage ||
		settled.ReportArtifactID != artifact.ID {
		t.Fatalf("settled task = %+v", settled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkDelegationChildIsIdempotentWhenAlreadyLinked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs("child-0", "parent-1", "delegation-1", 0, "child-0").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT child_run_id FROM agent_delegation_tasks").
		WithArgs("parent-1", "delegation-1", 0).
		WillReturnRows(sqlmock.NewRows([]string{"child_run_id"}).AddRow("child-0"))
	if err := store.LinkDelegationChild(
		t.Context(), "parent-1", "delegation-1", 0, "child-0",
	); err != nil {
		t.Fatalf("LinkDelegationChild: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkDelegationChildConflictsOnDifferentChild(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs("child-0", "parent-1", "delegation-1", 0, "child-0").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT child_run_id FROM agent_delegation_tasks").
		WithArgs("parent-1", "delegation-1", 0).
		WillReturnRows(sqlmock.NewRows([]string{"child_run_id"}).AddRow("other-child"))
	err = store.LinkDelegationChild(
		t.Context(), "parent-1", "delegation-1", 0, "child-0",
	)
	if !errors.Is(err, ErrDelegationTaskConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkDelegationChildSucceedsOnFirstWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs("child-0", "parent-1", "delegation-1", 0, "child-0").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.LinkDelegationChild(
		t.Context(), "parent-1", "delegation-1", 0, "child-0",
	); err != nil {
		t.Fatalf("LinkDelegationChild: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testDelegationAdmission() DelegationAdmission {
	deadline := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	reservation := DelegationReservation{
		ParentRunID:  "parent-1",
		DelegationID: "delegation-1",
		TaskIndex:    0,
		ChildRunID:   "child-0",
		Capability: agentapi.CapabilityRef{
			ID: "knowledge.code.inspect", Version: 3,
		},
		CapabilityHash: strings.Repeat("a", 64),
		ObjectiveHash:  strings.Repeat("b", 64),
		Limits: agentapi.RunLimits{
			Deadline: deadline, MaxSteps: 4, MaxToolCalls: 3,
			MaxTotalTokens: 500, MaxCostMicros: 200,
		},
		ReservedTokens: 500, ReservedCostMicros: 200,
	}
	return DelegationAdmission{
		ParentRunID:         "parent-1",
		DelegationID:        "delegation-1",
		MaxChildren:         3,
		MaxTotalTokens:      2000,
		MaxTotalCostMicros:  1000,
		ParentAnswerReserve: 200,
		Reservations:        []DelegationReservation{reservation},
	}
}

func expectDelegationParentBudget(
	mock sqlmock.Sqlmock,
	parentRunID string,
	totalTokens,
	costMicros,
	maxTokens,
	maxCost int64,
) {
	limits, _ := json.Marshal(agentapi.RunLimits{
		MaxTotalTokens: maxTokens,
		MaxCostMicros:  maxCost,
	})
	mock.ExpectQuery("SELECT status,total_tokens,cost_micros,run_limits_json").
		WithArgs(parentRunID).
		WillReturnRows(sqlmock.NewRows([]string{
			"status", "total_tokens", "cost_micros", "run_limits_json",
		}).AddRow(StatusRunning, totalTokens, costMicros, limits))
}

func emptyDelegationTaskRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"parent_run_id",
		"delegation_id",
		"task_index",
		"child_run_id",
		"capability_id",
		"capability_version",
		"capability_content_hash",
		"objective_hash",
		"admitted",
		"rejection_code",
		"reservation_json",
		"settled_usage_json",
		"report_artifact_id",
	})
}
