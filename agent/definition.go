package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var canonicalID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// Definition is one immutable, versioned executable agent contract.
type Definition struct {
	ID            string           `json:"id"`
	Version       int64            `json:"version"`
	DisplayName   string           `json:"display_name"`
	Purpose       string           `json:"purpose"`
	Prompt        PromptSpec       `json:"prompt"`
	InputSchema   SchemaRef        `json:"input_schema"`
	OutputSchema  SchemaRef        `json:"output_schema"`
	Model         ModelPolicy      `json:"model"`
	Tools         ToolPolicy       `json:"tools"`
	Budget        BudgetPolicy     `json:"budget"`
	Permissions   PermissionPolicy `json:"permissions"`
	FailurePolicy FailurePolicy    `json:"failure_policy"`
	ContentHash   string           `json:"content_hash"`
}

type DefinitionRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version,omitempty"`
}

type PromptSpec struct {
	System  string `json:"system"`
	Version string `json:"version"`
}

type SchemaRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type ModelPolicy struct {
	Provider                          string         `json:"provider"`
	Model                             string         `json:"model"`
	MaxOutputTokens                   int            `json:"max_output_tokens"`
	InputPriceMicrosPerMillionTokens  int64          `json:"input_price_micros_per_million_tokens"`
	OutputPriceMicrosPerMillionTokens int64          `json:"output_price_micros_per_million_tokens"`
	Parameters                        map[string]any `json:"parameters,omitempty"`
}

type ToolPolicy struct {
	VisibleToolIDs  []string `json:"visible_tool_ids,omitempty"`
	RestrictVisible bool     `json:"restrict_visible"`
	AllowWrite      bool     `json:"allow_write"`
}

type BudgetPolicy struct {
	Timeout            time.Duration `json:"timeout"`
	MaxSteps           int           `json:"max_steps"`
	MaxToolCalls       int64         `json:"max_tool_calls,omitempty"`
	ContextTokens      int           `json:"context_tokens"`
	MaxToolResultBytes int           `json:"max_tool_result_bytes,omitempty"`
	MaxContinueRounds  int           `json:"max_continue_rounds,omitempty"`
}

type PermissionPolicy struct {
	Scopes []string `json:"scopes,omitempty"`
}

type FailurePolicy struct {
	MaxInfrastructureRetries int `json:"max_infrastructure_retries"`
}

// Prepare validates and hashes a detached immutable copy.
func Prepare(definition Definition) (Definition, error) {
	prepared := cloneDefinition(definition)
	prepared.ID = strings.TrimSpace(prepared.ID)
	if err := validatePreparedDefinition(prepared); err != nil {
		return Definition{}, err
	}
	hash, err := definitionHash(prepared)
	if err != nil {
		return Definition{}, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return Definition{}, fmt.Errorf("agent definition %q content hash mismatch", prepared.ID)
	}
	prepared.ContentHash = hash
	return prepared, nil
}

func validatePreparedDefinition(prepared Definition) error {
	if !canonicalID.MatchString(prepared.ID) {
		return fmt.Errorf("agent definition id %q is not canonical", prepared.ID)
	}
	if prepared.Version <= 0 {
		return fmt.Errorf("agent definition %q version must be positive", prepared.ID)
	}
	if err := validatePreparedPrompt(prepared); err != nil {
		return err
	}
	if err := validatePreparedModel(prepared); err != nil {
		return err
	}
	if err := validatePreparedBudget(prepared); err != nil {
		return err
	}
	if prepared.FailurePolicy.MaxInfrastructureRetries < 0 {
		return fmt.Errorf("agent definition %q retries cannot be negative", prepared.ID)
	}
	if err := validateCanonicalList(prepared.ID, "tool", prepared.Tools.VisibleToolIDs); err != nil {
		return err
	}
	if err := validateCanonicalList(prepared.ID, "permission", prepared.Permissions.Scopes); err != nil {
		return err
	}
	return nil
}

func validatePreparedPrompt(prepared Definition) error {
	if strings.TrimSpace(prepared.Prompt.System) == "" {
		return fmt.Errorf("agent definition %q system prompt is required", prepared.ID)
	}
	if strings.TrimSpace(prepared.Prompt.Version) == "" {
		return fmt.Errorf("agent definition %q prompt version is required", prepared.ID)
	}
	if err := validateSchema(prepared.ID, "input", prepared.InputSchema); err != nil {
		return err
	}
	if err := validateSchema(prepared.ID, "output", prepared.OutputSchema); err != nil {
		return err
	}
	return nil
}

func validatePreparedModel(prepared Definition) error {
	if strings.TrimSpace(prepared.Model.Provider) == "" {
		return fmt.Errorf("agent definition %q model provider is required", prepared.ID)
	}
	if strings.TrimSpace(prepared.Model.Model) == "" {
		return fmt.Errorf("agent definition %q model is required", prepared.ID)
	}
	if prepared.Model.MaxOutputTokens <= 0 {
		return fmt.Errorf("agent definition %q max output tokens must be positive", prepared.ID)
	}
	if prepared.Model.InputPriceMicrosPerMillionTokens < 0 ||
		prepared.Model.OutputPriceMicrosPerMillionTokens < 0 {
		return fmt.Errorf("agent definition %q model prices cannot be negative", prepared.ID)
	}
	if (prepared.Model.InputPriceMicrosPerMillionTokens == 0) !=
		(prepared.Model.OutputPriceMicrosPerMillionTokens == 0) {
		return fmt.Errorf("agent definition %q model prices must be configured together", prepared.ID)
	}
	return nil
}

func validatePreparedBudget(prepared Definition) error {
	if prepared.Budget.Timeout <= 0 || prepared.Budget.MaxSteps <= 0 || prepared.Budget.ContextTokens <= 0 {
		return fmt.Errorf("agent definition %q budgets must be positive", prepared.ID)
	}
	if prepared.Budget.MaxToolCalls < 0 {
		return fmt.Errorf("agent definition %q max tool calls cannot be negative", prepared.ID)
	}
	if prepared.Budget.MaxToolResultBytes < 0 {
		return fmt.Errorf("agent definition %q max tool result bytes cannot be negative", prepared.ID)
	}
	if prepared.Budget.MaxContinueRounds < 0 {
		return fmt.Errorf("agent definition %q continuation rounds cannot be negative", prepared.ID)
	}
	return nil
}

func validateSchema(agentID, kind string, schema SchemaRef) error {
	if !canonicalID.MatchString(strings.TrimSpace(schema.ID)) {
		return fmt.Errorf("agent definition %q %s schema id %q is not canonical", agentID, kind, schema.ID)
	}
	if schema.Version <= 0 {
		return fmt.Errorf("agent definition %q %s schema version must be positive", agentID, kind)
	}
	return nil
}

func validateCanonicalList(agentID, kind string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !canonicalID.MatchString(value) {
			return fmt.Errorf("agent definition %q %s id %q is not canonical", agentID, kind, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("agent definition %q contains duplicate %s id %q", agentID, kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func definitionHash(definition Definition) (string, error) {
	definition.ContentHash = ""
	payload, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("marshal agent definition %q: %w", definition.ID, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func cloneDefinition(definition Definition) Definition {
	definition.Tools.VisibleToolIDs = append([]string(nil), definition.Tools.VisibleToolIDs...)
	definition.Permissions.Scopes = append([]string(nil), definition.Permissions.Scopes...)
	if definition.Model.Parameters != nil {
		parameters := make(map[string]any, len(definition.Model.Parameters))
		for key, value := range definition.Model.Parameters {
			parameters[key] = value
		}
		definition.Model.Parameters = parameters
	}
	return definition
}
