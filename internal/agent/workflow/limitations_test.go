package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
)

func TestNormalizeLimitationsDeduplicatesSortsAndRetainsDetail(t *testing.T) {
	raw := []rawLimitation{
		{Text: "  security\tevidence unavailable  ", ProducerNodeIDs: []string{"code"}, EvidenceRefs: []string{"ev-2"}, FirstSeen: 0},
		{Text: "security evidence unavailable", ProducerNodeIDs: []string{"runtime"}, EvidenceRefs: []string{"ev-1", "ev-2"}, FirstSeen: 1},
		{Text: "service unavailable", ProducerNodeIDs: []string{"runtime"}, EvidenceRefs: []string{"ev-3"}, FirstSeen: 2},
		{Text: "A\u3000Ｂ evidence gap", ProducerNodeIDs: []string{"docs"}, FirstSeen: 3},
		{Text: "", FirstSeen: 4},
	}

	result, err := normalizeLimitations("workflow_1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(result.Primary), 3; got != want {
		t.Fatalf("primary length = %d, want %d", got, want)
	}
	if result.Primary[0] != "security evidence unavailable" ||
		result.Primary[1] != "service unavailable" ||
		result.Primary[2] != "A B evidence gap" {
		t.Fatalf("primary ordering = %#v", result.Primary)
	}

	var detail limitationsDetailPayload
	if err := json.Unmarshal(result.Detail.Content, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RawCount != 5 || detail.DeduplicatedCount != 3 ||
		detail.MergedCount != 1 || detail.DisplayedCount != 3 || detail.OmittedCount != 0 {
		t.Fatalf("detail counts = %+v", detail)
	}
	if len(detail.Limitations) != 3 {
		t.Fatalf("detail limitations length = %d, want 3", len(detail.Limitations))
	}
	merged := detail.Limitations[0]
	if len(merged.MergedFromIDs) != 2 ||
		len(merged.ProducerNodeIDs) != 2 ||
		len(merged.EvidenceRefs) != 2 ||
		merged.MergeMethod != "exact_normalized_text" {
		t.Fatalf("merged limitation = %+v", merged)
	}
	if result.Ref.TotalCount != 3 || result.Ref.DisplayedCount != 3 || result.Ref.OmittedCount != 0 {
		t.Fatalf("detail ref = %+v", result.Ref)
	}
	if !regexp.MustCompile(`^art_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(result.Detail.ID) {
		t.Fatalf("artifact id = %q", result.Detail.ID)
	}
}

func TestNormalizeLimitationsCapsPrimaryButKeepsAllInArtifact(t *testing.T) {
	raw := make([]rawLimitation, 0, PrimaryLimitationsDisplayLimit+3)
	for index := 0; index < PrimaryLimitationsDisplayLimit+3; index++ {
		raw = append(raw, rawLimitation{
			Text:      "limitation " + string(rune('a'+index)),
			FirstSeen: index,
		})
	}

	result, err := normalizeLimitations("workflow_2", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(result.Primary), PrimaryLimitationsDisplayLimit; got != want {
		t.Fatalf("primary length = %d, want %d", got, want)
	}
	var detail limitationsDetailPayload
	if err := json.Unmarshal(result.Detail.Content, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RawCount != PrimaryLimitationsDisplayLimit+3 ||
		detail.DeduplicatedCount != PrimaryLimitationsDisplayLimit+3 ||
		detail.DisplayedCount != PrimaryLimitationsDisplayLimit ||
		detail.OmittedCount != 3 ||
		len(detail.Limitations) != PrimaryLimitationsDisplayLimit+3 {
		t.Fatalf("detail retention = %+v", detail)
	}
	for index, limitation := range detail.Limitations {
		wantDisplayed := index < PrimaryLimitationsDisplayLimit
		if limitation.Displayed != wantDisplayed || limitation.Rank != index+1 {
			t.Fatalf("limitation[%d] = %+v", index, limitation)
		}
	}
}

func TestNormalizeLimitationsDoesNotMergeSimilarText(t *testing.T) {
	result, err := normalizeLimitations("workflow_3", []rawLimitation{
		{Text: "service unavailable for Alexa", FirstSeen: 0},
		{Text: "service unavailable for Google", FirstSeen: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var detail limitationsDetailPayload
	if err := json.Unmarshal(result.Detail.Content, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.DeduplicatedCount != 2 || len(detail.Limitations) != 2 {
		t.Fatalf("similar limitations were merged: %+v", detail.Limitations)
	}
}

func TestLimitationsDetailPersistenceFailureHasSpecificErrorCode(t *testing.T) {
	persistErr := &workflowArtifactPersistenceError{
		code: limitationsDetailPersistFailedCode,
		err:  fmt.Errorf("database unavailable"),
	}
	wrapped := fmt.Errorf("%w: persist node success: %w", ErrNodePersistence, persistErr)
	status, code := resultStatus(wrapped)
	if status != RunFailed || code != limitationsDetailPersistFailedCode {
		t.Fatalf("persistence failure status = %s/%q", status, code)
	}
}
