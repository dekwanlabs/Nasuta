package writeaction

import (
	"context"
	"fmt"

	toolruntime "github.com/dekwanlabs/astris/tool"
)

const (
	proposeBranch toolruntime.ToolID = "propose_branch"
	proposeCommit toolruntime.ToolID = "propose_commit"
)

// Proposer persists a request but cannot change the platform-owned catalog.
type Proposer interface {
	Propose(context.Context, Proposal) (toolruntime.Result, error)
}

// Proposal is the canonical request emitted by the closed catalog.
type Proposal struct {
	ToolID     toolruntime.ToolID
	IncidentID string
	Arguments  toolruntime.Arguments
	Rationale  string
	Impact     string
}

// RegisterBuiltins installs the closed set of approval-gated write actions.
func RegisterBuiltins(registry *toolruntime.Registry, proposer Proposer) error {
	if registry == nil {
		return fmt.Errorf("write action registry is required")
	}
	if proposer == nil {
		return fmt.Errorf("write action proposer is required")
	}
	return registry.RegisterAll([]toolruntime.Tool{
		writeTool(
			proposeBranch,
			"Propose creating a fix branch for services affected by an incident. This creates a pending request for human approval and never writes directly.",
			map[string]any{
				"incident_id": stringProperty("The incident to create a fix branch for."),
				"service":     stringProperty("Optional service restriction."),
				"branch_name": stringProperty("Optional generated branch-name override."),
				"assignee":    stringProperty("Git assignee for the branch."),
			},
			[]string{"incident_id"},
			proposer,
			func(args toolruntime.Arguments) string { return fmt.Sprintf("assignee=%s", args.String("assignee")) },
		),
		writeTool(
			proposeCommit,
			"Propose committing a prepared fix branch and creating a merge request. This creates a pending request for human approval and never writes directly.",
			map[string]any{
				"incident_id": stringProperty("The incident whose fix is ready to commit."),
				"branch_name": stringProperty("Optional branch to commit."),
			},
			[]string{"incident_id"},
			proposer,
			func(args toolruntime.Arguments) string {
				return fmt.Sprintf("branch_name=%s", args.String("branch_name"))
			},
		),
	})
}

func writeTool(
	id toolruntime.ToolID,
	description string,
	properties map[string]any,
	required []string,
	proposer Proposer,
	impact func(toolruntime.Arguments) string,
) toolruntime.Tool {
	return toolruntime.Tool{
		ID: id, Description: description, Kind: toolruntime.KindWrite,
		InputSchema: toolruntime.JSONSchema{
			"type": "object", "properties": properties, "required": required,
		},
		MCPHidden: true,
		Handler: toolruntime.HandlerFunc(func(ctx context.Context, args toolruntime.Arguments) (toolruntime.Result, error) {
			incidentID := args.String("incident_id")
			return proposer.Propose(ctx, Proposal{
				ToolID: id, IncidentID: incidentID, Arguments: args,
				Rationale: fmt.Sprintf("agent proposed %s for incident %s", id, incidentID),
				Impact:    impact(args),
			})
		}),
	}
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
