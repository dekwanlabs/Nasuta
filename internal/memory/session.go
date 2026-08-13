package memory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

type SessionStore struct {
	db *sql.DB
}

type SessionRecord struct {
	ID                    string               `json:"id"`
	UserID                int64                `json:"user_id"`
	Title                 string               `json:"title"`
	ArchivedSummaryTokens int64                `json:"archived_summary_tokens,omitempty"`
	CompactedThroughTurn  int                  `json:"compacted_through_turn,omitempty"`
	Messages              []llm.Message        `json:"messages,omitempty"`
	RecentTurns           []TurnMetadata       `json:"-"`
	RecentDialogue        []RecentDialogueTurn `json:"-"`
	MessageCount          int                  `json:"message_count"`
	LatestTurn            int                  `json:"latest_turn,omitempty"`
	CreatedAt             string               `json:"created_at"`
	UpdatedAt             string               `json:"updated_at"`
}

// CompactionCandidate is one contiguous, not-yet-summarized turn range.
type CompactionCandidate struct {
	SessionID                string
	UserID                   int64
	PreviousThrough          int
	FromTurn                 int
	ToTurn                   int
	EligibleThrough          int
	EstimatedReclaimedTokens int
	Turns                    []TurnCompactionCandidate
}

// SessionContextStats is the bounded persisted footprint used for compaction decisions.
type SessionContextStats struct {
	ArchivedSummaryTokens int64
	UncompactedTokens     int
	CompactedThroughTurn  int
	LatestTurn            int
}

// CompactionSelection describes one oldest-first batch target.
type CompactionSelection struct {
	KeepRecentTurns       int
	TargetReductionTokens int
}

// TurnCompactionCandidate keeps one logical turn intact before compression.
type TurnCompactionCandidate struct {
	RunID        string
	TurnNumber   int
	SourceTokens int
	Messages     []llm.Message
}

// TurnContextRecord is the bounded detail exposed by one stable reference.
type TurnContextRecord struct {
	Ref            string          `json:"ref"`
	SessionID      string          `json:"sessionId"`
	UserID         int64           `json:"userId,string"`
	RunID          string          `json:"-"`
	DetailJSON     json.RawMessage `json:"detail"`
	TurnNumber     int             `json:"turnNumber"`
	SummaryText    string          `json:"-"`
	SummaryTokens  int             `json:"-"`
	SourceTokens   int             `json:"-"`
	RetainedTokens int             `json:"-"`
	Terms          []HistoryTerm   `json:"-"`
}

// HistoryTerm is one canonical lexical key persisted with an archived turn.
type HistoryTerm struct {
	Value  string
	Weight int
}

// MessagePage is one reverse-cursor page returned in chronological order.
type MessagePage struct {
	Messages      []SessionMessage `json:"messages"`
	NextBeforeSeq int              `json:"next_before_seq"`
	HasMore       bool             `json:"has_more"`
}

// SessionMessage adds persistence metadata needed by history views.
type SessionMessage struct {
	llm.Message
	Seq       int    `json:"seq"`
	CreatedAt string `json:"created_at"`
	Feedback  string `json:"feedback,omitempty"`
	RunID     string `json:"-"`
}

var ErrSessionOwnership = errors.New("memory/session: session belongs to another user")

// NewSessionStore binds QA session queries to the platform-owned MySQL pool.
func NewSessionStore(db *sql.DB) *SessionStore {
	if db == nil {
		return nil
	}
	return &SessionStore{db: db}
}

func (ss *SessionStore) List(userID int64) ([]SessionRecord, error) {
	rows, err := ss.db.Query(
		`SELECT s.id, s.title, COALESCE(s.user_id,0), s.created_at, s.updated_at,
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
		if err := rows.Scan(&r.ID, &r.Title, &r.UserID, &createdAt, &updatedAt, &r.MessageCount); err != nil {
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
			`UPDATE qa_sessions SET title=?,archived_summary_tokens=0,compacted_through_turn=0,updated_at=? WHERE id=? AND user_id=?`,
			r.Title, store.DatabaseTime(r.UpdatedAt), r.ID, r.UserID,
		); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(
			`INSERT INTO qa_sessions(id,user_id,title,compacted_through_turn,created_at,updated_at) VALUES(?,?,?,0,?,?)`,
			r.ID, r.UserID, r.Title,
			store.DatabaseTime(r.CreatedAt), store.DatabaseTime(r.UpdatedAt),
		); err != nil {
			return err
		}
	}
	if err := enqueueSessionHistoryDeletes(tx, r.ID, r.UserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM qa_session_history_terms WHERE session_id = ?`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM qa_turns WHERE session_id = ?`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM qa_messages WHERE session_id = ?`, r.ID); err != nil {
		return err
	}
	if len(r.Messages) > 0 {
		turnNos, turns := assignSessionTurns(r.Messages)
		if err := insertSessionMessages(tx, r.ID, 0, turnNos, r.Messages, r.UpdatedAt); err != nil {
			return err
		}
		if err := insertSessionTurns(tx, r.ID, turns, r.UpdatedAt); err != nil {
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
	if err := enqueueSessionHistoryDeletes(tx, id, userID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM qa_session_history_terms WHERE session_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM qa_turns WHERE session_id = ?`, id); err != nil {
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
		`SELECT s.id, s.user_id, s.title,
		        s.archived_summary_tokens,s.compacted_through_turn,s.created_at,s.updated_at,
		        COALESCE((SELECT MAX(t.turn_no) FROM qa_turns t WHERE t.session_id=s.id),0)
		 FROM qa_sessions s WHERE s.id = ? AND s.user_id = ?`, id, userID)
	var r SessionRecord
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.UserID, &r.Title,
		&r.ArchivedSummaryTokens, &r.CompactedThroughTurn,
		&createdAt, &updatedAt, &r.LatestTurn); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	r.CreatedAt = store.FormatDatabaseTime(createdAt)
	r.UpdatedAt = store.FormatDatabaseTime(updatedAt)
	return &r, nil
}

// GetRecentSession loads one explicitly bounded message tail.
func (ss *SessionStore) GetRecentSession(id string, userID int64, limit int) (*SessionRecord, error) {
	r, err := ss.getSession(id, userID)
	if err != nil || r == nil || limit <= 0 {
		return r, err
	}
	rows, err := ss.db.Query(
		`SELECT m.role, m.content, COALESCE(m.tool_calls_json,''), m.tool_call_id, m.tool_name
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
		message, err := scanSessionMessage(rows)
		if err != nil {
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

// ListMessagesBefore fetches at most limit messages before seq; beforeSeq < 0 starts at the tail.
func (ss *SessionStore) ListMessagesBefore(id string, userID int64, beforeSeq, limit int) (*MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT m.seq, m.role, m.content, COALESCE(m.tool_calls_json,''), m.tool_call_id, m.tool_name, m.feedback, m.created_at,
	                 COALESCE(CASE WHEN m.seq=t.last_seq AND m.role='assistant' THEN t.run_id ELSE '' END,'')
	          FROM qa_messages m
	          JOIN qa_sessions s ON s.id = m.session_id
	          LEFT JOIN qa_turns t ON t.session_id=m.session_id AND t.turn_no=m.turn_no
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
		msg SessionMessage
	}
	desc := make([]sequencedMessage, 0, limit+1)
	for rows.Next() {
		var item sequencedMessage
		var toolCalls string
		var createdAt sql.NullTime
		if err := rows.Scan(&item.seq, &item.msg.Role, &item.msg.Content, &toolCalls, &item.msg.ToolCallID, &item.msg.Name, &item.msg.Feedback, &createdAt, &item.msg.RunID); err != nil {
			return nil, err
		}
		item.msg.Seq = item.seq
		item.msg.CreatedAt = store.FormatDatabaseTime(createdAt)
		if err := unmarshalToolCalls(toolCalls, &item.msg.Message); err != nil {
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
	page := &MessagePage{Messages: make([]SessionMessage, len(desc)), HasMore: hasMore, NextBeforeSeq: -1}
	if len(desc) > 0 {
		page.NextBeforeSeq = desc[len(desc)-1].seq
	}
	for i := range desc {
		page.Messages[len(desc)-1-i] = desc[i].msg
	}
	return page, nil
}

// ListTurnsBefore keeps tool-heavy turns intact while bounding history by turn count.
func (ss *SessionStore) ListTurnsBefore(id string, userID int64, beforeSeq, limit int) (*MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	type turnBound struct {
		firstSeq int
		lastSeq  int
	}
	query := `SELECT t.first_seq, t.last_seq
	          FROM qa_turns t
	          JOIN qa_sessions s ON s.id = t.session_id
	          WHERE t.session_id = ? AND s.user_id = ?`
	args := []any{id, userID}
	if beforeSeq >= 0 {
		query += ` AND t.last_seq < ?`
		args = append(args, beforeSeq)
	}
	query += ` ORDER BY t.turn_no DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := ss.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	bounds := make([]turnBound, 0, limit+1)
	for rows.Next() {
		var bound turnBound
		if err := rows.Scan(&bound.firstSeq, &bound.lastSeq); err != nil {
			rows.Close()
			return nil, err
		}
		bounds = append(bounds, bound)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(bounds) == 0 {
		return &MessagePage{NextBeforeSeq: -1}, nil
	}

	hasMore := len(bounds) > limit
	if hasMore {
		bounds = bounds[:limit]
	}
	firstSeq := bounds[len(bounds)-1].firstSeq
	messageQuery := `SELECT m.seq, m.role, m.content, COALESCE(m.tool_calls_json,''), m.tool_call_id, m.tool_name, m.feedback, m.created_at,
	                        COALESCE(CASE WHEN m.seq=t.last_seq AND m.role='assistant' THEN t.run_id ELSE '' END,'')
	                 FROM qa_messages m
	                 JOIN qa_sessions s ON s.id = m.session_id
	                 LEFT JOIN qa_turns t ON t.session_id=m.session_id AND t.turn_no=m.turn_no
	                 WHERE m.session_id = ? AND s.user_id = ? AND m.seq >= ?`
	messageArgs := []any{id, userID, firstSeq}
	if beforeSeq >= 0 {
		messageQuery += ` AND m.seq < ?`
		messageArgs = append(messageArgs, beforeSeq)
	}
	messageQuery += ` ORDER BY m.seq ASC`
	messageRows, err := ss.db.Query(messageQuery, messageArgs...)
	if err != nil {
		return nil, err
	}
	defer messageRows.Close()
	page := &MessagePage{
		Messages:      make([]SessionMessage, 0, limit*2),
		HasMore:       hasMore,
		NextBeforeSeq: firstSeq,
	}
	for messageRows.Next() {
		var seq int
		var msg SessionMessage
		var toolCalls string
		var createdAt sql.NullTime
		if err := messageRows.Scan(&seq, &msg.Role, &msg.Content, &toolCalls, &msg.ToolCallID, &msg.Name, &msg.Feedback, &createdAt, &msg.RunID); err != nil {
			return nil, err
		}
		msg.Seq = seq
		msg.CreatedAt = store.FormatDatabaseTime(createdAt)
		if err := unmarshalToolCalls(toolCalls, &msg.Message); err != nil {
			return nil, err
		}
		page.Messages = append(page.Messages, msg)
	}
	if err := messageRows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// SetMessageFeedback updates only an owned final assistant answer.
func (ss *SessionStore) SetMessageFeedback(sessionID string, userID int64, seq int, runID, feedback string) (bool, error) {
	tx, err := ss.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	query := `SELECT m.id, m.feedback
	          FROM qa_messages m
	          JOIN qa_sessions s ON s.id=m.session_id
	          JOIN qa_turns t ON t.session_id=m.session_id AND t.last_seq=m.seq
	          WHERE m.session_id=? AND s.user_id=? AND m.role='assistant'`
	args := []any{sessionID, userID}
	if seq > 0 {
		query += ` AND m.seq=?`
		args = append(args, seq)
	} else {
		query += ` AND t.run_id=?`
		args = append(args, runID)
	}
	query += ` FOR UPDATE`

	var messageID int64
	var current string
	if err := tx.QueryRow(query, args...).Scan(&messageID, &current); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("memory/session: find answer feedback target: %w", err)
	}
	if current != feedback {
		if _, err := tx.Exec(`UPDATE qa_messages SET feedback=? WHERE id=?`, feedback, messageID); err != nil {
			return false, fmt.Errorf("memory/session: update answer feedback: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// AppendTurn atomically records the model-visible protocol for audit and replay.
func (ss *SessionStore) AppendTurn(sessionID, runID string, userID int64, msgs []llm.Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	if runID == "" {
		return 0, fmt.Errorf("memory/session: run id is required")
	}
	tx, err := ss.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	exists, err := lockSessionOwner(tx, sessionID, userID)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, fmt.Errorf("memory/session: session %q not found", sessionID)
	}
	existingTurn, found, err := findSessionRunTurn(tx, sessionID, runID)
	if err != nil {
		return 0, err
	}
	if found {
		return existingTurn, tx.Commit()
	}

	firstSeq, turnNo, err := nextSessionTurnPosition(tx, sessionID)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	metadata := buildTurnMetadata(turnNo, runID, msgs, now)
	turnNos := make([]int, len(msgs))
	for i := range turnNos {
		turnNos[i] = turnNo
	}
	if err := insertSessionMessages(tx, sessionID, firstSeq, turnNos, msgs, now); err != nil {
		return 0, err
	}
	turn := pendingSessionTurn{
		no: turnNo, runID: runID, firstSeq: firstSeq, lastSeq: firstSeq + len(msgs) - 1, metadata: metadata,
	}
	if err := insertSessionTurns(tx, sessionID, []pendingSessionTurn{turn}, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE qa_sessions SET updated_at = ? WHERE id = ? AND user_id=?`,
		store.DatabaseTime(now), sessionID, userID,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return turnNo, nil
}

// SessionContextStats avoids loading message bodies for threshold checks.
func (ss *SessionStore) SessionContextStats(sessionID string, userID int64) (SessionContextStats, error) {
	var stats SessionContextStats
	err := ss.db.QueryRow(
		`SELECT s.archived_summary_tokens,s.compacted_through_turn,
		        COALESCE((SELECT SUM(t.token_estimate) FROM qa_turns t
		                  WHERE t.session_id=s.id AND t.turn_no>s.compacted_through_turn),0),
		        COALESCE((SELECT MAX(t.turn_no) FROM qa_turns t WHERE t.session_id=s.id),0)
		 FROM qa_sessions s WHERE s.id=? AND s.user_id=?`,
		sessionID, userID).Scan(
		&stats.ArchivedSummaryTokens, &stats.CompactedThroughTurn,
		&stats.UncompactedTokens, &stats.LatestTurn,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionContextStats{}, nil
	}
	return stats, err
}

// PrepareCompaction reads only the newly eligible contiguous turn range.
func (ss *SessionStore) PrepareCompaction(sessionID string, userID int64, selection CompactionSelection) (*CompactionCandidate, error) {
	if selection.KeepRecentTurns < 1 {
		return nil, fmt.Errorf("memory/session: keep turns must be positive")
	}
	if selection.TargetReductionTokens <= 0 {
		return nil, fmt.Errorf("memory/session: target reduction must be positive")
	}
	session, err := ss.getSession(sessionID, userID)
	if err != nil || session == nil {
		return nil, err
	}
	eligibleThrough := session.LatestTurn - selection.KeepRecentTurns
	if eligibleThrough <= session.CompactedThroughTurn {
		return nil, nil
	}
	fromTurn := session.CompactedThroughTurn + 1
	metaRows, err := ss.db.Query(
		`SELECT t.turn_no,t.token_estimate
		 FROM qa_turns t JOIN qa_sessions s ON s.id=t.session_id
		 WHERE t.session_id=? AND s.user_id=? AND t.turn_no BETWEEN ? AND ?
		 ORDER BY t.turn_no`,
		sessionID, userID, fromTurn, eligibleThrough)
	if err != nil {
		return nil, err
	}
	toTurn := 0
	estimatedReclaimed := 0
	expectedTurn := fromTurn
	for metaRows.Next() {
		var turnNumber, sourceTokens int
		if err := metaRows.Scan(&turnNumber, &sourceTokens); err != nil {
			metaRows.Close()
			return nil, err
		}
		if turnNumber != expectedTurn {
			metaRows.Close()
			return nil, fmt.Errorf("memory/session: incomplete turn range %d-%d (missing: %d)",
				fromTurn, eligibleThrough, expectedTurn)
		}
		toTurn = turnNumber
		estimatedReclaimed += sourceTokens
		expectedTurn++
		if estimatedReclaimed >= selection.TargetReductionTokens {
			break
		}
	}
	if err := metaRows.Err(); err != nil {
		metaRows.Close()
		return nil, err
	}
	metaRows.Close()
	if toTurn == 0 {
		return nil, fmt.Errorf("memory/session: incomplete turn range %d-%d (missing: %d)",
			fromTurn, eligibleThrough, fromTurn)
	}
	if estimatedReclaimed < selection.TargetReductionTokens && toTurn < eligibleThrough {
		return nil, fmt.Errorf("memory/session: incomplete turn range %d-%d (missing: %d)",
			fromTurn, eligibleThrough, toTurn+1)
	}
	rows, err := ss.db.Query(
		`SELECT m.turn_no,t.run_id,t.token_estimate,m.role,m.content,
		        COALESCE(m.tool_calls_json,''),m.tool_call_id,m.tool_name
		 FROM qa_messages m
		 JOIN qa_sessions s ON s.id=m.session_id
		 JOIN qa_turns t ON t.session_id=m.session_id AND t.turn_no=m.turn_no
		 WHERE m.session_id=? AND s.user_id=? AND m.turn_no BETWEEN ? AND ?
		 ORDER BY m.turn_no,m.seq`, sessionID, userID, fromTurn, toTurn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := make([]TurnCompactionCandidate, 0, toTurn-fromTurn+1)
	for rows.Next() {
		var turnNumber, sourceTokens int
		var runID, toolCalls string
		var message llm.Message
		if err := rows.Scan(
			&turnNumber, &runID, &sourceTokens, &message.Role, &message.Content,
			&toolCalls, &message.ToolCallID, &message.Name,
		); err != nil {
			return nil, err
		}
		if err := unmarshalToolCalls(toolCalls, &message); err != nil {
			return nil, err
		}
		if len(turns) == 0 || turns[len(turns)-1].TurnNumber != turnNumber {
			turns = append(turns, TurnCompactionCandidate{
				RunID: runID, TurnNumber: turnNumber, SourceTokens: sourceTokens,
			})
		}
		turn := &turns[len(turns)-1]
		turn.Messages = append(turn.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(turns) != toTurn-fromTurn+1 {
		missing := make([]string, 0, toTurn-fromTurn+1-len(turns))
		turnIndex := 0
		for expected := fromTurn; expected <= toTurn; expected++ {
			if turnIndex < len(turns) && turns[turnIndex].TurnNumber == expected {
				turnIndex++
				continue
			}
			missing = append(missing, strconv.Itoa(expected))
		}
		return nil, fmt.Errorf("memory/session: incomplete turn range %d-%d (missing: %s)",
			fromTurn, toTurn, strings.Join(missing, ","))
	}
	return &CompactionCandidate{
		SessionID: sessionID, UserID: userID, PreviousThrough: session.CompactedThroughTurn, FromTurn: fromTurn,
		ToTurn: toTurn, EligibleThrough: eligibleThrough,
		EstimatedReclaimedTokens: estimatedReclaimed, Turns: turns,
	}, nil
}

// ApplyCompaction rejects stale generators instead of overwriting newer progress.
func (ss *SessionStore) ApplyCompaction(candidate CompactionCandidate, records []TurnContextRecord) (bool, error) {
	if len(records) != candidate.ToTurn-candidate.FromTurn+1 {
		return false, fmt.Errorf("memory/session: compressed records do not cover turns %d-%d", candidate.FromTurn, candidate.ToTurn)
	}
	for i, record := range records {
		expectedTurn := candidate.FromTurn + i
		if record.SessionID != candidate.SessionID || record.UserID != candidate.UserID || record.TurnNumber != expectedTurn {
			return false, fmt.Errorf("memory/session: invalid compressed record for turn %d", expectedTurn)
		}
		if !json.Valid(record.DetailJSON) {
			return false, fmt.Errorf("memory/session: invalid detail JSON for turn %d", expectedTurn)
		}
	}
	tx, err := ss.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var ownerID int64
	var currentThrough int
	if err := tx.QueryRow(
		`SELECT user_id,compacted_through_turn FROM qa_sessions WHERE id=? FOR UPDATE`,
		candidate.SessionID).Scan(&ownerID, &currentThrough); err != nil {
		return false, err
	}
	if ownerID != candidate.UserID {
		return false, ErrSessionOwnership
	}
	if currentThrough != candidate.PreviousThrough {
		return false, nil
	}
	var contextRows strings.Builder
	args := make([]any, 0, len(records)*9+1)
	now := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339))
	archivedTokens := int64(0)
	for i, record := range records {
		if record.SummaryTokens <= 0 {
			return false, fmt.Errorf("memory/session: summary token count for turn %d must be positive", record.TurnNumber)
		}
		if i > 0 {
			contextRows.WriteString(" UNION ALL ")
		}
		contextRows.WriteString(
			"SELECT ? AS turn_no,? AS run_id,? AS ref,? AS detail_json," +
				"? AS summary_text,? AS summary_tokens,? AS source_tokens," +
				"? AS retained_tokens,? AS archived_at",
		)
		args = append(args,
			record.TurnNumber, record.RunID, record.Ref, []byte(record.DetailJSON),
			record.SummaryText, record.SummaryTokens, record.SourceTokens,
			record.RetainedTokens, now,
		)
		archivedTokens += int64(record.SummaryTokens)
	}
	args = append(args, candidate.SessionID)
	result, err := tx.Exec(
		`UPDATE qa_turns target
		 JOIN (`+contextRows.String()+`) context
		   ON context.turn_no=target.turn_no AND context.run_id=target.run_id
		 SET target.context_ref=context.ref,
		     target.context_detail_json=context.detail_json,
		     target.context_summary_text=context.summary_text,
		     target.context_summary_tokens=context.summary_tokens,
		     target.context_source_tokens=context.source_tokens,
		     target.context_retained_tokens=context.retained_tokens,
		     target.context_archived_at=context.archived_at
		 WHERE target.session_id=? AND target.context_ref IS NULL`,
		args...,
	)
	if err != nil {
		return false, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return false, err
	} else if affected != int64(len(records)) {
		return false, fmt.Errorf(
			"memory/session: archived %d of %d selected turns",
			affected, len(records),
		)
	}
	if err := insertHistoryTerms(tx, records); err != nil {
		return false, err
	}
	if err := enqueueHistoryUpserts(tx, records, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`UPDATE qa_sessions SET archived_summary_tokens=archived_summary_tokens+?,compacted_through_turn=?,updated_at=?
		 WHERE id=? AND user_id=?`,
		archivedTokens, candidate.ToTurn, now,
		candidate.SessionID, candidate.UserID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// GetTurnDetail resolves exactly one bounded context under the current owner.
func (ss *SessionStore) GetTurnDetail(sessionID string, userID int64, reference string) (*TurnContextRecord, error) {
	var record TurnContextRecord
	var detail []byte
	err := ss.db.QueryRow(
		`SELECT t.context_ref,t.session_id,s.user_id,t.run_id,t.context_detail_json,
			t.turn_no,t.context_summary_text,t.context_summary_tokens,
			t.context_source_tokens,t.context_retained_tokens
		 FROM qa_turns t
		 JOIN qa_sessions s ON s.id=t.session_id
		 WHERE t.context_ref=? AND t.session_id=? AND s.user_id=?`,
		reference, sessionID, userID).Scan(
		&record.Ref, &record.SessionID, &record.UserID, &record.RunID, &detail,
		&record.TurnNumber, &record.SummaryText, &record.SummaryTokens,
		&record.SourceTokens, &record.RetainedTokens,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("memory/session: turn reference %q is not available in the current session", reference)
		}
		return nil, err
	}
	if !json.Valid(detail) {
		return nil, fmt.Errorf("memory/session: stored turn detail %q is not valid JSON", reference)
	}
	record.DetailJSON = detail
	return &record, nil
}

type pendingSessionTurn struct {
	no, firstSeq, lastSeq int
	runID                 string
	metadata              TurnMetadata
}

func assignSessionTurns(messages []llm.Message) ([]int, []pendingSessionTurn) {
	turnNos := make([]int, len(messages))
	turns := make([]pendingSessionTurn, 0, len(messages)/2+1)
	turnNo := 0
	for i, message := range messages {
		if turnNo == 0 || message.Role == "user" {
			turnNo++
			turns = append(turns, pendingSessionTurn{no: turnNo, firstSeq: i, lastSeq: i})
		}
		turnNos[i] = turnNo
		turn := &turns[len(turns)-1]
		turn.lastSeq = i
	}
	for i := range turns {
		turn := &turns[i]
		turn.metadata = buildTurnMetadata(turn.no, "", messages[turn.firstSeq:turn.lastSeq+1], "")
	}
	return turnNos, turns
}

func insertSessionTurns(tx *sql.Tx, sessionID string, turns []pendingSessionTurn, createdAt string) error {
	if len(turns) == 0 {
		return nil
	}
	placeholders := make([]string, len(turns))
	args := make([]any, 0, len(turns)*12)
	for i, turn := range turns {
		entitiesJSON, err := encodeMetadataJSON(turn.metadata.Entities)
		if err != nil {
			return fmt.Errorf("memory/session: encode turn %d entities: %w", turn.no, err)
		}
		termsJSON, err := encodeMetadataJSON(turn.metadata.QuestionTerms)
		if err != nil {
			return fmt.Errorf("memory/session: encode turn %d terms: %w", turn.no, err)
		}
		manifestJSON, err := encodeMetadataJSON(turn.metadata.EvidenceManifest)
		if err != nil {
			return fmt.Errorf("memory/session: encode turn %d evidence manifest: %w", turn.no, err)
		}
		placeholders[i] = "(?,?,?,?,?,?,?,?,?,?,?,?)"
		args = append(args, sessionID, turn.no, turn.runID, turn.firstSeq, turn.lastSeq, turn.metadata.TokenEstimate,
			turn.metadata.Question, turn.metadata.TopicKey, entitiesJSON, termsJSON, manifestJSON, store.DatabaseTime(createdAt))
	}
	_, err := tx.Exec(
		"INSERT INTO qa_turns(session_id,turn_no,run_id,first_seq,last_seq,token_estimate,question_text,topic_key,entities_json,question_terms_json,evidence_manifest_json,created_at) VALUES "+strings.Join(placeholders, ","),
		args...)
	if err != nil {
		return fmt.Errorf("memory/session: insert turns for session %q: %w", sessionID, err)
	}
	return nil
}

func insertSessionMessages(
	tx *sql.Tx,
	sessionID string,
	firstSeq int,
	turnNos []int,
	messages []llm.Message,
	createdAt string,
) error {
	placeholders := make([]string, len(messages))
	args := make([]any, 0, len(messages)*9)
	for i, message := range messages {
		toolCalls, err := marshalToolCalls(message.ToolCalls)
		if err != nil {
			return fmt.Errorf("memory/session: encode message %d tool calls: %w", i, err)
		}
		placeholders[i] = "(?,?,?,?,?,?,?,?,?)"
		args = append(args, sessionID, firstSeq+i, turnNos[i], message.Role, message.Content,
			toolCalls, message.ToolCallID, message.Name, store.DatabaseTime(createdAt))
	}
	_, err := tx.Exec(
		"INSERT INTO qa_messages(session_id,seq,turn_no,role,content,tool_calls_json,tool_call_id,tool_name,created_at) VALUES "+strings.Join(placeholders, ","),
		args...)
	if err != nil {
		return fmt.Errorf("memory/session: insert messages for session %q: %w", sessionID, err)
	}
	return nil
}

func findSessionRunTurn(tx *sql.Tx, sessionID, runID string) (int, bool, error) {
	var turnNo int
	err := tx.QueryRow(
		`SELECT turn_no FROM qa_turns WHERE session_id=? AND run_id=? LIMIT 1`,
		sessionID, runID,
	).Scan(&turnNo)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("memory/session: find run %q in session %q: %w", runID, sessionID, err)
	}
	return turnNo, true, nil
}

func nextSessionTurnPosition(tx *sql.Tx, sessionID string) (int, int, error) {
	var maxSeq, maxTurn int
	if err := tx.QueryRow(
		`SELECT COALESCE((SELECT MAX(seq) FROM qa_messages WHERE session_id=?),-1),
		        COALESCE((SELECT MAX(turn_no) FROM qa_turns WHERE session_id=?),0)`,
		sessionID, sessionID,
	).Scan(&maxSeq, &maxTurn); err != nil {
		return 0, 0, fmt.Errorf("memory/session: find append position for session %q: %w", sessionID, err)
	}
	return maxSeq + 1, maxTurn + 1, nil
}

func estimateSessionTokens(messages []llm.Message) int {
	tokens := 0
	for _, message := range messages {
		tokens += estimateSessionMessageTokens(message)
	}
	return tokens
}

func estimateSessionMessageTokens(message llm.Message) int {
	units := 0
	for _, value := range []string{message.Role, message.Content, message.ToolCallID, message.Name} {
		units += estimateSessionTextUnits(value)
	}
	for _, call := range message.ToolCalls {
		for _, value := range []string{call.ID, call.Function.Name, call.Function.Arguments} {
			units += estimateSessionTextUnits(value)
		}
	}
	return (units + 29) / 30
}

func estimateSessionTextUnits(value string) int {
	units := 0
	for _, r := range value {
		if r <= 127 {
			units += 11
		} else {
			units += 66
		}
	}
	return units
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
			`INSERT INTO qa_sessions(id,user_id,title,created_at,updated_at) VALUES(?,?,?,?,?)`,
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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSessionMessage(scanner rowScanner) (llm.Message, error) {
	var message llm.Message
	var toolCalls string
	if err := scanner.Scan(&message.Role, &message.Content, &toolCalls, &message.ToolCallID, &message.Name); err != nil {
		return llm.Message{}, err
	}
	if err := unmarshalToolCalls(toolCalls, &message); err != nil {
		return llm.Message{}, err
	}
	return message, nil
}

func marshalToolCalls(calls []llm.ToolCall) (string, error) {
	if len(calls) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalToolCalls(raw string, message *llm.Message) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &message.ToolCalls); err != nil {
		return fmt.Errorf("memory/session: decode tool calls: %w", err)
	}
	return nil
}

func firstUserQuestion(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" && m.Content != "" {
			return m.Content[:min(len(m.Content), 50)]
		}
	}
	return ""
}
