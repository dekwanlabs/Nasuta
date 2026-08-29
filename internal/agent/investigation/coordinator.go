package investigation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

type Coordinator struct {
	Catalog             *TaskTemplateCatalog
	Schemas             SchemaResolver
	Tools               tool.Snapshot
	Store               RunStore
	Executors           ExecutorRegistry
	Composer            Composer
	Delivery            DeliveryGate
	Lease               LeaseStore
	BudgetLimit         BudgetVector
	CompositionBudget   BudgetVector
	BudgetProfile       BudgetProfile
	PolicyVersion       string
	MaxRounds           int
	MaxTasks            int
	MaxParallelism      int
	MaxAgentParallelism int
	MaxToolParallelism  int
	Observer            ProgressObserver
	cancels             map[string]context.CancelFunc
	cancelMu            sync.Mutex
}

type CoordinatorOptions struct {
	Catalog             *TaskTemplateCatalog
	Schemas             SchemaResolver
	Tools               tool.Snapshot
	Store               RunStore
	Executors           ExecutorRegistry
	Composer            Composer
	Delivery            DeliveryGate
	Lease               LeaseStore
	BudgetLimit         BudgetVector
	CompositionBudget   BudgetVector
	BudgetProfile       BudgetProfile
	PolicyVersion       string
	MaxRounds           int
	MaxTasks            int
	MaxParallelism      int
	MaxAgentParallelism int
	MaxToolParallelism  int
	Observer            ProgressObserver
}

// NewCoordinator creates the isolated  coordinator. Runtime validation stays in Execute so
// callers can construct the coordinator before optional capabilities are wired.
func NewCoordinator(options CoordinatorOptions) *Coordinator {
	return &Coordinator{
		Catalog:             options.Catalog,
		Schemas:             options.Schemas,
		Tools:               options.Tools,
		Store:               options.Store,
		Executors:           options.Executors,
		Composer:            options.Composer,
		Delivery:            options.Delivery,
		Lease:               options.Lease,
		BudgetLimit:         options.BudgetLimit,
		CompositionBudget:   options.CompositionBudget,
		BudgetProfile:       options.BudgetProfile,
		PolicyVersion:       options.PolicyVersion,
		MaxRounds:           options.MaxRounds,
		MaxTasks:            options.MaxTasks,
		MaxParallelism:      options.MaxParallelism,
		MaxAgentParallelism: options.MaxAgentParallelism,
		MaxToolParallelism:  options.MaxToolParallelism,
		Observer:            options.Observer,
		cancels:             make(map[string]context.CancelFunc),
	}
}

// LoadRun returns the persisted investigation snapshot. It is the read boundary
// used by reconcilers and tests; recovery from an unfinished snapshot remains a
// separate transport concern until P1-06 is fully wired.
func (coordinator *Coordinator) LoadRun(_ context.Context, runID string) (InvestigationRun, error) {
	if coordinator == nil || coordinator.Store == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	return coordinator.Store.Get(runID)
}

// Cancel stops an in-flight run or persists cancellation for an unfinished one.
// Cancelling an already terminal run is an idempotent no-op.
func (coordinator *Coordinator) Cancel(ctx context.Context, runID string) error {
	if coordinator == nil || coordinator.Store == nil {
		return fmt.Errorf("run store is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	run, err := coordinator.Store.Get(runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return nil
	}
	coordinator.cancelMu.Lock()
	cancel := coordinator.cancels[runID]
	coordinator.cancelMu.Unlock()
	if cancel != nil {
		cancel()
		return nil
	}
	store := coordinator.Store
	if coordinator.Lease != nil {
		revoker, ok := coordinator.Lease.(LeaseRevoker)
		if !ok {
			return fmt.Errorf("cancel run %q: lease store cannot fence an active remote worker", runID)
		}
		owner, err := newLeaseOwner()
		if err != nil {
			return err
		}
		grant, err := revoker.RevokeLeaseWithToken(ctx, runID, owner, coordinator.leaseTTL())
		if err != nil {
			return fmt.Errorf("cancel run %q: revoke worker lease: %w", runID, err)
		}
		if fencing, ok := coordinator.Lease.(FencingLeaseStore); ok {
			defer func() {
				_ = fencing.ReleaseLeaseWithToken(context.WithoutCancel(ctx), runID, owner, grant.Token)
			}()
		}
		if adopter, ok := coordinator.Store.(fencingTokenAdopter); ok {
			if err := adopter.adoptFencingToken(runID, owner, grant.Token); err != nil {
				return fmt.Errorf("cancel run %q: adopt cancellation fence: %w", runID, err)
			}
		}
		store = bindLeaseRunStore(coordinator.Store, coordinator.Lease, runID, owner, grant.Token)
	}
	return store.Fail(runID, RunFailure{
		Code: FailureCancelled, Message: "investigation cancelled", Stage: string(StageExecution), Retryable: false,
	}, RunCancelled)
}

// Replay loads the current snapshot together with its persisted event sequence.
// Recovery uses the snapshot for state and the event log for observability and
// consistency checks rather than maintaining a second in-memory state graph.
func (coordinator *Coordinator) Replay(
	_ context.Context,
	runID string,
) (InvestigationRun, []RunEvent, error) {
	if coordinator == nil || coordinator.Store == nil {
		return InvestigationRun{}, nil, fmt.Errorf("run store is required")
	}
	run, err := coordinator.Store.Get(runID)
	if err != nil {
		return InvestigationRun{}, nil, err
	}
	events, err := coordinator.Store.Events(runID)
	if err != nil {
		return InvestigationRun{}, nil, err
	}
	return run, events, nil
}

func (coordinator *Coordinator) acquireLease(
	ctx context.Context,
	runID string,
) (string, uint64, func(), error) {
	if coordinator.Lease == nil {
		return "", 0, func() {}, nil
	}
	owner, err := newLeaseOwner()
	if err != nil {
		return "", 0, nil, err
	}
	ttl := coordinator.leaseTTL()
	var token uint64
	if fencing, ok := coordinator.Lease.(FencingLeaseStore); ok {
		grant, err := fencing.AcquireLeaseWithToken(ctx, runID, owner, ttl)
		if err != nil {
			return "", 0, nil, fmt.Errorf("acquire run %q lease: %w", runID, err)
		}
		token = grant.Token
	} else if err := coordinator.Lease.AcquireLease(ctx, runID, owner, ttl); err != nil {
		return "", 0, nil, fmt.Errorf("acquire run %q lease: %w", runID, err)
	}
	release := func() {
		releaseCtx := context.WithoutCancel(ctx)
		if fencing, ok := coordinator.Lease.(FencingLeaseStore); ok && token > 0 {
			_ = fencing.ReleaseLeaseWithToken(releaseCtx, runID, owner, token)
			return
		}
		_ = coordinator.Lease.ReleaseLease(releaseCtx, runID, owner)
	}
	return owner, token, release, nil
}

func newLeaseOwner() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate investigation lease owner: %w", err)
	}
	return "lease-" + hex.EncodeToString(entropy[:]), nil
}

func (coordinator *Coordinator) leaseTTL() time.Duration {
	if coordinator.BudgetLimit.Duration > 0 {
		return coordinator.BudgetLimit.Duration
	}
	return 10 * time.Minute
}

// withLeaseRenewal makes lease loss visible to the active run instead of
// allowing a worker to continue writing after another worker takes ownership.
func (coordinator *Coordinator) withLeaseRenewal(
	parent context.Context,
	runID string,
	owner string, tokens ...uint64,
) (context.Context, func()) {
	if coordinator.Lease == nil || owner == "" {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancelCause(parent)
	var token uint64
	if len(tokens) > 0 {
		token = tokens[0]
	}
	interval := coordinator.leaseTTL() / 3
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ticker.C:
				var err error
				if fencing, ok := coordinator.Lease.(FencingLeaseStore); ok && token > 0 {
					err = fencing.RenewLeaseWithToken(ctx, runID, owner, token, coordinator.leaseTTL())
				} else {
					err = coordinator.Lease.RenewLease(ctx, runID, owner, coordinator.leaseTTL())
				}
				if err != nil {
					cancel(fmt.Errorf("%w: renew run %q lease owned by %q: %w", ErrLeaseFenced, runID, owner, err))
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return ctx, func() {
		cancel(context.Canceled)
		<-done
	}
}

func appendStructuredRunEvent(store RunStore, runID, eventType string, fields map[string]any) error {
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	if err := store.AppendEvent(runID, eventType, string(payload)); err != nil {
		return fmt.Errorf("append %s event: %w", eventType, err)
	}
	return nil
}

func failureWithPersistenceError(failure RunFailure, operation string, err error) RunFailure {
	if err == nil {
		return failure
	}
	if strings.TrimSpace(failure.Message) == "" {
		failure.Message = string(failure.Code)
	}
	failure.Message = fmt.Sprintf("%s; %s: %v", failure.Message, operation, err)
	return failure
}

func persistResumeFailure(
	store RunStore,
	runID string,
	ledger *BudgetLedger,
	failure RunFailure,
	status RunStatus,
) (InvestigationRun, error) {
	if failure.Message == "" {
		failure.Message = string(failure.Code)
	}
	status = runFailureStatusForFailure(failure, status)
	resultErr := failureError(failure)
	if ledger != nil {
		if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("persist run budget: %w", err))
		}
	}
	if err := store.Fail(runID, failure, status); err != nil {
		return InvestigationRun{}, errors.Join(resultErr, fmt.Errorf("persist run failure: %w", err))
	}
	failedRun, err := store.Get(runID)
	if err != nil {
		return InvestigationRun{}, errors.Join(resultErr, fmt.Errorf("load failed run: %w", err))
	}
	return failedRun, resultErr
}

// Resume continues a non-terminal run whose evidence, claims, and task results
// were persisted by Execute. Planned and executing runs resume pending tasks;
// later stages rebuild the report or delivery from the persisted ledgers.
func (coordinator *Coordinator) Resume(
	ctx context.Context,
	runID string,
) (InvestigationRun, error) {
	if coordinator == nil || coordinator.Store == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	run, _, err := coordinator.Replay(ctx, runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	if err := ValidateContractVersion(run.Contract); err != nil {
		return InvestigationRun{}, fmt.Errorf("resume run %q: %w", runID, err)
	}
	if run.Status.Terminal() {
		return run, nil
	}
	leaseOwner, leaseToken, releaseLease, err := coordinator.acquireLease(ctx, runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	if releaseLease != nil {
		defer releaseLease()
	}
	ctx, stopLeaseRenewal := coordinator.withLeaseRenewal(ctx, runID, leaseOwner, leaseToken)
	defer stopLeaseRenewal()
	if adopter, ok := coordinator.Store.(fencingTokenAdopter); ok && leaseToken > 0 {
		if err := adopter.adoptFencingToken(runID, leaseOwner, leaseToken); err != nil {
			return InvestigationRun{}, fmt.Errorf("adopt run %q fencing token: %w", runID, err)
		}
	}
	store := bindLeaseRunStore(coordinator.Store, coordinator.Lease, runID, leaseOwner, leaseToken)
	if run.Status != RunPlanned && run.Status != RunExecuting &&
		run.Status != RunVerifying && run.Status != RunReplanning &&
		run.Status != RunComposing {
		return InvestigationRun{}, fmt.Errorf("%w: resume from %q is unsupported", ErrInvalidTransition, run.Status)
	}
	if len(run.Plan.Tasks) == 0 {
		return InvestigationRun{}, fmt.Errorf("run %q has no persisted plan", runID)
	}
	ledger, err := NewBudgetLedger(run.Budget.Run.Limit)
	if err != nil {
		return InvestigationRun{}, err
	}
	if err := ledger.Restore(run.Budget); err != nil {
		return InvestigationRun{}, err
	}
	if coordinator.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
		if err := validateVerifierPlan(run.Plan.Tasks); err != nil {
			return persistResumeFailure(store, runID, ledger, planFailure(err), RunFailed)
		}
	}
	evidence := NewEvidenceLedgerFrom(run.Evidence)
	claims := NewClaimLedgerFrom(run.Contract.EvidenceGoals, evidence, run.Claims)
	failures := append([]RunFailure(nil), run.Report.Failures...)
	tasks := make([]ExecutableTask, 0, len(run.Plan.Tasks))
	seededResults := make(map[string]ScheduledTaskResult, len(run.Results))
	seededOutputs := make(map[string]json.RawMessage, len(run.Results))
	for _, task := range run.Plan.Tasks {
		record, completed := run.Results[task.ID]
		if !completed {
			task.Status = TaskPending
			tasks = append(tasks, task)
			continue
		}
		task.Status = record.Status
		result := ScheduledTaskResult{
			Task: task, Status: record.Status,
			Result: TaskExecutionResult{
				Output:             record.Output,
				EvidenceCandidates: evidenceCandidatesForTask(run.Evidence, task.ID),
				Usage:              record.Usage,
			},
			Failure:     record.Failure,
			Attempts:    append([]TaskAttempt(nil), record.Attempts...),
			Discoveries: append([]Discovery(nil), record.Discoveries...),
		}
		if (record.Status == TaskSucceeded || record.Status == TaskPartial) && len(record.Output) > 0 {
			// Partial investigators may have durable report context that the
			// verifier needs after a resume. Evidence is restored separately;
			// upstream output must be restored with the same terminal artifact
			// contract.
			seededOutputs[task.ID] = append([]byte(nil), record.Output...)
		}
		seededResults[task.ID] = result
		tasks = append(tasks, task)
	}

	executionResults := orderedTaskResults(tasks, seededResults)
	var requiredFailure *RunFailure
	for _, seeded := range seededResults {
		if failure := requiredTaskFailure(seeded); failure != nil {
			requiredFailure = preferRunFailure(requiredFailure, failure)
		}
	}

	deadlineCtx, cancel := coordinator.withRunDeadline(ctx, run.Budget.Run.Limit)
	coordinator.registerCancel(runID, func() {
		cancel()
		stopLeaseRenewal()
	})
	defer func() {
		coordinator.unregisterCancel(runID)
		cancel()
	}()
	resumedAt := time.Now().UTC()
	resumeStatus := run.Status
	var compositionReservation BudgetReservation
	compositionBudgetUnavailable := false
	releaseComposition := func() {
		if compositionReservation.ID == "" {
			return
		}
		_ = compositionReservation.Release()
		compositionReservation = BudgetReservation{}
	}
	var persistErr error
	var scheduleErr error
	if requiredFailure != nil && !budgetFailureCanDeliverForPlan(tasks, evidence, claims, requiredFailure) {
		report := BuildReport(evidence, claims, failures)
		if err := store.SaveReport(runID, report); err != nil {
			failure := failureWithPersistenceError(*requiredFailure, "save failure report", err)
			return persistResumeFailure(store, runID, ledger, failure, runFailureStatusForFailure(failure, RunFailed))
		}
		return persistResumeFailure(store, runID, ledger, *requiredFailure, runFailureStatusForFailure(*requiredFailure, RunFailed))
	}
	if resumeStatus == RunPlanned || resumeStatus == RunExecuting {
		if resumeStatus == RunPlanned {
			if err := store.Transition(runID, RunExecuting); err != nil {
				return InvestigationRun{}, err
			}
			resumeStatus = RunExecuting
		}
		// Synthesizer budget is deliberately acquired after investigation and
		// verification, never during resumed execution admission.
		verificationAdmissions, err := coordinator.reserveVerification(ledger, tasks)
		if err != nil {
			releaseComposition()
			failure := verifierReservationFailure(err)
			return persistResumeFailure(store, runID, ledger, failure, runFailureStatusForFailure(failure, RunFailed))
		}
		// Composition is optional and runs after execution. Do not keep its
		// reservation alive while Investigators consume the shared Run budget;
		// otherwise the later Verifier can be rejected even though its own floor
		// was protected successfully.
		releaseComposition()
		compositionBudgetUnavailable = false
		scheduler := Scheduler{
			Executors:           coordinator.Executors,
			Schemas:             coordinator.Schemas,
			Ledger:              ledger,
			MaxParallelism:      effectiveParallelism(coordinator.MaxParallelism, run.Plan.Policy.MaxParallelism),
			MaxAgentParallelism: effectiveParallelism(coordinator.MaxAgentParallelism, run.Plan.Policy.MaxParallelism),
			MaxToolParallelism:  effectiveParallelism(coordinator.MaxToolParallelism, run.Plan.Policy.MaxParallelism),
			InitialResults:      seededResults,
			InitialOutputs:      seededOutputs,
			ProtectedAdmissions: verificationAdmissions,
			OnStart: func(task ExecutableTask) {
				coordinator.emitProgress(runID, ProgressTaskStarted, task.ID, task.Executor, directToolID(task), "running", "")
				if err := appendStructuredRunEvent(store, runID, "task_started", map[string]any{
					"task_id": task.ID, "executor": task.Executor, "status": TaskRunning, "evidence_goal_ids": task.EvidenceGoalIDs,
				}); err != nil && persistErr == nil {
					persistErr = err
				}
			},
			OnComplete: func(scheduled ScheduledTaskResult) {
				beforeEvidence := len(evidence.All())
				record, taskFailure := coordinator.admitTaskResult(scheduled, evidence, claims)
				admitted := len(evidence.All()) - beforeEvidence
				if taskFailure != nil {
					failures = append(failures, *taskFailure)
					if !scheduled.Task.Optional {
						requiredFailure = preferRunFailure(requiredFailure, taskFailure)
					}
					if record.Status == TaskSucceeded {
						record.Status = TaskFailed
						record.Failure = taskFailure
					}
				}
				if scheduled.Failure != nil {
					failures = append(failures, *scheduled.Failure)
					if failure := requiredTaskFailure(scheduled); failure != nil {
						requiredFailure = preferRunFailure(requiredFailure, failure)
					}
				}
				if err := store.SaveResult(runID, record); err != nil && persistErr == nil {
					persistErr = err
				}
				if err := store.SaveEvidence(runID, evidence.All()); err != nil && persistErr == nil {
					persistErr = err
				}
				if err := store.SaveClaims(runID, claims.All()); err != nil && persistErr == nil {
					persistErr = err
				}
				fields := taskBudgetEventFields(runID, scheduled, ledger)
				fields["attempt"] = len(scheduled.Attempts)
				fields["usage"] = scheduled.Result.Usage
				fields["evidence_candidates"] = len(scheduled.Result.EvidenceCandidates)
				fields["evidence_admitted"] = admitted
				fields["claims"] = len(scheduled.Result.Claims)
				if scheduled.Failure != nil {
					fields["failure_code"] = scheduled.Failure.Code
				}
				if taskFailure != nil {
					fields["failure_code"] = taskFailure.Code
				}
				if err := appendStructuredRunEvent(store, runID, "task_completed", fields); err != nil && persistErr == nil {
					persistErr = err
				}
				reason := ""
				if scheduled.Failure != nil {
					reason = scheduled.Failure.Message
				}
				coordinator.emitProgress(runID, ProgressTaskCompleted, scheduled.Task.ID, scheduled.Task.Executor, directToolID(scheduled.Task), string(scheduled.Status), reason)
			},
		}
		executionResults, scheduleErr = scheduler.Execute(deadlineCtx, tasks, func(task ExecutableTask, upstream map[string]json.RawMessage) TaskExecutionInput {
			return TaskExecutionInput{
				Task: task, Evidence: evidence.All(), Claims: claims.All(), Upstream: upstream,
				WorkflowRunID: runID, ParentRunID: run.Contract.ParentRunID, Actor: run.Contract.Actor,
			}
		})
		if requiredFailure != nil && !budgetFailureCanDeliverForPlan(tasks, evidence, claims, requiredFailure) {
			report := BuildReport(evidence, claims, failures)
			if err := store.SaveReport(runID, report); err != nil {
				releaseComposition()
				failure := failureWithPersistenceError(*requiredFailure, "save failure report", err)
				return persistResumeFailure(store, runID, ledger, failure, runFailureStatusForFailure(failure, RunFailed))
			}
			releaseComposition()
			return persistResumeFailure(store, runID, ledger, *requiredFailure, runFailureStatusForFailure(*requiredFailure, RunFailed))
		}
		if persistErr != nil {
			releaseComposition()
			return persistResumeFailure(store, runID, ledger, RunFailure{
				Code: FailureExecution, Message: persistErr.Error(), Stage: string(StageExecution),
			}, RunFailed)
		}
		if scheduleErr != nil {
			failure := runFailureFromContext(deadlineCtx, scheduleErr)
			failures = append(failures, failure)
		}
	}
	if requiredFailure != nil && !budgetFailureCanDeliverForPlan(tasks, evidence, claims, requiredFailure) {
		report := BuildReport(evidence, claims, failures)
		if err := store.SaveReport(runID, report); err != nil {
			releaseComposition()
			failure := failureWithPersistenceError(*requiredFailure, "save failure report", err)
			return persistResumeFailure(store, runID, ledger, failure, runFailureStatusForFailure(failure, RunFailed))
		}
		releaseComposition()
		return persistResumeFailure(store, runID, ledger, *requiredFailure, runFailureStatusForFailure(*requiredFailure, RunFailed))
	}
	if scheduleErr != nil {
		releaseComposition()
		return persistResumeFailure(
			store, runID, ledger, runFailureFromContext(deadlineCtx, scheduleErr), runFailureStatus(deadlineCtx, scheduleErr),
		)
	}
	if coordinator.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
		if verifierErr := validateVerifierExecution(tasks, executionResults, evidence, claims); verifierErr != nil {
			report := BuildReport(evidence, claims, failures)
			if err := store.SaveReport(runID, report); err != nil {
				releaseComposition()
				failure := failureWithPersistenceError(*verifierErr, "save failure report", err)
				return persistResumeFailure(store, runID, ledger, failure, runFailureStatusForFailure(failure, RunFailed))
			}
			releaseComposition()
			return persistResumeFailure(store, runID, ledger, *verifierErr, runFailureStatusForFailure(*verifierErr, RunFailed))
		}
	}
	if err := deadlineCtx.Err(); err != nil {
		releaseComposition()
		return persistResumeFailure(
			store, runID, ledger, runFailureFromContext(deadlineCtx, err), runFailureStatus(deadlineCtx, err),
		)
	}
	if resumeStatus == RunExecuting {
		if err := store.Transition(runID, RunVerifying); err != nil {
			releaseComposition()
			return InvestigationRun{}, err
		}
		resumeStatus = RunVerifying
	}
	report := BuildReport(evidence, claims, failures)
	if resumeStatus == RunVerifying || resumeStatus == RunReplanning {
		if err := store.SaveReport(runID, report); err != nil {
			releaseComposition()
			return InvestigationRun{}, err
		}
		if err := store.Transition(runID, RunComposing); err != nil {
			releaseComposition()
			return InvestigationRun{}, err
		}
		resumeStatus = RunComposing
	}
	var compositionErr error
	if compositionReservation.ID == "" {
		compositionReservation, compositionErr = coordinator.reserveComposition(ledger)
		if compositionErr != nil {
			if !errors.Is(compositionErr, ErrBudgetExceeded) {
				return persistResumeFailure(store, runID, ledger, RunFailure{
					Code: FailureComposer, Message: compositionErr.Error(), Stage: string(StageComposition),
				}, RunFailed)
			}
			compositionBudgetUnavailable = true
			compositionReservation = BudgetReservation{}
		}
	}
	composer := composerForDelivery(run.Contract, coordinator.Composer)
	if compositionBudgetUnavailable {
		composer = nil
	}
	composer, compositionReservation, compositionErr = beginComposition(ledger, compositionReservation, composer, report, !isZeroBudget(coordinator.CompositionBudget))
	if compositionErr != nil {
		releaseComposition()
		return persistResumeFailure(store, runID, ledger, RunFailure{
			Code: FailureComposer, Message: compositionErr.Error(), Stage: string(StageComposition),
		}, RunFailed)
	}
	deliveryContext := agentapi.WithRunBudgetGate(deadlineCtx, ledger)
	if compositionReservation.ID != "" {
		deliveryContext = agentapi.WithRunBudgetGate(deliveryContext, reservationBudgetGate{
			ledger: ledger,
			id:     compositionReservation.ID,
		})
	}
	delivery := coordinator.Delivery.Deliver(deliveryContext, run.Contract, report, composer)
	if err := deadlineCtx.Err(); err != nil {
		releaseComposition()
		return persistResumeFailure(store, runID, ledger, runFailureFromContext(deadlineCtx, err), runFailureStatus(deadlineCtx, err))
	}
	if compositionReservation.ID != "" {
		actual := delivery.Usage
		if composer != nil && len(report.Claims) > 0 && isZeroBudget(actual) && delivery.Failure == nil {
			actual.OutputTokens = int64(len(strings.Fields(delivery.Text)))
		}
		if err := compositionReservation.Settle(actual); err != nil {
			releaseComposition()
			return persistResumeFailure(store, runID, ledger, RunFailure{
				Code: FailureBudget, Message: err.Error(), Stage: string(StageComposition),
			}, RunBudgetExhausted)
		}
		compositionReservation = BudgetReservation{}
	}
	if err := store.SaveDelivery(runID, delivery); err != nil {
		releaseComposition()
		return persistResumeFailure(store, runID, ledger, RunFailure{
			Code: FailureEmptyOutput, Message: err.Error(), Stage: string(StageComposition),
		}, RunFailed)
	}
	persisted, err := store.Get(runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	snapshot := ledger.Snapshot()
	taskUsage, templateUsage, goalCoverage := runMetricDimensions(store, runID)
	executorCounts := make(map[ExecutorType]int)
	agentTaskCount := 0
	for taskID := range persisted.Results {
		task, ok := persisted.Tasks[taskID]
		if !ok {
			continue
		}
		executorCounts[task.Executor]++
		if isAgentExecutor(task.Executor) {
			agentTaskCount++
		}
	}
	metrics := persisted.Metrics
	if metrics.Rounds <= 0 {
		metrics.Rounds = 1
	}
	metrics.Tasks = len(persisted.Results)
	metrics.AgentTasks = agentTaskCount
	metrics.ToolCalls = snapshot.Run.Used.ToolCalls
	metrics.InputTokens = snapshot.Run.Used.InputTokens
	metrics.OutputTokens = snapshot.Run.Used.OutputTokens
	metrics.CostMicros = snapshot.Run.Used.CostMicros
	metrics.Duration += time.Since(resumedAt)
	metrics.ComposerFallback = delivery.Failure != nil && delivery.Failure.Code == FailureComposer
	metrics.ExecutorCounts = cloneExecutorCounts(executorCounts)
	metrics.TaskUsage = taskUsage
	metrics.TemplateUsage = templateUsage
	metrics.GoalCoverage = goalCoverage
	var postDeliveryErr error
	if err := store.SaveMetrics(runID, metrics); err != nil {
		postDeliveryErr = err
	}
	if err := appendStructuredRunEvent(store, runID, "delivery_completed", map[string]any{
		"status": delivery.Status, "text_non_empty": strings.TrimSpace(delivery.Text) != "",
		"coverage": len(delivery.Report.Coverage), "gaps": len(delivery.Report.Gaps),
	}); err != nil {
		if postDeliveryErr == nil {
			postDeliveryErr = err
		} else {
			postDeliveryErr = errors.Join(postDeliveryErr, fmt.Errorf("append delivery event: %w", err))
		}
	}
	if err := store.SaveBudget(runID, snapshot); err != nil {
		if postDeliveryErr == nil {
			postDeliveryErr = err
		} else {
			postDeliveryErr = errors.Join(postDeliveryErr, fmt.Errorf("save run budget: %w", err))
		}
	}
	coordinator.emitProgress(runID, ProgressWorkflowCompleted, "", "", "", string(delivery.Status), "")
	completed, err := store.Get(runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	if postDeliveryErr != nil {
		return completed, fmt.Errorf("delivery persisted but post-delivery persistence failed: %w", postDeliveryErr)
	}
	return completed, nil
}

func (coordinator *Coordinator) registerCancel(runID string, cancel context.CancelFunc) {
	coordinator.cancelMu.Lock()
	defer coordinator.cancelMu.Unlock()
	coordinator.cancels[runID] = cancel
}

func (coordinator *Coordinator) unregisterCancel(runID string) {
	coordinator.cancelMu.Lock()
	defer coordinator.cancelMu.Unlock()
	delete(coordinator.cancels, runID)
}

func (coordinator *Coordinator) emitProgress(
	runID string,
	kind ProgressKind,
	nodeID string,
	executor ExecutorType,
	toolID string,
	status string,
	reason string,
) {
	if coordinator == nil || coordinator.Observer == nil {
		return
	}
	coordinator.Observer(ProgressEvent{
		RunID: runID, Kind: kind, NodeID: nodeID, Executor: executor, ToolID: toolID,
		Status: status, Reason: reason,
	})
}

func directToolID(task ExecutableTask) string {
	if task.Executor != ExecutorDirectTool || len(task.ToolCalls) != 1 {
		return ""
	}
	return string(task.ToolCalls[0].ToolID)
}

// Execute runs one immutable contract through plan, execution, verification, and delivery.
// The returned run is the persisted snapshot even when execution fails.
func (coordinator *Coordinator) Execute(
	ctx context.Context,
	contract InvestigationContract,
) (InvestigationRun, error) {
	return coordinator.execute(ctx, contract, nil, nil)
}

// ExecuteWithProposal applies a server-validated task graph before execution.
// The ordinary Execute path remains available for recovery and deterministic plans.
func (coordinator *Coordinator) ExecuteWithProposal(
	ctx context.Context,
	contract InvestigationContract,
	proposal *agentapi.TaskGraphProposal,
) (InvestigationRun, error) {
	return coordinator.execute(ctx, contract, proposal, nil)
}

// ExecuteWithProposalReady starts a proposed run and invokes onPersisted after
// its initial snapshot is durable, before any planning or task execution.
func (coordinator *Coordinator) ExecuteWithProposalReady(
	ctx context.Context,
	contract InvestigationContract,
	proposal *agentapi.TaskGraphProposal,
	onPersisted func(InvestigationRun),
) (InvestigationRun, error) {
	return coordinator.execute(ctx, contract, proposal, onPersisted)
}

// ValidateContractVersion rejects snapshots that cannot satisfy current workflow invariants.
func ValidateContractVersion(contract InvestigationContract) error {
	if contract.Version != InvestigationContractVersion {
		return fmt.Errorf(
			"%w: investigation contract version %d is unsupported; current version is %d",
			ErrPlanInvalid, contract.Version, InvestigationContractVersion,
		)
	}
	return nil
}

func (coordinator *Coordinator) execute(
	ctx context.Context,
	contract InvestigationContract,
	proposal *agentapi.TaskGraphProposal,
	onPersisted func(InvestigationRun),
) (InvestigationRun, error) {
	if coordinator == nil {
		return InvestigationRun{}, fmt.Errorf("investigation coordinator is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return InvestigationRun{}, err
	}
	if coordinator.Store == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	if coordinator.Catalog == nil {
		return InvestigationRun{}, fmt.Errorf("task template catalog is required")
	}
	if coordinator.Executors == nil {
		return InvestigationRun{}, fmt.Errorf("executor registry is required")
	}
	if err := validateBudgetVector(coordinator.BudgetLimit); err != nil {
		return InvestigationRun{}, fmt.Errorf("coordinator budget: %w", err)
	}
	if err := ValidateContractVersion(contract); err != nil {
		return InvestigationRun{}, err
	}
	policy, err := coordinator.executionPolicy(contract, proposal)
	if err != nil {
		return InvestigationRun{}, err
	}
	runLimit := policy.Budget

	contract.ID = strings.TrimSpace(contract.ID)
	contract.Question = strings.TrimSpace(contract.Question)
	if contract.CreatedAt.IsZero() {
		contract.CreatedAt = time.Now().UTC()
	}
	runID := investigationRunID(contract)
	leaseOwner, leaseToken, releaseLease, err := coordinator.acquireLease(ctx, runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	if releaseLease != nil {
		defer releaseLease()
	}
	ctx, stopLeaseRenewal := coordinator.withLeaseRenewal(ctx, runID, leaseOwner, leaseToken)
	defer stopLeaseRenewal()
	store := bindLeaseRunStore(coordinator.Store, coordinator.Lease, runID, leaseOwner, leaseToken)
	if existing, err := store.Get(runID); err == nil {
		if existing.Status.Terminal() {
			if onPersisted != nil {
				onPersisted(existing)
			}
			return existing, nil
		}
		return InvestigationRun{}, fmt.Errorf("%w: run %q is already in progress", ErrInvalidTransition, runID)
	}
	ledger, err := NewBudgetLedger(runLimit)
	if err != nil {
		return InvestigationRun{}, err
	}
	policyVersion := coordinator.PolicyVersion
	if policyVersion == "" {
		policyVersion = DefaultBudgetPolicyVersion
	}
	profile, err := coordinator.resolveBudgetProfile(contract)
	if err != nil {
		return InvestigationRun{}, err
	}
	maxRounds := policy.MaxRounds
	maxTasks := coordinator.maxTasks(contract)
	if proposal != nil && proposal.Stop.MaxTasks > 0 && (maxTasks == 0 || proposal.Stop.MaxTasks < maxTasks) {
		maxTasks = proposal.Stop.MaxTasks
	}
	stageLimits, err := AllocateStageLimits(profile, runLimit)
	if err != nil {
		return InvestigationRun{}, err
	}
	// Token dimensions are owned by the Run-level shared ledger, not by a
	// stage or by an average per-Agent pool. Every stage therefore sees the
	// same Run token hard limit. Downstream protection is represented by the
	// explicit composition reservation, not by a hidden stage token quota.
	for stage, limit := range stageLimits {
		limit.InputTokens = runLimit.InputTokens
		limit.OutputTokens = runLimit.OutputTokens
		limit.TotalTokens = runLimit.TotalTokens
		stageLimits[stage] = limit
	}
	for stage, limit := range stageLimits {
		if err := ledger.SetStageLimit(stage, limit); err != nil {
			return InvestigationRun{}, err
		}
	}
	if err := ledger.SetRunPolicy(maxRounds, maxTasks, policyVersion, profile); err != nil {
		return InvestigationRun{}, err
	}
	run := InvestigationRun{
		ID:       runID,
		Contract: contract,
		Status:   RunCreated,
		Budget:   ledger.Snapshot(),
	}
	if err := store.Create(run); err != nil {
		return InvestigationRun{}, err
	}
	if onPersisted != nil {
		onPersisted(run)
	}

	deadlineCtx, cancel := coordinator.withRunDeadline(ctx, runLimit)
	coordinator.registerCancel(runID, func() {
		cancel()
		stopLeaseRenewal()
	})
	defer func() {
		coordinator.unregisterCancel(runID)
		cancel()
	}()
	startedAt := time.Now().UTC()
	roundCount := 0
	executedTaskCount := 0
	agentTaskCount := 0
	executorCounts := make(map[ExecutorType]int)
	var compositionReservation BudgetReservation
	saveMetrics := func(composerFallback bool) error {
		snapshot := ledger.Snapshot()
		stageUsage := make(map[BudgetStage]BudgetVector, len(snapshot.Stages))
		for stage, budget := range snapshot.Stages {
			stageUsage[stage] = budget.Used
		}
		taskUsage, templateUsage, goalCoverage := runMetricDimensions(
			store, runID,
		)
		return store.SaveMetrics(runID, RunMetrics{
			Rounds:           roundCount,
			Tasks:            executedTaskCount,
			AgentTasks:       agentTaskCount,
			ToolCalls:        snapshot.Run.Used.ToolCalls,
			InputTokens:      snapshot.Run.Used.InputTokens,
			OutputTokens:     snapshot.Run.Used.OutputTokens,
			CostMicros:       snapshot.Run.Used.CostMicros,
			Duration:         time.Since(startedAt),
			ComposerFallback: composerFallback,
			StageUsage:       stageUsage,
			ExecutorCounts:   cloneExecutorCounts(executorCounts),
			TaskUsage:        taskUsage,
			TemplateUsage:    templateUsage,
			GoalCoverage:     goalCoverage,
		})
	}
	fail := func(failure RunFailure, status RunStatus) (InvestigationRun, error) {
		if compositionReservation.ID != "" {
			_ = compositionReservation.Release()
			compositionReservation = BudgetReservation{}
		}
		if failure.Message == "" {
			failure.Message = string(failure.Code)
		}
		status = runFailureStatusForFailure(failure, status)
		resultErr := failureError(failure)
		if err := saveMetrics(false); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("persist run metrics: %w", err))
		}
		coordinator.emitProgress(runID, ProgressWorkflowCompleted, "", "", "", string(status), failure.Message)
		if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("persist run budget: %w", err))
		}
		if err := store.Fail(runID, failure, status); err != nil {
			return InvestigationRun{}, errors.Join(resultErr, fmt.Errorf("persist run failure: %w", err))
		}
		failedRun, getErr := store.Get(runID)
		if getErr != nil {
			return InvestigationRun{}, errors.Join(resultErr, fmt.Errorf("load failed run: %w", getErr))
		}
		return failedRun, resultErr
	}
	transition := func(next RunStatus) error {
		if err := store.Transition(runID, next); err != nil {
			return err
		}
		return store.SaveBudget(runID, ledger.Snapshot())
	}

	if err := transition(RunAnalyzing); err != nil {
		failure := planFailure(err)
		failure.Stage = "analysis"
		return fail(failure, planFailureStatus(err))
	}
	coordinator.emitProgress(runID, ProgressWorkflowStarted, "", "", "", "running", "")

	// Synthesizer is optional. Do not reserve its budget while planning or
	// investigating; request it only after verification has completed.
	compositionBudgetUnavailable := false
	compiler := PlanCompiler{
		Catalog:  coordinator.Catalog,
		Schemas:  coordinator.Schemas,
		Tools:    coordinator.Tools,
		Ledger:   ledger,
		MaxTasks: minPositive(maxTasks, coordinator.planTaskLimit(contract, stageLimits)),
		Overhead: withoutTokenBudget(addVector(
			addVector(stageLimits[StagePlanning], stageLimits[StageVerification]),
			stageLimits[StageFallback],
		)),
	}
	var plan PlanRevision
	if proposal != nil {
		plan, err = compiler.CompileProposal(contract, *proposal)
	} else {
		plan, err = compiler.CompileGenerated(contract)
	}
	if err != nil {
		_ = compositionReservation.Release()
		return fail(planFailure(err), planFailureStatus(err))
	}
	// Keep this check at the execution boundary as well as in the compiler.
	// It protects runs restored from older snapshots and prevents a future
	// planner path from silently bypassing the server-owned verifier.
	if coordinator.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
		if err := validateVerifierPlan(plan.Tasks); err != nil {
			_ = compositionReservation.Release()
			return fail(planFailure(err), RunFailed)
		}
	}
	plan.Policy = policy
	if err := ledger.reallocateAvailable(StagePlanning, StageExecution); err != nil {
		_ = compositionReservation.Release()
		return fail(planFailure(err), planFailureStatus(err))
	}
	if err := transition(RunPlanned); err != nil {
		_ = compositionReservation.Release()
		return fail(planFailure(err), planFailureStatus(err))
	}
	if err := store.SavePlan(runID, plan); err != nil {
		_ = compositionReservation.Release()
		return fail(planFailure(err), planFailureStatus(err))
	}
	if err := appendStructuredRunEvent(store, runID, "plan_compiled", map[string]any{
		"proposal_hash":       plan.ProposalHash,
		"revision":            plan.Revision,
		"task_count":          len(plan.Tasks),
		"evidence_task_count": planEvidenceTaskCount(plan.Tasks),
		"verifier_task_count": planVerifierTaskCount(plan.Tasks),
		"task_ids":            executableTaskIDs(plan.Tasks),
	}); err != nil {
		_ = compositionReservation.Release()
		return fail(planFailure(err), planFailureStatus(err))
	}

	evidence := NewEvidenceLedger()
	claims := NewClaimLedger(contract.EvidenceGoals, evidence)
	for _, unit := range contract.SeedEvidence {
		if _, _, err := evidence.AdmitSeed("seed", unit); err != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{
				Code: FailureSchema, Message: err.Error(),
				Stage: string(StageVerification), Retryable: false,
			}, RunFailed)
		}
	}
	if err := store.SaveEvidence(runID, evidence.All()); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureSchema, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
	}
	if err := store.SaveClaims(runID, claims.All()); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
	}
	if err := appendStructuredRunEvent(store, runID, "evidence_snapshot", map[string]any{
		"admitted": len(evidence.All()), "claims": len(claims.All()), "round": 1,
	}); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
	}
	verificationAdmissions, err := coordinator.reserveVerification(ledger, plan.Tasks)
	if err != nil {
		_ = compositionReservation.Release()
		failure := verifierReservationFailure(err)
		return fail(failure, runFailureStatusForFailure(failure, RunFailed))
	}
	// Composition is admitted at the composition stage, not during evidence
	// collection. Holding this reservation through Investigator execution would
	// turn an optional final response into a sibling reservation that starves the
	// mandatory Verifier.
	_ = compositionReservation.Release()
	compositionReservation = BudgetReservation{}
	compositionBudgetUnavailable = false
	failures := make([]RunFailure, 0)
	var requiredFailure *RunFailure
	var persistErr error
	evidenceCandidateCount := 0
	duplicateEvidenceCount := 0
	duplicateStop := false
	var stopRound context.CancelFunc
	scheduler := Scheduler{
		Executors:           coordinator.Executors,
		Schemas:             coordinator.Schemas,
		Ledger:              ledger,
		MaxParallelism:      effectiveParallelism(coordinator.MaxParallelism, policy.MaxParallelism),
		MaxAgentParallelism: effectiveParallelism(coordinator.MaxAgentParallelism, policy.MaxParallelism),
		MaxToolParallelism:  effectiveParallelism(coordinator.MaxToolParallelism, policy.MaxParallelism),
		ProtectedAdmissions: verificationAdmissions,
		OnStart: func(task ExecutableTask) {
			coordinator.emitProgress(runID, ProgressTaskStarted, task.ID, task.Executor, directToolID(task), "running", "")
			if err := appendStructuredRunEvent(store, runID, "task_started", map[string]any{
				"task_id": task.ID, "executor": task.Executor, "status": TaskRunning, "evidence_goal_ids": task.EvidenceGoalIDs,
			}); err != nil && persistErr == nil {
				persistErr = err
			}
		},
		OnComplete: func(scheduled ScheduledTaskResult) {
			beforeEvidence := len(evidence.All())
			evidenceCandidateCount += len(scheduled.Result.EvidenceCandidates)
			record, taskFailure := coordinator.admitTaskResult(scheduled, evidence, claims)
			afterEvidence := len(evidence.All())
			admitted := afterEvidence - beforeEvidence
			if duplicateCount := len(scheduled.Result.EvidenceCandidates) - admitted; duplicateCount > 0 {
				duplicateEvidenceCount += duplicateCount
			}
			if policy.MaxDuplicateRatio > 0 && evidenceCandidateCount > 0 &&
				float64(duplicateEvidenceCount)/float64(evidenceCandidateCount) > policy.MaxDuplicateRatio {
				duplicateStop = true
				if stopRound != nil {
					stopRound()
				}
			}
			if taskFailure != nil {
				failures = append(failures, *taskFailure)
				if !scheduled.Task.Optional {
					requiredFailure = preferRunFailure(requiredFailure, taskFailure)
				}
				if record.Status == TaskSucceeded {
					record.Status = TaskFailed
					record.Failure = taskFailure
				}
			}
			if scheduled.Failure != nil {
				failures = append(failures, *scheduled.Failure)
				if failure := requiredTaskFailure(scheduled); failure != nil {
					requiredFailure = preferRunFailure(requiredFailure, failure)
				}
			}
			if err := store.SaveResult(runID, record); err != nil {
				if persistErr == nil {
					persistErr = err
				}
			}
			if err := store.SaveEvidence(runID, evidence.All()); err != nil {
				if persistErr == nil {
					persistErr = err
				}
			}
			if err := store.SaveClaims(runID, claims.All()); err != nil {
				if persistErr == nil {
					persistErr = err
				}
			}
			fields := taskBudgetEventFields(runID, scheduled, ledger)
			fields["attempt"] = len(scheduled.Attempts)
			fields["usage"] = scheduled.Result.Usage
			fields["evidence_candidates"] = len(scheduled.Result.EvidenceCandidates)
			fields["evidence_admitted"] = admitted
			fields["claims"] = len(scheduled.Result.Claims)
			if scheduled.Failure != nil {
				fields["failure_code"] = scheduled.Failure.Code
			}
			if err := appendStructuredRunEvent(store, runID, "task_completed", fields); err != nil && persistErr == nil {
				persistErr = err
			}
			reason := ""
			if scheduled.Failure != nil {
				reason = scheduled.Failure.Message
			}
			coordinator.emitProgress(runID, ProgressTaskCompleted, scheduled.Task.ID, scheduled.Task.Executor, directToolID(scheduled.Task), string(scheduled.Status), reason)
		},
	}
	executedTasks := make(map[string]struct{})
	roundLimit := policy.MaxRounds
	if roundLimit <= 0 {
		roundLimit = 1
	}
	report := InvestigationReport{}
	for round := 0; round < roundLimit; round++ {
		roundCount++
		if round > 0 {
			if err := transition(RunPlanned); err != nil {
				_ = compositionReservation.Release()
				return fail(RunFailure{Code: FailurePlan, Message: err.Error(), Stage: string(StagePlanning)}, RunFailed)
			}
			if err := store.SavePlan(runID, plan); err != nil {
				_ = compositionReservation.Release()
				return fail(RunFailure{Code: FailurePlan, Message: err.Error(), Stage: string(StagePlanning)}, RunFailed)
			}
			if err := appendStructuredRunEvent(store, runID, "plan_compiled", map[string]any{
				"proposal_hash":       plan.ProposalHash,
				"revision":            plan.Revision,
				"task_count":          len(plan.Tasks),
				"evidence_task_count": planEvidenceTaskCount(plan.Tasks),
				"verifier_task_count": planVerifierTaskCount(plan.Tasks),
				"task_ids":            executableTaskIDs(plan.Tasks),
			}); err != nil {
				_ = compositionReservation.Release()
				return fail(RunFailure{Code: FailurePlan, Message: err.Error(), Stage: string(StagePlanning)}, RunFailed)
			}
		}
		if err := transition(RunExecuting); err != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{Code: FailurePlan, Message: err.Error(), Stage: string(StageExecution)}, RunFailed)
		}

		persistErr = nil
		scheduler.ProtectedAdmissions = verificationAdmissions
		executionCtx, cancelRound := context.WithCancel(deadlineCtx)
		stopRound = cancelRound
		executionResults, scheduleErr := scheduler.Execute(executionCtx, plan.Tasks, func(task ExecutableTask, upstream map[string]json.RawMessage) TaskExecutionInput {
			return TaskExecutionInput{
				Task: task, Evidence: evidence.All(), Claims: claims.All(), Upstream: upstream,
				WorkflowRunID: runID, ParentRunID: contract.ParentRunID, Actor: contract.Actor,
			}
		})
		cancelRound()
		stopRound = nil
		if requiredFailure != nil && !budgetFailureCanDeliverForPlan(plan.Tasks, evidence, claims, requiredFailure) {
			report = BuildReport(evidence, claims, failures)
			if err := store.SaveReport(runID, report); err != nil {
				_ = compositionReservation.Release()
				failure := failureWithPersistenceError(*requiredFailure, "save failure report", err)
				return fail(failure, runFailureStatusForFailure(failure, RunFailed))
			}
			_ = compositionReservation.Release()
			return fail(*requiredFailure, runFailureStatusForFailure(*requiredFailure, RunFailed))
		}
		if duplicateStop {
			failures = append(failures, RunFailure{Code: FailureExecution, Message: fmt.Sprintf("duplicate evidence ratio exceeded %.3f", policy.MaxDuplicateRatio), Stage: string(StageExecution), Retryable: false})
		}
		if persistErr != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{Code: FailureExecution, Message: persistErr.Error(), Stage: string(StageExecution)}, RunFailed)
		}
		if scheduleErr != nil {
			failure := runFailureFromContext(deadlineCtx, scheduleErr)
			failures = append(failures, failure)
		}
		if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
			return fail(RunFailure{Code: FailureExecution, Message: err.Error(), Stage: string(StageExecution)}, RunFailed)
		}
		if scheduleErr != nil {
			_ = compositionReservation.Release()
			return fail(runFailureFromContext(deadlineCtx, scheduleErr), runFailureStatus(deadlineCtx, scheduleErr))
		}
		if coordinator.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
			if verifierErr := validateVerifierExecution(plan.Tasks, executionResults, evidence, claims); verifierErr != nil {
				report = BuildReport(evidence, claims, failures)
				if err := store.SaveReport(runID, report); err != nil {
					_ = compositionReservation.Release()
					failure := failureWithPersistenceError(*verifierErr, "save failure report", err)
					return fail(failure, runFailureStatusForFailure(failure, RunFailed))
				}
				_ = compositionReservation.Release()
				return fail(*verifierErr, runFailureStatusForFailure(*verifierErr, RunFailed))
			}
		}
		if err := deadlineCtx.Err(); err != nil {
			_ = compositionReservation.Release()
			return fail(runFailureFromContext(deadlineCtx, err), runFailureStatus(deadlineCtx, err))
		}
		if err := transition(RunVerifying); err != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
		}
		for _, task := range plan.Tasks {
			executedTasks[task.ID] = struct{}{}
			executorCounts[task.Executor]++
			switch task.Executor {
			case ExecutorInvestigator, ExecutorVerifier, ExecutorComposer:
				agentTaskCount++
			}
		}
		executedTaskCount += len(plan.Tasks)
		report = BuildReport(evidence, claims, failures)
		if err := store.SaveReport(runID, report); err != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
		}

		if duplicateStop || round+1 >= roundLimit {
			break
		}
		discoveries, discoverErr := coordinator.persistedDiscoveries(runID)
		if discoverErr != nil {
			failures = append(failures, RunFailure{Code: FailurePlan, Message: discoverErr.Error(), Stage: string(StagePlanning)})
			break
		}
		next, replanErr := coordinator.replanCandidates(
			contract,
			report.Coverage,
			executedTasks,
			coordinator.maxTasks(contract),
			discoveries,
			ledger.Available(StageExecution),
		)
		if replanErr != nil {
			failures = append(failures, RunFailure{Code: FailurePlan, Message: replanErr.Error(), Stage: string(StagePlanning)})
			break
		}
		if len(next) == 0 {
			break
		}
		if err := transition(RunReplanning); err != nil {
			_ = compositionReservation.Release()
			return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
		}
		// Replan compiles new Workflow nodes against the same shared Run ledger.
		// It deliberately does not reset or repartition a child-Agent budget.
		plan, replanErr = compiler.CompileReplan(
			contract,
			next,
			coveredRequiredGoals(contract, report.Coverage),
			round+2,
		)
		if replanErr != nil {
			failures = append(failures, RunFailure{Code: FailurePlan, Message: replanErr.Error(), Stage: string(StagePlanning)})
			break
		}
		verificationAdmissions, replanErr = coordinator.reserveVerification(ledger, plan.Tasks)
		if replanErr != nil {
			failures = append(failures, verifierReservationFailure(replanErr))
			break
		}
	}
	report = BuildReport(evidence, claims, failures)
	if err := store.SaveReport(runID, report); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification)}, RunFailed)
	}
	if err := transition(RunComposing); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureComposer, Message: err.Error(), Stage: string(StageComposition)}, RunFailed)
	}

	if compositionReservation.ID == "" {
		compositionReservation, err = coordinator.reserveComposition(ledger)
		if err != nil {
			if !errors.Is(err, ErrBudgetExceeded) {
				return fail(RunFailure{Code: FailureComposer, Message: err.Error(), Stage: string(StageComposition)}, RunFailed)
			}
			compositionBudgetUnavailable = true
			compositionReservation = BudgetReservation{}
		}
	}
	composer := composerForDelivery(contract, coordinator.Composer)
	if compositionBudgetUnavailable {
		composer = nil
	}
	composer, compositionReservation, err = beginComposition(ledger, compositionReservation, composer, report, !isZeroBudget(coordinator.CompositionBudget))
	if err != nil {
		return fail(RunFailure{Code: FailureComposer, Message: err.Error(), Stage: string(StageComposition)}, RunFailed)
	}
	deliveryContext := agentapi.WithRunBudgetGate(deadlineCtx, ledger)
	if compositionReservation.ID != "" {
		deliveryContext = agentapi.WithRunBudgetGate(deliveryContext, reservationBudgetGate{
			ledger: ledger,
			id:     compositionReservation.ID,
		})
	}
	delivery := coordinator.Delivery.Deliver(deliveryContext, contract, report, composer)
	if err := deadlineCtx.Err(); err != nil {
		_ = compositionReservation.Release()
		return fail(runFailureFromContext(deadlineCtx, err), runFailureStatus(deadlineCtx, err))
	}
	if compositionReservation.ID != "" {
		actual := BudgetVector{}
		if coordinator.Composer != nil && len(report.Claims) > 0 {
			actual = delivery.Usage
			// Legacy/custom composers may not report provider usage. Preserve the
			// old bounded fallback estimate for successful composition only.
			if isZeroBudget(actual) && delivery.Failure == nil {
				actual.OutputTokens = int64(len(strings.Fields(delivery.Text)))
			}
		}
		if err := compositionReservation.Settle(actual); err != nil {
			_ = compositionReservation.Release()
			report.Failures = append(report.Failures, RunFailure{
				Code: FailureBudget, Message: err.Error(), Stage: string(StageComposition),
			})
			if saveErr := store.SaveReport(runID, report); saveErr != nil {
				failure := failureWithPersistenceError(RunFailure{Code: FailureBudget, Message: err.Error(), Stage: string(StageComposition)}, "save budget failure report", saveErr)
				return fail(failure, RunBudgetExhausted)
			}
			return fail(RunFailure{Code: FailureBudget, Message: err.Error(), Stage: string(StageComposition)}, RunBudgetExhausted)
		}
		compositionReservation = BudgetReservation{}
	}
	if err := saveMetrics(delivery.Failure != nil && delivery.Failure.Code == FailureComposer); err != nil {
		_ = compositionReservation.Release()
		return fail(RunFailure{Code: FailureComposer, Message: err.Error(), Stage: string(StageComposition)}, RunFailed)
	}
	if err := store.SaveDelivery(runID, delivery); err != nil {
		return fail(RunFailure{Code: FailureEmptyOutput, Message: err.Error(), Stage: string(StageComposition)}, RunFailed)
	}
	var postDeliveryErr error
	if err := appendStructuredRunEvent(store, runID, "delivery_completed", map[string]any{
		"status": delivery.Status, "text_non_empty": strings.TrimSpace(delivery.Text) != "",
		"coverage": len(delivery.Report.Coverage), "gaps": len(delivery.Report.Gaps),
	}); err != nil {
		postDeliveryErr = err
	}
	if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
		if postDeliveryErr == nil {
			postDeliveryErr = err
		} else {
			postDeliveryErr = errors.Join(postDeliveryErr, fmt.Errorf("save run budget: %w", err))
		}
	}
	coordinator.emitProgress(runID, ProgressWorkflowCompleted, "", "", "", string(delivery.Status), "")
	completed, err := store.Get(runID)
	if err != nil {
		return InvestigationRun{}, err
	}
	if postDeliveryErr != nil {
		return completed, fmt.Errorf("delivery persisted but post-delivery persistence failed: %w", postDeliveryErr)
	}
	return completed, nil
}

func validateVerifierExecution(tasks []ExecutableTask, results []ScheduledTaskResult, evidence *EvidenceLedger, claims *ClaimLedger) *RunFailure {
	verifierID := ""
	verifierCount := 0
	for _, task := range tasks {
		if task.Executor != ExecutorVerifier {
			continue
		}
		verifierCount++
		verifierID = task.ID
	}
	if verifierCount != 1 {
		return &RunFailure{
			Code:      FailureVerifier,
			Message:   fmt.Sprintf("executable plan must contain exactly one verifier, found %d", verifierCount),
			Stage:     string(StageVerification),
			Retryable: false,
		}
	}
	for _, result := range results {
		if result.Task.ID != verifierID {
			continue
		}
		if result.Status == TaskSucceeded {
			return nil
		}
		if result.Status == TaskPartial && len(result.Result.Claims) > 0 {
			return nil
		}
		if failure := requiredTaskFailure(result); failure != nil {
			if budgetFailureCanDeliverForPlan(tasks, evidence, claims, failure) {
				return nil
			}
			failure.Code = FailureVerifier
			failure.Stage = string(StageVerification)
			return failure
		}
		return &RunFailure{
			Code:      FailureVerifier,
			Message:   fmt.Sprintf("verifier task %q ended with status %q", verifierID, result.Status),
			Stage:     string(StageVerification),
			TaskID:    verifierID,
			Retryable: false,
		}
	}
	return &RunFailure{
		Code:      FailureVerifier,
		Message:   fmt.Sprintf("verifier task %q did not execute", verifierID),
		Stage:     string(StageVerification),
		TaskID:    verifierID,
		Retryable: false,
	}
}

func planEvidenceTaskCount(tasks []ExecutableTask) int {
	count := 0
	for _, task := range tasks {
		if task.Executor != ExecutorVerifier {
			count++
		}
	}
	return count
}

func planVerifierTaskCount(tasks []ExecutableTask) int {
	count := 0
	for _, task := range tasks {
		if task.Executor == ExecutorVerifier {
			count++
		}
	}
	return count
}

func executableTaskIDs(tasks []ExecutableTask) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func (coordinator *Coordinator) replanCandidates(
	contract InvestigationContract,
	coverage []GoalCoverage,
	executed map[string]struct{},
	maxTasks int,
	discoveries []Discovery,
	available BudgetVector,
) ([]TaskCandidate, error) {
	if coordinator.Catalog == nil {
		return nil, nil
	}
	unresolved := unresolvedRequiredGoals(contract, coverage)
	if len(unresolved) == 0 {
		return nil, nil
	}
	candidates, err := coordinator.Catalog.GenerateCandidatesForGoals(contract, unresolved)
	if errors.Is(err, ErrCapabilityGap) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	discovered, err := coordinator.Catalog.GenerateCandidatesForDiscoveries(
		contract, discoveries, unresolved,
	)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, discovered...)

	filtered := make([]TaskCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, done := executed[candidate.ID]; done {
			continue
		}
		if coordinator.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
			template, resolveErr := coordinator.Catalog.Resolve(candidate.Template)
			if resolveErr != nil {
				return nil, fmt.Errorf("resolve replan candidate %q: %w", candidate.ID, resolveErr)
			}
			// Verification is a fixed server-owned stage and is appended by
			// CompileReplan; it must not consume the next evidence-task slot.
			if template.Executor == ExecutorVerifier {
				continue
			}
		}
		if candidateTargetsUnresolvedGoal(candidate, unresolved) {
			filtered = append(filtered, candidate)
		}
	}
	return selectReplanCandidates(
		coordinator.Catalog,
		contract,
		filtered,
		unresolved,
		executed,
		maxTasks,
		available,
		coverage,
	), nil
}

func coveredRequiredGoals(contract InvestigationContract, coverage []GoalCoverage) map[string]struct{} {
	covered := make(map[string]struct{})
	required := make(map[string]struct{})
	for _, goal := range contract.EvidenceGoals {
		if goal.Required {
			required[goal.ID] = struct{}{}
		}
	}
	for _, item := range coverage {
		if _, ok := required[item.GoalID]; ok && item.Status == GoalCovered {
			covered[item.GoalID] = struct{}{}
		}
	}
	return covered
}

func unresolvedRequiredGoals(contract InvestigationContract, coverage []GoalCoverage) map[string]struct{} {
	required := make(map[string]struct{})
	for _, goal := range contract.EvidenceGoals {
		if goal.Required {
			required[goal.ID] = struct{}{}
		}
	}
	status := make(map[string]GoalCoverageStatus, len(coverage))
	for _, item := range coverage {
		status[item.GoalID] = item.Status
	}
	unresolved := make(map[string]struct{})
	for goalID := range required {
		if status[goalID] != GoalCovered {
			unresolved[goalID] = struct{}{}
		}
	}
	return unresolved
}

func candidateTargetsUnresolvedGoal(candidate TaskCandidate, unresolved map[string]struct{}) bool {
	for _, goalID := range candidate.EvidenceGoalIDs {
		if _, ok := unresolved[goalID]; ok {
			return true
		}
	}
	return false
}

func (coordinator *Coordinator) maxTasks(contract InvestigationContract) int {
	maxTasks := coordinator.MaxTasks
	if contract.MaxTasks > 0 && (maxTasks == 0 || contract.MaxTasks < maxTasks) {
		maxTasks = contract.MaxTasks
	}
	return maxTasks
}

func (coordinator *Coordinator) planTaskLimit(
	contract InvestigationContract,
	stageLimits map[BudgetStage]BudgetVector,
) int {
	maxTasks := coordinator.maxTasks(contract)
	toolCallLimit := stageLimits[StageExecution].ToolCalls
	if toolCallLimit > 0 && (maxTasks == 0 || toolCallLimit < maxTasks) {
		maxTasks = toolCallLimit
	}
	return maxTasks
}

func (coordinator *Coordinator) maxRounds(contract InvestigationContract) int {
	maxRounds := coordinator.MaxRounds
	if contract.MaxRounds > 0 && (maxRounds == 0 || contract.MaxRounds < maxRounds) {
		maxRounds = contract.MaxRounds
	}
	return maxRounds
}

// resolveBudgetProfile picks the run profile. A request-supplied profile must be
// valid and, when the coordinator pins a default, cannot widen the run budget.
func (coordinator *Coordinator) resolveBudgetProfile(contract InvestigationContract) (BudgetProfile, error) {
	requested := BudgetProfile(strings.TrimSpace(contract.BudgetProfile))
	if requested != "" {
		parsed, err := ParseBudgetProfile(string(requested))
		if err != nil {
			return "", err
		}
		return parsed, nil
	}
	if coordinator.BudgetProfile != "" {
		return coordinator.BudgetProfile, nil
	}
	return ProfileInteractive, nil
}

func (coordinator *Coordinator) withRunDeadline(ctx context.Context, limit BudgetVector) (context.Context, context.CancelFunc) {
	if limit.Duration <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, limit.Duration)
}

func (coordinator *Coordinator) executionPolicy(
	contract InvestigationContract,
	proposal *agentapi.TaskGraphProposal,
) (PlanExecutionPolicy, error) {
	policy := PlanExecutionPolicy{
		MaxParallelism: coordinator.MaxParallelism,
		MaxRounds:      coordinator.maxRounds(contract),
		Budget:         coordinator.BudgetLimit,
	}
	if proposal == nil {
		return policy, nil
	}
	proposalPolicy, err := proposalPolicy(proposal.Stop)
	if err != nil {
		return PlanExecutionPolicy{}, err
	}
	policy.MaxParallelism = minPositive(policy.MaxParallelism, proposalPolicy.MaxParallelism)
	policy.MaxRounds = minPositive(policy.MaxRounds, proposalPolicy.MaxRounds)
	policy.MaxDepth = proposalPolicy.MaxDepth
	policy.MaxDuplicateRatio = proposalPolicy.MaxDuplicateRatio
	policy.MaxRetries = proposalPolicy.MaxRetries
	policy.Budget = tightenBudget(policy.Budget, proposalPolicy.Budget)
	return policy, nil
}

func tightenBudget(base, requested BudgetVector) BudgetVector {
	out := base
	if requested.InputTokens > 0 && (out.InputTokens == 0 || requested.InputTokens < out.InputTokens) {
		out.InputTokens = requested.InputTokens
	}
	if requested.OutputTokens > 0 && (out.OutputTokens == 0 || requested.OutputTokens < out.OutputTokens) {
		out.OutputTokens = requested.OutputTokens
	}
	if requested.TotalTokens > 0 && (out.TotalTokens == 0 || requested.TotalTokens < out.TotalTokens) {
		out.TotalTokens = requested.TotalTokens
	}
	if requested.ToolCalls > 0 && (out.ToolCalls == 0 || requested.ToolCalls < out.ToolCalls) {
		out.ToolCalls = requested.ToolCalls
	}
	if requested.CostMicros > 0 && (out.CostMicros == 0 || requested.CostMicros < out.CostMicros) {
		out.CostMicros = requested.CostMicros
	}
	return out
}

func effectiveParallelism(base, requested int) int {
	return minPositive(base, requested)
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

func cloneExecutorCounts(input map[ExecutorType]int) map[ExecutorType]int {
	if len(input) == 0 {
		return nil
	}
	cloned := make(map[ExecutorType]int, len(input))
	for executor, count := range input {
		cloned[executor] = count
	}
	return cloned
}

func cloneTaskAttempts(input []TaskAttempt) []TaskAttempt {
	if len(input) == 0 {
		return nil
	}
	cloned := make([]TaskAttempt, len(input))
	for index, attempt := range input {
		if attempt.Failure != nil {
			failure := *attempt.Failure
			attempt.Failure = &failure
		}
		cloned[index] = attempt
	}
	return cloned
}

func cloneFailure(failure *RunFailure) *RunFailure {
	if failure == nil {
		return nil
	}
	cloned := *failure
	return &cloned
}

func (coordinator *Coordinator) persistedDiscoveries(runID string) ([]Discovery, error) {
	if coordinator == nil || coordinator.Store == nil {
		return nil, nil
	}
	run, err := coordinator.Store.Get(runID)
	if err != nil {
		return nil, err
	}
	discoveries := make([]Discovery, 0)
	for _, record := range run.Results {
		discoveries = append(discoveries, record.Discoveries...)
	}
	return discoveries, nil
}

func runMetricDimensions(
	store RunStore,
	runID string,
) (map[string]BudgetVector, map[string]BudgetVector, map[string]GoalCoverageStatus) {
	taskUsage := make(map[string]BudgetVector)
	templateUsage := make(map[string]BudgetVector)
	goalCoverage := make(map[string]GoalCoverageStatus)
	if store == nil {
		return taskUsage, templateUsage, goalCoverage
	}
	run, err := store.Get(runID)
	if err != nil {
		return taskUsage, templateUsage, goalCoverage
	}
	for taskID, record := range run.Results {
		taskUsage[taskID] = record.Usage
		if task, ok := run.Tasks[taskID]; ok {
			templateUsage[task.Template.ID] = addVector(templateUsage[task.Template.ID], record.Usage)
		}
	}
	for _, coverage := range run.Report.Coverage {
		goalCoverage[coverage.GoalID] = coverage.Status
	}
	return taskUsage, templateUsage, goalCoverage
}

func withoutTokenBudget(value BudgetVector) BudgetVector {
	value.InputTokens = 0
	value.OutputTokens = 0
	value.TotalTokens = 0
	return value
}

// beginComposition releases the stage-entry protection floor before opening
// the actual composer accounting reservation. The active admission is soft to
// avoid double-reserving provider calls; AgentComposer independently applies
// the allocated Composition input/total limits to its child Run.
func beginComposition(ledger *BudgetLedger, protection BudgetReservation, composer Composer, report InvestigationReport, enabled bool) (Composer, BudgetReservation, error) {
	if protection.ID != "" {
		if err := protection.Release(); err != nil {
			return nil, protection, fmt.Errorf("release composition protection: %w", err)
		}
	}
	if !enabled || composer == nil || len(report.Claims) == 0 {
		return composer, BudgetReservation{}, nil
	}
	active, err := ledger.ReserveAdmission(StageComposition, "composition", BudgetVector{})
	if err != nil {
		if errors.Is(err, ErrBudgetExceeded) {
			// Deterministic delivery remains valid when no model-call capacity is
			// left; only the optional composer is unavailable.
			return nil, BudgetReservation{}, nil
		}
		return nil, BudgetReservation{}, fmt.Errorf("reserve composition admission: %w", err)
	}
	return composer, active, nil
}

// reserveVerification protects the Verification share of the frozen Run
// budget for every server-owned verifier in a plan before Investigator
// execution starts. The reservation is later handed to Scheduler so the
// verifier is not charged a second time during normal admission.
func verifierReservationFailure(err error) RunFailure {
	code := FailureVerifier
	if errors.Is(err, ErrBudgetExceeded) {
		code = FailureBudget
	}
	return RunFailure{
		Code: code, Message: err.Error(), Stage: string(StageVerification),
	}
}

func (coordinator *Coordinator) reserveVerification(ledger *BudgetLedger, tasks []ExecutableTask) (map[string]BudgetReservation, error) {
	if ledger == nil {
		return nil, fmt.Errorf("budget ledger is required")
	}
	if coordinator == nil || coordinator.Executors == nil {
		return nil, fmt.Errorf("executor registry is required")
	}

	type verifierCandidate struct {
		task    ExecutableTask
		minimum BudgetVector
	}
	candidates := make([]verifierCandidate, 0)
	for _, task := range tasks {
		if task.Executor != ExecutorVerifier {
			continue
		}
		executor, err := coordinator.Executors.Resolve(task.Executor)
		if err != nil {
			if errors.Is(err, ErrCapabilityGap) {
				continue
			}
			return nil, fmt.Errorf("resolve verifier executor for budget admission: %w", err)
		}
		provider, ok := executor.(TaskMinimumBudgetProvider)
		if !ok {
			continue
		}
		minimum, err := provider.MinimumBudget(task)
		if err != nil {
			return nil, fmt.Errorf("minimum budget for verifier task %q: %w", task.ID, err)
		}
		if isZeroBudget(minimum) {
			continue
		}
		candidates = append(candidates, verifierCandidate{task: task, minimum: minimum})
	}
	if len(candidates) == 0 {
		return map[string]BudgetReservation{}, nil
	}

	snapshot := ledger.Snapshot()
	profile, err := coordinator.profileForBudgetSnapshot(snapshot)
	if err != nil {
		return nil, fmt.Errorf("resolve verifier budget profile: %w", err)
	}
	pool, err := AllocateRoleBudget(profile, snapshot.Run.Limit, StageVerification)
	if err != nil {
		return nil, fmt.Errorf("allocate verifier budget: %w", err)
	}

	admissions := make([]BudgetAdmission, 0, len(candidates))
	for index, candidate := range candidates {
		// The role pool is split before task limits are applied. This keeps the
		// sum of all verifier grants inside the Verification share even when a
		// plan contains more than one verifier task.
		grant := splitBudgetVector(pool, index, len(candidates))
		grant = capBudgetToLimit(grant, candidate.task.Budget.Limit)
		if !budgetVectorCovers(grant, candidate.minimum) {
			return nil, fmt.Errorf(
				"%w: verifier task %q minimum %+v exceeds its allocated budget %+v",
				ErrBudgetExceeded, candidate.task.ID, candidate.minimum, grant,
			)
		}
		admissions = append(admissions, BudgetAdmission{TaskID: candidate.task.ID, Grant: grant})
	}

	reservations, err := ledger.ReserveProtectedAdmissionGroup(StageVerification, admissions)
	if err != nil {
		return nil, fmt.Errorf("reserve verifier budget: %w", err)
	}
	return reservations, nil
}

func (coordinator *Coordinator) profileForBudgetSnapshot(snapshot BudgetSnapshot) (BudgetProfile, error) {
	if profile := strings.TrimSpace(snapshot.Run.Profile); profile != "" {
		return ParseBudgetProfile(profile)
	}
	if coordinator != nil && coordinator.BudgetProfile != "" {
		return coordinator.BudgetProfile, nil
	}
	return ProfileInteractive, nil
}

// budgetVectorCovers reports whether a role grant can cover a required
// minimum. A zero grant dimension retains the ledger's unbounded meaning.
func budgetVectorCovers(grant, minimum BudgetVector) bool {
	return coversInt64(grant.InputTokens, minimum.InputTokens) &&
		coversInt64(grant.OutputTokens, minimum.OutputTokens) &&
		coversInt64(grant.TotalTokens, minimum.TotalTokens) &&
		coversInt(grant.ToolCalls, minimum.ToolCalls) &&
		coversDuration(grant.Duration, minimum.Duration) &&
		coversInt64(grant.CostMicros, minimum.CostMicros)
}

func coversInt64(grant, minimum int64) bool {
	return grant == 0 || grant >= minimum
}

func coversInt(grant, minimum int) bool {
	return grant == 0 || grant >= minimum
}

func coversDuration(grant, minimum time.Duration) bool {
	return grant == 0 || grant >= minimum
}

func (coordinator *Coordinator) reserveComposition(ledger *BudgetLedger) (BudgetReservation, error) {
	if isZeroBudget(coordinator.CompositionBudget) {
		return BudgetReservation{}, nil
	}
	// The reserve is a delivery-protection floor, not a child-Agent quota.
	// Keep it at the profile's composition share so a default answer budget
	// equal to the Run budget does not reserve the entire Run up front.
	snapshot := ledger.Snapshot()
	profile, err := ParseBudgetProfile(snapshot.Run.Profile)
	if err != nil {
		profile = ProfileInteractive
	}
	allocation, _ := profile.Allocation()
	protection := scaleBudgetVector(snapshot.Run.Limit, allocation.Composition)
	// A composition percentage is only a starting point. Never let it fall
	// below the smallest usable synthesizer response when the Run itself can
	// afford that response; otherwise Investigators can consume the last few
	// tokens and leave no path to a final answer.
	minimumCompositionOutput := minInt64(composerMinimumOutputTokens, coordinator.CompositionBudget.OutputTokens)
	if coordinator.CompositionBudget.OutputTokens >= composerMinimumOutputTokens &&
		snapshot.Run.Limit.OutputTokens >= minimumCompositionOutput &&
		protection.OutputTokens < minimumCompositionOutput {
		protection.OutputTokens = minimumCompositionOutput
	}
	grant := capCompositionGrant(coordinator.CompositionBudget, protection)
	if isZeroBudget(grant) {
		return BudgetReservation{}, nil
	}
	return ledger.Reserve(StageComposition, "composition", ledger.CapStageGrant(StageComposition, grant))
}

func capCompositionGrant(grant, protection BudgetVector) BudgetVector {
	out := grant
	if protection.InputTokens == 0 {
		out.InputTokens = 0
	} else if out.InputTokens > protection.InputTokens {
		out.InputTokens = protection.InputTokens
	}
	if protection.OutputTokens == 0 {
		out.OutputTokens = 0
	} else if out.OutputTokens > protection.OutputTokens {
		out.OutputTokens = protection.OutputTokens
	}
	if protection.TotalTokens == 0 {
		out.TotalTokens = 0
	} else if out.TotalTokens > protection.TotalTokens {
		out.TotalTokens = protection.TotalTokens
	}
	if protection.ToolCalls == 0 {
		out.ToolCalls = 0
	} else if out.ToolCalls > protection.ToolCalls {
		out.ToolCalls = protection.ToolCalls
	}
	if protection.Duration == 0 {
		out.Duration = 0
	} else if out.Duration > protection.Duration {
		out.Duration = protection.Duration
	}
	if protection.CostMicros > 0 && out.CostMicros > protection.CostMicros {
		out.CostMicros = protection.CostMicros
	}
	return out
}

func taskBudgetEventFields(runID string, scheduled ScheduledTaskResult, ledger *BudgetLedger) map[string]any {
	snapshot := ledger.Snapshot()
	remaining := subtractVector(snapshot.Run.Limit, addVector(snapshot.Run.Used, snapshot.Run.Reserved))
	return map[string]any{
		"run_id":                   runID,
		"task_id":                  scheduled.Task.ID,
		"executor":                 scheduled.Task.Executor,
		"status":                   scheduled.Status,
		"completion_status":        scheduled.Status,
		"budget_boundary":          "run",
		"budget_dimension":         budgetDimensions(snapshot.Run.Limit),
		"run_limit":                snapshot.Run.Limit,
		"run_reserved":             snapshot.Run.Reserved,
		"run_used":                 snapshot.Run.Used,
		"run_remaining":            remaining,
		"task_reserved":            scheduled.BudgetGrant,
		"task_used":                scheduled.Result.Usage,
		"requested_output":         scheduled.BudgetGrant.OutputTokens,
		"effective_output":         scheduled.Result.Usage.OutputTokens,
		"input_tokens":             scheduled.Result.Usage.InputTokens,
		"failure_code":             "",
		"evidence_candidate_count": len(scheduled.Result.EvidenceCandidates),
	}
}

func budgetDimensions(limit BudgetVector) []string {
	dimensions := make([]string, 0, 6)
	if limit.InputTokens > 0 {
		dimensions = append(dimensions, "input_tokens")
	}
	if limit.OutputTokens > 0 {
		dimensions = append(dimensions, "output_tokens")
	}
	if limit.TotalTokens > 0 {
		dimensions = append(dimensions, "total_tokens")
	}
	if limit.ToolCalls > 0 {
		dimensions = append(dimensions, "tool_calls")
	}
	if limit.Duration > 0 {
		dimensions = append(dimensions, "duration")
	}
	if limit.CostMicros > 0 {
		dimensions = append(dimensions, "cost_micros")
	}
	return dimensions
}

func (coordinator *Coordinator) admitTaskResult(
	scheduled ScheduledTaskResult,
	evidence *EvidenceLedger,
	claims *ClaimLedger,
) (TaskExecutionRecord, *RunFailure) {
	record := TaskExecutionRecord{
		TaskID:      scheduled.Task.ID,
		Status:      scheduled.Status,
		Output:      append([]byte(nil), scheduled.Result.Output...),
		Usage:       scheduled.Result.Usage,
		Failure:     scheduled.Failure,
		StartedAt:   scheduled.Started,
		EndedAt:     scheduled.Ended,
		Attempts:    cloneTaskAttempts(scheduled.Attempts),
		Discoveries: append([]Discovery(nil), scheduled.Discoveries...),
	}
	if len(record.Attempts) == 0 {
		record.Attempts = []TaskAttempt{{
			Attempt:   1,
			StartedAt: scheduled.Started,
			EndedAt:   scheduled.Ended,
			Status:    scheduled.Status,
			Failure:   cloneFailure(scheduled.Failure),
		}}
	}
	reject := func(failure *RunFailure) (TaskExecutionRecord, *RunFailure) {
		record.Status = TaskFailed
		record.Failure = failure
		return record, failure
	}
	if scheduled.Status != TaskSucceeded && scheduled.Status != TaskPartial {
		return record, nil
	}
	if scheduled.Status == TaskSucceeded && !isEvidenceWorkerTask(scheduled.Task) {
		if failure := validateTaskOutput(coordinator.Schemas, scheduled.Task, scheduled.Result.Output); failure != nil {
			return reject(failureForTask(scheduled.Task.ID, *failure))
		}
	}
	admittedEvidence := false
	admittedClaims := false
	for _, candidate := range scheduled.Result.EvidenceCandidates {
		if _, _, err := evidence.Admit(scheduled.Task.ID, candidate); err != nil {
			if errors.Is(err, ErrOpaqueEvidence) {
				continue
			}
			return reject(failureForTask(scheduled.Task.ID, RunFailure{
				Code: FailureSchema, Message: err.Error(), Stage: string(StageVerification), Retryable: false,
			}))
		}
		// Duplicate evidence is still an admitted dependency: it already exists
		// in the ledger and can be referenced by the verifier.
		admittedEvidence = true
	}
	for _, candidate := range scheduled.Result.Claims {
		if scheduled.Task.Executor != ExecutorVerifier {
			return reject(failureForTask(scheduled.Task.ID, RunFailure{
				Code: FailureVerifier, Message: "only verifier tasks may submit claims", Stage: string(StageVerification), Retryable: false,
			}))
		}
		if _, _, err := claims.Admit(scheduled.Task.ID, candidate); err != nil {
			return reject(failureForTask(scheduled.Task.ID, RunFailure{
				Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification), Retryable: false,
			}))
		}
		admittedClaims = true
	}
	if scheduled.Status == TaskPartial && !hasAdmittedArtifact(scheduled.Task, scheduled.Result.Output, admittedEvidence, admittedClaims) {
		return reject(failureForTask(scheduled.Task.ID, RunFailure{
			Code: FailureEmptyOutput, Message: "partial task produced no admissible artifact", Stage: string(StageExecution), Retryable: false,
		}))
	}
	if len(scheduled.Result.Output) == 0 && !admittedEvidence && !admittedClaims {
		return reject(failureForTask(scheduled.Task.ID, RunFailure{
			Code: FailureEmptyOutput, Message: "task produced no output or admissible evidence", Stage: string(StageExecution), Retryable: false,
		}))
	}
	return record, nil
}

func hasAdmittedArtifact(task ExecutableTask, output json.RawMessage, evidence, claims bool) bool {
	switch task.Executor {
	case ExecutorInvestigator:
		return evidence
	case ExecutorVerifier:
		return claims
	default:
		return len(output) > 0 || evidence || claims
	}
}

func isEvidenceWorkerTask(task ExecutableTask) bool {
	return task.Executor == ExecutorInvestigator
}

func preferRunFailure(current, candidate *RunFailure) *RunFailure {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Code == FailureBudget {
		return candidate
	}
	return current
}

func runFailureStatusForFailure(failure RunFailure, status RunStatus) RunStatus {
	if failure.Code == FailureBudget {
		return RunBudgetExhausted
	}
	return status
}

func budgetFailureCanDeliver(evidence *EvidenceLedger, claims *ClaimLedger, failure *RunFailure) bool {
	if failure == nil || failure.Code != FailureBudget || strings.TrimSpace(failure.TaskID) == "" {
		return false
	}
	return (evidence != nil && evidence.HasTask(failure.TaskID)) ||
		(claims != nil && claims.HasTask(failure.TaskID))
}

// budgetFailureCanDeliverForPlan also permits a verifier budget stop when an
// investigator already produced evidence. The verifier owns claims, so its
// failed attempt cannot own an artifact even though the run can still deliver
// a bounded evidence-insufficient or partial result.
func budgetFailureCanDeliverForPlan(
	tasks []ExecutableTask,
	evidence *EvidenceLedger,
	claims *ClaimLedger,
	failure *RunFailure,
) bool {
	if budgetFailureCanDeliver(evidence, claims, failure) {
		return true
	}
	if failure == nil || failure.Code != FailureBudget || evidence == nil {
		return false
	}
	for _, task := range tasks {
		if task.Executor == ExecutorVerifier && task.ID == failure.TaskID {
			for _, investigator := range tasks {
				if investigator.Executor == ExecutorInvestigator && evidence.HasTask(investigator.ID) {
					return true
				}
			}
			return false
		}
	}
	return false
}

func requiredTaskFailure(result ScheduledTaskResult) *RunFailure {
	// A worker that produced a usable artifact can hand control to verification
	// even when its own run ended early. The artifact, not the worker's final
	// prose, is the dependency contract.
	if result.Status == TaskPartial && hasAdmissibleArtifacts(result.Task, result.Result) {
		return nil
	}
	// A required task cannot hide a hard shared-budget stop behind an empty
	// fallback report. Optional persisted records with a report remain useful
	// diagnostics and do not gate resume.
	if result.Failure != nil && result.Failure.Code == FailureBudget {
		if result.Task.Optional && hasAdmissibleArtifacts(result.Task, result.Result) {
			return nil
		}
		failure := *result.Failure
		if failure.TaskID == "" {
			failure.TaskID = result.Task.ID
		}
		return &failure
	}
	if result.Task.Optional {
		return nil
	}
	if result.Status != TaskFailed && result.Status != TaskBlocked {
		return nil
	}
	if result.Failure != nil {
		failure := *result.Failure
		if failure.TaskID == "" {
			failure.TaskID = result.Task.ID
		}
		return &failure
	}
	return &RunFailure{
		Code: FailureExecution, Message: fmt.Sprintf("required task %q ended with status %q", result.Task.ID, result.Status),
		Stage: string(StageExecution), TaskID: result.Task.ID,
	}
}

func validateTaskOutput(schemas SchemaResolver, task ExecutableTask, output json.RawMessage) *RunFailure {
	if len(output) == 0 {
		return &RunFailure{Code: FailureSchema, Message: fmt.Sprintf("task %q output is empty", task.ID), Stage: string(StageExecution), Retryable: false}
	}
	if !json.Valid(output) {
		return &RunFailure{Code: FailureSchema, Message: fmt.Sprintf("task %q output is not valid JSON", task.ID), Stage: string(StageExecution), Retryable: false}
	}
	validator, ok := schemas.(schemaValidator)
	if !ok || validator == nil {
		return &RunFailure{Code: FailureSchema, Message: fmt.Sprintf("task %q schema registry cannot validate output", task.ID), Stage: string(StageExecution), Retryable: false}
	}
	if err := validator.Validate(task.OutputSchema, output); err != nil {
		return &RunFailure{Code: FailureSchema, Message: err.Error(), Stage: string(StageExecution), Retryable: false}
	}
	return nil
}

func failureForTask(taskID string, failure RunFailure) *RunFailure {
	failure.TaskID = taskID
	return &failure
}

func planFailure(err error) RunFailure {
	code, retryable := FailurePlan, false
	switch {
	case errors.Is(err, ErrLeaseFenced):
		code, retryable = FailureLease, true
	case errors.Is(err, ErrBudgetExceeded):
		code = FailureBudget
	}
	return RunFailure{Code: code, Message: err.Error(), Stage: string(StagePlanning), Retryable: retryable}
}

func planFailureStatus(err error) RunStatus {
	if errors.Is(err, ErrBudgetExceeded) {
		return RunBudgetExhausted
	}
	return RunFailed
}

func runFailureFromContext(ctx context.Context, err error) RunFailure {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrLeaseFenced) || errors.Is(err, ErrLeaseFenced) {
		message := err.Error()
		if cause != nil {
			message = cause.Error()
		}
		return RunFailure{Code: FailureLease, Message: message, Stage: string(StageExecution), Retryable: true}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return RunFailure{Code: FailureTimeout, Message: err.Error(), Stage: string(StageExecution), Retryable: false}
	}
	if ctx.Err() == context.Canceled {
		return RunFailure{Code: FailureCancelled, Message: err.Error(), Stage: string(StageExecution), Retryable: false}
	}
	if errors.Is(err, ErrBudgetExceeded) {
		return RunFailure{Code: FailureBudget, Message: err.Error(), Stage: string(StageExecution), Retryable: false}
	}
	return RunFailure{Code: FailureExecution, Message: err.Error(), Stage: string(StageExecution), Retryable: false}
}

func runFailureStatus(ctx context.Context, err error) RunStatus {
	if errors.Is(context.Cause(ctx), ErrLeaseFenced) || errors.Is(err, ErrLeaseFenced) {
		return RunFailed
	}
	if ctx.Err() == context.DeadlineExceeded {
		return RunTimedOut
	}
	if ctx.Err() == context.Canceled {
		return RunCancelled
	}
	if errors.Is(err, ErrBudgetExceeded) {
		return RunBudgetExhausted
	}
	return RunFailed
}

func failureError(failure RunFailure) error {
	return &RunFailureError{Failure: failure}
}

func investigationRunID(contract InvestigationContract) string {
	// Transport contracts already carry the durable workflow identity. Keeping
	// it as the Run ID makes restart, load, and cancel independent of process memory.
	if id := strings.TrimSpace(contract.ID); strings.HasPrefix(id, "workflow_") {
		return id
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(contract.ID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(contract.Question))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(contract.CreatedAt.UTC().Format(time.RFC3339Nano)))
	digest := hex.EncodeToString(hash.Sum(nil))
	return "run_" + digest[:24]
}

// ContractRunID exposes the deterministic run identity for transport adapters
// that need to load or cancel a run before execution has produced its snapshot.
func ContractRunID(contract InvestigationContract) string {
	return investigationRunID(contract)
}
