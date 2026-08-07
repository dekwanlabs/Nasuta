package agentworkflow

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const DelegatedInvestigationID = "delegated.investigation"

// DefaultDelegatedInvestigation fixes the standard three-role read-only investigation DAG.
func DefaultDelegatedInvestigation(version int64, nodeTimeout time.Duration) (WorkflowDefinition, error) {
	if version <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("delegated investigation version must be positive")
	}
	if nodeTimeout <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("delegated investigation node timeout must be positive")
	}
	request := agentapi.SchemaRef{ID: "investigation.request", Version: 1}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundle := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	answer := agentapi.SchemaRef{ID: "investigation.answer", Version: 1}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition := WorkflowDefinition{
		ID: DelegatedInvestigationID, Version: version,
		Purpose:      "Run independent code, runtime-topology, and documentation investigations before grounded synthesis.",
		InputSchema:  request,
		OutputSchema: answer,
		Permissions:  readOnly,
		Budget: WorkflowBudget{
			MaxNodes: 5, MaxParallelism: 3, Timeout: 3 * nodeTimeout,
			MaxHandoffBytes: 1 << 20,
		},
		FailurePolicy: WorkflowFailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{
			investigationNode("investigate.code", "investigator.code", version, request, report, readOnly, nodeTimeout),
			investigationNode("investigate.runtime", "investigator.runtime", version, request, report, readOnly, nodeTimeout),
			investigationNode("investigate.docs", "investigator.docs", version, request, report, readOnly, nodeTimeout),
			{
				ID: "evidence.join", Kind: NodeJoin, InputSchema: report, OutputSchema: bundle,
				Permissions: readOnly, Timeout: nodeTimeout,
			},
			{
				ID: "synthesize", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "synthesizer", Version: version},
				InputSchema: bundle, OutputSchema: answer,
				Permissions: readOnly, Timeout: nodeTimeout,
			},
		},
		Edges: []EdgeDefinition{
			{From: "investigate.code", To: "evidence.join", Required: true},
			{From: "investigate.runtime", To: "evidence.join", Required: true},
			{From: "investigate.docs", To: "evidence.join", Required: true},
			{From: "evidence.join", To: "synthesize", Required: true},
		},
	}
	return definition, nil
}

func investigationNode(
	nodeID string,
	agentID string,
	version int64,
	input agentapi.SchemaRef,
	output agentapi.SchemaRef,
	permissions agentapi.PermissionPolicy,
	timeout time.Duration,
) NodeDefinition {
	return NodeDefinition{
		ID: nodeID, Kind: NodeAgent,
		Agent:       agentapi.DefinitionRef{ID: agentID, Version: version},
		InputSchema: input, OutputSchema: output,
		Permissions: permissions, Timeout: timeout,
	}
}
