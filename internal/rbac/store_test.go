package rbac

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

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
