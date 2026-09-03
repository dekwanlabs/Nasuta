package definition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/budget"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

func hashRawInput(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func runtimeErrorCode(err error) string {
	if errors.Is(err, agentapi.ErrBudgetExceeded) {
		return "budget_exhausted"
	}
	return "runtime_failed"
}

// Run resolves and executes one exact immutable definition version.
func (runtime *Runtime) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	request = redactRequest(request)
	execution, err := runtime.prepare(request)
	if err != nil {
		return failedRun(request.RunID, "invalid_request", err), nil
	}
	trace, ownsTrace := beginExecutionTrace(ctx)
	managed, err := runtime.beginPrepared(
		ctx, runStart(request), execution, trace, ownsTrace,
		agentapi.RunBudgetGateFromContext(ctx),
	)
	if err != nil {
		return agentapi.RunResult{}, err
	}
	runCtx := managed.Context(ctx)
	result, err := managed.Execute(runCtx, request)
	if err != nil {
		_ = managed.Finish(&agentapi.RunError{Code: runtimeErrorCode(err), Message: err.Error()})
		return agentapi.RunResult{}, err
	}
	if err := managed.Finish(nil); err != nil {
		return agentapi.RunResult{}, err
	}
	return result, nil
}

// Begin creates a Run before model-backed scenario preparation starts.
func (runtime *Runtime) Begin(
	ctx context.Context,
	start agentapi.RunStart,
) (agentapi.ManagedRun, error) {
	start = redactStart(start)
	execution, err := runtime.prepare(agentapi.RunRequest{
		RunID: start.RunID, Agent: start.Agent, DefinitionHash: start.DefinitionHash,
		Selection: start.Selection, Input: start.Input, Permissions: start.Permissions,
		ToolScope: start.ToolScope, Policy: start.Policy, Actor: start.Actor,
		Limits: start.Limits, Delegation: start.Delegation, Correlation: start.Correlation,
	})
	if err != nil {
		return nil, err
	}
	trace, ownsTrace := beginExecutionTrace(ctx)
	return runtime.beginPrepared(
		ctx, start, execution, trace, ownsTrace,
		agentapi.RunBudgetGateFromContext(ctx),
	)
}

func beginExecutionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func (runtime *Runtime) beginPrepared(
	lifecycleCtx context.Context,
	start agentapi.RunStart,
	execution preparedExecution,
	trace *runtrace.Scope,
	ownsTrace bool,
	inheritedBudget agentapi.RunBudgetGate,
) (*activeRun, error) {
	recorder := &usageRecorder{
		store:                             runtime.usageStore,
		inputPriceMicrosPerMillionTokens:  execution.definition.Model.InputPriceMicrosPerMillionTokens,
		outputPriceMicrosPerMillionTokens: execution.definition.Model.OutputPriceMicrosPerMillionTokens,
		limits:                            execution.snapshot.Limits,
	}
	var durableCreatedBudget agentapi.RunBudgetGate
	var err error
	if inheritedBudget == nil && runtime.runStore != nil && runtime.runStore.DurableBudgetEnabled() && hasBudgetLimits(execution.snapshot.Limits) {
		durableCreatedBudget, err = runtime.runStore.CreateWithDurableBudgetContext(lifecycleCtx, runtime.runRecord(start, execution), execution.snapshot.Limits)
	} else {
		err = runtime.createRun(start, execution)
	}
	if err != nil {
		if ownsTrace {
			trace.Close()
		}
		runtime.hub.CompleteTransient(start.RunID, agentrun.Outcome{
			Status: agentrun.StatusFailed, ErrorCode: "persistence_failed", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		})
		return nil, err
	}
	budgetGate := inheritedBudget
	if budgetGate == nil {
		if durableCreatedBudget != nil {
			budgetGate = durableCreatedBudget
		} else {
			budgetGate = newRunBudget(execution.snapshot.Limits)
		}
	}
	return &activeRun{
		runtime: runtime, start: start, execution: execution, recorder: recorder,
		budget: budgetGate, trace: trace, ownsTrace: ownsTrace,
	}, nil
}

func newRunBudget(limits agentapi.RunLimits) agentapi.RunBudgetGate {
	if !hasBudgetLimits(limits) {
		return nil
	}
	return budget.NewRoot(limits)
}

func hasBudgetLimits(limits agentapi.RunLimits) bool {
	return limits.MaxInputTokens > 0 || limits.MaxTotalTokens > 0 ||
		limits.MaxCostMicros > 0 || limits.ParentAnswerReserve > 0
}

func (runtime *Runtime) runRecord(start agentapi.RunStart, execution preparedExecution) agentrun.Record {
	mode := "single"
	if start.Correlation.WorkflowRunID != "" {
		mode = "workflow"
	}
	return agentrun.Record{ID: start.RunID, RunKind: agentrun.KindAgent, UserID: start.Actor.UserID, SessionID: start.Correlation.SessionID,
		AgentID: execution.snapshot.AgentID, DefinitionVersion: execution.snapshot.DefinitionVersion, DefinitionHash: execution.snapshot.DefinitionHash,
		Selection: execution.snapshot.Selection, ToolSnapshotID: execution.snapshot.ToolSnapshotID, InputSchemaVersion: execution.snapshot.InputSchemaVersion,
		OutputSchemaVersion: execution.snapshot.OutputSchemaVersion, ParentRunID: start.Correlation.ParentRunID, CapabilityID: start.Delegation.Capability.ID,
		CapabilityVersion: start.Delegation.Capability.Version, CapabilityHash: start.Delegation.CapabilityContentHash, DelegationID: start.Delegation.DelegationID,
		DelegationDepth: start.Delegation.Depth, RunLimits: execution.snapshot.Limits, CapabilityRevision: start.Delegation.CapabilityRegistryRevision,
		WorkflowRunID: start.Correlation.WorkflowRunID, WorkflowNodeID: start.Correlation.NodeID, Question: string(start.Input), Mode: mode, MaxSteps: execution.snapshot.Limits.MaxSteps}
}

func (runtime *Runtime) createRun(start agentapi.RunStart, execution preparedExecution) error {
	if runtime == nil || runtime.runStore == nil {
		return nil
	}
	if err := runtime.runStore.Create(runtime.runRecord(start, execution)); err != nil {
		return fmt.Errorf("create definition run %q: %w", start.RunID, err)
	}
	return nil
}

func (run *activeRun) Context(ctx context.Context) context.Context {
	if root, ok := run.budget.(interface{ StartHeartbeat(context.Context) }); ok {
		root.StartHeartbeat(ctx)
	}
	if agentapi.RunBudgetGateFromContext(ctx) == nil && run.budget != nil {
		ctx = agentapi.WithRunBudgetGate(ctx, run.budget)
	}
	ctx = runtrace.WithScope(ctx, run.trace)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: run.start.RunID, ParentRunID: run.start.Correlation.ParentRunID,
		WorkflowRunID: run.start.Correlation.WorkflowRunID, AgentRunID: run.start.RunID,
		WorkflowNodeID: run.start.Correlation.NodeID,
	})
	ctx = llm.WithUsageRecorder(ctx, run.start.RunID, run.recorder)
	return llm.WithCallLifecycleObserver(ctx, run.start.RunID, run.runtime.hub)
}

func (run *activeRun) RecordEvidence(
	ctx context.Context,
	units []tool.EvidenceUnit,
) error {
	if run == nil || run.runtime == nil || run.runtime.runStore == nil || len(units) == 0 {
		return nil
	}
	_, err := run.runtime.runStore.PutEvidenceLedger(
		ctx,
		run.start.RunID,
		units,
	)
	return err
}

func (run *activeRun) Execute(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	run.mu.Lock()
	if run.executed {
		run.mu.Unlock()
		return agentapi.RunResult{}, fmt.Errorf("definition run %q was already executed", run.start.RunID)
	}
	if run.finished {
		run.mu.Unlock()
		return agentapi.RunResult{}, fmt.Errorf("definition run %q is already finished", run.start.RunID)
	}
	run.executed = true
	run.mu.Unlock()

	request = redactRequest(request)
	execution, err := run.validateRequest(request)
	if err != nil {
		outcome := agentrun.Outcome{
			Status: agentrun.StatusFailed, ErrorCode: "invalid_request", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		}
		outcome = run.mergePreparationOutcome(outcome)
		run.setOutcome(outcome)
		return failedRun(request.RunID, "invalid_request", err), nil
	}
	input := compileRequest(execution.definition, request)
	input.OfferedToolIDs = execution.offeredTools
	input.ToolPruningApplied = execution.pruneApplied
	execution.snapshot.PromptHash = hashMessages(input.Messages)
	initialState, err := agentexecution.MarshalLogicalLoopState(agentexecution.LogicalLoopState{
		Version: 1, Request: cloneRunRequest(request), Input: input, Messages: append([]llm.Message(nil), input.Messages...),
	})
	if err != nil {
		outcome := agentrun.Outcome{Status: agentrun.StatusFailed, ErrorCode: "checkpoint_persistence_failed", Err: err, Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable}}
		run.setOutcome(outcome)
		return failedRun(request.RunID, "checkpoint_persistence_failed", err), nil
	}
	if err := run.persistLogicalCheckpoint(ctx, agentexecution.LogicalLoopCheckpoint{StepNo: 0, Phase: "running", State: initialState}, execution.snapshot.PromptHash); err != nil {
		outcome := agentrun.Outcome{Status: agentrun.StatusFailed, ErrorCode: "checkpoint_persistence_failed", Err: err, Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable}}
		run.setOutcome(outcome)
		return failedRun(request.RunID, "checkpoint_persistence_failed", err), nil
	}

	return run.executePrepared(ctx, request, execution, input, nil)
}
func (run *activeRun) persistLogicalCheckpoint(ctx context.Context, checkpoint agentexecution.LogicalLoopCheckpoint, promptHash string) error {
	if run == nil || run.runtime == nil || run.runtime.runStore == nil {
		return nil
	}
	root, ok := run.budget.(interface{ LeaseInfo() (string, int64, error) })
	if !ok {
		return nil
	}
	owner, fence, err := root.LeaseInfo()
	if err != nil {
		return err
	}
	if fence <= 0 {
		return fmt.Errorf("logical checkpoint requires a positive lease fence")
	}
	state := checkpoint.State
	if len(state) == 0 {
		// The post-run compatibility call has no execution state. Keep the
		// previously persisted checkpoint authoritative rather than replacing
		// it with an invalid legacy envelope.
		return nil
	}
	if _, err := agentexecution.UnmarshalLogicalLoopState(state); err != nil {
		return fmt.Errorf("validate logical loop checkpoint: %w", err)
	}
	return run.runtime.runStore.SaveLogicalCheckpoint(context.WithoutCancel(ctx), agentrun.LogicalCheckpoint{
		RunID: run.start.RunID, StepNo: checkpoint.StepNo, Phase: checkpoint.Phase,
		InputHash: hashBytes(run.start.Input), PromptHash: promptHash, State: state,
		LeaseOwner: owner, LeaseFence: fence,
	})
}

func (run *activeRun) emitDelegationAdoptions(
	runID string,
	adoptions []agentapi.DelegationAdoption,
) {
	if run == nil || run.runtime == nil || run.runtime.hub == nil {
		return
	}
	for _, adoption := range adoptions {
		run.runtime.hub.EmitEvent(
			agentrun.EventDelegationAdoptionEvaluated,
			agentrun.ExecutionEvent{
				RunID:          runID,
				ParentRunID:    runID,
				DelegationID:   adoption.DelegationID,
				ReportIDs:      append([]string(nil), adoption.AdoptedReportIDs...),
				Status:         string(adoption.Status),
				AdoptionStatus: string(adoption.Status),
				Reason:         adoption.Reason,
			},
		)
	}
}

func (run *activeRun) Finish(runError *agentapi.RunError) error {
	outcome, err := run.prepareFinishOutcome(runError)
	if err != nil {
		return err
	}
	if run.ownsTrace {
		run.trace.Close()
	}
	completionErr := run.persistFinishOutcome(outcome)
	releaseErr := run.releaseFinishLease()
	return errors.Join(completionErr, releaseErr)
}

// prepareFinishOutcome validates the run state, finalizes the outcome (merging
// preparation evidence and applying runError), and marks the run finished.
func (run *activeRun) prepareFinishOutcome(runError *agentapi.RunError) (agentrun.Outcome, error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.finished {
		return agentrun.Outcome{}, fmt.Errorf("definition run %q is already finished", run.start.RunID)
	}
	if !run.executed && runError == nil {
		return agentrun.Outcome{}, fmt.Errorf("definition run %q has not executed", run.start.RunID)
	}
	outcome := run.outcome
	if !run.outcomeSet {
		outcome = mergePreparationOutcome(outcome, run.preparationEvidence)
	}
	if runError != nil {
		outcome = applyRunError(outcome, runError, run.start.Policy.RedactSensitive)
	}
	run.finished = true
	return outcome, nil
}

func applyRunError(outcome agentrun.Outcome, runError *agentapi.RunError, redact bool) agentrun.Outcome {
	code := strings.TrimSpace(runError.Code)
	if code == "" {
		code = "scenario_failed"
	}
	message := strings.TrimSpace(runError.Message)
	if message == "" {
		message = code
	}
	if redact {
		message = platform.RedactSensitiveText(message)
	}
	outcome.Status = agentrun.StatusFailed
	outcome.ErrorCode = code
	outcome.Err = errors.New(message)
	if outcome.Evidence.Status == "" {
		outcome.Evidence.Status = agentrun.EvidenceUnavailable
	}
	return outcome
}

// persistFinishOutcome publishes the outcome, preferring the fenced durable
// completion path when the budget is durable and exposes lease information.
func (run *activeRun) persistFinishOutcome(outcome agentrun.Outcome) error {
	completedByLease, fencedCompletion, completionErr := run.completeFencedIfDurable(outcome)
	if completedByLease {
		run.runtime.hub.ProjectTerminal(run.start.RunID, outcome)
	} else if !fencedCompletion {
		run.runtime.hub.Complete(run.start.RunID, outcome)
	}
	return completionErr
}

func (run *activeRun) completeFencedIfDurable(outcome agentrun.Outcome) (completedByLease, fencedCompletion bool, completionErr error) {
	if run.runtime.runStore == nil || !run.runtime.runStore.DurableBudgetEnabled() {
		return false, false, nil
	}
	root, ok := run.budget.(interface{ LeaseInfo() (string, int64, error) })
	if !ok {
		return false, false, nil
	}
	fencedCompletion = true
	owner, fence, leaseErr := root.LeaseInfo()
	if leaseErr != nil {
		return false, fencedCompletion, fmt.Errorf("read durable run lease: %w", leaseErr)
	}
	if completeErr := run.runtime.runStore.CompleteFenced(run.start.RunID, owner, fence, outcome); completeErr != nil {
		// Never fall back to the unfenced Hub.Complete path. A stale
		// owner must not publish or overwrite a result after reclamation.
		return false, fencedCompletion, fmt.Errorf("persist fenced run outcome: %w", completeErr)
	}
	return true, fencedCompletion, nil
}

func (run *activeRun) releaseFinishLease() error {
	if lease, ok := run.budget.(interface{ Close() }); ok {
		lease.Close()
	}
	if lease, ok := run.budget.(interface{ ReleaseLease() error }); ok {
		if err := lease.ReleaseLease(); err != nil {
			return fmt.Errorf("release durable budget lease for run %q: %w", run.start.RunID, err)
		}
	}
	return nil
}

func (run *activeRun) setOutcome(outcome agentrun.Outcome) {
	run.mu.Lock()
	run.outcome = outcome
	run.outcomeSet = true
	run.mu.Unlock()
}

func (run *activeRun) executePrepared(
	ctx context.Context,
	request agentapi.RunRequest,
	execution preparedExecution,
	input agentexecution.Input,
	resume *agentrun.LogicalCheckpoint,
) (agentapi.RunResult, error) {
	maxOutputTokens := effectiveOutputTokens(execution.definition, execution.snapshot.Limits)
	conclusionMaxTokens := effectiveConclusionTokens(
		run.runtime.settings.conclusionMaxTokens,
		maxOutputTokens,
	)
	client := llm.NewLLMClientWithHTTPAndProvider(
		run.runtime.settings.baseURL,
		run.runtime.settings.apiKey,
		execution.snapshot.Model,
		execution.snapshot.Provider,
		maxOutputTokens,
		nil,
	)
	observer := run.observer()
	budgetCheck := func() error {
		if gate := agentapi.RunBudgetGateFromContext(ctx); gate != nil {
			if err := gate.Check(); err != nil {
				return err
			}
		}
		return run.recorder.CheckLimits()
	}
	loop := agentexecution.NewAgent(client, run.runtime.executor, agentexecution.Config{
		MaxSteps:                          execution.snapshot.Limits.MaxSteps,
		MaxToolCalls:                      execution.snapshot.Limits.MaxToolCalls,
		Timeout:                           time.Until(execution.snapshot.Limits.Deadline),
		AnswerReserve:                     execution.answerReserve,
		AnswerMaxTokens:                   maxOutputTokens,
		ConclusionMaxTokens:               conclusionMaxTokens,
		ContextWindow:                     execution.snapshot.Budget.ContextTokens,
		MaxInputTokens:                    execution.snapshot.Limits.MaxInputTokens,
		MaxContextTokens:                  execution.snapshot.Limits.MaxContextTokens,
		MaxToolResultBytes:                execution.definition.Budget.MaxToolResultBytes,
		MaxContinueRounds:                 execution.definition.Budget.MaxContinueRounds,
		StructuredOutput:                  execution.structuredOutput,
		ModelParameters:                   execution.modelParameters,
		InputPriceMicrosPerMillionTokens:  execution.definition.Model.InputPriceMicrosPerMillionTokens,
		OutputPriceMicrosPerMillionTokens: execution.definition.Model.OutputPriceMicrosPerMillionTokens,
		BudgetCheck:                       budgetCheck,
		DisableLegacyAnswerRecovery:       run.runtime.settings.disableLegacyAnswerRecovery,
		Checkpoint: func(checkpoint agentexecution.LogicalLoopCheckpoint) error {
			return run.persistLogicalCheckpoint(ctx, checkpoint, execution.snapshot.PromptHash)
		},
	}, observer, run.runtime.hub)
	loop.SetOnFirstAnswerToken(func(runID string) {
		run.runtime.hub.EmitPhase(runID, "找到啦，我来把答案写出来 ✍️")
	})
	runCtx := llm.WithUsageRecorder(ctx, request.RunID, run.recorder)
	var result *agentexecution.RunResult
	var runErr error
	if resume != nil {
		result, runErr = loop.RunCompiledFromCheckpoint(runCtx, request.RunID, agentexecution.LogicalLoopCheckpoint{StepNo: resume.StepNo, Phase: resume.Phase, State: resume.State}, execution.toolSnapshot)
	} else {
		result, runErr = loop.RunCompiledWithRequest(runCtx, request.RunID, input, &request, execution.toolSnapshot)
	}
	checkpointPhase := "completed"
	if runErr != nil {
		checkpointPhase = "interrupted"
	}
	if checkpointErr := run.persistLogicalCheckpoint(ctx, agentexecution.LogicalLoopCheckpoint{StepNo: result.Steps, Phase: checkpointPhase}, execution.snapshot.PromptHash); checkpointErr != nil && runErr == nil {
		runErr = checkpointErr
	}
	publicResult, outcome := mapResult(
		request.RunID,
		result,
		runErr,
		context.Cause(ctx),
		run.recorder.Usage(),
		referencesFromRequest(request.Context),
		run.runtime.schemas,
		execution.definition.OutputSchema,
		outputRecoveryContext{
			AgentID: request.Agent.ID,
			Input:   request.Input,
			Context: request.Context,
			StrictOutput: request.Agent.ID == "investigator.docs" &&
				request.Delegation.Depth <= 0,
		},
	)
	outcome = run.mergePreparationOutcome(outcome)
	publicResult.Evidence = publicEvidence(outcome.Evidence)
	if request.Policy.RedactSensitive {
		publicResult = redactResult(publicResult)
		outcome = redactOutcome(outcome)
	}
	run.emitDelegationAdoptions(request.RunID, publicResult.DelegationAdoptions)
	run.setOutcome(outcome)
	return publicResult, nil
}

// fields that may have been redacted or rendered for display.
// Outcome returns the durable outcome computed by Execute for this run.
func (run *activeRun) Outcome() agentrun.Outcome {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.outcome
}

func (run *activeRun) validateRequest(
	request agentapi.RunRequest,
) (preparedExecution, error) {
	start := runStart(request)
	if mismatch := runStartMismatch(start, run.start); mismatch != "" {
		return preparedExecution{}, fmt.Errorf(
			"run request does not match the prepared run: %s", mismatch,
		)
	}
	if err := validateMessages(request.Messages); err != nil {
		return preparedExecution{}, err
	}
	contextHash, err := validateContext(request.Context)
	if err != nil {
		return preparedExecution{}, err
	}
	execution := run.execution
	execution.snapshot.ContextHash = contextHash
	offeredTools, err := canonicalToolIDSet(request.ToolScope.OfferedToolIDs)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("offered tools: %w", err)
	}
	available := make(map[tool.ToolID]struct{}, len(execution.snapshot.VisibleToolIDs))
	for _, id := range execution.snapshot.VisibleToolIDs {
		available[tool.ToolID(id)] = struct{}{}
	}
	for id := range offeredTools {
		if _, ok := available[id]; !ok {
			return preparedExecution{}, fmt.Errorf("offered tool %q is outside the run snapshot", id)
		}
	}
	execution.offeredTools = offeredTools
	execution.pruneApplied = request.ToolScope.PruneApplied
	return execution, nil
}

func runStartMismatch(actual, prepared agentapi.RunStart) string {
	switch {
	case actual.RunID != prepared.RunID:
		return "run_id"
	case actual.Agent != prepared.Agent:
		return "agent"
	case actual.DefinitionHash != prepared.DefinitionHash:
		return "definition_hash"
	case actual.Selection != prepared.Selection:
		return "selection"
	case !jsonBytesEqual(actual.Input, prepared.Input):
		return "input"
	case actual.Actor != prepared.Actor:
		return "actor"
	case actual.Correlation != prepared.Correlation:
		return "correlation"
	case actual.Policy.RedactSensitive != prepared.Policy.RedactSensitive:
		return "policy.redact_sensitive"
	case !sameOutputContract(actual.Policy.OutputContract, prepared.Policy.OutputContract):
		return "policy.output_contract"
	case !sameRunLimits(actual.Limits, prepared.Limits):
		return "limits"
	case actual.Delegation != prepared.Delegation:
		return "delegation"
	case !samePermissions(actual.Permissions, prepared.Permissions):
		return "permissions"
	case !sameToolScope(actual.ToolScope, prepared.ToolScope):
		return "tool_scope"
	default:
		return ""
	}
}

func effectiveConclusionTokens(configured, answerMax int) int {
	if answerMax <= 0 {
		return configured
	}
	if configured <= 0 {
		configured = answerMax / 4
		if configured <= 0 {
			configured = 1
		}
	}
	if configured > answerMax {
		return answerMax
	}
	return configured
}

func effectiveOutputTokens(definition agentapi.Definition, limits agentapi.RunLimits) int {
	maxOutputTokens := definition.Model.MaxOutputTokens
	if limits.MaxOutputTokens > 0 && limits.MaxOutputTokens < int64(maxOutputTokens) {
		maxOutputTokens = int(limits.MaxOutputTokens)
	}
	return maxOutputTokens
}

func runStart(request agentapi.RunRequest) agentapi.RunStart {
	return agentapi.RunStart{
		RunID: request.RunID, Agent: request.Agent, DefinitionHash: request.DefinitionHash,
		Selection: request.Selection,
		Input:     request.Input, Permissions: clonePermissions(request.Permissions),
		ToolScope: agentapi.ToolScope{
			AllowWrite: request.ToolScope.AllowWrite, RestrictVisible: request.ToolScope.RestrictVisible,
			VisibleToolIDs: append([]string(nil), request.ToolScope.VisibleToolIDs...),
		},
		Policy: request.Policy, Limits: request.Limits, Delegation: request.Delegation,
		Actor: request.Actor, Correlation: request.Correlation,
	}
}

func sameOutputContract(left, right agentapi.RunOutputContract) bool {
	if left.Kind != right.Kind || left.RequireMermaid != right.RequireMermaid ||
		left.MaxHops != right.MaxHops || len(left.Subjects) != len(right.Subjects) {
		return false
	}
	for index := range left.Subjects {
		if left.Subjects[index] != right.Subjects[index] {
			return false
		}
	}
	return true
}

func sameRunLimits(left, right agentapi.RunLimits) bool {
	return left.Deadline.Equal(right.Deadline) &&
		left.MaxSteps == right.MaxSteps &&
		left.MaxToolCalls == right.MaxToolCalls &&
		left.MaxInputTokens == right.MaxInputTokens &&
		left.MaxContextTokens == right.MaxContextTokens &&
		left.MaxOutputTokens == right.MaxOutputTokens &&
		left.MaxTotalTokens == right.MaxTotalTokens &&
		left.MaxCostMicros == right.MaxCostMicros &&
		left.ParentAnswerReserve == right.ParentAnswerReserve
}

func jsonBytesEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func sameToolScope(left, right agentapi.ToolScope) bool {
	if left.AllowWrite != right.AllowWrite || left.RestrictVisible != right.RestrictVisible {
		return false
	}
	leftIDs, leftErr := canonicalToolIDSet(left.VisibleToolIDs)
	rightIDs, rightErr := canonicalToolIDSet(right.VisibleToolIDs)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftIDs, rightIDs)
}

func samePermissions(left, right agentapi.PermissionPolicy) bool {
	leftScopes, leftErr := permissionSet(left)
	rightScopes, rightErr := permissionSet(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftScopes, rightScopes)
}
