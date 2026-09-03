package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// PutWorkflowArtifact stores an immutable secondary artifact idempotently.
// A repeated write is accepted only when the content hash is identical.
func validateWorkflowArtifact(artifact WorkflowArtifact) error {
	if artifact.ID == "" || artifact.WorkflowRunID == "" || artifact.ProducerNodeID == "" ||
		artifact.Kind == "" || artifact.Schema.ID == "" || len(artifact.Content) == 0 {
		return fmt.Errorf("workflow artifact fields are required")
	}
	if !json.Valid(artifact.Content) {
		return fmt.Errorf("workflow artifact %q content is not valid JSON", artifact.ID)
	}
	sum := sha256.Sum256(artifact.Content)
	hash := hex.EncodeToString(sum[:])
	if artifact.ContentHash != "" && artifact.ContentHash != hash {
		return fmt.Errorf("workflow artifact %q content hash mismatch", artifact.ID)
	}
	return nil
}

func contentHashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func ensureWorkflowArtifactInsert(ctx context.Context, workflowStore *Store, artifact WorkflowArtifact) error {
	var existingHash string
	err := workflowStore.db.QueryRowContext(ctx,
		`SELECT content_hash FROM handoff_artifacts WHERE id=? LIMIT 1`, artifact.ID,
	).Scan(&existingHash)
	switch {
	case err == nil:
		if existingHash != artifact.ContentHash {
			return fmt.Errorf("workflow artifact %q content conflicts: %w", artifact.ID, ErrConflict)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check workflow artifact %q: %w", artifact.ID, err)
	}

	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,evidence_units_json,evidence_conflicts_json,
		completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		artifact.ID, artifact.WorkflowRunID, artifact.ProducerNodeID, "",
		artifact.Schema.ID, artifact.Schema.Version, artifact.Content,
		[]byte("null"), []byte("null"), []byte("null"), Partial,
		artifact.ContentHash, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	)
	if err != nil {
		// A concurrent identical insert is safe; verify the resulting row.
		var concurrentHash string
		if queryErr := workflowStore.db.QueryRowContext(ctx,
			`SELECT content_hash FROM handoff_artifacts WHERE id=? LIMIT 1`, artifact.ID,
		).Scan(&concurrentHash); queryErr == nil && concurrentHash == artifact.ContentHash {
			return nil
		}
		return fmt.Errorf("save workflow artifact %q: %w", artifact.ID, err)
	}
	return nil
}

func (workflowStore *Store) PutWorkflowArtifact(
	ctx context.Context,
	artifact WorkflowArtifact,
) error {
	if err := validateWorkflowArtifact(artifact); err != nil {
		return err
	}
	artifact.ContentHash = contentHashOf(artifact.Content)
	if err := ensureWorkflowArtifactInsert(ctx, workflowStore, artifact); err != nil {
		return err
	}
	return nil
}

func (workflowStore *Store) CreateRun(ctx context.Context, run RunRecord) error {
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
		id,parent_run_id,round_number,base_depth,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,stop_reason,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ParentRunID, run.Round, run.BaseDepth,
		run.WorkflowID, run.WorkflowVersion, run.WorkflowHash, selection, run.InputHash,
		run.ActorUserID, run.ActorTenantID, actorPermissions, run.Scenario,
		scenarioPermissions, run.Status, budget,
		run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.ReasoningTokens,
		run.Usage.TotalTokens, run.Usage.ToolCalls, run.Usage.CostMicros,
		run.Usage.Retries, run.ErrorCode, run.StopReason,
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
	references, evidenceUnits, evidenceConflicts, err := marshalHandoffJSON(handoff)
	if err != nil {
		return err
	}
	_, err = workflowStore.db.ExecContext(ctx, `INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,evidence_units_json,evidence_conflicts_json,
		completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		handoff.ID, handoff.WorkflowRunID, handoff.ProducerNodeID, handoff.ProducerRunID,
		handoff.Schema.ID, handoff.Schema.Version, handoff.Payload, references,
		evidenceUnits, evidenceConflicts, handoff.Completeness, handoff.ContentHash,
		store.DatabaseTime(handoff.CreatedAt.UTC().Format(time.RFC3339)),
	)
	if err != nil {
		return fmt.Errorf("save handoff %q: %w", handoff.ID, err)
	}
	return nil
}

func marshalHandoffJSON(
	handoff Handoff,
) ([]byte, []byte, []byte, error) {
	references, err := json.Marshal(handoff.References)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal handoff references: %w", err)
	}
	evidenceUnits, err := json.Marshal(handoff.EvidenceUnits)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal handoff evidence units: %w", err)
	}
	evidenceConflicts, err := json.Marshal(handoff.EvidenceConflicts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal handoff evidence conflicts: %w", err)
	}
	return references, evidenceUnits, evidenceConflicts, nil
}
