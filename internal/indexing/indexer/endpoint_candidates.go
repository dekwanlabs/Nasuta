package indexer

import (
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

type valueKind uint8

const (
	valueUnresolved valueKind = iota
	valueLiteral
	valueConfigReference
)

type valueExpr struct {
	kind  valueKind
	raw   string
	value string
}

func literalValue(value string) valueExpr {
	return valueExpr{kind: valueLiteral, value: value}
}

func unresolvedValue(raw string) valueExpr {
	return valueExpr{kind: valueUnresolved, raw: strings.TrimSpace(raw)}
}

func configReferenceValue(raw, key string) valueExpr {
	return valueExpr{
		kind:  valueConfigReference,
		raw:   strings.TrimSpace(raw),
		value: strings.TrimSpace(key),
	}
}

type endpointCandidate struct {
	ServiceName   string
	Repo          string
	ModulePath    string
	Framework     string
	Methods       []valueExpr
	Paths         []valueExpr
	Handler       string
	HandlerMethod string
	Evidence      domain.Evidence
	Confidence    float64
}

type endpointSource struct {
	language    string
	root        string
	file        string
	rel         string
	repo        string
	moduleRoot  string
	modulePath  string
	serviceName string
	text        string
	syntax      any
}

type endpointFrontend struct {
	language string
	match    func(string) bool
	parse    func(root, file, text string) (endpointSource, bool)
}

type endpointAdapter struct {
	language  string
	framework string
	applies   func(endpointSource) bool
	scan      func(endpointSource) []endpointCandidate
}

func registeredEndpointFrontends() []endpointFrontend {
	return []endpointFrontend{
		{language: "java", match: hasSuffix(".java"), parse: parseJavaEndpointSource},
		{language: "kotlin", match: hasSuffix(".kt"), parse: parseKotlinEndpointSource},
		{language: "csharp", match: hasSuffix(".cs"), parse: parseCSharpEndpointSource},
		{language: "go", match: hasSuffix(".go"), parse: parseGoEndpointSource},
		{language: "nodejs", match: nodejsMatch, parse: parseNodeJSEndpointSource},
		{language: "python", match: hasSuffix(".py"), parse: parsePythonEndpointSource},
	}
}

func nodejsMatch(name string) bool {
	return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") ||
		strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs")
}

func registeredEndpointAdapters() []endpointAdapter {
	return []endpointAdapter{
		javaSpringMVCAdapter,
		javaJAXRSAdapter,
		kotlinSpringMVCAdapter,
		kotlinKtorAdapter,
		kotlinJavalinAdapter,
		csharpASPNETControllerAdapter,
		csharpMinimalAPIAdapter,
		csharpServiceStackAdapter,
		goNetHTTPAdapter,
		goGinAdapter,
		goEchoAdapter,
		goChiAdapter,
		goFiberAdapter,
		goHTTPRouterAdapter,
		goGorillaMuxAdapter,
		nodejsExpressAdapter,
		nodejsFastifyAdapter,
		nodejsKoaAdapter,
		nodejsNestJSAdapter,
		nodejsHapiAdapter,
		pythonFastAPIAdapter,
		pythonFlaskAdapter,
	}
}

func scanFrameworkEndpoints(root string, dirs []string, languages ...string) []domain.EndpointRecord {
	return projectEndpointCandidates(scanEndpointCandidates(root, dirs, languages...))
}

func scanEndpointCandidates(root string, dirs []string, languages ...string) []endpointCandidate {
	enabled := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		enabled[language] = struct{}{}
	}
	adapters := registeredEndpointAdapters()
	var candidates []endpointCandidate
	for _, frontend := range registeredEndpointFrontends() {
		if len(enabled) > 0 {
			if _, ok := enabled[frontend.language]; !ok {
				continue
			}
		}
		languageAdapters := make([]endpointAdapter, 0, len(adapters))
		for _, adapter := range adapters {
			if adapter.language == frontend.language {
				languageAdapters = append(languageAdapters, adapter)
			}
		}
		for _, file := range walkFiles(root, dirs, frontend.match) {
			if isTestSourcePath(relativeTo(root, file)) {
				continue
			}
			text := readFile(file)
			source, ok := frontend.parse(root, file, text)
			if !ok {
				continue
			}
			for _, adapter := range languageAdapters {
				if adapter.applies != nil && !adapter.applies(source) {
					continue
				}
				found := adapter.scan(source)
				for i := range found {
					if found[i].Framework == "" {
						found[i].Framework = adapter.framework
					}
				}
				candidates = append(candidates, found...)
			}
		}
	}
	return candidates
}

func projectEndpointCandidates(candidates []endpointCandidate) []domain.EndpointRecord {
	var records []domain.EndpointRecord
	for _, candidate := range candidates {
		methods, methodsResolved := literalValues(candidate.Methods)
		paths, pathsResolved := literalValues(candidate.Paths)
		if !methodsResolved || !pathsResolved || len(methods) == 0 || len(paths) == 0 {
			continue
		}
		for _, method := range methods {
			method = strings.ToUpper(strings.TrimSpace(method))
			if method == "" {
				continue
			}
			for _, path := range paths {
				records = append(records, domain.EndpointRecord{
					ServiceName:   candidate.ServiceName,
					Repo:          candidate.Repo,
					Method:        method,
					Path:          path,
					Handler:       candidate.Handler,
					HandlerMethod: candidate.HandlerMethod,
					File:          candidate.Evidence.Path,
					Line:          candidate.Evidence.Line,
					Source:        candidate.Evidence.Kind,
					Confidence:    candidate.Confidence,
				})
			}
		}
	}
	return records
}

func literalValues(values []valueExpr) ([]string, bool) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value.kind != valueLiteral {
			return nil, false
		}
		if _, exists := seen[value.value]; exists {
			continue
		}
		seen[value.value] = struct{}{}
		out = append(out, value.value)
	}
	return out, true
}

func joinPathValues(prefix, route valueExpr) valueExpr {
	if prefix.kind != valueLiteral || route.kind != valueLiteral {
		return unresolvedValue(strings.TrimSpace(prefix.raw + " " + route.raw))
	}
	return literalValue(joinPaths(prefix.value, route.value))
}

func sourceEndpointCandidate(
	source endpointSource,
	framework string,
	methods, paths []valueExpr,
	handler, handlerMethod string,
	line int,
	confidence float64,
) endpointCandidate {
	return endpointCandidate{
		ServiceName:   source.serviceName,
		Repo:          source.repo,
		ModulePath:    source.modulePath,
		Framework:     framework,
		Methods:       methods,
		Paths:         paths,
		Handler:       handler,
		HandlerMethod: handlerMethod,
		Evidence: domain.Evidence{
			Path: source.rel,
			Line: line,
			Kind: domain.SourceCodeScan,
		},
		Confidence: confidence,
	}
}
