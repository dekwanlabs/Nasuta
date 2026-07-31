# LLM Providers and Reliability

[English](06-llm-providers-and-reliability.md) | [中文](06-llm-providers-and-reliability.zh-CN.md)

> Status: OpenAI/Anthropic dispatch established; error classification and protocol repair continue to improve
> Sources: LLM Provider Design, LLM Retry/JSON Repair/QA Closure Evaluation

## 1. Provider Dispatch

A single explicit dispatcher creates providers:

```text
PlatformSettings.LLMProvider
  -> openai constructor
  -> anthropic constructor
  -> unsupported provider error
```

Never switch providers after configuration failure, hide missing credentials as an empty answer, or scatter provider switches through business code. Provider differences remain behind the shared `llm` contract.

## 2. Common Contract

Common capabilities include regular and streaming chat, tool schemas/calls, `tool_call_id` pairing, max output tokens, usage, finish reason, cancellation, timeout, and stable error classification.

Anthropic may group consecutive tool results into one user turn while OpenAI uses tool-role messages. Adapters own this protocol difference; the agent constructs canonical internal messages.

## 3. Error Classification

Retry only proven transient failures:

| Class | Examples | Default behavior |
|---|---|---|
| Transient | 429, selected 5xx, connection reset | Bounded exponential backoff with jitter |
| Timeout | request/read timeout | Retry only within remaining run budget |
| Invalid request | schema, arguments, context overflow | No blind retry; clear error |
| Authentication | 401/403 | No retry and no provider substitution |
| Content policy | provider block | Surface the restriction |
| Protocol | malformed tool call or stream event | Bounded repair or explicit failure |

Retry logs include provider, attempt, wait, classification, and outcome without secrets or unredacted payloads.

## 4. JSON and Tool-call Repair

Repair from most deterministic to most expensive:

1. strict JSON parse;
2. remove known transport noise such as code fences;
3. validate missing/type-invalid fields against schema;
4. reprompt the same provider for valid arguments only;
5. return a tool-protocol error after the cap.

Do not use aggressive regex to guess arbitrary text into JSON, and never mutate business values during repair.

Before provider send, normalize call/result pairing, create explicit interrupted results when required, preserve user order, and never attach an old result to a new call ID.

## 5. Streaming Reliability

- record first event, reasoning, content, and completion independently;
- retry classified failures only before visible output;
- do not transparently replay a stream after content was delivered;
- stop forwarding candidate answer text once tool-call deltas begin;
- treat `finish_reason=length` as continuation;
- return an explicit reasoning-truncated error after bounded continuation attempts.

## 6. Empty Responses and Final Convergence

An empty `stop` response differs from truncated reasoning. Bounded reprompts consume the same run budget. Intermediate empty responses must not discard tool evidence already acquired before finalization.

## 7. Token Configuration

Three budgets remain separate:

- context window: total model input capacity;
- answer max tokens: visible output and reasoning allowance;
- tool-result fuse: protection against one abnormal tool result.

Every agent turn receives sufficient answer tokens so reasoning models do not consume the entire allowance invisibly. Input budget is checked once before provider invocation.

## 8. Acceptance Criteria

1. Each provider has one dispatch path.
2. Configuration failure never substitutes providers.
3. Only transient failures retry automatically.
4. JSON repair preserves business values.
5. Tool call/results are valid before provider send.
6. Delivered stream content is not duplicated by retry.
7. Empty response, length truncation, and protocol failure have distinct observable errors.

## Detailed Consolidated Material

### LLM Provider Design

> Migrated from CodeLoom `docs/design/agent/agent-llm-providers.md`; incorporated into this module on 2026-07-31.

Status: Current

CodeLoom supports OpenAI-compatible chat completions and Anthropic Messages behind one `LLMClient`. The Agent loop uses common message, tool, streaming, reasoning, finish-reason, retry, and structured-JSON contracts.

#### Dispatch

Non-streaming text, structured JSON, and streaming tool calls each enter one explicit provider switch:

```text
provider=openai    -> OpenAI-compatible implementation
provider=anthropic -> Anthropic implementation
other              -> unsupported-provider error at dispatch
```

Provider selection comes from the MySQL platform setting `llm_provider` and is passed through `NewLLMClientWithHTTPAndProvider`. Runtime URL, key, model, and token settings are also platform settings. Indexing document generation is a separate OpenAI-format client and does not support Anthropic.

#### Common Contract

The Agent sees:

- `Message` with system/user/assistant/tool roles;
- `ToolDef` using JSON-schema parameters;
- `ToolCall` with a JSON argument string;
- streamed content, reasoning, tool deltas, and completed tool calls;
- normalized finish reasons `stop`, `length`, and `tool_calls`.

OpenAI-compatible providers use chat-completion messages and SSE deltas. Anthropic translation moves system content to the top-level system field, maps assistant calls to `tool_use`, maps tool results to user `tool_result` blocks, and converts Anthropic event types into the common stream callbacks.

#### Reliability

Shared call wrappers classify network, status, envelope, empty-output, invalid-JSON, and truncation failures. Retry and JSON-repair behavior stays above the provider-specific HTTP protocol so both providers follow the same caller contract.

A reasoning model may consume its token budget without visible content. That condition surfaces as `ErrReasoningTruncated`; it is not a successful empty answer. The Agent can continue length-truncated visible answers within its remaining loop budget.

#### Configuration Gap

The constructor currently canonicalizes every provider value other than `anthropic` to `openai`. This means an invalid configured value can be hidden before the explicit dispatcher sees it. The target invariant is ingress validation of exactly `openai|anthropic`, followed by no downstream alias or default. Until that is fixed in code, platform configuration must restrict the value.

#### Invariants

1. Provider-specific wire formats do not leak into the Agent loop.
2. A configured Anthropic provider is never served through the OpenAI path.
3. Provider errors retain their class and cause.
4. Document generation's OpenAI-only limitation remains explicit.
5. Adding a provider requires one implementation per operation and an explicit dispatcher case.

### LLM 重试、JSON 修复与 QA 链路闭环评估

> Migrated from CodeLoom `docs/design/agent/llm-retry-json-repair-evaluation.md`; incorporated into this module on 2026-07-31.

> 评估日期：2026-07-15
> 评估范围：commit `4048121` 及本次 provider、fallback、解析边界闭环修复

#### 变更概要

本次变更加强了 QA 主链 LLM 调用的鲁棒性，分为三个层面：

| 层 | 变更 | 涉及文件 |
|---|------|---------|
| LLM 基础设施 | 结构化错误分类、指数退避重试、JSON 程序化修复、reprompt 循环 | `internal/llm/call.go`, `errors.go`, `json.go`, `jsoncall.go`, `textcall.go`, `stream_retry.go` |
| 检索管线 | Router / Preprocess / QueryTerms 迁移至 `ChatJSON` / `ChatText`，消除手动 JSON 提取 | `internal/retrieval/route.go`, `preprocess.go`, `queryterms.go` |
| Agent 决策 | Router 故障与模型低置信度决策统一采用可观测的 Internal fallback | `internal/agent/service.go`, `internal/domain/retrieval_plan.go` |

同时消除了 `incident/analyze.go` 和 `memory/extract.go` 中的手动 JSON 提取代码，统一到 `ChatJSON`。

---

#### 1. 错误分类与重试

##### 设计

`CallError` 将 LLM 调用失败分为四类，retry 策略明确：

```
ErrKindNetwork     → 可重试（连接断开、读超时、EOF）
ErrKindStatus 429  → 可重试（服务端限流，优先使用 Retry-After）
ErrKindStatus 5xx  → 可重试（服务端临时故障）
ErrKindStatus 4xx  → 不可重试（401/400 重试无意义）
ErrKindEmpty       → 不可重试（模型返回了结构正确但无 choices 的结果）
ErrKindEnvelope    → 不可重试（响应体无法解码，重试大概率同样失败）
```

##### 关键安全约束

- **死 Context 不重试**：`context.Canceled` / `context.DeadlineExceeded` 即使包裹在 `ErrKindNetwork` 中也判定为不可重试。原因是：父 context 已死，重试必然再次失败，浪费一次 API 调用。
- **Retry-After 优先于指数退避**：当服务端返回 `Retry-After` 头（秒数或 HTTP-date），优先使用该延迟，上限 8s。避免在限流场景下雪上加霜。
- **指数退避**：500ms → 1s → 2s → 4s → 8s（上限）。默认最多 3 次尝试。

##### 调用链

```
ChatJSON / ChatText
  → chatMessages (provider dispatcher)
    ├─ openai → POST /chat/completions
    └─ anthropic → POST /v1/messages
    → 非 200 → CallError{Kind: ErrKindStatus}
    → 解析失败 → CallError{Kind: ErrKindEnvelope}
    → 空 choices → CallError{Kind: ErrKindEmpty}
  → retryableCallError(err)
    → errors.As(err, &CallError) && Retryable()
  → sleepFor(ctx, ce, backoff)
    → Retry-After > 0 → 用 Retry-After
    → 否则 → 指数退避
```

错误响应正文最多保留 64 KiB；streaming 错误体通过 `io.LimitReader` 读取，避免异常响应
造成无界内存和日志膨胀。可重试错误在最后一次失败时同时保留 `ErrMaxAttempts` 和原始
`CallError`，调用方既能 `errors.Is` 判断耗尽，也能 `errors.As` 检查状态码。

##### 判断：✅ QA 主链逻辑闭环

QA 主链使用的 `Chat` / `ChatMax` / `ChatJSON` / `ChatText` 和工具流 setup 都有明确的
有限重试策略，不死循环、不死等。未使用的旧 `StreamChat` 不在本次保证范围内。

---

#### 2. JSON 修复

##### 修复策略

`RepairJSON` 采用单遍扫描（O(n)），全程追踪 `inStr` / `escape` / `inLine` / `inBlock` 状态，确保结构性编辑只在字符串外部进行。

| 缺陷类型 | 修复方式 | 原理 |
|---------|---------|------|
| 尾随逗号 `{"a":1,}` | 扫描时删除 | `,` 后紧跟 `}` 或 `]` 即删除 |
| `//` 行注释 | 扫描时跳过 | 检测 `//` 后跳过直到 `\n` |
| `/* */` 块注释 | 扫描时跳过 | 检测 `/*` 后跳过直到 `*/` |
| 未引号 key `{a: "v"}` | 扫描时加引号 | `{` 或 `,` 后紧跟标识符且后跟 `:` |
| 字符串内裸换行符 | 转义为 `\n` | 仅当 `inStr=true` 时 |
| 字符串内裸控制字符 | 转义为 `\u00xx` | 仅当 `inStr=true` 且 `< 0x20` |
| 截断的 bracket `{"a":[1,2` | 栈补齐 | 扫描结束后从栈中弹出所有未闭合 bracket |
| 截断的字符串 `{"a":"val` | 补 `"` | 扫描结束时 `inStr=true` → 补 `"` |

##### 故意不修复（fall through 到 reprompt）

| 缺陷 | 原因 |
|------|------|
| 单引号字符串 `{'a': 'v'}` | 无法区分 `don't`（撇号）和字符串分隔符 |
| bareword 值 `{a: ok}` | `ok` 可能是 `true` / `false` / `null` 或枚举值，语义不确定 |
| 拼接对象 `{"a":1}{"b":2}` | 无法判定哪个是正确输出 |
| 字符串内的单引号 | 可能是字符串内容，不做假设 |

##### 字符串与结构边界

```
输入: {"url": "http://x", "note": "a,}"}
   → 字符串内的 // 不会被当注释删除
   → 字符串内的 ,} 不会被当尾随逗号删除
输出: {"url": "http://x", "note": "a,}"}  ✅ 不变
```

外层 JSON 提取同样追踪字符串和嵌套 bracket，不会把字符串中的 `}` / `]` 当作结构结束。
块注释删除时保留一个空格，避免 `1/* comment */2` 被错误拼接成合法但语义变化的 `12`。
修复仍是 best-effort，最终必须经过 `json.Unmarshal` 和业务 `Validate`，不能把修复结果
本身视为可信数据。

##### 判断：✅ 合理分层

修复覆盖当前测试语料中的尾随逗号、注释、未引号 key、控制字符和截断场景；不可安全
修复的缺陷不强行猜测，交给 reprompt。未基于生产语料统计覆盖率，不使用“90%+”结论。

---

#### 3. Reprompt 循环

##### 流程

```
chatJSONWith:
  msgs = [system, user]
  repairs = 0
  for attempt in 1..MaxAttempts:
    raw = call(msgs)
    if transport error:
      if retryable: backoff → continue
      else: return error

    if parseRepairValidate(raw, out):
      return nil  // ✅ 成功

    if repairs < maxRepair:
      msgs += [assistant: raw, user: "Your response was invalid: {problem}. Return ONLY valid JSON."]
      repairs++
      continue     // 🔄 reprompt

    return ErrInvalidJSON  // ❌ 耗尽
```

##### 关键细节

1. **传输重试与 reprompt 共享 `MaxAttempts` 预算**。默认 `MaxAttempts=3`，如果 transport 失败 1 次，则只剩 2 次机会给 reprompt。
2. **reprompt 消息包含模型自己的原始输出**（作为 assistant turn），让模型看到自己上次错在哪里。
3. **`parseRepairValidate` 每次解析都使用 fresh zero value**。原文解析失败后，修复文本会
   再创建一个 fresh value，避免第一次部分反序列化污染第二次结果；typed nil pointer
   会返回校验失败而不是 panic。
4. **`Validate` callback 在 `rv.Elem().Set` 之前执行**，即 parsed 值先校验，通过后才 copy 回 `out`。

##### 测试覆盖

- 正常 JSON → 1 次调用，直接返回
- 尾随逗号 → 1 次调用，RepairJSON 修复，无 reprompt
- 不可修复的垃圾 → reprompt 1 次后成功
- Validate 拒绝 → reprompt
- 耗尽 → `ErrInvalidJSON`
- Transport 重试 → 退避后成功
- 死 Context → 不重试

##### 判断：✅ 逻辑正确

---

#### 4. Stream 重试

##### 设计

`postStream` 只重试 HTTP 连接建立阶段（setup），不重试流传输中。正确原因：SSE 一旦开始推送 token，重试会导致重复输出 delta。

##### 注意点（非本次引入）

如果流中途断连，`scanner.Err()` 返回给上层，最终用户看到截断的答案。`loop.go` 中的 `continueIfNeeded` 机制只处理 `FinishReason == "length"`（正常截断），不处理网络断连。

**建议**：后续如生产指标证明有必要，可设计带已输出前缀去重的一次性恢复；在没有可靠
去重协议前，不直接重试整个 stream。

---

#### 5. Router 故障降级

##### 当前策略

| 场景 | DecisionFrom | Fallback 函数 | Plan | 效果 |
|------|-------------|--------------|------|------|
| Router 不可达（`client == nil`） | `DecisionFallback` | `InternalFallbackRoutingDecision` | `Internal` | 保守启用内部证据 |
| HTTP/网络错误 | `DecisionFallback` | `InternalFallbackRoutingDecision` | `Internal` | 同上 |
| 输出不可解析（reprompt 耗尽） | `DecisionFallback` | `InternalFallbackRoutingDecision` | `Internal` | 同上 |
| 模型判定 Direct 且置信度 < 阈值 | `DecisionModel` | `InternalFallbackRoutingDecision` | `Internal` | 保守启用内部检索 |

##### 关键 guard

```go
if decision.DecisionFrom == coretypes.DecisionModel &&  // 只覆盖模型决策
    decision.Plan.Direct() && decision.Confidence < svc.routerConfidence {
    effectiveDecision = coretypes.InternalFallbackRoutingDecision(...)
}
```

`DecisionFrom == DecisionModel` 确保 rule（如 `shouldShortCircuitMeta`）和 fallback 决策不被二次猜测。
Router 故障使用 Internal 与自适应路由主设计一致：未知证据需求时不允许 fail-open Direct。

Binder 还校验 reason/source 语义不变量：workspace/live/external/multi-source 原因必须包含
对应来源，self-contained/general-knowledge 必须是 Direct，显式来源不能是 Direct。
ambiguous 必须选择至少一个来源，不能用低置信度掩盖无来源 Direct。

##### 测试覆盖

- Router 输出缺少 `route` 字段 → reprompt 1 次后失败 → Internal fallback
- 模型判定 Direct 且高置信度 → 直接 Direct，不启用检索
- Router nil → Internal fallback
- reason/source 矛盾 → Validate 拒绝并进入 reprompt/fallback

##### 判断：✅ 与主设计一致

---

#### 6. 代码一致性

删除了所有手动 JSON 提取代码，统一到 `llm` 包：

| 位置 | 删除的代码 | 替换 |
|------|-----------|------|
| `incident/analyze.go` | `extractJSONObject` (14 行) | `ChatJSON` |
| `memory/extract.go` | `extractJSONArray` (7 行) | `ChatJSON` |
| `retrieval/preprocess.go` | `stripFences` + `parseJSONLoose` (20 行) | `ChatJSON` |
| `retrieval/route.go` | `parseJSONLoose` + 手动 bind (15 行) | `ChatJSON` + `Validate` callback |
| `retrieval/queryterms.go` | `stripFences` 手动调用 | `ChatText` + `llm.StripFences` |

`ChatJSON`、`ChatText` 和 `ChatMax` 共用 `chatMessages` provider dispatcher。OpenAI 和
Anthropic 分别进入自己的 wire adapter；Anthropic 的 JSON reprompt 会保留 assistant/user
多轮消息，不再误发到 OpenAI `/chat/completions` 路径。

---

#### 完整链路追踪

以下是一次 QA 请求的完整调用链，标出了本次变更触及的每个节点：

```
AskAgent(question, history)
  │
  ├─ [并行] AnalyzeWithRoute(fastLLM)          ← ChatJSON + Validate (本次)
  │   ├─ call → retry (CallError 分类)         ← NEW
  │   ├─ parse → RepairJSON → validate         ← NEW
  │   └─ fail → InternalFallbackRoutingDecision ← 保守 fallback
  │
  ├─ [并行] RewriteStandaloneQuery(fastLLM)    ← ChatText (本次)
  │   └─ StripFences                           ← 共享 llm.StripFences
  │
  ├─ decision 覆写逻辑                          ← 本次 (guard 强化)
  │   ├─ DecisionModel + Direct + 低置信度 → Internal
  │   └─ DecisionFallback → 保持 Internal
  │
  ├─ RetrievePlan (如非 Direct)
  │
  ├─ runAgentWithPlan
  │   ├─ RunWithPlan(plan)
  │   │   ├─ buildAgentMessages (工具策略)
  │   │   └─ [循环] ChatWithTools (streaming)
  │   │       └─ postStream ← 本次 (setup 重试, CallError)
  │   │
  │   └─ ExtractMemories                    ← ChatJSON (本次)
  │
  └─ 返回 AskResult{TokenCh, ErrCh}
```

---

#### 总结判断

| 维度 | 评估 |
|------|------|
| **错误处理闭环** | ✅ QA 主链非流式调用和工具流 setup 有限重试 + 分类；错误体有界 |
| **Provider 一致性** | ✅ Chat/ChatJSON/ChatText 显式分发 OpenAI 与 Anthropic |
| **JSON 鲁棒性** | ✅ 覆盖已测试缺陷，不可安全修复的 fallback 到 reprompt |
| **Retry 安全性** | ✅ 死 Context 不重试，streaming 不重复 token，Retry-After 优先 |
| **Router 故障降级** | ✅ 故障 → Internal，避免未知证据需求 fail-open Direct |
| **低置信度保护** | ✅ 仅覆盖模型决策，保守启用内部检索 |
| **代码一致性** | ✅ 消除了所有手动 JSON 提取，统一到 llm 包 |
| **字符串安全性** | ✅ 字符串感知扫描；修复结果仍必须严格解析和 Validate |
| **Stale 数据防护** | ✅ 原文解析和修复解析分别使用 fresh zero value |
| **已知限制** | ⚠️ 流中途断连不自动恢复，避免重复输出 token |
| **测试覆盖** | ✅ errors / json / jsoncall / textcall / stream / anthropic / route / service |

**结论：满足灰度验证条件。** 非流式 provider 分发、有限重试、JSON 修复与校验、Router
保守降级已经形成闭环；流中途断连恢复仍需以生产指标决定是否建设，不属于本次范围。
