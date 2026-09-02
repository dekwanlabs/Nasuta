package delegation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	ErrorUnknownCapability      = "unknown_capability"
	ErrorCapabilityDisabled     = "capability_disabled"
	ErrorCapabilityNotAllowed   = "capability_not_allowed"
	ErrorCapabilityNotReadOnly  = "capability_not_read_only"
	ErrorCapabilityRole         = "capability_not_investigator"
	ErrorInvalidObjective       = "invalid_objective"
	ErrorInvalidFacet           = "invalid_focus_facet"
	ErrorUnauthorizedEvidence   = "unauthorized_evidence_ref"
	ErrorDuplicateTask          = "duplicate_task"
	ErrorDepthExceeded          = "delegation_depth_exceeded"
	ErrorChildLimitExceeded     = "child_limit_exceeded"
	ErrorBudgetInsufficient     = "delegation_budget_insufficient"
	ErrorParentTimeInsufficient = "parent_time_budget_insufficient"
	ErrorChildExecution         = "child_execution_failed"
	ErrorChildInputLimit        = "child_input_limit_exceeded"
	ErrorChildOutputLimit       = "child_output_limit_exceeded"
)

const (
	maxObjectiveBytes = 2000
	maxEvidenceRefs   = 20

	// Flow investigations are intentionally shallower than ordinary evidence
	// deep-dives. The parent still owns the user-facing Mermaid contract; these
	// limits only bound the child evidence/Flow IR handoff.
	flowChildMaxTurns        = 2
	flowChildMaxToolCalls    = 6
	flowChildMaxOutputTokens = 8000
	flowReportMaxTokens      = 2000
)

type Persistence interface {
	ReserveDelegationBatch(
		context.Context,
		agentrun.DelegationAdmission,
	) ([]agentrun.DelegationTaskRecord, error)
	RejectDelegationTask(
		context.Context,
		agentrun.DelegationRejection,
	) (agentrun.DelegationTaskRecord, error)
	LinkDelegationChild(context.Context, string, string, int, string) error
	SettleDelegationTask(
		context.Context,
		agentrun.DelegationSettlement,
	) (agentrun.DelegationTaskRecord, error)
	GetDelegationTask(
		context.Context,
		string,
		string,
		int,
	) (agentrun.DelegationTaskRecord, *agentrun.DelegationArtifact, error)
	GetDelegationEvidence(
		context.Context,
		string,
		string,
		int,
	) ([]tool.EvidenceUnit, error)
}

// AttemptPersistence is optional for backwards-compatible test and embedding
// implementations. The production run Store implements it to make retries and
// recovery durable.
type AttemptPersistence interface {
	StartDelegationAttempt(context.Context, agentrun.DelegationAttemptStart) (agentrun.DelegationAttemptRecord, error)
	FinishDelegationAttempt(context.Context, agentrun.DelegationAttemptFinish) (agentrun.DelegationAttemptRecord, error)
	GetLatestDelegationAttempt(context.Context, string, string, int) (agentrun.DelegationAttemptRecord, error)
	LinkDelegationAttemptChild(context.Context, string, string, int, int, string) error
}

// CheckpointPersistence is optional so embedders can adopt the parent recovery
// boundary without changing the core delegation persistence contract.
type CheckpointPersistence interface {
	UpsertDelegationCheckpoint(context.Context, agentrun.DelegationCheckpoint) error
	GetDelegationCheckpoint(context.Context, string, string, int) (agentrun.DelegationCheckpoint, error)
}

// WorkQueue is the durable dispatch boundary for child execution. A queue
// implementation must make claim, renew and completion conditional on the
// worker owner/fence pair. The executor still supports an inline path when no
// queue is configured, which keeps embedders backwards compatible.
type WorkQueue interface {
	EnqueueWorkItem(context.Context, agentrun.WorkItem) error
	ClaimWorkItem(context.Context, string, time.Time, time.Duration) (agentrun.WorkItem, error)
	ClaimWorkItemByID(context.Context, string, string, time.Time, time.Duration) (agentrun.WorkItem, error)
	RenewWorkItem(context.Context, string, string, int64, time.Time, time.Duration) error
	CompleteWorkItem(context.Context, string, string, int64, string, string) error
}

type kindWorkQueue interface {
	ClaimWorkItemByKind(context.Context, string, string, time.Time, time.Duration) (agentrun.WorkItem, error)
}

type queuedDelegationWork struct {
	Parent ParentContext           `json:"parent"`
	Task   agentapi.DelegationTask `json:"task"`
}

type EventEmitter interface {
	EmitEvent(agentrun.EventType, agentrun.ExecutionEvent)
}

// toolEventProjector mirrors a child investigator's tool lifecycle onto the
// parent QA stream so the dashboard can nest those calls under the child.
type toolEventProjector interface {
	ProjectToolEvents(string, string, string, string) func()
}

type ExecutorConfig struct {
	Capabilities       *agentapi.CapabilityRegistry
	Definitions        agentapi.DefinitionResolver
	Runtime            agentapi.Runtime
	Persistence        Persistence
	Validator          *Validator
	Policy             agentapi.DelegationPolicy
	Allowlist          []string
	VerifierCapability string
	Events             EventEmitter
	Queue              WorkQueue
	WorkerOwner        string
	WorkerLeaseTTL     time.Duration
	DurableIOTimeout   time.Duration
}

type Executor struct {
	capabilities       *agentapi.CapabilityRegistry
	definitions        agentapi.DefinitionResolver
	runtime            agentapi.Runtime
	persistence        Persistence
	validator          *Validator
	policy             agentapi.DelegationPolicy
	allowlist          map[string]struct{}
	verifierCapability string
	events             EventEmitter
	queue              WorkQueue
	workerOwner        string
	workerLeaseTTL     time.Duration
	durableIOTimeout   time.Duration

	mu              sync.Mutex
	capabilitySlots map[string]chan struct{}
	flights         map[string]*taskFlight
}

type taskFlight struct {
	done         chan struct{}
	report       agentapi.DelegationReport
	evidence     []tool.EvidenceUnit
	observations []agentapi.EvidenceObservation
}

type preparedTask struct {
	index         int
	request       agentapi.DelegationTask
	capability    agentapi.Capability
	definition    agentapi.Definition
	objectiveHash string
	childRunID    string
	reportID      string
	artifactID    string
	limits        agentapi.RunLimits
	permissions   agentapi.PermissionPolicy
	context       []agentapi.ContextBlock
	inputTokens   int64
	outputTokens  int64
	reportTokens  int64
	budget        agentapi.RunBudgetTaskReservation
}

type taskOutcome struct {
	report       agentapi.DelegationReport
	evidence     []tool.EvidenceUnit
	observations []agentapi.EvidenceObservation
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if config.Capabilities == nil || config.Definitions == nil ||
		config.Runtime == nil || config.Persistence == nil {
		return nil, fmt.Errorf("delegation executor dependencies are required")
	}
	policy, err := normalizePolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	allowlist := make(map[string]struct{}, len(config.Allowlist))
	for _, id := range config.Allowlist {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("delegation capability allowlist contains an empty id")
		}
		allowlist[id] = struct{}{}
	}
	validator := config.Validator
	if validator == nil {
		validator = NewValidator(nil, ValidationLimits{})
	}
	verifierCapability := strings.TrimSpace(config.VerifierCapability)
	if verifierCapability == "" {
		verifierCapability = SemanticVerifierCapabilityID
	}
	if config.WorkerLeaseTTL <= 0 {
		config.WorkerLeaseTTL = 30 * time.Second
	}
	if config.DurableIOTimeout <= 0 {
		config.DurableIOTimeout = 5 * time.Second
	}
	return &Executor{
		capabilities:       config.Capabilities,
		definitions:        config.Definitions,
		runtime:            config.Runtime,
		persistence:        config.Persistence,
		validator:          validator,
		policy:             policy,
		allowlist:          allowlist,
		verifierCapability: verifierCapability,
		events:             config.Events,
		queue:              config.Queue,
		workerOwner:        strings.TrimSpace(config.WorkerOwner),
		workerLeaseTTL:     config.WorkerLeaseTTL,
		durableIOTimeout:   config.DurableIOTimeout,
		capabilitySlots:    make(map[string]chan struct{}),
		flights:            make(map[string]*taskFlight),
	}, nil
}

func (executor *Executor) Policy() agentapi.DelegationPolicy {
	return executor.policy
}

func (executor *Executor) Capabilities() []agentapi.Capability {
	candidates := executor.capabilities.DelegatableInvestigators()
	if len(executor.allowlist) == 0 {
		return candidates
	}
	filtered := make([]agentapi.Capability, 0, len(candidates))
	for _, capability := range candidates {
		if _, ok := executor.allowlist[capability.ID]; ok {
			filtered = append(filtered, capability)
		}
	}
	return filtered
}

func (executor *Executor) Execute(
	ctx context.Context,
	tasks []agentapi.DelegationTask,
) (agentapi.DelegationBatchResult, []tool.EvidenceUnit, error) {
	parent, ok := ParentContextFrom(ctx)
	if !ok || strings.TrimSpace(parent.RunID) == "" {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("delegation parent context is required")
	}
	invocationID, ok := tool.InvocationIDFromContext(ctx)
	if !ok {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("delegation invocation id is required")
	}
	if len(tasks) == 0 {
		return agentapi.DelegationBatchResult{}, nil,
			fmt.Errorf("at least one delegation task is required")
	}
	parent.InvocationID = invocationID
	delegationID := stableID("del", parent.RunID, invocationID)
	result := agentapi.DelegationBatchResult{
		DelegationID: delegationID,
		Results:      make([]agentapi.DelegationReport, len(tasks)),
	}
	prepared := make([]preparedTask, 0, len(tasks))
	seenTasks := make(map[string]struct{}, len(tasks))
	for index, task := range tasks {
		executor.emit(agentrun.EventDelegationCreated, parent, delegationID, "", task, "created", "", "", 0, agentapi.Usage{})
		candidate, code, err := executor.prepareTask(
			parent, delegationID, index, task, seenTasks,
		)
		if err != nil {
			report, persistErr := executor.reject(
				ctx, parent, delegationID, index, task,
				candidate.capability, candidate.objectiveHash, code, err,
			)
			if persistErr != nil {
				return agentapi.DelegationBatchResult{}, nil, persistErr
			}
			result.Results[index] = report
			continue
		}
		prepared = append(prepared, candidate)
	}
	rootGate := agentapi.RunBudgetTaskGateFromContext(ctx)
	if rootGate != nil {
		reserved := make([]agentapi.RunBudgetTaskReservation, 0, len(prepared))
		reserveErr := error(nil)
		for index := range prepared {
			grant := taskBudgetGrant(prepared[index])
			prepared[index].budget, reserveErr = rootGate.ReserveTask(grant)
			if reserveErr != nil {
				break
			}
			if prepared[index].budget != nil {
				reserved = append(reserved, prepared[index].budget)
			}
		}
		if reserveErr != nil {
			for _, reservation := range reserved {
				_ = reservation.Release()
			}
			for _, task := range prepared {
				report, rejectErr := executor.reject(
					ctx, parent, delegationID, task.index, task.request,
					task.capability, task.objectiveHash, ErrorBudgetInsufficient, reserveErr,
				)
				if rejectErr != nil {
					return agentapi.DelegationBatchResult{}, nil, rejectErr
				}
				result.Results[task.index] = report
			}
			validation, validateErr := executor.validator.ValidateWithContext(
				ctx, result.Results, cloneEvidenceIndex(parent.Evidence),
				parent.Context, nil, parent.HighRisk,
			)
			result.Validation = validation
			executor.emitValidation(parent, delegationID, validation, validateErr)
			executor.attachVerification(
				ctx, parent, &result, cloneEvidenceIndex(parent.Evidence), nil, validateErr,
			)
			return result, nil, validateErr
		}
	}
	if len(prepared) == 0 {
		validation, err := executor.validator.ValidateWithContext(
			ctx,
			result.Results,
			cloneEvidenceIndex(parent.Evidence),
			parent.Context,
			nil,
			parent.HighRisk,
		)
		result.Validation = validation
		executor.emitValidation(parent, delegationID, validation, err)
		executor.attachVerification(
			ctx,
			parent,
			&result,
			cloneEvidenceIndex(parent.Evidence),
			nil,
			err,
		)
		return result, nil, err
	}

	reservations := make([]agentrun.DelegationReservation, len(prepared))
	for index, task := range prepared {
		reservations[index] = agentrun.DelegationReservation{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: task.index, ChildRunID: task.childRunID,
			Capability: agentapi.CapabilityRef{
				ID: task.capability.ID, Version: task.capability.Version,
			},
			CapabilityHash:     task.capability.ContentHash,
			ObjectiveHash:      task.objectiveHash,
			Limits:             task.limits,
			ReservedTokens:     task.limits.MaxTotalTokens,
			ReservedCostMicros: task.limits.MaxCostMicros,
		}
	}
	records, err := executor.persistence.ReserveDelegationBatch(
		ctx,
		agentrun.DelegationAdmission{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			MaxChildren:         executor.policy.MaxChildren,
			MaxTotalTokens:      executor.policy.MaxTotalTokens,
			MaxTotalCostMicros:  executor.policy.MaxTotalCostMicros,
			ParentAnswerReserve: executor.policy.ParentAnswerReserve,
			Reservations:        reservations,
		},
	)
	if err != nil {
		for _, task := range prepared {
			if task.budget != nil {
				_ = task.budget.Release()
			}
		}
		code := ErrorBudgetInsufficient
		if errors.Is(err, agentrun.ErrDelegationChildLimit) {
			code = ErrorChildLimitExceeded
		} else if !errors.Is(err, agentrun.ErrDelegationBudgetInsufficient) {
			return agentapi.DelegationBatchResult{}, nil,
				fmt.Errorf("reserve delegation batch: %w", err)
		}
		for _, task := range prepared {
			report, rejectErr := executor.reject(
				ctx, parent, delegationID, task.index, task.request,
				task.capability, task.objectiveHash, code, err,
			)
			if rejectErr != nil {
				return agentapi.DelegationBatchResult{}, nil, rejectErr
			}
			result.Results[task.index] = report
		}
		validation, validateErr := executor.validator.ValidateWithContext(
			ctx,
			result.Results,
			cloneEvidenceIndex(parent.Evidence),
			parent.Context,
			nil,
			parent.HighRisk,
		)
		result.Validation = validation
		executor.emitValidation(parent, delegationID, validation, validateErr)
		executor.attachVerification(
			ctx,
			parent,
			&result,
			cloneEvidenceIndex(parent.Evidence),
			nil,
			validateErr,
		)
		return result, nil, validateErr
	}

	recordByIndex := make(map[int]agentrun.DelegationTaskRecord, len(records))
	for _, record := range records {
		recordByIndex[record.TaskIndex] = record
	}
	if err := executor.enqueueTasks(ctx, parent, delegationID, prepared); err != nil {
		return agentapi.DelegationBatchResult{}, nil, fmt.Errorf("enqueue delegation work: %w", err)
	}
	outcomes := executor.runTasks(ctx, parent, delegationID, prepared, recordByIndex)
	evidenceLedger := cloneEvidenceIndex(parent.Evidence)
	var returnedEvidence []tool.EvidenceUnit
	for _, outcome := range outcomes {
		result.Results[outcome.reportIndex()] = outcome.report
		evidenceLedger = AddEvidenceUnits(evidenceLedger, outcome.evidence)
		returnedEvidence = append(returnedEvidence, cloneEvidenceUnits(outcome.evidence)...)
	}
	var observations []agentapi.EvidenceObservation
	for _, outcome := range outcomes {
		observations = append(observations, outcome.observations...)
	}
	validation, err := executor.validator.ValidateWithContext(
		ctx,
		result.Results,
		evidenceLedger,
		parent.Context,
		observations,
		parent.HighRisk,
	)
	result.Validation = validation
	executor.emitValidation(parent, delegationID, validation, err)
	executor.attachVerification(
		ctx,
		parent,
		&result,
		evidenceLedger,
		observations,
		err,
	)
	return result, returnedEvidence, err
}

func (executor *Executor) prepareTask(
	parent ParentContext,
	delegationID string,
	index int,
	request agentapi.DelegationTask,
	seen map[string]struct{},
) (preparedTask, string, error) {
	request.Capability = strings.TrimSpace(request.Capability)
	request.Objective = strings.TrimSpace(request.Objective)
	request.FocusFacets = canonicalStrings(request.FocusFacets)
	request.EvidenceRefs = canonicalStrings(request.EvidenceRefs)
	candidate := preparedTask{index: index, request: request}
	if parent.Depth+1 > executor.policy.MaxDepth {
		return candidate, ErrorDepthExceeded, fmt.Errorf("delegation depth exceeds %d", executor.policy.MaxDepth)
	}
	if index >= executor.policy.MaxChildren {
		return candidate, ErrorChildLimitExceeded, fmt.Errorf("delegation child limit is %d", executor.policy.MaxChildren)
	}
	if request.Objective == "" || len(request.Objective) > maxObjectiveBytes {
		return candidate, ErrorInvalidObjective, fmt.Errorf("delegation objective must be between 1 and %d bytes", maxObjectiveBytes)
	}

	capability, err := executor.capabilities.Resolve(
		agentapi.CapabilityRef{ID: request.Capability},
	)
	if err != nil {
		return candidate, ErrorUnknownCapability, err
	}
	candidate.capability = capability
	if !capability.Enabled {
		return candidate, ErrorCapabilityDisabled, fmt.Errorf("capability %q is disabled", capability.ID)
	}
	if capability.Role != agentapi.RoleInvestigator {
		return candidate, ErrorCapabilityRole, fmt.Errorf("capability %q is not an investigator", capability.ID)
	}
	if capability.SideEffects != agentapi.SideEffectNone {
		return candidate, ErrorCapabilityNotReadOnly, fmt.Errorf("capability %q is not read-only", capability.ID)
	}
	if len(executor.allowlist) > 0 {
		if _, allowed := executor.allowlist[capability.ID]; !allowed {
			return candidate, ErrorCapabilityNotAllowed, fmt.Errorf("capability %q is not enabled for delegation", capability.ID)
		}
	}
	request.FocusFacets = filterAuthorizedFacets(request.FocusFacets, capability.InputFacets)
	request.EvidenceRefs = filterAuthorizedRefs(request.EvidenceRefs, parent)
	if len(request.EvidenceRefs) > maxEvidenceRefs {
		return candidate, ErrorUnauthorizedEvidence, fmt.Errorf("too many evidence references")
	}
	objectiveHash := hashJSON(request)
	candidate.request = request
	candidate.objectiveHash = objectiveHash
	key := hashJSON(struct {
		Capability   string
		Objective    string
		FocusFacets  []string
		EvidenceRefs []string
	}{
		Capability: request.Capability, Objective: request.Objective,
		FocusFacets: request.FocusFacets, EvidenceRefs: request.EvidenceRefs,
	})
	if _, duplicate := seen[key]; duplicate {
		return candidate, ErrorDuplicateTask, fmt.Errorf("duplicate delegation task")
	}
	seen[key] = struct{}{}
	definition, err := executor.definitions.Resolve(capability.Agent)
	if err != nil {
		return candidate, ErrorUnknownCapability, err
	}
	candidate.definition = definition
	candidate.permissions = intersectPermissions(
		parent.Permissions,
		agentapi.PermissionPolicy{Scopes: capability.PermissionScope},
	)
	if len(candidate.permissions.Scopes) == 0 {
		return candidate, ErrorCapabilityNotAllowed,
			fmt.Errorf("parent has no permission accepted by capability %q", capability.ID)
	}
	childBudget := executor.childBudget(parent)
	candidate.inputTokens = childBudget.inputTokens
	candidate.outputTokens = childBudget.outputTokens
	candidate.reportTokens = childBudget.reportTokens
	candidate.context = selectContext(
		parent, request.EvidenceRefs, request.FocusFacets,
		childBudget.inputTokens,
	)
	input, err := childInput(parent, delegationID, candidate)
	if err != nil {
		return candidate, ErrorChildInputLimit, err
	}
	if estimateTokens(input, candidate.context) > childBudget.inputTokens {
		return candidate, ErrorChildInputLimit, fmt.Errorf("delegation child input exceeds token limit")
	}
	candidate.childRunID = stableID(
		"run_child", parent.RunID, delegationID, fmt.Sprintf("%d", index),
		capability.ContentHash, objectiveHash,
	)
	candidate.reportID = stableID("report", candidate.childRunID)
	candidate.artifactID = stableID("artifact", candidate.reportID)
	limits, err := executor.childLimits(parent, definition)
	if err != nil {
		return candidate, ErrorParentTimeInsufficient, err
	}
	candidate.limits = limits
	if limits.MaxOutputTokens > 0 {
		candidate.outputTokens = limits.MaxOutputTokens
	}
	return candidate, "", nil
}

func delegationWorkID(delegationID string, taskIndex int) string {
	return stableID("work", delegationID, fmt.Sprintf("%d", taskIndex))
}

func (executor *Executor) enqueueTasks(ctx context.Context, parent ParentContext, delegationID string, tasks []preparedTask) error {
	if executor.queue == nil || len(tasks) == 0 {
		return nil
	}
	owner := executor.workerOwner
	if owner == "" {
		return fmt.Errorf("worker owner is required when durable delegation queue is enabled")
	}
	for _, task := range tasks {
		payload, err := json.Marshal(queuedDelegationWork{Parent: parent, Task: task.request})
		if err != nil {
			return err
		}
		durableCtx, cancelDurable := executor.durableContext(ctx)
		err = executor.queue.EnqueueWorkItem(durableCtx, agentrun.WorkItem{
			WorkID: delegationWorkID(delegationID, task.index), RunID: parent.RunID,
			ParentRunID: parent.RunID, DelegationID: delegationID, TaskIndex: task.index,
			AttemptNo: 1, Kind: "delegation_child", Payload: payload, State: agentrun.WorkReady,
		})
		cancelDurable()
		if err != nil {
			return err
		}
	}
	return nil
}

func (executor *Executor) runQueuedOrInline(ctx context.Context, parent ParentContext, delegationID string, task preparedTask, record agentrun.DelegationTaskRecord) taskOutcome {
	if executor.queue == nil {
		return executor.runTask(ctx, parent, delegationID, task, record)
	}
	durableCtx, cancelDurable := executor.durableContext(ctx)
	item, err := executor.queue.ClaimWorkItemByID(durableCtx, delegationWorkID(delegationID, task.index), executor.workerOwner, time.Now().UTC(), executor.workerLeaseTTL)
	cancelDurable()
	if err == nil {
		return executor.runClaimedWork(ctx, parent, delegationID, task, record, item)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// Queue infrastructure failure is not a durable child outcome. Do not
		// settle the logical task here: another worker may still claim the
		// already-enqueued item, and settling would race/overwrite its report.
		report := failedReport(task, ErrorReportPersistenceFailed, err)
		return taskOutcome{report: report}
	}

	// A process-local child worker may have won the queue lease between
	// enqueue and this parent-side claim. That is not a task failure: wait for
	// the durable delegation projection and replay the persisted report. The
	// task reservation remains owned by this parent until the persisted
	// settlement is observed, at which point Release closes it exactly once.
	return executor.waitForQueuedTask(ctx, parent, delegationID, task)
}

func (executor *Executor) runClaimedWork(ctx context.Context, parent ParentContext, delegationID string, task preparedTask, record agentrun.DelegationTaskRecord, item agentrun.WorkItem) taskOutcome {
	workCtx, stopHeartbeat := executor.withWorkLease(ctx, item)
	defer stopHeartbeat()
	outcome := executor.runTask(workCtx, parent, delegationID, task, record)
	state := agentrun.WorkSucceeded
	lastError := ""
	if outcome.report.Status != agentapi.DelegationCompleted && outcome.report.Status != agentapi.DelegationPartial {
		state = agentrun.WorkFailed
		if outcome.report.Error != nil {
			lastError = outcome.report.Error.Message
		}
	}
	completeCtx, cancelComplete := executor.cleanupContext(ctx)
	err := executor.queue.CompleteWorkItem(completeCtx, item.WorkID, item.LeaseOwner, item.LeaseFence, state, lastError)
	cancelComplete()
	if err != nil {
		if outcome.report.Error == nil {
			outcome.report.Error = &agentapi.RunError{Code: ErrorReportPersistenceFailed, Message: err.Error()}
			outcome.report.Status = agentapi.DelegationFailed
		}
	}
	return outcome
}

// durableContext keeps one queue/projection operation alive across request
// cancellation, preserves an earlier absolute request deadline, and always
// adds an I/O ceiling. It is deliberately created per database operation: a
// background worker may wait for an arbitrary amount of logical time, but no
// single database call may block forever.
func (executor *Executor) durableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	timeout := executor.durableIOTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ioDeadline := time.Now().Add(timeout)
	if ctx != nil {
		if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(ioDeadline) {
			return context.WithDeadline(base, requestDeadline)
		}
	}
	return context.WithDeadline(base, ioDeadline)
}

// cleanupContext grants terminal accounting writes a small bounded grace
// period even when the request deadline has just expired. The owner/fence
// predicate remains the authority, so this grace cannot let a stale worker
// commit after another worker has reclaimed the item.
func (executor *Executor) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	timeout := executor.durableIOTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(base, timeout)
}

func (executor *Executor) waitForQueuedTask(ctx context.Context, parent ParentContext, delegationID string, task preparedTask) taskOutcome {
	// The parent-side reservation belongs to the logical task, not to this
	// goroutine. Keep it held while another worker owns the queue item; release
	// only after a durable settlement is observed (or after this parent
	// reclaims and executes the item itself, whose runTaskOwned path releases
	// it). Releasing on parent cancellation would let the parent reuse capacity
	// while the admitted child can still execute after redelivery.
	released := false
	release := func() {
		if released || task.budget == nil {
			return
		}
		released = true
		_ = task.budget.Release()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		durableCtx, cancelDurable := executor.durableContext(ctx)
		record, _, err := executor.persistence.GetDelegationTask(durableCtx, parent.RunID, delegationID, task.index)
		cancelDurable()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			// A read failure is an infrastructure failure, not proof that the
			// child is unavailable. Leave the durable task and reservation intact
			// so a later recovery/worker pass can finish it.
			report := failedReport(task, ErrorReportPersistenceFailed, err)
			return taskOutcome{report: report}
		}
		if err == nil && record.SettledUsage != nil {
			replayCtx, cancelReplay := executor.durableContext(ctx)
			outcome := executor.replayTask(replayCtx, parent, delegationID, task)
			cancelReplay()
			release()
			return outcome
		}

		// If the previous claimant crashed before settling, a lease expiry may
		// make the item claimable again. Claiming here preserves the fast path
		// and still leaves another worker free to win the race.
		claimCtx, cancelClaim := executor.durableContext(ctx)
		item, claimErr := executor.queue.ClaimWorkItemByID(claimCtx, delegationWorkID(delegationID, task.index), executor.workerOwner, time.Now().UTC(), executor.workerLeaseTTL)
		cancelClaim()
		if claimErr == nil {
			// runTaskOwned normally releases the reservation. Calling release
			// here as well is intentionally idempotent and also covers the case
			// where runTask joins an already-running local flight.
			outcome := executor.runClaimedWork(ctx, parent, delegationID, task, record, item)
			release()
			return outcome
		}
		if !errors.Is(claimErr, sql.ErrNoRows) {
			report := failedReport(task, ErrorReportPersistenceFailed, claimErr)
			return taskOutcome{report: report}
		}

		select {
		case <-ctx.Done():
			// Cancellation stops the parent wait only. Never settleUnavailable
			// here because a different worker may still be executing the queued
			// child and will eventually publish the authoritative report.
			report := cancelledReport(task, ErrorParentCancelled, ctx.Err())
			return taskOutcome{report: report}
		case <-ticker.C:
		}
	}
}

// withWorkLease binds a claimed queue item to a cancellable execution context
// and renews its lease until the work returns. A stale worker is cancelled as
// soon as its owner/fence pair can no longer renew, so it cannot continue doing
// model work after another instance has reclaimed the item.
func (executor *Executor) withWorkLease(ctx context.Context, item agentrun.WorkItem) (context.Context, func()) {
	workCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	interval := executor.workerLeaseTTL / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	if interval >= executor.workerLeaseTTL {
		interval = executor.workerLeaseTTL / 2
		if interval <= 0 {
			interval = time.Millisecond
		}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-workCtx.Done():
				return
			case now := <-ticker.C:
				renewCtx, cancelRenew := executor.cleanupContext(workCtx)
				err := executor.queue.RenewWorkItem(renewCtx, item.WorkID, item.LeaseOwner, item.LeaseFence, now, executor.workerLeaseTTL)
				cancelRenew()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, func() {
		close(stop)
		cancel()
	}
}

// StartWorker runs a bounded polling loop for durable child work. The worker
// is intentionally owned by the executor and exits with ctx; each item still
// has an independent database lease and fencing token.
func (executor *Executor) StartWorker(ctx context.Context, pollInterval time.Duration) {
	if executor == nil || executor.queue == nil || strings.TrimSpace(executor.workerOwner) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	go func() {
		poll := func() {
			if _, err := executor.RunOneQueuedWork(ctx); err != nil {
				log.WarnfCtx(ctx, "[delegation] durable worker poll failed: %v", err)
			}
		}
		poll()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
}

// RunOneQueuedWork re-dispatches one expired or ready child. It is safe to call
// from multiple worker instances: the row claim is the single-winner fence.
func (executor *Executor) RunOneQueuedWork(ctx context.Context) (bool, error) {
	if executor == nil || executor.queue == nil {
		return false, fmt.Errorf("durable delegation queue is not configured")
	}
	var item agentrun.WorkItem
	var err error
	claimCtx, cancelClaim := executor.durableContext(ctx)
	if typed, ok := executor.queue.(kindWorkQueue); ok {
		item, err = typed.ClaimWorkItemByKind(claimCtx, "delegation_child", executor.workerOwner, time.Now().UTC(), executor.workerLeaseTTL)
	} else {
		item, err = executor.queue.ClaimWorkItem(claimCtx, executor.workerOwner, time.Now().UTC(), executor.workerLeaseTTL)
	}
	cancelClaim()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if item.Kind != "delegation_child" {
		_ = executor.completeWorkItem(ctx, item, agentrun.WorkFailed, "unsupported work kind")
		return true, fmt.Errorf("unsupported work kind %q", item.Kind)
	}
	var work queuedDelegationWork
	if err := json.Unmarshal(item.Payload, &work); err != nil {
		_ = executor.completeWorkItem(ctx, item, agentrun.WorkFailed, err.Error())
		return true, err
	}
	parent := work.Parent
	prepared, code, err := executor.prepareTask(parent, item.DelegationID, item.TaskIndex, work.Task, map[string]struct{}{})
	if err != nil {
		_ = executor.completeWorkItem(ctx, item, agentrun.WorkFailed, err.Error())
		return true, fmt.Errorf("prepare queued child: %s: %w", code, err)
	}
	readCtx, cancelRead := executor.durableContext(ctx)
	record, _, err := executor.persistence.GetDelegationTask(readCtx, parent.RunID, item.DelegationID, item.TaskIndex)
	cancelRead()
	if err != nil {
		_ = executor.completeWorkItem(ctx, item, agentrun.WorkFailed, err.Error())
		return true, err
	}
	workCtx, stopHeartbeat := executor.withWorkLease(ctx, item)
	outcome := executor.runTask(workCtx, parent, item.DelegationID, prepared, record)
	stopHeartbeat()
	state := agentrun.WorkSucceeded
	lastError := ""
	if outcome.report.Status != agentapi.DelegationCompleted && outcome.report.Status != agentapi.DelegationPartial {
		state = agentrun.WorkFailed
		if outcome.report.Error != nil {
			lastError = outcome.report.Error.Message
		}
	}
	return true, executor.completeWorkItem(ctx, item, state, lastError)
}

func (executor *Executor) completeWorkItem(ctx context.Context, item agentrun.WorkItem, state, lastError string) error {
	completeCtx, cancelComplete := executor.cleanupContext(ctx)
	err := executor.queue.CompleteWorkItem(completeCtx, item.WorkID, item.LeaseOwner, item.LeaseFence, state, lastError)
	cancelComplete()
	return err
}

func (executor *Executor) runTasks(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	tasks []preparedTask,
	records map[int]agentrun.DelegationTaskRecord,
) []indexedOutcome {
	outcomes := make([]indexedOutcome, len(tasks))
	jobs := make(chan int)
	workers := min(executor.policy.MaxConcurrent, len(tasks))
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wg.Done()
			for taskOffset := range jobs {
				task := tasks[taskOffset]
				outcome := executor.runQueuedOrInline(
					ctx, parent, delegationID, task, records[task.index],
				)
				outcomes[taskOffset] = indexedOutcome{
					index: task.index, taskOutcome: outcome,
				}
			}
		}()
	}
	for index := range tasks {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].index < outcomes[j].index
	})
	return outcomes
}

type indexedOutcome struct {
	index int
	taskOutcome
}

func (outcome indexedOutcome) reportIndex() int { return outcome.index }

func (executor *Executor) runTask(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	record agentrun.DelegationTaskRecord,
) taskOutcome {
	key := fmt.Sprintf("%s\x00%s\x00%d", parent.RunID, delegationID, task.index)
	executor.mu.Lock()
	if existing := executor.flights[key]; existing != nil {
		executor.mu.Unlock()
		select {
		case <-ctx.Done():
			return taskOutcome{report: cancelledReport(task, ErrorParentCancelled, ctx.Err())}
		case <-existing.done:
			return taskOutcome{
				report:       existing.report,
				evidence:     cloneEvidenceUnits(existing.evidence),
				observations: cloneEvidenceObservations(existing.observations),
			}
		}
	}
	flight := &taskFlight{done: make(chan struct{})}
	executor.flights[key] = flight
	executor.mu.Unlock()
	outcome := executor.runTaskOwned(ctx, parent, delegationID, task, record)
	executor.mu.Lock()
	flight.report = outcome.report
	flight.evidence = cloneEvidenceUnits(outcome.evidence)
	flight.observations = cloneEvidenceObservations(outcome.observations)
	close(flight.done)
	delete(executor.flights, key)
	executor.mu.Unlock()
	return outcome
}

func (executor *Executor) runTaskOwned(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	record agentrun.DelegationTaskRecord,
) taskOutcome {
	if task.budget != nil {
		defer func() { _ = task.budget.Release() }()
	}
	if record.Existing && record.SettledUsage != nil {
		return executor.replayTask(ctx, parent, delegationID, task)
	}

	attemptStore, _ := executor.persistence.(AttemptPersistence)
	maxAttempts := 1 + task.definition.FailurePolicy.MaxInfrastructureRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	attemptNo := 1
	if record.Existing && attemptStore != nil {
		latest, err := attemptStore.GetLatestDelegationAttempt(
			ctx, parent.RunID, delegationID, task.index,
		)
		if err == nil {
			attemptNo = latest.AttemptNo + 1
			// The task projection follows the latest physical child attempt.
			// Its immutable logical report ID remains unchanged.
			if latest.ChildRunID != "" {
				task.childRunID = latest.ChildRunID
			}
			if attemptNo > maxAttempts ||
				(latest.Status != agentrun.DelegationAttemptRunning && !latest.Retryable) {
				report := interruptedReport(task)
				executor.settleUnavailable(ctx, parent, delegationID, task, report)
				return taskOutcome{report: report}
			}
			if latest.Status == agentrun.DelegationAttemptRunning {
				// A process restart can leave the previous physical child in
				// running state. Close that attempt before starting its bounded
				// recovery attempt; otherwise the attempt history is ambiguous.
				interrupted := &agentapi.RunError{
					Code:    ErrorInterrupted,
					Message: "delegation attempt was interrupted before a durable result was available",
				}
				if err := executor.finishAttempt(
					ctx, parent, delegationID, task, latest.AttemptNo, latest.AttemptID,
					agentrun.DelegationAttemptInterrupted, true, interrupted, latest.Usage, "", "",
				); err != nil {
					report := failedReport(task, ErrorReportPersistenceFailed, err)
					executor.settleUnavailable(ctx, parent, delegationID, task, report)
					return taskOutcome{report: report}
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			report := failedReport(task, ErrorReportPersistenceFailed, err)
			executor.settleUnavailable(ctx, parent, delegationID, task, report)
			return taskOutcome{report: report}
		}
	}
	if attemptNo < 1 {
		attemptNo = 1
	}

	slot := executor.capabilitySlot(task.capability)
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		report := cancelledReport(task, ErrorParentCancelled, ctx.Err())
		executor.settleUnavailable(ctx, parent, delegationID, task, report)
		return taskOutcome{report: report}
	}
	// When the capability slot and cancellation become ready together, the
	// select above may legally choose the slot. Re-check before linking or
	// invoking the child so queued work never starts after parent cancellation.
	if err := ctx.Err(); err != nil {
		report := cancelledReport(task, ErrorParentCancelled, err)
		executor.settleUnavailable(ctx, parent, delegationID, task, report)
		return taskOutcome{report: report}
	}
	// The task may have waited in the worker queue while another child used the
	// capability slot. Recompute only the time limit at admission so queued work
	// gets a fresh child window without expanding any token or cost reservation.
	limits, err := executor.childLimitsForContext(ctx, parent, task.definition)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			report := cancelledReport(task, ErrorParentCancelled, ctxErr)
			executor.settleUnavailable(ctx, parent, delegationID, task, report)
			return taskOutcome{report: report}
		}
		report := failedReport(task, ErrorParentTimeInsufficient, err)
		executor.settleUnavailable(ctx, parent, delegationID, task, report)
		return taskOutcome{report: report}
	}
	task.limits = limits

	for {
		attemptTask := task
		if attemptNo > 1 {
			attemptTask.childRunID = stableID(
				"run_child_attempt", task.childRunID, fmt.Sprintf("%d", attemptNo),
			)
		}
		attemptID := stableID(
			"attempt", parent.RunID, delegationID, fmt.Sprintf("%d", task.index),
			fmt.Sprintf("%d", attemptNo),
		)
		if attemptStore != nil {
			attemptCtx, cancelAttempt := executor.cleanupContext(ctx)
			_, err := attemptStore.StartDelegationAttempt(
				attemptCtx, agentrun.DelegationAttemptStart{
					ParentRunID: parent.RunID, DelegationID: delegationID,
					TaskIndex: task.index, AttemptNo: attemptNo, AttemptID: attemptID,
					ChildRunID: attemptTask.childRunID,
					StartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
				},
			)
			cancelAttempt()
			if err != nil {
				report := failedReport(attemptTask, ErrorReportPersistenceFailed, err)
				executor.settleUnavailable(ctx, parent, delegationID, attemptTask, report)
				return taskOutcome{report: report}
			}
		}
		if attemptNo == 1 {
			linkCtx, cancelLink := executor.cleanupContext(ctx)
			err := executor.persistence.LinkDelegationChild(
				linkCtx, parent.RunID, delegationID, task.index, attemptTask.childRunID,
			)
			cancelLink()
			if err != nil {
				report := failedReport(attemptTask, ErrorChildExecution, err)
				executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
					agentrun.DelegationAttemptFailed, false, report.Error, nil, "", "")
				executor.settleUnavailable(ctx, parent, delegationID, attemptTask, report)
				return taskOutcome{report: report}
			}
		} else if attemptStore != nil {
			linkCtx, cancelLink := executor.cleanupContext(ctx)
			err := attemptStore.LinkDelegationAttemptChild(
				linkCtx, parent.RunID, delegationID,
				task.index, attemptNo, attemptTask.childRunID,
			)
			cancelLink()
			if err != nil {
				report := failedReport(attemptTask, ErrorReportPersistenceFailed, err)
				executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
					agentrun.DelegationAttemptFailed, false, report.Error, nil, "", "")
				executor.settleUnavailable(ctx, parent, delegationID, attemptTask, report)
				return taskOutcome{report: report}
			}
		} else {
			// Legacy embedders without attempt persistence cannot safely switch the
			// task's durable child identity, so keep retries on the stable child ID.
			attemptTask.childRunID = task.childRunID
		}

		started := time.Now()
		executor.emit(
			agentrun.EventDelegationStarted, parent, delegationID,
			attemptTask.childRunID, attemptTask.request, "running", "", "", 0, agentapi.Usage{},
		)
		executor.markCheckpoint(ctx, parent, delegationID, attemptTask, "pending", "", "")
		stopToolProjection := func() {}
		if projector, ok := executor.runtime.(toolEventProjector); ok {
			stopToolProjection = projector.ProjectToolEvents(
				attemptTask.childRunID, parent.RunID, "", attemptTask.childRunID,
			)
		}
		runCtx, cancel := context.WithDeadline(ctx, attemptTask.limits.Deadline)
		if attemptTask.budget != nil {
			runCtx = agentapi.WithRunBudgetGate(runCtx, attemptTask.budget)
		}
		result, runErr := executor.runtime.Run(runCtx, executor.runRequest(parent, delegationID, attemptTask))
		childErr := runCtx.Err()
		cancel()
		stopToolProjection()
		result = normalizeChildResult(result, attemptTask, runErr, childErr, ctx)
		if attemptTask.inputTokens > 0 && result.Usage.InputTokens > attemptTask.inputTokens {
			result.Status = agentapi.RunFailed
			result.Error = &agentapi.RunError{Code: ErrorChildInputLimit, Message: "child input token limit exceeded"}
		}
		if attemptTask.outputTokens > 0 && result.Usage.OutputTokens > attemptTask.outputTokens {
			result.Status = agentapi.RunFailed
			result.Error = &agentapi.RunError{Code: ErrorChildOutputLimit, Message: "child output token limit exceeded"}
		}
		retryable := retryableChildResult(result, runErr, childErr, ctx)
		if retryable && attemptNo < maxAttempts {
			delay := retryBackoff(attemptNo)
			next := time.Now().UTC().Add(delay)
			executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
				agentrun.DelegationAttemptFailed, true, result.Error,
				&result.Usage, next.Format(time.RFC3339Nano), "")
			if !waitRetry(ctx, delay, attemptTask.limits.Deadline) {
				retryable = false
			} else {
				attemptNo++
				continue
			}
		}

		flowEvidence := AddEvidenceUnits(
			cloneEvidenceIndex(parent.Evidence), result.EvidenceUnits,
		)
		report, err := projectReportWithEvidence(
			result, attemptTask.capability.ID, attemptTask.reportID, flowEvidence,
		)
		if err != nil {
			report = failedReport(attemptTask, ErrorChildExecution, err)
			report.Usage = publicDelegationUsage(result)
		}
		reportTokens := attemptTask.reportTokens
		if reportTokens <= 0 {
			reportTokens = executor.policy.MaxReportTokens
		}
		report = boundReport(report, reportTokens)
		raw, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			report = failedReport(attemptTask, ErrorReportPersistenceFailed, marshalErr)
			executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
				agentrun.DelegationAttemptFailed, false, report.Error, &result.Usage, "", "")
			executor.settleUnavailable(ctx, parent, delegationID, attemptTask, report)
			executor.emitTerminal(parent, delegationID, attemptTask, report, started)
			return taskOutcome{report: report}
		}
		artifact := &agentrun.DelegationArtifact{
			ID: attemptTask.artifactID, RunID: attemptTask.childRunID,
			Kind:        agentrun.DelegationReportArtifactKind,
			Schema:      agentapi.SchemaRef{ID: "delegation.report", Version: 1},
			ContentHash: hashBytes(raw), Content: raw,
		}
		var evidenceArtifact *agentrun.RunArtifact
		if len(result.EvidenceUnits) > 0 {
			persisted, evidenceErr := agentrun.NewEvidenceLedgerArtifact(
				attemptTask.childRunID, result.EvidenceUnits,
			)
			if evidenceErr != nil {
				report = failedReport(attemptTask, ErrorReportPersistenceFailed, evidenceErr)
				report.Usage = publicDelegationUsage(result)
				executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
					agentrun.DelegationAttemptFailed, false, report.Error, &result.Usage, "", "")
				executor.settleUnavailable(ctx, parent, delegationID, attemptTask, report)
				executor.emitTerminal(parent, delegationID, attemptTask, report, started)
				return taskOutcome{report: report}
			}
			evidenceArtifact = &persisted
		}
		settleCtx, cancelSettle := executor.cleanupContext(ctx)
		_, settleErr := executor.persistence.SettleDelegationTask(
			settleCtx,
			agentrun.DelegationSettlement{
				ParentRunID: parent.RunID, DelegationID: delegationID,
				TaskIndex: task.index, ChildRunID: attemptTask.childRunID,
				Usage: result.Usage, Artifact: artifact, EvidenceArtifact: evidenceArtifact,
			},
		)
		cancelSettle()
		if settleErr != nil {
			code := ErrorReportPersistenceFailed
			if errors.Is(settleErr, agentrun.ErrDelegationAccounting) {
				code = ErrorBudgetAccountingViolation
			}
			report = failedReport(attemptTask, code, settleErr)
			report.Usage = publicDelegationUsage(result)
			executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
				agentrun.DelegationAttemptFailed, false, report.Error, &result.Usage, "", "")
			executor.emitTerminal(parent, delegationID, attemptTask, report, started)
			return taskOutcome{report: report}
		}
		status := agentrun.DelegationAttemptSucceeded
		if result.Status == agentapi.RunCancelled {
			status = agentrun.DelegationAttemptCancelled
		} else if result.Status != agentapi.RunSucceeded {
			status = agentrun.DelegationAttemptFailed
			if result.Error != nil && result.Error.Code == ErrorChildTimeout {
				status = agentrun.DelegationAttemptTimedOut
			}
		}
		executor.finishAttempt(ctx, parent, delegationID, task, attemptNo, attemptID,
			status, false, report.Error, &result.Usage, "", attemptTask.artifactID)
		executor.markCheckpoint(ctx, parent, delegationID, attemptTask, "completed", "", attemptTask.artifactID)
		executor.emitTerminal(parent, delegationID, attemptTask, report, started)
		return taskOutcome{
			report: report, evidence: cloneEvidenceUnits(result.EvidenceUnits),
			observations: cloneEvidenceObservations(result.EvidenceObservations),
		}
	}
}

func normalizeChildResult(
	result agentapi.RunResult,
	task preparedTask,
	runErr error,
	childErr error,
	parentCtx context.Context,
) agentapi.RunResult {
	if runErr != nil {
		result = agentapi.RunResult{
			RunID: task.childRunID, Status: agentapi.RunFailed,
			Error: &agentapi.RunError{Code: ErrorChildExecution, Message: runErr.Error(), Retryable: retryableError(runErr)},
		}
	}
	if childErr != nil {
		code := ErrorChildTimeout
		if parentCtx.Err() != nil {
			code = ErrorParentCancelled
		}
		result.RunID = task.childRunID
		result.Status = agentapi.RunCancelled
		result.Error = &agentapi.RunError{Code: code, Message: childErr.Error()}
	}
	if result.RunID == "" {
		result.RunID = task.childRunID
	}
	return result
}

func retryableError(err error) bool {
	if err == nil {
		return false
	}
	var classified interface{ Retryable() bool }
	return errors.As(err, &classified) && classified.Retryable()
}

func retryableChildResult(result agentapi.RunResult, runErr, childErr error, parentCtx context.Context) bool {
	if parentCtx.Err() != nil || childErr != nil || result.Status != agentapi.RunFailed {
		return false
	}
	if result.Error != nil && result.Error.Retryable {
		return true
	}
	return retryableError(runErr)
}

func retryBackoff(attemptNo int) time.Duration {
	if attemptNo < 1 {
		attemptNo = 1
	}
	delay := 50 * time.Millisecond
	for i := 1; i < attemptNo && delay < 2*time.Second; i++ {
		delay *= 2
	}
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	// Stable jitter avoids synchronized retry storms while keeping tests and
	// replay deterministic for the same logical attempt number.
	return delay + time.Duration((attemptNo*17)%25)*time.Millisecond
}

func waitRetry(ctx context.Context, delay time.Duration, deadline time.Time) bool {
	if !deadline.IsZero() && time.Now().Add(delay).After(deadline) {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (executor *Executor) finishAttempt(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	attemptNo int,
	attemptID string,
	status agentrun.DelegationAttemptStatus,
	retryable bool,
	runErr *agentapi.RunError,
	usage *agentapi.Usage,
	nextAttemptAt string,
	reportArtifactID string,
) error {
	attemptStore, ok := executor.persistence.(AttemptPersistence)
	if !ok {
		return nil
	}
	code, message := "", ""
	if runErr != nil {
		code, message = runErr.Code, runErr.Message
	}
	finishCtx, cancelFinish := executor.cleanupContext(ctx)
	_, err := attemptStore.FinishDelegationAttempt(finishCtx, agentrun.DelegationAttemptFinish{
		ParentRunID: parent.RunID, DelegationID: delegationID, TaskIndex: task.index,
		AttemptNo: attemptNo, AttemptID: attemptID, Status: status, Retryable: retryable,
		ErrorCode: code, ErrorMessage: message, EndedAt: time.Now().UTC().Format(time.RFC3339Nano),
		NextAttemptAt: nextAttemptAt, Usage: usage,
		ReportArtifactID: reportArtifactID,
	})
	cancelFinish()
	if err != nil {
		log.ErrorfCtx(ctx, "[delegation] persist attempt finish failed parent=%s delegation=%s task=%d attempt=%d: %v",
			parent.RunID, delegationID, task.index, attemptNo, err)
	}
	return err
}

func (executor *Executor) settleUnavailable(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	report agentapi.DelegationReport,
) {
	settleCtx, cancelSettle := executor.cleanupContext(ctx)
	_, err := executor.persistence.SettleDelegationTask(settleCtx, agentrun.DelegationSettlement{
		ParentRunID: parent.RunID, DelegationID: delegationID,
		TaskIndex: task.index, ChildRunID: task.childRunID,
	})
	cancelSettle()
	if err != nil {
		log.ErrorfCtx(ctx, "[delegation] persist unavailable settlement failed parent=%s delegation=%s task=%d: %v",
			parent.RunID, delegationID, task.index, err)
	}
	executor.markCheckpoint(ctx, parent, delegationID, task, "unavailable", runErrorCode(report.Error), "")
	executor.emitTerminal(parent, delegationID, task, report, time.Time{})
}

func (executor *Executor) markCheckpoint(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
	status, errorCode, artifactID string,
) {
	checkpointStore, ok := executor.persistence.(CheckpointPersistence)
	if !ok {
		return
	}
	checkpointCtx, cancelCheckpoint := executor.cleanupContext(ctx)
	err := checkpointStore.UpsertDelegationCheckpoint(checkpointCtx, agentrun.DelegationCheckpoint{
		ParentRunID: parent.RunID, DelegationID: delegationID, TaskIndex: task.index,
		InvocationID: parent.InvocationID,
		RequestHash:  task.objectiveHash, Status: agentrun.DelegationCheckpointStatus(status),
		ChildRunID: task.childRunID, ReportArtifactID: artifactID,
		ErrorCode: errorCode,
	})
	cancelCheckpoint()
	if err != nil {
		log.ErrorfCtx(ctx, "[delegation] persist checkpoint failed parent=%s delegation=%s task=%d status=%s: %v",
			parent.RunID, delegationID, task.index, status, err)
	}
}

func (executor *Executor) replayTask(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	task preparedTask,
) taskOutcome {
	record, artifact, err := executor.persistence.GetDelegationTask(
		ctx, parent.RunID, delegationID, task.index,
	)
	if err != nil || record.SettledUsage == nil || artifact == nil {
		if err == nil {
			err = fmt.Errorf("settled delegation report is unavailable")
		}
		return taskOutcome{report: failedReport(task, ErrorReportPersistenceFailed, err)}
	}
	// Retry attempts may have a different physical child run identity while
	// retaining the logical report/artifact IDs. Replay must trust the settled
	// task projection, not reconstruct the first-attempt child ID.
	task.childRunID = record.ChildRunID
	if record.ReportArtifactID != "" {
		task.artifactID = record.ReportArtifactID
	}
	report, err := decodePersistedReport(*artifact, task)
	if err != nil {
		return taskOutcome{report: failedReport(task, ErrorReportPersistenceFailed, err)}
	}
	evidence, err := executor.persistence.GetDelegationEvidence(
		ctx,
		parent.RunID,
		delegationID,
		task.index,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return taskOutcome{report: failedReport(
			task,
			ErrorReportPersistenceFailed,
			fmt.Errorf("load delegation evidence: %w", err),
		)}
	}
	return taskOutcome{
		report: report, evidence: cloneEvidenceUnits(evidence),
	}
}

func decodePersistedReport(
	artifact agentrun.DelegationArtifact,
	task preparedTask,
) (agentapi.DelegationReport, error) {
	if artifact.ID != task.artifactID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact ID mismatch",
		)
	}
	if artifact.RunID != task.childRunID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact run ID mismatch",
		)
	}
	if artifact.Kind != agentrun.DelegationReportArtifactKind {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact kind mismatch",
		)
	}
	if artifact.Schema.ID != "delegation.report" ||
		artifact.Schema.Version != 1 {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact schema mismatch",
		)
	}
	if hashBytes(artifact.Content) != artifact.ContentHash {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report artifact hash mismatch",
		)
	}
	var report agentapi.DelegationReport
	if err := json.Unmarshal(artifact.Content, &report); err != nil {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"decode delegation report artifact: %w",
			err,
		)
	}
	if report.RunID != task.childRunID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report run ID mismatch",
		)
	}
	if report.ReportID != task.reportID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report ID mismatch",
		)
	}
	if report.Capability != task.capability.ID {
		return agentapi.DelegationReport{}, fmt.Errorf(
			"delegation report capability mismatch",
		)
	}
	return report, nil
}

func (executor *Executor) reject(
	ctx context.Context,
	parent ParentContext,
	delegationID string,
	index int,
	task agentapi.DelegationTask,
	capability agentapi.Capability,
	objectiveHash,
	code string,
	rejection error,
) (agentapi.DelegationReport, error) {
	ref := agentapi.CapabilityRef{ID: strings.TrimSpace(task.Capability)}
	hash := ""
	if capability.ID != "" {
		ref = agentapi.CapabilityRef{ID: capability.ID, Version: capability.Version}
		hash = capability.ContentHash
	}
	if objectiveHash == "" {
		objectiveHash = hashJSON(task)
	}
	rejectCtx, cancelReject := executor.cleanupContext(ctx)
	_, err := executor.persistence.RejectDelegationTask(
		rejectCtx,
		agentrun.DelegationRejection{
			ParentRunID: parent.RunID, DelegationID: delegationID,
			TaskIndex: index, Capability: ref, CapabilityHash: hash,
			ObjectiveHash: objectiveHash, Code: code,
		},
	)
	cancelReject()
	if err != nil {
		return agentapi.DelegationReport{}, fmt.Errorf("persist delegation rejection: %w", err)
	}
	report := agentapi.DelegationReport{
		Capability:   strings.TrimSpace(task.Capability),
		Status:       agentapi.DelegationRejected,
		Completeness: agentapi.DelegationIncomplete,
		Error: &agentapi.RunError{
			Code: code, Message: rejection.Error(),
		},
	}
	executor.emit(
		agentrun.EventDelegationRejected, parent, delegationID, "",
		task, string(report.Status), code, "", 0, agentapi.Usage{},
	)
	return report, nil
}

func (executor *Executor) runRequest(
	parent ParentContext,
	delegationID string,
	task preparedTask,
) agentapi.RunRequest {
	input, _ := childInput(parent, delegationID, task)
	tools := make([]string, 0, len(task.capability.ToolIDs))
	for _, id := range task.capability.ToolIDs {
		if id != string(DelegateToolID) {
			tools = append(tools, id)
		}
	}
	return agentapi.RunRequest{
		RunID: task.childRunID,
		Agent: agentapi.DefinitionRef{
			ID: task.definition.ID, Version: task.definition.Version,
		},
		DefinitionHash: task.definition.ContentHash,
		Input:          input,
		Context:        task.context,
		Permissions:    task.permissions,
		ToolScope: agentapi.ToolScope{
			RestrictVisible: true, VisibleToolIDs: tools,
		},
		Policy: agentapi.RunPolicy{
			EvidenceRequired: true, EvidenceSeeded: len(task.context) > 0,
			MaxToolCalls: task.limits.MaxToolCalls,
		},
		Limits: task.limits,
		Delegation: agentapi.RunDelegation{
			DelegationID: delegationID, Depth: parent.Depth + 1,
			Capability: agentapi.CapabilityRef{
				ID: task.capability.ID, Version: task.capability.Version,
			},
			CapabilityContentHash:      task.capability.ContentHash,
			CapabilityRegistryRevision: executor.capabilities.Revision(),
		},
		Actor: parent.Actor,
		Correlation: agentapi.Correlation{
			SessionID:   parent.Correlation.SessionID,
			ParentRunID: parent.RunID,
		},
	}
}

func childInput(
	parent ParentContext,
	delegationID string,
	task preparedTask,
) (json.RawMessage, error) {
	payload := struct {
		Capability            string   `json:"capability"`
		Objective             string   `json:"objective"`
		ParentQuestionSummary string   `json:"parent_question_summary"`
		FocusFacets           []string `json:"focus_facets"`
		EvidenceRefs          []string `json:"evidence_refs"`
		OutputKind            string   `json:"output_kind,omitempty"`
		MaxHops               int      `json:"max_hops,omitempty"`
		DelegationID          string   `json:"delegation_id"`
		ParentRunID           string   `json:"parent_run_id"`
		TaskIndex             int      `json:"task_index"`
	}{
		Capability: task.capability.ID, Objective: task.request.Objective,
		ParentQuestionSummary: boundedSummary(parent.QuestionSummary),
		FocusFacets:           jsonStringArray(task.request.FocusFacets),
		EvidenceRefs:          jsonStringArray(task.request.EvidenceRefs),
		OutputKind:            parent.OutputContract.Kind,
		MaxHops:               parent.OutputContract.MaxHops,
		DelegationID:          delegationID, ParentRunID: parent.RunID, TaskIndex: task.index,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

type childBudget struct {
	turns        int
	toolCalls    int64
	inputTokens  int64
	outputTokens int64
	reportTokens int64
}

func (executor *Executor) childBudget(parent ParentContext) childBudget {
	budget := childBudget{
		turns:        executor.policy.MaxChildTurns,
		toolCalls:    executor.policy.MaxChildToolCalls,
		inputTokens:  executor.policy.MaxChildInputTokens,
		outputTokens: executor.policy.MaxChildOutputTokens,
		reportTokens: executor.policy.MaxReportTokens,
	}
	if parent.OutputContract.Kind != "flow" {
		return budget
	}
	budget.turns = minPositive(budget.turns, flowChildMaxTurns)
	budget.toolCalls = minPositiveInt64(budget.toolCalls, flowChildMaxToolCalls)
	budget.outputTokens = minPositiveInt64(budget.outputTokens, flowChildMaxOutputTokens)
	budget.reportTokens = minPositiveInt64(budget.reportTokens, flowReportMaxTokens)
	return budget
}

func minPositive(left, right int) int {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func minPositiveInt64(left, right int64) int64 {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func taskBudgetGrant(task preparedTask) agentapi.Usage {
	return budgetGrant(task.limits, task.inputTokens, task.outputTokens)
}

func budgetGrant(limits agentapi.RunLimits, fallbackInput, fallbackOutput int64) agentapi.Usage {
	inputTokens := limits.MaxInputTokens
	if inputTokens <= 0 {
		inputTokens = fallbackInput
	}
	outputTokens := limits.MaxOutputTokens
	if outputTokens <= 0 {
		outputTokens = fallbackOutput
	}
	totalTokens := limits.MaxTotalTokens
	if totalTokens <= 0 {
		totalTokens = inputTokens + outputTokens
	}
	return agentapi.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens,
		TotalTokens: totalTokens, CostMicros: limits.MaxCostMicros,
	}
}

func (executor *Executor) childLimits(
	parent ParentContext,
	definition agentapi.Definition,
) (agentapi.RunLimits, error) {
	return executor.childLimitsAt(parent, definition, time.Now().UTC(), time.Time{})
}

func (executor *Executor) childLimitsForContext(
	ctx context.Context,
	parent ParentContext,
	definition agentapi.Definition,
) (agentapi.RunLimits, error) {
	contextDeadline := time.Time{}
	if deadline, ok := ctx.Deadline(); ok {
		contextDeadline = deadline
	}
	return executor.childLimitsAt(parent, definition, time.Now().UTC(), contextDeadline)
}

func (executor *Executor) childLimitsAt(
	parent ParentContext,
	definition agentapi.Definition,
	now, contextDeadline time.Time,
) (agentapi.RunLimits, error) {
	timeout := executor.policy.ChildTimeout
	if timeout >= definition.Budget.Timeout {
		timeout = definition.Budget.Timeout - time.Millisecond
	}
	deadline := now.Add(timeout)
	if !parent.Limits.Deadline.IsZero() && parent.Limits.Deadline.Before(deadline) {
		deadline = parent.Limits.Deadline
	}
	if !contextDeadline.IsZero() && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if !deadline.After(now) {
		return agentapi.RunLimits{}, fmt.Errorf("parent has no remaining execution time")
	}
	budget := executor.childBudget(parent)
	outputTokens := minPositiveInt64(budget.outputTokens, int64(definition.Model.MaxOutputTokens))
	tokens := budget.inputTokens + outputTokens
	cost := estimatedCostMicros(
		budget.inputTokens, outputTokens,
		definition.Model.InputPriceMicrosPerMillionTokens,
		definition.Model.OutputPriceMicrosPerMillionTokens,
	)
	return agentapi.RunLimits{
		Deadline:        deadline,
		MaxSteps:        min(definition.Budget.MaxSteps, budget.turns),
		MaxToolCalls:    clampChildToolCalls(budget.toolCalls, definition.Budget.MaxToolCalls),
		MaxInputTokens:  budget.inputTokens,
		MaxOutputTokens: outputTokens,
		MaxTotalTokens:  tokens, MaxCostMicros: cost,
	}, nil
}

// clampChildToolCalls keeps the child request inside the definition budget.
// A tool-less definition (budget 0) must request 0; asking for the parent
// child-tool cap fails admission with "max_tool_calls exceeds the definition budget".
func clampChildToolCalls(requested, definitionBudget int64) int64 {
	if definitionBudget <= 0 {
		return 0
	}
	if requested <= 0 || requested > definitionBudget {
		return definitionBudget
	}
	return requested
}

func (executor *Executor) capabilitySlot(
	capability agentapi.Capability,
) chan struct{} {
	key := fmt.Sprintf("%s@%d", capability.ID, capability.Version)
	executor.mu.Lock()
	defer executor.mu.Unlock()
	slot := executor.capabilitySlots[key]
	if slot == nil {
		slot = make(chan struct{}, capability.MaxConcurrency)
		executor.capabilitySlots[key] = slot
	}
	return slot
}

func (executor *Executor) emitTerminal(
	parent ParentContext,
	delegationID string,
	task preparedTask,
	report agentapi.DelegationReport,
	started time.Time,
) {
	eventType := agentrun.EventDelegationFailed
	switch report.Status {
	case agentapi.DelegationCompleted, agentapi.DelegationPartial:
		eventType = agentrun.EventDelegationDone
	case agentapi.DelegationCancelled:
		eventType = agentrun.EventDelegationCancelled
	}
	duration := int64(0)
	if !started.IsZero() {
		duration = time.Since(started).Milliseconds()
	}
	raw, _ := json.Marshal(report)
	executor.emitWithDetails(
		eventType, parent, delegationID, task.childRunID, task.request,
		string(report.Status), runErrorCode(report.Error), report.ReportID,
		duration,
		agentapi.Usage{
			InputTokens:     report.Usage.InputTokens,
			OutputTokens:    report.Usage.OutputTokens,
			ReasoningTokens: report.Usage.ReasoningTokens,
			TotalTokens:     report.Usage.TotalTokens,
			CostMicros:      report.Usage.CostMicros,
		},
		agentrun.ExecutionEvent{
			ToolCalls:    report.Usage.ToolCalls,
			ReportBytes:  len(raw),
			Completeness: string(report.Completeness),
		},
	)
}

func (executor *Executor) emit(
	eventType agentrun.EventType,
	parent ParentContext,
	delegationID,
	childRunID string,
	task agentapi.DelegationTask,
	status,
	errorCode,
	reportID string,
	durationMS int64,
	usage agentapi.Usage,
) {
	executor.emitWithDetails(
		eventType,
		parent,
		delegationID,
		childRunID,
		task,
		status,
		errorCode,
		reportID,
		durationMS,
		usage,
		agentrun.ExecutionEvent{},
	)
}

func (executor *Executor) emitWithDetails(
	eventType agentrun.EventType,
	parent ParentContext,
	delegationID,
	childRunID string,
	task agentapi.DelegationTask,
	status,
	errorCode,
	reportID string,
	durationMS int64,
	usage agentapi.Usage,
	details agentrun.ExecutionEvent,
) {
	if executor.events == nil {
		return
	}
	details.RunID = parent.RunID
	details.ParentRunID = parent.RunID
	details.ChildRunID = childRunID
	details.DelegationID = delegationID
	details.Capability = strings.TrimSpace(task.Capability)
	if details.AgentID == "" || details.AgentName == "" {
		agentID, agentName := executor.projectAgent(details.Capability)
		if details.AgentID == "" {
			details.AgentID = agentID
		}
		if details.AgentName == "" {
			details.AgentName = agentName
		}
	}
	details.ObjectiveSummary = truncateText(task.Objective, 240)
	details.Status = status
	details.ErrorCode = errorCode
	details.ReportID = reportID
	details.DurationMS = durationMS
	details.Usage = usage
	executor.events.EmitEvent(eventType, details)
}

func (executor *Executor) projectAgent(capabilityID string) (agentID, agentName string) {
	if executor == nil || executor.capabilities == nil {
		return "", ""
	}
	capability, err := executor.capabilities.Resolve(agentapi.CapabilityRef{
		ID: strings.TrimSpace(capabilityID),
	})
	if err != nil {
		return "", ""
	}
	agentID = strings.TrimSpace(capability.Agent.ID)
	if executor.definitions == nil || agentID == "" {
		return agentID, ""
	}
	definition, err := executor.definitions.Resolve(capability.Agent)
	if err != nil {
		return agentID, ""
	}
	return agentID, strings.TrimSpace(definition.DisplayName)
}

func (executor *Executor) emitValidation(
	parent ParentContext,
	delegationID string,
	validation agentapi.DelegationValidation,
	err error,
) {
	if executor.events == nil {
		return
	}
	status := "completed"
	errorCode := ""
	if err != nil {
		status = "failed"
		errorCode = "validation_failed"
	}
	executor.events.EmitEvent(
		agentrun.EventDelegationValidated,
		agentrun.ExecutionEvent{
			RunID: parent.RunID, ParentRunID: parent.RunID,
			DelegationID:            delegationID,
			Status:                  status,
			ErrorCode:               errorCode,
			CitationCoverage:        validation.CitationCoverage,
			EvidenceBodyCoverage:    validation.EvidenceBodyCoverage,
			StructuredClaimCoverage: validation.StructuredClaimCoverage,
			ConflictCount:           len(validation.Conflicts),
			RequiresVerification:    validation.RequiresVerification,
			VerificationReasons: append(
				[]string(nil),
				validation.VerificationReasons...,
			),
		},
	)
}

func normalizePolicy(
	policy agentapi.DelegationPolicy,
) (agentapi.DelegationPolicy, error) {
	if policy.MaxDepth <= 0 || policy.MaxDepth > 1 {
		return policy, fmt.Errorf("delegation max depth must be 1")
	}
	if policy.MaxChildren <= 0 || policy.MaxConcurrent <= 0 ||
		policy.MaxConcurrent > policy.MaxChildren ||
		policy.MaxChildTurns <= 0 || policy.MaxChildToolCalls <= 0 ||
		policy.MaxChildInputTokens <= 0 || policy.MaxChildOutputTokens <= 0 ||
		policy.MaxReportTokens <= 0 || policy.MaxTotalTokens <= 0 ||
		policy.MaxTotalCostMicros < 0 || policy.ParentAnswerReserve < 0 ||
		policy.ChildTimeout <= 0 {
		return policy, fmt.Errorf("delegation policy limits are invalid")
	}
	if policy.MaxReportTokens < minimumBoundedReportTokens() {
		return policy, fmt.Errorf(
			"delegation max report tokens must be at least %d",
			minimumBoundedReportTokens(),
		)
	}
	return policy, nil
}

func selectContext(
	parent ParentContext,
	references,
	facets []string,
	maxTokens int64,
) []agentapi.ContextBlock {
	if len(references) == 0 || len(parent.Context) == 0 {
		return nil
	}
	facetSet := make(map[string]struct{}, len(facets))
	for _, facet := range facets {
		facetSet[facet] = struct{}{}
	}
	seen := make(map[string]struct{}, len(references))
	var blocks []agentapi.ContextBlock
	remainingBytes := int(maxTokens * 4 / 2)
	for _, reference := range references {
		block, ok := parent.Context[reference]
		if !ok {
			continue
		}
		key := block.ContentHash + "\x00" + block.Source + "\x00" + block.Title
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		// ParentContext normally arrives through WithParentContext, but keep
		// this child boundary defensive for callers/tests that construct it
		// directly. Never pass malformed evidence to the definition runtime.
		filtered := cloneContextBlock(block)
		filtered.Evidence = make([]tool.EvidenceUnit, 0, len(block.Evidence))
		for _, rawUnit := range block.Evidence {
			unit, ok := canonicalContextEvidenceUnit(rawUnit)
			if ok && evidenceMatchesFacets(unit, facetSet) {
				filtered.Evidence = append(filtered.Evidence, unit)
			}
		}
		if remainingBytes <= 0 {
			break
		}
		filtered.Content = truncateText(filtered.Content, remainingBytes)
		filtered.ContentHash = hashBytes([]byte(filtered.Content))
		remainingBytes -= len(filtered.Content)
		blocks = append(blocks, filtered)
	}
	return blocks
}

func evidenceMatchesFacets(
	unit tool.EvidenceUnit,
	facets map[string]struct{},
) bool {
	if len(facets) == 0 || len(unit.Facets) == 0 {
		return true
	}
	for _, facet := range unit.Facets {
		if _, ok := facets[facet]; ok {
			return true
		}
	}
	return false
}

func intersectPermissions(
	left,
	right agentapi.PermissionPolicy,
) agentapi.PermissionPolicy {
	allowed := make(map[string]struct{}, len(right.Scopes))
	for _, scope := range right.Scopes {
		allowed[scope] = struct{}{}
	}
	var scopes []string
	for _, scope := range left.Scopes {
		if _, ok := allowed[scope]; ok {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return agentapi.PermissionPolicy{Scopes: scopes}
}

func estimatedCostMicros(
	inputTokens,
	outputTokens,
	inputPrice,
	outputPrice int64,
) int64 {
	inputCost := tokenCost(inputTokens, inputPrice)
	outputCost := tokenCost(outputTokens, outputPrice)
	if inputCost > math.MaxInt64-outputCost {
		return math.MaxInt64
	}
	return inputCost + outputCost
}

func tokenCost(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	if tokens > math.MaxInt64/price {
		return math.MaxInt64
	}
	product := tokens * price
	cost := product / 1_000_000
	if product%1_000_000 != 0 {
		cost++
	}
	return cost
}

func estimateTokens(
	input json.RawMessage,
	context []agentapi.ContextBlock,
) int64 {
	bytes := len(input)
	for _, block := range context {
		bytes += len(block.Content)
	}
	return int64((bytes + 3) / 4)
}

func failedReport(
	task preparedTask,
	code string,
	err error,
) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationFailed,
		Completeness: agentapi.DelegationIncomplete,
		Error:        &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func cancelledReport(
	task preparedTask,
	code string,
	err error,
) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationCancelled,
		Completeness: agentapi.DelegationIncomplete,
		Error:        &agentapi.RunError{Code: code, Message: err.Error()},
	}
}

func interruptedReport(task preparedTask) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		RunID: task.childRunID, ReportID: task.reportID,
		Capability:   task.request.Capability,
		Status:       agentapi.DelegationInterrupted,
		Completeness: agentapi.DelegationIncomplete,
		Error: &agentapi.RunError{
			Code:    ErrorInterrupted,
			Message: "delegation admission was recovered without a durable report",
		},
	}
}

func canonicalStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func filterAuthorizedFacets(requested, allowed []string) []string {
	if len(requested) == 0 || len(allowed) == 0 {
		return nil
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, facet := range allowed {
		allow[facet] = struct{}{}
	}
	kept := make([]string, 0, len(requested))
	for _, facet := range requested {
		if _, ok := allow[facet]; ok {
			kept = append(kept, facet)
		}
	}
	return kept
}

func jsonStringArray(values []string) []string {
	out := make([]string, 0, len(values))
	return append(out, values...)
}

func filterAuthorizedRefs(refs []string, parent ParentContext) []string {
	if len(refs) == 0 {
		return nil
	}
	kept := make([]string, 0, len(refs))
	for _, reference := range refs {
		if _, ok := parent.Evidence[reference]; ok {
			kept = append(kept, reference)
			continue
		}
		if _, ok := parent.Context[reference]; ok {
			kept = append(kept, reference)
		}
	}
	return kept
}

func appendUniqueStrings(target []string, values ...string) []string {
	seen := make(map[string]struct{}, len(target)+len(values))
	out := make([]string, 0, len(target)+len(values))
	for _, value := range append(append([]string(nil), target...), values...) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func stableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

func hashJSON(value any) string {
	raw, _ := json.Marshal(value)
	return hashBytes(raw)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func truncateText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		out[index] = cloneEvidenceUnit(unit)
	}
	return out
}
