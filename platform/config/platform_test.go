package config

import (
	"testing"
	"time"
)

func TestFastLLMConfiguredUsesMainEndpointAndKey(t *testing.T) {
	ps := &PlatformSettings{
		LLMBaseURL:   "https://main.example/v1",
		LLMAPIKey:    "main-key",
		LLMModel:     "main-model",
		LLMFastModel: "fast-model",
	}

	if !ps.FastLLMConfigured() {
		t.Fatal("FastLLMConfigured() = false, want true when fast model can reuse main endpoint/key")
	}
}

func TestFastLLMConfiguredRequiresFastModelAndMainConnection(t *testing.T) {
	cases := []PlatformSettings{
		{},
		{LLMFastModel: "fast-model"},
		{LLMBaseURL: "https://main.example/v1", LLMFastModel: "fast-model"},
		{LLMAPIKey: "main-key", LLMFastModel: "fast-model"},
	}

	for i, ps := range cases {
		if ps.FastLLMConfigured() {
			t.Fatalf("case %d: FastLLMConfigured() = true, want false", i)
		}
	}
}

func TestCanonicalRetrievalRouterSettings(t *testing.T) {
	cases := map[string]string{
		"retrieval_router_direct_min_confidence": "0.9",
		"retrieval_router_max_tokens":            "512",
	}
	for key, value := range cases {
		if _, err := CanonicalPlatformSetting(key, value); err != nil {
			t.Fatalf("CanonicalPlatformSetting(%q): %v", key, err)
		}
	}
	for key, value := range map[string]string{
		"retrieval_router_direct_min_confidence": "0",
		"retrieval_router_max_tokens":            "64",
	} {
		if _, err := CanonicalPlatformSetting(key, value); err == nil {
			t.Fatalf("CanonicalPlatformSetting(%q, %q) accepted invalid value", key, value)
		}
	}
}

func TestPlatformSettingsAppliesRetrievalRouterDefaults(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(nil)
	if settings.RetrievalRouterConfidence != DefaultRetrievalRouterDirectConfidence {
		t.Fatalf("confidence = %v", settings.RetrievalRouterConfidence)
	}
	if settings.RetrievalRouterMaxTokens != DefaultRetrievalRouterMaxTokens {
		t.Fatalf("max tokens = %d", settings.RetrievalRouterMaxTokens)
	}
	if time.Duration(settings.AgentAnswerReserve) != DefaultAgentAnswerReserve {
		t.Fatalf("answer reserve = %s", time.Duration(settings.AgentAnswerReserve))
	}
	if settings.LLMContextWindow != DefaultLLMContextWindow {
		t.Fatalf("context window = %d", settings.LLMContextWindow)
	}
	if time.Duration(settings.FeatureGenerationTimeout) != DefaultFeatureGenerationTimeout {
		t.Fatalf("feature generation timeout = %s", time.Duration(settings.FeatureGenerationTimeout))
	}
	if time.Duration(settings.CodingTimeout) != DefaultCodingTimeout {
		t.Fatalf("coding timeout = %s", time.Duration(settings.CodingTimeout))
	}
	if settings.CodingMaxConcurrency != DefaultCodingMaxConcurrency {
		t.Fatalf("coding concurrency = %d", settings.CodingMaxConcurrency)
	}
	if time.Duration(settings.CodingWorktreeTTL) != DefaultCodingWorktreeTTL {
		t.Fatalf("coding worktree TTL = %s", time.Duration(settings.CodingWorktreeTTL))
	}
}

func TestCanonicalLLMContextWindow(t *testing.T) {
	if got, err := CanonicalPlatformSetting("llm_context_window", "128000"); err != nil || got != "128000" {
		t.Fatalf("canonical context window = %q, err=%v", got, err)
	}
	for _, value := range []string{"8191", "2000001", "invalid"} {
		if _, err := CanonicalPlatformSetting("llm_context_window", value); err == nil {
			t.Fatalf("context window %q was accepted", value)
		}
	}
}

func TestCanonicalAgentAnswerReserveRequiresPositiveDuration(t *testing.T) {
	got, err := CanonicalPlatformSetting("agent_answer_reserve", "30s")
	if err != nil || got != "30s" {
		t.Fatalf("canonical reserve = %q, err=%v", got, err)
	}
	for _, value := range []string{"0s", "-1s", "invalid"} {
		if _, err := CanonicalPlatformSetting("agent_answer_reserve", value); err == nil {
			t.Fatalf("reserve %q was accepted", value)
		}
	}
}

func TestRetrievalRouterModeIsNotConfigurable(t *testing.T) {
	if IsPlatformSetting("retrieval_router_mode") {
		t.Fatal("retrieval_router_mode must not be exposed as a platform setting")
	}
}

func TestCanonicalCodingProvidersUsesFixedOrderAndDeduplicates(t *testing.T) {
	got, err := CanonicalPlatformSetting("coding_enabled_providers", " CLAUDE,codex,claude ")
	if err != nil {
		t.Fatalf("canonical providers: %v", err)
	}
	if got != "codex,claude" {
		t.Fatalf("providers = %q, want codex,claude", got)
	}
	if _, err := CanonicalPlatformSetting("coding_enabled_providers", "cursor"); err == nil {
		t.Fatal("unsupported provider was accepted")
	}
}

func TestValidateCodingSettingsRequiresEnabledDefault(t *testing.T) {
	settings := PlatformSettings{
		CodingEnabledProviders: []string{"codex"},
		CodingDefaultProvider:  "claude",
	}
	if err := settings.ValidateCodingSettings(); err == nil {
		t.Fatal("disabled default provider was accepted")
	}
	settings.CodingDefaultProvider = "codex"
	if err := settings.ValidateCodingSettings(); err != nil {
		t.Fatalf("enabled default provider: %v", err)
	}
}

func TestCanonicalCodingLimits(t *testing.T) {
	valid := map[string]string{
		"feature_generation_timeout": "5m",
		"coding_timeout":             "30m",
		"coding_worktree_ttl":        "72h",
		"coding_max_concurrency":     "2",
		"coding_allow_network":       "false",
	}
	for key, value := range valid {
		if _, err := CanonicalPlatformSetting(key, value); err != nil {
			t.Fatalf("CanonicalPlatformSetting(%q): %v", key, err)
		}
	}
}
