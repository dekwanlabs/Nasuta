package auth

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUpsertUserGrantsAdminOnlyToFirstUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &DB{db: db}

	first := &User{FeishuUID: "first", OpenID: "open-1", Name: "First", Email: "first@example.com"}
	expectUserUpsert(mock, first, false, 1, 1, true)
	if err := store.UpsertUser(first); err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 || !first.IsAdmin {
		t.Fatalf("first user = %#v, want ID 1 admin", first)
	}

	second := &User{FeishuUID: "second", OpenID: "open-2", Name: "Second", Email: "second@example.com"}
	expectUserUpsert(mock, second, true, 2, 0, false)
	if err := store.UpsertUser(second); err != nil {
		t.Fatal(err)
	}
	if second.ID != 2 || second.IsAdmin {
		t.Fatalf("second user = %#v, want ID 2 non-admin", second)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertUserReassignsMissingRoleForExistingAdmin(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &DB{db: db}

	user := &User{FeishuUID: "first", OpenID: "open-1", Name: "First", Email: "first@example.com"}
	expectUserUpsert(mock, user, true, 1, 1, true)
	if err := store.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if !user.IsAdmin {
		t.Fatal("existing administrator lost admin status")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectUserUpsert(mock sqlmock.Sqlmock, user *User, hasUsers bool, userID int64, storedAdmin int, expectAdminRole bool) {
	hasUsersValue := 0
	if hasUsers {
		hasUsersValue = 1
	}
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM users LIMIT 1\)`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(hasUsersValue))
	insertAdmin := 0
	if !hasUsers {
		insertAdmin = 1
	}
	mock.ExpectExec(`INSERT INTO users`).
		WithArgs(user.FeishuUID, user.OpenID, user.Name, user.Email, user.AvatarURL, user.Department, insertAdmin).
		WillReturnResult(sqlmock.NewResult(userID, 1))
	mock.ExpectQuery(`SELECT id,is_admin FROM users WHERE feishu_uid=\?`).
		WithArgs(user.FeishuUID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "is_admin"}).AddRow(userID, storedAdmin))
	if expectAdminRole {
		mock.ExpectExec(`INSERT IGNORE INTO rbac_user_roles`).
			WithArgs(userID, adminRoleID).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
}
