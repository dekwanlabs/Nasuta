package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

type sessionStoreStub struct {
	createErr   error
	deleteResp  bool
	deleteErr   error
	created     memory.SessionRecord
	deletedID   string
	deletedUser int64
}

func (s *sessionStoreStub) List(userID int64) ([]memory.SessionRecord, error) { return nil, nil }
func (s *sessionStoreStub) Create(rec memory.SessionRecord) error {
	s.created = rec
	return s.createErr
}
func (s *sessionStoreStub) Delete(id string, userID int64) (bool, error) {
	s.deletedID = id
	s.deletedUser = userID
	return s.deleteResp, s.deleteErr
}
func (s *sessionStoreStub) GetContextSnapshot(id string, userID int64, metadataLimit, dialogueLimit int) (*memory.SessionRecord, error) {
	return nil, nil
}
func (s *sessionStoreStub) ListMessagesBefore(id string, userID int64, beforeSeq, limit int) (*memory.MessagePage, error) {
	return nil, nil
}
func (s *sessionStoreStub) ListTurnsBefore(id string, userID int64, beforeSeq, limit int) (*memory.MessagePage, error) {
	return nil, nil
}
func (s *sessionStoreStub) SetMessageFeedback(sessionID string, userID int64, seq int, runID, feedback string) (bool, error) {
	return false, nil
}

type memoryStoreStub struct {
	deleteBySessionErr error
}

func (s *memoryStoreStub) List(ctx context.Context, userID int64, options memory.ListOptions) (memory.MemoryPage, error) {
	return memory.MemoryPage{}, nil
}
func (s *memoryStoreStub) Delete(ctx context.Context, userID int64, id string) (bool, error) {
	return false, nil
}
func (s *memoryStoreStub) Clear(ctx context.Context, userID int64) (int, error) { return 0, nil }
func (s *memoryStoreStub) DeleteBySession(ctx context.Context, userID int64, sessionID string) (int, error) {
	return 0, s.deleteBySessionErr
}

type runStoreStub struct {
	deleteBySessionErr error
}

func (s *runStoreStub) ListPage(userID int64, sessionID string, status agentrun.Status, page, pageSize int) (*domain.Page[agentrun.Record], error) {
	return nil, nil
}
func (s *runStoreStub) GetForUser(id string, userID int64) (*agentrun.Detail, error) { return nil, nil }
func (s *runStoreStub) GetToolArtifact(userID int64, sessionID, artifactID string, offset int64, limit int) (*agentrun.ToolResultArtifactChunk, error) {
	return nil, nil
}
func (s *runStoreStub) GetControlForUser(id string, userID int64) (agentrun.ControlRecord, error) {
	return agentrun.ControlRecord{}, nil
}
func (s *runStoreStub) UsageSummary(ctx context.Context, userID int64, sessionID, runID string) (agentrun.UsageSummary, error) {
	return agentrun.UsageSummary{}, nil
}
func (s *runStoreStub) EvidenceByIDs(userID int64, sessionID string, runIDs []string) (map[string]agentrun.EvidenceMetrics, error) {
	return nil, nil
}
func (s *runStoreStub) DeleteBySession(id string, userID int64) error { return s.deleteBySessionErr }

func newSessionHandler(session QASessionStorePort, mem QAMemoryStorePort, runs QARunStorePort) *Handler {
	return &Handler{qaPortsFn: func() QAApplicationPorts {
		return QAApplicationPorts{SessionStore: session, MemoryStore: mem, RunStore: runs}
	}}
}

func TestAPIQASessionSaveCreatesAndRejectsConflict(t *testing.T) {
	session := &sessionStoreStub{createErr: memory.ErrSessionExists}
	handler := newSessionHandler(session, nil, nil)

	body := bytes.NewBufferString(`{"id":"session-1","title":"hello","messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/api/qa/sessions", body)
	request = request.WithContext(auth.WithUser(context.Background(), &auth.User{ID: 42}))
	response := httptest.NewRecorder()

	handler.APIQASessionSave(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	if session.created.UserID != 42 || session.created.ID != "session-1" {
		t.Fatalf("created = %+v", session.created)
	}
}

func TestAPIQASessionSaveRejectsUnknownFields(t *testing.T) {
	session := &sessionStoreStub{}
	handler := newSessionHandler(session, nil, nil)

	body := bytes.NewBufferString(`{"id":"session-1","created_at":"2020-01-01"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/qa/sessions", body)
	request = request.WithContext(auth.WithUser(context.Background(), &auth.User{ID: 42}))
	response := httptest.NewRecorder()

	handler.APIQASessionSave(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
	}
}

func TestAPIQASessionDeletePropagatesCleanupFailure(t *testing.T) {
	session := &sessionStoreStub{deleteResp: true}
	mem := &memoryStoreStub{deleteBySessionErr: errors.New("vector cleanup failed")}
	runs := &runStoreStub{}
	handler := newSessionHandler(session, mem, runs)

	request := httptest.NewRequest(http.MethodDelete, "/api/qa/sessions/session-1", nil)
	request.SetPathValue("id", "session-1")
	request = request.WithContext(auth.WithUser(context.Background(), &auth.User{ID: 42}))
	response := httptest.NewRecorder()

	handler.APIQASessionDelete(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 on cleanup failure", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"status":"deleted"`)) {
		t.Fatalf("must not report full success on cleanup failure: %s", response.Body.String())
	}
}

func TestAPIQASessionDeleteSucceeds(t *testing.T) {
	session := &sessionStoreStub{deleteResp: true}
	handler := newSessionHandler(session, &memoryStoreStub{}, &runStoreStub{})

	request := httptest.NewRequest(http.MethodDelete, "/api/qa/sessions/session-1", nil)
	request.SetPathValue("id", "session-1")
	request = request.WithContext(auth.WithUser(context.Background(), &auth.User{ID: 42}))
	response := httptest.NewRecorder()

	handler.APIQASessionDelete(response, request)

	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"status":"deleted"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
