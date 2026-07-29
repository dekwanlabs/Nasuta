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
	if first.Evidence == nil {
		t.Fatal("empty evidence must be represented as an empty array")
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

func TestBuildArtifactRejectsEvidenceOutsideSnapshot(t *testing.T) {
	raw := []byte(`{
		"background":"b","goals":["g"],"functional_requirements":["f"],
		"acceptance_criteria":["a"],
		"claims":[{"statement":"existing behavior","classification":"fact","evidence_ids":[0]}]
	}`)
	_, err := BuildArtifact(KindRequirementAnalysis, "feat_1", "art_req", OriginAgent, raw, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "outside snapshot") {
		t.Fatalf("expected invalid evidence reference, got %v", err)
	}
}

func TestBuildArtifactCanonicalizesImplementationRepositories(t *testing.T) {
	raw := []byte(`{
		"repositories":[{
			"repository":" team/nasuta/ ",
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}]
	}`)
	artifact, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	var document ImplementationPlanDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Repositories[0].Repository; got != "team/nasuta" {
		t.Fatalf("repository = %q", got)
	}
}

func TestBuildArtifactCanonicalizesImplementationExpectedPaths(t *testing.T) {
	raw := []byte(`{
		"repositories":[{
			"repository":"team/nasuta",
			"expected_paths":[" internal/featuredelivery/ ","internal/featuredelivery"],
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}]
	}`)
	artifact, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	var document ImplementationPlanDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &document); err != nil {
		t.Fatal(err)
	}
	paths := document.Repositories[0].ExpectedPaths
	if len(paths) != 1 || paths[0] != "internal/featuredelivery" {
		t.Fatalf("expected paths = %v", paths)
	}

	raw = []byte(`{
		"repositories":[{
			"repository":"team/nasuta","expected_paths":["../outside"],
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}]
	}`)
	if _, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1); err == nil {
		t.Fatal("repository-escaping expected path must be rejected")
	}
}
