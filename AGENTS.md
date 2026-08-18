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
- **Design proposals**: documents under `docs/design/proposals/` must use the canonical proposal template in [`CLAUDE.md`](./CLAUDE.md#design-proposal-template), including background, problem, occurrence scenarios, change plan, pseudocode, and expected effects.
- **Simplicity must be justified**: keep code concise, direct, and easy to read. Do not add speculative fallbacks, legacy compatibility paths, defensive branches, or abstractions without a concrete supported requirement. Before introducing a state machine, mode, enum, type assertion/switch, or polymorphic wrapper, identify the distinct lifecycle or behavior it represents and why ordinary control flow or existing types are insufficient. Remove the mechanism if it only renames conditions, hides coupling, or handles states that cannot occur.
- **Read before changing**: inspect the affected implementation, tests, public API/MCP contract, configuration, and persistence schema first. Search existing call sites and terminology; keep the patch focused and avoid local abstractions or speculative compatibility paths.
- **Canonicalize at ingress**: normalize and validate external input once at its boundary; downstream code trusts the resulting domain invariant. Fix old non-canonical data with an explicit migration or repair job, not permanent read-time cleanup.
- **Keep ownership shallow**: inject exact dependencies, avoid generic dependency containers and long selector chains, and do not add pass-through getters that only hide coupling. Each business rule has one owner: adapters validate transport shape, services/workflows own business invariants, and stores own persistence and locking.
- **Make failures and causes observable**: wrap errors with `%w`, preserve classification, propagate server-generated trace IDs through context, and use stable structured logs without secrets or full payloads. An explicitly configured backend must fail visibly rather than silently switching providers or returning empty success.
- **Name precisely and verify proportionally**: use domain terms, standard Go acronym casing, focused package names, and behavior-oriented test names. For behavior changes cover success and failure paths, contract and concurrency boundaries as applicable; run `GOWORK=off` checks, `git diff --check`, and do not start live or credential-dependent services without opt-in.

For architecture, conventions, and the full command list, see
[CLAUDE.md](./CLAUDE.md).
