# Nasuta 任务驱动多 Agent 架构设计

[返回设计索引](README.zh-CN.md)

> 状态：目标设计，分阶段实施中
> 更新日期：2026-08-14
> 适用范围：Nasuta QA、Feature Delivery 和通用 Agent Workflow
> 替代基线：[QA 与研发任务多 Agent 路由方案](15-qa-and-feature-delivery-multi-agent-routing-proposal.zh-CN.md)（该文状态为“目标设计，待实施”，但其路由部分已随 12 号文档第一阶段落地为 `internal/agent/qa/route.go`；实施本方案时应同步澄清两文的归属）
> 相关基线：[Nasuta 多 Agent 平台方案](12-multi-agent-platform-proposal.zh-CN.md)（第一阶段已实现）
> 证据 substrate：[QA 证据收敛与检索治理](../proposals/qa-evidence-convergence-and-retrieval-governance.zh-CN.md)（部分实现）

## 实施进度

| 阶段 | 状态 | 已完成 | 剩余工作 |
|---|---|---|---|
| 阶段 0：观测与既有闭环 | 已完成 | QA Parent Run 收敛、取消与恢复；调查 Workflow/Node 的 token、tool、cost、retry 预算 | P1：补 single/multi 成本收益对照指标 |
| 阶段 1：Task Contract 与证据事实 | 已完成 | `task.contract`、canonical entity、source/freshness/minimum coverage、可变长度 Ledger View、canonical evidence identity、Goal Coverage、冲突传播、按 Evidence Goal 裁剪调查节点 | 无 |
| 阶段 2：Capability 与受约束计划 | 已完成 | `CapabilityRegistry`、Web/Memory/Runtime Capability、app 扩展注册、受限 Planner Proposal、服务端字段绑定与 Proposal Compiler、权限/预算/Schema/并发校验、`ExecutionAssessment`、确定性 fallback | 无；多轮修订归入阶段 3 |
| 阶段 3：验证与自适应扩展 | 部分完成 | `CollectAvailable`、Optional Task、Required Goal Coverage、complete/partial/unavailable；独立确定性 Verifier 将 claim 分类为 supported/partial/unsupported，并按 coverage、canonical binding、trust tier 和 Evidence Goal 风险分级；固定调查图与 Proposal Compiler 已插入服务端 `evidence.risk` Gate，高风险 partial 或证据冲突确定性进入 `needs_clarification`，Synthesizer 不会启动；Evidence Join 基于输入 baseline 产出 new identity/duplicate ratio 快照；结构化停止原因一致进入 Result、Trace、Store 与恢复路径；`MaxRounds`/`MaxDepth` 已在节点派发前准入，Run 固化 `Round`/`BaseDepth` 并由 Store/恢复路径复用 | P0：Adaptive Coordinator、跨轮不可变收敛快照串联与累计资源账本 |
| 阶段 4：Feature Delivery 复用 | 未开始 | 现有静态 Workflow、Human Approval 与 Worktree 隔离可复用 | P1：写 Capability、WriteSet 调度隔离、Change Set Review、Delivery Gate、动态任务图 |

当前实现应描述为“**单轮受限任务规划、编译与确定性执行底座完成**”，而不是“完整
任务驱动 Multi-Agent 闭环完成”。

### 剩余功能优先级

优先级定义：

- **P0**：阻断只读 Multi-Agent 闭环正确性、安全性、有界执行或恢复一致性的功能。
  P0 未完成前，不应宣称任务驱动 Multi-Agent 闭环完成，也不应启动动态写任务扩展。
- **P1**：不阻断只读闭环，但影响入口统一、收益评估、兼容清理或 Feature Delivery
  复用的功能。P1 在 P0 闭环稳定后实施。

| 顺序 | 优先级 | 工作包 | 完成标准 |
|---|---|---|---|
| 1 | P0（已完成） | canonical entity 统一 | Task Contract、Retrieval Intent、Memory 使用同一实体标识与有界去重语义，多轮任务不因大小写、函数括号或不同抽取路径重复扩展 |
| 2 | P0（单轮已完成） | 多轮收敛事实与终态 | 单轮 blocked dependency、needs_clarification、真实 no-progress、duplicate ratio、no-affordable 已由运行事实判定，并一致写入 Result、Trace、Store 和恢复路径；最终完成仍要求后续轮次复用同一不可变收敛快照 |
| 3 | P0（运行时准入已完成） | 多轮预算与调度准入 | `MaxRounds` 与全局 `MaxDepth` 已在任何节点派发前生效，`Round`/`BaseDepth` 已持久化并在恢复时复用；`MaxDuplicateRatio` 已固化进收敛快照，Node Attempt 已预留并结算 token/tool/cost/retry 预算。真正跨轮的累计资源账本仍随 Adaptive Coordinator 实现 |
| 4 | P0（已完成） | partial-support/risk policy 与 Gate | claim 已区分 supported/partial/unsupported；覆盖不完整、部分引用无法绑定或高风险证据低于 trust floor 时进入 partial；高风险 partial 或证据冲突由 `evidence.risk` Gate 转为 clarification，不能直接合成 |
| 5 | P0 | Adaptive Expansion 与顺序细化 | 只在新增 Goal、可信实体或冲突时生成有界后续任务；采用新不可变 Workflow 定义与 checkpoint 续跑，并保持预算、取消、恢复和 Trace 语义 |
| 6 | P1 | QA/Workflow preparation 统一 | Single-Agent 与 Workflow 共用 canonical context/evidence preparation，消除重复的 Prefetch、上下文组装和证据采集入口 |
| 7 | P1 | Single/Multi 收益评估 | 提供 token、cost、latency、claim support precision、unsupported claim rate 和扩展轮次收益对照 |
| 8 | P1 | Feature Delivery 复用 | 完成写 Capability、隔离 Worktree、WriteSet 冲突检测、Change Set Review、Delivery Gate 与受限动态写任务图 |
| 9 | P1 | 兼容与文档清理 | 收敛 `investigation.request` 兼容入口，删除已过时的固定 Handoff、旧 ContentHash 和未消费冲突描述 |

P0 的依赖顺序是：

```text
canonical entity
  -> 收敛事实
  -> 预算准入
  -> 风险策略 / Gate（已完成）
  -> Adaptive Expansion
```

Evidence 缺口补查由 Adaptive Expansion 的 Coordinator 创建新任务，不通过给
Synthesizer 直接开放工具实现。

## 1. 摘要

QA Multi-Agent 已从固定五节点图演进为按 canonical Evidence Goal 编译请求级任务图。
Task Contract 已携带 source/freshness/minimum coverage；Nasuta 提供
code/service topology/docs/Web/Memory 能力，CodeLoom 可通过 app 扩展注入实时 Runtime
Observe 能力。当前执行形态是：

```text
问题
  -> Evidence Goal + source/freshness
  -> 请求级 Capability 子集
  -> 受限 Planner Proposal
  -> 服务端字段绑定、校验或确定性 fallback
  -> evidence ledger join
  -> deterministic evidence verifier
  -> evidence risk gate
  -> synthesize
```

`evidence_not_internal_only`、`runtime_evidence_required`、
`history_dependency_required`、`time_resolution_failed` 以及
`PreloadedContext`/`ToolPlan.Prefetch`/`ParentRunID` 前置门已经从
`decideExecutionRoute` 和 `standardQARequest` 移除。显式工具声明
`RoutingEvidenceSource`，因此 temporal freshness 与 Runtime Observe 不再混为一谈。

固定 complexity/confidence 阈值已经移除。服务端 `ExecutionAssessment` 根据 Task
Contract 中可映射的独立 Capability 数量、并行性、共享上下文压力和预计协调规模做
裁决；低 complexity/confidence 不再直接降级。当前保留 Policy、写请求和 Workflow
不可用三类边界，并新增 `insufficient_independent_tasks` 与
`tasks_not_parallelizable` 两个任务结构降级原因。

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

当前固定 Investigation 定义了七个节点：

```text
investigate.code
investigate.runtime
investigate.docs
  -> evidence.join
  -> evidence.verify
  -> evidence.risk
  -> synthesize
```

该图目前仍作为兼容 fallback 存在，但已改为 `task.contract` 输入、
`CollectAvailable`、Optional Investigator、Evidence Ledger Join、显式预算与受限重试；
请求级编译还会按 canonical Evidence Goal 选择实际需要的 Capability；受限 Planner
为每个允许的 Capability 提出任务 ID、具体目的和 required facets，服务端固定绑定
Schema、Optional、尝试次数、Synthesizer 和终止边。当前遗留问题已从“固定图无法降级”
收敛为“跨轮运行期扩展和上游准备统一仍不完整”：

| 问题 | 后果 |
|---|---|
| Planner 仍是单轮并行 fan-out | 当前 `depends_on` 必须为空；尚不能基于 Ledger 缺口修订 Proposal 或创建后续调查轮次 |
| Synthesizer 无工具 | 发现证据缺口后不能补查，只能接受缺口或幻觉补全 |
| 运行期扩展仍是单轮 | `ExecutionAssessment` 已取代固定阈值，Join/Verifier 已有 baseline delta、no-progress 和 duplicate ratio 判定；执行器已能按持久化 `Round`/`BaseDepth` 执行 round/depth 准入，但尚无跨 round 快照串联和新增节点通路 |
| QA 与 Workflow 准备链路仍未完全合并 | Prefetch、PreloadedContext、ParentRunID 已进入 Task Contract，但 Single-Agent 与 Workflow 仍有独立执行入口 |

阶段 1 原有三处硬约束中，固定三元组 Schema 和双去重已完成收敛；当前真正阻断
Adaptive Expansion 的是执行器仍绑定静态节点集：

1. **执行器绑定静态节点集**：`executor_orchestration.go:32` 的主循环终止条件是
   `len(outputs)+len(failedOptional) < len(definition.Nodes)`，`readyNodes` 也只
   遍历该集合；`model.go:201` 在 prepare 期校验 `len(Nodes) <= Budget.MaxNodes`。
   `WorkflowDefinition` 是预注册、带 version 与 `ContentHash` 的不可变定义，**当前
   没有任何“运行中新增节点”的通路**。
2. **已解除固定三元组约束**：`investigation.bundle` 现为 1 到 50 个 Handoff 的
   Ledger View，并显式携带 `unavailable_tasks`、证据、冲突和 completeness。
3. **已统一主要证据合并语义**：QA 和 Workflow 合并路径复用 canonical identity，
   保留来源 `ContentHash`，冲突会进入 Handoff、Ledger、Trace 和确定性拒绝策略。

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

实体身份由 `internal/domain.CanonicalEntityIDs` 在领域入口统一：去除边界空白和函数
括号、转为小写、按 canonical ID 去重并限制为 8 个。Retrieval Intent 使用该入口规范
化 grounded identifiers；Memory 的 `CanonicalQuestionMetadata` 通过
`domain.CanonicalQuestionEntities` 复用同一 identity 与去重规则抽取会话实体；
Task Contract 构造再次建立该输入不变量。三条路径因此对
`PaymentHandler.handle()`、`paymenthandler.handle` 等表示生成同一实体 ID。

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

当前已新增公共 `agent.Capability`、`CapabilityRegistry` 和服务端 Proposal Compiler，
并把默认 Investigator 注册为 `knowledge.code.inspect`、
`knowledge.service.trace`、`knowledge.docs.verify`、`knowledge.web.research`、
`knowledge.memory.recall` 与 `evidence.synthesize`。`app.Extension` 可通过
`AgentCatalogProvider` 原子注入 Definition 与 Capability；CodeLoom 仅在
`observe_logs` 成功发布后注册 `knowledge.runtime.observe`，Provider 失败不会静默
替换实现。`investigation.report` 已允许通用 focus 字符串，具体任务边界由 Capability
与 `TaskDirective.RequiredFacets` 决定。
（`semantic.Capabilities`、`retrieval.RoutingCapabilities`、
`delivery.CodingCapabilityStatus` 与此无关，勿复用其命名空间。）

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

当前 QA Planner 使用已有的 fast preparation LLM，仅输出 Investigator 的 `id`、
`purpose`、`capability`、`required_facets` 和空 `depends_on`。服务端要求 Planner
恰好覆盖 Task Contract 推导出的 Capability/facet 集，随后固定添加
`investigation.report`、Optional、最大尝试次数、`evidence.synthesize` 和终止边；
Planner 不能控制 Output Schema、Agent/Provider、工具、权限、预算、并发组、重试或
Stop Policy。结构化规划失败会记录 degraded `agent.task_graph_plan` 并回退到
`DelegatedInvestigationProposalForGoals`；Proposal Compiler 拒绝模型计划时会记录
rejected `task_graph.proposed`，再显式编译同一确定性 fallback。

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

现有 `runEvidenceLedger`（`internal/agent/execution/evidence_ledger.go`）已经实现了
稳定 identity、同 hash 去重和异 hash 记冲突，本节沿用其字段而非另起一套。Ledger
记录必须保留当前 `tool.EvidenceUnit` 中在用且承重的字段，否则是能力回退：
`TrustTier` 与 `EvidenceClass` 用于 claim 支持度判定，`TokenCost` 用于预算结算，
`Sections` 是 identity 的组成部分。

```go
// WorkflowEvidenceRecord 是 Workflow 级 Ledger 条目。
// 命名避免与 internal/agent/run.EvidenceStatus（回答级证据充分性）冲突。
type WorkflowEvidenceRecord struct {
    Identity       EvidenceIdentity      // 五元组，见下
    ContentHash    string
    Coverage       tool.EvidenceCoverage // 沿用现有类型，非 []CoverageClaim
    Facets         []string
    TrustTier      int
    EvidenceClass  string
    TokenCost      int
    Authority      Authority
    Delivery       EvidenceDelivery      // candidate | delivered
    ProducerTaskID string
    References     []agentapi.Reference
}

// EvidenceIdentity 与现有 evidenceKey 五元组一致，不新增也不删减维度。
type EvidenceIdentity struct {
    SourceKind string
    Target     string
    Section    string
    Version    string
    TimeRange  string
}
```

规则：

- Identity 来自来源适配器提供的稳定标识，不使用 query hash 或语义相似度代替。
- 相同 identity、相同 hash 只保留一个可交付正文。
- 相同 identity、不同 hash 记录版本冲突，不静默覆盖。
- `delivered` 只表示实际进入模型上下文的证据；召回但未交付的内容保持 candidate。
  当前代码没有该状态维度，`Delivery` 是本方案新增字段。
- 子 Agent 只能提交新增 Evidence Item 或对已有 identity 的结构化更新。
- Join、Verifier 和 Synthesizer 使用同一 Ledger View，不重新拼接重复正文。

**升格前置条件（阻塞项）**：当前 `ContentHash` 有两套互不兼容的语义——执行层使用
工具返回的原始哈希，QA 层 `appendQAEvidenceUnits`
（`internal/agent/qa/tools.go:363-372`）覆写为**截断后投递文本**的哈希并重算
`TokenCost`。两者 identity 五元组相同，因此
同一份证据经不同投递路径会被判为版本冲突。必须先统一哈希语义（确定 `ContentHash`
指代来源正文还是投递正文，并在两处共用同一实现），再把 Ledger 扩展到 Workflow
作用域；否则跨 Agent 合并只会放大误判。同时应让 `conflicts` 真正被消费——当前它被
写入却从未被读取，冲突实际是静默丢弃的。

本节的证据 substrate（`EvidenceUnit`/`EvidenceScope`/`EvidenceCoverage`、run 级
Ledger、准入与投递门）来自 `qa-evidence-convergence-and-retrieval-governance`
（部分实现）。`qa-unified-evidence-acquisition-pipeline` 仍是“提案，待实现”且明确
标注迁移切片尚未开始，因此本方案不以其结论为既有事实，只在目标形态上与之一致。

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
| `evidence_not_internal_only` | 为 Web、Memory、Runtime 注册相应 Capability；没有能力时只阻断缺失任务或返回 partial，不把“多 Agent”本身视为非法 |
| `runtime_evidence_required` | **该条件实际判定的是“路由选中了任一 `ToolRouteCandidate.Temporal == true` 的工具”**（`route.go:155-171`），即时间敏感读取，与运行时日志观测不是同一概念。新语义：temporal 只是一种 freshness 约束，由 Task 的 `FreshnessPolicy` 表达，并对该任务施加时间范围与缓存策略；它不映射到 `knowledge.runtime.observe`（后者是日志/指标/Trace 观测能力，两者需分开建模） |
| `history_dependency_required` | 先由 Context Resolver 将历史依赖解析为实体和 EvidenceRef，再作为 Task Contract 输入 |
| `time_resolution_failed` | Time Resolver 是前置规范化节点；无法解析时，只有依赖该时间范围的任务阻断，静态任务可继续，或者要求澄清 |
| `standardQARequest` 前置门 | `PreloadedContext`/`ToolPlan.Prefetch`/`ParentRunID` 非空当前直接禁用多 Agent。新语义：这些是 Task Contract 的 `SeedEvidence` 与 `ConversationRefs` 输入，应作为已有证据参与规划，而不是排除条件 |

### 6.3 保留或需要单独演进的降级条件

固定阈值出口已经删除；当前保留或新增的降级出口如下：

| 降级条件 | 处理 |
|---|---|
| `policy_disallows_multi_agent` | 保留。这是显式 Policy 开关，属于 3.2 非目标中“不把所有请求强制转换成多 Agent” |
| `insufficient_independent_tasks` | 新增。Task Contract 无法映射出至少两个独立 Capability 路径时使用 |
| `tasks_not_parallelizable` | 新增。存在多个任务但 Planner 明确给出强顺序约束时使用 |
| `workflow_unavailable` | 保留。属于 6.4 的 Provider/能力不可用类 |
| `write_requested` | **需要单独设计，本方案不予放宽。** 当前只要 `AllowWrite` 即降级单 Agent；而 13 节阶段 4 要把 Feature Delivery 建在同一套任务图上，二者直接冲突。阶段 4 之前必须先明确：写任务的隔离 Worktree、非重叠写集合、Change Set Review 与 Delivery Gate 如何在动态任务图下保持现有强度。在该设计完成前，`write_requested` 保持为硬性降级 |

`ExecutionAssessment.Reasons` 复用现有的封闭白名单机制：
`executionReasonCodes` 已限定 11 个合法 model reason code 且最多 4 条，由
`bindExecutionSuggestion` 校验；服务端结构降级原因不接受模型输入。

### 6.4 仍然必须阻断或暂停的情况

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

**与当前执行器的兼容性（实施前必须决策）**：现有执行器无法在运行中新增节点。
`executor_orchestration.go:32` 的循环终止条件绑定在
`len(definition.Nodes)` 上，`readyNodes` 只遍历该固定集合，且
`WorkflowDefinition` 是带 `ContentHash` 的不可变预注册定义。因此 Adaptive
expansion 必须在以下两条路径中选择一条，14 节的“保留现有确定性编排”不足以覆盖：

1. **每轮生成新的不可变定义 + checkpoint 续跑**（推荐）：每轮规划产出一个新版本的
   `WorkflowDefinition`（节点集为上一轮的超集），复用现有
   `LoadFullRunState` / `workflowProgressFromState` / `Service.Resume`
   （`service_checkpoint.go`、`service_recovery.go`）把已完成节点的 Handoff 视为
   已有进度继续执行。优点是执行器语义不变、恢复路径已存在；代价是每轮一次定义
   注册与哈希。
2. **执行器支持可增长节点集**：改造循环终止条件与 `readyNodes`，引入
   `max_rounds` 与代际标记。改动落在编排核心，需重新验证恢复、预算与取消语义。

无论选哪条，`investigation.bundle` 那类固定长度 Schema（见 2.1）都必须先改为
可变长度的 Ledger View，否则 Optional Task 与新增任务在 Join 处即失败。

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

这一点在当前实现中尤为重要：现有 `investigator.runtime` 的工具集是
`get_service`/`trace_deps`/`list_apis`/`trace_calls`
（`catalog/defaults_investigation.go:23-27`），**全部是索引化的静态拓扑读取，没有
任何实时日志能力**——它名为 “Runtime Topology Investigator” 而非 live runtime
observer。真正的日志观测工具 `observe_logs` 注册在 **CodeLoom**
（`internal/observe/readtool.go`）而非 Nasuta，因此 `knowledge.runtime.observe`
必须由 app 层跨模块注入其工具实现，Nasuta 只持有能力声明与权限域。不得因为存在
`investigator.runtime` 就认为该能力已具备。

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

**当前已完成单轮收敛事实层**：Evidence Join 以 Workflow 输入中的 canonical evidence
identity 为 baseline，输出 `candidate_count`、`new_identity_count`、
`duplicate_count`、`duplicate_ratio` 和服务端 `max_duplicate_ratio`。baseline 中已有的
identity 不计为候选；多个 Investigator 重复提交同一新增 identity 才计入 duplicate。
Verifier 只依据该结构化快照、Required Goal Coverage、冲突和 unavailable task 的停止原因
收敛，不再把任意 `partial` 自动解释为 `no_new_evidence`。该快照当前只覆盖单轮 fan-out；
跨轮快照串联和下一轮任务生成仍归 Adaptive Expansion。

### 9.2 Verifier 与 Synthesizer 分离

Synthesizer 只负责把已经通过 Ledger 选择和验证的材料组织成 Result Contract。对于
高风险任务，先运行确定性或专用 Verifier：

```text
evidence ledger
  -> claim extraction
  -> verifier
  -> evidence risk gate
  -> approved evidence view
  -> synthesizer
```

Verifier 的输出必须说明：

- 哪些 claim 被支持；
- 哪些 claim 只有 partial support；
- 哪些证据互相冲突；
- 哪些限制必须出现在最终回答。

不采用“多个 Agent 说得一样就算正确”的多数票方案。

**当前确定性 Verifier 与风险 Gate 主链已落地**：Proposal Compiler 在最终 Evidence
Join 与唯一 Synthesizer 之间自动插入服务端拥有的 `NodeVerifier` 和
`evidence.risk` `NodeGate`，Planner 不能控制其 ID、Schema、权限、超时、trust floor
或 Gate policy。Verifier 将 `investigation.bundle` 收敛为
`investigation.verified_bundle`，输出 `supported_claims`、`partial_claims`、
`unsupported_claims`、`partial_goals`、`unresolved_goals`、`limitations`、合并证据、
冲突和 verification decision。

每个 supported/partial claim 保留 producer node、finding index、goal IDs、过滤后的
具体引用证据和去重后的 canonical `evidence_identities`。显式 identity 必须按
`source_kind/target/section/version/time_range` 五元组精确命中 Ledger；兼容报告中没有
identity 的引用只按 `kind/reference` 匹配同一 `source_kind/target`。覆盖不完整、部分
引用无法绑定，或高风险 claim 的最低证据 trust tier 低于服务端 floor 时标为 partial。
完全无法绑定 canonical evidence 的 finding 只进入不含 claim 文本的
`unsupported_claims` metadata，不贡献 Required Goal Coverage。

QA preparation 将 `bounded_live` Evidence Goal 标记为 high risk；当前调查策略的
high-risk 最低 trust tier 为 1。`evidence.risk` 对低风险 partial 原样放行 verified
bundle；发现任意证据冲突或 high-risk partial 时写入结构化 `GateDecision` 并以
`needs_clarification` 收敛，Synthesizer 不启动。Gate 成功时原样转发 payload、
references、canonical evidence、conflicts 和 completeness。通用 human approval policy
仍可作为后续场景扩展，但不再阻断当前只读调查闭环。

### 9.3 部分失败

默认使用 `CollectAvailable`，但是否允许形成最终答案由 Evidence Policy 决定：

- 非必需任务失败：继续，并在 limitations 中保留缺口；
- 必需任务失败但其他证据足够：返回 partial，不能返回 complete；
- 必需任务失败且没有替代覆盖：Workflow failed 或 needs clarification；
- 一个 identity 多版本冲突：由 Verifier 保留冲突事实，并由 risk Gate 转为
  clarification，不能静默选择。

这比改造前四条边全部 `Required: true`、五个节点均非 `Optional`、任一失败即整体失败
更符合实际调查任务；当前固定调查图已按该规则启用部分失败收敛。

**本节主链路已落地**：`CollectAvailable` 与 `NodeDefinition.Optional` 已在执行器中生效
（`executor_orchestration.go:57-60`：降级要求节点 `Optional` **且** 工作流为
`CollectAvailable`），`readyNodes` 也已实现“失败的 Optional 前驱位于 `Required` 边上
则后继不可运行”（`executor_handoff.go:53-58`）。`Handoff.Completeness` 已有
`complete/partial/unavailable` 三态并在 Join 处按 `Unavailable > Partial > Complete`
折叠。固定工作流已使用 `CollectAvailable`、Optional Investigator 和可变长度
`investigation.bundle`；独立 Verifier 现已根据请求级 Required Goal Coverage 生成
verified view，并允许 partial/unavailable 以明确 limitations 进入最终合成。后续仍需由
场景 Evidence Policy 决定风险级别；当前 QA 已将 `bounded_live` Goal 标为 high risk，
并由 `evidence.risk` Gate 把 high-risk partial 或证据冲突转为 clarification。低风险
partial 仍可携带限制进入最终合成。

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

类型层面，`WorkflowBudget` 已覆盖 nodes、parallelism、`MaxDepth`、`MaxRounds`、
timeout、handoff bytes、`MaxDuplicateRatio`、input/output/total tokens、tool calls、
cost micros 和 retries。`StopPolicy` 的 round/depth/duplicate 限制只能向下收紧服务端
ceiling，固定调查场景也会从场景 Policy 派生这些值。

运行层面的既有预算维度已经启用：`DelegatedInvestigationBudgetPolicy` 从不可变 Agent
Definition 派生 Node Budget，Workflow Budget 汇总 token、tool call、cost 与 retry
上限，Agent 节点使用受限重试。Evidence Join 把 `MaxDuplicateRatio` 固化进 convergence
snapshot，Verifier 在未满足 Required Goal 时据此产生
`duplicate_evidence_limit`。`WorkflowDefinition.Prepare` 计算稳定拓扑深度；每个 Run
固化 `Round`/`BaseDepth`，执行器在创建任何 Node Attempt 前校验
`Round <= MaxRounds` 和 `BaseDepth + graphDepth <= MaxDepth`，并把全局 round/depth
写入 Node trace。该位置同时进入 `workflow_runs`，恢复后重新执行同一准入。剩余工作是：

1. 在跨轮 Coordinator 中将当前 new-identity/duplicate snapshot 用作下一轮派发准入；
2. 为每轮扩展采用新的不可变定义和 checkpoint 续跑，保持恢复语义不变；
3. 将各轮 token/tool/cost/retry 预留和实际用量汇入同一累计账本，不能让新 Run
   重新获得完整 Workflow ceiling。

预算在调度前预留，在结果入账后按实际使用结算；不能只在 Workflow 结束后统计。当前
Workflow 已由并发安全的 budget account 在每个 Node Attempt 前执行 `Reserve`、结束后
执行 `Settle`。派发前无法预留时产生 `no_affordable_task`，不会伪装成已执行后的预算
耗尽；round/depth 准入失败，或实际用量超过节点预留或 Workflow ceiling 时产生
`budget_exhausted`。并行 wave 共享同一预留账本，不能同时超卖。单 Agent 侧仍保留
`tool_admission.go` 的上下文窗口准入、`tool_delivery.go` 的投递前模拟和
`run/store_usage.go:RecordLLMCall` 的事务结算。

注意区分两个易混概念：`AnswerReserve` 是 **wall-clock `time.Duration`**（默认 30s，
`platform/config/platform.go:14`），唯一用途是
`context.WithTimeout(runCtx, Timeout - AnswerReserve)`（`execution/loop.go:238`）；
token 侧的答案保留量是 `LLMAnswerMaxTokens` → `outputReserve`。多维预算讨论中不得
将二者混用。

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

`StopReason` 已使用上述封闭枚举。当前单轮执行会实际产生全部列出的结构化收敛原因：

- `required_goals_covered`：Verifier 确认 Required Goal 全部有 canonical evidence；
- `no_new_evidence`：存在 convergence snapshot，且相对输入 baseline 的
  `new_identity_count == 0`；
- `no_affordable_task`：Node Attempt 派发前无法完成预算预留；
- `duplicate_evidence_limit`：未满足 Required Goal，且实际 duplicate ratio 超过服务端
  ceiling；
- `needs_clarification`：`GateDecision.Decision` 明确为 `needs_clarification`，不解析自由
  文本；
- `verification_failed`、`deadline_exceeded`、`budget_exhausted` 和
  `capability_unavailable`：分别来自显式启用 verifier hard-reject 的场景、deadline、
  round/depth 准入或执行后结算超限，以及能力/依赖事实；默认调查图的证据冲突由
  `evidence.risk` 转为 `needs_clarification`。

Required 边被失败的 Optional 前驱阻断时，会传播该前驱已持久化的真实停止原因；没有更
具体事实时才归为 `capability_unavailable`。这些原因统一投影到 Verifier handoff、内存
`Result`、`workflow.converged` Trace 和 `workflow_runs.stop_reason`。checkpoint 会恢复
Optional failure reason 和 Gate decision，因此重启后保持相同终态；取消、等待人工审批和
未分类内部失败不伪造收敛原因。已有安装需执行
`docs/sql/migration_workflow_stop_reason_20260814.sql` 和
`docs/sql/migration_workflow_execution_position_20260814.sql`。

尚未完成的是把同一事实合同串联到真正的多轮 Coordinator：每一轮必须以上一轮不可变
snapshot 为 baseline，并在创建下一轮定义前复用现有 round/depth 准入，同时执行
duplicate、affordability 和跨轮累计资源准入。

### 10.3 重试与补偿

- Provider transient failure：按 Node RetryPolicy 重试，消耗 Workflow retry budget；
- capability permanent failure：不替换 Provider，标记该 Capability unavailable；
- Agent 输出 Schema 错误：只允许受限修复或一次重新执行，不把无效文本当作 Handoff；
- 超时：取消子 Run，保留已提交的 Handoff，按失败策略判断是否继续；
- Coordinator 失败：从最后一个不可变 Planner/ Ledger Snapshot 恢复，而不是从模型
  对话恢复。

### 10.4 Parent Run 收敛与取消

> **当前实施状态：已完成。** 本节保留实施前的问题基线，并记录当前必须持续成立的
> 生命周期约束。

实施前有两处具体缺陷：

1. **收敛逻辑活在请求派生的 goroutine 里**：`internal/agent/qa/submission.go:45-79`
   在 `go func()` 中依次执行 `investigation.Run` → `persistSessionTurn` →
   `archiveSessionHistoryAsync` → `scenario.Finish`。进程中途退出即全部丢失。
2. **启动恢复没有 QA 侧收敛器**：`app/server.go:85-100` 的恢复观察者调用的
   `ReconcileRecoveredRun` 属于 `*reviewworkflow.Coordinator`
   （`app/platform.go:73`，Feature Delivery 专用），且为 nil 时直接跳过。**QA parent
   run 在启动恢复路径中完全没有对应的收敛器**——进程中途死亡的 QA parent run 永不
   收敛，Session Turn 也永不落库。

此外 `submission.go:45` 的 `context.WithoutCancel` 在 `Execute` 之前就切断了取消链，
使 Workflow 侧 `registerActive(detached=false)`（`service_active.go:9-50`）失效，
用户显式取消无法传播到进行中的调查。

当前实现已经建立以下闭环：

- `internal/agent/qa/submission.go` 先持久化 QA Parent，再启动独立 Workflow；
  请求 goroutine 不再拥有 Parent 的终态事实。
- `internal/agent/workflow/service_execution.go` 的 `Start` 使用 detached Run Context
  完成首次持久化和后台执行；客户端断开不会取消持久任务。
- `internal/agent/qa/investigation_coordinator.go` 只根据 durable Workflow terminal
  facts 收敛 Parent，并以 Parent `run_id` 幂等写入 Session Turn。
- `app/server.go` 按持久化 Workflow `scenario` 分发恢复结果，并额外分页扫描启动前仍
  active 的 QA Parent；可恢复的 Child Workflow 不再被通用 interrupted-run 修复误杀。
- `internal/agent/workflow/service_approval.go` 与 `service_active.go` 先事务提交 Workflow、
  活动 Node 和取消事件，再取消本进程内执行；QA Coordinator 随后使用 detached context
  收敛 Session 与 Parent。
- `internal/agent/run/store_lifecycle.go` 和 `store_events.go` 在同一事务中提交 Parent
  终态及其唯一终态事件，重放收敛读取已持久化结果，不重复追加终态事实。
- `app/investigation.go` 将 Workflow EventHub 仅作为实时投影，并通过
  `AwaitTerminal` 的 durable completion 关闭桥接；即使终态 fan-out 事件丢失，订阅也
  不会永久悬挂。

实现后的收敛链路为：

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
- SSE 只是投影，不是终态事实源，客户端重连从持久化 Run 和 Event 读取。注意
  `EventHub` 是 32 槽缓冲的进程内 fan-out 且**满时静默丢事件**
  （`workflow/events.go:39-42`），只有 `store_events.go` 的行是持久事实；
- 客户端断开不取消持久任务；用户显式取消必须从 Parent 传播到 Workflow 和活动 Child
  Run，并记录统一的取消原因。这要求移除或下移 `submission.go:45` 的
  `context.WithoutCancel`，改由 Workflow 的 detached 语义统一裁决；
- Parent 不得在 Child Workflow 仍可恢复时被启动修复逻辑直接标记为 `aborted`；
- 进行中的 Workflow 从持久化状态恢复。当前恢复机制是 checkpoint 重建
  （`LoadFullRunState` → `workflowProgressFromState` → `Service.Resume`），
  workflow 包中并不存在 `Snapshot` 类型；本方案沿用 checkpoint 语义，不新造概念，
  也不依赖热加载锁或瞬时 `context.Context` value。

上述约束已有 `internal/agent/qa/investigation_coordinator_test.go`、
`internal/agent/workflow/service_test.go`、`internal/agent/run/store_test.go`、
`app/server_test.go` 和 `app/investigation_test.go` 覆盖。后续演进不得重新把 Parent
终态、Session 写入或订阅退出绑定到易丢失的进程内事件。

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

目标 Trace 节点：

```text
task_contract.created
agent.task_graph_plan
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

这些 span 应通过既有的 `runtrace.Spec`/`runtrace.Invoke` 机制注册，与现有 49 个
operation 共用同一 trace 合同（`internal/tracecontract`，当前为 `v1`），不另建通道。
当前已落地 `evidence.candidate`、`evidence.merged`、`evidence.rejected`、
`evidence.delivered` 和 `workflow.converged`：Join 边界可观测候选、Ledger 合并、
冲突拒绝与聚合交付，Workflow 执行边界在最后记录统一终态。`workflow.converged`
投影不可变 definition 标识、节点完成/不可用/等待计数、usage、completeness 和稳定
outcome；当前覆盖 `complete`、`partial`、`unavailable`、`waiting_human`、
`evidence_conflict`、`failed`、`cancelled` 和 `timed_out`。其中 `partial` /
`unavailable` 使用 `degraded` Trace status，避免与真正失败混淆。

Evidence Trace 只投影 canonical identity、content hash、origin、数量和 completeness，
不记录证据正文或 Handoff payload；每类事件最多投影 50 条 identity，避免挤占现有
Trace 上限。独立 `NodeVerifier` 记录真正的 `verification.completed`，投影 decision、
结构化 stop reason、supported/partial/unsupported claim、unresolved goal 和 conflict
数量；显式 verifier hard-reject 时使用 failed，partial/unavailable 使用 degraded，且不
记录 claim、引用正文或 Handoff payload。`evidence.risk` 的 `workflow_node` Trace 只投影
`gate_decision`、去重后的 `gate_reason_codes` 和 `gate_finding_count`，不投影 verified
bundle。
Goal Coverage 已由 `covered_goals`/`unresolved_goals`、evidence-backed finding 和
`minimum_coverage` 决定 Handoff completeness，最终 verified view 也可回答哪些 claim
由哪些具体引用和 canonical Evidence Identity 支持。Verifier 绑定失败与部分绑定的
limitation 只记录 producer node、finding index 和数量，不进入 claim、引用正文或
Handoff payload。第 7 问已经同时具备 canonical identity 级归因和
supported/partial/unsupported 支持强度。多 Agent 侧已有
`workflow.node.execute`、`multi_agent.dispatch`、
`multi_agent.child_run`、`multi_agent.aggregate` 四个 span。请求级 Proposal 编译还会记录
`agent.task_graph_plan`、`task_graph.proposed` 与 `task_graph.accepted`：规划节点投影
允许的 Capability、任务 ID/facet、接受状态和确定性 fallback，不记录问题、目的或
SeedEvidence 正文；`task_graph.proposed` 投影任务 ID、Capability、facet、依赖和
StopPolicy，并记录服务端拒绝；`task_graph.accepted` 记录不可变 Workflow
ID/version/hash、节点绑定和收紧后的预算。三者均不记录 Task purpose 等 Planner
自由文本。

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

以下两项与后续阶段解耦的修复已经完成：

1. **QA Parent Run 收敛与取消（见 10.4）**——已由 QA Coordinator、durable terminal
   facts、启动恢复分发和显式取消传播闭环；
2. **为 `delegated.investigation` 填充既有 token/tool/cost/retry 预算（见 10.1）**
   ——已由 `DelegatedInvestigationBudgetPolicy` 从不可变 Agent Definition 派生并写入
   Node/Workflow Budget，Agent 节点使用受限重试。

### 阶段 1：统一证据事实并引入 Task Contract

本阶段的 Ledger、Schema 和 Task Contract 主链路已完成，剩余工作集中在 canonical
entity 的统一和更细粒度 coverage：

1. **已完成**：统一主要证据 identity / `ContentHash` 合并语义，并让冲突进入
   Handoff、Ledger、Trace 和拒绝策略。
2. **已完成**：`investigation.bundle` 改为可变长度 Ledger View，显式表达
   `unavailable_tasks`、证据冲突和 completeness。
3. **已完成**：QA preparation 已生成 history ref、time range 和 Evidence Goal；
   `domain.CanonicalEntityIDs` 统一 Retrieval Intent、Memory 和 Task Contract 的
   canonical ID、大小写、函数括号、有界去重语义，Memory 不再维护第二套实体 identity。
4. **已完成**：新增 `task.contract`，可传实体、时间范围、ConversationRefs、
   SeedEvidence 和 Evidence Goals；保留 `investigation.request` 兼容 Schema。
5. **已完成**：Evidence Ledger 提升到 Workflow Join 作用域。
6. **已完成**：三个 Investigator 作为 Capability Adapter，并可按请求目标裁剪。
7. **已完成**：Join 使用 Ledger View，不再简单拼接固定三个 report。

验收：多 Agent 与 Single-Agent 使用同一 evidence identity 和预算事实；不同 query
返回同一证据时不会重复交付；同一份证据经不同投递路径不再被判为版本冲突。

### 阶段 2：Capability Registry 和受约束 Planner

底层工具已就绪，本阶段以注册与合同工作为主，不新建执行能力：`web_search` /
`web_fetch`（`internal/agent/tools/registry.go:247`）、memory recall
（`agent.memory_recall` 链路）、以及 CodeLoom 侧的 `observe_logs` 均已存在。

1. **已完成**：注册 `knowledge.web.research`、`knowledge.memory.recall`、
   `knowledge.runtime.observe` 等能力。其中 `observe_logs` 注册在 **CodeLoom**
   而非 Nasuta，该能力必须由 app 层跨模块注入，Nasuta 不得内置其领域实现
   （与 3.2 “不让通用 Workflow 层解析领域私有格式”一致）。
2. **已完成**：Planner 在 `ExecutionAssessment` 选中 Multi-Agent 后输出受限
   Task Graph Proposal，不再只输出 `strategy/complexity/confidence`。模型只控制
   Investigator 的任务 ID、目的、Capability 和 required facets；Capability/facet 必须
   与 Task Contract 推导集合完全一致，当前单轮 fan-out 不接受非空 `depends_on`。
   reason code 继续使用现有 `executionReasonCodes` 白名单，而非自由字符串。
3. **已完成**：服务端校验 Task Graph，保留确定性目标到 Proposal 的显式 fallback。
4. **已完成**：启用 Optional Task 与 `CollectAvailable`。
5. **已完成**：解除 `standardQARequest` 前置门，将 `PreloadedContext` / `ToolPlan.Prefetch` /
   `ParentRunID` 改为 Task Contract 的 SeedEvidence 与 ConversationRefs 输入。
6. **已完成**：以 `ExecutionAssessment` 取代固定 complexity/confidence 阈值。

验收：Web、Memory、Runtime 不再作为 Multi-Agent 总体硬性排除条件；缺失能力会准确
落在对应任务上，并且不会静默替换 Provider。

### 阶段 3：验证、冲突和自适应扩展

1. **独立 Verifier 与风险 Gate 主链已完成**：delegated investigation 固定图与
   Proposal Compiler 都在 Evidence Join 后插入服务端拥有的 `NodeVerifier` 和
   `evidence.risk` `NodeGate`。Join 只负责 Ledger merge；Verifier 负责 Required Goal
   Coverage、claim 到 canonical `EvidenceIdentity` 的绑定、coverage/trust 分级和
   limitations，输出 supported/partial/unsupported。无法绑定 Ledger identity 的 finding
   不再覆盖 Required Goal，且 unsupported metadata 不暴露 claim；部分绑定只保留已验证
   引用。Gate 对低风险 partial 原样放行，对 high-risk partial 或证据冲突确定性产生
   `needs_clarification`，此时 Synthesizer 不启动。Verifier 与 Gate 分别记录
   `verification.completed` 和不含 payload 的节点决策 Trace。
2. **单轮结构化收敛事实与终态已完成，多轮串联待实现**。Evidence Join 已相对 Workflow
   输入 baseline 计算 new identity 和 duplicate ratio；Verifier 据此产生真实
   `no_new_evidence` / `duplicate_evidence_limit`；预算预留失败产生
   `no_affordable_task`；结构化 Gate 产生 `needs_clarification`。这些原因进入 Result、
   `workflow.converged`、`workflow_runs.stop_reason` 和 checkpoint 恢复。静态 Proposal
   编译与运行时均已校验 round/depth ceiling；Run 固化的 `Round`/`BaseDepth` 会持久化并
   在恢复后复用，越界在任何节点派发前确定性收敛为 `budget_exhausted`。当前仍固定为
   单轮且没有运行期扩展；后续需跨轮 snapshot、下一轮定义和累计资源账本。
3. 按 7.2 的决策落地受限 Adaptive Expansion，且只允许在 Ledger 发现新 Goal 或冲突时
   启动。**这是本方案工作量最大的一项**：现有执行器绑定静态节点集，需在“每轮生成新
   不可变定义 + checkpoint 续跑”与“改造执行器支持可增长节点集”之间先行选定路径。
4. `evidence.candidate/merged/rejected/delivered`、独立 Verifier 的
   `verification.completed`、risk Gate decision 和统一终态 `workflow.converged` 已完成；
   单轮 claim support、no-progress/duplicate/affordability/clarification 决策已进入同一
   trace 合同。后续只需继续纳入跨 round 决策。

验收：任务失败、证据冲突、无新证据和部分完成均有明确终态；不存在无限 spawn。

### 阶段 4：Feature Delivery 复用

前置条件：6.3 所述的 `write_requested` 演进设计必须先完成。当前只要 `AllowWrite`
即降级单 Agent，本阶段与之直接冲突，不能默认放宽。

将同一 Task Contract、Capability、Ledger 和受约束 Graph 复用于：

- 需求拆解；
- 方案评审；
- 独立 Coding Task；
- Validation 和 Review。

写任务仍必须使用隔离 Worktree、非重叠写集合、Change Set Review 和 Delivery Gate，
不能因为任务图动态化而放松现有写安全规则。现有写动作目录是封闭的两项
（`internal/writeaction/catalog.go:11-12`：`propose_branch`、`propose_commit`，
均为 incident 域且经审批），扩展需按 09 号文档独立设计。

## 14. 兼容和废弃策略

### 保留

- `internal/agent/workflow` 的确定性编排、DAG 校验、Handoff、预算、事件，以及
  checkpoint 恢复（`LoadFullRunState`/`workflowProgressFromState`/`Service.Resume`）；
- 已在调查工作流启用的机制：`CollectAvailable` + `NodeDefinition.Optional`、
  `NodeBudget`、`Handoff.Completeness` 三态、Evidence Join、独立 Verifier 和
  `evidence.risk` Gate；
- 已存在但尚未用于调查图通用验证的机制：`NodeHumanApproval`、`NodeTransform`；
- `NodeGate` 与 `GateDecision` 已用于调查图的 high-risk partial / conflict policy；
- Agent Definition、Schema Registry、Tool Snapshot 和权限交集；
- Parent/Child Run 关系和统一 QA Outcome；
- 固定 Workflow 作为 Policy fallback 和离线评估基线。

注意“保留确定性编排”不等于“执行器无需改动”：运行中新增节点当前不被支持，
见 7.2 的两条路径决策。

### 逐步替换

- `ExecutionSuggestion{Strategy, Complexity, Confidence}` 的固定阈值已由
  `ExecutionAssessment` 替换；选中 Multi-Agent 后由受限 Planner 直接生成
  `TaskGraphProposal`，无效输出显式回退确定性 goal mapping；
- `investigator.code/runtime/docs` 来源角色已替换为 Capability 实现，并支持
  Web、Memory 和 app 扩展注入的 Runtime Observe；
- `investigation.request` 已只保留兼容用途，长期输入已切换为 `task.contract`；
- `investigation.bundle` 已从固定三元组替换为可变长度 Ledger View；
- `evidence.join` 已升级为 Ledger merge，`evidence.verify` 已生成 canonical identity
  级 supported/partial/unsupported claim 支持视图，`evidence.risk` 已接管高风险
  clarification；
- 固定工作流已使用 `CollectAvailable` 配合 Optional Investigator；
- QA 与 Workflow 已复用 canonical evidence merge，仍需继续统一 preparation 入口。

### 不采用

- 继续增加 `if source != internal`、`if temporal` 之类问题类别特判；
- 让模型直接返回 Agent ID、Workflow ID、工具权限或并行度；
- 通过 Agent 多数票判定事实；
- 用全文聊天记录替代结构化 Handoff；
- 为每种证据来源复制一套互不共享的检索和去重逻辑。

## 15. 决策清单

本设计需要在实现前确认以下平台决策：

1. **已确认**：当前 Task Planner 使用现有 QA preparation fast LLM，调用失败或输出
   非法时回退确定性 goal mapping；是否独立为低成本 Planner Definition 仅作为后续
   成本优化项；
2. 是否允许 Planner 在一次 Workflow 内提出第二轮任务，默认 `max_rounds` 取值；
3. Runtime Capability 的 freshness、租户隔离和敏感字段策略；
4. **已确认**：低风险 `partial` 可携带 limitations 进入合成并写入 Session；
   high-risk `partial` 由 `evidence.risk` 转为 `needs_clarification`；
5. 哪些 Evidence Goal 属于 QA 的 required facet，哪些属于 optional facet；
6. Workflow Ledger 的存储粒度和最大正文保留策略；
7. Feature Delivery 中哪些 Capability 可写，哪些只能产出 Change Set。

以下三项是结构性决策，直接决定实施路径与工作量，须最先确认：

8. **Adaptive Expansion 的执行路径**（见 7.2）：采用“每轮生成新不可变定义 +
   checkpoint 续跑”，还是改造执行器支持可增长节点集。这决定阶段 3 是配置工作还是
   编排核心改造；
9. **`ContentHash` 的权威语义**（见 4.4）：指代来源正文还是投递正文。两处现有实现
   各持一种，必须先定其一才能合并去重并升格 Ledger；
10. **`write_requested` 的演进方案**（见 6.3）：写任务在动态任务图下如何保持隔离
    Worktree、非重叠写集合与 Delivery Gate 的现有强度。阶段 4 的前置条件。

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
