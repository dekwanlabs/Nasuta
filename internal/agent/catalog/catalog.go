package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

var (
	ErrInvalid     = errors.New("agent catalog input invalid")
	ErrNotFound    = errors.New("agent catalog resource not found")
	ErrConflict    = errors.New("agent catalog conflict")
	ErrUnavailable = errors.New("agent catalog unavailable")
)

type key struct {
	id      string
	version int64
}

// DefinitionRecord adds mutable rollout metadata around an immutable definition.
type DefinitionRecord struct {
	agentapi.Definition
	Active    bool      `json:"active"`
	Default   bool      `json:"default"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type DefinitionCursor struct {
	ID      string
	Version int64
}

type AuditEvent struct {
	Seq          int64     `json:"seq"`
	DefinitionID string    `json:"definition_id"`
	Version      int64     `json:"version"`
	Action       string    `json:"action"`
	ActorUserID  int64     `json:"actor_user_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type catalogPersistence interface {
	Publish(context.Context, []agentapi.Definition, int64) ([]DefinitionRecord, error)
	LoadFullCatalog(context.Context) ([]DefinitionRecord, error)
	LoadRollouts(context.Context) ([]RolloutRule, error)
	ListDefinitions(context.Context, DefinitionCursor, int) ([]DefinitionRecord, error)
	SetDefault(context.Context, string, int64, int64) error
	SetActive(context.Context, string, int64, bool, int64) error
	SetRollout(context.Context, RolloutRule, int64) error
	ListAudit(context.Context, string, int64, int) ([]AuditEvent, error)
	ListRolloutAudit(context.Context, string, int64, int) ([]RolloutAuditEvent, error)
}

type state struct {
	revision         uint64
	records          map[key]DefinitionRecord
	highest          map[string]int64
	defaults         map[string]int64
	rollouts         map[string]RolloutRule
	audit            []AuditEvent
	nextAudit        int64
	rolloutAudit     []RolloutAuditEvent
	nextRolloutAudit int64
}

// Catalog atomically publishes immutable Agent Definition snapshots.
type Catalog struct {
	writeMu sync.Mutex
	state   atomic.Pointer[state]
	schemas *agentapi.SchemaRegistry
	store   catalogPersistence
}

func New(schemas *agentapi.SchemaRegistry) *Catalog {
	catalog := &Catalog{schemas: schemas}
	catalog.state.Store(&state{
		records:  make(map[key]DefinitionRecord),
		highest:  make(map[string]int64),
		defaults: make(map[string]int64),
		rollouts: make(map[string]RolloutRule),
	})
	return catalog
}

func (catalog *Catalog) Replace(definitions []agentapi.Definition) error {
	return catalog.Publish(definitions)
}

// Publish adds immutable versions while retaining snapshots needed by active runs.
func (catalog *Catalog) Publish(definitions []agentapi.Definition) error {
	return catalog.PublishAs(context.Background(), definitions, 0)
}

func (catalog *Catalog) PublishAs(
	ctx context.Context,
	definitions []agentapi.Definition,
	actorUserID int64,
) error {
	prepared, err := catalog.prepare(definitions)
	if err != nil {
		return err
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	records := make([]DefinitionRecord, 0, len(prepared))
	if catalog.store != nil {
		records, err = catalog.store.Publish(ctx, prepared, actorUserID)
		if err != nil {
			return err
		}
	} else {
		now := time.Now().UTC()
		for _, definition := range prepared {
			records = append(records, DefinitionRecord{
				Definition: definition,
				Active:     true,
				CreatedBy:  actorUserID,
				CreatedAt:  now,
			})
		}
	}
	current := catalog.state.Load()
	next := cloneState(current)
	for _, record := range records {
		id := key{id: record.ID, version: record.Version}
		if published, exists := next.records[id]; exists {
			if published.ContentHash != record.ContentHash {
				return fmt.Errorf(
					"agent definition %q version %d is already published: %w",
					id.id, id.version, ErrConflict,
				)
			}
			continue
		}
		if record.Default ||
			(catalog.store == nil && record.Version > next.defaults[record.ID]) {
			clearDefault(next, record.ID)
			record.Default = true
			next.defaults[record.ID] = record.Version
		}
		next.records[id] = cloneRecord(record)
		if record.Version > next.highest[record.ID] {
			next.highest[record.ID] = record.Version
		}
		if catalog.store == nil {
			appendMemoryAudit(next, record.ID, record.Version, "published", actorUserID, record.CreatedAt)
		}
	}
	next.revision++
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) prepare(
	definitions []agentapi.Definition,
) ([]agentapi.Definition, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("agent definitions are required: %w", ErrInvalid)
	}
	incoming := make(map[key]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		canonical, err := agentapi.Prepare(definition)
		if err != nil {
			return nil, fmt.Errorf("%v: %w", err, ErrInvalid)
		}
		if err := scope.ValidateAgentRuntime(canonical.Permissions.Scopes); err != nil {
			return nil, fmt.Errorf(
				"publish agent definition %q permissions: %v: %w",
				canonical.ID, err, ErrInvalid,
			)
		}
		if catalog.schemas == nil {
			return nil, fmt.Errorf(
				"publish agent definition %q: schema registry is required: %w",
				canonical.ID, ErrUnavailable,
			)
		}
		if _, err := catalog.schemas.Resolve(canonical.InputSchema); err != nil {
			return nil, fmt.Errorf(
				"publish agent definition %q input schema: %v: %w",
				canonical.ID, err, ErrInvalid,
			)
		}
		if _, err := catalog.schemas.Resolve(canonical.OutputSchema); err != nil {
			return nil, fmt.Errorf(
				"publish agent definition %q output schema: %v: %w",
				canonical.ID, err, ErrInvalid,
			)
		}
		id := key{id: canonical.ID, version: canonical.Version}
		if _, duplicate := incoming[id]; duplicate {
			return nil, fmt.Errorf(
				"agent definition %q version %d is duplicated: %w",
				canonical.ID, canonical.Version, ErrInvalid,
			)
		}
		incoming[id] = canonical
	}
	prepared := make([]agentapi.Definition, 0, len(incoming))
	for _, definition := range incoming {
		prepared = append(prepared, definition)
	}
	sort.Slice(prepared, func(i, j int) bool {
		if prepared[i].ID == prepared[j].ID {
			return prepared[i].Version < prepared[j].Version
		}
		return prepared[i].ID < prepared[j].ID
	})
	return prepared, nil
}

func (catalog *Catalog) Resolve(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
	definition, _, err := catalog.resolve(ref, "", false)
	return definition, err
}

// ResolveFor applies the active rollout rule when the reference is unpinned.
func (catalog *Catalog) ResolveFor(
	ref agentapi.DefinitionRef,
	stableKey string,
) (agentapi.Definition, agentapi.DefinitionSelection, error) {
	return catalog.resolve(ref, stableKey, true)
}

func (catalog *Catalog) resolve(
	ref agentapi.DefinitionRef,
	stableKey string,
	applyRollout bool,
) (agentapi.Definition, agentapi.DefinitionSelection, error) {
	current := catalog.state.Load()
	version := ref.Version
	selection := agentapi.DefinitionSelection{Reason: "explicit_version"}
	if version == 0 {
		version = current.defaults[ref.ID]
		selection.Reason = "default"
		if applyRollout {
			if rule, ok := current.rollouts[ref.ID]; ok && rule.Active {
				candidate, candidateOK := current.records[key{id: ref.ID, version: rule.CandidateVersion}]
				if !candidateOK {
					return agentapi.Definition{}, agentapi.DefinitionSelection{}, fmt.Errorf(
						"agent rollout for %q candidate version %d not found: %w",
						ref.ID, rule.CandidateVersion, ErrConflict,
					)
				}
				if !candidate.Active {
					return agentapi.Definition{}, agentapi.DefinitionSelection{}, fmt.Errorf(
						"agent rollout for %q candidate version %d is disabled: %w",
						ref.ID, rule.CandidateVersion, ErrConflict,
					)
				}
				var err error
				selection, candidate, err = catalog.rolloutSelection(
					current, rule, stableKey, current.defaults[ref.ID],
				)
				if err != nil {
					return agentapi.Definition{}, agentapi.DefinitionSelection{}, err
				}
				version = candidate.Version
			}
		}
	}
	record, ok := current.records[key{id: ref.ID, version: version}]
	if !ok {
		return agentapi.Definition{}, agentapi.DefinitionSelection{}, fmt.Errorf(
			"agent definition %q version %d not found: %w",
			ref.ID, version, ErrNotFound,
		)
	}
	if ref.Version == 0 && version == current.defaults[ref.ID] && !record.Active {
		return agentapi.Definition{}, agentapi.DefinitionSelection{}, fmt.Errorf(
			"agent definition %q default version %d is disabled: %w",
			ref.ID, version, ErrConflict,
		)
	}
	return clone(record.Definition), selection, nil
}

func (catalog *Catalog) rolloutSelection(
	current *state,
	rule RolloutRule,
	stableKey string,
	defaultVersion int64,
) (agentapi.DefinitionSelection, DefinitionRecord, error) {
	selection, candidate, err := selectRollout(rule, stableKey)
	if err != nil {
		return agentapi.DefinitionSelection{}, DefinitionRecord{}, err
	}
	if !candidate {
		record, ok := current.records[key{id: rule.AgentID, version: defaultVersion}]
		if !ok || !record.Active {
			return agentapi.DefinitionSelection{}, DefinitionRecord{}, fmt.Errorf(
				"agent definition %q active default version %d is unavailable: %w",
				rule.AgentID, defaultVersion, ErrConflict,
			)
		}
		return selection, record, nil
	}
	record, ok := current.records[key{id: rule.AgentID, version: rule.CandidateVersion}]
	if !ok || !record.Active {
		return agentapi.DefinitionSelection{}, DefinitionRecord{}, fmt.Errorf(
			"agent definition %q rollout candidate version %d is unavailable: %w",
			rule.AgentID, rule.CandidateVersion, ErrConflict,
		)
	}
	return selection, record, nil
}

func (catalog *Catalog) List() []agentapi.Definition {
	current := catalog.state.Load()
	out := make([]agentapi.Definition, 0, len(current.records))
	for _, record := range current.records {
		out = append(out, clone(record.Definition))
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

func (catalog *Catalog) MaxVersion() int64 {
	current := catalog.state.Load()
	var version int64
	for _, candidate := range current.highest {
		if candidate > version {
			version = candidate
		}
	}
	return version
}

// AttachStore loads persisted definitions before the catalog becomes observable.
func (catalog *Catalog) AttachStore(
	ctx context.Context,
	store catalogPersistence,
) error {
	if store == nil {
		return fmt.Errorf("agent catalog store is required: %w", ErrUnavailable)
	}
	records, err := store.LoadFullCatalog(ctx)
	if err != nil {
		return err
	}
	next := &state{
		records:  make(map[key]DefinitionRecord, len(records)),
		highest:  make(map[string]int64),
		defaults: make(map[string]int64),
		rollouts: make(map[string]RolloutRule),
	}
	for _, record := range records {
		prepared, prepareErr := catalog.prepare([]agentapi.Definition{record.Definition})
		if prepareErr != nil {
			return fmt.Errorf(
				"load agent definition %q version %d: %w",
				record.ID, record.Version, prepareErr,
			)
		}
		record.Definition = prepared[0]
		id := key{id: record.ID, version: record.Version}
		next.records[id] = cloneRecord(record)
		if record.Version > next.highest[record.ID] {
			next.highest[record.ID] = record.Version
		}
		if record.Default {
			if next.defaults[record.ID] != 0 {
				return fmt.Errorf(
					"agent definition %q has multiple defaults: %w",
					record.ID, ErrConflict,
				)
			}
			if !record.Active {
				return fmt.Errorf(
					"agent definition %q default version %d is disabled: %w",
					record.ID, record.Version, ErrConflict,
				)
			}
			next.defaults[record.ID] = record.Version
		}
	}
	rollouts, err := store.LoadRollouts(ctx)
	if err != nil {
		return err
	}
	for _, rule := range rollouts {
		preparedRule, prepareErr := prepareRolloutRule(rule)
		if prepareErr != nil {
			return fmt.Errorf("load agent rollout %q: %w", rule.AgentID, prepareErr)
		}
		if _, ok := next.records[key{id: rule.AgentID, version: rule.CandidateVersion}]; !ok {
			return fmt.Errorf(
				"load agent rollout %q candidate version %d not found: %w",
				rule.AgentID, rule.CandidateVersion, ErrConflict,
			)
		}
		if preparedRule.Active {
			candidate := next.records[key{id: rule.AgentID, version: rule.CandidateVersion}]
			if !candidate.Active {
				return fmt.Errorf(
					"load agent rollout %q candidate version %d is disabled: %w",
					rule.AgentID, rule.CandidateVersion, ErrConflict,
				)
			}
		}
		next.rollouts[rule.AgentID] = preparedRule
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	catalog.store = store
	next.revision = catalog.state.Load().revision + 1
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) ListRecords(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("agent definition limit must be positive: %w", ErrInvalid)
	}
	if catalog.store != nil {
		return catalog.store.ListDefinitions(ctx, cursor, limit)
	}
	current := catalog.state.Load()
	records := make([]DefinitionRecord, 0, len(current.records))
	for _, record := range current.records {
		if record.ID < cursor.ID ||
			(record.ID == cursor.ID && record.Version <= cursor.Version) {
			continue
		}
		records = append(records, cloneRecord(record))
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ID == records[j].ID {
			return records[i].Version < records[j].Version
		}
		return records[i].ID < records[j].ID
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (catalog *Catalog) SetDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
) error {
	return catalog.updateControl(ctx, id, version, actorUserID, func(next *state, record DefinitionRecord) error {
		if !record.Active {
			return fmt.Errorf(
				"agent definition %q version %d is disabled: %w",
				id, version, ErrConflict,
			)
		}
		if catalog.store != nil {
			if err := catalog.store.SetDefault(ctx, id, version, actorUserID); err != nil {
				return err
			}
		}
		clearDefault(next, id)
		record.Default = true
		next.records[key{id: id, version: version}] = record
		next.defaults[id] = version
		if catalog.store == nil {
			appendMemoryAudit(next, id, version, "default_set", actorUserID, time.Now().UTC())
		}
		return nil
	})
}

func (catalog *Catalog) SetActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
) error {
	return catalog.updateControl(ctx, id, version, actorUserID, func(next *state, record DefinitionRecord) error {
		if record.Active == active {
			return nil
		}
		if !active && record.Default {
			return fmt.Errorf(
				"agent definition %q version %d is the default: %w",
				id, version, ErrConflict,
			)
		}
		if catalog.store != nil {
			if err := catalog.store.SetActive(ctx, id, version, active, actorUserID); err != nil {
				return err
			}
		}
		record.Active = active
		next.records[key{id: id, version: version}] = record
		if catalog.store == nil {
			action := "disabled"
			if active {
				action = "enabled"
			}
			appendMemoryAudit(next, id, version, action, actorUserID, time.Now().UTC())
		}
		return nil
	})
}

// SetRollout publishes a new auditable rule for an Agent ID.
func (catalog *Catalog) SetRollout(
	ctx context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
) (RolloutRule, error) {
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	id = strings.TrimSpace(id)
	salt = strings.TrimSpace(salt)
	if id == "" {
		return RolloutRule{}, fmt.Errorf("agent rollout id is required: %w", ErrInvalid)
	}
	if candidateVersion <= 0 {
		return RolloutRule{}, fmt.Errorf(
			"agent rollout candidate_version must be positive: %w",
			ErrInvalid,
		)
	}
	if percentageBPS < 0 || percentageBPS > rolloutBucketCount {
		return RolloutRule{}, fmt.Errorf(
			"agent rollout percentage_bps must be between 0 and %d: %w",
			rolloutBucketCount, ErrInvalid,
		)
	}
	if salt == "" {
		return RolloutRule{}, fmt.Errorf("agent rollout salt is required: %w", ErrInvalid)
	}
	if _, ok := current.records[key{id: id, version: candidateVersion}]; !ok {
		return RolloutRule{}, fmt.Errorf(
			"agent definition %q version %d not found: %w",
			id, candidateVersion, ErrNotFound,
		)
	}
	candidate := current.records[key{id: id, version: candidateVersion}]
	if !candidate.Active {
		return RolloutRule{}, fmt.Errorf(
			"agent definition %q version %d is disabled: %w",
			id, candidateVersion, ErrConflict,
		)
	}
	rule := RolloutRule{
		AgentID: id, RuleVersion: current.rollouts[id].RuleVersion + 1,
		CandidateVersion: candidateVersion, PercentageBPS: percentageBPS,
		Salt: salt, Active: active, CreatedBy: actorUserID, CreatedAt: time.Now().UTC(),
	}
	prepared, err := prepareRolloutRule(rule)
	if err != nil {
		return RolloutRule{}, err
	}
	if catalog.store != nil {
		if err := catalog.store.SetRollout(ctx, prepared, actorUserID); err != nil {
			return RolloutRule{}, err
		}
	}
	next := cloneState(current)
	next.rollouts[id] = prepared
	if catalog.store == nil {
		action := "rollout_disabled"
		if active {
			action = "rollout_enabled"
		}
		appendRolloutAudit(next, prepared, action, actorUserID)
	}
	next.revision++
	catalog.state.Store(next)
	return prepared, nil
}

func (catalog *Catalog) GetRollout(id string) (RolloutRule, bool) {
	rule, ok := catalog.state.Load().rollouts[id]
	return rule, ok
}

func (catalog *Catalog) ListRollouts() []RolloutRule {
	current := catalog.state.Load()
	out := make([]RolloutRule, 0, len(current.rollouts))
	for _, rule := range current.rollouts {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

func (catalog *Catalog) updateControl(
	_ context.Context,
	id string,
	version int64,
	_ int64,
	update func(*state, DefinitionRecord) error,
) error {
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	record, ok := current.records[key{id: id, version: version}]
	if !ok {
		return fmt.Errorf(
			"agent definition %q version %d not found: %w",
			id, version, ErrNotFound,
		)
	}
	next := cloneState(current)
	if err := update(next, cloneRecord(record)); err != nil {
		return err
	}
	next.revision++
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) ListAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]AuditEvent, error) {
	if limit <= 0 || afterSeq < 0 {
		return nil, fmt.Errorf("invalid agent audit cursor or limit: %w", ErrInvalid)
	}
	if catalog.store != nil {
		return catalog.store.ListAudit(ctx, id, afterSeq, limit)
	}
	current := catalog.state.Load()
	events := make([]AuditEvent, 0, min(limit, len(current.audit)))
	for _, event := range current.audit {
		if event.DefinitionID != id || event.Seq <= afterSeq {
			continue
		}
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	return events, nil
}

func (catalog *Catalog) ListRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
) ([]RolloutAuditEvent, error) {
	if limit <= 0 || afterSeq < 0 {
		return nil, fmt.Errorf("invalid agent rollout audit cursor or limit: %w", ErrInvalid)
	}
	if catalog.store != nil {
		return catalog.store.ListRolloutAudit(ctx, id, afterSeq, limit)
	}
	current := catalog.state.Load()
	events := make([]RolloutAuditEvent, 0, min(limit, len(current.rolloutAudit)))
	for _, event := range current.rolloutAudit {
		if event.AgentID != id || event.Seq <= afterSeq {
			continue
		}
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	return events, nil
}

func cloneState(current *state) *state {
	next := &state{
		revision:         current.revision,
		records:          make(map[key]DefinitionRecord, len(current.records)),
		highest:          make(map[string]int64, len(current.highest)),
		defaults:         make(map[string]int64, len(current.defaults)),
		rollouts:         make(map[string]RolloutRule, len(current.rollouts)),
		audit:            append([]AuditEvent(nil), current.audit...),
		nextAudit:        current.nextAudit,
		rolloutAudit:     append([]RolloutAuditEvent(nil), current.rolloutAudit...),
		nextRolloutAudit: current.nextRolloutAudit,
	}
	for id, record := range current.records {
		next.records[id] = cloneRecord(record)
	}
	for id, version := range current.highest {
		next.highest[id] = version
	}
	for id, version := range current.defaults {
		next.defaults[id] = version
	}
	for id, rule := range current.rollouts {
		next.rollouts[id] = rule
	}
	return next
}

func clearDefault(current *state, id string) {
	version := current.defaults[id]
	if version == 0 {
		return
	}
	recordKey := key{id: id, version: version}
	record := current.records[recordKey]
	record.Default = false
	current.records[recordKey] = record
	delete(current.defaults, id)
}

func appendMemoryAudit(
	current *state,
	id string,
	version int64,
	action string,
	actorUserID int64,
	createdAt time.Time,
) {
	current.nextAudit++
	current.audit = append(current.audit, AuditEvent{
		Seq: current.nextAudit, DefinitionID: id, Version: version,
		Action: action, ActorUserID: actorUserID, CreatedAt: createdAt,
	})
}

func appendRolloutAudit(
	current *state,
	rule RolloutRule,
	action string,
	actorUserID int64,
) {
	current.nextRolloutAudit++
	current.rolloutAudit = append(current.rolloutAudit, RolloutAuditEvent{
		Seq: current.nextRolloutAudit, AgentID: rule.AgentID,
		RuleVersion: rule.RuleVersion, CandidateVersion: rule.CandidateVersion,
		PercentageBPS: rule.PercentageBPS, RuleHash: rule.RuleHash,
		Action: action, ActorUserID: actorUserID, CreatedAt: rule.CreatedAt,
	})
}

func cloneRecord(record DefinitionRecord) DefinitionRecord {
	record.Definition = clone(record.Definition)
	return record
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
