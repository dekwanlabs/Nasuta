package tool

import "context"

type entityScopeKey struct{}

// WithEntityScope binds the current investigation entity's business tokens to
// the context so relation tools can drop out-of-scope facts. An empty token set
// leaves the context unchanged, so non-delegated callers are unaffected.
func WithEntityScope(ctx context.Context, tokens []string) context.Context {
	if len(tokens) == 0 {
		return ctx
	}
	copied := append([]string(nil), tokens...)
	return context.WithValue(ctx, entityScopeKey{}, copied)
}

// EntityScopeFrom returns the entity business tokens bound to the context, or
// nil when no scope was set.
func EntityScopeFrom(ctx context.Context) []string {
	tokens, _ := ctx.Value(entityScopeKey{}).([]string)
	return tokens
}
