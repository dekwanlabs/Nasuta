# AGENTS.md

Guidance for AI coding agents working in this repository (Nasuta — the reusable
backend knowledge core, Go module `github.com/dekwanlabs/nasuta`).

The full, authoritative instructions live in [CLAUDE.md](./CLAUDE.md). Read it
first — the two files are kept as a single source of truth, and everything below
is a pointer, not a second copy.

## Essentials

- **Verify standalone with `GOWORK=off`** so the module is checked on its own,
  not against the `go.work` overlay from the sibling `codeloom` app:
  ```bash
  GOWORK=off go build ./...
  GOWORK=off go test ./...
  GOWORK=off go vet ./...
  GOWORK=off go test -race -count=1 ./...
  ```
- **Respect the public surface**: consumers import only `app`, `config`,
  `knowledge`, `tool` (plus reuse helpers `llm`/`log`/`platform`/`writeaction`).
  Everything under `internal/` carries no compatibility promise. Dependencies
  point inward; don't pull application-specific business policy up into this
  reusable core.
- **No silent fallbacks**: an explicitly configured backend that errors must
  fail loudly. Capability-boundary degradation (disable when credentials are
  absent) is fine; switching providers under the hood is not.

For architecture, conventions, and the full command list, see
[CLAUDE.md](./CLAUDE.md).
