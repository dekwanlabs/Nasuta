package delivery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeUsernameKey(t *testing.T) {
	key, err := NormalizeUsernameKey("  Ａlice Zhang  ")
	if err != nil {
		t.Fatal(err)
	}
	if key != "alice-zhang" {
		t.Fatalf("key=%q", key)
	}
	for _, value := range []string{"a/b", `a\b`, "a\x00b", "a\nb"} {
		if _, err := NormalizeUsernameKey(value); err == nil {
			t.Fatalf("expected unsafe username %q to fail", value)
		}
	}
}

func TestWorkspaceManagerReusesStableMapping(t *testing.T) {
	store := &workspaceStore{}
	manager, err := NewWorkspaceManager(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, firstPath, err := manager.ResolveUserWorkspace(context.Background(), OwnerIdentity{
		UserID: 7, Name: "Alice Zhang",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, secondPath, err := manager.ResolveUserWorkspace(context.Background(), OwnerIdentity{
		UserID: 7, Name: "Renamed User",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.UsernameKey != "alice-zhang" || second.UsernameKey != first.UsernameKey || firstPath != secondPath {
		t.Fatalf("workspace was not stable: %#v %#v", first, second)
	}
	if _, err := os.Stat(filepath.Join(firstPath, ownerFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceManagerAddsCollisionSuffix(t *testing.T) {
	store := &workspaceStore{
		byUser: map[int64]UserWorkspace{1: {UserID: 1, UsernameKey: "alice"}},
		byKey:  map[string]UserWorkspace{"alice": {UserID: 1, UsernameKey: "alice"}},
	}
	manager, err := NewWorkspaceManager(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, _, err := manager.ResolveUserWorkspace(context.Background(), OwnerIdentity{UserID: 71, Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.UsernameKey != "alice-u1z" {
		t.Fatalf("unexpected collision key %q", workspace.UsernameKey)
	}
}

func TestWorkspaceManagerRejectsOwnerMismatch(t *testing.T) {
	store := &workspaceStore{}
	manager, err := NewWorkspaceManager(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, path, err := manager.ResolveUserWorkspace(context.Background(), OwnerIdentity{UserID: 7, Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"user_id":8,"username_key":"alice"}`)
	if err := os.WriteFile(filepath.Join(path, ownerFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ResolveUserWorkspace(context.Background(), OwnerIdentity{UserID: workspace.UserID}); err == nil {
		t.Fatal("expected owner mismatch")
	}
}

type workspaceStore struct {
	Store
	byUser map[int64]UserWorkspace
	byKey  map[string]UserWorkspace
}

func (store *workspaceStore) GetUserWorkspace(_ context.Context, userID int64) (*UserWorkspace, error) {
	if value, ok := store.byUser[userID]; ok {
		copy := value
		return &copy, nil
	}
	return nil, ErrNotFound
}

func (store *workspaceStore) GetUserWorkspaceByKey(_ context.Context, key string) (*UserWorkspace, error) {
	if value, ok := store.byKey[key]; ok {
		copy := value
		return &copy, nil
	}
	return nil, ErrNotFound
}

func (store *workspaceStore) CreateUserWorkspace(_ context.Context, workspace UserWorkspace) error {
	if store.byUser == nil {
		store.byUser = make(map[int64]UserWorkspace)
	}
	if store.byKey == nil {
		store.byKey = make(map[string]UserWorkspace)
	}
	if _, ok := store.byUser[workspace.UserID]; ok {
		return ErrConflict
	}
	if _, ok := store.byKey[workspace.UsernameKey]; ok {
		return ErrConflict
	}
	store.byUser[workspace.UserID] = workspace
	store.byKey[workspace.UsernameKey] = workspace
	return nil
}
