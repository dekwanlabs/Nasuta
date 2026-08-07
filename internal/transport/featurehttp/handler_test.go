package featurehttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/internal/featurepipeline"
)

type pipelineStarterRecorder struct {
	request featurepipeline.Request
	actor   agentapi.Actor
	calls   int
}

type reviewCoordinatorRecorder struct {
	executeResult *featuredelivery.ReviewGateResult
	executeErr    error
	cancelErr     error
	roundID       string
	actor         agentapi.Actor
	admin         bool
	executeCalls  int
	cancelCalls   int
	cancel        func(context.Context, string, agentapi.Actor, bool) error
}

func (coordinator *reviewCoordinatorRecorder) Execute(
	_ context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) (*featuredelivery.ReviewGateResult, error) {
	coordinator.executeCalls++
	coordinator.roundID = roundID
	coordinator.actor = actor
	coordinator.admin = admin
	return coordinator.executeResult, coordinator.executeErr
}

func (coordinator *reviewCoordinatorRecorder) Cancel(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) error {
	coordinator.cancelCalls++
	coordinator.roundID = roundID
	coordinator.actor = actor
	coordinator.admin = admin
	if coordinator.cancel != nil {
		return coordinator.cancel(ctx, roundID, actor, admin)
	}
	return coordinator.cancelErr
}

func (starter *pipelineStarterRecorder) Start(
	_ context.Context,
	request featurepipeline.Request,
	actor agentapi.Actor,
) (*agentworkflow.WorkflowRunRecord, error) {
	starter.calls++
	starter.request = request
	starter.actor = actor
	return &agentworkflow.WorkflowRunRecord{
		ID:              "workflow_1",
		WorkflowID:      featurepipeline.WorkflowID,
		WorkflowVersion: featurepipeline.WorkflowVersion,
		ActorUserID:     actor.UserID,
		Status:          agentworkflow.RunRunning,
	}, nil
}

type downloadStore struct {
	featuredelivery.Store
	feature featuredelivery.FeatureRequest
	run     featuredelivery.ImplementationRun
}

type generationAuditStore struct {
	featuredelivery.Store
	feature featuredelivery.FeatureRequest
	run     featuredelivery.GenerationRun
}

type reviewPolicyStore struct {
	featuredelivery.Store
	policy featuredelivery.ReviewPolicy
}

type reviewPolicyRolloutHTTPStore struct {
	featuredelivery.Store
	policy       featuredelivery.ReviewPolicyRecord
	rollout      featuredelivery.ReviewPolicyRolloutRule
	rolloutFound bool
	audit        []featuredelivery.ReviewPolicyRolloutAuditEvent
	auditKind    featuredelivery.SubjectKind
	auditAfter   int64
	auditLimit   int
	savedRollout featuredelivery.ReviewPolicyRolloutRule
	savedActor   int64
}

func (store *reviewPolicyRolloutHTTPStore) PublishReviewPolicies(
	context.Context,
	[]featuredelivery.ReviewPolicy,
	int64,
) error {
	return nil
}

func (store *reviewPolicyRolloutHTTPStore) ListReviewPolicyRecords(
	context.Context,
	featuredelivery.ReviewPolicyCursor,
	int,
) ([]featuredelivery.ReviewPolicyRecord, error) {
	return nil, nil
}

func (store *reviewPolicyRolloutHTTPStore) GetReviewPolicyRecord(
	_ context.Context,
	id string,
	version int64,
) (featuredelivery.ReviewPolicyRecord, error) {
	if id != store.policy.ID || version != store.policy.Version {
		return featuredelivery.ReviewPolicyRecord{}, featuredelivery.ErrNotFound
	}
	return store.policy, nil
}

func (store *reviewPolicyRolloutHTTPStore) GetDefaultReviewPolicy(
	context.Context,
	featuredelivery.SubjectKind,
) (featuredelivery.ReviewPolicyRef, error) {
	return featuredelivery.ReviewPolicyRef{}, featuredelivery.ErrNotFound
}

func (store *reviewPolicyRolloutHTTPStore) EnsureReviewPolicyDefault(
	context.Context,
	string,
	int64,
	int64,
) error {
	return nil
}

func (store *reviewPolicyRolloutHTTPStore) SetReviewPolicyDefault(
	context.Context,
	string,
	int64,
	int64,
) error {
	return nil
}

func (store *reviewPolicyRolloutHTTPStore) SetReviewPolicyActive(
	context.Context,
	string,
	int64,
	bool,
	int64,
) error {
	return nil
}

func (store *reviewPolicyRolloutHTTPStore) ListReviewPolicyAudit(
	context.Context,
	string,
	int64,
	int,
) ([]featuredelivery.ReviewPolicyAuditEvent, error) {
	return nil, nil
}

func (store *reviewPolicyRolloutHTTPStore) GetReviewPolicyRollout(
	_ context.Context,
	_ featuredelivery.SubjectKind,
) (featuredelivery.ReviewPolicyRolloutRule, bool, error) {
	return store.rollout, store.rolloutFound, nil
}

func (store *reviewPolicyRolloutHTTPStore) SetReviewPolicyRollout(
	_ context.Context,
	rule featuredelivery.ReviewPolicyRolloutRule,
	actorUserID int64,
) error {
	store.savedRollout = rule
	store.savedActor = actorUserID
	store.rollout = rule
	store.rolloutFound = true
	return nil
}

func (store *reviewPolicyRolloutHTTPStore) ListReviewPolicyRolloutAudit(
	_ context.Context,
	kind featuredelivery.SubjectKind,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewPolicyRolloutAuditEvent, error) {
	store.auditKind = kind
	store.auditAfter = afterSeq
	store.auditLimit = limit
	return append([]featuredelivery.ReviewPolicyRolloutAuditEvent(nil), store.audit...), nil
}

type reviewRoundHTTPStore struct {
	featuredelivery.Store
	feature            featuredelivery.FeatureRequest
	artifact           featuredelivery.Artifact
	artifacts          map[string]featuredelivery.Artifact
	run                featuredelivery.ImplementationRun
	policy             featuredelivery.ReviewPolicy
	round              featuredelivery.ReviewRound
	assignments        []featuredelivery.ReviewAssignment
	report             featuredelivery.ReviewReport
	events             []featuredelivery.ReviewEvent
	afterSeq           int64
	eventLimit         int
	adjudications      []featuredelivery.ReviewAdjudication
	adjudicationCursor featuredelivery.ReviewAdjudicationCursor
	adjudicationLimit  int
	finding            featuredelivery.FindingDetail
	resolutions        []featuredelivery.FindingResolution
	resolution         featuredelivery.FindingResolution
	resolutionSubject  string
	resolutionCursor   featuredelivery.FindingResolutionCursor
	resolutionLimit    int
	cancelCalls        int
	reuseSources       []featuredelivery.ReviewReportReuseSource
	reusedReports      []featuredelivery.ReviewReport
	reportReuses       []featuredelivery.ReviewReportReuse
	roundSummaries     []featuredelivery.ReviewRoundSummary
	roundFilter        featuredelivery.ReviewRoundFilter
	roundCursor        featuredelivery.ReviewRoundCursor
	roundLimit         int
	roundUserID        int64
	roundAdmin         bool
	roundHasMore       bool
}

func (store *reviewPolicyStore) SaveReviewPolicies(
	_ context.Context,
	policies []featuredelivery.ReviewPolicy,
) error {
	if len(policies) > 0 {
		store.policy = policies[len(policies)-1]
	}
	return nil
}

func (store *reviewPolicyStore) GetReviewPolicy(
	_ context.Context,
	id string,
	version int64,
) (*featuredelivery.ReviewPolicy, error) {
	if id != store.policy.ID || version != store.policy.Version {
		return nil, featuredelivery.ErrNotFound
	}
	policy := store.policy
	return &policy, nil
}

func (store *reviewRoundHTTPStore) GetFeature(
	_ context.Context,
	id string,
) (*featuredelivery.FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *reviewRoundHTTPStore) GetArtifact(
	_ context.Context,
	id string,
) (*featuredelivery.Artifact, error) {
	if artifact, ok := store.artifacts[id]; ok {
		copy := artifact
		return &copy, nil
	}
	if id != store.artifact.ID {
		return nil, featuredelivery.ErrNotFound
	}
	artifact := store.artifact
	return &artifact, nil
}

func (store *reviewRoundHTTPStore) GetImplementation(
	_ context.Context,
	id string,
) (*featuredelivery.ImplementationRun, error) {
	if id != store.run.ID {
		return nil, featuredelivery.ErrNotFound
	}
	run := store.run
	return &run, nil
}

func (store *reviewRoundHTTPStore) GetReviewRound(
	_ context.Context,
	id string,
) (*featuredelivery.ReviewRound, error) {
	if id != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	round := store.round
	return &round, nil
}

func (store *reviewRoundHTTPStore) ListReviewRoundSummaries(
	_ context.Context,
	filter featuredelivery.ReviewRoundFilter,
	cursor featuredelivery.ReviewRoundCursor,
	limit int,
	userID int64,
	admin bool,
) ([]featuredelivery.ReviewRoundSummary, bool, error) {
	store.roundFilter = filter
	store.roundCursor = cursor
	store.roundLimit = limit
	store.roundUserID = userID
	store.roundAdmin = admin
	return append([]featuredelivery.ReviewRoundSummary(nil), store.roundSummaries...),
		store.roundHasMore,
		nil
}

func (store *reviewRoundHTTPStore) GetReviewPolicy(
	_ context.Context,
	id string,
	version int64,
) (*featuredelivery.ReviewPolicy, error) {
	if id != store.policy.ID || version != store.policy.Version {
		return nil, featuredelivery.ErrNotFound
	}
	policy := store.policy
	return &policy, nil
}

func (store *reviewRoundHTTPStore) GetReviewFinding(
	_ context.Context,
	id string,
) (*featuredelivery.FindingDetail, error) {
	if id != store.finding.ID {
		return nil, featuredelivery.ErrNotFound
	}
	finding := store.finding
	finding.Evidence = append([]featuredelivery.FindingEvidence(nil), store.finding.Evidence...)
	return &finding, nil
}

func (store *reviewRoundHTTPStore) CreateReviewRound(
	_ context.Context,
	round featuredelivery.ReviewRound,
	assignments []featuredelivery.ReviewAssignment,
) error {
	store.round = round
	store.assignments = append([]featuredelivery.ReviewAssignment(nil), assignments...)
	return nil
}

func (store *reviewRoundHTTPStore) GetReviewReportReuseSources(
	_ context.Context,
	reportIDs []string,
) ([]featuredelivery.ReviewReportReuseSource, error) {
	requested := make(map[string]struct{}, len(reportIDs))
	for _, reportID := range reportIDs {
		requested[reportID] = struct{}{}
	}
	sources := make([]featuredelivery.ReviewReportReuseSource, 0, len(reportIDs))
	for _, source := range store.reuseSources {
		if _, ok := requested[source.Report.ID]; ok {
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (store *reviewRoundHTTPStore) CreateReviewRoundWithReuses(
	_ context.Context,
	round featuredelivery.ReviewRound,
	assignments []featuredelivery.ReviewAssignment,
	reports []featuredelivery.ReviewReport,
	reuses []featuredelivery.ReviewReportReuse,
) error {
	store.round = round
	store.assignments = append([]featuredelivery.ReviewAssignment(nil), assignments...)
	store.reusedReports = append([]featuredelivery.ReviewReport(nil), reports...)
	store.reportReuses = append([]featuredelivery.ReviewReportReuse(nil), reuses...)
	return nil
}

func (store *reviewRoundHTTPStore) ListReviewEvents(
	_ context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewEvent, error) {
	if roundID != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	store.afterSeq = afterSeq
	store.eventLimit = limit
	return append([]featuredelivery.ReviewEvent(nil), store.events...), nil
}

func (store *reviewRoundHTTPStore) GetReviewReportByAssignment(
	_ context.Context,
	roundID, assignmentID string,
) (*featuredelivery.ReviewReport, error) {
	if roundID != store.report.RoundID || assignmentID != store.report.AssignmentID {
		return nil, featuredelivery.ErrNotFound
	}
	report := store.report
	return &report, nil
}

func (store *reviewRoundHTTPStore) ListReviewAdjudications(
	_ context.Context,
	roundID string,
	cursor featuredelivery.ReviewAdjudicationCursor,
	limit int,
) ([]featuredelivery.ReviewAdjudication, error) {
	if roundID != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	store.adjudicationCursor = cursor
	store.adjudicationLimit = limit
	return append([]featuredelivery.ReviewAdjudication(nil), store.adjudications...), nil
}

func (store *reviewRoundHTTPStore) RequestReviewRoundCancel(
	_ context.Context,
	roundID string,
	at time.Time,
) (bool, error) {
	if roundID != store.round.ID {
		return false, featuredelivery.ErrNotFound
	}
	store.cancelCalls++
	switch store.round.Status {
	case featuredelivery.RoundCancelled:
		return false, nil
	case featuredelivery.RoundCreated, featuredelivery.RoundRunning, featuredelivery.RoundEvaluating:
		store.round.Status = featuredelivery.RoundCancelled
		store.round.CompletedAt = &at
		return true, nil
	default:
		return false, featuredelivery.ErrConflict
	}
}

func (store *reviewRoundHTTPStore) AppendReviewEvent(
	_ context.Context,
	event featuredelivery.ReviewEvent,
) (*featuredelivery.ReviewEvent, error) {
	if event.RoundID != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	event.Seq = int64(len(store.events) + 1)
	store.events = append(store.events, event)
	persisted := event
	return &persisted, nil
}

func (store *reviewRoundHTTPStore) CreateFindingResolution(
	_ context.Context,
	resolution featuredelivery.FindingResolution,
) error {
	if resolution.FindingID != store.finding.ID {
		return featuredelivery.ErrNotFound
	}
	store.resolution = resolution
	store.resolutions = append(store.resolutions, resolution)
	return nil
}

func (store *reviewRoundHTTPStore) ListFindingResolutions(
	_ context.Context,
	findingID, subjectHash string,
	cursor featuredelivery.FindingResolutionCursor,
	limit int,
) ([]featuredelivery.FindingResolution, error) {
	if findingID != store.finding.ID {
		return nil, featuredelivery.ErrNotFound
	}
	store.resolutionSubject = subjectHash
	store.resolutionCursor = cursor
	store.resolutionLimit = limit
	return append([]featuredelivery.FindingResolution(nil), store.resolutions...), nil
}

func (store *generationAuditStore) GetFeature(_ context.Context, id string) (*featuredelivery.FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *generationAuditStore) GetGenerationRun(_ context.Context, id string) (*featuredelivery.GenerationRun, error) {
	if id != store.run.ID {
		return nil, featuredelivery.ErrNotFound
	}
	run := store.run
	return &run, nil
}

func (store *generationAuditStore) ListGenerationRuns(_ context.Context, requestID string, _ featuredelivery.GenerationCursor, _ int) ([]featuredelivery.GenerationRun, error) {
	if requestID != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	return []featuredelivery.GenerationRun{store.run}, nil
}

func (store *downloadStore) GetFeature(_ context.Context, id string) (*featuredelivery.FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *downloadStore) GetImplementation(_ context.Context, id string) (*featuredelivery.ImplementationRun, error) {
	if id != store.run.ID {
		return nil, featuredelivery.ErrNotFound
	}
	run := store.run
	return &run, nil
}

func TestRegisterRoutesIncludesFeatureDeliverySurface(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, target := range []struct {
		method string
		path   string
	}{
		{"POST", "/api/features"},
		{"GET", "/api/features/feat-1"},
		{"POST", "/api/features/feat-1/pipeline"},
		{"POST", "/api/features/feat-1/artifacts/system_design/generate"},
		{"GET", "/api/features/feat-1/generations"},
		{"GET", "/api/feature-generations/gen-1"},
		{"POST", "/api/features/feat-1/implementations"},
		{"GET", "/api/feature-implementations/run-1/events"},
		{"GET", "/api/feature-implementations/run-1/patch"},
		{"GET", "/api/feature-implementations/run-1/validations/1/output"},
		{"POST", "/api/feature-delivery/review-policies"},
		{"GET", "/api/feature-delivery/review-policies/system-design-review/versions/1"},
		{"GET", "/api/feature-delivery/review-policy-rollouts/system_design_artifact"},
		{"POST", "/api/feature-delivery/review-policy-rollouts/system_design_artifact"},
		{"GET", "/api/feature-delivery/review-policy-rollouts/system_design_artifact/audit"},
		{"POST", "/api/feature-delivery/subjects/system_design_artifact/artifact-1/review-rounds"},
		{"GET", "/api/feature-delivery/review-rounds"},
		{"GET", "/api/feature-delivery/review-rounds/round-1"},
		{"POST", "/api/feature-delivery/review-rounds/round-1/execute"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/assignments"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/assignments/assignment-1/report"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/findings"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/adjudications"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/gate"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/events"},
		{"GET", "/api/feature-delivery/review-rounds/round-1/events/stream"},
		{"POST", "/api/feature-delivery/review-rounds/round-1/cancel"},
		{"GET", "/api/feature-delivery/findings/finding-1"},
		{"POST", "/api/feature-delivery/findings/finding-1/waivers"},
		{"GET", "/api/feature-delivery/findings/finding-1/resolutions"},
		{"POST", "/api/feature-delivery/findings/finding-1/resolutions"},
	} {
		request, err := http.NewRequest(target.method, target.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, pattern := mux.Handler(request)
		if pattern == "" {
			t.Fatalf("route not registered: %s %s", target.method, target.path)
		}
	}
}

func TestAdministrativeMutationsRejectRegularUsers(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	paths := []string{
		"/api/features/feat-1/pipeline",
		"/api/features/feat-1/artifacts/artifact-1/review",
		"/api/features/feat-1/implementations",
		"/api/feature-implementations/run-1/cancel",
		"/api/feature-implementations/run-1/review",
		"/api/feature-delivery/review-policies",
		"/api/feature-delivery/review-policy-rollouts/system_design_artifact",
		"/api/feature-delivery/review-rounds/round-1/execute",
		"/api/feature-delivery/review-rounds/round-1/cancel",
		"/api/feature-delivery/findings/finding-1/waivers",
		"/api/feature-delivery/findings/finding-1/resolutions",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("POST %s status=%d, want %d", path, response.Code, http.StatusForbidden)
		}
	}
}

func TestStartPipelineCanonicalizesFixedAdminRequest(t *testing.T) {
	starter := &pipelineStarterRecorder{}
	handler := New(nil)
	handler.SetPipelineStarter(starter)
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/features/feat-1/pipeline",
		strings.NewReader(`{
			"client_request_id":"  client-1  ",
			"repository":" team/repo/ ",
			"base_ref":" ",
			"provider":" OpenAI ",
			"model":" gpt-5 ",
			"network_enabled":true
		}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(),
		&auth.User{ID: 41, IsAdmin: true},
	))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("start pipeline status=%d body=%s", response.Code, response.Body.String())
	}
	if starter.calls != 1 ||
		starter.request.FeatureID != "feat-1" ||
		starter.request.ClientRequestID != "client-1" ||
		starter.request.Repository != "team/repo" ||
		starter.request.BaseRef != "HEAD" ||
		starter.request.Provider != "openai" ||
		starter.request.Model != "gpt-5" ||
		!starter.request.NetworkEnabled ||
		starter.actor.UserID != 41 {
		t.Fatalf(
			"pipeline calls=%d request=%+v actor=%+v",
			starter.calls,
			starter.request,
			starter.actor,
		)
	}
}

func TestStartPipelineRejectsClientControlledWorkflowFields(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "feature id",
			body: `{"feature_id":"other","client_request_id":"client-1","repository":"team/repo","provider":"openai"}`,
		},
		{
			name: "workflow id",
			body: `{"workflow_id":"custom","client_request_id":"client-1","repository":"team/repo","provider":"openai"}`,
		},
		{
			name: "node id",
			body: `{"node_id":"skip","client_request_id":"client-1","repository":"team/repo","provider":"openai"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter := &pipelineStarterRecorder{}
			handler := New(nil)
			handler.SetPipelineStarter(starter)
			mux := http.NewServeMux()
			handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
				mux.HandleFunc(pattern, route)
			})
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/features/feat-1/pipeline",
				strings.NewReader(test.body),
			)
			request = request.WithContext(auth.WithUser(
				request.Context(),
				&auth.User{ID: 41, IsAdmin: true},
			))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest || starter.calls != 0 {
				t.Fatalf(
					"status=%d calls=%d body=%s",
					response.Code,
					starter.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestReviewPolicyHTTPControlPlaneCanonicalizesAdminInput(t *testing.T) {
	store := &reviewPolicyStore{}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/review-policies",
		strings.NewReader(`{
			"id":" System-Design-Review ",
			"version":1,
			"subject_kind":" SYSTEM_DESIGN_ARTIFACT ",
			"reviewers":[
				{
					"id":" Architecture ",
					"agent":{"id":" REVIEW.Architecture ","version":1},
					"definition_hash":" HASH-A ",
					"categories":[" Architecture ","architecture"],
					"required":true,
					"read_only":true
				},
				{
					"id":" Security ",
					"agent":{"id":" REVIEW.Security ","version":2},
					"definition_hash":" HASH-B ",
					"categories":[" Security "],
					"required":true,
					"read_only":true
				},
				{
					"id":" Operations ",
					"agent":{"id":" REVIEW.Operations ","version":1},
					"definition_hash":" HASH-C ",
					"categories":[" Operations "],
					"required":false,
					"read_only":true
					}
				],
				"adjudicator":{
					"agent":{"id":" REVIEW.Adjudicator ","version":3},
					"definition_hash":" ADJUDICATOR-HASH ",
					"read_only":true
				},
				"blocking_severities":[" CRITICAL ","HIGH","high"],
				"required_categories":[" Security ","architecture"],
				"max_parallelism":3,
				"max_input_tokens":4,
				"max_output_tokens":4,
				"max_total_tokens":4,
				"max_tool_calls":4,
				"max_cost_micros":4,
				"max_retries":1,
				"timeout":60000000000,
				"risk_rule_version":" CHANGE-RISK.V1 ",
				"risk_rules":[{
					"id":" LARGE-CHANGE ",
					"conditions":[{
						"fact":" FILES_CHANGED ",
						"operator":" GTE ",
						"value":10
					}],
					"reviewer_ids":[" Operations ","operations"]
				}]
		}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 7, IsAdmin: true},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var publishResponse struct {
		Data featuredelivery.ReviewPolicy `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &publishResponse); err != nil {
		t.Fatal(err)
	}
	published := publishResponse.Data
	if published.ID != "system-design-review" ||
		published.SubjectKind != featuredelivery.SubjectSystemDesign ||
		published.Reviewers[0].ID != "architecture" ||
		published.Reviewers[0].Agent.ID != "review.architecture" ||
		published.Reviewers[0].DefinitionHash != "hash-a" ||
		len(published.Reviewers[0].Categories) != 1 ||
		published.Adjudicator == nil ||
		published.Adjudicator.Agent != (agentapi.DefinitionRef{
			ID: "review.adjudicator", Version: 3,
		}) ||
		published.Adjudicator.DefinitionHash != "adjudicator-hash" ||
		!published.Adjudicator.ReadOnly ||
		len(published.BlockingSeverities) != 2 ||
		published.RiskRuleVersion != "change-risk.v1" ||
		len(published.RiskRules) != 1 ||
		published.RiskRules[0].ID != "large-change" ||
		published.RiskRules[0].Conditions[0].Fact != featuredelivery.RiskFactFilesChanged ||
		published.RiskRules[0].Conditions[0].Operator != featuredelivery.RiskGreaterThanOrEqual ||
		len(published.RiskRules[0].ReviewerIDs) != 1 ||
		published.RiskRules[0].ReviewerIDs[0] != "operations" ||
		published.ContentHash == "" || published.CreatedAt.IsZero() {
		t.Fatalf("published = %+v", published)
	}

	getRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-policies/System-Design-Review/versions/1",
		nil,
	)
	regularRequest := getRequest.WithContext(auth.WithUser(
		getRequest.Context(), &auth.User{ID: 8},
	))
	regularResponse := httptest.NewRecorder()
	mux.ServeHTTP(regularResponse, regularRequest)
	if regularResponse.Code != http.StatusForbidden {
		t.Fatalf("regular GET status = %d, want %d", regularResponse.Code, http.StatusForbidden)
	}
	getRequest = getRequest.WithContext(auth.WithUser(
		getRequest.Context(), &auth.User{ID: 7, IsAdmin: true},
	))
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}
}

func TestPublishReviewPolicyRejectsServerOwnedFields(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/review-policies",
		strings.NewReader(`{"id":"policy","content_hash":"client-controlled"}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 7, IsAdmin: true},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestReviewPolicyRolloutGetRequiresAuthentication(t *testing.T) {
	store, service, mux := reviewPolicyRolloutHTTPFixture(t)
	_, err := service.SetReviewPolicyRollout(
		context.Background(), featuredelivery.SubjectSystemDesign,
		store.policy.ID, store.policy.Version, 2500, "rollout-2026-08",
		true, 7, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/feature-delivery/review-policy-rollouts/system_design_artifact"

	unauthenticated := httptest.NewRecorder()
	mux.ServeHTTP(
		unauthenticated,
		httptest.NewRequest(http.MethodGet, path, nil),
	)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 42},
	))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Found   bool                                    `json:"found"`
			Rollout featuredelivery.ReviewPolicyRolloutRule `json:"rollout"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Data.Found || payload.Data.Rollout.RuleVersion != 1 ||
		payload.Data.Rollout.RuleHash == "" ||
		payload.Data.Rollout.CandidatePolicyID != store.policy.ID {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestReviewPolicyRolloutAdministrativeReadsAndWritesRejectRegularUsers(t *testing.T) {
	_, _, mux := reviewPolicyRolloutHTTPFixture(t)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{
			method: http.MethodPost,
			path:   "/api/feature-delivery/review-policy-rollouts/system_design_artifact",
			body:   `{}`,
		},
		{
			method: http.MethodGet,
			path:   "/api/feature-delivery/review-policy-rollouts/system_design_artifact/audit",
		},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request = request.WithContext(auth.WithUser(
			request.Context(), &auth.User{ID: 42},
		))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d", test.method, test.path, response.Code)
		}
	}
}

func TestSetReviewPolicyRolloutCanonicalizesAdministratorInput(t *testing.T) {
	store, _, mux := reviewPolicyRolloutHTTPFixture(t)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/review-policy-rollouts/system_design_artifact",
		strings.NewReader(`{
			"candidate_policy_id":" REVIEW-SYSTEM_DESIGN_ARTIFACT ",
			"candidate_policy_version":1,
			"percentage_bps":2500,
			"salt":" rollout-2026-08 ",
			"active":true
		}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 41, IsAdmin: true},
	))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if store.savedRollout.SubjectKind != featuredelivery.SubjectSystemDesign ||
		store.savedRollout.CandidatePolicyID != store.policy.ID ||
		store.savedRollout.CandidatePolicyVersion != store.policy.Version ||
		store.savedRollout.PercentageBPS != 2500 ||
		store.savedRollout.Salt != "rollout-2026-08" ||
		store.savedRollout.RuleVersion != 1 ||
		store.savedRollout.RuleHash == "" ||
		!store.savedRollout.Active || store.savedActor != 41 {
		t.Fatalf("saved rollout = %+v, actor = %d", store.savedRollout, store.savedActor)
	}
	var payload struct {
		Data featuredelivery.ReviewPolicyRolloutRule `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data != store.savedRollout {
		t.Fatalf("response rule = %+v, saved = %+v", payload.Data, store.savedRollout)
	}
}

func TestSetReviewPolicyRolloutRejectsInvalidInput(t *testing.T) {
	_, _, mux := reviewPolicyRolloutHTTPFixture(t)
	validBody := `{
		"candidate_policy_id":"review-system_design_artifact",
		"candidate_policy_version":1,
		"percentage_bps":2500,
		"salt":"rollout-2026-08",
		"active":true
	}`
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "subject kind",
			path: "/api/feature-delivery/review-policy-rollouts/not-a-kind",
			body: validBody,
		},
		{
			name: "negative percentage",
			path: "/api/feature-delivery/review-policy-rollouts/system_design_artifact",
			body: strings.Replace(validBody, `"percentage_bps":2500`, `"percentage_bps":-1`, 1),
		},
		{
			name: "excess percentage",
			path: "/api/feature-delivery/review-policy-rollouts/system_design_artifact",
			body: strings.Replace(validBody, `"percentage_bps":2500`, `"percentage_bps":10001`, 1),
		},
		{
			name: "empty salt",
			path: "/api/feature-delivery/review-policy-rollouts/system_design_artifact",
			body: strings.Replace(validBody, `"salt":"rollout-2026-08"`, `"salt":"  "`, 1),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request = request.WithContext(auth.WithUser(
				request.Context(), &auth.User{ID: 41, IsAdmin: true},
			))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestListReviewPolicyRolloutAuditForwardsCursorAndLimit(t *testing.T) {
	store, _, mux := reviewPolicyRolloutHTTPFixture(t)
	store.audit = []featuredelivery.ReviewPolicyRolloutAuditEvent{
		{Seq: 12, SubjectKind: featuredelivery.SubjectSystemDesign, RuleVersion: 2},
		{Seq: 13, SubjectKind: featuredelivery.SubjectSystemDesign, RuleVersion: 3},
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-policy-rollouts/system_design_artifact/audit?after_seq=11&limit=2",
		nil,
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 41, IsAdmin: true},
	))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if store.auditKind != featuredelivery.SubjectSystemDesign ||
		store.auditAfter != 11 || store.auditLimit != 2 {
		t.Fatalf("audit query = %s/%d/%d", store.auditKind, store.auditAfter, store.auditLimit)
	}
	var payload struct {
		Data struct {
			Items        []featuredelivery.ReviewPolicyRolloutAuditEvent `json:"items"`
			NextAfterSeq int64                                           `json:"next_after_seq"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 2 || payload.Data.NextAfterSeq != 13 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestCreateReviewRoundRejectsClientDefinedPolicy(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/subjects/system_design_artifact/artifact-1/review-rounds",
		strings.NewReader(`{
			"policy_id":"system-design-review",
			"policy_version":1,
			"reviewers":[{"id":"client-controlled"}]
		}`),
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestCreateReviewRoundUsesServerDefaultPolicy(t *testing.T) {
	store := reviewRoundHTTPFixture()
	store.artifact.Kind = featuredelivery.KindSystemDesign
	store.artifact.Version = 1
	store.policy = reviewHTTPPolicy(t)
	service := featuredelivery.NewService(store, nil, time.Second)
	service.SetReviewConfiguration(nil, map[featuredelivery.SubjectKind]featuredelivery.ReviewPolicyRef{
		featuredelivery.SubjectSystemDesign: {
			ID: store.policy.ID, Version: store.policy.Version,
		},
	})
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/subjects/system_design_artifact/artifact-1/review-rounds",
		strings.NewReader(`{}`),
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.round.PolicyID != store.policy.ID ||
		store.round.PolicyVersion != store.policy.Version ||
		store.round.PolicySelection.Reason != "default" ||
		len(store.assignments) != len(store.policy.Reviewers) {
		t.Fatalf("round = %+v, assignments = %+v", store.round, store.assignments)
	}
	var payload struct {
		Data struct {
			Round featuredelivery.ReviewRound `json:"round"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.Round.PolicySelection.Reason != "default" {
		t.Fatalf("response round = %+v", payload.Data.Round)
	}
}

func TestCreateReviewRoundAcceptsExplicitReportReuse(t *testing.T) {
	store := reviewRoundHTTPFixture()
	store.artifact.Kind = featuredelivery.KindSystemDesign
	store.artifact.Version = 1
	store.policy = reviewHTTPPolicy(t)
	subject, err := featuredelivery.BuildArtifactReviewSubject(store.artifact)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := store.policy.Reviewers[0]
	sourceAssignment := featuredelivery.ReviewAssignment{
		ID: "source-assignment", RoundID: "source-round", ReviewerID: reviewer.ID,
		Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
		Categories: append([]string(nil), reviewer.Categories...),
		Required:   reviewer.Required, Status: featuredelivery.AssignmentRunning, Attempt: 1,
	}
	sourceReport, err := featuredelivery.PrepareReviewReport(
		featuredelivery.ReviewReport{
			RoundID: sourceAssignment.RoundID, AssignmentID: sourceAssignment.ID,
			ReviewerID: sourceAssignment.ReviewerID, SubjectHash: subject.ContentHash,
			Coverage: []featuredelivery.CoverageItem{{
				Category: reviewer.Categories[0], Covered: true,
			}},
			Summary:     "The immutable subject remains covered.",
			CompletedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
		},
		sourceAssignment,
		subject.ContentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	sourceAssignment.Status = featuredelivery.AssignmentSucceeded
	store.reuseSources = []featuredelivery.ReviewReportReuseSource{{
		Report: sourceReport, Assignment: sourceAssignment,
		PolicyID: store.policy.ID, PolicyVersion: store.policy.Version,
		PolicyHash: store.policy.ContentHash,
	}}
	payload, err := json.Marshal(map[string]any{
		"policy_id": store.policy.ID, "policy_version": store.policy.Version,
		"reuse_reports": []map[string]any{{
			"reviewer_id":      " Architecture ",
			"source_report_id": strings.ToUpper(sourceReport.ID),
			"report_hash":      strings.ToUpper(sourceReport.ReportHash),
			"reason":           " The immutable inputs are unchanged. ",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := featuredelivery.NewService(store, nil, time.Second)
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/subjects/system_design_artifact/artifact-1/review-rounds",
		strings.NewReader(string(payload)),
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if len(store.reusedReports) != 1 ||
		store.reusedReports[0].ReportHash != sourceReport.ReportHash ||
		store.reusedReports[0].Reuse == nil ||
		store.reusedReports[0].Reuse.SourceReportID != sourceReport.ID ||
		len(store.reportReuses) != 1 {
		t.Fatalf(
			"reports = %+v, reuses = %+v",
			store.reusedReports,
			store.reportReuses,
		)
	}
}

func TestCreateImplementationReviewRoundUsesServerDefaultPolicy(t *testing.T) {
	for _, kind := range []featuredelivery.SubjectKind{
		featuredelivery.SubjectValidationBundle,
		featuredelivery.SubjectDeliveryBundle,
	} {
		t.Run(string(kind), func(t *testing.T) {
			store := reviewRoundHTTPFixture()
			design := featuredelivery.Artifact{
				ID: "design-1", RequestID: store.feature.ID,
				Kind: featuredelivery.KindSystemDesign, Version: 1,
				ContentHash: "design-hash",
			}
			plan := featuredelivery.Artifact{
				ID: "plan-1", RequestID: store.feature.ID,
				Kind: featuredelivery.KindImplementationPlan, Version: 1,
				ParentArtifactID: design.ID, ContentHash: "plan-hash",
			}
			store.artifacts = map[string]featuredelivery.Artifact{
				design.ID: design,
				plan.ID:   plan,
			}
			store.run = featuredelivery.ImplementationRun{
				ID: "run-1", RequestID: store.feature.ID,
				DesignArtifactID: design.ID, PlanArtifactID: plan.ID,
				Repo: "nasuta", BaseCommit: strings.Repeat("1", 40),
				Status: featuredelivery.RunSucceeded, RequestedBy: store.feature.CreatedBy,
				ChangeSet: &featuredelivery.ChangeSet{
					RunID: "run-1", WorktreeHead: strings.Repeat("2", 40),
					PatchSHA256: "patch-hash",
					ValidationResults: []featuredelivery.ValidationResult{{
						Sequence: 1, Status: "validation_not_configured",
					}},
				},
			}
			store.policy = reviewHTTPPolicyForSubject(t, kind)
			service := featuredelivery.NewService(store, nil, time.Second)
			service.SetReviewConfiguration(nil, map[featuredelivery.SubjectKind]featuredelivery.ReviewPolicyRef{
				kind: {ID: store.policy.ID, Version: store.policy.Version},
			})
			mux := http.NewServeMux()
			New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
				mux.HandleFunc(pattern, handler)
			})
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/feature-delivery/subjects/"+string(kind)+"/run-1/review-rounds",
				strings.NewReader(`{}`),
			)
			request = request.WithContext(auth.WithUser(
				request.Context(),
				&auth.User{ID: store.feature.CreatedBy},
			))
			response := httptest.NewRecorder()

			mux.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					response.Code,
					http.StatusOK,
					response.Body.String(),
				)
			}
			if store.round.Subject.Kind != kind ||
				store.round.PolicyID != store.policy.ID ||
				store.round.PolicyVersion != store.policy.Version ||
				len(store.assignments) != len(store.policy.Reviewers) {
				t.Fatalf("round = %+v, assignments = %+v", store.round, store.assignments)
			}
		})
	}
}

func TestCreateReviewRoundRejectsPartialPolicyReference(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, body := range []string{
		`{"policy_id":"system-design-review"}`,
		`{"policy_version":1}`,
	} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/feature-delivery/subjects/system_design_artifact/artifact-1/review-rounds",
			strings.NewReader(body),
		)
		request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
		response := httptest.NewRecorder()

		mux.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want %d", body, response.Code, http.StatusBadRequest)
		}
	}
}

func TestListReviewRoundsParsesFeatureFilterAndStableCursor(t *testing.T) {
	store := reviewRoundHTTPFixture()
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	store.roundSummaries = []featuredelivery.ReviewRoundSummary{{
		ID: "round-1", FeatureID: store.feature.ID,
		SubjectKind: featuredelivery.SubjectSystemDesign,
		SubjectID:   store.artifact.ID, SubjectVersion: store.artifact.Version,
		Status: featuredelivery.RoundCompleted, CreatedAt: createdAt,
	}}
	store.roundHasMore = true
	cursor := reviewRoundCursorPayload{
		CreatedAt: createdAt.Add(time.Minute),
		ID:        "round-before",
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds?feature_id="+store.feature.ID+
			"&subject_kind=system_design_artifact&status=completed&limit=1&cursor="+
			encodeCursor(cursor),
		nil,
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.roundFilter.FeatureID != store.feature.ID ||
		store.roundFilter.SubjectKind != featuredelivery.SubjectSystemDesign ||
		store.roundFilter.Status != featuredelivery.RoundCompleted ||
		store.roundCursor.CreatedAt != cursor.CreatedAt ||
		store.roundCursor.ID != cursor.ID ||
		store.roundLimit != 1 ||
		store.roundUserID != 7 ||
		store.roundAdmin {
		t.Fatalf(
			"filter=%+v cursor=%+v limit=%d user=%d admin=%t",
			store.roundFilter,
			store.roundCursor,
			store.roundLimit,
			store.roundUserID,
			store.roundAdmin,
		)
	}
	var payload struct {
		Data struct {
			Items      []featuredelivery.ReviewRoundSummary `json:"items"`
			NextCursor string                               `json:"next_cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 ||
		payload.Data.Items[0].FeatureID != store.feature.ID ||
		payload.Data.NextCursor == "" {
		t.Fatalf("payload = %+v", payload.Data)
	}
	next, err := decodeReviewRoundCursor(payload.Data.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if next.CreatedAt != createdAt || next.ID != "round-1" {
		t.Fatalf("next cursor = %+v", next)
	}
}

func TestListReviewEventsParsesBoundedQueryAndEnforcesOwnership(t *testing.T) {
	store := reviewRoundHTTPFixture()
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/events?after_seq=7&limit=25",
		nil,
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.afterSeq != 7 || store.eventLimit != 25 {
		t.Fatalf("after_seq = %d, limit = %d", store.afterSeq, store.eventLimit)
	}
	var payload struct {
		Data struct {
			Items []featuredelivery.ReviewEvent `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 || payload.Data.Items[0].Seq != 8 {
		t.Fatalf("events = %+v", payload.Data.Items)
	}

	otherRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/events",
		nil,
	)
	otherRequest = otherRequest.WithContext(auth.WithUser(
		otherRequest.Context(), &auth.User{ID: 8},
	))
	otherResponse := httptest.NewRecorder()
	mux.ServeHTTP(otherResponse, otherRequest)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("other user status = %d, want %d", otherResponse.Code, http.StatusNotFound)
	}
}

func TestListFindingResolutionsParsesCursorAndEnforcesOwnership(t *testing.T) {
	store := reviewRoundHTTPFixture()
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	store.resolutions = []featuredelivery.FindingResolution{{
		ID: "resolution-1", FindingID: "finding-1",
		Resolution:  featuredelivery.ResolutionInvalidated,
		SubjectHash: store.round.Subject.ContentHash,
		Rationale:   "The finding is no longer applicable.", ActorID: 9,
		CreatedAt: createdAt,
	}}
	cursor := findingResolutionCursorPayload{
		CreatedAt: createdAt.Add(time.Minute),
		ID:        "resolution-before",
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/findings/finding-1/resolutions?cursor="+
			encodeCursor(cursor)+"&limit=1",
		nil,
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.resolutionSubject != store.round.Subject.ContentHash ||
		store.resolutionCursor.CreatedAt != cursor.CreatedAt ||
		store.resolutionCursor.ID != cursor.ID ||
		store.resolutionLimit != 1 {
		t.Fatalf(
			"subject = %q, cursor = %+v, limit = %d",
			store.resolutionSubject, store.resolutionCursor, store.resolutionLimit,
		)
	}
	var payload struct {
		Data struct {
			Items      []featuredelivery.FindingResolution `json:"items"`
			NextCursor string                              `json:"next_cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 ||
		payload.Data.Items[0].Resolution != featuredelivery.ResolutionInvalidated ||
		payload.Data.NextCursor == "" {
		t.Fatalf("payload = %+v", payload.Data)
	}

	for _, test := range []struct {
		name string
		user *auth.User
		want int
	}{
		{name: "other user hidden", user: &auth.User{ID: 8}, want: http.StatusNotFound},
		{name: "administrator", user: &auth.User{ID: 8, IsAdmin: true}, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/feature-delivery/findings/finding-1/resolutions",
				nil,
			)
			request = request.WithContext(auth.WithUser(request.Context(), test.user))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestCreateFindingResolutionCanonicalizesAndRejectsUnknownJSON(t *testing.T) {
	store := reviewRoundHTTPFixture()
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/findings/finding-1/resolutions",
		strings.NewReader(`{
			"resolution": " INVALIDATED ",
			"subject_hash": " SUBJECT-HASH ",
			"rationale": "  No longer applicable.  "
		}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 9, IsAdmin: true},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.resolution.Resolution != featuredelivery.ResolutionInvalidated ||
		store.resolution.SubjectHash != "subject-hash" ||
		store.resolution.Rationale != "No longer applicable." ||
		store.resolution.ActorID != 9 {
		t.Fatalf("resolution = %+v", store.resolution)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/findings/finding-1/resolutions",
		strings.NewReader(`{
			"resolution": "invalidated",
			"subject_hash": "subject-hash",
			"rationale": "valid",
			"unexpected": true
		}`),
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 9, IsAdmin: true},
	))
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestGetReviewReportEnforcesOwnershipAndAssignmentScope(t *testing.T) {
	store := reviewRoundHTTPFixture()
	store.report = featuredelivery.ReviewReport{
		ID: "report-1", RoundID: "round-1", AssignmentID: "assignment-1",
		ReviewerID: "architecture", SubjectHash: "subject-hash",
		Summary: "No blocking findings.", ContentHash: "report-hash",
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	for _, test := range []struct {
		name         string
		user         *auth.User
		assignmentID string
		want         int
	}{
		{name: "owner", user: &auth.User{ID: 7}, assignmentID: "assignment-1", want: http.StatusOK},
		{name: "administrator", user: &auth.User{ID: 8, IsAdmin: true}, assignmentID: "assignment-1", want: http.StatusOK},
		{name: "other user hidden", user: &auth.User{ID: 8}, assignmentID: "assignment-1", want: http.StatusNotFound},
		{name: "other assignment", user: &auth.User{ID: 7}, assignmentID: "assignment-2", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/feature-delivery/review-rounds/round-1/assignments/"+
					test.assignmentID+"/report",
				nil,
			)
			request = request.WithContext(auth.WithUser(request.Context(), test.user))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					response.Code,
					test.want,
					response.Body.String(),
				)
			}
			if test.want != http.StatusOK {
				return
			}
			var payload struct {
				Data featuredelivery.ReviewReport `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Data.ID != store.report.ID ||
				payload.Data.AssignmentID != store.report.AssignmentID {
				t.Fatalf("report = %+v", payload.Data)
			}
		})
	}
}

func TestListReviewAdjudicationsParsesCursorAndEnforcesOwnership(t *testing.T) {
	store := reviewRoundHTTPFixture()
	store.adjudications = []featuredelivery.ReviewAdjudication{{
		ID:             "adjudication-1",
		RoundID:        "round-1",
		SubjectHash:    "subject-hash",
		PolicyHash:     "policy-hash",
		Fingerprint:    "fingerprint-1",
		FindingIDs:     []string{"finding-1", "finding-2"},
		Agent:          agentapi.DefinitionRef{ID: "review.adjudicator", Version: 1},
		DefinitionHash: "definition-hash",
		Decision:       featuredelivery.AdjudicationNeedsHuman,
		Rationale:      "The evidence remains ambiguous.",
		ErrorCode:      "adjudication_runtime_failed",
		ContentHash:    "content-hash",
		CreatedAt:      time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}}
	cursor := reviewAdjudicationCursorPayload{
		Fingerprint: "fingerprint-0",
		ID:          "adjudication-0",
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Second)).RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/adjudications?cursor="+
			encodeCursor(cursor)+"&limit=1",
		nil,
	)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.adjudicationCursor != (featuredelivery.ReviewAdjudicationCursor{
		Fingerprint: cursor.Fingerprint,
		ID:          cursor.ID,
	}) || store.adjudicationLimit != 1 {
		t.Fatalf(
			"cursor = %+v, limit = %d",
			store.adjudicationCursor,
			store.adjudicationLimit,
		)
	}
	var payload struct {
		Data struct {
			Items      []featuredelivery.ReviewAdjudication `json:"items"`
			NextCursor string                               `json:"next_cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 ||
		payload.Data.Items[0].ID != store.adjudications[0].ID ||
		payload.Data.Items[0].Rationale != store.adjudications[0].Rationale ||
		payload.Data.Items[0].ErrorCode != store.adjudications[0].ErrorCode ||
		payload.Data.NextCursor == "" {
		t.Fatalf("payload = %+v", payload.Data)
	}

	otherRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/adjudications",
		nil,
	)
	otherRequest = otherRequest.WithContext(auth.WithUser(
		otherRequest.Context(), &auth.User{ID: 8},
	))
	otherResponse := httptest.NewRecorder()
	mux.ServeHTTP(otherResponse, otherRequest)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("other user status = %d, want %d", otherResponse.Code, http.StatusNotFound)
	}
}

func TestListReviewAdjudicationsRejectsInvalidQuery(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, query := range []string{
		"cursor=invalid!",
		"cursor=" + encodeCursor(reviewAdjudicationCursorPayload{Fingerprint: "fingerprint-0"}),
		"cursor=" + encodeCursor(reviewAdjudicationCursorPayload{ID: "adjudication-0"}),
		"limit=101",
	} {
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/feature-delivery/review-rounds/round-1/adjudications?"+query,
			nil,
		)
		request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want %d", query, response.Code, http.StatusBadRequest)
		}
	}
}

func TestListReviewEventsRejectsInvalidQuery(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, query := range []string{"after_seq=-1", "after_seq=invalid", "limit=501"} {
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/feature-delivery/review-rounds/round-1/events?"+query,
			nil,
		)
		request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want %d", query, response.Code, http.StatusBadRequest)
		}
	}
}

func TestCancelReviewRoundAllowsAdministrator(t *testing.T) {
	store := reviewRoundHTTPFixture()
	mux := http.NewServeMux()
	service := featuredelivery.NewService(store, nil, time.Second)
	coordinator := &reviewCoordinatorRecorder{
		cancel: func(ctx context.Context, roundID string, _ agentapi.Actor, admin bool) error {
			return service.CancelReviewRound(ctx, roundID, admin)
		},
	}
	handler := New(service)
	handler.SetReviewCoordinator(coordinator)
	handler.RegisterRoutes(
		func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) },
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/review-rounds/round-1/cancel",
		nil,
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 9, IsAdmin: true},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.cancelCalls != 1 || store.round.Status != featuredelivery.RoundCancelled {
		t.Fatalf("cancel calls = %d, round = %+v", store.cancelCalls, store.round)
	}
	if len(store.events) != 2 ||
		store.events[1].Kind != featuredelivery.ReviewEventRoundCancelled {
		t.Fatalf("events = %+v", store.events)
	}
	if coordinator.cancelCalls != 1 || coordinator.roundID != "round-1" ||
		coordinator.actor.UserID != 9 || !coordinator.admin {
		t.Fatalf("coordinator = %+v", coordinator)
	}
}

func TestExecuteReviewRoundDelegatesToCoordinator(t *testing.T) {
	result := &featuredelivery.ReviewGateResult{
		ID: "gate-1", RoundID: "round-1", Decision: featuredelivery.GatePass,
	}
	coordinator := &reviewCoordinatorRecorder{executeResult: result}
	handler := New(nil)
	handler.SetReviewCoordinator(coordinator)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/feature-delivery/review-rounds/round-1/execute",
		nil,
	)
	request.SetPathValue("round_id", "round-1")
	request = request.WithContext(auth.WithUser(
		request.Context(),
		&auth.User{ID: 9, IsAdmin: true},
	))
	response := httptest.NewRecorder()

	handler.ExecuteReviewRound(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if coordinator.executeCalls != 1 || coordinator.roundID != "round-1" ||
		coordinator.actor.UserID != 9 || !coordinator.admin {
		t.Fatalf("coordinator = %+v", coordinator)
	}
}

func TestListArtifactsRequiresMatchingKind(t *testing.T) {
	handler := New(nil)
	cursor := encodeCursor(artifactCursorPayload{Kind: featuredelivery.KindSystemDesign, Version: 2})
	for _, target := range []struct {
		name string
		url  string
	}{
		{name: "missing kind", url: "/api/features/feat-1/artifacts"},
		{name: "cursor kind mismatch", url: "/api/features/feat-1/artifacts?kind=requirement_analysis&cursor=" + cursor},
	} {
		t.Run(target.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target.url, nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()

			handler.ListArtifacts(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

func reviewRoundHTTPFixture() *reviewRoundHTTPStore {
	return &reviewRoundHTTPStore{
		feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: featuredelivery.Artifact{
			ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash",
		},
		round: featuredelivery.ReviewRound{
			ID: "round-1", Status: featuredelivery.RoundCreated,
			Subject: featuredelivery.ReviewSubject{
				Kind: featuredelivery.SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash", ContentHash: "subject-hash",
			},
		},
		finding: featuredelivery.FindingDetail{
			FindingSummary: featuredelivery.FindingSummary{
				ID: "finding-1", RoundID: "round-1",
				Severity: featuredelivery.SeverityHigh,
			},
		},
		events: []featuredelivery.ReviewEvent{{
			RoundID: "round-1", Seq: 8, Kind: featuredelivery.ReviewEventRoundStarted,
		}},
	}
}

func reviewHTTPPolicy(t *testing.T) featuredelivery.ReviewPolicy {
	return reviewHTTPPolicyForSubject(t, featuredelivery.SubjectSystemDesign)
}

func reviewHTTPPolicyForSubject(
	t *testing.T,
	kind featuredelivery.SubjectKind,
) featuredelivery.ReviewPolicy {
	t.Helper()
	policy, err := featuredelivery.PrepareReviewPolicy(featuredelivery.ReviewPolicy{
		ID: "review-" + string(kind), Version: 1,
		SubjectKind: kind,
		Reviewers: []featuredelivery.ReviewerSpec{
			{
				ID: "architecture",
				Agent: agentapi.DefinitionRef{
					ID: "review.architecture", Version: 1,
				},
				DefinitionHash: "architecture-hash",
				Categories:     []string{"architecture"},
				Required:       true,
				ReadOnly:       true,
			},
			{
				ID: "reliability",
				Agent: agentapi.DefinitionRef{
					ID: "review.reliability", Version: 1,
				},
				DefinitionHash: "reliability-hash",
				Categories:     []string{"reliability"},
				Required:       true,
				ReadOnly:       true,
			},
		},
		BlockingSeverities: []featuredelivery.Severity{
			featuredelivery.SeverityCritical,
			featuredelivery.SeverityHigh,
		},
		RequiredCategories:     []string{"architecture", "reliability"},
		MaxParallelism:         2,
		MaxInputTokens:         2,
		MaxOutputTokens:        2,
		MaxTotalTokens:         2,
		MaxToolCalls:           2,
		MaxCostMicros:          2,
		MaxRetries:             1,
		Timeout:                time.Minute,
		OptionalReviewerAction: featuredelivery.OptionalReviewerContinue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func reviewPolicyRolloutHTTPFixture(
	t *testing.T,
) (*reviewPolicyRolloutHTTPStore, *featuredelivery.Service, *http.ServeMux) {
	t.Helper()
	policy := reviewHTTPPolicy(t)
	store := &reviewPolicyRolloutHTTPStore{
		policy: featuredelivery.ReviewPolicyRecord{
			ReviewPolicy: policy,
			Active:       true,
		},
	}
	service := featuredelivery.NewService(store, nil, time.Minute)
	service.SetReviewConfiguration(nil, nil)
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	return store, service, mux
}

func TestDownloadValidationOutput(t *testing.T) {
	content := []byte("go test ./...\nok\n")
	handler, _ := validationDownloadHandler(t, content, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/validations/1/output", nil)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != string(content) {
		t.Fatalf("download status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Disposition"); got != `inline; filename="run-1-validation-01.log"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "17" {
		t.Fatalf("Content-Length = %q", got)
	}
}

func TestDownloadPatch(t *testing.T) {
	content := []byte("diff --git a/file.go b/file.go\n")
	handler := patchDownloadHandler(t, content, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != string(content) {
		t.Fatalf("download status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="run-1.patch"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "31" {
		t.Fatalf("Content-Length = %q", got)
	}
}

func TestDownloadPatchEnforcesAuthenticationAndOwnership(t *testing.T) {
	handler := patchDownloadHandler(t, []byte("patch"), nil)
	for _, test := range []struct {
		name string
		user *auth.User
		want int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "other user hidden", user: &auth.User{ID: 8}, want: http.StatusNotFound},
		{name: "administrator", user: &auth.User{ID: 8, IsAdmin: true}, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestDownloadPatchVerifiesMetadataAndHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*featuredelivery.ChangeSet)
	}{
		{name: "size mismatch", mutate: func(change *featuredelivery.ChangeSet) { change.PatchBytes++ }},
		{name: "hash mismatch", mutate: func(change *featuredelivery.ChangeSet) { change.PatchSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := patchDownloadHandler(t, []byte("patch"), test.mutate)
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
			}
		})
	}
}

func TestGenerationAuditRoutesEnforceAuthenticationAndOwnership(t *testing.T) {
	store := &generationAuditStore{
		feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7},
		run:     featuredelivery.GenerationRun{ID: "gen-1", RequestID: "feat-1"},
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Minute)).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	tests := []struct {
		name string
		path string
		user *auth.User
		want int
	}{
		{name: "detail unauthenticated", path: "/api/feature-generations/gen-1", want: http.StatusUnauthorized},
		{name: "detail owner", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 7}, want: http.StatusOK},
		{name: "detail other user hidden", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 8}, want: http.StatusNotFound},
		{name: "detail administrator", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 8, IsAdmin: true}, want: http.StatusOK},
		{name: "list owner", path: "/api/features/feat-1/generations?limit=10", user: &auth.User{ID: 7}, want: http.StatusOK},
		{name: "list other user hidden", path: "/api/features/feat-1/generations?limit=10", user: &auth.User{ID: 8}, want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestDownloadValidationOutputRejectsUnauthenticatedAndInvalidSequence(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	tests := []struct {
		name string
		path string
		user *auth.User
		want int
	}{
		{name: "unauthenticated", path: "/api/feature-implementations/run-1/validations/1/output", want: http.StatusUnauthorized},
		{name: "invalid sequence", path: "/api/feature-implementations/run-1/validations/not-a-number/output", user: &auth.User{ID: 7}, want: http.StatusBadRequest},
		{name: "zero sequence", path: "/api/feature-implementations/run-1/validations/0/output", user: &auth.User{ID: 7}, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestDownloadValidationOutputVerifiesMetadataAndHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*featuredelivery.ValidationResult)
	}{
		{name: "size mismatch", mutate: func(result *featuredelivery.ValidationResult) { result.OutputBytes++ }},
		{name: "hash mismatch", mutate: func(result *featuredelivery.ValidationResult) { result.OutputSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := validationDownloadHandler(t, []byte("validation output"), test.mutate)
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/validations/1/output", nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
			}
		})
	}
}

func validationDownloadHandler(t *testing.T, content []byte, mutate func(*featuredelivery.ValidationResult)) (http.Handler, *featuredelivery.ValidationResult) {
	t.Helper()
	workspaceRoot := t.TempDir()
	codingRoot := t.TempDir()
	store := &downloadStore{feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7}}
	workspaces, err := featuredelivery.NewWorkspaceManager(store, codingRoot)
	if err != nil {
		t.Fatal(err)
	}
	git, err := featuredelivery.NewGitManager(workspaceRoot, codingRoot, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(codingRoot, "artifacts", "run-1")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "validation-01.log"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	validation := featuredelivery.ValidationResult{
		Sequence: 1, OutputRelPath: "run-1/validation-01.log",
		OutputSHA256: hex.EncodeToString(sum[:]), OutputBytes: int64(len(content)),
	}
	if mutate != nil {
		mutate(&validation)
	}
	store.run = featuredelivery.ImplementationRun{
		ID: "run-1", RequestID: "feat-1",
		ChangeSet: &featuredelivery.ChangeSet{ValidationResults: []featuredelivery.ValidationResult{validation}},
	}
	service := featuredelivery.NewService(store, nil, time.Minute)
	service.SetImplementationManager(featuredelivery.NewImplementationManager(
		store, workspaces, git, nil, featuredelivery.ImplementationConfig{},
	))
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	return mux, &store.run.ChangeSet.ValidationResults[0]
}

func patchDownloadHandler(t *testing.T, content []byte, mutate func(*featuredelivery.ChangeSet)) http.Handler {
	t.Helper()
	workspaceRoot := t.TempDir()
	codingRoot := t.TempDir()
	store := &downloadStore{feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7}}
	workspaces, err := featuredelivery.NewWorkspaceManager(store, codingRoot)
	if err != nil {
		t.Fatal(err)
	}
	git, err := featuredelivery.NewGitManager(workspaceRoot, codingRoot, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(codingRoot, "artifacts", "run-1")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "changes.patch"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	change := &featuredelivery.ChangeSet{
		RunID: "run-1", PatchRelPath: "run-1/changes.patch",
		PatchSHA256: hex.EncodeToString(sum[:]), PatchBytes: int64(len(content)),
	}
	if mutate != nil {
		mutate(change)
	}
	store.run = featuredelivery.ImplementationRun{
		ID: "run-1", RequestID: "feat-1", ChangeSet: change,
	}
	service := featuredelivery.NewService(store, nil, time.Minute)
	service.SetImplementationManager(featuredelivery.NewImplementationManager(
		store, workspaces, git, nil, featuredelivery.ImplementationConfig{},
	))
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	return mux
}
