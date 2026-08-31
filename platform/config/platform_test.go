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
	if !settings.DelegationEnabled {
		t.Fatal("delegation must default on")
	}
	settings.Apply(map[string]string{"delegation_enabled": "false"})
	if !settings.DelegationEnabled {
		t.Fatal("persisted delegation_enabled must be ignored; delegation stays on")
	}
	if settings.DelegationMaxChildren != DefaultDelegationMaxChildren ||
		settings.DelegationMaxConcurrent != DefaultDelegationMaxConcurrent {
		t.Fatalf(
			"delegation concurrency = %d/%d",
			settings.DelegationMaxConcurrent,
			settings.DelegationMaxChildren,
		)
	}
	if time.Duration(settings.DelegationChildTimeout) != DefaultDelegationChildTimeout {
		t.Fatalf("delegation child timeout = %s", time.Duration(settings.DelegationChildTimeout))
	}
	if settings.DelegationMaxTotalTokens != DefaultDelegationMaxTotalTokens ||
		settings.DelegationParentAnswerReserve != DefaultDelegationParentAnswerReserve {
		t.Fatalf(
			"delegation token budget = %d reserve=%d",
			settings.DelegationMaxTotalTokens,
			settings.DelegationParentAnswerReserve,
		)
	}
	if settings.DelegationMaxChildInputTokens != DefaultDelegationMaxChildInputTokens ||
		settings.DelegationMaxChildOutputTokens != DefaultDelegationMaxChildOutputTokens ||
		settings.DelegationMaxChildToolCalls != DefaultDelegationMaxChildToolCalls {
		t.Fatalf(
			"delegation child budget input=%d output=%d tools=%d",
			settings.DelegationMaxChildInputTokens,
			settings.DelegationMaxChildOutputTokens,
			settings.DelegationMaxChildToolCalls,
		)
	}
}

func TestApplyUpgradesShippedThreeChildDelegationCapacity(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(map[string]string{
		"agent_timeout":               "5m",
		"delegation_max_children":     "3",
		"delegation_max_total_tokens": "360000",
		"delegation_child_timeout":    "90s",
	})
	if settings.DelegationMaxChildren != DefaultDelegationMaxChildren ||
		settings.DelegationMaxTotalTokens != DefaultDelegationMaxTotalTokens ||
		time.Duration(settings.DelegationChildTimeout) != DefaultDelegationChildTimeout {
		t.Fatalf(
			"shipped 3-child capacity was not upgraded: children=%d total=%d timeout=%s",
			settings.DelegationMaxChildren,
			settings.DelegationMaxTotalTokens,
			time.Duration(settings.DelegationChildTimeout),
		)
	}
	if time.Duration(settings.AgentTimeout) != upgradedDelegationParentTimeout {
		t.Fatalf("parent timeout = %s, want %s", time.Duration(settings.AgentTimeout), upgradedDelegationParentTimeout)
	}
}

func TestApplyKeepsExplicitParentTimeoutWhenDelegationAlreadySized(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(map[string]string{
		"agent_timeout":               "5m",
		"delegation_max_children":     "6",
		"delegation_max_total_tokens": "720000",
		"delegation_child_timeout":    "150s",
	})
	if time.Duration(settings.AgentTimeout) != 5*time.Minute {
		t.Fatalf("explicit 5m parent timeout was rewritten: %s", time.Duration(settings.AgentTimeout))
	}
}

func TestApplyClampsAgentTimeoutToFourToTenMinutes(t *testing.T) {
	var short PlatformSettings
	short.Apply(map[string]string{"agent_timeout": "60s"})
	if time.Duration(short.AgentTimeout) != minAgentTimeout {
		t.Fatalf("60s parent timeout = %s, want %s", time.Duration(short.AgentTimeout), minAgentTimeout)
	}
	var long PlatformSettings
	long.Apply(map[string]string{"agent_timeout": "30m"})
	if time.Duration(long.AgentTimeout) != maxAgentTimeout {
		t.Fatalf("30m parent timeout = %s, want %s", time.Duration(long.AgentTimeout), maxAgentTimeout)
	}
}

func TestApplyUpgradesLookupSizedDelegationBudget(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(map[string]string{
		"delegation_max_concurrent":          "2",
		"delegation_max_child_tool_calls":    "8",
		"delegation_max_child_input_tokens":  "12000",
		"delegation_max_child_output_tokens": "1200",
		"delegation_max_report_tokens":       "1000",
		"delegation_max_total_tokens":        "48000",
	})
	if settings.DelegationMaxConcurrent != DefaultDelegationMaxConcurrent ||
		settings.DelegationMaxChildToolCalls != DefaultDelegationMaxChildToolCalls ||
		settings.DelegationMaxChildInputTokens != DefaultDelegationMaxChildInputTokens ||
		settings.DelegationMaxChildOutputTokens != DefaultDelegationMaxChildOutputTokens ||
		settings.DelegationMaxReportTokens != DefaultDelegationMaxReportTokens ||
		settings.DelegationMaxTotalTokens != DefaultDelegationMaxTotalTokens {
		t.Fatalf("legacy lookup budget was not upgraded: %+v", settings)
	}
}

func TestToolPruningDefaultsOffAndAppliesToggle(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(nil)
	if settings.ToolPruningEnabled {
		t.Fatalf("tool pruning default = true, want false (dry-run measurement)")
	}
	settings.Apply(map[string]string{"tool_pruning_enabled": "1"})
	if !settings.ToolPruningEnabled {
		t.Fatalf("tool pruning = false, want true after explicit 1")
	}
	settings.Apply(map[string]string{"tool_pruning_enabled": "0"})
	if settings.ToolPruningEnabled {
		t.Fatalf("tool pruning = true, want false after explicit 0")
	}
	if v, ok := settings.Values()["tool_pruning_enabled"]; !ok || v != false {
		t.Fatalf("values[tool_pruning_enabled] = %v, want false", v)
	}
	if got, err := CanonicalPlatformSetting("tool_pruning_enabled", "true"); err != nil || got != "true" {
		t.Fatalf("canonical = %q, err=%v", got, err)
	}
	if _, err := CanonicalPlatformSetting("tool_pruning_enabled", "banana"); err == nil {
		t.Fatalf("canonical accepted invalid value banana")
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

func TestAgentModelPricesMustBeConfiguredTogether(t *testing.T) {
	settings := PlatformSettings{AgentTimeout: Duration(time.Minute)}
	settings.Apply(nil)
	settings.LLMInputPriceMicrosPerMillionTokens = 1
	if err := settings.ValidateAgentSettings(); err == nil {
		t.Fatal("input-only model pricing was accepted")
	}
	settings.LLMOutputPriceMicrosPerMillionTokens = 2
	if err := settings.ValidateAgentSettings(); err != nil {
		t.Fatalf("complete model pricing: %v", err)
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

func TestDelegationEnabledIsNotConfigurable(t *testing.T) {
	if IsPlatformSetting("delegation_enabled") {
		t.Fatal("delegation_enabled must not be exposed as a platform setting")
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

func TestCanonicalDelegationSettings(t *testing.T) {
	valid := map[string]string{
		"delegation_capabilities":            " knowledge.docs.verify,knowledge.code.inspect,knowledge.docs.verify ",
		"delegation_max_children":            "3",
		"delegation_max_concurrent":          "2",
		"delegation_child_timeout":           "90s",
		"delegation_max_child_turns":         "4",
		"delegation_max_child_tool_calls":    "8",
		"delegation_max_child_input_tokens":  "12000",
		"delegation_max_child_output_tokens": "1200",
		"delegation_max_report_tokens":       "1000",
		"delegation_max_total_tokens":        "48000",
		"delegation_max_total_cost_micros":   "0",
		"delegation_parent_answer_reserve":   "4000",
	}
	for key, value := range valid {
		if _, err := CanonicalPlatformSetting(key, value); err != nil {
			t.Fatalf("CanonicalPlatformSetting(%q): %v", key, err)
		}
	}
	got, err := CanonicalPlatformSetting(
		"delegation_capabilities",
		valid["delegation_capabilities"],
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "knowledge.code.inspect,knowledge.docs.verify" {
		t.Fatalf("capabilities = %q", got)
	}
	for key, value := range map[string]string{
		"delegation_capabilities":           "Not Canonical",
		"delegation_max_children":           "0",
		"delegation_child_timeout":          "0s",
		"delegation_max_child_tool_calls":   "-1",
		"delegation_max_total_cost_micros":  "-1",
		"delegation_parent_answer_reserve":  "-1",
		"delegation_max_child_input_tokens": "invalid",
	} {
		if _, err := CanonicalPlatformSetting(key, value); err == nil {
			t.Fatalf("CanonicalPlatformSetting(%q, %q) accepted invalid value", key, value)
		}
	}
}

func TestValidateAgentSettingsChecksDelegationRelationships(t *testing.T) {
	var settings PlatformSettings
	settings.Apply(map[string]string{
		"agent_timeout":                      "5m",
		"agent_answer_reserve":               "30s",
		"delegation_max_children":            "3",
		"delegation_max_concurrent":          "2",
		"delegation_child_timeout":           "90s",
		"delegation_max_child_input_tokens":  "12000",
		"delegation_max_child_output_tokens": "1200",
		"delegation_max_total_tokens":        "48000",
		"delegation_parent_answer_reserve":   "4000",
		"delegation_max_child_turns":         "4",
		"delegation_max_child_tool_calls":    "8",
		"delegation_max_report_tokens":       "1000",
		"delegation_max_total_cost_micros":   "0",
	})
	if err := settings.ValidateAgentSettings(); err != nil {
		t.Fatalf("valid delegation settings: %v", err)
	}
	settings.DelegationMaxConcurrent = settings.DelegationMaxChildren + 1
	if err := settings.ValidateAgentSettings(); err == nil {
		t.Fatal("delegation concurrency above child count was accepted")
	}
	settings.DelegationMaxConcurrent = 2
	settings.DelegationMaxTotalTokens = 100
	if err := settings.ValidateAgentSettings(); err == nil {
		t.Fatal("insufficient aggregate delegation token budget was accepted")
	}
	settings.DelegationMaxTotalTokens = DefaultDelegationMaxTotalTokens
}

func TestEveryPlatformSettingHasCanonicalValidation(t *testing.T) {
	valid := map[string]string{
		"llm_model": "model", "llm_fast_model": "fast", "llm_base_url": "https://example.test",
		"llm_provider": "openai", "llm_max_tokens": "0", "llm_api_key": "key",
		"llm_answer_max_tokens": "1", "agent_conclusion_max_tokens": "1",
		"llm_max_continue_rounds": "0", "llm_context_window": "128000", "agent_answer_reserve": "30s",
		"llm_input_price_micros_per_million_tokens":  "0",
		"llm_output_price_micros_per_million_tokens": "0",
		"agent_timeout": "5m", "agent_max_steps": "1", "agent_max_tool_calls": "24", "context_budget": "1", "domain_knowledge": "domain",
		"retrieval_router_direct_min_confidence": "0.9", "retrieval_router_max_tokens": "512",
		"tool_pruning_enabled":           "false",
		"disable_legacy_answer_recovery": "false",
		"delegation_capabilities":        "knowledge.code.inspect", "delegation_max_children": "3", "delegation_max_concurrent": "2",
		"delegation_child_timeout": "90s", "delegation_max_child_turns": "4",
		"delegation_max_child_tool_calls": "8", "delegation_max_child_input_tokens": "12000",
		"delegation_max_child_output_tokens": "1200", "delegation_max_report_tokens": "1000",
		"delegation_max_total_tokens": "48000", "delegation_max_total_cost_micros": "0",
		"delegation_parent_answer_reserve": "4000",
		"rerank_enabled":                   "true", "rerank_pool": "1", "rerank_topk": "1", "rerank_min_score": "0.1",
		"rerank_min_dense_preflight": "0", "runbook_min_score": "0.2", "code_min_score": "1",
		"rerank_max_per_service": "1", "rerank_max_per_service_low_band": "1", "rerank_provider": "provider",
		"rerank_api_key": "key", "rerank_model": "model", "rerank_base_url": "https://example.test",
		"vcs_url": "https://example.test", "vcs_token": "token", "vcs_groups": "a,b",
		"vcs_webhook_secret": "secret", "vcs_clone_concurrency": "1", "vcs_exclude_projects": "x,y",
		"coding_enabled_providers": "codex", "coding_default_provider": "codex", "coding_codex_model": "model",
		"coding_claude_model": "model", "feature_generation_timeout": "5m", "coding_timeout": "30m",
		"coding_max_concurrency": "1", "coding_allow_network": "false", "coding_worktree_ttl": "72h",
	}
	if len(valid) != len(platformSettingKeys) {
		t.Fatalf("test values cover %d settings, contract has %d", len(valid), len(platformSettingKeys))
	}
	for key := range platformSettingKeys {
		if _, err := CanonicalPlatformSetting(key, valid[key]); err != nil {
			t.Fatalf("CanonicalPlatformSetting(%q): %v", key, err)
		}
	}
}

func TestCanonicalPlatformSettingRejectsInvalidTypedValues(t *testing.T) {
	invalid := map[string]string{
		"llm_provider": "other", "rerank_enabled": "yes", "agent_timeout": "soon",
		"agent_max_steps": "zero", "agent_max_tool_calls": "0", "rerank_min_score": "1.1", "vcs_clone_concurrency": "-1",
		"llm_input_price_micros_per_million_tokens": "-1",
		"delegation_max_total_tokens":               "0",
	}
	for key, value := range invalid {
		if _, err := CanonicalPlatformSetting(key, value); err == nil {
			t.Fatalf("CanonicalPlatformSetting(%q, %q) accepted invalid value", key, value)
		}
	}
	if _, err := CanonicalPlatformSetting("unknown", "value"); err == nil {
		t.Fatal("unknown platform setting was accepted")
	}
	for _, value := range []string{"60s", "12m"} {
		if _, err := CanonicalPlatformSetting("agent_timeout", value); err == nil {
			t.Fatalf("agent_timeout %s was accepted", value)
		}
	}
}
