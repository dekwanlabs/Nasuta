package indexing

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func TestRebuildSQLIndexRefreshesScanDirsAndPublishesCurrentRepositories(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repos", "team", "current")
	writeIndexingTestFile(t, filepath.Join(repo, "go.mod"), "module example.com/current\n\ngo 1.24\n")
	writeIndexingTestFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	runIndexingTestGit(t, repo, "init")
	runIndexingTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "add", ".")
	runIndexingTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	db, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &Service{
		Cfg:      config.Config{WorkspaceRoot: root, SQLitePath: filepath.Join(root, "index.db")},
		Platform: &config.PlatformSettings{},
		DB:       db,
		ScanDirs: []string{"repos/team/removed"},
	}
	svc.SetOntologyPublisher(ontologysqlite.New(db))

	if err := svc.RebuildSQLIndex(context.Background()); err != nil {
		t.Fatalf("RebuildSQLIndex: %v", err)
	}
	if len(svc.ScanDirs) != 1 || svc.ScanDirs[0] != "repos/team/current" {
		t.Fatalf("ScanDirs = %v, want current repository", svc.ScanDirs)
	}
	repos, err := db.ReposWithServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != "team/current" {
		t.Fatalf("published repositories = %v, want [team/current]", repos)
	}
}

func TestRepoHeadSHAErrorIncludesRepositoryPathAndGitStderr(t *testing.T) {
	root := t.TempDir()
	repo := "team/not-a-repository"
	dir := filepath.Join(root, "repos", filepath.FromSlash(repo))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := repoHeadSHA(context.Background(), root, repo)
	if err == nil {
		t.Fatal("repoHeadSHA succeeded outside a Git repository")
	}
	for _, want := range []string{repo, dir, "not a git repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("repoHeadSHA error %q does not contain %q", err, want)
		}
	}
}

func TestBuildWorkspaceBundleRejectsConfiguredDocStoreFailure(t *testing.T) {
	svc := &Service{
		Cfg:         config.Config{WorkspaceRoot: t.TempDir()},
		docStoreErr: errors.New("mysql unavailable"),
	}
	if _, err := svc.buildWorkspaceBundle(context.Background()); err == nil || !strings.Contains(err.Error(), "mysql unavailable") {
		t.Fatalf("buildWorkspaceBundle error = %v", err)
	}
}

func writeIndexingTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runIndexingTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
