package approval

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/log"
)

// ActionStatus enumerates pending action lifecycle states.
type ActionStatus string

const (
	ActionPending  ActionStatus = "pending"
	ActionApproved ActionStatus = "approved"
	ActionRejected ActionStatus = "rejected"
	ActionDone     ActionStatus = "done"
	ActionFailed   ActionStatus = "failed"
	ActionExpired  ActionStatus = "expired"
)

// PendingAction is one proposed write operation awaiting human approval.
type PendingAction struct {
	ID          string         `json:"id"`
	Tool        string         `json:"tool"`
	IncidentID  string         `json:"incident_id"`
	Args        map[string]any `json:"args"`
	Rationale   string         `json:"rationale"`
	Impact      string         `json:"impact"`
	Status      ActionStatus   `json:"status"`
	RequestedBy int64          `json:"requested_by"`
	Approver    int64          `json:"approver"`
	Result      any            `json:"result,omitempty"`
	CreatedAt   string         `json:"created_at"`
	DecidedAt   string         `json:"decided_at,omitempty"`
	ExpiresAt   string         `json:"expires_at"`
}

// Create records a proposed write action in pending status and returns it.
func (svc *Service) Create(action PendingAction) (*PendingAction, error) {
	if action.ID == "" {
		action.ID = newActionID()
	}
	now := time.Now().UTC()
	if action.CreatedAt == "" {
		action.CreatedAt = now.Format(time.RFC3339)
	}
	action.ExpiresAt = now.Add(svc.ttl).Format(time.RFC3339)
	action.Status = ActionPending

	argsJSON, _ := json.Marshal(action.Args)
	_, err := svc.db.Exec(
		`INSERT INTO pending_actions(id,tool,incident_id,args_json,rationale,impact,status,requested_by,created_at,expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		action.ID, action.Tool, action.IncidentID, string(argsJSON), action.Rationale, action.Impact, action.Status, action.RequestedBy,
		store.DatabaseTime(action.CreatedAt), store.DatabaseTime(action.ExpiresAt))
	if err != nil {
		return nil, err
	}
	return &action, nil
}

// Get returns one pending action by ID.
func (svc *Service) Get(id string) (*PendingAction, error) {
	act, err := scanPendingAction(svc.db.QueryRow(
		`SELECT id,tool,incident_id,args_json,rationale,impact,status,requested_by,approver,result_json,created_at,decided_at,expires_at
		 FROM pending_actions WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &act, nil
}

// ListPage returns paginated actions, optionally filtered by status.
func (svc *Service) ListPage(status ActionStatus, page, pageSize int) (*domain.Page[PendingAction], error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}

	q := `SELECT id,tool,incident_id,args_json,rationale,impact,status,requested_by,approver,result_json,created_at,decided_at,expires_at FROM pending_actions`
	countQ := `SELECT COUNT(*) FROM pending_actions`
	var args []any
	if status != "" {
		q += " WHERE status=?"
		countQ += " WHERE status=?"
		args = append(args, status)
	}

	var total int
	if err := svc.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, err
	}

	q += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	offset := (page - 1) * pageSize
	args = append(args, pageSize, offset)
	rows, err := svc.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PendingAction, 0, pageSize)
	for rows.Next() {
		act, err := scanPendingAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, act)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &domain.Page[PendingAction]{
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		List:     out,
	}, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanPendingAction(row rowScanner) (PendingAction, error) {
	var act PendingAction
	var argsJSON, resultJSON sql.NullString
	var approver sql.NullInt64
	var createdAt, decided, expiresAt sql.NullTime
	if err := row.Scan(&act.ID, &act.Tool, &act.IncidentID, &argsJSON, &act.Rationale, &act.Impact,
		&act.Status, &act.RequestedBy, &approver, &resultJSON, &createdAt, &decided, &expiresAt); err != nil {
		return act, err
	}
	act.CreatedAt = store.FormatDatabaseTime(createdAt)
	act.ExpiresAt = store.FormatDatabaseTime(expiresAt)
	fillAction(&act, argsJSON, resultJSON, approver, decided)
	return act, nil
}

func fillAction(act *PendingAction, argsJSON, resultJSON sql.NullString, approver sql.NullInt64, decided sql.NullTime) {
	act.Args = jsonMap(argsJSON.String)
	if resultJSON.Valid {
		var r any
		_ = json.Unmarshal([]byte(resultJSON.String), &r)
		act.Result = r
	}
	act.Approver = approver.Int64
	act.DecidedAt = store.FormatDatabaseTime(decided)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonMap(s string) map[string]any {
	m := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return m
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		log.Infof("[actions] malformed args json: %v", err)
	}
	return m
}

func newActionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "act_" + hex.EncodeToString(b[:])
}
