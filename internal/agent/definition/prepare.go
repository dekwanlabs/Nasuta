package definition

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

func (runtime *Runtime) prepare(request agentapi.RunRequest) (preparedExecution, error) {
	definition, modelParameters, err := runtime.resolveExecution(request)
	if err != nil {
		return preparedExecution{}, err
	}
	outputSchema, err := runtime.schemas.Resolve(definition.OutputSchema)
	if err != nil {
		return preparedExecution{}, fmt.Errorf("definition output schema: %w", err)
	}
	policy, err := preparePermissions(definition, request)
	if err != nil {
		return preparedExecution{}, err
	}
	if definition.Budget.Timeout <= runtime.settings.answerReserve {
		return preparedExecution{}, fmt.Errorf("definition timeout must exceed the answer reserve")
	}
	limits, err := prepareRunLimits(
		definition,
		request.Policy,
		request.Limits,
		runtime.settings.answerReserve,
		time.Now().UTC(),
	)
	if err != nil {
		return preparedExecution{}, err
	}
	if err := validateMessages(request.Messages); err != nil {
		return preparedExecution{}, err
	}
	contextHash, err := validateContext(request.Context)
	if err != nil {
		return preparedExecution{}, err
	}
	tools, err := runtime.prepareTools(definition, request.ToolScope, policy)
	if err != nil {
		return preparedExecution{}, err
	}
	if len(tools.visibleIDs) > 0 && limits.MaxToolCalls <= 0 {
		return preparedExecution{}, fmt.Errorf(
			"agent definition %q requires a positive max_tool_calls budget",
			definition.ID,
		)
	}
	return preparedExecution{
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
			Budget: definition.Budget, Limits: limits, Delegation: request.Delegation,
			Permissions: clonePermissions(request.Permissions),
			Actor:       request.Actor, Correlation: request.Correlation, CreatedAt: time.Now().UTC(),
		},
		modelParameters:  modelParameters,
		toolPolicy:       tools.policy,
		toolSnapshot:     tools.snapshot,
		offeredTools:     tools.offeredIDs,
		pruneApplied:     tools.pruneApplied,
		structuredOutput: schemaHasStructuredRoot(outputSchema.Document),
	}, nil
}

func prepareRunLimits(
	definition agentapi.Definition,
	policy agentapi.RunPolicy,
	requested agentapi.RunLimits,
	answerReserve time.Duration,
	now time.Time,
) (agentapi.RunLimits, error) {
	if requested.MaxSteps < 0 || requested.MaxToolCalls < 0 ||
		requested.MaxInputTokens < 0 || requested.MaxContextTokens < 0 ||
		requested.MaxTotalTokens < 0 || requested.MaxCostMicros < 0 {
		return agentapi.RunLimits{}, fmt.Errorf("run limits cannot be negative")
	}
	if requested.MaxContextTokens > 0 && definition.Budget.ContextTokens > 0 &&
		requested.MaxContextTokens > int64(definition.Budget.ContextTokens) {
		return agentapi.RunLimits{}, fmt.Errorf("run context limit exceeds the definition context window")
	}
	maxDeadline := now.Add(definition.Budget.Timeout)
	deadline := requested.Deadline
	if deadline.IsZero() {
		deadline = maxDeadline
	} else if deadline.After(maxDeadline) {
		return agentapi.RunLimits{}, fmt.Errorf("run deadline exceeds the definition timeout")
	}
	if !deadline.After(now.Add(answerReserve)) {
		return agentapi.RunLimits{}, fmt.Errorf("run deadline must exceed the answer reserve")
	}
	maxSteps := requested.MaxSteps
	if maxSteps == 0 {
		maxSteps = definition.Budget.MaxSteps
	} else if maxSteps > definition.Budget.MaxSteps {
		return agentapi.RunLimits{}, fmt.Errorf("run max_steps exceeds the definition budget")
	}
	maxToolCalls := definition.Budget.MaxToolCalls
	if requested.MaxToolCalls > 0 {
		if requested.MaxToolCalls > maxToolCalls {
			return agentapi.RunLimits{}, fmt.Errorf("run max_tool_calls exceeds the definition budget")
		}
		maxToolCalls = requested.MaxToolCalls
	}
	if policy.MaxToolCalls > 0 {
		if policy.MaxToolCalls > definition.Budget.MaxToolCalls {
			return agentapi.RunLimits{}, fmt.Errorf("run policy max_tool_calls exceeds the definition budget")
		}
		if maxToolCalls == 0 || policy.MaxToolCalls < maxToolCalls {
			maxToolCalls = policy.MaxToolCalls
		}
	}
	return agentapi.RunLimits{
		Deadline:         deadline,
		MaxSteps:         maxSteps,
		MaxToolCalls:     maxToolCalls,
		MaxInputTokens:   requested.MaxInputTokens,
		MaxContextTokens: requested.MaxContextTokens,
		MaxTotalTokens:   requested.MaxTotalTokens,
		MaxCostMicros:    requested.MaxCostMicros,
	}, nil
}

func (runtime *Runtime) resolveExecution(
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
	if err := validateDelegationSnapshot(request.Delegation); err != nil {
		return agentapi.Definition{}, llm.ModelParameters{}, err
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

func schemaHasStructuredRoot(document json.RawMessage) bool {
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(document, &schema); err != nil {
		return false
	}
	return schema.Type == "object" || schema.Type == "array"
}

func validateDelegationSnapshot(value agentapi.RunDelegation) error {
	empty := value.DelegationID == "" &&
		value.Depth == 0 &&
		value.Capability.ID == "" &&
		value.Capability.Version == 0 &&
		value.CapabilityContentHash == "" &&
		value.CapabilityRegistryRevision == 0
	if empty {
		return nil
	}
	if strings.TrimSpace(value.DelegationID) == "" {
		return fmt.Errorf("delegation_id is required for a delegated run")
	}
	if value.Depth <= 0 {
		return fmt.Errorf("delegation depth must be positive")
	}
	if value.Capability.ID == "" || value.Capability.Version <= 0 {
		return fmt.Errorf("delegation requires an exact capability version")
	}
	if len(value.CapabilityContentHash) != sha256.Size*2 ||
		!validHex(value.CapabilityContentHash) {
		return fmt.Errorf("capability_content_hash must be a SHA-256 hex digest")
	}
	if value.CapabilityRegistryRevision == 0 {
		return fmt.Errorf("capability registry revision is required")
	}
	return nil
}

func preparePermissions(
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

func (runtime *Runtime) prepareTools(
	definition agentapi.Definition,
	request agentapi.ToolScope,
	policy tool.Policy,
) (toolSelection, error) {
	definitionToolIDs, err := canonicalToolIDSet(definition.Tools.VisibleToolIDs)
	if err != nil {
		return toolSelection{}, fmt.Errorf("definition tools: %w", err)
	}
	requestedToolIDs, err := canonicalToolIDSet(request.VisibleToolIDs)
	if err != nil {
		return toolSelection{}, fmt.Errorf("tool scope: %w", err)
	}
	requestRestricted := request.RestrictVisible || request.VisibleToolIDs != nil
	allowedToolIDs, restricted, err := intersectToolIDs(
		definitionToolIDs,
		definition.Tools.RestrictVisible || len(definition.Tools.VisibleToolIDs) > 0,
		requestedToolIDs,
		requestRestricted,
	)
	if err != nil {
		return toolSelection{}, err
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
			return toolSelection{}, fmt.Errorf("tool %q is unavailable", id)
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
			return toolSelection{}, fmt.Errorf("requested tool %q is unavailable", id)
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
		return toolSelection{}, fmt.Errorf("offered tools: %w", err)
	}
	for id := range offeredTools {
		if _, ok := available[id]; !ok {
			return toolSelection{}, fmt.Errorf("offered tool %q is outside the run snapshot", id)
		}
	}
	return toolSelection{
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
	if err := scope.ValidateAgentRuntime(policy.Scopes); err != nil {
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

func compileRequest(
	definition agentapi.Definition,
	request agentapi.RunRequest,
) execution.Input {
	evidenceSeeded := evidenceSeeded(request.Policy, request.Context)
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
		return execution.Input{
			Question: question, Messages: messages,
			OutputMode:        request.Policy.OutputMode,
			EvidenceSeeded:    evidenceSeeded,
			Direct:            !request.Policy.EvidenceRequired,
			Web:               request.Policy.WebResearch,
			ReferenceTypes:    contextReferenceTypes(request.Context),
			EvidenceContent:   joinedContextContent(request.Context),
			EvidenceUnits:     contextEvidenceUnits(request.Context),
			EvidenceConflicts: evidenceConflicts(request.Context),
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
	return execution.Input{
		Question: question, Messages: messages,
		OutputMode:        request.Policy.OutputMode,
		EvidenceSeeded:    evidenceSeeded,
		Direct:            !request.Policy.EvidenceRequired,
		Web:               request.Policy.WebResearch,
		ReferenceTypes:    contextReferenceTypes(request.Context),
		EvidenceContent:   joinedContextContent(request.Context),
		EvidenceUnits:     contextEvidenceUnits(request.Context),
		EvidenceConflicts: evidenceConflicts(request.Context),
	}
}

func evidenceSeeded(
	policy agentapi.RunPolicy,
	blocks []agentapi.ContextBlock,
) bool {
	if policy.EvidenceSeeded {
		return true
	}
	for _, block := range blocks {
		if block.Source != "workflow.handoff" || len(block.Evidence) > 0 {
			return true
		}
	}
	return false
}

func validateMessages(messages []agentapi.Message) error {
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

func validateContext(blocks []agentapi.ContextBlock) (string, error) {
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
		for unitIndex, unit := range block.Evidence {
			if err := validateEvidenceUnit(index, fmt.Sprintf("evidence unit %d", unitIndex), unit); err != nil {
				return "", err
			}
		}
		for conflictIndex, conflict := range block.EvidenceConflicts {
			label := fmt.Sprintf("evidence conflict %d", conflictIndex)
			identity := conflict.Identity
			if identity.SourceKind == "" ||
				identity.SourceKind != strings.TrimSpace(identity.SourceKind) ||
				identity.Target == "" ||
				identity.Target != strings.TrimSpace(identity.Target) {
				return "", fmt.Errorf(
					"context block %d %s identity source_kind and target are required and canonical",
					index,
					label,
				)
			}
			if err := validateEvidenceUnit(index, label+" current", conflict.Current); err != nil {
				return "", err
			}
			if err := validateEvidenceUnit(index, label+" incoming", conflict.Incoming); err != nil {
				return "", err
			}
			if !evidenceIdentityMatches(identity, conflict.Current) ||
				!evidenceIdentityMatches(identity, conflict.Incoming) {
				return "", fmt.Errorf(
					"context block %d %s identity does not match current and incoming evidence",
					index,
					label,
				)
			}
		}
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return "", fmt.Errorf("marshal context snapshot: %w", err)
	}
	return hashBytes(raw), nil
}

func validateEvidenceUnit(
	blockIndex int,
	label string,
	unit tool.EvidenceUnit,
) error {
	if unit.SourceKind == "" || unit.SourceKind != strings.TrimSpace(unit.SourceKind) ||
		unit.Target == "" || unit.Target != strings.TrimSpace(unit.Target) {
		return fmt.Errorf(
			"context block %d %s source_kind and target are required and canonical",
			blockIndex,
			label,
		)
	}
	if unit.Coverage.Complete && unit.Coverage.Partial {
		return fmt.Errorf("context block %d %s coverage is contradictory", blockIndex, label)
	}
	if unit.TokenCost < 0 {
		return fmt.Errorf("context block %d %s token_cost is invalid", blockIndex, label)
	}
	if unit.ContentHash != "" &&
		(len(unit.ContentHash) != sha256.Size*2 || !validHex(unit.ContentHash)) {
		return fmt.Errorf("context block %d %s content_hash is invalid", blockIndex, label)
	}
	seenSections := make(map[string]struct{}, len(unit.Sections))
	for sectionIndex, section := range unit.Sections {
		if section == "" || section != strings.TrimSpace(section) {
			return fmt.Errorf(
				"context block %d %s section %d is invalid",
				blockIndex,
				label,
				sectionIndex,
			)
		}
		if _, duplicate := seenSections[section]; duplicate {
			return fmt.Errorf(
				"context block %d %s section %q is duplicated",
				blockIndex,
				label,
				section,
			)
		}
		seenSections[section] = struct{}{}
	}
	return nil
}

func evidenceIdentityMatches(
	identity agentapi.EvidenceIdentity,
	unit tool.EvidenceUnit,
) bool {
	if identity.SourceKind != unit.SourceKind ||
		identity.Target != unit.Target ||
		identity.Version != unit.Version ||
		identity.TimeRange != unit.TimeRange {
		return false
	}
	if identity.Section == "" {
		return len(unit.Sections) == 0
	}
	return len(unit.Sections) == 1 && unit.Sections[0] == identity.Section
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
