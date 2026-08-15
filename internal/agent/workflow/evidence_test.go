package workflow

import (
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestMeasureEvidenceConvergenceUsesBaselineDelta(t *testing.T) {
	baseline := []tool.EvidenceUnit{convergenceEvidenceUnit("code", "checkout")}
	tests := []struct {
		name          string
		inputs        []Handoff
		wantCandidate int
		wantNew       int
		wantDuplicate int
		wantRatio     float64
	}{
		{
			name: "baseline only",
			inputs: []Handoff{{
				EvidenceUnits: []tool.EvidenceUnit{
					convergenceEvidenceUnit("code", "checkout"),
				},
			}},
		},
		{
			name: "new identity",
			inputs: []Handoff{{
				EvidenceUnits: []tool.EvidenceUnit{
					convergenceEvidenceUnit("code", "checkout"),
					convergenceEvidenceUnit("runbook", "checkout"),
				},
			}},
			wantCandidate: 1,
			wantNew:       1,
		},
		{
			name: "duplicate across handoffs",
			inputs: []Handoff{
				{
					EvidenceUnits: []tool.EvidenceUnit{
						convergenceEvidenceUnit("code", "checkout"),
						convergenceEvidenceUnit("runbook", "checkout"),
					},
				},
				{
					EvidenceUnits: []tool.EvidenceUnit{
						convergenceEvidenceUnit("runbook", "checkout"),
					},
				},
			},
			wantCandidate: 2,
			wantNew:       1,
			wantDuplicate: 1,
			wantRatio:     0.5,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := measureConvergence(test.inputs, baseline, 0.8)
			if got.CandidateCount != test.wantCandidate ||
				got.NewIdentityCount != test.wantNew ||
				got.DuplicateCount != test.wantDuplicate ||
				got.DuplicateRatio != test.wantRatio ||
				got.MaxDuplicateRatio != 0.8 {
				t.Fatalf("convergence = %+v", got)
			}
		})
	}
}

func convergenceEvidenceUnit(sourceKind, target string) tool.EvidenceUnit {
	return tool.EvidenceUnit{
		SourceKind: sourceKind,
		Target:     target,
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}
}
