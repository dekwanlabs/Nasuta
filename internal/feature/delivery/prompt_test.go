package delivery

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/template"
)

var featurePromptNames = []string{
	"requirement_analysis.md",
	"technical_proposal.md",
	"system_design.md",
	"implementation_plan.md",
	"generation_request.md",
	"coding_task.md",
}

func TestFeaturePromptLocalesHaveMatchingFiles(t *testing.T) {
	for _, name := range featurePromptNames {
		t.Run(name, func(t *testing.T) {
			for _, locale := range []string{"en", "zh-CN"} {
				content := mustReadLocalizedFeaturePrompt(locale, name)
				if strings.TrimSpace(content) == "" {
					t.Fatalf("%s prompt %q is empty", locale, name)
				}
				for _, forbidden := range []string{"AGENTS.md", "CLAUDE.md"} {
					if strings.Contains(content, forbidden) {
						t.Fatalf("%s prompt %q references forbidden instruction file %q", locale, name, forbidden)
					}
				}
			}
		})
	}
}

func TestRequirementAnalysisPromptsRejectTechnicalInputs(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN"} {
		t.Run(locale, func(t *testing.T) {
			content := mustReadLocalizedFeaturePrompt(locale, "requirement_analysis.md")
			for _, required := range map[string][]string{
				"en": {
					"Use only the current requirement",
					"Do not use or request source code",
					"Technical discovery and impact analysis start in that next stage",
				},
				"zh-CN": {
					"只使用当前需求",
					"不使用或请求源代码",
					"技术发现和影响分析从下一阶段开始",
				},
			}[locale] {
				if !strings.Contains(content, required) {
					t.Fatalf("%s requirement analysis prompt is missing %q", locale, required)
				}
			}
			for _, forbidden := range []string{"initial_impact", "evidence_ids", "ontology_dependency"} {
				if strings.Contains(content, forbidden) {
					t.Fatalf("%s requirement analysis prompt contains technical field %q", locale, forbidden)
				}
			}
			for _, forbidden := range map[string][]string{
				"en":    {"approved requirement"},
				"zh-CN": {"已批准的需求", "已审核的需求"},
			}[locale] {
				if strings.Contains(content, forbidden) {
					t.Fatalf("%s requirement analysis prompt treats the requirement as reviewed: %q", locale, forbidden)
				}
			}
		})
	}
}

func TestLocalizedDynamicPromptsUseMatchingDataContracts(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN"} {
		t.Run(locale, func(t *testing.T) {
			request := template.Must(template.New("generation_request").Parse(
				mustReadLocalizedFeaturePrompt(locale, "generation_request.md"),
			))
			rendered, err := renderFeaturePrompt(request, generationPromptData{
				Contract: `{"problem_statement":"string"}`,
				Kind:     KindRequirementAnalysis,
				Input:    `{"feature":{"title":"export"}}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				`{"problem_statement":"string"}`,
				string(KindRequirementAnalysis),
				`{"feature":{"title":"export"}}`,
			} {
				if !strings.Contains(rendered, expected) {
					t.Fatalf("generation request is missing %q:\n%s", expected, rendered)
				}
			}

			coding := template.Must(
				template.New("coding_task").
					Funcs(template.FuncMap{
						"addOne": func(index int) int { return index + 1 },
					}).
					Parse(mustReadLocalizedFeaturePrompt(locale, "coding_task.md")),
			)
			rendered, err = renderFeaturePrompt(coding, codingTaskPromptData{
				Run: ImplementationRun{ID: "run-1", Repo: "team/service", BaseCommit: "abc123"},
				RepositoryPlan: RepositoryPlan{
					ExpectedPaths: []string{"internal/export"},
					Steps: []ImplementationStep{{
						Description: "implement export",
						DoneWhen:    []string{"tests pass"},
					}},
				},
				ApprovedArtifacts: []*Artifact{{
					ID: "artifact-1", Kind: KindRequirement, Version: 1,
					RenderedMarkdown: "# Requirement",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				"run-1",
				"team/service",
				"abc123",
				"internal/export",
				"implement export",
				"tests pass",
				"# Requirement",
			} {
				if !strings.Contains(rendered, expected) {
					t.Fatalf("coding task is missing %q:\n%s", expected, rendered)
				}
			}
		})
	}
}

func TestLocalizedStagePromptsExposeTheDocumentContractFields(t *testing.T) {
	stages := []struct {
		kind ArtifactKind
		name string
	}{
		{KindRequirementAnalysis, "requirement_analysis.md"},
		{KindTechnicalProposal, "technical_proposal.md"},
		{KindSystemDesign, "system_design.md"},
		{KindImplementationPlan, "implementation_plan.md"},
	}
	fieldPattern := regexp.MustCompile("(?m)^\\d+\\. `([^`]+)`")
	for _, stage := range stages {
		t.Run(string(stage.kind), func(t *testing.T) {
			var contract map[string]json.RawMessage
			if err := json.Unmarshal([]byte(generationDocumentContract(stage.kind)), &contract); err != nil {
				t.Fatal(err)
			}
			want := make([]string, 0, len(contract))
			for key := range contract {
				want = append(want, key)
			}
			sort.Strings(want)

			var previous []string
			for _, locale := range []string{"en", "zh-CN"} {
				matches := fieldPattern.FindAllStringSubmatch(mustReadLocalizedFeaturePrompt(locale, stage.name), -1)
				got := make([]string, 0, len(matches))
				for _, match := range matches {
					got = append(got, match[1])
				}
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s fields = %v, want %v", locale, got, want)
				}
				if previous != nil && !reflect.DeepEqual(got, previous) {
					t.Fatalf("localized field sets differ: %v != %v", got, previous)
				}
				previous = got
			}
		})
	}
}

func TestLocalizedGenerationPromptsDeclareTheirDirectParent(t *testing.T) {
	stages := []struct {
		name       string
		enParent   string
		zhCNParent string
	}{
		{"requirement_analysis.md", "current requirement", "当前需求"},
		{"technical_proposal.md", "approved `requirement_analysis`", "已批准的 `requirement_analysis`"},
		{"system_design.md", "approved `technical_proposal`", "已批准的 `technical_proposal`"},
		{"implementation_plan.md", "approved `system_design`", "已批准的 `system_design`"},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			for locale, parent := range map[string]string{"en": stage.enParent, "zh-CN": stage.zhCNParent} {
				content := mustReadLocalizedFeaturePrompt(locale, stage.name)
				if !strings.Contains(content, parent) {
					t.Fatalf("%s prompt does not declare direct parent %q", locale, parent)
				}
			}
		})
	}
}
