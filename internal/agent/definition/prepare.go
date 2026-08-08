package definition

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

func (runtime *DefinitionRuntime) prepare(request agentapi.RunRequest) (definitionExecution, error) {
	definition, modelParameters, err := runtime.resolveExecutionDefinition(request)
	if err != nil {
		return definitionExecution{}, err
	}
	policy, err := prepareExecutionPermissions(definition, request)
	if err != nil {
		return definitionExecution{}, err
	}
	if definition.Budget.Timeout <= runtime.settings.answerReserve {
		return definitionExecution{}, fmt.Errorf("definition timeout must exceed the answer reserve")
	}
	if err := validateDefinitionMessages(request.Messages); err != nil {
		return definitionExecution{}, err
	}
	contextHash, err := validateDefinitionContext(request.Context)
	if err != nil {
		return definitionExecution{}, err
	}
	tools, err := runtime.prepareExecutionTools(definition, request.ToolScope, policy)
	if err != nil {
		return definitionExecution{}, err
	}
	return definitionExecution{
		definition: definition,
		snapshot: agentapi.RunSnapshot{
			RunID: request.RunID, AgentID: definition.ID,
			DefinitionVersion: definition.Version, DefinitionHash: definition.ContentHash,
			Selection: request.Selection,
			Provider:  definition.Model.Provider, Model: definition.Model.Model,
			ModelParameters: modelParameters.Snapshot(),
			ToolSnapshotID:  tools.snapshot.ID(), VisibleToolIDs: tools.visibleIDs,
			InputSchemaVersion:  definition.InputSchema.Version,
			OutputSchemaVersion: definition.OutputSchema.Version,
			PromptHash:          hashString(definition.Prompt.System), ContextHash: contextHash,
			Budget: definition.Budget, Permissions: clonePermissions(request.Permissions),
			Actor: request.Actor, Correlation: request.Correlation, CreatedAt: time.Now().UTC(),
		},
		modelParameters: modelParameters,
		toolPolicy:      tools.policy,
		toolSnapshot:    tools.snapshot,
		offeredTools:    tools.offeredIDs,
		pruneApplied:    tools.pruneApplied,
	}, nil
}

func (runtime *DefinitionRuntime) resolveExecutionDefinition(
	request agentapi.RunRequest,
) (agentapi.Definition, llm.ModelParameters, error) {
	if runtime == nil || runtime.definitions == nil || runtime.registry == nil {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition runtime is unavailable")
	}
	if strings.TrimSpace(request.RunID) == "" {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("run_id is required")
	}
	if request.Agent.ID == "" || request.Agent.Version <= 0 {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("exact agent id and version are required")
	}
	if len(request.DefinitionHash) != sha256.Size*2 || !validHex(request.DefinitionHash) {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition_hash must be a SHA-256 hex digest")
	}
	definition, err := runtime.definitions.Resolve(request.Agent)
	if err != nil {
		return agentapi.Definition{}, llm.ModelParameters{}, err
	}
	if definition.ID != request.Agent.ID || definition.Version != request.Agent.Version {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition resolver returned an unpinned version")
	}
	if definition.ContentHash != request.DefinitionHash {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition hash does not match published version")
	}
	if err := runtime.schemas.Validate(definition.InputSchema, request.Input); err != nil {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition input: %w", err)
	}
	if _, err := runtime.schemas.Resolve(definition.OutputSchema); err != nil {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition output schema: %w", err)
	}
	if definition.Model.Provider != runtime.settings.provider ||
		definition.Model.Model != runtime.settings.model {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf(
			"definition model %s/%s does not match configured model %s/%s",
			definition.Model.Provider, definition.Model.Model,
			runtime.settings.provider, runtime.settings.model,
		)
	}
	modelParameters, err := llm.PrepareModelParameters(
		definition.Model.Provider, definition.Model.Parameters,
	)
	if err != nil {
		return agentapi.Definition{}, llm.ModelParameters{}, fmt.Errorf("definition model parameters: %w", err)
	}
	return definition, modelParameters, nil
}

func prepareExecutionPermissions(
	definition agentapi.Definition,
	request agentapi.RunRequest,
) (tool.Policy, error) {
	if request.ToolScope.AllowWrite && !definition.Tools.AllowWrite {
		return tool.Policy{}, fmt.Errorf("definition does not permit write tools")
	}
	definitionPermissions, err := permissionSet(definition.Permissions)
	if err != nil {
		return tool.Policy{}, fmt.Errorf("definition permissions: %w", err)
	}
	effectivePermissions, err := permissionSet(request.Permissions)
	if err != nil {
		return tool.Policy{}, fmt.Errorf("run permissions: %w", err)
	}
	for scope := range effectivePermissions {
		if _, allowed := definitionPermissions[scope]; !allowed {
			return tool.Policy{}, fmt.Errorf("run permission scope %q is outside the definition", scope)
		}
	}
	if request.ToolScope.AllowWrite {
		if _, allowed := effectivePermissions[knowledgeWriteScope]; !allowed {
			return tool.Policy{}, fmt.Errorf("write tools require %q permission", knowledgeWriteScope)
		}
	}
	_, allowRead := effectivePermissions[knowledgeReadScope]
	return tool.Policy{AllowRead: allowRead, AllowWrite: request.ToolScope.AllowWrite}, nil
}

func (runtime *DefinitionRuntime) prepareExecutionTools(
	definition agentapi.Definition,
	request agentapi.ToolScope,
	policy tool.Policy,
) (definitionToolSelection, error) {
	definitionToolIDs, err := canonicalToolIDSet(definition.Tools.VisibleToolIDs)
	if err != nil {
		return definitionToolSelection{}, fmt.Errorf("definition tools: %w", err)
	}
	requestedToolIDs, err := canonicalToolIDSet(request.VisibleToolIDs)
	if err != nil {
		return definitionToolSelection{}, fmt.Errorf("tool scope: %w", err)
	}
	requestRestricted := request.RestrictVisible || request.VisibleToolIDs != nil
	allowedToolIDs, restricted, err := intersectToolIDs(
		definitionToolIDs,
		definition.Tools.RestrictVisible || len(definition.Tools.VisibleToolIDs) > 0,
		requestedToolIDs,
		requestRestricted,
	)
	if err != nil {
		return definitionToolSelection{}, err
	}
	capabilitySnapshot := runtime.registry.Snapshot(tool.Policy{
		AllowRead: true, AllowWrite: definition.Tools.AllowWrite,
	})
	capabilityTools := capabilitySnapshot.Tools()
	capabilityAvailable := make(map[tool.ToolID]struct{}, len(capabilityTools))
	for _, candidate := range capabilityTools {
		capabilityAvailable[candidate.ID] = struct{}{}
	}
	for _, id := range definition.Tools.VisibleToolIDs {
		if _, ok := capabilityAvailable[tool.ToolID(id)]; !ok {
			return definitionToolSelection{}, fmt.Errorf("tool %q is unavailable", id)
		}
	}
	baseSnapshot := runtime.registry.Snapshot(policy)
	baseTools := baseSnapshot.Tools()
	baseAvailable := make(map[tool.ToolID]struct{}, len(baseTools))
	for _, candidate := range baseTools {
		baseAvailable[candidate.ID] = struct{}{}
	}
	for _, id := range request.VisibleToolIDs {
		if _, ok := baseAvailable[tool.ToolID(id)]; !ok {
			return definitionToolSelection{}, fmt.Errorf("requested tool %q is unavailable", id)
		}
	}
	toolSnapshot := baseSnapshot
	if restricted {
		toolSnapshot = baseSnapshot.Select(allowedToolIDs)
	}
	visibleTools := toolSnapshot.Tools()
	visibleToolIDs := make([]string, 0, len(visibleTools))
	available := make(map[tool.ToolID]struct{}, len(visibleTools))
	for _, candidate := range visibleTools {
		visibleToolIDs = append(visibleToolIDs, string(candidate.ID))
		available[candidate.ID] = struct{}{}
	}
	offeredTools, err := canonicalToolIDSet(request.OfferedToolIDs)
	if err != nil {
		return definitionToolSelection{}, fmt.Errorf("offered tools: %w", err)
	}
	for id := range offeredTools {
		if _, ok := available[id]; !ok {
			return definitionToolSelection{}, fmt.Errorf("offered tool %q is outside the run snapshot", id)
		}
	}
	return definitionToolSelection{
		policy:       policy,
		snapshot:     toolSnapshot,
		visibleIDs:   visibleToolIDs,
		offeredIDs:   offeredTools,
		pruneApplied: request.PruneApplied,
	}, nil
}

func permissionSet(policy agentapi.PermissionPolicy) (map[string]struct{}, error) {
	if len(policy.Scopes) == 0 {
		return nil, fmt.Errorf("at least one permission scope is required")
	}
	if err := platformscope.ValidateAgentRuntime(policy.Scopes); err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(policy.Scopes))
	for _, name := range policy.Scopes {
		set[name] = struct{}{}
	}
	return set, nil
}

func clonePermissions(policy agentapi.PermissionPolicy) agentapi.PermissionPolicy {
	return agentapi.PermissionPolicy{Scopes: append([]string(nil), policy.Scopes...)}
}

func compileDefinitionRequest(
	definition agentapi.Definition,
	request agentapi.RunRequest,
) agentexecution.Input {
	if len(request.Messages) > 0 {
		messages := make([]llm.Message, 0, len(request.Messages))
		question := string(request.Input)
		for _, message := range request.Messages {
			compiled := internalMessage(message)
			messages = append(messages, compiled)
			if compiled.Role == "user" {
				question = compiled.Content
			}
		}
		return agentexecution.Input{
			Question: question, Messages: messages,
			EvidenceSeeded:  request.Policy.EvidenceSeeded || len(request.Context) > 0,
			Direct:          !request.Policy.EvidenceRequired,
			Web:             request.Policy.WebResearch,
			ReferenceTypes:  contextReferenceTypes(request.Context),
			EvidenceContent: joinedContextContent(request.Context),
		}
	}
	messages := []llm.Message{{Role: "system", Content: definition.Prompt.System}}
	for _, block := range request.Context {
		payload, _ := json.Marshal(block)
		messages = append(messages, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentRuntimeContextBlock, struct {
				Payload string
			}{Payload: string(payload)}),
		})
	}
	question := prompts.MustRender(prompts.AgentRuntimeExecuteInput, struct {
		SchemaID      string
		SchemaVersion int64
		Input         string
	}{
		SchemaID:      definition.OutputSchema.ID,
		SchemaVersion: definition.OutputSchema.Version,
		Input:         string(request.Input),
	})
	messages = append(messages, llm.Message{Role: "user", Content: question})
	return agentexecution.Input{
		Question: question, Messages: messages,
		EvidenceSeeded:  request.Policy.EvidenceSeeded || len(request.Context) > 0,
		Direct:          !request.Policy.EvidenceRequired,
		Web:             request.Policy.WebResearch,
		ReferenceTypes:  contextReferenceTypes(request.Context),
		EvidenceContent: joinedContextContent(request.Context),
	}
}

func validateDefinitionMessages(messages []agentapi.Message) error {
	for index, message := range messages {
		switch message.Role {
		case "system", "user", "assistant", "tool":
		default:
			return fmt.Errorf("message %d has unsupported role %q", index, message.Role)
		}
		if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
			return fmt.Errorf("message %d content is required", index)
		}
		if message.Role == "tool" && (message.ToolCallID == "" || message.Name == "") {
			return fmt.Errorf("message %d tool_call_id and name are required", index)
		}
		for callIndex, call := range message.ToolCalls {
			if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
				return fmt.Errorf("message %d tool call %d is invalid", index, callIndex)
			}
		}
	}
	return nil
}

func validateDefinitionContext(blocks []agentapi.ContextBlock) (string, error) {
	for index, block := range blocks {
		if block.Source == "" || block.Title == "" || block.Content == "" {
			return "", fmt.Errorf("context block %d source, title, and content are required", index)
		}
		if len(block.ContentHash) != sha256.Size*2 || !validHex(block.ContentHash) {
			return "", fmt.Errorf("context block %d content_hash is invalid", index)
		}
		if hashString(block.Content) != block.ContentHash {
			return "", fmt.Errorf("context block %d content_hash does not match content", index)
		}
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return "", fmt.Errorf("marshal context snapshot: %w", err)
	}
	return hashBytes(raw), nil
}

func canonicalToolIDSet(ids []string) (map[tool.ToolID]struct{}, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	set := make(map[tool.ToolID]struct{}, len(ids))
	for _, raw := range ids {
		id := tool.ToolID(raw)
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("tool id is required")
		}
		if _, duplicate := set[id]; duplicate {
			return nil, fmt.Errorf("tool %q is duplicated", id)
		}
		set[id] = struct{}{}
	}
	return set, nil
}

func intersectToolIDs(
	definitionIDs map[tool.ToolID]struct{},
	definitionRestricted bool,
	requestedIDs map[tool.ToolID]struct{},
	requestRestricted bool,
) (map[tool.ToolID]struct{}, bool, error) {
	if !definitionRestricted {
		return requestedIDs, requestRestricted, nil
	}
	if !requestRestricted {
		return definitionIDs, true, nil
	}
	for id := range requestedIDs {
		if _, allowed := definitionIDs[id]; !allowed {
			return nil, false, fmt.Errorf("requested tool %q is outside the definition", id)
		}
	}
	return requestedIDs, true, nil
}
