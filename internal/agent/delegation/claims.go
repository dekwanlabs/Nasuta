package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const (
	ComparatorExclusiveScalar  = "exclusive_scalar"
	ComparatorBooleanAssertion = "boolean_assertion"
)

type claimPolicyState struct {
	policies    map[string]agentapi.ClaimPolicy
	comparators map[string]agentapi.ClaimComparator
}

// ClaimRegistry atomically publishes deterministic claim policies and comparators.
type ClaimRegistry struct {
	mu    sync.RWMutex
	state claimPolicyState
}

func NewClaimRegistry() *ClaimRegistry {
	registry := &ClaimRegistry{}
	registry.state = claimPolicyState{
		policies: map[string]agentapi.ClaimPolicy{},
		comparators: map[string]agentapi.ClaimComparator{
			ComparatorExclusiveScalar:  claimComparatorFunc(exclusiveScalar),
			ComparatorBooleanAssertion: claimComparatorFunc(booleanAssertion),
		},
	}
	return registry
}

func (registry *ClaimRegistry) RegisterComparator(
	id string,
	comparator agentapi.ClaimComparator,
) error {
	id = strings.TrimSpace(id)
	if id == "" || comparator == nil {
		return fmt.Errorf("claim comparator id and implementation are required")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.state.comparators[id]; exists {
		return fmt.Errorf("claim comparator %q is already registered", id)
	}
	registry.state.comparators[id] = comparator
	return nil
}

func (registry *ClaimRegistry) Publish(policies []agentapi.ClaimPolicy) error {
	if len(policies) == 0 {
		return nil
	}
	prepared := make([]agentapi.ClaimPolicy, len(policies))
	for index, policy := range policies {
		if policy.Schema.ID == "" || policy.Schema.Version <= 0 {
			return fmt.Errorf("claim policy schema must be pinned")
		}
		policy.ComparatorID = strings.TrimSpace(policy.ComparatorID)
		if policy.ComparatorID == "" {
			return fmt.Errorf("claim policy comparator is required")
		}
		policy.KeyFields = canonicalFields(policy.KeyFields)
		policy.ScopeFields = canonicalFields(policy.ScopeFields)
		if len(policy.KeyFields) == 0 {
			return fmt.Errorf("claim policy key fields are required")
		}
		for _, field := range policy.KeyFields {
			switch field {
			case "subject", "predicate":
			default:
				return fmt.Errorf(
					"claim policy key field %q is unsupported",
					field,
				)
			}
		}
		prepared[index] = policy
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := make(map[string]agentapi.ClaimPolicy, len(registry.state.policies)+len(prepared))
	for key, policy := range registry.state.policies {
		next[key] = policy
	}
	for _, policy := range prepared {
		if _, ok := registry.state.comparators[policy.ComparatorID]; !ok {
			return fmt.Errorf("claim comparator %q is not registered", policy.ComparatorID)
		}
		key := schemaKey(policy.Schema)
		if _, exists := next[key]; exists {
			return fmt.Errorf("claim policy for %q is already published", key)
		}
		next[key] = policy
	}
	registry.state.policies = next
	return nil
}

func (registry *ClaimRegistry) resolve(
	schema string,
) (agentapi.ClaimPolicy, agentapi.ClaimComparator, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	policy, ok := registry.state.policies[schema]
	if !ok {
		for key, candidate := range registry.state.policies {
			if schema == candidate.Schema.ID ||
				schema == candidate.Schema.ID+".v"+fmt.Sprint(candidate.Schema.Version) ||
				schema == key {
				policy, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		return agentapi.ClaimPolicy{}, nil, false
	}
	comparator, ok := registry.state.comparators[policy.ComparatorID]
	return policy, comparator, ok
}

func schemaKey(schema agentapi.SchemaRef) string {
	return fmt.Sprintf("%s@%d", schema.ID, schema.Version)
}

func canonicalFields(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type claimComparatorFunc func(context.Context, json.RawMessage, json.RawMessage) (bool, error)

func (fn claimComparatorFunc) Conflicts(
	ctx context.Context,
	left json.RawMessage,
	right json.RawMessage,
) (bool, error) {
	return fn(ctx, left, right)
}

func exclusiveScalar(
	_ context.Context,
	left json.RawMessage,
	right json.RawMessage,
) (bool, error) {
	var leftValue, rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false, err
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false, err
	}
	return !reflect.DeepEqual(leftValue, rightValue), nil
}

func booleanAssertion(
	_ context.Context,
	left json.RawMessage,
	right json.RawMessage,
) (bool, error) {
	var leftValue, rightValue bool
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false, err
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false, err
	}
	return leftValue != rightValue, nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
