package evaluationhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/evaluation"
)

type recordingService struct {
	traceUserID      int64
	traceAdmin       bool
	compareID        string
	baseVersion      int64
	candidateVersion int64
	from             time.Time
	to               time.Time
	labelInputs      []evaluation.ReviewLabelInput
	labelActor       int64
	labelAfterSeq    int64
	labelLimit       int
}

func (service *recordingService) WorkflowTrace(
	_ context.Context,
	_ string,
	userID int64,
	admin bool,
) (*evaluation.WorkflowTrace, error) {
	service.traceUserID = userID
	service.traceAdmin = admin
	return &evaluation.WorkflowTrace{
		Run: evaluation.WorkflowRunTrace{ID: "workflow-run-1"},
	}, nil
}

func (service *recordingService) CompareAgentVersions(
	_ context.Context,
	id string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	_ bool,
) (evaluation.Comparison[evaluation.AgentVersionMetrics], error) {
	service.recordComparison(id, baseVersion, candidateVersion, from, to)
	return evaluation.Comparison[evaluation.AgentVersionMetrics]{ID: id}, nil
}

func (service *recordingService) CompareWorkflowVersions(
	_ context.Context,
	id string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	_ bool,
) (evaluation.Comparison[evaluation.WorkflowVersionMetrics], error) {
	service.recordComparison(id, baseVersion, candidateVersion, from, to)
	return evaluation.Comparison[evaluation.WorkflowVersionMetrics]{ID: id}, nil
}

func (service *recordingService) CompareReviewPolicyVersions(
	_ context.Context,
	id string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	_ bool,
) (evaluation.Comparison[evaluation.ReviewPolicyVersionMetrics], error) {
	service.recordComparison(id, baseVersion, candidateVersion, from, to)
	return evaluation.Comparison[evaluation.ReviewPolicyVersionMetrics]{ID: id}, nil
}

func (service *recordingService) CreateReviewLabels(
	_ context.Context,
	_ string,
	inputs []evaluation.ReviewLabelInput,
	actorUserID int64,
	_ bool,
) ([]evaluation.ReviewLabel, error) {
	service.labelInputs = append([]evaluation.ReviewLabelInput(nil), inputs...)
	service.labelActor = actorUserID
	return []evaluation.ReviewLabel{{Seq: 4, Label: inputs[0].Label}}, nil
}

func (service *recordingService) ListReviewLabels(
	_ context.Context,
	_ string,
	afterSeq int64,
	limit int,
	_ bool,
) ([]evaluation.ReviewLabel, error) {
	service.labelAfterSeq = afterSeq
	service.labelLimit = limit
	return []evaluation.ReviewLabel{{Seq: 8}}, nil
}

func (service *recordingService) recordComparison(
	id string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
) {
	service.compareID = id
	service.baseVersion = baseVersion
	service.candidateVersion = candidateVersion
	service.from = from
	service.to = to
}

func TestWorkflowTraceRequiresAuthenticationAndUsesActorScope(t *testing.T) {
	service := &recordingService{}
	mux := evaluationMux(&Handler{service: service})
	response := serveEvaluationRequest(
		mux,
		http.MethodGet,
		"/api/evaluations/workflow-runs/workflow-run-1/trace",
		"",
		nil,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trace status = %d", response.Code)
	}
	response = serveEvaluationRequest(
		mux,
		http.MethodGet,
		"/api/evaluations/workflow-runs/workflow-run-1/trace",
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK ||
		service.traceUserID != 7 ||
		service.traceAdmin {
		t.Fatalf(
			"trace status=%d user=%d admin=%t body=%s",
			response.Code, service.traceUserID, service.traceAdmin,
			response.Body.String(),
		)
	}
}

func TestVersionComparisonRequiresAdministratorAndParsesWindow(t *testing.T) {
	service := &recordingService{}
	mux := evaluationMux(&Handler{service: service})
	target := "/api/evaluations/agents/qa.answerer/versions/compare" +
		"?base_version=1&candidate_version=2" +
		"&from=2026-08-01T00:00:00Z&to=2026-08-07T00:00:00Z"
	response := serveEvaluationRequest(
		mux, http.MethodGet, target, "", &auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin comparison status = %d", response.Code)
	}
	response = serveEvaluationRequest(
		mux, http.MethodGet, target, "", &auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		service.compareID != "qa.answerer" ||
		service.baseVersion != 1 ||
		service.candidateVersion != 2 ||
		service.from.Format(time.RFC3339) != "2026-08-01T00:00:00Z" ||
		service.to.Format(time.RFC3339) != "2026-08-07T00:00:00Z" {
		t.Fatalf("comparison status=%d service=%+v body=%s", response.Code, service, response.Body.String())
	}
}

func TestReviewLabelsAreAdministratorOnlyAndCanonicalizedAtIngress(t *testing.T) {
	service := &recordingService{}
	mux := evaluationMux(&Handler{service: service})
	body := `{"labels":[{"label":" FALSE_NEGATIVE ","target_hash":"` +
		strings.Repeat("A", 64) + `","category":" Security "}]}`
	target := "/api/evaluations/review-rounds/round-1/labels"
	response := serveEvaluationRequest(
		mux, http.MethodPost, target, body, &auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin label status = %d", response.Code)
	}
	response = serveEvaluationRequest(
		mux, http.MethodPost, target, body,
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		service.labelActor != 8 ||
		len(service.labelInputs) != 1 ||
		service.labelInputs[0].Label != evaluation.LabelFalseNegative ||
		service.labelInputs[0].TargetHash != strings.Repeat("a", 64) ||
		service.labelInputs[0].Category != "security" {
		t.Fatalf("label status=%d service=%+v body=%s", response.Code, service, response.Body.String())
	}

	response = serveEvaluationRequest(
		mux, http.MethodGet, target+"?after_seq=3&limit=25", "",
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		service.labelAfterSeq != 3 ||
		service.labelLimit != 25 {
		t.Fatalf("label list status=%d service=%+v", response.Code, service)
	}
}

func evaluationMux(handler *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	return mux
}

func serveEvaluationRequest(
	handler http.Handler,
	method string,
	target string,
	body string,
	user *auth.User,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if user != nil {
		request = request.WithContext(auth.WithUser(request.Context(), user))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
