# 可观测性、Token 统计与评估

[English](08-observability-and-evaluation.md) | [中文](08-observability-and-evaluation.zh-CN.md)

> 状态：统一设计；Run、Delegation、Shadow 与 Provider Usage 事件合同已实现
> 更新：2026-08-16
> 来源：Agent Observability Design；QA LLM Token 使用量记录设计

## 1. 定位

Agent 可观测性通过彼此独立但可关联的通道回答三个问题：

1. **业务记录**：Run 持久化执行了哪些步骤；
2. **实时体验**：当前连接的客户端应看到什么；
3. **评估与成本**：时间、Token、证据和上下文预算消耗在哪里。

三者共享 Run/Call 标识，但不能合并成一个无界事件日志。Telemetry 只描述执行行为，不能授予工具权限、修改证据计划，也不能成为业务事实来源。

## 2. Run、Step 与 Call 模型

`agent_runs` 保存一次生命周期，包括用户、Session、问题、模式、状态、时间、步骤数和 LLM 聚合用量。`agent_steps` 保存持久化的检索、推理、工具调用、工具结果、控制和回答步骤。`agent_llm_calls` 每次物理 Provider 请求保存一条明细。

重试、续写、强制结论、Memory 提取和 Session 摘要都是新的物理 LLM 调用。共享 LLM Client 不保存可变的“当前 Run”；Run ID、阶段和轻量 Usage Recorder 通过调用 Context 传递，避免并发串账。

大内容遵循模块 04：持久化原文、模型可见内容和展示摘要是不同表示。可观测性记录使用了哪种表示、是否遗漏，但不能以打点为名静默截断证据。

## 3. 实时事件合同

Run Hub 可以广播：

```text
phase       临时预处理或检索状态
reasoning   Provider 可见的推理流（若支持）
content     用户可见回答 Token
step        已持久化的业务步骤
evaluation  按需开启的评估 Trace
terminal    唯一 Run 终态，包含状态或错误
```

`phase` 是瞬时提示，不持久化。慢订阅者在有界缓冲策略下可能丢失瞬时流，但持久化步骤和终态仍可查询。一次 Run 只能产生一个 terminal 结果。用户取消、连接断开、Provider 失败、工具失败和超时必须分类记录。

### 3.1 Delegation、Validation、Verification、Adoption 与 Shadow 事件

Dynamic Delegation 通过现有 `ExecutionEvent`/RunHub 广播统一事件，不伪装成 Workflow
node：

```text
delegation.created
delegation.started
delegation.completed
delegation.failed
delegation.cancelled
delegation.rejected
delegation.validated
delegation.verification_started
delegation.verification_done
delegation.verification_failed
delegation.verification_rejected
delegation.adoption_evaluated
delegation.shadow_evaluated
```

`interrupted` 使用 terminal failure 事件类型传输，但 payload 的 delegation status
保持 `interrupted`。每个 child 的 terminal 事件包含 parent/delegation/child identity、
capability、duration、tool calls、report bytes、completeness、token 和 cost usage。
`delegation.validated` 包含 citation coverage、structured claim coverage、冲突数量、
`requires_verification` 和原因。verification 生命周期事件包含 parent/delegation/child
identity、verification ID、trigger reasons、status、error code、duration 和 usage；
预算 admission 被拒绝使用独立 rejected 事件，不伪装成未启动的 failed Run。

`delegation.adoption_evaluated` 在 public result 完成映射后按 delegation 单独发布，
包含 adopted report IDs、`adopted/not_adopted/unknown` 和 unknown reason。隐藏 adoption
marker 不进入 token、Session 或 answer Step content；同一结构化 adoption facts 还写入
answer Step 的 `delegation_adoptions_json`、Terminal/Outcome 和 public RunResult，使
临时 SSE 丢包时仍可查询。

Shadow 始终异步隔离，不改变 authoritative Workflow/Single 结果。
`delegation.shadow_evaluated` 记录 query kind、duration、usage、reference count、
conflict count 和成功/失败状态；它可以通过 parent Run 与 route event 中的
capability/risk 上下文关联。route 选择和显式 Workflow escalation 还必须记录稳定
reason code，使 Single、Delegate、Workflow 及降级原因可区分。

## 4. Evaluation Trace

评估按请求或环境开启，记录有界的节点输入元数据、输出元数据、状态、耗时和错误类别。典型节点包括：

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

Trace 分开保存 proposed/effective EvidencePlan；Memory Recall 不并入普通检索。模型轮次区分请求开始、Provider 首事件、reasoning、content、tool delta、tool call 和完成时间。端到端 TTFT 包含预处理与检索；轮次 TTFT 从物理 Provider 调用开始。

隐藏推理和敏感 Provider 内部信息不属于公共 Trace 合同。Trace 应保存字段投影、Hash、数量和脱敏片段，而不是无限制保存 Prompt、源码、凭据或完整工具结果。

## 5. Token 统计口径

Provider 返回的 Usage 是权威来源。字节数、字符数、SSE Delta 数量和历史字符计数不得称为 Token。

```text
call.total_tokens = Provider total_tokens
                  或 input_tokens + output_tokens

run.total_tokens = Σ call.total_tokens
run.peak_input_tokens = max(call.input_tokens)
run.peak_reserved_tokens = max(call.input_tokens + call.max_output_tokens)
                           仅统计输出预留已知的调用
```

`reasoning_tokens` 是输出 Token 的细分项，不能重复加到总量。`cached_input_tokens` 是输入用量的细分项，用于成本分析，但不能从上下文占用中扣除。`max_output_tokens = 0` 表示预留未知，不表示零占用。

推荐阶段为 `route`、`agent_step`、`continuation`、`forced_conclusion`、`memory_extract` 和 `session_summary`。阶段只用于费用归因，不是生命周期状态。

## 6. Provider Usage 映射

OpenAI-compatible 非流式读取顶层 Usage；流式读取 Provider 支持的 Usage 事件或最终 Chunk。Anthropic 流式合并起始输入用量和最终输出用量，Cache Creation/Read Input 都计入统一输入占用。

配置的 Gateway 未返回 Usage 时，按 Schema 记录状态和未知/零上报值；不能用字符数估算，也不能改用其他 Provider。模型上下文窗口必须来自明确的模型能力配置，不能从 Usage 反推。

## 7. 持久化与有界读取

物理调用明细包括 Run ID、单调递增 Call Sequence、阶段、Provider、模型、各类 Usage、输出预留、耗时、状态和时间。明细写入与 Run 聚合/峰值更新在同一事务完成，使取消或进程异常前已完成的调用仍可查询。

Run 列表只读聚合列。Run 详情只按 Call Sequence 读取指定 Run 的明细，并使用显式 Limit 或 Cursor。事件、日志、Trace 和工具 Payload 分别定义保留期与访问权限。

## 8. 指标

建议指标：

- Run 成功、取消、超时和强制结论比例；
- Provider/工具耗时、错误率、重试数和首 Token 延迟；
- 输入、输出、推理、缓存、总 Token 与峰值预留；
- 工具结果大小、遗漏数量、归档/重取比例和 AnswerContract 重试；
- EvidencePlan 覆盖率和 Required Evidence 失败率；
- delegation admission/rejection、child status、并发、report completeness、citation/
  structured claim coverage、verification reason/status/usage、adoption status/reason
  和 Workflow escalation reason；
- 按 QueryKind、capability 与风险等级分桶的 Single/Delegate/Workflow latency、
  LLM calls、token、cost、citation 与质量对照；
- 订阅丢包与 terminal 事件交付；
- 后台摘要和 Memory 成本，且与同步回答延迟分开统计。

指标只能描述系统行为。除非通过有权限的证据工具取得，否则指标不能作为客户、代码、配置或运行时事实引用。

Dynamic Delegation 的性能和质量目标属于生产 rollout gate，不是代码完成事实。
P95 延迟下降 20%、平均输入 token 下降 25%、LLM 调用减少，以及 citation/质量不退化，
都必须在真实基线和分桶 shadow 数据上验证后才能声明达标或淘汰旧 Workflow stage。

## 9. 用户控制与审计

Pause 在 Agent 步骤之间消费，不能中断正在进行的 Provider 或工具请求；Resume 释放暂停；Nudge 在下一个安全边界注入经过身份校验的指导；Abort 取消 Run Context，并记录操作者和原因。

状态转换和终态落库必须并发安全。审计保存 actor、request/run ID、动作、前后状态、时间和失败分类，但不保存密钥或无限制的模型内部内容。

## 10. 验收标准

1. 每次物理 LLM 调用都能关联到唯一 Run 和阶段，且并发不串账；
2. Provider Usage 不被字符估算替代；
3. Run 聚合等于 Call 明细的求和和峰值；
4. 端到端 TTFT 与 Provider 轮次 TTFT 分开统计；
5. 瞬时流丢失不影响持久化步骤和终态；
6. Trace 与指标不改变证据、权限和最终回答事实；
7. 工具结果截断、遗漏、归档和合同重试均可观察；
8. 在线列表和详情在存储边界执行有界读取。
9. delegation lifecycle、validator、verifier、adoption、shadow、route 和 escalation reason 可由 SSE/Trace 或持久 Step/terminal 还原；
10. adoption marker 不进入可见流和 Session，失败/取消/invalid-output 不沿用候选 adopted 结论；
11. 性能与质量结论使用 QueryKind/capability/risk 分桶的生产基线，不以全局平均或设计目标代替。

## 详细归并材料

### Agent 可观测性设计

> Migrated from CodeLoom `docs/design/agent/agent-observability-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现

Agent 可观测性分离业务 Step、实时流事件和按请求启用的评估 Trace。三条通道回答不同问题，不能合并成一个无界日志。

#### 数据模型

`agent_runs` 保存一次 Run 生命周期：用户、会话、问题、状态、模式、Step/Token 数和开始/结束时间。`agent_steps` 保存已持久化的检索、模型思考、工具调用、工具结果、控制和答案 Step。

完整工具输出可以保留在 Step 记录中；`result_summary` 和返回模型的内容分别限长。Run API 提供列表/详情访问及会话删除清理。

#### 实时事件

`RunHub` 广播：

```text
phase       临时预处理/检索状态
reasoning   provider 流式 reasoning
token       可见答案内容
step        已持久化业务 Step
trace       按请求启用的评估遥测
terminal    带 status/error 的唯一 Run 终态
```

Phase 提示不持久化。Token/Reasoning/Trace 使用有界订阅 Buffer；根据 Hub 超时策略，慢消费者可能延迟或丢失流数据，但已持久化 Step 仍可查询。

#### 评估 Trace

Trace 由每次 QA 请求显式启用，记录节点级输入、输出、状态和耗时。当前节点可能包括：

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

`evidence_plan` 分别记录建议和有效 Plan。`memory_recall` 与普通检索分离。`agent_model_turn` 记录 provider 首事件、Reasoning、Content、工具 Delta 和完整工具调用时间。`first_answer_token` 同时记录单轮和端到端 TTFT。

评估 Trace 为评估/UI 广播，不属于业务 Step。敏感的 provider 原始内部信息不能提升为公开 Trace 契约。

#### 用户控制

Run Control Endpoint 支持：

- `pause`：在下一循环 Step 前暂停并等待；
- `resume`：恢复暂停的 Run；
- `nudge`：继续前注入用户指导；
- `abort`：终止 Run。

控制信号在 Agent Step 之间轮询，因此无法中断正在进行的 provider 或工具 HTTP 请求；Run/工具 Context 仍执行各自 Deadline。Pause 在 Loop 消费信号时从 `running` 条件更新为 `paused`，Resume 从 `paused` 条件恢复为 `running`；terminal completion 允许从两种非终态原子进入 done/failed/aborted。

#### 不变量

1. 可观测性不能修改 `EvidencePlan` 或工具权限。
2. Trace 节点必须对应可评估功能阶段，而不是只服务 UI 的标签。
3. 以 Run 累计口径报告首词时间时，必须包含预处理和检索耗时。
4. 临时流丢失不能删除已持久化 Run Step。
5. 后台摘要和 Memory 工作不计入同步答案延迟。

### QA LLM Token 使用量记录设计

> Migrated from Nasuta `docs/design/qa-llm-token-accounting.zh-CN.md`; incorporated into this module on 2026-07-31.

#### 目标

Nasuta 必须同时回答两个不同问题：

1. 一次用户 QA Run 实际消耗了多少模型 token；
2. Run 内单次模型调用距离模型上下文窗口还有多少余量。

统计以 Provider 返回的 `usage` 为准，不再把字符串字节数、字符数或 SSE delta 数量称为 token。

#### 口径

每次物理 LLM API 调用记录一条明细，包括路由、查询分析、Agent step、续写、强制结论、
长期记忆提取和会话摘要。重试是新的物理调用；Provider 没有返回 usage 时记录状态和零 usage，
不能用字符数伪造。

```text
call.total_tokens = usage.total_tokens
                  或 usage.input_tokens + usage.output_tokens

run.total_tokens = Σ call.total_tokens
run.peak_input_tokens = max(call.input_tokens)
run.peak_reserved_tokens = max(call.input_tokens + call.max_output_tokens)
                           仅统计 max_output_tokens 已知的调用
```

`max_output_tokens = 0` 表示 Provider 请求没有设置可确认的输出上限，此时不参与
`peak_reserved_tokens`；峰值为 0 表示预留量未知，而不是上下文占用为 0。

`reasoning_tokens` 是 `output_tokens` 的细分项，不再重复加到 `total_tokens`。
`cached_input_tokens` 是 `input_tokens` 的细分项，保留用于成本分析，但不从上下文占用中扣除。

#### 调用阶段

`phase` 只标识费用来源，不表示生命周期状态：

| phase | 含义 |
|---|---|
| `route` | 证据路由、查询规范化和术语提取 |
| `agent_step` | Agent 工具决策或直接回答 |
| `continuation` | 长答案续写 |
| `forced_conclusion` | 工具步数耗尽后的最终结论 |
| `memory_extract` | 回答完成后的长期记忆提取 |
| `session_summary` | 保存对话后的滚动摘要 |

#### Provider 映射

##### OpenAI-compatible

非流式响应读取顶层 `usage`。流式请求发送 `stream_options.include_usage=true`，读取最终 SSE
chunk 的 `usage`。兼容网关若拒绝该字段必须返回可观察错误，不能伪造 usage 或切换 Provider。

映射：

```text
prompt_tokens                                  -> input_tokens
prompt_tokens_details.cached_tokens            -> cached_input_tokens
completion_tokens                              -> output_tokens
completion_tokens_details.reasoning_tokens      -> reasoning_tokens
total_tokens                                   -> total_tokens
```

##### Anthropic

非流式响应读取顶层 `usage`。流式响应合并 `message_start.usage` 与 `message_delta.usage`。

```text
input_tokens + cache_creation_input_tokens
  + cache_read_input_tokens      -> input_tokens
cache_creation_input_tokens
  + cache_read_input_tokens      -> cached_input_tokens
output_tokens                    -> output_tokens
```

Anthropic 没有独立返回 thinking token 时，`reasoning_tokens` 保持 0；thinking 已包含在输出计费中。
Anthropic 的 `input_tokens` 不包含 cache creation/read，因此统一口径的输入总量必须把三项相加；
否则缓存命中时会低估上下文占用和总量。

#### 数据模型

`agent_llm_calls` 保存物理调用明细：

```sql
CREATE TABLE agent_llm_calls (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id              VARCHAR(64) NOT NULL,
    call_seq            INT NOT NULL,
    phase               VARCHAR(32) NOT NULL,
    provider            VARCHAR(32) NOT NULL,
    model               VARCHAR(128) NOT NULL,
    input_tokens        INT NOT NULL DEFAULT 0,
    cached_input_tokens INT NOT NULL DEFAULT 0,
    output_tokens       INT NOT NULL DEFAULT 0,
    reasoning_tokens    INT NOT NULL DEFAULT 0,
    total_tokens        INT NOT NULL DEFAULT 0,
    max_output_tokens   INT NOT NULL DEFAULT 0,
    duration_ms         INT NOT NULL DEFAULT 0,
    status              VARCHAR(16) NOT NULL,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_run_call (run_id, call_seq),
    KEY idx_run (run_id)
);
```

`agent_runs` 增加聚合列：

```text
input_tokens
cached_input_tokens
output_tokens
reasoning_tokens
total_tokens
llm_call_count
peak_input_tokens
peak_reserved_tokens
```

每次写入调用明细时，在同一事务中增量更新 Run 聚合和峰值。这样失败、暂停、进程退出前已经
完成的调用仍然可查询。在线列表只读 `agent_runs`，Run 详情按 `run_id, call_seq` 有界读取明细。

已有库通过显式 SQL migration 增加表和字段；新建库同步更新 `dbschema`。旧的
`token_used`、`token_delta`、`reasoning_tokens` 字段保留兼容，但不再作为真实 token 指标，
也不把历史字符数转换成 token。其中步骤级 `token_delta` 继续记录内容字符数，仅用于旧页面兼容；
真实 token 只能读取 `agent_llm_calls` 和 `agent_runs` 的新增字段。

#### 调用关联

QA 在生成 `run_id` 后，把 `run_id`、`phase` 和 usage recorder 放进当前调用上下文。
`internal/llm` 只认识自己的轻量 Recorder 接口，不依赖 Agent 或数据库；RunStore 实现该接口。
共享 LLMClient 不持有可变的当前 Run 状态，因此并发请求不会串账。

后台 `memory_extract` 和 `session_summary` 使用 `context.WithoutCancel` 保留 usage 元数据，
并继续关联触发它们的 `run_id`。

#### API

Run 列表返回聚合字段。Run 详情额外返回按 `call_seq` 排序的 `llm_calls`，不得无界读取其他 Run。
前端可展示：调用次数、输入/输出/总 token、缓存输入、推理细分，以及峰值预留上下文。

模型上下文窗口大小不从 usage 推断；若要显示 `peak_reserved_tokens / context_window`，必须由明确的
模型能力配置提供，未知模型只显示峰值，不猜测窗口。

#### 验证

- OpenAI 非流式和流式 usage 映射测试；
- Anthropic 非流式和流式 usage 合并测试；
- reasoning/cached token 不重复计入 total；
- RunStore 明细与聚合在同一事务内成功或回滚；
- 并发 Run 不串账，失败调用不伪造 token；
- Run 列表使用聚合列，Run 详情只读取指定 Run 的调用明细；
- Nasuta 与 CodeLoom 构建、测试和 `go vet` 通过。
