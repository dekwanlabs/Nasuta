package delivery

import "testing"

func TestDeriveLineageMarksOldApprovedChainStale(t *testing.T) {
	approved := func(id string) *ArtifactReview {
		return &ArtifactReview{ArtifactID: id, Decision: DecisionApproved}
	}
	artifacts := []Artifact{
		{ID: "req1", Kind: KindRequirement, Version: 1},
		{ID: "analysis1", Kind: KindRequirementAnalysis, Version: 1, ParentArtifactID: "req1", Review: approved("analysis1")},
		{ID: "proposal1", Kind: KindTechnicalProposal, Version: 1, ParentArtifactID: "analysis1", Review: approved("proposal1")},
		{ID: "req2", Kind: KindRequirement, Version: 2},
		{ID: "analysis2", Kind: KindRequirementAnalysis, Version: 2, ParentArtifactID: "req2"},
	}
	lineage := DeriveLineage(artifacts)
	if lineage.Requirement == nil || lineage.Requirement.ID != "req2" {
		t.Fatalf("unexpected current requirement: %#v", lineage.Requirement)
	}
	if lineage.RequirementAnalysis != nil || lineage.TechnicalProposal != nil {
		t.Fatalf("old approved chain remained current: %#v", lineage)
	}
	for _, artifact := range artifacts {
		wantStale := artifact.ID != "req2"
		if artifact.Stale != wantStale {
			t.Fatalf("artifact %s stale=%v, want %v", artifact.ID, artifact.Stale, wantStale)
		}
	}
}
