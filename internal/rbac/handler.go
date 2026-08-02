package rbac

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type Handler struct {
	store *Store
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func GenerateAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "mcp-" + base64.RawURLEncoding.EncodeToString(b)
}

// Users

func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers()
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, users)
}

// Roles

func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.store.ListRoles()
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, roles)
}

func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var role Role
	if err := readJSON(r, &role); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	if err := h.store.CreateRole(&role); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var role Role
	if err := readJSON(r, &role); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	role.ID = id
	if err := h.store.UpdateRole(&role); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) DeleteRole(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := h.store.DeleteRole(id); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// User-Role

func (h *Handler) AssignRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID int64 `json:"user_id"`
		RoleID int64 `json:"role_id"`
	}
	if err := readJSON(r, &body); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	if err := h.store.AssignRole(body.UserID, body.RoleID); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) RevokeRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID int64 `json:"user_id"`
		RoleID int64 `json:"role_id"`
	}
	if err := readJSON(r, &body); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	if err := h.store.RevokeRole(body.UserID, body.RoleID); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) GetUserRoles(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	roles, err := h.store.GetUserRoles(userID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, roles)
}

// Menus

func (h *Handler) ListMyMenus(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromContext(r)
	menus, err := h.store.GetUserMenus(userID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if menus == nil {
		menus = []Menu{}
	}
	writeJSON(w, menus)
}

func userIDFromContext(r *http.Request) int64 {
	u := auth.UserFromContext(r.Context())
	if u != nil {
		return u.ID
	}
	return 0
}

func (h *Handler) ListMenus(w http.ResponseWriter, r *http.Request) {
	menus, err := h.store.ListMenus()
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, menus)
}

func (h *Handler) CreateMenu(w http.ResponseWriter, r *http.Request) {
	var m Menu
	if err := readJSON(r, &m); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	if err := h.store.CreateMenu(&m); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) UpdateMenu(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var m Menu
	if err := readJSON(r, &m); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	m.ID = id
	if err := h.store.UpdateMenu(&m); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) DeleteMenu(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := h.store.DeleteMenu(id); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// Role-Menu

func (h *Handler) GrantMenu(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RoleID  int64   `json:"role_id"`
		MenuIDs []int64 `json:"menu_ids"`
	}
	if err := readJSON(r, &body); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	for _, mid := range body.MenuIDs {
		h.store.GrantMenu(body.RoleID, mid)
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) GetRoleMenus(w http.ResponseWriter, r *http.Request) {
	roleID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	menus, err := h.store.GetRoleMenus(roleID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, menus)
}

// MCP Keys

func (h *Handler) ListMCPKeys(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	keys, err := h.store.ListMCPKeys(userID)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, keys)
}

func (h *Handler) CreateMCPKey(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		KeyName string `json:"key_name"`
	}
	if err := readJSON(r, &body); err != nil {
		httputil.WriteBadRequest(w, "bad request")
		return
	}
	body.KeyName = strings.TrimSpace(body.KeyName)
	if body.KeyName == "" {
		httputil.WriteBadRequest(w, "key name is required")
		return
	}
	key := GenerateAPIKey()
	if err := h.store.CreateMCPKey(userID, body.KeyName, key); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"api_key": key})
}

func (h *Handler) RevokeMCPKey(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	keyID, _ := strconv.ParseInt(r.PathValue("keyID"), 10, 64)
	if err := h.store.RevokeMCPKey(userID, keyID); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
