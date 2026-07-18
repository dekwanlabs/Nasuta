// Package vcs lists projects from a VCS (GitLab today) and clones or fetches
// them onto the local filesystem for indexing.
package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/astris/log"
	"github.com/dekwanlabs/astris/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

// Project is the subset of the GitLab project API we use.
type Project struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
}

// RepoDirName returns the <group>/<project> tail used as the local checkout dir.
// Projects live under <root>/repos/ with one level of grouping.
// This mirrors the useful VCS structure without the deep prefix.
func RepoDirName(pathWithNamespace string) string {
	p := strings.Trim(strings.TrimSpace(pathWithNamespace), "/")
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return filepath.Join(parts[len(parts)-2:]...)
	}
	return p
}

// IsExcluded reports whether a project identifier matches any exclude pattern.
func IsExcluded(id string, patterns []string) bool {
	if id == "" || len(patterns) == 0 {
		return false
	}
	full := normalizeID(id)
	base := full
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		base = full[i+1:]
	}
	forms := []string{full, base} // "iot/cloud/hsmf/test" and "test"
	for _, pat := range patterns {
		p := normalizeID(pat)
		if p == "" {
			continue
		}
		for _, f := range forms {
			if strings.Contains(f, p) {
				return true
			}
			if ok, _ := filepath.Match(p, f); ok {
				return true
			}
		}
	}
	return false
}

// normalizeID lowercases and unifies path separators.
func normalizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "__", "/")
	return strings.Trim(s, "/")
}

// FilterProjects splits projects into kept and excluded by the patterns.
func FilterProjects(projects []Project, patterns []string) (kept, excluded []Project) {
	for _, p := range projects {
		if IsExcluded(p.PathWithNamespace, patterns) {
			excluded = append(excluded, p)
		} else {
			kept = append(kept, p)
		}
	}
	return kept, excluded
}

// Client talks to the GitLab REST API.
type Client struct {
	baseURL string
	token   string
	rc      *resty.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		rc:      httpclient.New(30*time.Second, nil),
	}
}

// GetProject looks up a single project by its path_with_namespace (e.g. "airone/dreo").
func (client *Client) GetProject(ctx context.Context, pathWithNamespace string) (*Project, error) {
	enc := strings.ReplaceAll(strings.Trim(pathWithNamespace, "/"), "/", "%2F")
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s", client.baseURL, enc)
	resp, err := httpclient.Request(ctx, client.rc).
		SetHeader("PRIVATE-TOKEN", client.token).
		Get(endpoint)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("vcs get project %s: status %d", pathWithNamespace, resp.StatusCode())
	}
	var proj Project
	if err := json.Unmarshal(resp.Body(), &proj); err != nil {
		return nil, err
	}
	return &proj, nil
}

// ListGroupProjects returns all projects in a group, including subgroups.
func (client *Client) ListGroupProjects(ctx context.Context, group string) ([]Project, error) {
	groupEnc := strings.ReplaceAll(strings.Trim(group, "/"), "/", "%2F")
	var all []Project
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/api/v4/groups/%s/projects?include_subgroups=true&archived=false&per_page=100&page=%d",
			client.baseURL, groupEnc, page)
		resp, err := httpclient.Request(ctx, client.rc).
			SetHeader("PRIVATE-TOKEN", client.token).
			Get(endpoint)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() != http.StatusOK {
			return nil, fmt.Errorf("vcs list projects %s: status %d", group, resp.StatusCode())
		}
		var batch []Project
		if err := json.Unmarshal(resp.Body(), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		next := resp.Header().Get("X-Next-Page")
		if next == "" || len(batch) == 0 {
			break
		}
		if n, _ := strconv.Atoi(next); n == 0 {
			break
		}
	}
	return all, nil
}

// Syncer clones/fetches repositories onto the local filesystem.
type Syncer struct {
	token       string
	concurrency int
}

func NewSyncer(token string, concurrency int) *Syncer {
	if concurrency < 1 {
		concurrency = 4
	}
	return &Syncer{token: token, concurrency: concurrency}
}

// authURL injects oauth2:<token> credentials into an https clone URL.
func (s *Syncer) authURL(httpURL string) (string, error) {
	u, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("oauth2", s.token)
	return u.String(), nil
}

// CloneOrFetch ensures dir holds an up-to-date checkout of proj.
// Callers pass the project's default branch; if it is empty, git falls back to the remote default.
// This avoids the old hardcoded "master" failure on main-default projects.
func (s *Syncer) CloneOrFetch(ctx context.Context, proj Project, dir, branch string) error {
	auth, err := s.authURL(proj.HTTPURLToRepo)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if branch == "" {
			// DefaultBranch unknown: pull whatever the remote considers default.
			if out, err := runGit(ctx, dir, "fetch", "--depth", "1", "origin"); err != nil {
				return fmt.Errorf("fetch %s: %v: %s", proj.PathWithNamespace, err, out)
			}
			_, err = runGit(ctx, dir, "reset", "--hard", "origin/HEAD")
			return err
		}
		if out, err := runGit(ctx, dir, "fetch", "--depth", "1", "origin", branch); err != nil {
			return fmt.Errorf("fetch %s: %v: %s", proj.PathWithNamespace, err, out)
		}
		// Reset to origin/<branch> rather than FETCH_HEAD.
		// FETCH_HEAD is less reliable for empty branches on some git versions.
		// If the ref is still absent, skip reset so empty repos do not fail sync.
		ref := "origin/" + branch
		if _, err := runGit(ctx, dir, "rev-parse", "--verify", ref); err == nil {
			if out, err := runGit(ctx, dir, "reset", "--hard", ref); err != nil {
				return fmt.Errorf("reset %s: %v: %s", proj.PathWithNamespace, err, out)
			}
		}
		// Some default branches contain only an initial README commit.
		// If the working tree is effectively empty after reset, try master.
		// This recovers repos whose real code still lives elsewhere.
		if branch != "master" && countFiles(dir) <= 2 {
			if _, ferr := runGit(ctx, dir, "fetch", "--depth", "1", "origin", "master"); ferr == nil {
				log.Infof("[vcs] %s: default branch %q has %d file(s) (looks empty), trying master", proj.PathWithNamespace, branch, countFiles(dir))
				// Ignore reset errors for master — it may not exist.
				_, _ = runGit(ctx, dir, "reset", "--hard", "FETCH_HEAD")
			}
		}
		return nil
	}
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, auth, dir)
	if out, err := runGit(ctx, "", args...); err != nil {
		return fmt.Errorf("clone %s: %v: %s", proj.PathWithNamespace, err, sanitize(out, s.token))
	}
	return nil
}

// SyncAll clones or fetches every project under root concurrently.
func (s *Syncer) SyncAll(ctx context.Context, projects []Project, root string) []string {
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []string

	for _, p := range projects {
		p := p
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			dirName := RepoDirName(p.PathWithNamespace)
			dir := filepath.Join(root, "repos", dirName)
			if err := s.CloneOrFetch(ctx, p, dir, p.DefaultBranch); err != nil {
				log.Errorf("[vcs] sync %s failed: %v", p.PathWithNamespace, err)
				return
			}
			mu.Lock()
			ok = append(ok, dirName)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return ok
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}

// sanitize removes the token from command output before logging.
func sanitize(b []byte, token string) string {
	if token == "" {
		return string(b)
	}
	return strings.ReplaceAll(string(b), token, "***")
}

// countFiles returns the number of git-tracked files in the working tree.
func countFiles(dir string) int {
	// count newlines to avoid allocations from strings.Split.
	out, err := runGit(context.Background(), dir, "ls-files")
	if err != nil {
		return 0
	}
	n := 1 // last line may not end with newline
	for _, b := range out {
		if b == '\n' {
			n++
		}
	}
	if len(out) == 0 {
		n = 0
	}
	return n
}
