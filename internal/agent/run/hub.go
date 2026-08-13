package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

// RunHub owns live run events, controls, and the persistence projection.
type RunHub struct {
	mu        sync.Mutex
	subs      map[string][]*runSubscriber
	signals   map[string][]ControlSignal
	paused    map[string]chan struct{}
	completed map[string]struct{}
	stepErrs  map[string]error
	context   map[string]contextUsageState
	stepStore runStepStore
	completer runCompleter
	control   runControlStore
}

const (
	contextUsageRetention    = 30 * time.Minute
	maxContextUsageSnapshots = 1024
)

type contextUsageState struct {
	event     ContextUsageEvent
	expiresAt time.Time
}

func NewRunHub(runStore *RunStore) *RunHub {
	hub := &RunHub{
		subs:      map[string][]*runSubscriber{},
		signals:   map[string][]ControlSignal{},
		paused:    map[string]chan struct{}{},
		completed: map[string]struct{}{},
		stepErrs:  map[string]error{},
		context:   map[string]contextUsageState{},
	}
	if runStore != nil {
		hub.stepStore = runStore
		hub.completer = runStore
		hub.control = runStore
	}
	return hub
}

func (hub *RunHub) Subscribe(runID string) chan SSEEvent {
	sub := newRunSubscriber()
	hub.mu.Lock()
	hub.subs[runID] = append(hub.subs[runID], sub)
	hub.mu.Unlock()
	return sub.events
}

func (hub *RunHub) Unsubscribe(runID string, events chan SSEEvent) {
	hub.mu.Lock()
	subs := hub.subs[runID]
	var removed *runSubscriber
	for index, sub := range subs {
		if sub.events == events {
			removed = sub
			hub.subs[runID] = append(subs[:index], subs[index+1:]...)
			break
		}
	}
	if len(hub.subs[runID]) == 0 {
		delete(hub.subs, runID)
		delete(hub.completed, runID)
	}
	hub.mu.Unlock()
	if removed != nil {
		removed.close()
	}
}

func (hub *RunHub) OnStep(ctx context.Context, runID string, step StepRecord) error {
	if step.CreatedAt.IsZero() {
		step.CreatedAt = time.Now()
	}
	var persistErr error
	if hub.stepStore != nil && runID != "" {
		persistErr = hub.stepStore.AddStep(StepRow{
			RunID:               runID,
			StepNo:              step.StepNo,
			Kind:                step.Kind,
			TraceID:             step.TraceID,
			ArtifactID:          step.ArtifactID,
			ToolCallID:          step.ToolCallID,
			Tool:                step.Tool,
			Args:                step.Args,
			Failed:              step.Failed,
			DeliveryError:       step.DeliveryError,
			Content:             step.Content,
			PromptContent:       step.PromptContent,
			AuthoritativeSHA256: step.AuthoritativeSHA256,
			PromptSHA256:        step.PromptSHA256,
			SizeBytes:           step.SizeBytes,
			Coverage:            step.Coverage,
			AnswerContract:      step.AnswerContract,
			TokenDelta:          step.TokenDelta,
			ReasoningTokens:     step.ReasoningTokens,
			DurationMs:          step.DurationMs,
			CreatedAt:           step.CreatedAt.UTC().Format(time.RFC3339),
		})
		if persistErr != nil {
			log.ErrorfCtx(ctx, "[hub] persist step error: %v", persistErr)
			hub.mu.Lock()
			if _, exists := hub.stepErrs[runID]; !exists {
				hub.stepErrs[runID] = persistErr
			}
			hub.mu.Unlock()
		}
	}
	switch step.Kind {
	case StepKindToolCall:
		hub.broadcast(runID, SSEEvent{Type: EventToolStarted, Data: ToolStartedEvent{
			Step: step.StepNo, ToolCallID: step.ToolCallID, Name: step.Tool, Args: step.Args,
		}})
	case StepKindToolResult:
		hub.broadcast(runID, SSEEvent{Type: EventToolFinished, Data: ToolFinishedEvent{
			Step: step.StepNo, ToolCallID: step.ToolCallID,
			Tool: step.Tool, Summary: toolResultPreview(step.Content),
			TraceID: step.TraceID, ArtifactID: step.ArtifactID, Failed: step.Failed,
			DeliveryError: step.DeliveryError, DurationMs: step.DurationMs, SizeBytes: step.SizeBytes,
		}})
	}
	return persistErr
}

func (hub *RunHub) OnToken(_ context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventAnswerDelta, Data: TextEvent{Text: token}})
}

func (hub *RunHub) OnReasoning(_ context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventReasoningDelta, Data: TextEvent{Text: token}})
}

// OnLLMCall publishes model request boundaries without duplicating persistence.
func (hub *RunHub) OnLLMCall(_ context.Context, runID string, call llm.CallLifecycle) {
	hub.broadcast(runID, SSEEvent{Type: EventLLMCall, Data: call})
}

// OnContextUsage publishes and retains the largest projected context footprint
// observed during a run. The peak stays independent of provider input usage
// because compaction can reduce the payload before the provider sees it.
func (hub *RunHub) OnContextUsage(_ context.Context, runID string, event ContextUsageEvent) {
	hub.mu.Lock()
	now := time.Now()
	hub.cleanupContextUsageLocked(now)
	if previous, ok := hub.context[runID]; ok {
		event.PeakProjectedTokens = max(event.PeakProjectedTokens, previous.event.PeakProjectedTokens)
	}
	event.PeakProjectedTokens = max(
		event.PeakProjectedTokens,
		event.ProjectedBeforeTokens,
		event.ProjectedAfterTokens,
	)
	hub.context[runID] = contextUsageState{
		event:     event,
		expiresAt: now.Add(contextUsageRetention),
	}
	hub.trimContextUsageLocked()
	hub.mu.Unlock()
	hub.broadcast(runID, SSEEvent{Type: EventContextUsage, Data: event})
}

// ContextUsage returns the latest projected context snapshot for a run.
func (hub *RunHub) ContextUsage(runID string) (ContextUsageEvent, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.cleanupContextUsageLocked(time.Now())
	state, ok := hub.context[runID]
	if !ok {
		return ContextUsageEvent{}, false
	}
	return state.event, true
}

func (hub *RunHub) cleanupContextUsageLocked(now time.Time) {
	for runID, state := range hub.context {
		if !state.expiresAt.After(now) {
			delete(hub.context, runID)
		}
	}
}

func (hub *RunHub) trimContextUsageLocked() {
	for len(hub.context) > maxContextUsageSnapshots {
		oldestRunID := ""
		var oldest time.Time
		for runID, state := range hub.context {
			if oldestRunID == "" || state.expiresAt.Before(oldest) {
				oldestRunID = runID
				oldest = state.expiresAt
			}
		}
		if oldestRunID == "" {
			return
		}
		delete(hub.context, oldestRunID)
	}
}

// EmitPhase publishes transient UI status without creating a persisted step.
func (hub *RunHub) EmitPhase(runID, text string) {
	hub.EmitStatus(runID, text, "", 0)
}

// EmitStatus publishes a structured transient phase while preserving the
// legacy EmitPhase contract used by existing observers.
func (hub *RunHub) EmitStatus(runID, text, code string, elapsedMS int64) {
	hub.broadcast(runID, SSEEvent{Type: EventStatus, Data: TextEvent{
		Text: text, Code: code, ElapsedMS: elapsedMS,
	}})
}

func (hub *RunHub) EmitSessionStatus(runID string, event SessionStatusEvent) {
	hub.broadcast(runID, SSEEvent{Type: EventSessionStatus, Data: event})
}

func (hub *RunHub) EmitTrace(runID string, event domain.EvaluationTrace) {
	hub.broadcast(runID, SSEEvent{Type: EventTrace, Data: event})
}

func (hub *RunHub) EmitExecutionEvent(eventType EventType, event ExecutionEvent) {
	hub.broadcast(event.RunID, SSEEvent{Type: eventType, Data: event})
}

func (hub *RunHub) Complete(runID string, outcome RunOutcome) {
	hub.complete(runID, outcome, true)
}

// CompleteTransient publishes a terminal outcome when no Run row was created.
func (hub *RunHub) CompleteTransient(runID string, outcome RunOutcome) {
	hub.complete(runID, outcome, false)
}

// ProjectTerminal publishes a terminal outcome already committed by another owner.
func (hub *RunHub) ProjectTerminal(runID string, outcome RunOutcome) {
	hub.complete(runID, outcome, false)
}

func (hub *RunHub) complete(runID string, outcome RunOutcome, persist bool) {
	if !outcome.Status.Terminal() {
		outcome.Status = RunStatusFailed
		outcome.Err = fmt.Errorf("agent: non-terminal outcome")
	}

	stepErr, paused, accepted := hub.beginTerminal(runID)
	if !accepted {
		return
	}
	if paused != nil {
		close(paused)
	}

	if stepErr != nil {
		outcome.Status = RunStatusFailed
		outcome.Err = fmt.Errorf("persist agent step: %w", stepErr)
	}
	if persist && hub.completer != nil {
		if err := hub.completer.Complete(runID, outcome); err != nil {
			if errors.Is(err, ErrRunNotActive) {
				log.WarnfCtx(ctxWithRunID(runID), "[hub] terminal transition rejected: %v", err)
			} else {
				log.ErrorfCtx(ctxWithRunID(runID), "[hub] complete run error: %v", err)
			}
			outcome.Status = RunStatusFailed
			outcome.Err = fmt.Errorf("persist run outcome: %w", err)
		}
	}
	hub.projectTerminal(runID, outcome)
}

func (hub *RunHub) beginTerminal(runID string) (error, chan struct{}, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if _, done := hub.completed[runID]; done {
		return nil, nil, false
	}
	hub.completed[runID] = struct{}{}
	delete(hub.signals, runID)
	paused := hub.paused[runID]
	delete(hub.paused, runID)
	stepErr := hub.stepErrs[runID]
	delete(hub.stepErrs, runID)
	return stepErr, paused, true
}

func (hub *RunHub) projectTerminal(runID string, outcome RunOutcome) {
	terminal := terminalFromOutcome(runID, outcome)
	hub.broadcast(runID, SSEEvent{Type: EventRunFinished, Data: &terminal})
	hub.mu.Lock()
	if len(hub.subs[runID]) == 0 {
		delete(hub.completed, runID)
	}
	hub.mu.Unlock()
}

func (hub *RunHub) broadcast(runID string, event SSEEvent) {
	hub.mu.Lock()
	subs := append([]*runSubscriber(nil), hub.subs[runID]...)
	hub.mu.Unlock()
	for _, sub := range subs {
		if sub.enqueue(event) || !sub.reportDrop() {
			continue
		}
		log.WarnfCtx(
			ctxWithRunID(runID),
			"[hub] subscriber queue saturated for run %s; best-effort events will be coalesced or dropped",
			runID,
		)
	}
}
