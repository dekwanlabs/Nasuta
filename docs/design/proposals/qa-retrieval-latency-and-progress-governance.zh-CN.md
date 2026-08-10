# QA Retrieval 首屏时延与进度反馈治理提案

> 状态：实施中，核心链路已实现，生产校准与端到端门禁待完成
> 创建日期：2026-08-10
> 最近更新：2026-08-10
> 范围：QA 请求准备、意图解析、会话历史召回、内部 Retrieval、Rerank、Dependency Expansion 与 Dashboard 实时状态反馈
> 诊断来源：CodeLoom 日志 `all-2026-08-10.log`，请求 `59faffa3fa53`、`ffa90627a16b`
> 关联提案：`qa-evidence-convergence-and-retrieval-governance.zh-CN.md`、`qa-unified-evidence-acquisition-pipeline.zh-CN.md`、`archive/qa-sse-protocol-simplification.zh-CN.md`
> 待修订正式文档：`../agent-platform/03-retrieval-and-knowledge.zh-CN.md`、`../agent-platform/08-observability-and-evaluation.zh-CN.md`

## 0. 文档生命周期

本文用于记录 QA 首屏等待和内部 Retrieval 时延的现状审计、目标链路、实施顺序和验收门禁，不直接替代当前正式架构合同。

只有同时满足以下条件，本文的有效结论才可归并到正式模块文档：

1. 请求准备和 Retrieval 获得默认开启的毫秒级阶段指标；
2. Planner、历史召回、Embedding、向量查询、依赖扩展和 Rerank 的真实 P50/P95/P99 已完成生产校准；
3. Canonical RetrievalIntent、共享 Query Embedding、召回预算、Rerank 降级和分词器预热完成实现；
4. Dashboard 能稳定展示准备和检索进度，首个状态与最长静默时间满足验收门禁；
5. 固定评估集证明延迟下降没有造成不可接受的召回率、证据覆盖率或回答质量回退；
6. 最终实现与本文重新核对，并将稳定合同归并到模块 03 和 08。

在完成上述工作前，本文保持活动 proposal 状态，不进入 `archive/`。

### 0.1 当前实施状态（2026-08-10）

已完成：

- Canonical `RetrievalIntent`、`flow` 收敛、Facet/TargetEntities 有界化，并在 QA Preparation 唯一解析后传入 Retrieval；
- 请求级 Query Embedding 复用、Intent 召回预算、Session History lexical/dense 并发；
- Retrieval 共享向量失败时不再回退到会重复 Embedding 的兼容入口，Runbook/Service 使用显式降级路径；
- Session History Candidate Discovery 与 Evidence Planner 并发，普通历史路径复用候选，强历史依赖保留完整 Recall 语义；
- Rerank、Dependency、GSE tokenizer 和结构化 `status` 阶段治理。

仍待完成：

- 生产 P50/P95/P99、首个状态和最长静默区间的端到端采集；
- 固定评估集质量回归、浏览器 SSE 可见性测试和跨仓库集成验证；
- 全量 Race Test 与既有全仓测试失败项复核。

## 1. 背景与问题定义

用户反馈从输入问题到 Retrieval 输出约需 6 秒，这段时间没有可感知的内容更新，界面接近“卡死”。

日志审计表明，需要先纠正一个关键归因：

> “用户输入到 Retrieval 输出”的总时间，不等于 Retrieval 后端自身耗时。

当前总等待由两部分构成：

```text
请求准备
  -> Evidence Planner
  -> Query Analysis
  -> Active Context Assemble
  -> Session History Recall
  -> Route / Prefetch / Memory / Query Rewrite

内部 Retrieval
  -> Discover
  -> Expand
  -> Rerank
  -> Assemble
```

两条样例中，内部 Retrieval 本身约为 2 至 3 秒；更长且波动更明显的等待发生在 Retrieval 之前。只优化向量检索或 Rerank，不能解决完整的首屏等待问题。

因此本文同时治理两个目标：

1. 降低用户输入到内部证据准备完成的真实时间；
2. 在尚未生成答案文本时，持续提供真实、有限且不误导的进度反馈。

## 2. 范围与非目标

本提案处理：

- Evidence Planner 与会话历史召回的串行等待；
- ResponseMode、RetrievalIntent 与代码图触发条件的重复意图派生；
- Session History 内 lexical、query embedding 和 dense search 的串行等待；
- Code、Runbook、Service 三路发现重复生成相同 Query Embedding；
- Code 和 Runbook 过量召回后大量丢弃；
- Dependency Expansion 的顺序查询与无界尾延迟风险；
- 外部 Rerank 的候选规模、超时和降级策略；
- GSE 分词器首次请求冷启动；
- QA 准备和 Retrieval 阶段的状态反馈与毫秒级可观测性。

本次不做：

- 不更换向量数据库、Embedding Provider 或 Rerank Provider；
- 不改变 EvidencePlan、RetrievalIntent 或 EvidenceUnit 的正确性语义；
- 不新增独立的意图分析 LLM 调用；
- 不把所有 Retrieval 子步骤暴露为新的公共 SSE 事件类型；
- 不使用固定 sleep、伪进度百分比或循环文案掩盖真实等待；
- 不为样例问题、错误码、Matter 业务或某个服务增加特殊规则；
- 不以降低证据正确性为代价追求单次样例的最低耗时；
- 不在没有生产分位数数据前扩大现有超时。

## 3. 当前请求链路

### 3.1 请求准备

`internal/agent/qa/prepare.go` 当前按以下关键路径执行：

```text
emit status: 分析问题
  -> History Candidate Discovery || planEvidence
  -> analyzeQuery
  -> assembleContext
       -> assembleActiveHistory
       -> sessionhistory.Materialize
  -> routeQAExecution
  -> executePrefetch
  -> recallMemory
  -> rewriteQuery
  -> emit status: 查询资料
  -> Retriever.RetrievePlan
```

`planEvidence` 是一次带 12 秒 helper timeout 的 LLM 网络调用。`assembleContext` 必须等 Planner 和 Query Analysis 返回后才开始，其中 Session History Recall 又包含 lexical 查询、Query Embedding 和 dense search。

当最终历史关系需要 prior entities、conclusion、evidence 或显式 continuity 时，`assembleContext` 会保守地回到带 continuity 的完整 `Recall`，避免提前候选改变历史语义。

### 3.2 Retrieval

`internal/retrieval/pipeline.go` 当前执行：

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
  -> code pool dedup
  -> coarse truncate
  -> optional remote rerank
  -> threshold
  -> diversity
  -> context budget selection
```

Discover 的三路来源和 Expand 的主要收集器已经并发执行，但每个来源内部仍有重复网络请求或过量数据处理。

### 3.3 实时状态

后端目前会在准备开始和 Retrieval 开始前发送 `status`：

```text
嗯...让我先琢磨一下你在问什么
好嘞，关键词到手了，我去查一下资料
```

RunHub 将 `status` 作为可合并的 transient 事件，Dashboard SSE 会 Flush，当前前端也具备 `status` 消费和展示逻辑。

因此代码层面并非完全没有状态通路，但仍有三个问题：

1. 日志不能证明生产请求中的状态确实在规定时间内到达并渲染；
2. Planner、History 和 Retrieval 内部缺少子阶段进度，单个状态可能停留数秒；
3. transient 状态不持久化，刷新、重连和链路异常时无法还原用户实际看到的进度。

### 3.4 当前意图分析链路

当前系统仍有意图分析，但它不是一条独立、统一的分类链路，而是分散在三个层次：

| 层次 | 当前所有者 | 是否网络调用 | 主要输出 | 当前用途 |
|---|---|---:|---|---|
| Evidence Planner | `internal/agent/qa/evidence.go`、`internal/retrieval/route.go` | 是，一次 LLM | Evidence source、Tool IDs、Query Terms、Time、History Relation、Execution Strategy | 决定证据来源、工具路由、历史关系与执行策略 |
| ResponseMode | `internal/domain/agent.go` | 否，本地确定性规则 | `bug_analysis`、`requirements_analysis`、`architecture_review`、`code_review`、`codebase_qa` | 决定回答结构和表达模式 |
| RetrievalIntent | `internal/domain/retrieval_intent.go` | 否，本地确定性映射 | `focused_fact`、`overview`、`flow`、`inventory`、`runtime_diagnosis` | 描述 Retrieval 所需的证据形状和 Facet |

Evidence Planner 的一次 LLM 调用已经包含广义的路由意图分析。样例请求中耗时 3 至 7 秒的 Planner 网络调用也包含这部分工作，因此当前没有、也不应再增加第二次独立的意图分析 LLM 请求。

当前 RetrievalIntent 由 `RetrievalIntentFor(ClassifyResponseMode(rawQuestion))` 本地派生，不增加网络调用，但存在以下断点：

1. `planEvidence` 的 trace 为观测重新分类一次，`RetrievePlan` 又根据 `rawQuestion` 重新分类一次，没有把准备阶段的唯一结果传入 Retrieval；
2. `flow` 已定义，但 `RetrievalIntentFor` 的统一映射不会产出它，代码图扩展改由 `shouldExpandCodeGraph` 的独立正则和标识符判断触发；
3. `TargetEntities` 已定义，但当前没有统一填充；
4. `overview` 已用于限制 Service Expansion 和 Facet Coverage Selection，其他 Intent 对召回预算、依赖方向和扩展策略的影响仍然有限；
5. ResponseMode、RetrievalIntent 和 EvidencePlan 的权限边界没有在一个入口显式表达，后续容易把“回答方式”“证据形状”和“允许访问的来源”混在一起。

目标意图链路为：

```text
Evidence Planner（唯一一次 LLM）
  -> source / tool / time / history / execution
  -> Local Intent Resolver
       -> ResponseMode
       -> canonical RetrievalIntent
       -> RequiredFacets
       -> TargetEntities
  -> retrieval budget / expansion policy
```

约束：

- 一次请求只生成一次 canonical `RetrievalIntent`，作为准备结果传入 `RetrievePlan`，Retrieval 内不再根据原始问题重算；
- Local Intent Resolver 以本地确定性信号为基础，可使用 Planner 已返回的 Query Terms、Time、History 和 Execution 进行补充，但不再发起网络调用；
- `flow` 纳入统一 Resolver，代码标识符和调用链关键词作为确定性信号，不再作为旁路意图系统；
- 无法识别或 Planner 失败时回退到 `focused_fact`，本地 Resolver 必须可以独立工作；
- Intent 只描述证据形状和预算，不能授予 Provider、Evidence Source、Tool 或写操作权限；
- 多意图问题未来通过有界 `RequiredFacets` 和 `TargetEntities` 表达，不串行调用多个分类器。

## 4. 样例时延审计

当前普通日志只有秒级时间戳，以下数值是阶段区间，不是精确的毫秒级 Duration。

### 4.1 请求 `59faffa3fa53`

```text
10:04:22  请求进入，Evidence Planner 发起
10:04:25  Session History Recall 完成
10:04:25  Evidence Plan 与 Retrieval Query 准备完成
10:04:26  Runbook Search 完成
10:04:27  Code / Service Search 完成
10:04:28  Dependency、Rerank、Assemble 完成
```

估算：

| 区间 | 耗时 |
|---|---:|
| 请求进入到 Retrieval Query 就绪 | 约 3 秒 |
| Retrieval Query 就绪到 Retrieval 输出 | 约 3 秒 |
| 请求进入到 Retrieval 输出 | 约 6 秒 |

该请求同时承担了 GSE 中文分词词典首次加载，日志在 `10:04:27` 记录加载完成。第二条样例没有重复加载，说明它是进程级冷启动成本，不应由首个用户请求承担。

### 4.2 请求 `ffa90627a16b`

```text
10:08:46  请求进入，Evidence Planner 发起
10:08:53  Session History Recall 与 Evidence Plan 完成
10:08:53  Retrieval Query 准备完成
10:08:54  Code / Runbook / Service Discover 完成
10:08:55  Dependency、Rerank、Assemble 完成
```

估算：

| 区间 | 耗时 |
|---|---:|
| 请求进入到 Retrieval Query 就绪 | 约 7 秒 |
| Retrieval Query 就绪到 Retrieval 输出 | 约 2 秒 |
| 请求进入到 Retrieval 输出 | 约 9 秒 |

这条请求表明：即使把内部 Retrieval 从 2 秒压缩到 1 秒，用户仍可能先等待约 7 秒。首屏治理不能只修改 `internal/retrieval`。

## 5. 已确认问题

### 5.1 P0：Planner 与历史召回形成串行关键路径

改造前 Planner 完成后才执行 Query Analysis 和 Context Assemble，Session History Recall 位于 Context Assemble 内部。当前已将其中可提前的 Candidate Discovery 从 `Recall` 拆出，并在 QA Preparation 启动时与 Planner 并发。

改造前影响：

- Planner Provider 的网络波动直接推迟历史召回开始时间；
- Planner 重试、JSON 修复或接近 helper timeout 时，后续所有步骤一起等待；
- 历史候选发现本可使用原始问题，却被 Planner 的最终结构化输出阻塞；
- 两个独立的远程耗时无法重叠。

目标不是简单地把整个 `assembleContext` 与 Planner 并发，因为 History Relation、continuity 和最终选择仍依赖分析结果。当前实现把历史召回拆为两个阶段：

```text
history candidate discovery
  -> lexical candidates
  -> dense candidates

history selection/materialization
  -> 使用 Planner / Query Analysis 的 relation 与 continuity
  -> 相关性门槛
  -> 加载权威摘要
  -> token budget selection
```

第一阶段可以与 Planner 并发，第二阶段在分析结果返回后快速收敛。

### 5.2 P0：状态链路存在，但阶段粒度不足且缺少到达验证

当前主要状态发生在：

- Planner 开始前；
- Retrieval 开始前；
- Agent Answer 开始前。

这只能告诉用户系统处于大阶段，无法解释 Planner、History、Embedding、Discover 或 Rerank 中的数秒等待。

目标状态仍使用现有 `status` 事件，不增加一组 Retrieval 专用公共事件。状态 Payload 可增加稳定、可选的 `code` 和阶段耗时：

```json
{
  "code": "retrieval.discover",
  "text": "正在查找相关代码、文档和服务",
  "elapsed_ms": 820
}
```

建议状态码：

```text
prepare.planning
prepare.history
prepare.routing
retrieval.embedding
retrieval.discover
retrieval.expand
retrieval.rerank
retrieval.ready
```

不发送虚假百分比。阶段开始、关键子任务完成和降级发生时发送状态；预计超过 1.5 秒的阶段可以发送一次带已耗时的更新，不使用固定高频心跳。

### 5.3 P1：相同 Query 最多重复生成三次 Embedding

Discover 并发调用：

```text
FindCode
FindRunbooks
FindServices
```

三条路径各自调用 `embedder.Embed(ctx, []string{query})`。在当前部署中，这意味着同一个 query 最多产生三次 Embedding 网络请求。

并发可以降低理想情况下的墙钟时间，但不能消除：

- 重复 Provider 处理；
- 共享连接池和限流竞争；
- Embedding 服务负载；
- 慢请求放大整体尾延迟；
- 同一请求的重复计费和观测噪声。

目标是在 Retrieval 边界建立请求级 Query Embedding 复用：

```text
query text
  -> resolve embedding profile
  -> one embedding per profile
  -> code / runbook / service vector search
```

Embedding 不能只按 query 文本复用，缓存键至少必须包含：

```text
provider
model
dimension
normalization
index embedding version
query text
```

如果 Code、Runbook 和 Service 的索引使用不同 Embedding Profile，则每个 Profile 生成一次，不强行共享不兼容向量。交互式工具调用继续允许自行 Embedding；本次只收敛一次 Retrieval Plan 内的重复调用。

### 5.4 P1：过量召回后大量丢弃

当前固定来源限制：

```text
codeSourceRecallLimit    = 20
runbookSourceRecallLimit = 20
serviceSourceRecallLimit = 8
```

Code Search 使用：

```text
fetchLimit = min(max(limit * 8, 40), 200)
```

当 limit 为 20 时，语义后端实际返回 160 条。Runbook Search 使用 `limit * 4`，会拉取 80 条。

样例日志显示：

```text
code semantic backend returned 160 hits
code pool input: 40 docs
rerank: 40 scored
threshold: 40 -> 20
diversity: 20 -> 10
```

这是一条典型的“先大召回、再大量丢弃”链路，浪费向量查询、结果转换、远程 Rerank、内存和日志成本。

目标是按 `RetrievalIntent` 和证据缺口分配召回预算，而不是所有请求使用同一规模：

| RetrievalIntent | Code 初选 | Runbook 初选 | Service 初选 | Rerank 候选上限 |
|---|---:|---:|---:|---:|
| `focused_fact` | 12 | 8 | 6 | 20 |
| `runtime_diagnosis` | 16 | 12 | 8 | 24 |
| `inventory` | 16 | 12 | 8 | 24 |
| `flow` | 16 | 8 | 6 | 24 |
| `overview` | 16 | 16 | 8 | 24 |

上述数值是首轮 proposal 默认值，最终值必须由固定评估集和生产分位数校准。Code fetch multiplier 建议先从 8 降到 3 至 4，并设置不高于 64 的默认上限；Runbook multiplier 建议从 4 降到 2 至 3。

### 5.5 P1：外部 Rerank 的 8 秒等待窗口过长

当前外部 Rerank 使用 8 秒 timeout。失败后虽然会回退到 recall order，但用户首屏仍可能先等待完整 timeout。

两条样例中 Rerank 在约 1 秒日志区间内结束，但秒级日志无法排除偶发尾延迟。

目标策略：

1. 首屏 Retrieval 的 Rerank 独立时间预算默认不超过 1.5 至 2 秒；
2. 超时、取消、限流或 Provider 错误时立即按 recall order 降级；
3. 候选数不大于最终 TopK 时跳过远程 Rerank；
4. Recall 置信度过低时沿用现有 preflight skip；
5. 高置信度且 TopK 分差足够时允许跳过远程 Rerank；
6. 记录 `mode=remote|skipped|recall_after_error` 和准确耗时。

Rerank 是质量增强器，不应成为首屏可用性的单点阻塞。

### 5.6 P2：Dependency Expansion 按 Service 顺序查询

`collectDependencyEdges` 当前逐个 Service 调用：

```text
TraceDeps(service, both, depth=2)
```

最多保留 30 条边，但样例出现：

```text
services=1/19 edges=30 omitted_edges=48
services=10/20 edges=30 omitted_edges=27
```

这说明 Dependency 的查询范围和结果预算没有按证据价值收敛：

- 单个高连接 Service 即可填满预算；
- 低置信 Service 仍可能进入顺序查询；
- 已达到边数上限后才停止，之前的查询成本无法回收；
- `both` 同时扩展 upstream 和 downstream，可能超过问题实际需要。

目标策略：

1. 只选择高置信 Anchor Service，默认最多 3 个；
2. 根据问题意图选择 `upstream`、`downstream` 或 `both`；
3. 设置独立边数预算和时间预算；
4. 优先支持 Ontology Store 批量查询；
5. 未提供批量接口前，可以有界并发，但不得为追求并发制造大量无用查询；
6. Dependency 超时只降低依赖证据完整度，不阻塞其他已完成证据进入 Assemble。

### 5.7 P2：Session History 内部仍有可并行步骤

当前 `sessionhistory.findHistory` 先执行 lexical 查询，再执行：

```text
query embedding
  -> dense vector search
```

lexical 查询和 Query Embedding 不依赖彼此，应并发执行。Dense Search 只依赖 Embedding，可以在向量返回后立即开始。

目标链路：

```text
extract query terms
  -> lexical search -------------------+
  -> query embedding -> dense search --+-> merge -> relevance filter
```

错误语义保持不变：

- dense 不可用时 lexical 可独立降级；
- lexical 失败仍按当前错误合同处理；
- 上层取消必须终止两个分支；
- 不为并发引入无限 goroutine 或脱离请求生命周期的后台任务。

### 5.8 P2：GSE 冷启动由首个请求承担

样例 `59faffa3fa53` 在 Retrieval 期间首次加载 GSE 词典，约占一个秒级日志区间。该成本只出现在进程冷启动后的首次使用。

目标是在服务启动或 readiness 前显式预热分词器：

```text
initialize tokenizer
  -> load dictionaries
  -> run minimal tokenize probe
  -> mark retrieval dependency ready
```

预热失败应记录明确错误，并根据系统现有降级策略决定阻止 readiness 或禁用 lexical/sparse 能力，不能把初始化错误延迟到首个用户请求。

### 5.9 P0：普通请求缺少毫秒级关键路径数据

执行 trace 已定义多个 `DurationMS` 节点，但普通请求不一定启用完整 evaluation trace。现有业务日志主要使用秒级时间戳，因此只能判断阶段区间，不能回答：

- 三次 Embedding 各自耗时多少；
- Code、Runbook、Service 向量查询谁构成 Discover 关键路径；
- Dependency 与 Rerank 是否存在偶发 P99；
- Planner 的 7 秒发生在 Provider、重试、JSON repair 还是本地等待；
- 状态事件何时生成、写入、到达浏览器并完成渲染。

低开销阶段指标必须默认开启，完整输入输出和高基数 trace 仍按现有诊断开关控制。

### 5.10 P0：意图拥有者分散且在 Retrieval 内重复派生

当前 Planner 已经解析来源路由、查询词、时间、历史关系和执行策略，但 RetrievalIntent 没有成为准备阶段的 canonical 输出。`RetrievePlan` 再次读取原始问题进行 ResponseMode 分类和 Intent 映射，代码图又维护独立的 `flow` 触发规则。

这会造成：

- 同一请求的意图事实可能在 trace、准备阶段和 Retrieval 阶段出现漂移；
- `flow`、`TargetEntities` 和 RequiredFacets 无法统一驱动召回预算；
- 新增 Intent 时需要同时修改分类、映射和旁路正则；
- Planner 的结构化结果没有充分复用，却存在新增第二次 LLM 分类器的诱因；
- 无法稳定记录一次请求最终采用了哪个 Intent、来源于哪种信号。

目标是在 QA Preparation 建立唯一的 Local Intent Resolver：

```text
raw question
  + deterministic query analysis
  + optional planner output
  -> ResponseMode
  -> canonical RetrievalIntent
  -> RetrievePlan input
```

Resolver 本身必须是本地计算。Planner 已经是准备阶段唯一一次 LLM，意图收敛不得增加新的 LLM 或网络关键路径。

## 6. 目标链路

目标关键路径：

```text
run.started + prepare.planning status
  |
  +-> Evidence Planner -----------------------------+
  |                                                 |
  +-> History Candidate Discovery                   |
        +-> lexical search                          |
        +-> query embedding -> dense search         |
                                                    v
                                      Query Analysis / History Selection
                                                    |
                                      Local Intent Resolver
                                      -> ResponseMode
                                      -> canonical RetrievalIntent
                                      -> facets / target entities
                                                    |
                                      Route / Prefetch / Memory / Rewrite
                                                    |
                                      retrieval.embedding status
                                                    |
                               one Query Embedding per compatible profile
                                                    |
                                      retrieval.discover status
                                                    |
                         +-> code vector search -----+
                         +-> runbook vector search --+-> Anchor
                         +-> service vector search --+
                                                    |
                                      retrieval.expand status
                                                    |
                         +-> services / code / docs
                         +-> budgeted dependency expansion
                                                    |
                                      retrieval.rerank status
                                                    |
                                      bounded rerank or fallback
                                                    |
                                      retrieval.ready status
                                                    |
                                           Agent answer
```

目标链路遵循五个原则：

1. 不依赖彼此的网络调用并发执行；
2. 相同且兼容的计算在请求内只执行一次；
3. 质量增强步骤有独立预算并可降级；
4. 每个请求只解析一次 canonical RetrievalIntent；
5. 每个长阶段都有真实状态和准确耗时。

## 7. 时延预算

在生产分位数校准前，先使用以下工程目标作为验收门禁：

| 指标 | 目标 |
|---|---:|
| 请求接收至首个 `status` 写出 | P95 不超过 200 ms |
| 答案首个 Delta 前最长无状态更新区间 | P95 不超过 1.5 s |
| 请求准备总耗时 | P95 不超过 3 s |
| 内部 Retrieval 总耗时 | P95 不超过 2 s，P99 不超过 4 s |
| 请求进入至 Retrieval Ready | P95 不超过 4 s |
| Rerank 等待 | 默认上限 1.5 至 2 s |
| Dependency Expansion | 默认上限 500 ms，超时可降级 |

这些目标不通过扩大总请求超时实现。任一子阶段耗尽预算后，应按其正确性等级选择：

- 必需阶段失败并返回明确错误；
- 可降级阶段保留已完成结果继续；
- 可选质量增强阶段跳过或使用本地顺序。

## 8. 进度反馈合同

进度反馈继续复用 `status`，不改变 `run.finished` 的唯一终态语义。

### 8.1 发送规则

- 请求进入并订阅完成后立即发送准备状态；
- 阶段开始时发送一次；
- 并行子任务完成一个或发生降级时发送一次有信息量的更新；
- 同一阶段预计超过 1.5 秒时，允许发送一次带 `elapsed_ms` 的更新；
- 相同状态可以合并，不能阻塞 Agent；
- 状态丢失不改变回答、工具或终态；
- 不发送逐百分比、固定循环文案或与真实执行无关的动画状态。

### 8.2 验证边界

状态可见性需要端到端验证四个时间点：

```text
status_created_at
status_enqueued_at
status_flushed_at
status_rendered_at
```

后端 metrics 只记录前三个低基数时延。`status_rendered_at` 由前端埋点采样上报，不携带问题正文或敏感内容。

必须补充真实浏览器测试：

1. Planner 人工延迟 5 秒，页面在 200 ms 内出现准备状态；
2. Retrieval 三路分别延迟，页面能看到阶段更新；
3. Rerank 超时后状态显示降级，最终回答正常完成；
4. SSE 断流不能把状态误判为成功答案；
5. 慢订阅者不能阻塞 Retrieval 或 Agent。

## 9. 可观测性合同

每次请求默认记录以下低开销 Duration：

```text
qa_prepare_duration_ms
evidence_plan_duration_ms
query_analysis_duration_ms
intent_resolution_duration_ms
active_history_assemble_duration_ms
session_history_total_duration_ms
session_history_lexical_duration_ms
session_history_embedding_duration_ms
session_history_dense_search_duration_ms
retrieval_total_duration_ms
retrieval_embedding_duration_ms
retrieval_discover_duration_ms
retrieval_code_search_duration_ms
retrieval_runbook_search_duration_ms
retrieval_service_search_duration_ms
retrieval_expand_duration_ms
retrieval_dependency_duration_ms
retrieval_rerank_duration_ms
retrieval_assemble_duration_ms
status_first_flush_duration_ms
status_max_silence_duration_ms
```

同时记录有界结果字段：

```text
embedding_profiles
embedding_calls
code_fetch_count
code_selected_count
runbook_fetch_count
runbook_selected_count
service_selected_count
dependency_services_queried
dependency_edges_selected
rerank_candidates
rerank_mode
rerank_fallback_reason
tokenizer_warm
response_mode
retrieval_intent
intent_origin
required_facets_count
target_entities_count
```

约束：

- Request ID、Session ID、query 文本、错误正文不得作为 metrics label；
- 完整请求时间线进入 trace 或结构化日志；
- metrics 使用阶段、结果模式、Provider 类型等低基数字段；
- Duration 使用单调时钟计算，不从秒级日志时间戳反推；
- 并行阶段同时记录总墙钟时间和各子任务时间，不能简单相加解释关键路径；
- `intent_origin` 只允许 `local`、`planner_assisted`、`fallback` 等低基数枚举；
- `TargetEntities` 的具体值进入受控 trace，不作为 metrics label。

## 10. 开发切片

### P0：建立基线与端到端状态验证

1. 为准备、History、Retrieval、各来源、Dependency 和 Rerank 增加毫秒级 Duration；
2. 记录首个状态写出和最大状态静默时间；
3. 增加浏览器 SSE 状态可见性测试；
4. 用固定问题集采集改造前 P50/P95/P99、候选数量和答案质量；
5. 不在指标建立前直接调整所有阈值。

### P1：缩短准备阶段关键路径

1. 将 Session History 拆为 candidate discovery 与 selection/materialization；
2. History Candidate Discovery 与 Evidence Planner 并发；
3. Session History lexical 与 dense 分支并发；
4. 在 QA Preparation 增加唯一的 Local Intent Resolver；
5. 将 canonical RetrievalIntent 随准备结果传入 `RetrievePlan`，删除 Retrieval 内的重复分类；
6. 将 `flow` 和代码图确定性信号收敛到统一 Resolver，并填充有界 TargetEntities；
7. 保持 Relation、continuity、相关性门槛和 token budget 的最终语义不变；
8. Planner 失败时沿用确定性分析、Intent fallback 和 History 降级，不等待无意义的后续重试。

### P2：收敛 Retrieval 重复计算

1. 建立请求级、按 Embedding Profile 隔离的 Query Vector Bundle；
2. Code、Runbook、Service Search 接受可选预计算向量；
3. 保留独立工具调用的自包含能力；
4. 增加测试证明不兼容 Profile 不会共享向量；
5. 增加指标保证一次 Retrieval Plan 每个 Profile 最多一次 Embedding。

### P3：降低候选规模与尾延迟

1. 按 RetrievalIntent 配置来源召回预算；
2. 降低 Code 和 Runbook fetch multiplier；
3. 限制进入远程 Rerank 的候选数量；
4. 将 Rerank timeout 收敛到首屏预算并验证 recall fallback；
5. Dependency 只查询高置信 Anchor，并增加方向、边数和时间预算；
6. 启动时预热 GSE 分词器。

### P4：质量评估与正式归并

1. 对固定标注集比较 Recall@K、MRR/NDCG、Evidence Coverage 和答案正确率；
2. 比较改造前后 P50/P95/P99 和 Provider 调用数；
3. 对 Provider 限流、Rerank 超时、Embedding 失败、Ontology 慢查询执行故障注入；
4. 根据生产数据校准预算，不保留仅为样例调优的阈值；
5. 将最终合同归并到模块 03 和 08；
6. 本文记录实施结果后进入 `archive/`。

## 11. 验收门禁

1. 请求接收后 P95 200 ms 内完成首个 `status` 写出；
2. 答案首个 Delta 前，P95 不出现超过 1.5 秒的无状态区间；
3. `qa_prepare_duration_ms` 与 `retrieval_total_duration_ms` 可以独立查询；
4. 不增加独立意图分析 LLM 或其他网络调用；
5. 每个请求只生成一次 canonical RetrievalIntent，`RetrievePlan` 不再根据原始问题重算；
6. `flow` 可以通过统一 Intent Resolver 产出，代码图不再维护旁路意图分类；
7. Planner 失败时本地 Resolver 能产生稳定 fallback；
8. Intent 不得扩大 EvidencePlan、Provider、Tool 或写操作权限；
9. 一次 Retrieval Plan 对每个兼容 Embedding Profile 最多调用一次 Query Embedding；
10. 不兼容模型、维度、归一化或索引版本绝不复用向量；
11. `focused_fact` 默认 Code 后端 fetch 不再固定拉取 160 条；
12. 远程 Rerank 超时不会让 Retrieval 等待完整 8 秒；
13. Rerank、Dependency 或某一非必需来源失败时，已完成证据可以继续 Assemble；
14. GSE 词典加载不再出现在首个用户请求关键路径；
15. Planner 与 History Candidate Discovery 的墙钟时间接近两者最大值，而不是两者之和；
16. Session History lexical 与 dense 分支取消、错误和降级行为通过测试；
17. 固定评估集的 Recall@K、证据覆盖率和答案正确率不出现超过约定阈值的回退；
18. SSE 状态测试、Retrieval 单元测试、Race Test、集成测试和跨仓库前端测试通过；
19. 日志和 metrics 不记录 query 正文、历史正文、认证信息或其他敏感数据。

## 12. 待实现时确认

- History Candidate Discovery 是否使用原始问题，还是使用不依赖 Planner 的确定性清洗结果；默认使用确定性清洗结果，避免等待 LLM。
- Local Intent Resolver 放在 `qaPreparation` 还是独立 domain service；默认由 QA Preparation 调用 domain 层纯函数，并将结果显式传入 Retrieval。
- Planner 的哪些结构化字段可以辅助 Intent；默认只使用已有 Query Terms、Time、History 和 Execution，不扩展 Planner Prompt 来增加首屏风险。
- TargetEntities 的上限和标准化规则；默认只保留已在问题或 Planner Query Terms 中出现的有依据实体，并设置较小上限。
- Query Vector Bundle 放在 `Retriever`、工具 Service 还是独立 Query Runtime；默认由 `Retriever` 持有请求级生命周期，避免把跨来源缓存泄漏为全局状态。
- Code、Runbook 和 Service 当前是否始终共享同一 Embedding Profile；实现前必须读取索引元数据验证，不能只依据配置中的模型名判断。
- Rerank 跳过所需的高置信分差如何定义；默认先做 shadow 观测，不在没有评估数据时直接启用。
- Dependency 的 500 ms 预算是否适合本地 SQLite 数据规模；默认先记录分位数，再决定最终值。
- 状态 Payload 是否直接扩展 `TextEvent`，还是保持只传 Text 并在后端内部记录 Code；默认增加可选 Code，不新增 SSE Event Type。
- 前端 `status_rendered_at` 的采样比例和上报入口；默认低比例采样，并严格禁止携带用户问题正文。

## 13. 预期结果

完成本提案后，QA 首屏链路应从：

```text
Planner
  -> History
  -> 3 x Query Embedding
  -> 大规模召回
  -> 远程 Rerank
  -> Retrieval Ready
```

收敛为：

```text
Planner || History Candidate Discovery
  -> local canonical intent resolution
  -> one Query Embedding per compatible profile
  -> intent-budgeted parallel search
  -> bounded dependency expansion
  -> bounded rerank or immediate fallback
  -> Retrieval Ready
```

用户侧不再把整个等待理解为“系统卡死”；工程侧也能明确区分准备慢、Embedding 慢、某个来源慢、Dependency 慢和 Rerank 慢，而不是继续依赖秒级日志猜测。
