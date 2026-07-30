package featuredelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/knowledge"
	"golang.org/x/sync/errgroup"
)

const (
	generationCodeLimit        = 8
	generationServiceLimit     = 6
	generationRunbookLimit     = 4
	generationDependencyDepth  = 2
	generationQueryCount       = 2
	generationQueryConcurrency = 4
)

type evidenceQuery struct {
	Text  string
	Limit int
}

type evidenceQueryPlan struct {
	Code            []evidenceQuery
	Services        []evidenceQuery
	Runbooks        []evidenceQuery
	DependencyDepth int
	DependencyLimit int
}

type Generator struct {
	knowledge knowledge.API
	llm       *llm.LLMClient
	provider  string
	model     string
	maxTokens int
}

func NewGenerator(knowledgeAPI knowledge.API, client *llm.LLMClient, provider, model string, maxTokens int) *Generator {
	return &Generator{
		knowledge: knowledgeAPI,
		llm:       client,
		provider:  provider,
		model:     model,
		maxTokens: maxTokens,
	}
}

func (generator *Generator) Enabled() bool {
	return generator != nil && generator.llm != nil && generator.model != ""
}

func (generator *Generator) Generate(ctx context.Context, runID string, feature FeatureRequest, parent Artifact, kind ArtifactKind, createdBy int64) (Artifact, int64, int64, error) {
	if !generator.Enabled() {
		return Artifact{}, 0, 0, ErrUnavailable
	}
	var (
		evidence []EvidenceRef
		err      error
	)
	if kind != KindRequirementAnalysis {
		evidence, err = generator.collectEvidence(ctx, parent)
		if err != nil {
			return Artifact{}, 0, 0, err
		}
	}
	request := generationPrompt(parent, kind, evidence)
	document := newDocument(kind)
	if document == nil {
		return Artifact{}, 0, 0, fmt.Errorf("unsupported generated artifact kind %q", kind)
	}
	usage := &generationUsage{}
	callCtx := llm.WithUsageRecorder(ctx, runID, usage)
	err = generator.llm.ChatJSON(callCtx, generationSystemPrompt(kind), request, document, llm.CallOptions{
		MaxTokens:             generator.maxTokens,
		DisallowUnknownFields: true,
		Validate: func(parsed any) error {
			return validateDocument(kind, parsed)
		},
	})
	if err != nil {
		return Artifact{}, usage.input, usage.output, fmt.Errorf("generate %s: %w", kind, err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return Artifact{}, usage.input, usage.output, fmt.Errorf("marshal generated %s: %w", kind, err)
	}
	artifact, err := BuildArtifact(kind, feature.ID, parent.ID, OriginAgent, raw, evidence, createdBy)
	if err != nil {
		return Artifact{}, usage.input, usage.output, err
	}
	return artifact, usage.input, usage.output, nil
}

func (generator *Generator) collectEvidence(ctx context.Context, parent Artifact) ([]EvidenceRef, error) {
	if generator.knowledge == nil {
		return nil, nil
	}
	plan := buildEvidenceQueryPlan(parent)
	code := make([]knowledge.CodeSearchResult, len(plan.Code))
	services := make([]knowledge.ServiceSearchResult, len(plan.Services))
	runbooks := make([]knowledge.RunbookSearchResult, len(plan.Runbooks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(generationQueryConcurrency)
	for index, query := range plan.Code {
		index, query := index, query
		group.Go(func() error {
			value, err := generator.knowledge.SearchCode(groupCtx, knowledge.CodeSearchQuery{Query: query.Text, Limit: query.Limit})
			if err != nil {
				return fmt.Errorf("search code for %q: %w", query.Text, err)
			}
			code[index] = value
			return nil
		})
	}
	for index, query := range plan.Services {
		index, query := index, query
		group.Go(func() error {
			value, err := generator.knowledge.SearchServices(groupCtx, knowledge.ServiceSearchQuery{Query: query.Text, Limit: query.Limit})
			if err != nil {
				return fmt.Errorf("search services for %q: %w", query.Text, err)
			}
			services[index] = value
			return nil
		})
	}
	for index, query := range plan.Runbooks {
		index, query := index, query
		group.Go(func() error {
			value, err := generator.knowledge.SearchRunbooks(groupCtx, knowledge.RunbookQuery{Query: query.Text, Limit: query.Limit})
			if err != nil {
				return fmt.Errorf("search runbooks for %q: %w", query.Text, err)
			}
			runbooks[index] = value
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("collect feature evidence: %w", err)
	}

	evidence := make([]EvidenceRef, 0, generationCodeLimit+generationServiceLimit+generationRunbookLimit+generationServiceLimit)
	seen := make(map[string]struct{}, maxEvidenceRefs)
	appendEvidence := func(ref EvidenceRef) {
		ref.Summary = truncateText(ref.Summary, maxEvidenceText)
		if ref.Summary == "" {
			return
		}
		key := ref.Kind + "\x00" + ref.Repo + "\x00" + ref.Path + "\x00" + ref.Service + "\x00" + ref.Summary
		if _, ok := seen[key]; ok || len(evidence) >= maxEvidenceRefs {
			return
		}
		seen[key] = struct{}{}
		sum := sha256.Sum256([]byte(key))
		ref.Hash = hex.EncodeToString(sum[:])
		evidence = append(evidence, ref)
	}
	for index, result := range code {
		for _, hit := range boundedSlice(result.Matches, plan.Code[index].Limit) {
			appendEvidence(EvidenceRef{
				Kind: "code", Repo: hit.Repo, Path: hit.Path,
				StartLine: hit.StartLine, EndLine: hit.EndLine, Summary: hit.Preview,
			})
		}
	}
	serviceCandidates := make([]string, 0, plan.DependencyLimit)
	seenServices := make(map[string]struct{}, plan.DependencyLimit)
	for index, result := range services {
		for _, service := range boundedSlice(result.Matches, plan.Services[index].Limit) {
			appendEvidence(EvidenceRef{
				Kind: "service", Repo: service.Repo, Service: service.ServiceName, Summary: service.Summary,
			})
			name := strings.TrimSpace(service.ServiceName)
			if name == "" || len(serviceCandidates) >= plan.DependencyLimit {
				continue
			}
			if _, ok := seenServices[name]; ok {
				continue
			}
			seenServices[name] = struct{}{}
			serviceCandidates = append(serviceCandidates, name)
		}
	}
	for index, result := range runbooks {
		for _, hit := range boundedSlice(result.Matches, plan.Runbooks[index].Limit) {
			for _, chunk := range hit.Chunks {
				appendEvidence(EvidenceRef{
					Kind: "runbook", Path: hit.Path,
					Summary: strings.TrimSpace(chunk.SectionHeader + "\n" + chunk.ChunkText),
				})
			}
		}
	}

	dependencies := make([]knowledge.DependencyResult, len(serviceCandidates))
	dependencyGroup, dependencyCtx := errgroup.WithContext(ctx)
	dependencyGroup.SetLimit(generationQueryConcurrency)
	for index, service := range serviceCandidates {
		index, service := index, service
		dependencyGroup.Go(func() error {
			result, err := generator.knowledge.TraceDependencies(dependencyCtx, knowledge.DependencyQuery{
				Service: service, Direction: "both", Depth: plan.DependencyDepth,
			})
			if err != nil {
				return fmt.Errorf("trace ontology dependencies for %q: %w", service, err)
			}
			dependencies[index] = result
			return nil
		})
	}
	if err := dependencyGroup.Wait(); err != nil {
		return nil, err
	}
	for index, result := range dependencies {
		appendDependencyEvidence := func(edge knowledge.DependencyEdge) {
			appendEvidence(EvidenceRef{
				Kind: "ontology_dependency", Service: serviceCandidates[index],
				Summary: fmt.Sprintf("%s -> %s (%s, confidence %.2f)", edge.From, edge.To, edge.Type, edge.Confidence),
			})
		}
		for _, edge := range result.Upstream {
			appendDependencyEvidence(edge)
		}
		for _, edge := range result.Downstream {
			appendDependencyEvidence(edge)
		}
	}
	return evidence, nil
}

func buildEvidenceQueryPlan(parent Artifact) evidenceQueryPlan {
	documentText := truncateText(string(parent.DocumentJSON), 4000)
	renderedText := truncateText(parent.RenderedMarkdown, 4000)
	queries := make([]string, 0, generationQueryCount)
	seen := make(map[string]struct{}, generationQueryCount)
	for _, query := range []string{documentText, renderedText} {
		if query == "" {
			continue
		}
		if _, ok := seen[query]; ok {
			continue
		}
		seen[query] = struct{}{}
		queries = append(queries, query)
		if len(queries) == generationQueryCount {
			break
		}
	}
	return evidenceQueryPlan{
		Code:            allocateEvidenceQueries(queries, generationCodeLimit),
		Services:        allocateEvidenceQueries(queries, generationServiceLimit),
		Runbooks:        allocateEvidenceQueries(queries, generationRunbookLimit),
		DependencyDepth: generationDependencyDepth,
		DependencyLimit: generationServiceLimit,
	}
}

func allocateEvidenceQueries(queries []string, totalLimit int) []evidenceQuery {
	if len(queries) == 0 || totalLimit <= 0 {
		return nil
	}
	if len(queries) > totalLimit {
		queries = queries[:totalLimit]
	}
	out := make([]evidenceQuery, len(queries))
	base, remainder := totalLimit/len(queries), totalLimit%len(queries)
	for index, query := range queries {
		limit := base
		if index < remainder {
			limit++
		}
		out[index] = evidenceQuery{Text: query, Limit: limit}
	}
	return out
}

func boundedSlice[T any](items []T, limit int) []T {
	if limit < 0 {
		return nil
	}
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func newDocument(kind ArtifactKind) any {
	switch kind {
	case KindRequirementAnalysis:
		return &RequirementAnalysisDocument{}
	case KindTechnicalProposal:
		return &TechnicalProposalDocument{}
	case KindSystemDesign:
		return &SystemDesignDocument{}
	case KindImplementationPlan:
		return &ImplementationPlanDocument{}
	default:
		return nil
	}
}

type generationUsage struct {
	mu     sync.Mutex
	input  int64
	output int64
}

func (usage *generationUsage) RecordLLMCall(_ context.Context, call llm.CallUsage) error {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	usage.input += int64(call.Usage.InputTokens)
	usage.output += int64(call.Usage.OutputTokens)
	return nil
}

func truncateText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
