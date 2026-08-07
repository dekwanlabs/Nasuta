package executiontrace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestScopeBridgesLegacyTraceAndExportsStableEvents(t *testing.T) {
	var exported []domain.EvaluationTrace
	scope := NewScope(Evaluation, func(event domain.EvaluationTrace) {
		exported = append(exported, event)
	})
	ctx := WithScope(context.Background(), scope)
	if !scope.Enabled() || !domain.TraceEnabled(ctx) || FromContext(ctx) != scope {
		t.Fatal("enabled scope was not attached to the context")
	}
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "evidence_plan"})
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "vector_search", ElapsedMS: 9})
	events := scope.Snapshot()
	if len(events) != 2 || len(exported) != 2 {
		t.Fatalf("events = %#v, exported = %#v", events, exported)
	}
	if events[0].Sequence != 1 || events[0].Status != "completed" || events[0].ElapsedMS < 0 {
		t.Fatalf("first event = %#v", events[0])
	}
	if events[1].Sequence != 2 || events[1].ElapsedMS != 9 {
		t.Fatalf("second event = %#v", events[1])
	}
}

func TestBeginCreatesOnlyRequestedEvaluationScope(t *testing.T) {
	if Begin(context.Background()) != nil {
		t.Fatal("ordinary requests must not create a trace scope")
	}
	scope := Begin(WithEvaluation(context.Background(), nil))
	if scope == nil || !scope.Enabled() {
		t.Fatal("evaluation request did not create an enabled trace scope")
	}
}

func TestDisabledScopeDoesNotAttachTraceRecorder(t *testing.T) {
	ctx := WithScope(context.Background(), NewScope(Disabled, nil))
	if domain.TraceEnabled(ctx) || FromContext(ctx) != nil {
		t.Fatal("disabled scope must not enable tracing")
	}
}

func TestScopeExporterPanicDoesNotEscapeBusinessTrace(t *testing.T) {
	scope := NewScope(Evaluation, func(domain.EvaluationTrace) { panic("export failed") })
	scope.Record(domain.EvaluationTrace{Node: "safe_export"})
	events := scope.Snapshot()
	if len(events) != 1 || events[0].Node != "safe_export" {
		t.Fatalf("events = %#v", events)
	}
}

func TestScopeExporterCanReadCurrentSnapshot(t *testing.T) {
	var exported []domain.EvaluationTrace
	var scope *Scope
	scope = NewScope(Evaluation, func(domain.EvaluationTrace) {
		exported = scope.Snapshot()
	})
	done := make(chan struct{})
	go func() {
		scope.Record(domain.EvaluationTrace{Node: "snapshot_export"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("exporter deadlocked while reading the scope snapshot")
	}
	if len(exported) != 1 || exported[0].Node != "snapshot_export" {
		t.Fatalf("exported = %#v", exported)
	}
}

func TestScopeFreezesCallerExporterAndSnapshotPayloads(t *testing.T) {
	inputNested := map[string]any{"state": "recorded"}
	inputItems := []string{"first"}
	outputNested := map[string]any{"count": 1}
	var exported domain.EvaluationTrace
	scope := NewScope(Evaluation, func(event domain.EvaluationTrace) {
		exported = event
		event.Input["exporter"] = "mutated"
		event.Input["nested"].(map[string]any)["state"] = "exported"
		event.Output["nested"].(map[string]any)["count"] = 2
	})
	scope.Record(domain.EvaluationTrace{
		Node: "immutable",
		Input: map[string]any{
			"nested": inputNested,
			"items":  inputItems,
			"nil":    nil,
		},
		Output: map[string]any{"nested": outputNested},
	})

	inputNested["state"] = "caller"
	inputItems[0] = "caller"
	outputNested["count"] = 3
	first := scope.Snapshot()
	first[0].Input["snapshot"] = "mutated"
	first[0].Input["nested"].(map[string]any)["state"] = "snapshot"
	first[0].Input["items"].([]string)[0] = "snapshot"
	first[0].Output["nested"].(map[string]any)["count"] = 4

	second := scope.Snapshot()
	if got := second[0].Input["nested"].(map[string]any)["state"]; got != "recorded" {
		t.Fatalf("nested input = %v", got)
	}
	if got := second[0].Input["items"].([]string)[0]; got != "first" {
		t.Fatalf("input item = %v", got)
	}
	if got := second[0].Output["nested"].(map[string]any)["count"]; got != 1 {
		t.Fatalf("nested output = %v", got)
	}
	if _, ok := second[0].Input["exporter"]; ok {
		t.Fatal("exporter mutation reached stored event")
	}
	if _, ok := second[0].Input["snapshot"]; ok {
		t.Fatal("snapshot mutation reached stored event")
	}
	if value, ok := second[0].Input["nil"]; !ok || value != nil {
		t.Fatalf("nil input = %#v, present = %t", value, ok)
	}
	if exported.Input["nested"].(map[string]any)["state"] != "exported" {
		t.Fatal("exporter did not receive its own mutable copy")
	}
}

func TestScopeExportsConcurrentEventsInSequenceOrder(t *testing.T) {
	var exported []domain.EvaluationTrace
	scope := NewScope(Evaluation, func(event domain.EvaluationTrace) {
		exported = append(exported, event)
	})
	ctx := WithScope(t.Context(), scope)
	const count = 32
	var wait sync.WaitGroup
	wait.Add(count)
	for index := 0; index < count; index++ {
		go func() {
			defer wait.Done()
			domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "workflow_node"})
		}()
	}
	wait.Wait()
	if len(exported) != count {
		t.Fatalf("exported = %d, want %d", len(exported), count)
	}
	for index, event := range exported {
		if event.Sequence != index+1 {
			t.Fatalf("event %d sequence = %d", index, event.Sequence)
		}
	}
}

func TestScopeCorrelatesParentAndParallelChildRuns(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	root := WithScope(t.Context(), scope)
	parent := WithCorrelation(root, Correlation{
		RunID: "workflow-1", WorkflowRunID: "workflow-1",
	})
	domain.RecordTrace(parent, domain.EvaluationTrace{Node: "workflow_start"})

	children := []Correlation{
		{
			RunID: "agent-a", ParentRunID: "workflow-1", WorkflowRunID: "workflow-1",
			AgentRunID: "agent-a", WorkflowNodeID: "review.a",
		},
		{
			RunID: "agent-b", ParentRunID: "workflow-1", WorkflowRunID: "workflow-1",
			AgentRunID: "agent-b", WorkflowNodeID: "review.b",
		},
	}
	var wait sync.WaitGroup
	wait.Add(len(children))
	for _, child := range children {
		go func() {
			defer wait.Done()
			ctx := WithCorrelation(parent, child)
			domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "agent_model_turn"})
		}()
	}
	wait.Wait()

	events := scope.Snapshot()
	if len(events) != 3 || events[0].RunID != "workflow-1" || events[0].WorkflowRunID != "workflow-1" {
		t.Fatalf("events = %#v", events)
	}
	traceID := events[0].TraceID
	if traceID == "" {
		t.Fatal("trace id is empty")
	}
	seen := make(map[string]domain.EvaluationTrace, 2)
	for index, event := range events {
		if event.Sequence != index+1 || event.TraceID != traceID {
			t.Fatalf("event %d = %#v", index, event)
		}
		if event.AgentRunID != "" {
			seen[event.AgentRunID] = event
		}
	}
	for _, child := range children {
		event, ok := seen[child.AgentRunID]
		if !ok || event.RunID != child.RunID || event.ParentRunID != "workflow-1" ||
			event.WorkflowRunID != "workflow-1" || event.WorkflowNodeID != child.WorkflowNodeID {
			t.Fatalf("child %q event = %#v", child.AgentRunID, event)
		}
	}
}

func TestScopeRecordsOneTruncationEventAtEventLimit(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	for index := 0; index < maxEvents+10; index++ {
		scope.Record(domain.EvaluationTrace{Node: "bounded"})
	}
	events := scope.Snapshot()
	if len(events) != maxEvents {
		t.Fatalf("events = %d, want %d", len(events), maxEvents)
	}
	last := events[len(events)-1]
	if last.Sequence != maxEvents || last.Node != "execution_trace" || last.Status != "truncated" || last.Output["reason"] != "event_count" {
		t.Fatalf("last event = %#v", last)
	}
}

func TestScopeReplacesOversizedEventWithTruncation(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	scope.Record(domain.EvaluationTrace{
		Node: "oversized", Output: map[string]any{"content": string(make([]byte, maxEventBytes))},
	})
	events := scope.Snapshot()
	if len(events) != 1 || events[0].Node != "execution_trace" || events[0].Status != "truncated" ||
		events[0].Output["reason"] != "event_bytes" || events[0].Output["omitted_node"] != "oversized" {
		t.Fatalf("events = %#v", events)
	}
	scope.Record(domain.EvaluationTrace{Node: "ignored"})
	if len(scope.Snapshot()) != 1 {
		t.Fatal("scope accepted events after truncation")
	}
}

func TestScopeBoundsTruncationEventWithOversizedNode(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	scope.Record(domain.EvaluationTrace{
		Node: strings.Repeat("节", maxEventBytes), Output: map[string]any{"content": strings.Repeat("x", maxEventBytes)},
	})
	events := scope.Snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	encoded, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > truncationEventByteReserve {
		t.Fatalf("truncation event bytes = %d, reserve = %d", len(encoded), truncationEventByteReserve)
	}
}

func TestScopeRecordsTotalByteTruncationAndExportsSnapshotEvent(t *testing.T) {
	var exported []domain.EvaluationTrace
	scope := NewScope(Evaluation, func(event domain.EvaluationTrace) {
		exported = append(exported, event)
	})
	payload := strings.Repeat("x", 32<<10)
	for len(scope.Snapshot()) < maxEvents {
		scope.Record(domain.EvaluationTrace{Node: "bounded", Output: map[string]any{"content": payload}})
		events := scope.Snapshot()
		if events[len(events)-1].Status == "truncated" {
			break
		}
	}
	events := scope.Snapshot()
	last := events[len(events)-1]
	if last.Status != "truncated" || last.Output["reason"] != "total_bytes" {
		t.Fatalf("last event = %#v", last)
	}
	if len(exported) != len(events) || !reflect.DeepEqual(exported[len(exported)-1], last) {
		t.Fatalf("exported last = %#v, snapshot last = %#v", exported[len(exported)-1], last)
	}
}

func TestScopeConcurrentLimitHasUniqueSequencesAndOneTruncation(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	const attempts = maxEvents + 128
	var wait sync.WaitGroup
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func() {
			defer wait.Done()
			scope.Record(domain.EvaluationTrace{Node: "concurrent"})
		}()
	}
	wait.Wait()
	events := scope.Snapshot()
	if len(events) != maxEvents {
		t.Fatalf("events = %d, want %d", len(events), maxEvents)
	}
	truncated := 0
	for index, event := range events {
		if event.Sequence != index+1 {
			t.Fatalf("event %d sequence = %d", index, event.Sequence)
		}
		if event.Status == "truncated" {
			truncated++
		}
	}
	if truncated != 1 {
		t.Fatalf("truncation events = %d, want 1", truncated)
	}
}

func TestScopeCloseRejectsLateEvents(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	scope.Record(domain.EvaluationTrace{Node: "before_close"})
	scope.Close()
	scope.Close()
	if scope.Enabled() || domain.TraceEnabled(WithScope(t.Context(), scope)) {
		t.Fatal("closed scope still reports enabled")
	}
	scope.Record(domain.EvaluationTrace{Node: "after_close"})
	events := scope.Snapshot()
	if len(events) != 1 || events[0].Node != "before_close" {
		t.Fatalf("events = %#v", events)
	}
}

func TestScopeObservesSharedToolExecutor(t *testing.T) {
	tests := []struct {
		name       string
		handler    tool.HandlerFunc
		wantStatus string
		wantPanic  bool
	}{
		{
			name: "completed", wantStatus: "completed",
			handler: func(context.Context, tool.Arguments) (tool.Result, error) {
				return tool.Result{Content: "ok", References: []tool.Reference{{Target: "orders"}}}, nil
			},
		},
		{
			name: "cancelled", wantStatus: "cancelled",
			handler: func(context.Context, tool.Arguments) (tool.Result, error) {
				return tool.Result{}, context.Canceled
			},
		},
		{
			name: "panic", wantStatus: "failed", wantPanic: true,
			handler: func(context.Context, tool.Arguments) (tool.Result, error) {
				panic("tool panic")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			if err := registry.Register(tool.Tool{
				ID: "lookup", Description: "Looks up a service.", Kind: tool.KindRead,
				InputSchema: tool.JSONSchema{
					"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
				},
				Handler: test.handler,
			}); err != nil {
				t.Fatal(err)
			}
			var events []domain.EvaluationTrace
			ctx := WithScope(t.Context(), NewScope(Evaluation, func(event domain.EvaluationTrace) {
				events = append(events, event)
			}))
			invoke := func() {
				_, _ = tool.NewExecutor(0).Execute(ctx, registry.Snapshot(tool.ReadPolicy()), "lookup", tool.Arguments{"query": "orders"})
			}
			if test.wantPanic {
				func() {
					defer func() {
						if recovered := recover(); recovered != "tool panic" {
							t.Fatalf("recovered = %#v", recovered)
						}
					}()
					invoke()
				}()
			} else {
				invoke()
			}
			if len(events) != 1 || events[0].Node != "tool_execution" || events[0].Status != test.wantStatus || events[0].Input["tool"] != tool.ToolID("lookup") {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestScopeObservesPhysicalLLMCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	var events []domain.EvaluationTrace
	ctx := WithScope(t.Context(), NewScope(Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	ctx = llm.WithUsagePhase(ctx, llm.PhaseAgentStep)
	client := llm.NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, nil)
	if _, err := client.ChatWithToolsMax(ctx, []llm.Message{{Role: "user", Content: "q"}}, nil, nil, 40); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Node != "llm_call" || events[0].Status != "completed" ||
		events[0].Input["phase"] != llm.PhaseAgentStep || events[0].Output["call_seq"] != 1 ||
		events[0].Output["provider"] != "openai" || events[0].Output["model"] != "model" {
		t.Fatalf("events = %#v", events)
	}
}
