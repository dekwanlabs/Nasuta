package indexing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dekwanlabs/nasuta/internal/indexing/docgen"
	"github.com/dekwanlabs/nasuta/log"
)

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
	if err := svc.refreshScanDirs(); err != nil {
		return err
	}
	if err := svc.RebuildSQLIndex(ctx); err != nil {
		return fmt.Errorf("rebuild structure snapshot: %w", err)
	}

	// Hash skip keeps this cheap on repeated runs while still filling brand-new repos.
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, scanDir)
	generator, err := docgen.New(svc.Cfg, svc.Platform, svc.docDB)
	if err != nil {
		return fmt.Errorf("create documentation generator: %w", err)
	}
	generator.GenerateDocs(ctx, []string{dir})
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
