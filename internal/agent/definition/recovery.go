package definition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/log"
)

// StartDurableRecoveryWorker starts the process-local dispatcher for parent
// logical-loop checkpoints. Database startup only claims leases and enqueues;
// this worker is the first place where provider/model execution is allowed.
func (runtime *Runtime) StartDurableRecoveryWorker(ctx context.Context, pollInterval time.Duration) {
	if runtime == nil || runtime.runStore == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	runtime.recoveryMu.Lock()
	if runtime.recoveryCancel != nil {
		runtime.recoveryMu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	runtime.recoveryGeneration++
	generation := runtime.recoveryGeneration
	runtime.recoveryCancel = cancel
	runtime.recoveryMu.Unlock()
	go func() {
		defer func() {
			runtime.recoveryMu.Lock()
			if runtime.recoveryGeneration == generation {
				runtime.recoveryCancel = nil
			}
			runtime.recoveryMu.Unlock()
		}()
		poll := func() {
			if err := runtime.runOneRecoveryWork(workerCtx); err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.WarnfCtx(workerCtx, "[agent] durable parent recovery failed: %v", err)
			}
		}
		poll()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
}

// StopDurableRecoveryWorker stops the runtime-owned dispatcher. It is safe to
// call during runtime reload and platform shutdown.
func (runtime *Runtime) StopDurableRecoveryWorker() {
	if runtime == nil {
		return
	}
	runtime.recoveryMu.Lock()
	cancel := runtime.recoveryCancel
	runtime.recoveryCancel = nil
	runtime.recoveryGeneration++
	runtime.recoveryMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (runtime *Runtime) runOneRecoveryWork(ctx context.Context) error {
	owner := runtime.runStore.LeaseOwner()
	item, err := runtime.runStore.ClaimWorkItemByKind(ctx, agentrun.WorkParentResume, owner, time.Now().UTC(), 30*time.Second)
	if err != nil {
		return err
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go renewRecoveryWork(workCtx, runtime.runStore, item, stop, cancel)

	resultErr := runtime.resumeParent(workCtx, item)
	state := agentrun.WorkSucceeded
	lastError := ""
	if resultErr != nil {
		state = agentrun.WorkFailed
		lastError = resultErr.Error()
	}
	completeErr := runtime.runStore.CompleteWorkItem(context.WithoutCancel(ctx), item.WorkID, item.LeaseOwner, item.LeaseFence, state, lastError)
	return errors.Join(resultErr, completeErr)
}

func renewRecoveryWork(ctx context.Context, queue *agentrun.Store, item agentrun.WorkItem, stop <-chan struct{}, cancel context.CancelFunc) {
	ttl := 30 * time.Second
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	if interval >= ttl {
		interval = ttl / 2
		if interval <= 0 {
			interval = time.Millisecond
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			if err := queue.RenewWorkItem(context.WithoutCancel(ctx), item.WorkID, item.LeaseOwner, item.LeaseFence, now, ttl); err != nil {
				cancel()
				return
			}
		}
	}
}

func parseRunStartedAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("recovered run started_at is empty")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse recovered run started_at: %w", err)
	}
	return startedAt.UTC(), nil
}

type parentRecoveryPayload struct {
	RunID           string `json:"run_id"`
	CheckpointStep  int    `json:"checkpoint_step"`
	CheckpointPhase string `json:"checkpoint_phase"`
	LeaseFence      int64  `json:"lease_fence"`
}

type parentRecoveryPlan struct {
	request    *agentapi.RunRequest
	execution  preparedExecution
	input      execution.Input
	checkpoint agentrun.LogicalCheckpoint
}

func (runtime *Runtime) resumeParent(ctx context.Context, item agentrun.WorkItem) error {
	var payload parentRecoveryPayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return fmt.Errorf("decode parent recovery work: %w", err)
	}
	if payload.RunID == "" || payload.RunID != item.RunID {
		return fmt.Errorf("parent recovery work run mismatch")
	}
	failBeforeExecution := func(code string, cause error) error {
		if payload.LeaseFence <= 0 {
			return cause
		}
		outcome := agentrun.Outcome{
			Status:    agentrun.StatusFailed,
			ErrorCode: code,
			Err:       cause,
			Evidence:  agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		}
		if err := runtime.runStore.CompleteFenced(payload.RunID, runtime.runStore.LeaseOwner(), payload.LeaseFence, outcome); err != nil {
			return errors.Join(cause, fmt.Errorf("persist recovered parent failure: %w", err))
		}
		runtime.hub.ProjectTerminal(payload.RunID, outcome)
		return cause
	}

	plan, err := runtime.loadParentRecoveryPlan(ctx, payload, failBeforeExecution)
	if err != nil {
		return err
	}
	return runtime.executeRecoveredParent(ctx, payload.RunID, plan, failBeforeExecution)
}

// loadParentRecoveryPlan validates the recovery payload against the persisted
// run and checkpoint, then re-prepares the execution plan with the authoritative
// limits and deadline.
func (runtime *Runtime) loadParentRecoveryPlan(
	ctx context.Context,
	payload parentRecoveryPayload,
	failBeforeExecution func(code string, cause error) error,
) (parentRecoveryPlan, error) {
	detail, err := runtime.loadRecoveryRunDetail(payload, failBeforeExecution)
	if err != nil {
		return parentRecoveryPlan{}, err
	}
	checkpoint, state, err := runtime.loadRecoveryCheckpoint(ctx, payload, failBeforeExecution)
	if err != nil {
		return parentRecoveryPlan{}, err
	}
	request, err := resolveRecoveryRequest(payload, detail, checkpoint, state, failBeforeExecution)
	if err != nil {
		return parentRecoveryPlan{}, err
	}
	startedAt, err := parseRunStartedAt(detail.StartedAt)
	if err != nil {
		return parentRecoveryPlan{}, failBeforeExecution("recovery_started_at_invalid", err)
	}
	now := time.Now().UTC()
	if !request.Limits.Deadline.After(now) {
		return parentRecoveryPlan{}, failBeforeExecution("deadline_exceeded", fmt.Errorf("recovered run deadline %s has expired", request.Limits.Deadline.UTC().Format(time.RFC3339Nano)))
	}
	executionPlan, err := runtime.prepareAt(*request, startedAt, now)
	if err != nil {
		code := "recovery_prepare_failed"
		if !request.Limits.Deadline.After(time.Now().UTC()) {
			code = "deadline_exceeded"
		}
		return parentRecoveryPlan{}, failBeforeExecution(code, fmt.Errorf("re-prepare recovered run: %w", err))
	}
	return parentRecoveryPlan{
		request:    request,
		execution:  executionPlan,
		input:      state.Input,
		checkpoint: checkpoint,
	}, nil
}

func (runtime *Runtime) loadRecoveryRunDetail(
	payload parentRecoveryPayload,
	failBeforeExecution func(code string, cause error) error,
) (*agentrun.Detail, error) {
	detail, err := runtime.runStore.Get(payload.RunID)
	if err != nil {
		return nil, failBeforeExecution("recovery_load_failed", fmt.Errorf("load recovered run: %w", err))
	}
	return detail, nil
}

func (runtime *Runtime) loadRecoveryCheckpoint(
	ctx context.Context,
	payload parentRecoveryPayload,
	failBeforeExecution func(code string, cause error) error,
) (agentrun.LogicalCheckpoint, execution.LogicalLoopState, error) {
	checkpoint, err := runtime.runStore.GetLogicalCheckpoint(ctx, payload.RunID)
	if err != nil {
		return agentrun.LogicalCheckpoint{}, execution.LogicalLoopState{}, failBeforeExecution("checkpoint_load_failed", fmt.Errorf("load logical checkpoint: %w", err))
	}
	if (payload.CheckpointStep != 0 && checkpoint.StepNo != payload.CheckpointStep) ||
		(payload.CheckpointPhase != "" && checkpoint.Phase != payload.CheckpointPhase) {
		return agentrun.LogicalCheckpoint{}, execution.LogicalLoopState{}, failBeforeExecution("checkpoint_changed", fmt.Errorf("recovery work checkpoint does not match persisted checkpoint"))
	}
	state, err := execution.UnmarshalLogicalLoopState(checkpoint.State)
	if err != nil {
		return agentrun.LogicalCheckpoint{}, execution.LogicalLoopState{}, failBeforeExecution("checkpoint_invalid", fmt.Errorf("decode logical checkpoint: %w", err))
	}
	return checkpoint, state, nil
}

func resolveRecoveryRequest(
	payload parentRecoveryPayload,
	detail *agentrun.Detail,
	checkpoint agentrun.LogicalCheckpoint,
	state execution.LogicalLoopState,
	failBeforeExecution func(code string, cause error) error,
) (*agentapi.RunRequest, error) {
	request := state.Request
	if request == nil {
		request = state.Input.OriginalRequest
	}
	if request == nil {
		return nil, failBeforeExecution("checkpoint_request_missing", fmt.Errorf("logical checkpoint does not contain replay request"))
	}
	if request.RunID != payload.RunID || detail.DefinitionHash != request.DefinitionHash || detail.AgentID != request.Agent.ID {
		return nil, failBeforeExecution("checkpoint_identity_mismatch", fmt.Errorf("logical checkpoint identity does not match persisted run"))
	}
	if checkpoint.InputHash != "" && checkpoint.InputHash != hashBytes(request.Input) {
		return nil, failBeforeExecution("checkpoint_input_mismatch", fmt.Errorf("logical checkpoint input hash mismatch"))
	}
	// The persisted, resolved limits and StartedAt are authoritative. In
	// particular, recovery must not recompute deadline as recovery-now plus
	// the definition timeout. An expired run is fenced failed before any model
	// call; a live run gets only the original deadline's remaining time.
	request.Limits = detail.RunLimits
	return request, nil
}

// executeRecoveredParent attaches the durable budget lease and replays the
// prepared execution plan against the persisted logical-loop state.
func (runtime *Runtime) executeRecoveredParent(
	ctx context.Context,
	runID string,
	plan parentRecoveryPlan,
	failBeforeExecution func(code string, cause error) error,
) error {
	budgetGate, err := runtime.runStore.AttachDurableBudgetContext(ctx, runID, plan.execution.snapshot.Limits)
	if err != nil {
		return failBeforeExecution("recovery_lease_lost", fmt.Errorf("attach recovered budget lease: %w", err))
	}
	trace, ownsTrace := beginExecutionTrace(ctx)
	run := &activeRun{
		runtime: runtime, start: runStart(*plan.request), execution: plan.execution,
		recorder: &usageRecorder{store: runtime.usageStore,
			inputPriceMicrosPerMillionTokens:  plan.execution.definition.Model.InputPriceMicrosPerMillionTokens,
			outputPriceMicrosPerMillionTokens: plan.execution.definition.Model.OutputPriceMicrosPerMillionTokens,
			limits:                            plan.execution.snapshot.Limits},
		budget: budgetGate, trace: trace, ownsTrace: ownsTrace, executed: true,
	}
	runCtx := run.Context(ctx)
	_, runErr := run.executePrepared(runCtx, *plan.request, plan.execution, plan.input, &plan.checkpoint)
	if runErr != nil {
		finishErr := run.Finish(&agentapi.RunError{Code: runtimeErrorCode(runErr), Message: runErr.Error()})
		return errors.Join(runErr, finishErr)
	}
	return run.Finish(nil)
}
