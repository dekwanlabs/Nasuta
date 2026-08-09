package qa

import "testing"

type compactionStatusRecorder struct {
	runID string
	event SessionStatusEvent
}

func (recorder *compactionStatusRecorder) EmitPhase(string, string) {}

func (recorder *compactionStatusRecorder) EmitSessionStatus(runID string, event SessionStatusEvent) {
	recorder.runID = runID
	recorder.event = event
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
