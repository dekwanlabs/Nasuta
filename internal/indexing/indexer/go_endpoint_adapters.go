package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

type goSource struct {
	file    *ast.File
	fset    *token.FileSet
	imports  map[string]string
	consts  map[string]string
	srcText string
}

func parseGoEndpointSource(root, file, text string) (endpointSource, bool) {
	fset := token.NewFileSet()
	syntax, err := parser.ParseFile(fset, file, text, 0)
	if err != nil || syntax == nil {
		return endpointSource{}, false
	}
	moduleRoot := findModuleRoot(root, file, "go.mod")
	modulePath := inferModulePathFromRel(relativeTo(root, file))
	serviceName := filepath.Base(modulePath)
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
		serviceName = readGoModuleName(moduleRoot)
	}
	return endpointSource{
		language:    "go",
		root:        root,
		file:        file,
		rel:         relativeTo(root, file),
		repo:        topSegment(relativeTo(root, file)),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: serviceName,
		text:        text,
		syntax: goSource{
			file:    syntax,
			fset:    fset,
			imports: goImports(syntax),
			consts:  goStringConstants(syntax),
			srcText: text,
		},
	}, true
}

func goImports(file *ast.File) map[string]string {
	imports := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path == "" {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

func goStringConstants(file *ast.File) map[string]string {
	constants := make(map[string]string)
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, specification := range group.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range values.Names {
				if i >= len(values.Values) {
					continue
				}
				if value, ok := goStaticString(values.Values[i], constants); ok {
					constants[name.Name] = value
				}
			}
		}
	}
	return constants
}

type goRouterSpec struct {
	framework    string
	importPaths  []string
	constructors map[string]struct{}
	groupMethods map[string]struct{}
	fixedMethods map[string]string
}

var (
	goGinAdapter = newGoRouterAdapter(goRouterSpec{
		framework:    "gin",
		importPaths:  []string{"github.com/gin-gonic/gin"},
		constructors: setOfStrings("New", "Default"),
		groupMethods: setOfStrings("Group"),
		fixedMethods: map[string]string{
			"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
			"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
			"Any": "ANY",
		},
	})
	goEchoAdapter = newGoRouterAdapter(goRouterSpec{
		framework:    "echo",
		importPaths:  []string{"github.com/labstack/echo/v4", "github.com/labstack/echo"},
		constructors: setOfStrings("New"),
		groupMethods: setOfStrings("Group"),
		fixedMethods: map[string]string{
			"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
			"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
			"Any": "ANY",
		},
	})
	goChiAdapter = newGoRouterAdapter(goRouterSpec{
		framework:    "chi",
		importPaths:  []string{"github.com/go-chi/chi/v5", "github.com/go-chi/chi"},
		constructors: setOfStrings("NewRouter", "NewMux"),
		groupMethods: setOfStrings("Route"),
		fixedMethods: map[string]string{
			"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
			"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
			"Handle": "ANY", "HandleFunc": "ANY",
		},
	})
	goFiberAdapter = newGoRouterAdapter(goRouterSpec{
		framework:    "fiber",
		importPaths:  []string{"github.com/gofiber/fiber/v2", "github.com/gofiber/fiber/v3"},
		constructors: setOfStrings("New"),
		groupMethods: setOfStrings("Group"),
		fixedMethods: map[string]string{
			"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
			"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
			"All": "ANY",
		},
	})
	goHTTPRouterAdapter = newGoRouterAdapter(goRouterSpec{
		framework:    "httprouter",
		importPaths:  []string{"github.com/julienschmidt/httprouter"},
		constructors: setOfStrings("New"),
		fixedMethods: map[string]string{
			"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
			"PATCH": "PATCH", "HEAD": "HEAD",
		},
	})
)

func newGoRouterAdapter(spec goRouterSpec) endpointAdapter {
	return endpointAdapter{
		language:  "go",
		framework: spec.framework,
		applies: func(source endpointSource) bool {
			syntax, ok := source.syntax.(goSource)
			return ok && goHasImport(syntax, spec.importPaths)
		},
		scan: func(source endpointSource) []endpointCandidate {
			return scanGoRouter(source, spec)
		},
	}
}

func scanGoRouter(source endpointSource, spec goRouterSpec) []endpointCandidate {
	syntax, ok := source.syntax.(goSource)
	if !ok {
		return nil
	}
	bindings := goRouterBindings(syntax, spec)
	if len(bindings) == 0 {
		return nil
	}
	var candidates []endpointCandidate
	ast.Inspect(syntax.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		methodName := selector.Sel.Name
		if _, isGroup := spec.groupMethods[methodName]; isGroup {
			return true
		}
		methods, pathIndex, ok := goRouteSignature(methodName, spec, call)
		if !ok {
			return true
		}
		prefix, known := goRouterExpr(syntax, selector.X, spec, bindings)
		if !known {
			return true
		}
		if pathIndex >= len(call.Args) {
			return true
		}
		paths := []valueExpr{joinPathValues(prefix, goStaticValue(syntax, call.Args[pathIndex]))}
		if methodName == "Match" {
			methods = goMethodValues(syntax, call.Args[0])
		}
		handler := ""
		handlerIndex := pathIndex + 1
		if handlerIndex < len(call.Args) {
			handler = goExprName(call.Args[handlerIndex])
		}
		line := syntax.fset.Position(call.Pos()).Line
		candidates = append(candidates, sourceEndpointCandidate(
			source, spec.framework, methods, paths, filepath.Base(source.rel), handler,
			line, 0.82,
		))
		return true
	})
	return candidates
}

func goHasImport(source goSource, paths []string) bool {
	for _, imported := range source.imports {
		for _, path := range paths {
			if imported == path {
				return true
			}
		}
	}
	return false
}

func goRouterBindings(source goSource, spec goRouterSpec) map[string]valueExpr {
	bindings := make(map[string]valueExpr)
	ast.Inspect(source.file, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for i, left := range statement.Lhs {
				ident, ok := left.(*ast.Ident)
				if !ok || i >= len(statement.Rhs) {
					continue
				}
				if value, ok := goRouterExpr(source, statement.Rhs[i], spec, bindings); ok {
					bindings[ident.Name] = value
				} else {
					delete(bindings, ident.Name)
				}
			}
		case *ast.ValueSpec:
			for i, name := range statement.Names {
				if i >= len(statement.Values) {
					continue
				}
				if value, ok := goRouterExpr(source, statement.Values[i], spec, bindings); ok {
					bindings[name.Name] = value
				}
			}
		}
		return true
	})
	return bindings
}

func goRouterExpr(
	source goSource,
	expression ast.Expr,
	spec goRouterSpec,
	bindings map[string]valueExpr,
) (valueExpr, bool) {
	switch expression := expression.(type) {
	case *ast.Ident:
		value, ok := bindings[expression.Name]
		return value, ok
	case *ast.ParenExpr:
		return goRouterExpr(source, expression.X, spec, bindings)
	case *ast.CallExpr:
		if goIsConstructor(source, expression, spec) {
			return literalValue(""), true
		}
		selector, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok {
			return valueExpr{}, false
		}
		if _, isGroup := spec.groupMethods[selector.Sel.Name]; !isGroup ||
			len(expression.Args) == 0 {
			return valueExpr{}, false
		}
		parent, ok := goRouterExpr(source, selector.X, spec, bindings)
		if !ok {
			return valueExpr{}, false
		}
		return joinPathValues(parent, goStaticValue(source, expression.Args[0])), true
	default:
		return valueExpr{}, false
	}
}

func goIsConstructor(source goSource, call *ast.CallExpr, spec goRouterSpec) bool {
	switch function := call.Fun.(type) {
	case *ast.SelectorExpr:
		packageName, ok := function.X.(*ast.Ident)
		if !ok {
			return false
		}
		imported, ok := source.imports[packageName.Name]
		if !ok {
			return false
		}
		if !containsString(spec.importPaths, imported) {
			return false
		}
		_, ok = spec.constructors[function.Sel.Name]
		return ok
	case *ast.Ident:
		if source.imports["."] == "" {
			return false
		}
		_, ok := spec.constructors[function.Name]
		return ok
	default:
		return false
	}
}

func goRouteSignature(
	name string,
	spec goRouterSpec,
	call *ast.CallExpr,
) ([]valueExpr, int, bool) {
	if method, ok := spec.fixedMethods[name]; ok {
		return []valueExpr{literalValue(method)}, 0, true
	}
	switch name {
	case "Add", "Handle", "HandleFunc", "Method", "MethodFunc":
		if name == "Add" || name == "Method" || name == "MethodFunc" {
			return nil, 1, true
		}
		return []valueExpr{literalValue("ANY")}, 0, true
	case "Match":
		if len(call.Args) < 2 {
			return nil, 0, false
		}
		return nil, 1, true
	default:
		return nil, 0, false
	}
}

func goMethodValues(source goSource, expression ast.Expr) []valueExpr {
	if literal, ok := goStaticString(expression, source.consts); ok {
		return []valueExpr{literalValue(strings.ToUpper(literal))}
	}
	switch expression := expression.(type) {
	case *ast.CompositeLit:
		out := make([]valueExpr, 0, len(expression.Elts))
		for _, element := range expression.Elts {
			value, ok := goStaticString(element, source.consts)
			if !ok {
				return []valueExpr{unresolvedValue(goExprText(source, element))}
			}
			out = append(out, literalValue(strings.ToUpper(value)))
		}
		if len(out) > 0 {
			return out
		}
	}
	return []valueExpr{unresolvedValue(goExprText(source, expression))}
}

func goStaticValue(source goSource, expression ast.Expr) valueExpr {
	if value, ok := goStaticString(expression, source.consts); ok {
		return literalValue(value)
	}
	return unresolvedValue(goExprText(source, expression))
}

func goStaticString(expression ast.Expr, constants map[string]string) (string, bool) {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expression.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := constants[expression.Name]
		return value, ok
	case *ast.ParenExpr:
		return goStaticString(expression.X, constants)
	case *ast.BinaryExpr:
		if expression.Op != token.ADD {
			return "", false
		}
		left, leftOK := goStaticString(expression.X, constants)
		right, rightOK := goStaticString(expression.Y, constants)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	default:
		return "", false
	}
}

func goStaticStringFromSource(source goSource, expression ast.Expr) (string, bool) {
	return goStaticString(expression, source.consts)
}

// goBuiltinMethod returns whether name is a recognised Go HTTP method name.
func goBuiltinMethod(name string) string {
	switch strings.ToUpper(name) {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		return strings.ToUpper(name)
	default:
		return ""
	}
}

// goHTTPMethodPattern splits a Go 1.22 enhanced pattern like "GET /users/{id}"
// into method and path. Returns ("", p) when p has no method prefix.
func goHTTPMethodPattern(pattern string) (method, path string) {
	pattern = strings.TrimSpace(pattern)
	if idx := strings.IndexByte(pattern, ' '); idx > 0 {
		if m := goBuiltinMethod(pattern[:idx]); m != "" {
			return m, strings.TrimSpace(pattern[idx+1:])
		}
	}
	return "ANY", pattern
}

var goNetHTTPAdapter = endpointAdapter{
	language:  "go",
	framework: "net/http",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(goSource)
		return ok && goHasImport(syntax, []string{"net/http"})
	},
	scan: scanNetHTTP,
}

func scanNetHTTP(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(goSource)
	if !ok {
		return nil
	}
	bindings := goNetHTTPBindings(syntax)
	if len(bindings) == 0 {
		return nil
	}
	var candidates []endpointCandidate
	ast.Inspect(syntax.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := selector.Sel.Name
		if name != "Handle" && name != "HandleFunc" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		prefix, known := goRouterExpr(syntax, selector.X, goNetHTTPSpec(), bindings)
		if !known {
			return true
		}
		pathValue := goStaticValue(syntax, call.Args[0])
		if pathValue.kind != valueLiteral {
			return true
		}
		method, path := goHTTPMethodPattern(pathValue.value)
		paths := joinPathValues(prefix, literalValue(path))
		handler := ""
		if len(call.Args) >= 2 {
			handler = goExprName(call.Args[1])
		}
		line := syntax.fset.Position(call.Pos()).Line
		candidates = append(candidates, sourceEndpointCandidate(
			source, "net/http",
			[]valueExpr{literalValue(method)},
			[]valueExpr{paths},
			filepath.Base(source.rel), handler,
			line, 0.8,
		))
		return true
	})
	return candidates
}

func goNetHTTPSpec() goRouterSpec {
	return goRouterSpec{
		framework:   "net/http",
		importPaths: []string{"net/http"},
		constructors: setOfStrings("NewServeMux"),
		groupMethods: map[string]struct{}{},
		fixedMethods: map[string]string{},
	}
}

func goNetHTTPBindings(source goSource) map[string]valueExpr {
	bindings := make(map[string]valueExpr)
	ast.Inspect(source.file, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for i, left := range statement.Lhs {
				ident, ok := left.(*ast.Ident)
				if !ok || i >= len(statement.Rhs) {
					continue
				}
				if goIsNetHTTPConstructor(source, statement.Rhs[i]) {
					bindings[ident.Name] = literalValue("")
				} else if value, ok := goRouterExpr(source, statement.Rhs[i], goNetHTTPSpec(), bindings); ok {
					bindings[ident.Name] = value
				} else {
					delete(bindings, ident.Name)
				}
			}
		case *ast.ValueSpec:
			for i, name := range statement.Names {
				if i >= len(statement.Values) {
					continue
				}
				if goIsNetHTTPConstructor(source, statement.Values[i]) {
					bindings[name.Name] = literalValue("")
				} else if value, ok := goRouterExpr(source, statement.Values[i], goNetHTTPSpec(), bindings); ok {
					bindings[name.Name] = value
				}
			}
		}
		return true
	})
	// Always consider stdlib http.DefaultServeMux as known.
	if _, exists := bindings["http"]; exists {
		bindings["http.DefaultServeMux"] = literalValue("")
	}
	return bindings
}

func goIsNetHTTPConstructor(source goSource, expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	imported, ok := source.imports[pkgIdent.Name]
	if !ok || imported != "net/http" {
		return false
	}
	return selector.Sel.Name == "NewServeMux"
}

var goGorillaMuxAdapter = newGoRouterAdapter(goRouterSpec{
	framework:   "gorilla/mux",
	importPaths: []string{"github.com/gorilla/mux"},
	constructors: setOfStrings("NewRouter"),
	groupMethods: setOfStrings("NewRoute", "PathPrefix", "Subrouter"),
	fixedMethods: map[string]string{
		"HandlerFunc": "ANY",
		"HandleFunc":  "ANY",
	},
})

func goExprText(source goSource, expression ast.Expr) string {
	start := source.fset.Position(expression.Pos()).Offset
	end := source.fset.Position(expression.End()).Offset
	if start < 0 || end < start || end > len(sourceText(source)) {
		return ""
	}
	return sourceText(source)[start:end]
}

func sourceText(source goSource) string {
	return source.srcText
}

func goExprName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		left := goExprName(expression.X)
		if left == "" {
			return expression.Sel.Name
		}
		return left + "." + expression.Sel.Name
	default:
		return ""
	}
}

func setOfStrings(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
