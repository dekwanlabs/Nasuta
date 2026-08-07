package featurepipeline

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestDefaultDefinitionFixesSequentialArtifactPipeline(t *testing.T) {
	definition, err := DefaultDefinition(WorkflowVersion)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		NodeGenerateRequirementAnalysis,
		NodeApproveRequirementAnalysis,
		NodeGenerateTechnicalProposal,
		NodeApproveTechnicalProposal,
		NodeGenerateSystemDesign,
		NodeApproveSystemDesign,
		NodeGenerateImplementationPlan,
		NodeApproveImplementationPlan,
		NodeCoding,
		NodeValidation,
	}
	if definition.ID != WorkflowID || definition.Version != WorkflowVersion {
		t.Fatalf("workflow = %s@%d", definition.ID, definition.Version)
	}
	if len(definition.Nodes) != len(want) || len(definition.Edges) != len(want)-1 {
		t.Fatalf("nodes=%d edges=%d", len(definition.Nodes), len(definition.Edges))
	}
	for index, nodeID := range want {
		if definition.Nodes[index].ID != nodeID {
			t.Fatalf("node %d = %q, want %q", index, definition.Nodes[index].ID, nodeID)
		}
		if index == 0 {
			continue
		}
		edge := definition.Edges[index-1]
		if edge.From != want[index-1] || edge.To != nodeID || !edge.Required {
			t.Fatalf("edge %d = %+v", index-1, edge)
		}
	}
}

func TestPipelineSchemasRejectUnknownFields(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(Schemas()); err != nil {
		t.Fatal(err)
	}
	err := registry.Validate(requestSchema, []byte(`{
		"feature_id":"feat-1",
		"client_request_id":"request-1",
		"repository":"repo",
		"base_ref":"HEAD",
		"provider":"codex",
		"network_enabled":false,
		"workflow_id":"client-controlled"
	}`))
	if err == nil {
		t.Fatal("request schema accepted an unknown field")
	}
}
