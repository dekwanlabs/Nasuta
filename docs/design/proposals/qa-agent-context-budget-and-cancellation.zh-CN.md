# QA Agent 上下文预算、收敛与取消治理提案

> 状态：提案，待实现
> 创建日期：2026-08-05
> 范围：QA Agent 工具结果交付、单次运行上下文、循环收敛、显式取消、调用方超时与输入观测
> 诊断来源：CodeLoom 评估运行 `20260805T032936.636941000Z`
> 关联提案：`qa-evaluation-findings-improvement.zh-CN.md`
> 待修订正式文档：`../agent-platform/04-context-session-and-tool-results.zh-CN.md`、`../agent-platform/08-observability-and-evaluation.zh-CN.md`

## 0. 文档生命周期

本文是针对 2026-08-05 QA 评估中上下文膨胀、超时和取消失效问题的独立开发提案，不直接修改正式架构基线。

执行顺序：

1. 建立可验证的 token 预算、工具结果 ledger 和活动调用取消测试。
2. 按开发切片修改工具结果交付、Agent loop、RunHub、Dashboard API 和评估调用方。
3. 通过单元测试、并发测试、端到端取消测试和固定数据集复评。
4. 将最终合同合并到正式上下文、会话、工具结果和可观测性文档。
5. 标记本文为 `Implemented`，记录实际参数、变更范围和遗留项。

当前状态：根因和目标行为已确认，尚未实施。

## 1. 评审范围

本提案评审以下主链：

```text
codeloom-eva attempt
  -> POST /api/qa/ask
  -> Dashboard 创建独立 run
  -> Agent 构造初始 messages 和 tool definitions
  -> model turn
  -> tool execution
  -> authoritative result 持久化
  -> PromptContent 追加到当前 run messages
  -> 下一轮重新发送完整 messages
  -> force conclusion
  -> run terminal
```

评审维度包括：

- 每个评估 attempt 是否复用 Session
- 单次 run 内工具结果是否有总量上限
- 权威结果与模型输入是否承担了不同职责
- 重复证据和重复字段是否浪费上下文
- 显式 abort 是否能中断活动中的 LLM 和工具调用
- 调用方 timeout、Agent timeout 和最终回答保留时间是否一致
- Agent 是否根据证据增量和剩余时间收敛
- 预算观测是否覆盖 Provider 实际接收的全部输入

不评审检索排序准确率、工具路由和实体消歧等其他评估问题；这些问题继续由 `qa-evaluation-findings-improvement.zh-CN.md` 管理。

## 2. 总体结论

本次故障不是“三次重试共享同一个 Session”导致的。`codeloom-eva` 每次请求都发送空 `session_id`，失败运行日志也持续显示：

```text
recent=0 recalledHistoryChars=0
```

上下文膨胀发生在单个 attempt 对应的单个 run 内。当前 Agent 每轮把 assistant tool-call 和完整工具 `PromptContent` 追加到 `messages`，下一轮再把整个消息数组发送给 Provider。`prepareToolDelivery` 只判断当前单个结果是否还能塞进整个剩余窗口，没有限制单工具、单轮和整次 run 的累计工具交付量。

问题由五个机制共同放大：

1. 新鲜工具结果默认无损进入模型，缺少 run 级共享预算。
2. 权威结果和模型投影虽然字段分开，成功合同仍要求二者相等。
3. `search_code` 同时携带 `text` 和 `preview`，两者经常重复。
4. Agent 缺少基于新证据和剩余时间的停止条件。
5. Eva 在 120 秒结束等待后，显式 abort 不能取消已经进行中的 Provider 调用。

目标不是恢复静默截断，也不是为评估中的服务名、类名或问题关键词增加分支。目标机制是：

```text
完整权威结果
  -> Trace / Artifact 持久化、审计和回放

有界模型投影
  -> 单工具预算
  -> 单轮预算
  -> 单 run 累计预算
  -> canonical evidence 去重
  -> 显式 partial / omitted / artifact reference

运行控制
  -> 证据增量收敛
  -> 剩余时间门禁
  -> 显式 abort 即时取消活动调用
```

## 3. 已确认问题

### 3.1 P0：单次 run 的工具结果无界累积

证据：

- `internal/agent/loop.go:349` 把每轮 assistant tool-call 追加到当前 `messages`。
- `internal/agent/loop.go:405` 把每个工具的完整 `execution.PromptContent` 继续追加到同一数组。
- `internal/agent/tool_delivery.go:29` 只计算当前结果能否进入剩余 Provider 窗口。
- 当前没有单工具最大交付量、单轮工具结果总量或单 run 累计工具结果总量。
- `codeloom-eva/internal/codeloom/qa.go:100` 每次请求都显式发送 `"session_id": ""`，可排除跨 attempt Session 累积。

示例：

`GLD-CON-017` 第三次尝试对应 `run_67a3c90b3528ba41fd1e2447`。日志中的可见上下文字符数从初始约 `26,448` 增长为：

```text
step 1  154,302
step 2  274,700
step 5  381,364
step 6  474,903
step 8  479,607
```

该 run 在 2026-08-05 11:43:14 开始强制总结，Eva 随后达到 120 秒 attempt timeout，但 CodeLoom 直到 11:45:09 才结束。

`GLD-TOOL-007` 第一次尝试对应 `run_01067abff9abfa288a63b393`。上下文从约 `29,452` 字符增长到 step 5 的约 `245,037` 字符，随后一次模型调用耗时约 83.6 秒。

影响：

- 同一个 run 的输入成本和延迟随工具轮次持续增长。
- 大量旧证据稀释当前未满足 claim 所需的直接证据。
- 即使每个 attempt 都是独立 Session，单次 attempt 仍可耗尽上下文和时间预算。
- 当前日志已经证明严重膨胀和超时，但没有证明 Provider 返回过硬性的 maximum context exceeded；方案不能把尚未发生的错误写成既成事实。

修改方案：

- 为每个 run 建立一个 token ledger，统一管理工具结果进入模型的预算。
- 预算只使用 token，不使用字符数参与运行决策：

```text
run tool-result budget =
    provider context window
  - immutable messages
  - tool definitions and schemas
  - final answer reserve
  - safety reserve
```

- immutable messages 包括 system、developer、当前问题、选中的历史原子轮次和 seed evidence。它们在 run 开始后不再与工具结果预算混算。
- 工具结果必须同时满足三层硬限制：

```text
delivered tokens for one tool call <= per-tool limit
delivered tokens for one model turn <= per-turn limit
sum of delivered tool tokens in one run <= run tool-result budget
```

- `prepareToolDelivery` 接收并更新同一个 run ledger，不再根据当前 messages 临时推断一个彼此独立的“剩余窗口”。
- 每次交付以实际新增的边际 token 为准。重复 canonical evidence 不再次扣除正文预算，只记录 dedup 指标。
- 预算耗尽后不再把新正文追加到模型上下文，只交付明确的 partial 元数据和 Artifact 引用。
- 所有上限由 Provider context、回答预留和运行配置推导；不得按 `GLD-*` ID、服务名、类名或问题关键词设定特殊值。

ledger 至少记录：

```text
authoritative_tokens
delivered_tokens
deduped_tokens
deduped_items
omitted_tokens
omitted_items
remaining_run_tool_tokens
```

预期效果：

- 独立 attempt 的语义保持不变，单个 run 的工具输入也有严格上限。
- 增加工具轮次不会使模型输入近似无界增长。
- 完整结果仍可审计，模型输入成本、TTFT 和超时率可被量化控制。

### 3.2 P0：权威结果与模型投影没有真正分离

证据：

- `internal/agent/tool_delivery.go:65` 在结果能放入剩余窗口时直接返回原 execution。
- 当前正式设计要求成功交付默认满足 `PromptContent == AuthoritativeContent`。
- 正式设计只允许无损转换、结构化错误或整次交付失败，不允许成功状态下的有损模型投影。
- 提交 `d9f8199 feat(agent): preserve authoritative tool results` 删除了旧的单工具默认压缩上限，修复了权威结果被截断的问题，但也移除了单工具保护。

示例：

一次 `search_code` 可以返回多个完整代码片段。它们对于 Trace 和回放是权威原文，但模型回答当前 claim 可能只需要候选身份、最相关片段和 coverage。当前合同只有两个极端：

```text
完整原文全部进入模型
或
整次交付失败并要求模型重新分页
```

多轮开放式 Agent 检索会反复选择第一种路径，直到整个 run 接近窗口上限。

影响：

- `AuthoritativeContent` 和 `PromptContent` 虽然是两个字段，实际成功语义仍然耦合。
- Runtime 无法在保留权威原文的同时，向模型交付明确、有界且可验证的证据投影。
- 若直接恢复旧的通用压缩，又会重新引入精确标识符丢失和静默不完整的问题。

修改方案：

- 修订正式合同，明确两个内容承担不同职责：

```text
AuthoritativeContent:
  持久化、哈希、审计、回放、Artifact 下载和精确校验的数据源

PromptContent:
  在当前 run token 预算内交付给模型的确定性证据投影
```

- 成功的部分交付必须是显式状态，不允许静默截断。模型投影至少携带：

```json
{
  "partial": true,
  "delivered_items": 8,
  "omitted_items": 21,
  "coverage": "partial",
  "artifact_id": "art_...",
  "items": []
}
```

- 完整权威内容先成功写入 Trace 或 Artifact，之后才能持久化 partial 投影和 Artifact 引用。
- 投影使用确定性结构规则，不使用另一次 LLM 调用生成摘要：
  - 保留结果身份、canonical source、score、coverage、错误和分页信息。
  - 按当前 claim 相关性和稳定顺序选择有界条目。
  - 同一 canonical evidence 只保留一次。
  - 明确记录省略条目和省略 token。
- `AnswerContract.RequiredLiterals` 仍是硬约束：
  - 所有 required literal 都进入 `PromptContent` 时，才允许激活对应答案合同。
  - 任一 required literal 无法在预算内完整交付时，本次交付为 delivery failure。
  - 不允许把 required literal 只放在 Artifact 中，却要求模型逐字回答。
- 普通非精确回答允许使用显式 partial 投影；不再要求成功状态下一律 `PromptContent == AuthoritativeContent`。
- Trace 同时保存权威内容哈希、模型投影哈希、预算决策和 omission 元数据。

预期效果：

- 权威原文保真与模型上下文有界不再互相排斥。
- 模型能够知道自己只看到了部分结果，不会把截断集合误认为完整集合。
- 精确答案合同继续保持可验证，不因预算治理而降级。

### 3.3 P1：`search_code` 重复承载正文

证据：

- `internal/agent/tools.go:363` 在同一个 match 中同时返回 `text` 和 `preview`。
- `knowledge/api.go:31` 和 `knowledge/api.go:32` 将两个字段都定义为 typed API 的序列化合同。
- 评估日志中多个 `search_code` 结果的 `text` 与 `preview` 完全相同。

示例：

当一个代码命中包含 8,000 字符正文，且 `preview` 复制同一段内容时，一个条目会向模型重复交付约 16,000 字符。10 个命中会在没有增加任何证据的情况下接近翻倍占用上下文。

影响：

- 单次工具结果在进入通用预算器之前已经存在结构性重复。
- token 预算被同一证据的两种展示表示消耗。
- typed API 把 UI 预览需求泄漏到模型输入合同。

修改方案：

- 模型使用的 `search_code` payload 每个命中只保留一个正文字段。
- `CodeSearchHit` 的稳定工具合同保留 `Text` 作为权威代码正文，删除作为模型输入字段的 `Preview`。
- Dashboard 或列表需要预览时，在展示和读取边界从 `Text` 有界派生，不持久化第二份正文。
- 若外部 REST 兼容性要求暂时保留 `preview`，应在 REST adapter 中派生，不能进入 Agent typed tool result。
- 增加测试验证一个 match 的序列化结果不会同时出现内容相同的 `text` 和 `preview`。

预期效果：

- `search_code` 模型载荷立即减少无信息增益的重复内容。
- UI 展示能力不再定义 Agent 工具合同。
- 后续 token ledger 统计的是实际证据，而不是重复表示。

### 3.4 P0：显式 abort 无法取消进行中的调用

证据：

- `internal/transport/dashboard/qa.go:322` 使用 `context.WithoutCancel(ctx)`，浏览器断线不会取消后台 run。
- 该行为符合“传输断开不影响后台任务”的产品语义，本身不是错误。
- `internal/transport/dashboard/qa.go:828` 的 abort 只向 RunHub 发送 `CtrlAbort`。
- `internal/agent/stream.go:460` 只把信号加入队列。
- `internal/agent/loop.go:220` 仅在每轮开始时轮询控制信号。
- 因此活动中的 LLM、工具或 force-conclusion 调用不会立即看到取消。

示例：

`GLD-CON-017` 的 Eva attempt 在 2026-08-05 11:43:32 达到 120 秒并发送 abort。Agent 已在 11:43:14 进入 force-conclusion，直到 11:45:09 才结束，终态仍记录：

```text
aborted=false err=nil
```

影响：

- 调用方已经结束等待后，Provider 调用仍继续消耗时间和 token。
- abort 的实际延迟取决于当前调用何时自然返回，无法作为运行控制保证。
- Eva 记录超时，CodeLoom 却记录正常完成，两侧终态不一致。

修改方案：

- RunHub 为每个活动 run 注册真实的 `context.CancelFunc`：

```text
RegisterRun(runID, cancel)
AbortRun(runID)
CompleteRun(runID)
```

- Dashboard 仍先使用 `context.WithoutCancel` 隔离浏览器断线，再为后台 run 创建独立可取消上下文：

```go
baseCtx := context.WithoutCancel(requestCtx)
runCtx, cancel := context.WithCancel(baseCtx)
```

- run 启动时注册 cancel，所有 LLM、工具和 force-conclusion 调用都从该 `runCtx` 派生。
- 显式 abort 同时执行两件事：
  - 调用活动 run 的 cancel，使当前阻塞调用立即收到 `context.Canceled`。
  - 保留 `CtrlAbort` 信号，用于 Agent 终态、暂停唤醒和审计语义。
- run 完成时注销 cancel。abort、complete 和重复 abort 必须幂等。
- abort 与自然完成并发时，终态转换使用现有持久化约束决定唯一结果，不增加第二套运行状态机。
- run 取消后的终态持久化可以使用独立、短时限的 `context.WithoutCancel`，但不得继续发起新的模型或业务工具调用。

预期效果：

- 浏览器断线仍不取消后台 run，显式 abort 则能即时中断活动工作。
- LLM、工具和 force-conclusion 使用同一取消链路。
- 调用方与 CodeLoom 对 aborted run 的终态认知一致。

### 3.5 P0：调用方与 Agent 的超时预算不一致

证据：

- `codeloom-eva/internal/config/config.go:162` 的默认 case timeout 是 120 秒。
- `codeloom-eva/internal/eval/runner.go:281` 为每个 attempt 建立该 deadline。
- 评估时 Agent 配置为 `timeout=5m0s`、`answerReserve=30s`。
- Dashboard 把请求上下文转换为 `context.WithoutCancel` 后，Eva 的 HTTP deadline 不再约束后台 Agent。

示例：

当 Eva 在 120 秒停止等待时，Agent 仍认为自己最多可以运行 5 分钟。即使 Eva 主动调用 abort，当前活动调用也不会被中断，形成：

```text
Eva:      timed_out
CodeLoom: running -> done
```

影响：

- 外层评估超时不等于底层计算停止。
- 最终回答保留时间只相对 Agent 的 5 分钟预算有效，对 Eva 的 120 秒窗口无效。
- 评估超时率、Provider 成本和 CodeLoom 成功率使用了不同的运行边界。

修改方案：

- QA API 增加可选的 `max_run_duration_ms`，由请求入口完成范围校验和 canonicalization。
- Agent 的有效运行时限为：

```text
effective run timeout =
  min(configured agent timeout, requested max_run_duration_ms)
```

- `max_run_duration_ms` 必须大于最终回答保留时间和最小安全余量；非法值在 API 入口直接返回明确错误。
- Eva 以 case timeout 减去传输和取消 grace 作为服务端运行预算。例如 120 秒 case timeout 可先配置 95 至 100 秒的 Agent run budget，实际值由基准测试确定，不硬编码在 Agent。
- Eva 达到本地 deadline 时：
  - 立即取消 SSE 请求。
  - 调用 abort。
  - 在有界 grace 内查询或等待 `aborted` 终态。
  - grace 结束后仍未终止时，记录 cancellation failure，而不是只记录普通 timeout。
- Agent 在开始新模型轮次或工具调用前检查剩余时间。若不足以覆盖下一调用预算和 final answer reserve，直接进入强制总结。
- 最终回答必须使用 run deadline 内预留的独立时间窗口，不能在外层调用方已经超时后继续生成。

预期效果：

- 调用方 timeout、Agent loop 和最终回答共享同一运行边界。
- Eva 在超时后可以验证底层工作是否真实停止。
- 运行不会因为 Agent 配置大于调用方预算而产生数分钟的尾部任务。

### 3.6 P1：工具循环缺少基于证据增量和剩余时间的收敛

证据：

- 当前 loop 主要受 `maxSteps`、重复 tool fingerprint 和整体 timeout 约束。
- `internal/agent/loop.go:422` 还可在边界证据出现时延长 step limit。
- 同一评估问题中，少量工具调用可以获得高分，另一些 attempt 在获得同类直接证据后继续扩搜并超时。
- `GLD-TOOL-007` 一次只调用权威依赖工具便完成并获得高分，另两次继续调用 12 至 13 个工具后超时。

示例：

模型已经通过专用依赖工具取得直接下游列表，但随后以不同查询参数继续执行代码搜索、符号查询和依赖查询。新结果大多重复已有来源，工具调用数增加，却没有对应的 claim coverage 增量。

影响：

- step limit 只能限制最坏轮数，不能判断继续检索是否有价值。
- 重复或低增量结果继续消耗 token ledger 和时间预算。
- 相同问题在不同 attempt 中出现明显的延迟和得分方差。

修改方案：

- 为 run 维护 O(1) canonical evidence 集合，key 至少由证据类型、canonical source 和稳定内容标识组成。
- 每轮单次遍历汇总：

```text
new_evidence_items
duplicate_evidence_items
failed_tool_calls
coverage_delta
remaining_tool_tokens
remaining_run_time
```

- 以下条件触发结束工具阶段并基于现有证据总结：
  - 直接回答 claim 的权威专用工具已成功，且没有未满足的必要 claim。
  - 连续达到明确阈值的轮次没有新增 canonical evidence 或 coverage 增量。
  - 剩余工具 token 不足以接受下一轮有意义的证据。
  - 剩余时间不足以覆盖下一次模型或工具调用预算以及 final answer reserve。
- capability 缺失、实时证据不可用或查询失败时，按现有证据边界明确 abstain；不得用代码或文档 provider 静默替代实时 provider。
- 收敛阈值是通用运行配置，并通过合成重复证据 fixture 验证，不针对评估 case 设置。
- 保留 `maxSteps` 作为最后一道硬上限，但不再把“还有 step”视为继续检索的充分条件。

预期效果：

- Agent 只有在产生新证据或解决明确 claim 缺口时继续调用工具。
- 已获得直接权威答案的简单问题可以提前结束。
- 重复扩搜、上下文增长和长尾延迟同步下降。

### 3.7 P2：当前上下文观测低估 Provider 实际输入

证据：

- `internal/agent/loop.go:813` 的 `contextChars` 只累计 `message.Content`。
- 该指标未统计 tool definitions 和 JSON schema。
- 该指标未统计 assistant tool-call arguments。
- 该指标未统计消息角色、名称、tool call ID 和 Provider 协议包装开销。
- 字符数也不能稳定映射不同模型 tokenizer 的实际 token 数。

示例：

日志中的 `contextChars=159031` 只表示可见消息正文字符数。若本轮还包含 16 个工具 schema 和多组较长 JSON 参数，Provider 实际输入会明显大于该值；不同语言和代码内容的 token/字符比例也不同。

影响：

- 运维看到的 context size 不是 Provider 输入预算的完整口径。
- 无法解释某轮为何突然变慢或接近窗口上限。
- 字符阈值容易在中文、英文、代码和 JSON 间产生不同误差。

修改方案：

- 运行预算统一使用与 `ensureInputBudget` 相同的 token estimator。
- preflight 指标至少拆分为：

```text
message_tokens
tool_definition_tokens
tool_call_argument_tokens
protocol_overhead_tokens
reserved_output_tokens
estimated_total_input_tokens
context_window
remaining_input_tokens
```

- Provider 返回 usage 后记录实际 `input_tokens`，并计算 estimator error；预算决策使用保守估计值。
- `contextChars` 可以保留为排障辅助指标，但不得继续作为容量判断依据。
- 每次模型调用、工具交付和强制总结使用同一套 token 口径，避免不同模块分别估算。
- 日志和 trace 记录统计值，不重复打印完整工具正文或敏感内容。

预期效果：

- 预算、日志和 Provider usage 可以互相核对。
- 可以定位成本来自历史、工具 schema、参数还是工具结果。
- 多语言和代码输入不再依赖不稳定的字符比例。

## 4. 安全性评估

本提案不增加工具权限和业务数据访问范围，但实现时必须保持以下边界：

- 权威工具结果进入 Artifact 后，访问继续按 user、tenant、session、run 和 tool call 校验。
- partial 投影必须保留 coverage、omitted count、error 和 Artifact 引用，不能把不完整结果伪装成完整集合。
- Artifact 引用不能绕过原工具的权限，也不能作为模型可自由拼接的本地文件路径。
- abort 只能控制调用者有权限访问的 run，不能仅凭 run ID 取消其他用户任务。
- 取消后的短时终态持久化不得重新执行业务工具、Provider 调用或写动作。
- token 观测只保存计数、哈希和必要元数据，不新增完整 prompt 日志。
- 配置了某个 Provider 后，其失败必须保持可观察；预算不足、超时或取消不得触发静默 Provider 替换。

## 5. 目标架构原则

### 5.1 Session 隔离不能替代 run 预算

每个 attempt 使用独立 Session，只能阻止跨 attempt 历史累积。每个 run 仍必须独立拥有工具 token、时间和 step 上限。

### 5.2 权威内容与模型投影职责分离

完整内容服务持久化、审计和回放；模型投影服务当前回答。二者通过哈希、Artifact 和 coverage 关联，不再通过内容必须相等来证明可信。

### 5.3 预算只有一个 owner 和一种单位

run token ledger 是工具结果预算的唯一 owner。单工具和单轮限制都从同一个 ledger 扣减，所有容量决策统一使用 token。

### 5.4 不完整必须显式

任何条目省略、正文裁剪或分页未完成都必须通过 `partial`、`omitted_items`、coverage 和 Artifact reference 表达。静默截断不是合法成功路径。

### 5.5 取消是活动调用中断，不是下一轮提示

控制信号负责业务语义，`context.CancelFunc` 负责中断正在运行的工作。两者缺一不可。

### 5.6 循环继续必须由新证据证明

剩余 step 不是继续调用工具的理由。只有新增 canonical evidence、coverage 增量或明确未满足 claim 才能证明下一轮有价值。

### 5.7 重复表示在生产边界消除

`text` 与 `preview` 等同一内容的展示副本不能进入模型合同。UI 所需预览在展示边界派生。

## 6. 开发切片

### Slice 1：建立统一 token ledger 和观测口径

- 抽取 Provider 输入 token 估算器，覆盖 messages、tool schema、tool-call arguments、协议开销和输出预留。
- 为单个 run 建立 tool-result ledger。
- 增加单工具、单轮和单 run 三层预算配置与默认推导。
- Trace 记录 authoritative、delivered、deduped、omitted 和 remaining tokens。
- 先保持现有交付行为，只增加观测和 shadow budget decision，验证估算准确度。

### Slice 2：实现权威内容与有界模型投影

- 修订 `ToolExecution` 的交付语义。
- Artifact 或完整 Trace 先保存权威结果。
- 对结构化结果实现确定性、显式 partial 的有界投影。
- RequiredLiteral 无法完整交付时保持 delivery failure。
- 增加预算边界、partial coverage、Artifact 和答案合同测试。
- 更新 `04-context-session-and-tool-results.zh-CN.md` 的正式合同。

### Slice 3：删除 `search_code` 双正文载荷

- 从 Agent typed payload 删除重复 `Preview`。
- 在需要兼容的 REST 展示 adapter 中有界派生 preview。
- 增加 JSON 契约和 token 体积回归测试。

### Slice 4：为活动 run 增加即时取消

- RunHub 注册和注销活动 run 的 cancel。
- abort 同时取消 context 并保留 `CtrlAbort` 语义。
- 统一 LLM、工具和 force-conclusion 的父 context。
- 增加阻塞 LLM、阻塞工具、paused run、重复 abort 和 abort/complete 并发测试。
- 使用 `go test -race` 验证 RunHub 并发访问。

### Slice 5：对齐 API、Eva 和 Agent 的 deadline

- QA 请求增加有界 `max_run_duration_ms`。
- Agent 使用配置和请求预算的最小值。
- Eva 发送服务端运行预算，超时后执行 abort 并验证终态。
- 增加调用方 deadline 小于 Agent 配置时的端到端测试。

### Slice 6：加入证据增量和剩余时间收敛

- 建立 canonical evidence map 和单轮增量统计。
- 为直接权威证据、连续无进展、token 耗尽和时间不足增加通用收敛条件。
- 保留 `maxSteps` 作为硬上限。
- 使用重复证据、空结果、部分结果和 capability 缺失 fixture 验证。

### Slice 7：固定基线复评并归档

- 在干净 revision 上重跑黄金用例。
- 对比超时率、P50/P95、TTFT、工具调用数、峰值输入 token 和分数方差。
- 将实现结果合并到正式上下文和可观测性文档。
- 记录预算默认值和未解决问题后归档本文。

## 7. 验收门禁

### 7.1 单元测试

- 单个 Eva attempt 继续发送空 `session_id`，不同 attempt 不共享 Session。
- 单工具结果超过 per-tool limit 时，完整权威内容仍可从 Trace 或 Artifact 恢复。
- 单轮多个工具结果的 delivered token 总和不超过 per-turn limit。
- 单 run 多轮工具结果的 delivered token 总和不超过 run budget。
- partial 投影包含 `partial`、`omitted_items`、coverage 和 Artifact 引用。
- 任意 RequiredLiteral 未进入 `PromptContent` 时，答案合同不能激活。
- canonical evidence 去重使用 map，重复证据不重复扣减正文预算。
- `search_code` 模型 payload 不同时包含重复的 `text` 和 `preview`。
- token estimator 覆盖 tool schema 和 tool-call arguments。

### 7.2 取消与并发测试

- 阻塞中的 fake LLM 在 abort 后的有界时间内收到 `context.Canceled`。
- 阻塞中的 fake tool 在 abort 后的有界时间内收到 `context.Canceled`。
- force-conclusion 在 abort 后停止，不再产生 answer token。
- paused run 被 abort 时立即唤醒并进入 aborted 终态。
- 重复 abort、complete 后 abort、abort 与 complete 并发均不 panic、不泄漏 goroutine、不产生双终态。
- `go test -race -count=1 ./internal/agent/...` 通过；若遇到已知历史 flaky test，单独重跑并记录。

### 7.3 端到端测试

- 浏览器断线不会自动取消后台 run。
- 显式 abort 会取消同一 run 的活动 LLM 或工具调用。
- Eva timeout 后 CodeLoom 不再产生该 run 的新模型调用或工具调用。
- Eva 能在取消 grace 内观测到 `aborted`；超出 grace 时明确记录 cancellation failure。
- `max_run_duration_ms` 小于 Agent 默认 timeout 时，run 使用请求预算。
- 剩余时间不足时不开始新工具轮次，仍保留最终回答预算。

### 7.4 评估门禁

- 固定 revision、固定配置和固定数据集重跑，不能使用 dirty worktree 结果作为正式基线。
- 评估 timeout 次数必须低于本次的 9 次，且不得出现 Eva 已超时后 Agent 继续运行超过取消 grace 的记录。
- 单次 run 的峰值 estimated input tokens 不超过配置的 context window 和 safety reserve。
- 工具结果 delivered token 不超过三层预算。
- 连续无新增证据的 run 必须在达到收敛阈值后结束工具阶段。
- 超时相关用例的最大工具调用数、P95 延迟和重复 attempt 方差均应下降。
- 不能以牺牲 RequiredLiteral、canonical citation 或权威结果可审计性换取超时下降。

## 8. 非目标

- 不把多个 Eva 重试合并为同一个 Session。
- 不通过减少重试次数掩盖单次 run 的上下文问题。
- 不恢复通用静默截断或不可审计的 LLM 摘要。
- 不要求所有 partial 结果都失败；只有精确合同无法满足或工具声明不可部分使用时才失败。
- 不为 `GLD-*`、具体服务、类名、文件名或问题关键词增加停止和预算特判。
- 不在本提案中调整 dense、BM25、rerank 或 `code_rank` 的召回排序逻辑。
- 不因取消、超时或预算不足静默替换 LLM、搜索、日志或配置 Provider。
- 不引入新的持久化运行状态机；终态继续由现有 run lifecycle 管理。
