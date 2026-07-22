package auth

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	_ "github.com/go-sql-driver/mysql"
)

const (
	tblUsers     = "users"
	tblSessions  = "sessions"
	tblSettings  = "settings"
	tblUserRoles = "rbac_user_roles"
	adminRoleID  = 1
)

// DB owns MySQL-backed auth, session, history, and settings data.
type DB struct {
	db *sql.DB
}

// NewDB opens a MySQL connection and ensures the auth schema exists.
func NewDB(dsn string) (*DB, error) {
	db, err := store.MySQL(dsn)
	if err != nil {
		return nil, fmt.Errorf("auth: open: %w", err)
	}
	store := &DB{db: db}
	if err := store.migrate(); err != nil {
		return nil, fmt.Errorf("auth: migrate: %w", err)
	}
	return store, nil
}

func (db *DB) migrate() error {
	if err := dbschema.MigrateMySQL(db.db, dbschema.GroupAuth); err != nil {
		return fmt.Errorf("migrate DDL: %w", err)
	}
	return nil
}

// User represents an authenticated user (via Feishu OAuth or email/password).
type User struct {
	ID           int64
	FeishuUID    string
	OpenID       string
	Name         string
	Email        string
	AvatarURL    string
	Department   string
	IsAdmin      bool
	PasswordHash string
}

// ErrEmailExists is returned by CreateUserWithPassword when the email is taken.
var ErrEmailExists = fmt.Errorf("email already registered")

func (db *DB) UpsertUser(u *User) error {
	first, err := db.firstUserIsAdmin()
	if err != nil {
		return err
	}
	isAdmin := 0
	if first {
		isAdmin = 1
	}
	_, err = db.db.Exec(`
		INSERT INTO `+tblUsers+` (feishu_uid, open_id, name, email, avatar_url, department, is_admin)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  open_id=VALUES(open_id), name=VALUES(name), email=VALUES(email),
		  avatar_url=VALUES(avatar_url), department=VALUES(department)
	`, u.FeishuUID, u.OpenID, u.Name, u.Email, u.AvatarURL, u.Department, isAdmin)
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	if err := db.db.QueryRow(`SELECT id,is_admin FROM `+tblUsers+` WHERE feishu_uid=?`, u.FeishuUID).Scan(&u.ID, &isAdmin); err != nil {
		return fmt.Errorf("read upserted user: %w", err)
	}
	u.IsAdmin = isAdmin == 1
	if u.IsAdmin {
		if err := db.assignAdminRole(u.ID); err != nil {
			return err
		}
	}
	return nil
}

// assignAdminRole grants the admin RBAC role, ignoring duplicates.
func (db *DB) assignAdminRole(userID int64) error {
	if _, err := db.db.Exec(`INSERT IGNORE INTO `+tblUserRoles+` (user_id, role_id) VALUES (?,?)`, userID, adminRoleID); err != nil {
		return fmt.Errorf("assign admin role: %w", err)
	}
	return nil
}

// firstUserIsAdmin reports whether the users table is empty, so the first
// registered account becomes admin.
func (db *DB) firstUserIsAdmin() (bool, error) {
	var hasUsers int
	if err := db.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM ` + tblUsers + ` LIMIT 1)`).Scan(&hasUsers); err != nil {
		return false, fmt.Errorf("check existing users: %w", err)
	}
	return hasUsers == 0, nil
}

// GetUserByEmail returns the user for an email (including password hash), or nil.
func (db *DB) GetUserByEmail(email string) (*User, error) {
	u := &User{}
	row := db.db.QueryRow(`SELECT id,COALESCE(feishu_uid,''),open_id,name,email,avatar_url,department,is_admin,password_hash FROM `+tblUsers+` WHERE email=?`, email)
	var isAdmin int
	err := row.Scan(&u.ID, &u.FeishuUID, &u.OpenID, &u.Name, &u.Email, &u.AvatarURL, &u.Department, &isAdmin, &u.PasswordHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return u, nil
}

// CreateUserWithPassword inserts an email/password user (no feishu_uid). The
// first user in the table becomes admin, mirroring the Feishu path.
func (db *DB) CreateUserWithPassword(email, name, passwordHash string) (*User, error) {
	if existing, err := db.GetUserByEmail(email); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrEmailExists
	}
	isAdmin, err := db.firstUserIsAdmin()
	if err != nil {
		return nil, err
	}
	adminFlag := 0
	if isAdmin {
		adminFlag = 1
	}
	res, err := db.db.Exec(
		`INSERT INTO `+tblUsers+` (name, email, password_hash, is_admin) VALUES (?,?,?,?)`,
		name, email, passwordHash, adminFlag,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	if isAdmin {
		if err := db.assignAdminRole(id); err != nil {
			return nil, err
		}
	}
	return &User{ID: id, Name: name, Email: email, IsAdmin: isAdmin}, nil
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	u := &User{}
	row := db.db.QueryRow(`SELECT id,COALESCE(feishu_uid,''),open_id,name,email,avatar_url,department,is_admin FROM `+tblUsers+` WHERE id=?`, id)
	var isAdmin int
	err := row.Scan(&u.ID, &u.FeishuUID, &u.OpenID, &u.Name, &u.Email, &u.AvatarURL, &u.Department, &isAdmin)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	u.IsAdmin = isAdmin == 1
	return u, err
}

func (db *DB) CreateSession(token string, userID int64, ttl time.Duration) error {
	exp := time.Now().Add(ttl)
	if _, err := db.db.Exec(`DELETE FROM `+tblSessions+` WHERE user_id=?`, userID); err != nil {
		return fmt.Errorf("delete old sessions: %w", err)
	}
	_, err := db.db.Exec(`INSERT INTO `+tblSessions+` (token,user_id,expires_at) VALUES (?,?,?)`, token, userID, exp)
	return err
}

// GetSession returns the user for a valid session token, or nil.
func (db *DB) GetSession(token string) (*User, error) {
	var userID int64
	row := db.db.QueryRow(`SELECT user_id FROM `+tblSessions+` WHERE token=? AND expires_at>NOW()`, token)
	if err := row.Scan(&userID); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return db.GetUserByID(userID)
}

func (db *DB) DeleteSession(token string) error {
	_, err := db.db.Exec(`DELETE FROM `+tblSessions+` WHERE token=?`, token)
	return err
}

// GetSettings returns all settings as a map[key]value.
func (db *DB) GetSettings() (map[string]string, error) {
	if db == nil || db.db == nil {
		return nil, fmt.Errorf("auth: db not available")
	}
	rows, err := db.db.Query(`SELECT k, v FROM ` + tblSettings)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// SetSettings upserts multiple key-value pairs atomically.
func (db *DB) SetSettings(pairs map[string]string) error {
	if len(pairs) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(pairs))
	args := make([]any, 0, len(pairs)*2)
	for k, v := range pairs {
		placeholders = append(placeholders, "(?, ?)")
		args = append(args, k, v)
	}
	_, err := db.db.Exec(
		`INSERT INTO `+tblSettings+` (k, v) VALUES `+strings.Join(placeholders, ",")+` ON DUPLICATE KEY UPDATE v=VALUES(v)`,
		args...,
	)
	return err
}
func (d *DB) RawDB() *sql.DB { return d.db }
