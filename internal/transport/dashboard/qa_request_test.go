package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
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
	var event string
	var data any
	handler.emitSessionRestartRecommendation(context.Background(), func(gotEvent string, gotData any) error {
		event, data = gotEvent, gotData
		return nil
	}, "qa-session", result, true)
	if event != "session.restart_recommended" {
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
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
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

	settings := &config.PlatformSettings{
		LLMBaseURL:       "https://api.example.com/v1",
		LLMAPIKey:        "test-key",
		LLMModel:         "gpt-test",
		LLMContextWindow: 256000,
	}
	hub := agentrun.NewRunHub(nil)
	hub.OnContextUsage(t.Context(), "run-1", agentrun.ContextUsageEvent{
		Phase:                 "session_pre_answer",
		ProjectedBeforeTokens: 104000,
		ProjectedAfterTokens:  76000,
		ContextWindow:         128000,
		HighWaterTokens:       102400,
		SafetyTokens:          6400,
		SafeLimitTokens:       121600,
		OutputReserveTokens:   8000,
		CompactionTriggered:   true,
		CompactionApplied:     true,
	})
	handler := &Handler{
		platform: settings,
		qaRuntimeFn: func() QARuntime {
			return QARuntime{Hub: hub, Settings: settings}
		},
	}
	recorder := httptest.NewRecorder()
	handler.APIQARuntimeStatus(recorder, httptest.NewRequest(
		"GET", "/api/qa/runtime?session_id=session-1&run_id=run-1", nil,
	))
	var response struct {
		Data struct {
			Model                    string `json:"model"`
			TokenUsageAvailable      bool   `json:"token_usage_available"`
			RoundCurrentTokens       int    `json:"round_current_tokens"`
			RoundProjectedTokens     int    `json:"round_projected_tokens"`
			RoundProjectedAfter      int    `json:"round_projected_after_tokens"`
			RoundHighWaterTokens     int    `json:"round_high_water_tokens"`
			RoundSafeLimitTokens     int    `json:"round_safe_limit_tokens"`
			RoundSafetyTokens        int    `json:"round_safety_tokens"`
			RoundOutputReserveTokens int    `json:"round_output_reserve_tokens"`
			RoundMaxTokens           int    `json:"round_max_tokens"`
			ContextWindowSource      string `json:"round_context_window_source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Model != "gpt-test" {
		t.Fatalf("model = %q", response.Data.Model)
	}
	if !response.Data.TokenUsageAvailable ||
		response.Data.RoundCurrentTokens != 104000 ||
		response.Data.RoundProjectedTokens != 104000 ||
		response.Data.RoundProjectedAfter != 76000 ||
		response.Data.RoundMaxTokens != 128000 ||
		response.Data.ContextWindowSource != "run" ||
		response.Data.RoundHighWaterTokens != 102400 ||
		response.Data.RoundSafeLimitTokens != 121600 ||
		response.Data.RoundSafetyTokens != 6400 ||
		response.Data.RoundOutputReserveTokens != 8000 {
		t.Fatalf("context runtime = %+v", response.Data)
	}
}

func TestRunQACompactionAsyncDetachesFromRequest(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	started := make(chan error, 1)
	release := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		runQACompactionAsync(requestCtx, time.Second, func(ctx context.Context) {
			started <- ctx.Err()
			<-release
		})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("async compaction blocked the request path")
	}
	select {
	case err := <-started:
		if err != nil {
			close(release)
			t.Fatalf("detached compaction inherited request cancellation: %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("async compaction did not start")
	}
	close(release)
}
