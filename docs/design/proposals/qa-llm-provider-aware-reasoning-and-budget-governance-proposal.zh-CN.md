# QA LLM Provider 感知的推理控制与父子预算治理设计

> 状态：草案，待评审
> 创建日期：2026-09-05
> 分支：`feat/multi-agent-platform`
> 范围：QA Single-Agent、Delegation Child Agent、Investigation Report、最终答案生成
> 诊断来源：`/Users/dequan.mac/.codex/attachments/39bde7fb-222f-4bd1-a566-dd95a78ad3c1/pasted-text.txt`
> 相关代码：`internal/llm`、`internal/agent/execution`、`internal/agent/delegation`、`internal/agent/run`、`internal/agent/qa`

## 0. 文档结论

本设计修正此前“统一把 `max_tokens` 换成 `max_completion_tokens`”的方向。

**核心结论：**

1. `max_tokens`、`max_completion_tokens`、`max_output_tokens` 是不同 API 契约下的请求字段，不是三个需要同时发送的预算字段；一次请求只选择 provider/API 实际支持的一个字段。
2. 当前 Nasuta 使用 `/chat/completions`，日志中的模型为 `deepseek-v4-flash`。在没有确认网关已经转换 OpenAI 新字段之前，当前路径继续使用 `max_tokens`，不同时发送 `max_completion_tokens`。
3. 真正需要解耦的是：
   - **provider 请求上限**：本次物理模型调用最多生成多少 completion token；
   - **reasoning 控制**：是否思考、思考强度和思考阶段；
   - **可见答案交付预算**：最终答案还剩多少 token、时间和重试机会；
   - **累计账本**：整个父/子 Run 实际消耗的 token、成本和调用次数。
4. 子 Agent 的本次故障主要是 completion 上限被 reasoning 消耗殆尽；父 Agent 的本次故障主要是委托批次占用了父答案阶段的时间窗口。两者需要分别治理。
5. 方案优先通过 provider capability 映射、阶段化 reasoning、父答案硬预留、批次级 deadline 和可观测性解决问题，不通过同时发送两个 token 字段或盲目增大 timeout/token 解决。

---

## 1. 背景与故障事实

### 1.1 当前链路

```text
POST /api/qa/ask
  → QA Service 选择 Single-Agent 或 Parent Dynamic Delegation
  → Parent Agent 执行 route / tool / delegation / answer loop
  → delegate_investigation 并发启动 Investigator Child Agent
  → Child Agent 生成 investigation.report
  → Parent Agent 汇总 evidence/report
  → Parent Agent 生成最终用户答案与 Mermaid flow
```

涉及的主要实现位置：

| 模块 | 主要职责 |
| --- | --- |
| `internal/llm/client.go` | 构造 `/chat/completions` 请求、解析 stream、记录 reasoning 与 usage |
| `internal/llm/model_parameters.go` | 校验并保存模型参数 |
| `internal/agent/execution/model_call.go` | 为一次物理模型调用做 budget reservation、限制 max output、结算 usage |
| `internal/agent/execution/answer_generation.go` | continuation、forced conclusion、空答案恢复 |
| `internal/agent/qa/prepare.go` | 创建父 Run 的 deadline、steps、tool calls、共享 delegation budget |
| `internal/agent/delegation/executor.go` | 计算 Child limits、派发/等待子任务 |
| `internal/agent/run/store_budget.go` | durable budget、reservation、phase reserve |
| `internal/agent/catalog/defaults_investigation.go` | Investigator、Verifier、Synthesizer 定义与角色限制 |

### 1.2 本次运行事实

日志中的关键时间线如下，日期为 **2026-09-05**：

| 时间 | 事件 |
| --- | --- |
| 16:50:22 | Parent Run 启动，`maxSteps=8`，总 timeout 约 `4m53.58s`，时间 reserve `30s` |
| 16:51:20 | 4 个 Child Investigator 并发启动，每个日志 timeout 约 `2m29.98s`，`max_tokens=12000` |
| 16:53:13 / 16:53:22 | Child 出现 `finish_reason=length`、`8981/10825 reasoning tokens`，没有 visible content |
| 16:53:23 | Delegation batch 完成，但多个 report 只能降级为 partial/unavailable |
| 16:54:46 | Parent 日志：`loop budget exhausted at step 4: context deadline exceeded` |
| 16:55:15 | Parent 安装 deterministic flow fallback |
| 16:55:16 | Parent 以 `context deadline exceeded` / `cancelled` 结束 |

直接故障分类：

```text
Child：completion token 池先耗尽，visible answer/report 没有产生
Parent：时间 deadline 先耗尽，最终答案模型调用被取消
```

### 1.3 当前请求参数链路

当前代码的内部参数和 wire 参数关系是：

```text
Definition.Model.MaxOutputTokens
        ↓
Run limits / Agent cfg.AnswerMaxTokens
        ↓
execution.callModel(..., maxTokens)
        ↓
llm.ChatWithToolsMaxWithParameters(..., maxTokens)
        ↓
chatRequest.MaxTokens
        ↓
JSON: "max_tokens"
```

当前 `chatRequest` 只有 `max_tokens` 字段。当前 `ModelParameters` 也没有 reasoning effort 或 thinking 开关。

需要明确：

```text
maxTokens（Nasuta 内部变量）
≠ max_tokens（某个 provider 的 wire 字段）
≠ 可见答案 token 数
```

---

## 2. 设计目标与非目标

### 2.1 目标

1. 让每个 provider/model 使用其明确支持的 completion limit 字段，禁止同一请求同时发送互相竞争的字段。
2. 让模型的 reasoning 行为可按执行阶段控制，而不是只依赖“请不要思考”的提示词。
3. 让 reasoning token、visible token、provider completion token、总成本在 usage 中可区分，同时保证计费和累计预算不被低估。
4. 为最终答案建立独立的 token、时间和重试保护，确保 Child 失败不会自动吞掉 Parent 的交付窗口。
5. 为 Delegation 建立父级、批次级、Child 级三层 deadline，并支持已完成结果的 partial 合成。
6. 让失败日志明确说明是哪个预算维度触发：`time`、`provider_completion_tokens`、`visible_answer_tokens`、`total_tokens`、`cost`、`steps` 或 `tool_calls`。
7. 保持 Single-Agent 和 Multi-Agent 使用同一套最终答案契约，不因是否委托而切换另一套用户输出规则。
8. 在不改变现有持久化兼容性的前提下逐步灰度，先增加观测和 capability routing，再启用新的 reasoning/phase 策略。

### 2.2 非目标

1. 不同时发送 `max_tokens` 与 `max_completion_tokens` 来“提高上限”。
2. 不把 reasoning token 从成本或总 token 账本中剔除；reasoning 仍然是 provider 消耗和账单的一部分。
3. 不承诺通过 prompt 文案完全关闭 provider 的 reasoning。
4. 不默认把所有模型强行切换为同一种请求参数或同一种 reasoning 参数。
5. 不通过无限增加 `AgentTimeout`、`ChildTimeout`、`MaxOutputTokens` 或 retry 次数掩盖任务拆分、证据投影和输出契约问题。
6. 不在本设计阶段修改生产代码；本文件只定义目标架构、迁移顺序、验证标准和待实现接口。
7. 不重新设计 evidence 数据模型；本方案只定义 evidence/report 对 token、时间和输出阶段的预算边界。

---

## 3. 关键术语与预算分层

### 3.1 四类 token

| 名称 | 含义 | 是否计入成本/总账 | 是否等于可见答案 |
| --- | --- | ---: | ---: |
| `InputTokens` | provider 实际接收的输入 token | 是 | 否 |
| `ProviderOutputTokens` | provider 报告的 completion/output token | 是 | 否；推理模型通常包含 reasoning |
| `ReasoningTokens` | provider 报告的 reasoning 明细 | 是 | 否 |
| `VisibleOutputTokens` | 可见 content token，通常可由 `ProviderOutputTokens - ReasoningTokens` 推导，但必须标记为估算值 | 是，已包含在 ProviderOutputTokens 内 | 是 |

注意：`VisibleOutputTokens` 不是所有 provider 都能准确提供。系统必须保留：

```text
reported provider usage
≠ inferred visible output usage
```

不能因为需要保证答案可见，就把 reasoning 从实际成本中删除。

### 3.2 五类预算

#### A. Provider 请求上限

限制一次物理 LLM 请求的 completion 上限。它最终映射为 provider 的一个字段：

```text
DeepSeek Chat Completions  → max_tokens
OpenAI Chat Completions    → max_completion_tokens
OpenAI Responses API        → max_output_tokens
```

这是**请求字段选择问题**，不是多预算相加问题。

#### B. Reasoning 控制预算

控制模型是否思考、思考强度或思考开关，例如：

```text
thinking = enabled / disabled
reasoning_effort = low / medium / high / provider-specific
```

它不是所有 provider 都支持，必须通过 capability 显式声明。

#### C. Visible answer 交付预算

保障最终用户能拿到可见答案的资源，包括：

- 最终答案阶段的可用时间；
- 最终答案阶段的模型调用次数；
- 最终答案阶段的最低输出下限；
- flow/JSON/引用等输出契约的修复机会。

它不能简单等同于 provider 的 `max_tokens`。

#### D. Run 累计账本

整个 Parent Run 及其 Child Run 实际累计消耗的：

- input token；
- provider output/completion token；
- total token；
- cost；
- 物理模型调用次数。

Reasoning 必须继续计入这一层。

#### E. 工作量与时间预算

包括：

- max steps；
- max tool calls；
- Child timeout；
- delegation batch timeout；
- Parent answer deadline。

这些预算不能用 token 预算替代。

---

## 4. 目标架构

### 4.1 Provider Capability Profile

新增一个 provider/model 能力层，禁止执行层根据模型字符串散落判断请求字段。

建议抽象为以下能力：

```text
ModelCapabilityProfile {
    provider
    model_pattern
    api_style                         // chat_completions / responses / messages
    completion_limit_field            // max_tokens / max_completion_tokens / max_output_tokens
    supports_thinking_toggle
    supports_reasoning_effort
    supports_reasoning_usage_detail
    supports_structured_output
    supports_tool_calls
    reasoning_usage_includes_output   // provider usage 语义说明
}
```

能力解析优先级：

```text
显式平台配置
  > provider adapter 默认能力
  > model family 默认能力
  > 安全的普通模型 fallback
```

未知模型不得静默假设支持 `max_completion_tokens` 或 reasoning 参数。未知能力下只发送基础字段，并记录 capability fallback 日志。

### 4.2 请求字段映射

内部统一保留一个逻辑字段：

```text
LogicalCompletionLimit
```

序列化时根据 capability 选择一个 wire 字段：

| capability | 请求字段 | 禁止同时发送 |
| --- | --- | --- |
| `chat_completions + deepseek` | `max_tokens` | `max_completion_tokens` |
| `chat_completions + openai_reasoning` | `max_completion_tokens` | `max_tokens` |
| `responses + openai` | `max_output_tokens` | `max_tokens`、`max_completion_tokens` |
| 未知 | 当前 adapter 默认字段 | 其他字段 |

伪代码：

```text
limitField := capability.CompletionLimitField
switch limitField:
case MaxTokens:
    request.max_tokens = logicalLimit
case MaxCompletionTokens:
    request.max_completion_tokens = logicalLimit
case MaxOutputTokens:
    request.max_output_tokens = logicalLimit
}
```

禁止以下做法：

```json
{
  "max_tokens": 12000,
  "max_completion_tokens": 12000
}
```

原因：不同网关可能拒绝请求、忽略其中一个字段，或者产生不可预测的优先级。

### 4.3 阶段化 Model Mode

同一个 Agent Run 不再所有阶段使用同一套模型参数，而是显式声明调用阶段：

```text
route
investigation
verification
synthesis
final_answer
forced_conclusion
recovery
```

每个阶段得到一个 `ModelMode`：

```text
ModelMode {
    completion_limit
    reasoning_state       // enabled / disabled / provider default
    reasoning_effort      // none / low / medium / high / unset
    tools_allowed
    structured_output_required
    continuation_allowed
    max_physical_calls
}
```

推荐初始策略：

| 阶段 | reasoning | 工具 | 输出要求 | 说明 |
| --- | --- | --- | --- | --- |
| route | disabled/low | 无或少量 | 短结构化结果 | 不让路由消耗大段隐式思考 |
| investigation | enabled + low/medium | 允许 | `investigation.report` | 需要分析证据和调用工具 |
| verification | low | 无 | 验证结构 | 只处理已有证据，不重新探索 |
| synthesis | disabled/low | 无 | 最终答案结构 | 只合成已验证材料 |
| final_answer | disabled/low | 无 | 用户答案/flow | 优先保证可见交付 |
| recovery | disabled | 无 | 最小可交付结果 | 只允许一次短重试 |

注意：阶段策略只是默认值。若 provider 不支持 reasoning 控制，系统必须记录 `unsupported`，不能假装参数已生效。

---

## 5. 父 Agent 与 Child Agent 预算设计

### 5.1 Parent 时间预算

Parent Run 使用三个时间点：

```text
run_deadline       = 整个请求的硬截止
answer_deadline    = Parent 必须开始/完成最终答案的硬线
delegation_deadline = 本批 Child 必须停止等待的硬线
```

推荐关系：

```text
answer_deadline = run_deadline - parent_answer_reserve
batch_deadline  = min(
    now + delegation_batch_timeout,
    answer_deadline - parent_answer_safety,
    context_deadline
)
child_deadline  = min(
    now + child_timeout,
    batch_deadline - child_completion_safety,
    context_deadline
)
```

与当前实现相比，关键变化是：

1. `ChildTimeout` 不能单独决定 batch 最长等待时间；
2. batch 必须在 Parent answer reserve 之前结束；
3. Parent 不能等到 `run_deadline` 才发现答案没有时间生成；
4. `parent_answer_reserve` 应按输出类型配置。需要 Mermaid/结构化修复的 flow 答案不应只预留 30 秒，初始灰度建议评估 `60~90s`，具体值由线上 P95 生成时间确定。

### 5.2 Parent token 预算

Parent 总账继续包括所有物理调用的实际 input/output/cost。

`ParentAnswerReserve` 的语义改为：

```text
非 answer 阶段不可使用的最低 answer-phase token/cost reserve
```

它不是额外免费 token，也不是 visible token 的精确保证。

建议拆成两个字段：

```text
ParentAnswerReserveTokens       // answer phase 的 provider token reserve
ParentAnswerReserveCostMicros   // answer phase 的成本 reserve，可选
```

在非 answer 阶段：

```text
available_for_non_answer = total_limit - used - in_flight - answer_reserve
```

在 answer 阶段：

```text
available_for_answer = total_limit - used - in_flight
```

所有 reasoning token 仍然进入 `used`，但 answer reserve 不应被 Child 的非 answer 请求提前消费。

### 5.3 Child 预算

每个 Child 仍有独立的工作量限制：

```text
max_steps
max_tool_calls
child_timeout
max_input_context_per_request
logical_completion_limit_per_call
```

但 Child 的累计 token/cost 必须通过 Parent 的共享 durable ledger 预留，不再只依赖“给每个 Child 一个固定总额”。

推荐模型：

```text
Child definition ceiling
    = 角色定义允许的最大值

Child task grant
    = 当前 delegation policy 的单任务上限

Shared root ledger
    = Parent/所有 Child 的实际消耗总账

实际可用值
    = min(definition ceiling, task grant, shared ledger remaining)
```

如果多个 Child 并发：

1. dispatch 时只预留可验证的上限；
2. 每次物理模型调用再做 call-level reservation；
3. provider 返回实际 usage 后 settle；
4. 未使用的 reservation 立即释放；
5. batch deadline 到达后，未完成 Child 进入 `timeout` 或 `orphaned`，不继续占用 Parent 等待窗口。

### 5.4 父子输出职责

```text
Child：只输出短、结构化、可引用的 investigation.report
Parent：负责最终自然语言答案、flow、排序、完整度说明
```

Child 不应生成用户最终答案，也不应复制 Parent 的 output contract。这样可以：

- 降低 Child 可见输出需求；
- 减少 reasoning/answer 混用；
- 避免 Child 生成 Mermaid 占用 token；
- 让 Parent 能在 partial report 上确定性交付。

---

## 6. Reasoning 与输出预算处理

### 6.1 不把 `max_completion_tokens` 当作 visible answer 上限

无论使用 `max_tokens` 还是 `max_completion_tokens`，对于支持 reasoning 的模型，都必须把它视为 provider completion 上限，不能直接解释成“可见答案最多 N token”。

因此，以下推断是不成立的：

```text
max_completion_tokens = 12000
⇒ visible answer 一定能得到 12000 token
```

正确模型是：

```text
provider completion cap
    = reasoning tokens + visible output tokens + provider-specific completion parts
```

### 6.2 Usage 结构

LLM 层建议保留原始 provider usage，并在统一 usage 中增加明确字段：

```text
Usage {
    InputTokens
    CachedInputTokens
    ProviderOutputTokens
    VisibleOutputTokens       // 可推导时填入，并带 estimated 标记
    ReasoningTokens
    TotalTokens
    CostMicros
    UsageSource               // provider_reported / inferred / missing
}
```

兼容旧字段的迁移策略：

```text
旧 OutputTokens = ProviderOutputTokens
```

在所有现有 ledger、成本和 limit 判断迁移完成前，不改变旧字段的含义。

### 6.3 可见答案保证策略

系统不能仅靠 token 字段保证 visible answer，必须使用组合策略：

1. final/synthesis 阶段关闭或降低 reasoning；
2. 为 answer phase 保留独立时间和 token/cost reserve；
3. 对 final answer 设定最小可交付内容，而不是追求长答案；
4. 没有 visible content 时，只允许一次低成本 direct-answer retry；
5. retry 前必须检查剩余时间和 answer-phase token；
6. retry 仍失败时，使用 evidence-preserving deterministic fallback；
7. fallback 结果必须标记 `partial`，不得伪装成模型完整结论。

### 6.4 “Do not think”提示词的定位

`force_conclusion_no_think.txt` 可以保留，但它只能作为软提示，不能被当成关闭 reasoning 的机制。

真正的控制顺序应为：

```text
provider capability parameter
    > phase model mode
    > prompt instruction
```

也就是说：

- provider 支持 `thinking=disabled` 时，使用 API 参数；
- provider 只支持 `reasoning_effort` 时，使用 effort；
- provider 不支持任何控制时，才退回 prompt + 更小 completion cap + deterministic fallback。

---

## 7. Delegation 交付协议

### 7.1 非阻塞派发

`delegate_investigation` 的默认行为应是：

```text
Parent 调用 delegate_investigation
    → 快速返回 delegation_id + task statuses
    → Parent 继续短循环或准备 draft
    → Child 完成后持久化 report
    → Parent 在有限窗口内 await/poll settled projection
    → 到 answer_deadline 交付当前最好答案
```

禁止在工具调用内部无限等待所有 Child。

### 7.2 Batch 状态

```text
dispatched
→ running
→ settled
→ partially_settled
→ timed_out
→ cancelled
```

每个 Task 状态：

```text
admitted
→ running
→ completed / partial / failed / timeout / orphaned
```

### 7.3 Partial Answer 契约

Parent 在 batch 未完全结束时必须能够生成：

```json
{
  "status": "partial",
  "confirmed": [],
  "pending": [],
  "evidence_refs": [],
  "limitations": []
}
```

用户可见答案必须明确：

- 哪些结论已有证据支持；
- 哪些 Child/report 已完成；
- 哪些 subject 仍待调查；
- 是否因为时间、token、provider 或证据不足提前交付。

---

## 8. 错误分类与可观测性

### 8.1 统一 Budget Exhaustion Code

建议将当前分散的错误归一为：

```text
budget.time.deadline
budget.provider_completion_tokens
budget.visible_answer_tokens
budget.total_tokens
budget.input_tokens
budget.cost
budget.steps
budget.tool_calls
budget.delegation_batch_timeout
budget.answer_reserve_unavailable
```

示例日志：

```text
[budget] phase=investigation dimension=provider_completion_tokens
requested=12000 effective=8981 reasoning_tokens=8981 visible_tokens=0
provider=deepseek model=deepseek-v4-flash
```

```text
[budget] phase=parent_answer dimension=time
remaining=18s required_floor=60s action=deterministic_partial_answer
```

### 8.2 每次物理模型调用必须记录

```text
run_id
parent_run_id
delegation_id / task_id
phase
provider
model
completion_limit_field
requested_limit
effective_limit
reasoning_mode
reasoning_effort
input_tokens
provider_output_tokens
reasoning_tokens
visible_output_tokens
finish_reason
duration
reservation_id
budget_failure_dimension
```

### 8.3 关键指标

| 指标 | 目的 |
| --- | --- |
| `llm.reasoning_to_output_ratio` | 识别 reasoning 吞掉 completion 的模型/阶段 |
| `llm.empty_visible_after_length_total` | 监控 `finish_reason=length` 但无可见内容 |
| `agent.answer_time_remaining_at_delegation_settle` | 判断 Child 是否侵占 Parent answer 窗口 |
| `delegation.batch_duration` | 监控批次等待时间 |
| `agent.partial_answer_rate` | 评估降级交付频率 |
| `budget.effective_output_shrink_rate` | 监控 durable ledger 对请求上限的压缩 |
| `agent.final_answer_first_token_ms` | 监控最终答案启动是否过晚 |

---

## 9. 配置设计

### 9.1 新增配置语义

不新增一个含义模糊的 `complete_max_token`。建议使用以下明确配置：

```text
llm_completion_limit_mode = auto
llm_reasoning_mode_investigation = enabled
llm_reasoning_effort_investigation = low
llm_reasoning_mode_answer = disabled
llm_reasoning_effort_answer = none
llm_parent_answer_reserve_tokens = 4000
llm_parent_answer_reserve_duration = 75s
llm_delegation_batch_timeout = 90s
llm_delegation_child_timeout = 60s
llm_answer_retry_max_tokens = 1024
```

字段名称最终应与现有平台设置命名规范统一；以上是语义草案，不是要求直接照抄。

### 9.2 Capability 配置示例

```yaml
llm:
  provider: openai-compatible
  model: deepseek-v4-flash
  api_style: chat_completions
  completion_limit_field: max_tokens
  reasoning:
    investigation:
      mode: enabled
      effort: low
    final_answer:
      mode: disabled
```

OpenAI Chat Completions 推理模型则应使用不同 profile：

```yaml
llm:
  provider: openai
  model: reasoning-model
  api_style: chat_completions
  completion_limit_field: max_completion_tokens
  reasoning:
    investigation:
      effort: low
    final_answer:
      effort: low
```

### 9.3 配置校验

启动或保存设置时校验：

1. `completion_limit_field` 必须属于当前 `api_style` 支持集合；
2. `max_tokens` 和 `max_completion_tokens` 不得在同一个 profile 中同时启用；
3. reasoning 参数必须在 capability 支持时才允许配置为显式模式；
4. `answer_reserve_duration < AgentTimeout`；
5. `delegation_batch_timeout + answer_safety < answer_deadline`；
6. Child timeout 不得超过 batch timeout；
7. answer retry 必须有独立的最小 token和时间门槛。

---

## 10. 实施阶段

### Phase 0：观测先行，不改行为

目标：确认实际网关对参数的支持情况。

工作：

1. 在请求日志中记录 `completion_limit_field`，不改变请求体；
2. 记录 provider 原始 usage、reasoning tokens、finish reason；
3. 增加 `visible_output_tokens` 的推导字段，但不用于拒绝请求；
4. 统计各阶段 reasoning/output 比例和 P95 时间；
5. 对当前 `deepseek-v4-flash` 网关做 capability canary：分别验证仅 `max_tokens`、仅 `max_completion_tokens`、thinking 参数的响应，不在生产主链路同时发送两个字段。

验收：

- 能确认网关实际接受的字段；
- 能区分 provider token 截断与 context deadline；
- 不改变现有业务输出。

### Phase 1：请求字段 capability routing

目标：内部只保留一个逻辑 completion limit，wire 层按 capability 序列化。

工作：

1. 增加 `ModelCapabilityProfile`；
2. 将 `chatRequest` 的 provider-specific 字段拆分/映射；
3. 禁止同时发送多个 completion limit 字段；
4. 增加 provider adapter 单测和请求体快照测试；
5. 保持当前 DeepSeek profile 使用 `max_tokens`。

验收：

- DeepSeek 请求体只有 `max_tokens`；
- OpenAI reasoning profile 只发送 `max_completion_tokens`；
- 未知 profile 能安全 fallback 并告警。

### Phase 2：阶段化 reasoning

目标：最终答案不再依赖 prompt 文案关闭 reasoning。

工作：

1. 扩展 `ModelParameters`/phase mode；
2. 在 investigation 与 final answer 调用中选择不同 mode；
3. provider 支持时传 `thinking` 或 `reasoning_effort`；
4. provider 不支持时保留 prompt fallback；
5. final answer 只保留一次短 direct-answer retry。

验收：

- Child report 的空 visible content 比例显著下降；
- final answer 阶段能在预留窗口内产生首个可见 token；
- 不支持参数的 provider 不会因未知字段失败。

### Phase 3：账本与答案 reserve 修正

目标：reasoning 继续计费，但不提前侵占 answer phase reserve。

工作：

1. 保存 `ProviderOutputTokens`、`ReasoningTokens`、推导的 `VisibleOutputTokens`；
2. 维持 total/cost 使用 provider 实际 usage；
3. 将 Parent answer reserve 明确应用于非 answer phase；
4. answer phase 使用 reserve，但仍受总账和 cost 上限约束；
5. 把错误码细分到具体预算维度。

验收：

- reasoning 不会被错误地从成本账本剔除；
- Child 的推理消耗不能消灭 Parent 已保留的 answer reserve；
- reservation settle/release 在超时和取消路径幂等。

### Phase 4：Delegation 时间治理

目标：Child 批次不能吞掉 Parent 最终答案时间。

工作：

1. 增加 batch deadline；
2. Child deadline 取 batch deadline、answer deadline、context deadline 的最小值；
3. `AwaitSettlement` 到 batch deadline 返回已 settle projection；
4. Parent 进入 answer phase 后停止等待 Child；
5. 未完成 Child 转为 timeout/orphaned，并由后台回收 reservation；
6. 使用 partial answer 契约交付。

验收：

- Parent 在 answer deadline 前稳定进入 final answer；
- Child 超时不会将 Parent 标记为整体失败；
- 已完成 report 能进入最终答案，未完成 subject 明确标注 pending。

### Phase 5：灰度与删除旧语义

只有 Phase 0~4 稳定后，才考虑：

- 删除只靠 prompt 的“no reasoning”假设；
- 删除未使用的旧 completion 字段；
- 收紧默认 Child timeout；
- 调整 answer reserve 默认值；
- 对 provider-specific adapter 做强校验。

---

## 11. 测试计划

### 11.1 Provider 请求体测试

| 场景 | 期望 |
| --- | --- |
| DeepSeek profile | 只有 `max_tokens` |
| OpenAI Chat reasoning profile | 只有 `max_completion_tokens` |
| Responses profile | 只有 `max_output_tokens` |
| profile 配置冲突 | 启动/保存时报错 |
| 未知模型 | 使用安全 fallback，不发送未声明字段 |
| reasoning 参数不支持 | 不发送该参数并记录 capability fallback |

### 11.2 Usage 与账本测试

1. provider 返回 `completion_tokens=12000`、`reasoning_tokens=10825`、content 为空；
2. 验证 `ProviderOutputTokens=12000`、`ReasoningTokens=10825`、`VisibleOutputTokens=0`；
3. 验证 total/cost 按 provider usage 结算；
4. 验证非 answer phase 不能使用 Parent answer reserve；
5. 验证 answer phase 可以使用保留额度；
6. 验证 reservation 在 provider error、context cancellation、settle error 时不泄漏。

### 11.3 时间预算测试

1. Child 在 token 截断前结束，不能被错误标记为 timeout；
2. Child 运行超过 batch deadline，Parent 获得 partial projection；
3. Parent 剩余时间不足 answer floor 时，直接走 deterministic partial answer；
4. Parent 不得在 run deadline 之后继续发起模型请求；
5. 4 个并发 Child 的最长墙钟时间受 batch deadline 限制，而不是四个 timeout 相加。

### 11.4 输出契约测试

1. Single-Agent 与 Synthesizer 使用同一用户可见答案规则；
2. Child 只输出 `investigation.report`，不会输出 Mermaid；
3. report JSON 回显输入字段时，返回 schema failure/partial，而不是伪造 success；
4. flow answer 在 fallback 时仍满足最低可交付格式；
5. partial answer 明确 confirmed/pending/limitations。

### 11.5 回放测试

使用以下日志回放：

```text
/Users/dequan.mac/.codex/attachments/39bde7fb-222f-4bd1-a566-dd95a78ad3c1/pasted-text.txt
```

回放目标：

- Child 不再因为 reasoning 吃满后才发现无 visible content；
- Parent 至少在 answer deadline 前进入最终答案阶段；
- 即使 Child report 为 partial，Parent 也能交付证据边界明确的答案；
- 日志能明确标记 token failure 或 time failure，而不是只显示 `cancelled`。

> 注：回放文件路径以实际附件为准；若附件文件名发生变化，应在测试说明中更新真实路径。

---

## 12. 风险与取舍

### 12.1 Provider 语义不一致

同名字段在不同兼容网关可能被接受但语义不同。解决方式：

- capability profile 显式配置；
- canary 请求验证；
- 请求日志记录实际 field；
- 禁止仅依赖 `provider=openai` 推断后端模型行为。

### 12.2 关闭 reasoning 可能降低答案质量

最终答案阶段关闭 reasoning 可能降低复杂推导质量。因此策略不是全局关闭，而是：

```text
investigation：允许低/中等 reasoning
synthesis：低 reasoning 或关闭
final answer：优先关闭，依赖已验证 evidence
```

如果 evidence 不足，应该回到 investigation，而不是让 final answer 无限思考。

### 12.3 Visible token 推导不准确

`ProviderOutputTokens - ReasoningTokens` 只能在 provider usage 语义明确时作为估算。系统必须保留 `UsageSource`，并避免用估算值做高风险拒绝判断。

### 12.4 增加 phase 后状态复杂度上升

阶段化会增加配置和日志字段。必须用固定 phase enum、统一预算错误码和 trace schema，避免每个 Agent 自定义一套阶段名称。

### 12.5 Partial answer 可能被误读为完整答案

所有 partial answer 必须显式包含状态、限制和 pending subject；API、SSE、持久化和前端展示使用同一状态字段，禁止只靠自然语言“可能不完整”提示。

---

## 13. 决策记录

### D1：是否同时发送 `max_tokens` 与 `max_completion_tokens`

**决定：否。**

理由：两者是 API 契约字段，不是可叠加预算；同时发送可能被拒绝、忽略或产生 provider-specific 优先级。

### D2：当前 DeepSeek V4 路径使用什么字段

**决定：默认使用 `max_tokens`，直到网关 capability 明确证明需要并支持其他字段。**

理由：当前链路是 `/chat/completions`，模型为 `deepseek-v4-flash`，现有代码和 DeepSeek Chat API 语义均以 `max_tokens` 为基础；`provider=openai` 只说明当前 adapter 使用 OpenAI-compatible 协议。

### D3：是否把 reasoning 从总 token/cost 中剔除

**决定：否。**

理由：reasoning 是真实 provider 消耗，剔除会导致成本和限额低估。需要隔离的是 answer phase reserve 和 visible answer 交付，而不是账单。

### D4：最终答案是否全局关闭 reasoning

**决定：不全局强制；按 phase 和 capability 控制，默认 final answer disabled/low。**

理由：调查需要 reasoning，合成阶段主要处理已有证据，应优先保证可见交付和时间稳定性。

### D5：是否通过提高 timeout 解决本次问题

**决定：否。**

理由：本次 Child 是 token 截断，Parent 是时间耗尽；单独提高 timeout 不能修复 Child 的 reasoning 消耗，反而可能进一步侵占 Parent answer reserve。

---

## 14. 最终验收标准

设计落地后，以下条件必须同时满足：

1. 任意一次请求只发送一个 provider 认可的 completion limit 字段；
2. DeepSeek profile 不再误发 `max_completion_tokens`，OpenAI reasoning profile 不再依赖旧 `max_tokens`；
3. Child 的 reasoning token 耗尽能被识别为 `budget.provider_completion_tokens`，而不是笼统的 `cancelled`；
4. Parent 的 answer deadline 在 delegation 开始时就被计算并受到硬保护；
5. Parent 在 Child 部分失败时仍能交付结构化 partial answer；
6. reasoning 继续计入实际总 token/cost；
7. answer phase 有独立 token/time/retry reserve；
8. Single-Agent 与 Multi-Agent 的用户可见答案使用同一套输出提示词和契约；
9. 所有 provider-specific 参数都有 capability、日志和回放测试；
10. 在相同的 2026-09-05 故障日志回放下，不再出现“Child 无 visible content + Parent deadline exhausted + 最终只能空/粗粒度 fallback”的组合故障。

---

## 15. 待评审问题

1. 当前生产网关是否完整支持 DeepSeek V4 的 `thinking` 和 `reasoning_effort` 字段，还是只透传基础 OpenAI-compatible 字段？
2. Parent flow 答案的 P95/P99 实际生成时长是多少，`parent_answer_reserve_duration` 应取 60 秒、75 秒还是 90 秒？
3. `VisibleOutputTokens` 是否需要进入持久化 schema，还是先作为 trace-only 派生字段？
4. Child report 的目标可见输出上限是否从 12000 降至 4000~8000，并由 `MaxReportTokens` 再做结构化限制？
5. batch timeout 是否应该按问题类型配置，还是先使用统一默认值？
6. 对于 provider 不支持 reasoning 控制的模型，是否允许路由到非 reasoning 模型作为 final answer fallback？

---

## 16. 实施顺序摘要

```text
先确认网关 capability
  ↓
只保留一个 completion limit wire 字段
  ↓
补充 reasoning mode / effort 的 phase 路由
  ↓
保留 reasoning 的实际成本记账，隔离 answer reserve
  ↓
增加 batch deadline 与 Child 回收
  ↓
启用 partial answer 与 deterministic fallback
  ↓
灰度、回放、再删除旧的隐式语义
```

本设计的最小可行修复不是“再传一个 `max_completion_tokens`”，而是：

```text
正确选择一个 provider completion 字段
+ 阶段化控制 reasoning
+ Parent answer 时间/额度硬预留
+ Child batch deadline
+ 可解释的 token/time 错误分类
```
