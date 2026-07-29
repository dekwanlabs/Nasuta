package featuredelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/knowledge"
)

type evidenceKnowledge struct {
	mu              sync.Mutex
	codeQueries     []knowledge.CodeSearchQuery
	serviceQueries  []knowledge.ServiceSearchQuery
	runbookQueries  []knowledge.RunbookQuery
	traceCalls      int
	traceActive     int
	maxTraceActive  int
	traceGate       chan struct{}
	traceGateClosed bool
	emptyServices   bool
}

func (source *evidenceKnowledge) SearchCode(_ context.Context, query knowledge.CodeSearchQuery) (knowledge.CodeSearchResult, error) {
	source.mu.Lock()
	source.codeQueries = append(source.codeQueries, query)
	source.mu.Unlock()
	prefix := queryPrefix(query.Query)
	matches := make([]knowledge.CodeSearchHit, 10)
	for index := range matches {
		matches[index] = knowledge.CodeSearchHit{
			Repo: "team/repo", Path: fmt.Sprintf("%s-%02d.go", prefix, index),
			StartLine: index + 1, EndLine: index + 2, Preview: fmt.Sprintf("code %s %d", prefix, index),
		}
	}
	return knowledge.CodeSearchResult{Matches: matches}, nil
}

func (source *evidenceKnowledge) SearchServices(_ context.Context, query knowledge.ServiceSearchQuery) (knowledge.ServiceSearchResult, error) {
	source.mu.Lock()
	source.serviceQueries = append(source.serviceQueries, query)
	empty := source.emptyServices
	source.mu.Unlock()
	if empty {
		return knowledge.ServiceSearchResult{}, nil
	}
	start := 0
	if strings.Contains(query.Query, "\n") {
		start = 3
	}
	matches := make([]knowledge.ServiceRecord, 10)
	for index := range matches {
		service := fmt.Sprintf("service-%02d", start+index)
		matches[index] = knowledge.ServiceRecord{ServiceName: service, Repo: "team/repo", Summary: service + " summary"}
	}
	return knowledge.ServiceSearchResult{Matches: matches}, nil
}

func (source *evidenceKnowledge) SearchRunbooks(_ context.Context, query knowledge.RunbookQuery) (knowledge.RunbookSearchResult, error) {
	source.mu.Lock()
	source.runbookQueries = append(source.runbookQueries, query)
	source.mu.Unlock()
	prefix := queryPrefix(query.Query)
	matches := make([]knowledge.RunbookSearchHit, 10)
	for index := range matches {
		matches[index] = knowledge.RunbookSearchHit{
			Record:        knowledge.RunbookRecord{Repo: "team/repo", Path: fmt.Sprintf("%s-%02d.md", prefix, index)},
			SectionHeader: "Runbook", ChunkText: fmt.Sprintf("runbook %s %d", prefix, index),
		}
	}
	return knowledge.RunbookSearchResult{Matches: matches}, nil
}

func (source *evidenceKnowledge) TraceDependencies(ctx context.Context, query knowledge.DependencyQuery) (knowledge.DependencyResult, error) {
	source.mu.Lock()
	source.traceCalls++
	source.traceActive++
	if source.traceActive > source.maxTraceActive {
		source.maxTraceActive = source.traceActive
	}
	if source.traceGate != nil && source.traceCalls == generationQueryConcurrency && !source.traceGateClosed {
		close(source.traceGate)
		source.traceGateClosed = true
	}
	gate := source.traceGate
	source.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return knowledge.DependencyResult{}, ctx.Err()
		}
	}
	source.mu.Lock()
	source.traceActive--
	source.mu.Unlock()
	return knowledge.DependencyResult{Downstream: []knowledge.DependencyEdge{{
		From: query.Service, To: query.Service + "-database", Type: "database", Confidence: 0.9,
	}}}, nil
}

func TestBuildEvidenceQueryPlanUsesBoundedTotalLimits(t *testing.T) {
	plan := buildEvidenceQueryPlan(Artifact{
		DocumentJSON:     json.RawMessage(`{"problem_statement":"add order export"}`),
		RenderedMarkdown: "# Requirement Analysis\n\napproved design context",
	})
	if len(plan.Code) != 2 || len(plan.Services) != 2 || len(plan.Runbooks) != 2 {
		t.Fatalf("unexpected query counts: code=%d services=%d runbooks=%d", len(plan.Code), len(plan.Services), len(plan.Runbooks))
	}
	if sumQueryLimits(plan.Code) != generationCodeLimit ||
		sumQueryLimits(plan.Services) != generationServiceLimit ||
		sumQueryLimits(plan.Runbooks) != generationRunbookLimit {
		t.Fatalf("unexpected plan limits: %#v", plan)
	}
	if plan.DependencyDepth != generationDependencyDepth || plan.DependencyLimit != generationServiceLimit {
		t.Fatalf("unexpected dependency bounds: %#v", plan)
	}
}

func TestCollectEvidenceEnforcesPlanBoundsAndDependencyConcurrency(t *testing.T) {
	source := &evidenceKnowledge{traceGate: make(chan struct{})}
	generator := &Generator{knowledge: source}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	evidence, err := generator.collectEvidence(ctx, Artifact{
		DocumentJSON:     json.RawMessage(`{"problem_statement":"add order export"}`),
		RenderedMarkdown: "# Requirement Analysis\n\napproved design context",
	})
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int, 4)
	for _, ref := range evidence {
		counts[ref.Kind]++
		if len(ref.Hash) != 64 {
			t.Fatalf("evidence hash is not SHA-256: %#v", ref)
		}
	}
	if counts["code"] != generationCodeLimit || counts["service"] != generationServiceLimit ||
		counts["runbook"] != generationRunbookLimit || counts["ontology_dependency"] != generationServiceLimit {
		t.Fatalf("unexpected evidence counts: %v", counts)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.codeQueries) != 2 || len(source.serviceQueries) != 2 || len(source.runbookQueries) != 2 {
		t.Fatalf("unexpected query calls: code=%d services=%d runbooks=%d", len(source.codeQueries), len(source.serviceQueries), len(source.runbookQueries))
	}
	if source.traceCalls != generationServiceLimit {
		t.Fatalf("dependency calls = %d", source.traceCalls)
	}
	if source.maxTraceActive != generationQueryConcurrency {
		t.Fatalf("maximum dependency concurrency = %d", source.maxTraceActive)
	}
}

func TestCollectEvidenceSkipsDependenciesWithoutServiceCandidates(t *testing.T) {
	source := &evidenceKnowledge{emptyServices: true}
	generator := &Generator{knowledge: source}
	if _, err := generator.collectEvidence(context.Background(), Artifact{}); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.traceCalls != 0 {
		t.Fatalf("dependency calls = %d", source.traceCalls)
	}
}

func TestGenerateRequirementAnalysisSkipsTechnicalEvidence(t *testing.T) {
	source := &evidenceKnowledge{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": `{
					"problem_statement":"Customers need export",
					"goals":["Allow customers to export"],
					"functional_requirements":["Customers can request an export"],
					"acceptance_criteria":["A requested export is available"]
				}`},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := llm.NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, server.Client())
	generator := NewGenerator(source, client, "openai", "model", 100)
	artifact, _, _, err := generator.Generate(
		context.Background(),
		"run-1",
		FeatureRequest{ID: "feat-1", Title: "Customer export"},
		Artifact{
			ID: "art-requirement", Kind: KindRequirement,
			DocumentJSON:     json.RawMessage(`{"description":"Customers need export"}`),
			RenderedMarkdown: "# Product Requirement\n\nCustomers need export.",
		},
		KindRequirementAnalysis,
		7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Evidence) != 0 {
		t.Fatalf("requirement analysis evidence = %#v", artifact.Evidence)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.codeQueries) != 0 || len(source.serviceQueries) != 0 ||
		len(source.runbookQueries) != 0 || source.traceCalls != 0 {
		t.Fatalf(
			"technical knowledge calls: code=%d services=%d runbooks=%d dependencies=%d",
			len(source.codeQueries), len(source.serviceQueries), len(source.runbookQueries), source.traceCalls,
		)
	}
}

func TestGenerateRepromptsOnUnknownDocumentField(t *testing.T) {
	responses := []string{
		`{
			"problem_statement":"Customers need export",
			"goals":["Allow customers to export"],
			"functional_requirements":["Customers can request an export"],
			"acceptance_criteria":["A requested export is available"],
			"technical_hint":"Add an export worker"
		}`,
		`{
			"problem_statement":"Customers need export",
			"goals":["Allow customers to export"],
			"functional_requirements":["Customers can request an export"],
			"acceptance_criteria":["A requested export is available"]
		}`,
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := responses[requests]
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	client := llm.NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, server.Client())
	generator := NewGenerator(nil, client, "openai", "model", 100)
	artifact, _, _, err := generator.Generate(
		context.Background(),
		"run-1",
		FeatureRequest{ID: "feat-1", Title: "Customer export"},
		Artifact{
			ID: "art-requirement", Kind: KindRequirement,
			DocumentJSON: json.RawMessage(`{"description":"Customers need export"}`),
		},
		KindRequirementAnalysis,
		7,
	)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if strings.Contains(string(artifact.DocumentJSON), "technical_hint") {
		t.Fatalf("unknown field survived generation: %s", artifact.DocumentJSON)
	}
}

func TestGenerationDocumentContractsMatchValidatedDocumentTypes(t *testing.T) {
	for _, kind := range []ArtifactKind{
		KindRequirementAnalysis,
		KindTechnicalProposal,
		KindSystemDesign,
		KindImplementationPlan,
	} {
		t.Run(string(kind), func(t *testing.T) {
			document := newDocument(kind)
			if err := json.Unmarshal([]byte(generationDocumentContract(kind)), document); err != nil {
				t.Fatalf("decode contract: %v", err)
			}
			if err := validateDocument(kind, document); err != nil {
				t.Fatalf("validate contract: %v", err)
			}
		})
	}
}

func TestGenerationPromptRequestsBareDocumentBody(t *testing.T) {
	prompt := generationPrompt(
		Artifact{
			ID:               "art-1",
			Kind:             KindRequirement,
			Version:          3,
			DocumentJSON:     json.RawMessage(`{"description":"Customers need export"}`),
			RenderedMarkdown: "# Requirement",
		},
		KindRequirementAnalysis,
		nil,
	)
	for _, expected := range []string{
		"Return only the document body",
		"Do not wrap it in artifact fields",
		`"problem_statement": "string"`,
		`"functional_requirements"`,
		`"acceptance_criteria"`,
		`"requirement":{"description":"Customers need export"}`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("generation prompt is missing %q", expected)
		}
	}
	for _, forbidden := range []string{
		`"evidence"`,
		`"claims"`,
		`"parent_artifact"`,
		`"feature"`,
		`"title"`,
		`"created_by"`,
		`"version"`,
		`"rendered_markdown"`,
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("requirement analysis prompt contains non-business field %q:\n%s", forbidden, prompt)
		}
	}
}

func TestGenerationStagesHaveDistinctRolesAndBoundaries(t *testing.T) {
	tests := []struct {
		kind     ArtifactKind
		role     string
		required []string
	}{
		{
			kind: KindRequirementAnalysis,
			role: "product manager",
			required: []string{
				"Do not choose architecture",
				"Provide the technical proposal stage",
			},
		},
		{
			kind: KindTechnicalProposal,
			role: "backend architect",
			required: []string{
				"at least two materially different, independently implementable candidates",
				"Do not produce file-by-file edits",
				"Provide the system design stage",
			},
		},
		{
			kind: KindSystemDesign,
			role: "software architect",
			required: []string{
				"treat its selected option, trade-offs, and obligations as binding",
				"Do not descend into repository paths, file changes",
				"Provide the implementation planning stage",
			},
		},
		{
			kind: KindImplementationPlan,
			role: "sprint prioritizer",
			required: []string{
				"the minimum evidence-backed repository set",
				"Do not redesign architecture",
				"Provide the minimal change engineer",
			},
		},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			system := generationSystemPrompt(test.kind)
			if !strings.Contains(system, test.role) {
				t.Fatalf("system prompt is missing role %q:\n%s", test.role, system)
			}
			for _, expected := range test.required {
				if !strings.Contains(system, expected) {
					t.Fatalf("system prompt is missing %q:\n%s", expected, system)
				}
			}
		})
	}
}

func TestGenerationPromptsCarryOnlyTheDirectParentArtifact(t *testing.T) {
	tests := []struct {
		kind   ArtifactKind
		parent ArtifactKind
	}{
		{KindRequirementAnalysis, KindRequirement},
		{KindTechnicalProposal, KindRequirementAnalysis},
		{KindSystemDesign, KindTechnicalProposal},
		{KindImplementationPlan, KindSystemDesign},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			parent := Artifact{
				ID: "direct-parent", Kind: test.parent, Version: 2,
				DocumentJSON:     json.RawMessage(`{"marker":"direct-parent-only"}`),
				RenderedMarkdown: "# direct-parent-only",
			}
			prompt := generationPrompt(
				parent, test.kind,
				[]EvidenceRef{{Kind: "code", Summary: "evidence"}},
			)
			if !strings.Contains(prompt, "direct-parent-only") {
				t.Fatalf("prompt does not contain its direct parent:\n%s", prompt)
			}
			if test.kind == KindRequirementAnalysis {
				for _, forbidden := range []string{`"parent_artifact"`, `"evidence"`} {
					if strings.Contains(prompt, forbidden) {
						t.Fatalf("requirement analysis prompt contains %s:\n%s", forbidden, prompt)
					}
				}
				return
			}
			inputLine := prompt[strings.LastIndex(prompt, "Input:\n")+len("Input:\n"):]
			if strings.Contains(inputLine, `"feature"`) || strings.Contains(inputLine, `"title"`) {
				t.Fatalf("input contains feature metadata outside the direct parent: %s", inputLine)
			}
			var input struct {
				Parent Artifact `json:"parent_artifact"`
			}
			if err := json.Unmarshal([]byte(inputLine), &input); err != nil {
				t.Fatalf("decode generation input: %v\n%s", err, inputLine)
			}
			if input.Parent.Kind != test.parent || input.Parent.ID != "direct-parent" {
				t.Fatalf("parent = %+v, want %s", input.Parent, test.parent)
			}
			for _, other := range []ArtifactKind{
				KindRequirement, KindRequirementAnalysis, KindTechnicalProposal, KindSystemDesign, KindImplementationPlan,
			} {
				if other != test.parent && strings.Contains(inputLine, `"kind":"`+string(other)+`"`) {
					t.Fatalf("input contains non-parent artifact kind %s: %s", other, inputLine)
				}
			}
		})
	}
}

func queryPrefix(query string) string {
	if strings.Contains(query, "\n") {
		return "context"
	}
	return "title"
}

func sumQueryLimits(queries []evidenceQuery) int {
	total := 0
	for _, query := range queries {
		total += query.Limit
	}
	return total
}
