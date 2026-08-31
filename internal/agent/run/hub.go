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

// Hub owns live run events, controls, and the persistence projection.
type Hub struct {
	mu        sync.Mutex
	subs      map[string][]*subscriber
	signals   map[string][]ControlSignal
	paused    map[string]chan struct{}
	completed map[string]struct{}
	stepErrs  map[string]error
	context   map[string]contextUsageState
	toolViews map[string]toolEventProjection
	stepStore stepStore
	completer completer
	control   controlStore
}

const (
	contextUsageRetention    = 30 * time.Minute
	maxContextUsageSnapshots = 1024
)

type contextUsageState struct {
	event     ContextUsageEvent
	expiresAt time.Time
}

// toolEventProjection identifies the parent QA stream and Workflow node that
// own one child Agent run.
type toolEventProjection struct {
	parentRunID   string
	agentRunID    string
	workflowRunID string
	nodeID        string
}

func NewHub(runStore *Store) *Hub {
	hub := &Hub{
		subs:      map[string][]*subscriber{},
		signals:   map[string][]ControlSignal{},
		paused:    map[string]chan struct{}{},
		completed: map[string]struct{}{},
		stepErrs:  map[string]error{},
		context:   map[string]contextUsageState{},
		toolViews: map[string]toolEventProjection{},
	}
	if runStore != nil {
		hub.stepStore = runStore
		hub.completer = runStore
		hub.control = runStore
	}
	return hub
}

func (hub *Hub) Subscribe(runID string) chan SSEEvent {
	sub := newSubscriber()
	hub.mu.Lock()
	hub.subs[runID] = append(hub.subs[runID], sub)
	hub.mu.Unlock()
	return sub.events
}

func (hub *Hub) Unsubscribe(runID string, events chan SSEEvent) {
	hub.mu.Lock()
	subs := hub.subs[runID]
	var removed *subscriber
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

func (hub *Hub) OnStep(ctx context.Context, runID string, step StepRecord) error {
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
			DelegationAdoptions: cloneDelegationAdoptions(
				step.DelegationAdoptions,
			),
			TokenDelta:      step.TokenDelta,
			ReasoningTokens: step.ReasoningTokens,
			DurationMs:      step.DurationMs,
			CreatedAt:       step.CreatedAt.UTC().Format(time.RFC3339),
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
		event := ToolStartedEvent{
			Step: step.StepNo, ToolCallID: step.ToolCallID, Name: step.Tool, Args: step.Args,
			AgentRunID: runID,
		}
		hub.broadcast(runID, SSEEvent{Type: EventToolStarted, Data: event})
		hub.projectToolEvent(runID, EventToolStarted, event)
	case StepKindToolResult:
		event := ToolFinishedEvent{
			Step: step.StepNo, ToolCallID: step.ToolCallID,
			Tool: step.Tool, Summary: toolResultPreview(step.Content),
			TraceID: step.TraceID, ArtifactID: step.ArtifactID, Failed: step.Failed,
			DeliveryError: step.DeliveryError, DurationMs: step.DurationMs, SizeBytes: step.SizeBytes,
			AgentRunID: runID,
		}
		hub.broadcast(runID, SSEEvent{Type: EventToolFinished, Data: event})
		hub.projectToolEvent(runID, EventToolFinished, event)
	}
	return persistErr
}

// ProjectToolEvents mirrors one child Agent's tool lifecycle onto its parent
// QA Run and returns an idempotent function that removes the projection.
func (hub *Hub) ProjectToolEvents(
	childRunID string,
	parentRunID string,
	workflowRunID string,
	nodeID string,
) func() {
	if hub == nil || childRunID == "" || parentRunID == "" {
		return func() {}
	}
	projection := toolEventProjection{
		parentRunID: parentRunID, agentRunID: childRunID,
		workflowRunID: workflowRunID, nodeID: nodeID,
	}
	hub.mu.Lock()
	hub.toolViews[childRunID] = projection
	hub.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			hub.mu.Lock()
			if hub.toolViews[childRunID] == projection {
				delete(hub.toolViews, childRunID)
			}
			hub.mu.Unlock()
		})
	}
}

// projectToolEvent enriches a child tool event with Workflow ownership before
// broadcasting it to the subscribed parent QA Run.
func (hub *Hub) projectToolEvent(
	childRunID string,
	eventType EventType,
	data any,
) {
	hub.mu.Lock()
	projection, ok := hub.toolViews[childRunID]
	hub.mu.Unlock()
	if !ok {
		return
	}
	switch event := data.(type) {
	case ToolStartedEvent:
		event.AgentRunID = projection.agentRunID
		event.WorkflowRunID = projection.workflowRunID
		event.NodeID = projection.nodeID
		data = event
	case ToolFinishedEvent:
		event.AgentRunID = projection.agentRunID
		event.WorkflowRunID = projection.workflowRunID
		event.NodeID = projection.nodeID
		data = event
	default:
		return
	}
	hub.broadcast(projection.parentRunID, SSEEvent{Type: eventType, Data: data})
}

func (hub *Hub) OnToken(_ context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventAnswerDelta, Data: TextEvent{Text: token}})
}

func (hub *Hub) OnReasoning(_ context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventReasoningDelta, Data: TextEvent{Text: token}})
}

// OnLLMCall publishes model request boundaries without duplicating persistence.
func (hub *Hub) OnLLMCall(_ context.Context, runID string, call llm.CallLifecycle) {
	hub.broadcast(runID, SSEEvent{Type: EventLLMCall, Data: call})
}

// OnContextUsage publishes and retains the largest projected context footprint
// observed during a run. The peak stays independent of provider input usage
// because compaction can reduce the payload before the provider sees it.
func (hub *Hub) OnContextUsage(_ context.Context, runID string, event ContextUsageEvent) {
	hub.mu.Lock()
	now := time.Now()
	hub.cleanupUsageLocked(now)
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
	hub.trimUsageLocked()
	hub.mu.Unlock()
	hub.broadcast(runID, SSEEvent{Type: EventContextUsage, Data: event})
}

// ContextUsage returns the latest projected context snapshot for a run.
func (hub *Hub) ContextUsage(runID string) (ContextUsageEvent, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.cleanupUsageLocked(time.Now())
	state, ok := hub.context[runID]
	if !ok {
		return ContextUsageEvent{}, false
	}
	return state.event, true
}

func (hub *Hub) cleanupUsageLocked(now time.Time) {
	for runID, state := range hub.context {
		if !state.expiresAt.After(now) {
			delete(hub.context, runID)
		}
	}
}

func (hub *Hub) trimUsageLocked() {
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
func (hub *Hub) EmitPhase(runID, text string) {
	hub.EmitStatus(runID, text, "", 0)
}

// EmitStatus publishes a structured transient phase while preserving the
// legacy EmitPhase contract used by existing observers.
func (hub *Hub) EmitStatus(runID, text, code string, elapsedMS int64) {
	hub.broadcast(runID, SSEEvent{Type: EventStatus, Data: TextEvent{
		Text: text, Code: code, ElapsedMS: elapsedMS,
	}})
}

func (hub *Hub) EmitSessionStatus(runID string, event SessionStatusEvent) {
	hub.broadcast(runID, SSEEvent{Type: EventSessionStatus, Data: event})
}

func (hub *Hub) EmitTrace(runID string, event domain.EvaluationTrace) {
	hub.broadcast(runID, SSEEvent{Type: EventTrace, Data: event})
}

func (hub *Hub) EmitEvent(eventType EventType, event ExecutionEvent) {
	hub.broadcast(event.RunID, SSEEvent{Type: eventType, Data: event})
}

// EmitToolStarted publishes a direct tool lifecycle event on the shared stream.
func (hub *Hub) EmitToolStarted(runID string, event ToolStartedEvent) {
	hub.broadcast(runID, SSEEvent{Type: EventToolStarted, Data: event})
	hub.projectToolEvent(runID, EventToolStarted, event)
}

// EmitToolFinished publishes a direct tool result event on the shared stream.
func (hub *Hub) EmitToolFinished(runID string, event ToolFinishedEvent) {
	hub.broadcast(runID, SSEEvent{Type: EventToolFinished, Data: event})
	hub.projectToolEvent(runID, EventToolFinished, event)
}

func (hub *Hub) Complete(runID string, outcome Outcome) {
	hub.complete(runID, outcome, true)
}

// CompleteTransient publishes a terminal outcome when no Run row was created.
func (hub *Hub) CompleteTransient(runID string, outcome Outcome) {
	hub.complete(runID, outcome, false)
}

// ProjectTerminal publishes a terminal outcome already committed by another owner.
func (hub *Hub) ProjectTerminal(runID string, outcome Outcome) {
	hub.complete(runID, outcome, false)
}

func (hub *Hub) complete(runID string, outcome Outcome, persist bool) {
	if !outcome.Status.Terminal() {
		outcome.Status = StatusFailed
		outcome.Err = fmt.Errorf("agent: non-terminal outcome")
	}

	paused, accepted, stepErr := hub.beginTerminal(runID)
	if !accepted {
		return
	}
	if paused != nil {
		close(paused)
	}

	if stepErr != nil {
		outcome.Status = StatusFailed
		outcome.Err = fmt.Errorf("persist agent step: %w", stepErr)
	}
	if persist && hub.completer != nil {
		if err := hub.completer.Complete(runID, outcome); err != nil {
			if errors.Is(err, ErrNotActive) {
				log.WarnfCtx(ctxWithRunID(runID), "[hub] terminal transition rejected: %v", err)
			} else {
				log.ErrorfCtx(ctxWithRunID(runID), "[hub] complete run error: %v", err)
			}
			outcome.Status = StatusFailed
			outcome.Err = fmt.Errorf("persist run outcome: %w", err)
		}
	}
	hub.projectTerminal(runID, outcome)
}

func (hub *Hub) beginTerminal(runID string) (chan struct{}, bool, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if _, done := hub.completed[runID]; done {
		return nil, false, nil
	}
	hub.completed[runID] = struct{}{}
	delete(hub.signals, runID)
	paused := hub.paused[runID]
	delete(hub.paused, runID)
	stepErr := hub.stepErrs[runID]
	delete(hub.stepErrs, runID)
	return paused, true, stepErr
}

func (hub *Hub) projectTerminal(runID string, outcome Outcome) {
	terminal := terminalFromOutcome(runID, outcome)
	hub.broadcast(runID, SSEEvent{Type: EventRunFinished, Data: &terminal})
	hub.mu.Lock()
	if len(hub.subs[runID]) == 0 {
		delete(hub.completed, runID)
	}
	hub.mu.Unlock()
}

func (hub *Hub) broadcast(runID string, event SSEEvent) {
	hub.mu.Lock()
	subs := append([]*subscriber(nil), hub.subs[runID]...)
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
