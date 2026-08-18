package qa

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

// InvestigationRunner owns the durable workflow boundary used by QA.
type InvestigationRunner interface {
	Available() bool
	Start(context.Context, InvestigationRequest) error
	AwaitTerminal(context.Context, string) (InvestigationTerminal, error)
	LoadTerminal(context.Context, string) (InvestigationTerminal, error)
	Cancel(context.Context, string, int64) error
}

type InvestigationRequest struct {
	WorkflowRunID string
	Contract      TaskContract
	Proposal      *agentapi.TaskGraphProposal
	SeedEvidence  []tool.EvidenceUnit
	Actor         agentapi.Actor
}

// TaskContract is the bounded task projection shared by delegated investigators.
// The parent Run retains the original user question; child agents receive only
// the objective, admitted evidence goals, and admitted context.
type TaskContract struct {
	TaskID             string              `json:"task_id"`
	Objective          string              `json:"objective"`
	Entities           []EntityRef         `json:"entities"`
	InvestigationGoals []InvestigationGoal `json:"investigation_goals,omitempty"`
	EvidenceGoals      []EvidenceGoal      `json:"evidence_goals"`
	Context            TaskContext         `json:"context"`
}

type EntityRef struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Role    string   `json:"role,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// InvestigationGoal is one distinct deliverable admitted by execution routing.
type InvestigationGoal struct {
	ID                  string   `json:"id"`
	Objective           string   `json:"objective"`
	IndependentlyUseful bool     `json:"independently_useful"`
	DependsOn           []string `json:"depends_on"`
}

type EvidenceGoal struct {
	ID              string                    `json:"id"`
	Facet           string                    `json:"facet"`
	Required        bool                      `json:"required"`
	Sources         []agentapi.EvidenceSource `json:"sources"`
	RequiredSources []agentapi.EvidenceSource `json:"required_sources,omitempty"`
	Freshness       agentapi.FreshnessPolicy  `json:"freshness"`
	MinimumCoverage int                       `json:"minimum_coverage"`
	HighRisk        bool                      `json:"high_risk"`
}

type TaskContext struct {
	ConversationRefs []ConversationRef       `json:"conversation_refs,omitempty"`
	TimeRange        *TaskTimeRange          `json:"time_range,omitempty"`
	SeedMaterial     []agentapi.ContextBlock `json:"seed_material,omitempty"`
}

type ConversationRef struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Turn      int    `json:"turn,omitempty"`
}

type TaskTimeRange struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	ToExclusive bool      `json:"to_exclusive"`
	Raw         string    `json:"raw,omitempty"`
}

type InvestigationEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type InvestigationCitation struct {
	Claim    string                  `json:"claim"`
	Evidence []InvestigationEvidence `json:"evidence"`
}

type InvestigationUsage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
	ToolCalls       int64
	CostMicros      int64
}

type InvestigationStatus string

const (
	InvestigationSucceeded InvestigationStatus = "succeeded"
	InvestigationFailed    InvestigationStatus = "failed"
	InvestigationCancelled InvestigationStatus = "cancelled"
	InvestigationTimedOut  InvestigationStatus = "timed_out"
)

type InvestigationCompleteness string

const (
	InvestigationComplete    InvestigationCompleteness = "complete"
	InvestigationPartial     InvestigationCompleteness = "partial"
	InvestigationUnavailable InvestigationCompleteness = "unavailable"
)

// InvestigationTerminal carries only durable workflow terminal facts.
type InvestigationTerminal struct {
	WorkflowRunID string
	Status        InvestigationStatus
	ErrorCode     string
	Output        *InvestigationResult
	Usage         InvestigationUsage
	Completeness  InvestigationCompleteness
}

type InvestigationResult struct {
	Answer      string                  `json:"answer"`
	Citations   []InvestigationCitation `json:"citations"`
	Limitations []string                `json:"limitations"`
}

func contractFromPreparation(
	prepared *preparation,
	seedMaterial []agentapi.ContextBlock,
) TaskContract {
	query := prepared.analysis.QueryPlan
	entitySpecs := query.EntitySpecs
	if len(entitySpecs) == 0 {
		entitySpecs = make([]domain.EntitySpec, 0, len(query.Entities))
		for _, entity := range query.Entities {
			entitySpecs = append(entitySpecs, domain.EntitySpec{ID: entity})
		}
	}
	entities := make([]EntityRef, 0, len(entitySpecs))
	for _, entity := range domain.CanonicalEntitySpecs(entitySpecs) {
		entities = append(entities, EntityRef{
			ID: entity.ID, Label: entity.Label, Role: entity.Role,
			Aliases: append([]string(nil), entity.Aliases...),
		})
	}
	requiredFacets := domain.RequiredFacetsFor(query.Kind)
	goals := make([]EvidenceGoal, 0, len(requiredFacets))
	sources := evidenceGoalSources(prepared)
	freshness := evidenceGoalFreshness(prepared)
	minimumCoverage := 1
	if query.Kind == domain.QueryComparison {
		minimumCoverage = max(2, len(entities))
	}
	for _, facet := range requiredFacets {
		value := string(facet)
		goals = append(goals, EvidenceGoal{
			ID: value, Facet: value, Required: true,
			Sources: append([]agentapi.EvidenceSource(nil), sources...),
			RequiredSources: requiredEvidenceSources(
				prepared.analysis.QueryPlan.Kind, sources,
			),
			Freshness: freshness, MinimumCoverage: minimumCoverage,
			HighRisk: freshness == agentapi.FreshnessBoundedLive,
		})
	}
	investigationGoals := make(
		[]InvestigationGoal,
		0,
		len(prepared.planning.Execution.Tasks),
	)
	for _, task := range prepared.planning.Execution.Tasks {
		if !task.IndependentlyUseful || len(task.DependsOn) != 0 {
			continue
		}
		investigationGoals = append(investigationGoals, InvestigationGoal{
			ID: task.ID, Objective: task.Objective,
			IndependentlyUseful: task.IndependentlyUseful,
			// The filter above guarantees task.DependsOn is empty here; emit a
			// non-nil slice so JSON renders [] rather than null, which the
			// task.contract schema requires.
			DependsOn: []string{},
		})
	}
	taskContext := TaskContext{
		ConversationRefs: append([]ConversationRef(nil), prepared.conversationRefs...),
		SeedMaterial:     cloneContextBlocks(seedMaterial),
	}
	if prepared.analysis.HasTimeRange {
		resolved := prepared.analysis.TimeRange
		taskContext.TimeRange = &TaskTimeRange{
			From: resolved.From, To: resolved.To,
			ToExclusive: resolved.ToExclusive, Raw: resolved.Raw,
		}
	}
	return TaskContract{
		TaskID: prepared.request.RunID, Objective: taskContractObjective(prepared),
		Entities:           entities,
		InvestigationGoals: investigationGoals,
		EvidenceGoals:      goals,
		Context:            taskContext,
	}
}

func requiredEvidenceSources(kind domain.QueryKind, sources []agentapi.EvidenceSource) []agentapi.EvidenceSource {
	if kind != domain.QueryComparison {
		return nil
	}
	for _, source := range sources {
		if source == agentapi.EvidenceSourceInternal {
			return []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
		}
	}
	return nil
}

func taskContractObjective(prepared *preparation) string {
	if prepared == nil {
		return ""
	}
	if objective := investigation.BoundedSummary(
		prepared.planning.CleanQuestion,
	); objective != "" {
		return objective
	}
	return investigation.BoundedSummary(prepared.request.Question)
}

func evidenceGoalSources(prepared *preparation) []agentapi.EvidenceSource {
	plan := prepared.planning.Effective.Plan
	sources := make([]agentapi.EvidenceSource, 0, 4)
	seen := make(map[agentapi.EvidenceSource]struct{}, 4)
	add := func(source agentapi.EvidenceSource) {
		if _, exists := seen[source]; exists {
			return
		}
		seen[source] = struct{}{}
		sources = append(sources, source)
	}
	if plan.Has(domain.Memory) {
		add(agentapi.EvidenceSourceMemory)
	}
	if plan.Has(domain.Internal) {
		add(agentapi.EvidenceSourceInternal)
	}
	if plan.Has(domain.Web) {
		add(agentapi.EvidenceSourceWeb)
	}
	selected := make(map[string]struct{}, len(prepared.planning.RoutedToolIDs))
	for _, id := range prepared.planning.RoutedToolIDs {
		selected[id] = struct{}{}
	}
	for _, candidate := range prepared.toolCandidates {
		if _, ok := selected[candidate.ID]; !ok {
			continue
		}
		switch candidate.EvidenceSource {
		case string(tool.RoutingEvidenceInternal):
			add(agentapi.EvidenceSourceInternal)
		case string(tool.RoutingEvidenceMemory):
			add(agentapi.EvidenceSourceMemory)
		case string(tool.RoutingEvidenceWeb):
			add(agentapi.EvidenceSourceWeb)
		case string(tool.RoutingEvidenceRuntime):
			add(agentapi.EvidenceSourceRuntime)
		}
	}
	return sources
}

func evidenceGoalFreshness(prepared *preparation) agentapi.FreshnessPolicy {
	if prepared.analysis.HasTimeRange || toolsNeedInvestigation(
		prepared.toolCandidates,
		prepared.planning.RoutedToolIDs,
	) {
		return agentapi.FreshnessBoundedLive
	}
	plan := prepared.planning.Effective.Plan
	if plan.Has(domain.Web) || plan.Has(domain.Memory) {
		return agentapi.FreshnessCurrent
	}
	return agentapi.FreshnessStable
}

func contextBlockEvidence(blocks []agentapi.ContextBlock) []tool.EvidenceUnit {
	ledger := evidence.New(nil, "")
	for _, block := range blocks {
		ledger.Add(block.Evidence, "preloaded")
	}
	return ledger.Units()
}

func cloneContextBlocks(blocks []agentapi.ContextBlock) []agentapi.ContextBlock {
	if len(blocks) == 0 {
		return nil
	}
	cloned := make([]agentapi.ContextBlock, len(blocks))
	for index, block := range blocks {
		block.References = append([]agentapi.Reference(nil), block.References...)
		block.Evidence = cloneEvidenceUnits(block.Evidence)
		block.EvidenceConflicts = append(
			[]agentapi.EvidenceConflict(nil),
			block.EvidenceConflicts...,
		)
		cloned[index] = block
	}
	return cloned
}

func investigationOutcome(terminal InvestigationTerminal) (RunOutcome, error) {
	switch terminal.Status {
	case InvestigationSucceeded:
		if terminal.Output == nil {
			return RunOutcome{}, fmt.Errorf(
				"investigation workflow %q succeeded without output",
				terminal.WorkflowRunID,
			)
		}
		return successfulOutcome(
			*terminal.Output,
			terminal.Usage,
			terminal.Completeness,
		), nil
	case InvestigationFailed:
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: terminal.ErrorCode,
			Err: fmt.Errorf(
				"investigation workflow %q failed with code %q",
				terminal.WorkflowRunID,
				terminal.ErrorCode,
			),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	case InvestigationCancelled:
		return RunOutcome{
			Status: RunStatusAborted, ErrorCode: terminal.ErrorCode,
			Err:      fmt.Errorf("investigation workflow %q was cancelled", terminal.WorkflowRunID),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	case InvestigationTimedOut:
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: terminal.ErrorCode,
			Err:      fmt.Errorf("investigation workflow %q timed out", terminal.WorkflowRunID),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	default:
		return RunOutcome{}, fmt.Errorf(
			"investigation workflow %q has unknown terminal status %q",
			terminal.WorkflowRunID,
			terminal.Status,
		)
	}
}

func successfulOutcome(
	result InvestigationResult,
	usage InvestigationUsage,
	completeness InvestigationCompleteness,
) RunOutcome {
	answer := strings.TrimSpace(result.Answer)
	if answer == "" {
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: "empty_output", Err: ErrEmptyAnswer,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
	}
	references := investigationReferences(result.Citations)
	evidenceStatus := EvidenceComplete
	partialResults := len(result.Limitations)
	switch completeness {
	case InvestigationComplete:
	case InvestigationPartial:
		evidenceStatus = EvidencePartial
		if partialResults == 0 {
			partialResults = 1
		}
	case InvestigationUnavailable:
		evidenceStatus = EvidenceUnavailable
	default:
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: "invalid_completeness",
			Err:      fmt.Errorf("invalid investigation completeness %q", completeness),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
	}
	return RunOutcome{
		Status: RunStatusDone, Answer: answer, TokenUsed: int(usage.TotalTokens),
		References: references, HitCount: len(references),
		Evidence: EvidenceMetrics{
			Status: evidenceStatus, ResultCount: len(references),
			ToolCallCount: int(usage.ToolCalls), PartialResultCount: partialResults,
		},
	}
}

func investigationReferences(citations []InvestigationCitation) []agentapi.Reference {
	count := 0
	for _, citation := range citations {
		count += len(citation.Evidence)
	}
	references := make([]agentapi.Reference, 0, count)
	seen := make(map[string]struct{}, count)
	for _, citation := range citations {
		for _, evidence := range citation.Evidence {
			kind := strings.TrimSpace(evidence.Kind)
			reference := strings.TrimSpace(evidence.Reference)
			if kind == "" || reference == "" {
				continue
			}
			key := kind + "\x00" + reference
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			references = append(references, agentapi.Reference{
				Type: kind, Label: strings.TrimSpace(evidence.Summary), Target: reference,
			})
		}
	}
	return references
}
