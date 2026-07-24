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
