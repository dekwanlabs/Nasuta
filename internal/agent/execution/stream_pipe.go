package execution

import (
	"context"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

// StreamPipe records provider timing and publishes only validated answer output.
type StreamPipe struct {
	observer   Observer
	runID      string
	stepNo     int
	discarding bool
	started    time.Time
	timingMu   sync.Mutex
	timing     StreamTiming
	// onFirstToken keeps the answering phase ordered after streamed reasoning.
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

func newStreamPipe(
	observer Observer,
	runID string,
	stepNo int,
	started time.Time,
	onFirstToken func(string),
) *StreamPipe {
	return &StreamPipe{
		observer: observer, runID: runID, stepNo: stepNo,
		started: started, onFirstToken: onFirstToken,
	}
}

func (pipe *StreamPipe) recordTiming(kind string) {
	if pipe.started.IsZero() {
		return
	}
	elapsed := time.Since(pipe.started)
	pipe.timingMu.Lock()
	defer pipe.timingMu.Unlock()
	if pipe.timing.FirstEvent == 0 {
		pipe.timing.FirstEvent = elapsed
	}
	switch kind {
	case "reasoning":
		if pipe.timing.FirstReasoning == 0 {
			pipe.timing.FirstReasoning = elapsed
		}
	case "content":
		if pipe.timing.FirstContent == 0 {
			pipe.timing.FirstContent = elapsed
		}
	case "tool_delta":
		if pipe.timing.FirstToolDelta == 0 {
			pipe.timing.FirstToolDelta = elapsed
		}
	case "tool_call":
		if pipe.timing.FirstToolCall == 0 {
			pipe.timing.FirstToolCall = elapsed
		}
	}
}

// Timings returns one immutable snapshot after a model turn completes.
func (pipe *StreamPipe) Timings() StreamTiming {
	pipe.timingMu.Lock()
	defer pipe.timingMu.Unlock()
	return pipe.timing
}

func (pipe *StreamPipe) OnToken(string) {
	pipe.recordTiming("content")
}

// Publish forwards a validated buffered answer as one visible token.
func (pipe *StreamPipe) Publish(content string) {
	if pipe.discarding || content == "" {
		return
	}
	if !pipe.firedFirst {
		pipe.firedFirst = true
		if pipe.onFirstToken != nil {
			pipe.onFirstToken(pipe.runID)
		}
	}
	if pipe.observer != nil {
		pipe.observer.OnToken(context.Background(), pipe.runID, content)
	}
}

// OnToolCallDelta prevents tool-call preamble from becoming visible answer text.
func (pipe *StreamPipe) OnToolCallDelta() {
	pipe.recordTiming("tool_delta")
	pipe.discarding = true
}

// Discard marks the turn as non-answer output.
func (pipe *StreamPipe) Discard() {
	pipe.discarding = true
}

// HasToolCallDelta reports whether this turn began a tool call instead of an answer.
func (pipe *StreamPipe) HasToolCallDelta() bool {
	return pipe.discarding
}

func (pipe *StreamPipe) OnReasoning(token string) {
	pipe.recordTiming("reasoning")
	if pipe.observer != nil {
		pipe.observer.OnReasoning(context.Background(), pipe.runID, token)
	}
}

func (pipe *StreamPipe) OnToolCall(llm.ToolCall) {
	pipe.recordTiming("tool_call")
}
