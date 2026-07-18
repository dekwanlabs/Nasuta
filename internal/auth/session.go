package auth

import (
	"context"
	"time"
)

const (
	sessionCookie   = "cl_session"
	sessionTTL      = 7 * 24 * time.Hour
	stateCookieName = "cl_oauth_state"
)

type contextKey int

const userKey contextKey = 1

// Session is a placeholder capability-edge type for auth session state.
type Session struct{}

// WithUser injects a User into the context.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// UserFromContext retrieves the User from a context, or nil.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(userKey).(*User)
	return u
}
