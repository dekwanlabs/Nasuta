package featuredelivery

import "sort"

func DeriveLineage(artifacts []Artifact) Lineage {
	if len(artifacts) == 0 {
		return Lineage{}
	}
	byKind := make(map[ArtifactKind][]*Artifact, 5)
	for i := range artifacts {
		artifact := &artifacts[i]
		artifact.Stale = true
		byKind[artifact.Kind] = append(byKind[artifact.Kind], artifact)
	}
	for _, values := range byKind {
		sort.Slice(values, func(i, j int) bool {
			return values[i].Version > values[j].Version
		})
	}
	requirement := first(byKind[KindRequirement], func(artifact *Artifact) bool { return true })
	if requirement == nil {
		return Lineage{}
	}
	requirement.Stale = false
	analysis := approvedChild(byKind[KindRequirementAnalysis], requirement.ID)
	proposal := approvedChild(byKind[KindTechnicalProposal], artifactID(analysis))
	design := approvedChild(byKind[KindSystemDesign], artifactID(proposal))
	plan := approvedChild(byKind[KindImplementationPlan], artifactID(design))
	for _, artifact := range []*Artifact{analysis, proposal, design, plan} {
		if artifact != nil {
			artifact.Stale = false
		}
	}
	return Lineage{
		Requirement:         requirement,
		RequirementAnalysis: analysis,
		TechnicalProposal:   proposal,
		SystemDesign:        design,
		ImplementationPlan:  plan,
	}
}

func ExpectedParent(lineage Lineage, kind ArtifactKind) (*Artifact, error) {
	switch kind {
	case KindRequirementAnalysis:
		if lineage.Requirement == nil {
			return nil, ErrConflict
		}
		return lineage.Requirement, nil
	case KindTechnicalProposal:
		if lineage.RequirementAnalysis == nil {
			return nil, ErrConflict
		}
		return lineage.RequirementAnalysis, nil
	case KindSystemDesign:
		if lineage.TechnicalProposal == nil {
			return nil, ErrConflict
		}
		return lineage.TechnicalProposal, nil
	case KindImplementationPlan:
		if lineage.SystemDesign == nil {
			return nil, ErrConflict
		}
		return lineage.SystemDesign, nil
	default:
		return nil, ErrConflict
	}
}

func approvedChild(artifacts []*Artifact, parentID string) *Artifact {
	if parentID == "" {
		return nil
	}
	return first(artifacts, func(artifact *Artifact) bool {
		return artifact.ParentArtifactID == parentID &&
			artifact.Review != nil &&
			artifact.Review.Decision == DecisionApproved
	})
}

func first(artifacts []*Artifact, predicate func(*Artifact) bool) *Artifact {
	for _, artifact := range artifacts {
		if predicate(artifact) {
			return artifact
		}
	}
	return nil
}

func artifactID(artifact *Artifact) string {
	if artifact == nil {
		return ""
	}
	return artifact.ID
}
