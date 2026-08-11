package definition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

// Run resolves and executes one exact immutable definition version.
func (runtime *DefinitionRuntime) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	request = redactDefinitionRequest(request)
	execution, err := runtime.prepare(request)
	if err != nil {
		return failedDefinitionRun(request.RunID, "invalid_request", err), nil
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
func (runtime *DefinitionRuntime) Begin(
	ctx context.Context,
	start agentapi.RunStart,
) (agentapi.ManagedRun, error) {
	start = redactDefinitionStart(start)
	execution, err := runtime.prepare(agentapi.RunRequest{
		RunID: start.RunID, Agent: start.Agent, DefinitionHash: start.DefinitionHash,
		Selection: start.Selection, Input: start.Input, Permissions: start.Permissions,
		ToolScope: start.ToolScope, Policy: start.Policy, Actor: start.Actor,
		Correlation: start.Correlation,
	})
	if err != nil {
		return nil, err
	}
	trace, ownsTrace := beginExecutionTrace(ctx)
	return runtime.beginPrepared(start, execution, trace, ownsTrace)
}

// BeginScenario persists a parent Run without inventing an agent definition snapshot.
func (runtime *DefinitionRuntime) BeginScenario(
	ctx context.Context,
	start ScenarioRunStart,
) (ScenarioRun, error) {
	trace, ownsTrace := beginExecutionTrace(ctx)
	if runtime.runStore != nil {
		if err := runtime.runStore.Create(agentrun.RunRecord{
			ID: start.RunID, RunKind: agentrun.RunKindQAParent, UserID: start.UserID,
			SessionID: start.SessionID, ParentRunID: start.ParentRunID,
			Question: start.Question, Mode: start.Mode,
		}); err != nil {
			if ownsTrace {
				trace.Close()
			}
			runtime.hub.CompleteTransient(start.RunID, agentrun.RunOutcome{
				Status: agentrun.RunStatusFailed, ErrorCode: "persistence_failed", Err: err,
				Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
			})
			return nil, fmt.Errorf("create scenario run %q: %w", start.RunID, err)
		}
	}
	return &scenarioManagedRun{
		runtime: runtime, start: start, trace: trace, ownsTrace: ownsTrace,
	}, nil
}

func (run *scenarioManagedRun) Context(ctx context.Context) context.Context {
	ctx = runtrace.WithScope(ctx, run.trace)
	return runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: run.start.RunID, ParentRunID: run.start.ParentRunID,
	})
}

func (run *scenarioManagedRun) Finish(outcome agentrun.RunOutcome) error {
	run.mu.Lock()
	if run.finished {
		run.mu.Unlock()
		return fmt.Errorf("scenario run %q is already finished", run.start.RunID)
	}
	run.finished = true
	run.mu.Unlock()
	if run.ownsTrace {
		run.trace.Close()
	}
	run.runtime.hub.Complete(run.start.RunID, outcome)
	return nil
}

func beginExecutionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func (runtime *DefinitionRuntime) beginPrepared(
	start agentapi.RunStart,
	execution definitionExecution,
	trace *runtrace.Scope,
	ownsTrace bool,
) (*definitionManagedRun, error) {
	recorder := &definitionUsageRecorder{
		store:                             runtime.usageStore,
		inputPriceMicrosPerMillionTokens:  execution.definition.Model.InputPriceMicrosPerMillionTokens,
		outputPriceMicrosPerMillionTokens: execution.definition.Model.OutputPriceMicrosPerMillionTokens,
	}
	if err := runtime.createRun(start, execution); err != nil {
		if ownsTrace {
			trace.Close()
		}
		runtime.hub.CompleteTransient(start.RunID, agentrun.RunOutcome{
			Status: agentrun.RunStatusFailed, ErrorCode: "persistence_failed", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		})
		return nil, err
	}
	return &definitionManagedRun{
		runtime: runtime, start: start, execution: execution, recorder: recorder, trace: trace, ownsTrace: ownsTrace,
	}, nil
}

func (runtime *DefinitionRuntime) createRun(
	start agentapi.RunStart,
	execution definitionExecution,
) error {
	if runtime.runStore == nil {
		return nil
	}
	mode := "single"
	if start.Correlation.WorkflowRunID != "" {
		mode = "workflow"
	}
	if err := runtime.runStore.Create(agentrun.RunRecord{
		ID: start.RunID, RunKind: agentrun.RunKindAgent, UserID: start.Actor.UserID,
		SessionID: start.Correlation.SessionID,
		AgentID:   execution.snapshot.AgentID, DefinitionVersion: execution.snapshot.DefinitionVersion,
		DefinitionHash:      execution.snapshot.DefinitionHash,
		Selection:           execution.snapshot.Selection,
		ToolSnapshotID:      execution.snapshot.ToolSnapshotID,
		InputSchemaVersion:  execution.snapshot.InputSchemaVersion,
		OutputSchemaVersion: execution.snapshot.OutputSchemaVersion,
		ParentRunID:         start.Correlation.ParentRunID,
		WorkflowRunID:       start.Correlation.WorkflowRunID,
		WorkflowNodeID:      start.Correlation.NodeID,
		Question:            string(start.Input), Mode: mode,
		MaxSteps: execution.snapshot.Budget.MaxSteps,
	}); err != nil {
		return fmt.Errorf("create definition run %q: %w", start.RunID, err)
	}
	return nil
}

func (run *definitionManagedRun) Context(ctx context.Context) context.Context {
	ctx = runtrace.WithScope(ctx, run.trace)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: run.start.RunID, ParentRunID: run.start.Correlation.ParentRunID,
		WorkflowRunID: run.start.Correlation.WorkflowRunID, AgentRunID: run.start.RunID,
		WorkflowNodeID: run.start.Correlation.NodeID,
	})
	ctx = llm.WithUsageRecorder(ctx, run.start.RunID, run.recorder)
	return llm.WithCallLifecycleObserver(ctx, run.start.RunID, run.runtime.hub)
}

func (run *definitionManagedRun) Execute(
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

	request = redactDefinitionRequest(request)
	execution, err := run.validateRequest(request)
	if err != nil {
		outcome := agentrun.RunOutcome{
			Status: agentrun.RunStatusFailed, ErrorCode: "invalid_request", Err: err,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidenceUnavailable},
		}
		outcome = run.mergePreparationOutcome(outcome)
		run.setOutcome(outcome)
		return failedDefinitionRun(request.RunID, "invalid_request", err), nil
	}
	input := compileDefinitionRequest(execution.definition, request)
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
	loop := agentexecution.NewAgent(client, run.runtime.executor, agentexecution.AgentConfig{
		MaxSteps:            execution.snapshot.Budget.MaxSteps,
		MaxToolCalls:        request.Policy.MaxToolCalls,
		Timeout:             execution.snapshot.Budget.Timeout,
		AnswerReserve:       run.runtime.settings.answerReserve,
		AnswerMaxTokens:     execution.definition.Model.MaxOutputTokens,
		ConclusionMaxTokens: execution.definition.Model.MaxOutputTokens,
		ContextWindow:       execution.snapshot.Budget.ContextTokens,
		MaxContinueRounds:   run.runtime.settings.maxContinueRounds,
		ModelParameters:     execution.modelParameters,
	}, observer, run.runtime.hub)
	loop.SetOnFirstAnswerToken(func(runID string) {
		run.runtime.hub.EmitPhase(runID, "找到啦，我来把答案写出来 ✍️")
	})
	runCtx := llm.WithUsageRecorder(ctx, request.RunID, run.recorder)
	result, runErr := loop.RunCompiled(runCtx, request.RunID, input, execution.toolSnapshot)
	publicResult, outcome := mapDefinitionResult(
		request.RunID,
		result,
		runErr,
		context.Cause(ctx),
		run.recorder.Usage(),
		contextReferencesFromRequest(request.Context),
		run.runtime.schemas,
		execution.definition.OutputSchema,
	)
	outcome = run.mergePreparationOutcome(outcome)
	publicResult.Evidence = publicEvidence(outcome.Evidence)
	if request.Policy.RedactSensitive {
		publicResult = redactDefinitionResult(publicResult)
		outcome = redactDefinitionOutcome(outcome)
	}
	run.setOutcome(outcome)
	return publicResult, nil
}

func (run *definitionManagedRun) Finish(runError *agentapi.RunError) error {
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
		outcome.Status = agentrun.RunStatusFailed
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

func (run *definitionManagedRun) setOutcome(outcome agentrun.RunOutcome) {
	run.mu.Lock()
	run.outcome = outcome
	run.outcomeSet = true
	run.mu.Unlock()
}

func (run *definitionManagedRun) validateRequest(
	request agentapi.RunRequest,
) (definitionExecution, error) {
	start := runStart(request)
	if start.RunID != run.start.RunID || start.Agent != run.start.Agent ||
		start.DefinitionHash != run.start.DefinitionHash ||
		start.Selection != run.start.Selection ||
		!jsonBytesEqual(start.Input, run.start.Input) || start.Actor != run.start.Actor ||
		start.Correlation != run.start.Correlation ||
		start.Policy.RedactSensitive != run.start.Policy.RedactSensitive ||
		!samePermissions(start.Permissions, run.start.Permissions) ||
		!sameExecutionToolScope(start.ToolScope, run.start.ToolScope) {
		return definitionExecution{}, fmt.Errorf("run request does not match the prepared run")
	}
	if err := validateDefinitionMessages(request.Messages); err != nil {
		return definitionExecution{}, err
	}
	contextHash, err := validateDefinitionContext(request.Context)
	if err != nil {
		return definitionExecution{}, err
	}
	execution := run.execution
	execution.snapshot.ContextHash = contextHash
	offeredTools, err := canonicalToolIDSet(request.ToolScope.OfferedToolIDs)
	if err != nil {
		return definitionExecution{}, fmt.Errorf("offered tools: %w", err)
	}
	available := make(map[tool.ToolID]struct{}, len(execution.snapshot.VisibleToolIDs))
	for _, id := range execution.snapshot.VisibleToolIDs {
		available[tool.ToolID(id)] = struct{}{}
	}
	for id := range offeredTools {
		if _, ok := available[id]; !ok {
			return definitionExecution{}, fmt.Errorf("offered tool %q is outside the run snapshot", id)
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
		Policy: request.Policy, Actor: request.Actor, Correlation: request.Correlation,
	}
}

func jsonBytesEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func sameExecutionToolScope(left, right agentapi.ToolScope) bool {
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
