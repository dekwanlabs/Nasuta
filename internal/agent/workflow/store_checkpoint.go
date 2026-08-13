package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

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

func (workflowStore *Store) loadFullHandoffs(
	ctx context.Context,
	workflowRunID string,
) ([]Handoff, error) {
	rows, err := workflowStore.db.QueryContext(ctx, `SELECT
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,evidence_units_json,evidence_conflicts_json,
		completeness,content_hash,created_at
		FROM handoff_artifacts
		WHERE workflow_run_id=?
		ORDER BY created_at,id`, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("load full workflow handoffs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	handoffs := make([]Handoff, 0)
	for rows.Next() {
		handoff, err := scanHandoff(rows)
		if err != nil {
			return nil, fmt.Errorf("scan full workflow handoff %q: %w", workflowRunID, err)
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
		node_id,gate_id,gate_subject_hash,gate_decision,gate_reason_codes_json,
		gate_finding_ids_json,gate_evaluated_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND gate_decision_id IS NOT NULL
		ORDER BY node_id,attempt`, workflowRunID)
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
		workflow_run_id,node_id,approval_decision,approver_user_id,
		approver_tenant_id,approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND approval_decision IS NOT NULL
		ORDER BY node_id,attempt`, workflowRunID)
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
		if _, duplicate := out[approval.NodeID]; duplicate {
			return fmt.Errorf(
				"workflow run %q has multiple approval facts for node %q",
				workflowRunID, approval.NodeID,
			)
		}
		out[approval.NodeID] = approval
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate full workflow approvals %q: %w", workflowRunID, err)
	}
	return nil
}
