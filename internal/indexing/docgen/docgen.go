package docgen

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

// Generator produces per-module documentation using LLM.
type Generator struct {
	cfg      config.Config
	docDB    *store.DocStore // MySQL document store; nil (no Core DB) → generation skipped
	llm      *llmClient
	platform *config.PlatformSettings
}

// docgenMaxTokens — documentation generation needs much more output budget than
// QA chat (large projects have 60+ endpoints). Override the global QA token limit.
const docgenMaxTokens = 8000

// New creates a Generator. docDB is the MySQL document store the generated
// module docs are written to; pass nil when MySQL is not configured (generation
// is then skipped — module docs require the document store).
func New(cfg config.Config, ps *config.PlatformSettings, docDB *store.DocStore) (*Generator, error) {
	if ps == nil {
		ps = &config.PlatformSettings{}
	}
	if ps.LLMProvider == "anthropic" {
		return nil, fmt.Errorf("doc generation does not support LLM provider %q", ps.LLMProvider)
	}
	return &Generator{
		cfg:      cfg,
		platform: ps,
		docDB:    docDB,
		llm:      newLLMClient(ps.LLMBaseURL, ps.LLMAPIKey, ps.LLMModel, docgenMaxTokens),
	}, nil
}

// GenerateDocs generates module documentation for the given directory roots.
func (g *Generator) GenerateDocs(ctx context.Context, roots []string) {
	_ = g.GenerateDocsChanged(ctx, roots)
}

// GenerateDocsChanged generates module documentation and reports whether at
// least one module document was actually created or updated. Hash-skipped
// modules do not count as changed, allowing scheduled indexing to avoid a
// redundant document embedding pass.
func (g *Generator) GenerateDocsChanged(ctx context.Context, roots []string) bool {
	if g.docDB == nil {
		log.Infof("[docgen] document store unavailable; skip documentation generation")
		return false
	}

	// Phase 1: each root is a project directory to generate docs for.
	type task struct{ group, docName, dir string }
	var tasks []task
	for _, root := range roots {
		// If root has a repos/ subdirectory, expand into individual repos.
		if reposDir := filepath.Join(root, "repos"); isDir(reposDir) {
			groups, _ := os.ReadDir(reposDir)
			for _, grp := range groups {
				if !grp.IsDir() || strings.HasPrefix(grp.Name(), ".") {
					continue
				}
				projects, _ := os.ReadDir(filepath.Join(reposDir, grp.Name()))
				for _, p := range projects {
					if !p.IsDir() || strings.HasPrefix(p.Name(), ".") {
						continue
					}
					tasks = append(tasks, task{grp.Name(), p.Name(), filepath.Join(reposDir, grp.Name(), p.Name())})
				}
			}
		} else {
			// Single project directory.
			name := filepath.Base(root)
			group := filepath.Base(filepath.Dir(root))
			if isDir(root) && !strings.HasPrefix(name, ".") {
				tasks = append(tasks, task{group, name, root})
			}
		}
	}

	// Phase 2: dispatch concurrently (6 at a time).
	if len(tasks) == 0 {
		for _, root := range roots {
			entries, _ := os.ReadDir(root)
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				suffix := ""
				if e.IsDir() {
					suffix = "/"
				}
				names = append(names, e.Name()+suffix)
			}
			if len(names) > 20 {
				names = names[:20]
			}
			log.Warnf("[docgen] no scannable modules in %s (top-level: %v)", root, names)
		}
		return false
	}
	log.Infof("[docgen] %d module(s) to generate across %d root(s)", len(tasks), len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)
	var changed atomic.Bool
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t task) {
			defer wg.Done()
			defer func() { <-sem }()
			if g.generateModule(ctx, t.group, t.docName, t.dir) {
				changed.Store(true)
			}
		}(t)
	}
	wg.Wait()
	return changed.Load()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (g *Generator) generateModule(ctx context.Context, group, name, dir string) bool {
	hash := hashModule(dir)
	id := indexer.DocID(name, group+"/"+name+".md")
	// Hash-skip: compare against the stored doc hash.
	if existing, err := g.docDB.GetDoc(id); err == nil {
		if h2, ok := extractDocHash([]byte(existing.Content)); ok && h2 == hash {
			log.Infof("[docgen] skip %s/%s (unchanged)", group, name)
			return false
		}
	}

	log.Infof("[docgen] reading %s/%s ...", group, name)
	filesCtx := collectProjectFiles(dir)
	fileTree := collectFileTree(dir)

	// Phase 1: quick classification (file tree only, short timeout).
	classifyPrompt := buildClassifyPrompt(fileTree)
	classifyCtx, classifyCancel := context.WithTimeout(ctx, 60*time.Second)
	classifyAnswer, err := g.llm.chat(classifyCtx, classifyPrompt)
	classifyCancel()
	projectType := "generic"
	if err == nil {
		if cr, e := parseClassifyJSON(classifyAnswer); e == nil && validTypes[cr.ProjectType] {
			projectType = cr.ProjectType
			log.Infof("[docgen] classify %s/%s → %s (confidence=%s)", group, name, cr.ProjectType, cr.Confidence)
		} else {
			log.Warnf("[docgen] classify %s/%s parse failed: %v, using generic", group, name, e)
		}
	} else {
		log.Warnf("[docgen] classify %s/%s LLM err: %v, using generic", group, name, err)
	}

	// Phase 2: send only the matching template + project files.
	templateName := "docgen_" + projectType
	generatePrompt := buildGeneratePrompt(filesCtx, templateName)
	log.Infof("[docgen] generating %s/%s (type=%s template=%s)...", group, name, projectType, templateName)

	genCtx, genCancel := context.WithTimeout(ctx, 300*time.Second)
	genAnswer, err := g.llm.chat(genCtx, generatePrompt)
	genCancel()
	if err != nil {
		log.Warnf("[docgen] LLM failed for %s/%s: %v", group, name, err)
		return false
	}
	if strings.TrimSpace(genAnswer) == "" {
		log.Warnf("[docgen] empty doc for %s/%s", group, name)
		return false
	}

	md := "<!-- hash:" + hash + " -->\n" + genAnswer
	log.Infof("[docgen] generated %s/%s (type=%s, %d chars)", group, name, projectType, len(md))

	now := time.Now().UTC().Format(time.RFC3339)
	chunkCount := len(indexer.ChunkMarkdown(id, name, stripDocHashLine(md), indexer.DefaultDocChunkConfig()))
	if err := g.docDB.InsertDoc(domain.DocRecord{
		ID:         id,
		Title:      name,
		Filename:   group + "/" + name + ".md",
		Kind:       domain.DocKindModule,
		Content:    md,
		ChunkCount: chunkCount,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		log.Infof("[docgen] insert %s/%s: %v", group, name, err)
		return false
	}
	log.Infof("[docgen] stored %s/%s.md (%d chars)", group, name, len(md))
	return true
}

// extractDocHash reads the `<!-- hash:XXX -->` marker from the head of a
// generated doc. Returns the bare hash (without the ` -->` suffix).
func extractDocHash(b []byte) (string, bool) {
	const pre = "<!-- hash:"
	if !bytes.HasPrefix(b, []byte(pre)) {
		return "", false
	}
	end := bytes.IndexByte(b, '\n')
	if end < 0 {
		return "", false
	}
	line := strings.TrimSpace(string(b[len(pre):end]))
	line = strings.TrimSpace(strings.TrimSuffix(line, "-->"))
	return line, line != ""
}

func stripDocHashLine(s string) string {
	const pre = "<!-- hash:"
	if !strings.HasPrefix(s, pre) {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// hashModuleSourceExts are the file types that define a module's shape.
var hashModuleSourceExts = map[string]bool{
	".java": true, ".kt": true, ".kts": true, ".scala": true, ".groovy": true,
	".py": true, ".go": true, ".rs": true,
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".vue": true,
	".swift": true, ".m": true, ".mm": true,
	".dart": true,
	".c":    true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true,
	".hpp": true, ".hxx": true,
	".cs": true,
	".rb": true, ".php": true,
}

var hashOnlySkipDirs = map[string]bool{
	"test": true, "tests": true, "bin": true, "obj": true, ".dart_tool": true,
}

func isHashModuleFile(name string) bool {
	if hashModuleSourceExts[strings.ToLower(filepath.Ext(name))] {
		return true
	}
	switch strings.ToLower(name) {
	case "pom.xml", "build.gradle", "build.gradle.kts", "go.mod", "requirements.txt",
		"package.json", "cargo.toml", "readme.md",
		"package.swift", "pubspec.yaml", "cmakelists.txt", "makefile":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "application.") || // application.yml/.yaml/.properties
		strings.HasSuffix(strings.ToLower(name), ".csproj") ||
		strings.HasSuffix(strings.ToLower(name), ".sln")
}

// hashModule hashes a module's source tree recursively.
// It skips build, test, and VCS directories.
// Files are hashed in sorted order for determinism.
func hashModule(dir string) string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirectory(d.Name()) || hashOnlySkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if isHashModuleFile(d.Name()) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	h := md5.New()
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		h.Write([]byte(rel)) // include path so moves/renames change the hash
		if data, err := os.ReadFile(f); err == nil {
			h.Write(data)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

type llmClient struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	rc        *resty.Client
}

func newLLMClient(baseURL, apiKey, model string, maxTokens int) *llmClient {
	if maxTokens <= 0 {
		maxTokens = 2000
	}
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	rc := httpclient.New(300*time.Second, headers)
	return &llmClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
		rc:        rc,
	}
}

type chatReq struct {
	Model     string `json:"model"`
	Messages  []msg  `json:"messages"`
	MaxTokens int    `json:"max_tokens"`
}

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *llmClient) chat(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(chatReq{
		Model: c.model,
		Messages: []msg{
			{Role: "user", Content: prompt},
		},
		MaxTokens: c.maxTokens,
	})
	resp, err := httpclient.Request(ctx, c.rc).
		SetHeader("Content-Type", "application/json").
		SetBody(body).
		Post(c.baseURL + "/chat/completions")
	if err != nil {
		return "", err
	}
	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("LLM returned %d", resp.StatusCode())
	}
	var cr chatResp
	if err := json.Unmarshal(resp.Body(), &cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("empty response")
	}
	return cr.Choices[0].Message.Content, nil
}
