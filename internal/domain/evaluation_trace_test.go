package domain

import (
	"context"
	"testing"
)

type traceRecorderStub struct{ events []EvaluationTrace }

func (recorder *traceRecorderStub) RecordTrace(event EvaluationTrace) {
	recorder.events = append(recorder.events, event)
}

func TestRecordTraceRequiresExplicitRecorder(t *testing.T) {
	recorder := &traceRecorderStub{}
	RecordTrace(context.Background(), EvaluationTrace{Node: "ignored"})
	ctx := WithTraceRecorder(context.Background(), recorder)
	if !TraceEnabled(ctx) || TraceEnabled(context.Background()) {
		t.Fatal("trace enablement did not follow the request recorder")
	}
	RecordTrace(ctx, EvaluationTrace{Node: "evidence_plan"})
	if len(recorder.events) != 1 || recorder.events[0].Status != "completed" {
		t.Fatalf("events = %#v", recorder.events)
	}
}
