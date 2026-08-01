package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httpclient"
)

type Fix = FixRequest

type FixBranch struct {
	Service    string   `json:"service"`
	Repo       string   `json:"repo"`
	BranchName string   `json:"branch_name"`
	BaseBranch string   `json:"base_branch"`
	Assignee   string   `json:"assignee"`
	FilesHint  []string `json:"files_hint,omitempty"`
	Pushed     bool     `json:"pushed"`
	CommitHash string   `json:"commit_hash,omitempty"`
	MRURL      string   `json:"mr_url,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type FixRequest struct {
	Assignee   string `json:"assignee"`
	BranchName string `json:"branchName"`
}

type ConfirmRequest struct {
	BranchName string `json:"branchName"`
}

func (manager *Manager) StartFix(ctx context.Context, id string, req FixRequest) (*Incident, error) {
	inc, err := manager.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Assignee == "" {
		req.Assignee = manager.cfg.FixDefaultAssignee
	}
	if req.Assignee == "" {
		req.Assignee = "unassigned"
	}
	now := time.Now()
	inc.Status = StatusFixing
	inc.AssignedTo = req.Assignee
	inc.FixStartedAt = &now

	services := inc.AffectedSvcs
	if len(services) == 0 {
		services = []string{"unknown"}
	}
	for _, svc := range services {
		resolvedSvc, repo, err := manager.repoForService(ctx, svc)
		if err != nil {
			return nil, fmt.Errorf("resolve repository for service %q: %w", svc, err)
		}
		if repo == "" {
			return nil, fmt.Errorf("resolve repository for service %q: no repository mapping", svc)
		}
		branch := req.BranchName
		if branch == "" {
			branch = defaultBranchName(manager.cfg.FixBranchPrefix, inc.RootCause, inc.AlertTitle, req.Assignee)
		}
		fb := FixBranch{
			Service: resolvedSvc, Repo: repo, BranchName: branch, BaseBranch: "master", Assignee: req.Assignee,
			FilesHint: manager.filesHint(ctx, resolvedSvc, inc),
		}
		if err := manager.createBranch(ctx, &fb, inc); err != nil {
			fb.Error = err.Error()
		}
		inc.FixBranches = upsertBranch(inc.FixBranches, fb)
	}
	return inc, manager.save(ctx, inc)
}

func (manager *Manager) CommitFix(ctx context.Context, id string, req ConfirmRequest) (*Incident, error) {
	inc, err := manager.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range inc.FixBranches {
		fb := &inc.FixBranches[i]
		if req.BranchName != "" && fb.BranchName != req.BranchName {
			continue
		}
		if err := manager.commitBranch(ctx, fb, inc); err != nil {
			fb.Error = err.Error()
		}
	}
	allPushed := len(inc.FixBranches) > 0 && !slices.ContainsFunc(inc.FixBranches, func(fb FixBranch) bool { return !fb.Pushed })
	if allPushed {
		now := time.Now()
		inc.Status = StatusFixed
		inc.FixedAt = &now
	}
	return inc, manager.save(ctx, inc)
}

func (manager *Manager) createBranch(ctx context.Context, fb *FixBranch, inc *Incident) error {
	repoDir := filepath.Join(manager.workspaceRoot, fb.Repo)
	if st, err := os.Stat(repoDir); err != nil || !st.IsDir() {
		return fmt.Errorf("repo dir not found: %s", repoDir)
	}
	if err := ensureCleanWorktree(ctx, repoDir); err != nil {
		return err
	}
	if err := runGit(ctx, repoDir, "fetch", "origin", fb.BaseBranch); err != nil {
		return err
	}
	if err := runGit(ctx, repoDir, "checkout", "-B", fb.BranchName, "origin/"+fb.BaseBranch); err != nil {
		return err
	}
	note := filepath.Join(repoDir, ".nasuta-fix.md")
	return os.WriteFile(note, []byte(inc.AnalysisDoc+"\n\n## 修复建议\n\n"+inc.Solution+"\n"), 0o644)
}

func ensureCleanWorktree(ctx context.Context, repoDir string) error {
	out, err := gitOutput(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("worktree is dirty; please commit/stash local changes before nasuta creates a fix branch: %s", truncate(sanitizeOneLine(out), 500))
	}
	return nil
}

func (manager *Manager) commitBranch(ctx context.Context, fb *FixBranch, inc *Incident) error {
	repoDir := filepath.Join(manager.workspaceRoot, fb.Repo)
	if err := runGit(ctx, repoDir, "checkout", fb.BranchName); err != nil {
		return err
	}
	out, err := gitOutput(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		fb.Error = "no changes to commit"
		return nil
	}
	if err := runGit(ctx, repoDir, "add", "-A"); err != nil {
		return err
	}
	msg := "fix(" + inc.ID + "): " + truncate(sanitizeOneLine(inc.RootCause), 80)
	if err := runGit(ctx, repoDir, "commit", "-m", msg); err != nil {
		return err
	}
	hash, err := gitOutput(ctx, repoDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	if err := runGit(ctx, repoDir, "push", "-u", "origin", fb.BranchName); err != nil {
		return err
	}
	fb.CommitHash = strings.TrimSpace(hash)
	fb.Pushed = true
	fb.Error = ""
	if mrURL, err := manager.createMergeRequest(ctx, fb, inc); err == nil {
		fb.MRURL = mrURL
	} else if manager.cfg.VCSURL != "" && manager.cfg.VCSToken != "" {
		fb.Error = strings.TrimSpace("pushed, MR creation failed: " + err.Error())
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (manager *Manager) createMergeRequest(ctx context.Context, fb *FixBranch, inc *Incident) (string, error) {
	if manager.cfg.VCSURL == "" || manager.cfg.VCSToken == "" {
		return "", nil
	}
	project := strings.ReplaceAll(fb.Repo, "__", "/")
	if project == "" {
		return "", fmt.Errorf("empty VCS project")
	}
	form := url.Values{}
	form.Set("source_branch", fb.BranchName)
	form.Set("target_branch", fb.BaseBranch)
	form.Set("title", fmt.Sprintf("fix(%s): %s", inc.ID, truncate(sanitizeOneLine(inc.AlertTitle), 80)))
	form.Set("description", inc.AnalysisDoc)
	form.Set("remove_source_branch", "false")
	endpoint := strings.TrimRight(manager.cfg.VCSURL, "/") + "/api/v4/projects/" + url.PathEscape(project) + "/merge_requests"
	resp, err := httpclient.Request(ctx, httpclient.New(30*time.Second, nil)).
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetHeader("PRIVATE-TOKEN", manager.cfg.VCSToken).
		SetBody(form.Encode()).
		Post(endpoint)
	if err != nil {
		return "", err
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return "", fmt.Errorf("gitlab MR HTTP %d: %s", resp.StatusCode(), truncate(string(resp.Body()), 600))
	}
	var out struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(resp.Body(), &out); err != nil {
		return "", err
	}
	return out.WebURL, nil
}

func defaultBranchName(prefix, rootCause, title, assignee string) string {
	if prefix == "" {
		prefix = "hotfix"
	}
	key := slug(rootCause)
	if key == "" {
		key = slug(title)
	}
	if key == "" {
		key = "incident"
	}
	if assignee == "" {
		assignee = "unassigned"
	}
	assigneeSlug := slug(assignee)
	if assigneeSlug == "" {
		assigneeSlug = "unassigned"
	}
	return strings.TrimRight(prefix+"/"+key+"/"+assigneeSlug, "/")
}

func upsertBranch(branches []FixBranch, next FixBranch) []FixBranch {
	for i := range branches {
		if branches[i].Service == next.Service && branches[i].BranchName == next.BranchName {
			branches[i] = next
			return branches
		}
	}
	return append(branches, next)
}
