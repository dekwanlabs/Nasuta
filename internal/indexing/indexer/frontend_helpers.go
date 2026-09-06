package indexer

import (
	"path/filepath"
	"strings"
)

func moduleServiceName(root, moduleRoot string, readName func(string) string) string {
	name := filepath.Base(relativeTo(root, moduleRoot))
	if moduleRoot != "" {
		name = readName(moduleRoot)
	}
	return name
}

// newEndpointSource centralizes the source metadata shared by language
// frontends while leaving syntax parsing and service-name inference local.
func newEndpointSource(
	language, root, file, text, moduleRoot, serviceName string, syntax any,
) endpointSource {
	rel := relativeTo(root, file)
	modulePath := ""
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
	}
	return endpointSource{
		language:    language,
		root:        root,
		file:        file,
		rel:         rel,
		repo:        topSegment(rel),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: serviceName,
		text:        text,
		syntax:      syntax,
	}
}

func literalValueExprs(values []string) []valueExpr {
	out := make([]valueExpr, 0, len(values))
	for _, value := range values {
		out = append(out, literalValue(value))
	}
	return out
}

// springControllerMappingIsDefault identifies the only class-level mapping
// shape whose route semantics are safe to combine with method mappings.
func springControllerMappingIsDefault(prefixes, methods []valueExpr) bool {
	return prefixes[0].kind == valueLiteral &&
		prefixes[0].value == "" &&
		methods[0].kind == valueLiteral &&
		methods[0].value == "ANY"
}

func kotlinImportsAny(source kotlinSource, prefix string) bool {
	for _, imported := range source.imports {
		if hasImportPrefix(imported, prefix) {
			return true
		}
	}
	return false
}

func hasImportPrefix(imported, prefix string) bool {
	return strings.HasPrefix(imported, prefix)
}
