# QA 查询意图、检索 Facet 与派生类型收敛提案

> 状态：提案草稿，尚未实施
> 创建日期：2026-08-15
> 范围：QA Query Analysis、Retrieval、EvidenceUnit、Task Contract、Capability、Workflow 与回答提示词
> 关联提案：`qa-evidence-convergence-and-retrieval-governance.zh-CN.md`、`qa-retrieval-latency-and-progress-governance.zh-CN.md`、`qa-unified-evidence-acquisition-pipeline.zh-CN.md`
> 关联正式文档：`../agent-platform/01-architecture-and-execution.zh-CN.md`、`../agent-platform/02-evidence-and-tooling.zh-CN.md`、`../agent-platform/03-retrieval-and-knowledge.zh-CN.md`、`../agent-platform/18-task-driven-multi-agent-architecture.zh-CN.md`

## 0. 文档生命周期

本文针对当前 QA 链路中查询类型、检索意图、Facet、Capability Facet 和诊断来源持续膨胀的问题提出收敛方案。本文只定义问题、目标模型、修改点、迁移顺序和验收标准，不在本文阶段直接修改生产代码。

本文的核心判断是：

1. 当前引入 `ResponseMode`、`RetrievalIntentKind`、`RequiredFacets`、`TargetEntities` 和 `IntentOrigin`，分别解决回答组织、证据形状、覆盖目标、调查对象和诊断来源问题，其初始动机成立。
2. 随着 `flow`、`comparison`、Task Graph、Capability 和 Evidence Coverage 接入，这些概念之间已经出现大量确定性派生、重复存储和词表漂移。
3. 当前主要问题不是“某个类型定义得不够细”，而是把可以由一个主分类确定性派生的结果，继续建模为多个平级、可独立变化的状态。
4. 如果沿用当前方式，每增加一种问题形态，就可能同时新增 ResponseMode、RetrievalIntent、Facet、Capability Facet、预算分支、回答模板和测试分支，形成组合式膨胀。
5. 应将请求级语义收敛成一个 canonical `QueryPlan`，只保留不可派生的数据；Facet、检索预算、扩展策略和回答提示都由统一的 `QueryKind` 派生。
6. Facet 体系应建设为一个轻量、中央化、可验证的 Catalog，而不是再引入一套重量级 Ontology 类型、图存储或推理引擎。

在本提案完成评审、实现和回归前，现有类型仍是运行时事实。本提案不将未实施目标描述为当前行为。

## 1. 背景

### 1.1 原始问题与已有演进

早期 QA 链路主要区分两类决策：

```text
ResponseMode
  回答应该如何组织

EvidencePlan
  本轮允许访问哪些证据来源
```

后续为了治理概览问题召回发散、调用链检索旁路、证据覆盖不可判断等问题，又引入：

```text
RetrievalIntent
  本轮需要什么形状的证据

RequiredFacets
  回答必须覆盖哪些证据维度

TargetEntities
  围绕哪些对象检索和调查

IntentOrigin
  意图来自明确规则还是 fallback
```

这些能力解决了真实问题：

- `overview` 可以限制服务扩展数量并按 Facet 增量选择证据；
- `flow` 可以显式触发 CodeGraph 扩展；
- `runtime_diagnosis` 可以使用更高的召回预算；
- `TargetEntities` 可以将方法、类、服务等对象传递到检索和多 Agent 调查；
- `IntentOrigin` 可以区分规则命中和默认兜底；
- `RequiredFacets` 可以转成 `EvidenceGoal`，继续驱动 Task Contract 和 Capability 选择。

但是当前实现逐步把这些派生结果都固化为独立字段或独立类型，导致同一份请求语义在多个结构中重复表达。

### 1.2 当前请求级类型

当前主要类型位于：

```text
internal/domain/agent.go
internal/domain/retrieval_intent.go
internal/domain/retrieval_plan.go
internal/agent/qa/query.go
```

主要结构为：

```go
type ResponseMode string

type RetrievalIntentKind string

type EvidenceFacet string

type RetrievalIntent struct {
    Kind           RetrievalIntentKind
    RequiredFacets []EvidenceFacet
    TargetEntities []string
}

type IntentOrigin string

type IntentResolution struct {
    ResponseMode ResponseMode
    Intent       RetrievalIntent
    Origin       IntentOrigin
}
```

`queryAnalysisOutput` 又继续保存：

```go
ResponseMode    domain.ResponseMode
RetrievalIntent domain.RetrievalIntent
IntentOrigin    domain.IntentOrigin
```

这意味着同一请求同时携带：

```text
回答分类
检索分类
分类派生出来的 Facet
实体数据
分类诊断信息
```

其中只有“具体目标实体”天然属于请求数据，其余大部分都可以由同一个 canonical 分类或执行上下文派生。

### 1.3 当前分类数量

当前 `ResponseMode` 有五种：

```text
bug_analysis
requirements_analysis
architecture_review
code_review
codebase_qa
```

当前 `RetrievalIntentKind` 有六种：

```text
focused_fact
overview
flow
comparison
inventory
runtime_diagnosis
```

当前 domain Facet 有七种：

```text
system_boundary
business_domain
entrypoint
core_flow
data_and_state
external_dependency
runtime_and_operations
```

Capability 和 Workflow 中又出现至少三种额外 Facet 字符串：

```text
implementation
service.topology
documentation
```

当前系统因此不是简单维护“六种查询类型”，而是在维护多组相互映射但未完全统一的分类空间。

## 2. 当前链路

### 2.1 Query Analysis

当前主要链路为：

```text
Question
  -> Evidence Planner
       -> EvidencePlan
       -> QueryTerms
       -> Time / History / Execution Strategy
  -> analyzeQuery
       -> ResolveTime
       -> ResolveRetrievalIntent
            -> ClassifyResponseMode(question)
            -> ResponseMode
            -> RetrievalIntentFor(ResponseMode)
            -> flow / comparison 规则覆盖
            -> RequiredFacets
            -> TargetEntities
            -> IntentOrigin
```

`ResolveRetrievalIntent` 当前同时承担：

1. 回答分类；
2. 检索分类；
3. Facet 默认集合选择；
4. 实体标准化；
5. 规则或 fallback 来源判断。

其结果随后被拆成多个平级字段保存在 `queryAnalysisOutput` 中。

### 2.2 Retrieval 消费

`RetrievalIntent` 被传入：

```text
Retriever.RetrievePlan
```

当前主要消费方式包括：

```text
intent.Kind
  -> budgetForIntent
  -> overview 服务扩展上限
  -> flow CodeGraph 扩展
  -> overview coverage selection

intent.RequiredFacets
  -> selectOverviewEvidence

intent.TargetEntities
  -> 调查合同和实体约束
```

当前 `Kind` 实际上已成为多个执行策略的间接开关：

```text
query kind
  -> recall budget
  -> expansion policy
  -> selection strategy
  -> codegraph switch
```

但这些关系分散在多个 `switch` 和条件判断中，没有一个可检查的策略入口。

### 2.3 回答组织消费

回答提示词侧仍在：

```text
internal/agent/execution/prompt_context.go
```

通过原始 `question` 再次执行：

```go
mode := ClassifyResponseMode(question)
```

因此当前请求虽然已经在 QA Preparation 中生成了 `ResponseMode`，构建 Agent Prompt 时仍重新分类。

这会形成两个分类边界：

```text
prepare 阶段的 ResponseMode
prompt 阶段重新计算的 ResponseMode
```

即使目前两个调用使用同一个函数，仍然存在以下风险：

- 后续其中一处加入额外信号，结果发生漂移；
- 改写问题和原始问题选择不同，产生不一致；
- 测试只能分别验证两个调用，不能保证“一次解析、全链复用”；
- 诊断时无法确认最终 Prompt 使用的是准备阶段结果还是重新分类结果。

### 2.4 Multi-Agent 消费

`RetrievalIntent.RequiredFacets` 在 QA 调查链路中继续转换为：

```text
EvidenceGoal
  -> TaskContract.EvidenceGoals
  -> taskGraphCapability.RequiredFacets
  -> TaskSpec.RequiredFacets
  -> TaskDirective.RequiredFacets
  -> report completeness
```

Facet 经过多层结构继续以 `[]string` 传播：

```text
agent.Capability.InputFacets
agent.TaskSpec.RequiredFacets
tool.EvidenceUnit.Facets
workflow.TaskDirective.RequiredFacets
```

这些层次并非都在表达新的业务语义，大量字段只是为了把同一组字符串从一个边界传到下一个边界。

### 2.5 当前 Facet 映射

当前已经形成三段关系：

```text
Query Intent --requires--> Facet
Evidence      --provides--> Facet
Capability    --supports--> Facet
```

#### Intent 到 Facet

定义在：

```text
internal/domain/retrieval_intent.go
```

例如：

```text
overview
  -> system_boundary
  -> business_domain
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency
  -> runtime_and_operations

runtime_diagnosis
  -> entrypoint
  -> core_flow
  -> external_dependency
  -> runtime_and_operations

flow
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency
```

#### Evidence 到 Facet

定义在：

```text
internal/retrieval/evidence_units.go
```

例如：

```text
service
  -> system_boundary
  -> business_domain

dependency
  -> external_dependency

code / codegraph
  -> entrypoint
  -> runtime_and_operations

runbook.flow
  -> system_boundary
  -> core_flow

runbook.schema
  -> data_and_state

runbook.module
  -> business_domain
  -> entrypoint
```

Runbook 的同类映射又重复存在于：

```text
internal/agent/tools/registry.go
```

#### Capability 到 Facet

默认 Capability 定义在：

```text
internal/agent/catalog/defaults_capability.go
```

其中同时使用 domain Facet 和另外一套角色型字符串：

```text
knowledge.code.inspect
  -> implementation
  -> entrypoint
  -> core_flow
  -> data_and_state

knowledge.service.trace
  -> service.topology
  -> system_boundary
  -> external_dependency
  -> runtime_and_operations

knowledge.docs.verify
  -> documentation
  -> business_domain
```

这套关系已经可以支持 coverage selection 和 capability matching，因此它是一个轻量 Facet Taxonomy 的雏形；但它目前只是多个 Go `switch`、常量和自由字符串之间的约定，不是统一、可验证的本体模型。

## 3. 已确认问题

### 3.1 P0：ResponseMode 与 RetrievalIntent 是高度重叠的双重分类

当前存在以下确定性映射：

```text
architecture_review
  -> overview

bug_analysis
  -> runtime_diagnosis

requirements_analysis
  -> inventory

code_review
  -> focused_fact

codebase_qa
  -> focused_fact
```

`flow` 和 `comparison` 又通过额外规则覆盖这个映射。

问题不是两个名称不同，而是两套分类都被当成独立状态保存和传递：

```text
ResponseMode = architecture_review
RetrievalIntent.Kind = overview
```

只要两者之间是确定性映射，就不应同时成为可独立修改的事实。否则系统必须永久处理不一致组合，例如：

```text
ResponseMode = architecture_review
RetrievalIntent.Kind = runtime_diagnosis
```

当前结构没有从类型层面禁止这种组合。

影响：

- 每增加一种问题形态，要决定两套枚举是否都增加；
- 分类规则和测试数量重复增长；
- 调用方不知道应该以哪个字段为准；
- 回答模板和检索策略可能使用不同分类；
- 多意图问题容易继续增加第三套修正字段，而不是收敛模型。

### 3.2 P0：RequiredFacets 是可派生数据，却被持久化为第二事实来源

当前 `RequiredFacets` 绝大多数由 `RetrievalIntent.Kind` 固定派生。

例如：

```text
overview
  -> 固定七个 Facet

flow
  -> 固定四个 Facet
```

但结构允许任意构造：

```go
RetrievalIntent{
    Kind: RetrievalFlow,
    RequiredFacets: []EvidenceFacet{
        FacetRuntimeOperations,
    },
}
```

系统无法判断：

```text
Kind 是权威事实
还是 RequiredFacets 是权威事实
```

影响：

- 测试和业务代码可以创建语义矛盾对象；
- 调整默认 Facet 时需要修改构造点和断言；
- Facet 集合经过 Task Contract 后可能继续被修改，难以追踪来源；
- 为支持“例外”而保留完全自由的字段，会让默认模型失去约束。

### 3.3 P0：分类没有做到真正的一次解析、全链复用

QA Preparation 已调用 `ResolveRetrievalIntent`，但 Prompt 构建仍调用 `ClassifyResponseMode(question)`。

这违背了已有提案中“一次请求只生成一次 canonical intent”的目标。

影响：

- 同一请求存在重复分类；
- 原始问题、清理问题和改写问题可能产生不同结果；
- 后续添加 Planner 信号后，Prompt 层无法复用；
- Trace 中记录的分类不一定等于最终回答模板实际使用的分类。

### 3.4 P1：Facet 词表已经分裂

当前至少存在两组语义不同但混在同一个 `[]string` 中的值。

证据内容维度：

```text
system_boundary
business_domain
entrypoint
core_flow
data_and_state
external_dependency
runtime_and_operations
```

调查角色或证据媒介：

```text
implementation
service.topology
documentation
```

其中：

- `implementation` 更像代码调查范围，通常可以展开成 `entrypoint`、`core_flow`、`data_and_state`；
- `service.topology` 更像 Capability 的专业方向，通常对应 `system_boundary` 和 `external_dependency`；
- `documentation` 描述证据媒介或文档覆盖情况，不是答案内容维度。

将它们都放入 Facet 后，会出现：

```text
回答是否必须覆盖 documentation？
还是必须使用 documentation 作为证据？

service.topology 是 system_boundary 的同义词、上位词，
还是某个 Agent 的角色标签？
```

当前没有统一定义。

### 3.5 P1：Facet 映射散落并重复

当前映射至少分散在：

```text
internal/domain/retrieval_intent.go
internal/retrieval/evidence_units.go
internal/agent/tools/registry.go
internal/agent/catalog/defaults_capability.go
internal/agent/workflow/investigation.go
```

Runbook kind 到 Facet 的映射已经重复实现。

影响：

- 修改一处不能保证所有边界同步；
- 同一个 Evidence 在预检索和工具调用链中可能获得不同 Facet；
- 无法在启动或测试阶段做全局完整性校验；
- 新增 Facet 时必须依赖人工检索所有字符串使用点。

### 3.6 P1：EvidenceSource 到 Facet 的静态映射粒度过粗

当前：

```text
code / codegraph
  -> entrypoint
  -> runtime_and_operations
```

这并不总是成立。

普通代码命中更可能证明：

```text
entrypoint
core_flow
data_and_state
external_dependency
```

但代码本身不必然证明：

```text
runtime_and_operations
```

`runtime_and_operations` 通常需要日志、指标、Trace、部署配置、告警规则或运行手册等运行时证据。

如果按来源种类粗粒度声明 Facet，coverage selection 可能把“命中一段代码”误判为“运行态证据已经覆盖”。这会导致检索过早停止，或者让最终答案高估证据完整性。

### 3.7 P1：Query Kind 同时隐式控制多个执行策略

当前 `intent.Kind` 被多个模块分别解释：

```text
budgetForIntent
expand 中的 overview 限制
expand 中的 flow CodeGraph 开关
selectOverviewEvidence
回答模式提示
Task Contract 生成
```

这些策略不是通过统一合同产生，而是调用方各自编写条件。

影响：

- 新增 Kind 时容易漏掉某个策略；
- 删除 Kind 时容易留下失效分支；
- 同一 Kind 在不同层的含义可能逐步漂移；
- 为修复一个局部行为，倾向新增更多 Kind，而不是调整策略数据。

### 3.8 P2：IntentOrigin 属于诊断元数据，不属于业务计划

`IntentOrigin` 当前只有：

```text
rule
fallback
```

它用于解释“为什么得到这个分类”，并不改变：

- 允许访问的 Evidence Source；
- 检索 Kind；
- Facet；
- Target Entity；
- 工具权限。

因此它更适合进入：

```text
runtrace metadata
structured log
metrics counter
```

而不是作为请求计划的一部分在业务链路中持续传递。

### 3.9 P2：TargetEntities 是必要数据，但命名容易让其被误认为新的分类轴

`TargetEntities` 与其他字段不同，它不能单纯由 `QueryKind` 推导。具体服务、类、方法、API 和文档 ID 来自用户输入、Query Terms 或 Planner 信号，应该保留。

当前问题不是它存在，而是它被包裹在 `RetrievalIntent` 中，与分类和派生 Facet 混在一起。后续容易继续派生：

```text
TargetEntityKind
TargetEntityOrigin
TargetEntityConfidence
TargetEntityScope
```

如果没有明确边界，实体识别也会走向类型膨胀。

### 3.10 P2：当前设计会形成组合式增长

如果新增一种 `security_review`，按当前模式通常需要同时评估：

```text
是否新增 ResponseMode
是否新增 RetrievalIntentKind
需要新增哪些 EvidenceFacet
Capability 是否增加新 Facet 字符串
EvidenceSource 是否提供新 Facet
budgetForIntent 是否增加分支
expand 是否增加开关
回答 Prompt 是否增加模板
Task Contract 是否增加目标
Trace 是否增加 origin
```

类型数量不是线性增加，而是映射关系和测试矩阵一起增加。这正是当前“水多加面、面多加水”的根因。

## 4. 根因判断

### 4.1 把派生结果建模为事实

当前模型没有严格区分：

```text
不可派生的输入事实
确定性派生的策略结果
只用于诊断的元数据
```

建议分类如下：

| 当前字段 | 性质 | 是否应保留为请求状态 |
|---|---|---:|
| `ResponseMode` | 可由主查询分类派生的回答策略 | 否 |
| `RetrievalIntent.Kind` | 主查询分类 | 是，但应收敛为唯一 `QueryKind` |
| `RequiredFacets` | 可由 `QueryKind` 派生 | 默认不保存 |
| `TargetEntities` | 请求相关实体数据 | 是 |
| `IntentOrigin` | 诊断元数据 | 否，移入 Trace |

### 4.2 把内容维度、证据媒介和执行角色混成 Facet

当前 `Facet` 同时承担：

1. 答案内容维度；
2. Evidence Coverage 标签；
3. Capability 路由标签；
4. Investigator 角色标签；
5. 文档媒介标签。

一个词表承担过多职责，就会不断新增无法比较的值。

Facet 应只回答一个问题：

> 这条证据可以支持答案中的哪个内容维度？

它不应该回答：

```text
证据来自代码还是文档
由哪个 Agent 调查
使用什么工具
属于哪个工作流角色
```

### 4.3 缺少中央 Catalog 和边界验证

当前映射靠调用方约定，没有：

- 唯一 Facet 定义；
- 语义说明；
- 别名淘汰规则；
- 引用验证；
- Required Facet 的可覆盖性检查；
- Provider/Capability 声明一致性检查。

因此每个模块都可以通过 `[]string` 扩展自己的局部词表。

## 5. 目标与非目标

### 5.1 目标

1. 每个请求只产生一个 canonical 查询分类。
2. 请求状态只保存不可派生数据。
3. 回答组织、默认 Facet、检索预算和扩展策略从同一个 `QueryKind` 派生。
4. Facet 只描述答案内容维度，使用统一 Catalog。
5. Evidence 和 Capability 只引用 Catalog 中存在的 Facet。
6. 删除重复分类和重复 Facet 映射。
7. 通过测试或启动校验阻止未注册 Facet、不可覆盖 Facet 和重复映射进入系统。
8. 在不引入重量级本体系统的情况下，保留 `requires / provides / supports` 三段覆盖关系。
9. 保持 `EvidencePlan` 的权限边界不变；查询分类不能授予来源或工具权限。
10. 允许后续新增查询行为，但新增行为必须优先通过策略配置表达，而不是默认新增类型。

### 5.2 非目标

- 不引入 OWL、RDF、SPARQL 或通用本体推理引擎。
- 不将 Facet Catalog 合并进现有 `internal/ontology` 结构知识图。
- 不让 Query Kind 决定 Evidence Source 或工具权限。
- 不重写 Evidence Planner 的 LLM 协议。
- 不在本提案中增加新的网络意图分类调用。
- 不在第一阶段设计复杂的实体层级、实体置信度图或别名推理。
- 不为了消除所有字符串而增加大量只有一个字段的包装类型。
- 不要求一次迁移同时修改所有外部 JSON 协议；可以先保持序列化兼容。

## 6. 目标模型

### 6.1 请求级只保留 QueryPlan

建议将请求级语义收敛为：

```go
type QueryKind string

type QueryPlan struct {
    Kind     QueryKind
    Entities []string
}
```

第一阶段可以复用当前 Retrieval Intent 的值，减少行为迁移：

```text
focused_fact
overview
flow
comparison
inventory
runtime_diagnosis
code_review
```

其中 `code_review` 用于承接当前无法从 `focused_fact` 派生出的回答结构差异。

为了降低迁移风险，第一阶段不要求立即把：

```text
inventory
```

重命名为：

```text
change_analysis
```

但应在 Catalog 中明确 `inventory` 当前代表“需求或变更影响面调查”，避免名称误导。是否重命名应作为单独兼容性决策，不与本次结构收敛捆绑。

### 6.2 删除请求级重复状态

目标状态不再同时保存：

```go
ResponseMode
RetrievalIntent
IntentOrigin
```

改为：

```go
QueryPlan
```

其中：

```text
QueryPlan.Kind
  唯一主分类

QueryPlan.Entities
  不能由 Kind 推导的具体调查对象
```

以下数据按需派生：

```text
Answer instruction       <- QueryKind
Default required facets  <- QueryKind
Retrieval budget         <- QueryKind
Expansion policy         <- QueryKind
Selection policy         <- QueryKind
```

以下数据进入 Trace：

```text
resolution origin
matched rule
fallback reason
signals used
```

### 6.3 QueryKind 不直接授予权限

必须继续保持：

```text
QueryPlan
  描述问题语义和检索形状

EvidencePlan
  描述允许访问的证据来源

Tool Snapshot / Permission Policy
  描述可见工具和执行权限
```

关系为：

```text
QueryPlan + EvidencePlan + Tool Snapshot
  -> effective execution route
```

禁止：

```text
runtime_diagnosis
  -> 自动授权 runtime tool

overview
  -> 自动开启 web
```

Query Kind 可以影响“如何使用已授权来源”，但不能扩大来源和工具权限。

## 7. Facet Catalog

### 7.1 Facet 的唯一语义

Facet 只表示：

> 最终答案中需要由证据支持的内容维度。

第一阶段保留现有七个 canonical Facet：

| Facet | 定义 |
|---|---|
| `system_boundary` | 系统、服务或模块的责任边界及范围 |
| `business_domain` | 业务目标、领域概念与业务职责 |
| `entrypoint` | API、事件、任务、命令或代码入口 |
| `core_flow` | 主要处理步骤、调用链和控制流 |
| `data_and_state` | 数据模型、存储、状态变化和一致性 |
| `external_dependency` | 外部服务、中间件、协议和依赖关系 |
| `runtime_and_operations` | 日志、指标、Trace、部署、告警和运行维护行为 |

### 7.2 不再作为 Facet 的值

以下值不再作为 canonical Facet：

```text
implementation
service.topology
documentation
```

迁移规则：

```text
implementation
  -> 根据任务映射为 entrypoint / core_flow / data_and_state

service.topology
  -> system_boundary / external_dependency

documentation
  -> EvidenceSource、EvidenceClass 或文档覆盖质量
     不作为答案内容 Facet
```

如果未来确实需要“文档完整度”作为答案目标，应建立明确的质量检查目标，例如：

```text
documentation_coverage
```

但它属于评估维度，不应未经评审直接混入内容 Facet。

### 7.3 中央 Catalog

建议建立单一 Catalog，至少包含：

```go
type FacetSpec struct {
    ID          EvidenceFacet
    Description string
}
```

并提供：

```go
func IsKnownFacet(value string) bool
func ValidateFacets(values []string) error
func RequiredFacetsFor(kind QueryKind) []EvidenceFacet
func ProvidedFacetsFor(source EvidenceSource, kind string) []EvidenceFacet
```

Catalog 是代码内的轻量语义注册表，不是持久化图数据库。

### 7.4 三类关系

统一维护：

```text
QueryKind
  --requires--> Facet

EvidenceKind
  --provides--> Facet

Capability
  --supports--> Facet
```

其中：

- `requires` 是默认回答覆盖目标；
- `provides` 是某条具体 EvidenceUnit 能证明的内容维度；
- `supports` 是 Capability 允许承接的目标范围，不代表其每次执行都实际产出全部 Facet。

### 7.5 不建设第二套 Ontology

现有 `internal/ontology` 表达的是工作区结构事实：

```text
repository
service
api_endpoint
code_symbol
external_system
runbook
```

及其关系：

```text
contains
exposes
implemented_by
depends_on
documented_by
```

Facet Catalog 表达的是答案证据覆盖维度。两者用途不同：

```text
Ontology
  回答“工作区里有哪些实体和事实关系”

Facet Catalog
  回答“证据支持答案的哪个维度”
```

本提案不将两者合并，也不增加新的 Ontology Graph。

## 8. 逐项修改方案

### 8.1 修改点一：用 QueryKind 替代双重分类

涉及：

```text
internal/domain/agent.go
internal/domain/retrieval_intent.go
internal/domain/retrieval_intent_test.go
```

修改：

1. 引入唯一 `QueryKind`，第一阶段可复用现有 `RetrievalIntentKind` 的序列化值。
2. 将 `RetrievalIntentKind` 重命名或以兼容 alias 迁移为 `QueryKind`。
3. 删除 `IntentResolution.ResponseMode`。
4. 删除请求链上的独立 `ResponseMode` 状态。
5. 将 `CodeReview` 对应为独立 `QueryKind`，避免回答模式丢失。
6. 保留当前确定性信号规则，但统一由一个 Resolver 产出 QueryPlan。

目标接口：

```go
func ResolveQueryPlan(
    question string,
    signals QuerySignals,
) QueryResolution
```

其中：

```go
type QueryResolution struct {
    Plan QueryPlan
    // Diagnostics 只用于当前边界写 Trace，不继续进入业务状态。
}
```

预期效果：

- 一个请求只有一个主分类；
- 不再维护 ResponseMode 到 RetrievalIntent 的映射；
- 回答和检索不会使用不同分类。

### 8.2 修改点二：QueryPlan 只保留 Kind 和 Entities

涉及：

```text
internal/domain/retrieval_intent.go
internal/agent/qa/query.go
internal/agent/qa/prepare.go
internal/agent/qa/context.go
internal/agent/qa/contracts.go
```

修改：

```go
type QueryPlan struct {
    Kind     QueryKind
    Entities []string
}
```

删除：

```go
RetrievalIntent.RequiredFacets
IntentResolution.Origin
queryAnalysisOutput.ResponseMode
queryAnalysisOutput.IntentOrigin
```

将：

```go
queryAnalysisOutput.RetrievalIntent
```

替换为：

```go
queryAnalysisOutput.QueryPlan
```

约束：

- `Entities` 继续使用 canonical、去重、有界规则；
- 第一阶段不增加 `EntityKind`、`EntityOrigin`、`Confidence` 等字段；
- 如果未来确实需要实体消歧，应基于真实消费需求单独提案。

### 8.3 修改点三：RequiredFacets 改为按需派生

涉及：

```text
internal/domain/retrieval_intent.go
internal/retrieval/evidence_units.go
internal/agent/qa/investigation.go
```

修改：

```go
required := RequiredFacetsFor(plan.Kind)
```

不再通过：

```go
plan.RequiredFacets
```

携带默认值。

`contractFromPreparation` 应从同一 Catalog 生成 `EvidenceGoal`：

```text
QueryPlan.Kind
  -> RequiredFacetsFor
  -> EvidenceGoals
```

禁止在多个构造点复制 Facet 列表。

第一阶段不引入通用 `AdditionalFacets`。只有出现经过验证的真实场景，证明同一个 QueryKind 必须在请求级覆盖默认 Facet 时，才评审以下受控结构：

```go
type FacetOverride struct {
    Include []EvidenceFacet
    Exclude []EvidenceFacet
}
```

不得为了预留扩展性提前加入。

### 8.4 修改点四：回答提示词直接消费 QueryKind

涉及：

```text
internal/agent/execution/prompt_context.go
internal/agent/execution/intent.go
internal/prompts/text/agent/qa/response_mode.txt
```

修改：

1. `BuildMessages` 接收准备阶段已确定的 `QueryKind` 或 `QueryPlan`。
2. 删除 `BuildMessages` 内部的 `ClassifyResponseMode(question)`。
3. 将回答结构提示改为：

```text
AnswerInstructionFor(QueryKind)
```

4. 保持提示词只控制答案组织，不改变工具权限。

建议接口：

```go
func BuildMessages(
    question string,
    query QueryPlan,
    conversation ConversationContext,
    rc *retrieval.RetrievedContext,
    evidencePlan domain.EvidencePlan,
    domainKnowledge string,
    historyLimit int,
) []llm.Message
```

如果调用链过长，可以只传：

```go
queryKind QueryKind
```

但禁止再次从 question 分类。

### 8.5 修改点五：集中 QueryKind 到默认 Facet 的映射

涉及：

```text
internal/domain/retrieval_intent.go
```

修改为单一数据表或单一函数：

```text
focused_fact
  -> []

overview
  -> system_boundary
  -> business_domain
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency
  -> runtime_and_operations

flow
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency

comparison
  -> business_domain
  -> core_flow
  -> data_and_state
  -> external_dependency

inventory
  -> system_boundary
  -> business_domain
  -> entrypoint
  -> data_and_state
  -> external_dependency

runtime_diagnosis
  -> entrypoint
  -> core_flow
  -> external_dependency
  -> runtime_and_operations

code_review
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency
```

以上 `code_review` 默认集合需要通过现有 QA 样例确认，未评估前不应作为既成事实直接提交。

规则：

- 每个 QueryKind 只能在一个位置定义默认 Facet；
- 返回值必须复制，禁止调用方修改 Catalog 内部切片；
- Facet 顺序稳定，用于 Prompt、Trace 和测试；
- `focused_fact` 为空表示不做全维度覆盖要求，不表示不需要证据。

### 8.6 修改点六：集中 Evidence 到 Facet 的映射

涉及：

```text
internal/retrieval/evidence_units.go
internal/agent/tools/registry.go
```

修改：

1. 删除重复的 `runbookFacets` 和局部 `evidenceFacets` 映射。
2. 两条链路统一调用一个 `ProvidedFacetsFor`。
3. `tool.EvidenceUnit.Facets` 继续作为实际产出标签，但必须来自 Catalog。
4. 不能仅根据宽泛 `source_kind` 声称过强覆盖。

第一阶段建议至少修正：

```text
code
  不再默认提供 runtime_and_operations

codegraph
  默认提供 core_flow
  只有明确入口节点时提供 entrypoint

runtime log / metric / trace
  才提供 runtime_and_operations
```

更精确的长期方向是按 Evidence Kind 或 Evidence Class 声明，而不是只按 Source：

```text
code.symbol
  -> entrypoint

code.call_path
  -> core_flow

code.schema
  -> data_and_state

dependency.trace
  -> external_dependency

runtime.log
runtime.metric
runtime.trace
runtime.deployment
  -> runtime_and_operations
```

第一阶段不要求一次建立全部细分 Kind，但必须停止明显不成立的广义映射。

### 8.7 修改点七：统一 Capability Facet 词表

涉及：

```text
agent/capability.go
agent/task_graph.go
internal/agent/catalog/defaults_capability.go
internal/agent/qa/task_graph_plan.go
internal/agent/workflow/investigation.go
```

修改：

1. Capability `InputFacets` 只能引用 canonical Facet。
2. 删除 `implementation`、`service.topology`、`documentation` 作为 Facet 的使用。
3. Investigator 的专业方向由 Capability ID 和 Purpose 表达，不通过发明 Facet 表达。
4. Task `RequiredFacets` 只能是 Capability `InputFacets` 的子集，并且全部通过 Catalog 校验。

目标映射示例：

```text
knowledge.code.inspect
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency（仅当工具确实可分析调用依赖）

knowledge.service.trace
  -> system_boundary
  -> external_dependency
  -> runtime_and_operations（仅当其有运行时工具）

knowledge.docs.verify
  -> system_boundary
  -> business_domain
  -> entrypoint
  -> core_flow
  -> data_and_state
  -> external_dependency
  -> runtime_and_operations
```

`knowledge.docs.verify` 支持哪些 Facet，应由可用 Runbook 种类和工具合同决定，不应该使用 `documentation` 代替内容维度。

`knowledge.web.research` 和 `knowledge.memory.recall` 不应仅因为“可能找到任何内容”就无条件声称支持全部 Facet。至少需要明确：

- Capability 支持表示可承担该调查目标；
- 实际覆盖仍以返回 `EvidenceUnit.Facets` 为准；
- Capability 支持不能直接被当成已完成覆盖。

### 8.8 修改点八：将 IntentOrigin 移入 Trace

涉及：

```text
internal/domain/retrieval_intent.go
internal/agent/qa/query.go
internal/agent/qa/prepare.go
internal/runtrace
```

修改：

业务对象中删除：

```go
IntentOrigin
```

在 Resolver 返回边界直接记录：

```text
query_kind
resolution_origin
matched_rule
fallback_reason
signal_count
entity_count
```

约束：

- `resolution_origin` 使用低基数值，例如 `rule`、`fallback`；
- 完整问题、实体值和匹配文本不进入 metrics label；
- 需要调试时可以进入受控 Trace payload；
- 下游业务逻辑不得根据 origin 改变权限或结果。

### 8.9 修改点九：检索执行策略集中化

涉及：

```text
internal/retrieval/pipeline.go
internal/retrieval/route.go
internal/retrieval/pipeline_test.go
internal/retrieval/route_test.go
```

当前以下行为分散判断 Query Kind：

```text
retrieval budget
overview service limit
flow codegraph expansion
coverage selection mode
```

建议在 retrieval 包内集中为一个私有策略：

```go
type queryRetrievalPolicy struct {
    Budget              retrievalBudget
    MaxExpandedServices int
    ExpandCodeGraph     bool
    CoverageSelection   bool
}

func retrievalPolicyFor(kind domain.QueryKind) queryRetrievalPolicy
```

这是执行策略数据，不是新的业务分类，也不应暴露到跨包 JSON。

规则：

- 新增 QueryKind 必须有明确默认策略；
- 没有特殊需求时使用基础策略，不能因为新增 Kind 自动复制一整套分支；
- `EvidencePlan` 仍决定来源是否可执行；
- 策略只能收紧或调整已授权来源内的预算和展开方式。

### 8.10 修改点十：避免把策略表升级成“万能配置对象”

为了防止收敛后再次膨胀，禁止建立跨层巨型结构：

```go
type QueryPolicy struct {
    AnswerPrompt          string
    Sources               ...
    ToolIDs               ...
    Facets                ...
    RetrievalBudget       ...
    Workflow              ...
    Permissions           ...
    RetryPolicy           ...
    Observability         ...
}
```

推荐做法是：

```text
Domain
  QueryKind -> Default Facets

Retrieval
  QueryKind -> Retrieval Policy

Execution
  QueryKind -> Answer Instruction
```

三者共享同一个 `QueryKind`，但策略仍由各自层拥有。这样既避免重复分类，也避免形成跨层 God Object。

### 8.11 修改点十一：建立 Facet 完整性验证

新增自动验证：

```text
所有 QueryKind 引用的 Facet 都存在于 Catalog
所有 EvidenceUnit Facet 都存在于 Catalog
所有 Capability InputFacet 都存在于 Catalog
所有 Task RequiredFacet 都存在于 Catalog
每个默认 RequiredFacet 至少存在一种可提供的 Evidence Kind
禁止重复 Facet
Facet 顺序稳定
禁止已淘汰别名继续出现在默认配置
```

建议提供：

```go
func ValidateFacetCatalog() error
func ValidateCapabilityFacets(capability Capability) error
func ValidateTaskFacets(task TaskSpec, capability Capability) error
```

对外 JSON 字段第一阶段可以继续使用 `[]string`，但进入系统边界时必须校验并标准化。

### 8.12 修改点十二：统一命名与注释

建议术语统一为：

```text
QueryKind
  用户问题的主任务形态

QueryPlan
  本轮 canonical QueryKind 和目标实体

Facet
  答案需要证据覆盖的内容维度

EvidenceKind
  具体证据形态或来源细类

Capability
  可执行的调查能力

EvidencePlan
  允许使用的证据来源
```

停止混用：

```text
intent
mode
shape
facet
focus
role
source
```

来表达同一件事。

如果保留 `Intent` 术语，只能作为自然语言说明，不再同时保留 `ResponseMode`、`RetrievalIntent` 和 `QueryKind` 三套代码类型。

## 9. 迁移方案

### 9.1 Slice 0：固化当前行为

先增加 characterization tests，覆盖：

- 五种当前 ResponseMode 的分类结果；
- 六种当前 RetrievalIntent 的结果；
- flow 和 comparison 优先级；
- Query Entities canonical 化和上限；
- 每种 Kind 的 retrieval budget；
- overview coverage selection；
- flow CodeGraph 扩展；
- 当前回答提示词选择。

目的不是永久保留双类型，而是确保迁移时明确知道哪些行为发生变化。

### 9.2 Slice 1：建立 Facet Catalog，不改变协议

1. 建立中央 Facet 常量和描述。
2. 将 Intent 到 Facet 映射迁移到 `RequiredFacetsFor`。
3. 将 Evidence 到 Facet 映射迁移到 `ProvidedFacetsFor`。
4. 让 `runbookFacets` 使用统一实现后删除重复函数。
5. 对现有 `[]string` 增加 Catalog 校验。
6. 暂时保留现有字段和 JSON，确保行为兼容。

### 9.3 Slice 2：引入 QueryPlan 并全链传递

1. 新增 `QueryKind` 和 `QueryPlan`。
2. `ResolveQueryPlan` 成为唯一解析入口。
3. `queryAnalysisOutput` 只保存 QueryPlan。
4. `RetrievePlan` 接收 QueryPlan。
5. Task Contract 从 QueryPlan 派生 EvidenceGoal。
6. Prompt 构建接收 QueryKind，不再重分类。

此阶段可以保留兼容转换函数：

```go
func legacyResponseModeFor(kind QueryKind) ResponseMode
```

但只允许在尚未迁移的边界调用，并标记删除时间。

### 9.4 Slice 3：删除派生字段和双重分类

删除：

```text
ResponseMode 请求状态
RetrievalIntent struct
RequiredFacets 字段
IntentOrigin 业务字段
BuildMessages 内重新分类
RetrievalIntentFor(ResponseMode)
```

保留：

```text
QueryKind
QueryPlan.Entities
Trace resolution_origin
```

### 9.5 Slice 4：清理 Capability Facet

1. 替换 `implementation`。
2. 替换 `service.topology`。
3. 移除 `documentation` 内容 Facet。
4. 更新 Capability 默认注册、Task Graph、Workflow 默认计划和测试 fixture。
5. 启用严格 Catalog 校验。
6. 对旧持久化 Capability 版本明确兼容策略：重新发布新版本，不原地修改已发布不可变版本。

### 9.6 Slice 5：修正 Evidence Coverage 语义

1. 移除 `code -> runtime_and_operations` 的默认映射。
2. 根据已有 Evidence metadata 区分 symbol、call path、schema 和 runtime evidence。
3. 检查 overview selection 是否因覆盖变严格而需要调整预算。
4. 用固定问题集验证不会因为 Facet 更准确而造成不可接受的召回缺失。

### 9.7 Slice 6：正式文档合并

更新：

```text
docs/design/agent-platform/01-architecture-and-execution.zh-CN.md
docs/design/agent-platform/02-evidence-and-tooling.zh-CN.md
docs/design/agent-platform/03-retrieval-and-knowledge.zh-CN.md
docs/design/agent-platform/18-task-driven-multi-agent-architecture.zh-CN.md
```

完成后将本提案状态改为：

```text
Implemented
```

并记录实际迁移差异和遗留兼容层。

## 10. 兼容性与风险

### 10.1 JSON 和持久化兼容

`Capability`、`TaskSpec`、`EvidenceUnit` 当前使用 `[]string` 序列化 Facet。第一阶段不要求修改 JSON 形状，只要求值经过 Catalog 校验。

如果 Capability 版本已经发布且按内容哈希不可变：

- 不原地改写历史版本；
- 发布新版本；
- Planner 和默认目录切换到新版本；
- 旧版本进入兼容窗口后下线。

### 10.2 QueryKind 单枚举的表达能力

单一主分类不能无界表达所有复合问题，例如：

```text
比较两个服务为什么在运行时失败
```

本提案的处理原则是：

1. 选择一个主 QueryKind；
2. 通过实体和默认 Facet 表达主要调查范围；
3. 不因为复合问题立即增加多个正交枚举；
4. 只有固定评估证明单一 Kind 无法满足时，才考虑受控的附加 Facet，而不是增加第二套分类系统。

当前 Resolver 已对 comparison 和 flow 设优先级，因此本提案不会新引入这个限制，只是要求优先级显式、可测试。

### 10.3 Facet 收紧可能降低表面 Coverage

移除 `code -> runtime_and_operations` 后，部分请求会显示运行态 Facet 未覆盖。

这是语义纠正，不应通过恢复虚假映射解决。正确做法是：

- 使用 runtime evidence；
- 明确回答中缺少运行态证据；
- 在权限允许时由 Agent 调用日志、Trace 或监控工具；
- 必要时调整 retrieval budget，而不是伪造 coverage。

### 10.4 中央 Catalog 可能成为耦合点

Catalog 必须只承载稳定语义和映射，不承载：

- Provider 配置；
- 工具 ID；
- 权限；
- Prompt 正文；
- 重试策略；
- 工作流拓扑。

各层只通过稳定 `QueryKind` 和 Facet ID 对齐，执行细节仍由各自包拥有。

### 10.5 插件和外部扩展

第一阶段采用封闭 canonical Facet 集。插件不能随意注册无命名空间 Facet。

未来如出现真实扩展需求，可评审：

```text
vendor_or_plugin_id.facet_name
```

但扩展 Facet 不自动进入核心 QueryKind 的 Required Facets，也不能绕过 Capability 和 Evidence 验证。

## 11. 测试与验收标准

### 11.1 结构验收

- 请求链上只有一个 canonical `QueryKind`。
- `QueryPlan` 只保存 `Kind` 和 `Entities`。
- `RequiredFacets` 不再作为 QueryPlan 的可变字段。
- `IntentOrigin` 不再出现在业务计划中。
- Prompt 构建不再从 question 重新分类。
- `ResponseMode` 与 `RetrievalIntentKind` 不再作为两套运行时分类并存。

### 11.2 Facet 验收

- 所有 Facet 来自一个 Catalog。
- 默认配置中不再出现：

```text
implementation
service.topology
documentation
```

- Runbook Facet 映射只有一个实现。
- `code` 不再默认覆盖 `runtime_and_operations`。
- 未注册 Facet 在发布 Capability、编译 Task Graph 或接收 Tool Result 时被拒绝或明确降级。
- 每个 QueryKind 的 Required Facet 都至少有一种可用 Evidence Kind 可以提供。

### 11.3 行为验收

固定问题集至少覆盖：

```text
普通事实查询
系统概览
调用链/写入链路
多实体比较
需求影响面
运行时故障诊断
代码审查
```

验证：

- QueryKind 稳定；
- Answer instruction 与 QueryKind 一致；
- Retrieval budget 与策略符合预期；
- flow 仍能触发 CodeGraph；
- overview 仍能进行 coverage selection；
- runtime diagnosis 在无运行态证据时不会被代码证据误判为已覆盖；
- Task Graph 只分配支持目标 Facet 的 Capability；
- EvidencePlan 和工具权限不因 QueryKind 被扩大。

### 11.4 可观测性验收

Query Analysis Trace 至少记录：

```text
query_kind
resolution_origin
matched_rule_kind
required_facets
entity_count
```

其中 `required_facets` 是 Trace 输出时派生，不是请求状态。

Metrics 不使用：

```text
question
entity value
matched text
```

作为 label。

## 12. 预期收益

### 12.1 类型数量收敛

请求级从：

```text
ResponseMode
RetrievalIntentKind
RetrievalIntent
RequiredFacets
TargetEntities
IntentOrigin
```

收敛为：

```text
QueryKind
QueryPlan{Kind, Entities}
```

Facet 和策略仍存在，但作为中央 Catalog 与派生结果，不再作为多份可变状态传播。

### 12.2 修改成本可控

新增或调整 Query Kind 时，开发者只需要明确：

1. 如何解析；
2. 默认覆盖哪些 Facet；
3. Retrieval 是否需要特殊策略；
4. Answer instruction 如何组织。

不再需要维护两套分类之间的双向一致性。

### 12.3 Coverage 更可信

Facet 只表达内容维度，EvidenceUnit 只声明实际能够证明的 Facet，避免：

```text
代码命中 = 运行态已覆盖
文档来源 = documentation Facet 已覆盖
Capability 支持 = Evidence 已产出
```

### 12.4 防止无限膨胀

本提案建立以下准入规则：

新增 QueryKind 前必须证明：

- 现有 Kind 加策略调整不能表达；
- 需要不同的稳定回答或检索行为；
- 有固定评估样例；
- 不只是某个工具或业务域的局部差异。

新增 Facet 前必须证明：

- 它是答案内容维度；
- 不能由现有 Facet 组合表达；
- 至少有一种 Evidence Kind 可以可靠提供；
- 至少有一种 QueryKind 或显式目标真正需要；
- 不是来源、工具、角色、格式或质量标签。

## 13. 建议决策

建议接受以下架构方向：

1. **合并 `ResponseMode` 和 `RetrievalIntentKind`，只保留 `QueryKind`。**
2. **将请求级结构收敛为 `QueryPlan{Kind, Entities}`。**
3. **`RequiredFacets` 默认从 `QueryKind` 派生，不继续作为独立事实保存。**
4. **`IntentOrigin` 移入 Trace，不进入业务执行计划。**
5. **建立中央 Facet Catalog，统一 `requires / provides / supports` 三类映射。**
6. **清理 `implementation`、`service.topology`、`documentation` 等混合语义 Facet。**
7. **删除 Prompt 层的重复分类，做到一次解析、全链复用。**
8. **集中 Retrieval 的 Kind 策略，但不建立跨层万能 QueryPolicy。**
9. **不建设第二套重量级 Ontology；Facet Catalog 与现有结构本体保持边界。**
10. **按兼容切片逐步迁移，先验证和收口，再删除旧类型。**

## 14. 待评审问题

1. 第一阶段是否保留 `inventory` 名称，还是同步重命名为 `change_analysis`？建议先保留值，避免把语义收敛和协议改名耦合。
2. `code_review` 的默认 Facet 是否包含 `external_dependency`？需要固定样例验证后确定。
3. `knowledge.service.trace` 在没有日志/Trace 工具时，是否还应声明支持 `runtime_and_operations`？建议按实际 Tool Snapshot 决定，而不是静态全局声明。
4. `knowledge.docs.verify` 是否支持全部内容 Facet，还是根据 Runbook kind 动态承接？建议 Capability 声明上限，实际 coverage 以 EvidenceUnit 为准。
5. 旧 Capability 版本和历史 Task Graph 中的 `implementation`、`service.topology`、`documentation` 保留多久兼容窗口？
6. Facet Catalog 放在 `internal/domain`，还是建立一个最小共享包供 `agent` 和 `tool` 公共合同引用？需要结合包依赖和对外 API 稳定性评审；无论放在哪里，只允许存在一个权威实现。
