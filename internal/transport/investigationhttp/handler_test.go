package investigationhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/investigation"
)

func TestCreateRequiresAuthenticationAndStrictCanonicalInput(t *testing.T) {
	service := &recordingService{result: investigation.Result{
		RunID: "workflow_1", Answer: investigation.Answer{Answer: "grounded"},
	}}
	handler := New(service)

	unauthorized := httptest.NewRecorder()
	handler.Create(unauthorized, httptest.NewRequest(http.MethodPost, "/api/investigations", strings.NewReader(`{"question":"why"}`)))
	if unauthorized.Code != http.StatusUnauthorized || service.calls != 0 {
		t.Fatalf("unauthorized status=%d calls=%d", unauthorized.Code, service.calls)
	}

	for _, body := range []string{
		`{"question":"why","extra":true}`,
		`{"question":"why"} {"question":"again"}`,
		`{"question":"   "}`,
		`{"question":"` + strings.Repeat("界", maxQuestionLength+1) + `"}`,
	} {
		response := httptest.NewRecorder()
		request := authenticatedRequest(body, 7)
		handler.Create(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400", body[:min(len(body), 80)], response.Code)
		}
	}
	if service.calls != 0 {
		t.Fatalf("invalid requests reached service %d times", service.calls)
	}

	response := httptest.NewRecorder()
	handler.Create(response, authenticatedRequest(`{"question":"  why now?  "}`, 7))
	if response.Code != http.StatusOK || service.calls != 1 || service.question != "why now?" ||
		service.actor.UserID != 7 {
		t.Fatalf("success status=%d calls=%d question=%q actor=%+v", response.Code, service.calls, service.question, service.actor)
	}
	var envelope struct {
		Code int                  `json:"code"`
		Data investigation.Result `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != 0 || envelope.Data.RunID != "workflow_1" || envelope.Data.Answer.Answer != "grounded" {
		t.Fatalf("response = %+v", envelope)
	}
}

func TestCreateMapsUnavailableAndExecutionFailures(t *testing.T) {
	tests := []struct {
		name    string
		service Service
		status  int
	}{
		{name: "missing service", status: http.StatusServiceUnavailable},
		{
			name: "unavailable", service: &recordingService{err: errors.Join(investigation.ErrUnavailable, errors.New("LLM disabled"))},
			status: http.StatusServiceUnavailable,
		},
		{name: "execution failure", service: &recordingService{err: errors.New("model endpoint failed")}, status: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			New(test.service).Create(response, authenticatedRequest(`{"question":"why"}`, 9))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

type recordingService struct {
	calls    int
	question string
	actor    agentapi.Actor
	result   investigation.Result
	err      error
}

func (service *recordingService) Run(
	_ context.Context,
	question string,
	actor agentapi.Actor,
) (investigation.Result, error) {
	service.calls++
	service.question = question
	service.actor = actor
	return service.result, service.err
}

func authenticatedRequest(body string, userID int64) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/investigations", strings.NewReader(body))
	return request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: userID}))
}
