package config

import "context"

// Ref identifies one external configuration value.
type Ref struct {
	Application string
	Key         string
}

// Value carries a resolved value and its non-secret evidence locator.
type Value struct {
	Value  string
	Source string
}

// Resolver resolves configuration references in one bounded batch.
type Resolver interface {
	ResolveConfig(context.Context, []Ref) (map[Ref]Value, error)
}
