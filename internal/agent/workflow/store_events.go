package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func (workflowStore *Store) AppendEvent(ctx context.Context, event Event) error {
	_, err := workflowStore.db.ExecContext(ctx, `INSERT INTO runtime_events(
		stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
		VALUES('workflow',?,?,?,?,?,?,?)`,
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
		stream_id,seq,kind,node_id,summary,detail_json,created_at
		FROM runtime_events
		WHERE stream_kind='workflow' AND stream_id=? AND seq>?
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
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
		workflowRunID,
	).Scan(&nextSeq); err != nil {
		return fmt.Errorf("allocate workflow event sequence for %q: %w", workflowRunID, err)
	}
	for index, event := range events {
		events[index].Seq = nextSeq + int64(index)
		event = events[index]
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`,
			workflowRunID, event.Seq, event.Kind, event.NodeID, event.Summary,
			nullableJSON(event.Detail),
			store.DatabaseTime(event.CreatedAt.UTC().Format(time.RFC3339Nano)),
		); err != nil {
			return fmt.Errorf("append workflow event %q/%d: %w", workflowRunID, event.Seq, err)
		}
	}
	return nil
}

func (workflowStore *Store) publish(events []Event) {
	for _, event := range events {
		workflowStore.hub.Publish(event)
	}
}
