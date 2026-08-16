package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

var ErrWorkflowBindingNotFound = errors.New("workflow binding not found")

type WorkflowBindingRegistration struct {
	Binding agentapi.WorkflowBinding
	Builder agentapi.WorkflowEscalationInputBuilder
}

type workflowBindingKey struct {
	id      string
	version int64
}

type workflowBindingCapabilityKey struct {
	id      string
	version int64
	hash    string
}

type preparedWorkflowBinding struct {
	binding    agentapi.WorkflowBinding
	builder    agentapi.WorkflowEscalationInputBuilder
	capability agentapi.Capability
	workflow   Definition
}

type workflowBindingState struct {
	records map[workflowBindingKey]preparedWorkflowBinding
	latest  map[workflowBindingCapabilityKey]workflowBindingKey
	owners  map[workflowBindingCapabilityKey]string
}

type CapabilityResolver interface {
	Resolve(agentapi.CapabilityRef) (agentapi.Capability, error)
}

type WorkflowDefinitionResolver interface {
	Resolve(DefinitionRef) (Definition, error)
}

// WorkflowBindingRegistry publishes immutable application-owned escalation bindings.
// Capability and Workflow catalogs remain independent sources of immutable identity.
type WorkflowBindingRegistry struct {
	writeMu      sync.Mutex
	state        atomic.Pointer[workflowBindingState]
	capabilities CapabilityResolver
	workflows    WorkflowDefinitionResolver
}

func NewWorkflowBindingRegistry(
	capabilities CapabilityResolver,
	workflows WorkflowDefinitionResolver,
) *WorkflowBindingRegistry {
	registry := &WorkflowBindingRegistry{
		capabilities: capabilities,
		workflows:    workflows,
	}
	registry.state.Store(&workflowBindingState{
		records: make(map[workflowBindingKey]preparedWorkflowBinding),
		latest:  make(map[workflowBindingCapabilityKey]workflowBindingKey),
		owners:  make(map[workflowBindingCapabilityKey]string),
	})
	return registry
}

func (registry *WorkflowBindingRegistry) Publish(
	registrations []WorkflowBindingRegistration,
) error {
	if registry == nil || registry.capabilities == nil || registry.workflows == nil {
		return fmt.Errorf("workflow binding registry dependencies are required")
	}
	if len(registrations) == 0 {
		return fmt.Errorf("workflow binding registrations are required")
	}
	incoming := make(map[workflowBindingKey]preparedWorkflowBinding, len(registrations))
	for _, registration := range registrations {
		prepared, err := registry.prepare(registration)
		if err != nil {
			return err
		}
		key := workflowBindingKey{
			id:      prepared.binding.ID,
			version: prepared.binding.Version,
		}
		if _, duplicate := incoming[key]; duplicate {
			return fmt.Errorf(
				"workflow binding %q version %d is duplicated",
				key.id,
				key.version,
			)
		}
		incoming[key] = prepared
	}

	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()
	current := registry.state.Load()
	next := &workflowBindingState{
		records: make(
			map[workflowBindingKey]preparedWorkflowBinding,
			len(current.records)+len(incoming),
		),
		latest: make(
			map[workflowBindingCapabilityKey]workflowBindingKey,
			len(current.latest)+len(incoming),
		),
		owners: make(
			map[workflowBindingCapabilityKey]string,
			len(current.owners)+len(incoming),
		),
	}
	for key, binding := range current.records {
		next.records[key] = binding
	}
	for key, binding := range current.latest {
		next.latest[key] = binding
	}
	for key, owner := range current.owners {
		next.owners[key] = owner
	}
	for key, prepared := range incoming {
		if published, exists := next.records[key]; exists {
			if published.binding.ContentHash != prepared.binding.ContentHash {
				return fmt.Errorf(
					"workflow binding %q version %d is already published",
					key.id,
					key.version,
				)
			}
			continue
		}
		capabilityKey := workflowBindingCapabilityKey{
			id:      prepared.binding.Capability.ID,
			version: prepared.binding.Capability.Version,
			hash:    prepared.binding.CapabilityHash,
		}
		if owner := next.owners[capabilityKey]; owner != "" &&
			owner != prepared.binding.ID {
			return fmt.Errorf(
				"capability %q version %d is already owned by workflow binding %q",
				capabilityKey.id,
				capabilityKey.version,
				owner,
			)
		}
		next.records[key] = prepared
		next.owners[capabilityKey] = prepared.binding.ID
		latest, exists := next.latest[capabilityKey]
		if !exists || key.version > latest.version {
			next.latest[capabilityKey] = key
		}
	}
	registry.state.Store(next)
	return nil
}

func (registry *WorkflowBindingRegistry) Resolve(
	ref agentapi.CapabilityRef,
	contentHash string,
) (WorkflowBindingRegistration, agentapi.Capability, Definition, error) {
	if registry == nil || registry.state.Load() == nil {
		return WorkflowBindingRegistration{}, agentapi.Capability{}, Definition{},
			fmt.Errorf("workflow binding registry is unavailable")
	}
	if ref.Version <= 0 || !validContentHash(contentHash) {
		return WorkflowBindingRegistration{}, agentapi.Capability{}, Definition{},
			fmt.Errorf("exact capability identity is required")
	}
	current := registry.state.Load()
	key, ok := current.latest[workflowBindingCapabilityKey{
		id: ref.ID, version: ref.Version, hash: contentHash,
	}]
	if !ok {
		return WorkflowBindingRegistration{}, agentapi.Capability{}, Definition{},
			ErrWorkflowBindingNotFound
	}
	prepared, ok := current.records[key]
	if !ok {
		return WorkflowBindingRegistration{}, agentapi.Capability{}, Definition{},
			fmt.Errorf("workflow binding registry state is inconsistent")
	}
	return WorkflowBindingRegistration{
			Binding: cloneWorkflowBinding(prepared.binding),
			Builder: prepared.builder,
		},
		cloneCapabilityForBinding(prepared.capability),
		cloneDefinition(prepared.workflow),
		nil
}

func (registry *WorkflowBindingRegistry) prepare(
	registration WorkflowBindingRegistration,
) (preparedWorkflowBinding, error) {
	if registration.Builder == nil {
		return preparedWorkflowBinding{}, fmt.Errorf("workflow binding builder is required")
	}
	binding := cloneWorkflowBinding(registration.Binding)
	binding.ID = strings.TrimSpace(binding.ID)
	if binding.ID != registration.Binding.ID || !canonicalID.MatchString(binding.ID) {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding id %q is not canonical",
			registration.Binding.ID,
		)
	}
	if binding.Version <= 0 {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q version must be positive",
			binding.ID,
		)
	}
	if binding.Capability.Version <= 0 ||
		!canonicalID.MatchString(binding.Capability.ID) ||
		!validContentHash(binding.CapabilityHash) {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q requires an exact capability identity",
			binding.ID,
		)
	}
	capability, err := registry.capabilities.Resolve(binding.Capability)
	if err != nil {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q capability: %w",
			binding.ID,
			err,
		)
	}
	if capability.ID != binding.Capability.ID ||
		capability.Version != binding.Capability.Version ||
		capability.ContentHash != binding.CapabilityHash {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q capability identity does not match the registry",
			binding.ID,
		)
	}
	if !canonicalID.MatchString(binding.Workflow.ID) ||
		binding.Workflow.Version <= 0 ||
		!validContentHash(binding.Workflow.ContentHash) {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q requires an exact workflow identity",
			binding.ID,
		)
	}
	definition, err := registry.workflows.Resolve(DefinitionRef{
		ID: binding.Workflow.ID, Version: binding.Workflow.Version,
	})
	if err != nil {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q workflow: %w",
			binding.ID,
			err,
		)
	}
	if definition.ContentHash != binding.Workflow.ContentHash {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q workflow content hash mismatch",
			binding.ID,
		)
	}
	if binding.InputSchema != definition.InputSchema {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q input schema does not match workflow %q",
			binding.ID,
			definition.ID,
		)
	}
	binding.Scenario = strings.TrimSpace(binding.Scenario)
	if binding.Scenario == "" || !canonicalID.MatchString(binding.Scenario) {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q scenario is not canonical",
			binding.ID,
		)
	}
	if err := scope.Validate(binding.ScenarioPermissions.Scopes); err != nil {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q scenario permissions: %w",
			binding.ID,
			err,
		)
	}
	binding.ScenarioPermissions.Scopes = canonicalBindingStrings(
		binding.ScenarioPermissions.Scopes,
	)
	binding.BuilderID = strings.TrimSpace(binding.BuilderID)
	if !canonicalID.MatchString(binding.BuilderID) {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q builder id %q is not canonical",
			binding.ID,
			registration.Binding.BuilderID,
		)
	}
	reasons, err := canonicalEscalationReasons(binding.AllowedReasons)
	if err != nil {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q: %w",
			binding.ID,
			err,
		)
	}
	binding.AllowedReasons = reasons
	contentHash, err := workflowBindingContentHash(binding)
	if err != nil {
		return preparedWorkflowBinding{}, err
	}
	if binding.ContentHash != "" && binding.ContentHash != contentHash {
		return preparedWorkflowBinding{}, fmt.Errorf(
			"workflow binding %q content hash mismatch",
			binding.ID,
		)
	}
	binding.ContentHash = contentHash
	return preparedWorkflowBinding{
		binding:    binding,
		builder:    registration.Builder,
		capability: capability,
		workflow:   definition,
	}, nil
}

func workflowBindingContentHash(binding agentapi.WorkflowBinding) (string, error) {
	binding.ContentHash = ""
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", fmt.Errorf("marshal workflow binding %q: %w", binding.ID, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalEscalationReasons(
	reasons []agentapi.WorkflowEscalationReason,
) ([]agentapi.WorkflowEscalationReason, error) {
	if len(reasons) == 0 {
		return nil, fmt.Errorf("allowed escalation reasons are required")
	}
	allowed := map[agentapi.WorkflowEscalationReason]struct{}{
		agentapi.EscalationStrongTaskDependencies:       {},
		agentapi.EscalationDurableExecutionRequired:     {},
		agentapi.EscalationHumanApprovalRequired:        {},
		agentapi.EscalationHighRiskVerificationRequired: {},
		agentapi.EscalationChildLimitExceeded:           {},
		agentapi.EscalationParentTimeBudgetInsufficient: {},
		agentapi.EscalationScenarioRequiresWorkflow:     {},
	}
	seen := make(map[agentapi.WorkflowEscalationReason]struct{}, len(reasons))
	out := make([]agentapi.WorkflowEscalationReason, 0, len(reasons))
	for _, reason := range reasons {
		if _, ok := allowed[reason]; !ok {
			return nil, fmt.Errorf("escalation reason %q is invalid", reason)
		}
		if _, duplicate := seen[reason]; duplicate {
			continue
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func canonicalBindingStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func cloneWorkflowBinding(binding agentapi.WorkflowBinding) agentapi.WorkflowBinding {
	binding.AllowedReasons = append(
		[]agentapi.WorkflowEscalationReason(nil),
		binding.AllowedReasons...,
	)
	binding.ScenarioPermissions.Scopes = append(
		[]string(nil),
		binding.ScenarioPermissions.Scopes...,
	)
	return binding
}

func cloneCapabilityForBinding(capability agentapi.Capability) agentapi.Capability {
	capability.InputFacets = append([]string(nil), capability.InputFacets...)
	capability.ToolIDs = append([]string(nil), capability.ToolIDs...)
	capability.PermissionScope = append([]string(nil), capability.PermissionScope...)
	capability.WriteSet = append([]string(nil), capability.WriteSet...)
	return capability
}

func validContentHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
