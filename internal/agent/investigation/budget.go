package investigation

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
	// calls is cumulative in-flight model usage: pending calls contribute their
	// estimates and settled calls contribute actual usage until the task settles.
	calls BudgetVector
	// soft marks an admission reservation: the grant is a concurrency floor,
	// not a hard per-task ceiling. Settle charges actual usage to the shared
	// run/stage limits without rejecting usage that exceeds the grant.
	soft    bool
	pending map[string]BudgetVector
}

// BudgetReservation represents an atomic grant. It must be settled or released exactly once.
type BudgetReservation struct {
	ledger *BudgetLedger
	ID     string
	Grant  BudgetVector
}

type budgetCallReservation struct {
	ledger        *BudgetLedger
	reservationID string
	callID        string
}

type reservationBudgetGate struct {
	ledger *BudgetLedger
	id     string
}

func (gate reservationBudgetGate) Check() error {
	if gate.ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	return gate.ledger.checkReservation(gate.id)
}

func (gate reservationBudgetGate) ReserveCall(usage agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	if gate.ledger == nil {
		return nil, fmt.Errorf("budget ledger is required")
	}
	reservation, err := gate.ledger.ReserveCall(gate.id, budgetVectorFromUsage(usage))
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

func (gate reservationBudgetGate) Available() agentapi.Usage {
	if gate.ledger == nil {
		return agentapi.Usage{}
	}
	return budgetUsageFromVector(gate.ledger.AvailableForReservation(gate.id))
}

func (gate reservationBudgetGate) MinimumOutputTokens() int64 {
	if gate.ledger == nil {
		return 0
	}
	return gate.ledger.MinimumOutputForReservation(gate.id)
}

func (reservation budgetCallReservation) Settle(usage agentapi.Usage) error {
	if reservation.ledger == nil || reservation.reservationID == "" || reservation.callID == "" {
		return fmt.Errorf("budget call reservation is invalid")
	}
	return reservation.ledger.settleCall(
		reservation.reservationID,
		reservation.callID,
		budgetVectorFromUsage(usage),
	)
}

func (reservation budgetCallReservation) Release() error {
	if reservation.ledger == nil || reservation.reservationID == "" || reservation.callID == "" {
		return fmt.Errorf("budget call reservation is invalid")
	}
	return reservation.ledger.releaseCall(reservation.reservationID, reservation.callID)
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
	ledger.run.Reserved = BudgetVector{}
	ledger.stages = make(map[BudgetStage]StageBudget, len(snapshot.Stages))
	for stage, budget := range snapshot.Stages {
		budget.Reserved = BudgetVector{}
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

// SetRunLimit raises (or tightens) the shared run hard limit after planning has
// determined how many Agent calls this run actually contains. It never resets
// settled usage or outstanding reservations.
func (ledger *BudgetLedger) SetRunLimit(limit BudgetVector) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if err := validateBudgetVector(limit); err != nil {
		return fmt.Errorf("run budget: %w", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if !fits(ledger.run.Used, limit) || !fits(ledger.run.Reserved, limit) {
		return fmt.Errorf("run budget is below already accounted usage")
	}
	ledger.run.Limit = limit
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

func (ledger *BudgetLedger) reallocateAvailable(from, to BudgetStage) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	if from == "" || to == "" || from == to {
		return fmt.Errorf("distinct budget stages are required")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	fromBudget := ledger.stageLocked(from)
	toBudget := ledger.stageLocked(to)
	fromBudget.Limit, toBudget.Limit = transferAvailableBudget(
		fromBudget.Limit,
		toBudget.Limit,
		fromBudget.Used,
		fromBudget.Reserved,
	)
	ledger.stages[from] = fromBudget
	ledger.stages[to] = toBudget
	return nil
}

func transferAvailableBudget(
	from, to, used, reserved BudgetVector,
) (BudgetVector, BudgetVector) {
	from.InputTokens, to.InputTokens = transferAvailableLimit(
		from.InputTokens, to.InputTokens, used.InputTokens, reserved.InputTokens,
	)
	from.OutputTokens, to.OutputTokens = transferAvailableLimit(
		from.OutputTokens, to.OutputTokens, used.OutputTokens, reserved.OutputTokens,
	)
	from.TotalTokens, to.TotalTokens = transferAvailableLimit(
		from.TotalTokens, to.TotalTokens, used.TotalTokens, reserved.TotalTokens,
	)
	from.ToolCalls, to.ToolCalls = transferAvailableIntLimit(
		from.ToolCalls, to.ToolCalls, used.ToolCalls, reserved.ToolCalls,
	)
	from.Duration, to.Duration = transferAvailableDurationLimit(
		from.Duration, to.Duration, used.Duration, reserved.Duration,
	)
	from.CostMicros, to.CostMicros = transferAvailableLimit(
		from.CostMicros, to.CostMicros, used.CostMicros, reserved.CostMicros,
	)
	return from, to
}

func transferAvailableLimit(from, to, used, reserved int64) (int64, int64) {
	if from == 0 {
		return 0, to
	}
	protected := saturatingAdd(used, reserved)
	if protected >= from {
		return from, to
	}
	available := from - protected
	if to == 0 {
		return protected, 0
	}
	return protected, saturatingAdd(to, available)
}

func transferAvailableIntLimit(from, to, used, reserved int) (int, int) {
	remaining, expanded := transferAvailableLimit(
		int64(from), int64(to), int64(used), int64(reserved),
	)
	return int(remaining), int(expanded)
}

func transferAvailableDurationLimit(
	from, to, used, reserved time.Duration,
) (time.Duration, time.Duration) {
	remaining, expanded := transferAvailableLimit(
		int64(from), int64(to), int64(used), int64(reserved),
	)
	return time.Duration(remaining), time.Duration(expanded)
}

func (ledger *BudgetLedger) CanReserve(stage BudgetStage, id string, grant BudgetVector) error {
	return ledger.canReserve(stage, id, grant, false)
}

func (ledger *BudgetLedger) Reserve(stage BudgetStage, id string, grant BudgetVector) (BudgetReservation, error) {
	return ledger.reserve(stage, id, grant, false)
}

// ReserveAdmission reserves a soft admission ceiling for a task whose real
// limit is the shared Run budget rather than a per-task quota. The grant still
// participates in run/stage reservation accounting so concurrent siblings
// cannot silently overcommit, but settling actual usage above the grant is
// allowed as long as the shared limits still fit.
func (ledger *BudgetLedger) ReserveAdmission(stage BudgetStage, id string, grant BudgetVector) (BudgetReservation, error) {
	return ledger.reserve(stage, id, grant, true)
}

// BudgetAdmission is one task's protected minimum in an atomic parallel-group
// admission. TaskID is only an ownership key; the ledger assigns reservation IDs.
type BudgetAdmission struct {
	TaskID string
	Grant  BudgetVector
}

// ReserveAdmissionGroup atomically protects all requested task minimums. A
// failed group leaves the ledger unchanged so the scheduler can reduce the
// batch and retry without first-wins overbooking. These normal admissions are
// soft: their grant protects capacity but does not become a private task quota.
func (ledger *BudgetLedger) ReserveAdmissionGroup(stage BudgetStage, admissions []BudgetAdmission) (map[string]BudgetReservation, error) {
	return ledger.reserveAdmissionGroup(stage, admissions, true)
}

// ReserveProtectedAdmissionGroup atomically locks role allocations that must be
// both protected from siblings and enforced as per-task ceilings. Verifier uses
// this path because its input/output/total slice is assigned before execution.
func (ledger *BudgetLedger) ReserveProtectedAdmissionGroup(stage BudgetStage, admissions []BudgetAdmission) (map[string]BudgetReservation, error) {
	return ledger.reserveAdmissionGroup(stage, admissions, false)
}

func (ledger *BudgetLedger) reserveAdmissionGroup(stage BudgetStage, admissions []BudgetAdmission, soft bool) (map[string]BudgetReservation, error) {
	if ledger == nil {
		return nil, fmt.Errorf("budget ledger is required")
	}
	if stage == "" {
		return nil, fmt.Errorf("budget stage is required")
	}
	if len(admissions) == 0 {
		return map[string]BudgetReservation{}, nil
	}
	var total BudgetVector
	seen := make(map[string]struct{}, len(admissions))
	for _, admission := range admissions {
		if admission.TaskID == "" {
			return nil, fmt.Errorf("budget admission task id is required")
		}
		if _, exists := seen[admission.TaskID]; exists {
			return nil, fmt.Errorf("duplicate budget admission task %q", admission.TaskID)
		}
		seen[admission.TaskID] = struct{}{}
		if err := validateBudgetVector(admission.Grant); err != nil {
			return nil, fmt.Errorf("budget admission %q: %w", admission.TaskID, err)
		}
		total = addVector(total, admission.Grant)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if !fits(addVector(addVector(ledger.run.Used, ledger.run.Reserved), total), ledger.run.Limit) {
		return nil, fmt.Errorf("%w: parallel admission exceeds run limit", ErrBudgetExceeded)
	}
	stageBudget := ledger.stageLocked(stage)
	// Admission is soft with respect to a stage: token dimensions are shared
	// at Run scope, while hard task/tool reservations still enforce their stage.
	reservations := make(map[string]BudgetReservation, len(admissions))
	for _, admission := range admissions {
		reservationID := fmt.Sprintf("reservation_%d", ledger.nextID.Add(1))
		ledger.reservations[reservationID] = budgetReservation{
			id: reservationID, stage: stage, grant: admission.Grant, soft: soft,
			pending: make(map[string]BudgetVector),
		}
		reservations[admission.TaskID] = BudgetReservation{
			ledger: ledger, ID: reservationID, Grant: admission.Grant,
		}
	}
	ledger.run.Reserved = addVector(ledger.run.Reserved, total)
	stageBudget.Reserved = addVector(stageBudget.Reserved, total)
	ledger.stages[stage] = stageBudget
	return reservations, nil
}

func (ledger *BudgetLedger) reserve(stage BudgetStage, id string, grant BudgetVector, soft bool) (BudgetReservation, error) {
	if err := ledger.canReserve(stage, id, grant, soft); err != nil {
		return BudgetReservation{}, err
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if err := ledger.canReserveLocked(stage, id, grant, soft); err != nil {
		return BudgetReservation{}, err
	}
	reservationID := fmt.Sprintf("reservation_%d", ledger.nextID.Add(1))
	ledger.reservations[reservationID] = budgetReservation{
		id: reservationID, stage: stage, grant: grant, soft: soft,
		pending: make(map[string]BudgetVector),
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

// Check implements agent.RunBudgetGate. It is intentionally run-scoped: a
// Workflow child is not assigned a private token quota. Call reservations add
// the missing in-flight accounting before the task is finally settled.
func (ledger *BudgetLedger) checkReservation(id string) error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	// The current reservation is already protected for this task. Exclude it
	// from global preflight so a full group can spend its own minimum while
	// refusing calls that invade a sibling's grant.
	otherReserved := subtractVector(ledger.run.Reserved, reservationReserved(reservation))
	other := addVector(ledger.run.Used, otherReserved)
	if !fits(other, ledger.run.Limit) ||
		(reservationReserved(reservation) == (BudgetVector{}) && exhausted(other, ledger.run.Limit)) {
		return fmt.Errorf("%w: shared investigation run budget is exhausted", ErrBudgetExceeded)
	}
	return nil
}

func (ledger *BudgetLedger) Check() error {
	if ledger == nil {
		return fmt.Errorf("budget ledger is required")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if exhausted(addVector(ledger.run.Used, ledger.run.Reserved), ledger.run.Limit) {
		return fmt.Errorf("%w: shared investigation run budget is exhausted", ErrBudgetExceeded)
	}
	return nil
}

// ReserveCall reserves one model request inside an Agent task. The estimate is
// kept in the task reservation so concurrent siblings see in-flight input.
func (ledger *BudgetLedger) ReserveCall(id string, estimate BudgetVector) (budgetCallReservation, error) {
	if ledger == nil {
		return budgetCallReservation{}, fmt.Errorf("budget ledger is required")
	}
	if err := validateBudgetVector(estimate); err != nil {
		return budgetCallReservation{}, fmt.Errorf("reserve call for %q: %w", id, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return budgetCallReservation{}, fmt.Errorf("reservation %q not found", id)
	}
	nextCalls := addVector(reservation.calls, estimate)
	if !reservation.soft && !fitsGrant(nextCalls, reservation.grant) {
		return budgetCallReservation{}, fmt.Errorf(
			"%w: model call for reservation %q exceeds its grant",
			ErrBudgetExceeded, id,
		)
	}
	currentReserved := reservationReserved(reservation)
	nextReserved := maxBudgetVector(reservation.grant, nextCalls)
	projectedRun := addVector(
		ledger.run.Used,
		addVector(subtractVector(ledger.run.Reserved, currentReserved), nextReserved),
	)
	if !fits(projectedRun, ledger.run.Limit) {
		return budgetCallReservation{}, fmt.Errorf(
			"%w: model call for reservation %q exceeds run limit dimensions=%s projected=%+v limit=%+v",
			ErrBudgetExceeded, id, strings.Join(exceededDimensions(projectedRun, ledger.run.Limit), ","), projectedRun, ledger.run.Limit,
		)
	}
	stageBudget := ledger.stageLocked(reservation.stage)
	if !reservation.soft {
		projectedStage := addVector(
			stageBudget.Used,
			addVector(subtractVector(stageBudget.Reserved, currentReserved), nextReserved),
		)
		if !fits(projectedStage, stageBudget.Limit) {
			return budgetCallReservation{}, fmt.Errorf(
				"%w: model call for reservation %q exceeds stage %q limit dimensions=%s projected=%+v limit=%+v",
				ErrBudgetExceeded, id, reservation.stage, strings.Join(exceededDimensions(projectedStage, stageBudget.Limit), ","), projectedStage, stageBudget.Limit,
			)
		}
	}
	if reservation.pending == nil {
		reservation.pending = make(map[string]BudgetVector)
	}
	callID := fmt.Sprintf("%s.call.%d", id, ledger.nextID.Add(1))
	reservation.pending[callID] = estimate
	reservation.calls = nextCalls
	ledger.reservations[id] = reservation
	ledger.run.Reserved = addVector(subtractVector(ledger.run.Reserved, currentReserved), nextReserved)
	stageBudget.Reserved = addVector(subtractVector(stageBudget.Reserved, currentReserved), nextReserved)
	ledger.stages[reservation.stage] = stageBudget
	return budgetCallReservation{ledger: ledger, reservationID: id, callID: callID}, nil
}

func (ledger *BudgetLedger) settleCall(id, callID string, actual BudgetVector) error {
	if err := validateBudgetVector(actual); err != nil {
		return fmt.Errorf("settle call %q: %w", callID, err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	estimate, exists := reservation.pending[callID]
	if !exists {
		return fmt.Errorf("call reservation %q not found", callID)
	}
	currentReserved := reservationReserved(reservation)
	nextCalls := addVector(subtractVector(reservation.calls, estimate), actual)
	if !reservation.soft && !fitsGrant(nextCalls, reservation.grant) {
		return fmt.Errorf("%w: actual model usage exceeds reservation %q", ErrBudgetExceeded, id)
	}
	nextReserved := maxBudgetVector(reservation.grant, nextCalls)
	projectedRun := addVector(
		ledger.run.Used,
		addVector(subtractVector(ledger.run.Reserved, currentReserved), nextReserved),
	)
	runExceeded := !fits(projectedRun, ledger.run.Limit)
	if runExceeded && !reservation.soft {
		return fmt.Errorf("%w: actual model usage exceeds run limit for reservation %q", ErrBudgetExceeded, id)
	}
	stageBudget := ledger.stageLocked(reservation.stage)
	if !reservation.soft {
		projectedStage := addVector(
			stageBudget.Used,
			addVector(subtractVector(stageBudget.Reserved, currentReserved), nextReserved),
		)
		if !fits(projectedStage, stageBudget.Limit) {
			return fmt.Errorf("%w: actual model usage exceeds stage %q limit for reservation %q", ErrBudgetExceeded, reservation.stage, id)
		}
	}
	delete(reservation.pending, callID)
	reservation.calls = nextCalls
	ledger.reservations[id] = reservation
	ledger.run.Reserved = addVector(subtractVector(ledger.run.Reserved, currentReserved), nextReserved)
	stageBudget.Reserved = addVector(subtractVector(stageBudget.Reserved, currentReserved), nextReserved)
	ledger.stages[reservation.stage] = stageBudget
	if runExceeded {
		return fmt.Errorf("%w: actual model usage exceeds run limit for reservation %q", ErrBudgetExceeded, id)
	}
	return nil
}

func (ledger *BudgetLedger) releaseCall(id, callID string) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	estimate, exists := reservation.pending[callID]
	if !exists {
		return fmt.Errorf("call reservation %q not found", callID)
	}
	currentReserved := reservationReserved(reservation)
	delete(reservation.pending, callID)
	reservation.calls = subtractVector(reservation.calls, estimate)
	nextReserved := reservationReserved(reservation)
	ledger.reservations[id] = reservation
	ledger.run.Reserved = addVector(subtractVector(ledger.run.Reserved, currentReserved), nextReserved)
	stageBudget := ledger.stageLocked(reservation.stage)
	stageBudget.Reserved = addVector(subtractVector(stageBudget.Reserved, currentReserved), nextReserved)
	ledger.stages[reservation.stage] = stageBudget
	return nil
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

// MinimumOutputForReservation returns the unconsumed output floor protected
// by an admission grant. It is zero after the grant has been consumed.
func (ledger *BudgetLedger) MinimumOutputForReservation(id string) int64 {
	if ledger == nil {
		return 0
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, ok := ledger.reservations[id]
	if !ok || reservation.grant.OutputTokens <= reservation.calls.OutputTokens {
		return 0
	}
	return reservation.grant.OutputTokens - reservation.calls.OutputTokens
}

// AvailableForReservation returns the incremental call budget left for one
// reservation. Admission reservations use only the shared run view because
// their stage allocation is not a private Agent quota.
func (ledger *BudgetLedger) AvailableForReservation(id string) BudgetVector {
	if ledger == nil {
		return BudgetVector{}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, ok := ledger.reservations[id]
	if !ok {
		return BudgetVector{}
	}
	reserved := reservationReserved(reservation)
	covered := subtractVector(reserved, reservation.calls)
	runAvailable := addAvailableCapacity(
		subtractVector(ledger.run.Limit, addVector(ledger.run.Used, ledger.run.Reserved)),
		covered,
	)
	if reservation.soft {
		return runAvailable
	}
	stageBudget := ledger.stageLocked(reservation.stage)
	stageAvailable := addAvailableCapacity(
		subtractVector(stageBudget.Limit, addVector(stageBudget.Used, stageBudget.Reserved)),
		covered,
	)
	available := minAvailable(runAvailable, stageAvailable)
	return capAvailableToGrant(available, reservation.calls, reservation.grant)
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

func (ledger *BudgetLedger) canReserve(stage BudgetStage, id string, grant BudgetVector, soft bool) error {
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
	return ledger.canReserveLocked(stage, id, grant, soft)
}

func (ledger *BudgetLedger) canReserveLocked(stage BudgetStage, id string, grant BudgetVector, soft bool) error {
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
	if !soft && !fits(addVector(addVector(stageBudget.Used, stageBudget.Reserved), grant), stageBudget.Limit) {
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
	if len(reservation.pending) > 0 {
		return fmt.Errorf("reservation %q has unsettled model calls", id)
	}
	// Settled model calls are authoritative for provider usage. The outer task
	// may add dimensions such as tool calls or duration, but cannot erase them.
	actual = maxBudgetVector(actual, reservation.calls)
	if !reservation.soft && !fitsGrant(actual, reservation.grant) {
		return fmt.Errorf("%w: actual usage exceeds reservation %q", ErrBudgetExceeded, id)
	}
	// Admission reservations have no private stage ceiling. The shared run
	// limit remains authoritative; hard reservations still enforce their stage.
	reserved := reservationReserved(reservation)
	projectedRun := addVector(addVector(subtractVector(ledger.run.Reserved, reserved), ledger.run.Used), actual)
	runExceeded := !fits(projectedRun, ledger.run.Limit)
	if runExceeded && !reservation.soft {
		return fmt.Errorf("%w: actual usage exceeds run limit for reservation %q", ErrBudgetExceeded, id)
	}
	stageBudget := ledger.stageLocked(reservation.stage)
	if !reservation.soft {
		projectedStage := addVector(addVector(subtractVector(stageBudget.Reserved, reserved), stageBudget.Used), actual)
		if !fits(projectedStage, stageBudget.Limit) {
			return fmt.Errorf("%w: actual usage exceeds stage %q limit for reservation %q", ErrBudgetExceeded, reservation.stage, id)
		}
	}
	delete(ledger.reservations, id)
	ledger.run.Reserved = subtractVector(ledger.run.Reserved, reserved)
	ledger.run.Used = addVector(ledger.run.Used, actual)
	stageBudget.Reserved = subtractVector(stageBudget.Reserved, reserved)
	stageBudget.Used = addVector(stageBudget.Used, actual)
	ledger.stages[reservation.stage] = stageBudget
	if runExceeded {
		return fmt.Errorf("%w: actual usage exceeds run limit for reservation %q", ErrBudgetExceeded, id)
	}
	return nil
}

func budgetVectorFromUsage(usage agentapi.Usage) BudgetVector {
	return BudgetVector{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		CostMicros:   usage.CostMicros,
	}
}

func budgetUsageFromVector(value BudgetVector) agentapi.Usage {
	return agentapi.Usage{
		InputTokens: value.InputTokens, OutputTokens: value.OutputTokens,
		TotalTokens: value.TotalTokens, CostMicros: value.CostMicros,
	}
}

func reservationReserved(reservation budgetReservation) BudgetVector {
	return maxBudgetVector(reservation.grant, reservation.calls)
}

func maxBudgetVector(left, right BudgetVector) BudgetVector {
	return BudgetVector{
		InputTokens:  maxInt64(left.InputTokens, right.InputTokens),
		OutputTokens: maxInt64(left.OutputTokens, right.OutputTokens),
		TotalTokens:  maxInt64(left.TotalTokens, right.TotalTokens),
		ToolCalls:    maxInt(left.ToolCalls, right.ToolCalls),
		Duration:     maxDuration(left.Duration, right.Duration),
		CostMicros:   maxInt64(left.CostMicros, right.CostMicros),
	}
}

func addAvailableCapacity(available, covered BudgetVector) BudgetVector {
	out := addVector(available, covered)
	if available.CostMicros == 0 {
		out.CostMicros = 0
	}
	return out
}

func capAvailableToGrant(available, used, grant BudgetVector) BudgetVector {
	remaining := subtractVector(grant, used)
	if grant.InputTokens > 0 {
		available.InputTokens = minInt64(available.InputTokens, remaining.InputTokens)
	}
	if grant.OutputTokens > 0 {
		available.OutputTokens = minInt64(available.OutputTokens, remaining.OutputTokens)
	}
	if grant.TotalTokens > 0 {
		available.TotalTokens = minInt64(available.TotalTokens, remaining.TotalTokens)
	}
	if grant.ToolCalls > 0 {
		available.ToolCalls = minInt(available.ToolCalls, remaining.ToolCalls)
	}
	if grant.Duration > 0 {
		available.Duration = maxDuration(0, minDuration(available.Duration, remaining.Duration))
	}
	if grant.CostMicros > 0 {
		if available.CostMicros == 0 {
			available.CostMicros = remaining.CostMicros
		} else {
			available.CostMicros = minInt64(available.CostMicros, remaining.CostMicros)
		}
	}
	return available
}

func minAvailable(left, right BudgetVector) BudgetVector {
	return BudgetVector{
		InputTokens:  minInt64(left.InputTokens, right.InputTokens),
		OutputTokens: minInt64(left.OutputTokens, right.OutputTokens),
		TotalTokens:  minInt64(left.TotalTokens, right.TotalTokens),
		CostMicros:   minAvailableCost(left.CostMicros, right.CostMicros),
	}
}

func minAvailableCost(left, right int64) int64 {
	if left == 0 {
		return right
	}
	if right == 0 {
		return left
	}
	return minInt64(left, right)
}

func (ledger *BudgetLedger) release(id string) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	reservation, exists := ledger.reservations[id]
	if !exists {
		return fmt.Errorf("reservation %q not found", id)
	}
	delete(ledger.reservations, id)
	reserved := reservationReserved(reservation)
	ledger.run.Reserved = subtractVector(ledger.run.Reserved, reserved)
	stageBudget := ledger.stageLocked(reservation.stage)
	stageBudget.Reserved = subtractVector(stageBudget.Reserved, reserved)
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

func exceededDimensions(value, limit BudgetVector) []string {
	dimensions := make([]string, 0, 6)
	if !within(value.InputTokens, limit.InputTokens) {
		dimensions = append(dimensions, "input_tokens")
	}
	if !within(value.OutputTokens, limit.OutputTokens) {
		dimensions = append(dimensions, "output_tokens")
	}
	if !withinTotalTokens(value, limit) {
		dimensions = append(dimensions, "total_tokens")
	}
	if !withinInt(value.ToolCalls, limit.ToolCalls) {
		dimensions = append(dimensions, "tool_calls")
	}
	if !withinDuration(value.Duration, limit.Duration) {
		dimensions = append(dimensions, "duration")
	}
	if !withinCost(value.CostMicros, limit.CostMicros) {
		dimensions = append(dimensions, "cost_micros")
	}
	return dimensions
}

// exhausted reports whether no further call can start under a positive run
// limit. Equality is exhausted for admission, while zero keeps a dimension
// unbounded just like the other budget helpers.
func exhausted(used, limit BudgetVector) bool {
	return exhaustedInt64(used.InputTokens, limit.InputTokens) ||
		exhaustedInt64(used.OutputTokens, limit.OutputTokens) ||
		exhaustedTotalTokens(used, limit) ||
		exhaustedInt(used.ToolCalls, limit.ToolCalls) ||
		exhaustedDuration(used.Duration, limit.Duration) ||
		exhaustedInt64(used.CostMicros, limit.CostMicros)
}

func exhaustedTotalTokens(used, limit BudgetVector) bool {
	if limit.TotalTokens == 0 {
		return false
	}
	total := used.TotalTokens
	if total == 0 {
		total = saturatingAdd(used.InputTokens, used.OutputTokens)
	}
	return total >= limit.TotalTokens
}

func exhaustedInt64(value, limit int64) bool {
	return limit > 0 && value >= limit
}

func exhaustedInt(value, limit int) bool {
	return limit > 0 && value >= limit
}

func exhaustedDuration(value, limit time.Duration) bool {
	return limit > 0 && value >= limit
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

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
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

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
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
