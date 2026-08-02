package auth

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
	"golang.org/x/crypto/bcrypt"
)

const minPasswordLen = 6

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

type registerRequest struct {
	Email           string `json:"email"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirm_password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register creates an email/password account and returns its bearer session.
func (service *Service) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteBadRequest(w, "invalid request body")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !emailRe.MatchString(email) {
		httputil.WriteBadRequest(w, "invalid email")
		return
	}
	if len(req.Password) < minPasswordLen {
		httputil.WriteBadRequest(w, "password too short (min 6 chars)")
		return
	}
	if req.Password != req.ConfirmPassword {
		httputil.WriteBadRequest(w, "passwords do not match")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Errorf("[auth] bcrypt hash error: %v", err)
		httputil.WriteErr(w, err)
		return
	}

	// Derive a display name from the email local part.
	name := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		name = email[:i]
	}

	user, err := service.db.CreateUserWithPassword(email, name, string(hash))
	if err == ErrEmailExists {
		httputil.WriteErrStatus(w, http.StatusConflict, ErrEmailExists)
		return
	}
	if err != nil {
		log.Errorf("[auth] create user error: %v", err)
		httputil.WriteErr(w, err)
		return
	}

	service.issueSession(w, user)
}

// LoginWithPassword authenticates an account and returns its bearer session.
func (service *Service) LoginWithPassword(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteBadRequest(w, "invalid request body")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))

	user, err := service.db.GetUserByEmail(email)
	if err != nil {
		log.Errorf("[auth] lookup user error: %v", err)
		httputil.WriteErr(w, err)
		return
	}
	// Same message for missing user and wrong password to avoid account enumeration.
	if user == nil || user.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		httputil.WriteUnauthorized(w, "invalid email or password")
		return
	}

	service.issueSession(w, user)
}

type sessionResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int64        `json:"expires_in"`
	User        userResponse `json:"user"`
}

type userResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
	IsAdmin   bool   `json:"is_admin"`
}

// issueSession returns the token required by protected API requests.
func (service *Service) issueSession(w http.ResponseWriter, user *User) {
	token := GenerateToken()
	if err := service.db.CreateSession(token, user.ID, sessionTTL); err != nil {
		log.Errorf("[auth] create session error: %v", err)
		httputil.WriteErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessionResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(sessionTTL.Seconds()),
		User: userResponse{
			ID:        user.ID,
			Name:      user.Name,
			Email:     user.Email,
			AvatarURL: user.AvatarURL,
			IsAdmin:   user.IsAdmin,
		},
	})
}
