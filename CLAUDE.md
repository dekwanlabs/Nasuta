# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Nasuta — the reusable backend knowledge core, published as the standalone Go module `github.com/dekwanlabs/nasuta`. It indexes a workspace of services (structure + docs) plus all languages (code/config/SQL/docs) semantically and exposes the result to AI agents over **MCP Streamable HTTP** and to a web UI over a **REST dashboard API**. Hybrid retrieval combines a semantic vector store with an in-process BM25 sparse layer; SQLite atomically holds structured records and the ontology snapshot used for dependency walks.

This module is consumed by the sibling `codeloom` application (via `go.work` + a local `replace` during development, a tagged version after release). Nasuta owns **reusable capability and platform composition**; it must not pull application-specific business policy upward. Keep that direction in mind — code that only makes sense for one downstream app does not belong here.

## Commands

```bash
# Standalone verification — always use GOWORK=off so the module is validated
# on its own, not against the go.work overlay.
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet ./...

# single package / single test
GOWORK=off go test ./internal/retrieval/
GOWORK=off go test -run TestParseAndDedupe ./platform/config/

# race detector
GOWORK=off go test -race -count=1 ./...

# run standalone (default :8201)
GOWORK=off go run ./cmd/nasuta

# full local stack (bundles Qdrant, connects Nasuta to qdrant:6334)
cp .env.example .env
docker compose up --build
```

After the server starts, MCP is at `http://localhost:8201/mcp` and the dashboard API at `http://localhost:8201/api/`. It scans the host `./workspace` by default (`NASUTA_WORKSPACE_PATH`).

### Configuration ownership

Standalone runtime uses the `NASUTA_*` env vars (see `.env.example`). The semantic store is configured through one Provider group (`SEMANTIC_PROVIDER`/`SEMANTIC_ENDPOINT`/`SEMANTIC_COLLECTION`/…); legacy `QDRANT_*` vars are normalized to the Qdrant provider at the config entrypoint. Embedding and MySQL disable themselves as a capability boundary when credentials are absent — but an **explicitly configured** backend that errors must fail loudly, never silently switch to another provider.

## Architecture

### Public surface vs internal

The module publishes a small stable surface; everything else is `internal/` and carries no compatibility promise.

```
app/          outward API assembly + standard distribution
cmd/nasuta/   standalone entrypoint
knowledge/    outward query contract
tool/         outward tool-extension contract
config/       outward config contract
incident/     outward incident workflow (analyze / fix / notify)
log/          thin slog facade
platform/     config / httpclient / httputil helpers reused across layers
internal/     implementation — not a compatibility promise
```

`internal/` groups the implementation: `agent` (QA loop + tool surface) with
`agent/catalog` and `agent/workflow`; `feature` with `feature/delivery`,
`feature/pipeline`, and `feature/reviewworkflow`; `retrieval`; `indexing`
(`indexer`, `docgen`); `callchain`; `memory`; `approval`; `auth`; `rbac`;
`domain`; `llm`; `ontology`; `semantic`; `websearch`; `writeaction`; `platform`
(`store`/`semanticstore`/`embed`/`ontologystore`/`dbschema`/`htmlconv`); and
`transport` (`mcp`/`dashboard`/`routes`/`incidenthttp`/`webhook`).

Downstream consumers import only the outward packages above. Business implementation, retrieval, indexing, and transport orchestration all stay collected under `internal/`. Authentication (`internal/auth`) is internal platform assembly: upper layers receive an already-scoped `APIRegistrar` via `app.Extension` and never touch an auth handle.

### Two external interfaces over the same index

1. **MCP** (`/mcp`, Streamable HTTP) — for agent clients, backed by the `tool` registry (built via `internal/agent`). Exposes nine built-in **read** tools: `get_service`, `trace_deps`, `list_apis`, `search_code`, `get_symbol`, `trace_calls`, `search_runbooks`, `check_docs`, and `index_stats`. `query_relations` is added when ontology is available; `web_search` is added when configured. Write actions never enter the upper-layer registrar or MCP; the platform-owned internal catalog exposes them only to authorized runs. Protected by bearer token when `NASUTA_AUTH_TOKEN` is set.
2. **REST dashboard** (`/api/*`) — for the web UI, including a conversational QA endpoint that drives the agent loop with SSE streaming.

Both share one indexing/retrieval state, structured store, and ontology provider, constructed by `app` and composed once at the entrypoint. Dependency tools and QA context traverse ontology `depends_on` facts; there is no second in-memory dependency graph.

### Hybrid retrieval + BM25 handoff (concurrency-sensitive)

`search_code` blends dense vectors with BM25 sparse. The BM25 corpus is rebuilt under `internal/indexing` and handed to the tools surface via `atomic.Pointer` so a background rebuild (writer) can't race a live search (reader). Reads must not retain the pointer across calls. The vocab is persisted atomically (temp file + rename) so an interrupted bootstrap never leaves a half-written file.

### QA agent loop

`internal/agent` runs reason→act(tool)→observe. The whole-run timeout is split: the tool-calling loop runs under `Timeout - AnswerReserve` so a slow loop can never starve the final answer. Every turn uses `AnswerMaxTokens` because reasoning models spend max tokens on invisible thinking before visible content. `continueIfNeeded` recovers length-truncated answers; `ErrReasoningTruncated` surfaces the unrecoverable case rather than returning empty.

## Conventions

- **Boundary discipline first**: dependencies point inward (`app`/`cmd` → `internal` → `platform`/`domain`, never the reverse). One package, one concept — no junk drawers. Invert cycles rather than hiding imports. `platform/*` must not pull business policy upward; `transport/*` stays thin and assembles services rather than owning business logic. When refactoring: one concern per commit, keep `GOWORK=off go build && go vet && go test -race` green, slice big moves.
- **Graceful degradation as a capability boundary**: stores/clients that fail to open are logged and skipped (`log.Warnf`), not fatal — check `X != nil && X.Enabled()` before using. This is *not* a license to silently fall back to a different backend (see next rule).
- **Clean dispatchers, no silent fallbacks**: every multi-backend feature (semantic providers, LLM providers, search engines, storage) uses an explicit dispatcher — one function per backend, one switch to dispatch. A failed prerequisite (missing key, unreachable host) MUST return a clear error, never quietly use another backend. If the user configured Qdrant, don't reach for Milvus under the hood.
- **Logging**: `log.Infof`/`Warnf`/`Errorf`/`Fatalf` (`log`, thin `slog` facade). String helpers in `platform` (`Normalize`/`TruncateForLog`/`CollapseSlashes`), deterministic UUID via `platform.UUIDFromString`.
- **LLM provider**: supported providers are `"openai"` (default) and `"anthropic"`. Wire new LLM clients via `NewLLMClientWithHTTPAndProvider` (in `llm`), not the bare `NewLLMClient`. Note: `internal/indexing/docgen` has its own OpenAI-format client and does not support Anthropic — it warns and uses the OpenAI format when `LLMProvider == "anthropic"`.
- **Errors wrap, don't hide**: `fmt.Errorf("... %q: %w", x, err)` so callers can `errors.Is`/`errors.As`.
- **Simplicity must be justified**: keep code concise, direct, and easy to read. Do not add speculative fallbacks, legacy compatibility paths, defensive branches, or abstractions without a concrete supported requirement. Before introducing a state machine, mode, enum, type assertion/switch, or polymorphic wrapper, identify the distinct lifecycle or behavior it represents and why ordinary control flow or existing types are insufficient. Remove the mechanism if it only renames conditions, hides coupling, or handles states that cannot occur.
- **Comments — short, why not what**: doc comments on exported symbols are required by Go convention but must be concise — explain the *why* (rationale, edge cases, non-obvious constraints), never restate the signature. One or two lines for most functions. Inline comments only when the code is genuinely surprising. No box-drawing banners, no Chinese/English mixing, no commented-out code.
- **No overfit fixes — improve the mechanism, not the case**: when a retrieval/ranking/QA case gives a wrong answer, treat it as one observation of a general weakness, never a target to hardcode against. No keyword/entity-specific rules, no bespoke branches keyed to one question's tokens. A fix is valid only if it improves general behavior (recall, scoring, dedup, disambiguation) and is justified independently of the case that surfaced it. If you can't state the fix without naming the case's tokens, it's overfit — rethink.
- **Bound reads at the storage boundary**: when a caller needs only the latest/top N, one page, or metadata, the query must enforce that with `LIMIT`, cursor/offset pagination, and a narrow `SELECT`. Never load an unbounded result then slice in memory. Prefer stable keyset cursors (e.g. `seq < before_seq`) for append-heavy data. A workflow that genuinely needs the full dataset uses an explicitly named full-read method and documents why.
- **Time complexity — always the least-complexity approach**: every data flow (fetch, persist, loop, search, dedup, transform) uses the lowest practical time complexity. O(1) membership via `map[K]struct{}` sets (never `slices.Contains` in a loop); stream don't buffer; single-pass aggregation; map-keyed dedup; in-place slice reuse for disposable input; early termination before sorts/loops; bound reads in storage; batch external calls into one `WHERE IN (…)`; cheap filter before regex. Accepted exception: `slices.Contains` on provably tiny inputs (k ≤ 10) — verify the bound is real.
