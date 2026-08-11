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
	started := time.Now()
	log.Infof("[ops] starting gitlab sync (groups=%s)", g)
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if _, err := h.idx.CheckoutAll(ctx, u, t, g, c, x); err != nil {
		log.Errorf("[ops] gitlab sync failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] gitlab sync completed after %s", time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIBootstrap(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "bootstrap not configured")
		return
	}
	started := time.Now()
	log.Infof("[ops] starting full bootstrap")
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if err := h.idx.RebuildGraph(ctx); err != nil {
		log.Errorf("[ops] full bootstrap codegraph rebuild failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	if err := h.refreshCodeGraph(); err != nil {
		log.Errorf("[ops] full bootstrap codegraph refresh failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	if err := h.idx.Bootstrap(ctx); err != nil {
		log.Errorf("[ops] full bootstrap failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] full bootstrap completed after %s", time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIRebuildSQLIndex(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "rebuild sql index not configured")
		return
	}
	started := time.Now()
	log.Infof("[ops] starting SQL index rebuild")
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if err := h.idx.RebuildSQLIndex(ctx); err != nil {
		log.Errorf("[ops] SQL index rebuild failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] SQL index rebuild completed after %s", time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIRebuildCodeGraph(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "rebuild codegraph not configured")
		return
	}
	started := time.Now()
	log.Infof("[ops] starting codegraph rebuild")
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if err := h.idx.RebuildGraph(ctx); err != nil {
		log.Errorf("[ops] codegraph rebuild failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	if err := h.refreshCodeGraph(); err != nil {
		log.Errorf("[ops] codegraph refresh failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] codegraph rebuild completed after %s", time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIEmbedDocs(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "embed docs not configured")
		return
	}
	started := time.Now()
	log.Infof("[ops] starting doc embed")
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	if err := h.idx.EmbedDocs(ctx); err != nil {
		log.Errorf("[ops] doc embed failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] doc embed completed after %s", time.Since(started).Round(time.Millisecond))
	httputil.WriteJSON(w, map[string]string{"status": "completed"})
}

func (h *Handler) APIEmbedCode(w http.ResponseWriter, r *http.Request) {
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "embed code not configured")
		return
	}
	started := time.Now()
	log.Infof("[ops] starting code embed")
	ctx, cancel := systemOperationContext(r)
	defer cancel()
	dirs, err := h.idx.DiscoverScanDirs()
	if err != nil {
		log.Errorf("[ops] discover code scan directories: %v", err)
		httputil.WriteErr(w, err)
		return
	}
	if err := h.idx.EmbedCodeChunks(ctx, dirs); err != nil {
		log.Errorf("[ops] code embed failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		httputil.WriteErr(w, err)
		return
	}
	log.Infof("[ops] code embed completed after %s", time.Since(started).Round(time.Millisecond))
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
	repo, err := decodeRepoReq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "embed repo not configured")
		return
	}
	log.Infof("[ops] submitting embed_repo for %q (async)", repo)
	go func() {
		if err := h.idx.EmbedRepoCode(context.Background(), repo); err != nil {
			log.Errorf("[ops] embed_repo %s failed: %v", repo, err)
		}
	}()
	httputil.WriteJSON(w, map[string]string{"status": "submitted"})
}

func (h *Handler) APIGendocsRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := decodeRepoReq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if h.idx == nil {
		httputil.WriteServiceUnavailable(w, "generate docs not configured")
		return
	}
	log.Infof("[ops] submitting gendocs_repo for %q (async)", repo)
	go func() {
		if err := h.idx.GenerateDocsForRepo(context.Background(), repo); err != nil {
			log.Errorf("[ops] gendocs_repo %s failed: %v", repo, err)
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
