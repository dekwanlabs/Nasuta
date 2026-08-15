package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// ListParentEvents returns one ownership-checked event page.
func (rs *Store) ListParentEvents(
	ctx context.Context,
	runID string,
	userID int64,
	afterSeq int64,
	limit int,
) ([]QAParentEvent, error) {
	if afterSeq < 0 {
		return nil, fmt.Errorf("list QA parent events: after sequence must be non-negative")
	}
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("list QA parent events: limit must be between 1 and 200")
	}
	var ownedRunID string
	if err := rs.db.QueryRowContext(
		ctx,
		`SELECT id FROM agent_runs
		 WHERE id=? AND run_kind=? AND user_id=? LIMIT 1`,
		runID,
		KindQAParent,
		userID,
	).Scan(&ownedRunID); err != nil {
		return nil, err
	}
	rows, err := rs.db.QueryContext(
		ctx,
		`SELECT stream_id,seq,kind,summary,detail_json,created_at
		 FROM runtime_events
		 WHERE stream_kind=? AND stream_id=? AND seq>?
		 ORDER BY seq LIMIT ?`,
		qaParentStreamKind,
		ownedRunID,
		afterSeq,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list QA parent events %q: %w", runID, err)
	}
	defer rows.Close()
	events := make([]QAParentEvent, 0, limit)
	for rows.Next() {
		var (
			event     QAParentEvent
			detail    []byte
			createdAt sql.NullTime
		)
		if err := rows.Scan(
			&event.RunID,
			&event.Seq,
			&event.Kind,
			&event.Summary,
			&detail,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan QA parent event %q: %w", runID, err)
		}
		event.Detail, err = decodeQAParentTerminal(event.RunID, detail)
		if err != nil {
			return nil, err
		}
		event.CreatedAt = store.FormatDatabaseTime(createdAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate QA parent events %q: %w", runID, err)
	}
	return events, nil
}

func loadQAParentTerminal(db *sql.DB, runID string) (Terminal, error) {
	var detail []byte
	err := db.QueryRow(
		`SELECT detail_json FROM runtime_events
		 WHERE stream_kind=? AND stream_id=? AND seq=? AND kind=? LIMIT 1`,
		qaParentStreamKind,
		runID,
		qaParentTerminalEventSeq,
		qaParentTerminalEventKind,
	).Scan(&detail)
	if err != nil {
		return Terminal{}, err
	}
	return decodeQAParentTerminal(runID, detail)
}

func decodeQAParentTerminal(runID string, detail []byte) (Terminal, error) {
	var terminal Terminal
	if err := json.Unmarshal(detail, &terminal); err != nil {
		return Terminal{}, fmt.Errorf("decode QA parent %q terminal event: %w", runID, err)
	}
	if terminal.RunID != runID {
		return Terminal{}, fmt.Errorf(
			"QA parent %q terminal event belongs to %q",
			runID,
			terminal.RunID,
		)
	}
	if !terminal.Status.Terminal() {
		return Terminal{}, fmt.Errorf(
			"QA parent %q terminal event has non-terminal status %q",
			runID,
			terminal.Status,
		)
	}
	return terminal, nil
}
