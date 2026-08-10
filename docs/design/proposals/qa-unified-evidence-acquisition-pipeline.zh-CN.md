# QA Retrieval 与 Tool Calling 统一证据链路提案

> 状态：提案，待实现
> 创建日期：2026-08-09
> 最近更新：2026-08-09
> 范围：QA 预检索、可信 prefetch、Agent 运行时工具调用、多 Agent 取证与同一运行上下文治理
> 关联提案：`qa-evidence-convergence-and-retrieval-governance.zh-CN.md`、`qa-agent-context-budget-and-cancellation.zh-CN.md`
> 待修订正式文档：`../agent-platform/03-retrieval-and-knowledge.zh-CN.md`、`../agent-platform/04-context-session-and-tool-results.zh-CN.md`、`../agent-platform/08-observability-and-evaluation.zh-CN.md`

## 0. 文档生命周期

本文定义 QA 证据获取的目标架构。它不针对某个 query、某个 runbook 或某次评估做
专项修复，而是统一 retrieval 与 tool calling 在证据身份、相关性、覆盖度、预算和
停止条件上的运行语义。

本文与现有提案的关系：

- `qa-evidence-convergence-and-retrieval-governance.zh-CN.md` 已实现或设计了历史相关性
  门槛、`EvidenceUnit`、run-local ledger 和 `search_runbooks` admission，是本文的
  已有实现基础。
- `qa-agent-context-budget-and-cancellation.zh-CN.md` 定义权威结果、模型投影、run 预算
  和 Agent 收敛要求，是本文预算与交付语义的约束来源。
- 本文负责把上述局部机制连接为所有证据入口共同经过的主链。

执行顺序：

1. 固化统一证据合同和观测字段，不改变线上结果。
2. 为所有内置 read tool 补齐稳定证据身份和有界结果合同。
3. 将 retrieval 的候选选择与运行时 ledger、预算连接。
4. 将 prefetch 和运行时 tool calling 接入同一证据决策层。
5. 支持自由查询工具在正文物化前按结果身份去重。
6. 将同一机制扩展到 multi-agent workflow。
7. 完成固定数据集评估后，合并到正式架构文档。

当前状态：现状与目标边界已确认，尚未开始实现本文迁移切片。

## 1. 背景与问题定义

当前系统已经分别具备较完整的 retrieval 和 Agent tool calling 能力，但两者没有形成
一条连续的证据获取链路。

retrieval 负责 Agent 启动前的批量取证：

```text
问题分析
  -> 候选发现
  -> 内容扩展
  -> rerank / overview coverage
  -> retrieval context budget
  -> 拼接 RetrievedContext.Text
  -> 注入 Agent prompt
```

tool calling 负责 Agent 运行中的按需取证：

```text
模型提出 ToolCall
  -> 调用前 admission
  -> 工具执行
  -> 结果加入 evidence ledger
  -> delivery budget guard
  -> 追加 tool message
  -> 下一轮模型推理
```

两条链路目前只通过以下数据发生弱连接：

- retrieval 生成的扁平 `RetrievedContext.Text`
- retrieval 生成并传入 Agent 的 `EvidenceUnit`
- Agent 启动时由 `EvidenceUnit` 初始化的 run-local evidence ledger

这使系统具备了“可以描述已有证据”的数据结构，但没有形成“所有新证据必须经过同一
套决策”的执行机制。

因此，问题不是缺少更多 admission 规则，而是缺少统一的证据决策所有者。

## 2. 评审范围

本文评审以下四个证据入口：

```text
1. QA 初始 retrieval
2. ToolPlan trusted prefetch
3. Agent runtime tool calling
4. Multi-agent workflow evidence acquisition
```

评审维度包括：

- 谁定义当前回答还缺少什么证据
- 候选证据如何表示和排序
- 不同 query 返回同一证据时如何识别
- 已有证据如何参与后续工具 admission
- 相关性、权威性和覆盖度由谁判断
- 初始证据与动态工具结果如何共享上下文预算
- 工具结果中的重复项如何在正文加载前剔除
- 何时继续取证，何时停止并回答
- 多 Agent 子任务如何避免重复取数和重复注入

本文不处理：

- 新增向量数据库、rerank Provider 或 embedding Provider
- 为具体问题增加关键词、文档 ID、服务名或工具名特殊分支
- 使用 query 文本相似度作为证据去重的正确性依据
- 在通用执行层解析某个工具私有的 JSON 内容
- 用新的复杂状态机包装本来可以直接推导的运行事实

## 3. 当前实现链路

### 3.1 QA 入口与执行策略

`internal/agent/qa/service.go` 的 `QA.Ask` 完成：

```text
prepareQA
  -> 根据 execution.Strategy 选择
       -> submitInvestigation
       -> prepareSingleAgentRun
```

这意味着 single-agent 与 multi-agent 在准备阶段已经分叉。single-agent 后续的
`prepareEvidence` 不会自动成为 multi-agent workflow 的统一预检索入口。

### 3.2 Single-agent 初始证据链路

`internal/agent/qa/prepare.go` 的 `prepareEvidence` 当前按以下顺序执行：

```text
executePrefetch
  -> recallMemory
  -> rewriteQuery
  -> Retriever.RetrievePlan
  -> mergePreloadedContext
```

其中 `RetrievePlan` 在 `internal/retrieval/pipeline.go` 内继续执行：

```text
discover
  -> FindCode
  -> FindRunbooks
  -> FindServices

expand
  -> collectServices
  -> collectCode
  -> collectRunbooks
  -> collectDeps
  -> optional collectCodeGraph

assemble
  -> optional rerank
  -> overview coverage selection
  -> priority sort
  -> context budget selection
  -> Text + References + EvidenceUnits
```

retrieval 当前已经拥有较完整的结果级选择能力，但选择完成后先生成一块扁平文本。
运行时 Agent 可以知道部分 canonical evidence metadata，却不能重新对其中内容做细粒度
预算分配或覆盖度调整。

### 3.3 Trusted prefetch 链路

`internal/agent/qa/tools.go` 的 `executePrefetch`：

```text
读取 ToolPlan.Prefetch
  -> 校验工具存在且允许 prefetch
  -> prepared.ExecuteArguments
  -> Result 转为 ContextBlock
  -> 与 retrieval context 合并
```

prefetch 直接执行工具，没有经过运行时的：

- `admitToolCall`
- run-local evidence ledger 覆盖检查
- tool result token admission
- 相同证据增量判断

它是独立于 retrieval 和 runtime tool calling 的第三条证据获取路径。

### 3.4 Agent runtime tool calling 链路

`internal/agent/execution/loop_execution.go` 在 Agent 启动时：

```text
messages = Input.Messages
evidenceLedger = newRunEvidenceLedger(Input.EvidenceUnits)
remainingToolTokens = initialToolTokenBudget(...)
```

`internal/agent/execution/loop_turn.go` 在每个工具轮次中：

```text
MaxToolCalls guard
  -> admitToolCall
  -> ToolExecutor.Execute
  -> ledger.add(tool EvidenceUnits)
  -> prepareToolDelivery
  -> consumeToolTokens
  -> append tool message
```

当前存在三层不同语义的去重：

```text
retrieval assemble:
  Reference / EvidenceUnit key 去重

ToolExecutor:
  tool name + exact arguments fingerprint 去重

tool admission:
  EvidenceScope 是否被 ledger 完整覆盖
```

三者解决的问题不同，不能互相替代。

### 3.5 Multi-agent 链路

multi-agent 在 `QA.Ask` 中直接进入 `submitInvestigation`，由 investigation workflow
执行 code、runtime、docs 等子任务并在 join/synthesize 阶段合并结果。

当前 single-agent 的初始 retrieval、run-local ledger 和 tool admission 不天然成为
workflow 级共享机制。各子 Agent 可以分别查询相同来源，最终只能在汇总阶段处理已经
物化的重复内容。

## 4. 当前架构的核心断点

### 4.1 Retrieval 与 tool calling 是两个独立证据选择器

retrieval 根据召回分数、权威性、覆盖度和 48k 上下文预算选择初始证据。

tool calling 根据模型输出、调用前声明上界、剩余 ContextWindow 和整块结果交付检查
选择动态证据。

两者没有共享同一个证据准入函数，也没有共享同一本完整的预算账。

### 4.2 Query identity 被误用为 evidence identity

以下身份只能表示请求，不能表示结果：

```text
tool + exact arguments
runbook_search + hash(query)
semantic similarity(query A, query B)
```

两个不同 query 可以返回同一文档、同一章节或同一代码 chunk。要解决重复问题，必须
比较结果的稳定身份，而不是比较 query 是否相同。

canonical evidence identity 应来自权威数据源：

```text
sourceKind + target + section/chunk + version + timeRange
```

例如：

```text
runbook + document_id + section_id + document_version
code + repository/path + symbol_or_chunk_id + commit
service + service_id + facet + ontology_version
logs + source_id + event_range + time_range
```

### 4.3 工具调用前只能判断 scope，不能判断混合结果的增量

当调用参数包含明确 `doc_id` 时，可以在执行前判断文档是否已完整覆盖。

当调用参数是自由 query 时，调用前不知道会返回哪些结果。当前只能执行整个工具，
获得一块 opaque `Result.Content` 后再做整块交付，无法从 10 条结果中保留新增的 2 条。

因此，自由搜索工具必须支持“候选发现”和“正文物化”分离，或者在工具所有者内部
先返回结构化结果项，再由统一层决定哪些项进入模型。

### 4.4 预算由多套机制分别管理

当前至少存在：

- retrieval `ContextBudget`
- Agent `remainingToolTokens`
- admission `MaxResultTokens`
- delivery 的实时 ContextWindow fit check
- 后续 context compaction

这些机制分别有价值，但缺少一个运行级事实：

```text
当前 run 已经为哪些证据花费了多少模型上下文，
剩余预算应该优先解决哪些未覆盖目标。
```

### 4.5 选择粒度不一致

retrieval 在 `partial` 级别做排序、选择和截断。

tool calling 通常在整个 `Result.Content` 级别做 allow/deny，无法复用 retrieval 的
结果项选择能力，也无法对重复项做到零正文成本过滤。

### 4.6 停止条件不一致

retrieval 在候选耗尽、来源上限或上下文预算耗尽时停止。

Agent 在模型返回答案、达到 step/tool call/time 限制时停止。

当前没有显式表达：

```text
哪些 EvidenceGoal 仍未解决
本次调用是否增加了有效覆盖
继续调用是否还存在正向边际收益
```

## 5. 目标与原则

### 5.1 目标

建立一条所有证据入口共同经过的运行主链：

```text
EvidenceGoal
  -> Candidate Discovery
  -> Relevance / Authority Gate
  -> Canonical Identity
  -> Ledger Novelty Check
  -> Coverage Selection
  -> Run Budget Admission
  -> Selected Body Materialization
  -> Prompt Delivery
  -> Ledger Update
  -> Unresolved Goal Recalculation
```

retrieval 与 tool calling 保留不同职责：

- retrieval 是启动阶段的批量候选发现策略。
- tool calling 是运行阶段由 Agent 发起的按需补证策略。
- 两者不需要使用相同的工具接口，但必须经过相同的证据决策语义。

### 5.2 原则

1. **证据身份来自结果，不来自 query。**
2. **先发现候选，再加载正文。**
3. **相关性硬门槛先于 token selection。**
4. **重复判断使用 canonical identity 和 coverage，不解析正文。**
5. **预算按实际交付给模型的新增证据计算。**
6. **完整权威结果与模型投影承担不同职责。**
7. **工具所有者定义来源语义，通用层只执行通用策略。**
8. **没有新增覆盖时停止继续取证。**
9. **同一机制覆盖 initial retrieval、prefetch、runtime tool 和 multi-agent。**
10. **机制必须对问题类别成立，不得为单个失败案例过拟合。**

## 6. 统一概念模型

以下类型用于说明合同边界，不是最终 Go API 定稿。

### 6.1 EvidenceGoal

`EvidenceGoal` 描述回答当前问题需要覆盖什么，而不是指定必须调用哪个工具。

```go
type EvidenceGoal struct {
    ID              string
    Intent          string
    RequiredFacets  []string
    PreferredKinds  []string
    TimeRange       string
    MinimumTrust    int
    Required        bool
}
```

来源：

- Planner 生成的 EvidencePlan
- `RetrievalIntent` 确定性派生的 overview/focused 要求
- Agent 运行时提出的缺口
- workflow 节点的明确交付合同

目标本身不持久化额外状态。是否已满足应由当前 ledger 中的 evidence facts 推导。

### 6.2 EvidenceCandidate

`EvidenceCandidate` 是尚未加载或尚未交付正文的轻量候选。

```go
type EvidenceCandidate struct {
    Identity       EvidenceIdentity
    Reference      Reference
    Relevance      RelevanceEvidence
    TrustTier      int
    EvidenceClass  string
    Facets         []string
    EstimatedTokens int
    Materializer   MaterializerRef
}
```

候选必须足够轻量，使系统可以在不读取全部正文的前提下完成：

- 相关性门槛
- canonical 去重
- authority 排序
- facet 增量计算
- 预算预估

### 6.3 EvidenceIdentity

```go
type EvidenceIdentity struct {
    SourceKind string
    Target     string
    Section    string
    Version    string
    TimeRange  string
}
```

要求：

- 同一权威内容在不同 query 下产生相同 identity。
- 内容版本变化时必须通过 `Version` 或权威内容 hash 可观察。
- section/chunk identity 必须由数据源提供，不得由标题模糊匹配生成。
- target 完整覆盖时，可以覆盖同版本下的 section 请求。
- partial、cursor 和 time range 必须保留边界，不能错误声明完整覆盖。

### 6.4 EvidenceItem

`EvidenceItem` 是经过选择后实际物化的权威证据。

```go
type EvidenceItem struct {
    Candidate   EvidenceCandidate
    Content     string
    ContentHash string
    Coverage    EvidenceCoverage
    TokenCost   int
}
```

一个工具可以返回多个 `EvidenceItem`。通用执行层不应再把一块 opaque content 当成
唯一选择单位。

### 6.5 RunEvidenceLedger

ledger 以 `EvidenceIdentity` 为 O(1) key，记录已经交付给当前模型上下文的证据，而
不是仅记录后端曾经返回过的证据。

至少记录：

```text
identity
coverage
content_hash
trust_tier
evidence_class
facets
delivered_tokens
origin
artifact_reference
```

`origin` 用于观测来源，例如：

```text
initial_retrieval
prefetch
runtime_tool
workflow_node
```

ledger 必须区分：

- 后端已执行并持久化的 authoritative evidence
- 实际进入当前模型上下文的 delivered evidence

只有 delivered evidence 可以声明“Agent 已经拥有该内容”。

### 6.6 EvidenceCoordinator

`EvidenceCoordinator` 是普通的运行级决策组件，不是需要持久化转换图的状态机。

它负责：

- 接收 `EvidenceGoal` 和候选
- 执行统一 relevance/authority gate
- 查询 ledger 覆盖情况
- 计算新增 facet 和新增 evidence identity
- 根据运行预算选择候选
- 请求来源 adapter 物化选中的正文
- 生成有界模型投影
- 更新 ledger 和未满足目标视图
- 返回继续取证或停止的可解释原因

它不负责：

- 生成领域私有 query
- 解析工具私有 JSON
- 判断 runbook、代码、日志内部字段语义
- 替代 Agent 的问题分解和工具选择
- 持久化可从 ledger 和目标直接推导的额外 lifecycle 状态

## 7. 目标执行链路

### 7.1 统一主链

```text
Question
  -> Planning
       -> EvidenceGoal[]
       -> allowed acquisition strategies

  -> Initial Acquisition
       -> retrieval adapters discover candidates
       -> coordinator filters, selects and materializes
       -> seed prompt delivery
       -> update run ledger

  -> Agent Reason
       -> answer when goals are sufficiently covered
       -> otherwise propose tool acquisition

  -> Runtime Acquisition
       -> resolve exact scope when possible
       -> skip if ledger fully covers exact scope
       -> discover candidates for free-query search
       -> coordinator removes covered identities
       -> select by relevance, authority, novelty and budget
       -> materialize only selected bodies
       -> deliver bounded projection
       -> update the same ledger

  -> Recalculate Unresolved Goals
       -> continue only when useful acquisition remains
       -> otherwise answer with current evidence and explicit limitations
```

### 7.2 Exact-scope 工具

例如 `get_service(service_id)`、`search_runbooks(doc_id=...)`：

```text
ToolCall
  -> ResolveScope
  -> ledger.fullyCovers(scope)
       -> yes: already_available
       -> no: execute/materialize missing scope
  -> coordinator admission
  -> delivery
```

这类工具可以保留当前执行前 skip 能力。

### 7.3 Free-query 搜索工具

例如 `search_runbooks(query=...)`、`search_code(query=...)`：

```text
ToolCall
  -> validate query and bounded limit
  -> discover lightweight candidates
  -> candidate identity dedup against ledger
  -> relevance/authority gate
  -> novelty and coverage selection
  -> materialize selected candidate bodies
  -> return structured evidence items
```

不同 query 返回同一结果时，候选 identity 会在正文物化前被过滤。query hash 仍可用于：

- trace correlation
- identical request suppression
- cache key

但不能用于声明证据已覆盖。

### 7.4 混合结果

当工具发现 10 条候选，其中：

```text
6 条已完整覆盖
2 条相关性不足
1 条与当前 facet 重复且预算收益低
1 条提供新的高权威 facet
```

目标行为是只物化并交付最后 1 条，而不是：

- 因 query 不同而再次交付全部 10 条
- 因整块结果过大而拒绝全部结果
- 在通用执行层解析正文后做字符串去重

## 8. 相关性、权威性与覆盖度

统一决策层按以下顺序处理候选：

```text
1. 数据有效性与用户/租户边界
2. 来源声明的硬相关性门槛
3. minimum trust / authority 要求
4. canonical identity coverage
5. required facet 增量
6. token 成本与剩余预算
7. 稳定排序和有界选择
```

不要求第一阶段立即引入一个复杂的统一分数公式。可以保留来源自己的 relevance
evidence，由 coordinator 执行明确的门槛和稳定排序：

```text
required goal coverage
  > authority
  > relevance
  > novelty
  > lower token cost
  > stable identity tie-break
```

overview 意图应优先：

```text
权威总览
  -> 每个未覆盖业务 facet 的代表证据
  -> 必要的细节补充
```

focused intent 应优先：

```text
直接回答目标实体/claim 的高相关证据
  -> 必要依赖和上下游
  -> 非必要背景最后考虑
```

## 9. 统一预算语义

运行预算应只有一个事实来源：

```text
available evidence tokens =
    provider context window
  - immutable prompt tokens
  - tool definitions
  - conversation/history allocation
  - final answer reserve
  - safety reserve
  - already delivered dynamic evidence
```

在此基础上可以派生阶段配额，但不能建立互不感知的独立账本：

```text
initial retrieval allocation
runtime acquisition allocation
per-call maximum
per-turn maximum
```

预算准入使用候选的预计 token 成本，最终扣账使用实际模型投影 token。

禁止：

- retrieval 先独占固定 48k，runtime tool 再独立计算另一份预算
- 工具执行后才发现整个 opaque payload 无法交付
- 为了预算在通用层静默截断带有精确合同的权威内容

推荐：

- 在候选阶段减少 limit 或候选数量
- 在物化阶段请求明确 section/page/cursor
- 由工具所有者生成有边界、带 coverage 的模型投影
- 完整 authoritative result 进入 trace/artifact，模型只接收被选择的 evidence items

## 10. 停止与收敛

系统不需要新增复杂状态机。每轮结束后可直接根据以下事实推导是否继续：

```text
required goals 是否仍未满足
最近一次 acquisition 是否增加 canonical evidence
是否增加 required facet 覆盖
是否还有未尝试且预算可承受的候选
是否仍有足够时间完成一次调用和最终回答
```

建议停止原因：

```text
goals_satisfied
no_incremental_evidence
no_affordable_candidate
time_reserve_reached
tool_call_limit_reached
source_exhausted
```

`no_incremental_evidence` 的判断依据是结果 identity 和 coverage，不是连续两次 query
是否相似。

Agent 收到的 convergence hint 应是结构化事实的简短投影，例如：

```text
当前请求未增加新的权威证据；已有证据已覆盖该文档和章节。
请基于现有证据回答，或仅针对仍未覆盖的目标选择其他来源。
```

## 11. 各入口如何接入

### 11.1 Initial retrieval

保留现有 discover/expand adapter，但将 `assemble` 拆分为：

```text
source candidate formatting
  -> coordinator selection
  -> selected item materialization
  -> prompt rendering
```

retrieval 不再独立拥有最终 run 预算，也不先生成无法再选择的整块文本。

### 11.2 Trusted prefetch

prefetch 不再直接 `ExecuteArguments -> ContextBlock`。

它应转换为可信 acquisition proposal，并经过：

```text
scope/candidate resolution
  -> ledger
  -> relevance/authority policy
  -> shared budget
  -> delivery
```

`Required` 只表示来源失败是否终止准备，不表示可以绕过重复和预算治理。

### 11.3 Runtime tool calling

模型仍然负责提出工具调用，通用执行层增加统一 acquisition boundary：

```text
ToolCall
  -> exact-scope pre-admission
  -> candidate discovery
  -> coordinator decision
  -> selected materialization
  -> result persistence and delivery
```

现有 exact argument fingerprint 保留，用于防止模型重复发出完全相同的调用；它不再
承担证据去重职责。

### 11.4 Multi-agent

workflow 使用 workflow-scoped evidence ledger：

- 子节点读取开始时的 ledger snapshot。
- 子节点发现候选时按 workflow evidence identity 去重。
- 子节点只提交新增 evidence items 和自己的分析结论。
- join 节点按 canonical identity 合并，不拼接重复正文。
- synthesize 节点接收统一预算选择后的证据投影。

并发子节点可能同时发现同一候选。workflow ledger 的 merge 必须按 identity 做原子
best-so-far 合并，并记录 origin，不允许依赖执行先后顺序决定是否重复注入。

## 12. 组件职责

### Planner / Query Analysis

负责：

- 问题清理、时间解析和意图判断
- EvidenceGoal 与允许来源
- 工具可见性和场景约束

不负责：

- 根据已返回正文做字符串去重
- 决定某条候选是否值得占用最终上下文

### Retrieval / Tool Source Adapter

负责：

- 构造领域查询
- 返回来源真实 relevance evidence
- 提供 canonical identity、authority、facet 和 cost estimate
- 按明确 identity/section/page 物化正文
- 声明 partial、omitted、cursor 和版本

不负责：

- 读取全局通用依赖容器
- 替其他来源定义 identity
- 静默替换配置的 Provider

### EvidenceCoordinator

负责：

- 通用 relevance/authority policy 调度
- ledger coverage 和 conflict 判断
- facet novelty
- run budget admission
- 稳定选择和停止原因

不负责：

- 解析来源私有内容
- 用 LLM 对每个结果做二次总结

### Renderer / Delivery

负责：

- 将选中的结构化 evidence items 投影为模型消息
- 保留引用、coverage、answer contract 和 artifact reference
- 计算实际交付 token

不负责：

- 对未选择的证据做补偿性截断
- 将 authoritative content 与 prompt projection 混为同一份数据

### Agent Loop

负责：

- 判断是否能够回答
- 针对 unresolved goal 提出新的 acquisition proposal
- 根据 coordinator 返回的事实调整下一步

不负责：

- 绕过 coordinator 直接把工具正文加入 messages
- 用重复 query 尝试突破预算或 coverage 判断

## 13. Tool 合同演进

当前 `tool.Result` 已包含：

```text
Content
References
EvidenceUnits
Coverage
AnswerContract
```

迁移目标是增加结构化结果项边界，而不是删除现有字段。建议分阶段：

### 阶段 A：所有 read tool 补齐现有合同

每个 read tool 至少声明：

- deterministic `MaxResultTokens`
- canonical `EvidenceUnit`
- bounded `Coverage`
- stable `Reference`

未声明工具继续使用保守上界，但必须产生观测告警，不能长期依赖默认 4096 tokens。

### 阶段 B：支持结构化 items

引入类似：

```go
type Result struct {
    Content        string
    Items          []EvidenceItem
    References     []Reference
    EvidenceUnits  []EvidenceUnit
    Coverage       EvidenceCoverage
    AnswerContract AnswerContract
}
```

兼容期规则：

- 有 `Items` 时，以 items 作为选择和 ledger 输入。
- 仅有 `Content` 时，按单个 opaque item 处理，不能做结果内增量去重。
- 新增和改造的搜索工具必须优先提供 items。

### 阶段 C：发现与物化分离

高成本搜索工具提供内部两阶段能力：

```text
Discover(args) -> []EvidenceCandidate
Materialize(selected identities) -> []EvidenceItem
```

这不要求把两个阶段都公开为 LLM 工具。对模型仍可表现为一次工具调用，执行层内部完成
候选选择后再物化。

## 14. 可观测性

每次 acquisition 至少记录：

```text
run_id
workflow_run_id
origin
goal_ids
tool_id / retrieval_source
query_fingerprint
candidates_discovered
candidates_relevance_rejected
candidates_covered
candidates_budget_rejected
candidates_materialized
items_delivered
new_identities
new_facets
estimated_tokens
delivered_tokens
remaining_evidence_tokens
decision
stop_reason
```

不得把完整 query、prompt、正文、用户数据或 URL 作为 metrics label。

建议核心指标：

```text
evidence_candidate_acceptance_ratio
evidence_duplicate_identity_ratio
evidence_materialization_avoidance_tokens
evidence_delivered_tokens_per_run
evidence_no_increment_stop_total
tool_already_available_total
tool_no_increment_total
tool_budget_denied_total
workflow_cross_node_dedup_total
```

trace 必须能够回答：

1. 为什么某次工具调用被允许、缩小、跳过或拒绝？
2. 不同 query 是否返回了相同 evidence identity？
3. 哪些候选在正文加载前被过滤？
4. 初始 retrieval 已经消耗了多少运行证据预算？
5. Agent 最终因为目标满足、无新增证据、预算还是时间而停止？

## 15. 迁移切片

### Slice 1：合同与 shadow 观测

- 定义 `EvidenceGoal`、`EvidenceIdentity`、`EvidenceCandidate` 的内部合同。
- 为现有 retrieval parts 和 tool results 生成 shadow candidates。
- 记录 identity、facet、cost 和 dedup 决策，但不改变线上选择。
- 验证 identity 稳定性和版本边界。

验收：

- 同一文档由不同 query 命中时产生同一 identity。
- trace 能区分 request dedup 与 evidence dedup。
- 不增加模型调用和正文读取次数。

### Slice 2：所有内置 read tool 合同化

- 为 runbook、code、service、symbol、dependency、docs、logs/config 等工具补齐
  `EvidenceUnit`、coverage 和稳定结果上界。
- 对没有稳定 target/section 的工具先修复数据源合同，不使用 query hash 代替。
- 为每个工具增加 identity、partial 和 version 测试。

验收：

- 工具成功结果均能映射到至少一个 canonical evidence identity。
- 声明 complete 的结果确实覆盖对应 scope。
- 存储读取在工具声明的 limit/cursor 边界内完成。

### Slice 3：统一 initial retrieval 选择

- 将 retrieval assemble 中的 ledger、coverage 和 budget 决策迁入 coordinator。
- seed evidence 与 runtime evidence 使用同一本 run ledger。
- 保留现有 rerank 和 overview policy 作为候选排序输入。

验收：

- 初始 prompt 内容与 ledger 完全一致。
- 截断或清洗后不会保留错误的 complete coverage。
- retrieval 和 runtime tool 看到相同的 remaining evidence budget。

### Slice 4：Prefetch 接入统一链路

- prefetch 转换为 acquisition proposal。
- required prefetch 仍保留失败语义，但不能绕过 ledger 和预算。
- 删除 prefetch 独立的内容拼接决策。

验收：

- prefetch 与 retrieval 返回同一 identity 时正文只交付一次。
- prefetch trace 包含统一 admission 字段。

### Slice 5：Runtime free-query 增量过滤

- 优先改造 `search_runbooks` 和 `search_code` 为 candidate/item 两阶段。
- 在 materialize 前过滤 ledger 已覆盖 identity。
- mixed result 只交付新增 items。

验收：

- 两个不同 query 返回同一文档时，第二次不重复加载或交付正文。
- 一次结果部分重复时仍能交付其中新增项。
- opaque legacy tool 保持保守整块语义，不在通用层解析内容。

### Slice 6：收敛与停止

- 从 EvidenceGoal 和 ledger 推导 unresolved goals。
- 增加 `no_incremental_evidence`、`no_affordable_candidate` 等停止原因。
- Agent prompt 只接收简短、可解释的 convergence hint。

验收：

- 连续无新增证据不会通过换 query 无限继续。
- required goal 已覆盖时不再调用同类来源。
- 保留最终回答 token 和时间。

### Slice 7：Multi-agent workflow

- 引入 workflow-scoped ledger 和原子 identity merge。
- 子节点只上交新增 evidence items。
- join/synthesize 使用统一选择后的证据投影。

验收：

- 并发子节点返回同一 identity 时只注入一次正文。
- 节点来源和冲突版本仍可审计。
- workflow 总证据预算有硬上限。

## 16. 测试策略

### 单元测试

- identity normalization 与 key 稳定性
- complete target 覆盖 section
- partial/cursor/time range 不错误覆盖
- 相同 identity 相同 hash 去重
- 相同 identity 不同 hash 记录 conflict
- relevance gate 先于 materialize
- facet novelty 和稳定 tie-break
- budget selection 使用实际新增 token
- no-increment stop reason

### 集成测试

- initial retrieval 与 runtime tool 返回同一 runbook
- prefetch 与 retrieval 返回同一 service evidence
- 两个不同 search query 返回部分重叠结果
- tool mixed result 中只交付新增项
- context budget 接近上限时仍保留 final answer reserve
- multi-agent 并发节点返回相同 evidence identity

### 回归评估

固定数据集至少覆盖：

- focused fact
- overview / architecture
- cross-service dependency
- exact document lookup
- ambiguous free-query search
- history-heavy unrelated conversation
- repeated tool query with paraphrase
- partial pagination and time-range evidence

比较指标：

```text
answer correctness
required facet coverage
duplicate delivered tokens
materialized but not delivered tokens
tool calls per run
input tokens per turn
time to first answer
run completion latency
```

## 17. 验收标准

本文完成需要同时满足：

1. initial retrieval、prefetch 和 runtime tool 使用同一本 run evidence ledger。
2. 不同 query 返回同一 canonical identity 时不会重复交付正文。
3. free-query mixed result 可以保留新增项并剔除重复项。
4. 所有内置 read tool 有稳定 identity、coverage 和 bounded result contract。
5. retrieval 与 tool calling 使用同一个运行证据预算事实来源。
6. ledger 只声明实际进入模型上下文的 delivered evidence。
7. 没有新增 identity 或 required facet 时可以明确停止。
8. multi-agent 在 workflow 范围内按 identity 去重。
9. trace 可以解释每次候选拒绝、物化、交付和停止决策。
10. 固定数据集证明重复 token 和无收益工具调用下降，回答正确率不回退。

## 18. 风险与约束

### 18.1 Identity 不稳定

如果数据源无法提供稳定 document/chunk/version identity，通用层无法可靠去重。不能用
标题、正文片段或 query hash 长期替代，应先修复索引和存储合同。

### 18.2 Candidate estimate 不准确

token estimate 只用于 admission，最终扣账必须使用实际模型投影 token。估算偏差需要
可观测，但不能导致预算突破。

### 18.3 过早过滤降低召回

shadow 阶段必须记录“被过滤但原系统会选择”的候选，并在固定标注集上校准硬门槛。
不能从单次 trace 直接推导通用阈值。

### 18.4 Coordinator 变成通用业务容器

coordinator 只拥有通用 evidence policy。runbook、代码、日志、配置的字段解析和
materialization 必须留在各自 adapter，避免形成新的跨领域 junk drawer。

### 18.5 一次性大重构

迁移必须保持旧 `Content` 合同可运行，通过 shadow、单工具 items、initial retrieval、
prefetch、multi-agent 逐步切换。每个切片都应独立可测和可回滚。

## 19. 最终结论

当前系统的问题不是 retrieval 不够强，也不是 tool admission 规则不够多，而是多个
证据入口分别拥有局部决策权：

```text
retrieval 选择初始证据
prefetch 直接执行可信工具
Agent admission 判断调用是否可执行
tool delivery 判断整块结果是否可进入上下文
multi-agent 在另一个 workflow 中独立取证
```

目标架构必须把这些入口收敛到同一条证据主线：

```text
目标
  -> 候选
  -> 相关性
  -> 稳定身份
  -> 增量覆盖
  -> 统一预算
  -> 按需物化
  -> 有界交付
  -> 同一 ledger
  -> 收敛停止
```

retrieval 与 tool calling 仍然是不同的取证策略，但不再是两套证据治理系统。后续问题
应优先修复这条统一机制，而不是继续在单个来源、单个 query 或单个工具上叠加补丁。
