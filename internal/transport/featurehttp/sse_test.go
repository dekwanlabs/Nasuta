package featurehttp

import (
	"net/http/httptest"
	"testing"
)

func TestEventCursorUsesLastEventID(t *testing.T) {
	request := httptest.NewRequest("GET", "/events", nil)
	request.Header.Set("Last-Event-ID", "42")
	seq, err := eventCursor(request)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 {
		t.Fatalf("event cursor = %d", seq)
	}
}
