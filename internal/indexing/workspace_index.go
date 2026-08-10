package indexing

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
)

func (svc *Service) attachRepositorySnapshots(ctx context.Context, bundle *domain.IndexBundle) error {
	repositories := make(map[string]struct{}, len(bundle.Services))
	for _, service := range bundle.Services {
		repositories[service.Repo] = struct{}{}
	}
	names := make([]string, 0, len(repositories))
	for repo := range repositories {
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
	var err error
	svc.ScanDirs, err = svc.LoadScanDirs()
	if err != nil {
		return fmt.Errorf("load scan directories: %w", err)
	}
	log.Infof("[rebuild-sql] scanning %s (dirs: %v)", svc.Cfg.WorkspaceRoot, svc.ScanDirs)
	bundle, err := svc.buildWorkspaceBundle(ctx)
	if err != nil {
		return err
	}
	log.Infof(
		"[rebuild-sql] scan complete after %s: services=%d endpoints=%d dependencies=%d",
		time.Since(started).Round(time.Millisecond),
		len(bundle.Services),
		len(bundle.Endpoints),
		len(bundle.Dependencies),
	)
	if err := svc.attachRepositorySnapshots(ctx, &bundle); err != nil {
		return fmt.Errorf("attach repository snapshots: %w", err)
	}
	log.Infof("[rebuild-sql] repository revisions loaded: repositories=%d", len(bundle.Repositories))
	if err := svc.publishWorkspace(ctx, bundle); err != nil {
		return err
	}
	log.Infof("[rebuild-sql] snapshot published after %s", time.Since(started).Round(time.Millisecond))
	svc.invalidateToolCaches()
	log.Infof(
		"[rebuild-sql] completed after %s: services=%d endpoints=%d dependencies=%d",
		time.Since(started).Round(time.Millisecond),
		len(bundle.Services),
		len(bundle.Endpoints),
		len(bundle.Dependencies),
	)
	return nil
}

// Bootstrap rebuilds the workspace index end to end.
func (svc *Service) Bootstrap(ctx context.Context) error {
	log.Infof("[bootstrap] scanning %s (dirs: %v)", svc.Cfg.WorkspaceRoot, svc.ScanDirs)
	bundle, err := svc.buildWorkspaceBundle(ctx)
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
	log.Infof(
		"[bootstrap] services=%d endpoints=%d dependencies=%d runbooks=%d",
		len(bundle.Services),
		len(bundle.Endpoints),
		len(bundle.Dependencies),
		len(bundle.Runbooks),
	)

	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
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

func (svc *Service) buildWorkspaceBundle(ctx context.Context) (domain.IndexBundle, error) {
	if svc.docStoreErr != nil {
		return domain.IndexBundle{}, fmt.Errorf("document store unavailable: %w", svc.docStoreErr)
	}
	return indexer.BuildBundleWithResolver(
		ctx,
		svc.Cfg.WorkspaceRoot,
		svc.ScanDirs,
		svc.docDB,
		svc.configs,
	)
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
	log.Infof(
		"[ontology] published generation=%s entities=%d facts=%d",
		generation,
		len(snapshot.Entities),
		len(snapshot.Facts),
	)
	return nil
}

// ReindexRepo refreshes one repository's structural and ontology snapshot.
func (svc *Service) ReindexRepo(ctx context.Context, repo, commit string) error {
	if repo == "" {
		return fmt.Errorf("empty repo")
	}
	log.Infof("[reindex] repo=%q commit=%q", repo, commit)
	if err := svc.refreshScanDirs(); err != nil {
		return err
	}
	return svc.RebuildSQLIndex(ctx)
}
