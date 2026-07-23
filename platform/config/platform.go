package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRetrievalRouterDirectConfidence = 0.90
	DefaultRetrievalRouterMaxTokens        = 512
)

// PlatformSettings holds runtime settings managed from the platform UI.
type PlatformSettings struct {
	VCSURL             string
	VCSToken           string
	VCSGroups          []string
	VCSWebhookSecret   string
	VCSConcurrency     int
	VCSExcludeProjects []string

	LLMBaseURL             string
	LLMAPIKey              string
	LLMModel               string
	LLMFastModel           string
	LLMProvider            string // "openai" (default) | "anthropic"
	LLMMaxTokens           int
	LLMAnswerMaxTokens     int
	LLMConclusionMaxTokens int
	LLMMaxContinueRounds   int

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

	DomainKnowledge string
}

var platformSettingKeys = map[string]bool{
	"llm_model": true, "llm_fast_model": true,
	"llm_base_url": true, "llm_provider": true, "llm_max_tokens": true, "llm_api_key": true,
	"llm_answer_max_tokens": true, "agent_conclusion_max_tokens": true,
	"llm_max_continue_rounds": true, "agent_answer_reserve": true,
	"agent_timeout": true, "agent_max_steps": true, "context_budget": false, "domain_knowledge": true,
	"retrieval_router_direct_min_confidence": false, "retrieval_router_max_tokens": false,
	"rerank_enabled": false, "rerank_pool": false, "rerank_topk": false,
	"rerank_min_score": false, "rerank_min_dense_preflight": false,
	"runbook_min_score": false, "code_min_score": false,
	"rerank_max_per_service": false, "rerank_max_per_service_low_band": false,
	"rerank_provider": false,
	"rerank_api_key":  false, "rerank_model": false, "rerank_base_url": false,
	"vcs_url": true, "vcs_token": true, "vcs_groups": true, "vcs_webhook_secret": true,
	"vcs_clone_concurrency": true, "vcs_exclude_projects": true,
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
		"llm_model":                              p.LLMModel,
		"llm_fast_model":                         p.LLMFastModel,
		"llm_base_url":                           p.LLMBaseURL,
		"llm_provider":                           p.LLMProvider,
		"llm_max_tokens":                         p.LLMMaxTokens,
		"llm_api_key":                            p.LLMAPIKey,
		"llm_answer_max_tokens":                  p.LLMAnswerMaxTokens,
		"agent_conclusion_max_tokens":            p.LLMConclusionMaxTokens,
		"llm_max_continue_rounds":                strconv.Itoa(p.LLMMaxContinueRounds),
		"agent_timeout":                          time.Duration(p.AgentTimeout).String(),
		"agent_answer_reserve":                   time.Duration(p.AgentAnswerReserve).String(),
		"agent_max_steps":                        p.AgentMaxSteps,
		"retrieval_router_direct_min_confidence": p.routerConfidence(),
		"retrieval_router_max_tokens":            p.routerMaxTokens(),
		"context_budget":                         p.ContextBudget,
		"domain_knowledge":                       p.DomainKnowledge,
		"rerank_enabled":                         p.RerankEnabled,
		"rerank_pool":                            p.RerankPool,
		"rerank_topk":                            p.RerankTopK,
		"rerank_min_score":                       p.RerankMinScore,
		"rerank_min_dense_preflight":             p.RerankMinDensePreflight,
		"runbook_min_score":                      p.RunbookMinScore,
		"code_min_score":                         p.CodeMinScore,
		"rerank_max_per_service":                 p.RerankMaxPerService,
		"rerank_max_per_service_low_band":        p.RerankMaxPerServiceLowBand,
		"rerank_provider":                        p.RerankProvider,
		"rerank_api_key":                         p.RerankAPIKey,
		"rerank_model":                           p.RerankModel,
		"rerank_base_url":                        p.RerankBaseURL,
		"vcs_url":                                p.VCSURL,
		"vcs_token":                              p.VCSToken,
		"vcs_groups":                             strings.Join(p.VCSGroups, "\n"),
		"vcs_webhook_secret":                     p.VCSWebhookSecret,
		"vcs_clone_concurrency":                  strconv.Itoa(p.VCSConcurrency),
		"vcs_exclude_projects":                   strings.Join(p.VCSExcludeProjects, "\n"),
	}
}

func (p *PlatformSettings) Apply(m map[string]string) {
	if p.RetrievalRouterConfidence == 0 {
		p.RetrievalRouterConfidence = DefaultRetrievalRouterDirectConfidence
	}
	if p.RetrievalRouterMaxTokens == 0 {
		p.RetrievalRouterMaxTokens = DefaultRetrievalRouterMaxTokens
	}
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

	if v := strings.TrimSpace(m["rerank_enabled"]); v != "" {
		p.RerankEnabled = v == "1" || v == "true"
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
	switch key {
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
	default:
		return value, nil
	}
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
