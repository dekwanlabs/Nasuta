package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRetrievalRouterDirectConfidence = 0.90
	DefaultRetrievalRouterMaxTokens        = 512
	DefaultAgentAnswerReserve              = 30 * time.Second
	DefaultLLMContextWindow                = 128000
	DefaultFeatureGenerationTimeout        = 5 * time.Minute
	DefaultCodingTimeout                   = 30 * time.Minute
	DefaultCodingMaxConcurrency            = 1
	DefaultCodingWorktreeTTL               = 72 * time.Hour
	DefaultDelegationMaxChildren           = 3
	DefaultDelegationMaxConcurrent         = 2
	DefaultDelegationChildTimeout          = 90 * time.Second
	DefaultDelegationMaxChildTurns         = 4
	DefaultDelegationMaxChildToolCalls     = 8
	DefaultDelegationMaxChildInputTokens   = 12000
	DefaultDelegationMaxChildOutputTokens  = 1200
	DefaultDelegationMaxReportTokens       = 1000
	DefaultDelegationMaxTotalTokens        = 48000
	DefaultDelegationParentAnswerReserve   = 4000
)

var canonicalCapabilityID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// PlatformSettings holds runtime settings managed from the platform UI.
type PlatformSettings struct {
	VCSURL             string
	VCSToken           string
	VCSGroups          []string
	VCSWebhookSecret   string
	VCSConcurrency     int
	VCSExcludeProjects []string

	LLMBaseURL                           string
	LLMAPIKey                            string
	LLMModel                             string
	LLMFastModel                         string
	LLMProvider                          string // "openai" (default) | "anthropic"
	LLMMaxTokens                         int
	LLMAnswerMaxTokens                   int
	LLMConclusionMaxTokens               int
	LLMMaxContinueRounds                 int
	LLMContextWindow                     int
	LLMInputPriceMicrosPerMillionTokens  int64
	LLMOutputPriceMicrosPerMillionTokens int64

	RerankEnabled              bool
	RerankPool                 int
	RerankTopK                 int
	RerankMinScore             float64
	RerankMinDensePreflight    float64
	RunbookMinScore            float64
	CodeMinScore               float64
	ContextBudget              int
	RerankMaxPerService        int
	RerankMaxPerServiceLowBand int
	RerankProvider             string
	RerankAPIKey               string
	RerankModel                string
	RerankBaseURL              string
	AgentTimeout               Duration
	AgentMaxSteps              int
	AgentAnswerReserve         Duration
	RetrievalRouterConfidence  float64
	RetrievalRouterMaxTokens   int
	ToolPruningEnabled         bool

	DelegationEnabled                   bool
	DelegationShadowEnabled             bool
	DelegationCapabilities              []string
	DelegationMaxChildren               int
	DelegationMaxConcurrent             int
	DelegationWorkflowEscalationEnabled bool
	DelegationChildTimeout              Duration
	DelegationMaxChildTurns             int
	DelegationMaxChildToolCalls         int64
	DelegationMaxChildInputTokens       int64
	DelegationMaxChildOutputTokens      int64
	DelegationMaxReportTokens           int64
	DelegationMaxTotalTokens            int64
	DelegationMaxTotalCostMicros        int64
	DelegationParentAnswerReserve       int64

	CodingEnabledProviders   []string
	CodingDefaultProvider    string
	CodingCodexModel         string
	CodingClaudeModel        string
	FeatureGenerationTimeout Duration
	CodingTimeout            Duration
	CodingMaxConcurrency     int
	CodingAllowNetwork       bool
	CodingWorktreeTTL        Duration

	DomainKnowledge string
}

var platformSettingKeys = map[string]bool{
	"llm_model": true, "llm_fast_model": true,
	"llm_base_url": true, "llm_provider": true, "llm_max_tokens": true, "llm_api_key": true,
	"llm_answer_max_tokens": true, "agent_conclusion_max_tokens": true,
	"llm_max_continue_rounds": true, "llm_context_window": false, "agent_answer_reserve": true,
	"llm_input_price_micros_per_million_tokens":  true,
	"llm_output_price_micros_per_million_tokens": true,
	"agent_timeout": true, "agent_max_steps": true, "context_budget": false, "domain_knowledge": true,
	"retrieval_router_direct_min_confidence": false, "retrieval_router_max_tokens": false,
	"tool_pruning_enabled": false,
	"delegation_enabled":   true, "delegation_shadow_enabled": true,
	"delegation_capabilities": true, "delegation_max_children": true,
	"delegation_max_concurrent": true, "delegation_workflow_escalation_enabled": true,
	"delegation_child_timeout": true, "delegation_max_child_turns": true,
	"delegation_max_child_tool_calls": true, "delegation_max_child_input_tokens": true,
	"delegation_max_child_output_tokens": true, "delegation_max_report_tokens": true,
	"delegation_max_total_tokens": true, "delegation_max_total_cost_micros": true,
	"delegation_parent_answer_reserve": true,
	"rerank_enabled":                   false, "rerank_pool": false, "rerank_topk": false,
	"rerank_min_score": false, "rerank_min_dense_preflight": false,
	"runbook_min_score": false, "code_min_score": false,
	"rerank_max_per_service": false, "rerank_max_per_service_low_band": false,
	"rerank_provider": false,
	"rerank_api_key":  false, "rerank_model": false, "rerank_base_url": false,
	"vcs_url": true, "vcs_token": true, "vcs_groups": true, "vcs_webhook_secret": true,
	"vcs_clone_concurrency": true, "vcs_exclude_projects": true,
	"coding_enabled_providers": true, "coding_default_provider": true,
	"coding_codex_model": true, "coding_claude_model": true,
	"feature_generation_timeout": true, "coding_timeout": true,
	"coding_max_concurrency": true, "coding_allow_network": true,
	"coding_worktree_ttl": true,
}

// IsPlatformSetting reports whether key belongs to the persisted settings contract.
func IsPlatformSetting(key string) bool {
	_, ok := platformSettingKeys[key]
	return ok
}

// MergeStoredPlatformValues preserves the existing REST response types for
// settings historically returned as their stored string representation.
func MergeStoredPlatformValues(dst map[string]any, stored map[string]string) {
	for key, rawResponse := range platformSettingKeys {
		if !rawResponse {
			continue
		}
		if value := strings.TrimSpace(stored[key]); value != "" {
			dst[key] = value
		}
	}
}

// Values returns the API representation using the persisted setting keys.
func (p *PlatformSettings) Values() map[string]any {
	return map[string]any{
		"llm_model":                                  p.LLMModel,
		"llm_fast_model":                             p.LLMFastModel,
		"llm_base_url":                               p.LLMBaseURL,
		"llm_provider":                               p.LLMProvider,
		"llm_max_tokens":                             p.LLMMaxTokens,
		"llm_api_key":                                p.LLMAPIKey,
		"llm_answer_max_tokens":                      p.LLMAnswerMaxTokens,
		"agent_conclusion_max_tokens":                p.LLMConclusionMaxTokens,
		"llm_max_continue_rounds":                    strconv.Itoa(p.LLMMaxContinueRounds),
		"llm_context_window":                         p.LLMContextWindow,
		"llm_input_price_micros_per_million_tokens":  strconv.FormatInt(p.LLMInputPriceMicrosPerMillionTokens, 10),
		"llm_output_price_micros_per_million_tokens": strconv.FormatInt(p.LLMOutputPriceMicrosPerMillionTokens, 10),
		"agent_timeout":                              time.Duration(p.AgentTimeout).String(),
		"agent_answer_reserve":                       time.Duration(p.AgentAnswerReserve).String(),
		"agent_max_steps":                            p.AgentMaxSteps,
		"retrieval_router_direct_min_confidence":     p.routerConfidence(),
		"retrieval_router_max_tokens":                p.routerMaxTokens(),
		"tool_pruning_enabled":                       p.ToolPruningEnabled,
		"delegation_enabled":                         strconv.FormatBool(p.DelegationEnabled),
		"delegation_shadow_enabled":                  strconv.FormatBool(p.DelegationShadowEnabled),
		"delegation_capabilities":                    strings.Join(p.DelegationCapabilities, ","),
		"delegation_max_children":                    strconv.Itoa(p.DelegationMaxChildren),
		"delegation_max_concurrent":                  strconv.Itoa(p.DelegationMaxConcurrent),
		"delegation_workflow_escalation_enabled":     strconv.FormatBool(p.DelegationWorkflowEscalationEnabled),
		"delegation_child_timeout":                   time.Duration(p.DelegationChildTimeout).String(),
		"delegation_max_child_turns":                 strconv.Itoa(p.DelegationMaxChildTurns),
		"delegation_max_child_tool_calls":            strconv.FormatInt(p.DelegationMaxChildToolCalls, 10),
		"delegation_max_child_input_tokens":          strconv.FormatInt(p.DelegationMaxChildInputTokens, 10),
		"delegation_max_child_output_tokens":         strconv.FormatInt(p.DelegationMaxChildOutputTokens, 10),
		"delegation_max_report_tokens":               strconv.FormatInt(p.DelegationMaxReportTokens, 10),
		"delegation_max_total_tokens":                strconv.FormatInt(p.DelegationMaxTotalTokens, 10),
		"delegation_max_total_cost_micros":           strconv.FormatInt(p.DelegationMaxTotalCostMicros, 10),
		"delegation_parent_answer_reserve":           strconv.FormatInt(p.DelegationParentAnswerReserve, 10),
		"context_budget":                             p.ContextBudget,
		"domain_knowledge":                           p.DomainKnowledge,
		"rerank_enabled":                             p.RerankEnabled,
		"rerank_pool":                                p.RerankPool,
		"rerank_topk":                                p.RerankTopK,
		"rerank_min_score":                           p.RerankMinScore,
		"rerank_min_dense_preflight":                 p.RerankMinDensePreflight,
		"runbook_min_score":                          p.RunbookMinScore,
		"code_min_score":                             p.CodeMinScore,
		"rerank_max_per_service":                     p.RerankMaxPerService,
		"rerank_max_per_service_low_band":            p.RerankMaxPerServiceLowBand,
		"rerank_provider":                            p.RerankProvider,
		"rerank_api_key":                             p.RerankAPIKey,
		"rerank_model":                               p.RerankModel,
		"rerank_base_url":                            p.RerankBaseURL,
		"vcs_url":                                    p.VCSURL,
		"vcs_token":                                  p.VCSToken,
		"vcs_groups":                                 strings.Join(p.VCSGroups, "\n"),
		"vcs_webhook_secret":                         p.VCSWebhookSecret,
		"vcs_clone_concurrency":                      strconv.Itoa(p.VCSConcurrency),
		"vcs_exclude_projects":                       strings.Join(p.VCSExcludeProjects, "\n"),
		"coding_enabled_providers":                   strings.Join(p.CodingEnabledProviders, ","),
		"coding_default_provider":                    p.CodingDefaultProvider,
		"coding_codex_model":                         p.CodingCodexModel,
		"coding_claude_model":                        p.CodingClaudeModel,
		"feature_generation_timeout":                 time.Duration(p.FeatureGenerationTimeout).String(),
		"coding_timeout":                             time.Duration(p.CodingTimeout).String(),
		"coding_max_concurrency":                     strconv.Itoa(p.CodingMaxConcurrency),
		"coding_allow_network":                       strconv.FormatBool(p.CodingAllowNetwork),
		"coding_worktree_ttl":                        time.Duration(p.CodingWorktreeTTL).String(),
	}
}

// Apply decodes persisted platform settings into runtime configuration.
// Missing values receive the canonical defaults owned by this boundary.
// Downstream consumers can rely on the resulting normalized representation.
func (p *PlatformSettings) Apply(m map[string]string) {
	if p.LLMContextWindow == 0 {
		p.LLMContextWindow = DefaultLLMContextWindow
	}
	if p.RetrievalRouterConfidence == 0 {
		p.RetrievalRouterConfidence = DefaultRetrievalRouterDirectConfidence
	}
	if p.RetrievalRouterMaxTokens == 0 {
		p.RetrievalRouterMaxTokens = DefaultRetrievalRouterMaxTokens
	}
	if p.FeatureGenerationTimeout <= 0 {
		p.FeatureGenerationTimeout = Duration(DefaultFeatureGenerationTimeout)
	}
	if p.CodingTimeout <= 0 {
		p.CodingTimeout = Duration(DefaultCodingTimeout)
	}
	if p.CodingMaxConcurrency <= 0 {
		p.CodingMaxConcurrency = DefaultCodingMaxConcurrency
	}
	if p.CodingWorktreeTTL <= 0 {
		p.CodingWorktreeTTL = Duration(DefaultCodingWorktreeTTL)
	}
	if p.DelegationMaxChildren <= 0 {
		p.DelegationMaxChildren = DefaultDelegationMaxChildren
	}
	if p.DelegationMaxConcurrent <= 0 {
		p.DelegationMaxConcurrent = DefaultDelegationMaxConcurrent
	}
	if p.DelegationChildTimeout <= 0 {
		p.DelegationChildTimeout = Duration(DefaultDelegationChildTimeout)
	}
	if p.DelegationMaxChildTurns <= 0 {
		p.DelegationMaxChildTurns = DefaultDelegationMaxChildTurns
	}
	if p.DelegationMaxChildToolCalls <= 0 {
		p.DelegationMaxChildToolCalls = DefaultDelegationMaxChildToolCalls
	}
	if p.DelegationMaxChildInputTokens <= 0 {
		p.DelegationMaxChildInputTokens = DefaultDelegationMaxChildInputTokens
	}
	if p.DelegationMaxChildOutputTokens <= 0 {
		p.DelegationMaxChildOutputTokens = DefaultDelegationMaxChildOutputTokens
	}
	if p.DelegationMaxReportTokens <= 0 {
		p.DelegationMaxReportTokens = DefaultDelegationMaxReportTokens
	}
	if p.DelegationMaxTotalTokens <= 0 {
		p.DelegationMaxTotalTokens = DefaultDelegationMaxTotalTokens
	}
	if p.DelegationParentAnswerReserve <= 0 {
		p.DelegationParentAnswerReserve = DefaultDelegationParentAnswerReserve
	}
	p.ToolPruningEnabled = false // default off; dry-run measurement logs what pruning would save
	p.DelegationEnabled = false
	p.DelegationShadowEnabled = false
	p.DelegationWorkflowEscalationEnabled = false
	if v := strings.TrimSpace(m["llm_model"]); v != "" {
		p.LLMModel = v
	}
	if v := strings.TrimSpace(m["llm_fast_model"]); v != "" {
		p.LLMFastModel = v
	}
	if v := strings.TrimSpace(m["llm_provider"]); v != "" {
		p.LLMProvider = v
	}
	if v := strings.TrimSpace(m["llm_base_url"]); v != "" {
		p.LLMBaseURL = v
	}
	if v := strings.TrimSpace(m["llm_max_tokens"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.LLMMaxTokens = n
		}
	}
	if v := strings.TrimSpace(m["llm_api_key"]); v != "" {
		p.LLMAPIKey = v
	}
	if v := strings.TrimSpace(m["llm_answer_max_tokens"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.LLMAnswerMaxTokens = n
		}
	}
	if v := strings.TrimSpace(m["agent_conclusion_max_tokens"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.LLMConclusionMaxTokens = n
		}
	}
	if v := strings.TrimSpace(m["llm_max_continue_rounds"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.LLMMaxContinueRounds = n
		}
	}
	if v := strings.TrimSpace(m["llm_context_window"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.LLMContextWindow = n
		}
	}
	if v := strings.TrimSpace(m["llm_input_price_micros_per_million_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.LLMInputPriceMicrosPerMillionTokens = n
		}
	}
	if v := strings.TrimSpace(m["llm_output_price_micros_per_million_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			p.LLMOutputPriceMicrosPerMillionTokens = n
		}
	}

	if v := strings.TrimSpace(m["rerank_enabled"]); v != "" {
		p.RerankEnabled = v == "1" || v == "true"
	}
	if v := strings.TrimSpace(m["tool_pruning_enabled"]); v != "" {
		p.ToolPruningEnabled = v == "1" || v == "true"
	}
	if v := strings.TrimSpace(m["rerank_pool"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.RerankPool = n
		}
	}
	if v := strings.TrimSpace(m["rerank_topk"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.RerankTopK = n
		}
	}
	if v := strings.TrimSpace(m["rerank_min_score"]); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.RerankMinScore = f
		}
	}
	if v := strings.TrimSpace(m["rerank_min_dense_preflight"]); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.RerankMinDensePreflight = f
		}
	}
	if v := strings.TrimSpace(m["runbook_min_score"]); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.RunbookMinScore = f
		}
	}
	if v := strings.TrimSpace(m["code_min_score"]); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			p.CodeMinScore = f
		}
	}
	if v := strings.TrimSpace(m["context_budget"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.ContextBudget = n
		}
	}
	if v := strings.TrimSpace(m["rerank_max_per_service"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.RerankMaxPerService = n
		}
	}
	if v := strings.TrimSpace(m["rerank_max_per_service_low_band"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.RerankMaxPerServiceLowBand = n
		}
	}
	if v := strings.TrimSpace(m["rerank_provider"]); v != "" {
		p.RerankProvider = v
	}
	if v := strings.TrimSpace(m["rerank_api_key"]); v != "" {
		p.RerankAPIKey = v
	}
	if v := strings.TrimSpace(m["rerank_model"]); v != "" {
		p.RerankModel = v
	}
	if v := strings.TrimSpace(m["rerank_base_url"]); v != "" {
		p.RerankBaseURL = v
	}
	if v := strings.TrimSpace(m["domain_knowledge"]); v != "" {
		p.DomainKnowledge = v
	}
	if v := strings.TrimSpace(m["agent_timeout"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.AgentTimeout = Duration(d)
		}
	}
	if v := strings.TrimSpace(m["agent_answer_reserve"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			p.AgentAnswerReserve = Duration(d)
		}
	}
	if p.AgentAnswerReserve <= 0 {
		p.AgentAnswerReserve = Duration(DefaultAgentAnswerReserve)
	}
	if v := strings.TrimSpace(m["agent_max_steps"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.AgentMaxSteps = n
		}
	}
	if v := strings.TrimSpace(m["retrieval_router_direct_min_confidence"]); v != "" {
		if confidence, err := strconv.ParseFloat(v, 64); err == nil {
			p.RetrievalRouterConfidence = confidence
		}
	}
	if v := strings.TrimSpace(m["retrieval_router_max_tokens"]); v != "" {
		if tokens, err := strconv.Atoi(v); err == nil {
			p.RetrievalRouterMaxTokens = tokens
		}
	}
	if raw, ok := m["delegation_enabled"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		p.DelegationEnabled = value == "1" || value == "true"
	}
	if raw, ok := m["delegation_shadow_enabled"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		p.DelegationShadowEnabled = value == "1" || value == "true"
	}
	if raw, ok := m["delegation_workflow_escalation_enabled"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		p.DelegationWorkflowEscalationEnabled = value == "1" || value == "true"
	}
	if raw, ok := m["delegation_capabilities"]; ok {
		if value, err := canonicalCapabilityList(raw); err == nil {
			p.DelegationCapabilities = splitList(value)
		}
	}
	if v := strings.TrimSpace(m["delegation_max_children"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.DelegationMaxChildren = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_concurrent"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.DelegationMaxConcurrent = n
		}
	}
	if v := strings.TrimSpace(m["delegation_child_timeout"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.DelegationChildTimeout = Duration(d)
		}
	}
	if v := strings.TrimSpace(m["delegation_max_child_turns"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.DelegationMaxChildTurns = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_child_tool_calls"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.DelegationMaxChildToolCalls = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_child_input_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.DelegationMaxChildInputTokens = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_child_output_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.DelegationMaxChildOutputTokens = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_report_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.DelegationMaxReportTokens = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_total_tokens"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			p.DelegationMaxTotalTokens = n
		}
	}
	if v := strings.TrimSpace(m["delegation_max_total_cost_micros"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			p.DelegationMaxTotalCostMicros = n
		}
	}
	if v := strings.TrimSpace(m["delegation_parent_answer_reserve"]); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			p.DelegationParentAnswerReserve = n
		}
	}
	if v := strings.TrimSpace(m["vcs_url"]); v != "" {
		p.VCSURL = v
	}
	if v := strings.TrimSpace(m["vcs_token"]); v != "" {
		p.VCSToken = v
	}
	if v := strings.TrimSpace(m["vcs_groups"]); v != "" {
		p.VCSGroups = ParseExcludeList(v)
	}
	if v := strings.TrimSpace(m["vcs_webhook_secret"]); v != "" {
		p.VCSWebhookSecret = v
	}
	if v := strings.TrimSpace(m["vcs_clone_concurrency"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.VCSConcurrency = n
		}
	}
	if v := strings.TrimSpace(m["vcs_exclude_projects"]); v != "" {
		p.VCSExcludeProjects = ParseExcludeList(v)
	}
	if raw, ok := m["coding_enabled_providers"]; ok {
		if value, err := canonicalCodingProviders(raw); err == nil {
			p.CodingEnabledProviders = splitList(value)
		}
	}
	if raw, ok := m["coding_default_provider"]; ok {
		p.CodingDefaultProvider = strings.ToLower(strings.TrimSpace(raw))
	}
	if raw, ok := m["coding_codex_model"]; ok {
		p.CodingCodexModel = strings.TrimSpace(raw)
	}
	if raw, ok := m["coding_claude_model"]; ok {
		p.CodingClaudeModel = strings.TrimSpace(raw)
	}
	if v := strings.TrimSpace(m["feature_generation_timeout"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.FeatureGenerationTimeout = Duration(d)
		}
	}
	if v := strings.TrimSpace(m["coding_timeout"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.CodingTimeout = Duration(d)
		}
	}
	if v := strings.TrimSpace(m["coding_max_concurrency"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.CodingMaxConcurrency = n
		}
	}
	if raw, ok := m["coding_allow_network"]; ok {
		value := strings.ToLower(strings.TrimSpace(raw))
		p.CodingAllowNetwork = value == "1" || value == "true"
	}
	if v := strings.TrimSpace(m["coding_worktree_ttl"]); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.CodingWorktreeTTL = Duration(d)
		}
	}
}

func (p *PlatformSettings) routerConfidence() float64 {
	if p.RetrievalRouterConfidence == 0 {
		return DefaultRetrievalRouterDirectConfidence
	}
	return p.RetrievalRouterConfidence
}

func (p *PlatformSettings) routerMaxTokens() int {
	if p.RetrievalRouterMaxTokens == 0 {
		return DefaultRetrievalRouterMaxTokens
	}
	return p.RetrievalRouterMaxTokens
}

func CanonicalPlatformSetting(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !IsPlatformSetting(key) {
		return "", fmt.Errorf("unknown platform setting %q", key)
	}
	if value == "" {
		return "", nil
	}
	switch key {
	case "llm_provider":
		value = strings.ToLower(value)
		if value != "" && value != "openai" && value != "anthropic" {
			return "", fmt.Errorf("llm_provider must be empty, openai, or anthropic")
		}
		return value, nil
	case "rerank_enabled", "coding_allow_network", "tool_pruning_enabled",
		"delegation_enabled", "delegation_shadow_enabled",
		"delegation_workflow_escalation_enabled":
		return canonicalBoolSetting(key, value)
	case "llm_max_tokens", "llm_answer_max_tokens", "agent_conclusion_max_tokens", "llm_max_continue_rounds":
		return canonicalNonNegativeIntSetting(key, value)
	case "llm_input_price_micros_per_million_tokens", "llm_output_price_micros_per_million_tokens":
		return canonicalNonNegativeInt64Setting(key, value)
	case "agent_max_steps", "context_budget", "rerank_pool",
		"rerank_topk", "rerank_max_per_service", "rerank_max_per_service_low_band",
		"vcs_clone_concurrency", "delegation_max_children",
		"delegation_max_concurrent", "delegation_max_child_turns":
		return canonicalPositiveIntSetting(key, value)
	case "delegation_max_child_tool_calls", "delegation_max_child_input_tokens",
		"delegation_max_child_output_tokens", "delegation_max_report_tokens",
		"delegation_max_total_tokens":
		return canonicalPositiveInt64Setting(key, value)
	case "delegation_max_total_cost_micros", "delegation_parent_answer_reserve":
		return canonicalNonNegativeInt64Setting(key, value)
	case "rerank_min_score", "rerank_min_dense_preflight", "runbook_min_score", "code_min_score":
		return canonicalScoreSetting(key, value)
	case "agent_timeout":
		return canonicalDurationSetting(key, value, time.Second, 24*time.Hour)
	case "delegation_child_timeout":
		return canonicalDurationSetting(key, value, time.Second, 24*time.Hour)
	case "delegation_capabilities":
		return canonicalCapabilityList(value)
	case "coding_enabled_providers":
		return canonicalCodingProviders(value)
	case "coding_default_provider":
		value = strings.ToLower(value)
		if value != "" && value != "codex" && value != "claude" {
			return "", fmt.Errorf("coding_default_provider must be empty, codex, or claude")
		}
		return value, nil
	case "feature_generation_timeout":
		return canonicalDurationSetting(key, value, time.Second, time.Hour)
	case "coding_timeout":
		return canonicalDurationSetting(key, value, time.Minute, 12*time.Hour)
	case "coding_worktree_ttl":
		return canonicalDurationSetting(key, value, time.Hour, 30*24*time.Hour)
	case "coding_max_concurrency":
		concurrency, err := strconv.Atoi(value)
		if err != nil || concurrency < 1 || concurrency > 32 {
			return "", fmt.Errorf("coding_max_concurrency must be between 1 and 32")
		}
		return strconv.Itoa(concurrency), nil
	case "agent_answer_reserve":
		reserve, err := time.ParseDuration(value)
		if err != nil || reserve <= 0 {
			return "", fmt.Errorf("agent_answer_reserve must be a positive duration")
		}
		return reserve.String(), nil
	case "retrieval_router_direct_min_confidence":
		confidence, err := strconv.ParseFloat(value, 64)
		if err != nil || confidence <= 0 || confidence > 1 {
			return "", fmt.Errorf("retrieval_router_direct_min_confidence must be greater than 0 and at most 1")
		}
		return strconv.FormatFloat(confidence, 'f', -1, 64), nil
	case "retrieval_router_max_tokens":
		tokens, err := strconv.Atoi(value)
		if err != nil || tokens < 128 || tokens > 4096 {
			return "", fmt.Errorf("retrieval_router_max_tokens must be between 128 and 4096")
		}
		return strconv.Itoa(tokens), nil
	case "llm_context_window":
		tokens, err := strconv.Atoi(value)
		if err != nil || tokens < 8192 || tokens > 2_000_000 {
			return "", fmt.Errorf("llm_context_window must be between 8192 and 2000000")
		}
		return strconv.Itoa(tokens), nil
	case "vcs_groups", "vcs_exclude_projects":
		return strings.Join(ParseExcludeList(value), "\n"), nil
	default:
		return value, nil
	}
}

func canonicalBoolSetting(key, value string) (string, error) {
	switch strings.ToLower(value) {
	case "1", "true":
		return "true", nil
	case "0", "false":
		return "false", nil
	default:
		return "", fmt.Errorf("%s must be true or false", key)
	}
}

func canonicalPositiveIntSetting(key, value string) (string, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%s must be a positive integer", key)
	}
	return strconv.Itoa(n), nil
}

func canonicalNonNegativeIntSetting(key, value string) (string, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return "", fmt.Errorf("%s must be a non-negative integer", key)
	}
	return strconv.Itoa(n), nil
}

func canonicalNonNegativeInt64Setting(key, value string) (string, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return "", fmt.Errorf("%s must be a non-negative integer", key)
	}
	return strconv.FormatInt(n, 10), nil
}

func canonicalPositiveInt64Setting(key, value string) (string, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%s must be a positive integer", key)
	}
	return strconv.FormatInt(n, 10), nil
}

func canonicalScoreSetting(key, value string) (string, error) {
	score, err := strconv.ParseFloat(value, 64)
	if err != nil || score < 0 || score > 1 {
		return "", fmt.Errorf("%s must be between 0 and 1", key)
	}
	return strconv.FormatFloat(score, 'f', -1, 64), nil
}

func canonicalCodingProviders(value string) (string, error) {
	requested := make(map[string]struct{}, 2)
	for _, provider := range splitList(strings.ToLower(value)) {
		switch provider {
		case "codex", "claude":
			requested[provider] = struct{}{}
		default:
			return "", fmt.Errorf("unsupported coding provider %q", provider)
		}
	}
	ordered := make([]string, 0, len(requested))
	for _, provider := range []string{"codex", "claude"} {
		if _, ok := requested[provider]; ok {
			ordered = append(ordered, provider)
		}
	}
	return strings.Join(ordered, ","), nil
}

func canonicalCapabilityList(value string) (string, error) {
	requested := make(map[string]struct{})
	for _, capability := range splitList(strings.ToLower(value)) {
		if !canonicalCapabilityID.MatchString(capability) {
			return "", fmt.Errorf("delegation capability %q is not canonical", capability)
		}
		requested[capability] = struct{}{}
	}
	capabilities := make([]string, 0, len(requested))
	for capability := range requested {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	return strings.Join(capabilities, ","), nil
}

func canonicalDurationSetting(key, value string, min, max time.Duration) (string, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < min || duration > max {
		return "", fmt.Errorf("%s must be between %s and %s", key, min, max)
	}
	return duration.String(), nil
}

// ValidateCodingSettings checks relationships that cannot be validated one key at a time.
func (p *PlatformSettings) ValidateCodingSettings() error {
	enabled := make(map[string]struct{}, len(p.CodingEnabledProviders))
	for _, provider := range p.CodingEnabledProviders {
		enabled[provider] = struct{}{}
	}
	if p.CodingDefaultProvider != "" {
		if _, ok := enabled[p.CodingDefaultProvider]; !ok {
			return fmt.Errorf("coding_default_provider %q is not enabled", p.CodingDefaultProvider)
		}
	}
	return nil
}

// ValidateAgentSettings checks relationships between independently stored limits.
func (p *PlatformSettings) ValidateAgentSettings() error {
	timeout := time.Duration(p.AgentTimeout)
	reserve := time.Duration(p.AgentAnswerReserve)
	if timeout <= 0 {
		return fmt.Errorf("agent_timeout must be positive")
	}
	if reserve <= 0 {
		return fmt.Errorf("agent_answer_reserve must be positive")
	}
	if reserve >= timeout {
		return fmt.Errorf("agent_answer_reserve must be less than agent_timeout")
	}
	if p.LLMContextWindow > 0 && p.ContextBudget >= p.LLMContextWindow {
		return fmt.Errorf("context_budget must be less than llm_context_window")
	}
	if p.LLMInputPriceMicrosPerMillionTokens < 0 ||
		p.LLMOutputPriceMicrosPerMillionTokens < 0 {
		return fmt.Errorf("LLM model prices cannot be negative")
	}
	if (p.LLMInputPriceMicrosPerMillionTokens == 0) !=
		(p.LLMOutputPriceMicrosPerMillionTokens == 0) {
		return fmt.Errorf("LLM input and output model prices must be configured together")
	}
	if p.DelegationShadowEnabled && !p.DelegationEnabled {
		return fmt.Errorf("delegation_shadow_enabled requires delegation_enabled")
	}
	if p.DelegationWorkflowEscalationEnabled && !p.DelegationEnabled {
		return fmt.Errorf("delegation_workflow_escalation_enabled requires delegation_enabled")
	}
	if !p.DelegationEnabled {
		return nil
	}
	if p.DelegationMaxConcurrent > p.DelegationMaxChildren {
		return fmt.Errorf("delegation_max_concurrent must not exceed delegation_max_children")
	}
	if time.Duration(p.DelegationChildTimeout) <= reserve {
		return fmt.Errorf("delegation_child_timeout must exceed agent_answer_reserve")
	}
	childTokens := p.DelegationMaxChildInputTokens + p.DelegationMaxChildOutputTokens
	if childTokens < p.DelegationMaxChildInputTokens {
		return fmt.Errorf("delegation child token limit overflow")
	}
	if childTokens > 0 && int64(p.DelegationMaxChildren) >
		(math.MaxInt64-p.DelegationParentAnswerReserve)/childTokens {
		return fmt.Errorf("delegation aggregate token limit overflow")
	}
	requiredTokens := int64(p.DelegationMaxChildren)*childTokens +
		p.DelegationParentAnswerReserve
	if p.DelegationMaxTotalTokens < requiredTokens {
		return fmt.Errorf(
			"delegation_max_total_tokens must cover all child grants and parent answer reserve",
		)
	}
	return nil
}

// VCSEnabled reports whether VCS syncing is configured.
func (p *PlatformSettings) VCSEnabled() bool {
	return p.VCSURL != "" && p.VCSToken != "" && len(p.VCSGroups) > 0
}

// LLMEnabled reports whether the main LLM endpoint is configured.
func (p *PlatformSettings) LLMEnabled() bool {
	return p.LLMBaseURL != "" && p.LLMAPIKey != "" && p.LLMModel != ""
}

// FastLLMConfigured reports whether the optional fast model can reuse the main endpoint.
func (p *PlatformSettings) FastLLMConfigured() bool {
	return p.LLMFastModel != "" && p.LLMBaseURL != "" && p.LLMAPIKey != ""
}

// LoadMySQLDSN reads the bootstrap MySQL DSN from the environment.
func LoadMySQLDSN() string {
	return strings.TrimSpace(os.Getenv("MYSQL_DSN"))
}
