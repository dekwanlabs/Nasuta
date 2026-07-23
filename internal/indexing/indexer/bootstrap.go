package indexer

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// ScanCode runs every structural code scanner and publishes canonical records.
func ScanCode(root string, dirs []string) domain.IndexBundle {
	services := append(scanJavaServices(root, dirs), scanPythonServices(root, dirs)...)
	services = append(services, scanGoServices(root, dirs)...)
	services = append(services, scanKotlinServices(root, dirs)...)
	services = append(services, scanCSharpServices(root, dirs)...)
	services = append(services, scanNodeJSServices(root, dirs)...)
	services = append(services, scanAndroidServices(root, dirs)...)
	services = append(services, scanIOSServices(root, dirs)...)

	endpoints := append(scanJavaEndpoints(root, dirs), scanPythonEndpoints(root, dirs)...)
	endpoints = append(endpoints, scanGoEndpoints(root, dirs)...)
	endpoints = append(endpoints, scanKotlinEndpoints(root, dirs)...)
	endpoints = append(endpoints, scanCSharpEndpoints(root, dirs)...)
	endpoints = append(endpoints, scanNodeJSEndpoints(root, dirs)...)

	deps := append(scanFeignClients(root, dirs), scanKotlinFeigns(root, dirs)...)
	deps = append(deps, scanCSharpRefits(root, dirs)...)
	deps = append(deps, scanGoDependencies(root, dirs)...)
	deps = append(deps, scanCSharpDependencies(root, dirs)...)
	deps = append(deps, scanNodeJSDependencies(root, dirs)...)
	deps = append(deps, scanAndroidDependencies(root, dirs)...)
	deps = append(deps, scanIOSDependencies(root, dirs)...)
	deps = append(deps, scanJVMAndPythonDependencies(root, dirs)...)
	deps = append(deps, scanKafkaDependencies(root, dirs)...)

	return CanonicalizeBundle(domain.IndexBundle{
		Services: services, Endpoints: endpoints, Dependencies: deps,
	})
}

// BuildStructuralBundle builds only the SQLite structural snapshot.
func BuildStructuralBundle(root string, dirs []string) domain.IndexBundle {
	return ScanCode(root, dirs)
}

// BuildBundle builds structural records and separately sourced runbook records.
func BuildBundle(root string, dirs []string, docStore *store.DocStore) (domain.IndexBundle, error) {
	var runbooks []domain.RunbookRecord
	var runbookEdges []domain.DependencyEdge
	if docStore != nil {
		var err error
		runbooks, runbookEdges, err = LoadKnowledgeBase(docStore)
		if err != nil {
			return domain.IndexBundle{}, fmt.Errorf("load knowledge base: %w", err)
		}
	}
	log.Infof("[indexer] loaded %d KB docs from DocStore", len(runbooks))
	code := ScanCode(root, dirs)
	code.Dependencies = append(code.Dependencies, runbookEdges...)
	code.Runbooks = runbooks
	return CanonicalizeBundle(code), nil
}

// ScanRepo builds the structural snapshot for one repository.
func ScanRepo(root, repo string) domain.IndexBundle {
	b := ScanCode(root, []string{repo})
	b.Repo = repoKey(repo)
	return CanonicalizeBundle(b)
}

func repoKey(path string) string {
	p := strings.Trim(filepath.ToSlash(path), "/")
	if strings.HasPrefix(p, "repos/") {
		return repoFromPath(p)
	}
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return p
}

// CanonicalizeBundle establishes the complete structural-store write invariant.
func CanonicalizeBundle(bundle domain.IndexBundle) domain.IndexBundle {
	services := make([]domain.ServiceRecord, 0, len(bundle.Services))
	for _, service := range bundle.Services {
		service = canonicalService(service)
		if service.ServiceName == "" {
			continue
		}
		services = append(services, service)
	}

	for i := range services {
		if services[i].ModulePath == "" {
			services[i].ModulePath = "."
		}
		services[i].ServiceKey = platform.UUIDFromString(services[i].Repo + "\x00" + services[i].ModulePath)
		services[i].Tags = nonNil(platform.Dedupe(services[i].Tags))
		services[i].Docs = nonNil(platform.Dedupe(services[i].Docs))
		services[i].SourceOfTruth = nonNil(platform.Dedupe(services[i].SourceOfTruth))
		services[i].Entrypoints = dedupeEvidence(services[i].Entrypoints)
		services[i].Ports = nonNil(platform.Dedupe(services[i].Ports))
	}
	services = domain.MergeServices(services)

	lookup := newServiceLookup(services)
	endpoints := canonicalEndpoints(bundle.Endpoints, lookup)
	dependencies := canonicalDependencies(bundle.Dependencies, lookup)
	repositories := canonicalRepositories(bundle.Repositories)
	runbooks := canonicalRunbooks(bundle.Runbooks)

	bundle.Repositories = repositories
	bundle.Services = services
	bundle.Endpoints = endpoints
	bundle.Dependencies = dependencies
	bundle.Runbooks = runbooks
	return bundle
}

func canonicalRunbooks(records []domain.RunbookRecord) []domain.RunbookRecord {
	out := make([]domain.RunbookRecord, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		record.ID = strings.TrimSpace(record.ID)
		record.Repo = canonicalRepo(record.Repo)
		record.Title = strings.TrimSpace(record.Title)
		record.Path = canonicalPath(record.Path)
		record.Scope = strings.TrimSpace(record.Scope)
		record.ServiceName = strings.TrimSpace(record.ServiceName)
		tags := make([]string, 0, len(record.Tags))
		for _, tag := range record.Tags {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags = append(tags, tag)
			}
		}
		record.Tags = nonNil(platform.Dedupe(tags))
		key := record.Repo + "\x00" + record.ID
		if record.ID == "" || record.Path == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, record)
	}
	return out
}

func canonicalService(service domain.ServiceRecord) domain.ServiceRecord {
	service.ServiceName = strings.TrimSpace(service.ServiceName)
	service.Repo = canonicalRepo(service.Repo)
	module := canonicalPath(service.ModulePath)
	if repo, rel, ok := splitRepoPath(module); ok {
		service.Repo = repo
		module = rel
	} else if service.Repo != "" {
		module = strings.TrimPrefix(module, service.Repo+"/")
	}
	if module == "" {
		module = "."
	}
	service.ModulePath = module
	for i := range service.SourceOfTruth {
		service.SourceOfTruth[i] = canonicalPath(service.SourceOfTruth[i])
	}
	for i := range service.Docs {
		service.Docs[i] = canonicalPath(service.Docs[i])
	}
	service.Entrypoints = canonicalEvidence(service.Entrypoints)
	return service
}

func canonicalEndpoints(records []domain.EndpointRecord, lookup serviceLookup) []domain.EndpointRecord {
	byKey := make(map[string]int, len(records))
	out := make([]domain.EndpointRecord, 0, len(records))
	for _, endpoint := range records {
		endpoint.File = canonicalPath(endpoint.File)
		service, ok := lookup.resolve(endpoint.ServiceName, endpoint.Repo, endpoint.File)
		if !ok {
			log.Warnf("[indexer] drop endpoint with unresolved service: %s %s (%s)", endpoint.Method, endpoint.Path, endpoint.ServiceName)
			continue
		}
		endpoint.ServiceKey = service.ServiceKey
		endpoint.ServiceName = service.ServiceName
		endpoint.Repo = service.Repo
		endpoint.Method = strings.ToUpper(strings.TrimSpace(endpoint.Method))
		endpoint.Path = strings.TrimSpace(endpoint.Path)
		if endpoint.Source == "" {
			endpoint.Source = domain.SourceCodeScan
		}
		if endpoint.Path == "" {
			endpoint.Path = "/"
		} else if !strings.HasPrefix(endpoint.Path, "/") {
			endpoint.Path = "/" + endpoint.Path
		}
		key := endpoint.ServiceKey + "\x00" + endpoint.Method + "\x00" + endpoint.Path
		if existing, found := byKey[key]; found {
			if endpoint.Confidence > out[existing].Confidence {
				out[existing] = endpoint
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, endpoint)
	}
	return out
}

func canonicalDependencies(records []domain.DependencyEdge, lookup serviceLookup) []domain.DependencyEdge {
	byKey := make(map[string]int, len(records))
	out := make([]domain.DependencyEdge, 0, len(records))
	for _, edge := range records {
		edge.From = strings.TrimSpace(edge.From)
		edge.To = strings.TrimSpace(edge.To)
		edge.Evidence = dedupeEvidence(canonicalEvidence(edge.Evidence))
		path := ""
		if len(edge.Evidence) > 0 {
			path = edge.Evidence[0].Path
		}
		caller, ok := lookup.resolve(edge.From, "", path)
		if !ok {
			log.Warnf("[indexer] drop dependency with unresolved caller: %s -> %s", edge.From, edge.To)
			continue
		}
		edge.CallerServiceKey = caller.ServiceKey
		edge.From = caller.ServiceName
		if target, found := lookup.resolve(edge.To, "", ""); found {
			edge.TargetKind = domain.DependencyTargetService
			edge.TargetServiceKey = target.ServiceKey
			edge.ExternalTarget = ""
			edge.To = target.ServiceName
		} else {
			edge.TargetKind = domain.DependencyTargetExternal
			edge.TargetServiceKey = ""
			edge.ExternalTarget = strings.ToLower(edge.To)
		}
		targetRef := edge.TargetServiceKey
		if edge.TargetKind == domain.DependencyTargetExternal {
			targetRef = edge.ExternalTarget
		}
		key := edge.CallerServiceKey + "\x00" + string(edge.TargetKind) + "\x00" + targetRef + "\x00" + string(edge.Type)
		if existing, found := byKey[key]; found {
			out[existing].Evidence = dedupeEvidence(append(out[existing].Evidence, edge.Evidence...))
			out[existing].Confidence = max(out[existing].Confidence, edge.Confidence)
			continue
		}
		byKey[key] = len(out)
		out = append(out, edge)
	}
	return out
}

type serviceLookup struct {
	byName map[string][]domain.ServiceRecord
}

func newServiceLookup(services []domain.ServiceRecord) serviceLookup {
	byName := make(map[string][]domain.ServiceRecord, len(services))
	for _, service := range services {
		name := platform.Normalize(service.ServiceName)
		byName[name] = append(byName[name], service)
	}
	return serviceLookup{byName: byName}
}

func (lookup serviceLookup) resolve(name, repo, file string) (domain.ServiceRecord, bool) {
	candidates := lookup.byName[platform.Normalize(strings.TrimSpace(name))]
	if len(candidates) == 1 {
		return candidates[0], true
	}
	file = canonicalPath(file)
	repo = canonicalRepo(repo)
	best := -1
	bestPrefix := ""
	for i, candidate := range candidates {
		if repo != "" && candidate.Repo != repo {
			continue
		}
		prefix := "repos/" + candidate.Repo
		if candidate.ModulePath != "." {
			prefix += "/" + candidate.ModulePath
		}
		if file != "" && !pathHasPrefix(file, prefix) {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			best, bestPrefix = i, prefix
		}
	}
	if best >= 0 {
		return candidates[best], true
	}
	return domain.ServiceRecord{}, false
}

func canonicalRepositories(records []domain.RepositoryRecord) []domain.RepositoryRecord {
	seen := make(map[string]int, len(records))
	out := make([]domain.RepositoryRecord, 0, len(records))
	for _, record := range records {
		record.Repo = canonicalRepo(record.Repo)
		if record.Repo == "" {
			continue
		}
		if i, ok := seen[record.Repo]; ok {
			out[i] = record
			continue
		}
		seen[record.Repo] = len(out)
		out = append(out, record)
	}
	return out
}

func canonicalEvidence(evidence []domain.Evidence) []domain.Evidence {
	for i := range evidence {
		evidence[i].Path = canonicalPath(evidence[i].Path)
		evidence[i].Symbol = strings.TrimSpace(evidence[i].Symbol)
		if evidence[i].Kind == "" {
			evidence[i].Kind = domain.SourceCodeScan
		}
	}
	return evidence
}

func dedupeEvidence(evidence []domain.Evidence) []domain.Evidence {
	out := make([]domain.Evidence, 0, len(evidence))
	seen := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		key := item.Path + "\x00" + strconv.Itoa(item.Line) + "\x00" + item.Symbol + "\x00" + string(item.Kind)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func canonicalRepo(repo string) string {
	repo = canonicalPath(repo)
	repo = strings.TrimPrefix(repo, "repos/")
	parts := strings.Split(repo, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return repo
}

func canonicalPath(path string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "workspace/")
	return strings.Trim(path, "/")
}

func splitRepoPath(path string) (repo, relative string, ok bool) {
	path = canonicalPath(path)
	var found bool
	path, found = strings.CutPrefix(path, "repos/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	repo = parts[0] + "/" + parts[1]
	relative = strings.Join(parts[2:], "/")
	if relative == "" {
		relative = "."
	}
	return repo, relative, true
}

func pathHasPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
