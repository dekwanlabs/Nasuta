package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestEvidenceRiskGateEvaluatorAppliesRiskPolicy(t *testing.T) {
	tests := []struct {
		name            string
		verified        verifiedEvidenceView
		wantDecision    string
		wantReasonCodes []string
		wantFindingIDs  []string
	}{
		{
			name: "low-risk partial support passes",
			verified: verifiedEvidenceView{
				PartialClaims: []verifiedClaimView{{
					ProducerNodeID: "investigate.code",
					FindingIndex:   1,
				}},
			},
			wantDecision: EvidenceRiskPassDecision,
		},
		{
			name: "high-risk partial support needs clarification",
			verified: verifiedEvidenceView{
				PartialClaims: []verifiedClaimView{{
					ProducerNodeID: "investigate.runtime",
					FindingIndex:   2,
					HighRisk:       true,
				}},
			},
			wantDecision:    string(StopNeedsClarification),
			wantReasonCodes: []string{riskReasonHighRiskPartialSupport},
			wantFindingIDs:  []string{"investigate.runtime:2"},
		},
		{
			name: "evidence conflict needs clarification",
			verified: verifiedEvidenceView{
				EvidenceConflicts: []agentapi.EvidenceConflict{{}},
			},
			wantDecision:    string(StopNeedsClarification),
			wantReasonCodes: []string{riskReasonEvidenceConflict},
		},
		{
			name: "reasons are unique and findings remain complete",
			verified: verifiedEvidenceView{
				PartialClaims: []verifiedClaimView{
					{
						ProducerNodeID: "investigate.runtime",
						FindingIndex:   3,
						HighRisk:       true,
					},
					{
						ProducerNodeID: "investigate.runtime",
						FindingIndex:   5,
						HighRisk:       true,
					},
				},
				EvidenceConflicts: []agentapi.EvidenceConflict{{}},
			},
			wantDecision: string(StopNeedsClarification),
			wantReasonCodes: []string{
				riskReasonEvidenceConflict,
				riskReasonHighRiskPartialSupport,
			},
			wantFindingIDs: []string{
				"investigate.runtime:3",
				"investigate.runtime:5",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.verified)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := (RiskGateEvaluator{}).Evaluate(
				t.Context(),
				NodeRequest{
					Node: NodeDefinition{ID: "evidence.risk"},
					Inputs: []Handoff{{
						ContentHash: "verified-hash",
						Payload:     payload,
					}},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if decision.SubjectHash != "verified-hash" ||
				decision.Decision != test.wantDecision ||
				!reflect.DeepEqual(
					decision.ReasonCodes,
					test.wantReasonCodes,
				) ||
				!reflect.DeepEqual(
					decision.FindingIDs,
					test.wantFindingIDs,
				) {
				t.Fatalf("gate decision = %+v", decision)
			}
		})
	}
}

func TestEvidenceRiskGateEvaluatorRejectsInvalidInput(t *testing.T) {
	evaluator := RiskGateEvaluator{}
	for _, inputs := range [][]Handoff{
		nil,
		{{Payload: json.RawMessage(`{}`)}, {Payload: json.RawMessage(`{}`)}},
	} {
		if _, err := evaluator.Evaluate(t.Context(), NodeRequest{
			Node:   NodeDefinition{ID: "evidence.risk"},
			Inputs: inputs,
		}); err == nil {
			t.Fatalf("Evaluate inputs = %#v, want cardinality error", inputs)
		}
	}
	if _, err := evaluator.Evaluate(t.Context(), NodeRequest{
		Node: NodeDefinition{ID: "evidence.risk"},
		Inputs: []Handoff{{
			Payload: json.RawMessage(`not-json`),
		}},
	}); err == nil {
		t.Fatal("Evaluate invalid payload: expected decode error")
	}
}

func TestEvidenceRiskGateForwardsVerifiedHandoff(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 25)
	schema := agentapi.SchemaRef{
		ID: "investigation.verified_bundle", Version: 1,
	}
	units := []tool.EvidenceUnit{{
		SourceKind: "runtime",
		Target:     "checkout",
		Coverage:   tool.EvidenceCoverage{Complete: true},
		TrustTier:  2,
	}}
	payload, err := json.Marshal(verifiedEvidenceView{
		SupportedClaims:   []verifiedClaimView{},
		PartialClaims:     []verifiedClaimView{},
		UnsupportedClaims: []unsupportedClaimView{},
		PartialGoals:      []string{},
		UnresolvedGoals:   []string{},
		Limitations:       []string{},
		EvidenceUnits:     units,
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Verification: verificationView{
			Decision: Partial, StopReason: StopCapabilityUnavailable,
		},
		Completeness: Partial,
	})
	if err != nil {
		t.Fatal(err)
	}
	references := []agentapi.Reference{{
		Type: "runtime", Label: "checkout", Target: "observe://checkout",
	}}
	source, err := PrepareHandoff(Handoff{
		WorkflowRunID:  "risk-run",
		ProducerNodeID: "evidence.verify",
		Schema:         schema,
		Payload:        payload,
		References:     references,
		EvidenceUnits:  units,
		Completeness:   Partial,
	}, 1<<20, schemas)
	if err != nil {
		t.Fatal(err)
	}
	node := NodeDefinition{
		ID:           "evidence.risk",
		Kind:         NodeGate,
		InputSchema:  schema,
		OutputSchema: schema,
		Gate: &GateSpec{
			ID: EvidenceRiskGateID,
			AllowedDecisions: []string{
				EvidenceRiskPassDecision,
				string(StopNeedsClarification),
			},
			ForwardInput: true,
		},
	}
	account, err := newBudgetAccount(Budget{}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	outcome := NewOrchestrator(
		schemas,
		nil,
		map[string]GateEvaluator{
			EvidenceRiskGateID: RiskGateEvaluator{},
		},
	).executeAttempt(
		t.Context(),
		Definition{
			Budget: Budget{MaxHandoffBytes: 1 << 20},
		},
		RunRequest{RunID: "risk-run"},
		NodeRequest{
			WorkflowRunID: "risk-run",
			Node:          node,
			Inputs:        []Handoff{source},
			Attempt:       1,
		},
		account,
		nil,
	)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.gate == nil ||
		outcome.gate.Decision != EvidenceRiskPassDecision ||
		outcome.gate.SubjectHash != source.ContentHash {
		t.Fatalf("gate decision = %+v", outcome.gate)
	}
	if !reflect.DeepEqual(outcome.handoff.Payload, source.Payload) ||
		!reflect.DeepEqual(outcome.handoff.References, references) ||
		!reflect.DeepEqual(outcome.handoff.EvidenceUnits, units) ||
		len(outcome.handoff.EvidenceConflicts) != 0 ||
		outcome.handoff.Completeness != Partial {
		t.Fatalf("forwarded handoff = %+v", outcome.handoff)
	}
}
