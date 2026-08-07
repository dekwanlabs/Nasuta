package evaluation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const (
	maxTraceNodes   = 512
	maxTraceAgents  = 512
	maxTraceEvents  = 1000
	maxReviewLabels = 100
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("evaluation store database is required")
	}
	return &Store{db: db}, nil
}

func (evaluationStore *Store) WorkflowTrace(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*WorkflowTrace, error) {
	run, err := evaluationStore.getWorkflowRun(ctx, runID, userID, admin)
	if err != nil {
		return nil, err
	}
	nodes, nodesTruncated, err := evaluationStore.listTraceNodes(ctx, runID)
	if err != nil {
		return nil, err
	}
	agents, agentsTruncated, err := evaluationStore.listTraceAgents(ctx, runID)
	if err != nil {
		return nil, err
	}
	events, eventsTruncated, err := evaluationStore.listTraceEvents(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &WorkflowTrace{
		Run: *run, Nodes: nodes, Agents: agents, Events: events,
		Truncated: TraceTruncation{
			Nodes: nodesTruncated, Agents: agentsTruncated, Events: eventsTruncated,
		},
	}, nil
}

func (evaluationStore *Store) getWorkflowRun(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*WorkflowRunTrace, error) {
	query := `SELECT id,workflow_id,workflow_version,workflow_hash,input_hash,scenario,
		status,input_tokens,output_tokens,reasoning_tokens,total_tokens,tool_call_count,
		cost_micros,retry_count,error_code,started_at,ended_at
		FROM workflow_runs WHERE id=?`
	args := []any{runID}
	if !admin {
		query += ` AND actor_user_id=?`
		args = append(args, userID)
	}
	query += ` LIMIT 1`
	var (
		run   WorkflowRunTrace
		ended sql.NullTime
	)
	err := evaluationStore.db.QueryRowContext(ctx, query, args...).Scan(
		&run.ID, &run.WorkflowID, &run.WorkflowVersion, &run.WorkflowHash,
		&run.InputHash, &run.Scenario, &run.Status, &run.InputTokens,
		&run.OutputTokens, &run.ReasoningTokens, &run.TotalTokens,
		&run.ToolCalls, &run.CostMicros, &run.Retries, &run.ErrorCode,
		&run.StartedAt, &ended,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow run %q: %w", runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow trace run %q: %w", runID, err)
	}
	run.EndedAt = nullableTime(ended)
	return &run, nil
}

func (evaluationStore *Store) listTraceNodes(
	ctx context.Context,
	runID string,
) ([]NodeRunTrace, bool, error) {
	rows, err := evaluationStore.db.QueryContext(ctx, `SELECT
		node_id,attempt,kind,agent_run_id,status,input_tokens,output_tokens,
		reasoning_tokens,total_tokens,tool_call_count,cost_micros,retry_count,
		error_code,started_at,ended_at
		FROM workflow_node_runs
		WHERE workflow_run_id=?
		ORDER BY node_id,attempt LIMIT ?`, runID, maxTraceNodes+1)
	if err != nil {
		return nil, false, fmt.Errorf("list workflow trace nodes %q: %w", runID, err)
	}
	defer rows.Close()
	nodes := make([]NodeRunTrace, 0, maxTraceNodes)
	truncated := false
	for rows.Next() {
		if len(nodes) == maxTraceNodes {
			truncated = true
			break
		}
		var (
			node  NodeRunTrace
			ended sql.NullTime
		)
		if err := rows.Scan(
			&node.NodeID, &node.Attempt, &node.Kind, &node.AgentRunID,
			&node.Status, &node.InputTokens, &node.OutputTokens,
			&node.ReasoningTokens, &node.TotalTokens, &node.ToolCalls,
			&node.CostMicros, &node.Retries, &node.ErrorCode,
			&node.StartedAt, &ended,
		); err != nil {
			return nil, false, fmt.Errorf("scan workflow trace node %q: %w", runID, err)
		}
		node.EndedAt = nullableTime(ended)
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate workflow trace nodes %q: %w", runID, err)
	}
	return nodes, truncated, nil
}

func (evaluationStore *Store) listTraceAgents(
	ctx context.Context,
	runID string,
) ([]AgentRunTrace, bool, error) {
	rows, err := evaluationStore.db.QueryContext(ctx, `SELECT
		id,agent_id,definition_version,definition_hash,tool_snapshot_id,
		workflow_node_id,status,evidence_status,input_tokens,output_tokens,
		reasoning_tokens,total_tokens,tool_call_count,tool_failure_count,
		partial_result_count,omitted_evidence_count,llm_call_count,error_code,
		started_at,ended_at
		FROM agent_runs
		WHERE workflow_run_id=?
		ORDER BY started_at,id LIMIT ?`, runID, maxTraceAgents+1)
	if err != nil {
		return nil, false, fmt.Errorf("list workflow trace agents %q: %w", runID, err)
	}
	defer rows.Close()
	agents := make([]AgentRunTrace, 0, maxTraceAgents)
	truncated := false
	for rows.Next() {
		if len(agents) == maxTraceAgents {
			truncated = true
			break
		}
		var (
			run   AgentRunTrace
			ended sql.NullTime
		)
		if err := rows.Scan(
			&run.ID, &run.AgentID, &run.DefinitionVersion, &run.DefinitionHash,
			&run.ToolSnapshotID, &run.WorkflowNodeID, &run.Status,
			&run.EvidenceStatus, &run.InputTokens, &run.OutputTokens,
			&run.ReasoningTokens, &run.TotalTokens, &run.ToolCalls,
			&run.ToolFailures, &run.PartialResults, &run.OmittedEvidence,
			&run.LLMCalls, &run.ErrorCode, &run.StartedAt, &ended,
		); err != nil {
			return nil, false, fmt.Errorf("scan workflow trace agent %q: %w", runID, err)
		}
		run.EndedAt = nullableTime(ended)
		agents = append(agents, run)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate workflow trace agents %q: %w", runID, err)
	}
	return agents, truncated, nil
}

func (evaluationStore *Store) listTraceEvents(
	ctx context.Context,
	runID string,
) ([]WorkflowEvent, bool, error) {
	rows, err := evaluationStore.db.QueryContext(ctx, `SELECT
		seq,kind,node_id,summary,created_at
		FROM workflow_events
		WHERE workflow_run_id=?
		ORDER BY seq LIMIT ?`, runID, maxTraceEvents+1)
	if err != nil {
		return nil, false, fmt.Errorf("list workflow trace events %q: %w", runID, err)
	}
	defer rows.Close()
	events := make([]WorkflowEvent, 0, maxTraceEvents)
	truncated := false
	for rows.Next() {
		if len(events) == maxTraceEvents {
			truncated = true
			break
		}
		var event WorkflowEvent
		if err := rows.Scan(
			&event.Seq, &event.Kind, &event.NodeID, &event.Summary, &event.CreatedAt,
		); err != nil {
			return nil, false, fmt.Errorf("scan workflow trace event %q: %w", runID, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate workflow trace events %q: %w", runID, err)
	}
	return events, truncated, nil
}

func (evaluationStore *Store) AgentVersionMetrics(
	ctx context.Context,
	agentID string,
	version int64,
	window Window,
) (AgentVersionMetrics, error) {
	metrics := AgentVersionMetrics{Version: version}
	var definitionJSON []byte
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		definition_json,content_hash FROM agent_definitions
		WHERE id=? AND version=? LIMIT 1`, agentID, version).Scan(
		&definitionJSON, &metrics.DefinitionHash,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return metrics, fmt.Errorf("agent %q version %d: %w", agentID, version, ErrNotFound)
		}
		return metrics, fmt.Errorf("get agent definition %q version %d: %w", agentID, version, err)
	}
	var definition agentapi.Definition
	if err := json.Unmarshal(definitionJSON, &definition); err != nil {
		return metrics, fmt.Errorf("decode agent definition %q version %d: %w", agentID, version, err)
	}
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='done' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN evidence_status<>'not_required' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN evidence_status='complete' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(tool_call_count),0),COALESCE(SUM(tool_failure_count),0)
		FROM agent_runs
		WHERE agent_id=? AND definition_version=? AND started_at>=? AND started_at<?`,
		agentID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.RunCount, &metrics.SuccessCount,
		&metrics.EvidenceRequiredRunCount, &metrics.EvidenceCompleteCount,
		&metrics.InputTokens, &metrics.OutputTokens, &metrics.ReasoningTokens,
		&metrics.TotalTokens, &metrics.ToolCalls, &metrics.ToolFailures,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate agent metrics %q version %d: %w", agentID, version, err)
	}
	metrics.P95LatencyMillis, err = evaluationStore.agentP95(
		ctx, agentID, version, window,
	)
	if err != nil {
		return metrics, err
	}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(
		CEIL(CAST(agent_llm_calls.input_tokens AS DECIMAL(65,0))*?/1000000)+
		CEIL(CAST(agent_llm_calls.output_tokens AS DECIMAL(65,0))*?/1000000)
	),0)
		FROM agent_llm_calls
		JOIN agent_runs ON agent_runs.id=agent_llm_calls.run_id
		WHERE agent_runs.agent_id=? AND agent_runs.definition_version=?
			AND agent_runs.started_at>=? AND agent_runs.started_at<?`,
		definition.Model.InputPriceMicrosPerMillionTokens,
		definition.Model.OutputPriceMicrosPerMillionTokens,
		agentID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&metrics.CostMicros); err != nil {
		return metrics, fmt.Errorf("aggregate agent cost %q version %d: %w", agentID, version, err)
	}
	return metrics, nil
}

func (evaluationStore *Store) agentP95(
	ctx context.Context,
	agentID string,
	version int64,
	window Window,
) (int64, error) {
	var latency sql.NullInt64
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT duration_ms FROM (
		SELECT duration_ms,ROW_NUMBER() OVER (ORDER BY duration_ms) AS row_num,
			COUNT(*) OVER () AS total_count
		FROM (
			SELECT TIMESTAMPDIFF(MICROSECOND,started_at,ended_at) DIV 1000 AS duration_ms
			FROM agent_runs
			WHERE agent_id=? AND definition_version=? AND started_at>=? AND started_at<?
				AND ended_at IS NOT NULL
		) durations
	) ranked
	WHERE row_num=CEIL(total_count*0.95) LIMIT 1`,
		agentID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&latency)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("calculate agent P95 %q version %d: %w", agentID, version, err)
	}
	return latency.Int64, nil
}

func (evaluationStore *Store) WorkflowVersionMetrics(
	ctx context.Context,
	workflowID string,
	version int64,
	window Window,
) (WorkflowVersionMetrics, error) {
	metrics := WorkflowVersionMetrics{Version: version}
	var definitionJSON []byte
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		definition_json,content_hash FROM workflow_definitions
		WHERE id=? AND version=? LIMIT 1`, workflowID, version).Scan(
		&definitionJSON, &metrics.DefinitionHash,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return metrics, fmt.Errorf("workflow %q version %d: %w", workflowID, version, ErrNotFound)
		}
		return metrics, fmt.Errorf("get workflow definition %q version %d: %w", workflowID, version, err)
	}
	var definition agentworkflow.WorkflowDefinition
	if err := json.Unmarshal(definitionJSON, &definition); err != nil {
		return metrics, fmt.Errorf("decode workflow definition %q version %d: %w", workflowID, version, err)
	}
	for _, node := range definition.Nodes {
		if node.Kind == agentworkflow.NodeAgent {
			metrics.AgentNodeCount++
		}
	}
	metrics.Mode = "single_agent"
	if metrics.AgentNodeCount > 1 {
		metrics.Mode = "multi_agent"
	}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(agent_node_count),0)
		FROM (
			SELECT agent_runs.workflow_run_id,
				COUNT(DISTINCT agent_runs.workflow_node_id) AS agent_node_count
			FROM agent_runs
			JOIN workflow_runs ON workflow_runs.id=agent_runs.workflow_run_id
			WHERE workflow_runs.workflow_id=? AND workflow_runs.workflow_version=?
				AND workflow_runs.started_at>=? AND workflow_runs.started_at<?
				AND agent_runs.workflow_node_id<>''
			GROUP BY agent_runs.workflow_run_id
		) observed`,
		workflowID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&metrics.ObservedAgentNodeCount); err != nil {
		return metrics, fmt.Errorf(
			"aggregate observed workflow agents %q version %d: %w",
			workflowID, version, err,
		)
	}
	if metrics.ObservedAgentNodeCount > 0 {
		metrics.Mode = "single_agent"
		if metrics.ObservedAgentNodeCount > 1 {
			metrics.Mode = "multi_agent"
		}
	}
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='succeeded' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN retry_count>0 OR EXISTS (
			SELECT 1 FROM workflow_events events
			WHERE events.workflow_run_id=workflow_runs.id
				AND events.kind='workflow_resumed'
		) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(tool_call_count),0),COALESCE(SUM(cost_micros),0),
		COALESCE(SUM(retry_count),0)
		FROM workflow_runs
		WHERE workflow_id=? AND workflow_version=? AND started_at>=? AND started_at<?`,
		workflowID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.RunCount, &metrics.SuccessCount, &metrics.RecoveredRunCount,
		&metrics.InputTokens, &metrics.OutputTokens, &metrics.ReasoningTokens,
		&metrics.TotalTokens, &metrics.ToolCalls, &metrics.CostMicros,
		&metrics.Retries,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate workflow metrics %q version %d: %w", workflowID, version, err)
	}
	var (
		linkedSuccess  int64
		evidenceRuns   int64
		evidencePassed int64
	)
	err = evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN agent_runs.status='done' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN agent_runs.evidence_status<>'not_required' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN agent_runs.evidence_status='complete' THEN 1 ELSE 0 END),0)
		FROM agent_runs
		JOIN workflow_runs ON workflow_runs.id=agent_runs.workflow_run_id
		WHERE workflow_runs.workflow_id=? AND workflow_runs.workflow_version=?
			AND workflow_runs.started_at>=? AND workflow_runs.started_at<?`,
		workflowID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.LinkedAgentRuns, &linkedSuccess, &evidenceRuns, &evidencePassed,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate workflow agent quality %q version %d: %w", workflowID, version, err)
	}
	metrics.LinkedAgentSuccessRate = ratio(linkedSuccess, metrics.LinkedAgentRuns)
	metrics.EvidenceCompletenessRate = ratio(evidencePassed, evidenceRuns)
	metrics.P95LatencyMillis, err = evaluationStore.workflowP95(
		ctx, workflowID, version, window,
	)
	if err != nil {
		return metrics, err
	}
	return metrics, nil
}

func (evaluationStore *Store) workflowP95(
	ctx context.Context,
	workflowID string,
	version int64,
	window Window,
) (int64, error) {
	var latency sql.NullInt64
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT duration_ms FROM (
		SELECT duration_ms,ROW_NUMBER() OVER (ORDER BY duration_ms) AS row_num,
			COUNT(*) OVER () AS total_count
		FROM (
			SELECT TIMESTAMPDIFF(MICROSECOND,started_at,ended_at) DIV 1000 AS duration_ms
			FROM workflow_runs
			WHERE workflow_id=? AND workflow_version=? AND started_at>=? AND started_at<?
				AND ended_at IS NOT NULL
		) durations
	) ranked
	WHERE row_num=CEIL(total_count*0.95) LIMIT 1`,
		workflowID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&latency)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("calculate workflow P95 %q version %d: %w", workflowID, version, err)
	}
	return latency.Int64, nil
}

func (evaluationStore *Store) ReviewPolicyVersionMetrics(
	ctx context.Context,
	policyID string,
	version int64,
	window Window,
) (ReviewPolicyVersionMetrics, error) {
	metrics := ReviewPolicyVersionMetrics{Version: version}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT content_hash
		FROM review_policies WHERE id=? AND version=? LIMIT 1`,
		policyID, version,
	).Scan(&metrics.PolicyHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return metrics, fmt.Errorf("review policy %q version %d: %w", policyID, version, ErrNotFound)
		}
		return metrics, fmt.Errorf("get review policy %q version %d: %w", policyID, version, err)
	}
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN review_rounds.status='completed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN review_gate_results.decision='pass' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(workflow_runs.cost_micros),0),
		COALESCE(SUM(CASE WHEN workflow_runs.id IS NOT NULL THEN 1 ELSE 0 END),0)
		FROM review_rounds
		LEFT JOIN review_gate_results ON review_gate_results.round_id=review_rounds.id
		LEFT JOIN workflow_runs ON workflow_runs.id=review_rounds.workflow_run_id
		WHERE review_rounds.policy_id=? AND review_rounds.policy_version=?
			AND review_rounds.created_at>=? AND review_rounds.created_at<?`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.RoundCount, &metrics.CompletedRoundCount,
		&metrics.PassedRoundCount, &metrics.CostMicros,
		&metrics.CostTrackedRoundCount,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate review rounds %q version %d: %w", policyID, version, err)
	}
	err = evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(DISTINCT review_reports.id),
		COUNT(review_findings.id),
		COUNT(DISTINCT CONCAT(review_findings.round_id,CHAR(0),review_findings.fingerprint))
		FROM review_rounds
		LEFT JOIN review_reports ON review_reports.round_id=review_rounds.id
		LEFT JOIN review_findings ON review_findings.report_id=review_reports.id
		WHERE review_rounds.policy_id=? AND review_rounds.policy_version=?
			AND review_rounds.created_at>=? AND review_rounds.created_at<?`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.ReportCount, &metrics.FindingCount, &metrics.UniqueFindingCount,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate review findings %q version %d: %w", policyID, version, err)
	}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT review_rounds.id)
		FROM review_rounds
		JOIN review_adjudications ON review_adjudications.round_id=review_rounds.id
		WHERE review_rounds.policy_id=? AND review_rounds.policy_version=?
			AND review_rounds.created_at>=? AND review_rounds.created_at<?`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&metrics.ConflictRoundCount); err != nil {
		return metrics, fmt.Errorf("aggregate review conflicts %q version %d: %w", policyID, version, err)
	}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN resolution IN ('fixed','waived','superseded') THEN 1 ELSE 0 END),0)
		FROM (
			SELECT finding_resolutions.resolution,
				ROW_NUMBER() OVER (
					PARTITION BY finding_resolutions.finding_id,finding_resolutions.subject_hash
					ORDER BY finding_resolutions.created_at DESC,finding_resolutions.id DESC
				) AS resolution_rank
			FROM finding_resolutions
			JOIN review_findings ON review_findings.id=finding_resolutions.finding_id
			JOIN review_rounds ON review_rounds.id=review_findings.round_id
			WHERE review_rounds.policy_id=? AND review_rounds.policy_version=?
				AND review_rounds.created_at>=? AND review_rounds.created_at<?
		) latest
		WHERE resolution_rank=1`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.LabeledResolutionCount, &metrics.AdoptedFindingCount,
	); err != nil {
		return metrics, fmt.Errorf("aggregate review adoption %q version %d: %w", policyID, version, err)
	}
	if err := evaluationStore.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN label='true_positive' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN label='false_positive' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN label='false_negative' THEN 1 ELSE 0 END),0)
		FROM review_evaluation_labels
		JOIN review_rounds ON review_rounds.id=review_evaluation_labels.round_id
		WHERE review_rounds.policy_id=? AND review_rounds.policy_version=?
			AND review_rounds.created_at>=? AND review_rounds.created_at<?`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(
		&metrics.TruePositiveCount, &metrics.FalsePositiveCount,
		&metrics.FalseNegativeCount,
	); err != nil {
		return metrics, fmt.Errorf("aggregate review labels %q version %d: %w", policyID, version, err)
	}
	metrics.P95LatencyMillis, err = evaluationStore.reviewP95(
		ctx, policyID, version, window,
	)
	if err != nil {
		return metrics, err
	}
	return metrics, nil
}

func (evaluationStore *Store) reviewP95(
	ctx context.Context,
	policyID string,
	version int64,
	window Window,
) (int64, error) {
	var latency sql.NullInt64
	err := evaluationStore.db.QueryRowContext(ctx, `SELECT duration_ms FROM (
		SELECT duration_ms,ROW_NUMBER() OVER (ORDER BY duration_ms) AS row_num,
			COUNT(*) OVER () AS total_count
		FROM (
			SELECT TIMESTAMPDIFF(MICROSECOND,created_at,completed_at) DIV 1000 AS duration_ms
			FROM review_rounds
			WHERE policy_id=? AND policy_version=? AND created_at>=? AND created_at<?
				AND completed_at IS NOT NULL
		) durations
	) ranked
	WHERE row_num=CEIL(total_count*0.95) LIMIT 1`,
		policyID, version, databaseTime(window.From), databaseTime(window.To),
	).Scan(&latency)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("calculate review P95 %q version %d: %w", policyID, version, err)
	}
	return latency.Int64, nil
}

func (evaluationStore *Store) CreateReviewLabels(
	ctx context.Context,
	roundID string,
	inputs []ReviewLabelInput,
	actorUserID int64,
	createdAt time.Time,
) ([]ReviewLabel, error) {
	tx, err := evaluationStore.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin review evaluation labels: %w", err)
	}
	defer tx.Rollback()
	var (
		policyID      string
		policyVersion int64
		subjectHash   string
	)
	if err := tx.QueryRowContext(ctx, `SELECT policy_id,policy_version,subject_hash
		FROM review_rounds WHERE id=? LIMIT 1`, roundID).Scan(
		&policyID, &policyVersion, &subjectHash,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("review round %q: %w", roundID, ErrNotFound)
		}
		return nil, fmt.Errorf("get review round %q for evaluation: %w", roundID, err)
	}
	findings, err := loadLabelFindings(ctx, tx, roundID, inputs)
	if err != nil {
		return nil, err
	}
	labels := make([]ReviewLabel, 0, len(inputs))
	byTarget := make(map[string]ReviewLabelKind, len(inputs))
	for _, input := range inputs {
		label := ReviewLabel{
			RoundID: roundID, PolicyID: policyID, PolicyVersion: policyVersion,
			SubjectHash: subjectHash, Label: input.Label,
			CreatedBy: actorUserID, CreatedAt: createdAt,
		}
		if input.Label == LabelFalseNegative {
			label.TargetHash = input.TargetHash
			label.Category = input.Category
		} else {
			finding, ok := findings[input.FindingID]
			if !ok {
				return nil, fmt.Errorf(
					"review finding %q in round %q: %w",
					input.FindingID, roundID, ErrNotFound,
				)
			}
			label.FindingID = input.FindingID
			label.TargetHash = finding.contentHash
			label.Category = finding.category
		}
		if existing, duplicate := byTarget[label.TargetHash]; duplicate {
			if existing != label.Label {
				return nil, fmt.Errorf(
					"review target %q has conflicting labels: %w",
					label.TargetHash, ErrConflict,
				)
			}
			continue
		}
		byTarget[label.TargetHash] = label.Label
		label.ID = reviewLabelID(label)
		labels = append(labels, label)
	}
	if err := insertReviewLabels(ctx, tx, labels); err != nil {
		return nil, err
	}
	stored, err := listLabelsByTargets(ctx, tx, roundID, labels)
	if err != nil {
		return nil, err
	}
	for _, label := range stored {
		if byTarget[label.TargetHash] != label.Label {
			return nil, fmt.Errorf(
				"review target %q already has label %q: %w",
				label.TargetHash, label.Label, ErrConflict,
			)
		}
	}
	if len(stored) != len(labels) {
		return nil, fmt.Errorf("review evaluation labels were not fully persisted: %w", ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit review evaluation labels: %w", err)
	}
	return stored, nil
}

func (evaluationStore *Store) ListReviewLabels(
	ctx context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]ReviewLabel, error) {
	rows, err := evaluationStore.db.QueryContext(ctx, `SELECT
		seq,id,round_id,policy_id,policy_version,subject_hash,finding_id,
		target_hash,category,label,created_by,created_at
		FROM review_evaluation_labels
		WHERE round_id=? AND seq>?
		ORDER BY seq LIMIT ?`, roundID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list review evaluation labels %q: %w", roundID, err)
	}
	defer rows.Close()
	labels := make([]ReviewLabel, 0, limit)
	for rows.Next() {
		label, err := scanReviewLabel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan review evaluation label %q: %w", roundID, err)
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review evaluation labels %q: %w", roundID, err)
	}
	return labels, nil
}

type labelFinding struct {
	category    string
	contentHash string
}

func loadLabelFindings(
	ctx context.Context,
	tx *sql.Tx,
	roundID string,
	inputs []ReviewLabelInput,
) (map[string]labelFinding, error) {
	ids := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if input.Label == LabelFalseNegative {
			continue
		}
		if _, duplicate := seen[input.FindingID]; duplicate {
			continue
		}
		seen[input.FindingID] = struct{}{}
		ids = append(ids, input.FindingID)
	}
	findings := make(map[string]labelFinding, len(ids))
	if len(ids) == 0 {
		return findings, nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, roundID)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT id,category,content_hash
		FROM review_findings
		WHERE round_id=? AND id IN (`+placeholders(len(ids))+`)
		ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("load review findings for evaluation: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			finding labelFinding
		)
		if err := rows.Scan(&id, &finding.category, &finding.contentHash); err != nil {
			return nil, fmt.Errorf("scan review finding for evaluation: %w", err)
		}
		findings[id] = finding
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review findings for evaluation: %w", err)
	}
	return findings, nil
}

func insertReviewLabels(
	ctx context.Context,
	tx *sql.Tx,
	labels []ReviewLabel,
) error {
	if len(labels) == 0 {
		return nil
	}
	const columns = 11
	args := make([]any, 0, len(labels)*columns)
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		values = append(values, "("+placeholders(columns)+")")
		args = append(
			args,
			label.ID, label.RoundID, label.PolicyID, label.PolicyVersion,
			label.SubjectHash, label.FindingID, label.TargetHash, label.Category,
			label.Label, label.CreatedBy, databaseTime(label.CreatedAt),
		)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO review_evaluation_labels(
		id,round_id,policy_id,policy_version,subject_hash,finding_id,target_hash,
		category,label,created_by,created_at)
		VALUES `+strings.Join(values, ",")+`
		ON DUPLICATE KEY UPDATE id=id`, args...)
	if err != nil {
		return fmt.Errorf("persist review evaluation labels: %w", err)
	}
	return nil
}

func listLabelsByTargets(
	ctx context.Context,
	tx *sql.Tx,
	roundID string,
	labels []ReviewLabel,
) ([]ReviewLabel, error) {
	if len(labels) == 0 {
		return []ReviewLabel{}, nil
	}
	args := make([]any, 0, len(labels)+2)
	args = append(args, roundID)
	for _, label := range labels {
		args = append(args, label.TargetHash)
	}
	args = append(args, len(labels))
	rows, err := tx.QueryContext(ctx, `SELECT
		seq,id,round_id,policy_id,policy_version,subject_hash,finding_id,
		target_hash,category,label,created_by,created_at
		FROM review_evaluation_labels
		WHERE round_id=? AND target_hash IN (`+placeholders(len(labels))+`)
		ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read persisted review evaluation labels: %w", err)
	}
	defer rows.Close()
	stored := make([]ReviewLabel, 0, len(labels))
	for rows.Next() {
		label, err := scanReviewLabel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan persisted review evaluation label: %w", err)
		}
		stored = append(stored, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate persisted review evaluation labels: %w", err)
	}
	return stored, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanReviewLabel(row rowScanner) (ReviewLabel, error) {
	var label ReviewLabel
	err := row.Scan(
		&label.Seq, &label.ID, &label.RoundID, &label.PolicyID,
		&label.PolicyVersion, &label.SubjectHash, &label.FindingID,
		&label.TargetHash, &label.Category, &label.Label,
		&label.CreatedBy, &label.CreatedAt,
	)
	return label, err
}

func reviewLabelID(label ReviewLabel) string {
	sum := sha256.Sum256([]byte(
		label.RoundID + "\x00" + label.TargetHash + "\x00" + string(label.Label),
	))
	return "review_eval_" + hex.EncodeToString(sum[:12])
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

func databaseTime(value time.Time) any {
	return store.DatabaseTime(value.UTC().Format(time.RFC3339Nano))
}

func nullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func ratio(numerator, denominator int64) float64 {
	if denominator <= 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
