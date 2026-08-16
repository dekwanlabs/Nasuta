package definition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

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
	managed, err := runtime.beginPrepared(runStart(request), execution, trace, ownsTrace)
	if err != nil {
		return agentapi.RunResult{}, err
	}
	runCtx := managed.Context(ctx)
	result, err := managed.Execute(runCtx, request)
	if err != nil {
		_ = managed.Finish(&agentapi.RunError{Code: "runtime_failed", Message: err.Error()})
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
	return runtime.beginPrepared(start, execution, trace, ownsTrace)
}

// Start persists a parent Run without inventing an agent definition snapshot.
func (runtime *Runtime) Start(
	ctx context.Context,
	start ScenarioRunStart,
) (ScenarioRun, error) {
	if runtime == nil || runtime.scenarios == nil {
		return nil, fmt.Errorf("create scenario run %q: runtime is unavailable", start.RunID)
	}
	return runtime.scenarios.Start(ctx, start)
}

// Start persists a parent Run without requiring model execution.
func (runtime *ScenarioRuntime) Start(
	ctx context.Context,
	start ScenarioRunStart,
) (ScenarioRun, error) {
	trace, ownsTrace := beginExecutionTrace(ctx)
	if runtime.runStore == nil {
		if ownsTrace {
			trace.Close()
		}
		return nil, fmt.Errorf("create scenario run %q: run store is unavailable", start.RunID)
	}
	if err := runtime.runStore.Create(agentrun.Record{
		ID: start.RunID, RunKind: agentrun.KindQAParent, UserID: start.UserID,
		SessionID: start.SessionID, ParentRunID: start.ParentRunID,
		WorkflowRunID: start.WorkflowRunID,
		Question:      start.Question, Mode: start.Mode,
		RunLimits: start.Limits,
	}); err != nil {
		if ownsTrace {
			trace.Close()
		}
		runtime.hub.CompleteTransient(start.RunID, agentrun.Outcome{
			Status: agentrun.StatusFailed, ErrorCode: "persistence_failed", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		})
		return nil, fmt.Errorf("create scenario run %q: %w", start.RunID, err)
	}
	return &scenarioManagedRun{
		runtime: runtime, start: start, trace: trace, ownsTrace: ownsTrace,
	}, nil
}

// Complete commits a durable terminal fact before projecting it.
func (runtime *Runtime) Complete(
	ctx context.Context,
	runID string,
	outcome agentrun.Outcome,
) error {
	if runtime == nil || runtime.scenarios == nil {
		return fmt.Errorf("complete scenario run %q: runtime is unavailable", runID)
	}
	return runtime.scenarios.Complete(ctx, runID, outcome)
}

// Complete commits a durable terminal fact before projecting it.
func (runtime *ScenarioRuntime) Complete(
	ctx context.Context,
	runID string,
	outcome agentrun.Outcome,
) error {
	if runtime.runStore == nil {
		return fmt.Errorf("complete scenario run %q: run store is unavailable", runID)
	}
	if !outcome.Status.Terminal() {
		return fmt.Errorf(
			"complete scenario run %q with non-terminal status %q",
			runID,
			outcome.Status,
		)
	}
	persisted, err := runtime.runStore.CompleteQAParent(ctx, runID, outcome)
	if err != nil {
		return fmt.Errorf("complete scenario run %q: %w", runID, err)
	}
	if persisted.Status != outcome.Status {
		return fmt.Errorf(
			"complete scenario run %q as %q conflicts with persisted status %q",
			runID,
			outcome.Status,
			persisted.Status,
		)
	}
	runtime.hub.ProjectTerminal(runID, persisted)
	return nil
}

func (run *scenarioManagedRun) Context(ctx context.Context) context.Context {
	ctx = runtrace.WithScope(ctx, run.trace)
	return runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: run.start.RunID, ParentRunID: run.start.ParentRunID,
	})
}

func (run *scenarioManagedRun) RecordEvidence(
	ctx context.Context,
	units []tool.EvidenceUnit,
) error {
	if run == nil || run.runtime == nil || run.runtime.runStore == nil ||
		len(units) == 0 {
		return nil
	}
	_, err := run.runtime.runStore.PutEvidenceLedger(
		ctx,
		run.start.RunID,
		units,
	)
	return err
}

func (run *scenarioManagedRun) Release() {
	run.release.Do(func() {
		if run.ownsTrace {
			run.trace.Close()
		}
	})
}

func beginExecutionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func (runtime *Runtime) beginPrepared(
	start agentapi.RunStart,
	execution preparedExecution,
	trace *runtrace.Scope,
	ownsTrace bool,
) (*activeRun, error) {
	recorder := &usageRecorder{
		store:                             runtime.usageStore,
		inputPriceMicrosPerMillionTokens:  execution.definition.Model.InputPriceMicrosPerMillionTokens,
		outputPriceMicrosPerMillionTokens: execution.definition.Model.OutputPriceMicrosPerMillionTokens,
		limits:                            execution.snapshot.Limits,
	}
	if err := runtime.createRun(start, execution); err != nil {
		if ownsTrace {
			trace.Close()
		}
		runtime.hub.CompleteTransient(start.RunID, agentrun.Outcome{
			Status: agentrun.StatusFailed, ErrorCode: "persistence_failed", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		})
		return nil, err
	}
	return &activeRun{
		runtime: runtime, start: start, execution: execution, recorder: recorder, trace: trace, ownsTrace: ownsTrace,
	}, nil
}

func (runtime *Runtime) createRun(
	start agentapi.RunStart,
	execution preparedExecution,
) error {
	if runtime.scenarios == nil || runtime.scenarios.runStore == nil {
		return nil
	}
	mode := "single"
	if start.Correlation.WorkflowRunID != "" {
		mode = "workflow"
	}
	if err := runtime.scenarios.runStore.Create(agentrun.Record{
		ID: start.RunID, RunKind: agentrun.KindAgent, UserID: start.Actor.UserID,
		SessionID: start.Correlation.SessionID,
		AgentID:   execution.snapshot.AgentID, DefinitionVersion: execution.snapshot.DefinitionVersion,
		DefinitionHash:      execution.snapshot.DefinitionHash,
		Selection:           execution.snapshot.Selection,
		ToolSnapshotID:      execution.snapshot.ToolSnapshotID,
		InputSchemaVersion:  execution.snapshot.InputSchemaVersion,
		OutputSchemaVersion: execution.snapshot.OutputSchemaVersion,
		ParentRunID:         start.Correlation.ParentRunID,
		CapabilityID:        start.Delegation.Capability.ID,
		CapabilityVersion:   start.Delegation.Capability.Version,
		CapabilityHash:      start.Delegation.CapabilityContentHash,
		DelegationID:        start.Delegation.DelegationID,
		DelegationDepth:     start.Delegation.Depth,
		RunLimits:           execution.snapshot.Limits,
		CapabilityRevision:  start.Delegation.CapabilityRegistryRevision,
		WorkflowRunID:       start.Correlation.WorkflowRunID,
		WorkflowNodeID:      start.Correlation.NodeID,
		Question:            string(start.Input), Mode: mode,
		MaxSteps: execution.snapshot.Limits.MaxSteps,
	}); err != nil {
		return fmt.Errorf("create definition run %q: %w", start.RunID, err)
	}
	return nil
}

func (run *activeRun) Context(ctx context.Context) context.Context {
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
	if run == nil || run.runtime == nil || run.runtime.scenarios == nil ||
		run.runtime.scenarios.runStore == nil || len(units) == 0 {
		return nil
	}
	_, err := run.runtime.scenarios.runStore.PutEvidenceLedger(
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

	client := llm.NewLLMClientWithHTTPAndProvider(
		run.runtime.settings.baseURL,
		run.runtime.settings.apiKey,
		execution.snapshot.Model,
		execution.snapshot.Provider,
		execution.definition.Model.MaxOutputTokens,
		nil,
	)
	observer := run.observer()
	loop := agentexecution.NewAgent(client, run.runtime.executor, agentexecution.Config{
		MaxSteps:            execution.snapshot.Limits.MaxSteps,
		MaxToolCalls:        execution.snapshot.Limits.MaxToolCalls,
		Timeout:             time.Until(execution.snapshot.Limits.Deadline),
		AnswerReserve:       run.runtime.settings.answerReserve,
		AnswerMaxTokens:     execution.definition.Model.MaxOutputTokens,
		ConclusionMaxTokens: execution.definition.Model.MaxOutputTokens,
		ContextWindow:       execution.snapshot.Budget.ContextTokens,
		MaxContinueRounds:   execution.definition.Budget.MaxContinueRounds,
		ModelParameters:     execution.modelParameters,
		BudgetCheck:         run.recorder.CheckLimits,
	}, observer, run.runtime.hub)
	loop.SetOnFirstAnswerToken(func(runID string) {
		run.runtime.hub.EmitPhase(runID, "找到啦，我来把答案写出来 ✍️")
	})
	runCtx := llm.WithUsageRecorder(ctx, request.RunID, run.recorder)
	result, runErr := loop.RunCompiled(runCtx, request.RunID, input, execution.toolSnapshot)
	publicResult, outcome := mapResult(
		request.RunID,
		result,
		runErr,
		context.Cause(ctx),
		run.recorder.Usage(),
		referencesFromRequest(request.Context),
		run.runtime.schemas,
		execution.definition.OutputSchema,
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
	run.mu.Lock()
	if run.finished {
		run.mu.Unlock()
		return fmt.Errorf("definition run %q is already finished", run.start.RunID)
	}
	if !run.executed && runError == nil {
		run.mu.Unlock()
		return fmt.Errorf("definition run %q has not executed", run.start.RunID)
	}
	outcome := run.outcome
	if !run.outcomeSet {
		outcome = mergePreparationOutcome(
			outcome,
			run.preparationEvidence,
		)
	}
	if runError != nil {
		code := strings.TrimSpace(runError.Code)
		if code == "" {
			code = "scenario_failed"
		}
		message := strings.TrimSpace(runError.Message)
		if message == "" {
			message = code
		}
		if run.start.Policy.RedactSensitive {
			message = platform.RedactSensitiveText(message)
		}
		outcome.Status = agentrun.StatusFailed
		outcome.ErrorCode = code
		outcome.Err = errors.New(message)
		if outcome.Evidence.Status == "" {
			outcome.Evidence.Status = agentrun.EvidenceUnavailable
		}
	}
	run.finished = true
	run.mu.Unlock()
	if run.ownsTrace {
		run.trace.Close()
	}
	run.runtime.hub.Complete(run.start.RunID, outcome)
	return nil
}

func (run *activeRun) setOutcome(outcome agentrun.Outcome) {
	run.mu.Lock()
	run.outcome = outcome
	run.outcomeSet = true
	run.mu.Unlock()
}

func (run *activeRun) validateRequest(
	request agentapi.RunRequest,
) (preparedExecution, error) {
	start := runStart(request)
	if start.RunID != run.start.RunID || start.Agent != run.start.Agent ||
		start.DefinitionHash != run.start.DefinitionHash ||
		start.Selection != run.start.Selection ||
		!jsonBytesEqual(start.Input, run.start.Input) || start.Actor != run.start.Actor ||
		start.Correlation != run.start.Correlation ||
		start.Policy.RedactSensitive != run.start.Policy.RedactSensitive ||
		!sameRunLimits(start.Limits, run.start.Limits) ||
		start.Delegation != run.start.Delegation ||
		!samePermissions(start.Permissions, run.start.Permissions) ||
		!sameToolScope(start.ToolScope, run.start.ToolScope) {
		return preparedExecution{}, fmt.Errorf("run request does not match the prepared run")
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

func sameRunLimits(left, right agentapi.RunLimits) bool {
	return left.Deadline.Equal(right.Deadline) &&
		left.MaxSteps == right.MaxSteps &&
		left.MaxToolCalls == right.MaxToolCalls &&
		left.MaxTotalTokens == right.MaxTotalTokens &&
		left.MaxCostMicros == right.MaxCostMicros
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
