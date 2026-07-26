package indexing

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/docgen"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// ToolsSink lets indexing refresh agent-side search helpers after rebuilds.
type ToolsSink interface {
	SetBM25(*retrieval.BM25Builder)
	InvalidateServices()
}

// Service owns the durable index plus the derived BM25 search state.
type Service struct {
	Cfg       config.Config
	Platform  *config.PlatformSettings
	DB        *store.SQLite
	Semantic  semantic.Store
	Embedder  embed.Embedder
	tools     ToolsSink
	ScanDirs  []string
	publisher ontology.Publisher

	docDB       *store.DocStore
	docStoreErr error

	VCS    *indexer.Client
	Syncer *indexer.Syncer

	activeVCSTokenFingerprint string

	bm25 atomic.Pointer[retrieval.BM25Builder]

	indexMu               sync.Mutex
	bm25MigrationRequired atomic.Bool
}

func (svc *Service) DocDB() *store.DocStore { return svc.docDB }

// SetTools connects indexing rebuilds to the agent-facing caches.
func (svc *Service) SetTools(t ToolsSink) {
	svc.tools = t
	if t != nil {
		if b := svc.bm25.Load(); b != nil {
			t.SetBM25(b)
		}
	}
}

func (svc *Service) SetOntologyPublisher(publisher ontology.Publisher) {
	svc.publisher = publisher
}

func (svc *Service) Close() {
	if svc.Semantic != nil {
		if err := svc.Semantic.Close(); err != nil {
			log.Infof("[indexing] semantic close: %v", err)
		}
	}
	if svc.DB != nil {
		if err := svc.DB.Close(); err != nil {
			log.Infof("[indexing] db close: %v", err)
		}
	}
}

// Build initializes stores and optional backends.
func Build(cfg config.Config, docDB *store.DocStore, docStoreErr error) (*Service, error) {
	db, err := store.Open(cfg.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	semanticBackend, err := semanticstore.New(cfg.Semantic)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("build semantic store: %w", err)
	}
	embedder := embed.New(cfg)
	svc := &Service{
		Cfg: cfg, DB: db, Semantic: semanticBackend, Embedder: embedder,
		docDB: docDB, docStoreErr: docStoreErr, Platform: &config.PlatformSettings{},
	}
	svc.loadBM25()
	svc.ScanDirs = svc.LoadScanDirs()
	return svc, nil
}

// SetPlatform swaps in the latest platform-managed settings.
func (svc *Service) SetPlatform(ps *config.PlatformSettings) {
	if ps != nil {
		svc.Platform = ps
	}
}

func (svc *Service) loadBM25() {
	vocabPath := filepath.Join(svc.Cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "bm25_vocab.json")
	if fileExists(vocabPath) {
		if builder, err := retrieval.LoadVocab(vocabPath); err == nil {
			svc.setBM25(builder)
			log.Infof("[indexing] loaded BM25 vocab from %s (%d tokens)", vocabPath, builder.VocabularySize())
			return
		} else if errors.Is(err, retrieval.ErrLegacyVocabulary) {
			svc.bm25MigrationRequired.Store(true)
			log.Warnf("[indexing] legacy BM25 vocabulary at %s - run the full Embed Code operation once before repository-only embedding", vocabPath)
			return
		} else {
			log.Errorf("[indexing] BM25 vocab at %s failed to load: %v - search is dense-only until rebuilt", vocabPath, err)
			return
		}
	}
	log.Warnf("[indexing] BM25 vocab missing at %s - hybrid search disabled (dense-only). Trigger the \"Embed Code\" platform action to rebuild it; it is no longer auto-rebuilt on startup.", vocabPath)
}

func (svc *Service) LoadScanDirs() []string {
	if len(svc.Cfg.ScanDirs) > 0 {
		return svc.Cfg.ScanDirs
	}
	return svc.DiscoverScanDirs()
}

func (svc *Service) initVCS() {
	if !svc.Platform.VCSEnabled() {
		return
	}
	svc.VCS = indexer.NewClient(svc.Platform.VCSURL, svc.Platform.VCSToken)
	svc.Syncer = indexer.NewSyncer(svc.Platform.VCSToken, svc.Platform.VCSConcurrency)
	svc.activeVCSTokenFingerprint = vcsTokenFingerprint(svc.Platform.VCSToken)
	log.Infof("[vcs] client initialized (token_fingerprint=%s)", svc.activeVCSTokenFingerprint)
}

func vcsTokenFingerprint(token string) string {
	if token == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:6])
}

// DiscoverScanDirs applies VCS exclusions to workspace scan roots.
func (svc *Service) DiscoverScanDirs() []string {
	dirs := indexer.DiscoverScanDirs(svc.Cfg.WorkspaceRoot)
	if len(svc.Platform.VCSExcludeProjects) == 0 {
		return dirs
	}
	out := dirs[:0]
	for _, d := range dirs {
		if !indexer.IsExcluded(d, svc.Platform.VCSExcludeProjects) {
			out = append(out, d)
		}
	}
	return out
}

func (svc *Service) setBM25(b *retrieval.BM25Builder) {
	svc.bm25.Store(b)
	if svc.tools != nil {
		svc.tools.SetBM25(b)
	}
}

func (svc *Service) invalidateToolCaches() {
	if svc.tools != nil {
		svc.tools.InvalidateServices()
	}
}

func (svc *Service) RebuildGraph(ctx context.Context) error {
	if err := svc.runCodegraphIndex(ctx); err != nil {
		return fmt.Errorf("rebuild codegraph: %w", err)
	}
	return nil
}

func (svc *Service) runCodegraphIndex(ctx context.Context) error {
	if name := svc.Cfg.CodeGraphContainer; name != "" {
		if _, err := exec.LookPath("docker"); err == nil {
			if err := runCodegraphDocker(ctx, name); err == nil {
				return nil
			} else {
				if ctx.Err() != nil {
					return fmt.Errorf("docker codegraph: %w", err)
				}
				log.Warnf("[codegraph] docker exec failed, trying local CLI: %v", err)
			}
		}
	}
	cliPath, err := exec.LookPath("codegraph")
	if err != nil {
		return fmt.Errorf("codegraph unavailable (no docker container %q and no local binary)", svc.Cfg.CodeGraphContainer)
	}
	return runCodegraphAt(ctx, cliPath, svc.Cfg.WorkspaceRoot)
}

const codegraphWorkspace = "/workspace"

func runCodegraphDocker(ctx context.Context, container string) error {
	log.Infof("[codegraph] docker exec %s: init %s", container, codegraphWorkspace)
	if out, err := runWithStream(ctx, "[codegraph]", "docker", "exec", container, "codegraph", "init", codegraphWorkspace); err != nil {
		log.Warnf("[codegraph] init: %v (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] docker exec %s: full index rebuild (this may take a minute or two)", container)
	args := append([]string{"exec", container, "codegraph"}, codegraphIndexArgs(codegraphWorkspace)...)
	out, err := runWithStream(ctx, "[codegraph]", "docker", args...)
	if err != nil {
		return fmt.Errorf("index: %w (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] index complete")
	return nil
}

func runCodegraphAt(ctx context.Context, cliPath, target string) error {
	log.Infof("[codegraph] init %s", target)
	if out, err := runWithStream(ctx, "[codegraph]", cliPath, "init", target); err != nil {
		log.Warnf("[codegraph] init: %v (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] full index rebuild (this may take a minute or two)")
	out, err := runWithStream(ctx, "[codegraph]", cliPath, codegraphIndexArgs(target)...)
	if err != nil {
		return fmt.Errorf("index: %w (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] index complete")
	return nil
}

func codegraphIndexArgs(target string) []string {
	return []string{"index", "--force", "--quiet", target}
}

// runWithStream drains both pipes continuously so verbose CLI output cannot block the child.
func runWithStream(ctx context.Context, prefix string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var mu sync.Mutex
	var outBuf strings.Builder
	done := make(chan struct{}, 2)

	for _, r := range []io.Reader{stdout, stderr} {
		go func(r io.Reader) {
			defer func() { done <- struct{}{} }()
			err := drainStream(r, func(chunk []byte) {
				mu.Lock()
				outBuf.Write(chunk)
				mu.Unlock()
				log.Infof("%s %s", prefix, strings.TrimRight(string(chunk), "\r\n"))
			})
			if err != nil {
				log.Warnf("%s output reader: %v", prefix, err)
			}
		}(r)
	}

	<-done
	<-done
	err = cmd.Wait()
	return []byte(outBuf.String()), err
}

// drainStream emits bounded chunks, including fragments of lines larger than the buffer.
func drainStream(r io.Reader, consume func([]byte)) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			consume(chunk)
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func trimExcludedNames(projects []indexer.Project) string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.PathWithNamespace
	}
	return strings.Join(names, ", ")
}

type semDoc struct {
	id            string
	text          string
	payload       map[string]any
	sparseIndices []uint32
	sparseValues  []float32
}

func (svc *Service) embedBatch(ctx context.Context, label string, docs []semDoc) error {
	if len(docs) == 0 {
		return nil
	}
	batch := svc.Cfg.EmbeddingBatch
	if batch <= 0 {
		batch = 16
	}
	conc := svc.Cfg.EmbeddingConcurrency
	if conc <= 0 {
		conc = 1
	}

	type rng struct{ start, end int }
	var batches []rng
	for s := 0; s < len(docs); s += batch {
		e := s + batch
		if e > len(docs) {
			e = len(docs)
		}
		batches = append(batches, rng{s, e})
	}

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, conc)
		done    int64
		skipped int64
		mu      sync.Mutex
		errs    []string
	)

	for _, b := range batches {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(b rng) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprintf("panic batch [%d:%d]: %v", b.start, b.end, r))
					skipped += int64(b.end - b.start)
					mu.Unlock()
				}
			}()

			group := docs[b.start:b.end]
			texts := make([]string, len(group))
			for i, d := range group {
				texts[i] = trimText(d.text)
			}
			vecs, err := svc.embedWithRetry(ctx, texts)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("embed batch [%d:%d]: %v", b.start, b.end, err))
				skipped += int64(len(group))
				mu.Unlock()
				return
			}
			points := make([]semantic.Record, 0, len(group))
			for i, d := range group {
				if i >= len(vecs) {
					break
				}
				points = append(points, semantic.Record{
					ID: d.id, DenseVector: vecs[i], Metadata: d.payload,
					SparseVector: &semantic.SparseVector{Indices: d.sparseIndices, Values: d.sparseValues},
				})
			}
			if len(points) == 0 {
				return
			}
			if err := svc.Semantic.Upsert(ctx, points); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("upsert batch [%d:%d]: %v", b.start, b.end, err))
				skipped += int64(len(points))
				mu.Unlock()
				return
			}
			n := atomic.AddInt64(&done, int64(len(points)))
			if n%2000 < int64(len(points)) {
				log.Infof("[semantic] %s: %d/%d embedded", label, n, len(docs))
			}
		}(b)
	}
	wg.Wait()

	if len(errs) > 0 {
		log.Warnf("[semantic] %s: %d/%d embedded, %d skipped (%d errors: %v)",
			label, done, len(docs), skipped, len(errs), errs[0])
	} else {
		log.Infof("[semantic] embedded %d %s (concurrency %d)", done, label, conc)
	}
	if done == 0 && len(errs) > 0 {
		return fmt.Errorf("all %d batches failed: %s", len(errs), errs[0])
	}
	return nil
}

func (svc *Service) embedWithRetry(ctx context.Context, texts []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		t0 := time.Now()
		vecs, err := svc.Embedder.Embed(ctx, texts)
		if d := time.Since(t0); d > 10*time.Second {
			log.Infof("[semantic] slow embed: %d texts in %v, err=%v", len(texts), d, err)
		}
		if err == nil {
			return vecs, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (svc *Service) embedServices(ctx context.Context, services []domain.ServiceRecord) error {
	docs := make([]semDoc, 0, len(services))
	for _, sv := range services {
		docs = append(docs, serviceDoc(sv))
	}
	return svc.embedBatch(ctx, "services", docs)
}

func (svc *Service) embedRunbooks(ctx context.Context, runbooks []domain.RunbookRecord) error {
	inputs := make([]indexer.EmbedDocInput, 0, len(runbooks))
	for _, rb := range runbooks {
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      rb.ID,
			Title:   rb.Title,
			Path:    rb.Path,
			Scope:   rb.Scope,
			Repo:    rb.Repo,
			Content: rb.Text,
		})
	}
	n, err := indexer.EmbedDocsCanonical(ctx, svc.Embedder, svc.Semantic, inputs, svc.Cfg.EmbeddingBatch)
	if err != nil {
		return fmt.Errorf("embed runbooks: %w", err)
	}
	log.Infof("[embed] runbooks: %d docs, %d chunks", len(runbooks), n)
	return nil
}

// EmbedDocs refreshes generated docs and the docs-backed vectors as one unit.
func (svc *Service) EmbedDocs(ctx context.Context) error {
	if svc.docDB == nil {
		log.Infof("[embed] document store unavailable; skip document embedding")
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	docgen.New(svc.Cfg, svc.Platform, svc.docDB).GenerateDocs(ctx, []string{svc.Cfg.WorkspaceRoot})

	runbooks, _, err := indexer.LoadKnowledgeBase(svc.docDB)
	if err != nil {
		return fmt.Errorf("load runbooks: %w", err)
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: "docs"}); err != nil {
		return fmt.Errorf("delete docs-repo vectors: %w", err)
	}
	if err := svc.embedRunbooks(ctx, runbooks); err != nil {
		return fmt.Errorf("embed runbooks: %w", err)
	}
	if err := svc.EmbedDocuments(ctx); err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[embed] docs re-embedded: runbooks=%d, documents done", len(runbooks))
	return nil
}

// DailySync refreshes repos and the generated-doc layer only.
func (svc *Service) DailySync(ctx context.Context, vcsURL, vcsToken, vcsGroups, vcsConcurrency, vcsExcludeProjects string) error {
	log.Infof("[daily-sync] starting (groups=%s)", vcsGroups)
	synced, err := svc.CheckoutAll(ctx, vcsURL, vcsToken, vcsGroups, vcsConcurrency, vcsExcludeProjects)
	if err != nil {
		return fmt.Errorf("checkout: %w", err)
	}
	svc.ScanDirs = svc.DiscoverScanDirs()
	log.Infof("[daily-sync] checked out %d repos", len(synced))

	// Detect repos with new commits by comparing HEAD SHA against last indexed SHA.
	type changedRepo struct {
		name string
		sha  string
	}
	var changed []changedRepo
	for _, repo := range synced {
		sha, err := repoHeadSHA(ctx, svc.Cfg.WorkspaceRoot, repo)
		if err != nil {
			log.Warnf("[daily-sync] read HEAD for %s: %v — skipping", repo, err)
			continue
		}
		last, err := svc.DB.GetIndexSHA(ctx, repo)
		if err != nil {
			log.Warnf("[daily-sync] read index state for %s: %v", repo, err)
		}
		if sha != "" && sha != last {
			changed = append(changed, changedRepo{repo, sha})
		}
	}
	if len(changed) == 0 {
		log.Infof("[daily-sync] no changes — done")
		return nil
	}
	log.Infof("[daily-sync] %d repos changed: %v", len(changed), changed)

	// Codegraph indexes the entire workspace. Do this once before embedding so
	// that method-level chunking (chunkByNodes) can use fresh symbol ranges.
	if err := svc.runCodegraphIndex(ctx); err != nil {
		log.Warnf("[daily-sync] codegraph index failed: %v", err)
	}
	if err := svc.RebuildSQLIndex(ctx); err != nil {
		return fmt.Errorf("rebuild structure snapshot: %w", err)
	}

	docsChangedAny := false
	for _, cr := range changed {
		repo := cr.name
		docsChanged, err := svc.generateDocsForRepo(ctx, repo)
		if err != nil {
			log.Errorf("[daily-sync] generate docs %s: %v", repo, err)
		}
		docsChangedAny = docsChangedAny || docsChanged
		if err := svc.EmbedRepoCode(ctx, repo); err != nil {
			log.Errorf("[daily-sync] embed code %s: %v", repo, err)
			continue
		}
		if docsChanged {
			log.Infof("[daily-sync] generated docs changed for %s; defer document embedding until repository sync completes", repo)
		}
	}
	if docsChangedAny {
		if err := svc.EmbedDocuments(ctx); err != nil {
			return fmt.Errorf("embed docs: %w", err)
		}
	} else {
		log.Infof("[daily-sync] generated documents unchanged — skip document embedding")
	}
	log.Infof("[daily-sync] done")
	return nil
}

func repoHeadSHA(ctx context.Context, workspaceRoot, repo string) (string, error) {
	dir := filepath.Join(workspaceRoot, "repos", repo)
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return "", fmt.Errorf("git revision for repository %q at %q: %w", repo, dir, err)
		}
		return "", fmt.Errorf("git revision for repository %q at %q: %w (%s)", repo, dir, err, detail)
	}
	return strings.TrimSpace(string(out)), nil
}

// EmbedDocuments re-embeds generated document kinds from the doc store.
func (svc *Service) EmbedDocuments(ctx context.Context) error {
	if svc.docDB == nil {
		log.Infof("[embed] document store unavailable; skip generated document embedding")
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}

	docs, err := svc.docDB.ListDocsByKinds(domain.GeneratedDocKinds)
	if err != nil {
		return fmt.Errorf("list docs: %w", err)
	}
	for _, d := range docs {
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{DocumentID: d.ID}); err != nil {
			log.Warnf("[embed] delete doc %s: %v", d.ID, err)
		}
	}
	inputs := make([]indexer.EmbedDocInput, 0, len(docs))
	for _, d := range docs {
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      d.ID,
			Title:   d.Title,
			Path:    d.Filename,
			Scope:   d.Kind,
			Repo:    "docs",
			Content: stripHashLine(d.Content),
		})
	}
	n, err := indexer.EmbedDocsCanonical(ctx, svc.Embedder, svc.Semantic, inputs, svc.Cfg.EmbeddingBatch)
	if err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[embed] re-indexing documents: %d docs, %d chunks", len(docs), n)
	return nil
}

func stripHashLine(s string) string {
	const pre = "<!-- hash:"
	if !strings.HasPrefix(s, pre) {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// EmbedCodeChunks performs the workspace-wide v2 migration/rebuild. Repository
// updates use EmbedRepoCode so existing token IDs remain stable.
func (svc *Service) EmbedCodeChunks(ctx context.Context, dirs []string) error {
	svc.indexMu.Lock()
	defer svc.indexMu.Unlock()
	if len(dirs) == 0 {
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}

	builder := retrieval.NewBM25Builder()
	preserveVocab := false
	if current := svc.bm25.Load(); current != nil && !svc.bm25MigrationRequired.Load() {
		builder = current.Clone()
		preserveVocab = true
	}
	generation := newIndexGeneration("workspace")
	chunks := indexer.ScanCodeChunks(svc.Cfg.WorkspaceRoot, dirs)
	docs, err := svc.buildCodeDocs(ctx, chunks, builder, generation)
	if err != nil {
		return err
	}
	if preserveVocab {
		if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
			return fmt.Errorf("save bm25 vocab: %w", err)
		}
	}
	if err := svc.embedBatch(ctx, "code", docs); err != nil {
		return err
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{
		Filter: semantic.Filter{Keywords: map[string]string{"kind": "code_chunk"}},
		Except: semantic.Filter{Keywords: map[string]string{"index_generation": generation}},
	}); err != nil {
		return fmt.Errorf("delete stale code vectors: %w", err)
	}
	if !preserveVocab {
		if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
			return fmt.Errorf("save bm25 vocab: %w", err)
		}
	}
	svc.bm25MigrationRequired.Store(false)
	svc.setBM25(builder)
	log.Infof("[indexing] embedded %d code chunks, bm25 vocab=%d", len(docs), builder.VocabularySize())
	return nil
}

func (svc *Service) buildCodeDocs(ctx context.Context, chunks []domain.CodeChunk, builder *retrieval.BM25Builder, generation string) ([]semDoc, error) {
	services, err := svc.DB.AllServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("load service modules: %w", err)
	}
	docs := make([]semDoc, 0, len(chunks))
	for _, ch := range chunks {
		text := trimText(ch.Text)
		var indices []uint32
		var values []float32
		if indexer.IsSparseIndexableFile(ch.Path) {
			tokens := builder.AddDoc(text)
			indices, values = retrieval.SparseToSorted(builder.BuildSparse(tokens))
		}
		evidenceClass, trustTier := domain.EvidenceForCodeChunk(ch.Lang, ch.Repo)
		docs = append(docs, semDoc{
			id:   platform.UUIDFromString("code:" + ch.Path + ":" + strconv.Itoa(ch.StartLine) + ":" + strconv.Itoa(ch.EndLine)),
			text: text,
			payload: map[string]any{
				"kind":             "code_chunk",
				"repo":             ch.Repo,
				"path":             ch.Path,
				"lang":             ch.Lang,
				"layer":            layerForCodePath(services, ch.Path),
				"start_line":       ch.StartLine,
				"end_line":         ch.EndLine,
				"text":             text,
				"preview":          trimText(ch.Text),
				"evidence_class":   evidenceClass,
				"trust_tier":       trustTier,
				"index_generation": generation,
			},
			sparseIndices: indices,
			sparseValues:  values,
		})
	}
	return docs, nil
}

func layerForCodePath(services []domain.ServiceRecord, path string) string {
	path = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(path), "./"))
	bestPrefix := ""
	layer := ""
	for _, service := range services {
		prefix := "repos/" + service.Repo
		if service.ModulePath != "." {
			prefix += "/" + service.ModulePath
		}
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			layer = service.Layer
		}
	}
	return layer
}

func (svc *Service) bm25VocabPath() string {
	return filepath.Join(svc.Cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "bm25_vocab.json")
}

func newIndexGeneration(scope string) string {
	return platform.UUIDFromString(scope + ":" + strconv.FormatInt(time.Now().UnixNano(), 10))
}

func trimText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.TrimSpace(s)
	if len(s) <= 8000 {
		return s
	}
	return strings.TrimSpace(s[:8000])
}

func serviceDoc(sv domain.ServiceRecord) semDoc {
	text := sv.ServiceName
	if sv.Summary != "" {
		text += "\n" + sv.Summary
	}
	return semDoc{
		id:   platform.UUIDFromString("service:" + sv.ServiceName),
		text: text,
		payload: map[string]any{
			"kind": "service", "service_name": sv.ServiceName,
			"repo": serviceRepoBucket, "layer": sv.Layer, "owner": sv.Owner,
			"evidence_class": domain.EvidenceClassServiceMeta,
			"trust_tier":     domain.TrustServiceMeta,
		},
	}
}

// CheckoutAll syncs every configured VCS project into the workspace.
func (svc *Service) CheckoutAll(ctx context.Context, vcsURL, vcsToken, vcsGroups, vcsConcurrency, vcsExcludeProjects string) ([]string, error) {
	requestedFingerprint := vcsTokenFingerprint(vcsToken)
	activeFingerprint := svc.activeVCSTokenFingerprint
	if activeFingerprint == "" {
		activeFingerprint = "none"
	}
	log.Infof("[vcs] checkout credentials loaded (token_fingerprint=%s, client_initialized=%t, active_token_fingerprint=%s)",
		requestedFingerprint, svc.VCS != nil, activeFingerprint)
	if svc.VCS != nil && requestedFingerprint != activeFingerprint {
		log.Warnf("[vcs] stored token differs from initialized client; rebuilding client (stored_fingerprint=%s, active_fingerprint=%s)",
			requestedFingerprint, activeFingerprint)
	}
	if vcsURL != "" && vcsToken != "" && vcsGroups != "" {
		svc.Platform.VCSURL = vcsURL
		svc.Platform.VCSToken = vcsToken
		// Accept both newline-separated UI values and older comma-joined ones.
		svc.Platform.VCSGroups = nil
		for _, g := range strings.FieldsFunc(vcsGroups, func(r rune) bool { return r == '\n' || r == ',' || r == '\r' }) {
			if t := strings.TrimSpace(g); t != "" {
				svc.Platform.VCSGroups = append(svc.Platform.VCSGroups, t)
			}
		}
		if n, err := strconv.Atoi(vcsConcurrency); err == nil && n > 0 {
			svc.Platform.VCSConcurrency = n
		}
		svc.initVCS()
	}
	// Refresh exclusions on every call so settings changes take effect immediately.
	if vcsExcludeProjects != "" {
		svc.Platform.VCSExcludeProjects = config.ParseExcludeList(vcsExcludeProjects)
	} else {
		svc.Platform.VCSExcludeProjects = nil
	}
	if svc.VCS == nil {
		return nil, fmt.Errorf("VCS not configured — please set vcs_url, vcs_token, vcs_groups in platform settings")
	}
	if err := os.MkdirAll(svc.Cfg.WorkspaceRoot, 0o755); err != nil {
		return nil, err
	}
	projects, err := svc.loadProjects(ctx)
	if err != nil {
		return nil, err
	}
	synced, err := svc.Syncer.SyncAll(ctx, projects, svc.Cfg.WorkspaceRoot)
	log.Infof("[vcs] checked out %d/%d projects into %s", len(synced), len(projects), svc.Cfg.WorkspaceRoot)
	svc.ScanDirs = svc.DiscoverScanDirs()
	return synced, err
}

// SyncOne fetches a single project and updates its local checkout.
func (svc *Service) SyncOne(ctx context.Context, pathWithNamespace string) error {
	if svc.VCS == nil {
		if svc.Platform.VCSEnabled() {
			svc.initVCS()
		}
		if svc.VCS == nil {
			return fmt.Errorf("VCS not configured")
		}
	}
	proj, err := svc.VCS.GetProject(ctx, pathWithNamespace)
	if err != nil {
		return fmt.Errorf("lookup project %q: %w", pathWithNamespace, err)
	}
	dirName := indexer.RepoDirName(proj.PathWithNamespace)
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, "repos", dirName)
	if err := svc.Syncer.CloneOrFetch(ctx, *proj, dir, proj.DefaultBranch); err != nil {
		return fmt.Errorf("sync %s: %w", pathWithNamespace, err)
	}
	svc.ScanDirs = svc.DiscoverScanDirs()
	log.Infof("[vcs] synced %q into %s", pathWithNamespace, dir)
	return nil
}

func (svc *Service) loadProjects(ctx context.Context) ([]indexer.Project, error) {
	var projects []indexer.Project
	for _, group := range svc.Platform.VCSGroups {
		ps, err := svc.VCS.ListGroupProjects(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("list group %q: %w", group, err)
		}
		log.Infof("[vcs] group %q: %d projects", group, len(ps))
		projects = append(projects, ps...)
	}
	if len(svc.Platform.VCSExcludeProjects) == 0 {
		return projects, nil
	}
	kept, excluded := indexer.FilterProjects(projects, svc.Platform.VCSExcludeProjects)
	if len(excluded) > 0 {
		log.Infof("[vcs] excluding %d deprecated project(s): %s", len(excluded), trimExcludedNames(excluded))
	}
	return kept, nil
}

// SyncProject fetches one project and incrementally reindexes it.
func (svc *Service) SyncProject(ctx context.Context, pathWithNamespace, gitURL, branch, commit string) error {
	if svc.Syncer == nil {
		return fmt.Errorf("vcs not configured")
	}
	if indexer.IsExcluded(pathWithNamespace, svc.Platform.VCSExcludeProjects) {
		log.Warnf("[vcs] skipping excluded project %q (webhook)", pathWithNamespace)
		return nil
	}
	dirName := indexer.RepoDirName(pathWithNamespace)
	proj := indexer.Project{PathWithNamespace: pathWithNamespace, HTTPURLToRepo: gitURL, DefaultBranch: branch}
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, "repos", dirName)
	if err := svc.Syncer.CloneOrFetch(ctx, proj, dir, branch); err != nil {
		return err
	}
	svc.ScanDirs = svc.DiscoverScanDirs()
	return svc.ReindexRepo(ctx, dirName, commit)
}

func (svc *Service) attachRepositorySnapshots(ctx context.Context, bundle *domain.IndexBundle) error {
	repos := make(map[string]struct{}, len(bundle.Services))
	for _, service := range bundle.Services {
		repos[service.Repo] = struct{}{}
	}
	names := make([]string, 0, len(repos))
	for repo := range repos {
		names = append(names, repo)
	}
	sort.Strings(names)
	indexedAt := time.Now().UnixMilli()
	bundle.Repositories = make([]domain.RepositoryRecord, 0, len(names))
	for _, repo := range names {
		sha, err := repoHeadSHA(ctx, svc.Cfg.WorkspaceRoot, repo)
		if err != nil {
			return fmt.Errorf("read repository revision %q: %w", repo, err)
		}
		bundle.Repositories = append(bundle.Repositories, domain.RepositoryRecord{
			Repo: repo, HeadSHA: sha, IndexedAt: indexedAt,
		})
	}
	return nil
}

// RebuildSQLIndex refreshes the structural SQLite index without touching vectors.
func (svc *Service) RebuildSQLIndex(ctx context.Context) error {
	started := time.Now()
	svc.ScanDirs = svc.LoadScanDirs()
	log.Infof("[rebuild-sql] scanning %s (dirs: %v)", svc.Cfg.WorkspaceRoot, svc.ScanDirs)
	bundle, err := svc.buildWorkspaceBundle()
	if err != nil {
		return err
	}
	log.Infof("[rebuild-sql] scan complete after %s: services=%d endpoints=%d dependencies=%d",
		time.Since(started).Round(time.Millisecond), len(bundle.Services), len(bundle.Endpoints), len(bundle.Dependencies))
	if err := svc.attachRepositorySnapshots(ctx, &bundle); err != nil {
		return fmt.Errorf("attach repository snapshots: %w", err)
	}
	log.Infof("[rebuild-sql] repository revisions loaded: repositories=%d", len(bundle.Repositories))
	if err := svc.publishWorkspace(ctx, bundle); err != nil {
		return err
	}
	log.Infof("[rebuild-sql] snapshot published after %s", time.Since(started).Round(time.Millisecond))
	svc.invalidateToolCaches()
	log.Infof("[rebuild-sql] completed after %s: services=%d endpoints=%d dependencies=%d",
		time.Since(started).Round(time.Millisecond), len(bundle.Services), len(bundle.Endpoints), len(bundle.Dependencies))
	return nil
}

// Bootstrap rebuilds the workspace index end to end.
func (svc *Service) Bootstrap(ctx context.Context) error {
	log.Infof("[bootstrap] scanning %s (dirs: %v)", svc.Cfg.WorkspaceRoot, svc.ScanDirs)
	bundle, err := svc.buildWorkspaceBundle()
	if err != nil {
		return err
	}
	if err := svc.attachRepositorySnapshots(ctx, &bundle); err != nil {
		return err
	}
	if err := svc.publishWorkspace(ctx, bundle); err != nil {
		return err
	}
	svc.invalidateToolCaches()
	log.Infof("[bootstrap] services=%d endpoints=%d dependencies=%d runbooks=%d",
		len(bundle.Services), len(bundle.Endpoints), len(bundle.Dependencies), len(bundle.Runbooks))

	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: serviceRepoBucket}); err != nil {
		return fmt.Errorf("delete service bucket: %w", err)
	}
	if err := svc.embedServices(ctx, bundle.Services); err != nil {
		return fmt.Errorf("embed services: %w", err)
	}
	// Clear the shared docs bucket first so deleted or re-keyed doc chunks do not linger.
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: "docs"}); err != nil {
		return fmt.Errorf("delete docs runbook vectors: %w", err)
	}
	if err := svc.embedRunbooks(ctx, bundle.Runbooks); err != nil {
		return fmt.Errorf("embed runbooks: %w", err)
	}
	if svc.Cfg.IndexCode {
		if err := svc.EmbedCodeChunks(ctx, svc.ScanDirs); err != nil {
			return fmt.Errorf("embed code: %w", err)
		}
	}
	return nil
}

func (svc *Service) buildWorkspaceBundle() (domain.IndexBundle, error) {
	if svc.docStoreErr != nil {
		return domain.IndexBundle{}, fmt.Errorf("document store unavailable: %w", svc.docStoreErr)
	}
	return indexer.BuildBundle(svc.Cfg.WorkspaceRoot, svc.ScanDirs, svc.docDB)
}

func (svc *Service) publishWorkspace(ctx context.Context, bundle domain.IndexBundle) error {
	if svc.publisher == nil {
		return fmt.Errorf("ontology publisher is not configured")
	}
	snapshot, err := ontology.Project(bundle)
	if err != nil {
		return fmt.Errorf("project ontology snapshot: %w", err)
	}
	workspace := ontology.WorkspaceSnapshot{Structure: bundle, Ontology: snapshot}
	generation, err := svc.publisher.PublishWorkspace(ctx, workspace)
	if err != nil {
		return fmt.Errorf("publish workspace snapshot: %w", err)
	}
	log.Infof("[ontology] published generation=%s entities=%d facts=%d", generation, len(snapshot.Entities), len(snapshot.Facts))
	return nil
}

// ReindexRepo refreshes one repository's structural and ontology snapshot.
func (svc *Service) ReindexRepo(ctx context.Context, repo, commit string) error {
	if repo == "" {
		return fmt.Errorf("empty repo")
	}
	log.Infof("[reindex] repo=%q commit=%q", repo, commit)
	svc.ScanDirs = svc.DiscoverScanDirs()
	return svc.RebuildSQLIndex(ctx)
}

// EmbedRepoCode refreshes code vectors for a single repo.
func (svc *Service) EmbedRepoCode(ctx context.Context, repo string) error {
	if repo == "" {
		return fmt.Errorf("empty repo")
	}
	svc.indexMu.Lock()
	defer svc.indexMu.Unlock()
	if svc.bm25MigrationRequired.Load() {
		return retrieval.ErrLegacyVocabulary
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	scanDir := filepath.Join("repos", repo)
	log.Infof("[embed-repo] repo=%q", repo)
	chunks := indexer.ScanCodeChunks(svc.Cfg.WorkspaceRoot, []string{scanDir})
	base := svc.bm25.Load()
	builder := retrieval.NewBM25Builder()
	if base != nil {
		builder = base.Clone()
	}
	generation := newIndexGeneration(repo)
	docs, err := svc.buildCodeDocs(ctx, chunks, builder, generation)
	if err != nil {
		return err
	}
	// Append-only IDs are persisted before the semantic write. A failed upsert can leave
	// unused IDs, but can never leave vectors with coordinates unknown on restart.
	if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
		return fmt.Errorf("save bm25 vocab: %w", err)
	}
	if len(docs) == 0 {
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: repo}); err != nil {
			return fmt.Errorf("delete empty repo vectors: %w", err)
		}
		svc.setBM25(builder)
		return nil
	}
	if err := svc.embedBatch(ctx, "code repo="+repo, docs); err != nil {
		return fmt.Errorf("embed code: %w", err)
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{
		Repository: repo,
		Except:     semantic.Filter{Keywords: map[string]string{"index_generation": generation}},
	}); err != nil {
		return fmt.Errorf("delete stale repo vectors: %w", err)
	}
	svc.setBM25(builder)
	log.Infof("[embed-repo] repo=%q done", repo)
	return nil
}

// ReindexAllServices refreshes service-level vectors after metadata-heavy rebuilds.
func (svc *Service) ReindexAllServices(ctx context.Context) error {
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: serviceRepoBucket}); err != nil {
		return fmt.Errorf("delete service bucket: %w", err)
	}
	allSvcs, err := svc.DB.AllServices(ctx)
	if err != nil {
		return fmt.Errorf("load services for re-embed: %w", err)
	}
	if err := svc.embedServices(ctx, allSvcs); err != nil {
		return fmt.Errorf("embed services: %w", err)
	}
	log.Infof("[reindex] re-embedded %d service vectors", len(allSvcs))
	return nil
}

const serviceRepoBucket = "_services"

// InitProject runs the full indexing pipeline for one repo.
func (svc *Service) InitProject(ctx context.Context, repo string) error {
	if repo == "" {
		return fmt.Errorf("empty repo")
	}
	if _, err := os.Stat(filepath.Join(svc.Cfg.WorkspaceRoot, "repos", repo)); err != nil {
		return fmt.Errorf("repo %q not found under repos/ (clone it first): %w", repo, err)
	}
	log.Infof("[init-project] repo=%q", repo)
	scanDir := filepath.Join("repos", repo)
	svc.ScanDirs = svc.DiscoverScanDirs()
	if err := svc.RebuildSQLIndex(ctx); err != nil {
		return fmt.Errorf("rebuild structure snapshot: %w", err)
	}

	// Hash skip keeps this cheap on repeated runs while still filling brand-new repos.
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, scanDir)
	docgen.New(svc.Cfg, svc.Platform, svc.docDB).GenerateDocs(ctx, []string{dir})
	if svc.Cfg.IndexCode {
		if err := svc.EmbedRepoCode(ctx, repo); err != nil {
			return fmt.Errorf("embed code: %w", err)
		}
	}
	if err := svc.embedDocsForRepo(ctx, repo); err != nil {
		return fmt.Errorf("embed docs: %w", err)
	}
	log.Infof("[init-project] repo=%q done", repo)
	return nil
}

// GenerateDocsForRepo regenerates module docs for one repo.
func (svc *Service) GenerateDocsForRepo(ctx context.Context, repo string) error {
	_, err := svc.generateDocsForRepo(ctx, repo)
	return err
}

func (svc *Service) generateDocsForRepo(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("empty repo")
	}
	scanDir := filepath.Join("repos", repo)
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, scanDir)
	if _, err := os.Stat(dir); err != nil {
		return false, fmt.Errorf("repo %q not found: %w", repo, err)
	}
	if svc.docDB == nil {
		log.Infof("[gendocs] document store unavailable; skip repo=%q", repo)
		return false, nil
	}

	log.Infof("[gendocs] repo=%q", repo)
	changed := docgen.New(svc.Cfg, svc.Platform, svc.docDB).GenerateDocsChanged(ctx, []string{dir})
	log.Infof("[gendocs] repo=%q done", repo)
	return changed, nil
}

// embedDocsForRepo re-embeds generated module docs under one repo.
func (svc *Service) embedDocsForRepo(ctx context.Context, repo string) error {
	if svc.docDB == nil {
		log.Infof("[embed-docs] document store unavailable; skip repo=%q", repo)
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{Collection: svc.Cfg.Semantic.Collection, DenseDim: svc.Embedder.Dim()}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	docs, err := svc.docDB.ListDocsByKinds(domain.GeneratedDocKinds)
	if err != nil {
		return fmt.Errorf("list docs: %w", err)
	}
	prefix := repo + "/"
	inputs := make([]indexer.EmbedDocInput, 0, len(docs))
	for _, d := range docs {
		if !strings.HasPrefix(d.Filename, prefix) {
			continue
		}
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{DocumentID: d.ID}); err != nil {
			log.Warnf("[init-project] delete doc %s: %v", d.ID, err)
		}
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      d.ID,
			Title:   d.Title,
			Path:    d.Filename,
			Scope:   d.Kind,
			Repo:    "docs",
			Content: stripHashLine(d.Content),
		})
	}
	if len(inputs) == 0 {
		return nil
	}
	if _, err := indexer.EmbedDocsCanonical(ctx, svc.Embedder, svc.Semantic, inputs, svc.Cfg.EmbeddingBatch); err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[init-project] re-embedded %d docs for repo=%q", len(inputs), repo)
	return nil
}
