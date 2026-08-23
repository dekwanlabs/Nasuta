package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type projectInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Synced    bool   `json:"synced"`
	Indexed   bool   `json:"indexed"`
	CodeGraph bool   `json:"codegraph"`
	Docs      bool   `json:"docs"`
	Embedded  bool   `json:"embedded"`
}

type projectGroup struct {
	Name     string        `json:"name"`
	Projects []projectInfo `json:"projects"`
	// syncedRepos is scratch state used during a detail pass to collect the
	// repos needing a semantic count; never serialized.
	syncedRepos []string `json:"-"`
}

func (handler *Handler) APISettingsGet(w http.ResponseWriter, _ *http.Request) {
	result := handler.defaultSettings()
	if handler.authDB != nil {
		if stored, err := handler.authDB.GetSettings(); err == nil {
			config.MergeStoredPlatformValues(result, stored)
		}
	}
	httputil.WriteJSON(w, result)
}

func (handler *Handler) defaultSettings() map[string]any {
	return handler.platformSettings().Values()
}

func (handler *Handler) APISettingsPut(w http.ResponseWriter, r *http.Request) {
	if handler.authDB == nil {
		httputil.WriteErr(w, fmt.Errorf("settings not available"))
		return
	}
	var body map[string]string
	if err := httputil.DecodeJSON(r, &body); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	filtered, err := filterSettings(body)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if len(filtered) == 0 {
		httputil.WriteErr(w, fmt.Errorf("no valid setting keys provided"))
		return
	}
	stored, err := handler.authDB.GetSettings()
	if err != nil {
		httputil.WriteErr(w, fmt.Errorf("load settings for update: %w", err))
		return
	}
	if err := handler.validatePlatformSettingsUpdate(stored, filtered); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if _, providersChanged := filtered["coding_enabled_providers"]; providersChanged {
		if err := handler.validateCodingSettingsUpdate(stored, filtered); err != nil {
			httputil.WriteBadRequest(w, err.Error())
			return
		}
	} else if _, defaultChanged := filtered["coding_default_provider"]; defaultChanged {
		if err := handler.validateCodingSettingsUpdate(stored, filtered); err != nil {
			httputil.WriteBadRequest(w, err.Error())
			return
		}
	}

	changed := changedSettings(stored, filtered)
	changedKeys := keysOf(changed)
	if len(changed) == 0 {
		httputil.WriteJSON(w, map[string]any{"updated": 0, "keys": changedKeys})
		return
	}
	if err := handler.authDB.SetSettings(changed); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if err := handler.applySettings(changedKeys); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"updated": len(changed), "keys": changedKeys})
}

// validatePlatformSettingsUpdate validates the merged platform settings
// without rereading or mutating persisted values.
func (handler *Handler) validatePlatformSettingsUpdate(
	stored map[string]string,
	update map[string]string,
) error {
	settings := *handler.platformSettings()
	settings.Apply(stored)
	settings.Apply(update)
	return settings.ValidateAgentSettings()
}

// validateCodingSettingsUpdate validates provider relationships against the
// same persisted snapshot used to calculate the actual changes.
func (handler *Handler) validateCodingSettingsUpdate(
	stored map[string]string,
	update map[string]string,
) error {
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	settings.Apply(stored)
	settings.Apply(update)
	return settings.ValidateCodingSettings()
}

// changedSettings returns only canonical values that differ from persistence.
func changedSettings(
	stored map[string]string,
	update map[string]string,
) map[string]string {
	changed := make(map[string]string, len(update))
	for key, value := range update {
		if storedValue, exists := stored[key]; exists {
			canonicalStored, err := config.CanonicalPlatformSetting(
				key,
				storedValue,
			)
			if err == nil && canonicalStored == value {
				continue
			}
		}
		changed[key] = value
	}
	return changed
}

func filterSettings(body map[string]string) (map[string]string, error) {
	filtered := map[string]string{}
	for k, v := range body {
		if config.IsPlatformSetting(k) {
			canonical, err := config.CanonicalPlatformSetting(k, v)
			if err != nil {
				return nil, err
			}
			filtered[k] = canonical
		}
	}
	return filtered, nil
}

func (handler *Handler) APISystemStatus(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{
		"git_installed": false,
		"git_version":   "",
	}
	result["git_installed"], result["git_version"] = gitStatus()
	if handler.featureStatusFn != nil {
		result["feature_delivery"] = handler.featureStatusFn(r.Context())
	}
	httputil.WriteJSON(w, result)
}

func gitStatus() (bool, string) {
	if _, err := exec.LookPath("git"); err == nil {
		if out, e := exec.Command("git", "--version").Output(); e == nil {
			return true, strings.TrimSpace(string(out))
		}
	}
	return false, ""
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func (handler *Handler) APIProjects(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(handler.cfg.WorkspaceRoot, "repos")
	detail := httputil.Query(r).Bool("detail")
	groups, total := handler.scanProjects(r.Context(), root, detail)
	httputil.WriteJSON(w, map[string]any{"groups": groups, "total": total})
}

// scanProjects walks repos and returns grouped project info.
// With detail=true it also loads indexed, codegraph, docs, and embedded status.
// Those status reads are batched so large repo lists stay cheap.
func (handler *Handler) scanProjects(ctx context.Context, root string, detail bool) ([]projectGroup, int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []projectGroup{}, 0
	}

	var groups []projectGroup
	total := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		groupDir := filepath.Join(root, e.Name())
		subs, err := os.ReadDir(groupDir)
		if err != nil {
			continue
		}
		g := projectGroup{Name: e.Name()}
		for _, s := range subs {
			if !s.IsDir() || strings.HasPrefix(s.Name(), ".") {
				continue
			}
			projDir := filepath.Join(groupDir, s.Name())
			repo := e.Name() + "/" + s.Name()
			pi := projectInfo{
				Name:   s.Name(),
				Path:   repo,
				Synced: hasGitDir(filepath.Join(projDir, ".git")),
			}
			if pi.Synced {
				g.syncedRepos = append(g.syncedRepos, repo)
			}
			g.Projects = append(g.Projects, pi)
		}
		if len(g.Projects) > 0 {
			total += len(g.Projects)
			groups = append(groups, g)
		}
	}

	if detail {
		// Collect synced repos once for the parallel semantic count pass.
		var allRepos []string
		for _, g := range groups {
			allRepos = append(allRepos, g.syncedRepos...)
		}
		sets := handler.buildProjectStatusSets(ctx, allRepos)
		for i := range groups {
			for j := range groups[i].Projects {
				repo := groups[i].Projects[j].Path
				if !groups[i].Projects[j].Synced {
					continue
				}
				groups[i].Projects[j].Indexed = sets.indexed[repo]
				groups[i].Projects[j].CodeGraph = sets.codegraph[repo]
				groups[i].Projects[j].Docs = sets.docs[repo]
				groups[i].Projects[j].Embedded = sets.embedded[repo]
			}
			groups[i].syncedRepos = nil // drop transient scratch field
		}
	}
	return groups, total
}

// projectStatusSets holds the four per-repo "has data" sets built once per
// /api/projects?detail=true call, so each project row is an O(1) map lookup
// instead of four round-trips.
type projectStatusSets struct {
	indexed   map[string]bool
	codegraph map[string]bool
	docs      map[string]bool
	embedded  map[string]bool
}

// buildProjectStatusSets queries the backing stores concurrently.
// SQLite, CodeGraph, and DocStore are batched; semantic counts use a bounded pool.
// Unconfigured or failing stores are left nil and treated as "no data".
func (handler *Handler) buildProjectStatusSets(ctx context.Context, repos []string) *projectStatusSets {
	var (
		indexed   map[string]bool
		codegraph map[string]bool
		docs      map[string]bool
		embedded  map[string]bool
	)
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		if handler.db == nil {
			return
		}
		rs, err := handler.db.ReposWithServices(ctx)
		if err != nil {
			log.Warnf("[projects] indexed set: %v", err)
			return
		}
		indexed = make(map[string]bool, len(rs))
		for _, r := range rs {
			indexed[r] = true
		}
	}()

	go func() {
		defer wg.Done()
		if handler.codegraphDB == nil {
			return
		}
		paths, err := handler.codegraphDB.DistinctNodeFilePaths()
		if err != nil {
			log.Warnf("[projects] codegraph set: %v", err)
			return
		}
		codegraph = bucketCodegraphPaths(paths)
	}()

	go func() {
		defer wg.Done()
		if handler.docDB == nil {
			return
		}
		metas, err := handler.docDB.ListDocsMetaByKind(domain.DocKindModule)
		if err != nil {
			log.Warnf("[projects] docs set: %v", err)
			return
		}
		docs = bucketModuleDocs(metas)
	}()

	go func() {
		defer wg.Done()
		if handler.semantic == nil || !handler.semantic.Capabilities().Count {
			return
		}
		embedded = handler.countEmbeddedByRepo(ctx, repos)
	}()

	wg.Wait()
	return &projectStatusSets{indexed: indexed, codegraph: codegraph, docs: docs, embedded: embedded}
}

// countEmbeddedByRepo runs semantic count queries for each repo concurrently
// (capped at 8 in flight) and returns the set of repos that have ≥1 vector.
func (handler *Handler) countEmbeddedByRepo(ctx context.Context, repos []string) map[string]bool {
	out := map[string]bool{}
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, repo := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(repo string) {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := handler.semantic.Count(ctx, semantic.Filter{Keywords: map[string]string{"repo": repo}})
			if err != nil {
				log.Warnf("[projects] embedded count %q: %v", repo, err)
				return
			}
			if n > 0 {
				mu.Lock()
				out[repo] = true
				mu.Unlock()
			}
		}(repo)
	}
	wg.Wait()
	return out
}

// bucketCodegraphPaths maps file paths under repos/ to repo keys (group/project).
// A path like "repos/hsds/hsds-shopify/src/..." → "hsds/hsds-shopify".
func bucketCodegraphPaths(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		// "repos/group/project/..." → first 3 segments.
		parts := strings.SplitN(p, "/", 4)
		if len(parts) < 3 {
			continue
		}
		out[parts[1]+"/"+parts[2]] = true
	}
	return out
}

// bucketModuleDocs maps module-doc filenames to repo keys. docgen stores
// module docs as "group/name.md" or "group/name__submodule.md"; the repo key
// is "group/<name before __>".
func bucketModuleDocs(metas []domain.DocRecord) map[string]bool {
	out := make(map[string]bool, len(metas))
	for _, m := range metas {
		fn := m.Filename
		slash := strings.Index(fn, "/")
		if slash < 0 {
			continue
		}
		group := fn[:slash]
		rest := strings.TrimSuffix(fn[slash+1:], ".md")
		if i := strings.Index(rest, "__"); i >= 0 {
			rest = rest[:i]
		}
		if group != "" && rest != "" {
			out[group+"/"+rest] = true
		}
	}
	return out
}

func hasGitDir(path string) bool {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// loadVCSSettings reads the VCS connection params from the platform settings
// store. Shared by gitlab_sync and daily_sync so both stay in sync with the
// saved configuration.
func (handler *Handler) loadVCSSettings() (vcsURL, vcsToken, vcsGroups, vcsConcurrency, vcsExcludeProjects string, err error) {
	if handler.authDB == nil {
		err = fmt.Errorf("settings store unavailable")
		return
	}
	m, e := handler.authDB.GetSettings()
	if e != nil {
		err = fmt.Errorf("load settings: %w", e)
		return
	}
	vcsURL = m["vcs_url"]
	vcsToken = m["vcs_token"]
	vcsGroups = m["vcs_groups"]
	vcsConcurrency = m["vcs_clone_concurrency"]
	vcsExcludeProjects = m["vcs_exclude_projects"]
	if vcsURL == "" || vcsToken == "" || vcsGroups == "" {
		err = fmt.Errorf("VCS not configured — please set vcs_url, vcs_token, vcs_groups in platform settings")
		return
	}
	return
}
