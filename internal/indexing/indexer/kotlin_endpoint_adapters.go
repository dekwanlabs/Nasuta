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
	start int // token index
	line  int
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
			functions:  kotlinFunctionDeclarations(syntax.tokens),
			classes:    kotlinClassDeclarations(syntax.tokens),
		},
	}, true
}

// kotlinFunctionDeclarations finds fun declarations in JVM tokens.
// Kotlin uses "fun" keyword followed by identifier and open paren.
func kotlinFunctionDeclarations(tokens []jvmToken) []kotlinFunctionInfo {
	var functions []kotlinFunctionInfo
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken || token.text != "fun" {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].kind != jvmIdentifierToken {
			continue
		}
		// Skip if preceded by "class" keyword (function type parameter)
		if i > 0 && tokens[i-1].kind == jvmIdentifierToken &&
			(tokens[i-1].text == "class" || tokens[i-1].text == "interface") {
			continue
		}
		functions = append(functions, kotlinFunctionInfo{
			name:  tokens[i+1].text,
			start: i,
			line:  tokens[i].line,
		})
	}
	return functions
}

// kotlinClassDeclarations finds class/object declarations in JVM tokens.
func kotlinClassDeclarations(tokens []jvmToken) []kotlinClassInfo {
	var classes []kotlinClassInfo
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		switch token.text {
		case "class", "object":
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
		// Find the name (after keyword, skip modifiers)
		nameIdx := i + 1
		for nameIdx < len(tokens) && tokens[nameIdx].kind != jvmIdentifierToken {
			if tokens[nameIdx].text == "{" || tokens[nameIdx].text == ";" ||
				tokens[nameIdx].text == "(" {
				break
			}
			nameIdx++
		}
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
		for j := nameIdx + 1; j < len(tokens); j++ {
			if tokens[j].text == "{" && tokens[j].kind == jvmSymbolToken {
				body = j
				break
			}
			if tokens[j].text == ";" {
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
		classes = append(classes, kotlinClassInfo{
			name:      name,
			kind:      kind,
			start:     tokens[i].start,
			bodyStart: bodyEnd,
			bodyEnd:   bodyEnd,
			depth:     tokens[i].braceDepth,
		})
	}
	return classes
}

// kotlinEnclosingClass returns the class/object that contains the given offset.
func kotlinEnclosingClass(classes []kotlinClassInfo, offset, depth int) *kotlinClassInfo {
	var best *kotlinClassInfo
	bestSpan := 0
	for i := range classes {
		c := classes[i]
		if c.bodyStart < 0 || offset <= c.bodyStart || offset >= c.bodyEnd {
			continue
		}
		if depth < c.depth+1 {
			continue
		}
		span := c.bodyEnd - c.bodyStart
		if best == nil || span < bestSpan {
			best = &classes[i]
			bestSpan = span
		}
	}
	return best
}

// kotlinAnnotationController returns the class declaration for a @RestController
// or @Controller annotation, or nil.
func kotlinAnnotationController(source kotlinSource, annotation jvmAnnotation) *kotlinClassInfo {
	if !javaAnnotationIsController(annotation.name) {
		return nil
	}
	// Find the class/object declaration directly after the annotation.
	for _, class := range source.classes {
		if class.start > annotation.tokenEnd &&
			class.start <= annotation.tokenEnd+500 { // reasonable bound
			return &class
		}
	}
	return nil
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
			if javaAnnotationIsSpring(annotation.name) {
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
	// Reuse the Java Spring MVC scanning logic since annotations are JVM-common.
	// Build a jvmSource from the embedded data.
	jvmSrc := syntax.jvmSource

	// Controller bindings: find @RestController/@Controller on classes/objects.
	type controllerBinding struct {
		class     kotlinClassInfo
		prefixes  []valueExpr
		methods   []valueExpr
		resolved  bool
	}
	controllers := make(map[int]*controllerBinding)
	for _, annotation := range jvmSrc.annotations {
		if !javaAnnotationIsController(annotation.name) {
			continue
		}
		class := kotlinAnnotationController(syntax, annotation)
		if class == nil {
			continue
		}
		if _, exists := controllers[class.start]; exists {
			continue
		}
		controllers[class.start] = &controllerBinding{
			class:    *class,
			prefixes: []valueExpr{literalValue("")},
			methods:  []valueExpr{literalValue("ANY")},
			resolved:  true,
		}
	}
	if len(controllers) == 0 {
		return nil
	}

	// Process @RequestMapping on classes.
	for _, annotation := range jvmSrc.annotations {
		if annotation.name != "RequestMapping" {
			continue
		}
		class := kotlinAnnotationController(syntax, annotation)
		if class == nil {
			continue
		}
		ctrl := controllers[class.start]
		if ctrl == nil || !ctrl.resolved {
			continue
		}
		prefixes, pathsResolved := springMappingPaths(annotation)
		methods, methodsResolved := springMappingMethods(annotation)
		if !pathsResolved || !methodsResolved {
			ctrl.resolved = false
			ctrl.prefixes = []valueExpr{unresolvedValue(annotation.text)}
			ctrl.methods = []valueExpr{unresolvedValue(annotation.text)}
			continue
		}
		if ctrl.prefixes[0].kind != valueLiteral ||
			ctrl.prefixes[0].value != "" ||
			ctrl.methods[0].kind != valueLiteral ||
			ctrl.methods[0].value != "ANY" {
			ctrl.resolved = false
			ctrl.prefixes = []valueExpr{unresolvedValue(annotation.text)}
			ctrl.methods = []valueExpr{unresolvedValue(annotation.text)}
			continue
		}
		ctrl.prefixes = prefixes
		ctrl.methods = methods
	}

	// Process method mapping annotations.
	var candidates []endpointCandidate
	for _, annotation := range jvmSrc.annotations {
		if !isJavaMappingAnnotation(annotation.name) {
			continue
		}
		// Find enclosing controller.
		var ctrl *controllerBinding
		bestSpan := 0
		for _, c := range controllers {
			if !c.class.classContains(annotation.start, annotation.braceDepth) {
				continue
			}
			span := c.class.bodyEnd - c.class.bodyStart
			if ctrl == nil || span < bestSpan {
				ctrl = c
				bestSpan = span
			}
		}
		if ctrl == nil {
			continue
		}
		paths, pathsResolved := springMappingPaths(annotation)
		methods, methodsResolved := springMappingMethods(annotation)
		if !pathsResolved {
			paths = []valueExpr{unresolvedValue(annotation.text)}
		}
		if !methodsResolved {
			methods = []valueExpr{unresolvedValue(annotation.text)}
		}
		if !ctrl.resolved {
			paths = []valueExpr{unresolvedValue(annotation.text)}
		} else {
			paths = combinePathValues(ctrl.prefixes, paths)
			methods = combineMethodValues(ctrl.methods, methods)
		}
		if len(paths) == 0 {
			paths = []valueExpr{unresolvedValue(annotation.text)}
		}
		if len(methods) == 0 {
			methods = []valueExpr{unresolvedValue(annotation.text)}
		}
		// Find handler function name after annotation.
		handlerMethod := ""
		for _, fn := range syntax.functions {
			if fn.line > annotation.line && fn.line <= annotation.line+8 {
				handlerMethod = fn.name
				break
			}
		}
		candidates = append(candidates, sourceEndpointCandidate(
			source, "spring-mvc", methods, paths,
			ctrl.class.name, handlerMethod,
			annotation.line, 0.85,
		))
	}
	return candidates
}

func (c kotlinClassInfo) classContains(offset, depth int) bool {
	return c.bodyStart >= 0 && c.bodyEnd > 0 &&
		offset > c.bodyStart && offset < c.bodyEnd && depth == c.depth+1
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
