package webhook

import (
	"context"
	"encoding/json"
	"github.com/dekwanlabs/nasuta/platform/httputil"
	"net/http"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/indexing"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/log"
)

// VCSPush is the subset of a GitLab push-event payload we consume.
type VCSPush struct {
	ObjectKind  string `json:"object_kind"`
	Ref         string `json:"ref"`
	CheckoutSHA string `json:"checkout_sha"`
	Project     struct {
		PathWithNamespace string `json:"path_with_namespace"`
		GitHTTPURL        string `json:"git_http_url"`
		DefaultBranch     string `json:"default_branch"`
	} `json:"project"`
	Commits []VCSCommit `json:"commits"`
}

// VCSCommit is the subset of a commit in a GitLab push event.
type VCSCommit struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

// VCSHandler returns an HTTP handler for POST /internal/vcs-hook. It
// validates the X-Gitlab-Token header, fetches the pushed project and
// incrementally reindexes it.
func VCSHandler(app *indexing.Service, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httputil.WriteMethodNotAllowed(w, "method not allowed")
			return
		}
		if secret != "" && r.Header.Get("X-Gitlab-Token") != secret {
			httputil.WriteUnauthorized(w, "unauthorized")
			return
		}
		var p VCSPush
		if err := httputil.DecodeJSON(r, &p); err != nil {
			httputil.WriteBadRequest(w, err.Error())
			return
		}
		if p.ObjectKind != "" && p.ObjectKind != "push" {
			replyJSON(w, map[string]any{"accepted": false, "reason": "ignored event " + p.ObjectKind})
			return
		}
		branch := strings.TrimPrefix(p.Ref, "refs/heads/")
		if branch == "" {
			branch = p.Project.DefaultBranch
		}
		path := p.Project.PathWithNamespace

		// Only reindex pushes to the default branch.
		if branch != p.Project.DefaultBranch {
			log.WarnfCtx(r.Context(), "[vcs-hook] skip project=%q branch=%q (not default %q)", path, branch, p.Project.DefaultBranch)
			replyJSON(w, map[string]any{"accepted": false, "reason": "branch " + branch + " is not default branch " + p.Project.DefaultBranch})
			return
		}

		// Only reindex if at least one changed file is indexable.
		if !hasIndexableFiles(p.Commits) {
			log.WarnfCtx(r.Context(), "[vcs-hook] skip project=%q: no indexable files changed", path)
			replyJSON(w, map[string]any{"accepted": false, "reason": "no indexable files changed"})
			return
		}

		log.InfofCtx(r.Context(), "[vcs-hook] push project=%q branch=%q sha=%q", path, branch, p.CheckoutSHA)

		go func() {
			bgCtx := log.WithTraceID(context.Background(), log.GenerateTraceID())
			log.InfofCtx(bgCtx, "[vcs-hook] sync starting: project=%q branch=%q", path, branch)
			if err := app.SyncProject(bgCtx, path, p.Project.GitHTTPURL, branch, p.CheckoutSHA); err != nil {
				log.ErrorfCtx(bgCtx, "[vcs-hook] sync %q error: %v", path, err)
			}
		}()

		replyJSON(w, map[string]any{"accepted": true, "project": path})
	}
}

// hasIndexableFiles checks whether any commit in the push touches a file whose
// extension is in the indexable set (source, config, SQL, docs, etc.).
func hasIndexableFiles(commits []VCSCommit) bool {
	for _, c := range commits {
		for _, files := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range files {
				if indexer.IsIndexableFile(f) {
					return true
				}
			}
		}
	}
	return false
}

func replyJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
