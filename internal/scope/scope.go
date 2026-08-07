// Package scope owns the permission vocabulary shared by runtime executors.
package scope

import "fmt"

const (
	KnowledgeRead   = "knowledge.read"
	KnowledgeWrite  = "knowledge.write"
	FeatureDelivery = "feature.delivery"

	OwnerAgentRuntime    = "agent.runtime"
	OwnerFeatureDelivery = "feature.delivery"
)

// Metadata describes the executor and side-effect boundary of one scope.
type Metadata struct {
	Name         string
	Owner        string
	AgentRuntime bool
	SideEffect   bool
}

var vocabulary = map[string]Metadata{
	KnowledgeRead: {
		Name: KnowledgeRead, Owner: OwnerAgentRuntime, AgentRuntime: true,
	},
	KnowledgeWrite: {
		Name: KnowledgeWrite, Owner: OwnerAgentRuntime, AgentRuntime: true, SideEffect: true,
	},
	FeatureDelivery: {
		Name: FeatureDelivery, Owner: OwnerFeatureDelivery, SideEffect: true,
	},
}

// Lookup returns metadata for one registered scope.
func Lookup(name string) (Metadata, bool) {
	metadata, ok := vocabulary[name]
	return metadata, ok
}

// Validate rejects unknown or duplicated scopes.
func Validate(scopes []string) error {
	seen := make(map[string]struct{}, len(scopes))
	for _, name := range scopes {
		if _, ok := vocabulary[name]; !ok {
			return fmt.Errorf("permission scope %q is not registered", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("permission scope %q is duplicated", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// ValidateAgentRuntime rejects scopes owned by another executor.
func ValidateAgentRuntime(scopes []string) error {
	if err := Validate(scopes); err != nil {
		return err
	}
	for _, name := range scopes {
		metadata := vocabulary[name]
		if !metadata.AgentRuntime {
			return fmt.Errorf(
				"permission scope %q belongs to %q and is not supported by the agent runtime",
				name,
				metadata.Owner,
			)
		}
	}
	return nil
}

// EnsureSubset guarantees delegation does not add a scope.
func EnsureSubset(subset, superset []string) error {
	if err := Validate(subset); err != nil {
		return err
	}
	if err := Validate(superset); err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(superset))
	for _, name := range superset {
		allowed[name] = struct{}{}
	}
	for _, name := range subset {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("permission scope %q is outside the allowed set", name)
		}
	}
	return nil
}

// Has reports whether a policy contains one exact scope.
func Has(scopes []string, target string) bool {
	for _, name := range scopes {
		if name == target {
			return true
		}
	}
	return false
}

// HasSideEffect reports whether any registered scope can mutate domain state.
func HasSideEffect(scopes []string) bool {
	for _, name := range scopes {
		if metadata, ok := vocabulary[name]; ok && metadata.SideEffect {
			return true
		}
	}
	return false
}
