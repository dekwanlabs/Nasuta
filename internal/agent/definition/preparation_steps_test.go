package definition

import (
	"context"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDefinitionManagedRunRecordsPreparationStepsBeforeRuntimeSteps(t *testing.T) {
	const runID = "run-with-prefetch"
	runtime := &Runtime{hub: agentrun.NewHub(nil)}
	run := &activeRun{
		runtime: runtime,
		start:   agentapi.RunStart{RunID: runID},
	}
	events := runtime.hub.Subscribe(runID)

	steps := []agentrun.StepRecord{
		{
			Kind: agentrun.StepKindToolCall, ToolCallID: "call-prefetch-1",
			Tool: "observe", Args: `{"query":"checkout"}`, CreatedAt: time.Now(),
		},
		{
			Kind: agentrun.StepKindToolResult, ToolCallID: "call-prefetch-1",
			Tool: "observe", Content: "evidence",
			Coverage:  tool.EvidenceCoverage{Partial: true, OmittedItems: 2},
			CreatedAt: time.Now(),
		},
		{
			Kind: agentrun.StepKindToolCall, ToolCallID: "call-prefetch-2",
			Tool: "lookup", Args: `{}`, CreatedAt: time.Now(),
		},
		{
			Kind: agentrun.StepKindToolResult, ToolCallID: "call-prefetch-2",
			Tool: "lookup", Content: "error: unavailable", Failed: true,
			CreatedAt: time.Now(),
		},
	}
	for _, step := range steps {
		if err := run.RecordStep(t.Context(), step); err != nil {
			t.Fatalf("RecordStep: %v", err)
		}
	}

	for wantStep := 1; wantStep <= len(steps); wantStep++ {
		select {
		case event := <-events:
			switch payload := event.Data.(type) {
			case agentrun.ToolStartedEvent:
				if payload.Step != wantStep || payload.ToolCallID == "" {
					t.Fatalf("tool started event = %#v, want step %d", payload, wantStep)
				}
			case agentrun.ToolFinishedEvent:
				if payload.Step != wantStep || payload.ToolCallID == "" {
					t.Fatalf("tool finished event = %#v, want step %d", payload, wantStep)
				}
			default:
				t.Fatalf("event = %#v", event)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing preparation event for step %d", wantStep)
		}
	}

	if run.preparationStepCount != 4 ||
		run.preparationEvidence.ToolCallCount != 2 ||
		run.preparationEvidence.ToolFailureCount != 1 {
		t.Fatalf(
			"preparation steps=%d evidence=%+v",
			run.preparationStepCount,
			run.preparationEvidence,
		)
	}

	if err := run.observer().OnStep(context.Background(), runID, agentrun.StepRecord{
		StepNo: 1, Kind: agentrun.StepKindToolCall,
		ToolCallID: "call-runtime-1", Tool: "search",
	}); err != nil {
		t.Fatalf("runtime OnStep: %v", err)
	}
	select {
	case event := <-events:
		payload, ok := event.Data.(agentrun.ToolStartedEvent)
		if !ok || payload.Step != 5 || payload.ToolCallID != "call-runtime-1" {
			t.Fatalf("runtime event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing offset runtime event")
	}

	merged := run.mergePreparationOutcome(agentrun.Outcome{
		StepCount: 3,
		Evidence: agentrun.EvidenceMetrics{
			Status:        agentrun.EvidenceComplete,
			ToolCallCount: 1,
			ResultCount:   1,
		},
	})
	if merged.StepCount != 3 ||
		merged.Evidence.Status != agentrun.EvidencePartial ||
		merged.Evidence.ToolCallCount != 3 ||
		merged.Evidence.ResultCount != 1 ||
		merged.Evidence.ToolFailureCount != 1 ||
		merged.Evidence.PartialResultCount != 0 ||
		merged.Evidence.OmittedItemCount != 0 {
		t.Fatalf("merged outcome = %+v", merged)
	}
}
