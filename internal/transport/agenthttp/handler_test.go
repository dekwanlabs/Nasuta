package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
	"github.com/dekwanlabs/nasuta/internal/auth"
)

type recordingCatalog struct {
	records      []agentcatalog.DefinitionRecord
	audit        []agentcatalog.AuditEvent
	rollout      agentcatalog.RolloutRule
	hasRollout   bool
	rolloutAudit []agentcatalog.RolloutAuditEvent

	publishCalls      int
	defaultCalls      int
	statusCalls       int
	rolloutCalls      int
	lastActorID       int64
	lastID            string
	lastVersion       int64
	lastActive        bool
	lastPercentageBPS int
	lastSalt          string
	lastAfterSeq      int64
	lastLimit         int
}

func (catalog *recordingCatalog) PublishAs(
	_ context.Context,
	_ []agentapi.Definition,
	actorUserID int64,
) error {
	catalog.publishCalls++
	catalog.lastActorID = actorUserID
	return nil
}

func (catalog *recordingCatalog) ListRecords(
	_ context.Context,
	cursor agentcatalog.DefinitionCursor,
	limit int,
) ([]agentcatalog.DefinitionRecord, error) {
	catalog.lastLimit = limit
	start := 0
	for start < len(catalog.records) {
		record := catalog.records[start]
		if record.ID > cursor.ID ||
			(record.ID == cursor.ID && record.Version > cursor.Version) {
			break
		}
		start++
	}
	end := min(start+limit, len(catalog.records))
	return append([]agentcatalog.DefinitionRecord(nil), catalog.records[start:end]...), nil
}

func (catalog *recordingCatalog) SetDefault(
	_ context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	catalog.defaultCalls++
	catalog.lastID = id
	catalog.lastVersion = version
	catalog.lastActorID = actorUserID
	return nil
}

func (catalog *recordingCatalog) SetActive(
	_ context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
) error {
	catalog.statusCalls++
	catalog.lastID = id
	catalog.lastVersion = version
	catalog.lastActive = active
	catalog.lastActorID = actorUserID
	return nil
}

func (catalog *recordingCatalog) ListAudit(
	_ context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]agentcatalog.AuditEvent, error) {
	catalog.lastID = id
	catalog.lastAfterSeq = afterSeq
	catalog.lastLimit = limit
	return append([]agentcatalog.AuditEvent(nil), catalog.audit...), nil
}

func (catalog *recordingCatalog) GetRollout(
	id string,
) (agentcatalog.RolloutRule, bool) {
	catalog.lastID = id
	return catalog.rollout, catalog.hasRollout
}

func (catalog *recordingCatalog) SetRollout(
	_ context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
) (agentcatalog.RolloutRule, error) {
	catalog.rolloutCalls++
	catalog.lastID = id
	catalog.lastVersion = candidateVersion
	catalog.lastPercentageBPS = percentageBPS
	catalog.lastSalt = salt
	catalog.lastActive = active
	catalog.lastActorID = actorUserID
	return catalog.rollout, nil
}

func (catalog *recordingCatalog) ListRolloutAudit(
	_ context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]agentcatalog.RolloutAuditEvent, error) {
	catalog.lastID = id
	catalog.lastAfterSeq = afterSeq
	catalog.lastLimit = limit
	return append([]agentcatalog.RolloutAuditEvent(nil), catalog.rolloutAudit...), nil
}

func TestAgentControlPlaneRequiresAuthenticationAndAdministratorWrites(t *testing.T) {
	catalog := &recordingCatalog{}
	mux := agentMux(&Handler{catalog: catalog})

	response := serveAgentRequest(mux, http.MethodGet, "/api/agents", "", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d", response.Code)
	}

	body := `{"definitions":[{"id":"qa.answerer","version":1}]}`
	response = serveAgentRequest(
		mux, http.MethodPost, "/api/agents", body, &auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || catalog.publishCalls != 0 {
		t.Fatalf("non-admin publish status=%d calls=%d", response.Code, catalog.publishCalls)
	}

	response = serveAgentRequest(
		mux, http.MethodPost, "/api/agents", body,
		&auth.User{ID: 8, IsAdmin: true},
	)
	if response.Code != http.StatusOK ||
		catalog.publishCalls != 1 ||
		catalog.lastActorID != 8 {
		t.Fatalf(
			"admin publish status=%d calls=%d actor=%d body=%s",
			response.Code, catalog.publishCalls, catalog.lastActorID,
			response.Body.String(),
		)
	}
}

func TestAgentListUsesBoundedStableCursor(t *testing.T) {
	catalog := &recordingCatalog{records: []agentcatalog.DefinitionRecord{
		{Definition: agentapi.Definition{ID: "a.agent", Version: 1}},
		{Definition: agentapi.Definition{ID: "a.agent", Version: 2}},
		{Definition: agentapi.Definition{ID: "b.agent", Version: 1}},
	}}
	mux := agentMux(&Handler{catalog: catalog})
	cursor := encodeDefinitionCursor(catalog.records[0])
	response := serveAgentRequest(
		mux, http.MethodGet, "/api/agents?limit=1&cursor="+cursor, "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Items      []agentcatalog.DefinitionRecord `json:"items"`
			NextCursor string                          `json:"next_cursor"`
		} `json:"data"`
	}
	decodeAgentResponse(t, response, &envelope)
	if len(envelope.Data.Items) != 1 ||
		envelope.Data.Items[0].Version != 2 ||
		envelope.Data.NextCursor == "" ||
		catalog.lastLimit != 1 {
		t.Fatalf("agent page=%+v limit=%d", envelope.Data, catalog.lastLimit)
	}
}

func TestAgentVersionControlsAndAuditAreAdministratorOnly(t *testing.T) {
	catalog := &recordingCatalog{audit: []agentcatalog.AuditEvent{{
		Seq: 4, DefinitionID: "qa.answerer", Version: 2, Action: "default_set",
	}}}
	mux := agentMux(&Handler{catalog: catalog})

	response := serveAgentRequest(
		mux, http.MethodPost,
		"/api/agents/qa.answerer/versions/2/default", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || catalog.defaultCalls != 0 {
		t.Fatalf("non-admin default status=%d calls=%d", response.Code, catalog.defaultCalls)
	}

	admin := &auth.User{ID: 8, IsAdmin: true}
	response = serveAgentRequest(
		mux, http.MethodPost,
		"/api/agents/qa.answerer/versions/2/default", "", admin,
	)
	if response.Code != http.StatusOK ||
		catalog.defaultCalls != 1 ||
		catalog.lastID != "qa.answerer" ||
		catalog.lastVersion != 2 ||
		catalog.lastActorID != 8 {
		t.Fatalf("default status=%d catalog=%+v", response.Code, catalog)
	}

	response = serveAgentRequest(
		mux, http.MethodPost,
		"/api/agents/qa.answerer/versions/1/status",
		`{"active":false}`, admin,
	)
	if response.Code != http.StatusOK ||
		catalog.statusCalls != 1 ||
		catalog.lastActive {
		t.Fatalf("status update status=%d catalog=%+v", response.Code, catalog)
	}

	response = serveAgentRequest(
		mux, http.MethodGet,
		"/api/agents/qa.answerer/audit?after_seq=3&limit=1", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin audit status=%d", response.Code)
	}
	response = serveAgentRequest(
		mux, http.MethodGet,
		"/api/agents/qa.answerer/audit?after_seq=3&limit=1", "", admin,
	)
	if response.Code != http.StatusOK ||
		catalog.lastAfterSeq != 3 ||
		catalog.lastLimit != 1 {
		t.Fatalf("audit status=%d catalog=%+v", response.Code, catalog)
	}
}

func TestAgentRolloutControlsAreAuthenticatedAndAuditable(t *testing.T) {
	rule := agentcatalog.RolloutRule{
		AgentID: "qa.answerer", RuleVersion: 3, CandidateVersion: 2,
		PercentageBPS: 2500, Salt: "rollout-2026-08",
		RuleHash: strings.Repeat("a", 64), Active: true, CreatedBy: 8,
	}
	catalog := &recordingCatalog{
		rollout: rule, hasRollout: true,
		rolloutAudit: []agentcatalog.RolloutAuditEvent{{
			Seq: 5, AgentID: rule.AgentID, RuleVersion: rule.RuleVersion,
			CandidateVersion: rule.CandidateVersion, PercentageBPS: rule.PercentageBPS,
			RuleHash: rule.RuleHash, Action: "rollout_enabled", ActorUserID: 8,
		}},
	}
	mux := agentMux(&Handler{catalog: catalog})

	response := serveAgentRequest(
		mux, http.MethodGet, "/api/agents/qa.answerer/rollout", "", nil,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rollout status=%d", response.Code)
	}
	response = serveAgentRequest(
		mux, http.MethodPost, "/api/agents/qa.answerer/rollout",
		`{"candidate_version":2,"percentage_bps":2500,"salt":"rollout-2026-08","active":true}`,
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden || catalog.rolloutCalls != 0 {
		t.Fatalf("non-admin rollout status=%d calls=%d", response.Code, catalog.rolloutCalls)
	}

	admin := &auth.User{ID: 8, IsAdmin: true}
	response = serveAgentRequest(
		mux, http.MethodPost, "/api/agents/qa.answerer/rollout",
		`{"candidate_version":2,"percentage_bps":2500,"salt":"rollout-2026-08","active":true}`,
		admin,
	)
	if response.Code != http.StatusOK ||
		catalog.rolloutCalls != 1 ||
		catalog.lastID != "qa.answerer" ||
		catalog.lastVersion != 2 ||
		catalog.lastPercentageBPS != 2500 ||
		catalog.lastSalt != "rollout-2026-08" ||
		!catalog.lastActive ||
		catalog.lastActorID != 8 {
		t.Fatalf("rollout update status=%d catalog=%+v", response.Code, catalog)
	}

	response = serveAgentRequest(
		mux, http.MethodGet, "/api/agents/qa.answerer/rollout", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("rollout get status=%d body=%s", response.Code, response.Body.String())
	}
	var rolloutEnvelope struct {
		Data agentcatalog.RolloutRule `json:"data"`
	}
	decodeAgentResponse(t, response, &rolloutEnvelope)
	if rolloutEnvelope.Data.RuleHash != rule.RuleHash {
		t.Fatalf("rollout response=%+v", rolloutEnvelope.Data)
	}

	response = serveAgentRequest(
		mux, http.MethodGet,
		"/api/agents/qa.answerer/rollout/audit?after_seq=4&limit=1", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-admin rollout audit status=%d", response.Code)
	}
	response = serveAgentRequest(
		mux, http.MethodGet,
		"/api/agents/qa.answerer/rollout/audit?after_seq=4&limit=1", "",
		admin,
	)
	if response.Code != http.StatusOK ||
		catalog.lastAfterSeq != 4 ||
		catalog.lastLimit != 1 {
		t.Fatalf("rollout audit status=%d catalog=%+v", response.Code, catalog)
	}
}

func TestAgentRolloutReturnsNotFoundWhenNoRuleExists(t *testing.T) {
	mux := agentMux(&Handler{catalog: &recordingCatalog{}})
	response := serveAgentRequest(
		mux, http.MethodGet, "/api/agents/qa.answerer/rollout", "",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing rollout status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAgentHandlerMapsCatalogErrors(t *testing.T) {
	handler := &Handler{catalog: failingCatalog{}}
	mux := agentMux(handler)
	response := serveAgentRequest(
		mux, http.MethodGet, "/api/agents", "", &auth.User{ID: 7},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable list status=%d body=%s", response.Code, response.Body.String())
	}
}

type failingCatalog struct{}

func (failingCatalog) PublishAs(context.Context, []agentapi.Definition, int64) error {
	return agentcatalog.ErrUnavailable
}
func (failingCatalog) ListRecords(
	context.Context,
	agentcatalog.DefinitionCursor,
	int,
) ([]agentcatalog.DefinitionRecord, error) {
	return nil, agentcatalog.ErrUnavailable
}
func (failingCatalog) SetDefault(context.Context, string, int64, int64) error {
	return errors.New("unused")
}
func (failingCatalog) SetActive(context.Context, string, int64, bool, int64) error {
	return errors.New("unused")
}
func (failingCatalog) GetRollout(string) (agentcatalog.RolloutRule, bool) {
	return agentcatalog.RolloutRule{}, false
}
func (failingCatalog) SetRollout(
	context.Context,
	string,
	int64,
	int,
	string,
	bool,
	int64,
) (agentcatalog.RolloutRule, error) {
	return agentcatalog.RolloutRule{}, errors.New("unused")
}
func (failingCatalog) ListAudit(
	context.Context,
	string,
	int64,
	int,
) ([]agentcatalog.AuditEvent, error) {
	return nil, errors.New("unused")
}
func (failingCatalog) ListRolloutAudit(
	context.Context,
	string,
	int64,
	int,
) ([]agentcatalog.RolloutAuditEvent, error) {
	return nil, errors.New("unused")
}

func agentMux(handler *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	return mux
}

func serveAgentRequest(
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

func decodeAgentResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
	target any,
) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
}
