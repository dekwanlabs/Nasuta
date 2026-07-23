package ontology

import (
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

func Project(bundle domain.IndexBundle) (Snapshot, error) {
	build := newBuilder()
	services := make(map[string]domain.ServiceRecord, len(bundle.Services))
	serviceIDsByAlias := make(map[string][]string, len(bundle.Services))

	for _, repository := range bundle.Repositories {
		build.addEntity(repositoryEntity(repository))
	}
	for _, service := range bundle.Services {
		services[service.ServiceKey] = service
		alias := normalizedAlias(service.ServiceName)
		serviceIDsByAlias[alias] = append(serviceIDsByAlias[alias], service.ServiceKey)
		build.addEntity(serviceEntity(service))
		build.addFact(Fact{
			SubjectID: RepositoryID(service.Repo), Predicate: PredicateContains,
			ObjectID: service.ServiceKey, Confidence: service.Confidence,
		})
	}
	for _, endpoint := range bundle.Endpoints {
		service := services[endpoint.ServiceKey]
		entity := endpointEntity(endpoint)
		build.addEntity(entity)
		build.addFact(Fact{
			SubjectID: endpoint.ServiceKey, Predicate: PredicateExposes, ObjectID: entity.ID,
			Confidence: endpoint.Confidence, Evidence: []Evidence{endpointEvidence(endpoint)},
		})
		if endpoint.HandlerMethod != "" {
			symbol := symbolEntity(service, endpoint)
			build.addEntity(symbol)
			build.addFact(Fact{
				SubjectID: entity.ID, Predicate: PredicateImplementedBy, ObjectID: symbol.ID,
				Confidence: endpoint.Confidence, Evidence: []Evidence{endpointEvidence(endpoint)},
			})
		}
	}
	for _, dependency := range bundle.Dependencies {
		objectID := dependency.TargetServiceKey
		if dependency.TargetKind == domain.DependencyTargetExternal {
			external := externalSystemEntity(dependency.ExternalTarget, dependency)
			build.addEntity(external)
			objectID = external.ID
		}
		build.addFact(Fact{
			SubjectID: dependency.CallerServiceKey, Predicate: PredicateDependsOn, ObjectID: objectID,
			Qualifiers: map[string]string{"protocol": string(dependency.Type)}, Confidence: dependency.Confidence,
			Evidence: ontologyEvidence(dependency.Evidence),
		})
	}
	for _, runbook := range bundle.Runbooks {
		entity := runbookEntity(runbook)
		build.addEntity(entity)
		ids := serviceIDsByAlias[normalizedAlias(runbook.ServiceName)]
		if runbook.ServiceName == "" || len(ids) != 1 {
			continue
		}
		build.addFact(Fact{
			SubjectID: ids[0], Predicate: PredicateDocumentedBy, ObjectID: entity.ID,
			Qualifiers: map[string]string{"scope": runbook.Scope}, Confidence: runbook.Confidence,
			Evidence: []Evidence{{Path: runbook.Path, Source: EvidenceSourceDoc}},
		})
	}
	return build.snapshot()
}

func repositoryEntity(record domain.RepositoryRecord) Entity {
	return Entity{
		ID: RepositoryID(record.Repo), Class: ClassRepository, Key: record.Repo, Name: record.Repo,
		Properties: map[string]string{"repo": record.Repo, "head_sha": record.HeadSHA}, Confidence: 1,
	}
}

func serviceEntity(record domain.ServiceRecord) Entity {
	aliases := []string{normalizedAlias(record.ServiceName), normalizedAlias(record.Repo + "/" + record.ServiceName)}
	return Entity{
		ID: record.ServiceKey, Class: ClassService, Key: record.ServiceKey, Name: record.ServiceName,
		Properties: map[string]string{
			"repo": record.Repo, "module_path": record.ModulePath, "language": record.Language,
			"owner": record.Owner, "runtime": record.Runtime,
		},
		Aliases: aliases, Confidence: record.Confidence,
	}
}

func endpointEntity(record domain.EndpointRecord) Entity {
	key := strings.Join([]string{record.ServiceKey, record.Method, record.Path}, "\x00")
	return Entity{
		ID: APIEndpointID(record.ServiceKey, record.Method, record.Path), Class: ClassAPIEndpoint,
		Key: key, Name: record.Method + " " + record.Path,
		Properties: map[string]string{
			"method": record.Method, "path": record.Path, "file": record.File, "handler": record.HandlerMethod,
		},
		Aliases: []string{normalizedAlias(record.Method + " " + record.Path)}, Confidence: record.Confidence,
	}
}

func symbolEntity(service domain.ServiceRecord, endpoint domain.EndpointRecord) Entity {
	key := strings.Join([]string{endpoint.Repo, endpoint.File, endpoint.HandlerMethod}, "\x00")
	return Entity{
		ID: CodeSymbolID(endpoint.Repo, endpoint.File, endpoint.HandlerMethod), Class: ClassCodeSymbol,
		Key: key, Name: endpoint.HandlerMethod,
		Properties: map[string]string{
			"repo": endpoint.Repo, "file": endpoint.File, "qualified_name": endpoint.HandlerMethod, "language": service.Language,
		},
		Aliases: []string{normalizedAlias(endpoint.HandlerMethod)}, Confidence: endpoint.Confidence,
	}
}

func externalSystemEntity(target string, dependency domain.DependencyEdge) Entity {
	return Entity{
		ID: ExternalSystemID(target), Class: ClassExternalSystem, Key: target, Name: target,
		Properties: map[string]string{"target": target},
		Aliases:    []string{normalizedAlias(target)}, Confidence: dependency.Confidence,
	}
}

func runbookEntity(record domain.RunbookRecord) Entity {
	key := record.Repo + "\x00" + record.ID
	return Entity{
		ID: RunbookID(record.Repo, record.ID), Class: ClassRunbook, Key: key, Name: record.Title,
		Properties: map[string]string{
			"repo": record.Repo, "title": record.Title, "path": record.Path,
			"scope": record.Scope, "tags": strings.Join(record.Tags, ","),
		},
		Aliases: []string{normalizedAlias(record.Title)}, Confidence: record.Confidence,
	}
}

func endpointEvidence(record domain.EndpointRecord) Evidence {
	return Evidence{Path: record.File, Line: record.Line, Symbol: record.HandlerMethod, Source: EvidenceSource(record.Source)}
}

func ontologyEvidence(records []domain.Evidence) []Evidence {
	out := make([]Evidence, 0, len(records))
	for _, record := range records {
		out = append(out, Evidence{Path: record.Path, Line: record.Line, Symbol: record.Symbol, Source: EvidenceSource(record.Kind)})
	}
	return out
}

func normalizedAlias(value string) string {
	return platform.Normalize(value)
}
