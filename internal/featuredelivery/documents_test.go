package featuredelivery

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildArtifactRejectsTrailingJSON(t *testing.T) {
	_, err := BuildArtifact(
		KindRequirement, "feat_1", "", OriginUser,
		[]byte(`{"description":"needed"} {"description":"extra"}`), nil, 1,
	)
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestBuildArtifactRendersDeterministically(t *testing.T) {
	raw, err := json.Marshal(RequirementDocument{
		Description:        "Add delivery runs",
		TargetRepositories: []string{"team/nasuta"},
		AcceptanceCriteria: []string{"A patch is produced"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildArtifact(KindRequirement, "feat_1", "", OriginUser, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildArtifact(KindRequirement, "feat_1", "", OriginUser, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash || first.RenderedMarkdown != second.RenderedMarkdown {
		t.Fatal("same document did not produce stable content")
	}
}

func TestBlockingQuestions(t *testing.T) {
	raw := []byte(`{
		"background":"b","goals":["g"],"functional_requirements":["f"],
		"acceptance_criteria":["a"],"blocking_questions":["Who owns this?"]
	}`)
	artifact, err := BuildArtifact(KindRequirementAnalysis, "feat_1", "art_req", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	questions, err := BlockingQuestions(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("unexpected questions: %#v", questions)
	}
}
