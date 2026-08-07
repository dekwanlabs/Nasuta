package tracecontract

import (
	"encoding/json"
	"testing"
)

func TestEventV1JSONGolden(t *testing.T) {
	event := EventV1{
		TraceID: "trace-1", RunID: "agent-1", ParentRunID: "workflow-1",
		WorkflowRunID: "workflow-1", AgentRunID: "agent-1", WorkflowNodeID: "review.code",
		Sequence: 3, Node: "evidence_plan", Status: "completed",
		ElapsedMS: 12, DurationMS: 5,
		Input: map[string]any{"query": "auth"}, Output: map[string]any{"hits": 1},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"trace_id":"trace-1","run_id":"agent-1","parent_run_id":"workflow-1","workflow_run_id":"workflow-1","agent_run_id":"agent-1","workflow_node_id":"review.code","sequence":3,"node":"evidence_plan","status":"completed","elapsed_ms":12,"duration_ms":5,"input":{"query":"auth"},"output":{"hits":1}}`
	if got := string(encoded); got != want {
		t.Fatalf("EventV1 JSON = %s\nwant %s", got, want)
	}
}
