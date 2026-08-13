package workflow

import (
	"context"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

// CompilationPolicy fixes every executable choice that a planner cannot make.
type CompilationPolicy struct {
	WorkflowID              string
	WorkflowVersion         int64
	Purpose                 string
	InputSchema             agentapi.SchemaRef
	OutputSchema            agentapi.SchemaRef
	Permissions             agentapi.PermissionPolicy
	CallerPermissions       agentapi.PermissionPolicy
	Budget                  WorkflowBudget
	NodeTimeout             time.Duration
	CapabilityBudgets       map[string]NodeBudget
	CapabilityVersions      map[string]int64
	MaxTasks                int
	MaxParallelism          int
	MaxAttempts             int
	MaxRounds               int
	MaxDepth                int
	RequiredGoals           []string
	JoinID                  string
	JoinMode                JoinMode
	JoinInputSchema         agentapi.SchemaRef
	JoinOutputSchema        agentapi.SchemaRef
	RejectEvidenceConflicts bool
	FailureMode             FailureMode
}

// ProposalCompiler resolves planner choices through immutable server registries.
type ProposalCompiler struct {
	schemas      *agentapi.SchemaRegistry
	capabilities *agentapi.CapabilityRegistry
}

func NewProposalCompiler(
	schemas *agentapi.SchemaRegistry,
	capabilities *agentapi.CapabilityRegistry,
) (*ProposalCompiler, error) {
	if schemas == nil {
		return nil, fmt.Errorf("proposal compiler schema registry is required")
	}
	if capabilities == nil {
		return nil, fmt.Errorf("proposal compiler capability registry is required")
	}
	return &ProposalCompiler{schemas: schemas, capabilities: capabilities}, nil
}

// Compile turns an untrusted task proposal into one immutable workflow definition.
func (compiler *ProposalCompiler) Compile(
	proposal agentapi.TaskGraphProposal,
	policy CompilationPolicy,
) (WorkflowDefinition, error) {
	return compiler.CompileContext(context.Background(), proposal, policy)
}

// CompileContext compiles a proposal and projects its acceptance into the active trace.
func (compiler *ProposalCompiler) CompileContext(
	ctx context.Context,
	proposal agentapi.TaskGraphProposal,
	policy CompilationPolicy,
) (WorkflowDefinition, error) {
	if compiler == nil || compiler.schemas == nil || compiler.capabilities == nil {
		return WorkflowDefinition{}, fmt.Errorf("proposal compiler is unavailable")
	}
	definition, err := runtrace.Invoke(
		ctx,
		taskGraphProposedTraceSpec,
		taskGraphTraceInput{proposal: proposal, policy: policy},
		func(
			_ context.Context,
			input taskGraphTraceInput,
		) (WorkflowDefinition, error) {
			return compiler.compileUntraced(input.proposal, input.policy)
		},
	)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	_, _ = runtrace.Invoke(
		ctx,
		taskGraphAcceptedTraceSpec,
		definition,
		func(
			_ context.Context,
			input WorkflowDefinition,
		) (WorkflowDefinition, error) {
			return input, nil
		},
	)
	return definition, nil
}

func (compiler *ProposalCompiler) compileUntraced(
	proposal agentapi.TaskGraphProposal,
	policy CompilationPolicy,
) (WorkflowDefinition, error) {
	validated, err := compiler.validate(proposal, policy)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	definition, err := compiler.compile(validated, policy)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	prepared, err := Prepare(definition, compiler.schemas)
	if err != nil {
		return WorkflowDefinition{}, fmt.Errorf("prepare compiled workflow: %w", err)
	}
	return prepared, nil
}

type taskGraphTraceInput struct {
	proposal agentapi.TaskGraphProposal
	policy   CompilationPolicy
}

var taskGraphProposedTraceSpec = runtrace.Spec[
	taskGraphTraceInput,
	WorkflowDefinition,
]{
	Operation: "task_graph.proposed",
	Node:      "task_graph.proposed",
	Input: func(input taskGraphTraceInput) map[string]any {
		tasks := make([]map[string]any, 0, len(input.proposal.Tasks))
		for _, task := range input.proposal.Tasks {
			tasks = append(tasks, map[string]any{
				"id":              task.ID,
				"capability":      task.Capability,
				"required_facets": append([]string(nil), task.RequiredFacets...),
				"optional":        task.Optional,
				"parallel_group":  task.ParallelGroup,
			})
		}
		edges := make([]map[string]any, 0, len(input.proposal.Edges))
		for _, edge := range input.proposal.Edges {
			edges = append(edges, map[string]any{
				"from": edge.From, "to": edge.To, "required": edge.Required,
			})
		}
		return map[string]any{
			"workflow_id":      input.policy.WorkflowID,
			"workflow_version": input.policy.WorkflowVersion,
			"tasks":            tasks,
			"edges":            edges,
			"stop": map[string]any{
				"max_tasks":       input.proposal.Stop.MaxTasks,
				"max_parallelism": input.proposal.Stop.MaxParallelism,
				"max_attempts":    input.proposal.Stop.MaxAttempts,
				"max_rounds":      input.proposal.Stop.MaxRounds,
				"max_depth":       input.proposal.Stop.MaxDepth,
			},
		}
	},
	Output: func(
		_ taskGraphTraceInput,
		output WorkflowDefinition,
		err error,
	) map[string]any {
		if err != nil {
			return map[string]any{"accepted": false, "error": err.Error()}
		}
		return map[string]any{
			"accepted":      true,
			"workflow_id":   output.ID,
			"workflow_hash": output.ContentHash,
		}
	},
}

var taskGraphAcceptedTraceSpec = runtrace.Spec[
	WorkflowDefinition,
	WorkflowDefinition,
]{
	Operation: "task_graph.accepted",
	Node:      "task_graph.accepted",
	Input: func(input WorkflowDefinition) map[string]any {
		return map[string]any{
			"workflow_id":      input.ID,
			"workflow_version": input.Version,
			"workflow_hash":    input.ContentHash,
		}
	},
	Output: func(
		_ WorkflowDefinition,
		output WorkflowDefinition,
		_ error,
	) map[string]any {
		nodes := make([]map[string]any, 0, len(output.Nodes))
		for _, node := range output.Nodes {
			item := map[string]any{
				"id":       node.ID,
				"kind":     node.Kind,
				"optional": node.Optional,
			}
			if node.Capability.ID != "" {
				item["capability"] = node.Capability.ID
				item["capability_version"] = node.Capability.Version
			}
			nodes = append(nodes, item)
		}
		return map[string]any{
			"nodes": nodes,
			"budget": map[string]any{
				"max_nodes":       output.Budget.MaxNodes,
				"max_parallelism": output.Budget.MaxParallelism,
				"max_tool_calls":  output.Budget.MaxToolCalls,
				"max_retries":     output.Budget.MaxRetries,
			},
		}
	},
}

type validatedProposal struct {
	tasks          []validatedTask
	edges          []agentapi.TaskEdge
	maxParallelism int
	maxDepth       int
	workflowBudget WorkflowBudget
	joinTargets    map[string]bool
}

type validatedTask struct {
	spec       agentapi.TaskSpec
	capability agentapi.Capability
	budget     NodeBudget
	attempts   int
}
