package memory

import "testing"

func TestNormalizeExtractedDedupesByFactKeyAndCapsResults(t *testing.T) {
	entries := []extractedEntry{
		{FactKey: "user:response-language", Kind: string(KindAssistantInference), Content: "Probably uses English", SourceType: string(SourceAssistantInference), Confidence: 0.9},
		{FactKey: "user:response-language", Kind: string(KindPreference), Content: "Use Chinese", SourceType: string(SourceExplicitUser), Confidence: 1},
		{FactKey: "user:response-style", Kind: string(KindPreference), Content: "Lead with the answer", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:role:app", Kind: string(KindProfile), Content: "Owns App", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:role:iot", Kind: string(KindProfile), Content: "Owns IoT", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "user:role:cloud", Kind: string(KindProfile), Content: "Owns Cloud", SourceType: string(SourceUserStated), Confidence: 1},
		{FactKey: "workspace:user-center:owner", Kind: string(KindProfile), Content: "Owns user center", SourceType: string(SourceUserStated), Confidence: 1},
	}

	records := normalizeExtracted(entries)
	if len(records) != 5 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Content != "Use Chinese" || records[0].SourceType != SourceExplicitUser {
		t.Fatalf("deduped record = %#v", records[0])
	}
	for _, record := range records {
		if record.Authority != 0 || record.Status != "" {
			t.Fatalf("extracted record leaked storage state: %#v", record)
		}
	}
}
