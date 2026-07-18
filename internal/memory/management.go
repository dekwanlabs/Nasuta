package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ListOptions defines one bounded keyset page.
type ListOptions struct {
	Limit  int
	Cursor string
	Kind   MemoryKind
	Status MemoryStatus
}

// MemoryPage is one cursor page of user-owned memories.
type MemoryPage struct {
	Records    []MemoryRecord `json:"records"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
}

type memoryCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

var ErrInvalidMemoryQuery = errors.New("memory: invalid query")

// List returns only the authenticated user's memories.
func (memory *MemoryStore) List(ctx context.Context, userID int64, options ListOptions) (MemoryPage, error) {
	if userID <= 0 {
		return MemoryPage{}, fmt.Errorf("memory: authenticated user is required")
	}
	if options.Limit <= 0 {
		options.Limit = 20
	}
	if options.Limit > 100 {
		return MemoryPage{}, fmt.Errorf("%w: limit must not exceed 100", ErrInvalidMemoryQuery)
	}
	if options.Kind != "" && !validKind(options.Kind) {
		return MemoryPage{}, fmt.Errorf("%w: invalid kind %q", ErrInvalidMemoryQuery, options.Kind)
	}
	if options.Status != "" && options.Status != StatusActive && options.Status != StatusSuperseded {
		return MemoryPage{}, fmt.Errorf("%w: invalid status %q", ErrInvalidMemoryQuery, options.Status)
	}
	cursor, err := decodeMemoryCursor(options.Cursor)
	if err != nil {
		return MemoryPage{}, err
	}

	conditions := []string{"user_id=?"}
	args := []any{userID}
	if options.Kind != "" {
		conditions = append(conditions, "kind=?")
		args = append(args, options.Kind)
	}
	if options.Status != "" {
		conditions = append(conditions, "status=?")
		args = append(args, options.Status)
	}
	if cursor != nil {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	args = append(args, options.Limit+1)

	query := `SELECT ` + memorySelectColumns + `
		FROM qa_memories
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY created_at DESC,id DESC
		LIMIT ?`
	rows, err := memory.db.QueryContext(ctx, query, args...)
	if err != nil {
		return MemoryPage{}, fmt.Errorf("memory: list: %w", err)
	}
	defer rows.Close()

	records := make([]MemoryRecord, 0, options.Limit+1)
	for rows.Next() {
		rec, err := scanMemory(rows.Scan)
		if err != nil {
			return MemoryPage{}, fmt.Errorf("memory: scan list: %w", err)
		}
		records = append(records, *rec)
	}
	if err := rows.Err(); err != nil {
		return MemoryPage{}, fmt.Errorf("memory: iterate list: %w", err)
	}

	page := MemoryPage{Records: records}
	if len(records) > options.Limit {
		page.HasMore = true
		page.Records = records[:options.Limit]
		last := page.Records[len(page.Records)-1]
		page.NextCursor, err = encodeMemoryCursor(memoryCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			return MemoryPage{}, err
		}
	}
	return page, nil
}

// Delete removes one user-owned memory and its vector point.
func (memory *MemoryStore) Delete(ctx context.Context, userID int64, id string) (bool, error) {
	if userID <= 0 || strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("memory: authenticated user and id are required")
	}
	result, err := memory.db.ExecContext(ctx, `DELETE FROM qa_memories WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return false, fmt.Errorf("memory: delete %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("memory: inspect delete %q: %w", id, err)
	}
	if affected == 0 {
		return false, nil
	}
	if err := memory.deleteVectors(ctx, []string{id}); err != nil {
		return true, fmt.Errorf("memory: metadata deleted but vector cleanup failed: %w", err)
	}
	return true, nil
}

// Clear removes the complete memory set owned by one user.
func (memory *MemoryStore) Clear(ctx context.Context, userID int64) (int, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("memory: authenticated user is required")
	}
	return memory.deleteWhere(ctx, `user_id=?`, userID)
}

// DeleteBySession removes only memories owned by the session owner.
func (memory *MemoryStore) DeleteBySession(ctx context.Context, userID int64, sessionID string) (int, error) {
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return 0, fmt.Errorf("memory: authenticated user and session id are required")
	}
	return memory.deleteWhere(ctx, `user_id=? AND source_session=?`, userID, sessionID)
}

func (memory *MemoryStore) deleteWhere(ctx context.Context, where string, args ...any) (int, error) {
	tx, err := memory.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("memory: begin delete: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM qa_memories WHERE `+where+` FOR UPDATE`, args...)
	if err != nil {
		return 0, fmt.Errorf("memory: list delete ids: %w", err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("memory: scan delete id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("memory: close delete ids: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("memory: iterate delete ids: %w", err)
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("memory: commit empty delete: %w", err)
		}
		return 0, nil
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM qa_memories WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("memory: delete records: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("memory: inspect delete records: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("memory: commit delete: %w", err)
	}
	if err := memory.deleteVectors(ctx, ids); err != nil {
		return int(affected), fmt.Errorf("memory: metadata deleted but vector cleanup failed: %w", err)
	}
	return int(affected), nil
}

func (memory *MemoryStore) deleteVectors(ctx context.Context, ids []string) error {
	if memory.semantic == nil || !memory.semantic.Enabled() || len(ids) == 0 {
		return nil
	}
	if err := memory.semantic.DeletePoints(ctx, ids); err != nil {
		return fmt.Errorf("delete %d vector points: %w", len(ids), err)
	}
	return nil
}

func encodeMemoryCursor(cursor memoryCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("memory: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryCursor(value string) (*memoryCursor, error) {
	if value == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidMemoryQuery)
	}
	var cursor memoryCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.CreatedAt.IsZero() || cursor.ID == "" {
		return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidMemoryQuery)
	}
	return &cursor, nil
}
