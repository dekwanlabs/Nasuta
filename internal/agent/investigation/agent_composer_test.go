package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func synthesizerDefinition() agentapi.Definition {
	def := testDefinition()
	def.ID = "synthesizer"
	return def
}

func TestAgentComposerComposeSuccessful(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID:  "contract-1:synthesize",
		Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{"answer":"grounded answer"}`),
	}}
	composer := AgentComposer{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: synthesizerDefinition()},
	}
	contract := InvestigationContract{
		ID:       "contract-1",
		Question: "how is AI integrated?",
		Goals:    []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
	}
	report := InvestigationReport{
		Evidence: []EvidenceUnit{{
			ID:          "evidence-1",
			SourceKind:  "code",
			Target:      "svc.go:42",
			Content:     "the model client is called here",
			ContentHash: "abc",
		}},
		Claims: []VerifiedClaim{{
			ID:             "claim-1",
			GoalID:         "g1",
			Text:           "the service calls the model client",
			Status:         ClaimSupported,
			Confidence:     0.9,
			EvidenceRefs:   []EvidenceRef{{EvidenceID: "evidence-1"}},
			VerifierTaskID: "verify-1",
		}},
		Coverage: []GoalCoverage{{
			GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1"},
		}},
	}
	draft, err := composer.Compose(context.Background(), contract, report)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if draft.Text != "grounded answer" {
		t.Fatalf("draft text = %q", draft.Text)
	}
	if runtime.gotReq == nil {
		t.Fatal("runtime received no request")
	}
	request := *runtime.gotReq
	if request.Agent.ID != "synthesizer" || request.Agent.Version != 1 {
		t.Fatalf("agent ref = %+v", request.Agent)
	}
	if !request.Policy.EvidenceSeeded {
		t.Fatal("synthesis request is not evidence-seeded")
	}
	if len(request.Context) != 1 || request.Context[0].Source != "workflow.synthesis_objective" {
		t.Fatalf("context = %+v", request.Context)
	}
	sum := sha256.Sum256([]byte(request.Context[0].Content))
	if request.Context[0].ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatal("synthesis objective content_hash does not match content")
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(request.Input, &bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(bundle.SupportedClaims) != 1 {
		t.Fatalf("supported claims = %d, want 1", len(bundle.SupportedClaims))
	}
	claim := bundle.SupportedClaims[0]
	if claim.Claim != "the service calls the model client" ||
		claim.ProducerNodeID != "verify-1" ||
		len(claim.EvidenceIdentities) != 1 ||
		claim.EvidenceIdentities[0].SourceKind != "code" {
		t.Fatalf("supported claim = %+v", claim)
	}
	if len(bundle.EvidenceUnits) != 1 || bundle.Verification.Decision != "complete" ||
		bundle.Verification.StopReason != "required_goals_covered" {
		t.Fatalf("bundle = %+v", bundle)
	}
}

func TestAgentComposerRejectsEmptyAnswer(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID:  "contract-1:synthesize",
		Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{"answer":"   "}`),
	}}
	composer := AgentComposer{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: synthesizerDefinition()},
	}
	_, err := composer.Compose(context.Background(), InvestigationContract{
		ID: "contract-1", Question: "q", Goals: []EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}},
	}, InvestigationReport{Claims: []VerifiedClaim{{ID: "claim-1", GoalID: "g1", Text: "x", Status: ClaimSupported}}})
	if err == nil {
		t.Fatal("expected empty answer error")
	}
}

func TestMarshalVerifiedBundleInsufficientEvidence(t *testing.T) {
	contract := InvestigationContract{
		ID:       "contract-1",
		Question: "how is AI integrated?",
		Goals:    []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
	}
	report := InvestigationReport{
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalUnresolved}},
		Gaps: []EvidenceGap{{
			GoalID: "g1", Reason: "no verified claim covers this goal",
			MissingFacets: []string{"health"}, MissingSources: []string{"runtime"},
		}},
	}
	raw, err := marshalVerifiedBundle(contract, report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bundle.Verification.Decision != "unavailable" ||
		bundle.Verification.StopReason != "evidence_insufficient" ||
		len(bundle.UnresolvedGoals) != 1 ||
		bundle.UnresolvedGoals[0] != "g1" {
		t.Fatalf("bundle = %+v", bundle)
	}
	if len(bundle.Limitations) != 1 || !strings.Contains(bundle.Limitations[0], "g1") ||
		!strings.Contains(bundle.Limitations[0], "missing facets: health") ||
		!strings.Contains(bundle.Limitations[0], "missing sources: runtime") {
		t.Fatalf("limitations = %+v", bundle.Limitations)
	}
	if !strings.HasPrefix(bundle.LimitationsDetail.ArtifactID, "art_") {
		t.Fatalf("artifact id = %q", bundle.LimitationsDetail.ArtifactID)
	}
}

func TestMarshalVerifiedBundleRejectedClaim(t *testing.T) {
	contract := InvestigationContract{
		ID:       "contract-1",
		Question: "q",
		Goals:    []EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}},
	}
	report := InvestigationReport{
		Claims: []VerifiedClaim{{
			ID: "claim-1", GoalID: "g1", Text: "unsupported finding", Status: ClaimRejected,
		}},
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalUnresolved}},
	}
	raw, err := marshalVerifiedBundle(contract, report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bundle.UnsupportedClaims) != 1 || len(bundle.SupportedClaims) != 0 {
		t.Fatalf("bundle = %+v", bundle)
	}
	if bundle.UnsupportedClaims[0].Support != "unsupported" ||
		bundle.UnsupportedClaims[0].ReasonCode != unsupportedReasonCode {
		t.Fatalf("unsupported claim = %+v", bundle.UnsupportedClaims[0])
	}
}
