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
