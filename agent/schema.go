package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/jsonschema-go/jsonschema"
)

type schemaKey struct {
	id      string
	version int64
}

// SchemaDefinition is one immutable JSON Schema contract and its explicit compatibility claims.
type SchemaDefinition struct {
	ID       string          `json:"id"`
	Version  int64           `json:"version"`
	Document json.RawMessage `json:"document"`
	// CompatibleFrom is an explicit consumer claim, never inferred structurally.
	CompatibleFrom []SchemaRef `json:"compatible_from,omitempty"`
	// ContentHash prevents a published version from being redefined.
	ContentHash string `json:"content_hash"`
}

type compiledSchema struct {
	definition SchemaDefinition
	resolved   *jsonschema.Resolved
	compatible map[schemaKey]struct{}
}

type schemaState struct {
	revision uint64
	schemas  map[schemaKey]compiledSchema
}

// SchemaRegistry atomically publishes compiled contracts used by Agents and Workflows.
type SchemaRegistry struct {
	writeMu sync.Mutex
	state   atomic.Pointer[schemaState]
}

// NewSchemaRegistry creates an empty registry whose readers observe immutable snapshots.
func NewSchemaRegistry() *SchemaRegistry {
	registry := &SchemaRegistry{}
	registry.state.Store(&schemaState{schemas: make(map[schemaKey]compiledSchema)})
	return registry
}

// PrepareSchema validates, compiles, and hashes a detached immutable definition.
func PrepareSchema(definition SchemaDefinition) (SchemaDefinition, error) {
	prepared, _, err := compileSchema(definition)
	return prepared, err
}

// Publish adds an all-or-nothing batch while retaining versions pinned by active runs.
func (registry *SchemaRegistry) Publish(definitions []SchemaDefinition) error {
	if registry == nil {
		return fmt.Errorf("schema registry is required")
	}
	incoming := make(map[schemaKey]compiledSchema, len(definitions))
	for _, definition := range definitions {
		prepared, resolved, err := compileSchema(definition)
		if err != nil {
			return err
		}
		key := schemaKey{id: prepared.ID, version: prepared.Version}
		if _, duplicate := incoming[key]; duplicate {
			return fmt.Errorf("schema %q version %d is duplicated", prepared.ID, prepared.Version)
		}
		compatible := make(map[schemaKey]struct{}, len(prepared.CompatibleFrom))
		for _, ref := range prepared.CompatibleFrom {
			compatible[schemaKey{id: ref.ID, version: ref.Version}] = struct{}{}
		}
		incoming[key] = compiledSchema{
			definition: prepared, resolved: resolved, compatible: compatible,
		}
	}

	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()
	current := registry.state.Load()
	next := make(map[schemaKey]compiledSchema, len(current.schemas)+len(incoming))
	for key, schema := range current.schemas {
		next[key] = schema
	}
	for key, schema := range incoming {
		if published, exists := next[key]; exists &&
			published.definition.ContentHash != schema.definition.ContentHash {
			return fmt.Errorf("schema %q version %d is already published", key.id, key.version)
		}
		next[key] = schema
	}
	for key, schema := range incoming {
		for compatible := range schema.compatible {
			if _, exists := next[compatible]; !exists {
				return fmt.Errorf(
					"schema %q version %d compatibility reference %q version %d not found",
					key.id, key.version, compatible.id, compatible.version,
				)
			}
		}
	}
	registry.state.Store(&schemaState{revision: current.revision + 1, schemas: next})
	return nil
}

// Resolve returns a detached copy of one exact Schema version.
func (registry *SchemaRegistry) Resolve(ref SchemaRef) (SchemaDefinition, error) {
	schema, err := registry.resolve(ref)
	if err != nil {
		return SchemaDefinition{}, err
	}
	return cloneSchema(schema.definition), nil
}

// Validate decodes one JSON value and validates it against an exact Schema version.
func (registry *SchemaRegistry) Validate(ref SchemaRef, payload json.RawMessage) error {
	schema, err := registry.resolve(ref)
	if err != nil {
		return err
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("schema %q version %d payload is not valid JSON", ref.ID, ref.Version)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var instance any
	if err := decoder.Decode(&instance); err != nil {
		return fmt.Errorf("decode schema %q version %d payload: %w", ref.ID, ref.Version, err)
	}
	if err := schema.resolved.Validate(instance); err != nil {
		return fmt.Errorf("validate schema %q version %d payload: %w", ref.ID, ref.Version, err)
	}
	return nil
}

// ValidateCompatibility requires identity or an explicit claim by the consumer Schema.
func (registry *SchemaRegistry) ValidateCompatibility(producer, consumer SchemaRef) error {
	if _, err := registry.resolve(producer); err != nil {
		return fmt.Errorf("producer schema: %w", err)
	}
	consumerSchema, err := registry.resolve(consumer)
	if err != nil {
		return fmt.Errorf("consumer schema: %w", err)
	}
	if producer == consumer {
		return nil
	}
	if _, ok := consumerSchema.compatible[schemaKey{id: producer.ID, version: producer.Version}]; ok {
		return nil
	}
	return fmt.Errorf(
		"schema %q version %d is not compatible with consumer %q version %d",
		producer.ID, producer.Version, consumer.ID, consumer.Version,
	)
}

// Revision changes only after an atomic schema publication.
func (registry *SchemaRegistry) Revision() uint64 {
	if registry == nil || registry.state.Load() == nil {
		return 0
	}
	return registry.state.Load().revision
}

func (registry *SchemaRegistry) resolve(ref SchemaRef) (compiledSchema, error) {
	if registry == nil || registry.state.Load() == nil {
		return compiledSchema{}, fmt.Errorf("schema registry is unavailable")
	}
	if err := validateSchemaRef("schema", ref); err != nil {
		return compiledSchema{}, err
	}
	schema, ok := registry.state.Load().schemas[schemaKey{id: ref.ID, version: ref.Version}]
	if !ok {
		return compiledSchema{}, fmt.Errorf("schema %q version %d not found", ref.ID, ref.Version)
	}
	return schema, nil
}

func compileSchema(definition SchemaDefinition) (SchemaDefinition, *jsonschema.Resolved, error) {
	prepared := cloneSchema(definition)
	prepared.ID = strings.TrimSpace(prepared.ID)
	if !canonicalID.MatchString(prepared.ID) {
		return SchemaDefinition{}, nil, fmt.Errorf("schema id %q is not canonical", definition.ID)
	}
	if prepared.Version <= 0 {
		return SchemaDefinition{}, nil, fmt.Errorf("schema %q version must be positive", prepared.ID)
	}
	document, err := canonicalJSON(prepared.Document)
	if err != nil {
		return SchemaDefinition{}, nil, fmt.Errorf("schema %q version %d document: %w", prepared.ID, prepared.Version, err)
	}
	prepared.Document = document
	seen := make(map[schemaKey]struct{}, len(prepared.CompatibleFrom))
	for _, ref := range prepared.CompatibleFrom {
		if err := validateSchemaRef("compatible", ref); err != nil {
			return SchemaDefinition{}, nil, fmt.Errorf("schema %q version %d: %w", prepared.ID, prepared.Version, err)
		}
		key := schemaKey{id: ref.ID, version: ref.Version}
		if _, duplicate := seen[key]; duplicate {
			return SchemaDefinition{}, nil, fmt.Errorf(
				"schema %q version %d duplicates compatibility reference %q version %d",
				prepared.ID, prepared.Version, ref.ID, ref.Version,
			)
		}
		seen[key] = struct{}{}
	}
	sort.Slice(prepared.CompatibleFrom, func(i, j int) bool {
		if prepared.CompatibleFrom[i].ID == prepared.CompatibleFrom[j].ID {
			return prepared.CompatibleFrom[i].Version < prepared.CompatibleFrom[j].Version
		}
		return prepared.CompatibleFrom[i].ID < prepared.CompatibleFrom[j].ID
	})
	var schema jsonschema.Schema
	if err := json.Unmarshal(prepared.Document, &schema); err != nil {
		return SchemaDefinition{}, nil, fmt.Errorf("parse JSON Schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return SchemaDefinition{}, nil, fmt.Errorf("compile JSON Schema: %w", err)
	}
	hash, err := schemaHash(prepared)
	if err != nil {
		return SchemaDefinition{}, nil, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return SchemaDefinition{}, nil, fmt.Errorf("schema %q version %d content hash mismatch", prepared.ID, prepared.Version)
	}
	prepared.ContentHash = hash
	return prepared, resolved, nil
}

func validateSchemaRef(label string, ref SchemaRef) error {
	if !canonicalID.MatchString(strings.TrimSpace(ref.ID)) || ref.ID != strings.TrimSpace(ref.ID) {
		return fmt.Errorf("%s schema id %q is not canonical", label, ref.ID)
	}
	if ref.Version <= 0 {
		return fmt.Errorf("%s schema %q version must be positive", label, ref.ID)
	}
	return nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("must be valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func schemaHash(definition SchemaDefinition) (string, error) {
	definition.ContentHash = ""
	payload, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("marshal schema %q version %d: %w", definition.ID, definition.Version, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func cloneSchema(definition SchemaDefinition) SchemaDefinition {
	definition.Document = append(json.RawMessage(nil), definition.Document...)
	definition.CompatibleFrom = append([]SchemaRef(nil), definition.CompatibleFrom...)
	return definition
}
