package indexer

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/platform"
)

// langByExt maps file extensions to a language label for semantic code chunks.
var langByExt = map[string]string{
	".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".scala": "scala", ".groovy": "groovy",
	".py": "python", ".go": "go", ".rs": "rust",
	".ts": "typescript", ".tsx": "typescript", ".js": "javascript", ".jsx": "javascript", ".vue": "vue",
	".swift": "swift",
	".m":     "objective-c", ".mm": "objective-c++",
	".dart": "dart",
	".c":    "c", ".h": "c",
	".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".c++": "cpp",
	".hpp": "cpp", ".hxx": "cpp", ".h++": "cpp",
	".cs":  "csharp",
	".rb":  "ruby",
	".php": "php", ".phtml": "php",
	".lua": "lua",
	".r":   "r", ".R": "r",
	".pl": "perl", ".pm": "perl",
	".sql": "sql",
	".yml": "yaml", ".yaml": "yaml", ".properties": "properties", ".xml": "xml", ".json": "json", ".toml": "toml",
	".md": "markdown", ".http": "http", ".sh": "shell", ".proto": "protobuf",
	".vm": "velocity", ".ftl": "freemarker", ".html": "html",
}

// IsIndexableFile reports whether a filename has an extension we index.
func IsIndexableFile(name string) bool {
	_, ok := langByExt[strings.ToLower(filepath.Ext(name))]
	return ok
}

// IsSparseIndexableFile limits lexical indexing to source and interface files.
// Configuration, markup, and documentation remain available to dense search
// without allocating low-value sparse vocabulary coordinates.
func IsSparseIndexableFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return sparseCodeExts[ext]
}

var sparseCodeExts = map[string]bool{
	".java": true, ".kt": true, ".kts": true, ".scala": true, ".groovy": true,
	".py": true, ".go": true, ".rs": true,
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".vue": true,
	".swift": true, ".m": true, ".mm": true, ".dart": true,
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true,
	".c++": true, ".hpp": true, ".hxx": true, ".h++": true,
	".cs": true, ".rb": true, ".php": true, ".phtml": true,
	".lua": true, ".r": true, ".pl": true, ".pm": true,
	".sql": true, ".sh": true, ".proto": true,
}

const (
	maxChunkFileBytes = 256 * 1024 // skip very large files
	chunkLines        = 80         // lines per chunk (line-window fallback)
	chunkOverlap      = 15         // overlapping lines between chunks
	maxMethodLines    = 200        // methods larger than this are sub-windowed
)

// noiseDirSegments are path segments whose files are test or fixture noise.
var noiseDirSegments = []string{
	"/src/test/", "/test/", "/tests/", "/__tests__/", "/testdata/",
	"/fixtures/", "/fixture/", "/mock/", "/mocks/", "/e2e/", "/snapshots/",
}

// noiseNamePatterns are filename substrings indicating generated/derived files.
var noiseNamePatterns = []string{
	"generated", ".min.", ".pb.go", "_pb.go", ".g.dart", "_generated",
	".pb.cc", ".pb.h", "_test.dart", ".g.cs", ".designer.cs", ".generated.cs",
}

// noiseExts are file extensions that remain structurally indexed but are not
// embedded.
var noiseExts = map[string]bool{
	".properties": true, // i18n bundles (often non-UTF-8 encoding → panic risk)
	".json":       true, // test fixtures, package.json, generated JSON
	".http":       true, // API test files
	".html":       true, // template/static HTML (not source)
	".toml":       true, // Cargo.toml / pyproject.toml (build config)
	".vm":         true, // Apache Velocity templates
	".ftl":        true, // FreeMarker templates
}

// isNoiseFile reports whether a repo-relative path should not be embedded.
func isNoiseFile(rel string) bool {
	if strings.HasPrefix(filepath.Base(rel), platform.WorkspaceMetadataDir) {
		return true
	}
	if noiseExts[strings.ToLower(filepath.Ext(rel))] {
		return true
	}
	p := "/" + strings.ToLower(toPosix(rel)) + "/"
	for _, seg := range noiseDirSegments {
		if strings.Contains(p, seg) {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(rel))
	for _, pat := range noiseNamePatterns {
		if strings.Contains(base, pat) {
			return true
		}
	}
	return false
}

// ScanCodeChunks walks the scan dirs and splits eligible files for semantic
// embedding.
func ScanCodeChunks(root string, dirs []string) []domain.CodeChunk {
	files := walkFiles(root, dirs, IsIndexableFile)
	methodsByFile := loadCodegraphRanges(root)

	var chunks []domain.CodeChunk
	for _, file := range files {
		rel := relativeTo(root, file)
		if isNoiseFile(rel) {
			continue
		}
		size, err := statSize(file)
		if err != nil || size > maxChunkFileBytes {
			continue
		}
		text := readFile(file)
		if text == "" {
			continue
		}
		lang := langByExt[strings.ToLower(filepath.Ext(file))]
		repo := topSegment(rel)
		if nodes := methodsByFile[toPosix(rel)]; len(nodes) > 0 {
			chunks = append(chunks, chunkByNodes(rel, repo, lang, text, nodes)...)
		} else {
			chunks = append(chunks, chunkFile(rel, repo, lang, text)...)
		}
	}
	return chunks
}

// loadCodegraphRanges returns method/function node ranges grouped by posix path.
func loadCodegraphRanges(root string) map[string][]codegraph.Node {
	db, err := codegraph.Open(root)
	if err != nil || db == nil {
		return nil
	}
	defer db.Close()
	nodes, err := db.ListChunkNodes()
	if err != nil || len(nodes) == 0 {
		return nil
	}
	m := make(map[string][]codegraph.Node, 4096)
	for _, n := range nodes {
		key := toPosix(n.FilePath)
		m[key] = append(m[key], n)
	}
	return m
}

// chunkByNodes emits one chunk per method/function and sub-windows oversized
// methods.
func chunkByNodes(path, repo, lang, text string, nodes []codegraph.Node) []domain.CodeChunk {
	lines := strings.Split(text, "\n")
	n := len(lines)
	var out []domain.CodeChunk
	for _, node := range nodes {
		s, e := node.StartLine, node.EndLine
		if s < 1 {
			s = 1
		}
		if e > n {
			e = n
		}
		if e < s {
			continue
		}
		header := codeHeader(lang, node, path)
		if e-s+1 > maxMethodLines {
			for start := s; start <= e; start += (chunkLines - chunkOverlap) {
				end := start + chunkLines - 1
				if end > e {
					end = e
				}
				body := strings.TrimSpace(strings.Join(lines[start-1:end], "\n"))
				if body != "" {
					out = append(out, domain.CodeChunk{Path: path, Repo: repo, Lang: lang,
						StartLine: start, EndLine: end, Text: header + body})
				}
				if end == e {
					break
				}
			}
			continue
		}
		body := strings.TrimSpace(strings.Join(lines[s-1:e], "\n"))
		if body != "" {
			out = append(out, domain.CodeChunk{Path: path, Repo: repo, Lang: lang,
				StartLine: s, EndLine: e, Text: header + body})
		}
	}
	return out
}

// codeHeader builds a short context prefix for a method chunk.
func codeHeader(lang string, node codegraph.Node, path string) string {
	name := node.QualifiedName
	if name == "" {
		name = node.Name
	}
	h := fmt.Sprintf("// %s %s: %s\n// %s:%d-%d\n", lang, node.Kind, name, path, node.StartLine, node.EndLine)
	if sig := strings.TrimSpace(node.Signature); sig != "" {
		h += "// " + sig + "\n"
	}
	return h
}

func chunkFile(path, repo, lang, text string) []domain.CodeChunk {
	lines := strings.Split(text, "\n")
	var out []domain.CodeChunk
	step := chunkLines - chunkOverlap

	for start := 0; start < len(lines); start += step {
		end := start + chunkLines
		if end > len(lines) {
			end = len(lines)
		}
		body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		if body != "" {
			out = append(out, domain.CodeChunk{
				Path:      path,
				Repo:      repo,
				Lang:      lang,
				StartLine: start + 1,
				EndLine:   end,
				Text:      body,
			})
		}
		if end == len(lines) {
			break
		}
	}
	return out
}

func topSegment(rel string) string {
	p := toPosix(rel)
	// Projects are checked out under repos/<group>/<project> — strip the
	// prefix and return the group/project pair as the repo identifier
	// (e.g. "hsds/hsds-cookbook" from "repos/hsds/hsds-cookbook/src/...").
	p, _ = strings.CutPrefix(p, "repos/")
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return ""
}

// repoFromPath converts repos/group/project/... into the repository key used
// by incremental SQLite cleanup. Evidence outside repos/ has no code repo.
func repoFromPath(path string) string {
	p := strings.Trim(filepath.ToSlash(path), "/")
	p, _ = strings.CutPrefix(p, "./")
	p, ok := strings.CutPrefix(p, "repos/")
	if !ok {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
