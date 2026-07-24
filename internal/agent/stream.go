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

// StreamPipe streams tokens immediately. If a tool-call delta fires mid-turn
// (OnToolCallDelta), it stops forwarding — the tokens already sent serve as
// the model's visible preamble/thinking for that tool call.
type StreamPipe struct {
	observer   Observer
	runID      string
	stepNo     int
	discarding bool // set true when tool-call delta received; stop forwarding tokens
	buffered   bool // hold answer tokens until the caller validates the complete response
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

func newBufferedStreamPipe(observer Observer, runID string, stepNo int, started time.Time, onFirstToken func(string)) *StreamPipe {
	h := newStreamPipe(observer, runID, stepNo, started, onFirstToken)
	h.buffered = true
	return h
}

func (h *StreamPipe) recordTiming(kind string) {
	if h == nil || h.started.IsZero() {
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
	if h == nil {
		return StreamTiming{}
	}
	h.timingMu.Lock()
	defer h.timingMu.Unlock()
	return h.timing
}

func (h *StreamPipe) OnToken(token string) {
	h.recordTiming("content")
	if h.discarding || h.buffered {
		return
	}
	// The first visible token means the model has started the answer.
	// Emit the "writing the answer" hint here so it lands after reasoning.
	// Tool-call turns may leak a short preamble before discarding kicks in.
	if !h.firedFirst {
		h.firedFirst = true
		if h.onFirstToken != nil {
			h.onFirstToken(h.runID)
		}
	}
	if h.observer != nil {
		h.observer.OnToken(context.Background(), h.runID, token)
	}
}

// Publish forwards a validated buffered answer as one visible token.
func (h *StreamPipe) Publish(content string) {
	if h == nil || h.discarding || content == "" {
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

// Flush is a no-op — tokens already streamed live.
func (h *StreamPipe) Flush() {}

// Discard stops forwarding — tokens already streamed are fine as preamble.
func (h *StreamPipe) Discard() { h.discarding = true }

// OnReasoning forwards streamed reasoning deltas to the observer.
func (h *StreamPipe) OnReasoning(token string) {
	h.recordTiming("reasoning")
	if h.observer != nil {
		h.observer.OnReasoning(context.Background(), h.runID, token)
	}
}

func (h *StreamPipe) OnToolCall(_ llm.ToolCall) { h.recordTiming("tool_call") }

// SSEEvent is one event pushed to a connected SSE client during a live run.
type SSEEvent struct {
	Step      *StepRecord             `json:"step,omitempty"`
	Token     string                  `json:"token,omitempty"`
	Reasoning string                  `json:"reasoning,omitempty"`
	Trace     *domain.EvaluationTrace `json:"trace,omitempty"`
	// Phase is a lightweight pre-loop status hint.
	// Unlike Step, it is not persisted; unlike Reasoning, it is not model output.
	// It covers preprocessing and retrieval before LLM streaming starts.
	Phase    string       `json:"phase,omitempty"`
	Terminal *RunTerminal `json:"terminal,omitempty"`
}

// RunTerminal is the sole real-time projection of one persisted Run outcome.
type RunTerminal struct {
	Status          RunStatus     `json:"status"`
	StepCount       int           `json:"step_count"`
	TokenUsed       int           `json:"token_used"`
	Error           string        `json:"error,omitempty"`
	SessionMessages []llm.Message `json:"-"`
}

// EmitTrace broadcasts opt-in evaluation telemetry without persisting business steps.
func (hub *RunHub) EmitTrace(runID string, event domain.EvaluationTrace) {
	hub.broadcastTrace(ctxWithRunID(runID), runID, SSEEvent{Trace: &event})
}

// RunHub fans out live agent events to SSE subscribers and stores control signals.
type RunHub struct {
	mu        sync.Mutex
	subs      map[string][]chan SSEEvent
	signals   map[string][]ControlSignal
	paused    map[string]chan struct{}
	completed map[string]struct{}
	stepErrs  map[string]error
	runStore  *RunStore
}

func NewRunHub(runStore *RunStore) *RunHub {
	return &RunHub{
		subs:      map[string][]chan SSEEvent{},
		signals:   map[string][]ControlSignal{},
		paused:    map[string]chan struct{}{},
		completed: map[string]struct{}{},
		stepErrs:  map[string]error{},
		runStore:  runStore,
	}
}

func (hub *RunHub) Subscribe(runID string) chan SSEEvent {
	ch := make(chan SSEEvent, 512)
	hub.mu.Lock()
	hub.subs[runID] = append(hub.subs[runID], ch)
	hub.mu.Unlock()
	return ch
}

func (hub *RunHub) Unsubscribe(runID string, ch chan SSEEvent) {
	hub.mu.Lock()
	subs := hub.subs[runID]
	for i, c := range subs {
		if c == ch {
			hub.subs[runID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(hub.subs[runID]) == 0 {
		delete(hub.subs, runID)
		delete(hub.completed, runID)
	}
	close(ch)
	hub.mu.Unlock()
}

func (hub *RunHub) OnStep(ctx context.Context, runID string, step StepRecord) {
	if hub.runStore != nil && runID != "" {
		if err := hub.runStore.AddStep(StepRow{
			RunID:           runID,
			StepNo:          step.StepNo,
			Kind:            step.Kind,
			Tool:            step.Tool,
			Args:            step.Args,
			ResultSummary:   step.ResultSummary,
			Content:         step.Content,
			TokenDelta:      step.TokenDelta,
			ReasoningTokens: step.ReasoningTokens,
			DurationMs:      step.DurationMs,
			CreatedAt:       step.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			log.ErrorfCtx(ctx, "[hub] persist step error: %v", err)
			hub.mu.Lock()
			if _, exists := hub.stepErrs[runID]; !exists {
				hub.stepErrs[runID] = err
			}
			hub.mu.Unlock()
		}
	}
	hub.broadcast(ctxWithRunID(runID), runID, SSEEvent{Step: &step})
}

func (hub *RunHub) OnToken(ctx context.Context, runID, token string) {
	hub.broadcastToken(ctx, runID, SSEEvent{Token: token})
}

func (hub *RunHub) OnReasoning(ctx context.Context, runID, token string) {
	hub.broadcastToken(ctx, runID, SSEEvent{Reasoning: token})
}

// EmitPhase broadcasts a lightweight pre-loop status hint to SSE subscribers.
// Unlike OnStep, it does NOT persist a step row — phase hints are transient UI
// status, not agent reasoning steps, so they must not pollute agent_steps.
func (hub *RunHub) EmitPhase(runID, text string) {
	hub.broadcast(ctxWithRunID(runID), runID, SSEEvent{Phase: text})
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
		Status: outcome.Status, StepCount: outcome.StepCount, TokenUsed: outcome.TokenUsed,
		SessionMessages: append([]llm.Message(nil), outcome.SessionMessages...),
	}
	if outcome.Err != nil {
		terminal.Error = outcome.Err.Error()
	}
	hub.broadcast(ctxWithRunID(runID), runID, SSEEvent{Terminal: terminal})
	hub.mu.Lock()
	if len(hub.subs[runID]) == 0 {
		delete(hub.completed, runID)
	}
	hub.mu.Unlock()
}

func (hub *RunHub) broadcast(_ context.Context, runID string, ev SSEEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, ch := range hub.subs[runID] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (hub *RunHub) broadcastToken(ctx context.Context, runID string, ev SSEEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, ch := range hub.subs[runID] {
		select {
		case ch <- ev:
		case <-time.After(tokenSendTimeout):
			log.WarnfCtx(ctx, "[hub] token dropped for run %s: subscriber buffer full after %s", runID, tokenSendTimeout)
		}
	}
}

func (hub *RunHub) broadcastTrace(ctx context.Context, runID string, ev SSEEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, ch := range hub.subs[runID] {
		select {
		case ch <- ev:
		case <-time.After(tokenSendTimeout):
			log.WarnfCtx(ctx, "[hub] evaluation trace blocked for run %s after %s", runID, tokenSendTimeout)
		}
	}
}

const tokenSendTimeout = 5 * time.Second

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
	OnStep(ctx context.Context, runID string, step StepRecord)
	OnToken(ctx context.Context, runID string, token string)
	OnReasoning(ctx context.Context, runID string, token string)
}

// Controller delivers out-of-band control signals to a running loop.
type Controller interface {
	Poll(runID string) ControlSignal
	WaitResume(ctx context.Context, runID string) error
}

type noopObserver struct{}

func (noopObserver) OnStep(context.Context, string, StepRecord) {}

func (noopObserver) OnToken(context.Context, string, string) {}

func (noopObserver) OnReasoning(context.Context, string, string) {}

func NoopObserver() Observer { return noopObserver{} }

// ctxWithRunID returns a context carrying the runID as a trace identifier.
// Use for non-request-scoped paths (hub internals) that only have a runID.
func ctxWithRunID(runID string) context.Context {
	return log.WithTraceID(context.Background(), runID)
}
