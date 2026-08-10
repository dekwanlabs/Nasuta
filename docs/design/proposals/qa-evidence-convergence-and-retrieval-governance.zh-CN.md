# QA 证据收敛、历史相关性与工具准入治理提案

> 状态：部分实现（核心链路已落地，生产校准与正式文档合并待完成）
> 创建日期：2026-08-09
> 最近更新：2026-08-09
> 范围：Planner LLM 调用、会话历史召回、QA 预检索、Agent 工具执行与同一 run 上下文治理
> 诊断来源：CodeLoom trace `5ebdb0d6c561`
> 关联提案：`qa-agent-context-budget-and-cancellation.zh-CN.md`、`qa-evaluation-findings-improvement.zh-CN.md`
> 待修订正式文档：`../agent-platform/04-context-session-and-tool-results.zh-CN.md`、`../agent-platform/08-observability-and-evaluation.zh-CN.md`

## 0. 文档生命周期

本文是针对 QA 证据链不收敛问题的独立开发提案，不直接修改正式架构基线。

执行顺序：

1. 固化 trace `5ebdb0d6c561` 暴露的事实和术语，先纠正错误观测含义。
2. 为 Planner 重试和历史候选建立可观测但不改变结果的 shadow 数据。
3. 校准并启用历史相关性硬门槛。
4. 引入覆盖目标、同一 run 证据账本和工具执行前准入。
5. 复用已有 run token ledger，完成固定数据集评估和回归测试。
6. 将最终合同合并到正式上下文、检索、工具执行和可观测性文档。
7. 标记本文为 `Implemented`，记录实际阈值、指标和遗留项。

当前状态：Slice 1、3、4、5 和工具预算准入的核心代码已落地；固定数据集评估、
指标导出、embedding 模型版本绑定、更多工具的 scope/result contract 和正式文档合并
仍待完成，因此本文尚不标记为 `Implemented`。

### 0.1 实际实现记录

截至 2026-08-09，已落地：

- 公共 LLM 调用层记录 transport attempt 和 JSON repair 事件，包含逻辑调用序号、
  attempt、耗时、稳定错误类型、状态码、是否重试和 backoff；结构化输出终态错误不再
  携带模型 response body。
- Session history 使用独立 dense-only 查询保留 cosine score，与有界 SQL lexical
  evidence 在本地 O(n) 合并；相关性门槛在加载摘要和扩展邻居前执行。
- 当前首轮 enforce 阈值为：

```text
dense-only: dense >= 0.80
dense + lexical: dense >= 0.70 AND lexical coverage >= 0.35
lexical-only: lexical coverage >= 0.70 AND matched terms >= 2
```

- `RetrievalIntent` 当前由既有 `ResponseMode` 在 domain 边界确定性派生，未扩大
  Planner JSON schema；overview 预展开最多 4 个锚点服务，coverage selection 最多
  保留 8 个有增量的 evidence part。
- `RetrievedContext`、`agent.ContextBlock` 和 prefetch `ContextBlock` 均可携带
  canonical `EvidenceUnit`；实际投影发生路径清洗或截断后会同步更新或保守移除
  manifest，避免账本声称未进入 Prompt 的内容已经存在。
- Agent execution 建立 run-local evidence ledger；full target 覆盖 target 及其章节
  请求，section/chunk evidence 按 canonical identity 去重，不使用标题或正文模糊匹配。
- `search_runbooks` 已声明 doc/query scope、结果 evidence units 和稳定结果上界：
  `256 + limit * 1800` tokens；通用未声明工具使用 4096 tokens 的保守上界。
- 工具执行前支持 `allow`、`narrow`、`already_available` 和 `deny_budget`，并保留
  执行后的 delivery guard；已完整覆盖和超预算调用不会进入后端实现。

尚未完成：

- 固定标注集、shadow 周期及本文第 7 节的量化验收结果。
- 历史阈值与 embedding 模型版本的持久配置和启动期兼容性校验；当前通过
  `RelevancePolicy` 暴露覆盖入口，默认值仍由代码提供。
- attempt/history/retrieval/admission 的完整 metrics exporter；当前以 trace 为主。
- `deny_no_increment` 和显式 unresolved goals；当前重复 full scope 使用
  `already_available`，其余调用按 canonical coverage 和预算判定。
- 对支持 section/page/cursor 的更多工具补齐确定性 partial narrowing；当前
  `search_runbooks` 后端只支持 doc/query/limit，不能伪造章节过滤能力。
- 固定评估通过后的正式 agent platform 文档合并。

## 1. 评审范围

本提案评审以下链路：

```text
POST /api/qa/ask
  -> prepareQA
  -> Planner 生成 EvidencePlan
  -> 会话历史候选召回
  -> QA 内部预检索
  -> 构造 seed context
  -> Agent reason
  -> 工具执行前准入
  -> tool execution
  -> 工具结果投影
  -> 下一轮 reason 或结束回答
```

评审维度包括：

- Planner 一次逻辑调用内部发生了什么重试
- 历史候选是否在读取正文和扩展邻居前通过相关性门槛
- 概览类问题是否按权威性和覆盖度收敛
- 预检索与 Agent 工具是否共享当前 run 的证据身份和覆盖范围
- 工具调用是否在执行前检查预算、重复度和未满足目标
- 观测字段是否准确表达实际行为

本提案不处理：

- Provider 选择或故障时的隐式替换
- 新的向量数据库或 embedding Provider
- 用 LLM 二次总结工具结果
- 为 trace 中的具体问题、文档或服务增加特殊分支

## 2. 总体结论

trace `5ebdb0d6c561` 不是一个孤立的召回错误，而是五个机制没有共享同一套“证据准入”语义：

1. Planner 有重试能力，但没有 attempt 级观测，无法区分网络重试、状态码重试和 JSON 修复。
2. 会话历史只做候选召回和 rank fusion，没有相关性硬门槛。
3. 概览问题仍按固定来源数量扩散，而不是先取权威总览、再补未覆盖维度。
4. 预检索只向 Agent 传入一块扁平文本，工具无法识别哪些文档和章节已经存在。
5. 工具结果只在执行后检查是否能交付，无法在调用前判断重复和预算收益。

需要先纠正一个关键术语：

```text
dense=64 lexical=0 fused=64 revalidated=64
```

这里的 `revalidated=64` 不表示 64 条结果通过了语义相关性复核。
`LoadHistorySummaries` 只确认候选引用仍存在，并且仍属于当前用户和 Session；
日志中的 `revalidated` 实际等于成功加载的记录数。当前链路没有任何 semantic
score threshold，dense 命中的原始 `Score`、`ScoreKind`、`DenseScore` 和
`FusionScore` 在融合前已被丢弃。

因此目标机制应统一为：

```text
候选召回
  -> 保留来源、原始分数和 lexical 支持
  -> 相关性硬门槛
  -> 加载权威正文
  -> 覆盖度选择
  -> 生成 canonical evidence manifest
  -> 工具执行前检查重复、缺口和 token 预算
  -> 只执行能增加有效证据的调用
  -> 交付后更新同一 run 证据账本
```

这不是为某个“整体架构”问题增加关键词规则。意图、权威度、覆盖目标、证据身份、
相关性分数和预算都是通用数据合同，任何领域都使用同一条链路。

## 3. 已确认问题

### 3.1 P0：Planner 重试缺少 attempt 级可观测性

证据：

- `internal/agent/qa/prepare.go` 使用 `helperTimeout` 包住整个 `planEvidence`。
- `internal/agent/qa/service.go` 当前 `helperTimeout` 为 12 秒。
- `internal/llm/call.go` 默认允许 3 次 attempt，并使用退避。
- `internal/llm/jsoncall.go` 将 transport retry 和 JSON repair 共用一份 attempt 预算。
- `internal/llm/errors.go` 已有 `CallError`、`ErrorKind` 和 `Retryable()`，但重试循环没有输出 attempt 级结构化事件。
- `internal/llm/usage.go` 能观测底层请求用量，不能关联一次逻辑调用中的 attempt 序号、重试原因和退避。

trace 示例：

```text
20:04:35  gpt-4.1 messages=2
20:04:41  gpt-4.1 messages=2
20:04:49  planner timeout
```

两次请求的消息数相同，更像 transport retry，而不是附加修复提示后的 JSON
repair；但当前日志无法证明第一次请求的错误类型，也无法确认是否进入了退避。

影响：

- Planner 慢时只能看到总超时，无法判断时间消耗在 Provider、退避还是 JSON 修复。
- 增加 `helperTimeout` 只能推迟失败，不能说明重试是否合理。
- Provider 稳定性、Planner 结构化输出质量和本地超时被混在同一个失败率里。
- 无法按错误类型评估重试是否真正提高成功率。

修改方案：

- 在 `internal/llm` 公共调用层记录重试，而不是在 Planner 调用点复制逻辑。
- 每次逻辑调用生成稳定的 `logical_call_id`，所有 attempt 和 repair 都关联到该 ID。
- 明确区分两种动作：

```text
transport_attempt:
  同一 messages 再次请求 Provider

json_repair:
  上一次响应可达但结构无效，追加修复指令后重新请求
```

- 每个 transport attempt 至少记录：

```text
logical_call_id
phase
provider
model
attempt
max_attempts
started_at
duration_ms
error_kind
status_code
retryable
retry_scheduled
backoff_ms
outcome
```

- 每次 JSON repair 额外记录：

```text
repair_round
validation_error_kind
remaining_attempts
```

- `error_kind` 复用已有稳定枚举，例如 `network`、`status`、`empty`、
  `envelope`；结构校验增加稳定分类，不直接把完整错误文本作为指标标签。
- trace 保存 attempt 事件；metrics 只使用低基数字段，避免
  `logical_call_id`、错误消息或 URL 进入 label。
- 不记录 prompt、response body、认证信息或完整请求参数。
- Planner 层只记录逻辑调用终态：

```text
attempts_total
transport_retries
repair_rounds
total_duration_ms
final_error_kind
fallback_used
```

- 12 秒超时保持不变，先用数据确认主要耗时，再单独评审是否调整。
- 上层取消或 deadline 已到时不再启动新 attempt，也不等待下一次 backoff。

预期效果：

- 一条 Planner 超时可以还原为明确的 attempt 时间线。
- 可以分别统计 Provider 重试成功率和 JSON 修复成功率。
- 超时调整基于可验证数据，而不是靠扩大时间窗口掩盖问题。
- 所有使用公共 LLM 调用层的能力自动获得一致观测。

### 3.2 P1：历史召回缺少相关性硬门槛

证据：

- `internal/sessionhistory/service.go` 调用 semantic search 后只保留 `ref`，丢弃命中的分数证据。
- `fuseRanks` 只根据 dense 和 lexical 排名计算 RRF。
- `internal/semantic/semantic.go` 的 `semantic.Hit` 已定义 `Score`、
  `ScoreKind`、`DenseScore` 和 `FusionScore`。
- 当前 Qdrant、Milvus 和内存 adapter 在 hybrid 查询中只填充 RRF
  `FusionScore`，不会同时返回 constituent `DenseScore`；因此不能直接在现有
  hybrid 结果上增加 cosine 门槛。
- `internal/memory/session_history.go` 的 `FindHistoryRefs` 只返回引用排序，
  没有返回 lexical 命中权重和覆盖度。
- `LoadHistorySummaries` 是所有权和存在性校验，不是相关性校验。
- `selectPayloadWithCount` 只限制条数和 token，不判断内容是否相关。

当前链路：

```text
query
  -> lexical refs
  -> dense/hybrid hits，hybrid 只返回 RRF，随后丢弃原始分数
  -> RRF
  -> LoadHistorySummaries
  -> neighbor expansion
  -> item/token selection
```

trace 示例：

```text
dense=64 lexical=0 fused=64 revalidated=64 selected_tokens=3429
```

最终历史上下文包含旅行、学校比较、美国政治和 Coding Agent 等与当前系统架构问题
无关的旧问答。问题不是“64 条都通过了 revalidate”，而是链路根本没有相关性门槛，
随后只按 top N 和 token 预算选择。

影响：

- dense top K 即使整体相似度很低，也会产生非空历史上下文。
- `lexical=0` 没有提高 semantic 接受标准。
- 无关历史会污染 Planner、预检索查询和最终回答。
- 邻居扩展会放大一个错误 primary candidate 的影响。
- `revalidated` 字段制造了已经做过语义复核的错误印象。

修改方案：

#### 3.2.1 保留候选证据

将 dense 和 lexical 结果统一为轻量候选，不先读取摘要正文：

```go
type HistoryCandidate struct {
    Ref             string
    DenseRank       int
    LexicalRank     int
    DenseScore      float32
    RankFusionScore float64
    ScoreKind       semantic.ScoreKind
    LexicalWeight   int
    LexicalCoverage float64
}
```

约束：

- `Ref` 是唯一身份，使用 `map[string]int` 在 O(n) 内合并候选。
- 会话历史改为分别执行 SQL lexical 查询和 semantic dense-only 查询；
  dense-only 查询不携带 `SparseVector`，必须返回 `ScoreKind=dense`。
- dense-only 路径保留 `semantic.Hit.DenseScore`，再与 lexical ranking 在本地做 RRF。
- 如果 dense-only 查询返回非 dense score kind，视为 Provider 合同错误并记录；
  当前请求只允许进入严格 lexical-only 降级，不把 RRF 当作 cosine。
- lexical 查询返回 `SUM(weight)`、命中 term 数和查询总权重，由 SessionStore
  在有界 SQL 查询内完成聚合。
- RRF 只用于候选排序，不作为跨 Provider 通用置信度。
- `ScoreKind=rrf` 时不能把 `Score` 当 cosine 阈值；历史相关性门槛只读取
  独立 dense-only 查询的 `DenseScore` 和 lexical 证据。

#### 3.2.2 在加载摘要前执行硬门槛

目标顺序：

```text
bounded candidate recall
  -> merge score evidence
  -> relevance gate
  -> LoadHistorySummaries(accepted refs)
  -> expand neighbors from accepted primary turns
  -> token selection
```

初始准入策略：

```text
dense-only:
  DenseScore >= 0.80

dense + lexical:
  DenseScore >= 0.70
  AND LexicalCoverage >= 0.35

lexical-only degradation:
  LexicalCoverage >= 0.70
  AND matched_terms >= 2
```

这些值是当前实现的首轮候选值，不声明为跨 embedding 模型的通用常数。正式启用前
必须在当前 embedding 模型和历史数据集上校准；配置按 score kind 和 embedding
模型版本持有，启动时校验，不能在调用链下游散落魔法数字。

统一接受规则可表达为：

```text
accepted =
  denseOnlyPass
  OR denseLexicalAgreementPass
  OR lexicalDegradationPass
```

其中：

- `lexical=0` 时只能走更严格的 `dense-only` 门槛。
- lexical 命中不能让低 semantic score 自动通过。
- semantic 后端不可用时允许 lexical-only 降级，但条件比普通候选更严格，并记录模式。
- 所有候选均未通过时返回 `no_relevant_history`，不为了保持非空上下文降低门槛。
- 邻居只围绕已通过门槛的 primary turn 扩展，不单独参与召回，也不能绕过 primary gate。
- 明确的 turn 引用或当前会话连续轮次可以使用独立的确定性路径，但必须基于
  turn/ref 身份，不允许把通用 dense 候选伪装成显式引用。

#### 3.2.3 Shadow 校准后再强制启用

上线分两步：

1. shadow 模式计算 `would_accept`，保持现有交付结果，用固定评估集检查阈值。
2. 指标达到门禁后启用 enforce 模式，未通过候选不再加载正文。

阈值校准数据至少覆盖：

- 用户明确引用较早轮次
- 同主题连续追问
- 相似术语但不同实体
- 完全换题
- 中英文混合问题
- lexical 为零但语义确实相关
- semantic 后端不可用时的 lexical-only 降级

观测字段调整为：

```text
dense_candidates
lexical_candidates
fused_candidates
score_filtered
accepted_candidates
loaded_records
stale_candidates
dense_only_accepted
neighbor_items
selected_items
selected_tokens
gate_mode
dense_threshold
dense_only_threshold
```

删除或重命名当前 `revalidated`，避免继续表达错误语义。

预期效果：

- 无关 dense top K 不再自动进入历史上下文。
- lexical 完全无命中时，只有高置信 semantic 候选能够通过。
- 空结果成为合法结果，不再用低相关历史填满 token 预算。
- 阈值可通过评估数据校准，并且不会混用 cosine 和 RRF 分数。

### 3.3 P1：概览问题按来源扩散，而不是按权威性和覆盖度收敛

证据：

- `internal/retrieval/pipeline.go` 的 `discoverSources` 固定并行执行代码、
  Runbook 和服务召回。
- 当前默认上限为 code 20、runbook 20、service 8，代码命中的服务还会继续扩大 anchor。
- `expand` 对 services、code、runbooks 和 deps 统一展开，没有概览问题的覆盖合同。
- `domain.ClassifyResponseMode` 已能识别 `ArchitectureReview`，但当前主要影响回答结构，
  没有形成检索策略。
- `RunbookSearchHit` 已有 `DocID`、`DocKind`、`EvidenceClass` 和 `TrustTier`，
  但权威度主要用于排序 tie-break，未驱动停止条件。
- `RetrievedContext` 只保留文本、引用和选择统计，组装后丢失文档章节与覆盖信息。

trace 示例：

- 已命中高权威系统总览文档，trust 为 85，semantic score 为 0.645。
- 仍继续展开 20 篇 Runbook、20 个代码命中和 16 个服务。
- 依赖上下文只覆盖 `services=1/16`，同时出现大量省略边。

影响：

- 概览回答得到很多候选，却不一定覆盖用户需要的主要维度。
- 服务数量和文档数量替代了证据覆盖度，导致上下文大但信息不完整。
- 高权威总览与低增量局部材料竞争相同预算。
- Agent 无法知道预检索已经覆盖哪些业务域或架构层次。

修改方案：

#### 3.3.1 将回答模式提升为检索意图

Planner 输出通用 `RetrievalIntent`，而不是依赖具体问题关键词：

```go
type RetrievalIntent struct {
    Kind          RetrievalIntentKind
    RequiredFacets []EvidenceFacet
    TargetEntities []string
}
```

首批意图：

```text
focused_fact
overview
flow
inventory
runtime_diagnosis
```

意图表示检索目标，不改变 Provider 或工具权限。`ArchitectureReview` 可以映射为
`overview`，调用链问题可以映射为 `flow`，映射规则集中在 Planner/domain 边界。

#### 3.3.2 概览检索使用“权威主干 + 覆盖补齐”

`overview` 的选择顺序：

1. 从候选中选择一个或少量高权威 overview spine。
2. 从 spine 的标题、章节和结构化元数据计算已覆盖 facet。
3. 对未覆盖 facet 各补一份最具代表性的证据。
4. 新候选不增加 facet、实体或证据等级时停止扩展。
5. 达到文档、服务、依赖边和 token 总预算时停止。

通用 facet 示例：

```text
system_boundary
business_domain
entrypoint
core_flow
data_and_state
external_dependency
runtime_and_operations
```

facet 是通用分类，不写入 trace 中出现的具体业务名。具体业务域作为
`TargetEntities` 或候选 metadata，由检索结果动态产生。

候选排序使用稳定字典序：

```text
intent match
authority / TrustTier
new facet coverage
new entity coverage
semantic relevance
stable source identity
```

权威度不是唯一分数：

- 高权威但不相关的文档不能进入主干。
- 高相关但低权威的局部材料用于补缺，不覆盖更高权威来源的系统级结论。
- 同一 facet 已有完整高权威证据时，不再无差别追加同类文档。

#### 3.3.3 让组装结果保留覆盖元数据

`RetrievedContext` 除文本和引用外，增加结构化证据单元：

```go
type EvidenceUnit struct {
    SourceKind    string
    Target        string
    Sections      []string
    ContentHash   string
    Coverage      EvidenceCoverage
    Facets        []EvidenceFacet
    TrustTier     int
    EvidenceClass string
    TokenCost     int
}
```

要求：

- `Target` 使用工具拥有的 canonical ID，例如 doc ID、service ID、代码路径和行区间。
- `Sections` 使用稳定章节或 chunk ID，不使用正文模糊匹配。
- `Coverage` 明确为 `full` 或 `partial`，并携带省略数量。
- 组装只选择证据，不重新定义权威身份；身份由各检索工具或 store 边界产生。
- 使用 map 按 `(source_kind, target, section)` 做 O(1) 去重。

预期效果：

- 概览问题先形成可信主干，再用有限材料补齐真正缺失的维度。
- 检索停止条件从“来源上限用完”变成“覆盖目标已满足或没有增量”。
- 服务、文档和依赖数量不再无界互相放大。
- 后续 Agent 能准确知道 seed evidence 的身份和覆盖范围。

### 3.4 P1：预检索与 Agent 工具没有共享证据账本

证据：

- `internal/agent/qa/submission.go` 的 `qaContextBlocks` 将全部预检索结果放入一个
  `Source=qa.evidence`、`Complete=true` 的扁平 `ContextBlock`。
- `agent.ContextBlock` 只有文本、引用、完整标记和内容哈希，没有文档章节级覆盖。
- `internal/memory/session_context.go` 的 `EvidenceManifest` 是历史 Session 路由信息，
  由既往 tool messages 构造，不代表当前 run 的 seed evidence。
- 当前 Agent 主要按 tool-call fingerprint 和已调用工具去重，不能判断某个文档正文
  是否已经由预检索完整提供。

trace 示例：

- 预检索已经包含系统总览文档。
- Agent 后续三次调用 `search_runbooks`，其中一次再次按同一 doc ID 获取该文档。
- 可见上下文从约 39.6k 字符增长到约 68.5k 字符。

影响：

- 同一文档在 seed context 和 tool result 中重复占用 token。
- Agent 根据自然语言摘要猜测覆盖范围，无法稳定判断是否需要再次取数。
- 仅按工具名或参数字符串去重会误伤合法分页，也无法识别不同参数返回的相同证据。
- persisted Session manifest 和当前 run 证据生命周期不同，强行复用会混淆职责。

修改方案：

#### 3.4.1 新增同一 run 的 Evidence Ledger

不要扩展 `memory.EvidenceManifest` 承担运行时职责。Agent execution 在 run 开始时根据
`RetrievedContext.EvidenceUnits` 建立 `RunEvidenceLedger`：

```go
type EvidenceKey struct {
    SourceKind string
    Target     string
    Section    string
}

type EvidenceLedgerItem struct {
    Key           EvidenceKey
    Coverage      EvidenceCoverage
    ContentHash   string
    TrustTier     int
    EvidenceClass string
    TokenCost     int
    Origin        string
}
```

`Origin` 至少区分：

```text
seed
tool
```

账本只存在于当前 run 的执行状态和 trace 中，不作为新的业务状态机，也不修改历史
Session 记忆语义。

#### 3.4.2 工具声明可比较的请求范围和结果范围

检索工具需要提供两个确定性合同：

```text
ResolveScope(arguments) -> requested evidence scope
DescribeResult(result)  -> delivered evidence units
```

例如：

- doc-scoped Runbook 请求解析为 `(runbook, doc_id, requested_sections)`。
- 分页代码搜索解析为 `(code_search, query_hash, page/cursor)`，结果项再落到具体
  `(path, line_range)`。
- runtime 查询包含时间窗口和数据版本；相同资源但不同时间窗口不视为重复。

不能使用正文相似度、标题模糊匹配或 LLM 判断重复。只有 canonical identity 和明确
coverage 才能驱动短路。

#### 3.4.3 执行前按覆盖关系决策

```text
fully covered:
  不执行工具，返回 already_available 和 ledger reference

partially covered:
  确定性缩小为缺失 section/page/scope 后执行

not covered:
  进入预算准入

freshness-sensitive:
  即使静态身份相同，只要时间窗口或版本不同，仍允许执行
```

短路结果必须是结构化 observation，使模型知道证据已经存在，而不是模拟一次新的工具
成功响应。短路不增加正文 token，只提供现有 evidence key 和 coverage。

#### 3.4.4 执行后更新账本

- 工具 owner 根据权威结果生成 `EvidenceUnit`。
- 使用 `map[EvidenceKey]LedgerItem` O(1) 合并。
- `full` 覆盖可以替换同 scope 的 `partial`；`partial` 不能覆盖已有 `full`。
- 相同 key 和 content hash 记为 exact duplicate。
- 相同 key 但 hash 不同必须保留版本或 freshness 信息，不能静默覆盖。
- Prompt 投影是否截断不改变 authoritative result 的 evidence identity。

预期效果：

- 已完整存在的文档不会被同一 run 再次拉取。
- 部分覆盖时只请求缺失范围，不阻断正常分页。
- 去重基于工具拥有的稳定身份，不依赖具体问题词和模糊文本规则。
- seed evidence、工具权威结果和模型投影共享一致的覆盖语义。

### 3.5 P2：工具执行前没有预算和增量证据准入

证据：

- `internal/agent/execution/loop_turn.go` 的 `executeToolTurn` 先执行工具。
- `internal/agent/execution/tool_delivery.go` 的 `prepareToolDelivery` 在工具返回后才判断
  内容是否能进入剩余窗口。
- `ensureTurnBudget` 在模型轮次前做上下文检查，不评估下一次工具调用的最大成本。
- 当前检查回答的是“结果已经生成后能否交付”，不是“执行前是否值得消耗预算”。

trace 示例：

预检索上下文约 39.6k 字符，已经足以形成架构回答；Agent 仍继续调用 Runbook 工具，
追加约 29k 字符，其中包含已存在文档。

影响：

- 大结果即使最终无法交付，也已经消耗后端查询、序列化和时间。
- 剩余上下文不足时，Agent 仍可能发起高上限请求。
- 重复调用和新增调用使用相同准入条件。
- 当前 run 的未满足目标、证据增量和 token 预算没有在一个决策点汇合。

修改方案：

本问题复用 `qa-agent-context-budget-and-cancellation.zh-CN.md` 定义的 run token ledger。
该提案继续负责：

- authoritative result 与 PromptContent 分离
- 单工具、单轮和单 run token 限制
- 活动调用取消
- 执行后的最终 delivery guard

本文只增加工具执行前的 admission：

```go
type ToolAdmissionInput struct {
    RequestedScope       EvidenceScope
    UnresolvedGoals      []EvidenceGoal
    RemainingToolTokens  int
    DeclaredMaxTokens    int
}

type ToolAdmissionDecision struct {
    Action ToolAdmissionAction
    Reason string
    Scope  EvidenceScope
}
```

首批动作：

```text
allow
narrow
already_available
deny_budget
deny_no_increment
```

准入顺序：

1. 解析并 canonicalize 请求 scope。
2. 查询 `RunEvidenceLedger`，计算 full、partial 或 no coverage。
3. 对 partial coverage 确定性缩小请求。
4. 检查请求能否覆盖至少一个未满足 goal。
5. 读取工具声明的最大交付 token 或按 page size 计算稳定上界。
6. 与 run token ledger 的剩余预算比较。
7. 决定执行、缩小、短路或拒绝。

决策只依赖显式事实：

```text
canonical overlap
coverage state
unresolved goal count
declared result upper bound
remaining tool tokens
```

不引入由 LLM 生成的自由形式“价值分”，也不根据 trace 中的文档名、服务名或字符数
写特殊规则。

工具合同要求：

- 可分页工具必须根据 `limit` 或 page size 声明结果上界。
- 不能给出稳定上界的工具使用保守的 per-tool maximum。
- `narrow` 只能通过工具 schema 已支持的 limit、cursor、section 或 target 参数实现。
- 无法确定性缩小时返回 `deny_budget`，要求 Agent 基于已有证据收敛。
- 工具执行后仍运行现有 delivery guard，处理估算误差和异常大结果。
- exact identifiers 属于 answer contract 时，准入必须为其保留完整交付预算。

预期效果：

- 明知重复或超预算的工具不再实际执行。
- 可分页调用会在执行前缩小到当前 run 可承受范围。
- 工具循环根据未满足证据目标收敛，而不是用完 MaxSteps 才停止。
- 预算估算不需要完全预测结果大小，工具声明上界加执行后 guard 即可形成双层保护。

## 4. 目标链路

完整目标链路如下：

```text
prepareQA
  |
  +-- planEvidence
  |     +-- logical LLM call
  |     +-- transport attempt events
  |     +-- JSON repair events
  |     `-- RetrievalIntent + EvidenceGoals
  |
  +-- session history recall
  |     +-- bounded lexical candidates with support
  |     +-- bounded semantic candidates with raw scores
  |     +-- O(n) candidate merge
  |     +-- score-kind-aware hard gate
  |     +-- load accepted summaries
  |     +-- accepted-primary neighbor expansion
  |     `-- token selection
  |
  +-- pre-retrieval
  |     +-- intent-aware discovery
  |     +-- authoritative overview spine
  |     +-- uncovered-facet expansion
  |     +-- marginal-coverage stop
  |     `-- RetrievedContext + EvidenceUnits
  |
  `-- Agent run
        +-- initialize RunEvidenceLedger from seed units
        +-- initialize/reuse run token ledger
        +-- model proposes tool call
        +-- resolve canonical request scope
        +-- evidence overlap check
        +-- unresolved-goal check
        +-- token preflight
        +-- allow / narrow / short-circuit / deny
        +-- execute allowed tool
        +-- persist authoritative result
        +-- update evidence ledger
        +-- bounded PromptContent delivery
        `-- answer when goals are covered or no admissible call remains
```

核心顺序约束：

- 相关性 gate 必须早于历史摘要加载、邻居扩展和 token selection。
- coverage selection 必须早于 seed context 文本组装。
- evidence overlap 和预算 admission 必须早于工具执行。
- authoritative result 持久化必须早于有损 PromptContent 投影。
- 执行后 guard 保留，不能因为有 preflight 就删除。

## 5. 核心数据合同

### 5.1 Evidence Goal

```go
type EvidenceGoal struct {
    ID       string
    Facet    EvidenceFacet
    Target   string
    Required bool
}
```

- Goal 由 Planner 在一次请求内生成。
- ID 在当前 run 内稳定，用于 trace 和覆盖统计。
- `Target` 为空时表示通用 facet，不要求提前知道具体服务或文档。
- Goal 不是持久化状态机；是否满足由当前 evidence ledger 直接推导。

### 5.2 Evidence Scope

```go
type EvidenceScope struct {
    SourceKind string
    Target     string
    Sections   []string
    Cursor     string
    Version    string
    TimeRange  string
}
```

- 工具参数在工具边界 canonicalize 一次。
- 运行时代码信任 scope 已规范化，不重复 trim、lowercase 或兼容旧别名。
- `Version` 和 `TimeRange` 用于区分需要重新验证的新鲜证据。

### 5.3 Evidence Coverage

```go
type EvidenceCoverage struct {
    Complete     bool
    Included     int
    Omitted      int
    NextCursor   string
}
```

- `Complete=true` 只表示声明 scope 已完整交付，不表示整个数据源完整。
- 未提供明确 scope 的扁平文本不能标记为完整文档覆盖。
- `NextCursor` 存在时必须视为 partial。

### 5.4 Run Evidence Ledger

ledger 的职责只有三个：

1. 判断当前 run 是否已经拥有某个 canonical evidence。
2. 计算 evidence goals 的覆盖状态。
3. 为工具准入和 Prompt 投影提供去重依据。

ledger 不负责：

- 存储跨 Session 长期记忆
- 替代 Artifact 或权威工具结果
- 推断正文语义相似度
- 决定 Provider 或权限

### 5.5 Token Ledger

本文不新建第二套 token 预算。工具预检必须读取关联提案定义的同一份 run ledger：

```text
remaining_run_tool_tokens
per_tool_limit
per_turn_limit
final_answer_reserve
safety_reserve
```

preflight 消费声明上界进行准入；实际交付按新增 token 更新 ledger。重复 evidence
不重复扣正文预算，但短路 observation 自身的少量 token 仍正常计入。

### 5.6 与行业通用实践的对应

本方案采用以下通用机制，而不是项目特例：

- LLM 调用使用 logical call 加 attempt 事件，错误类型、耗时和 retry decision 分开记录。
- 检索采用 candidate generation、score gate、rerank/coverage selection、context assembly 分层。
- 不比较不同 score kind 的裸分数，阈值按模型和评分语义校准。
- Agent 以 canonical evidence identity 判断重复，不对正文做模糊去重。
- 权威工具结果和模型上下文投影分离，完整数据可审计，输入保持有界。
- 工具执行使用 admission control 和执行后 guard 两层预算保护。
- 检索质量通过带标签数据集和线上 shadow 指标校准，不根据单条 trace 调参。

## 6. 开发切片

### Slice 1：纠正术语并补齐 Planner attempt 观测

范围：

- `internal/llm/call.go`
- `internal/llm/jsoncall.go`
- `internal/llm/textcall.go`
- `internal/llm/usage.go`
- `internal/agent/qa/prepare.go`
- `internal/sessionhistory/service.go`

变更：

- 增加 logical call ID 和 attempt/repair 事件。
- 复用 `CallError` 分类和 `Retryable()`。
- 记录 retry decision 和 backoff。
- 将历史 trace 的 `revalidated` 改为 `loaded_records`。
- 不改变重试次数、12 秒 timeout 和历史选择结果。

测试：

- 首次成功
- retryable transport error 后成功
- non-retryable error
- JSON repair 后成功
- deadline 在 backoff 前到期
- 日志和 trace 不包含 prompt/body
- `loaded_records` 与实际加载数量一致

### Slice 2：历史候选分数保留和 shadow gate

范围：

- `internal/sessionhistory/service.go`
- `internal/memory/session_history.go`
- history trace 和评估 fixture

变更：

- lexical store 返回有界候选证据。
- semantic 历史召回改为 dense-only 查询并保留 cosine score。
- 使用 map 合并候选，RRF 只排序。
- 实现 score-kind-aware gate，但先以 shadow 模式运行。
- 增加阈值配置校验和 embedding 模型版本关联。

测试：

- dense-only 高分/低分
- dense 与 lexical 一致/冲突
- dense-only 查询意外返回 RRF 时进入可观测的 lexical-only 降级
- lexical-only 降级
- stale ref
- 所有候选被过滤
- shadow 模式不改变当前输出

### Slice 3：启用历史硬门槛

前置条件：

- 固定数据集完成标注。
- dense-only、dense+lexical 和 lexical-only 三种模式分别达到验收门禁。
- shadow 指标至少覆盖一个完整评估周期。

变更：

- gate 切换为 enforce。
- 只加载 accepted refs。
- 只围绕 accepted primary turn 扩展 neighbor。
- 空候选返回 `no_relevant_history`。

测试：

- 明确历史引用保持可召回
- 换题不注入旧历史
- 错误 primary 不触发邻居扩散
- 查询和正文读取数量受 accepted refs 限制

### Slice 4：检索意图和覆盖度选择

范围：

- `internal/domain`
- `internal/agent/qa`
- `internal/retrieval`
- retrieval trace

变更：

- Planner/domain 增加 `RetrievalIntent` 和 `EvidenceGoal`。
- overview 使用权威主干和 facet 补齐。
- `RetrievedContext` 保留 `EvidenceUnits`。
- 增加 coverage stop 和稳定排序。

测试：

- 高权威总览优先
- 高权威但不相关候选被拒绝
- 缺失 facet 由代表证据补齐
- 已覆盖 facet 不重复扩展
- 没有增量时提前停止
- 固定输入产生稳定顺序

### Slice 5：同一 run Evidence Ledger

范围：

- `agent.ContextBlock` 或等价 seed contract
- `internal/agent/qa/submission.go`
- `internal/agent/execution`
- 具备 canonical identity 的检索工具

变更：

- seed context 携带 evidence units。
- run 初始化时建立 evidence ledger。
- 工具实现 scope resolver 和 result descriptor。
- 增加 full/partial/freshness coverage 判定。
- 工具执行后更新 ledger。

测试：

- seed 已完整包含 doc 时短路
- seed 只包含部分 section 时只请求缺口
- 分页结果可以继续下一页
- 同 key 同 hash 去重
- 同 key 不同版本不静默覆盖
- runtime 时间窗口变化允许重新查询

### Slice 6：工具执行前 Admission

前置条件：

- 复用或先实现关联提案的 run token ledger。

范围：

- `internal/agent/execution/loop_turn.go`
- `internal/agent/execution/tool_delivery.go`
- tool contracts

变更：

- 在实际执行前调用 admission。
- 支持 allow、narrow、already_available、deny_budget 和 deny_no_increment。
- 将决策写入 trace。
- 保留执行后的 `prepareToolDelivery` guard。

测试：

- 重复调用不会触发实际 tool invocation
- 超预算调用不会触发实际 tool invocation
- limit 可确定性缩小
- 无法缩小时要求 Agent 收敛
- 估算偏小时由执行后 guard 接住
- required literal 预算不被普通结果占用

### Slice 7：固定数据集复评和正式文档合并

变更：

- 对 trace 暴露的同类问题进行复评，但不把原问题词写入生产规则。
- 记录阈值版本、embedding 模型版本和评估集版本。
- 将稳定合同合并到正式设计文档。
- 标记本文和关联提案的实现关系。

## 7. 验收门禁

### 7.1 Planner 可观测性

- 100% 的 Planner 逻辑调用可关联到 attempt 时间线。
- 每个失败 attempt 都有稳定 `error_kind`、duration 和 retry decision。
- transport retry 与 JSON repair 可以独立统计。
- deadline 后不再启动新 attempt。
- trace 和日志不包含 prompt、response body 或认证信息。

### 7.2 历史相关性

- 固定标注集 `precision@selected >= 0.90`。
- 完全换题样本的无关历史注入率 `<= 0.02`。
- 明确引用既往轮次样本的 recall `>= 0.95`。
- `lexical=0` 时没有低于 dense-only threshold 的候选进入摘要加载。
- 所有候选被过滤时稳定返回空历史，不发生阈值自动放宽。
- dense-only、dense+lexical 和 lexical-only 模式分别报告指标；RRF 只报告排序统计。

### 7.3 概览覆盖

- overview 回答至少有一份满足意图的高权威主干证据，存在时必须优先选择。
- 每个 required facet 要么被覆盖，要么在 trace 中记录未覆盖原因。
- 已完整覆盖的 facet 不再追加同类低增量候选。
- 达到 coverage stop 后不继续按固定来源上限扩展。
- 文档、服务、依赖边和 token 数均保持显式上限。

### 7.4 重复检索

- seed 已完整覆盖的 doc-scoped 请求不会执行后端查询。
- partial coverage 只请求缺失 section/page。
- exact duplicate 的正文重复交付 token 降低至少 80%。
- freshness-sensitive 查询不被静态 evidence 错误短路。
- 所有短路均能从 trace 还原命中的 evidence key。

### 7.5 工具预算

- admission 拒绝的调用不会进入工具实现。
- 每个允许调用都有 declared upper bound 和剩余预算记录。
- `narrow` 后参数仍通过原工具 schema 校验。
- run tool-result token 不超过关联提案定义的硬预算。
- 执行后 delivery guard 仍能处理估算误差。
- 没有可准入调用时，Agent 基于已有证据结束，而不是空转到 MaxSteps。

## 8. 可观测性与评估

### 8.1 Planner 指标

```text
llm_logical_calls_total{phase,outcome}
llm_attempts_total{phase,provider,model,error_kind,outcome}
llm_retries_total{phase,reason}
llm_json_repairs_total{phase,outcome}
llm_attempt_duration_ms{phase,provider,model,attempt}
```

高基数字段只进入 trace，不进入 metrics labels。

### 8.2 历史召回指标

```text
history_candidates_total{mode,source}
history_candidates_filtered_total{mode,reason}
history_candidates_accepted_total{mode,support}
history_loaded_records_total{mode}
history_no_relevant_hit_total{mode}
history_selected_tokens{mode}
```

trace 额外记录阈值版本、score kind、候选分数分布和 selected refs；不得记录完整历史正文。

### 8.3 检索覆盖指标

```text
retrieval_goals_total{intent,required}
retrieval_goals_covered_total{intent,facet}
retrieval_expansion_stopped_total{intent,reason}
retrieval_evidence_units_total{intent,source_kind,coverage}
```

停止原因至少包括：

```text
coverage_complete
no_increment
source_limit
token_budget
backend_exhausted
```

### 8.4 工具准入指标

```text
tool_admission_total{tool,action,reason}
tool_admission_narrowed_total{tool}
tool_execution_avoided_total{tool,reason}
evidence_duplicate_items_total{source_kind,origin}
evidence_duplicate_tokens_total{source_kind,origin}
```

### 8.5 评估方法

离线评估集按意图和失败类型分层，不能只保留本次 trace：

- history relevance
- overview coverage
- focused fact
- multi-hop flow
- inventory
- runtime diagnosis
- explicit historical reference
- topic switch
- duplicate seed/tool retrieval
- budget exhaustion

每次阈值或 embedding 模型变化都生成新评估版本。线上先 shadow，再 enforce；出现
precision 回退时回滚阈值配置，不回滚观测字段和 evidence contract。

## 9. 非目标

以下内容不属于本提案：

- 不为“整体架构”、具体学校、旅行、政治或 Coding Agent 等词增加黑名单。
- 不硬编码某个 doc ID 为系统总览。
- 不把所有 dense 结果一律丢弃。
- 不把 12 秒 Planner timeout 直接增加到更大值作为首要修复。
- 不用另一次 LLM 调用判断两个证据是否重复。
- 不用字符数替代运行时 token 预算。
- 不把 persisted Session `EvidenceManifest` 改造成运行时共享容器。
- 不移除工具执行后的 delivery guard。
- 不在 semantic 后端失败时静默切换 Provider。

## 10. 风险与迁移

### 10.1 阈值过高导致历史 recall 下降

缓解：

- shadow 校准后再 enforce。
- dense-only、dense+lexical 和 lexical-only 使用独立门槛。
- 显式 turn/ref 引用走确定性路径。
- 以 precision 和 explicit-reference recall 共同验收，不能只优化一个指标。

### 10.2 Provider 或 embedding 模型更换导致分数漂移

缓解：

- 阈值绑定 score kind 和 embedding 模型版本。
- 启动时发现未知组合直接禁用 enforce 并告警，不套用旧阈值。
- 新模型先重新运行 shadow 评估。

### 10.3 Evidence identity 不完整

缓解：

- 先覆盖有稳定 doc ID、service ID、path/line 和 cursor 的工具。
- 无稳定 identity 的工具只使用预算 admission，不进行模糊去重。
- identity 由工具 owner 提供，不在 Agent 层猜测。

### 10.4 coverage 分类过细导致 Planner 不稳定

缓解：

- 首批 facet 保持少量、通用和封闭枚举。
- required goal 数量设置硬上限。
- 未识别意图回落到 `focused_fact`，不动态发明新 facet。
- coverage 是否满足由 evidence metadata 推导，不由后续 LLM 自报。

### 10.5 与已有 token ledger 提案重复实现

缓解：

- Slice 6 明确依赖 `qa-agent-context-budget-and-cancellation.zh-CN.md`。
- 运行态只保留一份 token ledger。
- 本文的 admission 读取该 ledger，不持有另一组剩余预算字段。
- 两份提案合并正式文档时统一术语和数据结构。

## 11. 完成定义

只有同时满足以下条件，本文才能标记为 `Implemented`：

1. Planner attempt 级 trace 和 metrics 已上线。
2. `revalidated` 错误术语已删除或更名。
3. 历史 score evidence 被保留，硬门槛完成 shadow 校准并 enforce。
4. overview 检索按权威主干和 coverage goal 收敛。
5. seed evidence 可以初始化同一 run ledger。
6. 重复或超预算工具在执行前被短路或缩小。
7. 工具执行后的权威结果、Prompt 投影和 budget guard 保持有效。
8. 固定评估集和回归测试通过。
9. 正式上下文、检索、工具执行和可观测性文档已更新。
10. 实际阈值、embedding 模型版本、指标结果和遗留项已记录。
