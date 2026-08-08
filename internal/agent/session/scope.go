package session

import "context"

// ToolScope identifies the current compressed session for private history tools.
type ToolScope struct {
	SessionID string
	UserID    int64
}

type toolScopeKey struct{}

// WithToolScope attaches a current-session scope only when archived history exists.
func WithToolScope(ctx context.Context, sessionID string, compactedThroughTurn int, userID int64) context.Context {
	if sessionID == "" || compactedThroughTurn <= 0 {
		return ctx
	}
	return context.WithValue(ctx, toolScopeKey{}, ToolScope{
		SessionID: sessionID, UserID: userID,
	})
}

// ScopeFromContext returns the current compressed-session tool scope.
func ScopeFromContext(ctx context.Context) (ToolScope, bool) {
	scope, ok := ctx.Value(toolScopeKey{}).(ToolScope)
	return scope, ok
}
