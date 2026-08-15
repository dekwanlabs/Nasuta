package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// SideEffectClass declares whether one capability can mutate domain state.
type SideEffectClass string

const (
	SideEffectNone  SideEffectClass = "none"
	SideEffectWrite SideEffectClass = "write"
)

// Capability binds one planner-visible capability to a pinned Agent Definition.
type Capability struct {
	ID              string          `json:"id"`
	Version         int64           `json:"version"`
	Purpose         string          `json:"purpose"`
	InputFacets     []string        `json:"input_facets,omitempty"`
	InputSchema     SchemaRef       `json:"input_schema"`
	OutputSchema    SchemaRef       `json:"output_schema"`
	ToolIDs         []string        `json:"tool_ids,omitempty"`
	PermissionScope []string        `json:"permission_scope,omitempty"`
	Freshness       FreshnessPolicy `json:"freshness"`
	SideEffects     SideEffectClass `json:"side_effects"`
	RetrySafe       bool            `json:"retry_safe"`
	MaxConcurrency  int             `json:"max_concurrency"`
	Enabled         bool            `json:"enabled"`
	Agent           DefinitionRef   `json:"agent"`
	WriteSet        []string        `json:"write_set,omitempty"`
	ContentHash     string          `json:"content_hash"`
}

// CapabilityRef selects one immutable version; zero selects the latest published version.
type CapabilityRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version,omitempty"`
}

// DefinitionResolver resolves an exact immutable Agent Definition.
type DefinitionResolver interface {
	Resolve(DefinitionRef) (Definition, error)
}

type capabilityKey struct {
	id      string
	version int64
}

type capabilityState struct {
	revision     uint64
	capabilities map[capabilityKey]Capability
	latest       map[string]int64
}

// CapabilityRegistry atomically publishes immutable planner capability snapshots.
type CapabilityRegistry struct {
	writeMu sync.Mutex
	state   atomic.Pointer[capabilityState]
	schemas *SchemaRegistry
	agents  DefinitionResolver
}

func NewCapabilityRegistry(
	schemas *SchemaRegistry,
	agents DefinitionResolver,
) *CapabilityRegistry {
	registry := &CapabilityRegistry{schemas: schemas, agents: agents}
	registry.state.Store(&capabilityState{
		capabilities: make(map[capabilityKey]Capability),
		latest:       make(map[string]int64),
	})
	return registry
}

// Publish validates and adds an all-or-nothing capability batch.
func (registry *CapabilityRegistry) Publish(capabilities []Capability) error {
	if registry == nil || registry.schemas == nil || registry.agents == nil {
		return fmt.Errorf("capability registry dependencies are required")
	}
	if len(capabilities) == 0 {
		return fmt.Errorf("capabilities are required")
	}
	incoming := make(map[capabilityKey]Capability, len(capabilities))
	for _, capability := range capabilities {
		prepared, err := registry.prepare(capability)
		if err != nil {
			return err
		}
		key := capabilityKey{id: prepared.ID, version: prepared.Version}
		if _, duplicate := incoming[key]; duplicate {
			return fmt.Errorf(
				"capability %q version %d is duplicated",
				prepared.ID,
				prepared.Version,
			)
		}
		incoming[key] = prepared
	}

	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()
	current := registry.state.Load()
	next := &capabilityState{
		revision:     current.revision + 1,
		capabilities: make(map[capabilityKey]Capability, len(current.capabilities)+len(incoming)),
		latest:       make(map[string]int64, len(current.latest)+len(incoming)),
	}
	for key, capability := range current.capabilities {
		next.capabilities[key] = capability
	}
	for id, version := range current.latest {
		next.latest[id] = version
	}
	for key, capability := range incoming {
		if published, exists := next.capabilities[key]; exists &&
			published.ContentHash != capability.ContentHash {
			return fmt.Errorf(
				"capability %q version %d is already published",
				key.id,
				key.version,
			)
		}
		next.capabilities[key] = capability
		if key.version > next.latest[key.id] {
			next.latest[key.id] = key.version
		}
	}
	registry.state.Store(next)
	return nil
}

// Resolve returns a detached immutable capability version.
func (registry *CapabilityRegistry) Resolve(ref CapabilityRef) (Capability, error) {
	if registry == nil || registry.state.Load() == nil {
		return Capability{}, fmt.Errorf("capability registry is unavailable")
	}
	id := strings.TrimSpace(ref.ID)
	if id != ref.ID || !canonicalID.MatchString(id) {
		return Capability{}, fmt.Errorf("capability id %q is not canonical", ref.ID)
	}
	current := registry.state.Load()
	version := ref.Version
	if version == 0 {
		version = current.latest[id]
	}
	capability, ok := current.capabilities[capabilityKey{id: id, version: version}]
	if !ok {
		return Capability{}, fmt.Errorf("capability %q version %d not found", id, version)
	}
	return cloneCapability(capability), nil
}

func (registry *CapabilityRegistry) Revision() uint64 {
	if registry == nil || registry.state.Load() == nil {
		return 0
	}
	return registry.state.Load().revision
}

func (registry *CapabilityRegistry) prepare(capability Capability) (Capability, error) {
	prepared := cloneCapability(capability)
	prepared.ID = strings.TrimSpace(prepared.ID)
	if prepared.ID != capability.ID || !canonicalID.MatchString(prepared.ID) {
		return Capability{}, fmt.Errorf("capability id %q is not canonical", capability.ID)
	}
	if prepared.Version <= 0 {
		return Capability{}, fmt.Errorf("capability %q version must be positive", prepared.ID)
	}
	if strings.TrimSpace(prepared.Purpose) == "" {
		return Capability{}, fmt.Errorf("capability %q purpose is required", prepared.ID)
	}
	if err := validateSchemaRef("capability input", prepared.InputSchema); err != nil {
		return Capability{}, fmt.Errorf("capability %q: %w", prepared.ID, err)
	}
	if err := validateSchemaRef("capability output", prepared.OutputSchema); err != nil {
		return Capability{}, fmt.Errorf("capability %q: %w", prepared.ID, err)
	}
	if _, err := registry.schemas.Resolve(prepared.InputSchema); err != nil {
		return Capability{}, fmt.Errorf("capability %q input schema: %w", prepared.ID, err)
	}
	if _, err := registry.schemas.Resolve(prepared.OutputSchema); err != nil {
		return Capability{}, fmt.Errorf("capability %q output schema: %w", prepared.ID, err)
	}
	if err := validateCapabilityList(prepared.ID, "input facet", prepared.InputFacets); err != nil {
		return Capability{}, err
	}
	if err := validateCapabilityList(prepared.ID, "tool", prepared.ToolIDs); err != nil {
		return Capability{}, err
	}
	if err := validateCapabilityList(prepared.ID, "permission", prepared.PermissionScope); err != nil {
		return Capability{}, err
	}
	switch prepared.Freshness {
	case FreshnessStable, FreshnessCurrent, FreshnessBoundedLive:
	default:
		return Capability{}, fmt.Errorf(
			"capability %q freshness policy %q is invalid",
			prepared.ID,
			prepared.Freshness,
		)
	}
	if prepared.MaxConcurrency <= 0 {
		return Capability{}, fmt.Errorf("capability %q max concurrency must be positive", prepared.ID)
	}
	switch prepared.SideEffects {
	case SideEffectNone:
		if len(prepared.WriteSet) != 0 {
			return Capability{}, fmt.Errorf("capability %q read-only write set must be empty", prepared.ID)
		}
	case SideEffectWrite:
		if len(prepared.WriteSet) == 0 {
			return Capability{}, fmt.Errorf("capability %q write set is required", prepared.ID)
		}
	default:
		return Capability{}, fmt.Errorf(
			"capability %q side effect class %q is invalid",
			prepared.ID,
			prepared.SideEffects,
		)
	}
	if err := validateWriteSet(prepared.ID, prepared.WriteSet); err != nil {
		return Capability{}, err
	}
	if !canonicalID.MatchString(prepared.Agent.ID) || prepared.Agent.Version <= 0 {
		return Capability{}, fmt.Errorf("capability %q requires a pinned agent definition", prepared.ID)
	}
	definition, err := registry.agents.Resolve(prepared.Agent)
	if err != nil {
		return Capability{}, fmt.Errorf("capability %q agent definition: %w", prepared.ID, err)
	}
	if definition.ID != prepared.Agent.ID || definition.Version != prepared.Agent.Version {
		return Capability{}, fmt.Errorf("capability %q agent definition is not pinned", prepared.ID)
	}
	if err := registry.schemas.ValidateCompatibility(
		prepared.InputSchema,
		definition.InputSchema,
	); err != nil {
		return Capability{}, fmt.Errorf("capability %q agent input: %w", prepared.ID, err)
	}
	if err := registry.schemas.ValidateCompatibility(
		definition.OutputSchema,
		prepared.OutputSchema,
	); err != nil {
		return Capability{}, fmt.Errorf("capability %q agent output: %w", prepared.ID, err)
	}
	if err := ensureStringSubset(
		prepared.PermissionScope,
		definition.Permissions.Scopes,
	); err != nil {
		return Capability{}, fmt.Errorf("capability %q permissions: %w", prepared.ID, err)
	}
	if definition.Tools.RestrictVisible || len(definition.Tools.VisibleToolIDs) > 0 {
		if err := ensureStringSubset(
			prepared.ToolIDs,
			definition.Tools.VisibleToolIDs,
		); err != nil {
			return Capability{}, fmt.Errorf("capability %q tools: %w", prepared.ID, err)
		}
	}
	hash, err := capabilityHash(prepared)
	if err != nil {
		return Capability{}, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return Capability{}, fmt.Errorf("capability %q content hash mismatch", prepared.ID)
	}
	prepared.ContentHash = hash
	return prepared, nil
}

func validateCapabilityList(capabilityID, label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !canonicalID.MatchString(value) {
			return fmt.Errorf("capability %q %s id %q is not canonical", capabilityID, label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("capability %q contains duplicate %s id %q", capabilityID, label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateWriteSet(capabilityID string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("capability %q write set entry %q is invalid", capabilityID, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("capability %q write set entry %q is duplicated", capabilityID, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func ensureStringSubset(subset, superset []string) error {
	allowed := make(map[string]struct{}, len(superset))
	for _, value := range superset {
		allowed[value] = struct{}{}
	}
	for _, value := range subset {
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("%q is outside the allowed set", value)
		}
	}
	return nil
}

func capabilityHash(capability Capability) (string, error) {
	capability.ContentHash = ""
	payload, err := json.Marshal(capability)
	if err != nil {
		return "", fmt.Errorf("marshal capability %q: %w", capability.ID, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func cloneCapability(capability Capability) Capability {
	capability.InputFacets = append([]string(nil), capability.InputFacets...)
	capability.ToolIDs = append([]string(nil), capability.ToolIDs...)
	capability.PermissionScope = append([]string(nil), capability.PermissionScope...)
	capability.WriteSet = append([]string(nil), capability.WriteSet...)
	return capability
}
