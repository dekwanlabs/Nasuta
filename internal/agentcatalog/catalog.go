package agentcatalog

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
)

type key struct {
	id      string
	version int64
}

type state struct {
	revision    uint64
	definitions map[key]agentapi.Definition
	latest      map[string]int64
}

// Catalog atomically publishes immutable Agent Definition snapshots.
type Catalog struct {
	writeMu sync.Mutex
	state   atomic.Pointer[state]
}

func New() *Catalog {
	catalog := &Catalog{}
	catalog.state.Store(&state{
		definitions: make(map[key]agentapi.Definition),
		latest:      make(map[string]int64),
	})
	return catalog
}

func (catalog *Catalog) Replace(definitions []agentapi.Definition) error {
	return catalog.Publish(definitions)
}

// Publish adds immutable versions while retaining snapshots needed by active runs.
func (catalog *Catalog) Publish(definitions []agentapi.Definition) error {
	incoming := make(map[key]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		canonical, err := agentapi.Prepare(definition)
		if err != nil {
			return err
		}
		id := key{id: canonical.ID, version: canonical.Version}
		if _, duplicate := incoming[id]; duplicate {
			return fmt.Errorf("agent definition %q version %d is duplicated", canonical.ID, canonical.Version)
		}
		incoming[id] = canonical
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	prepared := make(map[key]agentapi.Definition, len(current.definitions)+len(incoming))
	latest := make(map[string]int64, len(current.latest))
	for id, definition := range current.definitions {
		prepared[id] = definition
	}
	for id, version := range current.latest {
		latest[id] = version
	}
	for id, definition := range incoming {
		if published, exists := prepared[id]; exists && published.ContentHash != definition.ContentHash {
			return fmt.Errorf("agent definition %q version %d is already published", id.id, id.version)
		}
		prepared[id] = definition
		if id.version > latest[id.id] {
			latest[id.id] = id.version
		}
	}
	catalog.state.Store(&state{
		revision: current.revision + 1, definitions: prepared, latest: latest,
	})
	return nil
}

func (catalog *Catalog) Resolve(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
	current := catalog.state.Load()
	version := ref.Version
	if version == 0 {
		version = current.latest[ref.ID]
	}
	definition, ok := current.definitions[key{id: ref.ID, version: version}]
	if !ok {
		return agentapi.Definition{}, fmt.Errorf("agent definition %q version %d not found", ref.ID, version)
	}
	return clone(definition), nil
}

func (catalog *Catalog) List() []agentapi.Definition {
	current := catalog.state.Load()
	out := make([]agentapi.Definition, 0, len(current.definitions))
	for _, definition := range current.definitions {
		out = append(out, clone(definition))
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

func DefaultQA(settings *config.PlatformSettings) (agentapi.Definition, error) {
	return DefaultQAVersion(settings, 1)
}

func DefaultQAVersion(settings *config.PlatformSettings, version int64) (agentapi.Definition, error) {
	systemPrompt := settings.DomainKnowledge
	if systemPrompt == "" {
		systemPrompt = "Answer the request using only attributable evidence and the available tools."
	}
	return agentapi.Prepare(agentapi.Definition{
		ID: "qa.answerer", Version: version, DisplayName: "QA Answerer",
		Purpose: "Answer questions using bounded, attributable evidence.",
		Prompt: agentapi.PromptSpec{
			System:  systemPrompt,
			Version: "qa-loop-v1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "qa.answer", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens: settings.LLMAnswerMaxTokens,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout:       time.Duration(settings.AgentTimeout),
			MaxSteps:      settings.AgentMaxSteps,
			ContextTokens: settings.LLMContextWindow,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
}

func clone(definition agentapi.Definition) agentapi.Definition {
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
