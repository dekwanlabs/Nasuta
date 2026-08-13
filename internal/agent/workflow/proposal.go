package workflow

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// CompilationPolicy fixes every executable choice that a planner cannot make.
type CompilationPolicy struct {
	WorkflowID        string
	WorkflowVersion   int64
	Purpose           string
	InputSchema       agentapi.SchemaRef
	OutputSchema      agentapi.SchemaRef
	Permissions       agentapi.PermissionPolicy
	CallerPermissions agentapi.PermissionPolicy
	Budget            WorkflowBudget
	NodeTimeout       time.Duration
	CapabilityBudgets map[string]NodeBudget
	MaxTasks          int
	MaxParallelism    int
	MaxAttempts       int
	MaxRounds         int
	MaxDepth          int
	RequiredGoals     []string
	JoinID            string
	JoinMode          JoinMode
	JoinInputSchema   agentapi.SchemaRef
	JoinOutputSchema  agentapi.SchemaRef
	FailureMode       FailureMode
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
	if compiler == nil || compiler.schemas == nil || compiler.capabilities == nil {
		return WorkflowDefinition{}, fmt.Errorf("proposal compiler is unavailable")
	}
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

type validatedProposal struct {
	tasks          []validatedTask
	edges          []agentapi.TaskEdge
	maxParallelism int
	maxDepth       int
	workflowBudget WorkflowBudget
}

type validatedTask struct {
	spec       agentapi.TaskSpec
	capability agentapi.Capability
	budget     NodeBudget
	attempts   int
}
