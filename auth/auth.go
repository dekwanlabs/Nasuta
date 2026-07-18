package auth

import internalauth "github.com/dekwanlabs/astris/internal/auth"

// DB is the stable auth persistence boundary exposed to scenario compositions.
type DB = internalauth.DB

// Service is the stable auth HTTP capability exposed to scenario compositions.
type Service = internalauth.Service

// User is the authenticated identity attached to request context.
type User = internalauth.User

// Session is the stable placeholder session type shared at the auth boundary.
type Session = internalauth.Session

// FeishuOAuth is the Feishu SSO adapter exposed at the auth boundary.
type FeishuOAuth = internalauth.FeishuOAuth

// FeishuUser is the normalized Feishu profile returned by OAuth exchange.
type FeishuUser = internalauth.FeishuUser

var (
	NewDB           = internalauth.NewDB
	NewService      = internalauth.NewService
	NewFeishuOAuth  = internalauth.NewFeishuOAuth
	GenerateState   = internalauth.GenerateState
	GenerateToken   = internalauth.GenerateToken
	WithUser        = internalauth.WithUser
	UserFromContext = internalauth.UserFromContext
)
