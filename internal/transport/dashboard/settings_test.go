package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

func TestSystemStatusIncludesFeatureDeliveryCapability(t *testing.T) {
	handler := &Handler{}
	handler.SetFeatureDeliveryStatus(func(context.Context) delivery.FeatureDeliveryStatus {
		return delivery.FeatureDeliveryStatus{
			Persistence: delivery.CapabilityStatus{Enabled: true},
		}
	})
	recorder := httptest.NewRecorder()
	handler.APISystemStatus(recorder, httptest.NewRequest("GET", "/api/system/status", nil))
	if !strings.Contains(recorder.Body.String(), `"feature_delivery"`) ||
		!strings.Contains(recorder.Body.String(), `"persistence":{"enabled":true}`) {
		t.Fatalf("status response = %s", recorder.Body.String())
	}
}

func TestDefaultSettingsIncludesRerankAndContext(t *testing.T) {
	handler := &Handler{
		platform: &config.PlatformSettings{
			ContextBudget:              64000,
			RerankEnabled:              true,
			RerankPool:                 80,
			RerankTopK:                 12,
			RerankMinScore:             0.42,
			RerankMinDensePreflight:    0.55,
			RunbookMinScore:            0.33,
			CodeMinScore:               0.11,
			RerankMaxPerService:        4,
			RerankMaxPerServiceLowBand: 2,
			RerankProvider:             "dashscope",
			RerankAPIKey:               "rk-test",
			RerankModel:                "gte-rerank-v2",
			RerankBaseURL:              "https://example.test/rerank",
			AgentTimeout:               config.Duration(2 * time.Minute),
			RetrievalRouterConfidence:  0.95,
			RetrievalRouterMaxTokens:   768,
		},
	}

	got := handler.defaultSettings()

	if got["context_budget"] != 64000 {
		t.Fatalf("context_budget = %v, want 64000", got["context_budget"])
	}
	if got["rerank_enabled"] != true {
		t.Fatalf("rerank_enabled = %v, want true", got["rerank_enabled"])
	}
	if got["rerank_provider"] != "dashscope" {
		t.Fatalf("rerank_provider = %v, want dashscope", got["rerank_provider"])
	}
	if got["rerank_api_key"] != "rk-test" {
		t.Fatalf("rerank_api_key = %v, want rk-test", got["rerank_api_key"])
	}
	if got["rerank_base_url"] != "https://example.test/rerank" {
		t.Fatalf("rerank_base_url = %v, want test URL", got["rerank_base_url"])
	}
	if got["retrieval_router_direct_min_confidence"] != 0.95 {
		t.Fatalf("retrieval_router_direct_min_confidence = %v", got["retrieval_router_direct_min_confidence"])
	}
}

func TestFilterSettingsKeepsExplicitEmptyValues(t *testing.T) {
	got, err := filterSettings(map[string]string{
		"rerank_provider":  "",
		"rerank_api_key":   "   ",
		"llm_max_tokens":   "0",
		"unknown_setting":  "x",
		"domain_knowledge": "  domain  ",
	})
	if err != nil {
		t.Fatalf("filterSettings: %v", err)
	}

	if v, ok := got["rerank_provider"]; !ok || v != "" {
		t.Fatalf("rerank_provider = %q ok=%v, want empty value preserved", v, ok)
	}
	if v, ok := got["rerank_api_key"]; !ok || v != "" {
		t.Fatalf("rerank_api_key = %q ok=%v, want trimmed empty value preserved", v, ok)
	}
	if got["llm_max_tokens"] != "0" {
		t.Fatalf("llm_max_tokens = %q, want 0", got["llm_max_tokens"])
	}
	if got["domain_knowledge"] != "domain" {
		t.Fatalf("domain_knowledge = %q, want trimmed content", got["domain_knowledge"])
	}
	if _, ok := got["unknown_setting"]; ok {
		t.Fatal("unknown_setting should be filtered out")
	}
}

func TestChangedSettingsTreatsCanonicalEquivalentValuesAsUnchanged(t *testing.T) {
	got := changedSettings(
		map[string]string{"coding_allow_network": "1"},
		map[string]string{"coding_allow_network": "true"},
	)
	if len(got) != 0 {
		t.Fatalf("changed settings = %v, want none", got)
	}
}

func TestCodingSettingsRelationshipValidation(t *testing.T) {
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	settings.Apply(map[string]string{
		"coding_enabled_providers": "codex,claude",
		"coding_default_provider":  "claude",
	})
	settings.Apply(map[string]string{"coding_enabled_providers": "codex"})
	if err := settings.ValidateCodingSettings(); err == nil {
		t.Fatal("expected disabled default provider to be rejected")
	}
	settings.Apply(map[string]string{"coding_default_provider": "codex"})
	if err := settings.ValidateCodingSettings(); err != nil {
		t.Fatalf("valid coding settings rejected: %v", err)
	}
}

func TestSettingsPutRejectsCodingDefaultOutsideEnabledProviders(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT k, v FROM settings`).WillReturnRows(
		sqlmock.NewRows([]string{"k", "v"}).
			AddRow("coding_enabled_providers", "codex,claude").
			AddRow("coding_default_provider", "claude"),
	)
	handler := &Handler{authDB: auth.NewDB(db), platform: &config.PlatformSettings{}}
	request := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(
		`{"coding_enabled_providers":"codex"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.APISettingsPut(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPutPassesChangedKeysToPlatformPort(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT k, v FROM settings`).WillReturnRows(
		sqlmock.NewRows([]string{"k", "v"}).
			AddRow("vcs_url", "https://old.example"),
	)
	mock.ExpectExec(`INSERT INTO settings \(k, v\) VALUES \(\?, \?\) ON DUPLICATE KEY UPDATE v=VALUES\(v\)`).
		WithArgs("vcs_url", "https://new.example").
		WillReturnResult(sqlmock.NewResult(1, 1))

	platformSettings := &config.PlatformSettings{
		AgentTimeout:       config.Duration(5 * time.Minute),
		AgentAnswerReserve: config.Duration(30 * time.Second),
	}
	platformSettings.Apply(nil)
	var gotKeys []string
	handler := &Handler{
		authDB:   auth.NewDB(db),
		platform: platformSettings,
		settingsChangedFn: func(keys []string) error {
			gotKeys = append([]string(nil), keys...)
			return nil
		},
	}
	request := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(
		`{"vcs_url":"https://new.example"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.APISettingsPut(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if len(gotKeys) != 1 || gotKeys[0] != "vcs_url" {
		t.Fatalf("changed keys = %v, want [vcs_url]", gotKeys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPutSkipsPersistenceAndReloadWhenValuesAreUnchanged(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT k, v FROM settings`).WillReturnRows(
		sqlmock.NewRows([]string{"k", "v"}).
			AddRow("vcs_url", "https://same.example"),
	)

	platformSettings := &config.PlatformSettings{
		AgentTimeout:       config.Duration(5 * time.Minute),
		AgentAnswerReserve: config.Duration(30 * time.Second),
	}
	platformSettings.Apply(nil)
	reloads := 0
	handler := &Handler{
		authDB:   auth.NewDB(db),
		platform: platformSettings,
		settingsChangedFn: func([]string) error {
			reloads++
			return nil
		},
	}
	request := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(
		`{"vcs_url":"https://same.example"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.APISettingsPut(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if reloads != 0 {
		t.Fatalf("runtime reloads = %d, want 0", reloads)
	}
	if !strings.Contains(response.Body.String(), `"updated":0`) {
		t.Fatalf("response = %s, want updated=0", response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
