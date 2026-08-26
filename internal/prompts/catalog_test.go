package prompts

import (
	"strings"
	"testing"
)

func TestCatalogContainsDeclaredPrompts(t *testing.T) {
	for id, file := range idFiles {
		if got := Text(id); strings.TrimSpace(got) == "" {
			t.Errorf("%s (%s) is empty", id, file)
		}
	}
}

func TestRenderRejectsMissingTemplateData(t *testing.T) {
	_, err := Render(AgentQACore, struct{}{})
	if err == nil {
		t.Fatal("Render accepted missing template data")
	}
}

func TestRenderSupportsFeatureDeliveryHelpers(t *testing.T) {
	data := struct {
		Run struct {
			ID             string
			Repo           string
			BaseCommit     string
			NetworkEnabled bool
		}
		RepositoryPlan struct {
			ExpectedPaths []string
			Steps         []struct {
				Description string
				DoneWhen    []string
			}
		}
		ApprovedArtifacts []struct {
			Kind             string
			Version          int
			ID               string
			RenderedMarkdown string
		}
	}{}
	data.Run.ID = "run-1"
	data.Run.Repo = "team/service"
	data.Run.BaseCommit = "abc123"
	data.RepositoryPlan.ExpectedPaths = []string{"internal/export"}
	data.RepositoryPlan.Steps = append(data.RepositoryPlan.Steps, struct {
		Description string
		DoneWhen    []string
	}{Description: "implement export", DoneWhen: []string{"tests pass"}})
	data.ApprovedArtifacts = append(data.ApprovedArtifacts, struct {
		Kind             string
		Version          int
		ID               string
		RenderedMarkdown string
	}{Kind: "requirement", Version: 1, ID: "artifact-1", RenderedMarkdown: "# Requirement"})

	got, err := Render(FeatureDeliveryCodingTask, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"run-1", "1. implement export", "tests pass", "# Requirement"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("render is missing %q:\n%s", expected, got)
		}
	}
}

func TestIncidentPromptDoesNotForceUnsupportedRootCause(t *testing.T) {
	prompt := Text(IncidentSystem)
	for _, required := range []string{
		"Use only evidence supplied in the incident input",
		"Logs, traces, and code hints are optional",
		"state that the root cause is not established",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("incident prompt missing evidence boundary %q", required)
		}
	}
}

func TestInvestigationSynthesizerPromptPreservesUserFacingStructure(t *testing.T) {
	prompt := Text(AgentCatalogSynthesizer)
	for _, required := range []string{
		`"workflow.synthesis_objective"`,
		`"investigation_goals"`,
		`Use short "##" headings`,
		"Never collapse a multi-goal or multi-path answer into one dense paragraph",
		"final evidence-boundary section",
		`Do not lead with "verification"`,
		`Markdown is required inside the "answer" string`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("synthesizer prompt missing structure rule %q", required)
		}
	}
}

func TestRetrievalExecutionPromptDecomposesIndependentComparisons(t *testing.T) {
	prompt := Text(RetrievalExecution)
	for _, required := range []string{
		"do not collapse all named alternatives into one generic comparison task",
		"one task per alternative or coherent mechanism",
		"do not model final synthesis as a parallel investigation task",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("retrieval execution prompt missing comparison rule %q", required)
		}
	}
}

func TestRetrievalExecutionAuditPromptIsNarrow(t *testing.T) {
	prompt := Text(RetrievalExecutionAudit)
	for _, required := range []string{
		`exactly one top-level property: "tasks"`,
		"user-level objective",
		"Every returned task must set independently_useful to true and depends_on to []",
		"Drop genuinely sequential candidates",
		"For a focused fact question, return tasks as []",
		"Do not return strategy",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("retrieval execution audit prompt missing %q", required)
		}
	}
}
