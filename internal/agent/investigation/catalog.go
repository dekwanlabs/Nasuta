package investigation

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

var templateIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type templateKey struct {
	id      string
	version int64
}

// TaskTemplateCatalog is the bounded vocabulary from which a planner may compose tasks.
type TaskTemplateCatalog struct {
	mu        sync.RWMutex
	templates map[templateKey]TaskTemplate
	audit     []CatalogAuditEntry
}

// CatalogAuditEntry is an append-only record of template lifecycle changes.
type CatalogAuditEntry struct {
	TemplateID string
	Version    int64
	Action     string
	Reason     string
	At         string
}

func NewTaskTemplateCatalog() *TaskTemplateCatalog {
	return &TaskTemplateCatalog{templates: make(map[templateKey]TaskTemplate)}
}

func (catalog *TaskTemplateCatalog) Register(template TaskTemplate) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	if err := validateTemplate(template); err != nil {
		return err
	}
	key := templateKey{id: template.ID, version: template.Version}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if existing, exists := catalog.templates[key]; exists {
		if !sameTemplate(existing, template) {
			return fmt.Errorf("task template %q version %d is already registered", template.ID, template.Version)
		}
		return nil
	}
	catalog.templates[key] = cloneTemplate(template)
	catalog.audit = append(catalog.audit, CatalogAuditEntry{
		TemplateID: template.ID, Version: template.Version, Action: "publish",
		At: fmt.Sprintf("%d", timeNowUnixMilli()),
	})
	return nil
}

func (catalog *TaskTemplateCatalog) Resolve(ref TaskTemplateRef) (TaskTemplate, error) {
	if catalog == nil {
		return TaskTemplate{}, fmt.Errorf("task template catalog is required")
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	template, ok := catalog.templates[templateKey{id: ref.ID, version: ref.Version}]
	if !ok || !template.Enabled || template.Deprecated {
		return TaskTemplate{}, fmt.Errorf("%w: template %q version %d is unavailable", ErrCapabilityGap, ref.ID, ref.Version)
	}
	return cloneTemplate(template), nil
}

func (catalog *TaskTemplateCatalog) List() []TaskTemplate {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	out := make([]TaskTemplate, 0, len(catalog.templates))
	for _, template := range catalog.templates {
		if template.Enabled && !template.Deprecated {
			out = append(out, cloneTemplate(template))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Deprecate marks one template version unavailable for future plans while
// retaining its audit metadata. Idempotent, unlike removing the template.
func (catalog *TaskTemplateCatalog) Deprecate(ref TaskTemplateRef) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	template, ok := catalog.templates[templateKey{id: ref.ID, version: ref.Version}]
	if !ok {
		return fmt.Errorf("%w: template %q version %d does not exist", ErrCapabilityGap, ref.ID, ref.Version)
	}
	template.Deprecated = true
	catalog.templates[templateKey{id: ref.ID, version: ref.Version}] = template
	catalog.audit = append(catalog.audit, CatalogAuditEntry{
		TemplateID: ref.ID, Version: ref.Version, Action: "deprecate",
		At: fmt.Sprintf("%d", timeNowUnixMilli()),
	})
	return nil
}

// Audit returns a copy of the append-only template lifecycle log.
func (catalog *TaskTemplateCatalog) Audit() []CatalogAuditEntry {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return append([]CatalogAuditEntry(nil), catalog.audit...)
}

func timeNowUnixMilli() int64 {
	return time.Now().UTC().UnixMilli()
}

// GenerateCandidates runs before retrieval. Retrieval results may be used by later replans.
func (catalog *TaskTemplateCatalog) GenerateCandidates(contract InvestigationContract) ([]TaskCandidate, error) {
	return catalog.generateCandidates(contract, nil)
}

// GenerateCandidatesForGoals returns only candidates that can advance the
// supplied goals. It is used for replanning after earlier rounds have already
// covered some contract goals.
func (catalog *TaskTemplateCatalog) GenerateCandidatesForGoals(
	contract InvestigationContract,
	goalIDs map[string]struct{},
) ([]TaskCandidate, error) {
	if len(goalIDs) == 0 {
		return nil, nil
	}
	return catalog.generateCandidates(contract, goalIDs)
}

// GenerateCandidatesForDiscoveries turns normalized discoveries into bounded
// candidates whose templates explicitly declare the discovery shapes they can use.
// The generic explore template remains the fallback when no typed template matches.
func (catalog *TaskTemplateCatalog) GenerateCandidatesForDiscoveries(
	contract InvestigationContract,
	discoveries []Discovery,
	unresolved map[string]struct{},
) ([]TaskCandidate, error) {
	if catalog == nil {
		return nil, fmt.Errorf("task template catalog is required")
	}
	if len(discoveries) == 0 || len(unresolved) == 0 {
		return nil, nil
	}
	templates := catalog.List()
	if len(templates) == 0 {
		return nil, nil
	}
	goalByID := indexGoalsByKind(contract.Goals)
	seenDiscoveries := make(map[string]struct{}, len(discoveries))
	candidates := make([]TaskCandidate, 0, len(discoveries))
	for _, rawDiscovery := range discoveries {
		discovery, ok := normalizeDiscovery(rawDiscovery)
		if !ok {
			continue
		}
		discoveryKey := discoveryID(discovery)
		if _, seen := seenDiscoveries[discoveryKey]; seen {
			continue
		}
		seenDiscoveries[discoveryKey] = struct{}{}

		matched := false
		for _, template := range templates {
			if template.ProposalOnly || !templateMatchesDiscovery(template, discovery) {
				continue
			}
			goalIDs := matchingDiscoveryGoals(template, goalByID, unresolved)
			if len(goalIDs) == 0 {
				continue
			}
			candidates = append(candidates, discoveryCandidate(template, discovery, goalIDs))
			matched = true
		}
		if matched {
			continue
		}
		// Catalogs created before DiscoveryTypes existed may only expose the
		// generic fallback. Resolve it explicitly rather than weakening typed
		// template matching for every other template.
		fallback, err := catalog.Resolve(TaskTemplateRef{ID: "investigation.explore", Version: 1})
		if err != nil || !templateMatchesDiscovery(fallback, discovery) {
			continue
		}
		goalIDs := matchingDiscoveryGoals(fallback, goalByID, unresolved)
		if len(goalIDs) > 0 {
			candidates = append(candidates, discoveryCandidate(fallback, discovery, goalIDs))
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates, nil
}

func matchingDiscoveryGoals(
	template TaskTemplate,
	goals map[string]EvidenceGoal,
	unresolved map[string]struct{},
) []string {
	matched := make([]string, 0, len(unresolved))
	for goalID := range unresolved {
		goal, ok := goals[goalID]
		if ok && templateMatchesGoal(template, goal) {
			matched = append(matched, goalID)
		}
	}
	sort.Strings(matched)
	return matched
}

func discoveryCandidate(template TaskTemplate, discovery Discovery, goalIDs []string) TaskCandidate {
	taskID := fmt.Sprintf("task_%s_v%d_discovery_%s", template.ID, template.Version, discoveryID(discovery))
	if template.ID == "investigation.explore" && template.Version == 1 {
		// Preserve the original fallback IDs so a resumed run does not repeat
		// discoveries solely because the catalog gained typed matching metadata.
		taskID = "task_explore_discovery_" + discoveryID(discovery)
	}
	return TaskCandidate{
		ID:           taskID,
		Template:     TaskTemplateRef{ID: template.ID, Version: template.Version},
		Objective:    discoveryObjective(discovery),
		GoalIDs:      append([]string(nil), goalIDs...),
		Entities:     discoveryEntities(discovery),
		AllowedTools: append([]tool.ToolID(nil), template.ToolGrant...),
		Budget:       template.CostProfile,
	}
}

func normalizeDiscovery(discovery Discovery) (Discovery, bool) {
	discovery.Type = strings.TrimSpace(discovery.Type)
	discovery.Entity = strings.TrimSpace(discovery.Entity)
	discovery.From = strings.TrimSpace(discovery.From)
	discovery.To = strings.TrimSpace(discovery.To)
	discovery.Kind = strings.TrimSpace(discovery.Kind)
	switch discovery.Type {
	case "entity":
		return discovery, discovery.Entity != ""
	case "dependency":
		return discovery, discovery.From != "" && discovery.To != ""
	default:
		return Discovery{}, false
	}
}

func templateMatchesDiscovery(template TaskTemplate, discovery Discovery) bool {
	if len(template.DiscoveryTypes) == 0 {
		return template.ID == "investigation.explore"
	}
	return containsString(template.DiscoveryTypes, discovery.Type)
}

func indexGoalsByKind(goals []EvidenceGoal) map[string]EvidenceGoal {
	index := make(map[string]EvidenceGoal, len(goals))
	for _, goal := range goals {
		index[goal.ID] = goal
	}
	return index
}

func discoveryEntities(discovery Discovery) []string {
	entities := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	for _, entity := range []string{discovery.Entity, discovery.From, discovery.To} {
		if entity == "" {
			continue
		}
		if _, exists := seen[entity]; exists {
			continue
		}
		seen[entity] = struct{}{}
		entities = append(entities, entity)
	}
	return entities
}

func discoveryObjective(discovery Discovery) string {
	switch discovery.Type {
	case "dependency":
		kind := ""
		if discovery.Kind != "" {
			kind = " (" + discovery.Kind + ")"
		}
		return "Investigate the discovered dependency" + kind + " " + discovery.From + " -> " + discovery.To
	case "entity":
		return "Investigate the discovered entity " + discovery.Entity
	default:
		return "Investigate discovered evidence"
	}
}

func discoveryID(discovery Discovery) string {
	return discovery.Type + "\x00" + discovery.Entity + "\x00" +
		discovery.From + "\x00" + discovery.To + "\x00" + discovery.Kind
}

func (catalog *TaskTemplateCatalog) generateCandidates(
	contract InvestigationContract,
	goalFilter map[string]struct{},
) ([]TaskCandidate, error) {
	if strings.TrimSpace(contract.Question) == "" {
		return nil, fmt.Errorf("%w: question is required", ErrPlanInvalid)
	}
	templates := catalog.List()
	if len(templates) == 0 {
		return nil, fmt.Errorf("%w: task template catalog is empty", ErrCapabilityGap)
	}
	goals := make(map[string]EvidenceGoal, len(contract.Goals))
	for _, goal := range contract.Goals {
		if strings.TrimSpace(goal.ID) == "" {
			return nil, fmt.Errorf("%w: goal id is required", ErrPlanInvalid)
		}
		goals[goal.ID] = goal
	}
	candidates := make([]TaskCandidate, 0)
	for _, template := range templates {
		if template.ProposalOnly {
			continue
		}
		matched := make([]string, 0, len(contract.Goals))
		for _, goal := range contract.Goals {
			if goalFilter != nil {
				if _, wanted := goalFilter[goal.ID]; !wanted {
					continue
				}
			}
			if templateMatchesGoal(template, goal) {
				matched = append(matched, goal.ID)
			}
		}
		if len(matched) == 0 {
			continue
		}
		// One candidate per template covers all matched goals so a shared
		// investigation chain never emits duplicate capability providers.
		candidates = append(candidates, TaskCandidate{
			ID:           fmt.Sprintf("task_%s_v%d", template.ID, template.Version),
			Template:     TaskTemplateRef{ID: template.ID, Version: template.Version},
			Objective:    templateObjective(template, contract),
			GoalIDs:      matched,
			AllowedTools: append([]tool.ToolID(nil), template.ToolGrant...),
			Budget:       template.CostProfile,
		})
	}
	requiredFilter := goalFilter
	if requiredFilter == nil {
		requiredFilter = make(map[string]struct{}, len(contract.Goals))
		for _, goal := range contract.Goals {
			requiredFilter[goal.ID] = struct{}{}
		}
	}
	for _, goal := range contract.Goals {
		if !goal.Required {
			continue
		}
		if _, required := requiredFilter[goal.ID]; !required {
			continue
		}
		found := false
		for _, candidate := range candidates {
			if containsString(candidate.GoalIDs, goal.ID) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: no template can cover required goal %q", ErrCapabilityGap, goal.ID)
		}
	}
	if err := wireCandidateDependencies(catalog, candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

// wireCandidateDependencies links a candidate to the single candidate that provides
// each of its template's required inputs. The wiring is driven by template metadata,
// not by any concrete question or entity name.
func wireCandidateDependencies(catalog *TaskTemplateCatalog, candidates []TaskCandidate) error {
	providers := make(map[string][]string)
	for _, candidate := range candidates {
		template, err := catalog.Resolve(candidate.Template)
		if err != nil {
			return err
		}
		for _, provided := range template.Provides {
			providers[provided] = append(providers[provided], candidate.ID)
		}
	}
	for index := range candidates {
		template, err := catalog.Resolve(candidates[index].Template)
		if err != nil {
			return err
		}
		for _, required := range template.RequiredInputs {
			providerIDs := providers[required]
			if len(providerIDs) == 0 {
				return fmt.Errorf("%w: no template provides required input %q for task %q", ErrCapabilityGap, required, candidates[index].ID)
			}
			if len(providerIDs) > 1 {
				return fmt.Errorf("%w: required input %q for task %q has multiple providers", ErrCapabilityGap, required, candidates[index].ID)
			}
			if providerIDs[0] != candidates[index].ID {
				candidates[index].Dependencies = append(candidates[index].Dependencies, providerIDs[0])
			}
		}
	}
	return nil
}

func validateTemplate(template TaskTemplate) error {
	if !templateIDPattern.MatchString(template.ID) {
		return fmt.Errorf("%w: template id %q is not canonical", ErrPlanInvalid, template.ID)
	}
	if template.Version <= 0 {
		return fmt.Errorf("%w: template %q version must be positive", ErrPlanInvalid, template.ID)
	}
	if err := validateSchemaRef(template.InputSchema); err != nil {
		return fmt.Errorf("%w: template %q input schema: %v", ErrPlanInvalid, template.ID, err)
	}
	if err := validateSchemaRef(template.OutputSchema); err != nil {
		return fmt.Errorf("%w: template %q output schema: %v", ErrPlanInvalid, template.ID, err)
	}
	switch template.Executor {
	case ExecutorDirectTool, ExecutorToolPipeline, ExecutorInvestigator, ExecutorVerifier, ExecutorComposer:
	default:
		return fmt.Errorf("%w: template %q executor %q is invalid", ErrPlanInvalid, template.ID, template.Executor)
	}
	if err := validateBudgetVector(template.CostProfile); err != nil {
		return fmt.Errorf("%w: template %q cost profile: %v", ErrPlanInvalid, template.ID, err)
	}
	if err := validateDiscoveryTypes(template.DiscoveryTypes); err != nil {
		return fmt.Errorf("%w: template %q discovery types: %v", ErrPlanInvalid, template.ID, err)
	}
	if err := validateCapabilityLabels(template.RequiredInputs); err != nil {
		return fmt.Errorf("%w: template %q required inputs: %v", ErrPlanInvalid, template.ID, err)
	}
	if err := validateCapabilityLabels(template.Provides); err != nil {
		return fmt.Errorf("%w: template %q provides: %v", ErrPlanInvalid, template.ID, err)
	}
	seen := make(map[string]struct{}, len(template.ToolGrant))
	for _, id := range template.ToolGrant {
		if strings.TrimSpace(string(id)) == "" {
			return fmt.Errorf("%w: template %q has an empty tool grant", ErrPlanInvalid, template.ID)
		}
		if _, duplicate := seen[string(id)]; duplicate {
			return fmt.Errorf("%w: template %q repeats tool %q", ErrPlanInvalid, template.ID, id)
		}
		seen[string(id)] = struct{}{}
	}
	for _, call := range template.ToolCalls {
		if _, allowed := seen[string(call.ToolID)]; !allowed {
			return fmt.Errorf("%w: template %q call uses ungranted tool %q", ErrPlanInvalid, template.ID, call.ToolID)
		}
	}
	return nil
}

func validateDiscoveryTypes(types []string) error {
	seen := make(map[string]struct{}, len(types))
	for _, discoveryType := range types {
		if discoveryType != "entity" && discoveryType != "dependency" {
			return fmt.Errorf("unsupported discovery type %q", discoveryType)
		}
		if _, duplicate := seen[discoveryType]; duplicate {
			return fmt.Errorf("discovery type %q is duplicated", discoveryType)
		}
		seen[discoveryType] = struct{}{}
	}
	return nil
}

func validateCapabilityLabels(labels []string) error {
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("capability label must not be empty")
		}
		if _, duplicate := seen[label]; duplicate {
			return fmt.Errorf("capability label %q is duplicated", label)
		}
		seen[label] = struct{}{}
	}
	return nil
}

func templateObjective(template TaskTemplate, contract InvestigationContract) string {
	var descriptions []string
	for _, goal := range contract.Goals {
		if templateMatchesGoal(template, goal) && strings.TrimSpace(goal.Description) != "" {
			descriptions = append(descriptions, goal.Description)
		}
	}
	if len(descriptions) > 0 {
		return strings.Join(descriptions, "; ")
	}
	return template.ID
}

func templateMatchesGoal(template TaskTemplate, goal EvidenceGoal) bool {
	if len(template.GoalKinds) == 0 {
		return true
	}
	return containsString(template.GoalKinds, goal.Kind)
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

func cloneTemplate(template TaskTemplate) TaskTemplate {
	template.GoalKinds = append([]string(nil), template.GoalKinds...)
	template.DiscoveryTypes = append([]string(nil), template.DiscoveryTypes...)
	template.SourceKinds = append([]string(nil), template.SourceKinds...)
	template.RequiredInputs = append([]string(nil), template.RequiredInputs...)
	template.Provides = append([]string(nil), template.Provides...)
	template.ToolGrant = append([]tool.ToolID(nil), template.ToolGrant...)
	template.Preconditions = append([]string(nil), template.Preconditions...)
	template.ToolCalls = append([]ToolCallSpec(nil), template.ToolCalls...)
	return template
}

func sameTemplate(left, right TaskTemplate) bool {
	return reflect.DeepEqual(left, right)
}
