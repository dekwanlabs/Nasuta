package auth

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUpsertUserAssignsSystemRole(t *testing.T) {
	tests := []struct {
		name        string
		hasUsers    bool
		userID      int64
		storedAdmin int
		hasRoles    bool
		roleName    string
		roleID      int64
	}{
		{name: "first user is administrator", userID: 1, storedAdmin: 1, roleName: adminRole, roleID: 10},
		{name: "later user gets default role", hasUsers: true, userID: 2, roleName: defaultRole, roleID: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := &DB{db: db}
			user := &User{
				FeishuUID: "feishu-user",
				OpenID:    "open-id",
				Name:      "User",
				Email:     "user@example.com",
			}

			expectUserUpsert(mock, user, tt.hasUsers, tt.userID, tt.storedAdmin, tt.hasRoles, tt.roleName, tt.roleID)
			if err := store.UpsertUser(user); err != nil {
				t.Fatal(err)
			}
			if user.ID != tt.userID || user.IsAdmin != (tt.storedAdmin == 1) {
				t.Fatalf("user = %#v", user)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
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
	expectUserUpsert(mock, user, true, 1, 1, false, adminRole, 10)
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

func TestUpsertUserPreservesExistingNonAdminRoles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &DB{db: db}
	user := &User{FeishuUID: "member", OpenID: "open-2", Name: "Member", Email: "member@example.com"}

	expectUserUpsert(mock, user, true, 2, 0, true, defaultRole, 20)
	if err := store.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if user.IsAdmin {
		t.Fatal("non-administrator gained administrator status")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUserWithPasswordAssignsSystemRole(t *testing.T) {
	tests := []struct {
		name     string
		hasUsers bool
		userID   int64
		roleName string
		roleID   int64
	}{
		{name: "first user is administrator", userID: 1, roleName: adminRole, roleID: 10},
		{name: "later user gets default role", hasUsers: true, userID: 2, roleName: defaultRole, roleID: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := &DB{db: db}

			expectPasswordRegistration(mock, "user@example.com", "User", "hash", tt.hasUsers, tt.userID, tt.roleName, tt.roleID)
			user, err := store.CreateUserWithPassword("user@example.com", "User", "hash")
			if err != nil {
				t.Fatal(err)
			}
			if user.ID != tt.userID || user.IsAdmin != !tt.hasUsers {
				t.Fatalf("user = %#v", user)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateUserWithPasswordRollsBackWhenRoleAssignmentFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &DB{db: db}
	assignErr := errors.New("write failed")

	mock.ExpectBegin()
	expectEmailLookup(mock, "user@example.com")
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM users LIMIT 1\)`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
	mock.ExpectExec(`INSERT INTO users`).
		WithArgs("User", "user@example.com", "hash", 0).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectQuery(`SELECT id FROM rbac_roles WHERE name=\?`).
		WithArgs(defaultRole).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(20))
	mock.ExpectExec(`INSERT IGNORE INTO rbac_user_roles`).
		WithArgs(int64(2), int64(20)).
		WillReturnError(assignErr)
	mock.ExpectRollback()

	user, err := store.CreateUserWithPassword("user@example.com", "User", "hash")
	if user != nil || !errors.Is(err, assignErr) {
		t.Fatalf("user = %#v, err = %v", user, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectUserUpsert(
	mock sqlmock.Sqlmock,
	user *User,
	hasUsers bool,
	userID int64,
	storedAdmin int,
	hasRoles bool,
	roleName string,
	roleID int64,
) {
	mock.ExpectBegin()
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
	if storedAdmin == 0 {
		hasRolesValue := 0
		if hasRoles {
			hasRolesValue = 1
		}
		mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM rbac_user_roles WHERE user_id=\? LIMIT 1\)`).
			WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(hasRolesValue))
		if hasRoles {
			mock.ExpectCommit()
			return
		}
	}
	mock.ExpectQuery(`SELECT id FROM rbac_roles WHERE name=\?`).
		WithArgs(roleName).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(roleID))
	mock.ExpectExec(`INSERT IGNORE INTO rbac_user_roles`).
		WithArgs(userID, roleID).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
}

func expectPasswordRegistration(
	mock sqlmock.Sqlmock,
	email string,
	name string,
	passwordHash string,
	hasUsers bool,
	userID int64,
	roleName string,
	roleID int64,
) {
	mock.ExpectBegin()
	expectEmailLookup(mock, email)
	hasUsersValue := 0
	adminFlag := 1
	if hasUsers {
		hasUsersValue = 1
		adminFlag = 0
	}
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM users LIMIT 1\)`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(hasUsersValue))
	mock.ExpectExec(`INSERT INTO users`).
		WithArgs(name, email, passwordHash, adminFlag).
		WillReturnResult(sqlmock.NewResult(userID, 1))
	mock.ExpectQuery(`SELECT id FROM rbac_roles WHERE name=\?`).
		WithArgs(roleName).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(roleID))
	mock.ExpectExec(`INSERT IGNORE INTO rbac_user_roles`).
		WithArgs(userID, roleID).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
}

func expectEmailLookup(mock sqlmock.Sqlmock, email string) {
	mock.ExpectQuery(`SELECT id,COALESCE\(feishu_uid,''\),open_id,name,email,avatar_url,department,is_admin,password_hash FROM users WHERE email=\?`).
		WithArgs(email).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "feishu_uid", "open_id", "name", "email", "avatar_url", "department", "is_admin", "password_hash",
		}))
}
