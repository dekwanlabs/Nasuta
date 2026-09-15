package memory

import "testing"

func TestNormalizeExtractedDedupesByFactKeyAndCapsResults(t *testing.T) {
	entries := []extractedEntry{
		{FactKey: "user:response-language", Kind: string(KindAssistantInference), Content: "Probably uses English", SourceType: string(SourceAssistantInference), Confidence: 0.9},
		{FactKey: "user:response-language", Kind: string(KindPreference), Content: "Use Chinese", SourceType: string(SourceExplicitUser), Confidence: 1},
		{FactKey: "user:response-style", Kind: string(KindPreference), Content: "Lead with the answer", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:preference:app", Kind: string(KindProfile), Content: "Owns App", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:preference:iot", Kind: string(KindProfile), Content: "Owns IoT", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:preference:cloud", Kind: string(KindProfile), Content: "Owns Cloud", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:preference:user-center", Kind: string(KindProfile), Content: "Owns user center", SourceType: string(SourceUserStated), Confidence: 1},
	}

	records := normalizeExtracted(entries)
	// The cap keeps only the first maxExtractedMemories distinct fact keys; the
	// higher-authority "Use Chinese" wins the duplicated response-language slot.
	if len(records) != maxExtractedMemories {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Content != "Use Chinese" || records[0].SourceType != SourceExplicitUser {
		t.Fatalf("deduped record = %#v", records[0])
	}
	if records[1].FactKey != "user:response-style" {
		t.Fatalf("second record = %#v", records[1])
	}
	for _, record := range records {
		if record.Authority != 0 || record.Status != "" {
			t.Fatalf("extracted record leaked storage state: %#v", record)
		}
	}
}

func TestNormalizeExtractedLeavesWorkContextExpiryForStore(t *testing.T) {
	records := normalizeExtracted([]extractedEntry{{
		FactKey:    "user:current-focus",
		Kind:       string(KindWorkContext),
		Content:    "Refactor user center",
		SourceType: string(SourceUserStated),
		Confidence: 1,
	}})

	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].ExpiresAt != nil {
		t.Fatalf("expires_at = %v, want nil", records[0].ExpiresAt)
	}
}

func TestNormalizeConsolidatedAdmitsHighConfidenceEquivalent(t *testing.T) {
	existing := []ConsolidationMatch{{Record: MemoryRecord{
		ID: "active", FactKey: "user:response-style", Kind: KindPreference,
		Content: "Lead with the conclusion", SourceType: SourceUserStated, Status: StatusActive,
	}}}
	decisions := normalizeConsolidated([]extractedEntry{{
		FactKey: "user:response-style", Kind: string(KindPreference),
		Content: "Start with the main point", SourceType: string(SourceExplicitUser),
		Confidence: 1, Action: string(ConsolidationRefresh), TargetID: "active",
		Relation: "equivalent", DecisionConfidence: 0.92,
	}}, existing)
	if len(decisions) != 1 || decisions[0].Action != ConsolidationRefresh ||
		decisions[0].TargetID != "active" {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestNormalizeConsolidatedDropsUncertainMutation(t *testing.T) {
	existing := []ConsolidationMatch{{Record: MemoryRecord{
		ID: "active", FactKey: "user:response-style", Kind: KindPreference,
		Content: "Lead with the conclusion", SourceType: SourceUserStated, Status: StatusActive,
	}}}
	decisions := normalizeConsolidated([]extractedEntry{{
		FactKey: "user:response-style", Kind: string(KindPreference),
		Content: "Use detailed introductions", SourceType: string(SourceUserStated),
		Confidence: 1, Action: string(ConsolidationReplace), TargetID: "active",
		Relation: "contradiction", DecisionConfidence: 0.84,
	}}, existing)
	if len(decisions) != 0 {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestNormalizeConsolidatedRejectsLowerAuthorityReplacement(t *testing.T) {
	existing := []ConsolidationMatch{{Record: MemoryRecord{
		ID: "active", FactKey: "user:preference:billing", Kind: KindProfile,
		Content: "Owned by the platform team", SourceType: SourceExplicitUser,
		Authority: AuthorityExplicitUser, Status: StatusActive,
	}}}
	decisions := normalizeConsolidated([]extractedEntry{{
		FactKey: "user:preference:billing", Kind: string(KindProfile),
		Content: "Owned by the commerce team", SourceType: string(SourceUserStated),
		Confidence: 1, Action: string(ConsolidationReplace), TargetID: "active",
		Relation: "contradiction", DecisionConfidence: 0.95,
	}}, existing)
	if len(decisions) != 1 || decisions[0].Action != ConsolidationReject {
		t.Fatalf("decisions = %#v", decisions)
	}
}
