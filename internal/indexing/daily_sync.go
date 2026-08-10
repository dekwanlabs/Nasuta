package indexing

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/log"
)

// DailySync refreshes repos and the generated-doc layer only.
func (svc *Service) DailySync(
	ctx context.Context,
	vcsURL string,
	vcsToken string,
	vcsGroups string,
	vcsConcurrency string,
	vcsExcludeProjects string,
) error {
	log.Infof("[daily-sync] starting (groups=%s)", vcsGroups)
	synced, err := svc.CheckoutAll(
		ctx,
		vcsURL,
		vcsToken,
		vcsGroups,
		vcsConcurrency,
		vcsExcludeProjects,
	)
	if err != nil {
		return fmt.Errorf("checkout: %w", err)
	}
	if err := svc.refreshScanDirs(); err != nil {
		return err
	}
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
	// that method-level chunking can use fresh symbol ranges.
	if err := svc.runCodegraphIndex(ctx); err != nil {
		log.Warnf("[daily-sync] codegraph index failed: %v", err)
	}
	if err := svc.RebuildSQLIndex(ctx); err != nil {
		return fmt.Errorf("rebuild structure snapshot: %w", err)
	}

	docsChangedAny := false
	for _, changedRepo := range changed {
		repo := changedRepo.name
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
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return "", fmt.Errorf("git revision for repository %q at %q: %w", repo, dir, err)
		}
		return "", fmt.Errorf(
			"git revision for repository %q at %q: %w (%s)",
			repo,
			dir,
			err,
			detail,
		)
	}
	return strings.TrimSpace(string(output)), nil
}
