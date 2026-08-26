# Investigation Run 共享预算与大上下文治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-08-25
关联事项：`workflow_af59e5ff74aca4c61df9fbde`、`run_f4553b7a160ea3406fa4fc06`、`docs/design/multi-agent-orchestration-simplification-proposal.zh-CN.md`、`docs/design/evidence-context-budget-and-dedup.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案用于解决 Investigation 多 Agent 工作流中的预算语义混用、子 Agent 被小额累计 token 上限提前终止，以及 Run 总预算随 Agent 数量动态放大的问题。

当前实现同时存在四种容易混淆的“上限”：模型单次上下文窗口、Run 累计预算、Agent 工作量限制和 evidence 投影上限。旧默认值又把 `investigation_max_input_tokens=20000` 用在整个 Run、子 Agent 执行和 evidence 上下文派生等不同位置；另一方面，Run 总预算还会按计划中的 Agent 数量动态相乘。结果是在“先定总账再切给子 Agent”和“先定子 Agent 基数再推导总账”两种模型之间来回切换，两种模型都无法稳定表达真实资源约束。

本提案统一为一个简单模型：

```text
模型上下文能力：限制一次请求最多能携带多少上下文
Run 共享总账：限制整个 Investigation 最多累计消耗多少资源
Agent 工作限制：限制每个角色最多做多少步、调用多少工具、运行多久
Evidence 投影：限制一次角色输入中装入多少有效证据
```

目标执行流程从：

```text
平台默认小预算
→ 按 Agent 数量扩张 Run 总预算
→ 再给子 Agent 注入累计 token/cost 限制
→ 子 Agent 与共享总账重复拦截
→ partial / failed，且错误语义混乱
```

调整为：

```text
Run 创建时冻结一份共享硬上限
→ 子 Agent 只保留工作量限制与单请求上下文限制
→ 每次真实模型调用向共享 ledger 预留
→ 调用结束按实际 usage 结算并释放差额
→ 预算耗尽时按 evidence 完整度返回 partial 或 failed
```

同时建议把当前部署的默认模型上下文提升到 `1_000_000`，把 Run 共享输入默认值提升到 `300_000`，并明确这两个数字不属于同一个维度：前者是单请求能力上限，后者是整个工作流的累计成本上限。

## 2. 背景

### 2.1 业务与技术背景

Investigation 用于回答需要跨代码、服务拓扑、文档、运行证据和外部资料的复杂问题。Coordinator 将问题拆成多个 Task，调用已有 Single-Agent Runtime 执行 Investigator、Verifier 和 Synthesizer，最终生成带证据的回答。

当前链路为：

```text
POST /api/qa/ask
→ Investigation Coordinator 创建 Run 与计划
→ Scheduler 并行调度 Investigator
→ Verifier 校验证据与冲突
→ Synthesizer 生成最终答案
→ 持久化 Run、usage、failure 与 report
```

各模块主要职责如下：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `platform/config/platform.go` | 定义平台默认值、持久化设置键和设置解析 | 平台设置 | `PlatformSettings` |
| `internal/agent/investigation/policy.go` | 把平台设置冻结为 Run 预算策略 | `PlatformSettings` | `BudgetPolicy` |
| `internal/agent/investigation/budget_policy.go` | 定义 profile、阶段分配及 Agent 数量扩张逻辑 | Run 基础预算、Agent 数量 | Run/Stage limit |
| `internal/agent/investigation/budget.go` | 维护共享 ledger、reservation 和 usage settlement | 预算预留与实际 usage | 共享预算快照 |
| `internal/agent/investigation/scheduler.go` | 调度 Task、创建 attempt reservation、注入 RuntimeBudget | Task 与 ledger | Task execution result |
| `internal/agent/investigation/runtime_executor.go` | 将 Workflow Task 转换为 Single-Agent Run | Task contract、definition、共享 gate | 调查报告与 usage |
| `internal/agent/execution/model_call.go` | 发起物理模型调用并登记模型 usage | messages、tools、max tokens | provider result |
| `internal/agent/catalog/defaults_investigation.go` | 定义各角色的步骤、工具、输出和上下文限制 | 平台 Agent/LLM 设置 | Agent Definition |
| `app/investigation.go` | 组装 Coordinator、Executor、Composer 和 evidence budget | 平台运行时 | Investigation service |

### 2.2 当前实现

当前平台默认值为：

```go
DefaultLLMContextWindow             = 128000
DefaultInvestigationMaxInputTokens  = 20000
DefaultInvestigationMaxOutputTokens = 8000
DefaultInvestigationMaxToolCalls    = 24
DefaultInvestigationMaxDuration     = 5 * time.Minute
DefaultInvestigationMaxRounds       = 4
DefaultInvestigationMaxTasks        = 24
DefaultInvestigationMaxParallelism  = 4
DefaultInvestigationMaxCostMicros   = 0
DefaultInvestigationBudgetProfile   = "interactive"
```

相关设置通过平台设置契约持久化，并由以下接口管理：

```text
GET /api/settings
PUT /api/settings
```

当前预算生成与执行还存在以下行为：

1. `BudgetVectorFromPlatformSettings` 读取 `investigation_max_input_tokens`、`investigation_max_tool_calls`、`investigation_max_duration` 和 `investigation_max_cost_micros`；
2. Run output hard limit 名义上来自 `investigation_max_output_tokens`，但只要 `llm_answer_max_tokens > 0`，后者就会覆盖前者；
3. `expandRunLimitForAgents` 按计划 Agent 数量放大 input、output、total、tool calls 和 cost，只保留 duration 不变；
4. Agent Definition 使用 `llm_context_window` 作为单请求上下文限制，使用 `agent_max_steps`、`agent_max_tool_calls`、`agent_timeout` 等作为角色工作上限；
5. 物理模型调用已接入共享 `RunBudgetUsageGate`，调用前预留 input，调用后按 provider usage 结算；
6. evidence 上下文当前以内置 `6000` 为基线，又使用 `investigation_max_input_tokens` 对其截断；
7. 已实施的 P0 修复使 tool-less Verifier 不再继承全局工具额度，并避免共享 RuntimeBudget 再被解释成 child 的 input/total/cost 私有额度；本提案在此基础上统一剩余的配置和总账语义。

### 2.3 为什么现在需要修改

本次提案由 2026-08-25 13:59:00（Asia/Shanghai）的 Investigation 失败触发：

- Workflow：`workflow_af59e5ff74aca4c61df9fbde`；
- Parent Run：`run_f4553b7a160ea3406fa4fc06`；
- `investigate.describe_overall_architecture.code.2`：累计输入 `23960`，超过 `20000`；
- `investigate.describe_overall_architecture.service.1`：累计输入 `33175`，超过 `20000`；
- `evidence.verify`：`run max_tool_calls exceeds the definition budget`；
- 最终结果：workflow `execution_failed`。

直接提高 `20000` 可以缓解这一次失败，但不能解决下面的机制问题：

- 模型支持更大上下文，不代表 Run 应无限累计消费；
- Run 总预算不应由计划 Agent 数量动态决定，否则 replan 会改变硬上限；
- 子 Agent 私有累计 token 额度与共享 ledger 同时存在时，会产生双重限制；
- 单次回答输出上限与整个 Run 输出总额不应共用同一事实源；
- 配置默认值变化不会自动覆盖数据库中已经持久化的旧值。

### 2.4 范围与非目标

#### 目标

1. 明确模型上下文、Run 总账、Agent 工作限制和 evidence 投影四类边界的唯一语义；
2. 让所有 child Agent、retry、replan 和 composition 共享同一个不可扩张的 Run ledger；
3. 删除 child 累计 input/total/cost quota，避免重复计费与提前终止；
4. 把 Run input 默认值从 `20000` 提升到适合复杂调查的初始水平，同时保持成本上限；
5. 让预算耗尽、provider context 不匹配和配置迁移失败均可明确诊断；
6. 保证已有持久化设置通过显式 migration/repair 进入新的 canonical 语义。

#### 非目标

1. 本提案不允许一次请求或一次 Run 自动用满模型宣称的上下文窗口；
2. 本提案不为每个 Investigator 平均分配 token，也不保证所有 Agent 都能完整执行；
3. 本提案不新增按任务类型、关键词或具体问题动态调预算的规则；
4. 本提案不自动探测或静默替换 LLM provider/model；
5. 本提案不重新设计 evidence dedup、引用投影和 Synthesizer 压缩算法，相关机制继续由既有 evidence 设计负责；
6. 本提案不以无限提高 token、并发、重试或超时掩盖检索和规划质量问题。

## 3. 问题

### 3.1 问题描述

**期望行为：**

一次 Investigation 在创建时获得一份明确、不可因 Agent 数量或 replan 扩张的 Run 硬上限。各 Agent 根据职责执行有限步骤，所有物理模型调用按真实 usage 记入同一总账。模型的上下文窗口只限制单次请求，不被当成累计消费额度。

**实际行为：**

当前基础 Run 预算来自平台设置，但执行时又按 Agent 数量放大；Run output 设置会被单次回答设置覆盖；旧的 `20000` input 上限曾继续进入 child Runtime；evidence 投影又依赖同一个字段派生。相同配置项在多个层次承担不同职责。

**差异：**

系统没有一份稳定、可解释的资源契约。用户无法仅通过配置回答“单次请求能多大”“整个工作流能花多少”“一个 Agent 能工作多久”这三个不同问题，日志中的预算错误也无法直接指出是 context、Run ledger 还是角色工作限制触发。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | Investigator 输入超过 `20000`；tool-less Verifier 出现 tool budget 冲突；workflow 最终失败 | 2026-08-25 运行日志 |
| 直接原因 | 小额 Investigation input 被解释为 child 累计限制；共享 tool limit 与 definition tool limit 混用 | `runtime_executor.go`、`scheduler.go` 的预算注入路径 |
| 机制根因一 | 模型能力、Run 消费和 Agent 工作量没有严格分层 | `PlatformSettings` 多组字段在不同层交叉使用 |
| 机制根因二 | `expandRunLimitForAgents` 用 child 数量推导 Run hard limit | `budget_policy.go`、`coordinator.go` |
| 机制根因三 | Run output 的事实源不唯一 | `policy.go` 中 `llm_answer_max_tokens` 覆盖 investigation output |
| 机制根因四 | evidence 投影从 Run 累计 input 字段派生 | `app/investigation.go` 中 `evidenceContextTokens` |

根因链路：

```text
一个字段同时表达多种预算语义
→ Coordinator 按 Agent 数量扩大 Run 总额
→ Scheduler/Runtime 再把预算注入 child
→ child definition 与共享 ledger 重复检查
→ 任一局部限制提前触发
→ partial 或 failed，且难以判断真正耗尽的维度
```

本问题不能只通过把 `20000` 改成 `1000000` 解决。这样做会暂时绕开旧案例，但仍保留动态扩张、output 事实源冲突、replan 扩容和 evidence 配置耦合；同时可能让多 Agent Run 在没有成本保护的情况下累计消耗数百万 token。

### 3.3 影响

- **用户影响：** 复杂架构问题在已有有效证据时仍可能整体失败，或者只能返回不完整回答；
- **业务影响：** Investigation 成功率和成本不可预测，平台设置难以由管理员正确配置；
- **系统影响：** 动态扩张使硬上限失去稳定性，并行请求可能在结算阶段才发现超额；
- **工程影响：** 同一字段被多个层次解释，测试需要围绕实现细节构造预算，后续修改容易再次引入双重限制。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：复杂调查累计输入超过旧 20k

- **Given：** `investigation_max_input_tokens=20000`，计划包含 code、runtime、docs 三个 Investigator；
- **When：** 各 Investigator 分别读取任务上下文、工具结果和 evidence，单个执行累计输入达到 `23960` 或 `33175`；
- **Then：** 如果 Run 共享总账仍有额度，Agent 应继续；如果共享总账耗尽，应按 evidence 完整度受控停止；
- **But：** 旧 child 累计 input 限制先于共享 ledger 报错。

关键证据：

```text
run usage limit exceeded: input tokens 23960 exceed 20000
run usage limit exceeded: input tokens 33175 exceed 20000
```

#### 场景 B：四个 Agent 实际消耗不均衡

- **Given：** Run input hard limit 为 `300000`，四个 Agent 并行执行；
- **When：** Agent A 消耗 `180000`，Agent B/C/D 合计预计还需要 `180000`；
- **Then：** 不提前给四个 Agent 各切 `75000`，而是按物理调用先到先预留；当共享剩余额度不足时拒绝后续调用；
- **But：** 固定均分会让 Agent A 过早失败，按 Agent 数放大总账又会把原本 `300000` 的硬上限变成不稳定值。

这个场景没有“保证四个员工都完成”的技术解法：需求为 `360000`，资源只有 `300000`。正确行为是遵守总账、保留已有证据、明确标记未完成范围，而不是暗中借预算或重新分配出不存在的额度。

#### 场景 C：模型支持 1M，但 Run 不应默认消费 1M

- **Given：** `llm_context_window=1000000`，`investigation_max_input_tokens=300000`；
- **When：** 某次模型调用准备发送 `120000` input tokens；
- **Then：** 该请求同时满足单请求 context 上限和 Run 剩余额度时可以执行；
- **But：** 如果把 context window 当 Run budget，就会允许每个 Agent 都累计消费接近 1M；如果把 Run budget 当 context window，又会错误地把单请求能力限制在 300k。

#### 场景 D：Retry 或 Replan 产生新任务

- **Given：** Run 已使用 `220000/300000` input tokens；
- **When：** Verifier 触发 retry，或 Coordinator 生成新 plan revision；
- **Then：** 新 attempt 继续使用剩余 `80000`，Run hard limit 不变化；
- **But：** 如果再次按新 Agent 数调用扩张逻辑，retry/replan 会凭空获得额外预算。

#### 场景 E：配置的 context 大于 provider 实际能力

- **Given：** 平台设置 `llm_context_window=1000000`，但配置的 provider/model 实际不接受该请求大小；
- **When：** 请求超过 provider 的实际上下文能力；
- **Then：** provider 错误必须原样归类并可观测，提示配置与模型能力不匹配；
- **But：** 不允许静默改用另一个 provider/model，也不允许把失败伪装成空结果或普通预算耗尽。

#### 场景 F：Tool-less Verifier

- **Given：** Verifier definition 没有可见工具，`MaxToolCalls=0`；
- **When：** Run 共享 tool limit 为 `48`；
- **Then：** Verifier 仍保持 `0`，共享 Run tool limit 不应被注入为 Verifier 的角色工具额度；
- **But：** 如果把 Run limit 直接覆盖 definition，会出现 `run max_tool_calls exceeds the definition budget`。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 多 Agent 总 usage 未超过 Run limit | 可能受 child token cap 干扰 | 所有调用统一由 shared ledger 结算 |
| 单请求过大 | input 超过 `llm_context_window` | 可能只在 provider 处失败 | 调用前 context 校验；能力不匹配错误可见 |
| Run 预算耗尽且有证据 | 后续 reservation 不可满足 | task/workflow 可能直接 failed | task 标记 partial，进入受控验证或合成 |
| Run 预算耗尽且无证据 | 无 admissible evidence | 可能产生空 partial | 明确 `budget_exhausted` failure |
| Provider usage 缺失 | provider 未返回 usage | reservation 被释放 | 记录 usage unavailable；不得伪造精确成本 |
| Retry/Replan | 新 attempt 或 plan revision | 可能重新扩张总额 | 共享原 ledger，不重置、不扩张 |
| 并发预留 | 多 Agent 同时调用模型 | 结算时可能竞争额度 | 原子 reservation，不能 overbook |
| 持久化旧配置 | 数据库已有 `20000/8000` | 新默认值不生效 | 显式 migration/repair 更新 canonical 值 |

## 5. 如何修改

### 5.1 修改原则

1. **四类限制分层。** Context capability、Run shared budget、Agent work limits、evidence projection 各自只有一个含义；
2. **Run ledger 是累计资源唯一事实源。** child Agent、retry、replan、Verifier 和 Synthesizer 都不能创建第二份 token/cost 总账；
3. **不预分配 child token。** 子 Agent 只获得工作量限制和单请求 context ceiling；
4. **硬上限在 Run 创建时冻结。** 执行中的任务数量、并发度、retry 和 replan 都不能放大上限；
5. **调用前预留，调用后结算。** 并发 Agent 不能只在 provider 返回后才争抢剩余额度；
6. **配置在 ingress 规范化。** 已持久化旧值通过显式 migration/repair 修正，不保留长期运行时 fallback；
7. **失败必须说明耗尽的边界。** context、input、output、tool、duration、cost 和 provider failure 使用不同分类和日志字段；
8. **不增加不必要配置。** evidence projection 暂时保留内部稳定边界，待指标证明需要管理员调节后再公开。

### 5.2 目标预算模型

#### 5.2.1 模型上下文能力

唯一配置：

```text
llm_context_window
```

语义：一次 provider request 中，system prompt、messages、tools schema、evidence 和预留输出共同占用的最大上下文能力。

约束：

```text
estimated_input_tokens + requested_max_output_tokens <= llm_context_window
```

该值不累计，不代表一次 Run 可以消费的总 token，也不按 Agent 数量相乘。

#### 5.2.2 Run 共享硬上限

唯一配置组：

```text
investigation_max_input_tokens
investigation_max_output_tokens
investigation_max_tool_calls
investigation_max_duration
investigation_max_cost_micros
```

语义：从 Run 创建到最终结束，所有 child Agent、所有 attempt、retry、replan、verification 和 composition 的累计上限。

`investigation_max_output_tokens` 成为 Run output hard limit 的唯一事实源。`llm_answer_max_tokens` 仅控制 Single-Agent/角色一次回答允许请求的最大输出，不再覆盖 Run 总输出预算。

#### 5.2.3 Agent 工作限制

继续使用：

```text
agent_max_steps
agent_max_tool_calls
agent_timeout
llm_max_continue_rounds
Agent Definition.Model.MaxOutputTokens
```

语义：限制某个角色能进行多少轮工作、能否调用工具、单次最长运行多久，以及单次生成输出的形状。它们不是 Run token 配额。

具体规则：

- `MaxSteps` 保持角色级上限；
- `MaxToolCalls` 同时受 definition 和 Run 剩余额度约束，实际可用值为两者较小者；
- 没有工具的 definition 必须保持 `MaxToolCalls=0`；
- `Model.MaxOutputTokens` 必须同时满足角色输出上限、context 剩余空间和 Run output 剩余额度；
- child Runtime 不再接收累计 input/total/cost limit。

#### 5.2.4 Evidence 投影

EvidenceContextBudget 继续限制一次角色输入中装入的 evidence summary、context 和 bundle 大小，但不再从 `investigation_max_input_tokens` 派生。

第一阶段保留代码级稳定上限：

```text
MaxSummaryTokens = 256
MaxContextTokens = 6000
MaxBundleTokens  = 8000
```

这些值是输入投影边界，不是 child token quota。若后续观测证明不同模型需要不同投影，再单独提出配置化方案，避免本次引入更多预算旋钮。

### 5.3 默认值调整

建议目标默认值：

```go
DefaultLLMContextWindow             = 1_000_000
DefaultInvestigationMaxInputTokens  = 300_000
DefaultInvestigationMaxOutputTokens = 16_000
DefaultInvestigationMaxToolCalls    = 48
DefaultInvestigationMaxDuration     = 10 * time.Minute
DefaultInvestigationMaxRounds       = 6
DefaultInvestigationMaxTasks        = 32
DefaultInvestigationMaxParallelism  = 4
DefaultInvestigationMaxCostMicros   = 0
DefaultInvestigationBudgetProfile   = "interactive"
```

解释如下：

| 配置 | 建议默认 | 解释 |
| --- | ---: | --- |
| `llm_context_window` | `1_000_000` | 当前部署目标模型的单请求能力配置；不是 Run 总预算 |
| `investigation_max_input_tokens` | `300_000` | 整个 workflow 的累计输入硬上限，是旧值的 15 倍，但仍明显低于“每个 Agent 都用满 1M” |
| `investigation_max_output_tokens` | `16_000` | 所有角色累计输出上限；与单次回答上限分离 |
| `investigation_max_tool_calls` | `48` | 共享工具调用总额；不改变 tool-less role |
| `investigation_max_duration` | `10m` | Run wall-clock deadline；不随并行 Agent 数相乘 |
| `investigation_max_rounds` | `6` | 控制 replan/convergence 次数，不授予新 token |
| `investigation_max_tasks` | `32` | 控制计划规模，不用于推导 token 总额 |
| `investigation_max_parallelism` | `4` | 保持不变，避免同时放大 provider rate-limit 压力和在途成本 |
| `investigation_max_cost_micros` | `0` | 暂时表示不启用 cost ceiling；只有输入/输出价格均配置时才具备可靠结算基础 |

这些是初始默认值，不是模型能力的行业常数。若业务确实需要单次输入超过 `300000`，管理员应显式提高 Run input hard limit；不能仅因为模型 context 为 1M 就默认允许每个 Run 或每个 Agent 消费 1M。

### 5.4 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| Run hard limit | 按 Agent 数量动态放大 | Run 创建时冻结，后续不扩张 | `budget_policy.go`、`coordinator.go` | 删除动态扩张调用与对应测试 |
| Child token budget | task/runtime 可能携带累计 token/cost grant | child 不持有累计 input/total/cost quota | `scheduler.go`、`runtime_executor.go` | 以 shared gate 为唯一累计账本 |
| 物理调用 reservation | 主要预留 estimated input | 预留 input、requested max output 和可计算的 cost 上界 | `execution/model_call.go`、`investigation/budget.go` | 结算实际 usage 并释放差额 |
| Run output 来源 | `llm_answer_max_tokens` 覆盖 investigation setting | 只读取 `investigation_max_output_tokens` | `policy.go` | 显式迁移旧持久化值 |
| Context 能力 | 默认 `128000` | 当前部署默认 `1000000` | `platform/config/platform.go` | provider 不匹配时明确失败 |
| Evidence context | 从 Run input 上限截断 | 使用独立内部投影边界 | `app/investigation.go` | 不新增平台设置 |
| 失败分类 | 多种限制可能都表现为 usage limit | 标明 boundary、dimension、limit、used、reserved、requested | budget/execution/runner logs | 保留稳定 failure code，丰富 detail |
| 默认 Run 值 | `20k/8k/24/5m/4/24` | `300k/16k/48/10m/6/32` | platform defaults | 数据库旧值单独 repair |

#### 改动一：删除按 Agent 数量扩张 Run limit

**方案：**

- Coordinator 创建 Run 时调用 `BudgetPolicyFromPlatformSettings`，一次性冻结 hard limit；
- `expandRunLimitForAgents` 不再参与 token、tool、cost 或 total hard limit 计算；
- Agent 数量只用于计划合法性、并发调度和任务数量检查；
- profile 继续用于非 token 的阶段控制或 composition protection，但不能扩大 Run hard limit；
- retry、replan 和 resume 必须复用已持久化的 ledger snapshot。

**失败行为：**

- 新计划需要的预算超过剩余额度时，不创建额外预算；
- 已有 admissible evidence 时，未完成 Task 标记 partial；
- 无 admissible evidence 时，Run 返回明确的 budget exhausted failure。

#### 改动二：物理模型调用统一预留和结算

**方案：**

每次 provider 调用前计算：

```text
estimated input
requested max output
estimated maximum cost（仅价格可用时）
```

然后通过同一个 `RunBudgetUsageGate.ReserveCall` 原子预留。调用成功后以 provider reported usage 结算，释放未使用的 output/cost 预留；调用失败且没有 usage 时释放预留并记录 provider failure；provider 返回 usage 时即使调用同时报错，也要结算已发生的消费。

**约束：**

- reservation 必须防止并发 overbook；
- child 不能持有第二份累计 token/cost ledger；
- 预留失败时不得发起 provider 请求；
- provider usage 缺失时不得伪造精确 usage 或 cost。

#### 改动三：归一 Run output 配置

**方案：**

- `BudgetVectorFromPlatformSettings` 只读取 `InvestigationMaxOutputTokens`；
- `LLMAnswerMaxTokens` 继续服务 Agent Definition 的单次输出上限；
- 在设置写入/迁移边界把已有环境的 canonical `investigation_max_output_tokens` 补齐；
- 删除长期运行时 fallback，避免同一字段永久保持两种解释。

#### 改动四：保持角色限制，不再当作资金分配

**方案：**

- Investigator 保持有限 `MaxSteps`、合法工具 allowlist 和角色输出上限；
- Verifier/Synthesizer 保持 convergence step 限制；
- tool-less role 保持 tool calls 为零；
- Agent runtime 每次调用可用 output 为：

```text
min(role max output, context available output, run remaining output)
```

这属于调用准入，不是提前给角色分配累计份额。

### 5.5 数据结构或接口契约

本提案不新增公共 API 字段，重新定义现有字段的 canonical 语义：

| 字段 | 类型 | 所有者 | canonical 含义 | 建议默认 | 兼容性 |
| --- | --- | --- | --- | ---: | --- |
| `llm_context_window` | `int` | LLM/Agent 配置 | 单次 provider request 上下文能力 | `1000000` | 旧持久化值显式更新 |
| `investigation_max_input_tokens` | `int64` | Investigation Coordinator | Run 累计 input hard limit | `300000` | 旧值显式 repair |
| `investigation_max_output_tokens` | `int64` | Investigation Coordinator | Run 累计 output hard limit | `16000` | 不再被 answer setting 覆盖 |
| `investigation_max_tool_calls` | `int64` | Investigation ledger | Run 累计 tool calls hard limit | `48` | definition 可进一步收紧 |
| `investigation_max_duration` | `duration` | Investigation Coordinator | Run wall-clock hard deadline | `10m` | 不随 Agent 数相乘 |
| `investigation_max_cost_micros` | `int64` | Investigation ledger | Run 累计 cost hard limit | `0` | `0` 继续表示未启用 |
| `llm_answer_max_tokens` | `int` | Single-Agent/Definition | 单次回答或角色输出基线 | 保持现有 | 不再表达 Run 总输出 |

不变量：

1. Run hard limit 在 Run 创建后不可因 AgentCount、retry、replan 或 resume 增加；
2. 任一物理模型调用必须属于且只属于一个共享 Run ledger；
3. child Agent 不持有累计 input/total/cost quota；
4. definition 的工具权限不能被 Run 共享 tool limit 放宽；
5. `estimated_input + requested_output` 不得超过 `llm_context_window`；
6. Run 最终 usage 等于已结算物理调用和工具调用 usage 的聚合，不按 Task grant 求和；
7. configured provider failure 不触发 provider substitution。

### 5.6 兼容、迁移与回滚

- **向后兼容：** REST 设置键保持不变；不新增永久 fallback；运行中的 Run 使用创建时已冻结的 snapshot，新语义只应用于新 Run；
- **数据迁移：** 发布前通过 `PUT /api/settings` 或一次性 repair 更新已有平台设置。至少检查并显式设置 `llm_context_window`、所有 `investigation_max_*` 和价格字段；
- **输出迁移：** 若历史环境依赖 `llm_answer_max_tokens` 作为 Run output，应先把其期望的 Run 总额写入 `investigation_max_output_tokens`，再上线删除覆盖逻辑；
- **灰度方式：** 先在 QA/测试环境启用新默认和新 ledger 语义，再按环境发布；不建议增加长期 feature flag；
- **回滚条件：** provider context error 明显增加、Run budget failure 异常增加、usage ledger 出现负值/重复结算、P95 延迟或成本超过上线阈值；
- **回滚步骤：** 回滚代码版本并恢复先前平台设置值。已完成的设置迁移应使用保存的迁移前快照显式恢复，不依赖运行时猜测旧语义。

## 6. 修改伪代码

### 6.1 Run 创建与共享预算冻结

```go
func NewInvestigationRun(settings PlatformSettings, contract Contract) (*Run, error) {
    policy, err := BudgetPolicyFromPlatformSettings(settings)
    if err != nil {
        return nil, fmt.Errorf("resolve investigation budget policy: %w", err)
    }

    // Hard limit is frozen once. Agent count never multiplies it.
    ledger := NewBudgetLedger(policy.Limit)
    ledger.SetStageLimits(AllocateNonTokenStageControls(policy))

    return &Run{
        BudgetPolicy: policy,
        Ledger:       ledger,
        Contract:     contract,
    }, nil
}
```

删除：

```go
agentCount := countAgentTasks(tasks)
expanded := expandRunLimitForAgents(base, agentCount)
ledger.SetRunLimit(expanded)
```

### 6.2 Child Agent 执行

```go
func ExecuteAgentTask(ctx context.Context, task Task, run *Run) (Result, error) {
    definition := catalog.Get(task.AgentID)

    runtimePolicy := RuntimePolicy{
        MaxContextTokens: definition.Budget.ContextTokens,
        MaxSteps:         definition.Budget.MaxSteps,
        MaxToolCalls:     minDefinitionAndRunTools(definition, run.Ledger),
        Timeout:          minDuration(definition.Budget.Timeout, run.RemainingDuration()),
        MaxOutputTokens:  definition.Model.MaxOutputTokens,

        // No child cumulative token or cost quota.
        MaxInputTokens: 0,
        MaxTotalTokens: 0,
        MaxCostMicros:  0,
    }

    childCtx := WithRunBudgetUsageGate(ctx, run.Ledger.ForTask(task.ID))
    return runtime.Run(childCtx, definition, runtimePolicy, task.Input)
}
```

### 6.3 物理模型调用准入与结算

```go
func CallModel(
    ctx context.Context,
    messages []Message,
    tools []Tool,
    requestedMaxOutput int,
) (*Result, error) {
    inputTokens := EstimateInputTokens(messages, tools)
    outputTokens := ClampOutput(
        requestedMaxOutput,
        ContextRemaining(inputTokens),
        RunOutputRemaining(ctx),
    )
    if outputTokens <= 0 {
        return nil, ErrBudgetExceeded
    }
    if inputTokens+outputTokens > ModelContextWindow(ctx) {
        return nil, ErrContextWindowExceeded
    }

    estimate := Usage{
        InputTokens:  int64(inputTokens),
        OutputTokens: int64(outputTokens),
        CostMicros: EstimateMaxCost(inputTokens, outputTokens),
    }
    reservation, err := RunBudgetUsageGate(ctx).ReserveCall(estimate)
    if err != nil {
        return nil, fmt.Errorf("reserve shared investigation call budget: %w", err)
    }

    result, callErr := provider.Chat(ctx, messages, tools, outputTokens)
    if result != nil && result.HasUsage() {
        settleErr := reservation.Settle(result.Usage)
        return result, errors.Join(callErr, settleErr)
    }

    releaseErr := reservation.Release()
    return result, errors.Join(callErr, releaseErr)
}
```

### 6.4 预算耗尽的结果语义

```go
func HandleBudgetExhausted(task Task, evidence []EvidenceUnit, err error) TaskResult {
    if HasAdmissibleEvidence(evidence) {
        return TaskResult{
            Status:   Partial,
            Evidence: evidence,
            Failure: RunFailure{
                Code:      FailureBudget,
                Boundary:  "run_shared_ledger",
                Dimension: ExhaustedDimension(err),
                Message:   err.Error(),
            },
        }
    }

    return TaskResult{
        Status: Failed,
        Failure: RunFailure{
            Code:      FailureBudget,
            Boundary:  "run_shared_ledger",
            Dimension: ExhaustedDimension(err),
            Message:   err.Error(),
        },
    }
}
```

### 6.5 配置归一

```go
func BudgetVectorFromPlatformSettings(settings PlatformSettings) BudgetVector {
    return BudgetVector{
        InputTokens:  settings.InvestigationMaxInputTokens,
        OutputTokens: settings.InvestigationMaxOutputTokens,
        ToolCalls:    int(settings.InvestigationMaxToolCalls),
        Duration:     time.Duration(settings.InvestigationMaxDuration),
        CostMicros:   settings.InvestigationMaxCostMicros,
    }
}
```

一次性设置 repair 示例：

```json
{
  "llm_context_window": "1000000",
  "investigation_max_input_tokens": "300000",
  "investigation_max_output_tokens": "16000",
  "investigation_max_tool_calls": "48",
  "investigation_max_duration": "10m",
  "investigation_max_rounds": "6",
  "investigation_max_tasks": "32",
  "investigation_max_parallelism": "4",
  "investigation_max_cost_micros": "0"
}
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 复杂 Investigation 不再因为固定 `20000` child input quota 提前终止；
2. Run 总预算不再因计划 Agent 数量、retry 或 replan 自动扩大；
3. 一个 Agent 可以按实际需要使用较多预算，只要共享 ledger 仍有余额；
4. 当一个 Agent 消耗大部分预算时，后续 Agent 会在调用前被明确拒绝，而不是结算后才形成不可解释的超额；
5. 模型 1M context 能力可用于承载更大的单请求上下文，但不会自动变成每个 Agent 的消费额度；
6. tool-less Verifier 保持无工具权限，不再受共享 tool limit 错误覆盖；
7. Run output hard limit 与单次回答 output limit 可独立配置和解释；
8. 有证据的预算耗尽形成 partial，无证据的预算耗尽形成 failed，不返回伪成功。

### 7.2 可观测性效果

预算相关日志与持久化 detail 至少包含：

| 字段 | 目标 |
| --- | --- |
| `workflow_id`、`run_id`、`task_id`、`attempt` | 定位消费所属执行单元 |
| `budget_boundary` | 区分 `model_context`、`run_shared_ledger`、`agent_work_limit`、`evidence_projection` |
| `budget_dimension` | 区分 input、output、tool_calls、duration、cost |
| `limit`、`used`、`reserved`、`requested`、`actual` | 解释为何允许或拒绝调用 |
| `provider_usage_reported` | 识别 usage 是否可靠 |
| `status`、`completeness`、`partial_reason` | 区分资源停止与业务完成度 |
| `policy_version`、`budget_profile` | 追溯 Run 创建时的冻结策略 |

日志应能够直接回答：

- 是单次 context 不够，还是整个 Run 总账耗尽；
- 哪个 Agent/attempt 消耗了多少；
- retry/replan 是否复用了原总账；
- provider 请求是否在发出前被拒绝；
- partial 是否保留了可准入 evidence；
- 当前平台设置是否仍为迁移前旧值。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 共享预算下 child input/total/cost 私有额度触发次数 | 已出现 | `0` | 每次发布回归 + 7 天 | failure logs |
| tool-less role 因 run tool limit 冲突失败次数 | 已出现 | `0` | 每次发布回归 + 7 天 | failure logs |
| retry/replan 后 Run hard limit 增长次数 | 未独立统计 | `0` | 自动化测试 + 7 天 | budget snapshots |
| 调用前 reservation 导致的并发 overbook | 存在风险 | `0` | 并发测试 + 7 天 | ledger invariant metric |
| usage 已知时 Run 结算账实差异 | 未建立 | `0` | 自动化测试 + 7 天 | provider usage / ledger snapshot |
| 原触发 workflow 同类问题的成功或受控 partial 比例 | 失败 | `100%` 非错误崩溃 | 回归集 | QA eval |
| P95 Run input usage | 待采集 | 低于 hard limit 且可解释 | 14 天 | usage metrics |
| P95 延迟与成本 | 待采集 | 不超过评审确认阈值 | 14 天 | latency/cost metrics |

### 7.4 不应发生的变化

- Agent 的工具 allowlist、权限 scope 和只读边界保持不变；
- 并发度默认保持 `4`，不因 context 扩大而增加；
- configured provider 失败不得静默切换 provider；
- EvidenceContextBudget 不因 Run input 提升到 `300000` 而自动放大；
- 不新增针对“整体架构”等具体问题词汇的特殊预算；
- 不承诺在预算不足时所有 child Agent 都能完成；
- 不允许默认值变更绕过已持久化平台设置的显式迁移。

## 8. 测试与验收

### 8.1 单元测试

- `BudgetVectorFromPlatformSettings` 只使用 `InvestigationMaxOutputTokens`；
- 计划 Agent 数从 1 增加到 4 时，Run hard limit 保持不变；
- retry、replan 和 resume 复用同一 ledger usage；
- child RuntimeBudget 不含累计 input/total/cost quota；
- tool-less definition 在 Run tool limit 为正数时仍保持 `MaxToolCalls=0`；
- 有工具的 definition 实际 tool admission 不超过 definition 与 Run 剩余额度的较小值；
- 物理调用预留 estimated input、requested max output 和可计算 cost；
- 并发 reservation 不能使 `used + reserved` 超过 hard limit；
- settle 使用实际 usage 并释放预留差额；
- provider 调用失败但返回 usage 时仍结算；无 usage 时释放并记录 unavailable；
- context 检查使用 `input + requested output`，不使用 Run 累计 input 代替；
- 有 evidence 的预算耗尽返回 partial，无 evidence 返回 failed；
- evidence context 不再随 `InvestigationMaxInputTokens` 变化。

### 8.2 集成测试

- 使用四个并行 Agent 模拟不均衡 usage，验证不提前均分 token；
- 让第一个 Agent 消耗大部分 Run budget，验证后续调用在 provider 前被拒绝；
- 触发 retry 和 replan，验证 ledger limit 不增长且 usage 连续；
- 运行 Verifier/Synthesizer，验证它们与 Investigator 共享总账；
- 模拟 provider context error，验证不切换 provider且错误分类为 model context/provider capability；
- 更新平台设置后重建 QA runtime，验证新 Run 使用新 snapshot，已有 Run 保持原 snapshot；
- 模拟数据库保留旧值，验证迁移前默认值不覆盖持久化配置，迁移后新值生效。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 多 facet 整体架构调查 | 不再出现 child `23960/33175 > 20000`；结果为 success 或有证据 partial | 固定回归 fixture |
| 四 Agent 总账 | 预算 300k，需求 360k | 不均分；总账耗尽后受控停止，不扩容 | 并发 ledger test |
| 单请求大上下文 | context 1M，单次输入 120k | context 与 Run 余额均允许时执行 | model admission test |
| Provider 能力不匹配 | 配置 1M，provider 拒绝 | 错误可见，不切换 provider | fake provider test |
| Retry/Replan | 已使用 220k 后新增任务 | 只剩 80k，不产生新总账 | coordinator test |
| Tool-less Verifier | Run tool limit 48 | definition tool limit 仍为 0 | runtime executor test |
| 旧 output 设置 | answer 12k，investigation 16k | Run hard limit 为 16k，角色单次上限仍按 definition | policy/catalog test |

### 8.4 验收标准

提案实施完成必须同时满足：

1. `expandRunLimitForAgents` 不再影响 Run token/tool/cost hard limit；
2. child Agent 不再持有累计 input/total/cost quota；
3. 每次物理模型调用通过共享 gate 完成预留、结算或释放；
4. `investigation_max_output_tokens` 成为 Run output 唯一事实源；
5. context、Run budget、Agent work limit 和 evidence projection 可从日志中明确区分；
6. 原始失败案例不再以 child `20000` token limit 或 tool-less verifier 冲突失败；
7. `GOWORK=off go test` 覆盖相关 package，`GOWORK=off go build ./...` 和 `GOWORK=off go vet ./...` 通过；
8. 已有平台持久化设置完成显式迁移并留有迁移前快照；
9. 灰度期没有出现 ledger 超卖、负数 usage、重复结算或 provider 静默替换。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 默认 input 提升导致成本上升 | 复杂 Run 使用更多轮次 | 单 Run 成本增加 | 共享 hard limit、usage 指标、必要时配置 cost ceiling | P95 成本超过评审阈值 |
| 1M context 与 provider 不匹配 | 部署切换到较小 context 模型 | provider request 失败 | 平台设置与模型同步；失败可见；不静默替换 | context error 激增 |
| 预留 max output 降低并发利用率 | 多并发调用都预留最大输出 | 一些调用提前被拒绝 | 角色 output 上限保持有界；结算立即释放差额 | partial 比例异常升高 |
| 删除动态扩张后深度调查更早耗尽 | 旧行为依赖 Agent 数量扩容 | partial 增加 | 上线前按真实 usage 选择 Run 默认值；允许管理员显式提高 hard limit | 正常回归大量失败 |
| output 语义迁移遗漏 | 数据库仍保留旧 investigation output | Run output 与预期不一致 | 发布前 migration checklist 与设置快照校验 | 新 Run output 异常 |
| Provider usage 缺失 | 兼容 provider 不返回 usage | 成本统计不完整 | 标记 usage unavailable；不能宣称精确结算 | 无法满足成本治理要求 |
| 共享先到先得导致后期合成缺额 | Investigator 消耗过多 | Verifier/Synthesizer 无法完成 | 保持有界角色输出；可使用既有 composition protection，但不得新增 token 总额 | 最终答案生成率下降 |

## 10. 实施计划

### 阶段 1：冻结语义与迁移准备

- 记录上线前平台设置快照；
- 明确当前 provider/model 的 context 配置责任；
- 补充四类边界的配置文档与日志字段；
- 为 output 事实源迁移增加测试；
- 退出条件：迁移值、回滚值和验收基线均已确认。

### 阶段 2：统一 Run ledger

- 移除 Coordinator 按 Agent 数扩张 Run limit 的路径；
- 保留并完善已实施的 child input/total/cost quota 清理；
- 让物理模型调用预留 input、max output 和可计算 cost；
- 统一 retry/replan/resume 的 ledger 复用；
- 退出条件：预算、并发、结算和失败语义单元测试通过。

### 阶段 3：配置归一与默认值调整

- 让 `investigation_max_output_tokens` 成为 Run output 唯一来源；
- 将代码默认值调整为本提案建议值；
- 解除 evidence projection 对 Run input 字段的派生依赖；
- 对已持久化平台设置执行显式 migration/repair；
- 退出条件：新旧设置场景和 runtime reload 集成测试通过。

### 阶段 4：灰度与观测

- 在 QA/测试环境运行原始案例、四 Agent 不均衡案例和 retry/replan 案例；
- 观察 7～14 天 usage、partial、context error、P95 延迟和成本；
- 根据实测数据调整平台设置值，而不是改变预算模型；
- 退出条件：无 ledger invariant 违规，成功率、延迟和成本满足评审阈值。

### 阶段 5：清理旧语义

- 删除 `expandRunLimitForAgents` 及仅服务旧语义的测试；
- 删除 `llm_answer_max_tokens` 覆盖 Run output 的 fallback；
- 更新历史提案中的过时预算说明并添加指向本提案的注记；
- 退出条件：代码、配置文档、平台 UI 和运维手册使用同一术语。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 默认 context | 保持 128k | 当前部署默认 1M | B | 与目标模型能力一致，但必须明确不是 Run 预算 |
| 默认 Run input | 1M | 300k | B | 支持复杂调查，同时避免每个 Run 默认用满模型能力 |
| child token 分配 | 固定或按比例切分 | 不分配，统一 shared ledger | B | 避免双重限制与预算碎片 |
| Run 总额生成 | 按 Agent 数相乘 | Run 创建时冻结 | B | hard limit 必须稳定且可审计 |
| Run output 来源 | 复用 answer max | 独立 investigation output | B | 单次输出与累计输出语义不同 |
| Evidence budget | 立即增加平台配置 | 暂保留内部稳定边界 | B | 减少配置复杂度，先以观测证明需求 |
| Cost ceiling | 默认给固定金额 | 保持 0，价格完整后显式启用 | B | 没有可靠价格时固定 cost 上限会产生伪精度 |
| Profile 是否扩容 | deep 自动乘 Agent 数 | profile 不改变 hard limit | B | profile 只能改变策略，不能创造新预算 |

## 12. 决策摘要

本提案建议：

1. 将 `llm_context_window` 定义为单次模型调用能力，将 `investigation_max_*` 定义为整个 Run 的累计共享硬上限；
2. child Agent 不再获得固定或按比例切分的累计 token/cost 配额，只保留步骤、工具、timeout、单次输出和 context 限制；
3. Run hard limit 在创建时冻结，不按 Agent 数、retry、replan 或 resume 扩张；
4. 每次物理模型调用通过共享 ledger 预留 input、requested output 和可计算 cost，调用后按实际 usage 结算；
5. `investigation_max_output_tokens` 成为 Run output 唯一事实源，`llm_answer_max_tokens` 只负责单次回答；
6. 默认值调整为 context `1M`、Run input `300k`、Run output `16k`、tool calls `48`、duration `10m`、rounds `6`、tasks `32`、parallelism `4`；
7. EvidenceContextBudget 保持独立内部边界，不因模型 context 或 Run input 增大而自动膨胀；
8. 预算不足时不承诺所有 Agent 完成：有 evidence 返回 partial，无 evidence 返回 failed，且必须标明耗尽维度；
9. 已持久化平台设置通过显式 migration/repair 更新，不保留长期兼容 fallback；
10. 通过原始案例、并发 reservation、retry/replan、provider context mismatch 和配置迁移测试验证效果。

## 附录 A：配置位置与职责速查

| 配置 | 设置位置 | 代码默认位置 | 生效时机 | 负责的问题 |
| --- | --- | --- | --- | --- |
| `llm_context_window` | `PUT /api/settings` / 平台设置 UI | `platform/config/platform.go` | QA runtime rebuild 后的新调用 | 单请求最多能装多少上下文 |
| `investigation_max_input_tokens` | 同上 | 同上 | 新 Investigation Run 创建时冻结 | 整个 Run 最多累计多少 input |
| `investigation_max_output_tokens` | 同上 | 同上 | 新 Run 创建时冻结 | 整个 Run 最多累计多少 output |
| `investigation_max_tool_calls` | 同上 | 同上 | 新 Run 创建时冻结 | 整个 Run 最多调用多少工具 |
| `investigation_max_duration` | 同上 | 同上 | 新 Run 创建时冻结 | 整个 Run 最长 wall-clock 时间 |
| `investigation_max_rounds` | 同上 | 同上 | 新 Run 创建时冻结 | 最多多少轮规划/收敛 |
| `investigation_max_tasks` | 同上 | 同上 | 新 Run 创建时冻结 | 最多生成多少 Task |
| `investigation_max_parallelism` | 同上 | 同上 | runtime rebuild/新 Run | 同时运行多少 Task |
| `investigation_max_cost_micros` | 同上 | 同上 | 新 Run 创建时冻结 | 整个 Run 的金额硬上限 |
| `agent_max_steps` | 同上 | 同上 | definition rebuild 后 | 单个角色最多推理/行动多少步 |
| `agent_max_tool_calls` | 同上 | 同上 | definition rebuild 后 | 有工具角色的单角色工具上限 |
| `agent_timeout` | 同上 | 同上 | definition rebuild 后 | 单个角色最长执行时间 |
| `llm_answer_max_tokens` | 同上 | 同上 | definition rebuild 后 | 单次回答/角色输出基线 |

## 附录 B：提案提交前检查清单

- [x] 背景足以让非原作者理解系统和改动动机；
- [x] 问题以“期望行为—实际行为—差异”描述；
- [x] 包含可复现的典型场景和原始日志证据；
- [x] 已区分表面现象、直接原因和机制根因；
- [x] 修改方案明确了职责所有者和单一事实源；
- [x] 伪代码覆盖正常路径、失败路径和状态变化；
- [x] 预期效果包含可量化指标；
- [x] 已说明兼容、迁移、灰度和回滚方案；
- [x] 测试覆盖原始触发案例和关键边界场景；
- [x] 未引入只针对单个案例的硬编码特例。
