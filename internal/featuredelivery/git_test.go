package featuredelivery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/platform"
)

func TestApplyNumstatHandlesRenames(t *testing.T) {
	files := []ChangedFile{{Path: "new/name.go", Status: "R"}}
	applyNumstat(files, []byte("12\t3\t\x00old/name.go\x00new/name.go\x00"))
	if files[0].Additions != 12 || files[0].Deletions != 3 {
		t.Fatalf("rename stats = +%d -%d", files[0].Additions, files[0].Deletions)
	}
}

func TestRawDiffHasSubmoduleChecksBothModes(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(":160000 100644 a b M\x00module\x00"),
		[]byte(":100644 160000 a b M\x00module\x00"),
	} {
		if !rawDiffHasSubmodule(raw) {
			t.Fatalf("submodule mode not detected in %q", raw)
		}
	}
	if rawDiffHasSubmodule([]byte(":100644 100755 a b M\x00script\x00")) {
		t.Fatal("ordinary mode change detected as submodule")
	}
}

func TestLoadDeliveryConfigRejectsTrailingJSON(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, git, repo, "init")
	runGit(t, git, repo, "config", "user.email", "test@example.com")
	runGit(t, git, repo, "config", "user.name", "Test")
	if err := os.Mkdir(filepath.Join(repo, ".nasuta"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"validation":[{"argv":["go","test","./..."],"timeout":"1m"}]} {}`)
	if err := os.WriteFile(filepath.Join(repo, ".nasuta", "delivery.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "add", ".nasuta/delivery.json")
	runGit(t, git, repo, "commit", "-m", "config")

	manager := &GitManager{git: git}
	if _, _, err := manager.loadDeliveryConfig(context.Background(), repo, "HEAD"); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
}

func TestLoadDeliveryConfigAllowsMissingFile(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, git, repo, "init")
	runGit(t, git, repo, "config", "user.email", "test@example.com")
	runGit(t, git, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "add", "README.md")
	runGit(t, git, repo, "commit", "-m", "initial")

	manager := &GitManager{git: git}
	commands, exists, err := manager.loadDeliveryConfig(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if exists || commands != nil {
		t.Fatalf("exists=%t commands=%v", exists, commands)
	}
}

func TestVerifyArtifactDetectsHashMismatch(t *testing.T) {
	root := t.TempDir()
	manager := &GitManager{artifactsRoot: root}
	if err := os.WriteFile(filepath.Join(root, "patch"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.verifyArtifact("patch", strings.Repeat("0", 64), 0); err == nil {
		t.Fatal("hash mismatch must fail")
	}
}

func TestRunValidationIsolatesHomeAndRedactsArtifacts(t *testing.T) {
	originalHome := t.TempDir()
	t.Setenv("HOME", originalHome)
	artifactsRoot := t.TempDir()
	manager := &GitManager{artifactsRoot: artifactsRoot}
	prepared := PreparedWorktree{
		WorktreePath:     t.TempDir(),
		ValidationExists: true,
		Validation: []ValidationCommand{{
			Argv: []string{
				"sh", "-c", `printf 'home=%s\napi_key=output-secret\n' "$HOME"`,
				"validation", "--token", "argument-secret",
			},
			Timeout: configDuration(5 * time.Second),
		}},
	}

	results, err := manager.RunValidation(context.Background(), prepared, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("validation results = %d", len(results))
	}
	result := results[0]
	if result.OutputBytes <= 0 {
		t.Fatalf("validation output bytes = %d", result.OutputBytes)
	}
	if result.Argv[len(result.Argv)-1] != platform.RedactedValue {
		t.Fatalf("sensitive argv = %q", result.Argv[len(result.Argv)-1])
	}
	if strings.Contains(result.OutputSummary, originalHome) || strings.Contains(result.OutputSummary, "output-secret") {
		t.Fatalf("validation summary leaked sensitive data: %s", result.OutputSummary)
	}
	output, err := os.ReadFile(filepath.Join(artifactsRoot, filepath.FromSlash(result.OutputRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), originalHome) || strings.Contains(string(output), "output-secret") {
		t.Fatalf("validation artifact leaked sensitive data: %s", output)
	}
	if !strings.Contains(string(output), platform.RedactedValue) {
		t.Fatalf("validation artifact was not redacted: %s", output)
	}
	if result.OutputBytes != int64(len(output)) {
		t.Fatalf("validation output bytes = %d, file size = %d", result.OutputBytes, len(output))
	}
}

func TestRunBoundedCommandCancelsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, timedOut, err := runBoundedCommand(
		ctx, t.TempDir(), validationEnvironment(t.TempDir()), 1024,
		"sh", "-c", "sleep 30 & wait",
	)
	if err == nil || !timedOut {
		t.Fatalf("run result timedOut=%t err=%v", timedOut, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("process group termination took %v", elapsed)
	}
}

func runGit(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	command := exec.Command(git, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
