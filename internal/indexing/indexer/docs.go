package indexer

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/internal/platform/store"
	"github.com/dekwanlabs/astris/log"
	"github.com/dekwanlabs/astris/platform"
	"gopkg.in/yaml.v3"
)

type frontmatter struct {
	data    map[string]any
	content string
}

var fmRe = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

func parseFrontmatter(raw string) frontmatter {
	m := fmRe.FindStringSubmatch(raw)
	if m == nil {
		return frontmatter{data: map[string]any{}, content: raw}
	}
	data := map[string]any{}
	_ = yaml.Unmarshal([]byte(m[1]), &data)
	if data == nil {
		data = map[string]any{}
	}
	return frontmatter{data: data, content: m[2]}
}

func fmString(data map[string]any, key string) string {
	if v, ok := data[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func fmStringArray(data map[string]any, key string) []string {
	v, ok := data[key]
	if !ok {
		return []string{}
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) != "" {
			return []string{strings.TrimSpace(t)}
		}
	}
	return []string{}
}

var titleRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)

func extractTitle(content string) string {
	if m := titleRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

var headingRe = regexp.MustCompile(`^##\s+\d*\.?\s*(.+?)\s*$`)

// extractSection returns the body under a `## <heading>` until the next `## `.
// This replaces the TS lookahead regex (unsupported by Go RE2) with an
// explicit section split.
func extractSection(content, heading string) string {
	lines := strings.Split(content, "\n")
	var buf []string
	capturing := false
	for _, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			if capturing {
				break
			}
			if strings.TrimSpace(m[1]) == heading {
				capturing = true
			}
			continue
		}
		if capturing {
			buf = append(buf, line)
		}
	}
	return strings.TrimSpace(strings.Join(buf, "\n"))
}

func extractSectionAny(content string, headings ...string) string {
	for _, h := range headings {
		if s := extractSection(content, h); s != "" {
			return s
		}
	}
	return ""
}

var inlineCodeRe = regexp.MustCompile("`([^`]+)`")
var servicePrefixRe = regexp.MustCompile(`^hsmf-|^hsas-|^hsds-|^cdp-`)

func extractInlineCodeList(text string) []string {
	var out []string
	for _, m := range inlineCodeRe.FindAllStringSubmatch(text, -1) {
		if servicePrefixRe.MatchString(m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

func inferServiceNameFromDoc(data map[string]any, title, filePath string) string {
	id := fmString(data, "id")
	if strings.HasPrefix(id, "service-") {
		return strings.TrimPrefix(id, "service-")
	}
	if strings.Contains(title, "：") {
		parts := strings.Split(title, "：")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	return strings.TrimSuffix(filepath.Base(filePath), ".md")
}

var entrypointRe = regexp.MustCompile(`Application\.java$|main\.py$|bootstrap\.yml$|application\.ya?ml$`)
var portDocRe = regexp.MustCompile(`(?i)(?:默认端口|port[^\n]*?)\D(\d{3,5})`)

// IndexServiceDocs parses .docs/services/*.md into service records.
func IndexServiceDocs(root string) []types.ServiceRecord {
	dir := filepath.Join(root, ".docs", "services")
	files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	var records []types.ServiceRecord
	for _, file := range files {
		if filepath.Base(file) == "index.md" {
			continue
		}
		raw := readFile(file)
		if raw == "" {
			continue
		}
		fm := parseFrontmatter(raw)
		title := extractTitle(fm.content)
		serviceName := inferServiceNameFromDoc(fm.data, title, file)
		sourceOfTruth := fmStringArray(fm.data, "source_of_truth")

		var entrypoints []types.Evidence
		for _, sp := range sourceOfTruth {
			if entrypointRe.MatchString(sp) {
				entrypoints = append(entrypoints, types.Evidence{Path: sp, Kind: types.SourceDoc})
			}
		}

		var ports []int
		for _, m := range portDocRe.FindAllStringSubmatch(fm.content, -1) {
			ports = appendPort(ports, m[1])
		}

		conf := 0.65
		if len(sourceOfTruth) > 0 {
			conf = 0.85
		}

		records = append(records, types.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          "docs",
			Layer:         "server",
			Scope:         layerOrDoc(fmString(fm.data, "scope"), serviceName, extractSection(fm.content, "服务定位")),
			ModulePath:    inferModulePathFromSOT(serviceName, sourceOfTruth),
			Language:      inferLanguage(sourceOfTruth),
			Runtime:       inferRuntime(fm.content),
			Owner:         fmString(fm.data, "owner"),
			Status:        fmString(fm.data, "status"),
			Tags:          fmStringArray(fm.data, "tags"),
			Docs:          []string{relativeTo(root, file)},
			SourceOfTruth: sourceOfTruth,
			Entrypoints:   entrypoints,
			Ports:         platform.Dedupe(ports),
			Summary:       firstNonEmptyLine(extractSection(fm.content, "TL;DR")),
			Confidence:    conf,
		})
	}
	return records
}

func appendPort(ports []int, s string) []int {
	if n, err := strconv.Atoi(s); err == nil {
		return append(ports, n)
	}
	return ports
}

func layerOrDoc(scope, serviceName, servicePosition string) string {
	if scope != "" {
		return scope
	}
	if l := inferLayer(serviceName, ""); l != "" {
		return l
	}
	for _, layer := range knownLayers {
		if strings.Contains(servicePosition, layer) {
			return layer
		}
	}
	return ""
}

func inferModulePathFromSOT(serviceName string, sourceOfTruth []string) string {
	for _, item := range sourceOfTruth {
		p := strings.ReplaceAll(item, "\\", "/")
		if strings.Contains(p, "/"+serviceName+"/") || strings.HasSuffix(p, "/"+serviceName) {
			parts := strings.Split(p, "/")
			for i, part := range parts {
				if part == serviceName {
					if i < 1 {
						return serviceName
					}
					return parts[i-1] + "/" + parts[i]
				}
			}
		}
	}
	return ""
}

func inferLanguage(sourceOfTruth []string) string {
	for _, item := range sourceOfTruth {
		switch {
		case strings.HasSuffix(item, ".java") || strings.Contains(item, "/src/main/java/"):
			return "java"
		case strings.HasSuffix(item, ".kt") || strings.HasSuffix(item, ".kts") || strings.HasSuffix(item, "build.gradle.kts"):
			return "kotlin"
		case strings.HasSuffix(item, ".py") || strings.HasSuffix(item, "requirements.txt"):
			return "python"
		case strings.HasSuffix(item, ".go") || strings.HasSuffix(item, "go.mod"):
			return "go"
		case strings.HasSuffix(item, ".rs") || strings.HasSuffix(item, "Cargo.toml"):
			return "rust"
		case strings.HasSuffix(item, ".swift") || strings.HasSuffix(item, "Package.swift"):
			return "swift"
		case strings.HasSuffix(item, ".m") || strings.HasSuffix(item, ".mm"):
			return "objective-c"
		case strings.HasSuffix(item, ".dart") || strings.HasSuffix(item, "pubspec.yaml"):
			return "dart"
		case strings.HasSuffix(item, ".cpp") || strings.HasSuffix(item, ".cc") || strings.HasSuffix(item, ".cxx") ||
			strings.HasSuffix(item, ".hpp") || strings.HasSuffix(item, ".hxx"):
			return "cpp"
		case strings.HasSuffix(item, ".c") || strings.HasSuffix(item, ".h"):
			return "c"
		case strings.HasSuffix(item, ".cs") || strings.HasSuffix(item, ".csproj"):
			return "csharp"
		case strings.HasSuffix(item, ".js") || strings.HasSuffix(item, ".ts") || strings.HasSuffix(item, "package.json"):
			return "nodejs"
		case strings.HasSuffix(item, ".rb"):
			return "ruby"
		case strings.HasSuffix(item, ".php"):
			return "php"
		}
	}
	return "unknown"
}

var (
	fastapiRe    = regexp.MustCompile(`(?i)FastAPI|Uvicorn|Gunicorn`)
	springBootRe = regexp.MustCompile(`Spring Boot|@SpringBootApplication|Feign`)
)

func inferRuntime(content string) string {
	if fastapiRe.MatchString(content) {
		return "fastapi"
	}
	if springBootRe.MatchString(content) {
		return "spring-boot"
	}
	return ""
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func filterOut(items []string, exclude string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != exclude {
			out = append(out, item)
		}
	}
	return out
}

// IndexKnowledgeBaseFromDocStore reads flow and schema docs from MySQL.
// The DocStore is the sole runbook source.
// It returns nil on missing docStore or error so retrieval can degrade gracefully.
func IndexKnowledgeBaseFromDocStore(docStore *store.DocStore) []types.RunbookRecord {
	if docStore == nil {
		return nil
	}
	docs, err := docStore.ListDocsByKinds(types.KnowledgeDocKinds)
	if err != nil {
		log.Warnf("[indexer] failed to read KB docs from DocStore: %v", err)
		return nil
	}
	if len(docs) == 0 {
		return nil
	}
	var records []types.RunbookRecord
	for _, d := range docs {
		fm := parseFrontmatter(d.Content)
		id := fmString(fm.data, "id")
		if id == "" {
			id = d.ID
		}
		title := extractTitle(fm.content)
		if title == "" {
			title = d.Title
		}
		// scope mirrors DocStore kind verbatim.
		// The markdown frontmatter "scope" field is ignored as a legacy alias.
		// kind is now the single source of truth.
		scope := d.Kind
		records = append(records, types.RunbookRecord{
			ID:         id,
			Repo:       "docs",
			Title:      title,
			Path:       d.Filename,
			Scope:      scope,
			Tags:       fmStringArray(fm.data, "tags"),
			Text:       fm.content,
			Confidence: 1,
		})
	}
	return records
}

// RunbookEdgesFromDocStore extracts service dependency edges declared in runbook
// frontmatter. A runbook names its subject via `service:`, then `depends_on:`
// lists targets (subject → target) and `called_by:` lists callers (caller →
// subject). Runbook-declared edges fill gaps the code scanners miss (App→Server,
// cross-protocol, name mismatches). No subject → no edges (nothing to anchor).
func RunbookEdgesFromDocStore(docStore *store.DocStore) []types.DependencyEdge {
	if docStore == nil {
		return nil
	}
	docs, err := docStore.ListDocsByKinds(types.KnowledgeDocKinds)
	if err != nil {
		return nil
	}
	var edges []types.DependencyEdge
	for _, d := range docs {
		fm := parseFrontmatter(d.Content)
		subject := fmString(fm.data, "service")
		if subject == "" {
			continue
		}
		ev := []types.Evidence{{Path: d.Filename, Kind: types.SourceDoc}}
		for _, target := range fmStringArray(fm.data, "depends_on") {
			edges = append(edges, types.DependencyEdge{
				From: subject, To: target, Type: types.EdgeRunbook, Evidence: ev, Confidence: 0.9,
			})
		}
		for _, caller := range fmStringArray(fm.data, "called_by") {
			edges = append(edges, types.DependencyEdge{
				From: caller, To: subject, Type: types.EdgeRunbook, Evidence: ev, Confidence: 0.9,
			})
		}
	}
	return edges
}
