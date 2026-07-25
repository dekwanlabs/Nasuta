package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestCompactionRestartRecommendation(t *testing.T) {
	tests := []struct {
		name   string
		result agent.SessionCompactionResult
		failed bool
		want   string
	}{
		{name: "hard failure", failed: true, want: "compaction_failed"},
		{name: "critical", result: agent.SessionCompactionResult{NewSessionRecommended: true, CriticalWaterReached: true}, want: "context_critical"},
		{name: "archive limit", result: agent.SessionCompactionResult{NewSessionRecommended: true}, want: "archived_history_limit"},
		{name: "not recommended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message, recommend := compactionRestartRecommendation(tt.result, tt.failed)
			if reason != tt.want || recommend != (tt.want != "") || (recommend && message == "") {
				t.Fatalf("reason=%q message=%q recommend=%t", reason, message, recommend)
			}
		})
	}
}

func TestEmitCompactionFailureRecommendation(t *testing.T) {
	handler := &Handler{platform: &config.PlatformSettings{LLMContextWindow: 128000}}
	result := agent.SessionCompactionResult{
		ArchivedTurnCount:     24,
		RestartTurnThreshold:  209,
		ProjectedBeforeTokens: 176070,
	}
	var event, data string
	handler.emitSessionRestartRecommendation(context.Background(), func(gotEvent, gotData string) {
		event, data = gotEvent, gotData
	}, "qa-session", result, true)
	if event != "session_restart_recommended" {
		t.Fatalf("event = %q", event)
	}
	var payload struct {
		Text                 string `json:"text"`
		Reason               string `json:"reason"`
		ArchivedTurns        int    `json:"archived_turns"`
		RestartTurnThreshold int    `json:"restart_turn_threshold"`
		ProjectedTokens      int    `json:"projected_tokens"`
		ContextWindow        int    `json:"context_window"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Reason != "compaction_failed" || payload.Text == "" ||
		payload.ArchivedTurns != 24 || payload.RestartTurnThreshold != 209 ||
		payload.ProjectedTokens != 176070 || payload.ContextWindow != 128000 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestParseQAAskRequestSourceMode(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMode    string
		wantSources domain.EvidenceSources
		wantPlan    bool
		wantErr     bool
	}{
		{name: "default auto", body: `{"question":" q "}`, wantMode: "auto"},
		{name: "explicit web", body: `{"question":"q","source_mode":" WEB "}`, wantMode: "web", wantSources: domain.Web, wantPlan: true},
		{name: "explicit direct", body: `{"question":"q","source_mode":"direct"}`, wantMode: "direct", wantPlan: true},
		{name: "invalid", body: `{"question":"q","source_mode":"database"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parseQAAskRequest(httptest.NewRequest("POST", "/api/qa/ask", strings.NewReader(tt.body)))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr=%t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if req.SourceMode != tt.wantMode || (req.EvidencePlan != nil) != tt.wantPlan {
				t.Fatalf("request = %+v", req)
			}
			if req.EvidencePlan != nil && req.EvidencePlan.Sources != tt.wantSources {
				t.Fatalf("sources = %08b, want %08b", req.EvidencePlan.Sources, tt.wantSources)
			}
		})
	}
}

func TestQARuntimeStatusFormatting(t *testing.T) {
	if got := qaEndpointDomain("https://api.example.com:8443/v1"); got != "api.example.com" {
		t.Fatalf("endpoint domain = %q", got)
	}
	if got := cachePercent(90, 100); got != 90 {
		t.Fatalf("cache percent = %d, want 90", got)
	}
	if got := cachePercent(0, 0); got != 0 {
		t.Fatalf("empty cache percent = %d, want 0", got)
	}
}
