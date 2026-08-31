package indexer

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// activateFeignCalls turns Feign declarations into dependency candidates only
// when application code invokes a method on a value whose declared type is the
// Feign interface. Maven reachability is used only to assign a library call
// site to its deployable runtime consumer; it never activates a dependency.
func activateFeignCalls(
	root string,
	dirs []string,
	services []domain.ServiceRecord,
	refs []feignReference,
) []feignReference {
	if len(refs) == 0 {
		return nil
	}
	bySimpleName := make(map[string][]feignReference)
	for _, ref := range refs {
		if ref.InterfaceName == "" || len(ref.Methods) == 0 {
			continue
		}
		bySimpleName[ref.InterfaceName] = append(bySimpleName[ref.InterfaceName], ref)
	}
	if len(bySimpleName) == 0 {
		return nil
	}

	runtimeByPath := make(map[string]serviceIdentity)
	serviceByPath := make(map[string]serviceIdentity)
	for _, service := range services {
		path := canonicalPath(service.ModulePath)
		identity := serviceIdentityFromRecord(service)
		serviceByPath[path] = identity
		if service.Runtime == "spring-boot" {
			runtimeByPath[path] = identity
		}
	}
	consumers := mavenRuntimeConsumers(root, dirs, services)

	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".java") || strings.HasSuffix(name, ".kt")
	})
	var activated []feignReference
	for _, file := range files {
		rel := relativeTo(root, file)
		if isTestSourcePath(rel) {
			continue
		}
		text := readFile(file)
		source := scanJVMSource(text)
		packageName := jvmPackageName(source.tokens)
		moduleRoot := findJavaModuleRoot(root, file)
		if strings.HasSuffix(file, ".kt") {
			moduleRoot = findKotlinModuleRoot(root, file)
		}
		modulePath := canonicalPath(relativeTo(root, moduleRoot))
		owners := runtimeOwnersForModule(
			modulePath,
			runtimeByPath,
			serviceByPath,
			consumers,
			dependencyIdentity(root, file),
		)

		for simpleName, candidates := range bySimpleName {
			if !jvmSourceMentionsType(source.tokens, simpleName) {
				continue
			}
			for _, ref := range candidates {
				if len(ref.Evidence) > 0 && ref.Evidence[0].Path == rel {
					continue
				}
				if !jvmFeignTypeVisible(source, packageName, ref) {
					continue
				}
				bindings := jvmVariablesOfType(source.tokens, simpleName)
				if len(bindings) == 0 {
					continue
				}
				for _, call := range jvmCallsOnBindings(source.tokens, bindings, ref.Methods) {
					callEvidence := domain.Evidence{
						Path: rel, Line: call.line,
						Symbol: call.receiver + "." + call.method,
						Kind:   domain.SourceCodeScan,
					}
					methodEvidence := ref.Methods[call.method]
					for _, owner := range owners {
						expanded := ref
						expanded.From = owner.Name
						expanded.CallerServiceKey = owner.Key
						expanded.ModulePath = owner.ModulePath
						expanded.Evidence = []domain.Evidence{callEvidence, methodEvidence}
						expanded.Evidence = append(expanded.Evidence, ref.Evidence...)
						activated = append(activated, expanded)
					}
				}
			}
		}
	}
	return activated
}

func runtimeOwnersForModule(
	modulePath string,
	runtimeByPath map[string]serviceIdentity,
	serviceByPath map[string]serviceIdentity,
	consumers map[string][]serviceIdentity,
	fallback serviceIdentity,
) []serviceIdentity {
	if owner, ok := runtimeByPath[modulePath]; ok {
		return []serviceIdentity{owner}
	}
	if owners := consumers[modulePath]; len(owners) > 0 {
		return owners
	}
	if owner, ok := serviceByPath[modulePath]; ok {
		return []serviceIdentity{owner}
	}
	if fallback.Name == "" {
		return nil
	}
	return []serviceIdentity{fallback}
}

func jvmFeignTypeVisible(source jvmSource, packageName string, ref feignReference) bool {
	if imported, ok := source.imports[ref.InterfaceName]; ok {
		return imported == ref.QualifiedName
	}
	if ref.PackageName != "" && packageName == ref.PackageName {
		return true
	}
	for _, imported := range source.imports {
		if strings.HasSuffix(imported, ".*") &&
			strings.TrimSuffix(imported, ".*") == ref.PackageName {
			return true
		}
	}
	// Default-package fixtures and legacy sources without package declarations
	// are still unambiguous when both sides use the default package.
	return packageName == "" && ref.PackageName == ""
}

func jvmSourceMentionsType(tokens []jvmToken, name string) bool {
	for _, token := range tokens {
		if token.kind == jvmIdentifierToken && token.text == name {
			return true
		}
	}
	return false
}

func jvmVariablesOfType(tokens []jvmToken, typeName string) map[string]struct{} {
	variables := make(map[string]struct{})
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken || token.text != typeName {
			continue
		}
		// Kotlin property/parameter: private val client: UserFeign
		if i >= 2 && tokens[i-1].text == ":" && tokens[i-2].kind == jvmIdentifierToken {
			variables[tokens[i-2].text] = struct{}{}
		}
		// Java field/parameter/local: UserFeign client
		if i+1 < len(tokens) && tokens[i+1].kind == jvmIdentifierToken {
			variables[tokens[i+1].text] = struct{}{}
		}
	}
	return variables
}

type jvmBoundCall struct {
	receiver string
	method   string
	line     int
}

func jvmCallsOnBindings(
	tokens []jvmToken,
	bindings map[string]struct{},
	methods map[string]domain.Evidence,
) []jvmBoundCall {
	var calls []jvmBoundCall
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		if _, ok := methods[token.text]; !ok {
			continue
		}
		if i+1 >= len(tokens) || tokens[i+1].text != "(" || i < 2 || tokens[i-1].text != "." {
			continue
		}
		receiverIndex := i - 2
		if tokens[receiverIndex].text == "?" && receiverIndex > 0 {
			receiverIndex--
		}
		receiver := tokens[receiverIndex].text
		if _, ok := bindings[receiver]; !ok {
			continue
		}
		calls = append(calls, jvmBoundCall{
			receiver: receiver,
			method:   token.text,
			line:     token.line,
		})
	}
	return calls
}

func jvmPackageName(tokens []jvmToken) string {
	for i, token := range tokens {
		if token.text != "package" {
			continue
		}
		var parts []string
		for i++; i < len(tokens) && tokens[i].text != ";"; i++ {
			if tokens[i].kind == jvmIdentifierToken {
				parts = append(parts, tokens[i].text)
			}
		}
		return strings.Join(parts, ".")
	}
	return ""
}

func javaFeignMethods(
	source jvmSource,
	declaration javaDeclaration,
	interfaceName string,
	rel string,
) map[string]domain.Evidence {
	methods := make(map[string]domain.Evidence)
	for _, annotation := range source.annotations {
		if !isJavaMappingAnnotation(annotation.name) ||
			!javaDeclarationDirectlyContains(declaration, annotation.start, annotation.braceDepth) {
			continue
		}
		method, ok := javaDeclarationAfter(
			source.tokens, annotation.tokenEnd, annotation.braceDepth,
		)
		if !ok || method.kind != javaMethodDeclaration {
			continue
		}
		methods[method.name] = domain.Evidence{
			Path: rel, Line: annotation.line,
			Symbol: interfaceName + "." + method.name,
			Kind:   domain.SourceCodeScan,
		}
	}
	return methods
}

var (
	kotlinInterfaceRe   = regexp.MustCompile(`\binterface\s+([A-Za-z_$][\w$]*)`)
	kotlinFunNameRe     = regexp.MustCompile(`\bfun\s+([A-Za-z_$][\w$]*)\s*\(`)
	javaStringConstRe   = regexp.MustCompile(`\bString\s+([A-Za-z_$][\w$]*)\s*=\s*("(?:\\.|[^"\\])*")`)
	kotlinStringConstRe = regexp.MustCompile(
		`\bconst\s+val\s+([A-Za-z_$][\w$]*)\s*=\s*("(?:\\.|[^"\\])*")`,
	)
)

func kotlinFeignMethods(text, interfaceName, rel string) map[string]domain.Evidence {
	lines := strings.Split(stripJVMComments(text), "\n")
	methods := make(map[string]domain.Evidence)
	for i, line := range lines {
		if !strings.Contains(line, "@GET") && !strings.Contains(line, "@POST") &&
			!strings.Contains(line, "@PUT") && !strings.Contains(line, "@DELETE") &&
			!strings.Contains(line, "@PATCH") && !strings.Contains(line, "Mapping") {
			continue
		}
		end := min(i+10, len(lines))
		for j := i; j < end; j++ {
			if match := kotlinFunNameRe.FindStringSubmatch(lines[j]); match != nil {
				methods[match[1]] = domain.Evidence{
					Path: rel, Line: i + 1,
					Symbol: interfaceName + "." + match[1],
					Kind:   domain.SourceCodeScan,
				}
				break
			}
		}
	}
	return methods
}

// scanJVMStringConstants resolves only literal compile-time strings. This is
// enough for common Feign service constants such as
// SysConstants.APPLICATION_NAME, while computed values remain unresolved.
func scanJVMStringConstants(root string, dirs []string) map[string]string {
	values := make(map[string]string)
	simple := make(map[string]string)
	ambiguous := make(map[string]struct{})
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".java") || strings.HasSuffix(name, ".kt")
	})
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := stripJVMComments(readFile(file))
		source := scanJVMSource(text)
		pkg := jvmPackageName(source.tokens)
		typeName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		pattern := javaStringConstRe
		if strings.HasSuffix(file, ".kt") {
			pattern = kotlinStringConstRe
		}
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			value, err := strconv.Unquote(match[2])
			if err != nil {
				continue
			}
			typeKey := typeName + "." + match[1]
			qualified := typeKey
			if pkg != "" {
				qualified = pkg + "." + typeKey
			}
			values[qualified] = value
			if previous, exists := simple[typeKey]; exists && previous != value {
				ambiguous[typeKey] = struct{}{}
			} else {
				simple[typeKey] = value
			}
		}
	}
	for key, value := range simple {
		if _, conflict := ambiguous[key]; !conflict {
			values[key] = value
		}
	}
	return values
}

func feignStringArgument(
	annotation jvmAnnotation,
	name string,
	source jvmSource,
	constants map[string]string,
) (value string, present, resolved bool) {
	argument, ok := jvmNamedAnnotationArgument(annotation, name)
	if !ok {
		return "", false, true
	}
	value, ok = staticJVMString(argument.tokens, source.imports, constants)
	return value, true, ok
}

func feignClientName(
	annotation jvmAnnotation,
	source jvmSource,
	constants map[string]string,
) (string, bool) {
	if value, present, resolved := feignStringArgument(annotation, "name", source, constants); present {
		return value, resolved
	}
	if value, present, resolved := feignStringArgument(annotation, "value", source, constants); present {
		return value, resolved
	}
	for _, argument := range annotation.arguments {
		if argument.name == "" {
			value, ok := staticJVMString(argument.tokens, source.imports, constants)
			return value, ok
		}
	}
	return "", true
}

func staticJVMString(
	tokens []jvmToken,
	imports map[string]string,
	constants map[string]string,
) (string, bool) {
	tokens = unwrapJVMContainer(tokens)
	if len(tokens) == 0 {
		return "", false
	}
	parts := splitTopLevelJVMTokens(tokens, "+")
	var out strings.Builder
	for _, part := range parts {
		part = unwrapJVMContainer(part)
		if len(part) == 1 && part[0].kind == jvmStringToken {
			value, ok := decodeJVMString(part[0].text)
			if !ok {
				return "", false
			}
			out.WriteString(value)
			continue
		}
		identifiers := make([]string, 0, len(part))
		for i, token := range part {
			if i%2 == 0 {
				if token.kind != jvmIdentifierToken {
					return "", false
				}
				identifiers = append(identifiers, token.text)
			} else if token.text != "." {
				return "", false
			}
		}
		if len(identifiers) == 0 {
			return "", false
		}
		key := strings.Join(identifiers, ".")
		if len(identifiers) == 1 {
			if imported, ok := imports[identifiers[0]]; ok {
				key = imported
			}
		} else if imported, ok := imports[identifiers[0]]; ok {
			key = imported + "." + strings.Join(identifiers[1:], ".")
		}
		value, ok := constants[key]
		if !ok {
			return "", false
		}
		out.WriteString(value)
	}
	return out.String(), true
}
