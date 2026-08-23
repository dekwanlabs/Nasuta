package investigation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type RunEvent struct {
	Sequence  int64     `json:"sequence"`
	RunID     string    `json:"run_id"`
	Type      string    `json:"type"`
	Status    RunStatus `json:"status,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RunStore is the durable boundary for one investigation snapshot and its events.
type RunStore interface {
	Create(InvestigationRun) error
	Get(string) (InvestigationRun, error)
	Transition(string, RunStatus) error
	SavePlan(string, PlanRevision) error
	SaveBudget(string, BudgetSnapshot) error
	SaveTask(string, ExecutableTask) error
	SaveResult(string, TaskExecutionRecord) error
	SaveEvidence(string, []EvidenceUnit) error
	SaveClaims(string, []VerifiedClaim) error
	SaveReport(string, InvestigationReport) error
	SaveMetrics(string, RunMetrics) error
	SaveDelivery(string, DeliveryResult) error
	Fail(string, RunFailure, RunStatus) error
	AppendEvent(string, string, string) error
	Events(string) ([]RunEvent, error)
}

// runMutation applies one invariant-checked change to a detached run snapshot and
// returns the next snapshot plus the event (if any) the change should record.
type runMutation func(InvestigationRun) (InvestigationRun, RunEvent, error)

func mutateCreate(run InvestigationRun) (InvestigationRun, RunEvent, error) {
	if run.ID == "" {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("run id is required")
	}
	if run.Status == "" {
		run.Status = RunCreated
	}
	if run.Status != RunCreated {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("new run must start in %q", RunCreated)
	}
	now := time.Now().UTC()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	if run.Tasks == nil {
		run.Tasks = make(map[string]ExecutableTask)
	}
	if run.Results == nil {
		run.Results = make(map[string]TaskExecutionRecord)
	}
	return cloneRun(run), RunEvent{Type: "run_created", Status: RunCreated, Message: "run created"}, nil
}

func mutateTransition(run InvestigationRun, next RunStatus) (InvestigationRun, RunEvent, error) {
	if run.Status.Terminal() {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: run %q is already %q", ErrTerminalRun, run.ID, run.Status)
	}
	if !allowedRunTransition(run.Status, next) {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: %q -> %q", ErrInvalidTransition, run.Status, next)
	}
	run.Status = next
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{Type: "run_status_changed", Status: next, Message: string(next)}, nil
}

func mutateSavePlan(run InvestigationRun, plan PlanRevision) (InvestigationRun, RunEvent, error) {
	if run.Status != RunPlanned && run.Status != RunExecuting && run.Status != RunVerifying {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: cannot save plan while run is %q", ErrInvalidTransition, run.Status)
	}
	run.Plan = clonePlan(plan)
	run.Tasks = make(map[string]ExecutableTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		run.Tasks[task.ID] = cloneTask(task)
	}
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{Type: "plan_saved", Status: run.Status, Message: fmt.Sprintf("revision %d", plan.Revision)}, nil
}

func mutateSaveBudget(run InvestigationRun, budget BudgetSnapshot) (InvestigationRun, RunEvent, error) {
	run.Budget = cloneBudgetSnapshot(budget)
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveTask(run InvestigationRun, task ExecutableTask) (InvestigationRun, RunEvent, error) {
	if _, exists := run.Tasks[task.ID]; !exists {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("task %q is not part of run %q", task.ID, run.ID)
	}
	if task.Status == "" {
		task.Status = run.Tasks[task.ID].Status
	}
	run.Tasks[task.ID] = cloneTask(task)
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveResult(run InvestigationRun, record TaskExecutionRecord) (InvestigationRun, RunEvent, error) {
	if _, exists := run.Tasks[record.TaskID]; !exists {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("task %q is not part of run %q", record.TaskID, run.ID)
	}
	if record.Status != TaskSucceeded && record.Status != TaskFailed && record.Status != TaskBlocked && record.Status != TaskCancelled {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("task %q result has non-terminal status %q", record.TaskID, record.Status)
	}
	if previous, exists := run.Results[record.TaskID]; exists && previous.Status != TaskRunning {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("task %q result is already terminal", record.TaskID)
	}
	run.Results[record.TaskID] = cloneTaskResult(record)
	task := run.Tasks[record.TaskID]
	task.Status = record.Status
	run.Tasks[record.TaskID] = task
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{Type: "task_result_saved", Status: run.Status, Message: record.TaskID}, nil
}

func mutateSaveEvidence(run InvestigationRun, units []EvidenceUnit) (InvestigationRun, RunEvent, error) {
	run.Evidence = cloneEvidenceUnits(units)
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveClaims(run InvestigationRun, claims []VerifiedClaim) (InvestigationRun, RunEvent, error) {
	run.Claims = cloneClaims(claims)
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveReport(run InvestigationRun, report InvestigationReport) (InvestigationRun, RunEvent, error) {
	run.Report = cloneReport(report)
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveMetrics(run InvestigationRun, metrics RunMetrics) (InvestigationRun, RunEvent, error) {
	run.Metrics = metrics
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{}, nil
}

func mutateSaveDelivery(run InvestigationRun, delivery DeliveryResult) (InvestigationRun, RunEvent, error) {
	if run.Status != RunComposing {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: cannot save delivery while run is %q", ErrInvalidTransition, run.Status)
	}
	if err := ValidateDelivery(delivery); err != nil {
		return InvestigationRun{}, RunEvent{}, err
	}
	run.Delivery = cloneDelivery(&delivery)
	run.Report = cloneReport(delivery.Report)
	run.Status = RunDelivered
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{Type: "delivery_saved", Status: RunDelivered, Message: string(delivery.Status)}, nil
}

func mutateFail(run InvestigationRun, failure RunFailure, status RunStatus) (InvestigationRun, RunEvent, error) {
	if status != RunFailed && status != RunCancelled && status != RunTimedOut && status != RunBudgetExhausted {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("invalid terminal failure status %q", status)
	}
	if run.Status.Terminal() {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: run %q is already %q", ErrTerminalRun, run.ID, run.Status)
	}
	if !allowedRunTransition(run.Status, status) {
		return InvestigationRun{}, RunEvent{}, fmt.Errorf("%w: %q -> %q", ErrInvalidTransition, run.Status, status)
	}
	run.Status = status
	run.Failure = &failure
	run.UpdatedAt = time.Now().UTC()
	return cloneRun(run), RunEvent{Type: "run_failed", Status: status, Message: failure.Message}, nil
}

func mutateAppendEvent(run InvestigationRun, eventType, message string) (InvestigationRun, RunEvent, error) {
	return cloneRun(run), RunEvent{Type: eventType, Message: message}, nil
}

type MemoryRunStore struct {
	mu     sync.RWMutex
	runs   map[string]InvestigationRun
	events map[string][]RunEvent
}

func NewMemoryRunStore() *MemoryRunStore {
	return &MemoryRunStore{
		runs:   make(map[string]InvestigationRun),
		events: make(map[string][]RunEvent),
	}
}

func (store *MemoryRunStore) Create(run InvestigationRun) error {
	if store == nil {
		return fmt.Errorf("run store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.runs[run.ID]; exists {
		return fmt.Errorf("run %q already exists", run.ID)
	}
	next, event, err := mutateCreate(run)
	if err != nil {
		return err
	}
	store.runs[run.ID] = next
	store.appendEventLocked(run.ID, event)
	return nil
}

func (store *MemoryRunStore) Get(id string) (InvestigationRun, error) {
	if store == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	run, ok := store.runs[id]
	if !ok {
		return InvestigationRun{}, fmt.Errorf("run %q not found", id)
	}
	return cloneRun(run), nil
}

func (store *MemoryRunStore) Transition(id string, next RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, next)
	})
}

func (store *MemoryRunStore) SavePlan(id string, plan PlanRevision) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSavePlan(run, plan)
	})
}

func (store *MemoryRunStore) SaveBudget(id string, budget BudgetSnapshot) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveBudget(run, budget)
	})
}

func (store *MemoryRunStore) SaveTask(id string, task ExecutableTask) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveTask(run, task)
	})
}

func (store *MemoryRunStore) SaveResult(id string, record TaskExecutionRecord) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveResult(run, record)
	})
}

func (store *MemoryRunStore) SaveEvidence(id string, units []EvidenceUnit) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveEvidence(run, units)
	})
}

func (store *MemoryRunStore) SaveClaims(id string, claims []VerifiedClaim) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveClaims(run, claims)
	})
}

func (store *MemoryRunStore) SaveReport(id string, report InvestigationReport) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveReport(run, report)
	})
}

func (store *MemoryRunStore) SaveMetrics(id string, metrics RunMetrics) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveMetrics(run, metrics)
	})
}

func (store *MemoryRunStore) SaveDelivery(id string, delivery DeliveryResult) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveDelivery(run, delivery)
	})
}

func (store *MemoryRunStore) Fail(id string, failure RunFailure, status RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateFail(run, failure, status)
	})
}

func (store *MemoryRunStore) AppendEvent(id, eventType, message string) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateAppendEvent(run, eventType, message)
	})
}

func (store *MemoryRunStore) Events(id string) ([]RunEvent, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := store.runs[id]; !ok {
		return nil, fmt.Errorf("run %q not found", id)
	}
	return append([]RunEvent(nil), store.events[id]...), nil
}

func (store *MemoryRunStore) apply(id string, mutate runMutation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	run, ok := store.runs[id]
	if !ok {
		return fmt.Errorf("run %q not found", id)
	}
	next, event, err := mutate(run)
	if err != nil {
		return err
	}
	store.runs[id] = next
	if event.Type != "" {
		store.appendEventLocked(id, event)
	}
	return nil
}

func (store *MemoryRunStore) appendEventLocked(id string, event RunEvent) {
	events := store.events[id]
	event.Sequence = int64(len(events) + 1)
	event.RunID = id
	event.CreatedAt = time.Now().UTC()
	store.events[id] = append(events, event)
}

// SQLiteRunStore keeps one JSON snapshot per run plus an append-only event log.
// It reuses the same mutation functions as MemoryRunStore so the lifecycle rules
// are enforced in exactly one place.
type SQLiteRunStore struct {
	mu sync.Mutex
	db *sql.DB
}

func NewSQLiteRunStore(db *sql.DB) (*SQLiteRunStore, error) {
	if db == nil {
		return nil, fmt.Errorf("sqlite run store: database is required")
	}
	const schema = `
CREATE TABLE IF NOT EXISTS investigation_runs (
    id TEXT PRIMARY KEY,
    payload TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS investigation_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    type TEXT NOT NULL,
    status TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS investigation_events_run_id ON investigation_events(run_id);
`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("sqlite run store: create schema: %w", err)
	}
	return &SQLiteRunStore{db: db}, nil
}

func (store *SQLiteRunStore) Create(run InvestigationRun) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("run store is required")
	}
	next, event, err := mutateCreate(run)
	if err != nil {
		return err
	}
	return store.persist(next, event, true)
}

func (store *SQLiteRunStore) Get(id string) (InvestigationRun, error) {
	if store == nil || store.db == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, err := store.loadPayload(id)
	if err != nil {
		return InvestigationRun{}, err
	}
	return cloneRun(payload), nil
}

func (store *SQLiteRunStore) Transition(id string, next RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, next)
	})
}

func (store *SQLiteRunStore) SavePlan(id string, plan PlanRevision) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSavePlan(run, plan)
	})
}

func (store *SQLiteRunStore) SaveBudget(id string, budget BudgetSnapshot) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveBudget(run, budget)
	})
}

func (store *SQLiteRunStore) SaveTask(id string, task ExecutableTask) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveTask(run, task)
	})
}

func (store *SQLiteRunStore) SaveResult(id string, record TaskExecutionRecord) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveResult(run, record)
	})
}

func (store *SQLiteRunStore) SaveEvidence(id string, units []EvidenceUnit) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveEvidence(run, units)
	})
}

func (store *SQLiteRunStore) SaveClaims(id string, claims []VerifiedClaim) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveClaims(run, claims)
	})
}

func (store *SQLiteRunStore) SaveReport(id string, report InvestigationReport) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveReport(run, report)
	})
}

func (store *SQLiteRunStore) SaveMetrics(id string, metrics RunMetrics) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveMetrics(run, metrics)
	})
}

func (store *SQLiteRunStore) SaveDelivery(id string, delivery DeliveryResult) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveDelivery(run, delivery)
	})
}

func (store *SQLiteRunStore) Fail(id string, failure RunFailure, status RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateFail(run, failure, status)
	})
}

func (store *SQLiteRunStore) AppendEvent(id, eventType, message string) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateAppendEvent(run, eventType, message)
	})
}

func (store *SQLiteRunStore) Events(id string) ([]RunEvent, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("run store is required")
	}
	rows, err := store.db.Query(
		`SELECT sequence, run_id, type, status, message, created_at FROM investigation_events WHERE run_id = ? ORDER BY sequence`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]RunEvent, 0)
	for rows.Next() {
		var event RunEvent
		var createdAt int64
		if err := rows.Scan(&event.Sequence, &event.RunID, &event.Type, &event.Status, &event.Message, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		if _, err := store.loadPayload(id); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (store *SQLiteRunStore) apply(id string, mutate runMutation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	run, err := store.loadPayload(id)
	if err != nil {
		return err
	}
	next, event, err := mutate(run)
	if err != nil {
		return err
	}
	return store.persistLocked(next, event, false)
}

func (store *SQLiteRunStore) loadPayload(id string) (InvestigationRun, error) {
	var payload string
	if err := store.db.QueryRow(`SELECT payload FROM investigation_runs WHERE id = ?`, id).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return InvestigationRun{}, fmt.Errorf("run %q not found", id)
		}
		return InvestigationRun{}, err
	}
	var run InvestigationRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return InvestigationRun{}, fmt.Errorf("decode run %q: %w", id, err)
	}
	return normalizeStoredRun(run), nil
}

func (store *SQLiteRunStore) persist(next InvestigationRun, event RunEvent, create bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.persistLocked(next, event, create)
}

func (store *SQLiteRunStore) persistLocked(next InvestigationRun, event RunEvent, create bool) error {
	payload, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode run %q: %w", next.ID, err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if create {
		if _, err := tx.Exec(`INSERT INTO investigation_runs (id, payload, updated_at) VALUES (?, ?, ?)`, next.ID, string(payload), next.UpdatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("insert run %q: %w", next.ID, err)
		}
	} else {
		if _, err := tx.Exec(`UPDATE investigation_runs SET payload = ?, updated_at = ? WHERE id = ?`, string(payload), next.UpdatedAt.UnixMilli(), next.ID); err != nil {
			return fmt.Errorf("update run %q: %w", next.ID, err)
		}
	}
	if event.Type != "" {
		if _, err := tx.Exec(
			`INSERT INTO investigation_events (run_id, type, status, message, created_at) VALUES (?, ?, ?, ?, ?)`,
			next.ID, event.Type, string(event.Status), event.Message, time.Now().UTC().UnixMilli(),
		); err != nil {
			return fmt.Errorf("insert run %q event: %w", next.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func normalizeStoredRun(run InvestigationRun) InvestigationRun {
	if run.Tasks == nil {
		run.Tasks = make(map[string]ExecutableTask)
	}
	if run.Results == nil {
		run.Results = make(map[string]TaskExecutionRecord)
	}
	run.Budget = cloneBudgetSnapshot(run.Budget)
	return run
}

func allowedRunTransition(current, next RunStatus) bool {
	switch current {
	case RunCreated:
		return next == RunAnalyzing || next == RunFailed || next == RunCancelled || next == RunTimedOut
	case RunAnalyzing:
		return next == RunPlanned || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	case RunPlanned:
		return next == RunExecuting || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	case RunExecuting:
		return next == RunVerifying || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	case RunVerifying:
		return next == RunReplanning || next == RunComposing || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	case RunReplanning:
		return next == RunPlanned || next == RunComposing || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	case RunComposing:
		return next == RunDelivered || next == RunFailed || next == RunCancelled || next == RunTimedOut || next == RunBudgetExhausted
	default:
		return false
	}
}

func cloneRun(run InvestigationRun) InvestigationRun {
	data, err := json.Marshal(run)
	if err != nil {
		return run
	}
	var cloned InvestigationRun
	if err := json.Unmarshal(data, &cloned); err != nil {
		return run
	}
	if cloned.Tasks == nil {
		cloned.Tasks = make(map[string]ExecutableTask)
	}
	if cloned.Results == nil {
		cloned.Results = make(map[string]TaskExecutionRecord)
	}
	cloned.Budget = cloneBudgetSnapshot(cloned.Budget)
	return cloned
}

func cloneBudgetSnapshot(snapshot BudgetSnapshot) BudgetSnapshot {
	stages := snapshot.Stages
	snapshot.Stages = make(map[BudgetStage]StageBudget, len(stages))
	for stage, budget := range stages {
		snapshot.Stages[stage] = budget
	}
	return snapshot
}

func clonePlan(plan PlanRevision) PlanRevision {
	data, err := json.Marshal(plan)
	if err != nil {
		return plan
	}
	var cloned PlanRevision
	if err := json.Unmarshal(data, &cloned); err != nil {
		return plan
	}
	return cloned
}

func cloneTask(task ExecutableTask) ExecutableTask {
	data, err := json.Marshal(task)
	if err != nil {
		return task
	}
	var cloned ExecutableTask
	if err := json.Unmarshal(data, &cloned); err != nil {
		return task
	}
	return cloned
}

func cloneTaskResult(record TaskExecutionRecord) TaskExecutionRecord {
	record.Output = append([]byte(nil), record.Output...)
	if record.Failure != nil {
		failure := *record.Failure
		record.Failure = &failure
	}
	record.Attempts = cloneTaskAttempts(record.Attempts)
	record.Discoveries = append([]Discovery(nil), record.Discoveries...)
	return record
}

func cloneEvidenceUnits(units []EvidenceUnit) []EvidenceUnit {
	cloned := make([]EvidenceUnit, len(units))
	for index, unit := range units {
		unit.Facets = append([]string(nil), unit.Facets...)
		cloned[index] = unit
	}
	return cloned
}

func cloneClaims(claims []VerifiedClaim) []VerifiedClaim {
	cloned := make([]VerifiedClaim, len(claims))
	for index, claim := range claims {
		claim.EvidenceRefs = cloneEvidenceRefs(claim.EvidenceRefs)
		claim.ConflictRefs = cloneEvidenceRefs(claim.ConflictRefs)
		cloned[index] = claim
	}
	return cloned
}

func cloneReport(report InvestigationReport) InvestigationReport {
	data, err := json.Marshal(report)
	if err != nil {
		return report
	}
	var cloned InvestigationReport
	if err := json.Unmarshal(data, &cloned); err != nil {
		return report
	}
	return cloned
}

func cloneDelivery(delivery *DeliveryResult) *DeliveryResult {
	if delivery == nil {
		return nil
	}
	cloned := *delivery
	cloned.Report = cloneReport(delivery.Report)
	if delivery.Failure != nil {
		failure := *delivery.Failure
		cloned.Failure = &failure
	}
	return &cloned
}

// fencingMutationStore makes the durable mutation and fencing check one
// database operation. In-memory stores still use the validator path below.
type fencingMutationStore interface {
	applyFenced(string, string, uint64, runMutation) error
	createFenced(InvestigationRun, string, uint64) error
}

// fencedRunStore rejects writes after the lease owner loses authority. Durable
// stores may additionally enforce the token in the same conditional update.
type fencedRunStore struct {
	base  RunStore
	lease LeaseStore
	runID string
	owner string
	token uint64
}

func bindLeaseRunStore(base RunStore, lease LeaseStore, runID, owner string, tokens ...uint64) RunStore {
	if base == nil || lease == nil || owner == "" {
		return base
	}
	if _, ok := lease.(LeaseValidator); !ok {
		return base
	}
	var token uint64
	if len(tokens) > 0 {
		token = tokens[0]
	}
	return &fencedRunStore{base: base, lease: lease, runID: runID, owner: owner, token: token}
}

func (store *fencedRunStore) check() error {
	if store.token > 0 {
		if validator, ok := store.lease.(FencingLeaseStore); ok {
			if err := validator.ValidateLeaseWithToken(context.Background(), store.runID, store.owner, store.token); err != nil {
				return fmt.Errorf("%w: run %q owner %q token %d: %v", ErrLeaseFenced, store.runID, store.owner, store.token, err)
			}
			return nil
		}
	}
	validator, ok := store.lease.(LeaseValidator)
	if !ok {
		return nil
	}
	if err := validator.ValidateLease(context.Background(), store.runID, store.owner); err != nil {
		return fmt.Errorf("%w: run %q owner %q: %v", ErrLeaseFenced, store.runID, store.owner, err)
	}
	return nil
}

func (store *fencedRunStore) apply(id string, mutation runMutation, fallback func() error) error {
	if err := store.check(); err != nil {
		return err
	}
	if store.token > 0 {
		if durable, ok := store.base.(fencingMutationStore); ok {
			return durable.applyFenced(id, store.owner, store.token, mutation)
		}
	}
	return fallback()
}

func (store *fencedRunStore) Create(run InvestigationRun) error {
	if err := store.check(); err != nil {
		return err
	}
	if store.token > 0 {
		if durable, ok := store.base.(fencingMutationStore); ok {
			return durable.createFenced(run, store.owner, store.token)
		}
	}
	return store.base.Create(run)
}
func (store *fencedRunStore) Get(id string) (InvestigationRun, error) { return store.base.Get(id) }
func (store *fencedRunStore) Transition(id string, next RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, next)
	}, func() error { return store.base.Transition(id, next) })
}
func (store *fencedRunStore) SavePlan(id string, plan PlanRevision) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSavePlan(run, plan)
	}, func() error { return store.base.SavePlan(id, plan) })
}
func (store *fencedRunStore) SaveBudget(id string, budget BudgetSnapshot) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveBudget(run, budget)
	}, func() error { return store.base.SaveBudget(id, budget) })
}
func (store *fencedRunStore) SaveTask(id string, task ExecutableTask) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveTask(run, task)
	}, func() error { return store.base.SaveTask(id, task) })
}
func (store *fencedRunStore) SaveResult(id string, result TaskExecutionRecord) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveResult(run, result)
	}, func() error { return store.base.SaveResult(id, result) })
}
func (store *fencedRunStore) SaveEvidence(id string, units []EvidenceUnit) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveEvidence(run, units)
	}, func() error { return store.base.SaveEvidence(id, units) })
}
func (store *fencedRunStore) SaveClaims(id string, claims []VerifiedClaim) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveClaims(run, claims)
	}, func() error { return store.base.SaveClaims(id, claims) })
}
func (store *fencedRunStore) SaveReport(id string, report InvestigationReport) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveReport(run, report)
	}, func() error { return store.base.SaveReport(id, report) })
}
func (store *fencedRunStore) SaveMetrics(id string, metrics RunMetrics) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveMetrics(run, metrics)
	}, func() error { return store.base.SaveMetrics(id, metrics) })
}
func (store *fencedRunStore) SaveDelivery(id string, delivery DeliveryResult) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveDelivery(run, delivery)
	}, func() error { return store.base.SaveDelivery(id, delivery) })
}
func (store *fencedRunStore) Fail(id string, failure RunFailure, status RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateFail(run, failure, status)
	}, func() error { return store.base.Fail(id, failure, status) })
}
func (store *fencedRunStore) AppendEvent(id, eventType, message string) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateAppendEvent(run, eventType, message)
	}, func() error { return store.base.AppendEvent(id, eventType, message) })
}
func (store *fencedRunStore) Events(id string) ([]RunEvent, error) { return store.base.Events(id) }
