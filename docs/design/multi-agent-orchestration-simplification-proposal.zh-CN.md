# 多 Agent 编排复用 Single-Agent Runtime 简化提案

状态：实施中（P0/P1 已完成，性能关键路径优化已落地）
日期：2026-08-23
最近更新：2026-09-03
适用范围：Nasuta Investigation / QA 多 Agent Workflow

## 1. 提案摘要

Nasuta 的“多 Agent”不是一种新的 Agent，也不是要重新实现一套 Multi-Agent Runtime。
它的本质是：

> **编排多个已经存在的 Single-Agent Run，并让它们按照 Workflow 依赖关系协作。**

因此，本提案建议：

1. Investigator、Verifier、Composer 均复用现有 Single-Agent Runtime；
2. Multi-Agent Coordinator 只负责 Workflow 编排，不重复实现模型循环、工具调用、Step、Retry 和 Replan；
3. 整个 Investigation Run 只维护一个共享总 token 预算；
4. 不再提前给每个子 Agent 平均切分固定 token 预算；
5. 通过 DAG 依赖和并行配置决定 Agent 的执行顺序；
6. Investigator 输出 Workflow Artifact，Verifier 消费 Artifact，Composer 最后输出用户答案；
7. Retry 和 Replan 沿用当前 Single-Agent / Investigation 的既有机制，并且所有消耗都计入同一个 Run 总预算。

核心目标是消除当前两套预算和执行语义之间的冲突：

```text
现状：Single-Agent Runtime + Multi-Agent 专用预算/Step 逻辑
提案：Single-Agent Runtime + Coordinator 编排 + Run 级共享预算
```

---

## 2. 背景与当前问题

### 2.1 当前系统已有 Single-Agent Runtime

现有 Single-Agent Runtime 已经负责：

- 调用模型；
- 判断是否需要工具；
- 执行工具调用；
- 将工具结果回填上下文；
- 进入下一轮模型交互；
- `MaxSteps` 控制最大模型交互轮数；
- `MaxToolCalls` 控制工具调用次数；
- `MaxContinueRounds` 控制截断后的继续生成；
- 生成最终答案或结构化结果；
- 记录模型和工具 usage。

相关实现主要位于：

```text
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/loop_turn.go
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/loop_execution.go
```

### 2.2 当前 Multi-Agent 层重复引入了预算语义

当前 Investigation 层另外引入了：

- Run 总预算；
- Planning / Execution / Verification / Composition / Fallback Stage 预算；
- `AgentBudgetPool`；
- 所有 Agent Task 的平均预算分配；
- Task Reservation 和 Settlement；
- Task 预算和 Step 之间容易被误解的关系。

相关实现主要位于：

```text
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/investigation/budget.go
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/investigation/budget_policy.go
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/investigation/planning.go
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/investigation/scheduler.go
```

由此产生的问题包括：

```text
单次模型调用上限 = 12000
某个 Agent Task 总预算可能 = 1400
同一个 Investigator 最多运行 3 Steps
多个 Agent 还要继续从 StageExecution 中平均抢预算
```

最终容易出现：

```text
investigator 任务中途耗尽预算
    ↓
Settlement 发现实际消耗超过 Reservation
    ↓
investigator 失败
    ↓
Verifier 依赖失败并 blocked
```

### 2.3 当前多 Agent 的正确概念

当前系统应被理解为：

```text
Investigation Coordinator
    ├── 调用已有 Single-Agent：investigator.code
    ├── 调用已有 Single-Agent：investigator.runtime
    ├── 调用已有 Single-Agent：investigator.docs
    ├── 调用已有 Single-Agent：evidence.verify
    └── 调用已有 Single-Agent：synthesizer
```

不是：

```text
Multi-Agent Runtime
    └── 再实现一套新的 Agent 循环
```

---

## 3. 设计目标

### 3.1 必须达到的目标

1. **单 Agent 与多 Agent 使用同一个底层执行逻辑。**
2. **整个 Run 有明确的总 token 上限。**
3. **不再把 Run 总预算平均切成每个 Agent 的固定预算。**
4. **支持 Agent 是否可并行的显式配置。**
5. **所有 Task 依赖关系必须形成 DAG，禁止环。**
6. **Verifier 和 Composer 继续按照现有依赖流程启动。**
7. **子 Agent 不直接输出给用户，但必须输出 Workflow 可消费的结构化结果。**
8. **Retry、Replan、取消、超时和失败传播遵循现有机制。**
9. **所有子 Agent 的实际消耗都计入同一个 Run 总预算。**

### 3.2 非目标

本提案不做以下事情：

- 不重新设计一套 Multi-Agent 推理 Runtime；
- 不给每个 Step 单独分配 token；
- 不为 Investigator、Verifier、Composer 分别复制一套模型循环；
- 不因为进入 Workflow 就改变已有 Single-Agent 的 `MaxSteps` 语义；
- 不把多个 Agent 强行平均分配预算；
- 不让 Replan 重置 Run 总预算；
- 不让 Retry 获得一份新的完整 Run 预算。

---

## 4. 核心设计

## 4.1 执行模型：多个已有 Single-Agent Run 的编排

最终执行模型如下：

```text
Investigation Run
    │
    ├── Single-Agent Run: investigator.code
    │       └── 输出 Code Investigation Artifact
    │
    ├── Single-Agent Run: investigator.runtime
    │       └── 输出 Runtime Investigation Artifact
    │
    ├── Single-Agent Run: investigator.docs
    │       └── 输出 Documentation Investigation Artifact
    │
    ├── Single-Agent Run: evidence.verify
    │       └── 消费 Investigator Artifacts，输出 Verified Artifact
    │
    └── Single-Agent Run: synthesizer
            └── 消费 Verified Artifact，输出最终用户答案
```

每个节点仍然调用已有 Single-Agent Runtime。Coordinator 只负责：

- 创建节点；
- 注入输入；
- 管理依赖；
- 控制并行；
- 传递结构化产物；
- 汇总 usage；
- 控制 Run 总预算；
- 执行失败、Retry 和 Replan 策略。

当前 Investigator 和 Verifier 已经通过 `AgentRuntimeTaskExecutor` 调用 Agent Runtime；后续应将 Composer 的执行入口也统一到同一套预算传递和 usage 结算适配层。

---

## 4.2 预算模型：只保留 Run 级共享预算

### 4.2.1 Run 总预算

整个 Multi-Agent Investigation Run 使用一个共享总预算：

```text
RunMaxOutputTokens = llm_answer_max_tokens
```

以当前配置为例：

```text
llm_answer_max_tokens = 12000
```

表示本次 Investigation Run 所有 Agent 的 output token 累计上限为：

```text
12000
```

所有以下节点的实际消耗都从这一个共享额度扣减：

```text
investigator.code
investigator.runtime
investigator.docs
evidence.verify
synthesizer
Retry
Replan
```

### 4.2.2 配置语义兼容说明

当前 `llm_answer_max_tokens` 在 Single-Agent 中表示单次模型调用的最大输出。将其作为 Investigation Run 总预算时，需要在代码中区分调用上下文：

```text
Standalone Single-Agent
    llm_answer_max_tokens = 单次模型调用上限

Multi-Agent Investigation Run
    llm_answer_max_tokens = Run 总 output 上限的默认来源
```

为了避免长期歧义，建议后续新增或逐步引入：

```text
investigation_max_output_tokens
```

但迁移初期可以保持：

```text
investigation_max_output_tokens 未配置
    → 使用 llm_answer_max_tokens 作为 Run 总预算
```

重要的是：**数值可以复用，代码语义不能混为一谈。**

### 4.2.3 不给子 Agent 预分配固定预算

不再使用：

```text
AgentBudgetPool / Agent Task 数量
```

也不再提前生成：

```text
investigator.code = 1400
investigator.runtime = 1400
evidence.verify = 1400
```

改为共享预算、按实际消耗扣减：

```text
Run 总预算 = 12000

code 使用 1800 → Run 剩余 10200
runtime 使用 2200 → Run 剩余 8000
docs 使用 1300 → Run 剩余 6700
verify 使用 900 → Run 剩余 5800
composer 使用 700 → Run 剩余 5100
```

这样可以避免简单 Task 被分配过多预算、复杂 Task 被平均压缩的问题。

### 4.2.4 不分配子 Agent 预算不等于取消运行时预算控制

多个 Agent 并行时必须使用一个并发安全的共享预算闸门：

```text
模型调用前：检查 Run 剩余预算
模型调用期间：记录实际 usage
模型调用后：原子扣减实际 usage
达到 Run 上限：阻止后续模型调用
```

不能只在整个 Task 结束时 Settlement，否则并行情况下可能出现：

```text
Agent A 和 Agent B 同时读取到剩余 1000
Agent A 使用 800
Agent B 使用 800
最终实际使用 1600，超过 Run 上限
```

因此建议将当前 Ledger 从“Task Reservation 为中心”调整为：

```text
Run-level shared usage gate
```

Task 可以保留 ID、usage 和审计记录，但不再必须拥有一份预分配 token quota。

### 4.2.5 下游节点保护

如果 Investigator 完全共享 Run 预算，可能出现 Investigator 把预算全部消耗完，导致 Verifier 和 Composer 无法运行。

因此需要保留**Workflow 下游保护机制**，但它不是给每个子 Agent 分配预算：

```text
Verifier 保留额度
Composer 保留额度
```

例如：

```text
Run 总预算 = 12000
Verifier 保护额度 = 1500
Composer 保护额度 = 1000
Investigator 可消费额度 = 9500
```

这属于 Workflow 的可交付性保护，不属于每个子 Agent 的固定预算。

如果暂时不引入显式 reserve，则必须接受：

```text
Investigator 消耗完 Run 预算
    → Verifier/Composer 无法启动
    → Run 失败或只能走 fallback
```

生产环境建议保留 Verifier/Composer 保护额度。

---

## 4.3 Step 语义：完全复用 Single-Agent

`MaxSteps` 继续沿用已有 Single-Agent Definition：

```text
investigator.code     MaxSteps = 3
investigator.runtime  MaxSteps = 3
investigator.docs     MaxSteps = 3
evidence.verify       MaxSteps = 1
synthesizer            MaxSteps = 1
```

Step 的含义仍然是：

```text
一次模型决策
+ 本轮工具调用
+ 工具结果回填
```

不是：

```text
业务阶段
独立 token 预算
固定输出次数
```

也不再执行：

```text
Task Budget / MaxSteps
```

Single-Agent Runtime 自己决定 Agent 在第 1、2、3 轮是否继续运行。如果模型提前给出有效结果，可以提前结束；如果达到 `MaxSteps` 仍未回答，则沿用现有强制结论逻辑。

---

## 4.4 Workflow 输出模式

### 4.4.1 Standalone Mode

普通 Single-Agent 直接服务用户：

```text
Input
    ↓
Single-Agent Runtime
    ↓
用户可读 Answer
```

### 4.4.2 Workflow Node Mode

Multi-Agent 中的 Investigator 不直接给用户最终答案：

```text
Task Input
    ↓
Single-Agent Runtime
    ↓
Workflow Artifact
```

Workflow Artifact 需要供下游节点使用，例如：

```json
{
  "task_id": "investigator.code.1",
  "status": "succeeded",
  "findings": [],
  "evidence": [],
  "claims": [],
  "limitations": [],
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "total_tokens": 0
  }
}
```

因此准确说法是：

```text
子 Agent 不向用户输出最终 Answer
但必须向 Workflow 输出 Node Result / Evidence Artifact
```

建议由统一 Runtime 支持两类 Output Contract：

```text
Standalone Answer Contract
Workflow Artifact Contract
```

底层执行循环保持不变，只更换输入/输出 Schema 和交付适配器。

---

## 4.5 并行与依赖模型

### 4.5.1 DAG 约束

所有 Task 依赖必须形成有向无环图：

```text
A → B → C
```

允许：

```text
A → B
A → C
```

不允许：

```text
A → B → C → A
```

计划编译阶段必须进行环检测，发现环直接拒绝 Plan。

### 4.5.2 Agent 并行配置

建议在 Task Template 或 ExecutableTask 增加：

```text
AllowParallel bool
```

一个 Task 只有同时满足以下条件时才可以运行：

```text
1. 所有依赖已完成；
2. 依赖结果状态满足启动条件；
3. Task 自身允许并行；
4. Agent 并发上限未超出；
5. Tool 并发上限未超出；
6. 共享 Run 预算仍然可用。
```

没有依赖的 Investigator 默认可以并行：

```text
investigator.code
investigator.runtime
investigator.docs
```

Verifier 必须等待 Investigator 依赖完成。

Composer 必须等待 Verifier 完成。

---

## 4.6 Verifier 启动时机

沿用当前 Workflow 语义：

```text
Investigator 节点完成
    ↓
证据和调查结果写入 Workflow Artifact / Evidence Ledger
    ↓
Verifier 启动
```

默认策略：

```text
必需 Investigator 失败
    → Verifier blocked

可选 Investigator 失败
    → 根据策略决定是否允许 Verifier 使用 partial result
```

建议明确支持：

```text
RequiredDependency
OptionalDependency
```

这样不会因为一个可选的 docs Investigator 失败，就让整个验证节点无条件 blocked。

---

## 4.7 Composer 启动时机

沿用当前流程：

```text
Verifier 成功
    ↓
生成 Verified Bundle
    ↓
Composer 启动
    ↓
输出最终用户答案
```

Composer 不应再收集新的调查证据，默认不开放调查工具，只消费：

```text
Verified Bundle
```

需要将 Composer 的执行和 Investigator/Verifier 统一到同一个 Single-Agent Runtime 适配入口，避免 Composer 使用特殊预算传递逻辑。

当前 Composer 入口：

```text
/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/investigation/agent_composer.go
```

---

## 4.8 失败传播

默认保持当前依赖失败语义：

```text
上游 Agent 失败
    ↓
依赖节点 blocked
```

例如：

```text
investigator.code 失败
    ↓
evidence.verify 缺少必需依赖
    ↓
evidence.verify blocked
```

建议将失败策略明确为节点级配置：

```text
Block
    依赖失败，当前节点 blocked

ContinueWithPartial
    允许使用已完成节点的部分结果

RetryDependency
    先重试失败依赖

Replan
    重新生成替代 Task
```

默认建议：

```text
Required Investigator 失败
    → Retry
    → Retry 仍失败则 Replan
    → 没有替代方案则 Verifier blocked

Optional Investigator 失败
    → 记录 limitation
    → 允许 Verifier 继续使用 partial result
```

---

## 4.9 Retry 与 Replan

### Retry

Retry 继续使用现有 Single-Agent 的任务重试机制：

```text
同一个 Task
    ↓
重新调用同一个已有 Single-Agent Definition
```

Retry 不能重置 Run 总预算：

```text
第一次调用消耗 2000
第二次 Retry 消耗 1800
Run 总计消耗 3800
```

Retry 次数仍受：

```text
MaxAttempts
Retryable Failure
Timeout
Cancellation
```

控制。

### Replan

Replan 继续使用当前 Investigation 的 Gap-driven Replan：

```text
验证后发现目标未覆盖
    ↓
识别未完成 Goal
    ↓
生成新的 Task Candidates
    ↓
检查 DAG、并行和 Run 剩余预算
    ↓
启动新的已有 Single-Agent Run
```

Replan 不能重置：

```text
Run 总预算
已使用 token
已完成 Task 记录
```

新的 Task 只能使用：

```text
Run 剩余共享预算
```

---

## 5. 推荐的运行时接口边界

建议保留一个统一的 Single-Agent Runtime 入口，并让 Workflow 传递本次调用的上下文和输出模式：

```go
type AgentRunMode string

const (
    AgentRunStandalone   AgentRunMode = "standalone"
    AgentRunWorkflowNode AgentRunMode = "workflow_node"
)

type AgentRunRequest struct {
    RunID        string
    Definition   agentapi.DefinitionRef
    Mode         AgentRunMode
    Input        json.RawMessage
    OutputSchema agentapi.SchemaRef
    Limits       agentapi.RunLimits
    Context      []agentapi.ContextBlock
    Permissions  agentapi.PermissionPolicy
}
```

`Coordinator` 负责构造请求：

```text
Definition
Mode
Input
OutputSchema
Workflow correlation
Dependency artifacts
```

Single-Agent Runtime 负责：

```text
MaxSteps
MaxToolCalls
模型循环
工具调用
上下文处理
最终输出
usage 记录
```

Run Budget Gate 负责：

```text
Run 总额度
并发安全扣减
Verifier/Composer 保护额度
Retry/Replan 共享剩余额度
```

---

## 6. 推荐的配置变化

### 6.1 保留现有 Single-Agent 配置

```text
llm_answer_max_tokens
llm_context_window
llm_max_continue_rounds
agent_max_steps
agent_max_tool_calls
```

它们继续服务已有 Single-Agent Definition 和单次 Runtime 调用。

### 6.2 Investigation Run 总预算

短期兼容方案：

```text
investigation_max_output_tokens 未配置
    → 使用 llm_answer_max_tokens
```

推荐长期方案：

```text
investigation_max_output_tokens
```

用于明确表示：

```text
整个 Multi-Agent Run 的 output 总预算
```

### 6.3 并行和 DAG 配置

建议增加或明确：

```text
investigation_max_parallelism
investigation_max_agent_parallelism
investigation_max_tool_parallelism
Task.AllowParallel
Task.Dependencies
```

### 6.4 下游保护配置

可选增加：

```text
investigation_verifier_reserve_tokens
investigation_composer_reserve_tokens
```

如果不新增配置，可以先使用固定比例或固定默认值，但它们应被定义为：

```text
Workflow 可交付性保护
```

而不是子 Agent Task 配额。

---

## 7. 迁移方案

### P0：统一执行语义

1. 确认 Investigator、Verifier、Composer 均调用现有 Single-Agent Runtime；
2. 抽取统一的 Agent Run Request 构造和 Limits 传递逻辑；
3. 增加 `Standalone` 和 `WorkflowNode` 两种输出模式；
4. 移除 Multi-Agent 层对 Step 的二次解释；
5. 保留 Definition 级 `MaxSteps`、`MaxToolCalls` 和 `MaxContinueRounds`。

### P1：移除平均 Task token 分配

1. 删除或停用 `AgentBudgetPool / taskCount` 平均分配逻辑；
2. 将 Run 总预算改为共享预算闸门；
3. 模型调用前检查剩余 Run 预算；
4. 模型调用后按实际 provider usage 原子扣减；
5. Retry 和 Replan 继续使用同一个 Run 预算；
6. 增加 Verifier/Composer 下游保护额度。

### P2：完善 DAG 和并行能力

1. 增加 `AllowParallel`；
2. 增加完整 DAG 环检测；
3. 将依赖就绪、并行上限、工具上限统一纳入 Scheduler；
4. 区分 Required 和 Optional Dependency；
5. 增加 partial result 失败策略。

### P3：统一 Composer

1. 将 Composer 预算和 Runtime Limits 传递统一到 Agent Run 适配层；
2. Composer 只消费 Verified Bundle；
3. Composer 不持有调查工具权限；
4. Composer 输出最终 Answer，不向 Verifier 回流。

---

## 8. 验收标准

### 8.1 预算

- Run 总 output usage 不得超过 Run hard limit；
- 并行 Agent 的累计实际 usage 不得产生超额竞态；
- 不存在所有 Agent Task 平均切分 token 的逻辑；
- Retry 和 Replan 不会重置 Run 预算；
- Verifier 和 Composer 在保护额度存在时仍能运行。

### 8.2 Single-Agent 复用

- Investigator、Verifier、Composer 使用同一个底层模型循环；
- `MaxSteps` 只由 Agent Definition / Run Limits 控制；
- 工具调用、上下文回填和强制结论逻辑不复制；
- Workflow Node 不向用户交付最终答案，但会输出结构化 Artifact。

### 8.3 DAG 与并行

- 有依赖的节点不会提前执行；
- 无依赖且允许并行的 Investigator 可以并行执行；
- 存在环的 Plan 在编译阶段拒绝；
- 并行执行不会绕过 Run Budget Gate；
- Agent、Tool 并发上限分别生效。

### 8.4 失败、Retry、Replan

- 必需依赖失败后，Verifier 按策略 blocked；
- 可选依赖失败时可以使用 partial result；
- Retry 只重试当前已有 Single-Agent Task；
- Replan 只针对未覆盖目标生成新的 Task；
- Retry/Replan 产生的 token 消耗计入同一 Run；
- 所有失败和预算拒绝都有可追踪的 Task/Run 事件。

---

## 9. 风险与取舍

### 风险一：没有子 Agent 固定预算导致某个 Agent 消耗过多

缓解方式：

```text
共享 Run Budget Gate
+ Verifier/Composer 保护额度
+ MaxSteps
+ MaxToolCalls
+ Agent 并发上限
```

### 风险二：并行调用发生预算竞态

缓解方式：

```text
原子 TryAcquire / Charge
调用前预留当前轮所需额度
调用后按 provider usage 结算
```

### 风险三：`llm_answer_max_tokens` 语义混淆

缓解方式：

```text
短期：investigation_max_output_tokens 缺省时回退到 llm_answer_max_tokens
长期：使用独立的 Run 预算配置
```

### 风险四：子 Agent 只返回自然语言，Verifier 无法稳定消费

缓解方式：

```text
WorkflowNode Mode
固定 OutputSchema
结构化 Evidence / Finding / Claim / Limitation
```

### 风险五：某个非关键 Investigator 失败导致整个 Run 失败

缓解方式：

```text
Required / Optional Dependency
Partial Result 策略
Failure Policy 显式化
```

---

## 10. 最终决策建议

建议批准以下架构决策：

```text
1. 多 Agent = 多个已有 Single-Agent Run 的 Workflow 编排
2. 不再创建新的 Multi-Agent Agent Runtime
3. Single-Agent Runtime 继续负责 Step、工具循环和模型调用
4. Run 使用一个共享总 token 预算
5. 不再给每个子 Agent 平均分配固定 token quota
6. 使用共享预算闸门保证并行安全
7. Verifier 和 Composer 沿用当前依赖启动顺序
8. 子 Agent 输出 Workflow Artifact，不直接给用户最终答案
9. 增加 Agent 并行能力和 DAG 环检测
10. Retry 和 Replan 沿用现有机制且共享同一个 Run 预算
11. 保留 Verifier/Composer 下游保护额度，避免 Investigator 吃光预算
12. 长期拆分 Single-Agent 单次调用配置和 Investigation Run 总预算配置
```

最终架构可以概括为：

```text
已有 Single-Agent Runtime
        +
Workflow Coordinator
        +
Run 级共享预算 Gate
        +
DAG / 并行 / 失败传播
        =
可控的多 Agent Investigation Workflow
```


---

## 11. 性能根因分析与已实施优化

### 11.1 为什么多 Agent 可能比单 Agent 更慢

多 Agent 不是天然更快。它只有在任务能够拆成至少两个相互独立、耗时占主导的子任务，并且这些子任务能够真正并行时，才可能用并行收益覆盖编排开销。

修复前的动态 delegation 关键路径为：

```text
Parent 模型决定是否 delegation
    + Child 模型调用
    + enqueue 事务
    + claim 事务
    + Parent / Worker 抢 lease
    + claim 失败后的 100ms polling
    + settlement / artifact / checkpoint / event
    + validation / optional verifier
    + Parent 最终综合
```

主要根因如下：

1. **简单请求误入 delegation**：普通请求仍能看到 `delegate_investigation`，模型可能绕过服务端路由主动 fan-out；
2. **单子任务或串行任务也进入多 Agent**：无法产生并行收益，却承担完整的 Parent、持久化、校验和综合开销；
3. **配置并发度与路由脱节**：即使 `delegation_max_concurrent=1`，路由仍可能选择多 Agent，最终子任务只能串行执行；
4. **durable queue 存在 lease 竞争**：旧流程执行 `enqueue -> Worker claim -> Parent claim`，Parent 与后台 Worker 会竞争同一 Work Item；
5. **Parent claim 失败后轮询**：Parent 以 100ms 周期等待 Worker settlement，把固定调度延迟加入关键路径；
6. **Run 预算语义不统一**：并行节点、Retry、Replan 可能使用不同的预算投影，造成不必要的拒绝、等待或重复预算；
7. **缺少分阶段耗时**：只能看到总耗时，无法区分模型耗时、队列等待、claim、settlement 和 validation。

因此，性能目标不是“所有多 Agent 请求都一定快于单 Agent”，而是：

```text
没有可验证并行收益的请求不进入多 Agent；
真正可并行的请求消除固定轮询和重复事务；
多 Agent 关键路径接近最慢子任务，而不是所有子任务耗时之和。
```

理想关键路径近似为：

```text
T_multi ≈ T_parent_decision
        + max(T_child_1 ... T_child_n)
        + T_validation
        + T_parent_synthesis
```

而不是：

```text
T_multi ≈ T_parent_decision
        + sum(T_child_1 ... T_child_n)
        + T_queue_polling
        + T_validation
        + T_parent_synthesis
```

### 11.2 已实施的路由优化

QA 路由现在只有在以下条件全部满足时才暴露动态 delegation：

1. Retrieval 明确建议 `multi_agent`；
2. delegation 功能已启用；
3. `delegate_investigation` 工具已就绪；
4. 至少有两个 `IndependentlyUseful=true` 且没有前置依赖的任务；
5. 生产配置的 `delegation_max_concurrent >= 2`。

以下情况统一降级为 Single-Agent：

- Retrieval 建议 `single_agent`；
- 只有一个子任务；
- 任务必须顺序执行；
- 只有一个可独立执行的任务；
- delegation 不可用或工具不可见；
- `delegation_max_concurrent=1`；
- 请求包含写操作。

降级后不仅省略 delegation 提示词，还会从不可变的候选工具集合中移除 `delegate_investigation`，防止 Parent 模型绕过路由重新触发慢链路。

对应路由原因包括：

```text
single_agent_suggestion
multi_agent_not_worthwhile
delegation_unavailable
delegation_concurrency_too_low
write_requested
parent_dynamic_delegation
```

### 11.3 已实施的 durable queue 优化

生产 Store 新增原子派发能力：

```text
EnqueueAndClaimWorkItem(...)
```

在同一个事务中完成：

```text
enqueue + claim + lease/fence 建立
```

新的 Parent 快路径不再执行：

```text
enqueue
    -> 等待后台 Worker 抢 lease
    -> Parent 再次 ClaimWorkItemByID
    -> claim 失败后每 100ms 轮询 settlement
```

该优化保留以下 durable 语义：

- Work Item identity / payload 冲突校验；
- live lease 不可抢占；
- expired lease 可以 reclaim；
- reclaim 后 fence 单调增长；
- settlement 幂等；
- 不支持原子接口的测试队列或嵌入式实现继续使用兼容路径。

### 11.4 已实施的共享 Run 预算

Workflow Coordinator 与 Single-Agent Runtime 现在共享同一个 Run-level budget account：

- 每次物理模型调用前原子 Reserve；
- provider 返回 usage 后按实际值 Settle；
- 并行调用共享同一份已用量和 in-flight reservation；
- Retry 和 Replan 不创建新预算；
- Verifier 和 Composer 使用 phase reserve；
- Workflow Node 返回的 `ToolCalls` 在 Node accounting 阶段计入共享 Run 预算，不会因为模型 usage settlement 只包含 token/cost 而丢失；
- 自定义 Node Executor 即使没有进入 Runtime 的物理调用 Gate，其返回 usage 仍会由 Coordinator 补记。

### 11.5 已实施的 Workflow 约束

- Investigator、Verifier、Composer 继续复用 Single-Agent Runtime；
- Workflow Agent Node 使用 `RunOutputWorkflowNode`；
- Composer 只消费 Verified Bundle；
- Composer 输出 Answer；
- Composer 不暴露调查工具；
- 节点级预算仅作为兼容投影，不再是平均切分的硬 quota。

### 11.6 新增耗时观测指标

`ExecutionEvent` 已增加以下阶段耗时：

| 字段 | 含义 |
|---|---|
| `queue_wait_ms` | Work Item 从可执行到获得执行权的等待时间 |
| `queue_claim_ms` | enqueue/claim 或 claim 事务自身耗时 |
| `settlement_ms` | child result、artifact、checkpoint、event 等终态结算耗时 |
| `validation_ms` | delegation report 的确定性校验和可选语义验证耗时 |

分析多 Agent 慢请求时，应至少关联以下维度：

```text
route_reason
child_count
configured_max_concurrent
actual_peak_concurrency
parent_model_ms
child_model_ms (per child)
queue_wait_ms
queue_claim_ms
settlement_ms
validation_ms
retry_count
replan_count
```

其中本次已经落库/投影的是 queue、claim、settlement、validation 四类字段；其余字段作为后续聚合看板维度。

### 11.7 性能验收标准

新增以下性能验收标准：

1. Single-Agent 建议、单任务、串行任务和并发度为 1 的请求不得暴露 delegation 工具；
2. 多 Agent 路由必须具备至少两个可独立并行任务以及至少两个执行槽；
3. 生产 Store 原子快路径不得再次调用 Parent `ClaimWorkItemByID`；
4. 原子快路径不得引入固定 100ms polling；
5. 每个 child Runtime 只执行一次，每个 terminal settlement 只提交一次；
6. 并行 child 的模型调用必须共享同一个 Run budget gate；
7. Workflow `MaxToolCalls` 必须包含 Agent Node 实际返回的工具调用数；
8. 性能回归测试应分别覆盖：单 Agent 基线、无收益 fan-out、两个并行 child、并发度为 1、claim 冲突、expired lease reclaim；
9. 对可并行的同类任务，目标是 child execution 段接近 `max(child duration)`，不能退化为 `sum(child duration)`；
10. 端到端是否快于单 Agent 以实际 benchmark 为准；若 Parent 决策、validation 和 synthesis 的固定成本大于并行收益，路由必须保持 Single-Agent。

### 11.8 当前迁移状态

| 阶段 | 状态 | 已完成内容 | 后续工作 |
|---|---|---|---|
| P0 统一执行语义 | 已完成 | Workflow Node 复用 Runtime、统一输出模式、Composer 约束 | 持续删除遗留重复实现 |
| P1 共享 Run 预算 | 已完成 | 原子 Reserve/Settle、Retry/Replan 共享预算、Verifier/Composer reserve、ToolCalls 回归测试 | 增加预算看板和告警 |
| P2 DAG 与并行 | 部分完成 | DAG/并行调度已存在，QA 路由增加并行收益和实际并发槽校验 | Required/Optional 策略继续统一，增加端到端 benchmark |
| P3 Composer 统一 | 已完成核心路径 | Verified Bundle -> Composer -> Answer，Composer 无工具 | 清理旧 Composer 兼容入口 |
| 性能关键路径 | 已完成首轮 | 原子 enqueue+claim、移除 Parent claim polling、阶段耗时字段 | 基于线上分位数继续优化 validation 和 synthesis |
