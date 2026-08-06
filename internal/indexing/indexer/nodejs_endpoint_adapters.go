package indexer

import (
	"path/filepath"
	"strings"
)

// ---- Node.js/TypeScript language frontend ----

type nodejsSource struct {
	imports     map[string]string // name → module path
	decorators  []nodejsDecoratorInfo
	assignments []nodejsAssignmentInfo
}

type nodejsDecoratorInfo struct {
	name      string
	arguments string
	line      int
}

type nodejsAssignmentInfo struct {
	target string
	value  string
	line   int
}

type nodejsRoute struct {
	method string
	path   string
}

func parseNodeJSEndpointSource(root, file, text string) (endpointSource, bool) {
	source := parseNodeJSSource(text)
	moduleRoot := findNodeJSModuleRoot(root, file)
	modulePath := ""
	serviceName := filepath.Base(relativeTo(root, moduleRoot))
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
		serviceName = readNodeJSPackageName(moduleRoot)
	}
	return endpointSource{
		language:    "nodejs",
		root:        root,
		file:        file,
		rel:         relativeTo(root, file),
		repo:        topSegment(relativeTo(root, file)),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: serviceName,
		text:        text,
		syntax:      source,
	}, true
}

func parseNodeJSSource(text string) nodejsSource {
	return nodejsSource{
		imports:     extractNodeJSImports(text),
		decorators:  extractNodeJSDecorators(text),
		assignments: extractNodeJSAssignments(text),
	}
}

func extractNodeJSImports(text string) map[string]string {
	imports := make(map[string]string)
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// ESM: import { X } from "module"
		if strings.HasPrefix(trimmed, "import ") {
			if idx := strings.LastIndex(trimmed, " from "); idx >= 0 {
				module := strings.Trim(trimmed[idx+6:], "'\" ")
				// Extract named imports
				names := trimmed[7:idx]
				names = strings.TrimPrefix(names, "{")
				names = strings.TrimSuffix(names, "}")
				for _, name := range strings.Split(names, ",") {
					name = strings.TrimSpace(name)
					if _, local, hasAlias := strings.Cut(name, " as "); hasAlias {
						imports[strings.TrimSpace(local)] = module
					} else if name != "" {
						imports[name] = module
					}
				}
				// Default import: import X from "module"
				if !strings.Contains(trimmed[:idx], "{") {
					parts := strings.Fields(trimmed[7:idx])
					if len(parts) == 1 {
						imports[parts[0]] = module
					}
				}
			}
			continue
		}
		// CJS: const X = require("module")
		if strings.Contains(trimmed, "= require(") {
			parts := strings.SplitN(trimmed, "=", 2)
			target := strings.TrimSpace(parts[0])
			target = strings.TrimPrefix(target, "const ")
			target = strings.TrimPrefix(target, "let ")
			target = strings.TrimPrefix(target, "var ")
			target = strings.TrimSpace(target)
			// Handle destructuring: const { X } = require("module")
			if strings.HasPrefix(target, "{") {
				target = strings.TrimPrefix(target, "{")
				target = strings.TrimSuffix(target, "}")
				for _, name := range strings.Split(target, ",") {
					name = strings.TrimSpace(name)
					if _, ln, _ := strings.Cut(name, ":"); ln != "" {
						imports[strings.TrimSpace(ln)] = ""
					} else if name != "" {
						imports[name] = ""
					}
				}
			} else if target != "" {
				imports[target] = ""
			}
		}
	}
	return imports
}

func extractNodeJSDecorators(text string) []nodejsDecoratorInfo {
	var decorators []nodejsDecoratorInfo
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "@") {
			continue
		}
		rest := trimmed[1:]
		if idx := strings.IndexByte(rest, '('); idx >= 0 {
			name := rest[:idx]
			args := rest[idx+1:]
			if last := strings.LastIndexByte(args, ')'); last >= 0 {
				args = args[:last]
			}
			// Strip quotes from args
			args = strings.Trim(args, "'\"")
			decorators = append(decorators, nodejsDecoratorInfo{
				name:      name,
				arguments: args,
				line:      i + 1,
			})
		}
	}
	return decorators
}

func extractNodeJSAssignments(text string) []nodejsAssignmentInfo {
	var assignments []nodejsAssignmentInfo
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "=") {
			continue
		}
		for _, kw := range []string{"const ", "let ", "var "} {
			if strings.HasPrefix(trimmed, kw) {
				rhs := trimmed[len(kw):]
				if parts := strings.SplitN(rhs, "=", 2); len(parts) == 2 {
					target := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])
					value = strings.TrimSuffix(value, ";")
					assignments = append(assignments, nodejsAssignmentInfo{
						target: target,
						value:  value,
						line:   i + 1,
					})
				}
				break
			}
		}
	}
	return assignments
}

// nodejsImported checks if source imports the given module.
func nodejsImported(source nodejsSource, module string) bool {
	for _, imported := range source.imports {
		if imported == module || strings.HasPrefix(module, imported) {
			return true
		}
	}
	return false
}

// nodejsIsConstructor checks if a value expression constructs a known router/app.
func nodejsIsConstructor(value string, constructorNames ...string) bool {
	value = strings.TrimSuffix(value, ";")
	value = strings.TrimSpace(value)
	for _, name := range constructorNames {
		if strings.HasPrefix(value, name+"(") || strings.HasPrefix(value, name+".Router(") {
			return true
		}
	}
	return false
}

// nodejsRouterVars finds variable names assigned to known constructors.
func nodejsRouterVars(source nodejsSource, constructorNames ...string) map[string]struct{} {
	vars := make(map[string]struct{})
	for _, assignment := range source.assignments {
		if nodejsIsConstructor(assignment.value, constructorNames...) {
			vars[assignment.target] = struct{}{}
		}
	}
	return vars
}

// ---- Express adapter ----

var nodejsExpressAdapter = endpointAdapter{
	language:  "nodejs",
	framework: "express",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(nodejsSource)
		return ok && nodejsImported(syntax, "express")
	},
	scan: scanNodeJSExpress,
}

func scanNodeJSExpress(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(nodejsSource)
	if !ok {
		return nil
	}
	routerVars := nodejsRouterVars(syntax, "express", "express.Router")
	if len(routerVars) == 0 {
		return nil
	}
	return scanNodeJSRoutesWithReceivers(source.text, routerVars, "express", 0.85)
}

// ---- Fastify adapter ----

var nodejsFastifyAdapter = endpointAdapter{
	language:  "nodejs",
	framework: "fastify",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(nodejsSource)
		return ok && nodejsImported(syntax, "fastify")
	},
	scan: scanNodeJSFastify,
}

func scanNodeJSFastify(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(nodejsSource)
	if !ok {
		return nil
	}
	routerVars := nodejsRouterVars(syntax, "fastify")
	if len(routerVars) == 0 {
		return nil
	}
	return scanNodeJSRoutesWithReceivers(source.text, routerVars, "fastify", 0.85)
}

// ---- Koa adapter ----

var nodejsKoaAdapter = endpointAdapter{
	language:  "nodejs",
	framework: "koa",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(nodejsSource)
		return ok && (nodejsImported(syntax, "koa") || nodejsImported(syntax, "koa-router"))
	},
	scan: scanNodeJSKoa,
}

func scanNodeJSKoa(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(nodejsSource)
	if !ok {
		return nil
	}
	routerVars := nodejsRouterVars(syntax, "Koa", "koa", "koa.Router", "Router")
	if len(routerVars) == 0 {
		return nil
	}
	return scanNodeJSRoutesWithReceivers(source.text, routerVars, "koa", 0.8)
}

// ---- NestJS adapter ----

var nodejsNestJSAdapter = endpointAdapter{
	language:  "nodejs",
	framework: "nestjs",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(nodejsSource)
		return ok && nodejsImported(syntax, "@nestjs/common")
	},
	scan: scanNodeJSNestJS,
}

func scanNodeJSNestJS(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(nodejsSource)
	if !ok {
		return nil
	}
	// Find @Controller("prefix") to get class-level prefix
	controllerPrefix := ""
	for _, d := range syntax.decorators {
		if d.name == "Controller" {
			controllerPrefix = d.arguments
		}
	}
	httpDecoratorMethods := map[string]string{
		"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
		"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	}
	var candidates []endpointCandidate
	for _, d := range syntax.decorators {
		method, ok := httpDecoratorMethods[d.name]
		if !ok {
			continue
		}
		path := d.arguments
		candidates = append(candidates, sourceEndpointCandidate(
			source, "nestjs",
			[]valueExpr{literalValue(method)},
			[]valueExpr{literalValue(joinPaths(controllerPrefix, path))},
			filepath.Base(source.rel), "",
			d.line, 0.85,
		))
	}
	return candidates
}

// ---- Hapi adapter ----

var nodejsHapiAdapter = endpointAdapter{
	language:  "nodejs",
	framework: "hapi",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(nodejsSource)
		return ok && nodejsImported(syntax, "@hapi/hapi")
	},
	scan: scanNodeJSHapi,
}

func scanNodeJSHapi(source endpointSource) []endpointCandidate {
	// Hapi uses server.route({ method: "GET", path: "/..." }) object literals.
	var candidates []endpointCandidate
	for _, m := range hapiMethodFirstRe.FindAllStringSubmatch(source.text, -1) {
		if len(m) > 2 {
			candidates = append(candidates, sourceEndpointCandidate(
				source, "hapi",
				[]valueExpr{literalValue(strings.ToUpper(m[1]))},
				[]valueExpr{literalValue(m[2])},
				filepath.Base(source.rel), "",
				0, 0.8,
			))
		}
	}
	for _, m := range hapiPathFirstRe.FindAllStringSubmatch(source.text, -1) {
		if len(m) > 2 {
			candidates = append(candidates, sourceEndpointCandidate(
				source, "hapi",
				[]valueExpr{literalValue(strings.ToUpper(m[2]))},
				[]valueExpr{literalValue(m[1])},
				filepath.Base(source.rel), "",
				0, 0.8,
			))
		}
	}
	return candidates
}

// ---- Shared helpers ----

// scanNodeJSRoutesWithReceivers scans text for .method("path") calls on known
// receiver variable names. Only routes whose receiver is in knownVars are accepted.
func scanNodeJSRoutesWithReceivers(
	text string,
	knownVars map[string]struct{},
	framework string,
	confidence float64,
) []endpointCandidate {
	var candidates []endpointCandidate
	// Use the nodeMethodRouteRe from the old code for string extraction
	for _, m := range nodeMethodRouteRe.FindAllStringSubmatch(text, -1) {
		method := strings.ToUpper(m[1])
		path := m[2]
		// Find the receiver: look backward from the match for ".get(" pattern
		fullMatch := m[0]
		if dotIdx := strings.Index(fullMatch, "."); dotIdx > 0 {
			receiver := fullMatch[:dotIdx]
			// Receiver might end with \n or spaces, strip them
			receiver = strings.TrimSpace(receiver)
			// Check if receiver is in known vars
			if _, ok := knownVars[receiver]; !ok {
				continue
			}
		}
		candidates = append(candidates, endpointCandidate{
			ServiceName:   "",
			Repo:          "",
			Framework:     framework,
			Methods:       []valueExpr{literalValue(method)},
			Paths:         []valueExpr{literalValue(path)},
			Handler:       "",
			HandlerMethod: "",
			Confidence:    confidence,
		})
	}
	return candidates
}

// Re-export of compile-time variables from nodejs_indexer.go (still needed by the existing
// scanNodeJSEndpoints function until it's removed).
var _ = hapiMethodFirstRe // suppress unused warning