package rbac

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListMCPKeysReturnsOnlyPreview(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	createdAt := time.Now()
	mock.ExpectQuery(`SELECT id, user_id, key_name, CONCAT\(LEFT\(api_key, 12\), '\.\.\.'\), is_active, created_at, expires_at`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "user_id", "key_name", "key_preview", "is_active", "created_at", "expires_at",
		}).AddRow(7, 42, "local-agent", "mcp-abcdefgh...", 1, createdAt, nil))

	store := &Store{db: db}
	keys, err := store.ListMCPKeys(42)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].KeyPreview != "mcp-abcdefgh..." {
		t.Fatalf("keys = %#v", keys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateMCPKeyUsesActiveUnexpiredRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*WHERE api_key=\?.*AND is_active=1.*expires_at IS NULL OR expires_at > CURRENT_TIMESTAMP.*LIMIT 1`).
		WithArgs("mcp-assigned-key").
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(1))

	store := &Store{db: db}
	valid, err := store.AuthenticateMCPKey(context.Background(), "mcp-assigned-key")
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("assigned MCP key was not accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeMCPKeyScopesUpdateToOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE rbac_mcp_keys SET is_active=0 WHERE id=\? AND user_id=\?`).
		WithArgs(int64(7), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	store := &Store{db: db}
	if err := store.RevokeMCPKey(42, 7); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListUsersBatchesRoleHydration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id, name, email, is_admin FROM users ORDER BY id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "is_admin"}).
			AddRow(10, "A", "a@example.com", 1).
			AddRow(20, "B", "b@example.com", 0))
	mock.ExpectQuery(`SELECT ur\.user_id, r\.id, r\.name, r\.description, COALESCE\(r\.prompt,''\).*WHERE ur\.user_id IN \(\?,\?\)`).
		WithArgs(int64(10), int64(20)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "id", "name", "description", "prompt"}).
			AddRow(10, 1, "admin", "Administrator", "admin prompt").
			AddRow(20, 2, "reader", "Reader", ""))

	store := &Store{db: db}
	users, err := store.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || len(users[0].Roles) != 1 || users[0].Roles[0].Name != "admin" || users[1].Roles[0].Name != "reader" {
		t.Fatalf("users = %#v", users)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRepairEnsuresDefaultRolesAndPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT IGNORE INTO rbac_roles`).
		WithArgs(
			adminRoleName, "Administrator with full access",
			userRoleName, "Standard user access",
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM rbac_menus WHERE path = '/rbac'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM rbac_menus WHERE path = '/features'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`(?s)INSERT IGNORE INTO rbac_user_roles.*WHERE u\.is_admin = 1`).
		WithArgs(adminRoleName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT IGNORE INTO rbac_user_roles.*LEFT JOIN rbac_user_roles.*WHERE u\.is_admin = 0 AND ur\.user_id IS NULL`).
		WithArgs(userRoleName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT IGNORE INTO rbac_role_menus.*WHERE r\.name = \?`).
		WithArgs(adminRoleName).
		WillReturnResult(sqlmock.NewResult(0, 6))
	mock.ExpectExec(`(?s)INSERT IGNORE INTO rbac_role_menus.*WHERE r\.name = \? AND m\.path NOT IN \('/settings', '/rbac'\)`).
		WithArgs(userRoleName).
		WillReturnResult(sqlmock.NewResult(0, 4))

	store := &Store{db: db}
	if err := store.repair(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
