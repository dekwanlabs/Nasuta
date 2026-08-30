package investigation

import (
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestEvidenceLedgerDeduplicatesAndValidatesContentHash(t *testing.T) {
	ledger := NewEvidenceLedger()
	candidate := EvidenceCandidate{
		SourceKind: "code",
		Target:     "service-a",
		Section:    "handler",
		Content:    "model call is made here",
		Facets:     []string{"entrypoint"},
	}
	unit, admitted, err := ledger.Admit("task-a", candidate)
	if err != nil || !admitted {
		t.Fatalf("first admit = %#v, %v, want admitted", unit, err)
	}
	duplicate, admitted, err := ledger.Admit("task-b", candidate)
	if err != nil || admitted || duplicate.ID != unit.ID {
		t.Fatalf("duplicate admit = %#v, %v, %v", duplicate, admitted, err)
	}
	if _, _, err := ledger.Admit("task-c", EvidenceCandidate{
		SourceKind:  candidate.SourceKind,
		Target:      candidate.Target,
		Content:     candidate.Content,
		ContentHash: "not-the-content-hash",
	}); err == nil {
		t.Fatal("content hash mismatch was accepted")
	}
	if err := ledger.ValidateRef(EvidenceRef{
		EvidenceID:  unit.ID,
		SourceKind:  unit.SourceKind,
		Target:      unit.Target,
		ContentHash: unit.ContentHash,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimLedgerRequiresAdmittedEvidence(t *testing.T) {
	evidence := NewEvidenceLedger()
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "architecture", Required: true}}, evidence)
	_, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1",
		Text:   "unsupported claim",
		Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{
			EvidenceID: "missing",
		}},
	})
	if err == nil {
		t.Fatal("claim with missing evidence was accepted")
	}
}

func TestClaimCoverageDowngradesWhenSupportedAndPartialClaimsMix(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("collect", EvidenceCandidate{
		SourceKind: "docs",
		Target:     "runbook",
		Content:    "the runbook describes the flow",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}}, evidence)
	ref := EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	for _, candidate := range []ClaimCandidate{
		{GoalID: "g1", Text: "the flow is supported", Status: ClaimSupported, EvidenceRefs: []EvidenceRef{ref}},
		{GoalID: "g1", Text: "one transition remains uncertain", Status: ClaimPartial, EvidenceRefs: []EvidenceRef{ref}},
	} {
		if _, _, err := claims.Admit("verify", candidate); err != nil {
			t.Fatal(err)
		}
	}
	coverage := claims.Coverage()
	if len(coverage) != 1 || coverage[0].Status != GoalPartial {
		t.Fatalf("coverage = %#v, want one partial goal", coverage)
	}
}

func TestClaimCoverageRequiresHighRiskMinimumEvidence(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("collect", EvidenceCandidate{
		SourceKind: "code",
		Target:     "service-a",
		Content:    "the entrypoint is confirmed",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{
		ID: "g1", Kind: "entrypoint", Required: true,
		HighRisk: true, MinimumCoverage: 2,
	}}, evidence)
	ref := EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	if _, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1", Text: "the entrypoint is confirmed", Status: ClaimSupported, EvidenceRefs: []EvidenceRef{ref},
	}); err != nil {
		t.Fatal(err)
	}
	coverage := claims.Coverage()
	if len(coverage) != 1 || coverage[0].Status != GoalPartial {
		t.Fatalf("coverage = %#v, want partial until minimum evidence is met", coverage)
	}
}

func TestEvidenceLedgerAdmitsIdentityOnlySeed(t *testing.T) {
	ledger := NewEvidenceLedger()
	unit, admitted, err := ledger.AdmitSeed("seed", EvidenceUnit{
		SourceKind: "code", Target: "service-a", Section: "L10-L20",
		Version: "v1", TimeRange: "release-1", ContentHash: "seed-hash",
	})
	if err != nil || !admitted {
		t.Fatalf("seed admit = %#v, %v, want admitted", unit, err)
	}
	if unit.Content != "" {
		t.Fatalf("seed evidence content = %q, want empty", unit.Content)
	}
	if unit.Version != "v1" || unit.TimeRange != "release-1" {
		t.Fatalf("seed identity dimensions = %#v", unit)
	}
	if err := ledger.ValidateRef(EvidenceRef{
		EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target,
		Section: unit.Section, Version: unit.Version, TimeRange: unit.TimeRange,
		ContentHash: unit.ContentHash,
	}); err != nil {
		t.Fatal(err)
	}
	other, admitted, err := ledger.AdmitSeed("seed-2", EvidenceUnit{
		SourceKind: unit.SourceKind, Target: unit.Target, Section: unit.Section,
		Version: "v2", TimeRange: unit.TimeRange, ContentHash: unit.ContentHash,
	})
	if err != nil || !admitted || other.ID == unit.ID {
		t.Fatalf("versioned seed = %#v, %v, want independent identity", other, admitted)
	}
}

func TestEvidenceLedgerSeparatesIdentityDimensions(t *testing.T) {
	base := EvidenceCandidate{
		SourceKind: "code", Target: "service-a", Section: "handler",
		Version: "v1", TimeRange: "2026-08-22T00:00:00Z", Content: "same fact",
	}
	variants := []EvidenceCandidate{
		{SourceKind: "runtime", Target: base.Target, Section: base.Section, Version: base.Version, TimeRange: base.TimeRange, Content: base.Content},
		{SourceKind: base.SourceKind, Target: base.Target, Section: base.Section, Version: "v2", TimeRange: base.TimeRange, Content: base.Content},
		{SourceKind: base.SourceKind, Target: base.Target, Section: base.Section, Version: base.Version, TimeRange: "2026-08-21T00:00:00Z", Content: base.Content},
	}

	ledger := NewEvidenceLedger()
	first, admitted, err := ledger.Admit("task-1", base)
	if err != nil || !admitted {
		t.Fatalf("base admit = %#v, %v, want admitted", first, err)
	}
	for index, variant := range variants {
		unit, admitted, err := ledger.Admit("task-variant", variant)
		if err != nil || !admitted {
			t.Fatalf("variant %d admit = %#v, %v, want admitted", index, unit, err)
		}
		if unit.ID == first.ID {
			t.Fatalf("variant %d reused base evidence ID: %q", index, unit.ID)
		}
	}
	if conflicts := ledger.Conflicts(); len(conflicts) != 0 {
		t.Fatalf("independent evidence identities became conflicts: %#v", conflicts)
	}

	competing := base
	competing.Content = "different fact"
	unit, admitted, err := ledger.Admit("task-2", competing)
	if err != nil || !admitted {
		t.Fatalf("competing admit = %#v, %v, want admitted", unit, err)
	}
	conflicts := ledger.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("same complete identity produced %d conflicts: %#v", len(conflicts), conflicts)
	}
	identity := conflicts[0].Identity
	if identity.SourceKind != base.SourceKind || identity.Target != base.Target ||
		identity.Section != base.Section || identity.Version != base.Version ||
		identity.TimeRange != base.TimeRange {
		t.Fatalf("conflict identity = %#v", identity)
	}
}

func TestEvidenceLedgerDeduplicatesOnlyMatchingIdentity(t *testing.T) {
	ledger := NewEvidenceLedger()
	candidate := EvidenceCandidate{
		SourceKind: "docs", Target: "runbook", Section: "steps",
		Version: "2026-08", TimeRange: "release-1", Content: "same content",
	}
	first, admitted, err := ledger.Admit("task-1", candidate)
	if err != nil || !admitted {
		t.Fatalf("first admit = %#v, %v, want admitted", first, err)
	}
	duplicate, admitted, err := ledger.Admit("task-2", candidate)
	if err != nil || admitted || duplicate.ID != first.ID {
		t.Fatalf("duplicate admit = %#v, %v, want same evidence", duplicate, admitted)
	}
	otherVersion := candidate
	otherVersion.Version = "2026-09"
	versioned, admitted, err := ledger.Admit("task-3", otherVersion)
	if err != nil || !admitted || versioned.ID == first.ID {
		t.Fatalf("versioned admit = %#v, %v, want independent evidence", versioned, admitted)
	}
}

func TestEvidenceReferenceValidatesFullIdentity(t *testing.T) {
	ledger := NewEvidenceLedger()
	unit, _, err := ledger.Admit("task-1", EvidenceCandidate{
		SourceKind: "runtime", Target: "service-a", Section: "errors",
		Version: "build-42", TimeRange: "5m", Content: "timeout observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := EvidenceRef{
		EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target,
		Section: unit.Section, Version: unit.Version, TimeRange: unit.TimeRange,
		ContentHash: unit.ContentHash,
	}
	if err := ledger.ValidateRef(valid); err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}
	for name, mutate := range map[string]func(*EvidenceRef){
		"section":    func(ref *EvidenceRef) { ref.Section = "metrics" },
		"version":    func(ref *EvidenceRef) { ref.Version = "build-41" },
		"time_range": func(ref *EvidenceRef) { ref.TimeRange = "1h" },
	} {
		ref := valid
		mutate(&ref)
		if err := ledger.ValidateRef(ref); err == nil {
			t.Fatalf("%s mismatch was accepted", name)
		}
	}
}

func TestClaimLedgerMergesIndependentProvenanceForSameClaim(t *testing.T) {
	evidence := NewEvidenceLedger()
	first, _, err := evidence.Admit("code-task", EvidenceCandidate{
		SourceKind: "code", Target: "service-a", Content: "provider is configured",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := evidence.Admit("runtime-task", EvidenceCandidate{
		SourceKind: "runtime", Target: "service-a", Content: "provider was observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "dependency", Required: true}}, evidence)
	makeRef := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	}
	claim := ClaimCandidate{GoalID: "g1", Text: "the provider is configured and observed", Status: ClaimSupported, EvidenceRefs: []EvidenceRef{makeRef(first)}}
	merged, added, err := claims.Admit("verify-code", claim)
	if err != nil || !added || len(merged.EvidenceRefs) != 1 {
		t.Fatalf("first claim = %#v, added=%v, err=%v", merged, added, err)
	}
	claim.EvidenceRefs = []EvidenceRef{makeRef(second), makeRef(first)}
	merged, added, err = claims.Admit("verify-runtime", claim)
	if err != nil || added || len(merged.EvidenceRefs) != 2 {
		t.Fatalf("merged claim = %#v, added=%v, err=%v", merged, added, err)
	}
	if merged.EvidenceRefs[0].EvidenceID >= merged.EvidenceRefs[1].EvidenceID {
		t.Fatalf("merged refs are not deterministic: %#v", merged.EvidenceRefs)
	}
	if len(claims.All()) != 1 {
		t.Fatalf("claim count = %d, want one merged claim", len(claims.All()))
	}
}

func TestClaimLedgerDowngradesConflictingEvidenceAndTracksCoverageRequirements(t *testing.T) {
	evidence := NewEvidenceLedger()
	base := EvidenceCandidate{SourceKind: "runtime", Target: "service-a", Section: "status", Version: "v1", TimeRange: "5m", Content: "healthy", Facets: []string{"status"}}
	current, _, err := evidence.Admit("runtime-a", base)
	if err != nil {
		t.Fatal(err)
	}
	competing := base
	competing.Content = "unhealthy"
	incoming, _, err := evidence.Admit("runtime-b", competing)
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{
		{ID: "g1", Kind: "runtime", Required: true, Facets: []string{"status", "health"}, RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime}},
	}, evidence)
	ref := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, Section: unit.Section, Version: unit.Version, TimeRange: unit.TimeRange, ContentHash: unit.ContentHash}
	}
	claim, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1", Text: "the runtime state is settled", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{ref(current), ref(incoming)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != ClaimConflicting {
		t.Fatalf("claim status = %q, want conflicting", claim.Status)
	}
	coverage := claims.Coverage()
	if len(coverage) != 1 || coverage[0].Status != GoalPartial {
		t.Fatalf("coverage = %#v, want partial", coverage)
	}
	if len(coverage[0].MissingFacets) != 1 || coverage[0].MissingFacets[0] != "health" {
		t.Fatalf("missing facets = %#v", coverage[0].MissingFacets)
	}
	if len(coverage[0].MissingSources) != 0 {
		t.Fatalf("missing sources = %#v", coverage[0].MissingSources)
	}
}

func TestClaimLedgerCoverageReportsMissingRequiredSource(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("code", EvidenceCandidate{SourceKind: "internal", Target: "service-a", Content: "entrypoint found", Facets: []string{"entrypoint"}})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{
		{ID: "g1", Kind: "entrypoint", Required: true, RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime}},
	}, evidence)
	if _, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1", Text: "the entrypoint exists", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}},
	}); err != nil {
		t.Fatal(err)
	}
	coverage := claims.Coverage()
	if len(coverage) != 1 || coverage[0].Status != GoalPartial || len(coverage[0].MissingSources) != 1 || coverage[0].MissingSources[0] != "runtime" {
		t.Fatalf("coverage = %#v", coverage)
	}
}

func TestPruneUnreferencedEvidenceKeepsConflictReferences(t *testing.T) {
	evidence := NewEvidenceLedger()
	current, _, err := evidence.Admit("a", EvidenceCandidate{SourceKind: "runtime", Target: "service-a", Section: "state", Content: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	incoming, _, err := evidence.Admit("b", EvidenceCandidate{SourceKind: "runtime", Target: "service-a", Section: "state", Content: "unhealthy"})
	if err != nil {
		t.Fatal(err)
	}
	ref := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, Section: unit.Section, ContentHash: unit.ContentHash}
	}
	pruned := PruneUnreferencedEvidence(InvestigationReport{
		Evidence: evidence.All(),
		Claims:   []VerifiedClaim{{EvidenceRefs: []EvidenceRef{ref(current)}, ConflictRefs: []EvidenceRef{ref(incoming)}}},
	})
	if len(pruned.Evidence) != 2 {
		t.Fatalf("pruned evidence = %#v, want both provenance units", pruned.Evidence)
	}
}

func TestClaimLedgerCanonicalizesGoalIDBeforeLookup(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("source", EvidenceCandidate{SourceKind: "internal", Target: "service-a", Content: "entrypoint found"})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "entrypoint", Required: true}}, evidence)
	claim, added, err := claims.Admit("verify", ClaimCandidate{
		GoalID:       "  g1 ",
		Text:         "  the entrypoint exists  ",
		Status:       ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: unit.ID}},
	})
	if err != nil || !added {
		t.Fatalf("claim = %#v, added=%v, err=%v", claim, added, err)
	}
	if claim.GoalID != "g1" || claim.Text != "the entrypoint exists" {
		t.Fatalf("claim was not canonicalized: %#v", claim)
	}
	if len(claim.EvidenceRefs) != 1 || claim.EvidenceRefs[0].SourceKind != unit.SourceKind ||
		claim.EvidenceRefs[0].Target != unit.Target || claim.EvidenceRefs[0].ContentHash != unit.ContentHash {
		t.Fatalf("evidence ref was not completed from admitted unit: %#v", claim.EvidenceRefs)
	}
}

func TestClaimLedgerKeepsPartialStatusWithExplicitConflictReference(t *testing.T) {
	evidence := NewEvidenceLedger()
	current, _, err := evidence.Admit("current", EvidenceCandidate{SourceKind: "runtime", Target: "service-a", Section: "state", Content: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	incoming, _, err := evidence.Admit("incoming", EvidenceCandidate{SourceKind: current.SourceKind, Target: current.Target, Section: current.Section, Content: "unhealthy"})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "runtime", Required: true}}, evidence)
	ref := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, Section: unit.Section, ContentHash: unit.ContentHash}
	}
	claim, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1", Text: "the state is only partially established", Status: ClaimPartial,
		EvidenceRefs: []EvidenceRef{ref(current)}, ConflictRefs: []EvidenceRef{ref(incoming)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != ClaimPartial {
		t.Fatalf("claim status = %q, want partial", claim.Status)
	}
}

func TestClaimLedgerDowngradesSupportedClaimWithExplicitConflictReference(t *testing.T) {
	evidence := NewEvidenceLedger()
	support, _, err := evidence.Admit("support", EvidenceCandidate{SourceKind: "runtime", Target: "service-a", Content: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	conflict, _, err := evidence.Admit("other", EvidenceCandidate{SourceKind: "runtime", Target: "service-b", Content: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "runtime", Required: true}}, evidence)
	ref := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	}
	claim, _, err := claims.Admit("verify", ClaimCandidate{
		GoalID: "g1", Text: "the state is supported but disputed", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{ref(support)}, ConflictRefs: []EvidenceRef{ref(conflict)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != ClaimConflicting {
		t.Fatalf("claim status = %q, want conflicting", claim.Status)
	}
}

func TestClaimLedgerMergeReevaluatesConflictsAcrossAdmissions(t *testing.T) {
	evidence := NewEvidenceLedger()
	first, _, err := evidence.Admit("first", EvidenceCandidate{
		SourceKind: "runtime", Target: "service-a", Section: "state", Version: "v1", TimeRange: "5m", Content: "healthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := evidence.Admit("second", EvidenceCandidate{
		SourceKind: "runtime", Target: "service-a", Section: "state", Version: "v1", TimeRange: "5m", Content: "unhealthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "runtime", Required: true}}, evidence)
	ref := func(unit EvidenceUnit) EvidenceRef {
		return EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, Section: unit.Section, Version: unit.Version, TimeRange: unit.TimeRange, ContentHash: unit.ContentHash}
	}
	if _, _, err := claims.Admit("verify-first", ClaimCandidate{
		GoalID: "g1", Text: "the runtime state is healthy", Status: ClaimSupported, EvidenceRefs: []EvidenceRef{ref(first)},
	}); err != nil {
		t.Fatal(err)
	}
	merged, added, err := claims.Admit("verify-second", ClaimCandidate{
		GoalID: "g1", Text: "the runtime state is healthy", Status: ClaimSupported, EvidenceRefs: []EvidenceRef{ref(second)},
	})
	if err != nil || added {
		t.Fatalf("merged claim = %#v, added=%v, err=%v", merged, added, err)
	}
	if merged.Status != ClaimConflicting || len(merged.EvidenceRefs) != 2 {
		t.Fatalf("merged claim = %#v, want conflicting claim with both refs", merged)
	}
}

func TestEvidenceLedgerRejectsOpaqueContent(t *testing.T) {
	ledger := NewEvidenceLedger()
	_, admitted, err := ledger.Admit("task-opaque", EvidenceCandidate{
		SourceKind: "code",
		Target:     "svc.go:42",
		Content:    "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7",
	})
	if admitted || !errors.Is(err, ErrOpaqueEvidence) {
		t.Fatalf("opaque evidence = admitted=%v err=%v, want rejection", admitted, err)
	}
}

func TestUserReadableClaimTextRejectsMachineJSON(t *testing.T) {
	for _, text := range []string{
		`{"service":"service-a","upstream":[],"downstream":[],"truncated":false}`,
		`[{"source":"code","target":"service-a"}]`,
		"{\n  \"matches\":[{\"docId\":\"doc-2015a2bba8c6e812\",\"title\":\"hsds-product\"",
		`{"matches":[{"docId":"doc-2015a2bba8c6e812","title":"hsds-product","chunk":2`,
	} {
		if isUserReadableClaimText(text) {
			t.Fatalf("machine JSON claim accepted: %s", text)
		}
	}
}

func TestUserReadableClaimTextAllowsNaturalLanguageContainingMetadata(t *testing.T) {
	text := `The service returns metadata {"truncated":false} after the request completes.`
	if !isUserReadableClaimText(text) {
		t.Fatalf("natural-language claim containing metadata was rejected")
	}
}

func TestBudgetFailureRequiresOwnedArtifact(t *testing.T) {
	evidence := NewEvidenceLedger()
	candidate := EvidenceCandidate{SourceKind: "runbook", Target: "shared.md", Content: "usable evidence"}
	if _, admitted, err := evidence.Admit("sibling-task", candidate); err != nil || !admitted {
		t.Fatalf("admit sibling evidence = %v, %v", admitted, err)
	}
	failure := &RunFailure{Code: FailureBudget, TaskID: "failed-task"}
	if budgetFailureCanDeliver(evidence, nil, failure) {
		t.Fatal("sibling evidence satisfied budget failure")
	}
	failure.TaskID = "sibling-task"
	if !budgetFailureCanDeliver(evidence, nil, failure) {
		t.Fatal("task-owned evidence did not satisfy budget failure")
	}

	claims := NewClaimLedger([]EvidenceGoal{{ID: "goal"}}, evidence)
	claim := ClaimCandidate{GoalID: "goal", Text: "The runbook supports this claim.", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: evidence.All()[0].ID}}}
	if _, admitted, err := claims.Admit("verifier-task", claim); err != nil || !admitted {
		t.Fatalf("admit verifier claim = %v, %v", admitted, err)
	}
	failure.TaskID = "other-verifier"
	if budgetFailureCanDeliver(nil, claims, failure) {
		t.Fatal("sibling claim satisfied budget failure")
	}
	failure.TaskID = "verifier-task"
	if !budgetFailureCanDeliver(nil, claims, failure) {
		t.Fatal("task-owned claim did not satisfy budget failure")
	}
}

func TestClaimLedgerPreservesEntityIDsAcrossAdmitAndRestore(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("collect", EvidenceCandidate{
		SourceKind: "code", Target: "checkout.go", Content: "Checkout routes an order to billing.",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	ledger := NewClaimLedger([]EvidenceGoal{{ID: "core_flow", Kind: "core_flow", Required: true}}, evidence)
	claim, added, err := ledger.Admit("verify", ClaimCandidate{
		GoalID: "core_flow", Text: "Checkout routes an order to billing.", Status: ClaimSupported,
		EntityIDs: []string{"checkout"}, EvidenceRefs: []EvidenceRef{ref},
	})
	if err != nil || !added {
		t.Fatalf("admit claim = %#v added=%t err=%v", claim, added, err)
	}
	if len(claim.EntityIDs) != 1 || claim.EntityIDs[0] != "checkout" {
		t.Fatalf("claim entity ids = %#v", claim.EntityIDs)
	}

	restored := NewClaimLedgerFrom([]EvidenceGoal{{ID: "core_flow", Kind: "core_flow", Required: true}}, evidence, ledger.All())
	claims := restored.All()
	if len(claims) != 1 || len(claims[0].EntityIDs) != 1 || claims[0].EntityIDs[0] != "checkout" {
		t.Fatalf("restored claims = %#v", claims)
	}
}

func TestClaimLedgerDoesNotMergeSameTextAcrossDifferentEntities(t *testing.T) {
	evidence := NewEvidenceLedger()
	unit, _, err := evidence.Admit("collect", EvidenceCandidate{
		SourceKind: "docs", Target: "overview.md", Content: "The domain has a documented application flow.",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := EvidenceRef{EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash}
	ledger := NewClaimLedger([]EvidenceGoal{{ID: "core_flow", Kind: "core_flow", Required: true}}, evidence)
	for _, entityID := range []string{"checkout", "billing"} {
		if _, added, err := ledger.Admit("verify-"+entityID, ClaimCandidate{
			GoalID: "core_flow", Text: "The domain has a documented application flow.", Status: ClaimSupported,
			EntityIDs: []string{entityID}, EvidenceRefs: []EvidenceRef{ref},
		}); err != nil || !added {
			t.Fatalf("admit %s: added=%t err=%v", entityID, added, err)
		}
	}
	claims := ledger.All()
	if len(claims) != 2 {
		t.Fatalf("claims = %#v, want distinct claims per entity", claims)
	}
	if claims[0].ID == claims[1].ID {
		t.Fatalf("claim ids collided: %#v", claims)
	}
}
