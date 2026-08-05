package agentworkflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

type RunStatus string

const (
	RunRunning      RunStatus = "running"
	RunSucceeded    RunStatus = "succeeded"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
	RunTimedOut     RunStatus = "timed_out"
	RunWaitingHuman RunStatus = "waiting_human"
)

type WorkflowRunRecord struct {
	ID              string
	WorkflowID      string
	WorkflowVersion int64
	WorkflowHash    string
	InputHash       string
	ActorUserID     int64
	ActorTenantID   string
	Scenario        string
	Status          RunStatus
	Budget          WorkflowBudget
	ErrorCode       string
	StartedAt       time.Time
	EndedAt         *time.Time
}

type NodeRunRecord struct {
	WorkflowRunID   string
	NodeID          string
	Attempt         int
	Kind            NodeKind
	AgentRunID      string
	InputHandoffIDs []string
	OutputHandoffID string
	Status          RunStatus
	ErrorCode       string
	StartedAt       time.Time
	EndedAt         *time.Time
}

type Event struct {
	WorkflowRunID string
	Seq           int64
	Kind          string
	NodeID        string
	Summary       string
	Detail        json.RawMessage
	CreatedAt     time.Time
}

type HandoffCursor struct {
	CreatedAt time.Time
	ID        string
}

// Store persists immutable workflow facts and exposes only bounded list reads.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("workflow store database is required")
	}
	return &Store{db: db}, nil
}

func (workflowStore *Store) CreateRun(ctx context.Context, run WorkflowRunRecord) error {
	budget, err := json.Marshal(run.Budget)
	if err != nil {
		return fmt.Errorf("marshal workflow budget: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO workflow_runs(
		id,workflow_id,workflow_version,workflow_hash,input_hash,actor_user_id,
		actor_tenant_id,scenario,status,budget_json,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.WorkflowID, run.WorkflowVersion, run.WorkflowHash, run.InputHash,
		run.ActorUserID, run.ActorTenantID, run.Scenario, run.Status, budget,
		run.ErrorCode, store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("create workflow run %q: %w", run.ID, err)
	}
	return nil
}

func (workflowStore *Store) CreateNodeRun(ctx context.Context, run NodeRunRecord) error {
	inputs, err := json.Marshal(run.InputHandoffIDs)
	if err != nil {
		return fmt.Errorf("marshal workflow node inputs: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO workflow_node_runs(
		workflow_run_id,node_id,attempt,kind,agent_run_id,input_handoff_ids_json,
		output_handoff_id,status,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		run.WorkflowRunID, run.NodeID, run.Attempt, run.Kind, run.AgentRunID, inputs,
		run.OutputHandoffID, run.Status, run.ErrorCode,
		store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("create workflow node run %q/%q: %w", run.WorkflowRunID, run.NodeID, err)
	}
	return nil
}

func (workflowStore *Store) SaveHandoff(ctx context.Context, handoff Handoff) error {
	references, err := json.Marshal(handoff.References)
	if err != nil {
		return fmt.Errorf("marshal handoff references: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		handoff.ID, handoff.WorkflowRunID, handoff.ProducerNodeID, handoff.ProducerRunID,
		handoff.Schema.ID, handoff.Schema.Version, handoff.Payload, references,
		handoff.Completeness, handoff.ContentHash,
		store.DatabaseTime(handoff.CreatedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("save handoff %q: %w", handoff.ID, err)
	}
	return nil
}

func (workflowStore *Store) SaveGateDecision(ctx context.Context, id, workflowRunID, nodeID string, decision GateDecision) error {
	reasons, err := json.Marshal(decision.ReasonCodes)
	if err != nil {
		return fmt.Errorf("marshal gate reason codes: %w", err)
	}
	findings, err := json.Marshal(decision.FindingIDs)
	if err != nil {
		return fmt.Errorf("marshal gate finding ids: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO gate_decisions(
		id,workflow_run_id,node_id,gate_id,subject_hash,decision,reason_codes_json,
		finding_ids_json,evaluated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, workflowRunID, nodeID, decision.GateID, decision.SubjectHash,
		decision.Decision, reasons, findings,
		store.DatabaseTime(decision.EvaluatedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("save gate decision %q: %w", id, err)
	}
	return nil
}

func (workflowStore *Store) AppendEvent(ctx context.Context, event Event) error {
	_, err := workflowStore.db.ExecContext(ctx, `INSERT INTO workflow_events(
		workflow_run_id,seq,kind,node_id,summary,detail_json,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		event.WorkflowRunID, event.Seq, event.Kind, event.NodeID, event.Summary,
		nullableJSON(event.Detail), store.DatabaseTime(event.CreatedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("append workflow event %q/%d: %w", event.WorkflowRunID, event.Seq, err)
	}
	return nil
}

func (workflowStore *Store) ListEvents(ctx context.Context, workflowRunID string, afterSeq int64, limit int) ([]Event, error) {
	limit = boundedLimit(limit)
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		workflow_run_id,seq,kind,node_id,summary,detail_json,created_at
		FROM workflow_events
		WHERE workflow_run_id=? AND seq>?
		ORDER BY seq LIMIT ?`, workflowRunID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow events %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	events := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		var detail []byte
		if err := rows.Scan(
			&event.WorkflowRunID, &event.Seq, &event.Kind, &event.NodeID,
			&event.Summary, &detail, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow event: %w", err)
		}
		event.Detail = append(json.RawMessage(nil), detail...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow events: %w", err)
	}
	return events, nil
}

func (workflowStore *Store) ListHandoffs(ctx context.Context, workflowRunID string, cursor HandoffCursor, limit int) ([]Handoff, error) {
	limit = boundedLimit(limit)
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at
		FROM handoff_artifacts
		WHERE workflow_run_id=? AND (created_at>? OR (created_at=? AND id>?))
		ORDER BY created_at,id LIMIT ?`,
		workflowRunID, cursor.CreatedAt, cursor.CreatedAt, cursor.ID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list handoffs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	handoffs := make([]Handoff, 0, limit)
	for rows.Next() {
		var handoff Handoff
		var references []byte
		if err := rows.Scan(
			&handoff.ID, &handoff.WorkflowRunID, &handoff.ProducerNodeID,
			&handoff.ProducerRunID, &handoff.Schema.ID, &handoff.Schema.Version,
			&handoff.Payload, &references, &handoff.Completeness,
			&handoff.ContentHash, &handoff.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan handoff: %w", err)
		}
		if err := json.Unmarshal(references, &handoff.References); err != nil {
			return nil, fmt.Errorf("decode handoff references: %w", err)
		}
		handoffs = append(handoffs, handoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate handoffs: %w", err)
	}
	return handoffs, nil
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
