package workflow

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

type DefinitionRef struct {
	ID      string
	Version int64
}

type definitionKey struct {
	id      string
	version int64
}

type AgentResolver interface {
	Resolve(agentapi.DefinitionRef) (agentapi.Definition, error)
}

// DefinitionRecord adds rollout metadata without changing the immutable hash.
type DefinitionRecord struct {
	Definition
	Active    bool      `json:"active"`
	Default   bool      `json:"default"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type DefinitionCursor struct {
	ID      string
	Version int64
}

type DefinitionAuditEvent struct {
	Seq          int64     `json:"seq"`
	DefinitionID string    `json:"definition_id"`
	Version      int64     `json:"version"`
	Action       string    `json:"action"`
	ActorUserID  int64     `json:"actor_user_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type catalogPersistence interface {
	Publish(context.Context, []Definition, int64) ([]DefinitionRecord, error)
	LoadDefaultDefinitions(context.Context) ([]DefinitionRecord, error)
	LoadDefinition(context.Context, string, int64) (DefinitionRecord, error)
	LoadHighestVersions(context.Context) (map[string]int64, error)
	LoadRollouts(context.Context) ([]RolloutRule, error)
	ListDefinitions(context.Context, DefinitionCursor, int) ([]DefinitionRecord, error)
	SetDefault(context.Context, string, int64, int64) error
	SetActive(context.Context, string, int64, bool, int64) error
	SetRollout(context.Context, RolloutRule, int64) error
	ListAudit(context.Context, string, int64, int) ([]DefinitionAuditEvent, error)
	ListRolloutAudit(context.Context, string, int64, int) ([]RolloutAuditEvent, error)
}

type catalogState struct {
	revision         uint64
	records          map[definitionKey]DefinitionRecord
	highest          map[string]int64
	defaults         map[string]int64
	rollouts         map[string]RolloutRule
	audit            []DefinitionAuditEvent
	nextAudit        int64
	rolloutAudit     []RolloutAuditEvent
	nextRolloutAudit int64
}

// Catalog publishes immutable workflow snapshots without disrupting active runs.
type Catalog struct {
	writeMu sync.Mutex
	state   atomic.Pointer[catalogState]
	schemas *agentapi.SchemaRegistry
	agents  AgentResolver
	store   catalogPersistence
}

func NewCatalog(schemas *agentapi.SchemaRegistry, agents AgentResolver) *Catalog {
	catalog := &Catalog{schemas: schemas, agents: agents}
	catalog.state.Store(&catalogState{
		records:  make(map[definitionKey]DefinitionRecord),
		highest:  make(map[string]int64),
		defaults: make(map[string]int64),
		rollouts: make(map[string]RolloutRule),
	})
	return catalog
}

func (catalog *Catalog) Publish(definitions []Definition) error {
	return catalog.PublishAs(context.Background(), definitions, 0)
}

func (catalog *Catalog) PublishAs(
	ctx context.Context,
	definitions []Definition,
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
	next := cloneCatalogState(current)
	for _, record := range records {
		key := definitionKey{id: record.ID, version: record.Version}
		if published, exists := next.records[key]; exists {
			if published.ContentHash != record.ContentHash {
				return fmt.Errorf(
					"workflow %q version %d is already published: %w",
					key.id, key.version, ErrConflict,
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
		next.records[key] = cloneDefinitionRecord(record)
		if record.Version > next.highest[record.ID] {
			next.highest[record.ID] = record.Version
		}
		if catalog.store == nil {
			appendAudit(
				next, record.ID, record.Version, "published",
				actorUserID, record.CreatedAt,
			)
		}
	}
	next.revision++
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) prepare(
	definitions []Definition,
) ([]Definition, error) {
	return catalog.prepareDefinitions(definitions, Prepare)
}

func (catalog *Catalog) preparePersisted(
	definitions []Definition,
) ([]Definition, error) {
	return catalog.prepareDefinitions(definitions, preparePersisted)
}

func (catalog *Catalog) prepareDefinitions(
	definitions []Definition,
	prepare func(
		Definition,
		*agentapi.SchemaRegistry,
	) (Definition, error),
) ([]Definition, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("workflow definitions are required: %w", ErrInvalid)
	}
	incoming := make(map[definitionKey]Definition, len(definitions))
	for _, definition := range definitions {
		prepared, err := prepare(definition, catalog.schemas)
		if err != nil {
			return nil, fmt.Errorf("%v: %w", err, ErrInvalid)
		}
		if err := catalog.validateAgents(prepared); err != nil {
			return nil, fmt.Errorf("%v: %w", err, ErrInvalid)
		}
		key := definitionKey{id: prepared.ID, version: prepared.Version}
		if _, duplicate := incoming[key]; duplicate {
			return nil, fmt.Errorf(
				"workflow %q version %d is duplicated: %w",
				prepared.ID, prepared.Version, ErrInvalid,
			)
		}
		incoming[key] = prepared
	}
	prepared := make([]Definition, 0, len(incoming))
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

func (catalog *Catalog) validateAgents(definition Definition) error {
	if catalog.agents == nil {
		return fmt.Errorf("publish workflow %q: agent definition resolver is required", definition.ID)
	}
	for _, node := range definition.Nodes {
		if node.Kind != NodeAgent {
			continue
		}
		if err := catalog.validateAgentNode(definition, node); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentNodeContract(catalog *Catalog, definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	if agentDefinition.ID != node.Agent.ID || agentDefinition.Version != node.Agent.Version {
		return fmt.Errorf("publish workflow %q node %q agent is not pinned", definition.ID, node.ID)
	}
	if err := validateAgentNodePermissions(definition, node, agentDefinition); err != nil {
		return err
	}
	if err := validateComposerAgentContract(definition.ID, node, agentDefinition); err != nil {
		return err
	}
	if err := validateAgentNodeTools(catalog, definition, node, agentDefinition); err != nil {
		return err
	}
	if err := validateAgentNodeSchemas(catalog, definition, node, agentDefinition); err != nil {
		return err
	}
	if err := validateAgentNodeToolBudget(definition, node, agentDefinition); err != nil {
		return err
	}
	if err := validateAgentNodeModelPrices(definition, node, agentDefinition); err != nil {
		return err
	}
	return nil
}

func validateAgentNodePermissions(definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	if err := scope.ValidateAgentRuntime(agentDefinition.Permissions.Scopes); err != nil {
		return fmt.Errorf(
			"publish workflow %q node %q agent permissions: %w",
			definition.ID, node.ID, err,
		)
	}
	if err := scope.EnsureSubset(node.Permissions.Scopes, agentDefinition.Permissions.Scopes); err != nil {
		return fmt.Errorf(
			"publish workflow %q node %q permissions exceed agent definition: %w",
			definition.ID, node.ID, err,
		)
	}
	return nil
}

func validateAgentNodeTools(catalog *Catalog, definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	if !node.RestrictVisibleTools {
		return nil
	}
	if err := ensureToolSubset(
		node.VisibleToolIDs,
		agentDefinition.Tools.VisibleToolIDs,
		agentDefinition.Tools.RestrictVisible ||
			len(agentDefinition.Tools.VisibleToolIDs) > 0,
	); err != nil {
		return fmt.Errorf(
			"publish workflow %q node %q tools exceed agent definition: %w",
			definition.ID, node.ID, err,
		)
	}
	return nil
}

func validateAgentNodeSchemas(catalog *Catalog, definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	if err := catalog.schemas.ValidateCompatibility(node.InputSchema, agentDefinition.InputSchema); err != nil {
		return fmt.Errorf("publish workflow %q node %q agent input: %w", definition.ID, node.ID, err)
	}
	if err := catalog.schemas.ValidateCompatibility(agentDefinition.OutputSchema, node.OutputSchema); err != nil {
		return fmt.Errorf("publish workflow %q node %q agent output: %w", definition.ID, node.ID, err)
	}
	return nil
}

func validateAgentNodeModelPrices(definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	if definition.Budget.MaxCostMicros > 0 &&
		(agentDefinition.Model.InputPriceMicrosPerMillionTokens <= 0 ||
			agentDefinition.Model.OutputPriceMicrosPerMillionTokens <= 0) {
		return fmt.Errorf(
			"publish workflow %q node %q agent model prices are required for cost budgeting",
			definition.ID, node.ID,
		)
	}
	return nil
}

func (catalog *Catalog) validateAgentNode(definition Definition, node NodeDefinition) error {
	agentDefinition, err := catalog.agents.Resolve(node.Agent)
	if err != nil {
		return fmt.Errorf("publish workflow %q node %q agent: %w", definition.ID, node.ID, err)
	}
	if err := validateAgentNodeContract(catalog, definition, node, agentDefinition); err != nil {
		return err
	}
	return nil
}

func validateAgentNodeToolBudget(definition Definition, node NodeDefinition, agentDefinition agentapi.Definition) error {
	toolsDisabled := node.RestrictVisibleTools && len(node.VisibleToolIDs) == 0 ||
		!node.RestrictVisibleTools &&
			agentDefinition.Tools.RestrictVisible &&
			len(agentDefinition.Tools.VisibleToolIDs) == 0
	if toolsDisabled && node.Budget.MaxToolCalls != 0 {
		return fmt.Errorf(
			"publish workflow %q node %q tool budget must be zero because its agent disables tools",
			definition.ID, node.ID,
		)
	}
	return nil
}

func validateComposerAgentContract(
	workflowID string,
	node NodeDefinition,
	definition agentapi.Definition,
) error {
	if !isComposerAgent(node.Agent) {
		return nil
	}
	verified := agentapi.InvestigationVerifiedBundleSchemaRef()
	answer := agentapi.InvestigationAnswerSchemaRef()
	if node.InputSchema != verified {
		return fmt.Errorf(
			"publish workflow %q composer node %q input schema must be %s version %d",
			workflowID, node.ID, verified.ID, verified.Version,
		)
	}
	if node.OutputSchema != answer {
		return fmt.Errorf(
			"publish workflow %q composer node %q output schema must be %s version %d",
			workflowID, node.ID, answer.ID, answer.Version,
		)
	}
	if definition.InputSchema != verified {
		return fmt.Errorf(
			"publish workflow %q composer node %q agent input schema must be %s version %d",
			workflowID, node.ID, verified.ID, verified.Version,
		)
	}
	if definition.OutputSchema != answer {
		return fmt.Errorf(
			"publish workflow %q composer node %q agent output schema must be %s version %d",
			workflowID, node.ID, answer.ID, answer.Version,
		)
	}
	if !node.RestrictVisibleTools || len(node.VisibleToolIDs) != 0 {
		return fmt.Errorf(
			"publish workflow %q composer node %q must restrict visible tools and expose none",
			workflowID, node.ID,
		)
	}
	if !definition.Tools.RestrictVisible || len(definition.Tools.VisibleToolIDs) != 0 {
		return fmt.Errorf(
			"publish workflow %q composer node %q agent must restrict visible tools and expose none",
			workflowID, node.ID,
		)
	}
	return nil
}

func ensureToolSubset(subset, superset []string, restricted bool) error {
	if !restricted {
		return nil
	}
	allowed := make(map[string]struct{}, len(superset))
	for _, id := range superset {
		allowed[id] = struct{}{}
	}
	for _, id := range subset {
		if _, ok := allowed[id]; !ok {
			return fmt.Errorf("tool %q is outside the allowed set", id)
		}
	}
	return nil
}

func (catalog *Catalog) Resolve(ref DefinitionRef) (Definition, error) {
	definition, _, err := catalog.resolve(ref, "", false)
	return definition, err
}

// ResolveFor applies the active rollout rule when the reference is unpinned.
func (catalog *Catalog) ResolveFor(
	ref DefinitionRef,
	stableKey string,
) (Definition, DefinitionSelection, error) {
	return catalog.resolve(ref, stableKey, true)
}

func (catalog *Catalog) resolve(
	ref DefinitionRef,
	stableKey string,
	applyRollout bool,
) (Definition, DefinitionSelection, error) {
	if ref.Version > 0 {
		if err := catalog.loadDefinition(context.Background(), ref); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return Definition{}, DefinitionSelection{}, err
		}
	}
	current := catalog.state.Load()
	version := ref.Version
	selection := DefinitionSelection{Reason: "explicit_version"}
	if version == 0 {
		version = current.defaults[ref.ID]
		selection.Reason = "default"
		if applyRollout {
			if rule, ok := current.rollouts[ref.ID]; ok && rule.Active {
				var err error
				selection, version, err = catalog.rolloutSelection(
					current, rule, stableKey, current.defaults[ref.ID],
				)
				if err != nil {
					return Definition{}, DefinitionSelection{}, err
				}
			}
		}
	}
	record, ok := current.records[definitionKey{id: ref.ID, version: version}]
	if !ok {
		return Definition{}, DefinitionSelection{}, fmt.Errorf(
			"workflow %q version %d not found: %w",
			ref.ID, version, ErrNotFound,
		)
	}
	if ref.Version == 0 && version == current.defaults[ref.ID] && !record.Active {
		return Definition{}, DefinitionSelection{}, fmt.Errorf(
			"workflow %q default version %d is disabled: %w",
			ref.ID, version, ErrConflict,
		)
	}
	return cloneDefinition(record.Definition), selection, nil
}

func (catalog *Catalog) rolloutSelection(
	current *catalogState,
	rule RolloutRule,
	stableKey string,
	defaultVersion int64,
) (DefinitionSelection, int64, error) {
	selection, candidate, err := selectRollout(rule, stableKey)
	if err != nil {
		return DefinitionSelection{}, 0, err
	}
	version := defaultVersion
	if candidate {
		version = rule.CandidateVersion
	}
	record, ok := current.records[definitionKey{id: rule.WorkflowID, version: version}]
	if !ok || !record.Active {
		role := "active default"
		if candidate {
			role = "rollout candidate"
		}
		return DefinitionSelection{}, 0, fmt.Errorf(
			"workflow %q %s version %d is unavailable: %w",
			rule.WorkflowID, role, version, ErrConflict,
		)
	}
	return selection, version, nil
}

func (catalog *Catalog) List() []Definition {
	current := catalog.state.Load()
	out := make([]Definition, 0, len(current.records))
	for _, record := range current.records {
		out = append(out, cloneDefinition(record.Definition))
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

// AttachStore restores defaults, rollout candidates, and version watermarks.
// Historical versions remain in the store until a pinned lookup needs them.
func (catalog *Catalog) AttachStore(
	ctx context.Context,
	store catalogPersistence,
) error {
	if store == nil {
		return fmt.Errorf("workflow catalog store is required: %w", ErrUnavailable)
	}
	records, highest, rollouts, err := catalog.loadStoreState(ctx, store)
	if err != nil {
		return err
	}
	next, err := catalog.buildCatalogState(ctx, store, records, highest, rollouts)
	if err != nil {
		return err
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	catalog.store = store
	next.revision = catalog.state.Load().revision + 1
	catalog.state.Store(next)
	return nil
}

func (catalog *Catalog) loadStoreState(
	ctx context.Context,
	store catalogPersistence,
) ([]DefinitionRecord, map[string]int64, []RolloutRule, error) {
	records, err := store.LoadDefaultDefinitions(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	highest, err := store.LoadHighestVersions(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	rollouts, err := store.LoadRollouts(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return records, highest, rollouts, nil
}

func (catalog *Catalog) buildCatalogState(
	ctx context.Context,
	store catalogPersistence,
	records []DefinitionRecord,
	highest map[string]int64,
	rollouts []RolloutRule,
) (*catalogState, error) {
	loaded := make(map[definitionKey]DefinitionRecord, len(records)+len(rollouts))
	for _, record := range records {
		loaded[definitionKey{id: record.ID, version: record.Version}] = record
	}
	for _, rule := range rollouts {
		candidateKey := definitionKey{
			id: rule.WorkflowID, version: rule.CandidateVersion,
		}
		if _, ok := loaded[candidateKey]; ok {
			continue
		}
		record, loadErr := store.LoadDefinition(
			ctx,
			rule.WorkflowID,
			rule.CandidateVersion,
		)
		if loadErr != nil {
			return nil, fmt.Errorf(
				"load workflow rollout %q candidate version %d: %w",
				rule.WorkflowID, rule.CandidateVersion, loadErr,
			)
		}
		loaded[candidateKey] = record
	}
	next := &catalogState{
		records:  make(map[definitionKey]DefinitionRecord, len(loaded)),
		highest:  highest,
		defaults: make(map[string]int64),
		rollouts: make(map[string]RolloutRule),
	}
	if err := catalog.ingestLoadedRecords(next, loaded); err != nil {
		return nil, err
	}
	if err := catalog.ingestRolloutRules(next, rollouts); err != nil {
		return nil, err
	}
	return next, nil
}

func (catalog *Catalog) ingestLoadedRecords(next *catalogState, loaded map[definitionKey]DefinitionRecord) error {
	for _, record := range loaded {
		preparedRecord, prepareErr := catalog.preparePersistedRecord(record)
		if prepareErr != nil {
			return fmt.Errorf(
				"load workflow %q version %d: %w",
				record.ID, record.Version, prepareErr,
			)
		}
		record = preparedRecord
		recordKey := definitionKey{id: record.ID, version: record.Version}
		next.records[recordKey] = cloneDefinitionRecord(record)
		if record.Version > next.highest[record.ID] {
			next.highest[record.ID] = record.Version
		}
		if record.Default {
			if next.defaults[record.ID] != 0 {
				return fmt.Errorf(
					"workflow %q has multiple defaults: %w",
					record.ID, ErrConflict,
				)
			}
			if !record.Active {
				return fmt.Errorf(
					"workflow %q default version %d is disabled: %w",
					record.ID, record.Version, ErrConflict,
				)
			}
			next.defaults[record.ID] = record.Version
		}
	}
	return nil
}

func (catalog *Catalog) ingestRolloutRules(next *catalogState, rollouts []RolloutRule) error {
	for _, rule := range rollouts {
		preparedRule, prepareErr := prepareRolloutRule(rule)
		if prepareErr != nil {
			return fmt.Errorf("load workflow rollout %q: %w", rule.WorkflowID, prepareErr)
		}
		candidate, ok := next.records[definitionKey{
			id: rule.WorkflowID, version: rule.CandidateVersion,
		}]
		if !ok {
			return fmt.Errorf(
				"load workflow rollout %q candidate version %d not found: %w",
				rule.WorkflowID, rule.CandidateVersion, ErrConflict,
			)
		}
		if preparedRule.Active && !candidate.Active {
			return fmt.Errorf(
				"load workflow rollout %q candidate version %d is disabled: %w",
				rule.WorkflowID, rule.CandidateVersion, ErrConflict,
			)
		}
		next.rollouts[rule.WorkflowID] = preparedRule
	}
	return nil
}

// Preload hydrates pinned versions needed by startup recovery before work resumes.
func (catalog *Catalog) Preload(
	ctx context.Context,
	refs []DefinitionRef,
) error {
	for _, ref := range refs {
		if ref.Version <= 0 {
			continue
		}
		if err := catalog.loadDefinition(ctx, ref); err != nil {
			return fmt.Errorf(
				"preload workflow %q version %d: %w",
				ref.ID, ref.Version, err,
			)
		}
	}
	return nil
}

// loadDefinition reads and caches one historical version on a pinned cache miss.
func (catalog *Catalog) loadDefinition(
	ctx context.Context,
	ref DefinitionRef,
) error {
	recordKey := definitionKey{id: ref.ID, version: ref.Version}
	if _, ok := catalog.state.Load().records[recordKey]; ok {
		return nil
	}
	if catalog.store == nil {
		return fmt.Errorf(
			"workflow %q version %d not found: %w",
			ref.ID, ref.Version, ErrNotFound,
		)
	}
	record, err := catalog.store.LoadDefinition(ctx, ref.ID, ref.Version)
	if err != nil {
		return err
	}
	record, err = catalog.preparePersistedRecord(record)
	if err != nil {
		return fmt.Errorf(
			"load workflow %q version %d: %w",
			ref.ID, ref.Version, err,
		)
	}

	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	if _, ok := current.records[recordKey]; ok {
		return nil
	}
	next := cloneCatalogState(current)
	if record.Default {
		clearDefault(next, record.ID)
		next.defaults[record.ID] = record.Version
	}
	next.records[recordKey] = cloneDefinitionRecord(record)
	if record.Version > next.highest[record.ID] {
		next.highest[record.ID] = record.Version
	}
	catalog.state.Store(next)
	return nil
}

// preparePersistedRecord validates one persisted definition before caching it.
func (catalog *Catalog) preparePersistedRecord(
	record DefinitionRecord,
) (DefinitionRecord, error) {
	prepared, err := catalog.preparePersisted([]Definition{record.Definition})
	if err != nil {
		return DefinitionRecord{}, err
	}
	record.Definition = prepared[0]
	return record, nil
}

func (catalog *Catalog) ListRecords(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("workflow definition limit must be positive: %w", ErrInvalid)
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
		records = append(records, cloneDefinitionRecord(record))
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
	if err := catalog.loadDefinition(
		ctx,
		DefinitionRef{ID: id, Version: version},
	); err != nil {
		return err
	}
	return catalog.updateControl(id, version, func(next *catalogState, record DefinitionRecord) error {
		if !record.Active {
			return fmt.Errorf(
				"workflow %q version %d is disabled: %w",
				id, version, ErrConflict,
			)
		}
		if catalog.store != nil {
			if err := catalog.store.SetDefault(
				ctx, id, version, actorUserID,
			); err != nil {
				return err
			}
		}
		clearDefault(next, id)
		record.Default = true
		next.records[definitionKey{id: id, version: version}] = record
		next.defaults[id] = version
		if catalog.store == nil {
			appendAudit(
				next, id, version, "default_set", actorUserID, time.Now().UTC(),
			)
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
	if err := catalog.loadDefinition(
		ctx,
		DefinitionRef{ID: id, Version: version},
	); err != nil {
		return err
	}
	return catalog.updateControl(id, version, func(next *catalogState, record DefinitionRecord) error {
		if record.Active == active {
			return nil
		}
		if !active && record.Default {
			return fmt.Errorf(
				"workflow %q version %d is the default: %w",
				id, version, ErrConflict,
			)
		}
		if catalog.store != nil {
			if err := catalog.store.SetActive(
				ctx, id, version, active, actorUserID,
			); err != nil {
				return err
			}
		}
		record.Active = active
		next.records[definitionKey{id: id, version: version}] = record
		if catalog.store == nil {
			action := "disabled"
			if active {
				action = "enabled"
			}
			appendAudit(
				next, id, version, action, actorUserID, time.Now().UTC(),
			)
		}
		return nil
	})
}

// SetRollout publishes a new auditable rule for a Workflow ID.
func (catalog *Catalog) SetRollout(
	ctx context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
) (RolloutRule, error) {
	id = strings.TrimSpace(id)
	salt = strings.TrimSpace(salt)
	if id == "" {
		return RolloutRule{}, fmt.Errorf("workflow rollout id is required: %w", ErrInvalid)
	}
	if candidateVersion <= 0 {
		return RolloutRule{}, fmt.Errorf(
			"workflow rollout candidate_version must be positive: %w",
			ErrInvalid,
		)
	}
	if percentageBPS < 0 || percentageBPS > rolloutBucketCount {
		return RolloutRule{}, fmt.Errorf(
			"workflow rollout percentage_bps must be between 0 and %d: %w",
			rolloutBucketCount, ErrInvalid,
		)
	}
	if salt == "" {
		return RolloutRule{}, fmt.Errorf("workflow rollout salt is required: %w", ErrInvalid)
	}
	if err := catalog.loadDefinition(
		ctx,
		DefinitionRef{ID: id, Version: candidateVersion},
	); err != nil {
		return RolloutRule{}, err
	}
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	candidate, ok := current.records[definitionKey{id: id, version: candidateVersion}]
	if !ok {
		return RolloutRule{}, fmt.Errorf(
			"workflow %q version %d not found: %w",
			id, candidateVersion, ErrNotFound,
		)
	}
	if !candidate.Active {
		return RolloutRule{}, fmt.Errorf(
			"workflow %q version %d is disabled: %w",
			id, candidateVersion, ErrConflict,
		)
	}
	rule := RolloutRule{
		WorkflowID: id, RuleVersion: current.rollouts[id].RuleVersion + 1,
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
	next := cloneCatalogState(current)
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
	sort.Slice(out, func(i, j int) bool {
		return out[i].WorkflowID < out[j].WorkflowID
	})
	return out
}

func (catalog *Catalog) updateControl(
	id string,
	version int64,
	update func(*catalogState, DefinitionRecord) error,
) error {
	catalog.writeMu.Lock()
	defer catalog.writeMu.Unlock()
	current := catalog.state.Load()
	key := definitionKey{id: id, version: version}
	record, ok := current.records[key]
	if !ok {
		return fmt.Errorf(
			"workflow %q version %d not found: %w",
			id, version, ErrNotFound,
		)
	}
	next := cloneCatalogState(current)
	if err := update(next, cloneDefinitionRecord(record)); err != nil {
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
) ([]DefinitionAuditEvent, error) {
	if afterSeq < 0 || limit <= 0 {
		return nil, fmt.Errorf("invalid workflow audit cursor or limit: %w", ErrInvalid)
	}
	if catalog.store != nil {
		return catalog.store.ListAudit(ctx, id, afterSeq, limit)
	}
	current := catalog.state.Load()
	events := make([]DefinitionAuditEvent, 0, min(limit, len(current.audit)))
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
	if afterSeq < 0 || limit <= 0 {
		return nil, fmt.Errorf("invalid workflow rollout audit cursor or limit: %w", ErrInvalid)
	}
	if catalog.store != nil {
		return catalog.store.ListRolloutAudit(ctx, id, afterSeq, limit)
	}
	current := catalog.state.Load()
	events := make([]RolloutAuditEvent, 0, min(limit, len(current.rolloutAudit)))
	for _, event := range current.rolloutAudit {
		if event.WorkflowID != id || event.Seq <= afterSeq {
			continue
		}
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	return events, nil
}

func cloneCatalogState(current *catalogState) *catalogState {
	next := &catalogState{
		revision:         current.revision,
		records:          make(map[definitionKey]DefinitionRecord, len(current.records)),
		highest:          make(map[string]int64, len(current.highest)),
		defaults:         make(map[string]int64, len(current.defaults)),
		rollouts:         make(map[string]RolloutRule, len(current.rollouts)),
		audit:            append([]DefinitionAuditEvent(nil), current.audit...),
		nextAudit:        current.nextAudit,
		rolloutAudit:     append([]RolloutAuditEvent(nil), current.rolloutAudit...),
		nextRolloutAudit: current.nextRolloutAudit,
	}
	for key, record := range current.records {
		next.records[key] = cloneDefinitionRecord(record)
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

func clearDefault(current *catalogState, id string) {
	version := current.defaults[id]
	if version == 0 {
		return
	}
	key := definitionKey{id: id, version: version}
	record := current.records[key]
	record.Default = false
	current.records[key] = record
	delete(current.defaults, id)
}

func appendAudit(
	current *catalogState,
	id string,
	version int64,
	action string,
	actorUserID int64,
	createdAt time.Time,
) {
	current.nextAudit++
	current.audit = append(current.audit, DefinitionAuditEvent{
		Seq: current.nextAudit, DefinitionID: id, Version: version,
		Action: action, ActorUserID: actorUserID, CreatedAt: createdAt,
	})
}

func appendRolloutAudit(
	current *catalogState,
	rule RolloutRule,
	action string,
	actorUserID int64,
) {
	current.nextRolloutAudit++
	current.rolloutAudit = append(current.rolloutAudit, RolloutAuditEvent{
		Seq: current.nextRolloutAudit, WorkflowID: rule.WorkflowID,
		RuleVersion: rule.RuleVersion, CandidateVersion: rule.CandidateVersion,
		PercentageBPS: rule.PercentageBPS, RuleHash: rule.RuleHash,
		Action: action, ActorUserID: actorUserID, CreatedAt: rule.CreatedAt,
	})
}

func cloneDefinitionRecord(record DefinitionRecord) DefinitionRecord {
	record.Definition = cloneDefinition(record.Definition)
	return record
}
