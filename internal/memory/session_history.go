package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const maxHistoryQueryItems = 100

const historySummaryColumns = "ref,turn_number,summary_text,summary_tokens"

// HistorySummary is the authoritative archived text used after candidate retrieval.
type HistorySummary struct {
	Ref           string `json:"ref"`
	TurnNumber    int    `json:"turn"`
	Summary       string `json:"summary"`
	SummaryTokens int    `json:"-"`
}

// HistoryIndexTask is one pending vector index mutation.
type HistoryIndexTask struct {
	ID        int64
	Operation string
	Ref       string
	SessionID string
	UserID    int64
	Attempts  int
}

// HasPendingHistoryUpserts reports whether dense recall may lag committed summaries.
func (ss *SessionStore) HasPendingHistoryUpserts(ctx context.Context, userID int64, sessionID string) (bool, error) {
	var pending int
	err := ss.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM qa_session_history_index_outbox
		WHERE operation='upsert' AND user_id=? AND session_id=? LIMIT 1)`, userID, sessionID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("memory/session history: inspect pending index: %w", err)
	}
	return pending == 1, nil
}

// FindHistoryRefs returns a bounded lexical ranking for canonical query terms.
func (ss *SessionStore) FindHistoryRefs(ctx context.Context, userID int64, sessionID string, terms []string, limit int) ([]string, error) {
	if len(terms) == 0 || sessionID == "" || userID <= 0 {
		return nil, nil
	}
	if len(terms) > 16 {
		terms = terms[:16]
	}
	if limit <= 0 || limit > maxHistoryQueryItems {
		limit = 64
	}
	args := make([]any, 0, len(terms)+3)
	args = append(args, userID, sessionID)
	for _, term := range terms {
		args = append(args, term)
	}
	args = append(args, limit)
	rows, err := ss.db.QueryContext(ctx, `SELECT ref
		FROM qa_session_history_terms
		WHERE user_id=? AND session_id=? AND term IN (`+placeholders(len(terms))+`)
		GROUP BY ref
		ORDER BY SUM(weight) DESC,MAX(turn_number) DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory/session history: lexical candidates: %w", err)
	}
	defer rows.Close()
	refs := make([]string, 0, limit)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// LoadHistorySummaries batch-revalidates refs against the current owner and session.
func (ss *SessionStore) LoadHistorySummaries(ctx context.Context, userID int64, sessionID string, refs []string) ([]HistorySummary, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > maxHistoryQueryItems {
		refs = refs[:maxHistoryQueryItems]
	}
	args := make([]any, 0, len(refs)+2)
	args = append(args, userID, sessionID)
	for _, ref := range refs {
		args = append(args, ref)
	}
	rows, err := ss.db.QueryContext(ctx, `SELECT `+historySummaryColumns+`
		FROM qa_turn_contexts
		WHERE user_id=? AND session_id=? AND ref IN (`+placeholders(len(refs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory/session history: load summaries: %w", err)
	}
	defer rows.Close()
	records := make([]HistorySummary, 0, len(refs))
	for rows.Next() {
		var record HistorySummary
		if err := rows.Scan(&record.Ref, &record.TurnNumber, &record.Summary, &record.SummaryTokens); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// LoadHistoryNeighbors reads only immediate turn neighbors for selected candidates.
func (ss *SessionStore) LoadHistoryNeighbors(ctx context.Context, userID int64, sessionID string, turns []int) ([]HistorySummary, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	neighborSet := make(map[int]struct{}, len(turns)*2)
	for _, turn := range turns {
		if turn > 1 {
			neighborSet[turn-1] = struct{}{}
		}
		neighborSet[turn+1] = struct{}{}
	}
	if len(neighborSet) > maxHistoryQueryItems {
		return nil, fmt.Errorf("memory/session history: neighbor query exceeds %d turns", maxHistoryQueryItems)
	}
	args := make([]any, 0, len(neighborSet)+2)
	args = append(args, userID, sessionID)
	for turn := range neighborSet {
		args = append(args, turn)
	}
	rows, err := ss.db.QueryContext(ctx, `SELECT `+historySummaryColumns+`
		FROM qa_turn_contexts
		WHERE user_id=? AND session_id=? AND turn_number IN (`+placeholders(len(neighborSet))+`)
		ORDER BY turn_number`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory/session history: load neighbors: %w", err)
	}
	defer rows.Close()
	records := make([]HistorySummary, 0, len(neighborSet))
	for rows.Next() {
		var record HistorySummary
		if err := rows.Scan(&record.Ref, &record.TurnNumber, &record.Summary, &record.SummaryTokens); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ListHistoryIndexTasks claims no state; rows remain pending until explicitly completed.
func (ss *SessionStore) ListHistoryIndexTasks(ctx context.Context, limit int) ([]HistoryIndexTask, error) {
	if limit <= 0 || limit > maxHistoryQueryItems {
		limit = 64
	}
	rows, err := ss.db.QueryContext(ctx, `SELECT id,operation,ref,session_id,user_id,attempts
		FROM qa_session_history_index_outbox
		WHERE next_attempt IS NULL OR next_attempt<=?
		ORDER BY id LIMIT ?`, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), limit)
	if err != nil {
		return nil, fmt.Errorf("memory/session history: list index tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]HistoryIndexTask, 0, limit)
	for rows.Next() {
		var task HistoryIndexTask
		if err := rows.Scan(&task.ID, &task.Operation, &task.Ref, &task.SessionID, &task.UserID, &task.Attempts); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// CompleteHistoryIndexTasks removes only the successfully processed outbox rows.
func (ss *SessionStore) CompleteHistoryIndexTasks(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxHistoryQueryItems {
		return fmt.Errorf("memory/session history: complete exceeds %d tasks", maxHistoryQueryItems)
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := ss.db.ExecContext(ctx, `DELETE FROM qa_session_history_index_outbox WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	return err
}

// FailHistoryIndexTasks records one visible error and a bounded retry delay.
func (ss *SessionStore) FailHistoryIndexTasks(ctx context.Context, ids []int64, message string, retryAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxHistoryQueryItems {
		return fmt.Errorf("memory/session history: fail exceeds %d tasks", maxHistoryQueryItems)
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, message, store.DatabaseTime(retryAt.UTC().Format(time.RFC3339)))
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := ss.db.ExecContext(ctx, `UPDATE qa_session_history_index_outbox
		SET attempts=attempts+1,last_error=?,next_attempt=?
		WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	return err
}

func insertHistoryTerms(tx *sql.Tx, records []TurnContextRecord) error {
	count := 0
	for _, record := range records {
		count += len(record.Terms)
	}
	if count == 0 {
		return nil
	}
	placeholdersSQL := make([]string, 0, count)
	args := make([]any, 0, count*6)
	for _, record := range records {
		if len(record.Terms) > 32 {
			return fmt.Errorf("memory/session history: turn %d exceeds 32 terms", record.TurnNumber)
		}
		for _, term := range record.Terms {
			if term.Value == "" || len(term.Value) > 191 {
				return fmt.Errorf("memory/session history: invalid term for turn %d", record.TurnNumber)
			}
			placeholdersSQL = append(placeholdersSQL, "(?,?,?,?,?,?)")
			args = append(args, record.SessionID, record.UserID, term.Value, record.Ref, record.TurnNumber, max(1, term.Weight))
		}
	}
	_, err := tx.Exec(`INSERT INTO qa_session_history_terms(session_id,user_id,term,ref,turn_number,weight) VALUES `+strings.Join(placeholdersSQL, ","), args...)
	return err
}

func enqueueHistoryUpserts(tx *sql.Tx, records []TurnContextRecord, createdAt any) error {
	if len(records) == 0 {
		return nil
	}
	placeholdersSQL := make([]string, len(records))
	args := make([]any, 0, len(records)*6)
	for i, record := range records {
		placeholdersSQL[i] = "('upsert',?,?,?,?,?)"
		args = append(args, record.Ref, record.SessionID, record.UserID, nil, createdAt)
	}
	_, err := tx.Exec(`INSERT INTO qa_session_history_index_outbox(operation,ref,session_id,user_id,next_attempt,created_at) VALUES `+
		strings.Join(placeholdersSQL, ",")+` ON DUPLICATE KEY UPDATE attempts=0,next_attempt=NULL,last_error=''`, args...)
	return err
}

func enqueueSessionHistoryDeletes(tx *sql.Tx, sessionID string, userID int64) error {
	if _, err := tx.Exec(`DELETE o FROM qa_session_history_index_outbox o
		JOIN qa_turn_contexts c ON c.ref=o.ref
		WHERE c.session_id=? AND c.user_id=? AND o.operation='upsert'`, sessionID, userID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO qa_session_history_index_outbox(operation,ref,session_id,user_id,next_attempt,created_at)
		SELECT 'delete',ref,session_id,user_id,NULL,? FROM qa_turn_contexts
		WHERE session_id=? AND user_id=?
		ON DUPLICATE KEY UPDATE attempts=0,next_attempt=NULL,last_error=''`,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), sessionID, userID)
	return err
}
