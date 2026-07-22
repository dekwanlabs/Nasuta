package rbac

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
)

type Store struct {
	db *sql.DB
}

const (
	adminRoleName = "admin"
	userRoleName  = "user"
)

func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := dbschema.MigrateMySQL(db, dbschema.GroupRBAC); err != nil {
		return nil, fmt.Errorf("rbac: migrate: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE rbac_roles ADD COLUMN prompt TEXT`); err != nil {
		if !strings.Contains(err.Error(), "Duplicate column") && !strings.Contains(err.Error(), "1060") {
			return nil, fmt.Errorf("rbac: add prompt column: %w", err)
		}
	}
	if err := s.seed(); err != nil {
		return nil, fmt.Errorf("rbac: seed: %w", err)
	}
	if err := s.repair(); err != nil {
		return nil, fmt.Errorf("rbac: repair: %w", err)
	}
	return s, nil
}

func (s *Store) repair() error {
	if _, err := s.db.Exec(
		`INSERT IGNORE INTO rbac_roles (name, description) VALUES (?, ?), (?, ?)`,
		adminRoleName, "Administrator with full access",
		userRoleName, "Standard user access",
	); err != nil {
		return fmt.Errorf("ensure system roles: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM rbac_menus WHERE path IN ('/rbac/users','/rbac/roles','/rbac/menus','/rbac/keys')`); err != nil {
		return fmt.Errorf("remove legacy RBAC menus: %w", err)
	}
	var rbacCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM rbac_menus WHERE path = '/rbac'`).Scan(&rbacCount); err != nil {
		return fmt.Errorf("check RBAC menu: %w", err)
	}
	if rbacCount == 0 {
		if err := s.CreateMenu(&Menu{ParentID: 0, Name: "权限管理", Path: "/rbac", Icon: "Lock", SortOrder: 8}); err != nil {
			return fmt.Errorf("create RBAC menu: %w", err)
		}
	}

	if _, err := s.db.Exec(`
		INSERT IGNORE INTO rbac_user_roles (user_id, role_id)
		SELECT u.id, r.id FROM users u
		JOIN rbac_roles r ON r.name = ?
		WHERE u.is_admin = 1`, adminRoleName); err != nil {
		return fmt.Errorf("repair administrator roles: %w", err)
	}
	if _, err := s.db.Exec(`
		INSERT IGNORE INTO rbac_user_roles (user_id, role_id)
		SELECT u.id, r.id FROM users u
		JOIN rbac_roles r ON r.name = ?
		LEFT JOIN rbac_user_roles ur ON ur.user_id = u.id
		WHERE u.is_admin = 0 AND ur.user_id IS NULL`, userRoleName); err != nil {
		return fmt.Errorf("repair standard user roles: %w", err)
	}
	if _, err := s.db.Exec(`
		INSERT IGNORE INTO rbac_role_menus (role_id, menu_id)
		SELECT r.id, m.id FROM rbac_roles r
		CROSS JOIN rbac_menus m
		WHERE r.name = ?`, adminRoleName); err != nil {
		return fmt.Errorf("grant administrator menus: %w", err)
	}
	if _, err := s.db.Exec(`
		INSERT IGNORE INTO rbac_role_menus (role_id, menu_id)
		SELECT r.id, m.id FROM rbac_roles r
		CROSS JOIN rbac_menus m
		WHERE r.name = ? AND m.path NOT IN ('/settings', '/rbac')`, userRoleName); err != nil {
		return fmt.Errorf("grant standard user menus: %w", err)
	}
	return nil
}

func (s *Store) seed() error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM rbac_menus`).Scan(&count); err != nil {
		return fmt.Errorf("count menus: %w", err)
	}
	if count > 0 {
		return nil
	}
	menus := []Menu{
		{ParentID: 0, Name: "Dashboard", Path: "/dashboard", Icon: "Monitor", SortOrder: 1},
		{ParentID: 0, Name: "AI Q&A", Path: "/qa", Icon: "ChatLineRound", SortOrder: 4},
		{ParentID: 0, Name: "Agent Runs", Path: "/agent-runs", Icon: "View", SortOrder: 5},
		{ParentID: 0, Name: "Docs", Path: "/docs", Icon: "Document", SortOrder: 6},
		{ParentID: 0, Name: "Settings", Path: "/settings", Icon: "Setting", SortOrder: 7},
		{ParentID: 0, Name: "权限管理", Path: "/rbac", Icon: "Lock", SortOrder: 8},
	}
	for i := range menus {
		if err := s.CreateMenu(&menus[i]); err != nil {
			return fmt.Errorf("create menu %q: %w", menus[i].Path, err)
		}
	}
	return nil
}

type UserInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
	Roles   []Role `json:"roles"`
}

func (s *Store) ListUsers() ([]UserInfo, error) {
	rows, err := s.db.Query(`SELECT id, name, email, is_admin FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	positions := map[int64]int{}
	for rows.Next() {
		var u UserInfo
		var admin int
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &admin); err != nil {
			return nil, err
		}
		u.IsAdmin = admin == 1
		positions[u.ID] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	args := make([]any, len(out))
	for i := range out {
		args[i] = out[i].ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(out)), ",")
	roleRows, err := s.db.Query(`
		SELECT ur.user_id, r.id, r.name, r.description, COALESCE(r.prompt,'')
		FROM rbac_user_roles ur
		JOIN rbac_roles r ON r.id = ur.role_id
		WHERE ur.user_id IN (`+placeholders+`)
		ORDER BY ur.user_id, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer roleRows.Close()
	for roleRows.Next() {
		var userID int64
		var role Role
		if err := roleRows.Scan(&userID, &role.ID, &role.Name, &role.Description, &role.Prompt); err != nil {
			return nil, err
		}
		if pos, ok := positions[userID]; ok {
			out[pos].Roles = append(out[pos].Roles, role)
		}
	}
	return out, roleRows.Err()
}

type Role struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

func (s *Store) CreateRole(r *Role) error {
	_, err := s.db.Exec(`INSERT INTO rbac_roles (name, description, prompt) VALUES (?,?,?)`, r.Name, r.Description, r.Prompt)
	return err
}

func (s *Store) UpdateRole(r *Role) error {
	_, err := s.db.Exec(`UPDATE rbac_roles SET name=?, description=?, prompt=? WHERE id=?`, r.Name, r.Description, r.Prompt, r.ID)
	return err
}

func (s *Store) DeleteRole(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rbac_roles WHERE id=?`, id)
	return err
}

func (s *Store) ListRoles() ([]Role, error) {
	rows, err := s.db.Query(`SELECT id, name, description, COALESCE(prompt,'') FROM rbac_roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRoles(rows)
}

func (s *Store) AssignRole(userID, roleID int64) error {
	_, err := s.db.Exec(`INSERT IGNORE INTO rbac_user_roles (user_id, role_id) VALUES (?,?)`, userID, roleID)
	return err
}

func (s *Store) RevokeRole(userID, roleID int64) error {
	_, err := s.db.Exec(`DELETE FROM rbac_user_roles WHERE user_id=? AND role_id=?`, userID, roleID)
	return err
}

func (s *Store) RolePromptFor(userID int64) string {
	roles, err := s.GetUserRoles(userID)
	if err != nil {
		return ""
	}
	var parts []string
	for _, r := range roles {
		if p := strings.TrimSpace(r.Prompt); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (s *Store) GetUserRoles(userID int64) ([]Role, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.name, r.description, COALESCE(r.prompt,'') FROM rbac_roles r
		JOIN rbac_user_roles ur ON r.id = ur.role_id WHERE ur.user_id=? ORDER BY r.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRoles(rows)
}

func scanRoles(rows *sql.Rows) ([]Role, error) {
	var out []Role
	for rows.Next() {
		var role Role
		if err := rows.Scan(&role.ID, &role.Name, &role.Description, &role.Prompt); err != nil {
			return nil, err
		}
		out = append(out, role)
	}
	return out, rows.Err()
}

// Menu

type Menu struct {
	ID        int64  `json:"id"`
	ParentID  int64  `json:"parent_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Icon      string `json:"icon"`
	SortOrder int    `json:"sort_order"`
}

func (s *Store) CreateMenu(m *Menu) error {
	_, err := s.db.Exec(`INSERT INTO rbac_menus (parent_id, name, path, icon, sort_order) VALUES (?,?,?,?,?)`,
		m.ParentID, m.Name, m.Path, m.Icon, m.SortOrder)
	return err
}

func (s *Store) UpdateMenu(m *Menu) error {
	_, err := s.db.Exec(`UPDATE rbac_menus SET parent_id=?, name=?, path=?, icon=?, sort_order=? WHERE id=?`,
		m.ParentID, m.Name, m.Path, m.Icon, m.SortOrder, m.ID)
	return err
}

func (s *Store) DeleteMenu(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rbac_menus WHERE id=?`, id)
	return err
}

func (s *Store) ListMenus() ([]Menu, error) {
	rows, err := s.db.Query(`SELECT id, parent_id, name, path, icon, sort_order FROM rbac_menus ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMenus(rows)
}

// GetUserMenus returns menus accessible to a user through their assigned roles.
func (s *Store) GetUserMenus(userID int64) ([]Menu, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT m.id, m.parent_id, m.name, m.path, m.icon, m.sort_order
		FROM rbac_menus m
		JOIN rbac_role_menus rm ON m.id = rm.menu_id
		JOIN rbac_user_roles ur ON rm.role_id = ur.role_id
		WHERE ur.user_id = ?
		ORDER BY m.sort_order, m.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMenus(rows)
}

// Role-Menu

func (s *Store) GrantMenu(roleID, menuID int64) error {
	_, err := s.db.Exec(`INSERT IGNORE INTO rbac_role_menus (role_id, menu_id) VALUES (?,?)`, roleID, menuID)
	return err
}

func (s *Store) GetRoleMenus(roleID int64) ([]Menu, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.parent_id, m.name, m.path, m.icon, m.sort_order FROM rbac_menus m
		JOIN rbac_role_menus rm ON m.id = rm.menu_id WHERE rm.role_id=? ORDER BY m.sort_order, m.id`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMenus(rows)
}

func scanMenus(rows *sql.Rows) ([]Menu, error) {
	var out []Menu
	for rows.Next() {
		var menu Menu
		if err := rows.Scan(&menu.ID, &menu.ParentID, &menu.Name, &menu.Path, &menu.Icon, &menu.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, menu)
	}
	return out, rows.Err()
}

// MCP Keys

type MCPKey struct {
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	KeyName   string     `json:"key_name"`
	APIKey    string     `json:"api_key"`
	IsActive  bool       `json:"is_active"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func (s *Store) CreateMCPKey(userID int64, keyName, apiKey string) error {
	_, err := s.db.Exec(`INSERT INTO rbac_mcp_keys (user_id, key_name, api_key) VALUES (?,?,?)`, userID, keyName, apiKey)
	return err
}

func (s *Store) RevokeMCPKey(id int64) error {
	_, err := s.db.Exec(`UPDATE rbac_mcp_keys SET is_active=0 WHERE id=?`, id)
	return err
}

func (s *Store) ListMCPKeys(userID int64) ([]MCPKey, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, key_name, api_key, is_active, created_at, expires_at FROM rbac_mcp_keys WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPKey
	for rows.Next() {
		var k MCPKey
		var active int
		var exp sql.NullTime
		if err := rows.Scan(&k.ID, &k.UserID, &k.KeyName, &k.APIKey, &active, &k.CreatedAt, &exp); err != nil {
			return nil, err
		}
		k.IsActive = active == 1
		if exp.Valid {
			k.ExpiresAt = &exp.Time
		}
		out = append(out, k)
	}
	return out, nil
}
