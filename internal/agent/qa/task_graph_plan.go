package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

const taskGraphPrompt = `You are a constrained task graph planner.
Return JSON only with this exact shape:
{"tasks":[{"id":"canonical.id","purpose":"specific investigation objective","capability":"allowed.capability","required_facets":["allowed_facet"],"depends_on":[]}]}

Rules:
- Emit exactly one task for every allowed capability and no other tasks.
- Copy each capability's required_facets exactly.
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
		return map[string]any{
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

// planTaskGraph constrains model planning to server-selected capabilities and validates the result.
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
						return validateTaskGraphDraft(*value, input.Capabilities)
					},
				},
			); err != nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("plan task graph: %w", err)
			}
			proposal, err := bindTaskGraphDraft(draft, input.Capabilities)
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
	if len(capabilities) < 2 {
		return nil, fmt.Errorf(
			"task graph planner requires at least two investigation capabilities",
		)
	}
	return capabilities, nil
}

func validateTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
) error {
	if len(draft.Tasks) != len(allowed) {
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
	selected := make(map[string]struct{}, len(draft.Tasks))
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
		purpose := strings.TrimSpace(task.Purpose)
		if purpose == "" || len(purpose) > 500 {
			return fmt.Errorf("task %q purpose must contain 1 to 500 bytes", task.ID)
		}
		facets, ok := allowedByID[task.Capability]
		if !ok {
			return fmt.Errorf(
				"task %q capability %q is not allowed",
				task.ID,
				task.Capability,
			)
		}
		if _, duplicate := selected[task.Capability]; duplicate {
			return fmt.Errorf("capability %q is selected more than once", task.Capability)
		}
		selected[task.Capability] = struct{}{}
		if !sameStringSet(task.RequiredFacets, facets) {
			return fmt.Errorf(
				"task %q facets do not match capability %q",
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
	for capabilityID := range allowedByID {
		if _, ok := selected[capabilityID]; !ok {
			return fmt.Errorf("required capability %q is missing", capabilityID)
		}
	}
	return nil
}

func bindTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
) (agentapi.TaskGraphProposal, error) {
	if err := validateTaskGraphDraft(draft, allowed); err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	facetsByCapability := make(map[string][]string, len(allowed))
	for _, capability := range allowed {
		facetsByCapability[capability.ID] = capability.RequiredFacets
	}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	proposal := agentapi.TaskGraphProposal{
		Tasks: make([]agentapi.TaskSpec, 0, len(draft.Tasks)+1),
		Edges: make([]agentapi.TaskEdge, 0, len(draft.Tasks)),
	}
	for _, task := range draft.Tasks {
		proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
			ID: task.ID, Purpose: strings.TrimSpace(task.Purpose),
			RequiredFacets: append(
				[]string(nil),
				facetsByCapability[task.Capability]...,
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

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] ||
			index > 0 && leftCopy[index] == leftCopy[index-1] {
			return false
		}
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
