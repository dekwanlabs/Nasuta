package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/llm"
	_ "github.com/go-sql-driver/mysql"
)

type SessionStore struct {
	db *sql.DB
}

type SessionRecord struct {
	ID           string        `json:"id"`
	UserID       int64         `json:"user_id"`
	Title        string        `json:"title"`
	Summary      string        `json:"summary"`
	Messages     []llm.Message `json:"messages,omitempty"`
	MessageCount int           `json:"message_count"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
}

// MessagePage is one reverse-cursor page returned in chronological order.
type MessagePage struct {
	Messages      []llm.Message `json:"messages"`
	NextBeforeSeq int           `json:"next_before_seq"`
	HasMore       bool          `json:"has_more"`
}

var ErrSessionOwnership = errors.New("memory/session: session belongs to another user")

func OpenSessionStore(dsn string) (*SessionStore, error) {
	db, err := store.MySQL(dsn)
	if err != nil {
		return nil, fmt.Errorf("memory/session: open: %w", err)
	}
	if err := dbschema.MigrateMySQL(db, dbschema.GroupQASession); err != nil {
		return nil, fmt.Errorf("memory/session: migrate schema: %w", err)
	}
	return &SessionStore{db: db}, nil
}

func (ss *SessionStore) List(userID int64) ([]SessionRecord, error) {
	rows, err := ss.db.Query(
		`SELECT s.id, s.title, COALESCE(s.summary,''), COALESCE(s.user_id,0), s.created_at, s.updated_at,
		        (SELECT COUNT(*) FROM qa_messages m WHERE m.session_id = s.id)
		 FROM qa_sessions s WHERE s.user_id = ? ORDER BY s.updated_at DESC LIMIT 50`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRecord
	for rows.Next() {
		var r SessionRecord
		var createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.Title, &r.Summary, &r.UserID, &createdAt, &updatedAt, &r.MessageCount); err != nil {
			return nil, err
		}
		r.CreatedAt = store.FormatDatabaseTime(createdAt)
		r.UpdatedAt = store.FormatDatabaseTime(updatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (ss *SessionStore) Save(r SessionRecord) error {
	if r.CreatedAt == "" {
		r.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if r.Title == "" && len(r.Messages) > 0 {
		r.Title = firstUserQuestion(r.Messages)
	}

	tx, err := ss.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	exists, err := lockSessionOwner(tx, r.ID, r.UserID)
	if err != nil {
		return err
	}
	if exists {
		if _, err := tx.Exec(
			`UPDATE qa_sessions SET title=?,summary=?,updated_at=? WHERE id=? AND user_id=?`,
			r.Title, r.Summary, store.DatabaseTime(r.UpdatedAt), r.ID, r.UserID,
		); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(
			`INSERT INTO qa_sessions(id,user_id,title,summary,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			r.ID, r.UserID, r.Title, r.Summary,
			store.DatabaseTime(r.CreatedAt), store.DatabaseTime(r.UpdatedAt),
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM qa_messages WHERE session_id = ?`, r.ID); err != nil {
		return err
	}
	if len(r.Messages) > 0 {
		placeholders := make([]string, len(r.Messages))
		args := make([]any, 0, len(r.Messages)*5)
		for i, m := range r.Messages {
			placeholders[i] = "(?,?,?,?,?)"
			args = append(args, r.ID, i, m.Role, m.Content, store.DatabaseTime(r.UpdatedAt))
		}
		query := "INSERT INTO qa_messages(session_id,seq,role,content,created_at) VALUES " + strings.Join(placeholders, ",")
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (ss *SessionStore) Delete(id string, userID int64) (bool, error) {
	tx, err := ss.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var owned int
	if err := tx.QueryRow(`SELECT 1 FROM qa_sessions WHERE id=? AND user_id=? FOR UPDATE`, id, userID).Scan(&owned); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM qa_messages WHERE session_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM qa_sessions WHERE id = ? AND user_id=?`, id, userID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (ss *SessionStore) getSession(id string, userID int64) (*SessionRecord, error) {
	row := ss.db.QueryRow(
		`SELECT s.id, s.user_id, s.title, COALESCE(s.summary,''), s.created_at, s.updated_at
		 FROM qa_sessions s WHERE s.id = ? AND s.user_id = ?`, id, userID)
	var r SessionRecord
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.UserID, &r.Title, &r.Summary, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	r.CreatedAt = store.FormatDatabaseTime(createdAt)
	r.UpdatedAt = store.FormatDatabaseTime(updatedAt)
	return &r, nil
}

// GetRecentSession loads only the tail needed by the online agent path.
func (ss *SessionStore) GetRecentSession(id string, userID int64, limit int) (*SessionRecord, error) {
	r, err := ss.getSession(id, userID)
	if err != nil || r == nil || limit <= 0 {
		return r, err
	}
	rows, err := ss.db.Query(
		`SELECT m.role, m.content
		 FROM qa_messages m
		 JOIN qa_sessions s ON s.id = m.session_id
		 WHERE m.session_id = ? AND s.user_id = ?
		 ORDER BY m.seq DESC LIMIT ?`, id, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	desc := make([]llm.Message, 0, limit)
	for rows.Next() {
		var message llm.Message
		if err := rows.Scan(&message.Role, &message.Content); err != nil {
			return nil, err
		}
		desc = append(desc, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.Messages = make([]llm.Message, len(desc))
	for i := range desc {
		r.Messages[len(desc)-1-i] = desc[i]
	}
	return r, nil
}

// GetFullSession is reserved for workflows that intentionally need all messages.
func (ss *SessionStore) GetFullSession(id string, userID int64) (*SessionRecord, error) {
	r, err := ss.getSession(id, userID)
	if err != nil || r == nil {
		return r, err
	}
	mrows, err := ss.db.Query(
		`SELECT m.role, m.content
		 FROM qa_messages m
		 JOIN qa_sessions s ON s.id = m.session_id
		 WHERE m.session_id = ? AND s.user_id = ?
		 ORDER BY m.seq`, id, userID)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m llm.Message
		if err := mrows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		r.Messages = append(r.Messages, m)
	}
	r.MessageCount = len(r.Messages)
	return r, mrows.Err()
}

// ListMessagesBefore fetches at most limit messages before seq; beforeSeq < 0 starts at the tail.
func (ss *SessionStore) ListMessagesBefore(id string, userID int64, beforeSeq, limit int) (*MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT m.seq, m.role, m.content
	          FROM qa_messages m
	          JOIN qa_sessions s ON s.id = m.session_id
	          WHERE m.session_id = ? AND s.user_id = ?`
	args := []any{id, userID}
	if beforeSeq >= 0 {
		query += ` AND m.seq < ?`
		args = append(args, beforeSeq)
	}
	query += ` ORDER BY m.seq DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := ss.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type sequencedMessage struct {
		seq int
		msg llm.Message
	}
	desc := make([]sequencedMessage, 0, limit+1)
	for rows.Next() {
		var item sequencedMessage
		if err := rows.Scan(&item.seq, &item.msg.Role, &item.msg.Content); err != nil {
			return nil, err
		}
		desc = append(desc, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	page := &MessagePage{Messages: make([]llm.Message, len(desc)), HasMore: hasMore, NextBeforeSeq: -1}
	if len(desc) > 0 {
		page.NextBeforeSeq = desc[len(desc)-1].seq
	}
	for i := range desc {
		page.Messages[len(desc)-1-i] = desc[i].msg
	}
	return page, nil
}

func (ss *SessionStore) AppendMessages(sessionID string, userID int64, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := ss.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exists, err := lockSessionOwner(tx, sessionID, userID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("memory/session: session %q not found", sessionID)
	}

	var maxSeq int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), -1) FROM qa_messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	placeholders := make([]string, len(msgs))
	args := make([]any, 0, len(msgs)*5)
	for i, m := range msgs {
		placeholders[i] = "(?,?,?,?,?)"
		args = append(args, sessionID, maxSeq+1+i, m.Role, m.Content, store.DatabaseTime(now))
	}
	query := "INSERT INTO qa_messages(session_id,seq,role,content,created_at) VALUES " + strings.Join(placeholders, ",")
	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE qa_sessions SET updated_at = ? WHERE id = ? AND user_id=?`,
		store.DatabaseTime(now), sessionID, userID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (ss *SessionStore) UpdateSummary(id string, userID int64, summary string) error {
	result, err := ss.db.Exec(
		`UPDATE qa_sessions SET summary = ?, updated_at = ? WHERE id = ? AND user_id=?`,
		summary, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), id, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("memory/session: session %q not found for user", id)
	}
	return nil
}

func (ss *SessionStore) EnsureSession(id string, userID int64, title string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := ss.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exists, err := lockSessionOwner(tx, id, userID)
	if err != nil {
		return err
	}
	if exists {
		_, err = tx.Exec(
			`UPDATE qa_sessions
			 SET updated_at=?,title=CASE WHEN title='' THEN ? ELSE title END
			 WHERE id=? AND user_id=?`,
			store.DatabaseTime(now), title, id, userID,
		)
	} else {
		_, err = tx.Exec(
			`INSERT INTO qa_sessions(id,user_id,title,summary,created_at,updated_at) VALUES(?,?,?,'',?,?)`,
			id, userID, title, store.DatabaseTime(now), store.DatabaseTime(now),
		)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func lockSessionOwner(tx *sql.Tx, id string, userID int64) (bool, error) {
	var ownerID int64
	err := tx.QueryRow(`SELECT user_id FROM qa_sessions WHERE id=? FOR UPDATE`, id).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ownerID != userID {
		return false, ErrSessionOwnership
	}
	return true, nil
}

func firstUserQuestion(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" && m.Content != "" {
			return m.Content[:min(len(m.Content), 50)]
		}
	}
	return ""
}
