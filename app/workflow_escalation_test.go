package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestWorkflowEscalationParentLoaderRequiresActiveParentAndLoadsBudget(
	t *testing.T,
) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loader := workflowEscalationParentLoader{runs: run.Bind(db)}
	deadline := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	limits := agentapi.RunLimits{
		Deadline: deadline, MaxSteps: 8, MaxToolCalls: 12,
		MaxTotalTokens: 1000, MaxCostMicros: 700,
	}
	limitsRaw, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectQuery("SELECT id,run_kind,status,user_id,session_id,question").
		WithArgs("parent-1", run.KindQAParent).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "status", "user_id", "session_id", "question",
			"parent_run_id", "workflow_run_id", "run_limits_json",
			"total_tokens", "cost_micros",
		}).AddRow(
			"parent-1", run.KindQAParent, run.StatusRunning, int64(42),
			"session-1", "why did it fail?", "", "workflow-existing",
			limitsRaw, int64(350), int64(125),
		))
	parent, err := loader.LoadWorkflowEscalationParent(t.Context(), "parent-1")
	if err != nil {
		t.Fatalf("LoadWorkflowEscalationParent: %v", err)
	}
	if parent.RunID != "parent-1" ||
		parent.Actor.UserID != 42 ||
		parent.Question != "why did it fail?" ||
		parent.Correlation.SessionID != "session-1" ||
		parent.Correlation.WorkflowRunID != "workflow-existing" {
		t.Fatalf("parent = %+v", parent)
	}
	if len(parent.Permissions.Scopes) != 1 ||
		parent.Permissions.Scopes[0] != scope.KnowledgeRead {
		t.Fatalf("permissions = %+v", parent.Permissions)
	}
	if parent.Remaining.MaxTotalTokens != 650 ||
		parent.Remaining.MaxCostMicros != 575 ||
		!parent.Remaining.Deadline.Equal(deadline) {
		t.Fatalf("remaining budget = %+v", parent.Remaining)
	}

	mock.ExpectQuery("SELECT id,run_kind,status,user_id,session_id,question").
		WithArgs("parent-terminal", run.KindQAParent).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "status", "user_id", "session_id", "question",
			"parent_run_id", "workflow_run_id", "run_limits_json",
			"total_tokens", "cost_micros",
		}).AddRow(
			"parent-terminal", run.KindQAParent, run.StatusDone, int64(42),
			"session-1", "done", "", "", limitsRaw, int64(1000), int64(700),
		))
	_, err = loader.LoadWorkflowEscalationParent(t.Context(), "parent-terminal")
	if !errors.Is(err, run.ErrNotActive) {
		t.Fatalf("terminal parent error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowEscalationHandoffResolverReturnsOwnedEvidenceAndReport(
	t *testing.T,
) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schemas := testWorkflowEscalationSchemas(t, false)
	resolver := workflowEscalationHandoffResolver{
		runs: run.Bind(db), schemas: schemas,
	}
	unit := tool.EvidenceUnit{
		SourceKind: "runtime", Target: "trace-1",
		ContentHash: strings.Repeat("e", 64),
		Sections:    []string{"errors"},
		Facets:      []string{"runtime_behavior"},
	}
	evidenceArtifact, err := run.NewEvidenceLedgerArtifact(
		"child-1",
		[]tool.EvidenceUnit{unit},
	)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT DISTINCT a.artifact_id").
		WithArgs(
			run.EvidenceLedgerArtifactKind,
			"parent-1",
			"parent-1",
			"delegation-1",
			"parent-1",
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"artifact_id", "run_id", "kind", "schema_id", "schema_version",
			"content_hash", "content",
		}).AddRow(
			evidenceArtifact.ID,
			evidenceArtifact.RunID,
			evidenceArtifact.Kind,
			evidenceArtifact.Schema.ID,
			evidenceArtifact.Schema.Version,
			evidenceArtifact.ContentHash,
			evidenceArtifact.Content,
		))

	reportRef := "report-1"
	reportContent, err := json.Marshal(agentapi.DelegationReport{
		RunID: "child-1", ReportID: reportRef,
		Capability:   "knowledge.service.trace",
		Status:       agentapi.DelegationCompleted,
		Completeness: agentapi.DelegationComplete,
		Summary:      "the trace confirms the failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	reportHash := fmt.Sprintf("%x", sha256.Sum256(reportContent))
	reportArtifactID := testDelegationReportArtifactID(reportRef)
	mock.ExpectQuery("SELECT t.child_run_id").
		WithArgs("parent-1", "delegation-1", reportArtifactID).
		WillReturnRows(sqlmock.NewRows([]string{
			"child_run_id", "artifact_id", "run_id", "kind", "schema_id",
			"schema_version", "content_hash", "content",
		}).AddRow(
			"child-1",
			reportArtifactID,
			"child-1",
			run.DelegationReportArtifactKind,
			"delegation.report",
			int64(1),
			reportHash,
			reportContent,
		))

	handoff, err := resolver.ResolveWorkflowEscalationHandoff(
		t.Context(),
		agentapi.WorkflowEscalationParent{RunID: "parent-1"},
		agentapi.WorkflowEscalationRequest{
			DelegationID: "delegation-1",
			EvidenceRefs: []string{"runtime:trace-1"},
			ReportRefs:   []string{reportRef},
		},
	)
	if err != nil {
		t.Fatalf("ResolveWorkflowEscalationHandoff: %v", err)
	}
	if len(handoff.Evidence) != 1 ||
		handoff.Evidence[0].Ref != "runtime:trace-1" ||
		handoff.Evidence[0].Unit.ContentHash != unit.ContentHash {
		t.Fatalf("evidence handoff = %+v", handoff.Evidence)
	}
	if len(handoff.Reports) != 1 ||
		handoff.Reports[0].Ref != reportRef ||
		handoff.Reports[0].RunID != "child-1" ||
		handoff.Reports[0].ContentHash != reportHash ||
		string(handoff.Reports[0].Payload) != string(reportContent) {
		t.Fatalf("report handoff = %+v", handoff.Reports)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowEscalationHandoffResolverRejectsUntrustedReports(t *testing.T) {
	t.Run("wrapped missing row", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ref := "report-missing"
		mock.ExpectQuery("SELECT t.child_run_id").
			WithArgs(
				"parent-1",
				"delegation-1",
				testDelegationReportArtifactID(ref),
			).
			WillReturnError(fmt.Errorf("lookup report: %w", sql.ErrNoRows))
		resolver := workflowEscalationHandoffResolver{
			runs:    run.Bind(db),
			schemas: testWorkflowEscalationSchemas(t, false),
		}
		_, err = resolver.ResolveWorkflowEscalationHandoff(
			t.Context(),
			agentapi.WorkflowEscalationParent{RunID: "parent-1"},
			agentapi.WorkflowEscalationRequest{
				DelegationID: "delegation-1",
				ReportRefs:   []string{ref},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "does not belong") {
			t.Fatalf("missing report error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("content hash", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ref := "report-1"
		content, err := json.Marshal(agentapi.DelegationReport{
			RunID: "child-1", ReportID: ref,
			Capability:   "knowledge.code.inspect",
			Status:       agentapi.DelegationCompleted,
			Completeness: agentapi.DelegationComplete,
		})
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectQuery("SELECT t.child_run_id").
			WithArgs(
				"parent-1",
				"delegation-1",
				testDelegationReportArtifactID(ref),
			).
			WillReturnRows(testDelegationReportRows().AddRow(
				"child-1",
				testDelegationReportArtifactID(ref),
				"child-1",
				run.DelegationReportArtifactKind,
				"delegation.report",
				int64(1),
				strings.Repeat("0", 64),
				content,
			))
		resolver := workflowEscalationHandoffResolver{
			runs:    run.Bind(db),
			schemas: testWorkflowEscalationSchemas(t, false),
		}
		_, err = resolver.ResolveWorkflowEscalationHandoff(
			t.Context(),
			agentapi.WorkflowEscalationParent{RunID: "parent-1"},
			agentapi.WorkflowEscalationRequest{
				DelegationID: "delegation-1",
				ReportRefs:   []string{ref},
			},
		)
		if !errors.Is(err, run.ErrDelegationTaskConflict) {
			t.Fatalf("tampered report error = %v", err)
		}
	})

	t.Run("schema", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ref := "report-1"
		content, err := json.Marshal(agentapi.DelegationReport{
			RunID: "child-1", ReportID: ref,
			Capability:   "knowledge.code.inspect",
			Status:       agentapi.DelegationCompleted,
			Completeness: agentapi.DelegationComplete,
		})
		if err != nil {
			t.Fatal(err)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(content))
		mock.ExpectQuery("SELECT t.child_run_id").
			WithArgs(
				"parent-1",
				"delegation-1",
				testDelegationReportArtifactID(ref),
			).
			WillReturnRows(testDelegationReportRows().AddRow(
				"child-1",
				testDelegationReportArtifactID(ref),
				"child-1",
				run.DelegationReportArtifactKind,
				"delegation.report",
				int64(1),
				hash,
				content,
			))
		resolver := workflowEscalationHandoffResolver{
			runs:    run.Bind(db),
			schemas: testWorkflowEscalationSchemas(t, true),
		}
		_, err = resolver.ResolveWorkflowEscalationHandoff(
			t.Context(),
			agentapi.WorkflowEscalationParent{RunID: "parent-1"},
			agentapi.WorkflowEscalationRequest{
				DelegationID: "delegation-1",
				ReportRefs:   []string{ref},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "validate delegation report") {
			t.Fatalf("invalid schema error = %v", err)
		}
	})
}

func TestQAWorkflowEscalationBuilderPreservesProvenanceWithinBudget(t *testing.T) {
	const payloadTokens = 480
	parentQuestion := strings.Repeat("why did the checkout request fail? ", 1000)
	reportPayload := json.RawMessage(fmt.Sprintf(
		`{"summary":%q}`,
		strings.Repeat("trace evidence ", 600),
	))
	reportHash := strings.Repeat("a", 64)
	evidence := []tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1",
		ContentHash: strings.Repeat("b", 64),
		Sections:    []string{"errors"},
		Facets:      []string{"runtime_behavior"},
	}}
	result, err := (qaWorkflowEscalationBuilder{
		payloadTokens: payloadTokens,
	}).BuildWorkflowEscalation(
		t.Context(),
		agentapi.WorkflowEscalationBuildRequest{
			Request: agentapi.WorkflowEscalationRequest{
				Objective: "verify the runtime failure",
				Reason:    agentapi.EscalationHighRiskVerificationRequired,
				FocusFacets: []string{
					"runtime_behavior",
					"failure_handling",
				},
			},
			Parent: agentapi.WorkflowEscalationParent{
				RunID: "parent-1", Question: parentQuestion,
				Correlation: agentapi.Correlation{SessionID: "session-1"},
			},
			Capability: agentapi.Capability{
				ID:        "knowledge.service.trace",
				Freshness: agentapi.FreshnessCurrent,
			},
			Evidence: evidence,
			Reports: []agentapi.WorkflowEscalationReport{{
				Ref: "report-1", RunID: "child-1",
				Schema: agentapi.SchemaRef{
					ID: "delegation.report", Version: 1,
				},
				ContentHash: reportHash,
				Payload:     reportPayload,
			}},
		},
	)
	if err != nil {
		t.Fatalf("BuildWorkflowEscalation: %v", err)
	}
	if tokens := tooloutput.EstimateTokens(string(result.Input)); tokens > payloadTokens {
		t.Fatalf("input uses %d tokens, want <= %d", tokens, payloadTokens)
	}
	if strings.Contains(string(result.Input), parentQuestion) {
		t.Fatal("workflow escalation input contains the parent question")
	}
	if len(result.SeedEvidence) != 1 ||
		result.SeedEvidence[0].ContentHash != evidence[0].ContentHash {
		t.Fatalf("seed evidence = %+v", result.SeedEvidence)
	}
	result.SeedEvidence[0].Sections[0] = "changed"
	if evidence[0].Sections[0] != "errors" {
		t.Fatal("builder returned aliased seed evidence")
	}

	var contract platformagent.TaskContract
	if err := json.Unmarshal(result.Input, &contract); err != nil {
		t.Fatalf("decode task contract: %v", err)
	}
	if contract.TaskID != "parent-1" ||
		contract.Objective != "verify the runtime failure" ||
		len(contract.Context.ConversationRefs) != 1 ||
		contract.Context.ConversationRefs[0].SessionID != "session-1" {
		t.Fatalf("contract = %+v", contract)
	}
	if len(contract.EvidenceGoals) != 2 {
		t.Fatalf("evidence goals = %+v", contract.EvidenceGoals)
	}
	for _, goal := range contract.EvidenceGoals {
		if len(goal.Sources) != 1 ||
			goal.Sources[0] != agentapi.EvidenceSourceRuntime ||
			goal.Freshness != agentapi.FreshnessCurrent ||
			!goal.HighRisk {
			t.Fatalf("evidence goal = %+v", goal)
		}
	}
	if len(contract.Context.SeedMaterial) != 2 {
		t.Fatalf("seed material = %+v", contract.Context.SeedMaterial)
	}
	reportBlock := contract.Context.SeedMaterial[0]
	if reportBlock.Source != "delegation.report" ||
		len(reportBlock.References) != 2 ||
		reportBlock.References[0].Target != "report-1" ||
		reportBlock.References[1].Target != reportHash ||
		len(reportBlock.Content) >= len(reportPayload) {
		t.Fatalf("report block = %+v", reportBlock)
	}
	evidenceBlock := contract.Context.SeedMaterial[1]
	if evidenceBlock.Source != "qa.evidence" ||
		len(evidenceBlock.Evidence) != 1 ||
		evidenceBlock.Evidence[0].ContentHash != evidence[0].ContentHash {
		t.Fatalf("evidence block = %+v", evidenceBlock)
	}
}

func testWorkflowEscalationSchemas(
	t *testing.T,
	requireUnknownField bool,
) *agentapi.SchemaRegistry {
	t.Helper()
	required := `["report_id","run_id"]`
	if requireUnknownField {
		required = `["must_exist"]`
	}
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish([]agentapi.SchemaDefinition{{
		ID:      "delegation.report",
		Version: 1,
		Document: json.RawMessage(
			`{"type":"object","required":` + required + `}`,
		),
	}}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testDelegationReportArtifactID(reportRef string) string {
	sum := sha256.Sum256([]byte(reportRef))
	return fmt.Sprintf("artifact_%x", sum[:12])
}

func testDelegationReportRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"child_run_id", "artifact_id", "run_id", "kind", "schema_id",
		"schema_version", "content_hash", "content",
	})
}
