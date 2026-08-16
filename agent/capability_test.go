package agent

import (
	"strings"
	"testing"
	"time"
)

type capabilityDefinitionResolver map[DefinitionRef]Definition

func (resolver capabilityDefinitionResolver) Resolve(ref DefinitionRef) (Definition, error) {
	definition, ok := resolver[ref]
	if !ok {
		return Definition{}, errCapabilityDefinitionNotFound
	}
	return cloneDefinition(definition), nil
}

type capabilityDefinitionError string

func (err capabilityDefinitionError) Error() string {
	return string(err)
}

const errCapabilityDefinitionNotFound = capabilityDefinitionError("definition not found")

func TestCapabilityRegistryPublishesAtomicallyAndReturnsDetachedVersions(t *testing.T) {
	schemas, definitions := capabilityTestDependencies(t)
	registry := NewCapabilityRegistry(schemas, definitions)
	first := capabilityTestValue(1)
	if err := registry.Publish([]Capability{first}); err != nil {
		t.Fatal(err)
	}
	revision := registry.Revision()
	second := capabilityTestValue(2)
	invalid := capabilityTestValue(3)
	invalid.MaxConcurrency = 0
	if err := registry.Publish([]Capability{second, invalid}); err == nil {
		t.Fatal("registry accepted a partially invalid batch")
	}
	if registry.Revision() != revision {
		t.Fatalf("revision = %d, want %d after rejected batch", registry.Revision(), revision)
	}
	if _, err := registry.Resolve(CapabilityRef{ID: second.ID, Version: second.Version}); err == nil {
		t.Fatal("registry published the valid member of a rejected batch")
	}

	resolved, err := registry.Resolve(CapabilityRef{ID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != first.Version || resolved.ContentHash == "" {
		t.Fatalf("resolved capability = %+v", resolved)
	}
	resolved.ToolIDs[0] = "changed"
	resolved.PermissionScope[0] = "changed"
	resolved.WriteSet = append(resolved.WriteSet, "changed")
	pinned, err := registry.Resolve(CapabilityRef{ID: first.ID, Version: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ToolIDs[0] != "search_code" ||
		pinned.PermissionScope[0] != "knowledge.read" ||
		len(pinned.WriteSet) != 0 {
		t.Fatalf("resolved mutation changed registry state: %+v", pinned)
	}
}

func TestCapabilityRegistryRequiresPinnedAgentDefinition(t *testing.T) {
	schemas, definitions := capabilityTestDependencies(t)
	registry := NewCapabilityRegistry(schemas, definitions)
	capability := capabilityTestValue(1)
	capability.Agent.Version = 0
	if err := registry.Publish([]Capability{capability}); err == nil ||
		!strings.Contains(err.Error(), "pinned agent definition") {
		t.Fatalf("Publish error = %v, want pinned definition rejection", err)
	}
}

func TestCapabilityRegistryRejectsOversizedID(t *testing.T) {
	schemas, definitions := capabilityTestDependencies(t)
	registry := NewCapabilityRegistry(schemas, definitions)
	capability := capabilityTestValue(1)
	capability.ID = strings.Repeat("a", MaxCapabilityIDBytes+1)
	if err := registry.Publish([]Capability{capability}); err == nil ||
		!strings.Contains(err.Error(), "exceeds 128 bytes") {
		t.Fatalf("Publish error = %v, want oversized ID rejection", err)
	}
}

func TestCapabilityRegistryRejectsContractExpansion(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Capability)
		want   string
	}{
		{
			name: "schema",
			change: func(capability *Capability) {
				capability.OutputSchema = SchemaRef{ID: "capability.other", Version: 1}
			},
			want: "agent output",
		},
		{
			name: "tool",
			change: func(capability *Capability) {
				capability.ToolIDs = append(capability.ToolIDs, "trace_calls")
			},
			want: "tools",
		},
		{
			name: "permission",
			change: func(capability *Capability) {
				capability.PermissionScope = append(
					capability.PermissionScope,
					"knowledge.write",
				)
			},
			want: "permissions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemas, definitions := capabilityTestDependencies(t)
			registry := NewCapabilityRegistry(schemas, definitions)
			capability := capabilityTestValue(1)
			test.change(&capability)
			if err := registry.Publish([]Capability{capability}); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Publish error = %v, want %q rejection", err, test.want)
			}
		})
	}
}

func TestCapabilityRegistryRejectsPublishedVersionMutation(t *testing.T) {
	schemas, definitions := capabilityTestDependencies(t)
	registry := NewCapabilityRegistry(schemas, definitions)
	capability := capabilityTestValue(1)
	if err := registry.Publish([]Capability{capability}); err != nil {
		t.Fatal(err)
	}
	capability.RetrySafe = !capability.RetrySafe
	if err := registry.Publish([]Capability{capability}); err == nil ||
		!strings.Contains(err.Error(), "already published") {
		t.Fatalf("Publish error = %v, want immutable version conflict", err)
	}
}

func capabilityTestDependencies(
	t *testing.T,
) (*SchemaRegistry, capabilityDefinitionResolver) {
	t.Helper()
	schemas := NewSchemaRegistry()
	if err := schemas.Publish([]SchemaDefinition{
		{
			ID: "capability.input", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
		{
			ID: "capability.output", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
		{
			ID: "capability.other", Version: 1,
			Document: []byte(`{"type":"string"}`),
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := Prepare(Definition{
		ID: "capability.agent", Version: 1,
		Prompt:       PromptSpec{System: "Inspect the supplied subject.", Version: "1"},
		InputSchema:  SchemaRef{ID: "capability.input", Version: 1},
		OutputSchema: SchemaRef{ID: "capability.output", Version: 1},
		Model: ModelPolicy{
			Provider: "openai", Model: "test", MaxOutputTokens: 100,
		},
		Tools: ToolPolicy{
			VisibleToolIDs: []string{"search_code"}, RestrictVisible: true,
		},
		Budget: BudgetPolicy{
			Timeout: time.Second, MaxSteps: 1, ContextTokens: 1000,
		},
		Permissions: PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return schemas, capabilityDefinitionResolver{
		{ID: definition.ID, Version: definition.Version}: definition,
	}
}

func capabilityTestValue(version int64) Capability {
	return Capability{
		ID: "knowledge.code.inspect", Version: version,
		Role:            RoleInvestigator,
		Purpose:         "Inspect source implementation.",
		InputFacets:     []string{"implementation"},
		InputSchema:     SchemaRef{ID: "capability.input", Version: 1},
		OutputSchema:    SchemaRef{ID: "capability.output", Version: 1},
		ToolIDs:         []string{"search_code"},
		PermissionScope: []string{"knowledge.read"},
		Freshness:       FreshnessStable,
		SideEffects:     SideEffectNone,
		RetrySafe:       true,
		MaxConcurrency:  2,
		Enabled:         true,
		Agent:           DefinitionRef{ID: "capability.agent", Version: 1},
	}
}
