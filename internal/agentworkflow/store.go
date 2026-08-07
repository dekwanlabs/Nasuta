package agentworkflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

var (
	ErrInvalid          = errors.New("workflow input invalid")
	ErrNotFound         = errors.New("workflow resource not found")
	ErrForbidden        = errors.New("workflow operation forbidden")
	ErrConflict         = errors.New("workflow conflict")
	ErrUnavailable      = errors.New("workflow capability unavailable")
	ErrApprovalConflict = fmt.Errorf(
		"workflow approval decision conflicts with existing fact: %w",
		ErrConflict,
	)
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
	ID                  string                    `json:"id"`
	ParentRunID         string                    `json:"parent_run_id,omitempty"`
	WorkflowID          string                    `json:"workflow_id"`
	WorkflowVersion     int64                     `json:"workflow_version"`
	WorkflowHash        string                    `json:"workflow_hash"`
	Selection           DefinitionSelection       `json:"selection,omitempty"`
	InputHash           string                    `json:"input_hash"`
	ActorUserID         int64                     `json:"actor_user_id"`
	ActorTenantID       string                    `json:"actor_tenant_id"`
	ActorPermissions    agentapi.PermissionPolicy `json:"actor_permissions"`
	Scenario            string                    `json:"scenario"`
	ScenarioPermissions agentapi.PermissionPolicy `json:"scenario_permissions"`
	Status              RunStatus                 `json:"status"`
	Budget              WorkflowBudget            `json:"budget"`
	Usage               WorkflowUsage             `json:"usage"`
	ErrorCode           string                    `json:"error_code,omitempty"`
	StartedAt           time.Time                 `json:"started_at"`
	EndedAt             *time.Time                `json:"ended_at,omitempty"`
}

type NodeRunRecord struct {
	WorkflowRunID   string        `json:"workflow_run_id"`
	NodeID          string        `json:"node_id"`
	Attempt         int           `json:"attempt"`
	Kind            NodeKind      `json:"kind"`
	AgentRunID      string        `json:"agent_run_id,omitempty"`
	InputHandoffIDs []string      `json:"input_handoff_ids"`
	OutputHandoffID string        `json:"output_handoff_id,omitempty"`
	Status          RunStatus     `json:"status"`
	Usage           WorkflowUsage `json:"usage"`
	ErrorCode       string        `json:"error_code,omitempty"`
	StartedAt       time.Time     `json:"started_at"`
	FirstStartedAt  time.Time     `json:"first_started_at"`
	EndedAt         *time.Time    `json:"ended_at,omitempty"`
}

type Event struct {
	WorkflowRunID string          `json:"workflow_run_id"`
	Seq           int64           `json:"seq"`
	Kind          string          `json:"kind"`
	NodeID        string          `json:"node_id,omitempty"`
	Summary       string          `json:"summary"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type WorkflowRunState struct {
	Run         WorkflowRunRecord
	Input       Handoff
	Nodes       map[string]NodeRunRecord
	Handoffs    map[string]Handoff
	NodeOutputs map[string]Handoff
	Gates       map[string]GateDecision
	Approvals   map[string]WorkflowApproval
}

type ApprovalTransition struct {
	Approval  WorkflowApproval
	Applied   bool
	RunStatus RunStatus
}

type HandoffCursor struct {
	CreatedAt time.Time
	ID        string
}

type NodeRunCursor struct {
	NodeID  string
	Attempt int
}

type ActiveRunCursor struct {
	StartedAt time.Time
	ID        string
}

type ActiveRunRef struct {
	ID        string
	StartedAt time.Time
}

type CancelTransition struct {
	Applied bool
	Status  RunStatus
}

// Store persists immutable workflow facts and exposes only bounded list reads.
type Store struct {
	db  *sql.DB
	hub *EventHub
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("workflow store database is required")
	}
	return &Store{db: db, hub: NewEventHub()}, nil
}

// StartWorkflow atomically fixes the run, its input artifact, and initial events.
func (workflowStore *Store) StartWorkflow(
	ctx context.Context,
	run WorkflowRunRecord,
	input Handoff,
) error {
	budget, err := json.Marshal(run.Budget)
	if err != nil {
		return fmt.Errorf("marshal workflow budget: %w", err)
	}
	selection, err := json.Marshal(run.Selection)
	if err != nil {
		return fmt.Errorf("marshal workflow definition selection: %w", err)
	}
	actorPermissions, err := json.Marshal(run.ActorPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow actor permissions: %w", err)
	}
	scenarioPermissions, err := json.Marshal(run.ScenarioPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow scenario permissions: %w", err)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow %q: %w", run.ID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_runs(
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ParentRunID, run.WorkflowID, run.WorkflowVersion, run.WorkflowHash, selection, run.InputHash,
		run.ActorUserID, run.ActorTenantID, actorPermissions, run.Scenario,
		scenarioPermissions, RunRunning, budget,
		run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.ReasoningTokens,
		run.Usage.TotalTokens, run.Usage.ToolCalls, run.Usage.CostMicros,
		run.Usage.Retries, "",
		store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf("create workflow run %q: %w", run.ID, err)
	}
	if err := saveHandoffTx(ctx, tx, input); err != nil {
		return err
	}
	events := []Event{
		{
			WorkflowRunID: run.ID, Kind: "workflow_started",
			Summary: "workflow started", CreatedAt: run.StartedAt,
		},
		{
			WorkflowRunID: run.ID, Kind: "handoff_created", NodeID: input.ProducerNodeID,
			Summary: "workflow input created", CreatedAt: input.CreatedAt,
		},
	}
	if err := appendEventsTx(ctx, tx, run.ID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow %q start: %w", run.ID, err)
	}
	workflowStore.publish(events)
	return nil
}

// StartNode atomically creates an Attempt and its started event.
func (workflowStore *Store) StartNode(ctx context.Context, run NodeRunRecord) error {
	inputs, err := json.Marshal(run.InputHandoffIDs)
	if err != nil {
		return fmt.Errorf("marshal workflow node inputs: %w", err)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q: %w", run.WorkflowRunID, run.NodeID, err)
	}
	defer tx.Rollback()
	if err := lockRunningWorkflow(ctx, tx, run.WorkflowRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_node_runs(
		workflow_run_id,node_id,attempt,kind,agent_run_id,input_handoff_ids_json,
		output_handoff_id,status,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		run.WorkflowRunID, run.NodeID, run.Attempt, run.Kind, "", inputs, "",
		RunRunning, "", store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf("create workflow node run %q/%q: %w", run.WorkflowRunID, run.NodeID, err)
	}
	events := []Event{{
		WorkflowRunID: run.WorkflowRunID, Kind: "node_started", NodeID: run.NodeID,
		Summary: "workflow node started", CreatedAt: run.StartedAt,
	}}
	if err := appendEventsTx(ctx, tx, run.WorkflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q start: %w", run.WorkflowRunID, run.NodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// SucceedNode atomically records output facts before exposing a successful Attempt.
func (workflowStore *Store) SucceedNode(
	ctx context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	handoff Handoff,
	decision *GateDecision,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q success: %w", workflowRunID, nodeID, err)
	}
	defer tx.Rollback()
	if err := lockRunningWorkflow(ctx, tx, workflowRunID); err != nil {
		return err
	}
	if err := saveHandoffTx(ctx, tx, handoff); err != nil {
		return err
	}
	if decision != nil {
		if err := saveGateDecisionTx(ctx, tx, "gate_"+handoff.ContentHash[:24], workflowRunID, nodeID, *decision); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
		SET agent_run_id=?,output_handoff_id=?,status=?,error_code='',
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
		agentRunID, handoff.ID, RunSucceeded,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, nodeID, attempt, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("complete workflow node %q/%q: %w", workflowRunID, nodeID, err)
	}
	if err := requireSingleTransition(result, workflowRunID+"/"+nodeID); err != nil {
		return err
	}
	if err := accumulateWorkflowUsageTx(ctx, tx, workflowRunID, usage); err != nil {
		return err
	}
	events := []Event{
		{
			WorkflowRunID: workflowRunID, Kind: "handoff_created", NodeID: nodeID,
			Summary: "node handoff created", CreatedAt: endedAt,
		},
		{
			WorkflowRunID: workflowRunID, Kind: "node_succeeded", NodeID: nodeID,
			Summary: "workflow node succeeded", CreatedAt: endedAt,
		},
	}
	if decision != nil {
		events = append(events, Event{
			WorkflowRunID: workflowRunID, Kind: "gate_evaluated", NodeID: nodeID,
			Summary: "workflow gate evaluated", CreatedAt: endedAt,
		})
	}
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q success: %w", workflowRunID, nodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// FailNode atomically closes a non-successful Attempt and appends its terminal event.
func (workflowStore *Store) FailNode(
	ctx context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	status RunStatus,
	errorCode string,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	eventKind := "node_failed"
	summary := "workflow node failed"
	if status == RunWaitingHuman {
		eventKind = "human_review_required"
		summary = "workflow node requires human approval"
	} else if status != RunFailed && status != RunCancelled && status != RunTimedOut {
		return fmt.Errorf("workflow node terminal status %q is invalid", status)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q failure: %w", workflowRunID, nodeID, err)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if runStatus != RunRunning {
		if runStatus == RunCancelled && status == RunCancelled {
			nodeStatus, statusErr := getNodeRunStatusTx(
				ctx, tx, workflowRunID, nodeID, attempt,
			)
			if statusErr == nil && nodeStatus == RunCancelled {
				return nil
			}
			if statusErr != nil {
				return statusErr
			}
		}
		return fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID, runStatus, ErrConflict,
		)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
		SET agent_run_id=?,status=?,error_code=?,
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
		agentRunID, status, errorCode,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, nodeID, attempt, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("fail workflow node %q/%q: %w", workflowRunID, nodeID, err)
	}
	if err := requireSingleTransition(result, workflowRunID+"/"+nodeID); err != nil {
		return err
	}
	if err := accumulateWorkflowUsageTx(ctx, tx, workflowRunID, usage); err != nil {
		return err
	}
	events := []Event{{
		WorkflowRunID: workflowRunID, Kind: eventKind, NodeID: nodeID,
		Summary: summary, CreatedAt: endedAt,
	}}
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q failure: %w", workflowRunID, nodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// FinishWorkflow atomically records an optional final output and the Run terminal state.
func (workflowStore *Store) FinishWorkflow(
	ctx context.Context,
	workflowRunID string,
	status RunStatus,
	errorCode string,
	output *Handoff,
	endedAt time.Time,
) error {
	eventKind, summary, err := workflowTerminalEvent(status)
	if err != nil {
		return err
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow %q completion: %w", workflowRunID, err)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if runStatus != RunRunning {
		if runStatus == RunCancelled && status == RunCancelled {
			return nil
		}
		return fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID, runStatus, ErrConflict,
		)
	}
	events := make([]Event, 0, 2)
	if output != nil {
		if err := saveHandoffTx(ctx, tx, *output); err != nil {
			return err
		}
		events = append(events, Event{
			WorkflowRunID: workflowRunID, Kind: "handoff_created", NodeID: output.ProducerNodeID,
			Summary: "workflow output created", CreatedAt: endedAt,
		})
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`,
		status, errorCode, store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("complete workflow run %q: %w", workflowRunID, err)
	}
	if err := requireSingleTransition(result, workflowRunID); err != nil {
		return err
	}
	events = append(events, Event{
		WorkflowRunID: workflowRunID, Kind: eventKind, Summary: summary, CreatedAt: endedAt,
	})
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow %q completion: %w", workflowRunID, err)
	}
	workflowStore.publish(events)
	return nil
}

func (workflowStore *Store) GetRun(ctx context.Context, id string) (*WorkflowRunRecord, error) {
	var run WorkflowRunRecord
	row := workflowStore.db.QueryRowContext(ctx, `SELECT
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at,ended_at
		FROM workflow_runs WHERE id=? LIMIT 1`, id)
	if err := scanWorkflowRun(row, &run); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get workflow run %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get workflow run %q: %w", id, err)
	}
	return &run, nil
}

// ListNodeRuns returns all Attempts in stable keyset order.
func (workflowStore *Store) ListNodeRuns(
	ctx context.Context,
	workflowRunID string,
	cursor NodeRunCursor,
	limit int,
) ([]NodeRunRecord, error) {
	limit = boundedLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	const columns = `SELECT
		current.workflow_run_id,current.node_id,current.attempt,current.kind,
		current.agent_run_id,current.input_handoff_ids_json,current.output_handoff_id,
		current.status,current.error_code,current.input_tokens,current.output_tokens,
		current.reasoning_tokens,current.total_tokens,current.tool_call_count,
		current.cost_micros,current.retry_count,current.started_at,
		(SELECT MIN(first.started_at) FROM workflow_node_runs first
			WHERE first.workflow_run_id=current.workflow_run_id
			AND first.node_id=current.node_id),
		current.ended_at
		FROM workflow_node_runs current`
	if cursor.NodeID == "" {
		rows, err = workflowStore.db.QueryContext(
			ctx,
			columns+`
			WHERE current.workflow_run_id=?
			ORDER BY current.node_id,current.attempt LIMIT ?`,
			workflowRunID,
			limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(
			ctx,
			columns+`
			WHERE current.workflow_run_id=?
			AND (current.node_id>? OR (current.node_id=? AND current.attempt>?))
			ORDER BY current.node_id,current.attempt LIMIT ?`,
			workflowRunID,
			cursor.NodeID,
			cursor.NodeID,
			cursor.Attempt,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow node runs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	runs := make([]NodeRunRecord, 0, limit)
	for rows.Next() {
		run, err := scanNodeRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow node run %q: %w", workflowRunID, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow node runs %q: %w", workflowRunID, err)
	}
	return runs, nil
}

// ListActiveRuns pages only resumable run identities below a startup cutoff.
func (workflowStore *Store) ListActiveRuns(
	ctx context.Context,
	startedBefore time.Time,
	cursor ActiveRunCursor,
	limit int,
) ([]ActiveRunRef, error) {
	limit = boundedLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			ORDER BY started_at,id LIMIT ?`,
			RunRunning,
			store.DatabaseTime(startedBefore.UTC().Format(time.RFC3339Nano)),
			limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			AND (started_at>? OR (started_at=? AND id>?))
			ORDER BY started_at,id LIMIT ?`,
			RunRunning,
			store.DatabaseTime(startedBefore.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.StartedAt.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.StartedAt.UTC().Format(time.RFC3339Nano)),
			cursor.ID,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list active workflow runs: %w", err)
	}
	defer rows.Close()
	runs := make([]ActiveRunRef, 0, limit)
	for rows.Next() {
		var run ActiveRunRef
		if err := rows.Scan(&run.ID, &run.StartedAt); err != nil {
			return nil, fmt.Errorf("scan active workflow run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active workflow runs: %w", err)
	}
	return runs, nil
}

// CancelWorkflow atomically closes active node Attempts before the Run.
func (workflowStore *Store) CancelWorkflow(
	ctx context.Context,
	workflowRunID string,
	endedAt time.Time,
) (CancelTransition, error) {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("begin workflow %q cancellation: %w", workflowRunID, err)
	}
	defer tx.Rollback()
	status, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return CancelTransition{}, err
	}
	if status != RunRunning && status != RunWaitingHuman {
		if err := tx.Commit(); err != nil {
			return CancelTransition{}, fmt.Errorf(
				"commit idempotent workflow %q cancellation: %w",
				workflowRunID, err,
			)
		}
		return CancelTransition{Status: status}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id,attempt
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND status IN (?,?)
		ORDER BY node_id,attempt FOR UPDATE`,
		workflowRunID, RunRunning, RunWaitingHuman,
	)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("lock workflow %q active nodes: %w", workflowRunID, err)
	}
	type activeNode struct {
		id      string
		attempt int
	}
	nodes := make([]activeNode, 0)
	for rows.Next() {
		var node activeNode
		if err := rows.Scan(&node.id, &node.attempt); err != nil {
			rows.Close()
			return CancelTransition{}, fmt.Errorf("scan workflow %q active node: %w", workflowRunID, err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Close(); err != nil {
		return CancelTransition{}, fmt.Errorf("close workflow %q active nodes: %w", workflowRunID, err)
	}
	if err := rows.Err(); err != nil {
		return CancelTransition{}, fmt.Errorf("iterate workflow %q active nodes: %w", workflowRunID, err)
	}
	events := make([]Event, 0, len(nodes)+1)
	for _, node := range nodes {
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET status=?,error_code=?,ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=?
			AND status IN (?,?)`,
			RunCancelled,
			"workflow_cancelled",
			store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
			workflowRunID,
			node.id,
			node.attempt,
			RunRunning,
			RunWaitingHuman,
		)
		if err != nil {
			return CancelTransition{}, fmt.Errorf(
				"cancel workflow node %q/%q attempt %d: %w",
				workflowRunID, node.id, node.attempt, err,
			)
		}
		if err := requireSingleTransition(result, workflowRunID+"/"+node.id); err != nil {
			return CancelTransition{}, err
		}
		events = append(events, Event{
			WorkflowRunID: workflowRunID,
			Kind:          "node_cancelled",
			NodeID:        node.id,
			Summary:       "workflow node cancelled",
			CreatedAt:     endedAt,
		})
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=?
		WHERE id=? AND status IN (?,?)`,
		RunCancelled,
		"workflow_cancelled",
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID,
		RunRunning,
		RunWaitingHuman,
	)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("cancel workflow run %q: %w", workflowRunID, err)
	}
	if err := requireSingleTransition(result, workflowRunID); err != nil {
		return CancelTransition{}, err
	}
	events = append(events, Event{
		WorkflowRunID: workflowRunID,
		Kind:          "workflow_cancelled",
		Summary:       "workflow cancelled",
		CreatedAt:     endedAt,
	})
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return CancelTransition{}, err
	}
	if err := tx.Commit(); err != nil {
		return CancelTransition{}, fmt.Errorf("commit workflow %q cancellation: %w", workflowRunID, err)
	}
	workflowStore.publish(events)
	return CancelTransition{Applied: true, Status: RunCancelled}, nil
}

func (workflowStore *Store) CreateRun(ctx context.Context, run WorkflowRunRecord) error {
	budget, err := json.Marshal(run.Budget)
	if err != nil {
		return fmt.Errorf("marshal workflow budget: %w", err)
	}
	selection, err := json.Marshal(run.Selection)
	if err != nil {
		return fmt.Errorf("marshal workflow definition selection: %w", err)
	}
	actorPermissions, err := json.Marshal(run.ActorPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow actor permissions: %w", err)
	}
	scenarioPermissions, err := json.Marshal(run.ScenarioPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow scenario permissions: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO workflow_runs(
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ParentRunID, run.WorkflowID, run.WorkflowVersion, run.WorkflowHash, selection, run.InputHash,
		run.ActorUserID, run.ActorTenantID, actorPermissions, run.Scenario,
		scenarioPermissions, run.Status, budget,
		run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.ReasoningTokens,
		run.Usage.TotalTokens, run.Usage.ToolCalls, run.Usage.CostMicros,
		run.Usage.Retries, run.ErrorCode,
		store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("create workflow run %q: %w", run.ID, err)
	}
	return nil
}

// DecideHumanApproval atomically records the immutable decision and its state transition.
func (workflowStore *Store) DecideHumanApproval(
	ctx context.Context,
	approval WorkflowApproval,
	approvedHandoff *Handoff,
) (ApprovalTransition, error) {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalTransition{}, fmt.Errorf(
			"begin workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, approval.WorkflowRunID)
	if err != nil {
		return ApprovalTransition{}, err
	}
	existing, err := getApprovalTx(ctx, tx, approval.WorkflowRunID, approval.NodeID)
	if err == nil {
		if existing.Decision != approval.Decision {
			return ApprovalTransition{}, fmt.Errorf(
				"%w for workflow node %q/%q",
				ErrApprovalConflict, approval.WorkflowRunID, approval.NodeID,
			)
		}
		if err := tx.Commit(); err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"commit idempotent workflow approval %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		return ApprovalTransition{
			Approval: *existing, Applied: false, RunStatus: runStatus,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ApprovalTransition{}, err
	}
	if runStatus != RunWaitingHuman {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow run %q is %q, expected %q: %w",
			approval.WorkflowRunID, runStatus, RunWaitingHuman, ErrConflict,
		)
	}
	attempt, kind, nodeStatus, err := lockLatestNodeRun(
		ctx, tx, approval.WorkflowRunID, approval.NodeID,
	)
	if err != nil {
		return ApprovalTransition{}, err
	}
	if kind != NodeHumanApproval {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow node %q/%q kind %q does not require human approval: %w",
			approval.WorkflowRunID, approval.NodeID, kind, ErrConflict,
		)
	}
	if nodeStatus != RunWaitingHuman {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow node %q/%q is %q, expected %q: %w",
			approval.WorkflowRunID, approval.NodeID, nodeStatus, RunWaitingHuman, ErrConflict,
		)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_approvals(
		workflow_run_id,node_id,decision,approver_user_id,approver_tenant_id,
		comment,decided_at) VALUES(?,?,?,?,?,?,?)`,
		approval.WorkflowRunID, approval.NodeID, approval.Decision,
		approval.ApproverUserID, approval.ApproverTenantID, approval.Comment,
		store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return ApprovalTransition{}, fmt.Errorf(
			"save workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	var events []Event
	switch approval.Decision {
	case ApprovalApproved:
		if approvedHandoff == nil {
			return ApprovalTransition{}, fmt.Errorf(
				"approved workflow node %q/%q requires a handoff: %w",
				approval.WorkflowRunID, approval.NodeID, ErrInvalid,
			)
		}
		if err := saveHandoffTx(ctx, tx, *approvedHandoff); err != nil {
			return ApprovalTransition{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET output_handoff_id=?,status=?,error_code='',ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
			approvedHandoff.ID, RunSucceeded,
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, approval.NodeID, attempt, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"approve workflow node %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
			return ApprovalTransition{}, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
			SET status=?,error_code='',ended_at=NULL WHERE id=? AND status=?`,
			RunRunning, approval.WorkflowRunID, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"resume workflow run %q: %w", approval.WorkflowRunID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
			return ApprovalTransition{}, err
		}
		events = []Event{
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "human_approved",
				NodeID: approval.NodeID, Summary: "workflow node approved",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "handoff_created",
				NodeID: approval.NodeID, Summary: "approved handoff created",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "node_succeeded",
				NodeID: approval.NodeID, Summary: "workflow node succeeded",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "workflow_resumed",
				Summary: "workflow resumed", CreatedAt: approval.DecidedAt,
			},
		}
		if err := appendEventsTx(ctx, tx, approval.WorkflowRunID, events); err != nil {
			return ApprovalTransition{}, err
		}
		runStatus = RunRunning
	case ApprovalRejected:
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET status=?,error_code=?,ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
			RunFailed, "human_approval_rejected",
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, approval.NodeID, attempt, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"reject workflow node %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
			return ApprovalTransition{}, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
			SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`,
			RunFailed, "human_approval_rejected",
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"reject workflow run %q: %w", approval.WorkflowRunID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
			return ApprovalTransition{}, err
		}
		events = []Event{
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "human_rejected",
				NodeID: approval.NodeID, Summary: "workflow node rejected",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "node_failed",
				NodeID: approval.NodeID, Summary: "workflow node failed",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "workflow_failed",
				Summary: "workflow failed", CreatedAt: approval.DecidedAt,
			},
		}
		if err := appendEventsTx(ctx, tx, approval.WorkflowRunID, events); err != nil {
			return ApprovalTransition{}, err
		}
		runStatus = RunFailed
	default:
		return ApprovalTransition{}, fmt.Errorf(
			"workflow approval decision %q is invalid: %w",
			approval.Decision, ErrInvalid,
		)
	}
	if err := tx.Commit(); err != nil {
		return ApprovalTransition{}, fmt.Errorf(
			"commit workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	workflowStore.publish(events)
	return ApprovalTransition{
		Approval: approval, Applied: true, RunStatus: runStatus,
	}, nil
}

// LoadFullRunState reads the complete durable checkpoint required for resumption.
func (workflowStore *Store) LoadFullRunState(
	ctx context.Context,
	workflowRunID string,
) (*WorkflowRunState, error) {
	run, err := workflowStore.GetRun(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	state := &WorkflowRunState{
		Run: *run, Nodes: make(map[string]NodeRunRecord),
		Handoffs: make(map[string]Handoff), NodeOutputs: make(map[string]Handoff),
		Gates: make(map[string]GateDecision), Approvals: make(map[string]WorkflowApproval),
	}
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		current.workflow_run_id,current.node_id,current.attempt,current.kind,
		current.agent_run_id,current.input_handoff_ids_json,current.output_handoff_id,
		current.status,current.error_code,current.input_tokens,current.output_tokens,
		current.reasoning_tokens,current.total_tokens,current.tool_call_count,
		current.cost_micros,current.retry_count,current.started_at,
		(SELECT MIN(first.started_at) FROM workflow_node_runs first
			WHERE first.workflow_run_id=current.workflow_run_id
			AND first.node_id=current.node_id),
		current.ended_at
		FROM workflow_node_runs current
		WHERE current.workflow_run_id=? AND current.attempt=(
			SELECT MAX(latest.attempt) FROM workflow_node_runs latest
			WHERE latest.workflow_run_id=current.workflow_run_id
			AND latest.node_id=current.node_id)
		ORDER BY current.node_id`, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("load workflow node checkpoint %q: %w", workflowRunID, err)
	}
	for rows.Next() {
		node, err := scanNodeRun(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workflow node checkpoint %q: %w", workflowRunID, err)
		}
		state.Nodes[node.NodeID] = node
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close workflow node checkpoint %q: %w", workflowRunID, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow node checkpoint %q: %w", workflowRunID, err)
	}
	handoffs, err := workflowStore.loadFullHandoffs(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	for _, handoff := range handoffs {
		state.Handoffs[handoff.ID] = handoff
		if handoff.ProducerNodeID == "workflow.input" {
			if state.Input.ID != "" {
				return nil, fmt.Errorf("workflow run %q has multiple input handoffs", workflowRunID)
			}
			state.Input = handoff
		}
	}
	if state.Input.ID == "" {
		return nil, fmt.Errorf("workflow run %q input handoff is missing", workflowRunID)
	}
	for nodeID, node := range state.Nodes {
		if node.Status != RunSucceeded {
			continue
		}
		handoff, ok := state.Handoffs[node.OutputHandoffID]
		if !ok {
			return nil, fmt.Errorf(
				"workflow node %q/%q output handoff %q is missing",
				workflowRunID, nodeID, node.OutputHandoffID,
			)
		}
		state.NodeOutputs[nodeID] = handoff
	}
	if err := workflowStore.loadFullGateDecisions(ctx, workflowRunID, state.Gates); err != nil {
		return nil, err
	}
	if err := workflowStore.loadFullApprovals(ctx, workflowRunID, state.Approvals); err != nil {
		return nil, err
	}
	return state, nil
}

func (workflowStore *Store) CreateNodeRun(ctx context.Context, run NodeRunRecord) error {
	inputs, err := json.Marshal(run.InputHandoffIDs)
	if err != nil {
		return fmt.Errorf("marshal workflow node inputs: %w", err)
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO workflow_node_runs(
		workflow_run_id,node_id,attempt,kind,agent_run_id,input_handoff_ids_json,
		output_handoff_id,status,error_code,input_tokens,output_tokens,reasoning_tokens,
		total_tokens,tool_call_count,cost_micros,retry_count,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.WorkflowRunID, run.NodeID, run.Attempt, run.Kind, run.AgentRunID, inputs,
		run.OutputHandoffID, run.Status, run.ErrorCode,
		run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.ReasoningTokens,
		run.Usage.TotalTokens, run.Usage.ToolCalls, run.Usage.CostMicros,
		run.Usage.Retries,
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
	workflowStore.hub.Publish(event)
	return nil
}

func (workflowStore *Store) SubscribeEvents(runID string) (<-chan Event, func()) {
	return workflowStore.hub.Subscribe(runID)
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
	var (
		rows *sql.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT
			id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
			payload_json,references_json,completeness,content_hash,created_at
			FROM handoff_artifacts
			WHERE workflow_run_id=?
			ORDER BY created_at,id LIMIT ?`,
			workflowRunID, limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT
			id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
			payload_json,references_json,completeness,content_hash,created_at
			FROM handoff_artifacts
			WHERE workflow_run_id=? AND (created_at>? OR (created_at=? AND id>?))
			ORDER BY created_at,id LIMIT ?`,
			workflowRunID,
			store.DatabaseTime(cursor.CreatedAt.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.CreatedAt.UTC().Format(time.RFC3339Nano)),
			cursor.ID,
			limit,
		)
	}
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

type rowScanner interface {
	Scan(...any) error
}

func scanNodeRun(row rowScanner) (NodeRunRecord, error) {
	var run NodeRunRecord
	var inputs []byte
	var endedAt sql.NullTime
	if err := row.Scan(
		&run.WorkflowRunID,
		&run.NodeID,
		&run.Attempt,
		&run.Kind,
		&run.AgentRunID,
		&inputs,
		&run.OutputHandoffID,
		&run.Status,
		&run.ErrorCode,
		&run.Usage.InputTokens,
		&run.Usage.OutputTokens,
		&run.Usage.ReasoningTokens,
		&run.Usage.TotalTokens,
		&run.Usage.ToolCalls,
		&run.Usage.CostMicros,
		&run.Usage.Retries,
		&run.StartedAt,
		&run.FirstStartedAt,
		&endedAt,
	); err != nil {
		return NodeRunRecord{}, err
	}
	if err := json.Unmarshal(inputs, &run.InputHandoffIDs); err != nil {
		return NodeRunRecord{}, fmt.Errorf("decode input handoffs: %w", err)
	}
	if endedAt.Valid {
		ended := endedAt.Time
		run.EndedAt = &ended
	}
	return run, nil
}

func scanWorkflowRun(row rowScanner, run *WorkflowRunRecord) error {
	var selection, budget, actorPermissions, scenarioPermissions []byte
	var endedAt sql.NullTime
	if err := row.Scan(
		&run.ID, &run.ParentRunID, &run.WorkflowID, &run.WorkflowVersion, &run.WorkflowHash,
		&selection, &run.InputHash, &run.ActorUserID, &run.ActorTenantID, &actorPermissions,
		&run.Scenario, &scenarioPermissions, &run.Status, &budget,
		&run.Usage.InputTokens, &run.Usage.OutputTokens, &run.Usage.ReasoningTokens,
		&run.Usage.TotalTokens, &run.Usage.ToolCalls, &run.Usage.CostMicros,
		&run.Usage.Retries, &run.ErrorCode,
		&run.StartedAt, &endedAt,
	); err != nil {
		return err
	}
	if err := json.Unmarshal(selection, &run.Selection); err != nil {
		return fmt.Errorf("decode workflow definition selection: %w", err)
	}
	if err := json.Unmarshal(budget, &run.Budget); err != nil {
		return fmt.Errorf("decode budget: %w", err)
	}
	if err := json.Unmarshal(actorPermissions, &run.ActorPermissions); err != nil {
		return fmt.Errorf("decode actor permissions: %w", err)
	}
	if err := json.Unmarshal(scenarioPermissions, &run.ScenarioPermissions); err != nil {
		return fmt.Errorf("decode scenario permissions: %w", err)
	}
	if endedAt.Valid {
		ended := endedAt.Time
		run.EndedAt = &ended
	}
	return nil
}

func accumulateWorkflowUsageTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	usage WorkflowUsage,
) error {
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET input_tokens=input_tokens+?,output_tokens=output_tokens+?,
			reasoning_tokens=reasoning_tokens+?,total_tokens=total_tokens+?,
			tool_call_count=tool_call_count+?,cost_micros=cost_micros+?,
			retry_count=retry_count+?
		WHERE id=? AND status=?`,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		workflowRunID, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("accumulate workflow run %q usage: %w", workflowRunID, err)
	}
	return requireSingleTransition(result, workflowRunID+"/usage")
}

func lockWorkflow(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
) (RunStatus, error) {
	var status RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
		workflowRunID,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("lock workflow run %q: %w", workflowRunID, ErrNotFound)
		}
		return "", fmt.Errorf("lock workflow run %q: %w", workflowRunID, err)
	}
	return status, nil
}

func lockRunningWorkflow(ctx context.Context, tx *sql.Tx, workflowRunID string) error {
	status, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if status != RunRunning {
		return fmt.Errorf("workflow run %q is %q: %w", workflowRunID, status, ErrConflict)
	}
	return nil
}

func getNodeRunStatusTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
	attempt int,
) (RunStatus, error) {
	var status RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND attempt=? LIMIT 1`,
		workflowRunID, nodeID, attempt,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf(
				"get workflow node %q/%q attempt %d: %w",
				workflowRunID, nodeID, attempt, ErrNotFound,
			)
		}
		return "", fmt.Errorf(
			"get workflow node %q/%q attempt %d: %w",
			workflowRunID, nodeID, attempt, err,
		)
	}
	return status, nil
}

func getApprovalTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
) (*WorkflowApproval, error) {
	var approval WorkflowApproval
	if err := tx.QueryRowContext(ctx, `SELECT
		workflow_run_id,node_id,decision,approver_user_id,approver_tenant_id,
		comment,decided_at
		FROM workflow_approvals
		WHERE workflow_run_id=? AND node_id=? LIMIT 1`,
		workflowRunID, nodeID,
	).Scan(
		&approval.WorkflowRunID, &approval.NodeID, &approval.Decision,
		&approval.ApproverUserID, &approval.ApproverTenantID, &approval.Comment,
		&approval.DecidedAt,
	); err != nil {
		return nil, fmt.Errorf(
			"get workflow approval %q/%q: %w",
			workflowRunID, nodeID, err,
		)
	}
	return &approval, nil
}

func lockLatestNodeRun(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
) (int, NodeKind, RunStatus, error) {
	var attempt int
	var kind NodeKind
	var status RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT attempt,kind,status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=?
		ORDER BY attempt DESC LIMIT 1 FOR UPDATE`,
		workflowRunID, nodeID,
	).Scan(&attempt, &kind, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", "", fmt.Errorf(
				"lock workflow node %q/%q: %w",
				workflowRunID, nodeID, ErrNotFound,
			)
		}
		return 0, "", "", fmt.Errorf(
			"lock workflow node %q/%q: %w",
			workflowRunID, nodeID, err,
		)
	}
	return attempt, kind, status, nil
}

func (workflowStore *Store) loadFullHandoffs(
	ctx context.Context,
	workflowRunID string,
) ([]Handoff, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at
		FROM handoff_artifacts
		WHERE workflow_run_id=?
		ORDER BY created_at,id`, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("load full workflow handoffs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	handoffs := make([]Handoff, 0)
	for rows.Next() {
		var handoff Handoff
		var references []byte
		if err := rows.Scan(
			&handoff.ID, &handoff.WorkflowRunID, &handoff.ProducerNodeID,
			&handoff.ProducerRunID, &handoff.Schema.ID, &handoff.Schema.Version,
			&handoff.Payload, &references, &handoff.Completeness,
			&handoff.ContentHash, &handoff.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan full workflow handoff %q: %w", workflowRunID, err)
		}
		if err := json.Unmarshal(references, &handoff.References); err != nil {
			return nil, fmt.Errorf(
				"decode full workflow handoff %q references: %w",
				handoff.ID, err,
			)
		}
		handoffs = append(handoffs, handoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate full workflow handoffs %q: %w", workflowRunID, err)
	}
	return handoffs, nil
}

func (workflowStore *Store) loadFullGateDecisions(
	ctx context.Context,
	workflowRunID string,
	out map[string]GateDecision,
) error {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		node_id,gate_id,subject_hash,decision,reason_codes_json,finding_ids_json,
		evaluated_at
		FROM gate_decisions
		WHERE workflow_run_id=?
		ORDER BY node_id,gate_id`, workflowRunID)
	if err != nil {
		return fmt.Errorf("load full workflow gates %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		var decision GateDecision
		var reasons, findings []byte
		if err := rows.Scan(
			&nodeID, &decision.GateID, &decision.SubjectHash, &decision.Decision,
			&reasons, &findings, &decision.EvaluatedAt,
		); err != nil {
			return fmt.Errorf("scan full workflow gate %q: %w", workflowRunID, err)
		}
		if err := json.Unmarshal(reasons, &decision.ReasonCodes); err != nil {
			return fmt.Errorf("decode workflow gate %q reasons: %w", nodeID, err)
		}
		if err := json.Unmarshal(findings, &decision.FindingIDs); err != nil {
			return fmt.Errorf("decode workflow gate %q findings: %w", nodeID, err)
		}
		if _, duplicate := out[nodeID]; duplicate {
			return fmt.Errorf("workflow run %q has multiple gate facts for node %q", workflowRunID, nodeID)
		}
		out[nodeID] = decision
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate full workflow gates %q: %w", workflowRunID, err)
	}
	return nil
}

func (workflowStore *Store) loadFullApprovals(
	ctx context.Context,
	workflowRunID string,
	out map[string]WorkflowApproval,
) error {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		workflow_run_id,node_id,decision,approver_user_id,approver_tenant_id,
		comment,decided_at
		FROM workflow_approvals
		WHERE workflow_run_id=?
		ORDER BY node_id`, workflowRunID)
	if err != nil {
		return fmt.Errorf("load full workflow approvals %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var approval WorkflowApproval
		if err := rows.Scan(
			&approval.WorkflowRunID, &approval.NodeID, &approval.Decision,
			&approval.ApproverUserID, &approval.ApproverTenantID,
			&approval.Comment, &approval.DecidedAt,
		); err != nil {
			return fmt.Errorf("scan full workflow approval %q: %w", workflowRunID, err)
		}
		out[approval.NodeID] = approval
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate full workflow approvals %q: %w", workflowRunID, err)
	}
	return nil
}

func saveHandoffTx(ctx context.Context, tx *sql.Tx, handoff Handoff) error {
	references, err := json.Marshal(handoff.References)
	if err != nil {
		return fmt.Errorf("marshal handoff references: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		handoff.ID, handoff.WorkflowRunID, handoff.ProducerNodeID, handoff.ProducerRunID,
		handoff.Schema.ID, handoff.Schema.Version, handoff.Payload, references,
		handoff.Completeness, handoff.ContentHash,
		store.DatabaseTime(handoff.CreatedAt.UTC().Format(time.RFC3339Nano)),
	)
	if err != nil {
		return fmt.Errorf("save handoff %q: %w", handoff.ID, err)
	}
	return nil
}

func saveGateDecisionTx(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	workflowRunID string,
	nodeID string,
	decision GateDecision,
) error {
	reasons, err := json.Marshal(decision.ReasonCodes)
	if err != nil {
		return fmt.Errorf("marshal gate reason codes: %w", err)
	}
	findings, err := json.Marshal(decision.FindingIDs)
	if err != nil {
		return fmt.Errorf("marshal gate finding ids: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gate_decisions(
		id,workflow_run_id,node_id,gate_id,subject_hash,decision,reason_codes_json,
		finding_ids_json,evaluated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, workflowRunID, nodeID, decision.GateID, decision.SubjectHash,
		decision.Decision, reasons, findings,
		store.DatabaseTime(decision.EvaluatedAt.UTC().Format(time.RFC3339Nano)),
	)
	if err != nil {
		return fmt.Errorf("save gate decision %q: %w", id, err)
	}
	return nil
}

func appendEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	events []Event,
) error {
	if len(events) == 0 {
		return nil
	}
	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM workflow_events WHERE workflow_run_id=?`,
		workflowRunID,
	).Scan(&nextSeq); err != nil {
		return fmt.Errorf("allocate workflow event sequence for %q: %w", workflowRunID, err)
	}
	for index, event := range events {
		events[index].Seq = nextSeq + int64(index)
		event = events[index]
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_events(
			workflow_run_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			workflowRunID, event.Seq, event.Kind, event.NodeID, event.Summary,
			nullableJSON(event.Detail),
			store.DatabaseTime(event.CreatedAt.UTC().Format(time.RFC3339Nano)),
		); err != nil {
			return fmt.Errorf("append workflow event %q/%d: %w", workflowRunID, event.Seq, err)
		}
	}
	return nil
}

func requireSingleTransition(result sql.Result, target string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read workflow transition %q result: %w", target, err)
	}
	if affected != 1 {
		return fmt.Errorf(
			"workflow transition %q changed %d rows: %w",
			target, affected, ErrConflict,
		)
	}
	return nil
}

func (workflowStore *Store) publish(events []Event) {
	for _, event := range events {
		workflowStore.hub.Publish(event)
	}
}

func workflowTerminalEvent(status RunStatus) (string, string, error) {
	switch status {
	case RunSucceeded:
		return "workflow_succeeded", "workflow succeeded", nil
	case RunFailed:
		return "workflow_failed", "workflow failed", nil
	case RunCancelled:
		return "workflow_cancelled", "workflow cancelled", nil
	case RunTimedOut:
		return "workflow_timed_out", "workflow timed out", nil
	case RunWaitingHuman:
		return "human_review_required", "workflow requires human approval", nil
	default:
		return "", "", fmt.Errorf("workflow terminal status %q is invalid", status)
	}
}
