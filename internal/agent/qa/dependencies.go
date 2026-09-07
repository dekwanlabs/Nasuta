package qa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

// EventSink is the single QA progress and execution-event projection
// contract. It merges the previous PhaseEmitter and ExecutionEventEmitter
// fields, which were always the same *run.Hub instance injected twice.
type EventSink interface {
	EmitPhase(string, string)
	EmitStatus(string, string, string, int64)
	EmitContextUsage(string, run.ContextUsageEvent)
	EmitSessionStatus(string, run.SessionStatusEvent)
	EmitEvent(run.EventType, run.ExecutionEvent)
}

type sessionTurnStore interface {
	EnsureSession(string, int64, string) error
	AppendTurn(string, string, int64, []llm.Message) (int, error)
}

type preparationStepRecorder interface {
	RecordStep(context.Context, run.StepRecord) error
}

type evidenceLedgerRecorder interface {
	RecordEvidence(context.Context, []tool.EvidenceUnit) error
}

func recordEvidenceLedger(
	ctx context.Context,
	owner any,
	units []tool.EvidenceUnit,
) error {
	recorder, ok := owner.(evidenceLedgerRecorder)
	if !ok || len(units) == 0 {
		return nil
	}
	return recorder.RecordEvidence(ctx, units)
}

func toolPolicyForRun(allowWrite bool) tool.Policy {
	return tool.Policy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

func beginExecutionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// RunStarter starts immutable managed runs after the scenario has resolved
// the definition, budget, and tool admission boundary. It is the execution
// half of the former RuntimePort.
type RunStarter interface {
	agentapi.ManagedRuntime
}

// ScenarioToolSource is provided directly by the composition layer as the
// preparation-time tool snapshot boundary. It is no longer forced onto the
// same business port as the managed-run lifecycle.
type ScenarioToolSource interface {
	definition.ScenarioToolSource
}

// RuntimePort is retained as a compatibility alias for compositions that
// already bind both boundaries through one concrete runtime. New call sites
// should depend on RunStarter and ScenarioToolSource separately.
type RuntimePort interface {
	RunStarter
	ScenarioToolSource
}

// RunCompletion is the explicit terminal contract QA requires after Execute.
// It replaces the previous anonymous type assertion so a managed run's durable
// outcome is a named, documented boundary instead of an implementation guess.
type RunCompletion interface {
	Outcome() run.Outcome
}
