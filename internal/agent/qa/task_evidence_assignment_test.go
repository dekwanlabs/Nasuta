package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestAssignTaskEvidenceOwnersDoesNotShareSeedGroupsBetweenSiblings(
	t *testing.T,
) {
	report := agentapi.InvestigationReportSchemaRef()
	proposal := agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{
		{
			ID: "investigate.design.code.1", RequiredFacets: []string{"core_flow"},
			Capability: "knowledge.code.inspect", OutputSchema: report,
		},
		{
			ID: "investigate.implementation.code.1", RequiredFacets: []string{"core_flow"},
			Capability: "knowledge.code.inspect", OutputSchema: report,
		},
		{
			ID: "investigate.design.docs.1", RequiredFacets: []string{"business_domain"},
			Capability: "knowledge.docs.verify", OutputSchema: report,
		},
		{
			ID: "synthesize", Capability: "evidence.synthesize",
			OutputSchema: agentapi.InvestigationAnswerSchemaRef(),
		},
	}}
	seed := []tool.EvidenceUnit{
		{
			SourceKind: "code", Target: "repo/order.go",
			Sections: []string{"L1-L20", "L21-L40"}, ContentHash: "order",
			Facets: []string{"core_flow"},
		},
		{
			SourceKind: "code", Target: "repo/payment.go",
			Sections: []string{"L1-L20"}, ContentHash: "payment",
			Facets: []string{"core_flow"},
		},
		{
			SourceKind: "runbook", Target: "docs/order.md",
			Sections: []string{"overview"}, ContentHash: "docs",
			Facets: []string{"business_domain"},
		},
		{
			SourceKind: "runtime", Target: "trace-1",
			ContentHash: "runtime", Facets: []string{"runtime_behavior"},
		},
	}

	assignTaskEvidenceOwners(&proposal, seed, TaskContract{})

	first := proposal.Tasks[0].InputRefs
	second := proposal.Tasks[1].InputRefs
	docs := proposal.Tasks[2].InputRefs
	if first == nil || second == nil || docs == nil {
		t.Fatalf("investigator refs must be explicit: %#v", proposal.Tasks)
	}
	if len(first) != 2 || first[0].Target != "repo/order.go" ||
		first[1].Target != "repo/order.go" {
		t.Fatalf("first code task refs = %#v", first)
	}
	if len(second) != 1 || second[0].Target != "repo/payment.go" {
		t.Fatalf("second code task refs = %#v", second)
	}
	if len(docs) != 1 || docs[0].Target != "docs/order.md" {
		t.Fatalf("docs task refs = %#v", docs)
	}
	if proposal.Tasks[3].InputRefs != nil {
		t.Fatalf("synthesizer refs = %#v", proposal.Tasks[3].InputRefs)
	}

	owners := make(map[string]string)
	for _, task := range proposal.Tasks[:3] {
		for _, ref := range task.InputRefs {
			owner, exists := owners[ref.SourceKind+"\x00"+ref.Target]
			if exists && owner != task.ID {
				t.Fatalf(
					"evidence %s is shared by %q and %q",
					ref.Target,
					owner,
					task.ID,
				)
			}
			owners[ref.SourceKind+"\x00"+ref.Target] = task.ID
		}
	}
	if _, assigned := owners["runtime\x00trace-1"]; assigned {
		t.Fatal("unmatched runtime evidence was assigned")
	}
}

func TestAssignTaskEvidenceOwnersPrefersSubjectIdentity(t *testing.T) {
	report := agentapi.InvestigationReportSchemaRef()
	proposal := agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{
		{
			ID: "investigate.checkout.docs.1", Capability: "knowledge.docs.verify",
			InvestigationGoalIDs: []string{"checkout"}, RequiredFacets: []string{"business_domain"},
			OutputSchema: report,
		},
		{
			ID: "investigate.billing.docs.1", Capability: "knowledge.docs.verify",
			InvestigationGoalIDs: []string{"billing"}, RequiredFacets: []string{"business_domain"},
			OutputSchema: report,
		},
	}}
	assignTaskEvidenceOwners(&proposal, []tool.EvidenceUnit{
		{
			SourceKind: "runbook", Target: "docs/billing-overview.md",
			Sections: []string{"overview"}, Facets: []string{"business_domain"},
		},
		{
			SourceKind: "runbook", Target: "docs/shared-readme.md",
			Sections: []string{"overview"}, Facets: []string{"business_domain"},
		},
	}, TaskContract{
		Entities: []EntityRef{{ID: "checkout"}, {ID: "billing"}},
	})
	if len(proposal.Tasks[0].InputRefs) != 0 {
		t.Fatalf("checkout task must not receive unmatched seeds: %#v", proposal.Tasks[0].InputRefs)
	}
	if len(proposal.Tasks[1].InputRefs) != 1 ||
		proposal.Tasks[1].InputRefs[0].Target != "docs/billing-overview.md" {
		t.Fatalf("billing task refs = %#v", proposal.Tasks[1].InputRefs)
	}
}

func TestPrepareInvestigationProposalBuildsScopedFallback(t *testing.T) {
	proposal, err := prepareInvestigationProposal(
		nil,
		TaskContract{
			Objective: "Trace the flow.",
			EvidenceGoals: []EvidenceGoal{{
				ID: "core_flow", Facet: "core_flow", Required: true,
				Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
			}},
		},
		[]tool.EvidenceUnit{{
			SourceKind: "code", Target: "repo/flow.go",
			Facets: []string{"core_flow"},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if proposal == nil || len(proposal.Tasks) != 2 {
		t.Fatalf("fallback proposal = %#v", proposal)
	}
	if len(proposal.Tasks[0].InputRefs) != 1 ||
		proposal.Tasks[0].InputRefs[0].Target != "repo/flow.go" {
		t.Fatalf("fallback investigator refs = %#v", proposal.Tasks[0].InputRefs)
	}
}
