package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

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
