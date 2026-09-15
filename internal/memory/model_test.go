package memory

import "testing"

func TestValidFactKeyControlledVocabulary(t *testing.T) {
	allowed := []string{
		"user:response-language",
		"user:response-style",
		"user:current-focus",
		"user:role",
		"user:health",
		"user:culture",
		"user:environment",
		"user:profile-inference",
		"user:preference:svg",
		"user:correction:schedule-rgb",
	}
	for _, key := range allowed {
		if !validFactKey(key) {
			t.Errorf("validFactKey(%q) = false, want true", key)
		}
	}

	rejected := []string{
		"workspace:hesung-iot:architecture", // system/codebase fact, not a user memory
		"workspace:bert-slot:status",
		"user:role:iot",        // domain suffix forked the single role slot
		"user:preference:",     // empty topic
		"user:preference:SVG",  // topic must be kebab-case lowercase
		"user:unknown",         // not in the vocabulary
		"user:current-focus:x", // fixed slot takes no topic
		"",
	}
	for _, key := range rejected {
		if validFactKey(key) {
			t.Errorf("validFactKey(%q) = true, want false", key)
		}
	}
}

func TestCanonicalizeRecordRejectsNonSelfContainedContent(t *testing.T) {
	base := MemoryRecord{
		UserID: 1, FactKey: "user:response-language", Kind: KindPreference,
		SourceType: SourceUserStated, Confidence: 1,
	}
	for _, content := range []string{
		"zh",           // too short
		"????????",     // mojibake / placeholder
		"?? R100 ????", // fragment with placeholders
		"!@#$%^&*()",   // no letters
		"x�y�z�",       // replacement chars
	} {
		rec := base
		rec.Content = content
		if _, err := canonicalizeRecord(rec); err == nil {
			t.Errorf("canonicalizeRecord(content=%q) succeeded, want rejection", content)
		}
	}

	rec := base
	rec.Content = "用户希望回答使用中文。"
	if _, err := canonicalizeRecord(rec); err != nil {
		t.Errorf("canonicalizeRecord(valid content) rejected: %v", err)
	}
}

func TestCanonicalizeRecordSensitiveFactRequiresFirstPerson(t *testing.T) {
	// A sensitive fact stated in third person / as an inference is rejected.
	rec := MemoryRecord{
		UserID: 1, FactKey: "user:health", Kind: KindProfile,
		Content: "用户作息倾向早起。", SourceType: SourceUserStated, Confidence: 1,
	}
	if _, err := canonicalizeRecord(rec); err == nil {
		t.Fatal("sensitive fact without first-person marker was accepted")
	}

	// The same fact in the user's own voice is accepted.
	rec.Content = "我作息倾向早起。"
	if _, err := canonicalizeRecord(rec); err != nil {
		t.Fatalf("first-person sensitive fact rejected: %v", err)
	}

	// Non-sensitive facts do not require first person.
	rec = MemoryRecord{
		UserID: 1, FactKey: "user:response-language", Kind: KindPreference,
		Content: "用户希望回答使用中文。", SourceType: SourceUserStated, Confidence: 1,
	}
	if _, err := canonicalizeRecord(rec); err != nil {
		t.Fatalf("non-sensitive fact rejected: %v", err)
	}
}
