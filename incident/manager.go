package incident

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/knowledge"
)

type Status string

const (
	StatusOpen      Status = "open"
	StatusAnalyzing Status = "analyzing"
	StatusFixing    Status = "fixing"
	StatusFixed     Status = "fixed"
	StatusClosed    Status = "closed"
)

// Config contains only Incident-owned runtime settings.
type Config struct {
	WebBaseURL          string
	NotifyFeishuWebhook string
	NotifyWecomWebhook  string
	NotifyHTTPWebhook   string
	FixDefaultAssignee  string
	FixBranchPrefix     string
	LLMBaseURL          string
	LLMAPIKey           string
	LLMModel            string
	LLMProvider         string
	LLMMaxTokens        int
	VCSURL              string
	VCSToken            string
}

type Incident struct {
	ID           string         `json:"id"`
	DedupKey     string         `json:"dedup_key,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Status       Status         `json:"status"`
	Source       string         `json:"source"`
	AlertTitle   string         `json:"alert_title"`
	AlertPayload map[string]any `json:"alert_payload,omitempty"`
	ErrorLogs    []LogHit       `json:"error_logs,omitempty"`
	Traces       []*TraceResult `json:"traces,omitempty"`
	AffectedSvcs []string       `json:"affected_svcs,omitempty"`
	RootCause    string         `json:"root_cause,omitempty"`
	Solution     string         `json:"solution,omitempty"`
	AnalysisDoc  string         `json:"analysis_doc,omitempty"`
	AssignedTo   string         `json:"assigned_to,omitempty"`
	FixBranches  []FixBranch    `json:"fix_branches,omitempty"`
	FixStartedAt *time.Time     `json:"fix_started_at,omitempty"`
	FixedAt      *time.Time     `json:"fixed_at,omitempty"`
}

type Manager struct {
	db            *sql.DB
	cfg           Config
	evidence      EvidenceProvider
	knowledge     knowledge.API
	workspaceRoot string
	llm           *llm.LLMClient
}

const incidentTable = "incident_records"

func NewManager(cfg Config, db *sql.DB, workspaceRoot string, evidence EvidenceProvider, knowledgeAPI knowledge.API) (*Manager, error) {
	if db == nil {
		return nil, fmt.Errorf("application database is required for incident persistence")
	}
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is required for incident fixes")
	}
	client := llm.NewLLMClientWithHTTPAndProvider(
		cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMProvider, cfg.LLMMaxTokens, nil,
	)
	return &Manager{
		db: db, cfg: cfg, evidence: evidence, knowledge: knowledgeAPI,
		workspaceRoot: workspaceRoot, llm: client,
	}, nil
}

func (manager *Manager) Close() error {
	// The application composition owns the shared pool.
	return nil
}

func (manager *Manager) CreateFromAlert(ctx context.Context, source string, alert AlertPayload) (*Incident, error) {
	now := time.Now()
	if alert.Title == "" {
		alert.Title = "Untitled alert"
	}
	svcs := servicesFromAlert(alert)
	for i, s := range svcs {
		clean := stripRegion(s)
		if resolved, _ := manager.repoForService(ctx, clean); resolved != s && resolved != "" {
			svcs[i] = resolved
		}
	}
	dedupKey := dedupKey(alert, svcs)
	if existing, err := manager.findOpenDedup(ctx, dedupKey); err == nil && existing != nil {
		return existing, nil
	}
	inc := &Incident{
		ID:           newID(now),
		DedupKey:     dedupKey,
		CreatedAt:    now,
		UpdatedAt:    now,
		Status:       StatusAnalyzing,
		Source:       source,
		AlertTitle:   alert.Title,
		AlertPayload: alert.Raw,
		AffectedSvcs: svcs,
	}
	if len(inc.AffectedSvcs) == 0 {
		inc.AffectedSvcs = []string{}
	}
	if err := manager.save(ctx, inc); err != nil {
		return nil, err
	}
	return inc, nil
}

func (manager *Manager) List(ctx context.Context) ([]Incident, error) {
	rows, err := manager.db.QueryContext(ctx, `
SELECT id,dedup_key,created_at,updated_at,status,source,alert_title,affected_svcs_json,
       root_cause,solution,assigned_to,fix_branches_json,fix_started_at,fixed_at
FROM `+incidentTable+` ORDER BY created_unix DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	defer rows.Close()
	out := make([]Incident, 0, 200)
	for rows.Next() {
		var inc Incident
		var dedup, services, rootCause, solution, assignedTo, branches sql.NullString
		var created, updated, fixStarted, fixedAt sql.NullTime
		if err := rows.Scan(&inc.ID, &dedup, &created, &updated, &inc.Status, &inc.Source, &inc.AlertTitle,
			&services, &rootCause, &solution, &assignedTo, &branches, &fixStarted, &fixedAt); err != nil {
			return nil, fmt.Errorf("scan incident list row: %w", err)
		}
		inc.DedupKey = dedup.String
		inc.CreatedAt = created.Time
		inc.UpdatedAt = updated.Time
		inc.RootCause = rootCause.String
		inc.Solution = solution.String
		inc.AssignedTo = assignedTo.String
		decodeJSON(services.String, &inc.AffectedSvcs)
		decodeJSON(branches.String, &inc.FixBranches)
		if inc.AffectedSvcs == nil {
			inc.AffectedSvcs = []string{}
		}
		if inc.FixBranches == nil {
			inc.FixBranches = []FixBranch{}
		}
		if fixStarted.Valid {
			t := fixStarted.Time
			inc.FixStartedAt = &t
		}
		if fixedAt.Valid {
			t := fixedAt.Time
			inc.FixedAt = &t
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate incident list: %w", err)
	}
	return out, nil
}

func (manager *Manager) findOpenDedup(ctx context.Context, key string) (*Incident, error) {
	if key == "" {
		return nil, nil
	}
	row := manager.db.QueryRowContext(ctx, `
SELECT id FROM `+incidentTable+`
WHERE dedup_key=? AND status NOT IN ('fixed','closed')
ORDER BY created_unix DESC LIMIT 1`, key)
	var id string
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return manager.Get(ctx, id)
}

func (manager *Manager) Get(ctx context.Context, id string) (*Incident, error) {
	row := manager.db.QueryRowContext(ctx, `
SELECT id,dedup_key,created_at,updated_at,status,source,alert_title,alert_payload_json,error_logs_json,traces_json,
       affected_svcs_json,root_cause,solution,analysis_doc,assigned_to,fix_branches_json,fix_started_at,fixed_at
FROM `+incidentTable+` WHERE id=?`, id)
	var inc Incident
	var dedup, payload, logs, traces, svcs, rootCause, solution, analysisDoc, assignedTo, branches sql.NullString
	var created, updated, fixStarted, fixedAt sql.NullTime
	if err := row.Scan(&inc.ID, &dedup, &created, &updated, &inc.Status, &inc.Source, &inc.AlertTitle, &payload,
		&logs, &traces, &svcs, &rootCause, &solution, &analysisDoc, &assignedTo, &branches, &fixStarted, &fixedAt); err != nil {
		return nil, err
	}
	inc.DedupKey = dedup.String
	if created.Valid {
		inc.CreatedAt = created.Time
	}
	if updated.Valid {
		inc.UpdatedAt = updated.Time
	}
	inc.RootCause = rootCause.String
	inc.Solution = solution.String
	inc.AnalysisDoc = analysisDoc.String
	inc.AssignedTo = assignedTo.String
	decodeJSON(payload.String, &inc.AlertPayload)
	decodeJSON(logs.String, &inc.ErrorLogs)
	decodeJSON(traces.String, &inc.Traces)
	decodeJSON(svcs.String, &inc.AffectedSvcs)
	decodeJSON(branches.String, &inc.FixBranches)
	normalizeIncident(&inc)
	if fixStarted.Valid {
		t := fixStarted.Time
		inc.FixStartedAt = &t
	}
	if fixedAt.Valid {
		t := fixedAt.Time
		inc.FixedAt = &t
	}
	return &inc, nil
}

func normalizeIncident(inc *Incident) {
	if inc.AlertPayload == nil {
		inc.AlertPayload = map[string]any{}
	}
	if inc.ErrorLogs == nil {
		inc.ErrorLogs = []LogHit{}
	}
	if inc.Traces == nil {
		inc.Traces = []*TraceResult{}
	}
	if inc.AffectedSvcs == nil {
		inc.AffectedSvcs = []string{}
	}
	if inc.FixBranches == nil {
		inc.FixBranches = []FixBranch{}
	}
}

func (manager *Manager) save(ctx context.Context, inc *Incident) error {
	inc.UpdatedAt = time.Now()
	payload := mustJSON(inc.AlertPayload)
	logs := mustJSON(inc.ErrorLogs)
	traces := mustJSON(inc.Traces)
	svcs := mustJSON(inc.AffectedSvcs)
	branches := mustJSON(inc.FixBranches)
	fixStarted := timeStringPtr(inc.FixStartedAt)
	fixedAt := timeStringPtr(inc.FixedAt)
	_, err := manager.db.ExecContext(ctx, `
INSERT INTO `+incidentTable+`(id,dedup_key,created_at,updated_at,status,source,alert_title,alert_payload_json,error_logs_json,traces_json,
  affected_svcs_json,root_cause,solution,analysis_doc,assigned_to,fix_branches_json,fix_started_at,fixed_at,created_unix,updated_unix)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE
  dedup_key=VALUES(dedup_key),updated_at=VALUES(updated_at),status=VALUES(status),source=VALUES(source),alert_title=VALUES(alert_title),
  alert_payload_json=VALUES(alert_payload_json),error_logs_json=VALUES(error_logs_json),traces_json=VALUES(traces_json),
  affected_svcs_json=VALUES(affected_svcs_json),root_cause=VALUES(root_cause),solution=VALUES(solution),
  analysis_doc=VALUES(analysis_doc),assigned_to=VALUES(assigned_to),fix_branches_json=VALUES(fix_branches_json),
  fix_started_at=VALUES(fix_started_at),fixed_at=VALUES(fixed_at),updated_unix=VALUES(updated_unix)`,
		inc.ID, inc.DedupKey, inc.CreatedAt, inc.UpdatedAt, inc.Status, inc.Source,
		inc.AlertTitle, payload, logs, traces, svcs, inc.RootCause, inc.Solution, inc.AnalysisDoc, inc.AssignedTo,
		branches, fixStarted, fixedAt, inc.CreatedAt.Unix(), inc.UpdatedAt.Unix())
	return err
}

func (manager *Manager) SaveErrorLogs(ctx context.Context, inc *Incident) error {
	logsJSON := mustJSON(inc.ErrorLogs)
	_, err := manager.db.ExecContext(ctx,
		`UPDATE `+incidentTable+` SET error_logs_json = ?, updated_unix = ? WHERE id = ?`,
		logsJSON, time.Now().Unix(), inc.ID)
	return err
}

func (manager *Manager) Delete(ctx context.Context, id string) error {
	_, err := manager.db.ExecContext(ctx, `DELETE FROM `+incidentTable+` WHERE id = ?`, id)
	return err
}

func (manager *Manager) repoForService(ctx context.Context, service string) (resolvedName string, repo string) {
	if manager.knowledge == nil || service == "" || service == "unknown" {
		return service, service
	}
	service = stripRegion(service)
	result, err := manager.knowledge.SearchServices(ctx, knowledge.ServiceSearchQuery{Query: service, Limit: 1})
	if err == nil && len(result.Matches) > 0 {
		match := result.Matches[0]
		name := match.ServiceName
		r := match.Repo
		if name != "" {
			if r == "" {
				r = name
			}
			return name, r
		}
	}
	return service, service
}

func (manager *Manager) filesHint(ctx context.Context, service string, inc *Incident) []string {
	if manager.knowledge == nil {
		return nil
	}
	query := strings.TrimSpace(service + " " + inc.RootCause + " " + inc.AlertTitle)
	result, err := manager.knowledge.SearchCode(ctx, knowledge.CodeSearchQuery{Query: query, Limit: 5})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(result.Matches))
	for _, match := range result.Matches {
		if match.Path != "" {
			out = append(out, match.Path)
		}
	}
	return out
}

func newID(t time.Time) string {
	return fmt.Sprintf("INC-%s-%d", t.Format("20060102-150405"), t.UnixNano()%100000)
}

var (
	nonSlug      = regexp.MustCompile(`[^a-z0-9]+`)
	regionSuffix = regexp.MustCompile(`-(?:pro|dev|stg|test|uat|gray|canary)-[a-z]+-[a-z]+-\d+$`)
)

func slug(s string) string {
	s = strings.ToLower(s)
	s = nonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	parts := strings.Split(s, "-")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, "-")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeJSON(s string, v any) {
	if strings.TrimSpace(s) != "" {
		_ = json.Unmarshal([]byte(s), v)
	}
}

func timeStringPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func parseTime(s string) time.Time {
	if strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func strAny(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return x.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func floatAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func mergeStrings(a, b []string) []string {
	return unique(append(append([]string{}, a...), b...))
}

func sanitizeOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
