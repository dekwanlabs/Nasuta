package writeaction

import (
	"context"
	"testing"

	toolruntime "github.com/dekwanlabs/astris/tool"
)

type recordingProposer struct {
	proposal Proposal
}

func (proposer *recordingProposer) Propose(_ context.Context, proposal Proposal) (toolruntime.Result, error) {
	proposer.proposal = proposal
	return toolruntime.Result{Content: "pending"}, nil
}

func TestCatalogIsWriteOnlyAndHiddenFromMCP(t *testing.T) {
	registry := toolruntime.NewRegistry()
	proposer := &recordingProposer{}
	if err := RegisterBuiltins(registry, proposer); err != nil {
		t.Fatal(err)
	}
	if tools := registry.Snapshot(toolruntime.ReadPolicy()).Tools(); len(tools) != 0 {
		t.Fatalf("read policy exposed %d write tools", len(tools))
	}
	all := registry.Snapshot(toolruntime.AllPolicy())
	if tools := all.MCPTools(); len(tools) != 0 {
		t.Fatalf("MCP exposed %d write tools", len(tools))
	}
	result, err := toolruntime.NewExecutor(0).Execute(context.Background(), all, proposeBranch, toolruntime.Arguments{
		"incident_id": "inc-1", "assignee": "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "pending" || proposer.proposal.ToolID != proposeBranch || proposer.proposal.IncidentID != "inc-1" {
		t.Fatalf("result=%#v proposal=%#v", result, proposer.proposal)
	}
}
