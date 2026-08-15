package run

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// RecordLLMCall stores one provider call and updates its Run aggregate atomically.
func (rs *Store) RecordLLMCall(ctx context.Context, call llm.CallUsage) error {
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var callCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT llm_call_count FROM agent_runs WHERE id=? FOR UPDATE`, call.RunID,
	).Scan(&callCount); err != nil {
		return err
	}
	callSeq := callCount + 1
	usage := call.Usage
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_llm_calls(
			run_id,call_seq,phase,provider,model,input_tokens,cached_input_tokens,
			output_tokens,reasoning_tokens,total_tokens,max_output_tokens,duration_ms,status)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		call.RunID, callSeq, call.Phase, call.Provider, call.Model,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		usage.ReasoningTokens, usage.TotalTokens, call.MaxOutputTokens,
		call.Duration.Milliseconds(), call.Status,
	); err != nil {
		return err
	}
	reservedTokens := 0
	if call.MaxOutputTokens > 0 {
		reservedTokens = usage.InputTokens + call.MaxOutputTokens
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_runs SET
			input_tokens=input_tokens+?,cached_input_tokens=cached_input_tokens+?,
			output_tokens=output_tokens+?,reasoning_tokens=reasoning_tokens+?,
			total_tokens=total_tokens+?,llm_call_count=?,
			peak_input_tokens=GREATEST(peak_input_tokens,?),
			peak_reserved_tokens=GREATEST(peak_reserved_tokens,?)
		 WHERE id=?`,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		usage.ReasoningTokens, usage.TotalTokens, callSeq,
		usage.InputTokens, reservedTokens, call.RunID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// UsageSummary returns bounded token aggregates for one session and round.
func (rs *Store) UsageSummary(ctx context.Context, userID int64, sessionID, runID string) (UsageSummary, error) {
	var summary UsageSummary
	if sessionID == "" {
		return summary, nil
	}
	if err := rs.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens),0) FROM agent_runs WHERE user_id=? AND session_id=?`,
		userID, sessionID,
	).Scan(&summary.SessionTotalTokens); err != nil {
		return summary, err
	}

	query := `SELECT id,input_tokens,cached_input_tokens,total_tokens,peak_input_tokens,peak_reserved_tokens
		FROM agent_runs WHERE user_id=? AND session_id=?`
	args := []any{userID, sessionID}
	if runID != "" {
		query += " AND id=?"
		args = append(args, runID)
	} else {
		query += " ORDER BY started_at DESC,id DESC LIMIT 1"
	}
	err := rs.db.QueryRowContext(ctx, query, args...).Scan(
		&summary.RunID,
		&summary.RoundInputTokens,
		&summary.RoundCachedInputTokens,
		&summary.RoundTotalTokens,
		&summary.RoundPeakInputTokens,
		&summary.RoundPeakReservedTokens,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return summary, nil
	}
	return summary, err
}

// PeakInputTokens reads only the metric needed by session compaction.
func (rs *Store) PeakInputTokens(id string) (int, error) {
	var tokens int
	err := rs.db.QueryRow(`SELECT peak_input_tokens FROM agent_runs WHERE id=?`, id).Scan(&tokens)
	return tokens, err
}

// LatestUsage returns the latest round's observed input and reserved peaks.
func (rs *Store) LatestUsage(userID int64, sessionID string) (ContextUsageSnapshot, error) {
	if sessionID == "" {
		return ContextUsageSnapshot{}, nil
	}
	var usage ContextUsageSnapshot
	err := rs.db.QueryRow(
		`SELECT peak_input_tokens,peak_reserved_tokens
		 FROM agent_runs WHERE user_id=? AND session_id=?
		 ORDER BY started_at DESC,id DESC LIMIT 1`,
		userID, sessionID,
	).Scan(&usage.PeakInputTokens, &usage.PeakReservedTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextUsageSnapshot{}, nil
	}
	return usage, err
}

func (rs *Store) listLLMCalls(runID string, limit int) ([]LLMCallRow, error) {
	rows, err := rs.db.Query(
		`SELECT id,run_id,call_seq,phase,provider,model,input_tokens,cached_input_tokens,
			output_tokens,reasoning_tokens,total_tokens,max_output_tokens,duration_ms,status,created_at
		 FROM agent_llm_calls WHERE run_id=? ORDER BY call_seq LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := make([]LLMCallRow, 0, min(limit, 16))
	for rows.Next() {
		var call LLMCallRow
		var createdAt sql.NullTime
		if err := rows.Scan(
			&call.ID, &call.RunID, &call.CallSeq, &call.Phase, &call.Provider, &call.Model,
			&call.InputTokens, &call.CachedInputTokens, &call.OutputTokens,
			&call.ReasoningTokens, &call.TotalTokens, &call.MaxOutputTokens,
			&call.DurationMs, &call.Status, &createdAt,
		); err != nil {
			return nil, err
		}
		call.CreatedAt = store.FormatDatabaseTime(createdAt)
		calls = append(calls, call)
	}
	return calls, rows.Err()
}
