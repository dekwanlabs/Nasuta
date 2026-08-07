package evaluation

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func TestWorkflowTraceUsesOwnerScopeAndBoundedNarrowReads(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	evaluationStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	startedAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(time.Second)
	mock.ExpectQuery(`(?s)SELECT id,workflow_id,workflow_version.*FROM workflow_runs WHERE id=\? AND actor_user_id=\? LIMIT 1`).
		WithArgs("workflow-run-1", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_id", "workflow_version", "workflow_hash", "input_hash",
			"scenario", "status", "input_tokens", "output_tokens", "reasoning_tokens",
			"total_tokens", "tool_call_count", "cost_micros", "retry_count",
			"error_code", "started_at", "ended_at",
		}).AddRow(
			"workflow-run-1", "review.change", int64(2), "workflow-hash",
			"input-hash", "feature-review", "succeeded", int64(10), int64(5),
			int64(1), int64(15), int64(2), int64(8), int64(0), "",
			startedAt, endedAt,
		))
	mock.ExpectQuery(`(?s)SELECT\s+node_id,attempt,kind,agent_run_id,status.*FROM workflow_node_runs.*LIMIT \?`).
		WithArgs("workflow-run-1", maxTraceNodes+1).
		WillReturnRows(sqlmock.NewRows([]string{
			"node_id", "attempt", "kind", "agent_run_id", "status",
			"input_tokens", "output_tokens", "reasoning_tokens", "total_tokens",
			"tool_call_count", "cost_micros", "retry_count", "error_code",
			"started_at", "ended_at",
		}).AddRow(
			"review.security", 1, "agent", "agent-run-1", "succeeded",
			int64(10), int64(5), int64(1), int64(15), int64(2), int64(8),
			int64(0), "", startedAt, endedAt,
		))
	mock.ExpectQuery(`(?s)SELECT\s+id,agent_id,definition_version.*FROM agent_runs.*LIMIT \?`).
		WithArgs("workflow-run-1", maxTraceAgents+1).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "agent_id", "definition_version", "definition_hash",
			"tool_snapshot_id", "workflow_node_id", "status", "evidence_status",
			"input_tokens", "output_tokens", "reasoning_tokens", "total_tokens",
			"tool_call_count", "tool_failure_count", "partial_result_count",
			"omitted_evidence_count", "llm_call_count", "error_code",
			"started_at", "ended_at",
		}).AddRow(
			"agent-run-1", "review.security", int64(3), "definition-hash",
			"tools-1", "review.security", "done", "complete", int64(10),
			int64(5), int64(1), int64(15), int64(2), int64(0), int64(0),
			int64(0), int64(1), "", startedAt, endedAt,
		))
	mock.ExpectQuery(`(?s)SELECT\s+seq,kind,node_id,summary,created_at.*FROM workflow_events.*LIMIT \?`).
		WithArgs("workflow-run-1", maxTraceEvents+1).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "kind", "node_id", "summary", "created_at",
		}).AddRow(int64(1), "workflow_started", "", "workflow started", startedAt))

	trace, err := evaluationStore.WorkflowTrace(
		t.Context(), "workflow-run-1", 7, false,
	)
	if err != nil {
		t.Fatalf("WorkflowTrace: %v", err)
	}
	if trace.Run.ID != "workflow-run-1" ||
		len(trace.Nodes) != 1 ||
		len(trace.Agents) != 1 ||
		len(trace.Events) != 1 ||
		trace.Truncated != (TraceTruncation{}) {
		t.Fatalf("trace = %+v", trace)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAgentVersionMetricsUsesPersistedPricingAndDatabaseP95(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	evaluationStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	definitionJSON, err := json.Marshal(agentapi.Definition{
		Model: agentapi.ModelPolicy{
			InputPriceMicrosPerMillionTokens:  3,
			OutputPriceMicrosPerMillionTokens: 5,
		},
	})
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+definition_json,content_hash FROM agent_definitions.*LIMIT 1`).
		WithArgs("qa.answerer", int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{
			"definition_json", "content_hash",
		}).AddRow(definitionJSON, "definition-hash"))
	mock.ExpectQuery(`(?s)SELECT\s+COUNT\(\*\).*FROM agent_runs.*started_at<\?`).
		WithArgs(
			"qa.answerer", int64(2),
			store.DatabaseTime(from.Format(time.RFC3339Nano)),
			store.DatabaseTime(to.Format(time.RFC3339Nano)),
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"runs", "success", "evidence_runs", "evidence_complete",
			"input_tokens", "output_tokens", "reasoning_tokens", "total_tokens",
			"tool_calls", "tool_failures",
		}).AddRow(
			int64(4), int64(3), int64(2), int64(1),
			int64(2_000_000), int64(1_000_000), int64(200_000),
			int64(3_200_000), int64(10), int64(2),
		))
	mock.ExpectQuery(`(?s)SELECT duration_ms FROM \(.*FROM agent_runs.*LIMIT 1`).
		WithArgs(
			"qa.answerer", int64(2),
			store.DatabaseTime(from.Format(time.RFC3339Nano)),
			store.DatabaseTime(to.Format(time.RFC3339Nano)),
		).
		WillReturnRows(sqlmock.NewRows([]string{"duration_ms"}).AddRow(int64(2500)))
	mock.ExpectQuery(`(?s)SELECT COALESCE\(SUM\(.*FROM agent_llm_calls.*JOIN agent_runs.*started_at<\?`).
		WithArgs(
			int64(3), int64(5), "qa.answerer", int64(2),
			store.DatabaseTime(from.Format(time.RFC3339Nano)),
			store.DatabaseTime(to.Format(time.RFC3339Nano)),
		).
		WillReturnRows(sqlmock.NewRows([]string{"cost_micros"}).AddRow(int64(12)))

	metrics, err := evaluationStore.AgentVersionMetrics(
		t.Context(), "qa.answerer", 2, Window{From: from, To: to},
	)
	if err != nil {
		t.Fatalf("AgentVersionMetrics: %v", err)
	}
	if metrics.DefinitionHash != "definition-hash" ||
		metrics.CostMicros != 12 ||
		metrics.P95LatencyMillis != 2500 ||
		metrics.RunCount != 4 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCreateReviewLabelsResolvesFindingAndPersistsIdempotently(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	evaluationStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	targetHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT policy_id,policy_version,subject_hash
		FROM review_rounds WHERE id=? LIMIT 1`)).
		WithArgs("round-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"policy_id", "policy_version", "subject_hash",
		}).AddRow("review.change", int64(2), "subject-hash"))
	mock.ExpectQuery(`(?s)SELECT id,category,content_hash.*FROM review_findings.*IN \(\?\).*LIMIT \?`).
		WithArgs("round-1", "finding-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "category", "content_hash",
		}).AddRow("finding-1", "security", targetHash))
	mock.ExpectExec(`(?s)INSERT INTO review_evaluation_labels.*ON DUPLICATE KEY UPDATE id=id`).
		WithArgs(
			sqlmock.AnyArg(), "round-1", "review.change", int64(2),
			"subject-hash", "finding-1", targetHash, "security",
			LabelTruePositive, int64(7),
			store.DatabaseTime(now.Format(time.RFC3339Nano)),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`(?s)SELECT\s+seq,id,round_id.*FROM review_evaluation_labels.*target_hash IN \(\?\).*LIMIT \?`).
		WithArgs("round-1", targetHash, 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "id", "round_id", "policy_id", "policy_version",
			"subject_hash", "finding_id", "target_hash", "category", "label",
			"created_by", "created_at",
		}).AddRow(
			int64(1), "review-eval-1", "round-1", "review.change", int64(2),
			"subject-hash", "finding-1", targetHash, "security",
			LabelTruePositive, int64(7), now,
		))
	mock.ExpectCommit()

	labels, err := evaluationStore.CreateReviewLabels(
		t.Context(),
		"round-1",
		[]ReviewLabelInput{{
			Label: LabelTruePositive, FindingID: "finding-1",
		}},
		7,
		now,
	)
	if err != nil {
		t.Fatalf("CreateReviewLabels: %v", err)
	}
	if len(labels) != 1 ||
		labels[0].TargetHash != targetHash ||
		labels[0].Category != "security" {
		t.Fatalf("labels = %+v", labels)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
