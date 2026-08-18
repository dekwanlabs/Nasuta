package workflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestTrimVerifiedEvidencePrioritizesClaimsAndTracksOmissions(t *testing.T) {
	claim := func(name, goal, target string, highRisk bool) verifiedClaimView {
		return verifiedClaimView{
			Claim: name, GoalIDs: []string{goal}, Support: claimSupported,
			HighRisk: highRisk,
			EvidenceIdentities: []agentapi.EvidenceIdentity{{
				SourceKind: "source", Target: target,
			}},
		}
	}
	view := verifiedEvidenceView{
		SupportedClaims: []verifiedClaimView{
			claim("regular", "optional", "regular", false),
			claim("required", "required", "required", false),
			claim("high risk", "risk", "risk", true),
		},
		PartialClaims: []verifiedClaimView{
			claim("partial", "partial", "partial", false),
		},
		UnsupportedClaims: []unsupportedClaimView{{
			GoalIDs: []string{"unsupported"}, Support: claimUnsupported,
		}},
		PartialGoals:    []string{"partial"},
		UnresolvedGoals: []string{"missing"},
		Limitations:     []string{"runtime evidence unavailable"},
		EvidenceUnits: []tool.EvidenceUnit{
			verifiedEvidenceUnit("regular"),
			verifiedEvidenceUnit("required"),
			verifiedEvidenceUnit("risk"),
			verifiedEvidenceUnit("partial"),
		},
		EvidenceConflicts: []agentapi.EvidenceConflict{{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: "source", Target: "conflict",
			},
		}},
		Verification: verificationView{
			Decision: Partial, StopReason: StopCapabilityUnavailable,
		},
		Completeness: Partial,
	}
	original := view
	required := map[string]struct{}{"required": {}}
	slots := verifiedSlots(view, required)
	budget := verifiedViewTokens(viewAtVerifiedSlot(view, slots, 1))

	got, err := trimVerifiedEvidence(view, budget, required)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := verifiedViewTokens(got); tokens > budget {
		t.Fatalf("verified payload tokens = %d, budget = %d", tokens, budget)
	}
	if len(got.SupportedClaims) != 1 || got.SupportedClaims[0].Claim != "required" {
		t.Fatalf("retained claims = %+v", got.SupportedClaims)
	}
	if len(got.EvidenceUnits) != 1 || got.EvidenceUnits[0].Target != "required" {
		t.Fatalf("retained evidence = %+v", got.EvidenceUnits)
	}
	wantOmissions := omissionView{
		Claims: 4, Goals: 2, Limitations: 1,
		EvidenceUnits: 3, EvidenceConflicts: 1,
	}
	if got.Omissions != wantOmissions {
		t.Fatalf("omissions = %+v, want %+v", got.Omissions, wantOmissions)
	}
	if !reflect.DeepEqual(view, original) {
		t.Fatal("full verified evidence ledger was modified")
	}

	full, err := trimVerifiedEvidence(
		view,
		tooloutput.EstimateTokens(mustMarshalJSON(t, view)),
		required,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.EvidenceUnits) != len(view.EvidenceUnits) ||
		full.Omissions != (omissionView{}) {
		t.Fatalf("unbounded verified view = %+v", full)
	}
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func TestVerifyInvestigationEvidenceClassifiesRequiredGoalCoverage(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 21)
	tests := []struct {
		name           string
		reports        []handoffView
		evidenceUnits  []tool.EvidenceUnit
		completeness   Completeness
		wantDecision   Completeness
		wantStopReason StopReason
		wantClaims     int
		wantUnresolved []string
	}{
		{
			name: "complete",
			reports: []handoffView{verifiedReportHandoff(
				"investigate.code",
				"code",
				[]map[string]any{
					verifiedFinding("Checkout enters PlaceOrder.", "core_flow", "code:checkout"),
					verifiedFinding("The runbook documents rollback.", "documentation", "runbook:checkout"),
				},
				nil,
			)},
			evidenceUnits: []tool.EvidenceUnit{
				verifiedEvidenceUnit("code:checkout"),
				verifiedEvidenceUnit("runbook:checkout"),
			},
			completeness:   Complete,
			wantDecision:   Complete,
			wantStopReason: StopRequiredGoalsCovered,
			wantClaims:     2,
			wantUnresolved: []string{},
		},
		{
			name: "partial",
			reports: []handoffView{verifiedReportHandoff(
				"investigate.code",
				"code",
				[]map[string]any{
					verifiedFinding("Checkout enters PlaceOrder.", "core_flow", "code:checkout"),
				},
				[]string{"The documentation source was unavailable."},
			)},
			evidenceUnits: []tool.EvidenceUnit{
				verifiedEvidenceUnit("code:checkout"),
			},
			completeness:   Partial,
			wantDecision:   Partial,
			wantStopReason: StopCapabilityUnavailable,
			wantClaims:     1,
			wantUnresolved: []string{"documentation"},
		},
		{
			name:           "unavailable",
			reports:        []handoffView{},
			completeness:   Unavailable,
			wantDecision:   Unavailable,
			wantStopReason: StopCapabilityUnavailable,
			wantClaims:     0,
			wantUnresolved: []string{"core_flow", "documentation"},
		},
		{
			name: "finding without canonical evidence",
			reports: []handoffView{verifiedReportHandoff(
				"investigate.code",
				"code",
				[]map[string]any{
					verifiedFinding("Checkout enters PlaceOrder.", "core_flow", "code:checkout"),
				},
				nil,
			)},
			completeness:   Complete,
			wantDecision:   Unavailable,
			wantStopReason: StopCapabilityUnavailable,
			wantClaims:     0,
			wantUnresolved: []string{"core_flow", "documentation"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(ledgerView{
				Handoffs:          test.reports,
				UnavailableTasks:  []unavailableTaskView{},
				EvidenceUnits:     test.evidenceUnits,
				EvidenceConflicts: []agentapi.EvidenceConflict{},
				Completeness:      test.completeness,
			})
			if err != nil {
				t.Fatal(err)
			}
			output, err := verifyBundle(verificationRunInput{
				workflowRunID: "verification-run",
				node: NodeDefinition{
					ID: "evidence.verify", Kind: NodeVerifier,
					OutputSchema: agentapi.SchemaRef{
						ID: "investigation.verified_bundle", Version: 1,
					},
					Verifier: &VerifierSpec{
						RequiredGoals: []string{"core_flow", "documentation"},
					},
				},
				inputs: []Handoff{{
					ProducerNodeID: "evidence.join",
					Payload:        payload,
					EvidenceUnits:  test.evidenceUnits,
					Completeness:   test.completeness,
				}},
				maxBytes: 1 << 20,
				schemas:  schemas,
			})
			if err != nil {
				t.Fatal(err)
			}
			var verified verifiedEvidenceView
			if err := json.Unmarshal(output.handoff.Payload, &verified); err != nil {
				t.Fatal(err)
			}
			if verified.Verification.Decision != test.wantDecision ||
				verified.Verification.StopReason != test.wantStopReason ||
				verified.Completeness != test.wantDecision ||
				len(verified.SupportedClaims) != test.wantClaims ||
				!reflect.DeepEqual(verified.UnresolvedGoals, test.wantUnresolved) {
				t.Fatalf("verified evidence = %+v", verified)
			}
			if test.wantClaims > 0 {
				claim := verified.SupportedClaims[0]
				if claim.ProducerNodeID != "investigate.code" ||
					claim.FindingIndex != 0 ||
					claim.Claim != "Checkout enters PlaceOrder." ||
					!reflect.DeepEqual(claim.GoalIDs, []string{"core_flow"}) ||
					len(claim.Evidence) != 1 ||
					claim.Evidence[0].Reference != "code:checkout" ||
					!reflect.DeepEqual(
						claim.EvidenceIdentities,
						[]agentapi.EvidenceIdentity{{
							SourceKind: "source",
							Target:     "code:checkout",
						}},
					) {
					t.Fatalf("verified claim support = %+v", claim)
				}
			}
		})
	}
}

func TestStopReasonForVerificationUsesConvergenceFacts(t *testing.T) {
	tests := []struct {
		name         string
		completeness Completeness
		ledger       ledgerView
		want         StopReason
	}{
		{
			name:         "required goals covered",
			completeness: Complete,
			ledger: ledgerView{
				UnavailableTasks: []unavailableTaskView{{
					StopReason: StopNoAffordableTask,
				}},
			},
			want: StopRequiredGoalsCovered,
		},
		{
			name:         "missing convergence snapshot",
			completeness: Partial,
			want:         StopCapabilityUnavailable,
		},
		{
			name:         "no new evidence",
			completeness: Partial,
			ledger: ledgerView{
				Convergence: &convergenceView{
					CandidateCount:   0,
					NewIdentityCount: 0,
				},
			},
			want: StopNoNewEvidence,
		},
		{
			name:         "duplicate evidence limit",
			completeness: Partial,
			ledger: ledgerView{
				Convergence: &convergenceView{
					CandidateCount:    4,
					NewIdentityCount:  1,
					DuplicateCount:    3,
					DuplicateRatio:    0.75,
					MaxDuplicateRatio: 0.5,
				},
			},
			want: StopDuplicateEvidence,
		},
		{
			name:         "unaffordable unavailable task",
			completeness: Unavailable,
			ledger: ledgerView{
				UnavailableTasks: []unavailableTaskView{{
					ProducerNodeID: "investigate.runtime",
					StopReason:     StopNoAffordableTask,
				}},
				Convergence: &convergenceView{},
			},
			want: StopNoAffordableTask,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stopForVerification(test.completeness, test.ledger); got != test.want {
				t.Fatalf("stop reason = %q, want %q", got, test.want)
			}
		})
	}
}

func TestVerifyInvestigationEvidenceBindsExplicitCanonicalIdentity(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 23)
	units := []tool.EvidenceUnit{{
		SourceKind: "code",
		Target:     "Checkout.PlaceOrder",
		Sections:   []string{"handler", "validation"},
		Version:    "commit-123",
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}}
	finding := verifiedFinding(
		"PlaceOrder validates the request.",
		"core_flow",
		"display:checkout",
	)
	finding["evidence"] = []map[string]any{{
		"kind":      "source",
		"reference": "display:checkout",
		"summary":   "Concrete support.",
		"identity": map[string]any{
			"source_kind": "code",
			"target":      "Checkout.PlaceOrder",
			"section":     "validation",
			"version":     "commit-123",
		},
	}}
	mismatched := verifiedFinding(
		"Wrong version must not be accepted.",
		"core_flow",
		"Checkout.PlaceOrder",
	)
	mismatched["evidence"] = []map[string]any{{
		"kind":      "code",
		"reference": "Checkout.PlaceOrder",
		"summary":   "Mismatched support.",
		"identity": map[string]any{
			"source_kind": "code",
			"target":      "Checkout.PlaceOrder",
			"section":     "validation",
			"version":     "commit-999",
		},
	}}
	payload, err := json.Marshal(ledgerView{
		Handoffs: []handoffView{verifiedReportHandoff(
			"investigate.code",
			"code",
			[]map[string]any{finding, mismatched},
			nil,
		)},
		UnavailableTasks:  []unavailableTaskView{},
		EvidenceUnits:     units,
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Completeness:      Complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := verifyBundle(verificationRunInput{
		workflowRunID: "verification-run",
		node: NodeDefinition{
			ID: "evidence.verify", Kind: NodeVerifier,
			OutputSchema: agentapi.SchemaRef{
				ID: "investigation.verified_bundle", Version: 1,
			},
			Verifier: &VerifierSpec{RequiredGoals: []string{"core_flow"}},
		},
		inputs: []Handoff{{
			ProducerNodeID: "evidence.join",
			Payload:        payload,
			EvidenceUnits:  units,
			Completeness:   Complete,
		}},
		maxBytes: 1 << 20,
		schemas:  schemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified verifiedEvidenceView
	if err := json.Unmarshal(output.handoff.Payload, &verified); err != nil {
		t.Fatal(err)
	}
	want := []agentapi.EvidenceIdentity{{
		SourceKind: "code",
		Target:     "Checkout.PlaceOrder",
		Section:    "validation",
		Version:    "commit-123",
	}}
	if len(verified.SupportedClaims) != 1 ||
		!reflect.DeepEqual(
			verified.SupportedClaims[0].EvidenceIdentities,
			want,
		) ||
		len(verified.UnsupportedClaims) != 1 ||
		verified.UnsupportedClaims[0].ReasonCode !=
			"canonical_evidence_unbound" ||
		len(verified.Limitations) != 1 ||
		bytes.Contains(
			[]byte(verified.Limitations[0]),
			[]byte("Wrong version"),
		) {
		t.Fatalf("verified identity = %+v", verified.SupportedClaims)
	}
	unsupported, err := json.Marshal(verified.UnsupportedClaims)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(unsupported, []byte("Wrong version")) {
		t.Fatalf("unsupported metadata leaked claim text: %s", unsupported)
	}
}

func TestVerifyInvestigationEvidenceKeepsOnlyBoundReferences(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 24)
	units := []tool.EvidenceUnit{verifiedEvidenceUnit("code:checkout")}
	finding := verifiedFinding(
		"Checkout enters PlaceOrder.",
		"core_flow",
		"code:checkout",
	)
	finding["evidence"] = append(
		finding["evidence"].([]map[string]any),
		map[string]any{
			"kind":      "source",
			"reference": "missing:reference",
			"summary":   "Unbound support.",
		},
	)
	payload, err := json.Marshal(ledgerView{
		Handoffs: []handoffView{verifiedReportHandoff(
			"investigate.code",
			"code",
			[]map[string]any{finding},
			nil,
		)},
		UnavailableTasks:  []unavailableTaskView{},
		EvidenceUnits:     units,
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Completeness:      Complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := verifyBundle(verificationRunInput{
		workflowRunID: "verification-run",
		node: NodeDefinition{
			ID: "evidence.verify", Kind: NodeVerifier,
			OutputSchema: agentapi.SchemaRef{
				ID: "investigation.verified_bundle", Version: 1,
			},
			Verifier: &VerifierSpec{RequiredGoals: []string{"core_flow"}},
		},
		inputs: []Handoff{{
			ProducerNodeID: "evidence.join",
			Payload:        payload,
			EvidenceUnits:  units,
			Completeness:   Complete,
		}},
		maxBytes: 1 << 20,
		schemas:  schemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified verifiedEvidenceView
	if err := json.Unmarshal(output.handoff.Payload, &verified); err != nil {
		t.Fatal(err)
	}
	if len(verified.SupportedClaims) != 0 ||
		len(verified.PartialClaims) != 1 ||
		verified.PartialClaims[0].Support != claimPartial ||
		len(verified.PartialClaims[0].Evidence) != 1 ||
		verified.PartialClaims[0].Evidence[0].Reference != "code:checkout" ||
		!reflect.DeepEqual(verified.PartialGoals, []string{"core_flow"}) ||
		len(verified.Limitations) != 3 ||
		bytes.Contains(
			[]byte(verified.Limitations[0]),
			[]byte("missing:reference"),
		) {
		t.Fatalf("verified partial support = %+v", verified)
	}
}

func TestVerifyInvestigationEvidenceClassifiesCoverageAndTrust(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 26)
	tests := []struct {
		name          string
		coverage      tool.EvidenceCoverage
		trustTier     int
		highRisk      bool
		wantSupport   claimSupport
		wantDecision  Completeness
		wantHighRisk  bool
		wantPartial   []string
		wantSupported int
	}{
		{
			name:         "incomplete coverage is partial",
			coverage:     tool.EvidenceCoverage{Partial: true, Included: 1},
			trustTier:    2,
			wantSupport:  claimPartial,
			wantDecision: Partial,
			wantPartial:  []string{"core_flow"},
		},
		{
			name:          "high-risk trust below floor is partial",
			coverage:      tool.EvidenceCoverage{Complete: true},
			trustTier:     0,
			highRisk:      true,
			wantSupport:   claimPartial,
			wantDecision:  Partial,
			wantHighRisk:  true,
			wantPartial:   []string{"core_flow"},
			wantSupported: 0,
		},
		{
			name:          "high-risk trust at floor is supported",
			coverage:      tool.EvidenceCoverage{Complete: true},
			trustTier:     1,
			highRisk:      true,
			wantSupport:   claimSupported,
			wantDecision:  Complete,
			wantHighRisk:  true,
			wantPartial:   []string{},
			wantSupported: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unit := verifiedEvidenceUnit("code:checkout")
			unit.Coverage = test.coverage
			unit.TrustTier = test.trustTier
			payload, err := json.Marshal(ledgerView{
				Handoffs: []handoffView{verifiedReportHandoff(
					"investigate.code",
					"code",
					[]map[string]any{
						verifiedFinding(
							"Checkout enters PlaceOrder.",
							"core_flow",
							"code:checkout",
						),
					},
					nil,
				)},
				UnavailableTasks:  []unavailableTaskView{},
				EvidenceUnits:     []tool.EvidenceUnit{unit},
				EvidenceConflicts: []agentapi.EvidenceConflict{},
				Completeness:      Complete,
			})
			if err != nil {
				t.Fatal(err)
			}
			highRiskGoals := []string(nil)
			if test.highRisk {
				highRiskGoals = []string{"core_flow"}
			}
			output, err := verifyBundle(verificationRunInput{
				workflowRunID: "verification-run",
				node: NodeDefinition{
					ID: "evidence.verify", Kind: NodeVerifier,
					OutputSchema: agentapi.SchemaRef{
						ID: "investigation.verified_bundle", Version: 1,
					},
					Verifier: &VerifierSpec{
						RequiredGoals:            []string{"core_flow"},
						HighRiskGoals:            highRiskGoals,
						HighRiskMinimumTrustTier: 1,
					},
				},
				inputs: []Handoff{{
					ProducerNodeID: "evidence.join",
					Payload:        payload,
					EvidenceUnits:  []tool.EvidenceUnit{unit},
					Completeness:   Complete,
				}},
				maxBytes: 1 << 20,
				schemas:  schemas,
			})
			if err != nil {
				t.Fatal(err)
			}
			var verified verifiedEvidenceView
			if err := json.Unmarshal(
				output.handoff.Payload,
				&verified,
			); err != nil {
				t.Fatal(err)
			}
			if verified.Completeness != test.wantDecision ||
				!reflect.DeepEqual(
					verified.PartialGoals,
					test.wantPartial,
				) ||
				len(verified.SupportedClaims) != test.wantSupported {
				t.Fatalf("verified evidence = %+v", verified)
			}
			var claim verifiedClaimView
			switch test.wantSupport {
			case claimSupported:
				claim = verified.SupportedClaims[0]
			case claimPartial:
				if len(verified.PartialClaims) != 1 {
					t.Fatalf("partial claims = %+v", verified.PartialClaims)
				}
				claim = verified.PartialClaims[0]
			default:
				t.Fatalf("unsupported expected support %q", test.wantSupport)
			}
			if claim.Support != test.wantSupport ||
				claim.HighRisk != test.wantHighRisk {
				t.Fatalf("verified claim = %+v", claim)
			}
		})
	}
}

func TestVerifierTraceOmitsClaimPayload(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 22)
	units := []tool.EvidenceUnit{
		verifiedEvidenceUnit("private-evidence-reference"),
		{
			SourceKind: "source",
			Target:     "private-partial-evidence-reference",
			Coverage:   tool.EvidenceCoverage{Partial: true, Included: 1},
		},
	}
	payload, err := json.Marshal(ledgerView{
		Handoffs: []handoffView{verifiedReportHandoff(
			"investigate.code",
			"code",
			[]map[string]any{
				verifiedFinding("private-claim-text", "core_flow", "private-evidence-reference"),
				verifiedFinding(
					"private-partial-claim-text",
					"documentation",
					"private-partial-evidence-reference",
				),
				verifiedFinding(
					"private-unsupported-claim-text",
					"live_state",
					"private-missing-evidence-reference",
				),
			},
			nil,
		)},
		UnavailableTasks:  []unavailableTaskView{},
		EvidenceUnits:     units,
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Completeness:      Complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	scope := runtrace.Begin(ctx)
	ctx = runtrace.WithScope(ctx, scope)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: "verification-run", WorkflowRunID: "verification-run",
	})
	orchestrator := NewOrchestrator(schemas, nil, nil)
	_, err = orchestrator.verifyEvidence(
		ctx,
		"verification-run",
		NodeDefinition{
			ID: "evidence.verify", Kind: NodeVerifier,
			OutputSchema: agentapi.SchemaRef{
				ID: "investigation.verified_bundle", Version: 1,
			},
			Verifier: &VerifierSpec{RequiredGoals: []string{
				"core_flow",
				"documentation",
				"live_state",
			}},
		},
		[]Handoff{{
			ProducerNodeID: "evidence.join",
			Payload:        payload,
			EvidenceUnits:  units,
			Completeness:   Complete,
		}},
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	scope.Close()
	if len(traces) != 1 || traces[0].Node != "verification.completed" ||
		traces[0].Output["supported_claim_count"] != 1 ||
		traces[0].Output["partial_claim_count"] != 1 ||
		traces[0].Output["unsupported_claim_count"] != 1 ||
		traces[0].Output["stop_reason"] != StopCapabilityUnavailable {
		t.Fatalf("verification traces = %#v", traces)
	}
	encoded, err := json.Marshal(traces[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private-claim-text")) ||
		bytes.Contains(encoded, []byte("private-partial-claim-text")) ||
		bytes.Contains(encoded, []byte("private-unsupported-claim-text")) ||
		bytes.Contains(encoded, []byte("private-evidence-reference")) ||
		bytes.Contains(encoded, []byte("private-partial-evidence-reference")) ||
		bytes.Contains(encoded, []byte("private-missing-evidence-reference")) {
		t.Fatalf("verification trace leaked claim payload: %s", encoded)
	}
}

func verifiedReportHandoff(
	producerNodeID string,
	focus string,
	findings []map[string]any,
	gaps []string,
) handoffView {
	covered := make([]string, 0, len(findings))
	for _, finding := range findings {
		goalIDs, _ := finding["goal_ids"].([]string)
		covered = append(covered, goalIDs...)
	}
	payload, err := json.Marshal(map[string]any{
		"focus":            focus,
		"summary":          focus + " report",
		"findings":         findings,
		"gaps":             gaps,
		"covered_goals":    covered,
		"unresolved_goals": []string{},
	})
	if err != nil {
		panic(err)
	}
	return handoffView{
		ProducerNodeID: producerNodeID,
		Schema: agentapi.SchemaRef{
			ID: "investigation.report", Version: 1,
		},
		Payload:      payload,
		Completeness: Complete,
	}
}

func verifiedFinding(claim, goalID, reference string) map[string]any {
	return map[string]any{
		"claim":    claim,
		"goal_ids": []string{goalID},
		"evidence": []map[string]any{{
			"kind": "source", "reference": reference, "summary": "Concrete support.",
		}},
		"confidence": 0.9,
	}
}

func verifiedEvidenceUnit(reference string) tool.EvidenceUnit {
	return tool.EvidenceUnit{
		SourceKind: "source",
		Target:     reference,
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}
}

func TestVerifyBundleRequiresFacetCoveragePerComparisonSubject(t *testing.T) {
	tests := []struct {
		name             string
		targets          []string
		wantCompleteness Completeness
		wantStopReason   StopReason
		wantComplete     []bool
		wantMissing      [][]string
	}{
		{
			name:             "all subjects covered",
			targets:          []string{"our-agent/handler.go", "google/handler.go"},
			wantCompleteness: Complete,
			wantStopReason:   StopRequiredGoalsCovered,
			wantComplete:     []bool{true, true},
			wantMissing:      [][]string{{}, {}},
		},
		{
			name:             "second subject missing",
			targets:          []string{"our-agent/handler.go"},
			wantCompleteness: Partial,
			wantStopReason:   StopEvidenceInsufficient,
			wantComplete:     []bool{true, false},
			wantMissing:      [][]string{{}, {"core_flow"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verified := verifySubjectBundle(t, "investigate.code.1", test.targets, []agentapi.EvidenceSource{
				agentapi.EvidenceSourceInternal,
			})
			if verified.Completeness != test.wantCompleteness ||
				verified.Verification.StopReason != test.wantStopReason {
				t.Fatalf("verification = %+v", verified.Verification)
			}
			if len(verified.SubjectCoverage) != len(test.wantComplete) {
				t.Fatalf("subject coverage = %+v", verified.SubjectCoverage)
			}
			for index, subject := range verified.SubjectCoverage {
				if subject.Complete != test.wantComplete[index] ||
					!reflect.DeepEqual(subject.MissingFacets, test.wantMissing[index]) {
					t.Fatalf("subject coverage[%d] = %+v", index, subject)
				}
			}
		})
	}
}

func TestVerifyBundleDoesNotLetWebEvidenceSatisfyRequiredInternalSource(t *testing.T) {
	verified := verifySubjectBundle(
		t,
		"investigate.web.1",
		[]string{"our-agent/handler.go", "google/handler.go"},
		[]agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
	)
	if verified.Completeness != Partial ||
		verified.Verification.StopReason != StopEvidenceInsufficient {
		t.Fatalf("verification = %+v", verified.Verification)
	}
	for _, subject := range verified.SubjectCoverage {
		if subject.Complete ||
			!reflect.DeepEqual(subject.CoveredFacets, []string{"core_flow"}) ||
			!reflect.DeepEqual(subject.Sources, []string{"web"}) {
			t.Fatalf("subject coverage = %+v", subject)
		}
	}
}

func verifySubjectBundle(
	t *testing.T,
	producerNodeID string,
	targets []string,
	requiredSources []agentapi.EvidenceSource,
) verifiedEvidenceView {
	t.Helper()
	schemas, _ := investigationCatalogs(t, 31)
	units := make([]tool.EvidenceUnit, 0, len(targets))
	findings := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		sourceKind := "code"
		if strings.Contains(producerNodeID, ".web") {
			sourceKind = "web"
		}
		unit := tool.EvidenceUnit{
			SourceKind: sourceKind,
			Target:     target,
			Sections:   []string{"core_flow"},
			Coverage:   tool.EvidenceCoverage{Complete: true},
		}
		units = append(units, unit)
		finding := verifiedFinding(target+" handles device control.", "core_flow", target)
		finding["evidence"] = []map[string]any{{
			"kind": sourceKind, "reference": target, "summary": "Concrete support.",
			"identity": map[string]any{
				"source_kind": sourceKind,
				"target":      target,
				"section":     "core_flow",
			},
		}}
		findings = append(findings, finding)
	}
	payload, err := json.Marshal(ledgerView{
		Handoffs: []handoffView{verifiedReportHandoff(
			producerNodeID,
			"comparison",
			findings,
			nil,
		)},
		UnavailableTasks:  []unavailableTaskView{},
		EvidenceUnits:     units,
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Completeness:      Complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := verifyBundle(verificationRunInput{
		workflowRunID: "subject-verification-run",
		node: NodeDefinition{
			ID: "evidence.verify", Kind: NodeVerifier,
			OutputSchema: agentapi.SchemaRef{
				ID: "investigation.verified_bundle", Version: 2,
			},
			Verifier: &VerifierSpec{
				RequiredGoals: []string{"core_flow"},
				SubjectRequirements: []SubjectRequirement{
					{
						EntityID: "our_agent", Label: "Our Agent",
						RequiredFacets:  []string{"core_flow"},
						RequiredSources: append([]agentapi.EvidenceSource(nil), requiredSources...),
					},
					{
						EntityID: "google", Label: "Google",
						RequiredFacets:  []string{"core_flow"},
						RequiredSources: append([]agentapi.EvidenceSource(nil), requiredSources...),
					},
				},
			},
		},
		inputs: []Handoff{{
			ProducerNodeID: "evidence.join",
			Payload:        payload,
			EvidenceUnits:  units,
			Completeness:   Complete,
		}},
		maxBytes: 1 << 20,
		schemas:  schemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified verifiedEvidenceView
	if err := json.Unmarshal(output.handoff.Payload, &verified); err != nil {
		t.Fatal(err)
	}
	return verified
}
