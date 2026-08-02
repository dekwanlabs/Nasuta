package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

// Service is the auth capability entry: OAuth flow, session lookup, and HTTP middleware.
type Service struct {
	feishu      *FeishuOAuth
	db          *DB
	redirectURI string
	webBase     string
}

// NewService creates the auth capability entry.
func NewService(feishu *FeishuOAuth, db *DB, redirectURI, webBase string) *Service {
	return &Service{feishu: feishu, db: db, redirectURI: redirectURI, webBase: webBase}
}

func (service *Service) resolveUser(r *http.Request) *User {
	token := requestSessionToken(r)
	if token == "" {
		return nil
	}
	user, err := service.db.GetSession(token)
	if err != nil {
		log.Errorf("[auth] session lookup error: %v", err)
		return nil
	}
	return user
}

func (service *Service) Login(w http.ResponseWriter, r *http.Request) {
	if !service.feishu.Configured() {
		httputil.WriteServiceUnavailable(w, "Feishu OAuth not configured (FEISHU_APP_ID / FEISHU_APP_SECRET missing)")
		return
	}
	state := GenerateState()
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, service.feishu.AuthURL(service.redirectURI, state), http.StatusFound)
}

func (service *Service) Callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value != state {
		httputil.WriteBadRequest(w, "invalid state")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/", MaxAge: -1})

	if code == "" {
		errMsg := r.URL.Query().Get("error_description")
		if errMsg == "" {
			errMsg = r.URL.Query().Get("error")
		}
		httputil.WriteBadRequest(w, "oauth error: "+errMsg)
		return
	}

	feishuUser, err := service.feishu.ExchangeCode(r.Context(), code)
	if err != nil {
		log.Errorf("[auth] feishu exchange error: %v", err)
		httputil.WriteErr(w, err)
		return
	}

	dbUser := &User{
		FeishuUID:  feishuUser.UnionID,
		OpenID:     feishuUser.OpenID,
		Name:       feishuUser.Name,
		Email:      feishuUser.Email,
		AvatarURL:  feishuUser.AvatarURL,
		Department: feishuUser.Department,
	}
	if err := service.db.UpsertUser(dbUser); err != nil {
		log.Errorf("[auth] upsert user error: %v", err)
		httputil.WriteErr(w, fmt.Errorf("db error"))
		return
	}

	token := GenerateToken()
	if err := service.db.CreateSession(token, dbUser.ID, sessionTTL); err != nil {
		log.Errorf("[auth] create session error: %v", err)
		httputil.WriteErr(w, fmt.Errorf("db error"))
		return
	}

	dest := service.webBase
	if dest == "" {
		dest = "/"
	}
	destination, err := accessTokenRedirect(dest, token)
	if err != nil {
		log.Errorf("[auth] parse web base URL: %v", err)
		httputil.WriteErr(w, fmt.Errorf("invalid web base URL"))
		return
	}
	http.Redirect(w, r, destination, http.StatusFound)
}

func accessTokenRedirect(destination, token string) (string, error) {
	target, err := url.Parse(destination)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("access_token", token)
	values.Set("token_type", "Bearer")
	values.Set("expires_in", strconv.FormatInt(int64(sessionTTL.Seconds()), 10))
	target.Fragment = values.Encode()
	return target.String(), nil
}

func (service *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if token := requestSessionToken(r); token != "" {
		if err := service.db.DeleteSession(token); err != nil {
			log.Errorf("[auth] delete session error: %v", err)
			httputil.WriteErr(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, service.webBase+"/login", http.StatusFound)
}

func requestSessionToken(r *http.Request) string {
	const prefix = "Bearer "
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (service *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := service.resolveUser(r); u != nil {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

func (service *Service) RequireAuth(next http.HandlerFunc) http.Handler {
	return service.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (service *Service) Me(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]any{
		"id":         user.ID,
		"name":       user.Name,
		"email":      user.Email,
		"avatar_url": user.AvatarURL,
		"is_admin":   user.IsAdmin,
	})
}
