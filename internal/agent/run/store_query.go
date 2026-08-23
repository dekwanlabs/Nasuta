package run

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func (rs *Store) List(userID int64, sessionID string, status Status, limit int) ([]Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	page, err := rs.ListPage(userID, sessionID, status, 1, limit)
	if err != nil {
		return nil, err
	}
	return page.List, nil
}

func (rs *Store) ListPage(userID int64, sessionID string, status Status, page, pageSize int) (*domain.Page[Record], error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}

	q := `SELECT id,run_kind,user_id,session_id,agent_id,definition_version,definition_hash,selection_json,tool_snapshot_id,
		input_schema_version,output_schema_version,parent_run_id,
		capability_id,capability_version,capability_content_hash,delegation_id,delegation_depth,
		run_limits_json,capability_registry_revision,workflow_run_id,workflow_node_id,
		question,status,error_code,mode,max_steps,step_count,token_used,
		input_tokens,cached_input_tokens,output_tokens,reasoning_tokens,total_tokens,cost_micros,llm_call_count,
		peak_input_tokens,peak_reserved_tokens,evidence_status,forced_conclusion,evidence_result_count,
		tool_call_count,tool_failure_count,partial_result_count,omitted_evidence_count,started_at,ended_at FROM agent_runs`
	countQ := `SELECT COUNT(*) FROM agent_runs`
	var where []string
	var args []any
	if userID != 0 {
		where = append(where, "user_id=?")
		args = append(args, userID)
	}
	if sessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, sessionID)
	}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if len(where) > 0 {
		cond := " WHERE " + strings.Join(where, " AND ")
		q += cond
		countQ += cond
	}

	var total int
	if err := rs.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, err
	}

	q += " ORDER BY started_at DESC LIMIT ? OFFSET ?"
	offset := (page - 1) * pageSize
	args = append(args, pageSize, offset)

	rows, err := rs.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Record, 0, pageSize)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &domain.Page[Record]{Total: total, Page: page, PageSize: pageSize, List: out}, nil
}

func (rs *Store) Get(id string) (*Detail, error) {
	return rs.get(id, nil)
}

// GetControlForUser enforces ownership without loading execution history.
func (rs *Store) GetControlForUser(id string, userID int64) (ControlRecord, error) {
	var record ControlRecord
	err := rs.db.QueryRow(
		`SELECT id,run_kind,status,workflow_run_id,user_id
		 FROM agent_runs WHERE id=? AND user_id=? LIMIT 1`,
		id,
		userID,
	).Scan(
		&record.ID,
		&record.RunKind,
		&record.Status,
		&record.WorkflowRunID,
		&record.UserID,
	)
	return record, err
}

// GetQAParent loads one parent without loading child steps or model calls.
func (rs *Store) GetQAParent(id string) (QAParentRecord, error) {
	return rs.getQAParent(id, nil)
}

// GetParentForUser enforces ownership at the parent read boundary.
func (rs *Store) GetParentForUser(id string, userID int64) (QAParentRecord, error) {
	return rs.getQAParent(id, &userID)
}

func (rs *Store) getQAParent(id string, userID *int64) (QAParentRecord, error) {
	query := `SELECT id,workflow_run_id,user_id,session_id,question,status,started_at,ended_at
		FROM agent_runs WHERE id=? AND run_kind=?`
	args := []any{id, KindQAParent}
	if userID != nil {
		query += " AND user_id=?"
		args = append(args, *userID)
	}
	return scanQAParent(rs.db.QueryRow(query, args...))
}

// ListActiveQAParents returns one bounded page ordered by a stable keyset.
func (rs *Store) ListActiveQAParents(
	startedBefore time.Time,
	cursor QAParentCursor,
	limit int,
) ([]QAParentRecord, error) {
	if startedBefore.IsZero() {
		return nil, fmt.Errorf("list active QA parents: startup cutoff is required")
	}
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("list active QA parents: limit must be between 1 and 200")
	}
	query := `SELECT id,workflow_run_id,user_id,session_id,question,status,started_at,ended_at
		FROM agent_runs
		WHERE run_kind=? AND status IN (?,?) AND started_at<?`
	args := []any{
		KindQAParent,
		StatusRunning,
		StatusPaused,
		store.DatabaseTime(startedBefore.UTC().Format(time.RFC3339Nano)),
	}
	if cursor.StartedAt != "" || cursor.ID != "" {
		if cursor.StartedAt == "" || cursor.ID == "" {
			return nil, fmt.Errorf("list active QA parents: incomplete cursor")
		}
		query += ` AND (started_at>? OR (started_at=? AND id>?))`
		startedAt := store.DatabaseTime(cursor.StartedAt)
		args = append(args, startedAt, startedAt, cursor.ID)
	}
	query += ` ORDER BY started_at,id LIMIT ?`
	args = append(args, limit)
	rows, err := rs.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parents := make([]QAParentRecord, 0, limit)
	for rows.Next() {
		parent, err := scanQAParent(rows)
		if err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return parents, nil
}

// GetForUser loads one run only when it belongs to the requested user.
func (rs *Store) GetForUser(id string, userID int64) (*Detail, error) {
	return rs.get(id, &userID)
}

func (rs *Store) get(id string, userID *int64) (*Detail, error) {
	query := `SELECT id,run_kind,user_id,session_id,agent_id,definition_version,definition_hash,selection_json,tool_snapshot_id,
		input_schema_version,output_schema_version,parent_run_id,
		capability_id,capability_version,capability_content_hash,delegation_id,delegation_depth,
		run_limits_json,capability_registry_revision,workflow_run_id,workflow_node_id,
		question,status,error_code,mode,max_steps,step_count,token_used,
		input_tokens,cached_input_tokens,output_tokens,reasoning_tokens,total_tokens,cost_micros,llm_call_count,
		peak_input_tokens,peak_reserved_tokens,evidence_status,forced_conclusion,evidence_result_count,
		tool_call_count,tool_failure_count,partial_result_count,omitted_evidence_count,started_at,ended_at
		FROM agent_runs WHERE id=?`
	args := []any{id}
	if userID != nil {
		query += " AND user_id=?"
		args = append(args, *userID)
	}
	r, err := scanRecord(rs.db.QueryRow(query, args...))
	if err != nil {
		return nil, err
	}
	if r.RunKind == KindQAParent {
		detail := &Detail{Record: r}
		if r.Status.Terminal() {
			terminal, err := loadQAParentTerminal(rs.db, id)
			if err != nil {
				return nil, fmt.Errorf("load QA parent %q terminal result: %w", id, err)
			}
			detail.Terminal = &terminal
		}
		return detail, nil
	}

	rows, err := rs.db.Query(
		`SELECT s.id,s.run_id,s.step_no,s.kind,s.trace_id,s.artifact_id,s.tool_call_id,s.tool,s.args,
			s.content,s.prompt_content,s.authoritative_sha256,s.prompt_sha256,s.content_bytes,
			s.coverage_json,s.answer_contract_json,s.delegation_adoptions_json,s.failed,
			s.delivery_error,s.token_delta,
			s.reasoning_tokens,s.duration_ms,s.created_at,
			CASE WHEN s.artifact_id<>'' THEN CAST(SUBSTRING(s.artifact_content,1,4096) AS CHAR CHARACTER SET utf8mb4) ELSE NULL END
		 FROM agent_steps s
		 WHERE s.run_id=? ORDER BY s.step_no,s.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := make([]StepRow, 0)
	for rows.Next() {
		var st StepRow
		var traceID, artifactID, toolCallID, args, content, promptContent sql.NullString
		var authoritativeSHA, promptSHA, coverageRaw, contractRaw, adoptionsRaw sql.NullString
		var deliveryError, artifactPreview sql.NullString
		var createdAt sql.NullTime
		if err := rows.Scan(
			&st.ID, &st.RunID, &st.StepNo, &st.Kind, &traceID, &artifactID, &toolCallID, &st.Tool, &args,
			&content, &promptContent, &authoritativeSHA, &promptSHA, &st.SizeBytes, &coverageRaw,
			&contractRaw, &adoptionsRaw, &st.Failed, &deliveryError, &st.TokenDelta,
			&st.ReasoningTokens, &st.DurationMs, &createdAt, &artifactPreview,
		); err != nil {
			return nil, err
		}
		st.TraceID = traceID.String
		st.ArtifactID = artifactID.String
		st.ToolCallID = toolCallID.String
		st.Args = args.String
		st.Content = content.String
		st.PromptContent = promptContent.String
		st.AuthoritativeSHA256 = authoritativeSHA.String
		st.PromptSHA256 = promptSHA.String
		st.DeliveryError = deliveryError.String
		if coverageRaw.Valid && coverageRaw.String != "" {
			if err := json.Unmarshal([]byte(coverageRaw.String), &st.Coverage); err != nil {
				return nil, fmt.Errorf("decode step %d coverage: %w", st.StepNo, err)
			}
		}
		if contractRaw.Valid && contractRaw.String != "" {
			if err := json.Unmarshal([]byte(contractRaw.String), &st.AnswerContract); err != nil {
				return nil, fmt.Errorf("decode step %d answer contract: %w", st.StepNo, err)
			}
		}
		if adoptionsRaw.Valid && adoptionsRaw.String != "" {
			if err := json.Unmarshal(
				[]byte(adoptionsRaw.String),
				&st.DelegationAdoptions,
			); err != nil {
				return nil, fmt.Errorf(
					"decode step %d delegation adoptions: %w",
					st.StepNo,
					err,
				)
			}
		}
		previewSource := st.Content
		if previewSource == "" {
			previewSource = artifactPreview.String
		}
		st.ResultPreview = toolResultPreview(previewSource)
		st.CreatedAt = store.FormatDatabaseTime(createdAt)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	llmCalls, err := rs.listLLMCalls(id, 1000)
	if err != nil {
		return nil, err
	}
	return &Detail{Record: r, Steps: steps, LLMCalls: llmCalls}, nil
}

// EvidenceByIDs loads one bounded page of persisted evidence summaries.
func (rs *Store) EvidenceByIDs(userID int64, sessionID string, runIDs []string) (map[string]EvidenceMetrics, error) {
	evidence := make(map[string]EvidenceMetrics, len(runIDs))
	if len(runIDs) == 0 {
		return evidence, nil
	}
	args := make([]any, 0, len(runIDs)+2)
	args = append(args, userID, sessionID)
	for _, runID := range runIDs {
		args = append(args, runID)
	}
	query := `SELECT id,evidence_status,forced_conclusion,evidence_result_count,tool_call_count,
	                 tool_failure_count,partial_result_count,omitted_evidence_count
	          FROM agent_runs WHERE user_id=? AND session_id=? AND id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",") + `)`
	rows, err := rs.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var metrics EvidenceMetrics
		if err := rows.Scan(
			&runID, &metrics.Status, &metrics.ForcedConclusion, &metrics.ResultCount,
			&metrics.ToolCallCount, &metrics.ToolFailureCount, &metrics.PartialResultCount,
			&metrics.OmittedItemCount,
		); err != nil {
			return nil, err
		}
		evidence[runID] = metrics
	}
	return evidence, rows.Err()
}

func scanRecord(row rowScanner) (Record, error) {
	var record Record
	var selectionRaw, limitsRaw []byte
	var startedAt, endedAt sql.NullTime
	if err := row.Scan(&record.ID, &record.RunKind, &record.UserID, &record.SessionID,
		&record.AgentID, &record.DefinitionVersion, &record.DefinitionHash, &selectionRaw, &record.ToolSnapshotID,
		&record.InputSchemaVersion, &record.OutputSchemaVersion, &record.ParentRunID,
		&record.CapabilityID, &record.CapabilityVersion, &record.CapabilityHash,
		&record.DelegationID, &record.DelegationDepth, &limitsRaw, &record.CapabilityRevision,
		&record.WorkflowRunID, &record.WorkflowNodeID, &record.Question, &record.Status,
		&record.ErrorCode, &record.Mode, &record.MaxSteps, &record.StepCount, &record.TokenUsed,
		&record.InputTokens, &record.CachedInputTokens, &record.OutputTokens,
		&record.ReasoningTokens, &record.TotalTokens, &record.CostMicros, &record.LLMCallCount,
		&record.PeakInputTokens, &record.PeakReservedTokens, &record.EvidenceStatus,
		&record.ForcedConclusion, &record.EvidenceResultCount, &record.ToolCallCount,
		&record.ToolFailureCount, &record.PartialResultCount, &record.OmittedEvidenceCount,
		&startedAt, &endedAt); err != nil {
		return record, err
	}
	if len(selectionRaw) > 0 {
		if err := json.Unmarshal(selectionRaw, &record.Selection); err != nil {
			return record, fmt.Errorf("decode run %q selection: %w", record.ID, err)
		}
	}
	if len(limitsRaw) > 0 {
		if err := json.Unmarshal(limitsRaw, &record.RunLimits); err != nil {
			return record, fmt.Errorf("decode run %q limits: %w", record.ID, err)
		}
	}
	record.StartedAt = store.FormatDatabaseTime(startedAt)
	record.EndedAt = store.FormatDatabaseTime(endedAt)
	return record, nil
}

func scanQAParent(row rowScanner) (QAParentRecord, error) {
	var parent QAParentRecord
	var startedAt, endedAt sql.NullTime
	if err := row.Scan(
		&parent.ID,
		&parent.WorkflowRunID,
		&parent.UserID,
		&parent.SessionID,
		&parent.Question,
		&parent.Status,
		&startedAt,
		&endedAt,
	); err != nil {
		return parent, err
	}
	parent.StartedAt = store.FormatDatabaseTime(startedAt)
	parent.EndedAt = store.FormatDatabaseTime(endedAt)
	return parent, nil
}
