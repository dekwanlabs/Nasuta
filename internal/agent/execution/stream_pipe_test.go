package execution

import (
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestStreamPipeRecordsModelEventTiming(t *testing.T) {
	started := time.Now().Add(-20 * time.Millisecond)
	pipe := newStreamPipe(nil, "run", 1, started, nil)
	pipe.OnReasoning("thinking")
	pipe.OnToken("preamble")
	pipe.OnToolCallDelta()
	pipe.OnToolCall(llm.ToolCall{})

	got := pipe.Timings()
	if got.FirstEvent <= 0 || got.FirstReasoning <= 0 || got.FirstContent <= 0 || got.FirstToolDelta <= 0 || got.FirstToolCall <= 0 {
		t.Fatalf("timing = %+v", got)
	}
	if got.FirstEvent != got.FirstReasoning {
		t.Fatalf("first event = %s, first reasoning = %s", got.FirstEvent, got.FirstReasoning)
	}
}
