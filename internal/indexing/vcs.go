package indexing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/log"
)

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

// CheckoutAll syncs every configured VCS project into the workspace.
func (svc *Service) CheckoutAll(
	ctx context.Context,
	vcsURL string,
	vcsToken string,
	vcsGroups string,
	vcsConcurrency string,
	vcsExcludeProjects string,
) ([]string, error) {
	requestedFingerprint := vcsTokenFingerprint(vcsToken)
	activeFingerprint := svc.activeVCSTokenFingerprint
	if activeFingerprint == "" {
		activeFingerprint = "none"
	}
	log.Infof(
		"[vcs] checkout credentials loaded (token_fingerprint=%s, client_initialized=%t, active_token_fingerprint=%s)",
		requestedFingerprint,
		svc.VCS != nil,
		activeFingerprint,
	)
	if svc.VCS != nil && requestedFingerprint != activeFingerprint {
		log.Warnf(
			"[vcs] stored token differs from initialized client; rebuilding client (stored_fingerprint=%s, active_fingerprint=%s)",
			requestedFingerprint,
			activeFingerprint,
		)
	}
	if vcsURL != "" && vcsToken != "" && vcsGroups != "" {
		svc.Platform.VCSURL = vcsURL
		svc.Platform.VCSToken = vcsToken
		// Accept both newline-separated UI values and older comma-joined ones.
		svc.Platform.VCSGroups = nil
		for _, group := range strings.FieldsFunc(vcsGroups, func(r rune) bool {
			return r == '\n' || r == ',' || r == '\r'
		}) {
			if trimmed := strings.TrimSpace(group); trimmed != "" {
				svc.Platform.VCSGroups = append(svc.Platform.VCSGroups, trimmed)
			}
		}
		if concurrency, err := strconv.Atoi(vcsConcurrency); err == nil && concurrency > 0 {
			svc.Platform.VCSConcurrency = concurrency
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
	if scanErr := svc.refreshScanDirs(); scanErr != nil {
		return synced, scanErr
	}
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
	project, err := svc.VCS.GetProject(ctx, pathWithNamespace)
	if err != nil {
		return fmt.Errorf("lookup project %q: %w", pathWithNamespace, err)
	}
	dirName := indexer.RepoDirName(project.PathWithNamespace)
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, "repos", dirName)
	if err := svc.Syncer.CloneOrFetch(ctx, *project, dir, project.DefaultBranch); err != nil {
		return fmt.Errorf("sync %s: %w", pathWithNamespace, err)
	}
	if err := svc.refreshScanDirs(); err != nil {
		return err
	}
	log.Infof("[vcs] synced %q into %s", pathWithNamespace, dir)
	return nil
}

func (svc *Service) loadProjects(ctx context.Context) ([]indexer.Project, error) {
	var projects []indexer.Project
	for _, group := range svc.Platform.VCSGroups {
		groupProjects, err := svc.VCS.ListGroupProjects(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("list group %q: %w", group, err)
		}
		log.Infof("[vcs] group %q: %d projects", group, len(groupProjects))
		projects = append(projects, groupProjects...)
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

func trimExcludedNames(projects []indexer.Project) string {
	names := make([]string, len(projects))
	for i, project := range projects {
		names[i] = project.PathWithNamespace
	}
	return strings.Join(names, ", ")
}

// SyncProject fetches one project and incrementally reindexes it.
func (svc *Service) SyncProject(
	ctx context.Context,
	pathWithNamespace string,
	gitURL string,
	branch string,
	commit string,
) error {
	if svc.Syncer == nil {
		return fmt.Errorf("vcs not configured")
	}
	if indexer.IsExcluded(pathWithNamespace, svc.Platform.VCSExcludeProjects) {
		log.Warnf("[vcs] skipping excluded project %q (webhook)", pathWithNamespace)
		return nil
	}
	dirName := indexer.RepoDirName(pathWithNamespace)
	project := indexer.Project{
		PathWithNamespace: pathWithNamespace,
		HTTPURLToRepo:     gitURL,
		DefaultBranch:     branch,
	}
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, "repos", dirName)
	if err := svc.Syncer.CloneOrFetch(ctx, project, dir, branch); err != nil {
		return err
	}
	if err := svc.refreshScanDirs(); err != nil {
		return err
	}
	return svc.ReindexRepo(ctx, dirName, commit)
}
