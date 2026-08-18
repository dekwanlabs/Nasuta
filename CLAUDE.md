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

1. **MCP** (`/mcp`, Streamable HTTP) — for agent clients, backed by the `tool` registry (built via `internal/agent`). Exposes eight built-in **read** tools: `get_service`, `trace_deps`, `list_apis`, `search_code`, `get_symbol`, `trace_calls`, `search_runbooks`, and `check_docs`. `trace_relations` is added when ontology is available; `web_search` is added when configured. Index health counts stay a dashboard-only concern (`GET /api/tool/index_stats`) and are not offered as agent evidence. Write actions never enter the upper-layer registrar or MCP; the platform-owned internal catalog exposes them only to authorized runs. Protected by bearer token when `NASUTA_AUTH_TOKEN` is set.
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


## Change discipline

- **Read the contract before editing**: first inspect the affected implementation, tests, public API or MCP contract, configuration, and persistence schema. Search the repository for existing terminology and call sites before introducing a new name or behavior. Prefer the existing structure; do not introduce a framework, abstraction, or compatibility path for a local need.
- **Keep ownership and access chains shallow**: inject the exact dependencies a runtime object needs instead of retaining a generic dependency container. Treat four-or-more selector chains as a design review trigger; bind repeated branches to descriptive locals. Do not add pass-through getters that merely hide coupling.
- **Canonicalize once at an untrusted boundary**: trimming, case normalization, default resolution, and validation belong at the API, CLI, file, message, or other external ingress that establishes the domain invariant. Downstream services, stores, planners, and providers must trust canonical domain values. Repair existing non-canonical data with an explicit migration or repair job, not permanent read-time fallback logic.
- **One owner for each rule**: HTTP/MCP adapters bind and validate transport input; service/workflow code owns authorization, cross-field rules, idempotency, and state transitions; stores own persistence, locking, and storage error mapping. Do not repeat the same validation or business rule in every layer.
- **State machines must earn their complexity**: before adding a persisted state, mode, or transition graph, document the real lifecycle, allowed and forbidden transitions, invariants, and the persistence, recovery, timeout, or concurrency requirement that makes direct derivation insufficient. Derive availability or progress from existing facts when possible.
- **No speculative compatibility**: remove deprecated fields and paths according to the current contract. Keep a compatibility branch only for a confirmed caller and a stated migration window, with tests and a cleanup condition.

## Naming, errors, and observability

- **Use domain-precise names**: packages are short, lowercase, and single-purpose; avoid `common`, `utils`, `helpers`, and `base`. Use `NewX`, `ParseX`, `EncodeX`, `DecodeX`, and `ValidateX` for conventional operations. Prefer `XxxInput`, `XxxOutput`, `XxxFilter`, `XxxOptions`, and `XxxConfig`; avoid meaningless `DTO`/`VO`/`BO`. Keep acronyms as `ID`, `URL`, `HTTP`, `API`, `SQL`, `JSON`, and `MCP` in Go identifiers. Boolean names should be positive and directly readable. Test names must describe the behavior and scenario.
- **Errors preserve causes**: wrap errors with `%w` and add operation and domain context; use typed or sentinel errors where callers need classification. Transport layers map safe, stable error models and must not expose raw SQL, provider responses, credentials, or internal stack details.
- **Trace the whole request**: generate trace IDs at the service boundary rather than trusting client-supplied IDs. Propagate the context through workflows, tools, stores, and error logs. Structured logs should include stable fields such as operation, outcome, duration, and trace ID, while avoiding secrets, tokens, prompts containing sensitive data, private keys, and full source or binary payloads; log file or artifact metadata instead.
- **Make failure visible**: optional capabilities may be disabled when not configured and must be logged as such. A configured backend failure must remain an error; never substitute an unconfigured provider or return empty data as if the operation succeeded.

## Persistence and verification

- **Treat schema and migrations as one change**: changes to MySQL/SQLite schema, indexes, constraints, or stored contracts require an explicit migration under `docs/sql/` plus updates to the canonical schema definition, migration tests, and affected store queries. Never edit generated or derived artifacts by hand when a generator or source definition owns them.
- **Preserve storage semantics**: keep transaction boundaries, row locks, uniqueness, idempotency, and snapshot publication atomic. Queries for recent/top-N or metadata views must select only required columns and enforce limits/cursors at the storage boundary; full reads require an explicitly named method and a documented reason.
- **Test the behavior, not just the branch**: behavior changes should cover success, invalid input, authorization, missing resources, downstream/storage failures, retries/timeouts, duplicate requests, and relevant concurrency. Public contract changes require contract tests and documentation updates. Use `GOWORK=off` for standalone verification and run the narrowest relevant tests first, then build, vet, and race tests as the blast radius warrants. Do not start live, paid, destructive, or credential-dependent services/tests without explicit opt-in.
- **Keep the patch focused**: do not mix unrelated refactors with a feature or bug fix. Before finishing, run `gofmt` on changed Go files, `git diff --check`, and inspect `git status`; do not overwrite unrelated working-tree changes.

## Design proposal template

When creating or substantially rewriting a document under `docs/design/proposals/`, use the canonical Chinese template below. The sections 背景、问题、问题出现的场景、如何修改、修改伪代码、预期的效果 are mandatory. Optional sections may be removed only when they do not apply. Describe a general mechanism-level fix rather than a special case for the incident that exposed it. Scenarios must be reproducible, changes must identify ownership and failure behavior, and expected effects must be verifiable.

````markdown
# [提案名称]

状态：草案 / 评审中 / 已接受 / 已实施 / 已废弃
作者：[姓名或团队]
日期：YYYY-MM-DD
关联事项：[Issue / Trace / PR / 文档链接]
目标版本：[可选]

> 使用说明：将方括号中的占位内容替换为实际信息；不适用的可选章节可以删除。提案应优先描述可复现的问题、机制层根因和可验证的改动，避免只针对单个案例打补丁。

## 1. 摘要

<!-- 用 1～3 段回答：发生了什么、为什么需要修改、准备怎样修改、最终能获得什么效果。 -->

本提案用于解决[系统、模块或流程]中的[核心问题]。

当前，当[触发条件]发生时，系统会[当前错误行为或不足]，导致[用户影响、系统影响或工程影响]。其根因不是[表面现象]，而是[机制层根因]。

本提案计划通过[核心修改方式]，将当前流程从：

```text
[当前流程步骤 1]
→ [当前流程步骤 2]
→ [失败点或不可靠点]
→ [当前结果]
```

调整为：

```text
[目标流程步骤 1]
→ [目标流程步骤 2]
→ [新增或修改的保障机制]
→ [目标结果]
```

预期实现[最重要的效果 1]、[效果 2]和[效果 3]。

## 2. 背景

### 2.1 业务与技术背景

<!-- 说明相关业务目标、用户需求、系统职责和上下游关系。读者不查看代码也应能理解为什么存在这条流程。 -->

[描述该系统或功能解决的业务问题。]

当前相关链路为：

```text
[入口]
→ [模块 A]
→ [模块 B]
→ [外部依赖或存储]
→ [最终输出]
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `[模块 A]` | [职责] | [输入] | [输出] |
| `[模块 B]` | [职责] | [输入] | [输出] |
| `[模块 C]` | [职责] | [输入] | [输出] |

### 2.2 当前实现

<!-- 给出关键代码入口、配置、数据结构和运行时行为，不需要粘贴大段源码。 -->

相关实现主要位于：

- `[path/to/file_a.go]`：负责[职责]；
- `[path/to/file_b.go]`：负责[职责]；
- `[path/to/config.yaml]`：定义[配置或约束]；
- `[相关设计文档]`：规定[既有契约]。

当前执行逻辑概括如下：

1. [步骤 1]；
2. [步骤 2]；
3. [步骤 3]；
4. [步骤 4]。

### 2.3 为什么现在需要修改

<!-- 说明触发本提案的直接原因，如线上事故、用户反馈、规模增长、架构演进或历史约束失效。 -->

本次修改由[线上案例 / 测试失败 / 架构演进 / 性能瓶颈]触发：

- 触发时间：[YYYY-MM-DD HH:mm:ss，时区]；
- 触发标识：`[trace_id / request_id / issue_id]`；
- 直接表现：[表现]；
- 影响范围：[用户、请求比例、模块或数据范围]；
- 临时处置：[如有]。

### 2.4 范围与非目标

#### 目标

1. [目标 1，必须可验证]；
2. [目标 2]；
3. [目标 3]。

#### 非目标

1. 本提案不解决[相邻但独立的问题]；
2. 本提案不改变[必须保持稳定的契约或行为]；
3. 本提案不通过[扩容、提高上限、硬编码特例等掩盖根因的方式]解决问题。

## 3. 问题

### 3.1 问题描述

<!-- 用“期望行为—实际行为—差异”定义问题，避免只写“有 bug”或“体验不好”。 -->

**期望行为：**

[描述系统在给定输入和约束下应该做什么。]

**实际行为：**

[描述系统当前实际做了什么。]

**差异：**

[说明实际行为与既有契约、用户预期或系统目标之间的差距。]

### 3.2 根因分析

<!-- 区分表面故障、直接原因和机制层根因。若存在多个问题，可复制本节并按 3.2、3.3……编号。 -->

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | [用户或监控看到的现象] | [日志、指标、截图或复现结果] |
| 直接原因 | [直接触发错误的条件] | [代码路径或运行记录] |
| 机制根因 | [设计契约、状态模型、边界处理或职责划分上的缺陷] | [相关实现或设计文档] |

根因链路：

```text
[输入或触发条件]
→ [机制缺陷 1]
→ [机制缺陷 2]
→ [错误状态未被拦截或恢复]
→ [最终影响]
```

本问题不能只通过[追加关键词 / 增加重试 / 提高 token 或超时时间 / 针对单个 ID 写特例]解决，因为[说明为什么这些方式只缓解症状而不修复根因]。

### 3.3 影响

- **用户影响：** [错误结果、延迟、不可用、信息不完整等]；
- **业务影响：** [成功率、转化率、人工成本或风险]；
- **系统影响：** [资源浪费、状态不一致、数据丢失或扩展性问题]；
- **工程影响：** [难以测试、难以观测、职责混乱或维护成本增加]。

## 4. 问题出现的场景

### 4.1 典型场景

<!-- 推荐使用 Given / When / Then，使场景可直接转化为测试。 -->

#### 场景 A：[场景名称]

- **Given（前置条件）：** [系统状态、配置、输入数据和依赖状态]；
- **When（触发行为）：** [用户或系统执行的操作]；
- **Then（期望结果）：** [正确行为]；
- **But（当前结果）：** [当前错误或不足]。

示例输入：

```text
[请求、事件、配置或用户问题]
```

当前执行路径：

```text
[入口]
→ [分支判断]
→ [错误路径]
→ [错误结果]
```

关键证据：

```text
[只保留必要的日志、错误码、状态或指标；注意脱敏]
```

#### 场景 B：[场景名称]

- **Given：** [前置条件]；
- **When：** [触发行为]；
- **Then：** [期望结果]；
- **But：** [当前结果]。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | [条件] | [行为] | [行为] |
| 空输入或缺失字段 | [条件] | [行为] | [行为] |
| 超时或预算耗尽 | [条件] | [行为] | [行为] |
| 下游失败 | [条件] | [行为] | [行为] |
| 重试或重复请求 | [条件] | [行为] | [行为] |
| 并发或乱序 | [条件] | [行为] | [行为] |
| 兼容旧数据或旧客户端 | [条件] | [行为] | [行为] |

### 4.3 复现步骤

1. 准备[配置、测试数据或环境]；
2. 执行[命令、请求或操作]；
3. 观察[日志、响应、数据库或指标]；
4. 可见[错误结果]；
5. 重复[次数或特定条件]后，问题出现概率为[比例或必现]。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** [说明如何避免只修复触发案例。]
2. **保持单一事实源。** [说明哪个模型、字段或组件拥有最终解释权。]
3. **明确职责边界。** [说明入口、执行器、存储、校验器等分别负责什么。]
4. **失败可诊断。** [说明错误码、状态、日志和指标如何反映真实结果。]
5. **兼容与可回滚。** [说明如何灰度、兼容旧行为和快速回退。]

### 5.2 目标流程

```text
[输入]
→ [入口校验或规范化]
→ [核心处理]
→ [新增的边界保护、状态校验或恢复机制]
→ [持久化或下游调用]
→ [结果验证]
→ [输出与可观测性记录]
```

与当前流程相比，关键变化是：

1. 在[位置]新增[机制]，用于[目的]；
2. 将[职责]从[旧模块]移动到[新模块或已有所有者]；
3. 将[隐式约定]改为[显式数据结构、状态或接口契约]；
4. 在[失败边界]增加[降级、续写、重试、补偿或终止策略]；
5. 在[日志 / 指标 / 持久化记录]中公开[状态或完成度]。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| [改动 1] | [当前行为] | [目标行为] | `[模块或文件]` | [兼容方式] |
| [改动 2] | [当前行为] | [目标行为] | `[模块或文件]` | [兼容方式] |
| [改动 3] | [当前行为] | [目标行为] | `[模块或文件]` | [兼容方式] |

#### 改动一：[名称]

**方案：**

[说明具体修改内容、执行顺序和职责归属。]

**约束：**

- [不能破坏的约束 1]；
- [安全、权限或资源约束]；
- [幂等性、一致性或顺序约束]。

**失败行为：**

- 当[条件]发生时，系统返回或记录[明确状态]；
- 不允许[静默吞错、伪成功或产生不可信数据]；
- 如可降级，则降级为[降级结果]，并明确标记[状态字段]。

#### 改动二：[名称]

**方案：**

[说明具体修改内容。]

**约束与失败行为：**

[说明边界条件。]

### 5.4 数据结构或接口契约

<!-- 若不涉及数据结构或接口，可删除本节。 -->

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `[field_a]` | `[type]` | `[module]` | [含义] | [默认值] | [策略] |
| `[field_b]` | `[type]` | `[module]` | [含义] | [默认值] | [策略] |

状态转换：

```text
[pending]
  ├─ 成功 → [succeeded]
  ├─ 可降级 → [partial]
  └─ 不可恢复 → [failed]
```

不变量：

1. [任何状态下必须成立的条件]；
2. [字段之间必须保持的关系]；
3. [禁止出现的状态组合]。

### 5.5 兼容、迁移与回滚

- **向后兼容：** [旧调用方、旧数据和旧配置如何继续工作]；
- **数据迁移：** [是否需要回填，如何分批执行和验证]；
- **灰度方式：** [feature flag、比例、租户或环境]；
- **回滚条件：** [哪些指标或故障触发回滚]；
- **回滚步骤：** [关闭开关、恢复旧版本、回滚数据等]。

## 6. 修改伪代码

<!-- 伪代码应表达职责边界、关键分支、失败行为和状态变化，不要求与最终语言完全一致。 -->

### 6.1 核心流程

```go
func Handle(ctx Context, input Input) (Result, error) {
    normalized, err := NormalizeAndValidate(input)
    if err != nil {
        RecordFailure(ctx, "invalid_input", err)
        return Result{Status: Failed}, err
    }

    plan := BuildPlan(normalized)
    state := NewState(plan)

    for state.CanContinue() {
        step, ok := state.NextStep()
        if !ok {
            break
        }

        output, err := ExecuteStep(ctx, step)
        if err != nil {
            action := ClassifyFailure(err)

            switch action {
            case Retry:
                if state.RetryBudgetAvailable(step) {
                    state.RecordRetry(step, err)
                    continue
                }
                state.RecordFailure(step, err)

            case Degrade:
                state.RecordPartial(step, err)

            case Abort:
                state.RecordFailure(step, err)
                return BuildResult(state), err
            }
        } else {
            state.RecordSuccess(step, output)
        }
    }

    result := BuildResult(state)
    result.Completeness = EvaluateCompleteness(state)

    PersistOutcome(ctx, state, result)
    EmitMetrics(ctx, state, result)

    return result, nil
}
```

### 6.2 关键边界处理

```go
func NormalizeAndValidate(input Input) (NormalizedInput, error) {
    normalized := DeterministicNormalize(input)

    if !SchemaValid(normalized) {
        return NormalizedInput{}, ErrInvalidSchema
    }

    if ViolatesInvariant(normalized) {
        return NormalizedInput{}, ErrInvariantViolation
    }

    return normalized, nil
}
```

### 6.3 修改前后对比

修改前：

```go
// [当前逻辑的问题：例如将两个独立预算混用、忽略错误或直接假定成功]
if steps >= maxSteps {
    return success
}
```

修改后：

```go
if reasoningSteps >= budget.MaxReasoningSteps {
    return partial("reasoning_budget_exhausted")
}

if toolCalls >= budget.MaxToolCalls {
    return partial("tool_budget_exhausted")
}

return VerifyAndFinalize(evidence, requiredGoals)
```

### 6.4 配置或数据库变更

```yaml
feature:
  enabled: false
  rollout_percent: 0
  max_retries: 1
  timeout: 30s
```

```sql
-- 如无数据库变更，删除此代码块。
ALTER TABLE example
    ADD COLUMN completeness VARCHAR(32) NOT NULL DEFAULT 'unknown';
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当[正常条件]成立时，系统能够[目标行为]；
2. 当[异常条件]发生时，系统能够[明确失败、受控降级或恢复]；
3. 不再出现[当前关键错误行为]；
4. 对[多入口、多实现或边界情况]能够[正确区分和处理]。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `[metric_name]` | Counter / Gauge / Histogram | [衡量什么] |
| `[structured_log_field]` | 结构化日志字段 | [定位什么] |
| `[status_or_completeness]` | 持久化状态 | [反映真实完成度] |

日志应至少能够回答：

- 请求选择了哪条执行路径；
- 哪一步失败，以及失败原因；
- 是否发生重试、降级或回滚；
- 最终执行状态和结果完整度分别是什么；
- 哪些输出可以追溯到哪些输入或证据。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| [成功率或正确率] | [值] | [值] | [周期] | [监控或评测] |
| [P95/P99 延迟] | [值] | [值] | [周期] | [监控] |
| [错误率] | [值] | [值] | [周期] | [日志或监控] |
| [资源消耗] | [值] | [值] | [周期] | [成本或运行指标] |
| [完整度或覆盖率] | [值] | [值] | [周期] | [评测或持久化记录] |

### 7.4 不应发生的变化

- [既有正常路径]的行为保持不变；
- [延迟、成本或资源]不得超过[上限]；
- 不降低[安全性、一致性、准确性或可解释性]；
- 不引入针对[触发案例中的具体名称、ID 或关键词]的硬编码。

## 8. 测试与验收

### 8.1 单元测试

- [正常输入]返回[结果]；
- [非法输入]被入口校验拒绝；
- [预算耗尽]返回明确的[状态]；
- [下游失败]触发[重试、降级或终止]；
- [不变量]在所有状态转换中保持成立。

### 8.2 集成测试

- 验证从[入口]到[最终输出]的完整链路；
- 验证新旧调用方兼容；
- 验证日志、指标和持久化状态一致；
- 验证并发、重复请求、乱序和超时行为。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | [输入] | [目标结果] | [自动化测试或人工检查] |
| 正常路径 | [输入] | [保持既有行为] | [测试] |
| 边界路径 | [输入] | [受控行为] | [测试] |
| 故障注入 | [输入或故障] | [明确失败或降级] | [测试] |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. [功能验收条件]；
2. [正确性或完整度条件]；
3. [性能和成本条件]；
4. [可观测性条件]；
5. [兼容性条件]；
6. [原触发案例回归通过]。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| [风险 1] | [条件] | [影响] | [预防与缓解] | [条件] |
| [风险 2] | [条件] | [影响] | [预防与缓解] | [条件] |
| [风险 3] | [条件] | [影响] | [预防与缓解] | [条件] |

## 10. 实施计划

### 阶段 1：[最小安全改动]

- [任务]；
- [任务]；
- 退出条件：[条件]。

### 阶段 2：[核心机制修改]

- [任务]；
- [任务]；
- 退出条件：[条件]。

### 阶段 3：[灰度与观测]

- [任务]；
- [任务]；
- 退出条件：[条件]。

### 阶段 4：[全面启用与清理]

- [删除旧逻辑、迁移数据、更新文档等]；
- 退出条件：[条件]。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| [决策 1] | [说明] | [说明] | [A/B] | [原因] |
| [决策 2] | [说明] | [说明] | [A/B] | [原因] |

## 12. 决策摘要

本提案建议：

1. [核心决策 1]；
2. [核心决策 2]；
3. [核心决策 3]；
4. 通过[测试、指标和灰度策略]验证效果；
5. 当[回滚条件]满足时恢复到[旧行为或安全模式]。

## 附录 A：提案提交前检查清单

- [ ] 背景足以让非原作者理解系统和改动动机；
- [ ] 问题以“期望行为—实际行为—差异”描述；
- [ ] 至少包含一个可复现的典型场景；
- [ ] 已区分表面现象、直接原因和机制根因；
- [ ] 修改方案明确了职责所有者和单一事实源；
- [ ] 伪代码覆盖正常路径、失败路径和状态变化；
- [ ] 预期效果包含可量化指标，而不只是定性描述；
- [ ] 已说明兼容、迁移、灰度和回滚方案；
- [ ] 测试可以覆盖原始触发案例和关键边界场景；
- [ ] 未引入只针对单个案例的硬编码特例。
````
