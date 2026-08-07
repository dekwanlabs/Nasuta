package workflowhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
)

func TestWorkflowRoutesRequireAuthenticationAndAdminPublication(t *testing.T) {
	workflows := &recordingService{}
	handler := &Handler{service: workflows}
	mux := workflowMux(handler)

	response := serveWorkflowRequest(
		mux, http.MethodGet, "/api/workflows", "", nil,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d", response.Code)
	}

	body := `{"definitions":[{"id":"review.flow","version":1}]}`
	response = serveWorkflowRequest(
		mux, http.MethodPost, "/api/workflows", body,
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || workflows.publishCalls != 1 {
		t.Fatalf(
			"non-admin publish status=%d calls=%d",
			response.Code,
			workflows.publishCalls,
		)
	}

	response = serveWorkflowRequest(
		mux, http.MethodPost, "/api/workflows", body,
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK || workflows.publishCalls != 2 ||
		!workflows.lastPublishAdmin {
		t.Fatalf(
			"admin publish status=%d calls=%d admin=%t body=%s",
			response.Code,
			workflows.publishCalls,
			workflows.lastPublishAdmin,
			response.Body.String(),
		)
	}
}

func TestWorkflowListUsesBoundedStableCursor(t *testing.T) {
	workflows := &recordingService{definitions: []agentworkflow.DefinitionRecord{
		{WorkflowDefinition: agentworkflow.WorkflowDefinition{ID: "a.flow", Version: 1}},
		{WorkflowDefinition: agentworkflow.WorkflowDefinition{ID: "a.flow", Version: 2}},
		{WorkflowDefinition: agentworkflow.WorkflowDefinition{ID: "b.flow", Version: 1}},
	}}
	mux := workflowMux(&Handler{service: workflows})
	cursor := encodeDefinitionCursor(workflows.definitions[0])
	response := serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflows?limit=1&cursor="+cursor,
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Items      []agentworkflow.DefinitionRecord `json:"items"`
			NextCursor string                           `json:"next_cursor"`
		} `json:"data"`
	}
	decodeResponse(t, response, &envelope)
	if len(envelope.Data.Items) != 1 ||
		envelope.Data.Items[0].Version != 2 ||
		envelope.Data.NextCursor == "" {
		t.Fatalf("workflow page = %+v", envelope.Data)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflows?limit=101",
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized page status=%d", response.Code)
	}
}

func TestWorkflowDefinitionControlsRequireAdminAndExposeAudit(t *testing.T) {
	workflows := &recordingService{
		audit: []agentworkflow.DefinitionAuditEvent{{
			Seq: 4, DefinitionID: "review.flow", Version: 2,
			Action: "default_set", ActorUserID: 8,
		}},
	}
	mux := workflowMux(&Handler{service: workflows})

	response := serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/versions/2/default",
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || workflows.defaultCalls != 1 {
		t.Fatalf("non-admin default status=%d calls=%d", response.Code, workflows.defaultCalls)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/versions/2/default",
		"",
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		workflows.lastDefinitionID != "review.flow" ||
		workflows.lastDefinitionVersion != 2 ||
		workflows.lastActorUserID != 8 {
		t.Fatalf(
			"admin default status=%d id=%q version=%d actor=%d body=%s",
			response.Code,
			workflows.lastDefinitionID,
			workflows.lastDefinitionVersion,
			workflows.lastActorUserID,
			response.Body.String(),
		)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/versions/2/status",
		`{"active":false}`,
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		workflows.statusCalls != 1 ||
		workflows.lastActive {
		t.Fatalf(
			"status update status=%d calls=%d active=%t body=%s",
			response.Code,
			workflows.statusCalls,
			workflows.lastActive,
			response.Body.String(),
		)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflows/review.flow/audit?after_seq=3&limit=1",
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || workflows.auditCalls != 1 {
		t.Fatalf("non-admin audit status=%d calls=%d", response.Code, workflows.auditCalls)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflows/review.flow/audit?after_seq=3&limit=1",
		"",
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		workflows.lastAfterSeq != 3 ||
		workflows.lastLimit != 1 {
		t.Fatalf(
			"audit status=%d after=%d limit=%d body=%s",
			response.Code,
			workflows.lastAfterSeq,
			workflows.lastLimit,
			response.Body.String(),
		)
	}
	var envelope struct {
		Data struct {
			Items        []agentworkflow.DefinitionAuditEvent `json:"items"`
			NextAfterSeq int64                                `json:"next_after_seq"`
		} `json:"data"`
	}
	decodeResponse(t, response, &envelope)
	if len(envelope.Data.Items) != 1 || envelope.Data.NextAfterSeq != 4 {
		t.Fatalf("audit page = %+v", envelope.Data)
	}
}

func TestWorkflowRolloutControlsAreAuthenticatedAndAuditable(t *testing.T) {
	rule := agentworkflow.RolloutRule{
		WorkflowID: "review.flow", RuleVersion: 3, CandidateVersion: 2,
		PercentageBPS: 2500, Salt: "rollout-2026-08",
		RuleHash: strings.Repeat("a", 64), Active: true, CreatedBy: 8,
	}
	workflows := &recordingService{
		rollout: rule, hasRollout: true,
		rolloutAudit: []agentworkflow.RolloutAuditEvent{{
			Seq: 5, WorkflowID: rule.WorkflowID, RuleVersion: rule.RuleVersion,
			CandidateVersion: rule.CandidateVersion, PercentageBPS: rule.PercentageBPS,
			RuleHash: rule.RuleHash, Action: "rollout_enabled", ActorUserID: 8,
		}},
	}
	mux := workflowMux(&Handler{service: workflows})

	response := serveWorkflowRequest(
		mux, http.MethodGet, "/api/workflows/review.flow/rollout", "", nil,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rollout status=%d", response.Code)
	}
	response = serveWorkflowRequest(
		mux, http.MethodPost, "/api/workflows/review.flow/rollout",
		`{"candidate_version":2,"percentage_bps":2500,"salt":" rollout-2026-08 ","active":true}`,
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || workflows.rolloutCalls != 1 {
		t.Fatalf(
			"non-admin rollout status=%d calls=%d",
			response.Code, workflows.rolloutCalls,
		)
	}

	admin := &auth.User{ID: 8, IsAdmin: true}
	response = serveWorkflowRequest(
		mux, http.MethodPost, "/api/workflows/review.flow/rollout",
		`{"candidate_version":2,"percentage_bps":2500,"salt":" rollout-2026-08 ","active":true}`,
		admin,
	)
	if response.Code != http.StatusOK ||
		workflows.rolloutCalls != 2 ||
		workflows.lastDefinitionID != "review.flow" ||
		workflows.lastDefinitionVersion != 2 ||
		workflows.lastPercentageBPS != 2500 ||
		workflows.lastSalt != "rollout-2026-08" ||
		!workflows.lastActive ||
		workflows.lastActorUserID != 8 {
		t.Fatalf(
			"rollout update status=%d service=%+v",
			response.Code, workflows,
		)
	}

	response = serveWorkflowRequest(
		mux, http.MethodGet, "/api/workflows/review.flow/rollout", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("rollout get status=%d body=%s", response.Code, response.Body.String())
	}
	var rolloutEnvelope struct {
		Data agentworkflow.RolloutRule `json:"data"`
	}
	decodeResponse(t, response, &rolloutEnvelope)
	if rolloutEnvelope.Data.RuleHash != rule.RuleHash {
		t.Fatalf("rollout response=%+v", rolloutEnvelope.Data)
	}

	response = serveWorkflowRequest(
		mux, http.MethodGet,
		"/api/workflows/review.flow/rollout/audit?after_seq=4&limit=1", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || workflows.rolloutAuditCalls != 1 {
		t.Fatalf(
			"non-admin rollout audit status=%d calls=%d",
			response.Code, workflows.rolloutAuditCalls,
		)
	}
	response = serveWorkflowRequest(
		mux, http.MethodGet,
		"/api/workflows/review.flow/rollout/audit?after_seq=4&limit=1", "",
		admin,
	)
	if response.Code != http.StatusOK ||
		workflows.rolloutAuditCalls != 2 ||
		workflows.lastAfterSeq != 4 ||
		workflows.lastLimit != 1 {
		t.Fatalf(
			"rollout audit status=%d service=%+v",
			response.Code, workflows,
		)
	}
	var auditEnvelope struct {
		Data struct {
			Items        []agentworkflow.RolloutAuditEvent `json:"items"`
			NextAfterSeq int64                             `json:"next_after_seq"`
		} `json:"data"`
	}
	decodeResponse(t, response, &auditEnvelope)
	if len(auditEnvelope.Data.Items) != 1 ||
		auditEnvelope.Data.NextAfterSeq != 5 {
		t.Fatalf("rollout audit page=%+v", auditEnvelope.Data)
	}
}

func TestWorkflowRolloutReturnsNotFoundWhenNoRuleExists(t *testing.T) {
	mux := workflowMux(&Handler{service: &recordingService{}})
	response := serveWorkflowRequest(
		mux, http.MethodGet, "/api/workflows/review.flow/rollout", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf(
			"missing rollout status=%d body=%s",
			response.Code, response.Body.String(),
		)
	}
}

func TestWorkflowStartPreservesControlledReferenceAndReadOnlyBoundary(t *testing.T) {
	workflows := &recordingService{writeWorkflow: true}
	mux := workflowMux(&Handler{service: workflows})
	body := `{"version":3,"input":{"subject":"change"}}`

	response := serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/runs",
		body,
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin write workflow status=%d body=%s", response.Code, response.Body.String())
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/runs",
		body,
		&auth.User{ID: 9, IsAdmin: true},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("admin start status=%d body=%s", response.Code, response.Body.String())
	}
	if workflows.startCalls != 2 ||
		workflows.lastStart.Workflow.ID != "review.flow" ||
		workflows.lastStart.Workflow.Version != 3 ||
		workflows.lastStart.Actor.UserID != 9 ||
		!workflows.lastStart.Admin ||
		string(workflows.lastStart.Input) != `{"subject":"change"}` {
		t.Fatalf("start request = %+v", workflows.lastStart)
	}

	response = serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflows/review.flow/runs",
		`{"version":1}`,
		&auth.User{ID: 9, IsAdmin: true},
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing input status=%d", response.Code)
	}
}

func TestWorkflowRunAuthorizationAllowsOwnerAndAdmin(t *testing.T) {
	workflows := &recordingService{run: agentworkflow.WorkflowRunRecord{
		ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunRunning,
	}}
	mux := workflowMux(&Handler{service: workflows})

	for _, test := range []struct {
		name   string
		user   *auth.User
		status int
	}{
		{name: "owner", user: &auth.User{ID: 7}, status: http.StatusOK},
		{name: "administrator", user: &auth.User{ID: 8, IsAdmin: true}, status: http.StatusOK},
		{name: "other user", user: &auth.User{ID: 9}, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serveWorkflowRequest(
				mux,
				http.MethodGet,
				"/api/workflow-runs/workflow_1",
				"",
				test.user,
			)
			if response.Code != test.status {
				t.Fatalf(
					"status=%d want=%d body=%s",
					response.Code,
					test.status,
					response.Body.String(),
				)
			}
		})
	}
}

func TestWorkflowNodeAndHandoffPaginationPassStableCursors(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	workflows := &recordingService{
		run: agentworkflow.WorkflowRunRecord{
			ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunRunning,
		},
		nodes: []agentworkflow.NodeRunRecord{
			{NodeID: "review", Attempt: 2},
			{NodeID: "synthesize", Attempt: 1},
		},
		handoffs: []agentworkflow.Handoff{
			{ID: "handoff_2", CreatedAt: now.Add(time.Second)},
			{ID: "handoff_3", CreatedAt: now.Add(2 * time.Second)},
		},
	}
	mux := workflowMux(&Handler{service: workflows})
	nodeCursor := encodeCursor(agentworkflow.NodeRunCursor{
		NodeID: "review", Attempt: 1,
	})
	response := serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflow-runs/workflow_1/nodes?limit=2&cursor="+nodeCursor,
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK ||
		workflows.lastNodeCursor.NodeID != "review" ||
		workflows.lastNodeCursor.Attempt != 1 ||
		workflows.lastLimit != 2 {
		t.Fatalf(
			"node page status=%d cursor=%+v limit=%d body=%s",
			response.Code,
			workflows.lastNodeCursor,
			workflows.lastLimit,
			response.Body.String(),
		)
	}

	handoffCursor := encodeCursor(agentworkflow.HandoffCursor{
		CreatedAt: now, ID: "handoff_1",
	})
	response = serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflow-runs/workflow_1/handoffs?limit=2&cursor="+handoffCursor,
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK ||
		workflows.lastHandoffCursor.ID != "handoff_1" ||
		!workflows.lastHandoffCursor.CreatedAt.Equal(now) ||
		workflows.lastLimit != 2 {
		t.Fatalf(
			"handoff page status=%d cursor=%+v limit=%d body=%s",
			response.Code,
			workflows.lastHandoffCursor,
			workflows.lastLimit,
			response.Body.String(),
		)
	}
}

func TestWorkflowCancelIsIdempotent(t *testing.T) {
	workflows := &recordingService{run: agentworkflow.WorkflowRunRecord{
		ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunRunning,
	}}
	mux := workflowMux(&Handler{service: workflows})

	for call := 0; call < 2; call++ {
		response := serveWorkflowRequest(
			mux,
			http.MethodPost,
			"/api/workflow-runs/workflow_1/cancel",
			"",
			&auth.User{ID: 7},
		)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"cancel %d status=%d body=%s",
				call+1,
				response.Code,
				response.Body.String(),
			)
		}
		var envelope struct {
			Data agentworkflow.CancelTransition `json:"data"`
		}
		decodeResponse(t, response, &envelope)
		if envelope.Data.Applied != (call == 0) ||
			envelope.Data.Status != agentworkflow.RunCancelled {
			t.Fatalf("cancel %d transition=%+v", call+1, envelope.Data)
		}
	}
}

func TestWorkflowApprovalValidatesInputAndAuthorization(t *testing.T) {
	workflows := &recordingService{run: agentworkflow.WorkflowRunRecord{
		ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunWaitingHuman,
	}}
	mux := workflowMux(&Handler{service: workflows})
	path := "/api/workflow-runs/workflow_1/nodes/human.review/approval"

	response := serveWorkflowRequest(
		mux, http.MethodPost, path,
		`{"decision":"maybe"}`,
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusBadRequest || workflows.approvalCalls != 0 {
		t.Fatalf(
			"invalid approval status=%d calls=%d",
			response.Code,
			workflows.approvalCalls,
		)
	}

	response = serveWorkflowRequest(
		mux, http.MethodPost, path,
		`{"decision":"approved"}`,
		&auth.User{ID: 9},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-owner approval status=%d", response.Code)
	}

	response = serveWorkflowRequest(
		mux, http.MethodPost, path,
		`{"decision":" APPROVED ","comment":"  checked  "}`,
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		workflows.lastApproval.WorkflowRunID != "workflow_1" ||
		workflows.lastApproval.NodeID != "human.review" ||
		workflows.lastApproval.Decision != agentworkflow.ApprovalApproved ||
		workflows.lastApproval.Comment != "checked" ||
		!workflows.lastApproval.Admin {
		t.Fatalf(
			"admin approval status=%d request=%+v body=%s",
			response.Code,
			workflows.lastApproval,
			response.Body.String(),
		)
	}
}

func TestWorkflowApprovalDelegatesToConfiguredDecider(t *testing.T) {
	workflows := &recordingService{}
	decider := &recordingApprovalDecider{}
	handler := &Handler{service: workflows}
	handler.SetApprovalDecider(decider)
	mux := workflowMux(handler)

	response := serveWorkflowRequest(
		mux,
		http.MethodPost,
		"/api/workflow-runs/pipeline_1/nodes/approve.system_design/approval",
		`{"decision":"rejected","comment":"  revise  "}`,
		&auth.User{ID: 18, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		decider.calls != 1 ||
		workflows.approvalCalls != 0 ||
		decider.lastRequest.WorkflowRunID != "pipeline_1" ||
		decider.lastRequest.NodeID != "approve.system_design" ||
		decider.lastRequest.Decision != agentworkflow.ApprovalRejected ||
		decider.lastRequest.Approver.UserID != 18 ||
		!decider.lastRequest.Admin ||
		decider.lastRequest.Comment != "revise" {
		t.Fatalf(
			"status=%d decider_calls=%d service_calls=%d request=%+v body=%s",
			response.Code,
			decider.calls,
			workflows.approvalCalls,
			decider.lastRequest,
			response.Body.String(),
		)
	}
}

func TestWorkflowDomainErrorsMapToHTTPStatus(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{err: agentworkflow.ErrInvalid, status: http.StatusBadRequest},
		{err: agentworkflow.ErrForbidden, status: http.StatusForbidden},
		{err: agentworkflow.ErrNotFound, status: http.StatusNotFound},
		{err: agentworkflow.ErrConflict, status: http.StatusConflict},
		{err: agentworkflow.ErrUnavailable, status: http.StatusServiceUnavailable},
		{err: errors.New("database failed"), status: http.StatusInternalServerError},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		writeDomainError(response, test.err)
		if response.Code != test.status {
			t.Fatalf(
				"error=%v status=%d want=%d",
				test.err,
				response.Code,
				test.status,
			)
		}
	}
}

type recordingService struct {
	mu sync.Mutex

	definitions  []agentworkflow.DefinitionRecord
	audit        []agentworkflow.DefinitionAuditEvent
	rollout      agentworkflow.RolloutRule
	hasRollout   bool
	rolloutAudit []agentworkflow.RolloutAuditEvent
	run          agentworkflow.WorkflowRunRecord
	nodes        []agentworkflow.NodeRunRecord
	events       []agentworkflow.Event
	handoffs     []agentworkflow.Handoff
	reader       eventReader
	live         chan agentworkflow.Event

	writeWorkflow bool
	cancelled     bool

	publishCalls          int
	defaultCalls          int
	statusCalls           int
	auditCalls            int
	rolloutCalls          int
	rolloutAuditCalls     int
	startCalls            int
	approvalCalls         int
	openCalls             int
	subscribeCalls        int
	lastPublishAdmin      bool
	lastDefinitionID      string
	lastDefinitionVersion int64
	lastActorUserID       int64
	lastActive            bool
	lastPercentageBPS     int
	lastSalt              string
	lastStart             agentworkflow.StartRequest
	lastApproval          agentworkflow.ApprovalRequest
	lastNodeCursor        agentworkflow.NodeRunCursor
	lastHandoffCursor     agentworkflow.HandoffCursor
	lastAfterSeq          int64
	lastLimit             int
}

type recordingApprovalDecider struct {
	calls       int
	lastRequest agentworkflow.ApprovalRequest
}

func (decider *recordingApprovalDecider) DecideHumanApproval(
	_ context.Context,
	request agentworkflow.ApprovalRequest,
) (agentworkflow.ApprovalResult, error) {
	decider.calls++
	decider.lastRequest = request
	return agentworkflow.ApprovalResult{
		Applied: true,
		Status:  agentworkflow.RunRunning,
	}, nil
}

func (service *recordingService) PublishDefinitionsAs(
	_ context.Context,
	_ []agentworkflow.WorkflowDefinition,
	actorUserID int64,
	admin bool,
) error {
	service.publishCalls++
	service.lastPublishAdmin = admin
	service.lastActorUserID = actorUserID
	if !admin {
		return agentworkflow.ErrForbidden
	}
	return nil
}

func (service *recordingService) ListDefinitionRecords(
	_ context.Context,
	cursor agentworkflow.DefinitionCursor,
	limit int,
) ([]agentworkflow.DefinitionRecord, error) {
	service.lastLimit = limit
	start := 0
	for start < len(service.definitions) {
		record := service.definitions[start]
		if record.ID > cursor.ID ||
			(record.ID == cursor.ID && record.Version > cursor.Version) {
			break
		}
		start++
	}
	end := min(start+limit, len(service.definitions))
	return append([]agentworkflow.DefinitionRecord(nil), service.definitions[start:end]...), nil
}

func (service *recordingService) SetDefinitionDefault(
	_ context.Context,
	id string,
	version int64,
	actorUserID int64,
	admin bool,
) error {
	service.defaultCalls++
	service.lastDefinitionID = id
	service.lastDefinitionVersion = version
	service.lastActorUserID = actorUserID
	if !admin {
		return agentworkflow.ErrForbidden
	}
	return nil
}

func (service *recordingService) SetDefinitionActive(
	_ context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
	admin bool,
) error {
	service.statusCalls++
	service.lastDefinitionID = id
	service.lastDefinitionVersion = version
	service.lastActorUserID = actorUserID
	service.lastActive = active
	if !admin {
		return agentworkflow.ErrForbidden
	}
	return nil
}

func (service *recordingService) ListDefinitionAudit(
	_ context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]agentworkflow.DefinitionAuditEvent, error) {
	service.auditCalls++
	service.lastDefinitionID = id
	service.lastAfterSeq = afterSeq
	service.lastLimit = limit
	if !admin {
		return nil, agentworkflow.ErrForbidden
	}
	return append([]agentworkflow.DefinitionAuditEvent(nil), service.audit...), nil
}

func (service *recordingService) GetDefinitionRollout(
	id string,
) (agentworkflow.RolloutRule, bool, error) {
	service.lastDefinitionID = id
	return service.rollout, service.hasRollout, nil
}

func (service *recordingService) SetDefinitionRollout(
	_ context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
	admin bool,
) (agentworkflow.RolloutRule, error) {
	service.rolloutCalls++
	service.lastDefinitionID = id
	service.lastDefinitionVersion = candidateVersion
	service.lastPercentageBPS = percentageBPS
	service.lastSalt = salt
	service.lastActive = active
	service.lastActorUserID = actorUserID
	if !admin {
		return agentworkflow.RolloutRule{}, agentworkflow.ErrForbidden
	}
	return service.rollout, nil
}

func (service *recordingService) ListDefinitionRolloutAudit(
	_ context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]agentworkflow.RolloutAuditEvent, error) {
	service.rolloutAuditCalls++
	service.lastDefinitionID = id
	service.lastAfterSeq = afterSeq
	service.lastLimit = limit
	if !admin {
		return nil, agentworkflow.ErrForbidden
	}
	return append([]agentworkflow.RolloutAuditEvent(nil), service.rolloutAudit...), nil
}

func (service *recordingService) Start(
	_ context.Context,
	request agentworkflow.StartRequest,
) (*agentworkflow.WorkflowRunRecord, error) {
	service.startCalls++
	service.lastStart = request
	service.lastStart.Input = append(json.RawMessage(nil), request.Input...)
	if service.writeWorkflow && !request.Admin {
		return nil, agentworkflow.ErrForbidden
	}
	run := service.run
	if run.ID == "" {
		run = agentworkflow.WorkflowRunRecord{
			ID:          "workflow_1",
			WorkflowID:  request.Workflow.ID,
			ActorUserID: request.Actor.UserID,
			Status:      agentworkflow.RunRunning,
		}
	}
	return &run, nil
}

func (service *recordingService) GetRun(
	_ context.Context,
	runID string,
	userID int64,
	admin bool,
) (*agentworkflow.WorkflowRunRecord, error) {
	if service.run.ID != runID {
		return nil, agentworkflow.ErrNotFound
	}
	if !admin && service.run.ActorUserID != userID {
		return nil, agentworkflow.ErrForbidden
	}
	run := service.run
	return &run, nil
}

func (service *recordingService) ListNodeRuns(
	ctx context.Context,
	runID string,
	cursor agentworkflow.NodeRunCursor,
	limit int,
	userID int64,
	admin bool,
) ([]agentworkflow.NodeRunRecord, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	service.lastNodeCursor = cursor
	service.lastLimit = limit
	return append([]agentworkflow.NodeRunRecord(nil), service.nodes...), nil
}

func (service *recordingService) ListEvents(
	ctx context.Context,
	runID string,
	afterSeq int64,
	limit int,
	userID int64,
	admin bool,
) ([]agentworkflow.Event, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	service.lastAfterSeq = afterSeq
	service.lastLimit = limit
	return append([]agentworkflow.Event(nil), service.events...), nil
}

func (service *recordingService) OpenRunEvents(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*agentworkflow.WorkflowRunRecord, eventReader, error) {
	service.mu.Lock()
	service.openCalls++
	service.mu.Unlock()
	run, err := service.GetRun(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	reader := service.reader
	if reader == nil {
		reader = &sliceEventReader{events: service.events}
	}
	return run, reader, nil
}

func (service *recordingService) SubscribeEvents(
	string,
) (<-chan agentworkflow.Event, func(), error) {
	service.mu.Lock()
	service.subscribeCalls++
	service.mu.Unlock()
	if service.live == nil {
		service.live = make(chan agentworkflow.Event)
	}
	return service.live, func() {}, nil
}

func (service *recordingService) ListHandoffs(
	ctx context.Context,
	runID string,
	cursor agentworkflow.HandoffCursor,
	limit int,
	userID int64,
	admin bool,
) ([]agentworkflow.Handoff, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	service.lastHandoffCursor = cursor
	service.lastLimit = limit
	return append([]agentworkflow.Handoff(nil), service.handoffs...), nil
}

func (service *recordingService) Cancel(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (agentworkflow.CancelTransition, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return agentworkflow.CancelTransition{}, err
	}
	if service.cancelled {
		return agentworkflow.CancelTransition{
			Status: agentworkflow.RunCancelled,
		}, nil
	}
	service.cancelled = true
	service.run.Status = agentworkflow.RunCancelled
	return agentworkflow.CancelTransition{
		Applied: true, Status: agentworkflow.RunCancelled,
	}, nil
}

func (service *recordingService) DecideHumanApproval(
	_ context.Context,
	request agentworkflow.ApprovalRequest,
) (agentworkflow.ApprovalResult, error) {
	service.approvalCalls++
	service.lastApproval = request
	if !request.Admin && request.Approver.UserID != service.run.ActorUserID {
		return agentworkflow.ApprovalResult{}, agentworkflow.ErrForbidden
	}
	return agentworkflow.ApprovalResult{
		Applied: true, Status: agentworkflow.RunRunning,
		Approval: agentworkflow.WorkflowApproval{
			WorkflowRunID: request.WorkflowRunID,
			NodeID:        request.NodeID,
			Decision:      request.Decision,
		},
	}, nil
}

func workflowMux(handler *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	return mux
}

func serveWorkflowRequest(
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

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
}
