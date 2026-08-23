package investigation

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BudgetVector is the single accounting unit shared by run, stage, and task limits.
type BudgetVector struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// TotalTokens is an aggregate cap; usage falls back to input+output when
	// an executor reports only the component counters.
	TotalTokens int64         `json:"total_tokens,omitempty"`
	ToolCalls   int           `json:"tool_calls,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"`
	CostMicros  int64         `json:"cost_micros,omitempty"`
}

type BudgetStage string

const (
	StagePlanning     BudgetStage = "planning"
	StageExecution    BudgetStage = "execution"
	StageVerification BudgetStage = "verification"
	StageComposition  BudgetStage = "composition"
	StageFallback     BudgetStage = "fallback"
)

type RunBudget struct {
	Limit         BudgetVector `json:"limit"`
	Reserved      BudgetVector `json:"reserved"`
	Used          BudgetVector `json:"used"`
	MaxRounds     int          `json:"max_rounds,omitempty"`
	MaxTasks      int          `json:"max_tasks,omitempty"`
	PolicyVersion string       `json:"policy_version,omitempty"`
	Profile       string       `json:"profile,omitempty"`
}

type StageBudget struct {
	Limit    BudgetVector `json:"limit"`
	Reserved BudgetVector `json:"reserved"`
	Used     BudgetVector `json:"used"`
}

type BudgetSnapshot struct {
	Run    RunBudget                   `json:"run"`
	Stages map[BudgetStage]StageBudget `json:"stages"`
}

type budgetReservation struct {
	id    string
	stage BudgetStage
	grant BudgetVector
}

// BudgetReservation represents an atomic grant. It must be settled or released exactly once.
type BudgetReservation struct {
	ledger *BudgetLedger
	ID     string
	Grant  BudgetVector
}

func (reservation BudgetReservation) Settle(actual BudgetVector) error {
	if reservation.ledger == nil || reservation.ID == "" {
		return fmt.Errorf("budget reservation is invalid")
	}
	return reservation.ledger.settle(reservation.ID, actual)
}

func (reservation BudgetReservation) Release() error {
	if reservation.ledger == nil || reservation.ID == "" {
		return fmt.Errorf("budget reservation is invalid")
	}
	return reservation.ledger.release(reservation.ID)
}

type BudgetLedger struct {
	mu           sync.Mutex
	run          RunBudget
	stages       map[BudgetStage]StageBudget
	reservations map[string]budgetReservation
	nextID       atomic.Uint64
}

func NewBudgetLedger(limit BudgetVector) (*BudgetLedger, error) {
	if err := validateBudgetVector(limit); err != nil {
		return nil, fmt.Errorf("create budget ledger: %w", err)
	}
	return &BudgetLedger{
		run:          RunBudget{Limit: limit},
		stages:       make(map[BudgetStage]StageBudget),
		reservations: make(map[string]budgetReservation),
	}, nil
}

// Restore replaces the live accounting state with a previously persisted
// snapshot. Reservations are intentionally dropped because they belong to a
// dead process; only used and stage limits survive.
func (ledger *BudgetLedger) Restore(snapshot BudgetSnapshot) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if err := validateBudgetVector(snapshot.Run.Limit); err != nil {
		return fmt.Errorf("restore run budget: %w", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.run = snapshot.Run
	ledger.stages = make(map[BudgetStage]StageBudget, len(snapshot.Stages))
	for stage, budget := range snapshot.Stages {
		ledger.stages[stage] = budget
	}
	ledger.reservations = make(map[string]budgetReservation)
	return nil
}

// SetRunPolicy freezes the run-level controls that are not part of the vector
// limit. They are part of the snapshot so a resumed run keeps the same rules.
func (ledger *BudgetLedger) SetRunPolicy(maxRounds, maxTasks int, policyVersion string, profile BudgetProfile) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if maxRounds < 0 || maxTasks < 0 {
		return fmt.Errorf("budget rounds and tasks cannot be negative")
	}
	if strings.TrimSpace(policyVersion) == "" {
		return fmt.Errorf("budget policy version is required")
	}
	if _, err := profile.Allocation(); err != nil {
		return fmt.Errorf("budget profile: %w", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.run.MaxRounds = maxRounds
	ledger.run.MaxTasks = maxTasks
	ledger.run.PolicyVersion = policyVersion
	ledger.run.Profile = string(profile)
	return nil
}

func (ledger *BudgetLedger) SetStageLimit(stage BudgetStage, limit BudgetVector) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if stage == "" {
		return fmt.Errorf("budget stage is required")
	}
	if err := validateBudgetVector(limit); err != nil {
		return fmt.Errorf("stage %q budget: %w", stage, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	current := ledger.stages[stage]
	if !fits(current.Used, limit) || !fits(current.Reserved, limit) {
		return fmt.Errorf("stage %q budget is below already accounted usage", stage)
	}
	current.Limit = limit
	ledger.stages[stage] = current
	return nil
}

func (ledger *BudgetLedger) CanReserve(stage BudgetStage, id string, grant BudgetVector) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if stage == "" || id == "" {
		return fmt.Errorf("budget stage and reservation id are required")
	}
	if err := validateBudgetVector(grant); err != nil {
		return fmt.Errorf("reservation %q: %w", id, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.canReserveLocked(stage, id, grant)
}

func (ledger *BudgetLedger) Reserve(stage BudgetStage, id string, grant BudgetVector) (BudgetReservation, error) {
	if err := ledger.CanReserve(stage, id, grant); err != nil {
		return BudgetReservation{}, err
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if err := ledger.canReserveLocked(stage, id, grant); err != nil {
		return BudgetReservation{}, err
	}
	reservationID := fmt.Sprintf("reservation_%d", ledger.nextID.Add(1))
	ledger.reservations[reservationID] = budgetReservation{
		id: reservationID, stage: stage, grant: grant,
	}
	ledger.run.Reserved = addVector(ledger.run.Reserved, grant)
	stageBudget := ledger.stageLocked(stage)
	stageBudget.Reserved = addVector(stageBudget.Reserved, grant)
	ledger.stages[stage] = stageBudget
	return BudgetReservation{ledger: ledger, ID: reservationID, Grant: grant}, nil
}

// CanReserveRun checks a grant against the shared run limit only. It is used by
// plan admission to verify that non-execution overhead still fits after the hard
// composition reserve, without attributing that overhead to the execution stage.
func (ledger *BudgetLedger) CanReserveRun(id string, grant BudgetVector) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if id == "" {
		return fmt.Errorf("reservation id is required")
	}
	if err := validateBudgetVector(grant); err != nil {
		return fmt.Errorf("reservation %q: %w", id, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, exists := ledger.reservations[id]; exists {
		return fmt.Errorf("reservation %q already exists", id)
	}
	if !fits(addVector(addVector(ledger.run.Used, ledger.run.Reserved), grant), ledger.run.Limit) {
		return fmt.Errorf("%w: run reservation %q exceeds run limit", ErrBudgetExceeded, id)
	}
	return nil
}

// CapStageGrant lowers a grant so it fits the stage limit, leaving unbounded
// dimensions untouched. It prevents the composition reserve from being rejected
// by a profile's stage share that is smaller than the configured composer budget.
func (ledger *BudgetLedger) CapStageGrant(stage BudgetStage, grant BudgetVector) BudgetVector {
	if ledger == nil {
		return grant
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return capBudget(grant, ledger.stageLocked(stage).Limit)
}

func (ledger *BudgetLedger) Available(stage BudgetStage) BudgetVector {
	if ledger == nil {
		return BudgetVector{}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	stageBudget := ledger.stageLocked(stage)
	return subtractVector(stageBudget.Limit, addVector(stageBudget.Used, stageBudget.Reserved))
}

func (ledger *BudgetLedger) Snapshot() BudgetSnapshot {
	if ledger == nil {
		return BudgetSnapshot{}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	stages := make(map[BudgetStage]StageBudget, len(ledger.stages))
	for stage, budget := range ledger.stages {
		stages[stage] = budget
	}
	return BudgetSnapshot{Run: ledger.run, Stages: stages}
}

func (ledger *BudgetLedger) canReserveLocked(stage BudgetStage, id string, grant BudgetVector) error {
	if _, exists := ledger.reservations[id]; exists {
		return fmt.Errorf("reservation %q already exists", id)
	}
	// The requested grant must fit alongside both settled usage and other
	// outstanding reservations. Checking only the existing accounting would
	// allow concurrent callers to reserve past the run limit.
	if !fits(addVector(addVector(ledger.run.Used, ledger.run.Reserved), grant), ledger.run.Limit) {
		return fmt.Errorf("%w: run reservation %q exceeds run limit", ErrBudgetExceeded, id)
	}
	stageBudget := ledger.stageLocked(stage)
	if !fits(addVector(addVector(stageBudget.Used, stageBudget.Reserved), grant), stageBudget.Limit) {
		return fmt.Errorf("%w: stage %q reservation %q exceeds stage limit", ErrBudgetExceeded, stage, id)
	}
	return nil
}

func (ledger *BudgetLedger) settle(id string, actual BudgetVector) error {
	if err := validateBudgetVector(actual); err != nil {
		return fmt.Errorf("settle reservation %q: %w", id, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	if !fitsGrant(actual, reservation.grant) {
		return fmt.Errorf("%w: actual usage exceeds reservation %q", ErrBudgetExceeded, id)
	}
	// A zero grant dimension means the template did not publish a per-task
	// ceiling. It is not permission to exceed the run or stage hard limit.
	// Check the settled usage while the ledger lock is held, including grants
	// still reserved by sibling tasks.
	projectedRun := addVector(addVector(subtractVector(ledger.run.Reserved, reservation.grant), ledger.run.Used), actual)
	if !fits(projectedRun, ledger.run.Limit) {
		return fmt.Errorf("%w: actual usage exceeds run limit for reservation %q", ErrBudgetExceeded, id)
	}
	stageBudget := ledger.stageLocked(reservation.stage)
	projectedStage := addVector(addVector(subtractVector(stageBudget.Reserved, reservation.grant), stageBudget.Used), actual)
	if !fits(projectedStage, stageBudget.Limit) {
		return fmt.Errorf("%w: actual usage exceeds stage %q limit for reservation %q", ErrBudgetExceeded, reservation.stage, id)
	}
	delete(ledger.reservations, id)
	ledger.run.Reserved = subtractVector(ledger.run.Reserved, reservation.grant)
	ledger.run.Used = addVector(ledger.run.Used, actual)
	stageBudget.Reserved = subtractVector(stageBudget.Reserved, reservation.grant)
	stageBudget.Used = addVector(stageBudget.Used, actual)
	ledger.stages[reservation.stage] = stageBudget
	return nil
}

func (ledger *BudgetLedger) release(id string) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	delete(ledger.reservations, id)
	ledger.run.Reserved = subtractVector(ledger.run.Reserved, reservation.grant)
	stageBudget := ledger.stageLocked(reservation.stage)
	stageBudget.Reserved = subtractVector(stageBudget.Reserved, reservation.grant)
	ledger.stages[reservation.stage] = stageBudget
	return nil
}

func (ledger *BudgetLedger) stageLocked(stage BudgetStage) StageBudget {
	if budget, ok := ledger.stages[stage]; ok {
		return budget
	}
	return StageBudget{Limit: ledger.run.Limit}
}

func validateBudgetVector(value BudgetVector) error {
	if value.InputTokens < 0 || value.OutputTokens < 0 || value.TotalTokens < 0 || value.ToolCalls < 0 ||
		value.Duration < 0 || value.CostMicros < 0 {
		return fmt.Errorf("budget values cannot be negative")
	}
	return nil
}

func fits(used, limit BudgetVector, limits ...BudgetVector) bool {
	if len(limits) > 0 {
		limit = limits[0]
	}
	return within(used.InputTokens, limit.InputTokens) &&
		within(used.OutputTokens, limit.OutputTokens) &&
		withinTotalTokens(used, limit) &&
		withinInt(used.ToolCalls, limit.ToolCalls) &&
		withinDuration(used.Duration, limit.Duration) &&
		withinCost(used.CostMicros, limit.CostMicros)
}

func fitsGrant(used, grant BudgetVector) bool {
	// A zero grant means that the template did not specify a per-task bound.
	// The run/stage limit check in settle still enforces the hard platform cap.
	return withinGrant(used.InputTokens, grant.InputTokens) &&
		withinGrant(used.OutputTokens, grant.OutputTokens) &&
		withinGrantTotalTokens(used, grant) &&
		withinGrantInt(used.ToolCalls, grant.ToolCalls) &&
		withinGrantDuration(used.Duration, grant.Duration) &&
		(grant.CostMicros == 0 || used.CostMicros <= grant.CostMicros)
}

func withinGrant(value, limit int64) bool {
	return limit == 0 || value <= limit
}

func withinGrantInt(value, limit int) bool {
	return limit == 0 || value <= limit
}

func withinGrantDuration(value, limit time.Duration) bool {
	return limit == 0 || value <= limit
}

func withinGrantTotalTokens(value, limit BudgetVector) bool {
	if limit.TotalTokens == 0 {
		return true
	}
	total := value.TotalTokens
	if total == 0 {
		total = saturatingAdd(value.InputTokens, value.OutputTokens)
	}
	return total <= limit.TotalTokens
}

func withinTotalTokens(value, limit BudgetVector) bool {
	if limit.TotalTokens == 0 {
		return true
	}
	total := value.TotalTokens
	if total == 0 {
		total = saturatingAdd(value.InputTokens, value.OutputTokens)
	}
	return total <= limit.TotalTokens
}

func within(value, limit int64) bool {
	return limit > 0 && value <= limit || limit == 0 && value == 0
}

func withinInt(value, limit int) bool {
	return limit > 0 && value <= limit || limit == 0 && value == 0
}

func withinCost(value, limit int64) bool {
	return limit == 0 || value <= limit
}

func withinDuration(value, limit time.Duration) bool {
	return limit > 0 && value <= limit || limit == 0 && value == 0
}

func addVector(left, right BudgetVector) BudgetVector {
	return BudgetVector{
		InputTokens:  saturatingAdd(left.InputTokens, right.InputTokens),
		OutputTokens: saturatingAdd(left.OutputTokens, right.OutputTokens),
		TotalTokens:  saturatingAdd(left.TotalTokens, right.TotalTokens),
		ToolCalls:    saturatingAddInt(left.ToolCalls, right.ToolCalls),
		Duration:     saturatingAddDuration(left.Duration, right.Duration),
		CostMicros:   saturatingAdd(left.CostMicros, right.CostMicros),
	}
}

func subtractVector(left, right BudgetVector) BudgetVector {
	return BudgetVector{
		InputTokens:  maxInt64(0, left.InputTokens-right.InputTokens),
		OutputTokens: maxInt64(0, left.OutputTokens-right.OutputTokens),
		TotalTokens:  maxInt64(0, left.TotalTokens-right.TotalTokens),
		ToolCalls:    maxInt(0, left.ToolCalls-right.ToolCalls),
		Duration:     maxDuration(0, left.Duration-right.Duration),
		CostMicros:   maxInt64(0, left.CostMicros-right.CostMicros),
	}
}

// capBudget lowers each grant dimension that exceeds a positive stage limit. A
// zero limit means unbounded for that dimension and leaves the grant untouched.
func capBudget(grant, limit BudgetVector) BudgetVector {
	out := grant
	if limit.InputTokens > 0 && out.InputTokens > limit.InputTokens {
		out.InputTokens = limit.InputTokens
	}
	if limit.OutputTokens > 0 && out.OutputTokens > limit.OutputTokens {
		out.OutputTokens = limit.OutputTokens
	}
	if limit.TotalTokens > 0 && out.TotalTokens > limit.TotalTokens {
		out.TotalTokens = limit.TotalTokens
	}
	if limit.ToolCalls > 0 && out.ToolCalls > limit.ToolCalls {
		out.ToolCalls = limit.ToolCalls
	}
	if limit.Duration > 0 && out.Duration > limit.Duration {
		out.Duration = limit.Duration
	}
	if limit.CostMicros > 0 && out.CostMicros > limit.CostMicros {
		out.CostMicros = limit.CostMicros
	}
	return out
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func saturatingAddInt(left, right int) int {
	if right > 0 && left > int(^uint(0)>>1)-right {
		return int(^uint(0) >> 1)
	}
	return left + right
}

func saturatingAddDuration(left, right time.Duration) time.Duration {
	if right > 0 && left > time.Duration(math.MaxInt64)-right {
		return time.Duration(math.MaxInt64)
	}
	return left + right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}
