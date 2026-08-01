package agent

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

// StreamPipe records provider timing and publishes only validated answer output.
type StreamPipe struct {
	observer   Observer
	runID      string
	stepNo     int
	discarding bool // set true when tool-call delta received; stop forwarding tokens
	started    time.Time
	timingMu   sync.Mutex
	timing     StreamTiming
	// onFirstToken fires once, before the first non-discarded answer token is
	// forwarded. Lets the caller emit a phase hint ordered after reasoning.
	onFirstToken func(runID string)
	firedFirst   bool
}

// StreamTiming separates provider TTFT from reasoning, content, and tool events.
type StreamTiming struct {
	FirstEvent     time.Duration
	FirstReasoning time.Duration
	FirstContent   time.Duration
	FirstToolDelta time.Duration
	FirstToolCall  time.Duration
}

func newStreamPipe(observer Observer, runID string, stepNo int, started time.Time, onFirstToken func(string)) *StreamPipe {
	return &StreamPipe{observer: observer, runID: runID, stepNo: stepNo, started: started, onFirstToken: onFirstToken}
}

func (h *StreamPipe) recordTiming(kind string) {
	if h.started.IsZero() {
		return
	}
	elapsed := time.Since(h.started)
	h.timingMu.Lock()
	defer h.timingMu.Unlock()
	if h.timing.FirstEvent == 0 {
		h.timing.FirstEvent = elapsed
	}
	switch kind {
	case "reasoning":
		if h.timing.FirstReasoning == 0 {
			h.timing.FirstReasoning = elapsed
		}
	case "content":
		if h.timing.FirstContent == 0 {
			h.timing.FirstContent = elapsed
		}
	case "tool_delta":
		if h.timing.FirstToolDelta == 0 {
			h.timing.FirstToolDelta = elapsed
		}
	case "tool_call":
		if h.timing.FirstToolCall == 0 {
			h.timing.FirstToolCall = elapsed
		}
	}
}

// Timings returns one immutable snapshot after a model turn completes.
func (h *StreamPipe) Timings() StreamTiming {
	h.timingMu.Lock()
	defer h.timingMu.Unlock()
	return h.timing
}

func (h *StreamPipe) OnToken(token string) {
	h.recordTiming("content")
}

// Publish forwards a validated buffered answer as one visible token.
func (h *StreamPipe) Publish(content string) {
	if h.discarding || content == "" {
		return
	}
	if !h.firedFirst {
		h.firedFirst = true
		if h.onFirstToken != nil {
			h.onFirstToken(h.runID)
		}
	}
	if h.observer != nil {
		h.observer.OnToken(context.Background(), h.runID, content)
	}
}

// OnToolCallDelta marks this turn as a tool call.
// Subsequent OnToken calls are discarded — they're tool-call preamble
// that would duplicate what's already shown via OnReasoning.
func (h *StreamPipe) OnToolCallDelta() {
	h.recordTiming("tool_delta")
	h.discarding = true
}

// Discard marks the turn as non-answer output.
func (h *StreamPipe) Discard() { h.discarding = true }

// HasToolCallDelta reports whether this turn began a tool call instead of an answer.
func (h *StreamPipe) HasToolCallDelta() bool { return h.discarding }

// OnReasoning forwards streamed reasoning deltas to the observer.
func (h *StreamPipe) OnReasoning(token string) {
	h.recordTiming("reasoning")
	if h.observer != nil {
		h.observer.OnReasoning(context.Background(), h.runID, token)
	}
}

func (h *StreamPipe) OnToolCall(_ llm.ToolCall) { h.recordTiming("tool_call") }

type EventType string

const (
	EventAnswerDelta    EventType = "answer.delta"
	EventToolStarted    EventType = "tool.started"
	EventToolFinished   EventType = "tool.finished"
	EventStatus         EventType = "status"
	EventReasoningDelta EventType = "reasoning.delta"
	EventTrace          EventType = "trace"
	EventLLMCall        EventType = "llm.call"
	EventRunFinished    EventType = "run.finished"
)

type TextEvent struct {
	Text string `json:"text"`
}

type ToolStartedEvent struct {
	Step int    `json:"step"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type ToolFinishedEvent struct {
	Step          int    `json:"step"`
	Tool          string `json:"tool"`
	Summary       string `json:"summary"`
	TraceID       string `json:"trace_id,omitempty"`
	ArtifactID    string `json:"artifact_id,omitempty"`
	Failed        bool   `json:"failed"`
	DeliveryError string `json:"delivery_error,omitempty"`
	DurationMs    int    `json:"duration_ms"`
	SizeBytes     int64  `json:"size_bytes"`
}

// SSEEvent is the tagged event forwarded unchanged by the HTTP transport.
type SSEEvent struct {
	Type EventType `json:"type"`
	Data any       `json:"data"`
}

// RunTerminal is the sole real-time projection of one persisted Run outcome.
type RunTerminal struct {
	RunID           string          `json:"run_id"`
	Status          RunStatus       `json:"status"`
	Answer          string          `json:"answer,omitempty"`
	StepCount       int             `json:"step_count"`
	TokenUsed       int             `json:"token_used"`
	Error           string          `json:"error,omitempty"`
	Evidence        EvidenceMetrics `json:"evidence"`
	SessionMessages []llm.Message   `json:"-"`
}

func TerminalFromEvent(event SSEEvent) *RunTerminal {
	if event.Type != EventRunFinished {
		return nil
	}
	terminal, _ := event.Data.(*RunTerminal)
	return terminal
}

// EmitTrace broadcasts opt-in evaluation telemetry without persisting business steps.
func (hub *RunHub) EmitTrace(runID string, event domain.EvaluationTrace) {
	hub.broadcast(runID, SSEEvent{Type: EventTrace, Data: event})
}

// RunHub fans out live agent events to SSE subscribers and stores control signals.
type RunHub struct {
	mu        sync.Mutex
	subs      map[string][]*runSubscriber
	signals   map[string][]ControlSignal
	paused    map[string]chan struct{}
	completed map[string]struct{}
	stepErrs  map[string]error
	runStore  *RunStore
}

func NewRunHub(runStore *RunStore) *RunHub {
	return &RunHub{
		subs:      map[string][]*runSubscriber{},
		signals:   map[string][]ControlSignal{},
		paused:    map[string]chan struct{}{},
		completed: map[string]struct{}{},
		stepErrs:  map[string]error{},
		runStore:  runStore,
	}
}

func (hub *RunHub) Subscribe(runID string) chan SSEEvent {
	sub := newRunSubscriber(runID)
	hub.mu.Lock()
	hub.subs[runID] = append(hub.subs[runID], sub)
	hub.mu.Unlock()
	return sub.events
}

func (hub *RunHub) Unsubscribe(runID string, ch chan SSEEvent) {
	hub.mu.Lock()
	subs := hub.subs[runID]
	var removed *runSubscriber
	for i, sub := range subs {
		if sub.events == ch {
			removed = sub
			hub.subs[runID] = append(subs[:i], subs[i+1:]...)
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
	if hub.runStore != nil && runID != "" {
		persistErr = hub.runStore.AddStep(StepRow{
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
			Step: step.StepNo, Name: step.Tool, Args: step.Args,
		}})
	case StepKindToolResult:
		hub.broadcast(runID, SSEEvent{Type: EventToolFinished, Data: ToolFinishedEvent{
			Step: step.StepNo, Tool: step.Tool, Summary: toolResultPreview(step.Content),
			TraceID: step.TraceID, ArtifactID: step.ArtifactID, Failed: step.Failed,
			DeliveryError: step.DeliveryError, DurationMs: step.DurationMs, SizeBytes: step.SizeBytes,
		}})
	}
	return persistErr
}

func (hub *RunHub) OnToken(ctx context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventAnswerDelta, Data: TextEvent{Text: token}})
}

func (hub *RunHub) OnReasoning(ctx context.Context, runID, token string) {
	hub.broadcast(runID, SSEEvent{Type: EventReasoningDelta, Data: TextEvent{Text: token}})
}

// OnLLMCall publishes model request boundaries without persisting duplicate timing data.
func (hub *RunHub) OnLLMCall(ctx context.Context, runID string, call llm.CallLifecycle) {
	hub.broadcast(runID, SSEEvent{Type: EventLLMCall, Data: call})
}

// EmitPhase broadcasts a lightweight pre-loop status hint to SSE subscribers.
// Unlike OnStep, it does NOT persist a step row — phase hints are transient UI
// status, not agent reasoning steps, so they must not pollute agent_steps.
func (hub *RunHub) EmitPhase(runID, text string) {
	hub.broadcast(runID, SSEEvent{Type: EventStatus, Data: TextEvent{Text: text}})
}

func (hub *RunHub) Complete(runID string, outcome RunOutcome) {
	if !outcome.Status.Terminal() {
		outcome.Status = RunStatusFailed
		outcome.Err = fmt.Errorf("agent: non-terminal outcome")
	}

	hub.mu.Lock()
	if _, done := hub.completed[runID]; done {
		hub.mu.Unlock()
		return
	}
	hub.completed[runID] = struct{}{}
	delete(hub.signals, runID)
	paused := hub.paused[runID]
	delete(hub.paused, runID)
	stepErr := hub.stepErrs[runID]
	delete(hub.stepErrs, runID)
	hub.mu.Unlock()
	if paused != nil {
		close(paused)
	}

	if stepErr != nil {
		outcome.Status = RunStatusFailed
		outcome.Err = fmt.Errorf("persist agent step: %w", stepErr)
	}
	if hub.runStore != nil {
		if err := hub.runStore.Complete(runID, outcome); err != nil {
			if errors.Is(err, ErrRunNotActive) {
				log.WarnfCtx(ctxWithRunID(runID), "[hub] terminal transition rejected: %v", err)
			} else {
				log.ErrorfCtx(ctxWithRunID(runID), "[hub] complete run error: %v", err)
			}
			outcome.Status = RunStatusFailed
			outcome.Err = fmt.Errorf("persist run outcome: %w", err)
		}
	}
	terminal := &RunTerminal{
		RunID: runID, Status: outcome.Status, Answer: outcome.Answer,
		StepCount: outcome.StepCount, TokenUsed: outcome.TokenUsed,
		Evidence:        outcome.Evidence,
		SessionMessages: append([]llm.Message(nil), outcome.SessionMessages...),
	}
	if outcome.Err != nil {
		terminal.Error = outcome.Err.Error()
	}
	hub.broadcast(runID, SSEEvent{Type: EventRunFinished, Data: terminal})
	hub.mu.Lock()
	if len(hub.subs[runID]) == 0 {
		delete(hub.completed, runID)
	}
	hub.mu.Unlock()
}

func (hub *RunHub) broadcast(runID string, ev SSEEvent) {
	hub.mu.Lock()
	subs := append([]*runSubscriber(nil), hub.subs[runID]...)
	hub.mu.Unlock()
	for _, sub := range subs {
		if !sub.enqueue(ev) {
			log.WarnfCtx(ctxWithRunID(runID), "[hub] event %s dropped for run %s: subscriber buffer full", ev.Type, runID)
		}
	}
}

const subscriberDiagnosticLimit = 512

type runSubscriber struct {
	runID  string
	events chan SSEEvent
	wake   chan struct{}
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	queue  []SSEEvent
}

func newRunSubscriber(runID string) *runSubscriber {
	sub := &runSubscriber{
		runID: runID, events: make(chan SSEEvent, 512),
		wake: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	go sub.deliver()
	return sub
}

func (sub *runSubscriber) enqueue(event SSEEvent) bool {
	sub.mu.Lock()
	if isBestEffortEvent(event.Type) && len(sub.queue) >= subscriberDiagnosticLimit {
		sub.mu.Unlock()
		return false
	}
	sub.queue = append(sub.queue, event)
	sub.mu.Unlock()
	select {
	case sub.wake <- struct{}{}:
	default:
	}
	return true
}

func (sub *runSubscriber) deliver() {
	for {
		sub.mu.Lock()
		if len(sub.queue) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.wake:
				continue
			case <-sub.stop:
				return
			}
		}
		event := sub.queue[0]
		sub.queue[0] = SSEEvent{}
		sub.queue = sub.queue[1:]
		sub.mu.Unlock()
		select {
		case sub.events <- event:
			if event.Type == EventRunFinished {
				return
			}
		case <-sub.stop:
			return
		}
	}
}

func (sub *runSubscriber) close() {
	sub.once.Do(func() { close(sub.stop) })
}

func isBestEffortEvent(event EventType) bool {
	switch event {
	case EventReasoningDelta, EventTrace, EventStatus, EventLLMCall:
		return true
	default:
		return false
	}
}

func (hub *RunHub) Send(runID string, sig ControlSignal) {
	hub.mu.Lock()
	hub.signals[runID] = append(hub.signals[runID], sig)
	var paused chan struct{}
	if sig.Kind == CtrlAbort {
		paused = hub.paused[runID]
		delete(hub.paused, runID)
	}
	hub.mu.Unlock()
	if paused != nil {
		close(paused)
	}
}

func (hub *RunHub) Resume(runID string) error {
	hub.mu.Lock()
	ch, ok := hub.paused[runID]
	if !ok {
		// Resume before the loop consumes Pause cancels that queued request.
		queue := hub.signals[runID]
		out := queue[:0]
		for _, signal := range queue {
			if signal.Kind != CtrlPause {
				out = append(out, signal)
			}
		}
		hub.signals[runID] = out
		hub.mu.Unlock()
		return nil
	}
	hub.mu.Unlock()
	if hub.runStore != nil {
		if err := hub.runStore.TransitionControl(runID, RunStatusPaused, RunStatusRunning); err != nil {
			return fmt.Errorf("resume run: %w", err)
		}
	}
	hub.mu.Lock()
	if hub.paused[runID] == ch {
		delete(hub.paused, runID)
		hub.mu.Unlock()
		close(ch)
		return nil
	}
	hub.mu.Unlock()
	return nil
}

func (hub *RunHub) Poll(runID string) ControlSignal {
	hub.mu.Lock()
	q := hub.signals[runID]
	if len(q) == 0 {
		hub.mu.Unlock()
		return ControlSignal{Kind: CtrlNone}
	}
	sig := q[0]
	hub.signals[runID] = q[1:]
	if sig.Kind == CtrlPause {
		if hub.paused[runID] == nil {
			hub.paused[runID] = make(chan struct{})
		}
	}
	hub.mu.Unlock()
	if sig.Kind == CtrlPause && hub.runStore != nil {
		if err := hub.runStore.TransitionControl(runID, RunStatusRunning, RunStatusPaused); err != nil {
			log.WarnfCtx(ctxWithRunID(runID), "[hub] pause transition rejected: %v", err)
			hub.mu.Lock()
			paused := hub.paused[runID]
			delete(hub.paused, runID)
			hub.mu.Unlock()
			if paused != nil {
				close(paused)
			}
			return ControlSignal{Kind: CtrlAbort}
		}
	}
	return sig
}

func (hub *RunHub) WaitResume(ctx context.Context, runID string) error {
	hub.mu.Lock()
	ch := hub.paused[runID]
	hub.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Observer is the agent loop's window into live run observability.
type Observer interface {
	OnStep(ctx context.Context, runID string, step StepRecord) error
	OnToken(ctx context.Context, runID string, token string)
	OnReasoning(ctx context.Context, runID string, token string)
}

// Controller delivers out-of-band control signals to a running loop.
type Controller interface {
	Poll(runID string) ControlSignal
	WaitResume(ctx context.Context, runID string) error
}

type noopObserver struct{}

func (noopObserver) OnStep(context.Context, string, StepRecord) error { return nil }

func (noopObserver) OnToken(context.Context, string, string) {}

func (noopObserver) OnReasoning(context.Context, string, string) {}

func NoopObserver() Observer { return noopObserver{} }

// ctxWithRunID returns a context carrying the runID as a trace identifier.
// Use for non-request-scoped paths (hub internals) that only have a runID.
func ctxWithRunID(runID string) context.Context {
	return log.WithTraceID(context.Background(), runID)
}
