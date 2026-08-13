package definition

import (
	"context"
	"fmt"

	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

// RecordPreparationStep attaches trusted scenario work to the Run created
// before execution starts. Runtime steps are offset by this count later.
func (run *definitionManagedRun) RecordPreparationStep(
	ctx context.Context,
	step agentrun.StepRecord,
) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.executed {
		return fmt.Errorf("definition run %q has already started execution", run.start.RunID)
	}
	if run.finished {
		return fmt.Errorf("definition run %q is already finished", run.start.RunID)
	}
	step.StepNo = run.preparationStepCount + 1
	if run.start.Policy.RedactSensitive {
		step = redactDefinitionStep(step)
	}
	if err := run.runtime.hub.OnStep(ctx, run.start.RunID, step); err != nil {
		return err
	}

	run.preparationStepCount = step.StepNo
	switch step.Kind {
	case agentrun.StepKindToolCall:
		run.preparationEvidence.ToolCallCount++
	case agentrun.StepKindToolResult:
		if step.Failed {
			run.preparationEvidence.ToolFailureCount++
		}
	}
	return nil
}

// RecordPreparationStep persists trusted work performed before child workflows start.
func (run *scenarioManagedRun) RecordPreparationStep(
	ctx context.Context,
	step agentrun.StepRecord,
) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	step.StepNo = run.preparationStepCount + 1
	if err := run.runtime.hub.OnStep(ctx, run.start.RunID, step); err != nil {
		return err
	}
	run.preparationStepCount = step.StepNo
	return nil
}

type stepOffsetObserver struct {
	next   agentrun.Observer
	offset int
}

func (observer stepOffsetObserver) OnStep(
	ctx context.Context,
	runID string,
	step agentrun.StepRecord,
) error {
	step.StepNo += observer.offset
	return observer.next.OnStep(ctx, runID, step)
}

func (observer stepOffsetObserver) OnToken(ctx context.Context, runID, token string) {
	observer.next.OnToken(ctx, runID, token)
}

func (observer stepOffsetObserver) OnReasoning(ctx context.Context, runID, token string) {
	observer.next.OnReasoning(ctx, runID, token)
}

func (observer stepOffsetObserver) OnContextUsage(
	ctx context.Context,
	runID string,
	event agentrun.ContextUsageEvent,
) {
	if next, ok := observer.next.(agentrun.ContextUsageObserver); ok {
		next.OnContextUsage(ctx, runID, event)
	}
}

func (observer stepOffsetObserver) EmitPhase(runID, text string) {
	emitter, ok := observer.next.(interface {
		EmitPhase(string, string)
	})
	if ok {
		emitter.EmitPhase(runID, text)
	}
}

func (run *definitionManagedRun) observer() agentrun.Observer {
	observer := agentrun.Observer(run.runtime.hub)
	run.mu.Lock()
	offset := run.preparationStepCount
	run.mu.Unlock()
	if offset > 0 {
		observer = stepOffsetObserver{next: observer, offset: offset}
	}
	if run.start.Policy.RedactSensitive {
		observer = redactingDefinitionObserver{next: observer}
	}
	return observer
}

func (run *definitionManagedRun) mergePreparationOutcome(
	outcome agentrun.RunOutcome,
) agentrun.RunOutcome {
	run.mu.Lock()
	defer run.mu.Unlock()
	return mergePreparationOutcome(
		outcome,
		run.preparationEvidence,
	)
}

func mergePreparationOutcome(
	outcome agentrun.RunOutcome,
	evidence agentrun.EvidenceMetrics,
) agentrun.RunOutcome {
	outcome.Evidence.ToolCallCount += evidence.ToolCallCount
	outcome.Evidence.ToolFailureCount += evidence.ToolFailureCount
	if evidence.ToolCallCount > 0 {
		outcome.Evidence.Finalize(false)
	}
	return outcome
}
