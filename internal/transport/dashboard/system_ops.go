package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type repoReq struct {
	Repo string `json:"repo"`
}

const systemOperationTimeout = 6 * time.Hour

func systemOperationContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), systemOperationTimeout)
}

// ── Global ops ────────────────────────────────────────────────────────────────

func (h *Handler) APIGitlabSync(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "gitlab sync not configured")
		return
	}
	u, t, g, c, x, err := h.loadVCSSettings()
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	h.runSystemOperation(w, r, "gitlab sync", "gitlab sync not configured", func(ctx context.Context) error {
		_, err := h.idx.CheckoutAll(ctx, u, t, g, c, x)
		return err
	})
}

func (h *Handler) APIBootstrap(w http.ResponseWriter, r *http.Request) {
	h.runSystemOperation(w, r, "full bootstrap", "bootstrap not configured", func(ctx context.Context) error {
		if err := h.idx.RebuildGraph(ctx); err != nil {
			return fmt.Errorf("codegraph rebuild: %w", err)
		}
		if err := h.refreshCodeGraph(); err != nil {
			return fmt.Errorf("codegraph refresh: %w", err)
		}
		if err := h.idx.Bootstrap(ctx); err != nil {
			return err
		}
		return nil
	})
}

func (h *Handler) APIRebuildSQLIndex(w http.ResponseWriter, r *http.Request) {
	h.runSystemOperation(w, r, "SQL index rebuild", "rebuild sql index not configured", func(ctx context.Context) error {
		return h.idx.RebuildSQLIndex(ctx)
	})
}

func (h *Handler) APIRebuildCodeGraph(w http.ResponseWriter, r *http.Request) {
	h.runSystemOperation(w, r, "codegraph rebuild", "rebuild codegraph not configured", func(ctx context.Context) error {
		if err := h.idx.RebuildGraph(ctx); err != nil {
			return err
		}
		if err := h.refreshCodeGraph(); err != nil {
			return fmt.Errorf("codegraph refresh: %w", err)
		}
		return nil
	})
}

func (h *Handler) APIEmbedDocs(w http.ResponseWriter, r *http.Request) {
	h.runSystemOperation(w, r, "doc embed", "embed docs not configured", func(ctx context.Context) error {
		return h.idx.EmbedDocs(ctx)
	})
}

func (h *Handler) APIEmbedCode(w http.ResponseWriter, r *http.Request) {
	h.runSystemOperation(w, r, "code embed", "embed code not configured", func(ctx context.Context) error {
		dirs, err := h.idx.DiscoverScanDirs()
		if err != nil {
			return fmt.Errorf("discover code scan directories: %w", err)
		}
		return h.idx.EmbedCodeChunks(ctx, dirs)
	})
}

func (h *Handler) runSystemOperation(
	w http.ResponseWriter,
	r *http.Request,
	name string,
	unavailableMessage string,
	action func(context.Context) error,
) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, unavailableMessage)
		return
	}
	started := time.Now()
	log.Infof("[ops] starting %s", name)
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if err := action(ctx); err != nil {
		log.Errorf("[ops] %s failed after %s: %v", name, time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] %s completed after %s", name, time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

// ── Per-repo ops ──────────────────────────────────────────────────────────────

func (h *Handler) APIReindexRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := decodeRepoReq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "reindex repo not configured")
		return
	}
	log.Infof("[ops] starting reindex_repo for %q", repo)
	if err := h.idx.ReindexRepo(r.Context(), repo, ""); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIEmbedRepo(w http.ResponseWriter, r *http.Request) {
	h.submitRepoOperation(w, r, "embed_repo", "embed repo not configured", func(ctx context.Context, repo string) error {
		return h.idx.EmbedRepoCode(ctx, repo)
	})
}

func (h *Handler) APIGendocsRepo(w http.ResponseWriter, r *http.Request) {
	h.submitRepoOperation(w, r, "gendocs_repo", "generate docs not configured", func(ctx context.Context, repo string) error {
		return h.idx.GenerateDocsForRepo(ctx, repo)
	})
}

func (h *Handler) submitRepoOperation(
	w http.ResponseWriter,
	r *http.Request,
	name string,
	unavailableMessage string,
	action func(context.Context, string) error,
) {
	repo, err := decodeRepoReq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, unavailableMessage)
		return
	}
	log.Infof("[ops] submitting %s for %q (async)", name, repo)
	go func() {
		if err := action(context.Background(), repo); err != nil {
			log.Errorf("[ops] %s %s failed: %v", name, repo, err)
		}
	}()
	httputil.WriteJSON(w, map[string]string{"status": "submitted"})
}

func (h *Handler) APISyncProject(w http.ResponseWriter, r *http.Request) {
	repo, err := decodeRepoReq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "sync project not configured")
		return
	}
	log.Infof("[ops] starting sync_project for %q", repo)
	if err := h.idx.SyncOne(r.Context(), repo); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func decodeRepoReq(r *http.Request) (string, error) {
	var req repoReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		return "", err
	}
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		return "", fmt.Errorf("repo is required")
	}
	return repo, nil
}
