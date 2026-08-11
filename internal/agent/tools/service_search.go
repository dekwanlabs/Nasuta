package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/platform"
)

func (srv *Service) AllServices(ctx context.Context) ([]domain.ServiceRecord, error) {
	return srv.db.AllServices(ctx)
}

func (srv *Service) ServiceModules(ctx context.Context, repos []string) ([]domain.ServiceRecord, error) {
	if len(repos) == 0 {
		return srv.db.AllServices(ctx)
	}
	return srv.db.ServicesByRepos(ctx, repos)
}

func (srv *Service) services(ctx context.Context) ([]domain.ServiceRecord, error) {
	if cached := srv.mergedSvcCache.Load(); cached != nil {
		return *cached, nil
	}
	all, err := srv.db.AllServices(ctx)
	if err != nil {
		return nil, err
	}
	srv.mergedSvcCache.Store(&all)
	return all, nil
}

func (srv *Service) ServiceLookup(ctx context.Context, query string, limit int) map[string]any {
	result, err := srv.ServiceLookupResult(ctx, query, limit)
	if err != nil {
		return map[string]any{"matches": nil, "semantic": false, "error": err.Error()}
	}
	return result
}

// ServiceLookupResult returns the service lookup payload without hiding backend failures.
func (srv *Service) ServiceLookupResult(ctx context.Context, query string, limit int) (map[string]any, error) {
	limit = clampInt(limit, 1, 100)
	result, err := srv.FindServices(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"matches": result.Matches, "semantic": result.Semantic}, nil
}

// FindServices returns typed service matches for internal consumers.
func (srv *Service) FindServices(ctx context.Context, query string, limit int) (domain.SearchResult[domain.ServiceRecord], error) {
	input := serviceSearchInput{Query: query, Limit: limit}
	return runtrace.Invoke(ctx, serviceSearchSpec, input, func(ctx context.Context, input serviceSearchInput) (domain.SearchResult[domain.ServiceRecord], error) {
		return srv.findServices(ctx, input, nil)
	})
}

func (srv *Service) FindServicesWithVector(ctx context.Context, query string, limit int, vector []float32) (domain.SearchResult[domain.ServiceRecord], error) {
	input := serviceSearchInput{Query: query, Limit: limit}
	return runtrace.Invoke(ctx, serviceSearchSpec, input, func(ctx context.Context, input serviceSearchInput) (domain.SearchResult[domain.ServiceRecord], error) {
		return srv.findServicesWithSharedVector(ctx, input, vector)
	})
}

type serviceSearchInput struct {
	Query string
	Limit int
}

var serviceSearchSpec = runtrace.Spec[serviceSearchInput, domain.SearchResult[domain.ServiceRecord]]{
	Operation: "knowledge.service_search",
	Node:      "service_search",
	Input: func(input serviceSearchInput) map[string]any {
		return map[string]any{"query": input.Query, "limit": input.Limit}
	},
	Output: func(_ serviceSearchInput, result domain.SearchResult[domain.ServiceRecord], err error) map[string]any {
		output := map[string]any{
			"matches":  len(result.Matches),
			"semantic": result.Semantic,
			"services": traceServiceNames(result.Matches),
		}
		if err != nil {
			output["error"] = err.Error()
		}
		return output
	},
}

func (srv *Service) findServices(ctx context.Context, input serviceSearchInput, vector []float32) (domain.SearchResult[domain.ServiceRecord], error) {
	all, err := srv.services(ctx)
	if err != nil {
		return domain.SearchResult[domain.ServiceRecord]{}, err
	}
	matches := scoreServices(all, input.Query, input.Limit)

	semanticSearch := false
	if srv.semanticEnabled() {
		var names []string
		if len(vector) > 0 {
			names, err = srv.semanticServiceNamesWithVector(ctx, input.Limit, vector)
		} else {
			names, err = srv.semanticServiceNames(ctx, input.Query, input.Limit)
		}
		if err != nil {
			return domain.SearchResult[domain.ServiceRecord]{}, fmt.Errorf("semantic service search: %w", err)
		}
		matches = mergeServiceMatches(names, all, matches, input.Limit)
		semanticSearch = true
	}
	return domain.SearchResult[domain.ServiceRecord]{Matches: matches, Semantic: semanticSearch}, nil
}

func (srv *Service) findServicesWithSharedVector(
	ctx context.Context,
	input serviceSearchInput,
	vector []float32,
) (domain.SearchResult[domain.ServiceRecord], error) {
	all, err := srv.services(ctx)
	if err != nil {
		return domain.SearchResult[domain.ServiceRecord]{}, err
	}
	matches := scoreServices(all, input.Query, input.Limit)
	if !srv.semanticEnabled() || len(vector) == 0 {
		return domain.SearchResult[domain.ServiceRecord]{Matches: matches}, nil
	}
	names, err := srv.semanticServiceNamesWithVector(ctx, input.Limit, vector)
	if err != nil {
		return domain.SearchResult[domain.ServiceRecord]{}, fmt.Errorf("semantic service search: %w", err)
	}
	return domain.SearchResult[domain.ServiceRecord]{
		Matches:  mergeServiceMatches(names, all, matches, input.Limit),
		Semantic: true,
	}, nil
}

func traceServiceNames(matches []domain.ServiceRecord) []string {
	limit := min(len(matches), 10)
	names := make([]string, 0, limit)
	for _, match := range matches[:limit] {
		names = append(names, match.ServiceName)
	}
	return names
}

func (srv *Service) semanticServiceNames(ctx context.Context, query string, limit int) ([]string, error) {
	vectors, err := srv.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("embed query: empty vector")
	}
	return srv.semanticServiceNamesWithVector(ctx, limit, vectors[0])
}

func (srv *Service) semanticServiceNamesWithVector(ctx context.Context, limit int, vector []float32) ([]string, error) {
	hits, err := srv.semantic.Search(ctx, semantic.Query{
		DenseVector: vector,
		Filter:      semantic.Filter{Keywords: map[string]string{"kind": "service"}},
		Limit:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("search service vectors: %w", err)
	}
	names := make([]string, 0, len(hits))
	for _, hit := range hits {
		if name, ok := hit.Metadata["service_name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// SearchServices exposes typed service search to extensions.
func (srv *Service) SearchServices(ctx context.Context, query knowledge.ServiceSearchQuery) (knowledge.ServiceSearchResult, error) {
	query.Limit = clampInt(query.Limit, 1, 100)
	found, err := srv.FindServices(ctx, query.Query, query.Limit)
	if err != nil {
		return knowledge.ServiceSearchResult{}, err
	}
	return toServiceSearchResult(found), nil
}

func toServiceSearchResult(found domain.SearchResult[domain.ServiceRecord]) knowledge.ServiceSearchResult {
	matches := make([]knowledge.ServiceRecord, 0, len(found.Matches))
	for _, service := range found.Matches {
		matches = append(matches, knowledge.ServiceRecord{
			ServiceName: service.ServiceName,
			Repo:        service.Repo,
			Layer:       service.Layer,
			Language:    service.Language,
			Owner:       service.Owner,
			Status:      service.Status,
			Summary:     service.Summary,
			Tags:        service.Tags,
			Docs:        service.Docs,
			Confidence:  service.Confidence,
		})
	}
	return knowledge.ServiceSearchResult{Matches: matches, Semantic: found.Semantic}
}

type scoredService struct {
	record domain.ServiceRecord
	score  int
}

func scoreServices(all []domain.ServiceRecord, query string, limit int) []domain.ServiceRecord {
	normalizedQuery := platform.Normalize(query)
	scored := make([]scoredService, 0, len(all))
	for _, service := range all {
		if score := scoreService(service, normalizedQuery); score > 0 {
			scored = append(scored, scoredService{record: service, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	count := min(len(scored), limit)
	matches := make([]domain.ServiceRecord, count)
	for index := range count {
		matches[index] = scored[index].record
	}
	return matches
}

func scoreService(service domain.ServiceRecord, normalizedQuery string) int {
	if platform.Normalize(service.ServiceName) == normalizedQuery {
		return 100
	}
	fields := []string{service.ServiceName, service.ModulePath, service.Layer, service.Owner}
	fields = append(fields, service.Tags...)
	fields = append(fields, service.Docs...)
	normalizedFields := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			normalizedFields = append(normalizedFields, platform.Normalize(field))
		}
	}
	for _, field := range normalizedFields {
		if field == normalizedQuery {
			return 80
		}
	}
	for _, field := range normalizedFields {
		if strings.Contains(field, normalizedQuery) {
			return 50
		}
	}
	if service.Summary != "" && strings.Contains(platform.Normalize(service.Summary), normalizedQuery) {
		return 20
	}
	return 0
}

func mergeServiceMatches(
	semanticNames []string,
	all []domain.ServiceRecord,
	base []domain.ServiceRecord,
	limit int,
) []domain.ServiceRecord {
	byName := make(map[string]domain.ServiceRecord, len(all))
	for _, service := range all {
		byName[service.ServiceName] = service
	}
	capacity := min(limit, len(base)+len(semanticNames))
	seen := make(map[string]struct{}, capacity)
	matches := make([]domain.ServiceRecord, 0, capacity)
	for _, service := range base {
		matches = append(matches, service)
		seen[service.ServiceName] = struct{}{}
	}
	for _, name := range semanticNames {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		if service, ok := byName[name]; ok {
			matches = append(matches, service)
			seen[name] = struct{}{}
		}
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}
