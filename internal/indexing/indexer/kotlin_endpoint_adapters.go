package indexer

import (
	"path/filepath"
	"strings"
)

// ---- Kotlin language frontend ----

// kotlinSource extends the JVM source with Kotlin-specific declaration data.
// Annotations and imports are inherited from the embedded jvmSource; Kotlin
// adds fun/class/object tracking since these differ from Java declarations.
type kotlinSource struct {
	jvmSource
	functions []kotlinFunctionInfo
	classes   []kotlinClassInfo
}

type kotlinFunctionInfo struct {
	name  string
	start int
	line  int
	depth int
}

type kotlinClassInfo struct {
	name      string
	kind      string // "class", "object", "companion object", "data class", "sealed class"
	start     int
	bodyStart int
	bodyEnd   int
	depth     int
}

func parseKotlinEndpointSource(root, file, text string) (endpointSource, bool) {
	syntax := scanJVMSource(text)
	if len(syntax.tokens) == 0 {
		return endpointSource{}, false
	}
	syntax.imports = extractKotlinImports(syntax.tokens)
	moduleRoot := findKotlinModuleRoot(root, file)
	modulePath := ""
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
	}
	return endpointSource{
		language:    "kotlin",
		root:        root,
		file:        file,
		rel:         relativeTo(root, file),
		repo:        topSegment(relativeTo(root, file)),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: inferKotlinServiceName(root, file),
		text:        text,
		syntax: kotlinSource{
			jvmSource: syntax,
			functions: kotlinFunctionDeclarations(syntax.tokens),
			classes:   kotlinClassDeclarations(syntax.tokens),
		},
	}, true
}

// Kotlin imports end at a newline, unlike Java imports. Keeping this parser in
// the Kotlin frontend also preserves aliases and wildcard framework imports.
func extractKotlinImports(tokens []jvmToken) map[string]string {
	imports := make(map[string]string)
	for i := 0; i < len(tokens); i++ {
		if tokens[i].text != "import" {
			continue
		}
		line := tokens[i].line
		parts := make([]string, 0, 6)
		alias := ""
		wildcard := false
		j := i + 1
		for ; j < len(tokens) && tokens[j].line == line; j++ {
			switch tokens[j].text {
			case ";":
				goto importDone
			case "as":
				if j+1 < len(tokens) && tokens[j+1].line == line &&
					tokens[j+1].kind == jvmIdentifierToken {
					alias = tokens[j+1].text
					j++
				}
			case "*":
				wildcard = true
			default:
				if tokens[j].kind == jvmIdentifierToken {
					parts = append(parts, tokens[j].text)
				}
			}
		}
	importDone:
		if len(parts) == 0 {
			continue
		}
		qualified := strings.Join(parts, ".")
		if wildcard {
			qualified += ".*"
			imports[qualified] = qualified
		} else if alias != "" {
			imports[alias] = qualified
		} else {
			imports[parts[len(parts)-1]] = qualified
		}
	}
	return imports
}

// kotlinFunctionDeclarations finds fun declarations in JVM tokens.
// Kotlin uses "fun" keyword followed by identifier and open paren.
func kotlinFunctionDeclarations(tokens []jvmToken) []kotlinFunctionInfo {
	var functions []kotlinFunctionInfo
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken || token.text != "fun" {
			continue
		}
		function, ok := kotlinFunctionDeclarationAt(tokens, i)
		if ok {
			functions = append(functions, function)
		}
	}
	return functions
}

func kotlinFunctionDeclarationAt(tokens []jvmToken, keyword int) (kotlinFunctionInfo, bool) {
	for i := keyword + 1; i < len(tokens); i++ {
		if tokens[i].braceDepth != tokens[keyword].braceDepth {
			return kotlinFunctionInfo{}, false
		}
		switch tokens[i].text {
		case "(":
			if i == 0 || tokens[i-1].kind != jvmIdentifierToken {
				return kotlinFunctionInfo{}, false
			}
			return kotlinFunctionInfo{
				name: tokens[i-1].text, start: tokens[keyword].start,
				line: tokens[keyword].line, depth: tokens[keyword].braceDepth,
			}, true
		case "=", "{", "}", ";":
			return kotlinFunctionInfo{}, false
		}
	}
	return kotlinFunctionInfo{}, false
}

// kotlinClassDeclarations finds class/object declarations in JVM tokens.
func kotlinClassDeclarations(tokens []jvmToken) []kotlinClassInfo {
	var classes []kotlinClassInfo
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		switch token.text {
		case "class", "interface", "object":
		default:
			continue
		}
		// "companion object" — treat as class-like
		kind := token.text
		if kind == "object" && i > 0 && tokens[i-1].kind == jvmIdentifierToken &&
			tokens[i-1].text == "companion" {
			kind = "companion object"
		}
		// "data class", "sealed class"
		if kind == "class" && i > 0 && tokens[i-1].kind == jvmIdentifierToken {
			prev := tokens[i-1].text
			if prev == "data" || prev == "sealed" || prev == "inner" ||
				prev == "abstract" || prev == "open" || prev == "enum" {
				kind = prev + " class"
			}
		}
		nameIdx := i + 1
		if nameIdx >= len(tokens) || tokens[nameIdx].kind != jvmIdentifierToken {
			continue
		}
		name := tokens[nameIdx].text
		// Skip if name is a keyword
		if name == "by" || name == "where" || name == "constructor" {
			continue
		}
		// Find body
		body := -1
		parenDepth, bracketDepth := 0, 0
		for j := nameIdx + 1; j < len(tokens); j++ {
			switch tokens[j].text {
			case "(":
				parenDepth++
			case ")":
				if parenDepth > 0 {
					parenDepth--
				}
			case "[":
				bracketDepth++
			case "]":
				if bracketDepth > 0 {
					bracketDepth--
				}
			}
			if tokens[j].text == "{" && tokens[j].kind == jvmSymbolToken &&
				parenDepth == 0 && bracketDepth == 0 {
				body = j
				break
			}
			if tokens[j].text == ";" && parenDepth == 0 && bracketDepth == 0 {
				break
			}
		}
		bodyEnd := -1
		if body >= 0 {
			close := matchingJVMDelimiter(tokens, body, "{", "}")
			if close >= 0 {
				bodyEnd = tokens[close].start
			}
		}
		if body < 0 || bodyEnd < 0 {
			continue
		}
		classes = append(classes, kotlinClassInfo{
			name:      name,
			kind:      kind,
			start:     tokens[i].start,
			bodyStart: tokens[body].start,
			bodyEnd:   bodyEnd,
			depth:     tokens[i].braceDepth,
		})
	}
	return classes
}

type kotlinDeclarationKind uint8

const (
	kotlinClassDeclaration kotlinDeclarationKind = iota + 1
	kotlinFunctionDeclaration
)

type kotlinDeclaration struct {
	kind                      kotlinDeclarationKind
	name                      string
	start, bodyStart, bodyEnd int
	depth                     int
}

type boundKotlinAnnotation struct {
	annotation  jvmAnnotation
	declaration kotlinDeclaration
}

func bindKotlinAnnotations(source kotlinSource) []boundKotlinAnnotation {
	bindings := make([]boundKotlinAnnotation, 0, len(source.annotations))
	for _, annotation := range source.annotations {
		declaration, ok := kotlinDeclarationAfter(source, annotation.tokenEnd, annotation.braceDepth)
		if ok {
			bindings = append(bindings, boundKotlinAnnotation{
				annotation: annotation, declaration: declaration,
			})
		}
	}
	return bindings
}

func kotlinDeclarationAfter(source kotlinSource, from, depth int) (kotlinDeclaration, bool) {
	classes := make(map[int]kotlinClassInfo, len(source.classes))
	for _, class := range source.classes {
		classes[class.start] = class
	}
	functions := make(map[int]kotlinFunctionInfo, len(source.functions))
	for _, function := range source.functions {
		functions[function.start] = function
	}
	for i := from; i < len(source.tokens); i++ {
		token := source.tokens[i]
		if token.braceDepth != depth {
			return kotlinDeclaration{}, false
		}
		if token.text == "@" {
			if _, next, ok := parseJVMAnnotation("", source.tokens, i); ok {
				i = next - 1
				continue
			}
		}
		if token.kind == jvmIdentifierToken {
			switch token.text {
			case "class", "interface", "object":
				if class, ok := classes[token.start]; ok {
					return kotlinDeclaration{
						kind: kotlinClassDeclaration, name: class.name, start: class.start,
						bodyStart: class.bodyStart, bodyEnd: class.bodyEnd, depth: class.depth,
					}, true
				}
				return kotlinDeclaration{}, false
			case "fun":
				if function, ok := functions[token.start]; ok {
					return kotlinDeclaration{
						kind: kotlinFunctionDeclaration, name: function.name,
						start: function.start, bodyStart: -1, bodyEnd: -1, depth: function.depth,
					}, true
				}
				return kotlinDeclaration{}, false
			case "val", "var", "typealias", "constructor", "init":
				return kotlinDeclaration{}, false
			}
			continue
		}
		switch token.text {
		case ";", "=", "{", "}", "(":
			return kotlinDeclaration{}, false
		}
	}
	return kotlinDeclaration{}, false
}

var kotlinSpringAnnotationNames = map[string]string{
	"RestController": "org.springframework.web.bind.annotation.RestController",
	"Controller":     "org.springframework.stereotype.Controller",
	"RequestMapping": "org.springframework.web.bind.annotation.RequestMapping",
	"GetMapping":     "org.springframework.web.bind.annotation.GetMapping",
	"PostMapping":    "org.springframework.web.bind.annotation.PostMapping",
	"PutMapping":     "org.springframework.web.bind.annotation.PutMapping",
	"DeleteMapping":  "org.springframework.web.bind.annotation.DeleteMapping",
	"PatchMapping":   "org.springframework.web.bind.annotation.PatchMapping",
}

func resolvedKotlinSpringAnnotation(source kotlinSource, annotation jvmAnnotation) (jvmAnnotation, bool) {
	qualified := annotation.qualifiedName
	if !strings.Contains(qualified, ".") {
		if imported := source.imports[annotation.name]; imported != "" {
			qualified = imported
		}
	}
	for name, expected := range kotlinSpringAnnotationNames {
		if qualified == expected ||
			(annotation.name == name && kotlinImports(source, expected)) {
			annotation.name = name
			return annotation, true
		}
	}
	return jvmAnnotation{}, false
}

func kotlinImports(source kotlinSource, qualified string) bool {
	simple := qualified[strings.LastIndex(qualified, ".")+1:]
	if imported, exists := source.imports[simple]; exists {
		return imported == qualified
	}
	for _, imported := range source.imports {
		if !strings.HasSuffix(imported, ".*") {
			continue
		}
		prefix := strings.TrimSuffix(imported, "*")
		if strings.HasPrefix(qualified, prefix) &&
			!strings.Contains(strings.TrimPrefix(qualified, prefix), ".") {
			return true
		}
	}
	return false
}

func kotlinSpringAnnotationBindings(source kotlinSource) []boundKotlinAnnotation {
	bindings := bindKotlinAnnotations(source)
	resolved := make([]boundKotlinAnnotation, 0, len(bindings))
	for _, binding := range bindings {
		annotation, ok := resolvedKotlinSpringAnnotation(source, binding.annotation)
		if !ok {
			continue
		}
		binding.annotation = annotation
		resolved = append(resolved, binding)
	}
	return resolved
}

// ---- Kotlin Spring MVC adapter ----

var kotlinSpringMVCAdapter = endpointAdapter{
	language:  "kotlin",
	framework: "spring-mvc",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(kotlinSource)
		if !ok {
			return false
		}
		for _, annotation := range syntax.annotations {
			if _, ok := resolvedKotlinSpringAnnotation(syntax, annotation); ok {
				return true
			}
		}
		return false
	},
	scan: scanKotlinSpringMVC,
}

func scanKotlinSpringMVC(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(kotlinSource)
	if !ok {
		return nil
	}
	bindings := kotlinSpringAnnotationBindings(syntax)

	// Controller bindings: find @RestController/@Controller on classes/objects.
	type controllerBinding struct {
		declaration kotlinDeclaration
		prefixes    []valueExpr
		methods     []valueExpr
		resolved    bool
	}
	controllers := make(map[int]*controllerBinding)
	for _, binding := range bindings {
		if !javaAnnotationIsController(binding.annotation.name) ||
			binding.declaration.kind != kotlinClassDeclaration {
			continue
		}
		if _, exists := controllers[binding.declaration.start]; exists {
			continue
		}
		controllers[binding.declaration.start] = &controllerBinding{
			declaration: binding.declaration,
			prefixes:    []valueExpr{literalValue("")},
			methods:     []valueExpr{literalValue("ANY")},
			resolved:    true,
		}
	}
	if len(controllers) == 0 {
		return nil
	}

	// Process @RequestMapping on classes.
	for _, binding := range bindings {
		if binding.annotation.name != "RequestMapping" ||
			binding.declaration.kind != kotlinClassDeclaration {
			continue
		}
		ctrl := controllers[binding.declaration.start]
		if ctrl == nil || !ctrl.resolved {
			continue
		}
		prefixes, pathsResolved := springMappingPaths(binding.annotation)
		methods, methodsResolved := springMappingMethods(binding.annotation)
		if !pathsResolved || !methodsResolved {
			ctrl.resolved = false
			ctrl.prefixes = []valueExpr{unresolvedValue(binding.annotation.text)}
			ctrl.methods = []valueExpr{unresolvedValue(binding.annotation.text)}
			continue
		}
		if ctrl.prefixes[0].kind != valueLiteral ||
			ctrl.prefixes[0].value != "" ||
			ctrl.methods[0].kind != valueLiteral ||
			ctrl.methods[0].value != "ANY" {
			ctrl.resolved = false
			ctrl.prefixes = []valueExpr{unresolvedValue(binding.annotation.text)}
			ctrl.methods = []valueExpr{unresolvedValue(binding.annotation.text)}
			continue
		}
		ctrl.prefixes = prefixes
		ctrl.methods = methods
	}

	// Process method mapping annotations.
	var candidates []endpointCandidate
	for _, binding := range bindings {
		if !isJavaMappingAnnotation(binding.annotation.name) ||
			binding.declaration.kind != kotlinFunctionDeclaration {
			continue
		}
		// Find enclosing controller.
		var ctrl *controllerBinding
		bestSpan := 0
		for _, c := range controllers {
			if !kotlinDeclarationDirectlyContains(
				c.declaration, binding.annotation.start, binding.annotation.braceDepth,
			) {
				continue
			}
			span := c.declaration.bodyEnd - c.declaration.bodyStart
			if ctrl == nil || span < bestSpan {
				ctrl = c
				bestSpan = span
			}
		}
		if ctrl == nil {
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
		if !ctrl.resolved {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		} else {
			paths = combinePathValues(ctrl.prefixes, paths)
			methods = combineMethodValues(ctrl.methods, methods)
		}
		if len(paths) == 0 {
			paths = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		if len(methods) == 0 {
			methods = []valueExpr{unresolvedValue(binding.annotation.text)}
		}
		candidates = append(candidates, sourceEndpointCandidate(
			source, "spring-mvc", methods, paths,
			ctrl.declaration.name, binding.declaration.name,
			binding.annotation.line, 0.85,
		))
	}
	return candidates
}

func kotlinDeclarationDirectlyContains(declaration kotlinDeclaration, offset, depth int) bool {
	return declaration.kind == kotlinClassDeclaration &&
		declaration.bodyStart >= 0 && declaration.bodyEnd > declaration.bodyStart &&
		offset > declaration.bodyStart && offset < declaration.bodyEnd &&
		depth == declaration.depth+1
}

// ---- Kotlin Ktor adapter ----

var kotlinKtorAdapter = endpointAdapter{
	language:  "kotlin",
	framework: "ktor",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(kotlinSource)
		if !ok {
			return false
		}
		for _, imported := range syntax.imports {
			if strings.HasPrefix(imported, "io.ktor.") {
				return true
			}
		}
		return false
	},
	scan: scanKotlinKtor,
}

func scanKotlinKtor(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(kotlinSource)
	if !ok {
		return nil
	}
	tokens := syntax.tokens
	var candidates []endpointCandidate
	type prefixEntry struct {
		value valueExpr
		end   int // closing brace token index
	}
	var prefixStack []prefixEntry

	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		// Pop expired prefix entries (closing brace passed).
		for len(prefixStack) > 0 && prefixStack[len(prefixStack)-1].end > 0 &&
			i > prefixStack[len(prefixStack)-1].end {
			prefixStack = prefixStack[:len(prefixStack)-1]
		}
		switch token.text {
		case "routing":
			if i+1 < len(tokens) && tokens[i+1].text == "{" {
				closeBrace := matchingJVMDelimiter(tokens, i+1, "{", "}")
				prefixStack = append(prefixStack, prefixEntry{
					value: literalValue(""),
					end:   closeBrace,
				})
			}
		case "route":
			if i+1 >= len(tokens) || tokens[i+1].text != "(" {
				continue
			}
			closeParen := matchingJVMDelimiter(tokens, i+1, "(", ")")
			if closeParen < 0 || closeParen+1 >= len(tokens) {
				continue
			}
			braceIdx := closeParen + 1
			if braceIdx < len(tokens) && tokens[braceIdx].text != "{" {
				if braceIdx+1 < len(tokens) && tokens[braceIdx+1].text == "{" {
					braceIdx++
				} else {
					continue
				}
			}
			routeExpr := resolveKtorArg(tokens, i+1, closeParen)
			endBrace := matchingJVMDelimiter(tokens, braceIdx, "{", "}")
			prefixStack = append(prefixStack, prefixEntry{
				value: routeExpr,
				end:   endBrace,
			})
		case "get", "post", "put", "delete", "patch", "head", "options":
			if i+1 >= len(tokens) || tokens[i+1].text != "(" {
				continue
			}
			closeParen := matchingJVMDelimiter(tokens, i+1, "(", ")")
			if closeParen < 0 {
				continue
			}
			method := strings.ToUpper(token.text)
			pathExpr := resolveKtorArg(tokens, i+1, closeParen)
			combinedPrefix := literalValue("")
			for _, entry := range prefixStack {
				combinedPrefix = joinPathValues(combinedPrefix, entry.value)
			}
			candidates = append(candidates, sourceEndpointCandidate(
				source, "ktor",
				[]valueExpr{literalValue(method)},
				[]valueExpr{joinPathValues(combinedPrefix, pathExpr)},
				filepath.Base(source.rel), "",
				token.line, 0.8,
			))
		}
	}
	return candidates
}

// resolveKtorArg resolves the first argument of a Ktor call: a string literal
// becomes a literal; anything else stays unresolved.
func resolveKtorArg(tokens []jvmToken, open, close int) valueExpr {
	args := tokens[open+1 : close]
	args = unwrapJVMContainer(args)
	if len(args) == 1 && args[0].kind == jvmStringToken {
		if value, ok := decodeJVMString(args[0].text); ok {
			return literalValue(value)
		}
	}
	// Could be a string template "${prefix}/path" — keep unresolved.
	if len(args) > 0 {
		return unresolvedValue(tokensText(args))
	}
	return literalValue("")
}

// tokensText joins token texts for diagnostic purposes.
func tokensText(tokens []jvmToken) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, t.text)
	}
	return strings.Join(parts, "")
}

// ---- Kotlin Javalin adapter ----

var kotlinJavalinAdapter = endpointAdapter{
	language:  "kotlin",
	framework: "javalin",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(kotlinSource)
		if !ok {
			return false
		}
		for _, imported := range syntax.imports {
			if strings.HasPrefix(imported, "io.javalin.") {
				return true
			}
		}
		return false
	},
	scan: scanKotlinJavalin,
}

func scanKotlinJavalin(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(kotlinSource)
	if !ok {
		return nil
	}
	tokens := syntax.jvmSource.tokens
	imports := syntax.jvmSource.imports

	// Track Javalin app instance variable names.
	javalinVars := make(map[string]struct{})
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken || token.text != "Javalin" {
			continue
		}
		// Javalin.create() → var = Javalin.create()
		if i+2 < len(tokens) && tokens[i+1].text == "." &&
			tokens[i+2].text == "create" {
			// Look backward for assignment target.
			for j := i - 1; j >= 0 && j >= i-5; j-- {
				if tokens[j].kind == jvmIdentifierToken &&
					tokens[j-1].text == "=" {
					javalinVars[tokens[j].text] = struct{}{}
					break
				}
			}
		}
		// val/var name = Javalin.create()
		if i+4 < len(tokens) && tokens[i+1].text == "." &&
			tokens[i+2].text == "create" {
			for j := i - 1; j >= 0 && j >= i-10; j-- {
				if tokens[j].kind == jvmIdentifierToken {
					if tokens[j-1].text == "=" {
						javalinVars[tokens[j].text] = struct{}{}
					}
					break
				}
			}
		}
	}

	var candidates []endpointCandidate
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		if i == 0 || tokens[i-1].text != "." {
			continue
		}
		receiver := ""
		if i >= 2 && tokens[i-2].kind == jvmIdentifierToken {
			receiver = tokens[i-2].text
		}
		// Check if this is a Javalin variable or "app" (common convention for Javalin).
		// Per architecture doc: variable name alone is not evidence. Must have import.
		method := token.text
		if _, isGet := methodHTTPMap[method]; !isGet {
			continue
		}
		// Verify the receiver is a known Javalin variable or import-backed.
		if _, ok := javalinVars[receiver]; !ok {
			// Check if inferred from import pattern.
			if _, hasJavalin := imports["io.javalin.Javalin"]; !hasJavalin {
				continue
			}
		}
		if i+1 >= len(tokens) || tokens[i+1].text != "(" {
			continue
		}
		close := matchingJVMDelimiter(tokens, i+1, "(", ")")
		if close < 0 {
			continue
		}
		pathExpr := resolveKtorArg(tokens, i+1, close)
		candidates = append(candidates, sourceEndpointCandidate(
			source, "javalin",
			[]valueExpr{literalValue(strings.ToUpper(method))},
			[]valueExpr{pathExpr},
			filepath.Base(source.rel), "",
			token.line, 0.75,
		))
	}
	return candidates
}

var methodHTTPMap = map[string]struct{}{
	"get": {}, "post": {}, "put": {}, "delete": {}, "patch": {},
	"head": {}, "options": {},
}
