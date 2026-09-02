package delegation

import (
	"reflect"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestDelegationAdoptionContractIncludesOnlyAdoptableReports(t *testing.T) {
	contract := delegationAdoptionContract(agentapi.DelegationBatchResult{
		DelegationID: "del-1",
		Results: []agentapi.DelegationReport{
			{ReportID: "report-complete", Status: agentapi.DelegationCompleted},
			{ReportID: "report-rejected", Status: agentapi.DelegationRejected},
			{Status: agentapi.DelegationCompleted},
			{ReportID: "report-complete", Status: agentapi.DelegationCompleted},
			{ReportID: "report-partial", Status: agentapi.DelegationPartial},
		},
		Verification: &agentapi.DelegationVerification{
			VerificationID: "verification-1",
			Status:         agentapi.DelegationCompleted,
		},
	})
	if len(contract.Delegations) != 1 {
		t.Fatalf("contract = %#v", contract)
	}
	want := []string{"report-complete", "report-partial"}
	if got := contract.Delegations[0].ReportIDs; !reflect.DeepEqual(got, want) {
		t.Fatalf("report IDs = %#v, want %#v", got, want)
	}
}

func TestDelegationWarningsProtectClaimsWithoutEvidenceBodies(t *testing.T) {
	cases := []struct {
		name     string
		result   agentapi.DelegationBatchResult
		wantWarn bool
	}{
		{
			name: "body coverage is incomplete",
			result: agentapi.DelegationBatchResult{Validation: agentapi.DelegationValidation{
				CitationCoverage: 1, EvidenceBodyCoverage: 0.5,
			}},
			wantWarn: true,
		},
		{
			name: "verification unavailable",
			result: agentapi.DelegationBatchResult{Validation: agentapi.DelegationValidation{
				CitationCoverage: 1, EvidenceBodyCoverage: 1, RequiresVerification: true,
			}},
			wantWarn: true,
		},
		{
			name: "unresolved verdict",
			result: agentapi.DelegationBatchResult{
				Validation: agentapi.DelegationValidation{
					CitationCoverage: 1, EvidenceBodyCoverage: 1, RequiresVerification: true,
				},
				Verification: &agentapi.DelegationVerification{
					Status: agentapi.DelegationCompleted,
					Verdicts: []agentapi.DelegationVerificationVerdict{{
						Decision: "unresolved",
					}},
				},
			},
			wantWarn: true,
		},
		{
			name: "verified result",
			result: agentapi.DelegationBatchResult{
				Validation: agentapi.DelegationValidation{
					CitationCoverage: 1, EvidenceBodyCoverage: 1, RequiresVerification: true,
				},
				Verification: &agentapi.DelegationVerification{
					Status: agentapi.DelegationCompleted,
					Verdicts: []agentapi.DelegationVerificationVerdict{{
						Decision: "supported",
					}},
				},
			},
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := delegationWarnings(tc.result)
			if (len(warnings) > 0) != tc.wantWarn {
				t.Fatalf("warnings = %v, want warning=%t", warnings, tc.wantWarn)
			}
			if tc.wantWarn && warnings[0] != verificationUnavailableWarning &&
				warnings[0] != verificationUnresolvedWarning {
				t.Fatalf("unexpected warning = %q", warnings[0])
			}
		})
	}
}

func TestProjectVerificationWithoutVerdictsProjectsUnresolvedWarning(t *testing.T) {
	task := preparedVerification{request: agentapi.DelegationVerificationRequest{
		Claims: []agentapi.DelegationVerificationClaim{
			{ID: "claim-1", Statement: "first claim"},
			{ID: "claim-2", Statement: "second claim"},
		},
	}}
	verification := projectVerification(agentapi.RunResult{
		RunID:  "verifier-run",
		Status: agentapi.RunSucceeded,
		Output: []byte(`{"summary":"no claim-level decision","verdicts":[],"uncertainties":[]}`),
	}, task)

	if verification.Status != agentapi.DelegationCompleted {
		t.Fatalf("status = %q, want completed", verification.Status)
	}
	if len(verification.Verdicts) != 2 {
		t.Fatalf("verdicts = %#v, want one unresolved verdict per claim", verification.Verdicts)
	}
	for index, verdict := range verification.Verdicts {
		if verdict.Decision != "unresolved" {
			t.Fatalf("verdict %d decision = %q, want unresolved", index, verdict.Decision)
		}
	}
	warnings := delegationWarnings(agentapi.DelegationBatchResult{
		Validation: agentapi.DelegationValidation{
			CitationCoverage: 1, EvidenceBodyCoverage: 1, RequiresVerification: true,
		},
		Verification: &verification,
	})
	if !reflect.DeepEqual(warnings, []string{verificationUnresolvedWarning}) {
		t.Fatalf("warnings = %#v, want unresolved warning", warnings)
	}
}

func TestProjectVerificationPartialVerdictCoverageProjectsMissingClaimsUnresolved(t *testing.T) {
	task := preparedVerification{request: agentapi.DelegationVerificationRequest{
		Claims: []agentapi.DelegationVerificationClaim{
			{ID: "claim-1", Statement: "first claim"},
			{ID: "claim-2", Statement: "second claim"},
			{ID: "claim-3", Statement: "third claim"},
		},
	}}
	verification := projectVerification(agentapi.RunResult{
		RunID:  "verifier-run",
		Status: agentapi.RunSucceeded,
		Output: []byte(`{"summary":"partial decision","verdicts":[{"claim_ids":["claim-1","claim-3"],"decision":"supported","rationale":"evidence supports both"}],"uncertainties":[]}`),
	}, task)

	if len(verification.Verdicts) != 2 {
		t.Fatalf("verdicts = %#v, want grouped supported plus missing unresolved", verification.Verdicts)
	}
	if verification.Verdicts[0].Decision != "supported" || len(verification.Verdicts[0].ClaimIDs) != 2 {
		t.Fatalf("supported verdict = %#v", verification.Verdicts[0])
	}
	if verification.Verdicts[1].Decision != "unresolved" || !reflect.DeepEqual(verification.Verdicts[1].ClaimIDs, []string{"claim-2"}) {
		t.Fatalf("missing verdict = %#v", verification.Verdicts[1])
	}
	if len(verification.Uncertainties) != 1 {
		t.Fatalf("uncertainties = %#v", verification.Uncertainties)
	}
}

func TestDelegationAdoptionContractIncludesClaimAndCanonicalEdgeEvidence(t *testing.T) {
	result := agentapi.DelegationBatchResult{
		DelegationID: "del-1",
		Results: []agentapi.DelegationReport{{
			ReportID: "report-1", Status: agentapi.DelegationCompleted,
			Findings: []agentapi.DelegationFinding{{ID: "f-1", Statement: "API calls worker", Citations: []string{"ev-1"}}},
			Flow: &agentapi.FlowIR{
				Subject: "order", Status: "complete", Confidence: "high",
				Nodes: []agentapi.FlowNode{
					{ID: "api", Label: "Order API", Kind: "service"},
					{ID: "worker", Label: "Order Worker", Kind: "worker"},
				},
				Edges: []agentapi.FlowEdge{{
					From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync",
					EvidenceRefs: []string{"ev-1"}, EvidenceState: "verified",
				}},
			},
		}},
		Validation: agentapi.DelegationValidation{RequiresVerification: true},
		Verification: &agentapi.DelegationVerification{
			Status: agentapi.DelegationCompleted,
			Verdicts: []agentapi.DelegationVerificationVerdict{{
				ClaimIDs: []string{"report-1/f-1"}, Decision: "supported",
			}},
		},
	}
	contract := delegationAdoptionContract(result)
	if contract.Evidence == nil || len(contract.Evidence.Claims) != 1 || len(contract.Evidence.Edges) != 1 {
		t.Fatalf("evidence contract = %#v", contract.Evidence)
	}
	if got := contract.Evidence.Claims[0]; got.ClaimID != "report-1/f-1" || got.Decision != "supported" {
		t.Fatalf("claim = %#v", got)
	}
	edge := contract.Evidence.Edges[0]
	if edge.From == "api" || edge.To == "worker" || edge.EvidenceState != "verified" {
		t.Fatalf("edge was not canonicalized conservatively: %#v", edge)
	}
}

func TestDelegationAdoptionContractRequiresCitationForSupportedClaim(t *testing.T) {
	contract := delegationAdoptionContract(agentapi.DelegationBatchResult{
		DelegationID: "del-1",
		Results: []agentapi.DelegationReport{{
			ReportID: "report-1", Status: agentapi.DelegationCompleted,
			Findings: []agentapi.DelegationFinding{{ID: "f-1", Statement: "uncited claim"}},
		}},
		Validation: agentapi.DelegationValidation{RequiresVerification: false},
	})
	if contract.Evidence == nil || len(contract.Evidence.Claims) != 1 || contract.Evidence.Claims[0].Decision != "unresolved" {
		t.Fatalf("uncited claim was authorized as supported: %#v", contract.Evidence)
	}
}

func TestDelegationAdoptionContractProjectsUnavailableVerificationAsUnresolved(t *testing.T) {
	contract := delegationAdoptionContract(agentapi.DelegationBatchResult{
		DelegationID: "del-1",
		Results: []agentapi.DelegationReport{{
			ReportID: "report-1", Status: agentapi.DelegationCompleted,
			Findings: []agentapi.DelegationFinding{{ID: "f-1", Statement: "Unverified claim"}},
		}},
		Validation: agentapi.DelegationValidation{RequiresVerification: true},
	})
	if contract.Evidence == nil || len(contract.Evidence.Claims) != 1 || contract.Evidence.Claims[0].Decision != "unresolved" {
		t.Fatalf("evidence contract = %#v", contract.Evidence)
	}
}
