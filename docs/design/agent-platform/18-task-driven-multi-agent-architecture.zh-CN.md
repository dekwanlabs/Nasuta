# Nasuta 任务驱动多 Agent 架构设计

[返回设计索引](README.zh-CN.md)

> 状态：目标设计，待实施
> 更新日期：2026-08-12
> 适用范围：Nasuta QA、Feature Delivery 和通用 Agent Workflow
> 替代基线：[QA 与研发任务多 Agent 路由方案](15-qa-and-feature-delivery-multi-agent-routing-proposal.zh-CN.md)
> 相关基线：[Nasuta 多 Agent 平台方案](12-multi-agent-platform-proposal.zh-CN.md)

## 1. 摘要

当前 QA Multi-Agent 把三个实现细节绑定成了一件事：

```text
问题 -> internal evidence -> code/runtime/docs 三个 Agent -> join -> synthesize
```

这使得 `evidence_not_internal_only`、`runtime_evidence_required`、
`history_dependency_required` 和 `time_resolution_failed` 成为多 Agent 的硬性
排除条件。它们并不是多 Agent 的一般性要求，而是当前固定 Workflow 能力不足时的
降级分支。结果是执行策略由证据来源和预先写死的角色决定，而不是由任务结构决定。

本方案将 Nasuta 的多 Agent 重构为：

```text
用户请求
  -> canonical task / evidence goals
  -> bounded planner 分解任务
  -> capability registry 匹配 Agent 和工具
  -> constrained task graph 执行
  -> workflow evidence ledger 收敛证据
  -> verifier / synthesizer 形成结果
```

核心结论：

1. 多 Agent 的触发依据是**可并行、可隔离、可验证的任务结构**，不是 `internal`
   来源，也不是固定的 Agent 数量。
2. Web、Memory、Runtime、History 和 Time 是任务上下文或能力维度，不应直接作为
   Multi-Agent 的排除条件。
3. 模型可以提出任务分解和能力需求，但不能创建任意 Agent、扩大权限、修改预算、
   绕过 Gate 或无限生成任务。
4. 编排采用“模型规划、服务端校验、确定性执行”的混合方式：规划可以动态，执行
   边界必须固定、可审计、可恢复。
5. 所有子 Agent 共享 Workflow 级 Evidence Ledger；子 Agent 只交付结构化的新增
   证据和结论，不把独立上下文重新拼成一段无界对话。
6. 当前 `internal/agent/workflow`、Agent Catalog、Schema Registry、权限交集、
   Handoff、预算和恢复机制继续复用；重构重点在 QA planner、任务合同和证据收敛。

## 2. 设计问题

### 2.1 当前实现的问题

当前固定 Investigation 定义了三个必需节点：

```text
investigate.code
investigate.runtime
investigate.docs
  -> evidence.join
  -> synthesize
```

每个节点都必须成功，且三个节点只接受同一个 `investigation.request`。这会产生
以下结构性问题：

| 问题 | 后果 |
|---|---|
| 角色按来源预先固定 | 问题需要日志或 Web 时只能整体降级，而不是增加相应能力节点 |
| 三个节点全部必需 | 与当前问题无关的 Agent 也会消耗预算和延迟 |
| 子 Agent 只收到 `question` | 历史实体、时间范围、证据目标没有进入统一任务合同 |
| Synthesizer 无工具 | 发现证据缺口后不能补查，只能接受缺口或幻觉补全 |
| 路由依赖模型的 `complexity` 和 `confidence` | “是否值得并行”没有可计算的任务计划作为依据 |
| QA 与 Workflow 准备链路分叉 | Single-Agent 的预检索、工具准入和证据账本没有自然复用到 Workflow |
| Join 以报告为中心 | 结果可以重复、互相矛盾，但缺少统一 identity、coverage 和版本收敛 |

### 2.2 对业内实现的抽象

主流多 Agent 实现虽然 API 形态不同，但有效部分高度一致：

| 实现思路 | 可复用原则 | Nasuta 的取舍 |
|---|---|---|
| Orchestrator-Worker | 一个协调者先理解任务，再派发有界子任务 | 采用，但把动态派发限制在服务端 Policy 和预算内 |
| Handoff / 专业 Agent | 每个 Agent 有明确能力、工具和输出合同 | 采用 Definition + Capability + Schema |
| Graph Workflow | 依赖关系、并行和停止条件显式化 | 采用现有 DAG；增加运行时受限任务图，而非自由图 |
| Hierarchical manager | Manager 负责拆分、委派和收敛 | 采用 coordinator，但不允许 manager 直接扩大权限 |
| Parallel research | 只有独立方向才并行，结果必须压缩后再汇总 | 采用任务级并行和增量 Handoff |
| Reviewer / verifier | 生成和验证分离，冲突不能靠多数票静默消失 | 采用确定性 Gate + 专用 verifier |
| Group chat / swarm | Agent 自由互聊和自由转移 | 不采用，难以控制成本、权限、终止和审计 |

参考资料：

- OpenAI Agents SDK，多 Agent handoff 和 manager pattern：
  <https://openai.github.io/openai-agents-python/multi_agent/>
- Anthropic，Research multi-agent 的 orchestrator-worker、并行探索、任务边界和
  effort scaling：
  <https://www.anthropic.com/engineering/built-multi-agent-research-system>
- LangChain，多 Agent patterns（subagents、handoffs、router）：
  <https://docs.langchain.com/oss/python/langchain/multi-agent>
- Microsoft AutoGen，Selector Group Chat、Swarm 和 GraphFlow：
  <https://microsoft.github.io/autogen/stable/user-guide/agentchat-user-guide/teams.html>
- CrewAI，sequential 与 hierarchical process：
  <https://docs.crewai.com/en/concepts/processes>

这些实现不构成 Nasuta 的依赖选择。Nasuta 只吸收其稳定的领域原则，不引入自由群聊
或外部编排运行时。

## 3. 目标与非目标

### 3.1 目标

1. 用任务结构而不是来源标签决定是否以及如何使用多 Agent。
2. 支持内部知识、Web、Memory、运行时日志、代码修改和验证等不同能力组合。
3. 让多个 Agent 在独立上下文中工作，同时共享有界的证据账本和任务状态。
4. 允许动态增加、取消或重试任务，但所有动作都经过服务端 Policy、权限和预算校验。
5. 保持 Parent Run、Child Run、Handoff、SSE、Session、Trace、Usage 和 Evaluation
   的统一生命周期。
6. 让部分证据缺失、节点失败和冲突成为可表达的结果，而不是隐式成功或静默降级。
7. 保证简单问题走低成本路径，复杂问题才承担多 Agent 的额外协调开销。

### 3.2 非目标

- 不实现 Agent 自由群聊、无限 handoff 或模型自主创建 Definition。
- 不让模型决定 Provider、工具权限、最大并行度、总预算或是否跳过 Gate。
- 不把所有请求强制转换成多 Agent。
- 不用 Agent 数量、投票数量或平均置信度替代证据质量和确定性验证。
- 不在本阶段引入跨进程 Worker、Temporal 或任意外部任务队列。
- 不让通用 Workflow 层解析代码、日志、文档等领域私有数据格式。

## 4. 核心概念

### 4.1 Task Contract

Task Contract 是一次调查或交付任务的 canonical 输入。它在 QA 准备阶段形成，之后
所有 Agent 都使用同一版本，不再各自从原始问题猜测上下文。

```go
type TaskContract struct {
    TaskID          string
    Question        string
    Objective       string
    Constraints     []Constraint
    Entities        []EntityRef
    EvidenceGoals   []EvidenceGoal
    Context         ContextRefs
    RequestedOutput OutputContract
}

type ContextRefs struct {
    ConversationRefs []ConversationRef
    TimeRange        *TimeRange
    SeedEvidence     []EvidenceRef
    UserContent      []ContentRef
}

type EvidenceGoal struct {
    ID              string
    Facet           string
    Required        bool
    Sources         []SourceKind
    Freshness       FreshnessPolicy
    MinimumCoverage CoverageRequirement
}
```

`TaskContract` 的职责是确定语义边界，不是把所有正文塞入每个 Agent。正文通过
`EvidenceRef`、受控 Context View 或工具获取，避免上下文复制和 token 浪费。

### 4.2 Capability

Agent 不再以 `code/runtime/docs` 这样的来源角色作为第一分类，而以能力声明注册：

```go
type Capability struct {
    ID              string
    Purpose         string
    InputFacets     []string
    OutputSchema    agentapi.SchemaRef
    ToolIDs         []string
    PermissionScope []string
    Freshness       FreshnessPolicy
    SideEffects     SideEffectClass
    MaxConcurrency  int
}
```

示例能力：

```text
knowledge.code.inspect
knowledge.service.trace
knowledge.docs.verify
knowledge.web.research
knowledge.memory.recall
knowledge.runtime.observe
change.plan
change.implement
change.validate
evidence.verify
```

一个 Agent Definition 可以实现一个或多个能力，但每次 Node Run 只绑定一个明确的
Capability Contract。这样可以在不改任务模型的情况下，将内部拓扑和实时日志提供给
不同的实现或不同权限域。

### 4.3 Task Graph

Task Graph 是一次 Run 的受限执行计划。它可以由 Planner 提议，但只能由服务端验证
后运行：

```go
type TaskSpec struct {
    ID              string
    Purpose         string
    RequiredFacets  []string
    Capability      string
    InputRefs       []EvidenceRef
    OutputSchema    agentapi.SchemaRef
    ParallelGroup   string
    Optional        bool
    MaxAttempts     int
    Budget          NodeBudget
}

type TaskGraphProposal struct {
    Tasks []TaskSpec
    Edges []TaskEdge
    Stop  StopPolicy
}
```

服务端验证：

1. Capability 存在且已启用；
2. Task 的输入输出 Schema 兼容；
3. 图无环，所有必需 Evidence Goal 存在可达路径；
4. 每个 Task 的权限是 Workflow 和调用者权限的子集；
5. Task 数量、并行度、尝试次数、工具调用数、token 和费用不超过预算；
6. `ParallelGroup` 中的任务没有互相依赖，也没有重叠写集合；
7. Optional Task 不得成为必需输出的唯一来源；
8. Stop Policy 只能收紧服务端默认限制，不能放宽；
9. 任务描述不包含未授权工具、Provider、内部提示词或隐藏推理要求。

### 4.4 Evidence Ledger

Evidence Ledger 是 Workflow 级唯一证据事实源。它记录候选、物化、交付、覆盖、冲突
和缺口，而不是只在 Join 时保存最终文本。

```go
type EvidenceRecord struct {
    Identity        string
    Source          SourceKind
    Version         string
    TimeRange       *TimeRange
    ContentHash     string
    Coverage        []CoverageClaim
    Authority       Authority
    Status          EvidenceStatus
    ProducerTaskID  string
    References      []agentapi.Reference
}
```

规则：

- Identity 来自来源适配器提供的稳定标识，不使用 query hash 或语义相似度代替。
- 相同 identity、相同 hash 只保留一个可交付正文。
- 相同 identity、不同 hash 记录版本冲突，不静默覆盖。
- `delivered` 只表示实际进入模型上下文的证据；召回但未交付的内容保持 candidate。
- 子 Agent 只能提交新增 Evidence Item 或对已有 identity 的结构化更新。
- Join、Verifier 和 Synthesizer 使用同一 Ledger View，不重新拼接重复正文。

这延续 `qa-unified-evidence-acquisition-pipeline` 中的设计结论，但把 Ledger 从单 Agent
Run 扩展到 Workflow 作用域。

## 5. 目标架构

```text
QA / Feature Delivery API
  -> Scenario Run
  -> Canonicalization + Context Resolver
  -> Task Planner
       -> Task Graph Proposal
  -> Policy Validator
       -> immutable Execution Snapshot
  -> Constrained Orchestrator
       -> ready task queue
       -> Capability Runtime / Tool Snapshot
       -> Workflow Evidence Ledger
       -> deterministic join / verifier
  -> Result Contract
  -> Session / Artifact / SSE / Trace / Evaluation
```

### 5.1 Coordinator 的职责

Coordinator 是一次 Workflow 的控制节点，不是一个无限权限的超级 Agent。它负责：

- 读取 Task Contract 和当前 Ledger 摘要；
- 提议或选择下一批任务；
- 检查 unresolved Evidence Goals；
- 选择已有 Capability，而不是创建 Agent；
- 发起并行任务、取消无效任务、请求补查或进入收敛；
- 将工作结果交给确定性 Gate 和最终 Synthesizer。

Coordinator 不负责：

- 直接使用所有工具；
- 绕过 Capability 权限；
- 修改 Workflow Snapshot；
- 读取不属于当前 Task Contract 的历史全文；
- 用自己的判断把 unsupported claim 变成 confirmed evidence。

### 5.2 两层控制循环

```text
Planner loop:
  生成或修订 Task Graph Proposal
  -> 服务端校验
  -> 调度一批 ready tasks

Evidence loop:
  收集 task handoff
  -> ledger identity merge
  -> 更新 goal coverage
  -> 判断继续、补查、验证、部分完成或停止
```

Planner 不在每个工具调用后重写整张图。只有以下事件允许进入下一轮规划：

- 必需 Goal 已被覆盖或发现不可覆盖；
- 任务返回结构化 conflict；
- 任务失败且可重试次数耗尽；
- 新证据发现新的、经过 Policy 允许的子任务需求；
- 预算或超时即将触顶。

这避免了每轮 LLM 输出都产生不可预测的调度开销。

## 6. 什么时候触发多 Agent

### 6.1 触发条件

触发判断不再是“来源必须为 internal”。服务端依据 Task Contract 和静态计划计算：

```text
parallel_value
  = independent_task_value
  + context_capacity_value
  + specialist_tool_value
  - coordination_cost
  - duplicate_evidence_risk
  - critical_path_penalty
```

实际实现不必把该公式暴露给模型；需要可审计的字段：

```go
type ExecutionAssessment struct {
    Strategy              ExecutionStrategy
    IndependentTaskCount  int
    RequiredCapabilities  int
    Parallelizable        bool
    SharedContextPressure bool
    EstimatedCoordination CostEstimate
    Reasons               []ReasonCode
}
```

Multi-Agent 只有在以下条件同时满足时触发：

1. 至少存在两个独立的、有不同输入路径或不同专业能力的任务；
2. 任务可以通过稳定 Schema 合并，而不是依赖共享聊天状态；
3. 并行节省的关键路径时间或扩展的上下文容量大于协调成本；
4. 任务的权限、工具、Provider 和预算都可用；
5. 至少有一个确定性收敛方式：Goal coverage、Verifier、Gate 或用户批准；
6. 任务不是强顺序链，或强顺序部分可以与独立任务分开运行；
7. 预测的重复证据和失败成本在 Policy 阈值内。

### 6.2 不再作为硬性排除条件的因素

| 当前降级条件 | 新语义 |
|---|---|
| 非 internal evidence | 为 Web、Memory、Runtime 注册相应 Capability；没有能力时只阻断缺失任务或返回 partial，不把“多 Agent”本身视为非法 |
| runtime evidence required | Runtime 是一种 freshness 和权限要求；由 `knowledge.runtime.observe` 任务处理 |
| history dependency required | 先由 Context Resolver 将历史依赖解析为实体和 EvidenceRef，再作为 Task Contract 输入 |
| time resolution failed | Time Resolver 是前置规范化节点；无法解析时，只有依赖该时间范围的任务阻断，静态任务可继续，或者要求澄清 |

### 6.3 仍然必须阻断或暂停的情况

以下是数据或权限不成立，而不是“多 Agent 不适合”：

- 必需 Capability 未注册或其 Provider 明确失败；
- 必需的历史实体、时间范围或权限无法确定，且继续会改变问题语义；
- 任务没有可验证的输出合同；
- 所有候选任务都依赖同一共享状态，无法隔离；
- 预算不足以完成必需 Goal；
- 请求包含写操作但没有批准和封闭 Action Catalog；
- 发现当前请求需要用户澄清，不能由 Agent 自己猜测。

这些结果应记录为 `blocked`、`needs_clarification` 或 `partial`，而不是伪装成
`single_agent` 成功。

## 7. 任务分解与调度

### 7.1 分解原则

Planner 为每个任务输出以下内容：

```text
objective       要回答的一个明确子问题
required_facets  覆盖哪些 Evidence Goal
capability      需要的能力，而非具体工具名
input_refs      已有的实体、时间和证据引用
output_schema   必须返回的结构化报告
completion      什么事实足以结束本任务
limitations     不允许推断的边界
```

任务描述必须足够具体，避免多个 Agent 只收到“研究这个问题”而重复检索。Planner
不能把一个无法独立验证的超大任务简单复制给多个 Agent。

### 7.2 三类调度模式

#### Parallel fan-out

适用于不同证据路径可以独立探索的任务：

```text
Task Contract
  -> code path      \
  -> service graph    -> verifier -> synthesis
  -> runtime logs   /
```

#### Sequential refinement

适用于后一任务依赖前一任务产生的实体或候选：

```text
discover entities -> inspect selected symbols -> verify claim -> answer
```

#### Adaptive expansion

适用于第一批任务发现新的、可信且经过约束的调查方向：

```text
initial task -> new candidate facet -> policy check -> bounded task
```

Adaptive expansion 必须有：最大深度、最大新增任务数、总预算、重复 identity 阈值和
明确停止原因。它不是无限递归的 Agent spawn。

### 7.3 并行与强顺序混合

系统不要求一个请求只能选择 Single-Agent 或 Multi-Agent。一个 Task Graph 可以同时
包含：

- 一个先行的 Context Resolver；
- 两个并行的知识调查；
- 一个依赖调查结果的 Verifier；
- 一个最终 Synthesizer。

因此“存在历史依赖”或“存在时间解析”不应使整张图退回单 Agent；应该只为有依赖的
节点建立边。

## 8. Agent 与工具边界

### 8.1 Agent Definition

保留现有 Definition 的版本、Hash、Model、Tool Policy、Budget 和 Permission，但把
Definition 的 Purpose 从固定来源角色改为能力合同。例如：

```text
knowledge.runtime.observe
  purpose: 查询授权的时间范围内运行时日志、指标或 Trace
  input: TaskContract + TimeRange + EntityRefs
  output: RuntimeFindingReport
  freshness: bounded_live
  permissions: runtime.observe
```

一个 `Runtime Topology` Agent 不能声明自己拥有 live runtime 能力；能力注册必须和
真实工具一致。配置了 Runtime Provider 但连接失败时，必须返回 capability failure，
不能换成静态拓扑假装成功。

### 8.2 工具集合

工具仍由服务端 Registry、Definition Policy 和 Run Snapshot 决定。Planner 只请求
Capability，服务端把 Capability 解析成具体工具快照。这样可以：

- 让同一任务在不同部署中使用不同工具实现；
- 保持工具权限不随 LLM 输出扩大；
- 让工具失败和 Provider 失败可观察；
- 对不同 Agent 使用不同工具预算和速率限制。

### 8.3 Handoff

Handoff 只传：

- 结构化 finding；
- EvidenceRef 和稳定 identity；
- coverage、confidence、limitations；
- conflict 和 retryable failure；
- 需要下游关注的 unresolved goal。

不传：

- 隐藏推理链；
- 无限历史聊天；
- 未经 Ledger 注册的整块工具正文；
- “我认为另一个 Agent 应该……”这类非合同控制指令。

## 9. 证据收敛、验证与合成

### 9.1 统一收敛

Coordinator 每轮只看 Ledger 摘要：

```text
required goals covered
optional goals covered
unresolved goals
conflicting identities
new identities this round
duplicate ratio
remaining budget
```

它据此选择：

1. `continue`：仍有未覆盖的必需 Goal，且存在可负担的候选任务；
2. `verify`：证据已足够，但有关键 claim 或版本冲突需要核验；
3. `synthesize`：必需 Goal 满足且没有阻断冲突；
4. `partial`：必需 Goal 无法全部满足，但已有证据可以形成受限答案；
5. `needs_clarification`：继续执行会依赖用户未提供的语义；
6. `failed`：Workflow 或必需能力发生不可恢复失败。

### 9.2 Verifier 与 Synthesizer 分离

Synthesizer 只负责把已经通过 Ledger 选择和验证的材料组织成 Result Contract。对于
高风险任务，先运行确定性或专用 Verifier：

```text
evidence ledger
  -> claim extraction
  -> verifier / conflict gate
  -> approved evidence view
  -> synthesizer
```

Verifier 的输出必须说明：

- 哪些 claim 被支持；
- 哪些 claim 只有 partial support；
- 哪些证据互相冲突；
- 哪些限制必须出现在最终回答。

不采用“多个 Agent 说得一样就算正确”的多数票方案。

### 9.3 部分失败

默认使用 `CollectAvailable`，但是否允许形成最终答案由 Evidence Policy 决定：

- 非必需任务失败：继续，并在 limitations 中保留缺口；
- 必需任务失败但其他证据足够：返回 partial，不能返回 complete；
- 必需任务失败且没有替代覆盖：Workflow failed 或 needs clarification；
- 一个 identity 多版本冲突：进入 Verifier 或 human gate，不能静默选择。

这比当前三个节点全部 `Required=true`、任一失败即整体失败更符合实际调查任务。

## 10. 预算、停止和可靠性

### 10.1 多维预算

Workflow 预算至少包含：

```text
max_tasks
max_parallelism
max_depth
max_rounds
max_tool_calls
max_input_tokens
max_output_tokens
max_total_tokens
max_cost
max_handoff_bytes
max_duplicate_ratio
deadline
```

现有 `WorkflowBudget` 已覆盖多数资源维度，应增加 `MaxDepth`、`MaxRounds` 和
`MaxDuplicateRatio`，或由场景 Policy 派生等价限制。预算在调度前预留，在结果入账后
按实际使用结算；不能只在 Workflow 结束后统计。

### 10.2 停止条件

停止必须是结构化原因：

```text
required_goals_covered
no_new_evidence
no_affordable_task
duplicate_evidence_limit
verification_failed
deadline_exceeded
budget_exhausted
capability_unavailable
needs_clarification
```

特别禁止通过不断改变 query 让 Agent 在没有新增 identity 或 coverage 的情况下继续
搜索。

### 10.3 重试与补偿

- Provider transient failure：按 Node RetryPolicy 重试，消耗 Workflow retry budget；
- capability permanent failure：不替换 Provider，标记该 Capability unavailable；
- Agent 输出 Schema 错误：只允许受限修复或一次重新执行，不把无效文本当作 Handoff；
- 超时：取消子 Run，保留已提交的 Handoff，按失败策略判断是否继续；
- Coordinator 失败：从最后一个不可变 Planner/ Ledger Snapshot 恢复，而不是从模型
  对话恢复。

### 10.4 Parent Run 收敛与取消

Workflow 一旦进入持久化生命周期，QA Parent 的完成不能继续由请求进程中的 goroutine
独占。QA 场景必须拥有可恢复、幂等的 Coordinator：

```text
workflow terminal event / startup recovery
  -> QA Coordinator
  -> compare-and-set Parent terminal state
  -> append Session Turn once by run_id
  -> publish recoverable result event
```

约束：

- Workflow 恢复成功后，Parent Run、Session 和最终结果必须一起收敛；
- 重放同一 terminal event 不得重复写入 Session Turn；
- SSE 只是投影，不是终态事实源，客户端重连从持久化 Run 和 Event 读取；
- 客户端断开不取消持久任务；用户显式取消必须从 Parent 传播到 Workflow 和活动 Child
  Run，并记录统一的取消原因；
- Parent 不得在 Child Workflow 仍可恢复时被启动修复逻辑直接标记为 `aborted`；
- 进行中的 Workflow 使用不可变 Execution Snapshot 恢复，不依赖热加载锁或瞬时
  `context.Context` value。

## 11. 安全与权限

权限关系保持：

```text
effective permission
  = actor permission
  ∩ workflow policy
  ∩ task capability policy
  ∩ agent definition policy
  ∩ tool policy
```

任务规划不能扩大任何集合。尤其是：

- Web Agent 不能因为找不到内部证据而访问 Runtime；
- Runtime Agent 不能因为看到敏感字段而交给无权限的 Synthesizer；
- 写任务必须经过封闭 Action Catalog 和审批；
- Planner 提出的工具名、Provider 或并发数只是未授权字符串，不能直接执行；
- Handoff 和 Ledger 的 references 必须经过敏感信息策略。

## 12. 可观测性与评估

每个 Workflow Run 必须能回答：

1. 为什么选择 Multi-Agent，任务数量和并行价值是什么；
2. 每个任务为什么创建、为什么跳过或取消；
3. 哪个 Capability 和 Tool Snapshot 被使用；
4. 证据何时被发现、合并、拒绝、交付或标记冲突；
5. 停止时哪些 Goal 已覆盖、哪些没有覆盖；
6. 多 Agent 比 Single-Agent 多花了多少 token、费用和延迟；
7. 最终答案中的每条重要 claim 由哪些证据支持。

建议新增 Trace 节点：

```text
task_contract.created
task_graph.proposed
task_graph.accepted
task.dispatched
task.completed
task.cancelled
evidence.candidate
evidence.merged
evidence.rejected
evidence.delivered
goal.updated
verification.completed
workflow.converged
```

离线评估至少比较：

```text
answer correctness
required facet coverage
claim support precision
unsupported claim rate
duplicate delivered tokens
tool calls
total cost
time to first useful evidence
end-to-end latency
partial / failed / clarification rate
```

评估集要覆盖：单事实、跨服务、静态架构、实时故障、Web 研究、多轮指代、时间范围、
证据冲突、工具失败和需要写操作的任务。不能只用“复杂问题”验证 Multi-Agent。

## 13. 迁移方案

### 阶段 0：冻结旧行为并补齐观测

保留现有 `delegated.investigation`，但记录：

- Task Contract 缺失哪些字段；
- 三个固定节点的实际新增 Evidence identity；
- 重复证据、无关节点和失败原因；
- 如果按新评估计算，哪些 Run 本可以单 Agent 或不同任务图完成。

这一阶段不修改用户可见结果，只建立对照数据。

### 阶段 1：引入 Task Contract 和 Workflow Ledger

1. 在 QA preparation 完成 canonical entity、history ref、time range、Evidence Goal。
2. 将 `investigation.request` 升级为 `task.contract`，保留旧 Schema 适配器。
3. 将 Evidence Ledger 从单 Agent 机制提升为 Workflow 作用域。
4. 现有三个 Investigator 继续作为三个 Capability Adapter，先不改变节点数量。
5. Join 改为 Ledger View，而不是简单拼接三个 report。
6. 增加 QA Workflow Coordinator，幂等收敛恢复后的 Parent Run、Session Turn 和结果
   事件，并闭合显式取消传播。

验收：多 Agent 与 Single-Agent 使用同一 evidence identity 和预算事实；不同 query
返回同一证据时不会重复交付。

### 阶段 2：Capability Registry 和受约束 Planner

1. 注册 `knowledge.web.research`、`knowledge.memory.recall`、
   `knowledge.runtime.observe` 等能力。
2. Planner 输出 Task Graph Proposal，不再只输出 `strategy/complexity/confidence`。
3. 服务端校验 Task Graph，保留固定默认图作为 Planner 不可用时的显式 fallback。
4. 允许 Optional Task 和 `CollectAvailable`，将缺口传播到结果合同。

验收：Web、Memory、Runtime 不再作为 Multi-Agent 总体硬性排除条件；缺失能力会准确
落在对应任务上，并且不会静默替换 Provider。

### 阶段 3：验证、冲突和自适应扩展

1. 增加 Verifier、Conflict Gate 和 claim-level evidence view。
2. 增加 `max_depth`、`max_rounds`、duplicate ratio 和 no-progress 停止条件。
3. 只允许在 Ledger 发现新 Goal 或冲突时启动受限 Adaptive Expansion。
4. 将 `/api/investigations` 变为管理员调试入口，普通 QA 统一使用 `/api/qa/ask`。

验收：任务失败、证据冲突、无新证据和部分完成均有明确终态；不存在无限 spawn。

### 阶段 4：Feature Delivery 复用

将同一 Task Contract、Capability、Ledger 和受约束 Graph 复用于：

- 需求拆解；
- 方案评审；
- 独立 Coding Task；
- Validation 和 Review。

写任务仍必须使用隔离 Worktree、非重叠写集合、Change Set Review 和 Delivery Gate，
不能因为任务图动态化而放松现有写安全规则。

## 14. 兼容和废弃策略

### 保留

- `internal/agent/workflow` 的确定性编排、DAG 校验、Handoff、预算、恢复和事件；
- Agent Definition、Schema Registry、Tool Snapshot 和权限交集；
- Parent/Child Run 关系和统一 QA Outcome；
- 固定 Workflow 作为 Policy fallback 和离线评估基线。

### 逐步替换

- `ExecutionSuggestion{Strategy, Complexity, Confidence}` 替换为
  `ExecutionAssessment + TaskGraphProposal`；
- `investigator.code/runtime/docs` 来源角色替换为 Capability 实现；
- `investigation.request` 只保留兼容适配，不再作为长期输入合同；
- `evidence.join` 从报告拼接升级为 Ledger merge + coverage calculation；
- `FailFast` 默认替换为按 Evidence Goal 分类的部分失败策略。

### 不采用

- 继续增加 `if source != internal`、`if temporal` 之类问题类别特判；
- 让模型直接返回 Agent ID、Workflow ID、工具权限或并行度；
- 通过 Agent 多数票判定事实；
- 用全文聊天记录替代结构化 Handoff；
- 为每种证据来源复制一套互不共享的检索和去重逻辑。

## 15. 决策清单

本设计需要在实现前确认以下平台决策：

1. Task Planner 是使用现有 QA LLM，还是独立的低成本 Planner Definition；
2. 是否允许 Planner 在一次 Workflow 内提出第二轮任务，默认 `max_rounds` 取值；
3. Runtime Capability 的 freshness、租户隔离和敏感字段策略；
4. `partial` 是否允许直接写入 Session，还是必须用户确认；
5. 哪些 Evidence Goal 属于 QA 的 required facet，哪些属于 optional facet；
6. Workflow Ledger 的存储粒度和最大正文保留策略；
7. Feature Delivery 中哪些 Capability 可写，哪些只能产出 Change Set。

这些是 Policy 和领域合同决策，不应由通用 Orchestrator 在运行时猜测。

## 16. 最终结论

Nasuta 不需要从“固定三个 Agent”跳到“自由 Agent 群”。更可靠的方向是：

```text
稳定入口
  -> 规范化任务
  -> 受约束任务分解
  -> 能力匹配
  -> 有界并行 / 顺序 / 自适应扩展
  -> 统一证据账本
  -> 验证和收敛
  -> 可审计结果
```

这样，多 Agent 是一个由任务结构产生的执行策略，而不是一个把内部证据问题复制成
三个固定角色的模板。现有 Workflow 平台继续提供安全和可靠性底座；QA 和 Feature
Delivery 只负责定义自己的 Task Contract、Evidence Goal、Capability Policy 和结果
合同。这个边界既吸收业内 orchestrator-worker、handoff、graph 和 verifier 的有效
实践，也保留 Nasuta 对权限、预算、恢复、证据和审计的控制力。
