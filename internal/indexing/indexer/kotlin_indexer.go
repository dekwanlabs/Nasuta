package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanKotlinServices registers each build module containing Kotlin source so
// controllers and clients in library modules keep service ownership.
func scanKotlinServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, hasSuffix(".kt"))
	var records []domain.ServiceRecord
	byModule := make(map[string]int)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		rel := relativeTo(root, file)
		moduleRoot := findKotlinModuleRoot(root, file)
		modulePath := inferModulePathFromRel(rel)
		serviceName := filepath.Base(modulePath)
		if moduleRoot != "" {
			if readPackaging(readFile(filepath.Join(moduleRoot, "pom.xml"))) == "pom" {
				continue
			}
			if _, err := os.Stat(filepath.Join(moduleRoot, "src", "main", "AndroidManifest.xml")); err == nil {
				continue
			}
			modulePath = relativeTo(root, moduleRoot)
			serviceName = readKotlinArtifactID(moduleRoot)
		}
		moduleKey := canonicalPath(modulePath)
		idx, exists := byModule[moduleKey]
		if !exists {
			records = append(records, domain.ServiceRecord{
				ServiceName: serviceName,
				Repo:        topSegment(rel),
				Layer:       "",
				Scope:       "",
				ModulePath:  modulePath,
				Language:    "kotlin",
				Tags:        []string{"code-scan"},
				Docs:        []string{},
				Confidence:  0.7,
			})
			idx = len(records) - 1
			byModule[moduleKey] = idx
		}
		if !hasKotlinRuntimeEvidence(text) {
			continue
		}
		rec := &records[idx]
		rec.Runtime = "spring-boot"
		rec.SourceOfTruth = append(rec.SourceOfTruth, rel)
		rec.Entrypoints = append(rec.Entrypoints, domain.Evidence{Path: rel, Kind: domain.SourceCodeScan})
		rec.Ports = append(rec.Ports, readKotlinPorts(moduleRoot)...)
		rec.Confidence = 0.9
	}
	return records
}

// scanKotlinFeigns preserves Feign configuration until dependency resolution.
func scanKotlinFeigns(root string, dirs []string) []feignReference {
	files := walkFiles(root, dirs, hasSuffix(".kt"))
	constants := scanJVMStringConstants(root, dirs)
	var records []feignReference
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "@FeignClient") {
			continue
		}
		source := scanJVMSource(text)
		packageName := jvmPackageName(source.tokens)
		cleanText := stripJVMComments(text)
		rel := relativeTo(root, file)
		caller := inferKotlinServiceName(root, file)
		modulePath := ""
		callerServiceKey := ""
		if moduleRoot := findKotlinModuleRoot(root, file); moduleRoot != "" {
			modulePath = relativeTo(root, moduleRoot)
			callerServiceKey = serviceIdentityForModule(root, moduleRoot, caller).Key
		}
		iface := strings.TrimSuffix(filepath.Base(file), ".kt")
		if match := kotlinInterfaceRe.FindStringSubmatch(cleanText); match != nil {
			iface = match[1]
		}
		lines := strings.Split(cleanText, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "@FeignClient") {
				continue
			}
			var parsed *jvmAnnotation
			for index := range source.annotations {
				if source.annotations[index].name == "FeignClient" &&
					source.annotations[index].line == i+1 {
					parsed = &source.annotations[index]
					break
				}
			}
			if parsed == nil {
				continue
			}
			clientName, nameResolved := feignClientName(*parsed, source, constants)
			if !nameResolved {
				clientName = ""
			}
			targetURL, urlPresent, urlResolved := feignStringArgument(*parsed, "url", source, constants)
			if urlPresent && !urlResolved {
				targetURL = ""
			}
			if clientName == "" && targetURL == "" {
				continue
			}
			conf := 0.9
			if caller == "unknown" {
				conf = 0.65
			}
			records = append(records, feignReference{
				From: caller, CallerServiceKey: callerServiceKey, ModulePath: modulePath,
				PackageName: packageName, InterfaceName: iface,
				QualifiedName: strings.Trim(strings.Join([]string{packageName, iface}, "."), "."),
				ClientName:    clientName, URL: targetURL,
				Methods: kotlinFeignMethods(cleanText, iface, rel),
				Evidence: []domain.Evidence{{
					Path: rel, Line: i + 1, Symbol: iface, Kind: domain.SourceCodeScan,
				}},
				Confidence: conf,
			})
		}
	}
	return records
}

// ---- helpers ----

func findKotlinModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		for _, marker := range []string{"build.gradle.kts", "build.gradle", "pom.xml"} {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func readKotlinArtifactID(dir string) string {
	// Try gradle settings first
	text := readFile(filepath.Join(dir, "settings.gradle.kts"))
	if text == "" {
		text = readFile(filepath.Join(dir, "settings.gradle"))
	}
	if m := regexp.MustCompile(`rootProject\.name\s*=\s*"([^"]+)"`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	// Try pom.xml (some Kotlin projects use Maven)
	text = readFile(filepath.Join(dir, "pom.xml"))
	if text != "" {
		withoutParent := parentRe.ReplaceAllString(text, "")
		ownHeader := depSplitRe.Split(withoutParent, 2)[0]
		if m := artifactRe.FindStringSubmatch(ownHeader); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return filepath.Base(dir)
}

func inferKotlinServiceName(root, file string) string {
	moduleRoot := findKotlinModuleRoot(root, file)
	if moduleRoot != "" {
		return readKotlinArtifactID(moduleRoot)
	}
	return "unknown"
}

func readKotlinPorts(dir string) []int {
	if dir == "" {
		return nil
	}
	portRe := regexp.MustCompile(`port:\s*(?:\$\{port:)?(\d{3,5})`)
	candidates := []string{
		"src/main/resources/application.yml", "src/main/resources/application.yaml",
		"src/main/resources/bootstrap.yml", "src/main/resources/bootstrap.yaml",
	}
	for _, c := range candidates {
		text := readFile(filepath.Join(dir, c))
		if m := portRe.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return []int{n}
			}
		}
	}
	return nil
}
