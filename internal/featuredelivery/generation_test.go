package featuredelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
	plan := buildEvidenceQueryPlan(
		FeatureRequest{Title: "add order export"},
		Artifact{RenderedMarkdown: "approved design context"},
	)
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
	evidence, err := generator.collectEvidence(ctx,
		FeatureRequest{Title: "add order export"},
		Artifact{RenderedMarkdown: "approved design context"},
	)
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
	if _, err := generator.collectEvidence(context.Background(), FeatureRequest{Title: "task"}, Artifact{}); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.traceCalls != 0 {
		t.Fatalf("dependency calls = %d", source.traceCalls)
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
	prompt := generationPrompt(FeatureRequest{Title: "feature"}, Artifact{}, KindRequirementAnalysis, nil)
	for _, expected := range []string{
		"Return only the document body",
		"Do not wrap it in artifact fields",
		`"background": "string"`,
		`"functional_requirements"`,
		`"acceptance_criteria"`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("generation prompt is missing %q", expected)
		}
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
