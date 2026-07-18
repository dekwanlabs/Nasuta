package tool

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type registryState struct {
	revision uint64
	tools    map[ToolID]Tool
	order    []ToolID
}

// Registry atomically publishes immutable tool catalogs.
type Registry struct {
	writeMu sync.Mutex
	state   atomic.Pointer[registryState]
}

// ReadRegistry publishes only read tools into a private registry.
type ReadRegistry struct {
	registry *Registry
}

// NewReadRegistry creates the restricted publisher exposed to compositions.
func NewReadRegistry(registry *Registry) *ReadRegistry {
	return &ReadRegistry{registry: registry}
}

// Register publishes one read tool.
func (publisher *ReadRegistry) Register(candidate ReadTool) error {
	return publisher.RegisterAll([]ReadTool{candidate})
}

// RegisterAll publishes one atomic batch of read tools.
func (publisher *ReadRegistry) RegisterAll(candidates []ReadTool) error {
	if publisher == nil || publisher.registry == nil {
		return fmt.Errorf("register read tools: registry is required")
	}
	tools := make([]Tool, 0, len(candidates))
	for _, candidate := range candidates {
		tools = append(tools, candidate.tool())
	}
	return publisher.registry.RegisterAll(tools)
}

// Replace replaces one existing read tool without crossing the kind boundary.
func (publisher *ReadRegistry) Replace(candidate ReadTool) error {
	return publisher.ReplaceAll([]ReadTool{candidate})
}

// ReplaceAll atomically replaces one batch without crossing the kind boundary.
func (publisher *ReadRegistry) ReplaceAll(candidates []ReadTool) error {
	if publisher == nil || publisher.registry == nil {
		return fmt.Errorf("replace read tools: registry is required")
	}
	tools := make([]Tool, 0, len(candidates))
	for _, candidate := range candidates {
		tools = append(tools, candidate.tool())
	}
	return publisher.registry.replaceReadAll(tools)
}

// Unregister removes one read tool without allowing access to write IDs.
func (publisher *ReadRegistry) Unregister(id ToolID) error {
	return publisher.UnregisterAll([]ToolID{id})
}

// UnregisterAll atomically removes read IDs without allowing access to writes.
func (publisher *ReadRegistry) UnregisterAll(ids []ToolID) error {
	if publisher == nil || publisher.registry == nil {
		return fmt.Errorf("unregister read tools: registry is required")
	}
	return publisher.registry.unregisterReadAll(ids)
}

// Contains reports whether a read tool ID is currently published.
func (publisher *ReadRegistry) Contains(id ToolID) bool {
	if publisher == nil || publisher.registry == nil {
		return false
	}
	_, ok := publisher.registry.Snapshot(ReadPolicy()).Get(id)
	return ok
}

// NewRegistry starts with an empty catalog.
func NewRegistry() *Registry {
	registry := &Registry{}
	registry.state.Store(&registryState{tools: map[ToolID]Tool{}})
	return registry
}

// Register adds one tool without replacing an existing ID.
func (registry *Registry) Register(candidate Tool) error {
	return registry.RegisterAll([]Tool{candidate})
}

// RegisterAll publishes the whole batch or leaves the catalog unchanged.
func (registry *Registry) RegisterAll(candidates []Tool) error {
	if len(candidates) == 0 {
		return nil
	}
	prepared, err := prepareBatch(candidates)
	if err != nil {
		return err
	}
	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()

	current := registry.load()
	next := cloneState(current)
	for _, candidate := range prepared {
		if _, exists := next.tools[candidate.ID]; exists {
			return fmt.Errorf("register tools: id %q already exists", candidate.ID)
		}
		next.tools[candidate.ID] = candidate
		next.order = append(next.order, candidate.ID)
	}
	registry.publish(current, next)
	return nil
}

// Replace swaps one existing tool by ID.
func (registry *Registry) Replace(candidate Tool) error {
	return registry.ReplaceAll([]Tool{candidate})
}

func (registry *Registry) replaceReadAll(candidates []Tool) error {
	if len(candidates) == 0 {
		return nil
	}
	prepared, err := prepareBatch(candidates)
	if err != nil {
		return err
	}
	for _, candidate := range prepared {
		if candidate.Kind != KindRead {
			return fmt.Errorf("replace read tool %q: read kind is required", candidate.ID)
		}
	}
	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()

	current := registry.load()
	for _, candidate := range prepared {
		existing, exists := current.tools[candidate.ID]
		if !exists {
			return fmt.Errorf("replace read tool: id %q is not registered", candidate.ID)
		}
		if existing.Kind != KindRead {
			return fmt.Errorf("replace read tool: id %q is not a read tool", candidate.ID)
		}
	}
	next := cloneState(current)
	for _, candidate := range prepared {
		next.tools[candidate.ID] = candidate
	}
	registry.publish(current, next)
	return nil
}

// ReplaceAll publishes the whole replacement batch atomically.
func (registry *Registry) ReplaceAll(candidates []Tool) error {
	if len(candidates) == 0 {
		return nil
	}
	prepared, err := prepareBatch(candidates)
	if err != nil {
		return err
	}
	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()

	current := registry.load()
	next := cloneState(current)
	for _, candidate := range prepared {
		if _, exists := next.tools[candidate.ID]; !exists {
			return fmt.Errorf("replace tools: id %q is not registered", candidate.ID)
		}
		next.tools[candidate.ID] = candidate
	}
	registry.publish(current, next)
	return nil
}

// Unregister removes one existing tool.
func (registry *Registry) Unregister(id ToolID) error {
	return registry.UnregisterAll([]ToolID{id})
}

func (registry *Registry) unregisterReadAll(ids []ToolID) error {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[ToolID]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("unregister read tool: id is required")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("unregister read tools: duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()

	current := registry.load()
	for _, id := range ids {
		candidate, exists := current.tools[id]
		if !exists {
			return fmt.Errorf("unregister read tool: id %q is not registered", id)
		}
		if candidate.Kind != KindRead {
			return fmt.Errorf("unregister read tool: id %q is not a read tool", id)
		}
	}
	next := cloneState(current)
	for _, id := range ids {
		delete(next.tools, id)
	}
	next.order = next.order[:0]
	for _, currentID := range current.order {
		if _, removed := seen[currentID]; !removed {
			next.order = append(next.order, currentID)
		}
	}
	registry.publish(current, next)
	return nil
}

// UnregisterAll removes the whole batch atomically.
func (registry *Registry) UnregisterAll(ids []ToolID) error {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[ToolID]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("unregister tools: id is required")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("unregister tools: duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}

	registry.writeMu.Lock()
	defer registry.writeMu.Unlock()
	current := registry.load()
	for _, id := range ids {
		if _, exists := current.tools[id]; !exists {
			return fmt.Errorf("unregister tools: id %q is not registered", id)
		}
	}
	next := cloneState(current)
	for _, id := range ids {
		delete(next.tools, id)
	}
	next.order = next.order[:0]
	for _, id := range current.order {
		if _, removed := seen[id]; !removed {
			next.order = append(next.order, id)
		}
	}
	registry.publish(current, next)
	return nil
}

// Snapshot pins definitions and handlers to one registry revision.
func (registry *Registry) Snapshot(policy Policy) Snapshot {
	current := registry.load()
	tools := make(map[ToolID]Tool, len(current.tools))
	order := make([]ToolID, 0, len(current.order))
	for _, id := range current.order {
		candidate := cloneTool(current.tools[id])
		if !policy.Allows(candidate) {
			continue
		}
		tools[id] = candidate
		order = append(order, id)
	}
	return Snapshot{revision: current.revision, tools: tools, order: order}
}

// Revision reports the current published catalog revision.
func (registry *Registry) Revision() uint64 {
	return registry.load().revision
}

func (registry *Registry) load() *registryState {
	if registry == nil {
		return &registryState{tools: map[ToolID]Tool{}}
	}
	current := registry.state.Load()
	if current == nil {
		return &registryState{tools: map[ToolID]Tool{}}
	}
	return current
}

func (registry *Registry) publish(current, next *registryState) {
	next.revision = current.revision + 1
	registry.state.Store(next)
}

func cloneState(current *registryState) *registryState {
	tools := make(map[ToolID]Tool, len(current.tools))
	for id, candidate := range current.tools {
		tools[id] = candidate
	}
	return &registryState{
		revision: current.revision,
		tools:    tools,
		order:    append([]ToolID(nil), current.order...),
	}
}

func prepareBatch(candidates []Tool) ([]Tool, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	prepared := make([]Tool, len(candidates))
	seen := make(map[ToolID]struct{}, len(candidates))
	for i, candidate := range candidates {
		if err := validateTool(candidate); err != nil {
			return nil, err
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			return nil, fmt.Errorf("tool batch contains duplicate id %q", candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
		candidate.InputSchema = cloneSchema(candidate.InputSchema)
		if candidate.Routing != nil {
			spec := *candidate.Routing
			candidate.Routing = &spec
		}
		if candidate.Prefetch != nil {
			spec := *candidate.Prefetch
			candidate.Prefetch = &spec
		}
		prepared[i] = candidate
	}
	return prepared, nil
}

func cloneSchema(schema JSONSchema) JSONSchema {
	return cloneMap(map[string]any(schema))
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		switch nested := value.(type) {
		case map[string]any:
			out[key] = cloneMap(nested)
		case []any:
			items := make([]any, len(nested))
			for i, item := range nested {
				if object, ok := item.(map[string]any); ok {
					items[i] = cloneMap(object)
				} else {
					items[i] = item
				}
			}
			out[key] = items
		case []string:
			out[key] = append([]string(nil), nested...)
		default:
			out[key] = value
		}
	}
	return out
}

// Snapshot is immutable for the lifetime of one Agent or MCP operation.
type Snapshot struct {
	revision uint64
	tools    map[ToolID]Tool
	order    []ToolID
}

func (snapshot Snapshot) Revision() uint64 {
	return snapshot.revision
}

func (snapshot Snapshot) Get(id ToolID) (Tool, bool) {
	candidate, ok := snapshot.tools[id]
	if !ok {
		return Tool{}, false
	}
	return cloneTool(candidate), true
}

func (snapshot Snapshot) Tools() []Tool {
	out := make([]Tool, 0, len(snapshot.order))
	for _, id := range snapshot.order {
		out = append(out, cloneTool(snapshot.tools[id]))
	}
	return out
}

func (snapshot Snapshot) MCPTools() []Tool {
	out := make([]Tool, 0, len(snapshot.order))
	for _, id := range snapshot.order {
		candidate := snapshot.tools[id]
		if !candidate.MCPHidden {
			out = append(out, cloneTool(candidate))
		}
	}
	return out
}

// Select returns a filtered view while preserving the pinned revision.
func (snapshot Snapshot) Select(ids map[ToolID]struct{}) Snapshot {
	tools := make(map[ToolID]Tool, len(ids))
	order := make([]ToolID, 0, len(ids))
	for _, id := range snapshot.order {
		if _, selected := ids[id]; !selected {
			continue
		}
		tools[id] = cloneTool(snapshot.tools[id])
		order = append(order, id)
	}
	return Snapshot{revision: snapshot.revision, tools: tools, order: order}
}

func cloneTool(candidate Tool) Tool {
	candidate.InputSchema = cloneSchema(candidate.InputSchema)
	if candidate.Routing != nil {
		spec := *candidate.Routing
		candidate.Routing = &spec
	}
	if candidate.Prefetch != nil {
		spec := *candidate.Prefetch
		candidate.Prefetch = &spec
	}
	return candidate
}
