package httputil

import (
	"errors"
	"net/http/httptest"
	"testing"
)

type recordingWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *recordingWriter) RecordHTTPError(err error) {
	w.err = err
}

func TestWriteErrStatusRecordsOriginalError(t *testing.T) {
	want := errors.New("database unavailable")
	w := &recordingWriter{ResponseRecorder: httptest.NewRecorder()}

	WriteErrStatus(w, 503, want)

	if !errors.Is(w.err, want) {
		t.Fatalf("recorded error = %v, want %v", w.err, want)
	}
	if w.Code != 503 {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
