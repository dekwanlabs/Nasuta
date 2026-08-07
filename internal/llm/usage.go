package llm

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/log"
)

const (
	PhaseRoute            = "route"
	PhaseAgentStep        = "agent_step"
	PhaseContinuation     = "continuation"
	PhaseForcedConclusion = "forced_conclusion"
	PhaseMemoryExtract    = "memory_extract"
	PhaseSessionSummary   = "session_summary"
)

const (
	CallStatusSucceeded = "succeeded"
	CallStatusFailed    = "failed"
)

const (
	CallLifecycleStarted  = "started"
	CallLifecycleFinished = "finished"
)

// Usage is the provider-reported accounting for one model call.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
	TotalTokens       int `json:"total_tokens"`
}

func (usage Usage) normalized() Usage {
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

// Add combines usage from continuation calls without double-counting details.
func (usage Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:       usage.InputTokens + other.InputTokens,
		CachedInputTokens: usage.CachedInputTokens + other.CachedInputTokens,
		OutputTokens:      usage.OutputTokens + other.OutputTokens,
		ReasoningTokens:   usage.ReasoningTokens + other.ReasoningTokens,
		TotalTokens:       usage.normalized().TotalTokens + other.normalized().TotalTokens,
	}
}

// CallUsage carries one physical provider call to an optional recorder.
type CallUsage struct {
	RunID           string
	Phase           string
	Provider        string
	Model           string
	MaxOutputTokens int
	Duration        time.Duration
	Status          string
	Usage           Usage
}

// UsageRecorder persists usage without coupling the LLM transport to Agent storage.
type UsageRecorder interface {
	RecordLLMCall(context.Context, CallUsage) error
}

// CallLifecycle describes one logical model request for live clients.
type CallLifecycle struct {
	CallSeq    int    `json:"call_seq"`
	Phase      string `json:"phase"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// CallLifecycleObserver receives request boundaries independently of usage persistence.
type CallLifecycleObserver interface {
	OnLLMCall(context.Context, string, CallLifecycle)
}

// ExecutionObserver receives one physical model execution outcome.
type ExecutionObserver interface {
	OnLLMExecution(context.Context, Execution)
}

// Execution describes one model request without prescribing an export protocol.
type Execution struct {
	CallSeq         int
	Phase           string
	Provider        string
	Model           string
	MaxOutputTokens int
	Duration        time.Duration
	Err             error
	Panic           any
}

type callLifecycleState struct {
	runID    string
	observer CallLifecycleObserver
	next     atomic.Int64
}

type usageContext struct {
	runID     string
	phase     string
	recorder  UsageRecorder
	lifecycle *callLifecycleState
	execution ExecutionObserver
}

type usageContextKey struct{}

// WithUsageRecorder associates subsequent calls with one QA Run.
func WithUsageRecorder(ctx context.Context, runID string, recorder UsageRecorder) context.Context {
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	metadata.runID = runID
	metadata.recorder = recorder
	return context.WithValue(ctx, usageContextKey{}, metadata)
}

// WithCallLifecycleObserver enables live timing for every logical model request.
func WithCallLifecycleObserver(ctx context.Context, runID string, observer CallLifecycleObserver) context.Context {
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	metadata.runID = runID
	metadata.lifecycle = &callLifecycleState{runID: runID, observer: observer}
	return context.WithValue(ctx, usageContextKey{}, metadata)
}

// WithExecutionObserver attaches one run-scoped model execution observer.
func WithExecutionObserver(ctx context.Context, observer ExecutionObserver) context.Context {
	if observer == nil {
		return ctx
	}
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	metadata.execution = observer
	if metadata.lifecycle == nil {
		metadata.lifecycle = &callLifecycleState{}
	}
	return context.WithValue(ctx, usageContextKey{}, metadata)
}

// WithUsagePhase identifies why the next model call is being made.
func WithUsagePhase(ctx context.Context, phase string) context.Context {
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	metadata.phase = phase
	return context.WithValue(ctx, usageContextKey{}, metadata)
}

func beginCallLifecycle(ctx context.Context, provider, model string, maxOutputTokens int) func(error, any) {
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	state := metadata.lifecycle
	execution := metadata.execution
	if (state == nil || state.observer == nil || state.runID == "") && execution == nil {
		return func(error, any) {}
	}
	phase := metadata.phase
	if phase == "" {
		phase = "unclassified"
	}
	callSeq := 1
	if state != nil {
		callSeq = int(state.next.Add(1))
	}
	started := time.Now()
	if state != nil && state.observer != nil && state.runID != "" {
		state.observer.OnLLMCall(ctx, state.runID, CallLifecycle{
			CallSeq: callSeq, Phase: phase, Status: CallLifecycleStarted,
		})
	}
	return func(callErr error, recovered any) {
		status := CallLifecycleFinished
		if callErr != nil || recovered != nil {
			status = CallStatusFailed
		}
		duration := time.Since(started)
		if state != nil && state.observer != nil && state.runID != "" {
			state.observer.OnLLMCall(context.WithoutCancel(ctx), state.runID, CallLifecycle{
				CallSeq: callSeq, Phase: phase, Status: status, DurationMs: duration.Milliseconds(),
			})
		}
		if execution != nil {
			func() {
				defer func() {
					if observerPanic := recover(); observerPanic != nil {
						log.ErrorfCtx(ctx, "[llm] execution observer call=%d phase=%s: %v", callSeq, phase, observerPanic)
					}
				}()
				execution.OnLLMExecution(context.WithoutCancel(ctx), Execution{
					CallSeq: callSeq, Phase: phase, Provider: provider, Model: model,
					MaxOutputTokens: maxOutputTokens, Duration: duration, Err: callErr, Panic: recovered,
				})
			}()
		}
	}
}

func recordCallUsage(ctx context.Context, provider, model string, maxOutputTokens int, started time.Time, usage Usage, callErr error) {
	recordCallUsageWithDuration(ctx, provider, model, maxOutputTokens, time.Since(started), usage, callErr)
}

func recordCallUsageWithDuration(ctx context.Context, provider, model string, maxOutputTokens int, duration time.Duration, usage Usage, callErr error) {
	metadata, _ := ctx.Value(usageContextKey{}).(usageContext)
	if metadata.recorder == nil || metadata.runID == "" {
		return
	}
	status := CallStatusSucceeded
	if callErr != nil {
		status = CallStatusFailed
	}
	phase := metadata.phase
	if phase == "" {
		phase = "unclassified"
	}
	call := CallUsage{
		RunID: metadata.runID, Phase: phase,
		Provider: provider, Model: model, MaxOutputTokens: maxOutputTokens,
		Duration: duration, Status: status, Usage: usage.normalized(),
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := metadata.recorder.RecordLLMCall(recordCtx, call); err != nil {
		log.ErrorfCtx(recordCtx, "[llm] record usage run=%s phase=%s: %v", metadata.runID, metadata.phase, err)
	}
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (usage openAIUsage) shared() Usage {
	return Usage{
		InputTokens: usage.PromptTokens, CachedInputTokens: usage.PromptDetails.CachedTokens,
		OutputTokens: usage.CompletionTokens, ReasoningTokens: usage.CompletionDetails.ReasoningTokens,
		TotalTokens: usage.TotalTokens,
	}.normalized()
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (usage anthropicUsage) shared() Usage {
	cachedInputTokens := usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	return Usage{
		InputTokens:       usage.InputTokens + cachedInputTokens,
		CachedInputTokens: cachedInputTokens,
		OutputTokens:      usage.OutputTokens,
	}.normalized()
}
