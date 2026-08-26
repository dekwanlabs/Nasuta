package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
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
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "how is AI integrated?",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
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
		len(claim.Evidence) != 1 ||
		claim.Evidence[0].EvidenceID != "evidence-1" {
		t.Fatalf("supported claim = %+v", claim)
	}
	if bundle.EvidenceLookup["evidence-1"].Identity == nil ||
		bundle.EvidenceLookup["evidence-1"].Identity.SourceKind != "code" {
		t.Fatalf("evidence lookup = %+v", bundle.EvidenceLookup)
	}
	if len(bundle.EvidenceLookup) != 1 || bundle.Verification.Decision != "complete" ||
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
		Version: InvestigationContractVersion,
		ID:      "contract-1", Question: "q", EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}},
	}, InvestigationReport{Claims: []VerifiedClaim{{ID: "claim-1", GoalID: "g1", Text: "x", Status: ClaimSupported}}})
	if err == nil {
		t.Fatal("expected empty answer error")
	}
}

func TestMarshalVerifiedBundleInsufficientEvidence(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "how is AI integrated?",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
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
		len(bundle.UnresolvedEvidenceGoals) != 1 ||
		bundle.UnresolvedEvidenceGoals[0] != "g1" {
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
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "q",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}},
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

func TestMarshalVerifiedBundleDeduplicatesEvidenceAcrossClaims(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "why is the path slow?",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindCoreFlow, Required: true}},
	}
	fullContent := strings.Repeat("the request traverses a very long downstream path;", 20)
	report := InvestigationReport{
		Evidence: []EvidenceUnit{{
			ID:          "evidence-1",
			SourceKind:  "code",
			Target:      "svc.go:42",
			Content:     fullContent,
			ContentHash: "hash-1",
			TrustTier:   2,
		}},
		Claims: []VerifiedClaim{
			{
				ID: "claim-1", GoalID: "g1", Text: "claim one",
				Status: ClaimSupported, EvidenceRefs: []EvidenceRef{{EvidenceID: "evidence-1"}},
			},
			{
				ID: "claim-2", GoalID: "g1", Text: "claim two",
				Status: ClaimSupported, EvidenceRefs: []EvidenceRef{{EvidenceID: "evidence-1"}},
			},
			{
				ID: "claim-3", GoalID: "g1", Text: "claim three",
				Status: ClaimSupported, EvidenceRefs: []EvidenceRef{{EvidenceID: "evidence-1"}},
			},
		},
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1", "claim-2", "claim-3"}}},
	}

	raw, err := marshalVerifiedBundleWithBudget(contract, report, EvidenceContextBudget{
		MaxSummaryTokens: 40,
		MaxContextTokens: 120,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Count(string(raw), fullContent) != 0 {
		t.Fatal("full evidence content was repeated in the verified bundle")
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bundle.EvidenceLookup) != 1 {
		t.Fatalf("evidence lookup size = %d, want 1", len(bundle.EvidenceLookup))
	}
	if len(bundle.SupportedClaims) != 3 {
		t.Fatalf("supported claims = %d, want 3", len(bundle.SupportedClaims))
	}
	for _, claim := range bundle.SupportedClaims {
		if len(claim.Evidence) != 1 || claim.Evidence[0].EvidenceID != "evidence-1" {
			t.Fatalf("claim evidence = %+v", claim.Evidence)
		}
	}
	if strings.Count(string(raw), bundle.EvidenceLookup["evidence-1"].Summary) != 1 {
		t.Fatalf("evidence summary was repeated in bundle: %q", bundle.EvidenceLookup["evidence-1"].Summary)
	}
	if bundle.EvidenceContext.UsedTokens <= 0 {
		t.Fatalf("evidence context used tokens = %d", bundle.EvidenceContext.UsedTokens)
	}
}

func TestMarshalVerifiedBundleRemovesEvidenceRefsOutsideContextBudget(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "trace the request",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindCoreFlow, Required: true}},
	}
	report := InvestigationReport{
		Evidence: []EvidenceUnit{
			{ID: "ev-a", SourceKind: "code", Target: "a.go", Content: "first evidence", ContentHash: "a"},
			{ID: "ev-b", SourceKind: "code", Target: "b.go", Content: strings.Repeat("second evidence ", 20), ContentHash: "b"},
		},
		Claims: []VerifiedClaim{{
			ID: "claim-1", GoalID: "g1", Text: "the request follows the path", Status: ClaimSupported,
			EvidenceRefs: []EvidenceRef{{EvidenceID: "ev-a"}, {EvidenceID: "ev-b"}},
		}},
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1"}}},
	}
	raw, err := marshalVerifiedBundleWithBudget(contract, report, EvidenceContextBudget{
		MaxSummaryTokens: 20,
		MaxContextTokens: 4,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, claim := range bundle.SupportedClaims {
		for _, ref := range claim.Evidence {
			if _, ok := bundle.EvidenceLookup[ref.EvidenceID]; !ok {
				t.Fatalf("claim contains dangling evidence ref %q", ref.EvidenceID)
			}
		}
	}
	if len(bundle.EvidenceOmissions) == 0 {
		t.Fatalf("context omission was not recorded: %+v", bundle)
	}
}

func TestMarshalVerifiedBundleRecordsUnreferencedEvidenceOmissions(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "q",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindCoreFlow, Required: true}},
	}
	report := InvestigationReport{
		Evidence: []EvidenceUnit{
			{ID: "kept", SourceKind: "code", Target: "svc.go", Content: "kept evidence", ContentHash: "kept"},
			{ID: "pruned", SourceKind: "code", Target: "unused.go", Content: "unused evidence", ContentHash: "unused"},
		},
		Claims: []VerifiedClaim{{
			ID: "claim-1", GoalID: "g1", Text: "supported claim",
			Status: ClaimSupported, EvidenceRefs: []EvidenceRef{{EvidenceID: "kept"}},
		}},
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1"}}},
	}
	raw, err := marshalVerifiedBundle(contract, report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bundle.Omissions.EvidenceUnits != 1 {
		t.Fatalf("omissions.evidence_units = %d, want 1", bundle.Omissions.EvidenceUnits)
	}
	if len(bundle.EvidenceOmissions) != 1 || bundle.EvidenceOmissions[0].EvidenceID != "pruned" {
		t.Fatalf("evidence omissions = %+v", bundle.EvidenceOmissions)
	}
	if _, ok := bundle.EvidenceLookup["pruned"]; ok {
		t.Fatal("unreferenced evidence was still present in evidence_lookup")
	}
}

func TestMarshalVerifiedBundleBoundsCompleteComposerPayload(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "trace the request",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindCoreFlow, Required: true}},
	}
	content := strings.Repeat("the request passes through a verified downstream component;", 40)
	evidence := make([]EvidenceUnit, 0, 20)
	claims := make([]VerifiedClaim, 0, 20)
	claimIDs := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		evidenceID := "evidence-" + string(rune('a'+index))
		evidence = append(evidence, EvidenceUnit{
			ID: evidenceID, SourceKind: "code", Target: "service.go", Content: content,
			ContentHash: evidenceID,
		})
		claimID := "claim-" + evidenceID
		claimIDs = append(claimIDs, claimID)
		claims = append(claims, VerifiedClaim{
			ID: claimID, GoalID: "g1", Text: "the request follows the downstream path",
			Status: ClaimSupported, EvidenceRefs: []EvidenceRef{{EvidenceID: evidenceID}},
		})
	}
	raw, err := marshalVerifiedBundleWithBudget(contract, InvestigationReport{
		Evidence: evidence,
		Claims:   claims,
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: claimIDs}},
	}, EvidenceContextBudget{
		MaxSummaryTokens: 64,
		MaxContextTokens: 500,
		MaxBundleTokens:  1200,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if tokens := tooloutput.EstimateTokens(string(raw)); tokens > 1200 {
		t.Fatalf("bundle tokens = %d, want <= 1200", tokens)
	}
	var bundle verifiedBundleView
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if bundle.Omissions.Claims == 0 && len(bundle.EvidenceOmissions) == 0 {
		t.Fatalf("oversized bundle was not audited: %+v", bundle.Omissions)
	}
	for _, claim := range append(bundle.SupportedClaims, bundle.PartialClaims...) {
		for _, ref := range claim.Evidence {
			if _, ok := bundle.EvidenceLookup[ref.EvidenceID]; !ok {
				t.Fatalf("bounded bundle contains dangling evidence ref %q", ref.EvidenceID)
			}
		}
	}
}
