package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentcatalog "github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestTrimVerifiedEvidencePrioritizesClaimsAndTracksOmissions(t *testing.T) {
	claim := func(name, goal, target string, highRisk bool) verifiedClaimView {
		return verifiedClaimView{
			Claim: name, EvidenceGoalIDs: []string{goal}, Support: claimSupported,
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
			EvidenceGoalIDs: []string{"unsupported"}, Support: claimUnsupported,
		}},
		PartialEvidenceGoals:    []string{"partial"},
		UnresolvedEvidenceGoals: []string{"missing"},
		Limitations:             []string{"runtime evidence unavailable"},
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
	budget := verifiedViewTokens(viewAtVerifiedSlot(view, slots, 1, nil))

	got, err := trimVerifiedEvidence(view, budget, required, nil)
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
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.EvidenceUnits) != len(view.EvidenceUnits) ||
		full.Omissions != (omissionView{}) {
		t.Fatalf("unbounded verified view = %+v", full)
	}
}

func TestTrimVerifiedEvidencePreservesProtectedBaseline(t *testing.T) {
	baseline := verifiedEvidenceUnit("baseline")
	view := verifiedEvidenceView{
		SupportedClaims: []verifiedClaimView{{
			Claim: "derived", EvidenceGoalIDs: []string{"optional"},
			EvidenceIdentities: []agentapi.EvidenceIdentity{{
				SourceKind: "source", Target: "derived",
			}},
		}},
		EvidenceUnits: []tool.EvidenceUnit{
			baseline,
			verifiedEvidenceUnit("derived"),
		},
		Verification: verificationView{
			Decision: Partial, StopReason: StopEvidenceInsufficient,
		},
		Completeness: Partial,
	}
	protected := map[evidence.Key]struct{}{{
		SourceKind: baseline.SourceKind,
		Target:     baseline.Target,
	}: {}}
	slots := verifiedSlots(view, nil)
	budget := verifiedViewTokens(
		viewAtVerifiedSlot(view, slots, 0, protected),
	)
	got, err := trimVerifiedEvidence(view, budget, nil, protected)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SupportedClaims) != 0 ||
		len(got.EvidenceUnits) != 1 ||
		got.EvidenceUnits[0].Target != baseline.Target {
		t.Fatalf("protected baseline view = %+v", got)
	}
}

func TestTrimVerifiedEvidenceBoundsLargeSynthesisPayload(t *testing.T) {
	const maxTokens = 8192
	view := verifiedEvidenceView{
		SupportedClaims:         make([]verifiedClaimView, 0, 80),
		PartialClaims:           []verifiedClaimView{},
		UnsupportedClaims:       []unsupportedClaimView{},
		PartialEvidenceGoals:    []string{},
		UnresolvedEvidenceGoals: []string{},
		Limitations:             []string{},
		LimitationsDetail: &limitationsDetailRef{
			ArtifactID:           "art_00000000-0000-0000-0000-000000000000",
			NormalizationVersion: LimitationsNormalizationVersion,
		},
		EvidenceUnits:     make([]tool.EvidenceUnit, 0, 80),
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		Verification: verificationView{
			Decision:   Partial,
			StopReason: StopEvidenceInsufficient,
		},
		Completeness: Partial,
	}
	for index := range 80 {
		target := fmt.Sprintf("source-%03d", index)
		identity := agentapi.EvidenceIdentity{
			SourceKind: "code", Target: target, Section: "flow",
		}
		view.SupportedClaims = append(view.SupportedClaims, verifiedClaimView{
			ProducerNodeID:  fmt.Sprintf("investigate.%d", index%3),
			FindingIndex:    index,
			Claim:           fmt.Sprintf("%s %s", target, strings.Repeat("grounded detail ", 32)),
			EvidenceGoalIDs: []string{"optional"},
			Evidence: []findingEvidenceView{{
				Kind: "code", Reference: target,
				Summary:  strings.Repeat("concrete support ", 16),
				Identity: &identity,
			}},
			EvidenceIdentities: []agentapi.EvidenceIdentity{identity},
			Confidence:         0.9,
			Support:            claimSupported,
			HighRisk:           index == 0,
		})
		view.EvidenceUnits = append(view.EvidenceUnits, tool.EvidenceUnit{
			SourceKind: "code", Target: target, Sections: []string{"flow"},
			Coverage: tool.EvidenceCoverage{Complete: true},
		})
	}
	view.SupportedClaims[1].EvidenceGoalIDs = []string{"required"}

	required := map[string]struct{}{"required": {}}
	got, err := trimVerifiedEvidence(view, maxTokens, required, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := verifiedViewTokens(got); tokens > maxTokens {
		t.Fatalf("trimmed payload tokens = %d, budget = %d", tokens, maxTokens)
	}
	if got.Omissions.Claims == 0 || got.Omissions.EvidenceUnits == 0 {
		t.Fatalf("large payload was not compacted: omissions = %+v", got.Omissions)
	}
	if len(got.SupportedClaims) < 2 ||
		!got.SupportedClaims[0].HighRisk ||
		!reflect.DeepEqual(got.SupportedClaims[1].EvidenceGoalIDs, []string{"required"}) {
		t.Fatalf("priority claims were not retained: %+v", got.SupportedClaims[:min(2, len(got.SupportedClaims))])
	}

	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(
		agentapi.InvestigationVerifiedBundleSchemaRef(),
		payload,
	); err != nil {
		t.Fatalf("trimmed verified bundle failed schema validation: %v", err)
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
	schemas, _ := workflowTestCatalogs(t, 21)
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
			wantStopReason: StopEvidenceInsufficient,
			wantClaims:     1,
			wantUnresolved: []string{"documentation"},
		},
		{
			name:           "unavailable",
			reports:        []handoffView{},
			completeness:   Unavailable,
			wantDecision:   Unavailable,
			wantStopReason: StopEvidenceInsufficient,
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
			wantStopReason: StopEvidenceInsufficient,
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
					OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
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
				!reflect.DeepEqual(verified.UnresolvedEvidenceGoals, test.wantUnresolved) {
				t.Fatalf("verified evidence = %+v", verified)
			}
			if test.wantClaims > 0 {
				claim := verified.SupportedClaims[0]
				if claim.ProducerNodeID != "investigate.code" ||
					claim.FindingIndex != 0 ||
					claim.Claim != "Checkout enters PlaceOrder." ||
					!reflect.DeepEqual(claim.EvidenceGoalIDs, []string{"core_flow"}) ||
					len(claim.EvidenceRefs) != 1 ||
					claim.EvidenceRefs[0].EvidenceID == "" ||
					verified.EvidenceLookup[claim.EvidenceRefs[0].EvidenceID].Reference !=
						"code:checkout" {
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
			want:         StopEvidenceInsufficient,
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
	schemas, _ := workflowTestCatalogs(t, 23)
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
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
			Verifier:     &VerifierSpec{RequiredGoals: []string{"core_flow"}},
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
	identity := verified.EvidenceLookup[verified.SupportedClaims[0].EvidenceRefs[0].EvidenceID].Identity
	if len(verified.SupportedClaims) != 1 ||
		identity == nil || !reflect.DeepEqual(*identity, want[0]) ||
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
	schemas, _ := workflowTestCatalogs(t, 24)
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
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
			Verifier:     &VerifierSpec{RequiredGoals: []string{"core_flow"}},
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
		len(verified.PartialClaims[0].EvidenceRefs) != 1 ||
		verified.EvidenceLookup[verified.PartialClaims[0].EvidenceRefs[0].EvidenceID].Reference !=
			"code:checkout" ||
		!reflect.DeepEqual(verified.PartialEvidenceGoals, []string{"core_flow"}) ||
		len(verified.Limitations) != 3 ||
		bytes.Contains(
			[]byte(verified.Limitations[0]),
			[]byte("missing:reference"),
		) {
		t.Fatalf("verified partial support = %+v", verified)
	}
}

func TestVerifyInvestigationEvidenceClassifiesCoverageAndTrust(t *testing.T) {
	schemas, _ := workflowTestCatalogs(t, 26)
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
					OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
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
					verified.PartialEvidenceGoals,
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
	schemas, _ := workflowTestCatalogs(t, 22)
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
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
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
		traces[0].Output["stop_reason"] != StopEvidenceInsufficient {
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
		goalIDs, _ := finding["evidence_goal_ids"].([]string)
		covered = append(covered, goalIDs...)
	}
	payload, err := json.Marshal(map[string]any{
		"focus":                     focus,
		"summary":                   focus + " report",
		"findings":                  findings,
		"gaps":                      gaps,
		"covered_evidence_goals":    covered,
		"unresolved_evidence_goals": []string{},
	})
	if err != nil {
		panic(err)
	}
	return handoffView{
		ProducerNodeID: producerNodeID,
		Schema:         agentapi.InvestigationReportSchemaRef(),
		Payload:        payload,
		Completeness:   Complete,
	}
}

func verifiedFinding(claim, goalID, reference string) map[string]any {
	return map[string]any{
		"claim":             claim,
		"evidence_goal_ids": []string{goalID},
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
	schemas, _ := workflowTestCatalogs(t, 31)
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
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
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

func TestVerificationIndexBindsStableEvidenceHandle(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "repos/voice/Controller.java",
		Sections: []string{"L10-L20"}, Coverage: tool.EvidenceCoverage{Complete: true},
	}
	index := newEvidenceIndex([]tool.EvidenceUnit{unit})
	key, ok := evidence.UnitKey(unit)
	if !ok {
		t.Fatal("unit has no canonical key")
	}
	matches := index.match(findingEvidenceView{
		Kind: "code", Reference: "Controller.java", Summary: "support",
		EvidenceID: key.Handle(),
	})
	if len(matches) != 1 || matches[0].identity.Target != unit.Target ||
		matches[0].identity.Section != "L10-L20" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestSubjectCoverageUsesExplicitFindingEntityIDs(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "repos/hsds/hsds-aiot-agent/main.py",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	key, ok := evidence.UnitKey(unit)
	if !ok {
		t.Fatal("unit has no canonical key")
	}
	finding := map[string]any{
		"claim":             "The first-party agent sends device commands.",
		"entity_ids":        []string{"our_agent"},
		"evidence_goal_ids": []string{"core_flow"},
		"evidence": []map[string]any{{
			"kind": "code", "reference": unit.Target, "summary": "support",
			"evidence_id": key.Handle(),
		}},
		"confidence": 0.9,
	}
	payload, err := json.Marshal(ledgerView{
		Handoffs: []handoffView{verifiedReportHandoff(
			"investigate.code.1", "code", []map[string]any{finding}, nil,
		)},
		EvidenceUnits: []tool.EvidenceUnit{unit}, Completeness: Complete,
	})
	if err != nil {
		t.Fatal(err)
	}
	schemas, _ := workflowTestCatalogs(t, 21)
	output, err := verifyBundle(verificationRunInput{
		workflowRunID: "subject-handle-run",
		node: NodeDefinition{
			ID: "evidence.verify", Kind: NodeVerifier,
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
			Verifier: &VerifierSpec{
				RequiredGoals: []string{"core_flow"},
				SubjectRequirements: []SubjectRequirement{{
					EntityID: "our_agent", Label: "我们的agent",
					RequiredFacets:  []string{"core_flow"},
					RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
				}},
			},
		},
		inputs: []Handoff{{
			ProducerNodeID: "evidence.join", Payload: payload,
			EvidenceUnits: []tool.EvidenceUnit{unit}, Completeness: Complete,
		}},
		maxBytes: 1 << 20, schemas: schemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified verifiedEvidenceView
	if err := json.Unmarshal(output.handoff.Payload, &verified); err != nil {
		t.Fatal(err)
	}
	if len(verified.SupportedClaims) != 1 || len(verified.SubjectCoverage) != 1 ||
		!verified.SubjectCoverage[0].Complete {
		t.Fatalf("verified = %+v", verified)
	}
}

func TestVerificationIndexAcceptsCanonicalFileLineReference(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "repos/voice/Controller.java",
		Sections: []string{"L10-L20"}, Coverage: tool.EvidenceCoverage{Complete: true},
	}
	index := newEvidenceIndex([]tool.EvidenceUnit{unit})
	for _, reference := range []string{
		"repos/voice/Controller.java:L10-L20",
		"repos/voice/Controller.java#L10-L20",
		"repos/voice/Controller.java (L10-L20)",
	} {
		matches := index.match(findingEvidenceView{
			Kind: "code", Reference: reference, Summary: "support",
		})
		if len(matches) != 1 || matches[0].identity.Target != unit.Target ||
			matches[0].identity.Section != "L10-L20" {
			t.Fatalf("reference %q matches = %+v", reference, matches)
		}
	}
}
