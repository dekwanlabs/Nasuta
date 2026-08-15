package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

const taskGraphPrompt = `You are a constrained task graph planner.
Return JSON only with this exact shape:
{"tasks":[{"id":"canonical.id","purpose":"specific investigation objective","capability":"allowed.capability","required_facets":["allowed_facet"],"depends_on":[]}]}

Rules:
- Emit exactly one investigator task for every admitted investigation goal and no other tasks.
- Preserve each investigation goal's id and objective in the corresponding task.
- Choose one allowed capability for each goal; multiple goals may use the same capability.
- Required facets must be selected from the capability's allowed facets.
- Task ids must be unique canonical lowercase ids.
- Purpose must be specific to the question and no longer than 500 characters.
- This is one parallel investigation round, so depends_on must be empty.
- Do not add a synthesizer, tools, providers, schemas, permissions, budgets, retries, stop policies, or unknown fields.`

var taskGraphCanonicalID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type taskGraphCapability struct {
	ID             string   `json:"id"`
	RequiredFacets []string `json:"required_facets"`
}

type taskGraphPlannerRequest struct {
	Question            string                `json:"question"`
	Objective           string                `json:"objective"`
	Entities            []EntityRef           `json:"entities,omitempty"`
	InvestigationGoals  []InvestigationGoal   `json:"investigation_goals"`
	EvidenceGoals       []EvidenceGoal        `json:"evidence_goals"`
	AllowedCapabilities []taskGraphCapability `json:"allowed_capabilities"`
}

type taskGraphDraft struct {
	Tasks []taskGraphDraftTask `json:"tasks"`
}

type taskGraphDraftTask struct {
	ID             string   `json:"id"`
	Purpose        string   `json:"purpose"`
	Capability     string   `json:"capability"`
	RequiredFacets []string `json:"required_facets"`
	DependsOn      []string `json:"depends_on"`
}

type taskGraphPlanningInput struct {
	Contract     TaskContract
	Capabilities []taskGraphCapability
}

type taskGraphPlanningOutput struct {
	Proposal agentapi.TaskGraphProposal
}

var taskGraphPlanningSpec = runtrace.Spec[taskGraphPlanningInput, taskGraphPlanningOutput]{
	Operation: "agent.task_graph_plan",
	Node:      "task_graph_plan",
	Input: func(input taskGraphPlanningInput) map[string]any {
		capabilities := make([]string, 0, len(input.Capabilities))
		for _, capability := range input.Capabilities {
			capabilities = append(capabilities, capability.ID)
		}
		goals := make([]string, 0, len(input.Contract.InvestigationGoals))
		for _, goal := range input.Contract.InvestigationGoals {
			goals = append(goals, goal.ID)
		}
		return map[string]any{
			"investigation_goals":  len(input.Contract.InvestigationGoals),
			"goal_ids":             goals,
			"evidence_goals":       len(input.Contract.EvidenceGoals),
			"allowed_capabilities": capabilities,
		}
	},
	Output: func(
		_ taskGraphPlanningInput,
		output taskGraphPlanningOutput,
		err error,
	) map[string]any {
		if err != nil {
			return map[string]any{
				"accepted": false,
				"fallback": "deterministic_goal_mapping",
				"error":    err.Error(),
			}
		}
		tasks := make([]map[string]any, 0, len(output.Proposal.Tasks))
		for _, task := range output.Proposal.Tasks {
			tasks = append(tasks, map[string]any{
				"id":              task.ID,
				"capability":      task.Capability,
				"required_facets": append([]string(nil), task.RequiredFacets...),
			})
		}
		return map[string]any{"accepted": true, "tasks": tasks}
	},
	Status: func(_ taskGraphPlanningOutput, err error) string {
		if err != nil {
			return "degraded"
		}
		return ""
	},
}

// planTaskGraph asks the model for a bounded task decomposition.
// Only capabilities selected by the server are exposed to planning.
// The proposal is validated before it can reach Workflow execution.
func (svc *Service) planTaskGraph(
	ctx context.Context,
	contract TaskContract,
) (agentapi.TaskGraphProposal, error) {
	capabilities, capabilityErr := taskGraphCapabilities(contract)
	output, err := runtrace.Invoke(
		ctx,
		taskGraphPlanningSpec,
		taskGraphPlanningInput{Contract: contract, Capabilities: capabilities},
		func(
			ctx context.Context,
			input taskGraphPlanningInput,
		) (taskGraphPlanningOutput, error) {
			if capabilityErr != nil {
				return taskGraphPlanningOutput{}, capabilityErr
			}
			if svc.fastLLM == nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("task graph planner LLM is unavailable")
			}
			request, err := json.Marshal(taskGraphPlannerRequest{
				Question: contract.Question, Objective: contract.Objective,
				Entities: append([]EntityRef(nil), contract.Entities...),
				InvestigationGoals: append(
					[]InvestigationGoal(nil),
					contract.InvestigationGoals...,
				),
				EvidenceGoals: append(
					[]EvidenceGoal(nil),
					contract.EvidenceGoals...,
				),
				AllowedCapabilities: input.Capabilities,
			})
			if err != nil {
				return taskGraphPlanningOutput{}, fmt.Errorf(
					"marshal task graph planner request: %w",
					err,
				)
			}
			var draft taskGraphDraft
			maxTokens := svc.routerMaxTokens
			if maxTokens <= 0 {
				maxTokens = 512
			}
			if err := svc.fastLLM.ChatJSON(
				ctx,
				taskGraphPrompt,
				string(request),
				&draft,
				llm.CallOptions{
					MaxTokens:             maxTokens,
					MaxAttempts:           1,
					DisallowUnknownFields: true,
					Validate: func(parsed any) error {
						value, ok := parsed.(*taskGraphDraft)
						if !ok {
							return fmt.Errorf("unexpected task graph planner output %T", parsed)
						}
						return validateTaskGraphDraft(
							*value, input.Capabilities, input.Contract.InvestigationGoals...,
						)
					},
				},
			); err != nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("plan task graph: %w", err)
			}
			proposal, err := bindTaskGraphDraft(
				draft, input.Capabilities, input.Contract.InvestigationGoals...,
			)
			if err != nil {
				return taskGraphPlanningOutput{}, err
			}
			return taskGraphPlanningOutput{Proposal: proposal}, nil
		},
	)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	return output.Proposal, nil
}

func taskGraphCapabilities(contract TaskContract) ([]taskGraphCapability, error) {
	capabilities := make([]taskGraphCapability, 0, len(contract.EvidenceGoals))
	byID := make(map[string]int, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		sources := goal.Sources
		if len(sources) == 0 {
			sources = []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
		}
		for _, source := range sources {
			capabilityID := executionCapability(source, goal.Facet)
			if capabilityID == "" || capabilityID == "knowledge.internal.inspect" {
				return nil, fmt.Errorf(
					"evidence goal %q source %q has no investigation capability",
					goal.Facet,
					source,
				)
			}
			index, exists := byID[capabilityID]
			if !exists {
				index = len(capabilities)
				byID[capabilityID] = index
				capabilities = append(capabilities, taskGraphCapability{ID: capabilityID})
			}
			if !containsString(capabilities[index].RequiredFacets, goal.Facet) {
				capabilities[index].RequiredFacets = append(
					capabilities[index].RequiredFacets,
					goal.Facet,
				)
			}
		}
	}
	return capabilities, nil
}

func validateTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	goals ...InvestigationGoal,
) error {
	if len(goals) > 0 && len(draft.Tasks) != len(goals) {
		return fmt.Errorf(
			"task graph has %d investigator tasks, expected %d investigation goals",
			len(draft.Tasks), len(goals),
		)
	}
	if len(goals) == 0 && len(draft.Tasks) != len(allowed) {
		return fmt.Errorf(
			"task graph has %d investigator tasks, expected %d",
			len(draft.Tasks),
			len(allowed),
		)
	}
	allowedByID := make(map[string][]string, len(allowed))
	for _, capability := range allowed {
		allowedByID[capability.ID] = capability.RequiredFacets
	}
	taskIDs := make(map[string]struct{}, len(draft.Tasks))
	goalByID := make(map[string]InvestigationGoal, len(goals))
	for _, goal := range goals {
		goalByID[goal.ID] = goal
	}
	for _, task := range draft.Tasks {
		if !taskGraphCanonicalID.MatchString(task.ID) ||
			task.ID == "synthesize" ||
			strings.HasPrefix(task.ID, "evidence.join") ||
			strings.HasPrefix(task.ID, "evidence.verify") {
			return fmt.Errorf("task id %q is invalid or reserved", task.ID)
		}
		if _, duplicate := taskIDs[task.ID]; duplicate {
			return fmt.Errorf("task id %q is duplicated", task.ID)
		}
		taskIDs[task.ID] = struct{}{}
		if len(goals) > 0 {
			if _, ok := goalByID[task.ID]; !ok {
				return fmt.Errorf("task %q does not match an investigation goal", task.ID)
			}
		}
		purpose := strings.TrimSpace(task.Purpose)
		if purpose == "" || utf8.RuneCountInString(purpose) > 500 {
			return fmt.Errorf("task %q purpose must contain 1 to 500 characters", task.ID)
		}
		facets, ok := allowedByID[task.Capability]
		if !ok {
			return fmt.Errorf(
				"task %q capability %q is not allowed",
				task.ID,
				task.Capability,
			)
		}
		if len(task.RequiredFacets) == 0 || !subsetStringSet(task.RequiredFacets, facets) {
			return fmt.Errorf(
				"task %q facets are not allowed for capability %q",
				task.ID,
				task.Capability,
			)
		}
		if len(task.DependsOn) != 0 {
			return fmt.Errorf(
				"task %q dependencies are not supported in the single-round investigation planner",
				task.ID,
			)
		}
	}
	if len(goals) == 0 {
		for capabilityID := range allowedByID {
			found := false
			for _, task := range draft.Tasks {
				if task.Capability == capabilityID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("required capability %q is missing", capabilityID)
			}
		}
	} else {
		requiredFacets := make(map[string]struct{})
		for _, capability := range allowed {
			for _, facet := range capability.RequiredFacets {
				requiredFacets[facet] = struct{}{}
			}
		}
		for _, task := range draft.Tasks {
			for _, facet := range task.RequiredFacets {
				delete(requiredFacets, facet)
			}
		}
		if len(requiredFacets) > 0 {
			return fmt.Errorf("task graph does not cover every required evidence facet")
		}
	}
	return nil
}

func buildTaskGraphFallback(contract TaskContract) (agentapi.TaskGraphProposal, error) {
	capabilities, err := taskGraphCapabilities(contract)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	goals := contract.InvestigationGoals
	if len(goals) < 2 || len(goals) > 4 {
		return agentapi.TaskGraphProposal{}, fmt.Errorf(
			"deterministic task graph requires 2 to 4 investigation goals",
		)
	}
	selected, err := selectFallbackCapabilities(capabilities, len(goals))
	if err != nil {
		return agentapi.TaskGraphProposal{}, fmt.Errorf(
			"select deterministic task capabilities: %w",
			err,
		)
	}
	draft := taskGraphDraft{Tasks: make([]taskGraphDraftTask, 0, len(goals))}
	for index, goal := range goals {
		capability := selected[index%len(selected)]
		draft.Tasks = append(draft.Tasks, taskGraphDraftTask{
			ID: goal.ID, Purpose: goal.Objective,
			Capability: capability.ID,
			RequiredFacets: append(
				[]string(nil),
				capability.RequiredFacets...,
			),
		})
	}
	return bindTaskGraphDraft(draft, capabilities, goals...)
}

func selectFallbackCapabilities(
	capabilities []taskGraphCapability,
	limit int,
) ([]taskGraphCapability, error) {
	uncovered := make(map[string]struct{})
	for _, capability := range capabilities {
		for _, facet := range capability.RequiredFacets {
			uncovered[facet] = struct{}{}
		}
	}
	selected := make([]taskGraphCapability, 0, min(limit, len(capabilities)))
	used := make(map[string]struct{}, len(capabilities))
	for len(uncovered) > 0 && len(selected) < limit {
		bestIndex := -1
		bestCoverage := 0
		for index, capability := range capabilities {
			if _, ok := used[capability.ID]; ok {
				continue
			}
			coverage := 0
			for _, facet := range capability.RequiredFacets {
				if _, ok := uncovered[facet]; ok {
					coverage++
				}
			}
			if coverage > bestCoverage {
				bestIndex = index
				bestCoverage = coverage
			}
		}
		if bestIndex < 0 {
			break
		}
		capability := capabilities[bestIndex]
		selected = append(selected, capability)
		used[capability.ID] = struct{}{}
		for _, facet := range capability.RequiredFacets {
			delete(uncovered, facet)
		}
	}
	if len(selected) == 0 || len(uncovered) > 0 {
		return nil, fmt.Errorf(
			"cannot cover evidence facets with %d investigation tasks",
			limit,
		)
	}
	return selected, nil
}

func bindTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	goals ...InvestigationGoal,
) (agentapi.TaskGraphProposal, error) {
	if err := validateTaskGraphDraft(draft, allowed, goals...); err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	facetsByCapability := make(map[string][]string, len(allowed))
	for _, capability := range allowed {
		facetsByCapability[capability.ID] = capability.RequiredFacets
	}
	proposal := agentapi.TaskGraphProposal{
		Tasks: make([]agentapi.TaskSpec, 0, len(draft.Tasks)+1),
		Edges: make([]agentapi.TaskEdge, 0, len(draft.Tasks)),
	}
	goalByID := make(map[string]InvestigationGoal, len(goals))
	for _, goal := range goals {
		goalByID[goal.ID] = goal
	}
	for _, task := range draft.Tasks {
		purpose := strings.TrimSpace(task.Purpose)
		if goal, ok := goalByID[task.ID]; ok {
			purpose = goal.Objective
		}
		proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
			ID: task.ID, Purpose: purpose,
			RequiredFacets: orderedTaskFacets(
				task.RequiredFacets,
				facetsByCapability[task.Capability],
			),
			Capability: task.Capability, OutputSchema: report,
			Optional: true, MaxAttempts: 2,
		})
		proposal.Edges = append(proposal.Edges, agentapi.TaskEdge{
			From: task.ID, To: "synthesize",
		})
	}
	proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
		ID: "synthesize", Purpose: "Synthesize the available evidence.",
		Capability:   "evidence.synthesize",
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		MaxAttempts:  2,
	})
	return proposal, nil
}

func orderedTaskFacets(selected, allowed []string) []string {
	selectedSet := make(map[string]struct{}, len(selected))
	for _, facet := range selected {
		selectedSet[facet] = struct{}{}
	}
	ordered := make([]string, 0, len(selected))
	for _, facet := range allowed {
		if _, ok := selectedSet[facet]; ok {
			ordered = append(ordered, facet)
		}
	}
	return ordered
}

func subsetStringSet(values, allowed []string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		allowedSet[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := allowedSet[value]; !ok {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
