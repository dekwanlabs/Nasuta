package auth

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	tblUsers     = "users"
	tblSessions  = "sessions"
	tblSettings  = "settings"
	tblUserRoles = "rbac_user_roles"
	adminRole    = "admin"
	defaultRole  = "user"
)

// DB owns MySQL-backed auth, session, history, and settings data.
type DB struct {
	db *sql.DB
}

// NewDB binds authentication queries to the platform-owned MySQL pool.
func NewDB(db *sql.DB) *DB {
	if db == nil {
		return nil
	}
	return &DB{db: db}
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

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (db *DB) UpsertUser(u *User) error {
	tx, err := db.db.Begin()
	if err != nil {
		return fmt.Errorf("begin user upsert: %w", err)
	}
	defer tx.Rollback()

	first, err := firstUserIsAdmin(tx)
	if err != nil {
		return err
	}
	isAdmin := 0
	if first {
		isAdmin = 1
	}
	_, err = tx.Exec(`
		INSERT INTO `+tblUsers+` (feishu_uid, open_id, name, email, avatar_url, department, is_admin)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  open_id=VALUES(open_id), name=VALUES(name), email=VALUES(email),
		  avatar_url=VALUES(avatar_url), department=VALUES(department)
	`, u.FeishuUID, u.OpenID, u.Name, u.Email, u.AvatarURL, u.Department, isAdmin)
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	if err := tx.QueryRow(`SELECT id,is_admin FROM `+tblUsers+` WHERE feishu_uid=?`, u.FeishuUID).Scan(&u.ID, &isAdmin); err != nil {
		return fmt.Errorf("read upserted user: %w", err)
	}
	u.IsAdmin = isAdmin == 1
	role := defaultRole
	if u.IsAdmin {
		role = adminRole
		if err := assignRole(tx, u.ID, role); err != nil {
			return err
		}
	} else if err := assignRoleIfNone(tx, u.ID, role); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user upsert: %w", err)
	}
	return nil
}

func assignRoleIfNone(tx *sql.Tx, userID int64, roleName string) error {
	var hasRoles int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM `+tblUserRoles+` WHERE user_id=? LIMIT 1)`, userID).Scan(&hasRoles); err != nil {
		return fmt.Errorf("check user roles: %w", err)
	}
	if hasRoles == 1 {
		return nil
	}
	return assignRole(tx, userID, roleName)
}

func assignRole(tx *sql.Tx, userID int64, roleName string) error {
	var roleID int64
	if err := tx.QueryRow(`SELECT id FROM rbac_roles WHERE name=?`, roleName).Scan(&roleID); err != nil {
		return fmt.Errorf("find role %q: %w", roleName, err)
	}
	if _, err := tx.Exec(`INSERT IGNORE INTO `+tblUserRoles+` (user_id, role_id) VALUES (?,?)`, userID, roleID); err != nil {
		return fmt.Errorf("assign role %q: %w", roleName, err)
	}
	return nil
}

func firstUserIsAdmin(tx *sql.Tx) (bool, error) {
	var hasUsers int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM ` + tblUsers + ` LIMIT 1)`).Scan(&hasUsers); err != nil {
		return false, fmt.Errorf("check existing users: %w", err)
	}
	return hasUsers == 0, nil
}

// GetUserByEmail returns the user for an email (including password hash), or nil.
func (db *DB) GetUserByEmail(email string) (*User, error) {
	return getUserByEmail(db.db, email)
}

func getUserByEmail(db rowQuerier, email string) (*User, error) {
	u := &User{}
	row := db.QueryRow(`SELECT id,COALESCE(feishu_uid,''),open_id,name,email,avatar_url,department,is_admin,password_hash FROM `+tblUsers+` WHERE email=?`, email)
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
	tx, err := db.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin user registration: %w", err)
	}
	defer tx.Rollback()

	if existing, err := getUserByEmail(tx, email); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrEmailExists
	}
	isAdmin, err := firstUserIsAdmin(tx)
	if err != nil {
		return nil, err
	}
	adminFlag := 0
	if isAdmin {
		adminFlag = 1
	}
	res, err := tx.Exec(
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
	role := defaultRole
	if isAdmin {
		role = adminRole
	}
	if err := assignRole(tx, id, role); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit user registration: %w", err)
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
