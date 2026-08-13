package workflow

import (
	"fmt"
	"sort"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func (compiler *ProposalCompiler) compile(
	proposal validatedProposal,
	policy CompilationPolicy,
) (WorkflowDefinition, error) {
	nodes := make([]NodeDefinition, 0, len(proposal.tasks))
	tasks := make(map[string]validatedTask, len(proposal.tasks))
	for _, task := range proposal.tasks {
		tasks[task.spec.ID] = task
		nodes = append(nodes, NodeDefinition{
			ID: task.spec.ID, Kind: NodeAgent,
			Agent: agentapi.DefinitionRef{
				ID:      task.capability.Agent.ID,
				Version: task.capability.Agent.Version,
			},
			Capability: agentapi.CapabilityRef{
				ID:      task.capability.ID,
				Version: task.capability.Version,
			},
			Task: &TaskDirective{
				Purpose:        task.spec.Purpose,
				RequiredFacets: append([]string(nil), task.spec.RequiredFacets...),
				InputRefs:      append([]agentapi.EvidenceRef(nil), task.spec.InputRefs...),
				ParallelGroup:  task.spec.ParallelGroup,
			},
			CapabilityMaxConcurrency: task.capability.MaxConcurrency,
			RestrictVisibleTools:     true,
			VisibleToolIDs: append(
				[]string(nil),
				task.capability.ToolIDs...,
			),
			InputSchema:  task.capability.InputSchema,
			OutputSchema: task.capability.OutputSchema,
			Permissions: agentapi.PermissionPolicy{
				Scopes: append([]string(nil), task.capability.PermissionScope...),
			},
			Budget:    task.budget,
			Retry:     RetryPolicy{MaxAttempts: task.attempts},
			RetrySafe: task.capability.RetrySafe,
			Timeout:   policy.NodeTimeout,
			Optional:  task.spec.Optional,
		})
	}

	predecessors := make(map[string][]agentapi.TaskEdge, len(tasks))
	for _, edge := range proposal.edges {
		predecessors[edge.To] = append(predecessors[edge.To], edge)
	}
	joinTargets := make([]string, 0)
	for _, task := range proposal.tasks {
		if proposal.joinTargets[task.spec.ID] {
			joinTargets = append(joinTargets, task.spec.ID)
		}
	}
	sort.Strings(joinTargets)
	joinIDs := make(map[string]string, len(joinTargets))
	nodeIDs := make(map[string]struct{}, len(nodes)+len(joinTargets))
	for _, node := range nodes {
		nodeIDs[node.ID] = struct{}{}
	}
	for _, target := range joinTargets {
		joinID := policy.JoinID
		if len(joinTargets) > 1 {
			joinID += "." + target
		}
		if _, conflict := nodeIDs[joinID]; conflict {
			return WorkflowDefinition{}, fmt.Errorf(
				"compiled join id %q conflicts with a task",
				joinID,
			)
		}
		nodeIDs[joinID] = struct{}{}
		joinIDs[target] = joinID
		nodes = append(nodes, NodeDefinition{
			ID: joinID, Kind: NodeJoin,
			JoinMode:                policy.JoinMode,
			RejectEvidenceConflicts: policy.RejectEvidenceConflicts,
			InputSchema:             policy.JoinInputSchema, OutputSchema: policy.JoinOutputSchema,
			Permissions: agentapi.PermissionPolicy{
				Scopes: append([]string(nil), policy.Permissions.Scopes...),
			},
			Timeout: policy.NodeTimeout,
		})
	}

	edges := make([]EdgeDefinition, 0, len(proposal.edges)+len(joinTargets))
	for _, edge := range proposal.edges {
		target := edge.To
		if joinID := joinIDs[target]; joinID != "" {
			target = joinID
		}
		edges = append(edges, EdgeDefinition{
			From: edge.From, To: target, Required: edge.Required,
		})
	}
	for _, target := range joinTargets {
		edges = append(edges, EdgeDefinition{
			From: joinIDs[target], To: target, Required: true,
		})
	}
	return WorkflowDefinition{
		ID: policy.WorkflowID, Version: policy.WorkflowVersion,
		Purpose:      policy.Purpose,
		InputSchema:  policy.InputSchema,
		OutputSchema: policy.OutputSchema,
		Nodes:        nodes,
		Edges:        edges,
		Permissions: agentapi.PermissionPolicy{
			Scopes: append([]string(nil), policy.Permissions.Scopes...),
		},
		Budget:        proposal.workflowBudget,
		FailurePolicy: WorkflowFailurePolicy{Mode: policy.FailureMode},
	}, nil
}
