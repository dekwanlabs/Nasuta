package agentworkflow

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

type DefinitionRef struct {
	ID      string
	Version int64
}

type definitionKey struct {
	id      string
	version int64
}

type catalogState struct {
	revision    uint64
	definitions map[definitionKey]WorkflowDefinition
	latest      map[string]int64
}

// Catalog publishes immutable workflow snapshots without disrupting active runs.
type Catalog struct {
	writeMu sync.Mutex
	state   atomic.Pointer[catalogState]
}

func NewCatalog() *Catalog {
	catalog := &Catalog{}
	catalog.state.Store(&catalogState{
		definitions: make(map[definitionKey]WorkflowDefinition),
		latest:      make(map[string]int64),
	})
	return catalog
}

func (catalog *Catalog) Publish(definitions []WorkflowDefinition) error {
	incoming := make(map[definitionKey]WorkflowDefinition, len(definitions))
	for _, definition := range definitions {
		prepared, err := Prepare(definition)
		if err != nil {
			return err
		}
		key := definitionKey{id: prepared.ID, version: prepared.Version}
		if _, duplicate := incoming[key]; duplicate {
			return fmt.Errorf("workflow %q version %d is duplicated", prepared.ID, prepared.Version)
		}
		incoming[key] = prepared
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	next := &catalogState{
		revision:    current.revision + 1,
		definitions: make(map[definitionKey]WorkflowDefinition, len(current.definitions)+len(incoming)),
		latest:      make(map[string]int64, len(current.latest)),
	}
	for key, definition := range current.definitions {
		next.definitions[key] = definition
	}
	for id, version := range current.latest {
		next.latest[id] = version
	}
	for key, definition := range incoming {
		if published, exists := next.definitions[key]; exists && published.ContentHash != definition.ContentHash {
			return fmt.Errorf("workflow %q version %d is already published", key.id, key.version)
		}
		next.definitions[key] = definition
		if key.version > next.latest[key.id] {
			next.latest[key.id] = key.version
		}
	}
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) Resolve(ref DefinitionRef) (WorkflowDefinition, error) {
	current := catalog.state.Load()
	version := ref.Version
	if version == 0 {
		version = current.latest[ref.ID]
	}
	definition, ok := current.definitions[definitionKey{id: ref.ID, version: version}]
	if !ok {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q version %d not found", ref.ID, version)
	}
	return cloneDefinition(definition), nil
}

func (catalog *Catalog) List() []WorkflowDefinition {
	current := catalog.state.Load()
	out := make([]WorkflowDefinition, 0, len(current.definitions))
	for _, definition := range current.definitions {
		out = append(out, cloneDefinition(definition))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (catalog *Catalog) Revision() uint64 {
	return catalog.state.Load().revision
}
