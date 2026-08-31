package indexer

import (
	"strings"
)

var javaSpringMVCAdapter = endpointAdapter{
	language:  "java",
	framework: "spring-mvc",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(jvmSource)
		if !ok {
			return false
		}
		for _, annotation := range syntax.annotations {
			if isResolvedJavaSpringAnnotation(syntax, annotation) {
				return true
			}
		}
		return false
	},
	scan: scanSpringMVC,
}

var javaJAXRSAdapter = endpointAdapter{
	language:  "java",
	framework: "jax-rs",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(jvmSource)
		if !ok {
			return false
		}
		if hasJAXRSImport(syntax) {
			return true
		}
		for _, annotation := range syntax.annotations {
			if isJAXRSNamespace(annotation.qualifiedName) {
				return true
			}
		}
		return false
	},
	scan: scanJAXRS,
}

func parseJavaEndpointSource(root, file, text string) (endpointSource, bool) {
	syntax := scanJVMSource(text)
	if len(syntax.tokens) == 0 {
		return endpointSource{}, false
	}
	moduleRoot := findJavaModuleRoot(root, file)
	modulePath := ""
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
	}
	return endpointSource{
		language:    "java",
		root:        root,
		file:        file,
		rel:         relativeTo(root, file),
		repo:        topSegment(relativeTo(root, file)),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: inferJavaServiceName(root, file),
		text:        text,
		syntax:      syntax,
	}, true
}

type boundJavaAnnotation struct {
	annotation  jvmAnnotation
	declaration javaDeclaration
}

func bindJavaAnnotations(source jvmSource) []boundJavaAnnotation {
	bindings := make([]boundJavaAnnotation, 0, len(source.annotations))
	for _, annotation := range source.annotations {
		declaration, ok := javaDeclarationAfter(
			source.tokens, annotation.tokenEnd, annotation.braceDepth,
		)
		if !ok {
			continue
		}
		bindings = append(bindings, boundJavaAnnotation{
			annotation: annotation, declaration: declaration,
		})
	}
	return bindings
}

type springControllerBinding struct {
	declaration javaDeclaration
	prefixes    []valueExpr
	methods     []valueExpr
	resolved    bool
}

func scanSpringMVC(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(jvmSource)
	if !ok {
		return nil
	}
	bindings := bindJavaAnnotations(syntax)
	controllers := make(map[int]*springControllerBinding)
	for _, binding := range bindings {
		if !isResolvedJavaSpringAnnotation(syntax, binding.annotation) ||
			!javaAnnotationIsController(binding.annotation.name) ||
			binding.declaration.kind != javaTypeDeclaration {
			continue
		}
		if _, exists := controllers[binding.declaration.start]; exists {
			continue
		}
		controllers[binding.declaration.start] = &springControllerBinding{
			declaration: binding.declaration,
			prefixes:    []valueExpr{literalValue("")},
			methods:     []valueExpr{literalValue("ANY")},
			resolved:    true,
		}
	}
	if len(controllers) == 0 {
		return nil
	}

	for _, binding := range bindings {
		if !isResolvedJavaSpringAnnotation(syntax, binding.annotation) ||
			binding.annotation.name != "RequestMapping" ||
			binding.declaration.kind != javaTypeDeclaration {
			continue
		}
		controller := controllers[binding.declaration.start]
		if controller == nil {
			continue
		}
		if !controller.resolved {
			continue
		}
		prefixes, pathsResolved := springMappingPaths(binding.annotation)
		methods, methodsResolved := springMappingMethods(binding.annotation)
		if !pathsResolved || !methodsResolved {
			controller.resolved = false
			controller.prefixes = []valueExpr{unresolvedValue(binding.annotation.text)}
			controller.methods = []valueExpr{unresolvedValue(binding.annotation.text)}
			continue
		}
		if controller.prefixes[0].kind != valueLiteral ||
			controller.prefixes[0].value != "" ||
			controller.methods[0].kind != valueLiteral ||
			controller.methods[0].value != "ANY" {
			// Repeated class mappings may have different runtime semantics
			// (repeatable annotations, composed mappings). Keep them out of
			// the authoritative route set until a richer Java model exists.
			controller.resolved = false
			controller.prefixes = []valueExpr{unresolvedValue(binding.annotation.text)}
			controller.methods = []valueExpr{unresolvedValue(binding.annotation.text)}
			continue
		}
		controller.prefixes = prefixes
		controller.methods = methods
	}

	var candidates []endpointCandidate
	for _, binding := range bindings {
		if !isResolvedJavaSpringAnnotation(syntax, binding.annotation) ||
			!isJavaMappingAnnotation(binding.annotation.name) ||
			binding.declaration.kind != javaMethodDeclaration {
			continue
		}
		controller := enclosingSpringController(controllers, binding.annotation)
		if controller == nil {
			continue
		}
		paths, pathsResolved := springMappingPaths(binding.annotation)
		methods, methodsResolved := springMappingMethods(binding.annotation)
		if !pathsResolved {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		if !methodsResolved {
			methods = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		if !controller.resolved {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		} else {
			paths = combinePathValues(controller.prefixes, paths)
			methods = combineMethodValues(controller.methods, methods)
		}
		if len(paths) == 0 {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		if len(methods) == 0 {
			methods = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		candidates = append(candidates, sourceEndpointCandidate(
			source, "spring-mvc", methods, paths,
			controller.declaration.name, binding.declaration.name,
			binding.annotation.line, 0.85,
		))
	}
	return candidates
}

func enclosingSpringController(
	controllers map[int]*springControllerBinding,
	annotation jvmAnnotation,
) *springControllerBinding {
	var best *springControllerBinding
	bestSpan := 0
	for _, controller := range controllers {
		if !javaDeclarationDirectlyContains(
			controller.declaration, annotation.start, annotation.braceDepth,
		) {
			continue
		}
		span := controller.declaration.bodyEnd - controller.declaration.bodyStart
		if best == nil || span < bestSpan {
			best = controller
			bestSpan = span
		}
	}
	return best
}

func springMappingPaths(annotation jvmAnnotation) ([]valueExpr, bool) {
	values, ok := strictJavaMappingPaths(annotation)
	if !ok {
		return nil, false
	}
	out := make([]valueExpr, 0, len(values))
	for _, value := range values {
		out = append(out, literalValue(value))
	}
	return out, true
}

func springMappingMethods(annotation jvmAnnotation) ([]valueExpr, bool) {
	values, ok := strictJavaMappingMethods(annotation)
	if !ok {
		return nil, false
	}
	out := make([]valueExpr, 0, len(values))
	for _, value := range values {
		out = append(out, literalValue(value))
	}
	return out, true
}

func combinePathValues(prefixes, routes []valueExpr) []valueExpr {
	out := make([]valueExpr, 0, len(prefixes)*len(routes))
	for _, prefix := range prefixes {
		for _, route := range routes {
			out = append(out, joinPathValues(prefix, route))
		}
	}
	return out
}

func combineMethodValues(classMethods, methodMethods []valueExpr) []valueExpr {
	if len(classMethods) == 1 && classMethods[0].kind == valueLiteral &&
		classMethods[0].value == "ANY" {
		return methodMethods
	}
	if len(methodMethods) == 1 && methodMethods[0].kind == valueLiteral &&
		methodMethods[0].value == "ANY" {
		return classMethods
	}
	for _, method := range append(append([]valueExpr{}, classMethods...), methodMethods...) {
		if method.kind != valueLiteral {
			return []valueExpr{unresolvedValue(method.raw)}
		}
	}
	allowed := make(map[string]struct{}, len(classMethods))
	for _, method := range classMethods {
		allowed[method.value] = struct{}{}
	}
	out := make([]valueExpr, 0, len(methodMethods))
	for _, method := range methodMethods {
		if _, ok := allowed[method.value]; ok {
			out = append(out, method)
		}
	}
	return out
}

func isResolvedJavaSpringAnnotation(source jvmSource, annotation jvmAnnotation) bool {
	qualified, ok := javaSpringAnnotationQualifiedName(annotation.name)
	return ok && jvmAnnotationHasQualifiedName(source, annotation, qualified)
}

func javaSpringAnnotationQualifiedName(name string) (string, bool) {
	switch name {
	case "RestController":
		return "org.springframework.web.bind.annotation.RestController", true
	case "Controller":
		return "org.springframework.stereotype.Controller", true
	case "RequestMapping", "GetMapping", "PostMapping", "PutMapping",
		"DeleteMapping", "PatchMapping":
		return "org.springframework.web.bind.annotation." + name, true
	default:
		return "", false
	}
}

func jvmAnnotationHasQualifiedName(source jvmSource, annotation jvmAnnotation, expected string) bool {
	if annotation.qualifiedName == expected {
		return true
	}
	if strings.Contains(annotation.qualifiedName, ".") {
		return false
	}
	if imported := source.imports[annotation.name]; imported == expected {
		return true
	}
	prefix := expected[:strings.LastIndex(expected, ".")+1]
	wildcard := prefix + "*"
	for _, imported := range source.imports {
		if imported == wildcard {
			return true
		}
	}
	return false
}

func hasJAXRSImport(source jvmSource) bool {
	for _, qualified := range source.imports {
		if isJAXRSNamespace(qualified) || qualified == "javax.ws.rs.*" ||
			qualified == "jakarta.ws.rs.*" {
			return true
		}
	}
	return false
}

func isJAXRSNamespace(qualified string) bool {
	return strings.HasPrefix(qualified, "javax.ws.rs.") ||
		strings.HasPrefix(qualified, "jakarta.ws.rs.")
}

func isJAXRSAnnotation(source jvmSource, annotation jvmAnnotation, names ...string) bool {
	for _, name := range names {
		if annotation.name != name {
			continue
		}
		if isJAXRSNamespace(annotation.qualifiedName) {
			return true
		}
		if imported := source.imports[name]; isJAXRSNamespace(imported) {
			return true
		}
		for _, imported := range source.imports {
			if imported == "javax.ws.rs.*" || imported == "jakarta.ws.rs.*" {
				return true
			}
		}
	}
	return false
}

func scanJAXRS(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(jvmSource)
	if !ok {
		return nil
	}
	bindings := bindJavaAnnotations(syntax)
	types := javaTypeDeclarations(syntax.tokens)
	pathsByType := make(map[int][]valueExpr)
	for _, binding := range bindings {
		if binding.declaration.kind != javaTypeDeclaration ||
			!isJAXRSAnnotation(syntax, binding.annotation, "Path") {
			continue
		}
		paths, resolved := jaxrsPathValues(binding.annotation)
		if !resolved {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		if _, exists := pathsByType[binding.declaration.start]; exists {
			pathsByType[binding.declaration.start] = []valueExpr{
				unresolvedValue(binding.annotation.text),
			}
			continue
		}
		pathsByType[binding.declaration.start] = paths
	}

	var candidates []endpointCandidate
	for _, binding := range bindings {
		if binding.declaration.kind != javaMethodDeclaration {
			continue
		}
		method := jaxrsHTTPMethod(syntax, binding.annotation)
		if method == "" {
			continue
		}
		owner := enclosingJavaType(types, binding.annotation.start, binding.annotation.braceDepth)
		if owner == nil {
			continue
		}
		prefixes := pathsByType[owner.start]
		if len(prefixes) == 0 {
			prefixes = []valueExpr{literalValue("")}
		}
		routes := []valueExpr{literalValue("")}
		for _, other := range bindings {
			if other.declaration.start != binding.declaration.start ||
				!isJAXRSAnnotation(syntax, other.annotation, "Path") {
				continue
			}
			routes, _ = jaxrsPathValues(other.annotation)
			if len(routes) == 0 {
				routes = []valueExpr{unresolvedValue(other.annotation.text)}
			}
			break
		}
		candidates = append(candidates, sourceEndpointCandidate(
			source, "jax-rs",
			[]valueExpr{literalValue(method)},
			combinePathValues(prefixes, routes),
			owner.name, binding.declaration.name,
			binding.annotation.line, 0.8,
		))
	}
	return candidates
}

func jaxrsHTTPMethod(source jvmSource, annotation jvmAnnotation) string {
	switch {
	case isJAXRSAnnotation(source, annotation, "GET"):
		return "GET"
	case isJAXRSAnnotation(source, annotation, "POST"):
		return "POST"
	case isJAXRSAnnotation(source, annotation, "PUT"):
		return "PUT"
	case isJAXRSAnnotation(source, annotation, "DELETE"):
		return "DELETE"
	case isJAXRSAnnotation(source, annotation, "PATCH"):
		return "PATCH"
	case isJAXRSAnnotation(source, annotation, "HEAD"):
		return "HEAD"
	case isJAXRSAnnotation(source, annotation, "OPTIONS"):
		return "OPTIONS"
	default:
		return ""
	}
}

func jaxrsPathValues(annotation jvmAnnotation) ([]valueExpr, bool) {
	values, ok := strictJVMStringValues(firstJVMAnnotationArgument(annotation))
	if !ok {
		return nil, false
	}
	if len(values) == 0 {
		return []valueExpr{literalValue("")}, true
	}
	out := make([]valueExpr, 0, len(values))
	for _, value := range values {
		out = append(out, literalValue(value))
	}
	return out, true
}

func firstJVMAnnotationArgument(annotation jvmAnnotation) []jvmToken {
	if len(annotation.arguments) == 0 {
		return []jvmToken{{kind: jvmStringToken, text: `""`}}
	}
	return annotation.arguments[0].tokens
}
