# Agent 架构、执行流与 Run 收敛

[English](01-architecture-and-execution.md) | [中文](01-architecture-and-execution.zh-CN.md)

> 状态：统一基线；主体已实现，状态表达仍需持续收敛
> 来源：Agent End-to-End Execution Flow、QA Run State Convergence

## 1. 模块职责

本模块定义一次 QA Run 从请求进入到最终答案持久化的完整生命周期，以及运行状态由哪些事实推导。它不拥有检索算法、业务工具或长期记忆策略，只负责组合它们。

```text
HTTP/SSE request
  -> session and permission boundary
  -> question analysis and evidence plan
  -> bounded context assembly
  -> immutable tool snapshot
  -> reason -> act -> observe loop
  -> reserved final-answer phase
  -> stream, persist, trace
```

## 2. 所有权边界

- `internal/transport`：解析请求、鉴权、SSE 连接和 DTO 转换；不拥有 Agent 决策。
- `internal/agent`：运行编排、工具循环、超时、最终回答和 SessionMessages。
- `internal/retrieval`：返回有界证据候选，不决定最终答案。
- `tool.Registry`：定义本次 Run 可见的工具快照。
- Session/Store：原子保存用户消息、工具调用、工具结果和最终回答。
- CodeLoom：注册应用工具和业务证据源，不把业务策略上移到 Nasuta。

## 3. 入口与不可变快照

一次 Run 在入口建立以下不可变信息：

- 用户与 Session 身份；
- 当前问题和附件；
- Registry 版本及工具快照；
- 权限和写操作策略；
- Provider、模型窗口和回答 token 配置；
- EvidencePlan 和 ResponseMode。

Run 内不得因为路由结果、某次工具失败或上一轮工具选择而改变工具定义。配置热更新只影响下一次 Run。

## 4. 问题分析

问题分析产生两个独立结果：

```text
ResponseMode: 回答如何组织
EvidencePlan: 允许获取哪些证据来源
```

ResponseMode 只提供结构提示，不能授予工具权限。EvidencePlan 描述 Memory、Internal、Runtime、Web 等来源的可用性和优先级，不保证一定调用某个工具。

## 5. 上下文装配

模型输入按优先级单次装配：

1. 系统合同和当前用户问题；
2. 当前 Run 已取得的新鲜工具证据；
3. 强关联的近期原子轮；
4. 按需召回的归档历史；
5. 当前问题的内部检索证据；
6. 准入后的长期记忆。

装配必须使用 Provider 感知的 token 预算。每个分区有独立上限，总预算只递减一次，不通过反复扫描和裁剪碰运气。

## 6. 工具循环

标准循环是：

```text
LLM response
  -> final answer: validate and finish
  -> tool calls: execute against the same snapshot
       -> append paired assistant/tool messages
       -> update evidence and coverage
       -> continue
```

不变量：

- assistant tool call 与 tool result 必须按 `tool_call_id` 配对；
- 工具错误作为可见结果返回模型，不能伪装成成功；
- 一轮多个工具调用各自记录时长、结果、coverage 和错误；
- Step 上限是执行预算，不是跳过最终答案的理由；
- 当前 Run 中取得的工具结果优先于历史摘要。

## 7. 超时与最终回答

总超时拆成两个区间：

```text
Tool-loop deadline = Timeout - AnswerReserve
Final-answer deadline = reserved remainder
```

工具循环耗尽后进入无工具的最终总结阶段。最终回答必须：

- 说明已取得的证据；
- 说明失败、遗漏和不确定性；
- 不声称未调用的工具已经执行；
- 不因 required evidence 缺失而返回空字符串；
- 接受精确输出合同校验；
- 在长度截断时通过 continuation 恢复，无法恢复时返回明确错误。

## 8. Run 状态模型

Run 状态应从事实推导，不建立独立持久化状态机。

推荐事实：

- 是否存在 final answer；
- context 是否取消或超时；
- 是否存在未完成 tool call；
- 最终 Provider `finish_reason`；
- 是否通过答案合同；
- 是否持久化成功。

可展示状态可以是 `running`、`completed`、`failed`、`aborted`，但它们是上述事实的投影。禁止为“计划中”“正在总结”“等待工具”等线性步骤增加不可恢复的持久状态。

## 9. Streaming 与持久化

- 推理文本、工具调用和最终答案使用不同事件类型；
- 模型开始输出 tool call 后，之前的候选回答不得继续流给用户；
- 只有通过最终校验的答案才作为 deliverable answer 持久化；
- 一次逻辑轮次按 `user -> assistant(tool_calls) -> tool results -> assistant final` 原子保存；
- 取消时已完成的工具结果仍保存，避免协议配对断裂；
- Session 读取在存储边界有 `LIMIT`，不得全量加载后内存裁剪。

## 10. 失败语义

| 失败 | 行为 |
|---|---|
| 可选能力未配置 | 记录 warning，只禁用该能力 |
| 已配置 Provider 不可用 | 明确失败，不替换 Provider |
| 工具执行失败 | 作为 tool result 交给模型并限制结论 |
| 工具循环耗尽 | 使用 AnswerReserve 强制总结 |
| 模型空响应 | 有界重试；仍为空则明确错误 |
| 模型输出截断 | continuation；不可恢复则错误 |
| 答案合同失败 | 修复重试；仍失败则不交付残缺答案 |
| Session 持久化失败 | 返回可观察错误，不声称已保存 |

## 11. 验收标准

1. 同一 Run 的工具 schema 和权限快照保持不变；
2. 工具失败和步数耗尽都不会绕过最终回答阶段；
3. tool call/result 始终合法配对；
4. 未通过合同的候选答案不会流式交付或持久化为最终答案；
5. Run 状态可由持久化事实重建；
6. Session 在线读取保持有界；
7. Trace 可以还原入口、规划、工具、总结和持久化阶段。

## 详细归并材料

### Agent 端到端执行流

> Migrated from CodeLoom `docs/design/agent/agent-execution-flow.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现

本文是 Dashboard QA 的规范请求时序。MCP 工具调用复用同一套 Registry 和存储，但不进入这条对话编排链路。

#### 1. 时序

```text
POST /api/qa/ask
  -> 规范化 question 和 source_mode
  -> 加载会话摘要和最近六条消息
  -> 创建 run ID 并订阅 SSE 事件
  -> 分析问题
       ├─ ClassifyResponseMode（本地答案结构提示）
       ├─ 决定 EvidencePlan（model/rule/explicit/fallback）
       ├─ 执行已配置的问题预处理和术语提取
       └─ 可选地并行改写为独立问题
  -> Plan 选中 Memory 时执行召回
  -> Plan 选中 Internal/Observe 时执行预检索
  -> 根据 EvidencePlan 构建提示词和工具策略
  -> reason -> act(tool) -> observe 循环
  -> 流式输出 reasoning、step、reference、答案 token 和 trace
  -> 持久化用户/助手消息
  -> 异步重新生成会话摘要
  -> 成功运行后异步提取长期记忆
```

#### 2. 入口与会话上下文

Dashboard 边界负责裁剪问题、规范化 `source_mode`、校验显式证据选择，并在开始 SSE 前拒绝不可用的 QA 服务。

对于已存储会话，数据库直接返回摘要和最近六条消息，不先加载完整会话再在应用内截取。调用方传入的 history 只在没有已存储会话时作为 fallback。

Run 必须在同步规划前订阅 Hub，否则早期阶段事件会丢失。

#### 3. 问题分析

##### ResponseMode

`ClassifyResponseMode` 使用本地信号选择预期答案结构：

```text
bug_analysis | requirements_analysis | architecture_review | code_review | codebase_qa
```

它不能授权工具，也不选择证据。

##### EvidencePlan

API 显式 Plan 绕过模型路由。自动规划请求快速 LLM 返回证据来源集合和置信度；启用配置化 Observe 时，规划、Observe 预处理和查询术语提取共享一次结构化辅助调用。

短路的元问题记录为规则产生的 Direct Plan。规划失败和低置信度 Direct 使用可观测的 Internal fallback。决策完成后再检查所需来源是否可用。

完整策略和评估契约见 [Agent 证据规划](02-evidence-and-tooling.zh-CN.md)。

##### 独立问题改写

存在最近上下文且问题包含指代时，独立问题改写与证据分析并行。能够消解指代时检索使用改写结果；否则将清理后的问题与有界最近上下文组合。会话摘要是路由上下文，不会自动拼接到每次检索查询。

#### 4. 证据获取

证据执行严格遵循有效 Plan：

- Memory 最多召回三条当前用户或全局用户 `0` 的语义候选。
- Internal/Observe 进入 `Retriever.RetrievePlan`，完成分发、发现、扩展和预算化组装。
- Web 不预取；只有选中 Web 时才向 Agent 开放 `web_search` 和 `web_fetch`。
- Direct 和仅 Memory 的 Plan 跳过预检索。

选中但不可用的来源会记录日志，并在适用时写入上下文或 Trace，系统不会静默切换 provider。

各来源实现见[内部检索](03-retrieval-and-knowledge.zh-CN.md)、[Web 证据](02-evidence-and-tooling.zh-CN.md)和[运行时证据与事件](02-evidence-and-tooling.zh-CN.md)。

#### 5. 提示词与工具循环

Agent 组合系统指令、持久摘要、最近原始消息、召回记忆、检索上下文、角色提示和当前问题。平台先按请求权限固定候选 Snapshot，再按 `RoutingSpec` 的意图结果派生本轮 Snapshot；模型定义和执行都只使用这一份不可变视图。

每轮循环：

1. 检查 pause、resume、nudge 或 abort 控制；
2. 使用完整答案 token 预算流式执行一轮模型；
3. 记录耗时及模型/Step 输出；
4. 没有工具调用时返回答案；
5. 否则执行被允许的工具，记录有界结果，并将 Observation 加入下一轮。

完全相同的工具调用会去重。搜索结果重叠时添加收敛提示。发送给模型的工具结果与保存在 Run Step 中的完整结果分别限长。

#### 6. 超时与最终答案

整个 Run 使用 `AgentConfig.Timeout`。迭代循环只使用 `Timeout - AnswerReserve`，确保工具耗尽循环预算或达到最大步数后，`forceConclusion` 仍有保留时间。

首个可见内容事件记录 `first_answer_token`，同时包含单轮 TTFT 和 Run 累计 TTFT。因长度截断的答案可以续写；无法恢复的 reasoning 截断会返回错误，而不是空的成功答案。

#### 7. 流式输出与持久化

SSE 输出 Run 生命周期、Phase、Reasoning、工具调用/结果、答案 Token、Reference、Trace 节点和终态。配置 Run Store 时，Agent Run 和 Step 持久化到 MySQL。

流式输出结束后，Dashboard 保存用户和助手消息，并启动有界后台任务重新加载会话、生成单一持久摘要。另一个后台任务可在成功且未中止的 Run 后提取跨会话记忆。

会话摘要和长期 Memory 职责不同：

- Summary 保存一个会话内的进度；
- Memory 尝试保存跨会话可复用的用户上下文。

当前 Memory 限制和过期事实处理目标见[长期记忆](07-memory.zh-CN.md)。

#### 8. 评估节点

QA Trace 可包含：

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

每个节点拥有自己的输入、输出、状态和耗时。因此节点级评估可以区分规划慢、检索弱、工具重复、模型 TTFT 和最终答案质量问题。

#### 9. 不变量

1. 会话上下文在数据层完成限量。
2. 证据规划和独立问题改写可以并行，但检索必须等待二者完成。
3. `EvidencePlan` 在一个 Run 内不可变，并同时控制预检索和工具权限。
4. 迭代工具循环不能占用答案保留时间。
5. 后台摘要和记忆提取不能延迟流式答案。

### QA Run 状态收敛设计

> Migrated from CodeLoom `docs/design/agent/agent-run-state-convergence.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：P0-P2 已实施（2026-07-18）

本文盘点 Dashboard QA 从请求接入、证据规划、检索、Agent 工具循环、SSE 输出到会话和长期记忆的状态表达，区分真正状态机、不可变策略、事件分类和并发协调，并给出不引入新业务状态机的收敛方案。

#### 1. 结论

普通 QA 主链路不需要更多状态机。当前真正需要维护的只有：

1. Agent 的 model -> tool -> model -> answer 执行循环；
2. Run 的 `running -> done | failed | aborted` 持久生命周期；
3. 可选的 pause/resume/abort/nudge 运行控制；
4. 独立 Memory 领域的 `active -> superseded` 生命周期。

LLM 长度续写是 Provider 调用内部的局部协议。WriteGate 和 Incident 各自拥有独立状态机，但不属于普通只读 QA Run，不应并入 QA 生命周期。

修复前的复杂度主要不是状态机数量，而是同一个 Run 终态、暂停状态和实时完成信号由多套变量、通道和存储重复表达。应统一所有权，不应新增 facet coverage、证据完整度或检索阶段状态机。

实现结果：

- `RunOutcome` 统一推导 done/failed/aborted，空答案不再成功；
- `RunStore.Complete` 只允许 running/paused 原子进入一个终态；
- `RunHub` 成为 token/reasoning/step/trace/terminal 唯一实时通道；
- 删除无写入者的 `AskResult.TokenCh`、`ErrCh` 和 Transport 双通道 fallback；
- pause/resume 使用条件更新持久化，abort 可唤醒暂停等待；
- 服务启动时将遗留 `running/paused` Run 一次性收敛为 `aborted`；
- 删除 `internal/domain/agent.go` 中重复且未使用的 `RunRecord`、`StepKind`。

#### 2. 状态分类

| 表达 | 当前形式 | 分类 | 是否保留 |
| --- | --- | --- | --- |
| `EvidencePlan` | `memory/internal/observe/web` 位集 | 一次决策后的不可变策略 | 保留，不作为状态机 |
| `ResponseMode` | 五种答案模式 | 分类结果 | 保留，不作为状态机 |
| Retrieval | discover -> expand -> assemble | 顺序流水线 | 保留，不持久化阶段状态 |
| `StepKind` | retrieval/think/tool_call/tool_result/answer | 事件与审计分类 | 保留，不作为 Run 状态 |
| SSE `Phase` | 预处理、检索、生成提示 | 临时 UI 文案 | 保留，不持久化 |
| Evaluation Trace | 节点、状态、耗时 | 评估遥测 | 保留，不驱动业务转换 |
| Agent loop | model/tool 迭代 | 执行状态机 | 保留 |
| `RunStatus` | running/done/failed/aborted/paused | 持久生命周期 | 收敛所有权 |
| Run control | pause/resume/abort/nudge | 运行控制协议 | 按产品需要保留 |
| LLM continuation | stop/length/reasoning-truncated | Provider 局部协议 | 封装在生成层 |
| Memory status | active/superseded | 独立领域状态机 | 保留 |
| Web convergence | 已抓取域名集合 | 有界聚合状态 | 保留为局部值 |
| Trace recorder | buffered/live | 订阅前并发协调 | 保留，不暴露为业务状态 |

#### 3. 修复前问题

##### 3.1 Paused 有两套事实来源

`RunStatus` 和前端都声明了 `paused`，但生产代码没有调用 `RunStore.SetStatus`，暂停只存在于 `RunHub.paused` 的内存 channel 中。

结果是：

- Agent 已暂停时数据库仍可能是 `running`；
- Run 管理页依赖数据库状态展示恢复按钮，可能永远看不到 `paused`；
- 服务重启后暂停信息消失；
- `RunStatusPaused` 和 `SetStatus` 形成未闭环的伪状态机。

##### 3.2 Run 终态被重复表达

一次完成同时由以下位置表达：

1. `RunResult` 的 `Aborted`、`Err` 和 `Answer`；
2. `RunStore` 的 `done/failed/aborted`；
3. `RunHub` 的 `SSEEvent{Done:true}`；
4. Dashboard Transport 的 `hubSentDone` fallback。

Transport 必须判断 Hub 是否已经发送 Done，再决定是否补发 `run_end` 和 `done`，说明终态没有单一发布者。

##### 3.3 实时答案存在两套通道

`AskResult` 暴露 `TokenCh` 和 `ErrCh`，同时 Agent token、reasoning、step 和 done 又经 `RunHub` 广播。当前 QA Service 创建的 `tokenCh` 没有写入者，只有关闭操作；Transport 仍同时监听 `TokenCh` 和 Hub channel，并维护双通道收敛逻辑。

这不是业务状态机，但增加了与状态机相同的组合复杂度：任一通道关闭、Hub Done、请求取消和错误通道都可能结束等待。

##### 3.4 Agent 终态由多个布尔条件推导

Agent loop 同时使用：

```text
answered              loop 内局部，仅门控 forceConclusion，不持久化
RunResult.Aborted
RunResult.Err
loopCtx.Err
```

它们共同决定是否正常结束、强制结论、失败或中止。`FinishReason` 不在此列——它只存在于 `llm.ChatStreamResult`，在 `continueIfNeeded` 内部局部消费，从不上浮到 `RunResult`，归入第 2 节的 Provider 局部续写协议。

无需再新增 Run 内部状态枚举，但必须集中校验非法组合。修复前 loop 可返回 `err=nil && answer="" && !aborted`（loop 预算提前 break，或 `forceConclusion` 返回空内容时，`loop.go:303-316`），即“空成功”是实际缺陷而非假设，归一化必须拒绝该组合。

##### 3.5 类型存在重复权威风险

`internal/domain/agent.go` 定义了 `RunRecord` 和 `StepKind`，实际 Dashboard QA 主链路使用 `internal/agent/service.go` 中的另一套定义。即使当前没有形成运行时冲突，两套同名生命周期模型会让后续调用方无法判断哪个是权威类型。

#### 4. 目标模型

QA 主链路收敛为：

```text
EvidencePlan         一次请求的不可变能力策略
Agent loop           唯一 model/tool 执行循环
RunStore             Run 可查询生命周期的权威来源
RunHub               Run 状态和 Step 的实时投影，不拥有第二套终态
Memory/WriteGate/
Incident             各自独立的领域生命周期
```

##### 4.1 Run 生命周期

基本终态只允许：

```text
running -> done
running -> failed
running -> aborted
```

终态不可再次转换，且必须同时写入 `ended_at`、`step_count` 和实际使用量。

Pause 有两种合法产品选择，必须二选一：

1. **持久控制**：正式支持 `running <-> paused`，所有转换通过一个 Run lifecycle service 更新存储和 Hub；
2. **进程内控制**：暂停只承诺当前进程有效，不写入 `RunStatus`，删除数据库和前端的 `paused` 状态分支。

不能继续维持“类型和 UI 声明持久 paused、后端只保存内存 channel”的混合语义。

##### 4.2 实时事件

`RunHub` 是唯一实时事件通道：

```text
phase       临时 UI 提示
reasoning   Provider 流式 reasoning
token       可见答案
step        可持久化业务事件
trace       可选评估遥测
terminal    带 RunOutcome 的唯一完成事件
```

`phase`、`step.kind` 和 `trace.status` 都不是 Run 状态。它们不得修改 `RunStatus`，也不能成为恢复执行所需的持久状态。

##### 4.3 RunOutcome

Agent loop 返回事实，QA 编排层统一归一化为一个终态结果：

```go
type RunOutcome struct {
    Status    RunStatus
    StepCount int
    TokenUsed int
    Answer    string
    Err       error
}
```

该类型是完成操作的输入，不是新状态机。一个 `Complete` 边界负责持久化终态、广播 terminal event 和清理控制资源。

#### 5. 不引入的新状态

以下概念保持为值、事件或局部计算，不新增持久状态机：

- facet coverage；
- `strong/weak/empty` 证据状态；
- retrieval pending/completed；
- answer drafting/finalizing；
- Phase 文案；
- StepKind；
- Trace node status；
- Runbook chunk 选择阶段；
- Prompt 规则中的“链路完整”。

链路完整性继续作为回答前的证据约束：工具可用时执行一次定向补查，仍缺失时准确说明断点。它不需要跨请求恢复，也不应进入数据库。

#### 6. 已实施切片

##### P0：消除伪状态机（完成）

1. 决定 pause 是持久能力还是进程内能力；
2. 删除未使用或补全未闭环的 `RunStatusPaused` 和 `SetStatus`；
3. 为 Run 状态转换增加允许边校验；
4. 明确服务重启时遗留 `running/paused` Run 的修复策略。

##### P0：统一实时通道（完成）

1. 以 `RunHub` 作为唯一 token/reasoning/step/terminal 通道；
2. 删除无写入者的 `AskResult.TokenCh`；
3. 将异步错误并入 terminal outcome，或保留一个只用于初始化失败的同步错误返回；
4. 删除 Transport 的双 channel merge 和 `hubSentDone` fallback。

##### P1：统一完成边界（完成）

1. 增加单一 `Complete(runID, outcome)` 操作；
2. 一次性持久化状态、计数和结束时间；
3. 持久化成功后广播唯一 terminal event；
4. 幂等处理重复完成，禁止终态覆盖；
5. 统一清理 signal、pause channel 和订阅资源。

##### P1：集中终态推导（完成）

1. 将 `RunResult + run error` 归一化为 `RunOutcome`；
2. 空答案且无错误、未中止时返回明确错误；
3. reasoning 截断、循环预算耗尽和 force conclusion 失败必须得到稳定终态；
4. 后台 Memory 提取失败不反向修改已完成的 QA Run。

##### P2：删除重复模型（完成）

确认调用关系后，合并或删除 `internal/domain/agent.go` 中未使用的 `RunRecord` 和 `StepKind`。生命周期模型只保留一个拥有者，不通过别名或 pass-through 类型隐藏重复。

#### 7. 不变量

1. 一个 Run 只能从非终态转换到一个终态一次；
2. 终态 Run 必须有 `ended_at`；
3. `done` 必须包含非空答案；当前 QA 不允许空成功；
4. `aborted` 不能被记录为 `done`；
5. terminal event 只能由完成边界发布一次；
6. SSE 断连不改变 Run 的业务终态；
7. Phase、Step 和 Trace 不能直接修改 RunStatus；
8. pause 的存储语义与 UI、重启行为必须一致；
9. 后台 Summary 和 Memory 不延长同步 Run，也不改变其终态；
10. EvidencePlan 在 Run 内不可变，控制信号不能扩大工具权限。

#### 8. 测试与验收

##### 状态转换

- running 可转换为 done、failed 或 aborted；
- terminal 到任意状态的转换被拒绝或幂等忽略；
- pause/resume 行为与选定的持久或进程内语义一致；
- 服务重启后的遗留 Run 有确定结果。

##### 实时协议

- 每个 Run 只产生一个 terminal event；
- token、reasoning、step 和 trace 都从同一订阅通道到达；
- 慢订阅者丢失临时事件不会删除持久 Run/Step；
- SSE 断连后后台 Run 可按既定产品语义继续或取消，但状态可查询且不悬空。

##### 终态归一化

- 正常答案得到 done；
- Provider、工具和 force conclusion 错误得到 failed；
- 用户 abort 和暂停等待期间取消得到 aborted；
- 空答案不能得到 done；
- Memory 提取失败不改变 done。

#### 9. 验收标准

1. 普通 QA 主链路只有一个可查询 Run 生命周期来源；
2. 实时输出只有一个事件通道和一个 terminal 发布者；
3. 删除 `hubSentDone` 和无写入者的 Token channel；
4. pause 状态在数据库、Hub、UI 和服务重启语义上完全一致，或明确只作为进程内控制而不出现在 RunStatus；
5. 不新增 facet、检索阶段或证据完整度持久状态；
6. `go test ./internal/agent/ ./internal/transport/dashboard/`、`go build ./...` 和相关 race 测试通过。

#### 10. 发布边界

该收敛应分片交付：

1. 先统一 Run terminal outcome 和非法组合校验；
2. 再替换双实时通道；
3. 最后决定并迁移 pause 语义、删除重复类型。

每一片都保持 API 可回滚。不得用新增兼容状态、永久双写或前端兜底掩盖所有权未统一的问题。
