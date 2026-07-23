package writeaction

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

type recordingProposer struct {
	proposal Proposal
}

func (proposer *recordingProposer) Propose(_ context.Context, proposal Proposal) (tool.Result, error) {
	proposer.proposal = proposal
	return tool.Result{Content: "pending"}, nil
}

func TestCatalogIsWriteOnlyAndHiddenFromMCP(t *testing.T) {
	registry := tool.NewRegistry()
	proposer := &recordingProposer{}
	if err := RegisterBuiltins(registry, proposer); err != nil {
		t.Fatal(err)
	}
	if tools := registry.Snapshot(tool.ReadPolicy()).Tools(); len(tools) != 0 {
		t.Fatalf("read policy exposed %d write tools", len(tools))
	}
	all := registry.Snapshot(tool.AllPolicy())
	if tools := all.MCPTools(); len(tools) != 0 {
		t.Fatalf("MCP exposed %d write tools", len(tools))
	}
	result, err := tool.NewExecutor(0).Execute(context.Background(), all, proposeBranch, tool.Arguments{
		"incident_id": "inc-1", "assignee": "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "pending" || proposer.proposal.ToolID != proposeBranch || proposer.proposal.IncidentID != "inc-1" {
		t.Fatalf("result=%#v proposal=%#v", result, proposer.proposal)
	}
}
