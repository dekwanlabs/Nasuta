package run

import (
	"testing"
	"time"
)

func TestHubBroadcastsSessionStatus(t *testing.T) {
	hub := NewRunHub(nil)
	channel := hub.Subscribe("run-1")
	want := SessionStatusEvent{
		Status: "start", Text: "compacting",
		FromTurn: 1, ToTurn: 3, UpdatedAtMs: 123,
	}

	hub.EmitSessionStatus("run-1", want)

	select {
	case event := <-channel:
		got, ok := event.Data.(SessionStatusEvent)
		if event.Type != EventSessionStatus || !ok || got != want {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("session status event was not broadcast")
	}
}
