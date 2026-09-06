package indexer

import (
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// indexedClientMethod is the common method evidence shape used by annotated
// HTTP clients such as Retrofit and Refit.
type indexedClientMethod struct {
	name string
	line int
}

// indexedHTTPClient describes an annotated client interface before it is
// activated by a concrete call site.
type indexedHTTPClient struct {
	interfaceName string
	target        string
	path          string
	methods       map[string]indexedClientMethod
}

// extractIndexedClientMethods extracts method names and their source lines
// from an annotated HTTP client interface body.
func extractIndexedClientMethods(sourceText, body string, bodyOffset int, methodRe *regexp.Regexp) map[string]indexedClientMethod {
	methods := make(map[string]indexedClientMethod)
	for _, match := range methodRe.FindAllStringSubmatchIndex(body, -1) {
		if len(match) < 4 {
			continue
		}
		methodName := body[match[2]:match[3]]
		line := lineAt(sourceText, bodyOffset+match[0])
		methods[methodName] = indexedClientMethod{name: methodName, line: line}
	}
	return methods
}

// appendIndexedHTTPClientEdge records both the concrete client call and the
// annotated declaration that gives it its remote-service meaning.
func appendIndexedHTTPClientEdge(
	edges *[]domain.DependencyEdge,
	caller serviceIdentity,
	target, callPath string,
	callLine int,
	receiver, methodName string,
	declarationPath, interfaceName string,
	declarationLine int,
) {
	*edges = append(*edges, domain.DependencyEdge{
		CallerServiceKey: caller.Key,
		From:             caller.Name,
		To:               target,
		Type:             domain.EdgeHTTP,
		Evidence: []domain.Evidence{
			{Path: callPath, Line: callLine, Symbol: receiver + "." + methodName, Kind: domain.SourceCodeScan},
			{Path: declarationPath, Line: declarationLine, Symbol: interfaceName + "." + methodName, Kind: domain.SourceCodeScan},
		},
		Confidence: 0.75,
	})
}

// scanHTTPURLDependencies scans literal HTTP client URLs using the same
// ownership, comment, and target filtering rules across language frontends.
func scanHTTPURLDependencies(
	root string,
	files []string,
	hasClient func(string) bool,
	urlRe, clientCallRe *regexp.Regexp,
	normalizeTarget func(string) string,
	confidence float64,
) []domain.DependencyEdge {
	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !hasClient(text) {
			continue
		}
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		for _, match := range urlRe.FindAllStringSubmatchIndex(text, -1) {
			if len(match) < 4 || !httpURLUsedByClient(text, match[0], match[1], clientCallRe) {
				continue
			}
			target := normalizeTarget(text[match[2]:match[3]])
			if skipDependencyTarget(target) {
				continue
			}
			edges = append(edges, protocolEdge(caller, target, domain.EdgeHTTP, rel, lineAt(text, match[0]), confidence))
		}
	}
	return edges
}

func normalizeHTTPHost(target string) string {
	target = strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
	target, _, _ = strings.Cut(target, "/")
	return target
}
