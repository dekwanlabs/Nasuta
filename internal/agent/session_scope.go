package agent

import "context"

type sessionToolScope struct {
	SessionID string
	UserID    int64
}

type sessionToolScopeKey struct{}

func withSessionToolScope(ctx context.Context, conversation ConversationContext, userID int64) context.Context {
	if conversation.SessionID == "" || conversation.CompactedThroughTurn <= 0 {
		return ctx
	}
	return context.WithValue(ctx, sessionToolScopeKey{}, sessionToolScope{
		SessionID: conversation.SessionID, UserID: userID,
	})
}

func sessionScopeFromContext(ctx context.Context) (sessionToolScope, bool) {
	scope, ok := ctx.Value(sessionToolScopeKey{}).(sessionToolScope)
	return scope, ok
}
