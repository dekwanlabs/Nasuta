package writeaction

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/tool"
)

const (
	proposeBranch tool.ToolID = "propose_branch"
	proposeCommit tool.ToolID = "propose_commit"
)

// Proposer persists a request but cannot change the platform-owned catalog.
type Proposer interface {
	Propose(context.Context, Proposal) (tool.Result, error)
}

// Proposal is the canonical request emitted by the closed catalog.
type Proposal struct {
	ToolID     tool.ToolID
	IncidentID string
	Arguments  tool.Arguments
	Rationale  string
	Impact     string
}

// RegisterBuiltins installs the closed set of approval-gated write actions.
func RegisterBuiltins(registry *tool.Registry, proposer Proposer) error {
	if registry == nil {
		return fmt.Errorf("write action registry is required")
	}
	if proposer == nil {
		return fmt.Errorf("write action proposer is required")
	}
	return registry.RegisterAll([]tool.Tool{
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
			func(args tool.Arguments) string { return fmt.Sprintf("assignee=%s", args.String("assignee")) },
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
			func(args tool.Arguments) string {
				return fmt.Sprintf("branch_name=%s", args.String("branch_name"))
			},
		),
	})
}

func writeTool(
	id tool.ToolID,
	description string,
	properties map[string]any,
	required []string,
	proposer Proposer,
	impact func(tool.Arguments) string,
) tool.Tool {
	return tool.Tool{
		ID: id, Description: description, Kind: tool.KindWrite,
		InputSchema: tool.JSONSchema{
			"type": "object", "properties": properties, "required": required,
		},
		MCPHidden: true,
		Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
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
