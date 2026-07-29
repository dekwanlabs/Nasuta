package featuredelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxUsernameKeyBytes = 96
	ownerFileName       = ".nasuta-owner.json"
)

type workspaceOwner struct {
	Version     int    `json:"version"`
	UserID      int64  `json:"user_id"`
	UsernameKey string `json:"username_key"`
}

// WorkspaceManager owns stable user directory allocation and ownership checks.
type WorkspaceManager struct {
	store         Store
	worktreesRoot string
	now           func() time.Time
}

func NewWorkspaceManager(store Store, codingWorkRoot string) (*WorkspaceManager, error) {
	if store == nil {
		return nil, ErrUnavailable
	}
	root, err := secureDirectory(filepath.Join(codingWorkRoot, "worktrees"))
	if err != nil {
		return nil, fmt.Errorf("prepare coding worktree root: %w", err)
	}
	return &WorkspaceManager{
		store: store, worktreesRoot: root,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func NormalizeUsernameKey(value string) (string, error) {
	value = cases.Fold().String(norm.NFKC.String(strings.TrimSpace(value)))
	for _, r := range value {
		if r == '/' || r == '\\' || r == 0 || unicode.IsControl(r) {
			return "", fmt.Errorf("username contains an unsafe path character")
		}
	}
	var builder strings.Builder
	builder.Grow(len(value))
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '.' || r == '_' || r == '-' {
			builder.WriteRune(r)
			separator = false
			continue
		}
		if !separator {
			builder.WriteByte('-')
			separator = true
		}
	}
	key := strings.Trim(builder.String(), "._-")
	if key == "" {
		key = "user"
	}
	if key == "." || key == ".." || len(key) > maxUsernameKeyBytes || !utf8.ValidString(key) {
		return "", fmt.Errorf("normalized username is invalid or exceeds %d bytes", maxUsernameKeyBytes)
	}
	return key, nil
}

func (manager *WorkspaceManager) ResolveUserWorkspace(ctx context.Context, identity OwnerIdentity) (*UserWorkspace, string, error) {
	if identity.UserID <= 0 {
		return nil, "", fmt.Errorf("workspace owner is required")
	}
	existing, err := manager.store.GetUserWorkspace(ctx, identity.UserID)
	if err == nil {
		path, verifyErr := manager.ensureWorkspaceDirectory(*existing)
		return existing, path, verifyErr
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, "", err
	}

	snapshot := strings.TrimSpace(identity.Name)
	if snapshot == "" {
		snapshot = emailLocalPart(identity.Email)
	}
	if snapshot == "" {
		snapshot = "user"
	}
	base, err := NormalizeUsernameKey(snapshot)
	if err != nil {
		return nil, "", err
	}
	key := base
	occupied, err := manager.store.GetUserWorkspaceByKey(ctx, key)
	if err == nil && occupied.UserID != identity.UserID {
		key = collisionUsernameKey(base, identity.UserID)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, "", err
	}

	workspace := UserWorkspace{
		UserID: identity.UserID, UsernameKey: key,
		UsernameSnapshot: snapshot, CreatedAt: manager.now(),
	}
	if err := manager.store.CreateUserWorkspace(ctx, workspace); err != nil {
		if !errors.Is(err, ErrConflict) {
			return nil, "", err
		}
		current, getErr := manager.store.GetUserWorkspace(ctx, identity.UserID)
		if getErr != nil {
			return nil, "", fmt.Errorf("resolve concurrent user workspace: %w", getErr)
		}
		workspace = *current
	}
	path, err := manager.ensureWorkspaceDirectory(workspace)
	if err != nil {
		return nil, "", err
	}
	return &workspace, path, nil
}

func (manager *WorkspaceManager) UserWorkspacePath(workspace UserWorkspace) (string, error) {
	if workspace.UserID <= 0 {
		return "", fmt.Errorf("workspace owner is required")
	}
	key, err := NormalizeUsernameKey(workspace.UsernameKey)
	if err != nil || key != workspace.UsernameKey {
		return "", fmt.Errorf("workspace username key is not canonical")
	}
	return containedPath(manager.worktreesRoot, key+"-workspace")
}

func (manager *WorkspaceManager) RunPath(workspace UserWorkspace, runID string) (string, error) {
	if err := validateRunID(runID); err != nil {
		return "", err
	}
	parent, err := manager.UserWorkspacePath(workspace)
	if err != nil {
		return "", err
	}
	return containedPath(parent, runID)
}

func (manager *WorkspaceManager) ensureWorkspaceDirectory(workspace UserWorkspace) (string, error) {
	path, err := manager.UserWorkspacePath(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create user workspace %q: %w", path, err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect user workspace %q: %w", path, err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return "", fmt.Errorf("user workspace %q is not a managed directory", path)
	}
	ownerPath := filepath.Join(path, ownerFileName)
	expected := workspaceOwner{Version: 1, UserID: workspace.UserID, UsernameKey: workspace.UsernameKey}
	data, err := json.Marshal(expected)
	if err != nil {
		return "", fmt.Errorf("marshal workspace owner: %w", err)
	}
	file, err := os.OpenFile(ownerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(append(data, '\n')); writeErr != nil {
			_ = file.Close()
			return "", fmt.Errorf("write workspace owner: %w", writeErr)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close workspace owner: %w", err)
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create workspace owner: %w", err)
	}
	stored, err := os.ReadFile(ownerPath)
	if err != nil {
		return "", fmt.Errorf("read workspace owner: %w", err)
	}
	var actual workspaceOwner
	if err := json.Unmarshal(stored, &actual); err != nil {
		return "", fmt.Errorf("decode workspace owner: %w", err)
	}
	if actual != expected {
		return "", fmt.Errorf("workspace owner does not match persisted mapping")
	}
	return path, nil
}

func collisionUsernameKey(base string, userID int64) string {
	suffix := "-u" + strconv.FormatInt(userID, 36)
	limit := maxUsernameKeyBytes - len(suffix)
	base = truncateUTF8(base, limit)
	base = strings.TrimRight(base, "._-")
	if base == "" {
		base = "user"
	}
	return base + suffix
}

func emailLocalPart(email string) string {
	email = strings.TrimSpace(email)
	if index := strings.IndexByte(email, '@'); index > 0 {
		return email[:index]
	}
	return ""
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func validateRunID(value string) error {
	if value == "" || value == "." || value == ".." || len(value) > 64 {
		return fmt.Errorf("invalid run ID")
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-' {
			return fmt.Errorf("invalid run ID")
		}
	}
	return nil
}

func secureDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("managed root %q is not a directory", resolved)
	}
	return resolved, nil
}

func containedPath(root, element string) (string, error) {
	path := filepath.Join(root, element)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes managed root")
	}
	return path, nil
}
