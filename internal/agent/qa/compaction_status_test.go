package qa

import "testing"

type compactionStatusRecorder struct {
	runID        string
	event        SessionStatusEvent
	contextRunID string
	contextEvent ContextUsageEvent
}

func (recorder *compactionStatusRecorder) EmitPhase(string, string) {}

func (recorder *compactionStatusRecorder) EmitSessionStatus(runID string, event SessionStatusEvent) {
	recorder.runID = runID
	recorder.event = event
}

func (recorder *compactionStatusRecorder) EmitContextUsage(runID string, event ContextUsageEvent) {
	recorder.contextRunID = runID
	recorder.contextEvent = event
}

func TestUpdateSessionCompactionStoresAndPublishesLatestStatus(t *testing.T) {
	recorder := &compactionStatusRecorder{}
	svc := &QA{
		phaseEmitter:     recorder,
		compactionStatus: make(map[string]SessionStatusEvent),
	}

	svc.updateSessionCompaction(
		"run-1", "session-1", "start",
		"正在压缩第 1–3 轮历史上下文…", 1, 3,
	)

	status := svc.SessionCompactionStatus("session-1")
	if status.Status != "start" || status.FromTurn != 1 || status.ToTurn != 3 ||
		status.UpdatedAtMs == 0 {
		t.Fatalf("stored status = %+v", status)
	}
	if recorder.runID != "run-1" || recorder.event != status {
		t.Fatalf("published run=%q event=%+v, want run-1 %+v", recorder.runID, recorder.event, status)
	}
}

func TestEmitContextUsagePublishesProjection(t *testing.T) {
	recorder := &compactionStatusRecorder{}
	svc := &QA{phaseEmitter: recorder}
	event := ContextUsageEvent{
		Phase:                 "session_pre_answer",
		ProjectedBeforeTokens: 82000,
		ProjectedAfterTokens:  61000,
		ContextWindow:         100000,
		HighWaterTokens:       80000,
		SafeLimitTokens:       95000,
	}

	svc.emitContextUsage("run-2", event)

	if recorder.contextRunID != "run-2" || recorder.contextEvent != event {
		t.Fatalf("published run=%q event=%+v", recorder.contextRunID, recorder.contextEvent)
	}
}
