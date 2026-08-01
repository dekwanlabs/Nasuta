package featuredelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dekwanlabs/nasuta/platform"
)

const (
	maxGitOutputBytes     = 20 << 20
	maxChangedFiles       = 500
	maxValidationCommands = 20
	maxValidationArgs     = 32
	maxValidationArgBytes = 4096
	maxValidationOutput   = 2 << 20
)

type ValidationCommand struct {
	Argv       []string       `json:"argv"`
	Timeout    configDuration `json:"-"`
	RawTimeout string         `json:"timeout"`
}

type DeliveryConfig struct {
	Validation []ValidationCommand `json:"validation"`
}

type configDuration time.Duration

type PreparedWorktree struct {
	RepoPath         string
	WorktreePath     string
	BaseCommit       string
	Validation       []ValidationCommand
	ValidationExists bool
}

// GitManager owns repository resolution, isolated worktrees, patches, and validation.
type GitManager struct {
	git           string
	workspaceRoot string
	artifactsRoot string
	workspaces    *WorkspaceManager
}

func NewGitManager(workspaceRoot, codingWorkRoot string, workspaces *WorkspaceManager) (*GitManager, error) {
	if workspaces == nil {
		return nil, ErrUnavailable
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("find git: %w", err)
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root links: %w", err)
	}
	artifacts, err := secureDirectory(filepath.Join(codingWorkRoot, "artifacts"))
	if err != nil {
		return nil, fmt.Errorf("prepare coding artifact root: %w", err)
	}
	return &GitManager{
		git: git, workspaceRoot: root, artifactsRoot: artifacts, workspaces: workspaces,
	}, nil
}

func (manager *GitManager) ResolveBaseCommit(ctx context.Context, repo, baseRef string) (string, string, error) {
	repoPath, err := manager.resolveRepository(repo)
	if err != nil {
		return "", "", err
	}
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		baseRef = "HEAD"
	}
	format, err := manager.gitOutput(ctx, repoPath, nil, 1024, "rev-parse", "--show-object-format")
	if err != nil {
		return "", "", err
	}
	objectLength := 40
	switch strings.TrimSpace(string(format)) {
	case "sha1":
	case "sha256":
		objectLength = 64
	default:
		return "", "", fmt.Errorf("unsupported git object format %q", strings.TrimSpace(string(format)))
	}
	output, err := manager.gitOutput(ctx, repoPath, nil, 1024, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("resolve base ref %q: %w", baseRef, err)
	}
	commit := strings.TrimSpace(string(output))
	if len(commit) != objectLength || !isHex(commit) {
		return "", "", fmt.Errorf("git returned invalid base commit %q", commit)
	}
	return repoPath, strings.ToLower(commit), nil
}

func (manager *GitManager) PrepareWorktree(ctx context.Context, workspace UserWorkspace, runID, repo, baseRef, expectedCommit string) (*PreparedWorktree, error) {
	repoPath, commit, err := manager.ResolveBaseCommit(ctx, repo, baseRef)
	if err != nil {
		return nil, err
	}
	if expectedCommit != "" && commit != strings.ToLower(expectedCommit) {
		return nil, fmt.Errorf("base ref moved from fixed commit")
	}
	if _, err := manager.workspaces.ensureWorkspaceDirectory(workspace); err != nil {
		return nil, err
	}
	runPath, err := manager.workspaces.RunPath(workspace, runID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(runPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, fmt.Errorf("run worktree already exists")
		}
		return nil, fmt.Errorf("inspect run worktree: %w", err)
	}
	if _, err := manager.gitOutput(ctx, repoPath, nil, maxGitOutputBytes, "worktree", "add", "--detach", runPath, commit); err != nil {
		return nil, fmt.Errorf("create worktree: %w", err)
	}
	head, err := manager.gitOutput(ctx, runPath, nil, 1024, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(head)) != commit {
		return nil, fmt.Errorf("worktree HEAD does not match fixed base commit")
	}
	validation, exists, err := manager.loadDeliveryConfig(ctx, repoPath, commit)
	if err != nil {
		return nil, err
	}
	return &PreparedWorktree{
		RepoPath: repoPath, WorktreePath: runPath, BaseCommit: commit,
		Validation: validation, ValidationExists: exists,
	}, nil
}

func (manager *GitManager) BuildChangeSet(ctx context.Context, prepared PreparedWorktree, runID, providerSummary string) (ChangeSet, error) {
	if err := rejectNestedRepositories(prepared.WorktreePath); err != nil {
		return ChangeSet{}, err
	}
	index, err := os.CreateTemp("", "nasuta-index-*")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("create temporary git index: %w", err)
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		return ChangeSet{}, fmt.Errorf("close temporary git index: %w", err)
	}
	defer os.Remove(indexPath)
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "read-tree", prepared.BaseCommit); err != nil {
		return ChangeSet{}, err
	}
	if _, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "add", "-A", "--"); err != nil {
		return ChangeSet{}, fmt.Errorf("stage worktree in temporary index: %w", err)
	}
	if err := manager.rejectSubmoduleChanges(ctx, prepared, env); err != nil {
		return ChangeSet{}, err
	}
	nameStatus, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "diff", "--cached", "--name-status", "-z", prepared.BaseCommit)
	if err != nil {
		return ChangeSet{}, err
	}
	files, err := parseNameStatus(nameStatus)
	if err != nil {
		return ChangeSet{}, err
	}
	if len(files) > maxChangedFiles {
		return ChangeSet{}, fmt.Errorf("changed files exceed %d", maxChangedFiles)
	}
	numstat, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "diff", "--cached", "--numstat", "-z", prepared.BaseCommit)
	if err != nil {
		return ChangeSet{}, err
	}
	applyNumstat(files, numstat)
	patch, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "diff", "--cached", "--binary", "--full-index", prepared.BaseCommit)
	if err != nil {
		return ChangeSet{}, err
	}
	artifactDir, err := containedPath(manager.artifactsRoot, runID)
	if err != nil {
		return ChangeSet{}, err
	}
	if err := os.Mkdir(artifactDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return ChangeSet{}, fmt.Errorf("create run artifact directory: %w", err)
	}
	patchName := "changes.patch"
	patchPath := filepath.Join(artifactDir, patchName)
	if err := writeAtomic(patchPath, patch, 0o600); err != nil {
		return ChangeSet{}, fmt.Errorf("write patch: %w", err)
	}
	sum := sha256.Sum256(patch)
	head, err := manager.gitOutput(ctx, prepared.WorktreePath, nil, 1024, "rev-parse", "HEAD")
	if err != nil {
		return ChangeSet{}, err
	}
	change := ChangeSet{
		RunID: runID, WorktreeHead: strings.TrimSpace(string(head)),
		PatchRelPath: filepath.ToSlash(filepath.Join(runID, patchName)),
		PatchSHA256:  hex.EncodeToString(sum[:]), PatchBytes: int64(len(patch)),
		FilesChanged: len(files), Files: files, ProviderSummary: truncateText(providerSummary, 8000),
		CreatedAt: time.Now().UTC(),
	}
	for _, file := range files {
		change.Additions += file.Additions
		change.Deletions += file.Deletions
	}
	return change, nil
}

func (manager *GitManager) RunValidation(ctx context.Context, prepared PreparedWorktree, runID string) ([]ValidationResult, error) {
	if !prepared.ValidationExists {
		return []ValidationResult{{Sequence: 1, Status: "validation_not_configured"}}, nil
	}
	validationHome, err := os.MkdirTemp("", "nasuta-validation-*")
	if err != nil {
		return nil, fmt.Errorf("prepare validation environment: %w", err)
	}
	defer os.RemoveAll(validationHome)
	environment := validationEnvironment(validationHome)
	results := make([]ValidationResult, 0, len(prepared.Validation))
	artifactDir, err := containedPath(manager.artifactsRoot, runID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare validation artifacts: %w", err)
	}
	for index, command := range prepared.Validation {
		started := time.Now()
		commandCtx, cancel := context.WithTimeout(ctx, time.Duration(command.Timeout))
		output, exitCode, timedOut, runErr := runBoundedCommand(commandCtx, prepared.WorktreePath, environment, maxValidationOutput, command.Argv[0], command.Argv[1:]...)
		cancel()
		redactedOutput := []byte(platform.RedactSensitiveText(string(output)))
		if len(redactedOutput) > maxValidationOutput {
			return results, fmt.Errorf("redacted validation output exceeds %d bytes", maxValidationOutput)
		}
		name := fmt.Sprintf("validation-%02d.log", index+1)
		path := filepath.Join(artifactDir, name)
		if err := writeAtomic(path, redactedOutput, 0o600); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(redactedOutput)
		status := "passed"
		if runErr != nil {
			status = "failed"
		}
		result := ValidationResult{
			Sequence: index + 1, Argv: redactValidationArgv(command.Argv),
			Status: status, ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(),
			OutputSummary: truncateText(string(redactedOutput), 4000),
			OutputRelPath: filepath.ToSlash(filepath.Join(runID, name)),
			OutputSHA256:  hex.EncodeToString(sum[:]), OutputBytes: int64(len(redactedOutput)), TimedOut: timedOut,
		}
		results = append(results, result)
		if runErr != nil {
			return results, fmt.Errorf("validation command %d failed: %w", index+1, runErr)
		}
	}
	return results, nil
}

func (manager *GitManager) ArtifactPath(relative string) (string, error) {
	path, err := containedPath(manager.artifactsRoot, filepath.FromSlash(relative))
	if err != nil {
		return "", err
	}
	return path, nil
}

func (manager *GitManager) PatchPath(relative string) (string, error) {
	return manager.ArtifactPath(relative)
}

func (manager *GitManager) RemoveWorktree(ctx context.Context, run ImplementationRun) error {
	if err := manager.verifyRunArtifacts(run); err != nil {
		return err
	}
	workspace := UserWorkspace{UserID: run.WorkspaceUserID, UsernameKey: run.WorkspaceUsername}
	if _, err := manager.workspaces.ensureWorkspaceDirectory(workspace); err != nil {
		return err
	}
	runPath, err := manager.workspaces.RunPath(workspace, run.ID)
	if err != nil {
		return err
	}
	repoPath, err := manager.resolveRepository(run.Repo)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(runPath); errors.Is(err, os.ErrNotExist) {
		registered, listErr := manager.worktreeRegistered(ctx, repoPath, runPath)
		if listErr != nil {
			return listErr
		}
		if !registered {
			return nil
		}
		if _, pruneErr := manager.gitOutput(ctx, repoPath, nil, maxGitOutputBytes, "worktree", "prune", "--expire", "now"); pruneErr != nil {
			return fmt.Errorf("prune missing worktree metadata: %w", pruneErr)
		}
		registered, listErr = manager.worktreeRegistered(ctx, repoPath, runPath)
		if listErr != nil {
			return listErr
		}
		if registered {
			return fmt.Errorf("missing worktree remains registered with Git")
		}
		return nil
	} else if err != nil {
		return err
	}
	_, err = manager.gitOutput(ctx, repoPath, nil, maxGitOutputBytes, "worktree", "remove", "--force", runPath)
	return err
}

func (manager *GitManager) worktreeRegistered(ctx context.Context, repoPath, runPath string) (bool, error) {
	output, err := manager.gitOutput(ctx, repoPath, nil, maxGitOutputBytes, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	canonicalRunPath := filepath.Clean(runPath)
	for _, field := range bytes.Split(output, []byte{0}) {
		if !bytes.HasPrefix(field, []byte("worktree ")) {
			continue
		}
		path := filepath.Clean(string(bytes.TrimPrefix(field, []byte("worktree "))))
		if path == canonicalRunPath {
			return true, nil
		}
	}
	return false, nil
}

func (manager *GitManager) resolveRepository(repo string) (string, error) {
	normalized, err := NormalizeRepository(repo)
	if err != nil {
		return "", err
	}
	reposRoot := filepath.Join(manager.workspaceRoot, "repos")
	path, err := containedPath(reposRoot, filepath.FromSlash(normalized))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository %q: %w", normalized, err)
	}
	relative, err := filepath.Rel(reposRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository %q escapes workspace", normalized)
	}
	if info, err := os.Stat(filepath.Join(resolved, ".git")); err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return "", fmt.Errorf("repository %q is not a Git repository", normalized)
	}
	return resolved, nil
}

func (manager *GitManager) loadDeliveryConfig(ctx context.Context, repoPath, commit string) ([]ValidationCommand, bool, error) {
	raw, err := manager.gitOutput(ctx, repoPath, nil, 256<<10, "show", commit+":.nasuta/delivery.json")
	if err != nil {
		if isMissingGitPath(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read delivery config: %w", err)
	}
	var payload struct {
		Validation []struct {
			Argv    []string `json:"argv"`
			Timeout string   `json:"timeout"`
		} `json:"validation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, true, fmt.Errorf("decode delivery config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, true, fmt.Errorf("decode delivery config: multiple JSON values")
	}
	if len(payload.Validation) > maxValidationCommands {
		return nil, true, fmt.Errorf("validation commands exceed %d", maxValidationCommands)
	}
	commands := make([]ValidationCommand, 0, len(payload.Validation))
	for index, item := range payload.Validation {
		if len(item.Argv) == 0 || len(item.Argv) > maxValidationArgs {
			return nil, true, fmt.Errorf("validation command %d has invalid argv length", index+1)
		}
		for _, argument := range item.Argv {
			if argument == "" || len(argument) > maxValidationArgBytes || strings.ContainsRune(argument, 0) {
				return nil, true, fmt.Errorf("validation command %d contains an invalid argument", index+1)
			}
		}
		timeout, err := time.ParseDuration(item.Timeout)
		if err != nil || timeout <= 0 || timeout > time.Hour {
			return nil, true, fmt.Errorf("validation command %d has invalid timeout", index+1)
		}
		commands = append(commands, ValidationCommand{
			Argv: append([]string(nil), item.Argv...), Timeout: configDuration(timeout), RawTimeout: item.Timeout,
		})
	}
	return commands, true, nil
}

func (manager *GitManager) rejectSubmoduleChanges(ctx context.Context, prepared PreparedWorktree, env []string) error {
	output, err := manager.gitOutput(ctx, prepared.WorktreePath, env, maxGitOutputBytes, "diff", "--cached", "--raw", "-z", prepared.BaseCommit)
	if err != nil {
		return err
	}
	if rawDiffHasSubmodule(output) {
		return fmt.Errorf("submodule modifications are not supported")
	}
	return nil
}

func rawDiffHasSubmodule(output []byte) bool {
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 || record[0] != ':' {
			continue
		}
		fields := bytes.Fields(record[1:])
		if len(fields) >= 2 && (bytes.Equal(fields[0], []byte("160000")) || bytes.Equal(fields[1], []byte("160000"))) {
			return true
		}
	}
	return false
}

func (manager *GitManager) gitOutput(ctx context.Context, dir string, env []string, limit int, args ...string) ([]byte, error) {
	output, _, _, err := runBoundedCommand(ctx, dir, env, limit, manager.git, args...)
	if err != nil && len(output) > 0 {
		return output, fmt.Errorf("%w: %s", err, strings.TrimSpace(strings.ToValidUTF8(string(output), "")))
	}
	return output, err
}

func runBoundedCommand(ctx context.Context, dir string, env []string, limit int, name string, args ...string) ([]byte, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, -1, errors.Is(err, context.DeadlineExceeded), err
	}
	command := exec.Command(name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = env
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	buffer := &boundedBuffer{limit: limit}
	command.Stdout = buffer
	command.Stderr = buffer
	if err := command.Start(); err != nil {
		return nil, -1, false, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = terminateCommandGroup(command.Process.Pid, done)
		if err == nil {
			err = ctx.Err()
		}
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	if buffer.exceeded {
		return buffer.Bytes(), exitCode, timedOut, fmt.Errorf("command output exceeds %d bytes", limit)
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return buffer.Bytes(), exitCode, timedOut, fmt.Errorf("run %q: %w", name, err)
	}
	return buffer.Bytes(), exitCode, timedOut, nil
}

func terminateCommandGroup(pid int, done <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-done
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.Buffer.Write(data)
	return original, nil
}

func rejectNestedRepositories(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Name() == ".git" {
			if relative == ".git" {
				return nil
			}
			return fmt.Errorf("nested Git repository %q is not supported", relative)
		}
		return nil
	})
}

func parseNameStatus(raw []byte) ([]ChangedFile, error) {
	parts := bytes.Split(raw, []byte{0})
	files := make([]ChangedFile, 0, len(parts)/2)
	for index := 0; index < len(parts) && len(parts[index]) > 0; {
		status := string(parts[index])
		index++
		if index >= len(parts) {
			return nil, fmt.Errorf("invalid git name-status output")
		}
		path := string(parts[index])
		index++
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if index >= len(parts) {
				return nil, fmt.Errorf("invalid git rename output")
			}
			path = string(parts[index])
			index++
		}
		files = append(files, ChangedFile{Path: filepath.ToSlash(path), Status: status[:1]})
	}
	return files, nil
}

func applyNumstat(files []ChangedFile, raw []byte) {
	byPath := make(map[string]int, len(files))
	for index := range files {
		byPath[files[index].Path] = index
	}
	records := bytes.Split(raw, []byte{0})
	for recordIndex := 0; recordIndex < len(records); recordIndex++ {
		record := records[recordIndex]
		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if len(path) == 0 {
			if recordIndex+2 >= len(records) {
				continue
			}
			recordIndex += 2
			path = records[recordIndex]
		}
		canonicalPath := filepath.ToSlash(string(path))
		index, ok := byPath[canonicalPath]
		if !ok {
			continue
		}
		if string(fields[0]) == "-" || string(fields[1]) == "-" {
			files[index].Binary = true
			continue
		}
		files[index].Additions, _ = strconv.Atoi(string(fields[0]))
		files[index].Deletions, _ = strconv.Atoi(string(fields[1]))
	}
}

func validationEnvironment(home string) []string {
	out := make([]string, 0, 5)
	for _, key := range []string{"PATH", "LANG", "LC_ALL"} {
		if value := os.Getenv(key); value != "" {
			out = append(out, key+"="+value)
		}
	}
	out = append(out, "HOME="+home, "TMPDIR="+home)
	return out
}

func redactValidationArgv(argv []string) []string {
	redacted := make([]string, len(argv))
	for index, argument := range argv {
		redacted[index] = platform.RedactSensitiveText(argument)
		if index > 0 && redacted[index] == argument {
			key := strings.TrimLeft(argv[index-1], "-")
			redacted[index] = platform.RedactConfigValue(key, argument)
		}
	}
	return redacted
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".nasuta-write-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (manager *GitManager) verifyRunArtifacts(run ImplementationRun) error {
	if run.ChangeSet == nil {
		return nil
	}
	if err := manager.verifyArtifact(run.ChangeSet.PatchRelPath, run.ChangeSet.PatchSHA256, run.ChangeSet.PatchBytes); err != nil {
		return fmt.Errorf("verify patch: %w", err)
	}
	for _, validation := range run.ChangeSet.ValidationResults {
		if validation.OutputRelPath == "" {
			continue
		}
		if err := manager.verifyArtifact(validation.OutputRelPath, validation.OutputSHA256, validation.OutputBytes); err != nil {
			return fmt.Errorf("verify validation output %d: %w", validation.Sequence, err)
		}
	}
	return nil
}

func (manager *GitManager) verifyArtifact(relative, expectedHash string, expectedBytes int64) error {
	if expectedHash == "" || len(expectedHash) != sha256.Size*2 || !isHex(expectedHash) {
		return fmt.Errorf("artifact hash is invalid")
	}
	path, err := containedPath(manager.artifactsRoot, filepath.FromSlash(relative))
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxGitOutputBytes+1)); err != nil {
		return err
	}
	if info, err := file.Stat(); err != nil {
		return err
	} else if info.Size() != expectedBytes {
		return fmt.Errorf("artifact size mismatch")
	} else if info.Size() > maxGitOutputBytes {
		return fmt.Errorf("artifact exceeds %d bytes", maxGitOutputBytes)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash) {
		return fmt.Errorf("artifact hash mismatch")
	}
	return nil
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func isMissingGitPath(err error) bool {
	text := err.Error()
	return strings.Contains(text, "does not exist in") || strings.Contains(text, "exists on disk, but not in")
}

var _ io.Writer = (*boundedBuffer)(nil)
