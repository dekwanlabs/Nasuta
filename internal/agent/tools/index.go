package tools

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func (srv *Service) ListApis(ctx context.Context, service, keyword string, limit int) map[string]any {
	result, err := srv.ListApisResult(ctx, service, keyword, limit)
	if err != nil {
		return map[string]any{"matches": nil, "error": err.Error()}
	}
	return result
}

// ListApisResult returns indexed APIs without hiding storage failures.
func (srv *Service) ListApisResult(ctx context.Context, service, keyword string, limit int) (map[string]any, error) {
	limit = clampInt(limit, 1, 100)
	matches, err := srv.FindAPIs(ctx, service, keyword, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"matches": matches}, nil
}

// FindAPIs returns typed endpoint records for internal consumers.
func (srv *Service) FindAPIs(ctx context.Context, service, keyword string, limit int) ([]domain.EndpointRecord, error) {
	page, err := srv.db.ListApis(ctx, service, keyword, 1, limit)
	if err != nil {
		return nil, err
	}
	if page == nil {
		return nil, fmt.Errorf("list APIs: store returned nil page")
	}
	return page.List, nil
}

func (srv *Service) DocGapCheck(ctx context.Context, serviceName string) map[string]any {
	result, err := srv.DocGapCheckResult(ctx, serviceName)
	if err != nil {
		return map[string]any{"service": serviceName, "found": false, "error": err.Error()}
	}
	return result
}

// DocGapCheckResult reports documentation gaps without folding store failures into data.
func (srv *Service) DocGapCheckResult(ctx context.Context, serviceName string) (map[string]any, error) {
	all, err := srv.services(ctx)
	if err != nil {
		return nil, err
	}
	var service domain.ServiceRecord
	found := false
	for _, candidate := range all {
		if candidate.ServiceName == serviceName {
			service = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("service_not_found: service %q", serviceName)
	}
	endpoints, err := srv.db.EndpointCountFor(ctx, service.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("endpoint count for %q: %w", service.ServiceName, err)
	}
	outgoing, err := srv.db.OutgoingCountFor(ctx, service.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("outgoing dependency count for %q: %w", service.ServiceName, err)
	}

	missing := make([]string, 0, 5)
	if len(service.Docs) == 0 {
		missing = append(missing, "service-doc")
	}
	if len(service.Entrypoints) == 0 {
		missing = append(missing, "entrypoints")
	}
	if endpoints == 0 {
		missing = append(missing, "endpoints")
	}
	if outgoing == 0 {
		missing = append(missing, "dependencies")
	}
	if len(service.SourceOfTruth) == 0 {
		missing = append(missing, "source_of_truth")
	}
	return map[string]any{
		"service": service.ServiceName,
		"found":   true,
		"missing": missing,
		"counts": map[string]int{
			"docs":                 len(service.Docs),
			"entrypoints":          len(service.Entrypoints),
			"endpoints":            endpoints,
			"outgoingDependencies": outgoing,
		},
	}, nil
}

func (srv *Service) IndexSummary(ctx context.Context) map[string]any {
	result, err := srv.IndexSummaryResult(ctx)
	if err != nil {
		return map[string]any{
			"services": 0, "endpoints": 0, "dependencies": 0, "runbooks": 0, "repos": 0,
			"semanticEnabled": srv.semanticEnabled(), "error": err.Error(),
		}
	}
	return result
}

// IndexSummaryResult returns index health without hiding configured backend failures.
func (srv *Service) IndexSummaryResult(ctx context.Context) (map[string]any, error) {
	summary, err := srv.db.Summary(ctx)
	if err != nil {
		return nil, err
	}
	runbooks, err := srv.runbookCount()
	if err != nil {
		return nil, fmt.Errorf("count runbooks: %w", err)
	}
	result := map[string]any{
		"services":        summary.Services,
		"endpoints":       summary.Endpoints,
		"dependencies":    summary.Dependencies,
		"runbooks":        runbooks,
		"repos":           summary.Repos,
		"semanticEnabled": srv.semanticEnabled(),
	}
	if srv.docStore == nil {
		result["runbookIndex"] = map[string]any{"enabled": false, "status": "unavailable"}
	} else {
		result["runbookIndex"] = map[string]any{"enabled": true, "status": "available"}
	}
	if srv.ontology == nil {
		result["ontology"] = map[string]any{"enabled": false, "status": "unavailable"}
		return result, nil
	}
	stats, err := srv.ontology.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("ontology stats: %w", err)
	}
	result["ontology"] = map[string]any{
		"enabled": true, "status": "available", "generation": stats.Generation,
		"entities": stats.Entities, "facts": stats.Facts, "evidence": stats.Evidence,
	}
	return result, nil
}
