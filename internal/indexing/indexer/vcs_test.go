package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoDirName(t *testing.T) {
	cases := map[string]string{
		"group/proj":                 "group/proj",
		"group/sub/proj":             "sub/proj",
		"/group/proj/":               "group/proj",
		"proj":                       "proj",
		"  group/proj  ":             "group/proj",
		"backend/user/hsas-app-user": "user/hsas-app-user",
	}
	for in, want := range cases {
		if got := RepoDirName(in); got != want {
			t.Errorf("RepoDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthURLInjectsToken(t *testing.T) {
	s := NewSyncer("s3cr3t", 4)
	got, err := s.authURL("https://gitlab.example.com/group/proj.git")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://oauth2:s3cr3t@gitlab.example.com/group/proj.git"
	if got != want {
		t.Errorf("authURL = %q, want %q", got, want)
	}
}

func TestCloneOrFetchRefreshesExistingOriginCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	runTestGit(t, "", "init", dir)
	runTestGit(t, dir, "remote", "add", "origin", "https://oauth2:old-token@gitlab.example.com/group/proj.git")

	syncer := NewSyncer("new-token", 1)
	project := Project{
		PathWithNamespace: "group/proj",
		HTTPURLToRepo:     "http://127.0.0.1:1/group/proj.git",
		DefaultBranch:     "main",
	}
	if err := syncer.CloneOrFetch(context.Background(), project, dir, project.DefaultBranch); err == nil {
		t.Fatal("CloneOrFetch unexpectedly succeeded")
	}

	out := runTestGit(t, dir, "remote", "get-url", "origin")
	want := "http://oauth2:new-token@127.0.0.1:1/group/proj.git"
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("origin URL = %q, want %q", got, want)
	}
}

func TestSyncAllReturnsProjectFailure(t *testing.T) {
	syncer := NewSyncer("token", 1)
	projects := []Project{{
		PathWithNamespace: "group/proj",
		HTTPURLToRepo:     "http://127.0.0.1:1/group/proj.git",
		DefaultBranch:     "main",
	}}
	synced, err := syncer.SyncAll(context.Background(), projects, t.TempDir())
	if err == nil {
		t.Fatal("SyncAll unexpectedly succeeded")
	}
	if len(synced) != 0 {
		t.Fatalf("synced %d projects, want 0", len(synced))
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
