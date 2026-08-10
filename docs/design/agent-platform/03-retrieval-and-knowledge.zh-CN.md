# 检索、知识文档与调用链

[English](03-retrieval-and-knowledge.md) | [中文](03-retrieval-and-knowledge.zh-CN.md)

> 状态：混合检索主体已实现；预算、分数语义和多意图覆盖仍在收敛
> 来源：Retrieval Design、Retrieval Simplification、Context Evidence Quality、Runbook Boundaries、Call-chain Closure

## 1. 目标

检索模块把当前问题转换为有界、可追踪、按来源分组的证据。它不直接生成答案，也不把排名分数伪装成事实置信度。

```text
canonical query
  -> bounded source retrieval
  -> source-local normalization
  -> deduplication
  -> rerank/fusion
  -> diversity and facet coverage
  -> token-budgeted evidence assembly
```

## 2. 规范查询

一次请求只有一个 canonical query。完整问题不拼接上一轮全文；只有明显指代、省略或短追问才允许进行受校验的查询改写。

查询派生信息可以包括：

- terms 和稀有 token；
- service、symbol、API、config key 等实体；
- 有限 facet，例如 code、runbook、runtime、dependency；
- 时间或环境约束。

派生信息服务于召回和覆盖，不得形成第二套互相漂移的查询事实。

## 3. 混合检索

- Dense 检索负责语义相似；
- BM25/Sparse 检索负责精确 token、标识符和错误文本；
- 结果在来源内归一化，再通过 RRF 或明确融合策略合并；
- 重排器是可选能力，未配置时禁用；已配置但失败时错误必须可见；
- 稳定词表和索引快照通过原子替换发布，在线读不持有可变指针。

分数字段必须区分：

- 原始 dense/sparse score；
- fusion score；
- rerank score；
- trust/provenance adjustment；
- final selection score。

不同含义的分数不能复用同一字段。

## 4. 去重和多样性

去重按稳定身份执行，不按渲染文本猜测：

- 代码：repository + path + symbol/chunk identity；
- Runbook：document + section/chunk；
- 服务/API：ontology identity；
- 调用边：caller + callee + location + provenance。

先用 map 进行 O(n) 去重，再做必要排序。多样性选择优先覆盖不同来源、文档和 facet，随后补充同一来源的高分项。

## 5. Runbook 检索

`search_runbooks` 返回 chunk/section 级证据，而不是只返回整篇文档的最高分命中。

合同应包括：

```json
{
  "items": [],
  "total": 0,
  "truncated": false,
  "next_cursor": "",
  "coverage": {
    "documents": "complete|partial",
    "sections": "complete|partial"
  }
}
```

关键规则：

- 同一文档可以保留多个相关 section；
- 先保证文档覆盖，再补充同文档的额外 section；
- 每个 chunk 独立进入证据池并保留来源定位；
- keyword fallback 与 semantic score 分开记录；
- `truncated=true` 表示还有匹配项未返回，不表示文档中不存在相关事实；
- 存储查询使用 `limit+1` 或游标，不加载全量正文后裁剪。

## 6. 多意图与 Facet 覆盖

复合问题不能只用一个总体高分结果代表全部意图。Assembler 应维护有限 facet 覆盖状态，例如：

```text
requested: [code, runbook, dependency]
covered:   [code, runbook]
missing:   [dependency]
```

允许在预算内进行一次定向补查。不得无限循环补查，也不得针对某个问题关键词增加特例。

## 7. 调用链

方法级调用链由三类事实组成：

1. 静态调用点：caller、callee、文件、行列和语言；
2. 已验证的 service route：由入口、客户端和配置关系闭环；
3. 服务级 `depends_on`：只表示依赖，不能冒充方法调用。

`trace_calls`/调用链结果保留：

- 重复调用点及位置；
- confidence 和 provenance；
- unresolved 节点；
- `truncated` 和 `next_frontier`；
- `max_depth`、`max_nodes`、`max_fanout` 等边界。

禁止把服务级依赖自动提升为方法级链路，也禁止把 truncated 结果描述为完整路径。

## 8. 预算装配

检索阶段的字符/rune 上限与模型 token 窗口不是同一概念。统一目标是：

- Retriever 在数据源边界限制候选数和单项大小；
- Assembler 使用 Provider 感知 token 预算；
- 当前问题和新鲜工具证据优先；
- 每个证据项原子加入，预算不足时不从中间破坏 JSON/代码段；
- coverage 和 omitted count 随证据进入 Agent。

## 9. 失败和降级

| 情况 | 行为 |
|---|---|
| Dense 未配置 | 使用明确启用的 Sparse 能力并记录状态 |
| 已配置 Dense 后端失败 | 返回可见错误，不静默替换 |
| Rerank 未配置 | 保留融合排序 |
| Rerank 已配置但失败 | 记录错误并按已文档化策略结束或降级 |
| Runbook 结果截断 | 返回 cursor 和 partial coverage |
| 调用链预算耗尽 | 返回 frontier，不声称闭环 |
| 无命中 | 表述为当前查询未命中，不证明事实不存在 |

## 10. 验收标准

1. canonical query 在流水线中保持唯一；
2. 去重使用稳定 identity，复杂度保持 O(n)；
3. Runbook 可以返回同文档多个 section；
4. 所有分数的语义可追踪；
5. 多意图问题能够报告 facet 缺口；
6. 调用链区分 method call、service route 和 depends_on；
7. 所有读取和遍历都有明确边界与 continuation。

## 详细归并材料

### Agent 内部检索设计

> Migrated from CodeLoom `docs/design/agent/agent-retrieval-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前基线，包含已评估待办

Agent 检索组合结构化记录、稠密向量、稀疏词法匹配、依赖图和预算化证据组装。`EvidencePlan` 决定是否允许执行这条链路；本文定义 Internal 证据如何建立索引和被检索。

#### 检索流水线

```text
唯一规范查询 + 查询术语
  -> 检索分发
  -> 稠密/稀疏候选发现
  -> 命中驱动的结构化数据和 CodeGraph 扩展
  -> 去重与重排
  -> 排名保持型层级/服务多样性
  -> 按证据项执行上下文预算组装
```

Trace 将 `retrieval_dispatch`、`retrieval_discover`、`retrieval_expand` 和 `retrieval_assemble` 分开，使召回、补充、排序和上下文构造可以独立评估。

#### 稠密与稀疏检索

代码检索将 Qdrant 稠密向量与名为 `bm25` 的稀疏向量融合。稠密检索覆盖语义相似性；稀疏检索保留精确标识符、服务名、路径、错误文本和术语。

稀疏分词：

1. 分离 CJK 和非 CJK 片段；
2. 对中文分词，对非 CJK 按单词边界切分；
3. 拆分 camelCase 并统一大小写；
4. 过滤停用词、纯数字或噪声数字、UUID、长十六进制值、数字占比过高内容和过长术语；
5. 文档 token 保留重复次数用于 TF，查询 token 去重。

只有源码和接口类文件生成稀疏向量。配置和文档仍生成稠密向量，但不会污染代码词表。

#### 稳定词表与增量索引

稀疏 token ID 是 Qdrant 坐标，只允许追加。已发布 ID 永远不能重新分配或复用。

```json
{
  "version": 2,
  "tokenizer_version": 1,
  "next_id": 52936,
  "tokens": {"order":10,"payment":25}
}
```

词表通过临时文件替换原子保存。仓库索引会克隆当前词表、分配新 ID、持久化扩展后的词表、写入新仓库 generation，成功 upsert 后才删除旧 generation。失败可能留下未使用 ID，但不能在重启后留下 Qdrant 已使用而本地未知的坐标。

Qdrant 在 Collection 级应用 IDF。文档权重使用 `b=0` 的饱和 TF，避免每次更新仓库后重写依赖全局平均文档长度的权重。查询对已知 token 发送值 `1`。

索引锁串行化词表分配和仓库替换。完整 Builder 通过原子指针发布给在线 Agent 工具，读请求不会观察到构建一半的词表。检测到旧版词表时会阻止增量更新，直到完成全量迁移。

#### 证据归属与多样性

代码和服务记录携带 `app`、`server`、`front` 等运行形态 Layer；Runbook 使用 `docs`。向量载荷不可用时，CodeGraph 结果从仓库/服务路径推导 Layer。

重排多样性按 `layer + service` 计数，但始终按全局排名单遍选择，不通过分组轮询提升低排名证据。Layer 只作为展示标签；格式化不得重新排列证据。未知 Layer 保持可见，不强行归入已知分类。

Runbook 在线证据优先使用命中的 Chunk，同一文档只合并有限的去重 Chunk，并按 rune 限制单文档大小。完整文档只属于显式全文读取流程。

每条重排证据独立进入预算组装。只有实际写入上下文的证据才能贡献引用，`HitCount` 表示唯一可见引用数。截断按证据项执行，不能先把多条来源合并成大字符串。

依赖关系持久化到 `dependency_edges`，包含扫描器产出的 HTTP/gRPC/RPC 关系和 Runbook 显式声明的边。仓库替换按仓库归属删除旧边。

#### 失败规则

- Qdrant upsert 失败时保留上一 generation。
- 仓库扫描为空时，只删除该仓库的代码向量。
- 词表持久化失败时禁止发布向量。
- 缺少语义能力时，以可观测方式关闭语义检索，不静默替换其他后端。
- Rerank provider 失败时执行已记录的检索 fallback，并保留日志和 Trace。

#### 待完成的质量工作

##### C/C++ 运行形态

需要项目级 C/C++ 索引器根据构建/SDK 结构将固件仓库分类为 `mcu` 或 `module`。不能逐文件猜测，也不能硬编码已知仓库。

##### 事件驱动依赖

MQTT、Kafka、RabbitMQ 和 WebSocket 关系尚未全面提取。新增能力必须使用包含来源证据和置信度的通用边契约。

##### CodeGraph Fallback

增加空结构化 Walk 的 fallback 前，先度量依赖覆盖率。Fallback 必须改善一类通用缺边问题，不能重复或冲突已有边。

##### 改写评估

独立问题改写会对新增技术标识符做确定性校验；完整问题不再拼接最近会话。仍需建立多语言指代数据集，评估中文实体、短缩写和跨语言改写，避免针对特定 token 扩展启发式规则。

#### 评估

使用相关 Chunk、服务、依赖边和运行形态标签，度量 Recall@K、MRR/nDCG、上下文 Precision、重复率、来源/Layer 多样性、改写引入的无证据实体、延迟和最终答案 Groundedness。只有改善通用数据切片且不回归无关切片的机制才能接受。

### Agent 检索链路简化提案

> Migrated from CodeLoom `docs/design/agent/agent-retrieval-simplification-proposal.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：阶段一至三已实现，阶段四职责拆分待办

实现状态（2026-07-17）：已完成规范查询、Runbook 命中块读取、排名保持型多样性、逐项预算组装、可见引用统计、按需 CodeGraph 扩展和基于问题类型的 Agent 步数收敛。`AskAgentWithContext` 的职责拆分保留为后续结构改造。

本文解决 Internal 证据链路中“正确证据已召回，但在重排、格式化和预算组装后失去优先级”的机制问题，同时收敛查询改写、检索扩展和 Agent 补查中的意外复杂度。方案不针对某个问题、服务名或关键词编写特殊规则。

#### 1. 背景

当前链路具备多源检索、外部重排、结构扩展、上下文预算和 Agent 工具补查等能力。这些能力本身合理，但结构化证据在进入最终上下文前经历了多次排序和文本化：

```text
问题与会话历史
  -> 证据规划、问题清理、术语提取、可选独立问题改写
  -> Code / Runbook / Service 并行发现
  -> Service / Dependency / CodeGraph 扩展
  -> 统一代码池去重与重排
  -> layer + service 多样性轮询
  -> 按 server/app/front/docs 重新分组
  -> 根据 Markdown 标题推断 partial 优先级
  -> 大文本块按上下文预算截断
  -> Agent 最多继续五步工具调用
```

两类已观察到的问题具有同一个结构性原因：

1. 完整问题被无条件拼接最近会话，短查询被上一轮长问题主导。
2. 高分流程文档在重排后被 layer 分组移到低分代码之后；证据池合成大文本再截断，导致主链证据被旁路代码挤出，同时引用统计变为零。

#### 2. 目标

1. 一次请求只有一个用于 Internal 检索的规范查询。
2. 证据从召回到最终序列化始终保留来源、排名、可信等级和引用。
3. 重排后的全局顺序不会被多样性或展示分组破坏。
4. 上下文预算按证据项执行，不按合并后的大字符串执行。
5. Runbook 默认读取命中块，不把完整文档隐式带入在线路径。
6. `HitCount` 与实际注入上下文的唯一引用数一致。
7. 简单问题只执行一次预检索和至多一次定向补查；多跳调用链保留 Agent 能力。
8. 检索选择和组装保持单遍、有界、可观测，不引入按案例硬编码的分支。

#### 3. 非目标

- 不更换 Qdrant、BM25、DashScope Rerank、CodeGraph 或现有存储后端。
- 不重新设计索引文档载荷或触发全量重建。
- 不删除 Observe、Memory、Web 等能力边界。
- 不承诺一次改造完成所有答案质量问题。
- 不通过扩大上下文预算掩盖证据选择问题。
- 不为设备控制、IDP 或任何单一问题增加关键词规则。

#### 4. 当前根因

##### 4.1 查询表示不唯一

同一请求可能同时存在：

- 原始问题；
- `CleanQuestion`；
- 会话前缀加问题；
- 术语分析问题；
- 独立问题改写结果。

独立问题不需要历史补全时，当前实现仍可能把最近会话直接拼入检索查询。历史内容长度远大于当前问题时，稠密与稀疏召回都会偏向上一主题。

##### 4.2 结构化证据过早扁平化

`codeDoc` 已经携带 `source`、`layer`、`rerankScore`、`trustTier` 和位置，但 `formatCodePool` 将整个重排池转换为一个 `partial{text, refs}`。扁平化后无法知道预算截断实际覆盖了哪些引用，也无法继续执行按项选择。

##### 4.3 排名被两次破坏

第一处是按 `layer + service` 轮询选择多样性：低排名分组可能被提升到高排名证据之前。

第二处是格式化时固定按 `server -> app -> front -> mcu -> module -> docs` 输出。即使 Runbook 重排第一，也会因为 `layer=docs` 被移动到代码之后。

##### 4.4 Runbook 在线读取无界

Runbook 搜索返回相关块后，扩展阶段优先使用完整 `Record.Text`，只有全文为空才使用 `ChunkText`。这把一次有界块检索重新变成最长 8,000 字的文档读取，并可能重复合并同一全文。

##### 4.5 大块截断使引用失真

多个证据被合并为一个大 `partial`。当其长度超过剩余预算时，Assembler 截取字符串后立即退出，引用循环没有执行，因此可能出现“上下文非空但 `HitCount=0`”。若简单提前登记全部引用，又会把未实际进入上下文的证据计入结果。

##### 4.6 预检索与 Agent 补查职责重叠

预检索已经并行查询代码、Runbook、服务、依赖和 CodeGraph；随后 Agent 仍可用同类工具重复搜索最多五步。简单查询因此可能支付两套检索成本，失败时还会用不同措辞反复命中相同索引。

#### 5. 设计原则与不变量

##### 5.1 规范查询唯一

`canonicalQuery` 是 Internal 召回、术语提取和重排的唯一查询：

```text
问题需要指代消解 -> 使用经过校验的 standalone rewrite
问题语义完整       -> 使用 CleanQuestion
```

会话摘要和最近消息可供证据规划与最终回答使用，但不得直接拼入完整问题的向量或 BM25 查询。

##### 5.2 结构保留到序列化边界

检索、去重、重排、多样性和预算选择都操作结构化证据。只有最终注入 LLM 前才生成 Markdown 文本。

##### 5.3 相关性顺序优先

可信等级用于同分裁决或明确的权威性策略，多样性用于限制垄断，但二者都不能任意重排已排序证据。

##### 5.4 有界在线读取

代码窗口、Runbook 块、依赖边和服务信息必须在各自读取边界限制数量和大小。完整读取只能通过显式命名的完整文档路径触发。

##### 5.5 引用只描述可见证据

只有至少一个字符进入最终上下文的证据项才能贡献引用。被完全丢弃的证据不能出现在 `References` 或 `HitCount` 中。

#### 6. 目标架构

```text
Question + Conversation
          |
          v
QueryResolver ---------> canonicalQuery
          |
          v
EvidencePlanner -------> EvidencePlan
          |
          v
Backend Retrieval
  Code / Runbook / Service / Observe
          |
          v
[]EvidenceItem
          |
          v
Dedup -> Rerank -> Rank-preserving diversity -> Budget selection
          |
          v
ContextAssembler ------> text + included references + selection stats
          |
          v
Answerer --------------> final answer or one targeted evidence-gap lookup
```

##### 6.1 统一证据模型

建议在 retrieval 包内部引入：

```go
type EvidenceItem struct {
    Source        EvidenceSource
    Layer         string
    Service       string
    Text          string
    Reference     Reference
    RecallScore   float64
    RerankScore   float64
    TrustTier     int
    EvidenceClass string
    Rank          int
}
```

`EvidenceItem` 是 retrieval 内部契约，不向 domain 或 transport 扩散。现有 `codeDoc` 可以演进或在重排后转换为该类型。

##### 6.2 查询解析

`QueryResolver` 只负责：

1. 判断当前问题是否需要指代消解；
2. 必要时生成独立问题；
3. 校验改写未引入当前问题和会话中不存在的标识符；
4. 输出一个 `canonicalQuery`。

证据规划可以读取会话摘要，但查询解析不得把整个最近问答永久拼接到召回文本。

##### 6.3 后端召回

`EvidencePlan` 继续决定 Internal、Observe、Memory、Web 的能力边界。Internal 内部按意图选择必要后端：

- 架构、职责、流程：优先 Runbook、Service、Dependency；代码作为补充。
- 明确符号或调用链：优先 Code、CodeGraph；必要时补 Service。
- 运行时问题：Observe 与相关 Runbook；代码用于定位已知实现。
- 普通实体查询：Service 或 Symbol 精确查询，不默认扩展全部依赖和 CodeGraph。

该选择使用通用响应模式和结构化术语，不增加业务关键词表。无法可靠判断时允许并行召回，但扩展阶段仍必须命中驱动。

##### 6.4 Runbook 块读取

Runbook 证据默认使用 `ChunkText` 与 `SectionHeader`：

1. 按标题聚合命中块；
2. 同一标题按分数保留有限块；
3. 使用 map 去重完全相同的块；
4. 单文档总字符数有上限；
5. 只有显式全文请求使用 `Record.Text`。

##### 6.5 去重、重排与多样性

处理顺序保持为：

```text
有界候选 -> O(n) map 去重 -> 一次重排 -> 阈值 -> 排名保持型多样性
```

排名保持型多样性执行一次顺序扫描：

```text
for item in rankedItems:
    group = layer + service
    if group 未超过上限:
        选择 item
    if 已达到 topK:
        停止
```

非严格模式需要补齐 topK 时，再从第一次跳过的证据中按原排名补充。不能按分组轮询，也不能在格式化阶段再次排序。

##### 6.6 预算组装

Assembler 按已选证据的顺序单遍执行：

1. 为每项生成带来源标签的文本；
2. 应用来源级单项上限；
3. 取 `min(项长度, 剩余预算)`；
4. 实际写入后立即登记引用；
5. 预算耗尽后停止；
6. 最后生成标题和分隔符，不改变证据顺序。

展示 layer 时允许相邻同 layer 合并标题，但不得把非相邻证据搬到一起。

`RetrievedContext` 保留现有 API，并在 Trace 中增加：

- `retrieved_count`；
- `reranked_count`；
- `included_count`；
- `dropped_count`；
- `truncated_count`；
- `included_chars`。

`HitCount` 明确定义为 `len(unique included references)`。

##### 6.7 Agent 补查策略

预检索完成后按问题复杂度限制工具轮次：

- 简单查询和架构概览：直接回答，最多一次定向补查。
- 符号或依赖追踪：最多两到三步。
- 故障定位、写目标追踪等真实多跳任务：保留当前最大步数。

工具补查必须指出一个具体证据缺口。重复相同索引和相同意图的改写查询继续由现有去重与收敛机制阻止。

回答约束增加一条通用规则：证据包含多条路径时，分别标记主链、旁路、fallback 或主动刷新，禁止把不同调用链拼接成一条确认链路。

#### 7. 实施阶段

##### 阶段一：组装正确性

范围限制在 `internal/retrieval`：

1. 将重排结果转换为逐项证据，不再生成一个大 `partial`；
2. 多样性改为排名保持型扫描；
3. 移除按 layer 的二次排序；
4. Assembler 按项预算写入并同步引用；
5. 修正 `HitCount` 语义和 Trace。

该阶段不修改召回后端、索引数据和 Agent Prompt，能够独立验证与回滚。

##### 阶段二：查询与读取边界

1. 引入唯一 `canonicalQuery`；
2. 取消完整问题的历史前缀拼接；
3. Runbook 使用命中块优先；
4. 为不同来源设置可配置的单项字符上限；
5. 记录改写、召回和注入的查询与计数。

##### 阶段三：按需扩展与 Agent 收敛

1. 根据通用意图决定是否执行 Dependency 和 CodeGraph 扩展；
2. 简单问题限制为至多一次补查；
3. 多跳任务保持现有 Agent 能力；
4. 增加主链与分支路径回答约束。

##### 阶段四：职责拆分

在行为稳定后，将 `AskAgentWithContext` 的职责拆为：

- QueryResolver；
- EvidencePlanner；
- RetrievalCoordinator；
- ContextAssembler；
- AgentRunner。

QA Service 只负责顺序编排和能力降级，不直接拥有各阶段业务细节。该阶段不得与前面行为修复混在同一提交中。

#### 8. 测试方案

所有机制测试使用通用证据名称和跨 layer 样本，不使用已知失败问题的 token 作为生产规则。

##### 8.1 单元测试

- 排名第一的 docs 项与低排名 server/app 项混合时仍保持第一。
- 多样性限制不会提升低排名分组。
- 超长单项截断后 UTF-8 完整且引用被计入。
- 完全未写入的证据不贡献引用。
- 非空证据上下文的 `HitCount` 不为零。
- 同一 Runbook 全文不会因多个命中块重复合并。
- 语义完整问题不拼接历史；指代问题使用独立改写。
- 上下文总长度不超过配置预算。

##### 8.2 集成测试

- 高分流程文档与多语言代码同时命中时，注入顺序与重排顺序一致。
- 服务拓扑问题不默认注入无关 CodeGraph 符号。
- 调用链问题仍能触发必要的 CodeGraph 扩展。
- Rerank provider 失败时，本地顺序 fallback 保持确定性。

##### 8.3 回归评估

至少覆盖：

- 独立短问题接在长会话之后；
- 主链与旁路同时存在；
- 多服务架构概览；
- 明确符号查询；
- 多跳写路径；
- Observe 运行时问题。

指标包括 Recall@K、MRR/nDCG、上下文 Precision、重复率、有效引用率、截断率、答案 Groundedness、首答延迟和工具步数。

#### 9. 可观测性

每次 Internal 检索记录精简的结构化摘要：

```text
canonical_query_chars
retrieved / deduped / reranked / selected / included
context_budget / included_chars
included items: rank, source, layer, trust, chars, truncated
dropped reasons: threshold, diversity_cap, context_budget
```

日志不输出完整敏感正文。Trace 保留每阶段计数和前 N 个来源标识，支持区分召回失败、排序失败和组装失败。

建议增加以下不变量告警：

- `included_count > 0 && hit_count == 0`；
- `context_text != empty && references == 0`；
- 重排第一项未被注入且原因不是阈值或预算；
- standalone rewrite 引入未在问题或会话中出现的标识符。

#### 10. 验收标准

1. 重排后的第一条合格证据首先进入上下文。
2. 格式化阶段不改变证据全局顺序。
3. 不再产生包含多条来源的超大 `partial`。
4. 非空证据上下文的 `HitCount` 与唯一可见引用一致。
5. 完整独立问题不受上一轮长问题干扰。
6. Runbook 在线注入使用相关块且单文档有界。
7. 简单问题的平均工具步数不超过一次补查。
8. 多跳调用链与 Observe 能力无功能回归。
9. 不提高现有上下文预算也能稳定区分主链和旁路。

#### 11. 发布与回滚

- 每个阶段使用独立提交并运行 `go test ./internal/retrieval/...`、相关 Agent 测试、`go build ./...` 和 `go vet ./...`。
- 阶段一可通过保留旧 Formatter/Assembler 的临时配置开关进行对比，但开关只用于短期发布验证，稳定后必须删除。
- 上线前对固定评估集运行旧链路与新链路双跑，只记录结果，不重复调用最终回答 LLM。
- 若召回或答案指标回归，按阶段回滚；不通过提高预算或降低阈值临时掩盖。
- 阶段一至三不改变索引 Schema，无需全量重建。

#### 12. 预期复杂度变化

保留的必要复杂度：

- `EvidencePlan` 能力边界；
- 多后端并行召回；
- 一次重排与可信等级；
- 有界上下文组装；
- 多跳任务的 Agent 工具循环；
- 配置后端失败的可观测降级。

删除的意外复杂度：

- 多套竞争的检索查询；
- 排名后的分组轮询和 layer 二次排序；
- 从 Markdown 标题推断业务优先级；
- 证据全文合并后再粗粒度截断；
- 简单问题的重复预检索与多步搜索；
- 下游读取期的引用和命中数补救。

目标链路在外部重排的一次排序之外，选择和组装均为 O(n) 单遍处理，成员去重使用 map，所有在线读取在来源边界有明确上限。

### Agent 上下文与多意图证据质量修复提案

> Migrated from CodeLoom `docs/design/agent/agent-context-evidence-quality-remediation.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：阶段一实现中，阶段二和阶段三待实现

实现状态（2026-07-17）：当前工作树已加入语义 Runbook 多章节候选、文档/篇内/全局数量上限、独立章节证据项和候选诊断日志，但相关包尚未通过构建验收，数量上限也未配置化。模型 token 账本、Prompt 收敛、facet 覆盖和完整分数契约尚未实现。查询上下文改动还偏离“规范查询唯一”不变量。

本文是[检索链路简化提案](03-retrieval-and-knowledge.zh-CN.md)的后续专项设计。前一提案已经解决规范查询、排名保持、逐项预算和可见引用；本文处理仍然存在的四类问题：复合问题证据覆盖不完整、同一 Runbook 只保留一个命中块、字符预算与模型 token 窗口脱节，以及混合检索分数字段含义不清。

方案不针对某个问题、服务或关键词增加特殊规则。任何修复都必须改善一类通用输入，并保持在线读取和时间复杂度有界。

#### 1. 已确认的现状

##### 1.1 实现对齐表

| 能力 | 当前状态 | 已有实现 | 尚缺 |
|---|---|---|---|
| 规范检索查询 | 存在漂移 | 当前工作树将上一轮用户文本和回答中的反引号标识追加到查询 | 恢复“完整问题不拼历史、仅指代问题受校验改写” |
| Runbook 多章节语义召回 | 实现中 | `FindRunbookEvidence` 保留不同 section，先覆盖文档再扩展章节 | 选择器测试、配置化、构建验收 |
| Runbook 独立证据项 | 实现中 | 每个 chunk 独立进入后续证据池，单块仍按 4,000 rune 截断 | 阶段二改为 token 上限 |
| Runbook 关键词降级 | 旧路径 | 元数据筛选后回填文档正文 | 有界摘要读取、独立 `keyword` 分数语义 |
| 模型窗口预算 | 未实现 | Retriever 仍按 rune 消费 `ContextBudget` | 请求级和每轮 Provider 调用前的 token 账本 |
| Facet 与覆盖控制 | 未实现 | 只有规范查询和 QueryTerms | 有限 facet、召回来源、覆盖状态、一次定向补查 |
| 分数可观测性 | 部分完成 | Code hit 已区分 `ScoreKind`、`FusionScore`、`SemanticScore` | 原始重排、归一化、混合、信任和最终排序分 |

阶段完成的定义不是代码进入工作树，而是相关包测试、`go build ./...` 和本文对应验收项全部通过。

##### 1.2 “达到上下文预算”不是模型上下文溢出

`internal/retrieval/pipeline.go` 中的 `ContextBudget` 按 rune 扣减。配置名和默认常量使用 token 术语，但没有执行模型 tokenizer 计算。

当前一次 Internal 请求通常会同时产生 Code、Runbook、Service、Dependency 和可选 CodeGraph 候选。重排后的证据项总长度经常超过 16,000 rune，因此 Assembler 主动截断是预期行为，不代表 LLM API 拒绝了请求。

##### 1.3 主回答 Prompt 常驻成本偏高

基础系统 Prompt 与 Agent 模式附加 Prompt 合计约 20,000 个英文字符。规则中存在重复的证据边界、调用链验证、缺口处理和工具调用约束。它们增加固定输入成本，也让“优先直接回答”和“缺证据时继续查询”之间产生决策张力。

##### 1.4 Runbook 多章节主体已进入工作树

原实现中，Runbook 向量检索返回 chunk 级结果，但 `runbooksFromHits` 以文档 ID 为 key，只保留整篇文档的最高分 chunk。

当前工作树已经改为按文档和 section 去重，先给每篇入选文档一个主 section，再从统一扩展池补第 2、3 个 section，并让每个 chunk 独立进入后续证据池。尚未闭环的部分包括：

- 上限仍是代码常量；
- 选择和阈值仍混用 trust-adjusted `Score` 与原始 `SemanticScore`；
- 关键词降级仍是文档级正文回填，不能声明 section/facet 覆盖；
- 选择器缺少直接单元测试，当前相关包也尚未完成构建验收。

##### 1.5 规范查询不变量出现实现漂移

前一提案要求语义完整问题只使用 `CleanQuestion`，只有真实指代问题才使用经过实体约束校验的 standalone rewrite。当前工作树正在删除 standalone rewrite，并通过 `buildRagCtx` 将上一轮用户问题和回答中的反引号标识追加到每次检索查询。

这会让完整问题重新受上一主题影响。会话可以参与 EvidencePlan、facet 生成和最终回答，但不能无条件拼入 dense/BM25 查询。若改用确定性指代解析器，必须证明它只引入会话中真实存在的实体，并在失败时回退当前问题。

##### 1.6 RRF 结果没有 dense 分量

代码混合检索通过 Qdrant RRF 融合 dense 和 BM25 sparse 排名。当前适配层只保存最终 `FusionScore`，不保存各 prefetch 分量。因此 RRF 日志中的 `dense=0` 表示“不可用”，不能解释为余弦相似度为零。

#### 2. 目标

1. 一个问题包含多个独立意图时，每个关键意图至少有一条合格证据或一个明确缺口。
2. 语义完整问题只使用自身的规范查询；只有真实指代问题才能引入受校验的会话实体。
3. 同一 Runbook 可以贡献多个不同章节，但不能无条件填满固定数量。
4. Runbook 选择同时限制文档数、每篇章节数、全局块数、单块 token 和总 token。
5. 检索预算使用模型 token 语义，并从实际模型窗口动态计算。
6. 每次 Provider 调用前都校验 Prompt、工具、历史、证据、工具结果、输出和安全余量。
7. Prompt、工具定义、历史、证据、工具结果、输出预算和 Provider usage 分别可观测。
8. `dense`、`fusion`、本地重排、外部重排、混合分、信任加分和最终排序分不再共用含义模糊的 `score`。
9. 召回、选择、组装保持 O(n) 聚合和有界排序；不可通过加载全文或无界候选改善个别问题。

#### 3. 非目标

- 不通过简单扩大 `ContextBudget` 掩盖召回或选择缺陷。
- 不为特定业务问题增加关键词分支。
- 不假设私有模型别名对应某个公开模型窗口。
- 不为了生产日志额外执行一次完整 dense 和 sparse 查询。
- 不在本阶段替换 Qdrant、Voyage、BM25 或 DashScope Rerank。
- 不把完整 Runbook 读取恢复为普通在线路径。
- 不用原始历史拼接替代受约束的指代消解。
- 不把估算 token、Provider usage 和 reasoning delta 计数混为同一个指标。

#### 4. 模型窗口与动态预算

##### 4.1 显式模型能力

平台设置新增：

```text
llm_context_window_tokens
retrieval_evidence_cap_tokens
agent_tool_result_reserve_tokens
llm_context_safety_margin_tokens
```

复用并明确现有 `llm_max_tokens`、`llm_answer_max_tokens` 和 `agent_conclusion_max_tokens`。请求输出预留使用：

```text
output_reserve_tokens = max(
    llm_answer_max_tokens,
    agent_conclusion_max_tokens
)
```

新增设置属于 MySQL 平台设置表，由 `PlatformSettings` 管理，不新增 `CODELOOM_*` 环境变量。设置写入边界负责范围和交叉约束校验；运行时不重复修剪、补默认值或解释旧别名。

`llm_context_window_tokens` 对未知私有别名必须显式配置。类似 `deepseek-v4-pro` 的网关别名不能从字符串推导窗口；缺少配置时应拒绝启用动态预算并输出明确错误，不能静默套用另一个公开模型的默认值。

现有 `context_budget` 是 rune 语义，不能 1:1 复制为 token。迁移必须根据已确认模型窗口和评估集写入新的 token cap，记录旧值、新值和迁移版本，再删除旧 key。运行时不保留永久读取兼容分支。

##### 4.2 请求级预算公式

预算属于 Agent 领域的 QA 编排边界，而不是 Retriever。当前调用顺序是先检索、再进入 Agent loop，因此 QA 必须在调用 `RetrievePlan` 前生成与实际请求一致的 Prompt/工具策略，计算本次证据预算，再作为不可变请求参数传给 Retriever。不能通过修改共享 `PlatformSettings` 传递请求预算。

```text
fixed_input_tokens =
    system_prompt_tokens
  + identity_and_plan_tokens
  + tool_schema_tokens
  + conversation_tokens
  + question_tokens
  + message_envelope_tokens

available_evidence_tokens =
    model_context_window_tokens
  - fixed_input_tokens
  - output_reserve_tokens
  - agent_tool_result_reserve_tokens
  - llm_context_safety_margin_tokens

effective_evidence_tokens = min(
    retrieval_evidence_cap_tokens,
    available_evidence_tokens
)
```

若结果小于等于零，请求在进入 Retriever 前失败并报告各预算项，不能依赖 Provider 返回不可预测的上下文错误。

初始证据预算只约束第一轮。每次 Provider 调用前还必须重新校验：

```text
estimated_current_input
+ current_turn_max_output
+ safety_margin
<= model_context_window
```

工具结果按 token 从 `agent_tool_result_reserve_tokens` 扣减。单次结果在 ToolExecutor 边界截断并记录；累计预留耗尽后停止继续调用工具。收缩历史工具消息时必须保持 tool call/result 成对，不能制造非法消息序列。

##### 4.3 Token 估算边界

优先使用与主模型一致的 tokenizer。无法取得 DeepSeek 网关实际 tokenizer 时，使用明确标记为 `conservative_estimate` 的保守估算器：

```text
ASCII/code bytes: ceil(bytes / 3)
non-ASCII runes:  2 tokens per rune
message overhead: 10% safety factor
```

估算器类型和估算误差必须进入 Trace。`ChatStreamResult` 应增加可选的实际 input/output/reasoning token usage；Provider 不返回时标记 unavailable。现有按 reasoning delta 累加的数量只能作为流式诊断，不能冒充 Provider usage。不得在下游永久散布字符/token 换算逻辑。

##### 4.4 DeepSeek V4 Pro 初始值

实际窗口以网关模型卡为准。若确认窗口为 128K，可从以下配置开始：

```text
llm_context_window_tokens          = 131072
llm_answer_max_tokens              = 8192
agent_conclusion_max_tokens        = 8192
retrieval_evidence_cap_tokens      = 10000
agent_tool_result_reserve_tokens   = 16000
llm_context_safety_margin_tokens   = 12000
```

若窗口为 64K，证据上限调整到 6K–8K，工具结果预留 10K–12K，安全余量至少 6K。修复证据选择前不得仅因为窗口更大而扩大证据上限。

#### 5. Prompt 收敛

##### 5.1 常驻 Prompt 只保留不变量

常驻规则收敛为：

1. 证据边界与权威性；
2. 行为结论需要已验证调用链；
3. 不同客户端入口和分支路径必须分开；
4. 忽略无关召回；
5. 对用户的独立子问题执行覆盖检查；
6. 缺少关键证据时最多进行一次定向补查；
7. 使用用户语言并直接回答；
8. 不泄露内部上下文和控制标记。

Mermaid 语法、长格式示例和任务类型模板改为按 `ResponseMode` 加载。Agent 工具说明只保留工具选择和停止条件，不重复基础证据规则。

##### 5.2 最终回答覆盖门

最终生成前执行结构化覆盖检查：

```text
requested facets -> covered | missing
```

若关键 facet 缺失且仍有工具步数，执行一次针对该 facet 的查询。禁止用原问题的同义改写重复搜索相同索引。补查后仍缺失时，最终答案准确说明缺口。

该检查只输出状态，不要求保存或暴露模型思维链。

#### 6. Runbook 多章节选择

##### 6.1 Top 3 是上限，不是填充目标

每篇 Runbook 最多贡献三个合格章节。只有一个章节达到门槛时返回一个；不能为了凑满三个加入低相关块。

配置分别表达不同边界：

```text
runbook_max_documents       = 5
runbook_max_chunks_per_doc  = 3
runbook_max_chunks_total    = 10
runbook_max_chunk_tokens    = 1200
runbook_min_semantic_score  = migrated existing threshold
```

写入边界校验正数、`runbook_max_documents <= runbook_max_chunks_total` 和全局硬上限。

##### 6.2 篇内分数

Runbook 当前使用 dense 搜索。chunk 的篇内相关性使用 Qdrant 原始 `semantic_score`：

```text
semantic_score = cosine(query_embedding, chunk_embedding)
qualification = semantic_score >= runbook_min_semantic_score
final_rank_score = semantic_score + bounded_trust_bonus
```

trust bonus 不能救回低于语义门槛的 chunk。同一文档的证据等级相同，篇内顺序只看 `semantic_score`；跨文档可以使用 `final_rank_score`，但 Trace 必须保留两个分量。当前工作树仍混用 trust-adjusted `Score` 与 `SemanticScore`，这是阶段一完成前需要修正的契约偏差。

##### 6.3 Section 去重

候选先按下列 key 去重：

```text
document_key + "\x00" + section_key
```

`document_key` 优先使用 Runbook ID，其次使用规范 Path。`section_key` 优先使用已规范化的 `section_header`，为空时使用稳定 `chunk_id`；旧数据两者都缺失时可使用正文稳定哈希，但不能使用完整正文作为 map key。一个 section 有多个窗口时只保留 `semantic_score` 最高的窗口，并合并其 facet match。

##### 6.4 两阶段有界选择

没有 facet 时，第一阶段选择文档：

```text
doc_score = max(section.final_rank_score)
```

按 `doc_score` 保留最多 `runbook_max_documents` 篇文档。

第二阶段选择章节：

1. 每篇文档先选最高分章节，保证文档覆盖；
2. 将各文档剩余第 2、3 个章节放入统一候选池；
3. 按 `final_rank_score` 降序补充；
4. 达到每篇上限或全局上限立即停止。

所有集合使用 map 去重，排序只发生在有界候选集合上。选择复杂度为 O(n) 聚合加一次不可避免的 O(k log k) 有界排序。

##### 6.5 Facet 覆盖

查询预处理最多生成三个语义独立、未引入新实体的 facet。每个 chunk 保留它来自哪个 facet 的召回信息：

```go
type FacetMatch struct {
    FacetID string
    Score   float64
}
```

Facet 必须在文档 top-N 截断前参与选择，否则低频关键 facet 的唯一文档可能先被丢弃。选择器先让每个关键 facet 声明最佳合格 section，再补文档名额、每篇主 section 和扩展 section。没有可靠 facet 时退化为 Section 去重加两阶段选择，不通过关键词启发式猜测。

##### 6.6 关键词降级

Embedding 或 Qdrant 不可用时必须显式输出：

```text
score_kind=keyword
semantic_score=n/a
```

关键词路径只承诺文档级召回，不承诺 section/facet 覆盖。它先按元数据筛选有限文档，再读取有界摘要或显式命中片段；不能逐篇加载无界全文。关键词分数不能与余弦或 RRF 阈值共用一个 `runbook_min_score`。

##### 6.7 预算组装

选中的 Runbook chunk 保持独立 `EvidenceItem`，不得重新合并成单篇 4,000 rune 大字符串。Assembler 按项写入，以免第一个长章节再次截掉第二、第三章节。

每项先应用 `runbook_max_chunk_tokens`，再进入总预算。只有实际写入至少一个 token 的证据项才能贡献引用。阶段二完成后删除 4,000 rune 的永久兼容分支。

#### 7. 混合检索与重排可观测性

##### 7.1 字段契约

Trace 和日志明确输出：

```text
recall_score_kind    // dense | rrf | keyword
dense_score           // unavailable 时为 null / n/a
fusion_score
keyword_score
rerank_kind           // disabled | local | external
local_rerank_score
external_rerank_raw_score
external_rerank_normalized_score
blended_score
trust_bonus
final_rank_score
threshold_field
selected
drop_reason
```

禁止在不同阶段复用 `score` 表示不同含义。

##### 7.2 RRF 的 dense 语义

Qdrant RRF 普通响应只有融合结果时：

```text
recall_score_kind=rrf
dense_score=n/a
fusion_score=<rrf result>
```

不能输出 `dense_score=0`。生产请求不为填充该字段增加第二次查询；只有采样诊断模式可以分别执行 dense/sparse 查询，并按 point ID 关联 rank 和分数。

##### 7.3 重排公式

外部重排结果保留原始分、批内归一化分、混合分和信任加分：

```text
external_rerank_normalized = external_rerank_raw / max_external_rerank_raw
normalized_recall = recall / max_recall
blended_score = 0.7 * external_rerank_normalized
              + 0.3 * normalized_recall
final_rank_score = blended_score + trust_bonus
```

本地 fallback 使用 `rerank_kind=local` 和独立字段，不能冒充外部重排。阈值使用哪个字段必须在配置和 Trace 中明确。`drop_reason` 至少区分 `min_score`、`document_cap`、`section_cap`、`facet_cap`、`diversity_cap`、`top_k`、`duplicate` 和 `context_budget`。

#### 8. 实施阶段

##### 阶段一：完成 Runbook 多章节基础

已进入工作树：

1. Runbook 按文档保留多个不同 section；
2. 增加文档、篇内和全局 chunk 上限；
3. Runbook chunk 独立进入预算组装；
4. 增加去重前候选诊断日志。

完成前仍需：

1. 恢复规范查询唯一不变量，并补齐独立问题与指代问题测试；
2. 修正 qualification 与排序分数字段；
3. 将数量上限迁入平台设置并校验；
4. 为选择器、关键词降级和独立组装补齐测试；
5. 确保相关包测试、`go build ./...` 和 `go vet ./...` 通过；
6. 将逐条 Info 日志收敛为结构化、可采样的 Trace。

该阶段不引入 facet，不修改模型预算和索引 Schema。

##### 阶段二：Prompt 与 token 预算

1. 压缩基础和 Agent Prompt；
2. 增加模型窗口和各类 token reserve 配置；
3. 引入统一 TokenEstimator；
4. QA 编排层计算请求级证据预算并显式传给 Retriever；
5. Agent loop 在每轮 Provider 调用前更新账本；
6. 扩展 Provider usage 契约；
7. 一次性迁移并删除 rune `context_budget` 和 Runbook rune 单项上限。

##### 阶段三：多意图召回与分数可观测性

1. Query preprocess 输出有限 facet；
2. 多查询召回以结构化 facet metadata 合并；
3. facet 在文档 top-N 前参与选择；
4. 增加覆盖状态和最多一次定向补查；
5. 拆分 recall、rerank、blend、trust 和 final score 字段；
6. 采样记录估算 token 与 Provider 实际 usage 偏差。

阶段三依赖阶段二的工具结果 token 预留，不能把覆盖门提前放入阶段一。

#### 9. 测试

##### 9.1 Runbook 单元测试

- 同一文档三个不同 section 均达到门槛时可全部保留；
- 同一 section 多个窗口只保留最高分窗口；
- 第四个 section 不超过篇内上限；
- 低于门槛的 section 不为凑数进入结果；
- 五篇文档先各获得一个主 section，再分配扩展 section；
- 总块数不超过全局上限；
- 长第一块不会在组装阶段吞掉后续合格块。
- 低于语义门槛的 section 不会被 trust bonus 救回；
- 关键词降级输出 `score_kind=keyword`，且不声明 section coverage。

##### 9.2 多意图测试

- 语义完整问题不拼接上一轮用户问题或回答标识；
- 指代问题只引入会话中真实存在的实体；
- 指代消解失败时回退当前问题并可见记录；
- 两个独立 facet 命中同一文档不同章节时均进入上下文；
- facet 唯一文档不会在普通文档 top-N 阶段提前丢失；
- 一个 facet 无证据时产生显式缺口；
- 有证据但因预算未注入时与真正无证据区分；
- facet 改写不能引入问题和会话中不存在的技术实体；
- 单意图查询不被强行拆分。

##### 9.3 预算测试

- 系统、工具、历史、证据、工具结果、输出和余量之和不超过模型窗口；
- 预算不足时在检索前返回分项错误；
- 中英文和代码混合输入的估算不低于安全阈值；
- Provider usage 可用时记录估算偏差；
- 工具调用追加结果后仍保留输出和安全余量。
- 工具消息裁剪保持 tool call/result 协议成对；
- 并发请求使用各自不可变预算，不污染共享配置；
- 旧 rune 值不会被当成同数值 token 读取。

##### 9.4 分数日志测试

- RRF 结果打印 `dense=n/a`；
- dense-only 结果打印真实余弦分数；
- keyword 结果不写 semantic/fusion 分；
- 外部原始分、归一化分、混合分、信任加分和最终排序分可分别验证；
- 本地 fallback 不冒充外部重排；
- 未入选结果有稳定 `drop_reason`。

#### 10. 可观测性

每次请求至少记录一条预算摘要：

```text
window / fixed_input / evidence_cap / evidence_used
tool_reserve / tool_used / output_reserve / safety_margin
estimator_kind / provider_usage_available / estimate_error
```

Runbook 选择 Trace 记录 recalled、qualified、section_deduped、documents_selected、chunks_selected、chunks_included、facet coverage 和各类 `drop_reason` 计数。详细候选只在 Trace 或采样诊断中保留前 N 条来源标识和分数字段；普通日志不逐条输出所有候选，也不输出正文。

建议增加不变量告警：

- `estimated_input + output_reserve + margin > window`；
- `included_count > 0 && hit_count == 0`；
- covered facet 没有关联实际 included evidence；
- `recall_score_kind=rrf && dense_score != null`；
- 迁移完成后仍读取旧 `context_budget`。

#### 11. 验收标准

1. 复合问题的关键 facet 覆盖率显著提高，且上下文 Precision 不下降。
2. 完整独立问题的检索结果不受上一轮长问题干扰，指代问题仍能正确补全实体。
3. 同一 Runbook 多章节能够进入上下文，但读取、候选数和 token 成本始终有界。
4. 不提高证据上限也能补齐被文档级折叠丢失的相关章节。
5. 常驻 Prompt token 在同一估算器下至少下降 50%，核心证据约束无回归。
6. 任意 Provider turn 都能解释模型窗口如何分配给 Prompt、工具、历史、证据、工具结果和输出。
7. 日志不再把不可用的 dense 分量表示为零。
8. 主模型别名、窗口、输出上限或 Runbook 数量设置不一致时在配置边界明确失败。
9. 固定评估集的 Groundedness、facet coverage 和有效引用率提升，平均工具步数与首答延迟不显著回归。
10. 语义后端不可用时只降级为明确的文档级关键词结果，不伪造 section/facet 覆盖。
11. 相关单元测试、`go build ./...` 和 `go vet ./...` 全部通过；涉及共享并发状态时增加定向 race 测试。

#### 12. 发布与回滚

- 三个阶段分别交付，不在一个变更中同时修改召回、Prompt 和预算。
- 阶段一对旧/新 Runbook 选择执行影子评估，只记录证据序列，不重复调用最终回答模型。
- 阶段二上线前从网关确认 DeepSeek V4 Pro 的实际上下文和输出上限。
- 阶段三先以采样 Trace 开启详细分数，避免生产日志和额外查询成本失控。
- 回归时按阶段回滚；不得通过放大预算、降低阈值或加入业务关键词临时掩盖。

### QA 知识文档检索与工具类型边界设计

> Migrated from Nasuta `docs/design/qa-runbook-retrieval-and-tool-boundaries.zh-CN.md`; incorporated into this module on 2026-07-31.

#### 1. 状态与结论

- 状态：设计完成，待实施
- 所属模块：Nasuta QA、知识文档检索、Agent 工具执行
- 适用入口：`/api/qa/ask`、MCP `search_runbooks`、内部 `knowledge.Reader`
- 关联设计：`qa-tool-selection-and-multiturn-evidence.zh-CN.md`、`qa-context-pollution-control.zh-CN.md`

本文解决以下同一类问题：

1. 预检索已命中 `flow-system-overview`，Agent 却把 runbook ID 传给 `search_code`。
2. Agent 把 runbook ID 作为服务名传给 `check_docs`。
3. Agent 把 runbook ID 作为代码符号传给 `get_symbol`，进而命中 Android/iOS 中无关的 `SYSTEM` 枚举。
4. 系统总览只注入了一个片段，完整网关清单没有进入最终上下文。
5. 菜谱域的“三个入口网关”被错误提升为平台整体架构结论，而平台实际有七个网关。

核心决定如下：

1. 不新增 `get_knowledge_doc`，复用并增强现有 `search_runbooks`。
2. `search_runbooks` 增加可选 `doc_id`，支持在已知文档内检索多个相关分块。
3. 返回结构始终按文档分组，每个 `chunkText` 只对应一个 `chunkIndex`。
4. 预检索引用保留 `runbook`、`service`、`symbol` 类型；每个工具在自身定义中声明目标参数可接受的
   引用类型，执行器从工具定义读取约束，不维护中央允许关系表。
5. 类型错误属于工具调用失败，不能作为证据，也不能记录为成功调用。
6. 不新增 `applies_to` 类型。最终回答不得扩大证据原文中的主语、范围和量词；已有 `service` 关联
   和 ontology 关系继续用于可确定的服务级约束。
7. 不新增数据库表；已有向量 payload 已包含文档内检索所需的 `doc_id` 和分块字段。

#### 2. 根因

##### 2.1 两类文档使用了不同索引

`search_code` 实际查询 `kind=code_chunk`。代码仓库中的 Markdown 会作为代码分块进入该索引，
因此工具描述中的“docs”容易被理解成全部知识文档。

平台知识库中的架构、流程、Schema、模块和运维文档使用 `kind=runbook`。它们由
`search_runbooks` 查询，与仓库 Markdown 不是同一个检索集合。

因此，用以下调用读取已知知识文档在语义上是错误的：

```text
search_code("flow-system-overview architecture gateway")
```

该调用只能从 `code_chunk` 中召回相似仓库文件，不能精确补全 `flow-system-overview`。

##### 2.2 `search_runbooks` 只能发现文档，不能补全已知文档

当前全库搜索会把同一文档的多个向量命中按文档 ID 合并，只保留最佳分块。这适合发现相关
文档，但不适合回答“这篇文档中的完整网关清单是什么”。

Agent 已经知道文档 ID，却没有参数把后续检索限定在该文档内，只能继续尝试其他工具。

##### 2.3 工具执行只校验 JSON Schema

`check_docs(service="flow-system-overview")` 和
`get_symbol(query="flow-system-overview")` 都满足字符串参数的 JSON Schema，因此工具层会执行。
调用虽然没有产生有效证据，却仍被记录为工具成功。

##### 2.4 回答扩大了证据原文的陈述边界

现有 `scope=flow` 表示文档种类。菜谱域文档原文中的主语是菜谱域，但最终综合忽略了该主语，把
“菜谱域涉及三个入口网关”改写成“整个平台只有三个网关”。问题是证据使用时扩大了原文的主语和
量词，不是缺少一个预先枚举的平台/领域类型。

为当前案例新增 `applies_to=system|domain/*|service/*` 会形成第二套业务分类：每出现一个新领域或
新的组织层级就要扩展约定，还会与已有 `service`、tags 和 ontology 关系产生重复或冲突。因此本文
不引入该字段。

#### 3. 目标与非目标

##### 3.1 目标

1. 已知文档 ID 时，只在该文档内补充证据。
2. 同一文档可返回多个独立、可追溯的分块。
3. 已知 runbook ID 不得进入服务或代码符号工具。
4. 工具边界错误在 Run、证据清单和前端中均可见。
5. 最终结论不超出证据原文明确表达的主语、范围和量词。
6. 所有读取有硬上限，检索、去重和组装采用最低实际时间复杂度。
7. 现有 QA、MCP 和 Feature Delivery 使用同一个 `search_runbooks` 合同。

##### 3.2 非目标

- 不增加第二个功能重叠的知识文档读取工具。
- 不把所有普通搜索词预先分类成 runbook、service 或 symbol。
- 不维护“引用类型 → 工具 ID 列表”的中央静态映射。
- 不靠一个特殊网关关键词修复单个问题。
- 不新增 `applies_to` 字段、封闭枚举或另一套领域分类体系。
- 不返回或注入无界的完整 Markdown 正文。
- 不通过永久兼容分支同时维护两套返回结构。

#### 4. `search_runbooks` 合同

##### 4.1 输入

```json
{
  "query": "平台网关清单和接入层架构",
  "doc_id": "flow-system-overview",
  "limit": 3
}
```

| 字段 | 必填 | 约束 | 含义 |
| --- | --- | --- | --- |
| `query` | 是 | 规范化后非空 | 当前需要查证的事实，不是文档标题的重复描述 |
| `doc_id` | 否 | 规范化文档 ID | 不为空时只检索该文档 |
| `limit` | 否 | 默认 3，范围 1 至 10 | 全库检索时限制文档数，文档内检索时限制分块数 |

输入规范化只在工具入口执行一次。下游检索、分组和响应组装信任已建立的规范值，不重复执行
`TrimSpace`、大小写兼容或空值回退。

##### 4.2 统一输出

```json
{
  "matches": [
    {
      "docId": "flow-system-overview",
      "title": "System Overview",
      "path": "docs/knowledge-base/flows/flow-system-overview.md",
      "docKind": "flow",
      "evidenceClass": "curated_flow",
      "trustTier": 85,
      "chunks": [
        {
          "chunkIndex": 2,
          "sectionHeader": "Gateway Layer",
          "chunkText": "...七个网关...",
          "semanticScore": 0.81
        },
        {
          "chunkIndex": 3,
          "sectionHeader": "Service Layers",
          "chunkText": "...服务分层...",
          "semanticScore": 0.76
        }
      ]
    }
  ],
  "semantic": true,
  "docScoped": true,
  "truncated": false
}
```

输出不变量：

1. `matches` 始终按文档分组，不因是否传入 `doc_id` 改变结构。
2. 一个 `chunkText` 只对应一个 `chunkIndex`。
3. 不直接拼接不相邻分块，不伪造连续原文。
4. `chunks` 在选取完成后按 `chunkIndex` 升序排列。
5. `semanticScore` 属于分块，`evidenceClass` 和 `trustTier` 属于文档证据来源。
6. `truncated=true` 表示还有匹配分块未返回，不表示文档中的事实不存在。

现有内部 `knowledge.RunbookSearchResult`、Feature Delivery 消费者、Dashboard API 和 MCP 输出在
同一变更中切换到该结构，不保留旧平铺 `matches` 的运行时兼容分支。

#### 5. 检索行为

##### 5.1 全库检索

未传 `doc_id` 时，目标是发现最相关的不同文档：

```text
embed(query)
  -> semantic search where kind=runbook
  -> 按 doc_id 保留最佳分块
  -> 最多 limit 篇文档
  -> 每篇文档通常包含一个 chunks 元素
```

去重使用 `map[string]int` 保存文档 ID 到结果位置，单次扫描完成，时间复杂度为 O(n)。不在循环
中使用 `slices.Contains`。

语义命中的 payload 已包含文档 ID、标题、路径、分块和证据字段。语义检索成功时直接从有界
命中构建结果，不再先读取全部 runbook 元数据后做内存关联。

##### 5.2 文档内检索

传入 `doc_id` 时，目标是补齐一篇已知文档中的相关证据：

```text
RunbookMetaByID(doc_id) 用窄字段查询精确确认文档存在
  -> embed(query)
  -> semantic search where kind=runbook AND doc_id=<doc_id>
  -> 按 chunk_index 去重
  -> 请求 limit+1 个分块并据此计算 truncated
  -> 选取最多 limit 个分块
  -> 按 chunk_index 排序后返回
```

分块去重使用 `map[int]struct{}`，选择阶段为 O(n)。最终排序仅作用于硬上限 `k <= 10` 的结果，
复杂度为 O(k log k)，用于恢复文档阅读顺序。

`RunbookMetaByID` 只读取 ID、标题、文件路径和文档种类，不读取 `content`。语义路径直接使用向量
payload 中的分块正文，不允许为了确认文档存在或做结果关联而加载整篇 Markdown。文档内语义查询的
点 ID 已按 `doc_id + chunk_index` 唯一，因此读取 `limit+1` 条即可准确判断是否还有结果，时间与空间
复杂度均为 O(limit)。

文档不存在时返回明确错误：

```json
{
  "code": "runbook_not_found",
  "docId": "flow-system-overview"
}
```

不得移除 `doc_id` 后静默退化为全库搜索。

##### 5.3 后端失败

- 未配置语义检索时，使用已记录且有界的关键词检索能力。
- 已配置语义后端但调用失败时，返回可观察错误，不静默替换成另一 Provider。
- 关键词路径同样必须在存储边界设置 `LIMIT`，不能读取全部文档后在内存切片。
- 文档内关键词降级只读取目标文档；正文读取受文档上传大小上限约束，匹配采用单次扫描，输出仍受
  `limit` 和单分块字符预算约束。

#### 6. 工具职责边界

工具描述修改为以下语义。

##### `search_runbooks`

搜索知识库中的系统架构、业务流程、模块、Schema、业务说明和运维文档。已知文档 ID 时通过
`doc_id` 限定范围，并在该文档中检索相关分块。知识文档描述设计或预期行为，不自动证明当前
运行时状态。

##### `search_code`

搜索 `code_chunk` 类型的源码、配置、SQL 和代码仓库 Markdown。它不搜索知识库 runbook，
也不能用于读取预检索已知的知识文档。

##### `check_docs`

检查一个规范服务名的文档、入口点、API、下游依赖和 source-of-truth 覆盖情况。它不读取、
校验或补全某篇知识文档。

`check_docs` 不再模糊接受任意关键词。调用方先通过 `get_service` 获得规范服务名，再执行覆盖
检查。无法精确解析服务时返回 `service_not_found`，不能返回容易误解的 `missing: service-card`。

##### `get_symbol`

查询函数、方法、类或接口的代码图定义。参数描述中删除 `service keyword`，明确禁止传入服务
名、文档标题或 runbook ID。

#### 7. 工具自描述的引用约束

##### 7.1 引用类型

预检索引用使用受控常量：

```go
type ReferenceType string

const (
    ReferenceRunbook ReferenceType = "runbook"
    ReferenceService ReferenceType = "service"
    ReferenceSymbol  ReferenceType = "symbol"
)
```

引用继续携带规范目标：

```json
{
  "type": "runbook",
  "label": "flow-system-overview",
  "target": "flow-system-overview"
}
```

每次 Agent Run 在开始时对有界引用单次扫描，建立：

```go
map[string]ReferenceType{
    "flow-system-overview": ReferenceRunbook,
    "hsmf-mobile-gateway":  ReferenceService,
}
```

后续成员判断为 O(1)。该索引只属于当前 Run，不持久化为新的会话状态。

##### 7.2 约束归工具定义所有

不在校验器中维护以下形式的中央映射：

```go
map[ReferenceType][]tool.ToolID
```

工具在注册时与描述、输入 Schema 一起声明哪个参数承载目标引用，以及该参数接受哪些已有引用
类型。概念合同如下：

```go
type ReferenceInput struct {
    Argument string
    Accepts  []string
}

type Tool struct {
    // 现有字段省略。
    ReferenceInputs []ReferenceInput
}
```

例如，`check_docs` 自己声明 `service` 参数接受 `service` 引用；`get_symbol` 自己声明 `query` 参数接受
`symbol` 引用；`search_runbooks` 的 `doc_id` 参数接受 `runbook` 引用。`search_code.query` 是自由检索
入口，但若其中出现当前 Run 已知的规范引用，其合同可以接受 `service`、`symbol`，而不接受
`runbook`。这些知识与工具定义同处一处，不散落到执行器或提示词中的工具 ID switch。

带实体目标参数的扩展工具通过 `ReadTool` 注册时同时声明自己的 `ReferenceInputs`；没有实体目标的
工具保持为空。注册表快照基于这些声明派生反向索引，供错误提示查找候选工具：

```go
map[string][]tool.ToolID // reference type -> tools derived from the snapshot
```

该索引是工具目录的派生数据，不是另一份人工配置。新工具只修改自己的定义，无需修改校验器。
构建复杂度为 O(T×A)，其中 T 是本次快照中的工具数，A 是每个工具的目标参数数；每个 Run 复用
同一快照和索引。

##### 7.3 校验边界

执行器只检查两类已经有可靠类型的信息：

1. 预检索产生并携带类型的规范引用；
2. 工具调用目标参数中以完整 token 边界出现的规范引用。

校验器单次扫描目标参数，并通过当前 Run 的引用索引做 O(1) 查找。它不分类所有自然语言词语，
不根据网关、菜谱等业务关键词猜类型，也不限制没有命中规范引用的普通自由检索。

`ReferenceInputs` 为空表示该工具没有可执行的实体类型约束，而不是接受或拒绝全部引用。工具输入
仍由 JSON Schema 完成结构校验，两者职责不重叠。

##### 7.4 类型错误

以下调用在实际 Handler 执行前拒绝：

```text
search_code(query="flow-system-overview architecture gateway")
check_docs(service="flow-system-overview")
get_symbol(query="flow-system-overview")
```

统一错误：

```json
{
  "code": "entity_type_mismatch",
  "entity": "flow-system-overview",
  "actualType": "runbook",
  "tool": "get_symbol",
  "candidateTools": ["search_runbooks"]
}
```

`candidateTools` 从当前 Registry Snapshot 的反向索引实时派生，不在错误处理代码中写死
`search_runbooks`。没有候选工具时返回空数组，不进行跨 Provider 或跨工具的静默替换。

该结果必须满足：

- `ToolExecution.Failed=true`；
- `tool_failure_count` 增加；
- 不增加证据结果数；
- Evidence Manifest 记录失败工具和错误码；
- 最终证据状态至少为 `partial`，不存在其他证据时为 `unavailable`；
- `tool_result` 事件携带该次调用的 `failed=true`，前端在对应工具输出卡片原位显示红色“失败”并
  展示错误内容；不在最终回答下方增加“工具调用失败 N 次”的汇总标签。

#### 8. Agent 策略

Agent 工具策略增加：

```text
预检索引用带实体类型时，选择自身 ReferenceInputs 合同接受该类型的工具。

已知 runbook ID 且现有片段不足时，调用：
search_runbooks(query=<当前缺失事实>, doc_id=<runbook ID>)。

文档内检索失败后，不得切换到自身合同不接受 runbook 引用的工具。
一次有针对性的文档内检索仍不足时，以 partial 状态回答并指出缺失证据。
```

现有“自由文本失败后切换精确工具”的规则增加前提：候选工具必须来自当前 Registry Snapshot 中
接受该引用类型的工具定义。提示词负责引导，执行校验负责兜底；两者都不维护工具 ID 白名单。

#### 9. 证据陈述边界

##### 9.1 不新增 `applies_to`

文档继续使用现有元数据：

```yaml
scope: event-driven
tags: [event, cookbook, architecture]
```

其中 DocStore 的 `kind` 是文档种类，`tags` 只参与召回，已有 `service` 字段在能够精确解析时继续
投影为 ontology 中的 `service -documented_by-> runbook`。这些字段都不被重新解释为一套封闭的
`system/domain/service` 适用范围枚举。

不增加 `applies_to` 的原因：

1. 平台、领域、子域、服务、租户和地区不是稳定的单层分类；封闭枚举会持续膨胀。
2. 一篇文档可以同时包含不同粒度的陈述，文档级标签无法准确限定每个段落。
3. 新字段会与原文主语、`service`、tags 和 ontology 关系形成多个 source of truth。
4. 当前错误可由“回答不得扩大原文陈述边界”普遍解决，无需为菜谱案例新增分类。

##### 9.2 综合规则

每个返回 chunk 必须保留文档标题、章节标题和原始 chunk 文本。最终综合遵守以下通用规则：

1. 结论中的主语不得比证据原文的主语更宽。
2. “全部、仅、共有、唯一”等量词必须由同一主语下的明确原文支持，不能从局部清单推导。
3. 已有 `service -documented_by-> runbook` 关系可以证明文档与服务的关联，但不能把服务事实提升为
   平台事实。
4. 未关联服务的文档保持其原文陈述边界；“没有 service 关联”不等于“适用于全平台”。
5. 多个来源描述不同主语时分别陈述，不因某个来源细节更多就覆盖另一个来源。
6. 用户问题要求全局结论但证据只明确覆盖局部主语时，证据状态为 `partial`，并指出缺失的全局
   证据。

这套规则只比较问题、结论和证据文本中的陈述边界，不识别特定领域名，也不新增持久化状态。

因此正确结论应为：

```text
平台共有七个网关；其中菜谱域主要涉及 mobile、AI 和 backstage 三个入口网关。
```

#### 10. 正确执行链

```text
用户：我们的架构是什么样的
  -> 预检索命中 runbook:flow-system-overview
  -> 当前片段没有完整网关清单
  -> search_runbooks(
       query="平台网关清单和接入层架构",
       doc_id="flow-system-overview",
       limit=3
     )
  -> 返回同一文档中多个独立 chunks
  -> 原文以整个平台为主语，明确列出七个网关
  -> 菜谱文档原文以菜谱域为主语，补充其三个入口
  -> 分别保留两个主语输出，不把局部清单改写成平台总量
```

不得再出现：

```text
search_code -> check_docs -> get_symbol -> trace_relations
```

这种连续换工具但没有获得同类型新证据的路径。

#### 11. 实施范围

##### 阶段 A：文档内分块检索，P0

- `search_runbooks` 增加 `doc_id`；
- DocStore 增加不读取正文的 `RunbookMetaByID`；
- 语义查询增加 `doc_id` filter；
- 全库按文档去重，文档内按 `chunk_index` 去重；
- 文档内检索读取 `limit+1` 条并准确设置 `truncated`；
- 输出改成文档加 `chunks` 的统一结构；
- 同步升级 `knowledge.Reader`、Feature Delivery、Dashboard 和 MCP 消费者；
- 补充结果上限、截断和后端失败信息。

建议提交：

```text
feat(runbook): support document-scoped chunk search
```

##### 阶段 B：工具类型边界，P0

- 引用类型改为受控常量；
- 为当前 Run 构建规范引用类型索引；
- 在 `Tool` 和 `ReadTool` 定义中增加 `ReferenceInputs`；
- Registry Snapshot 从工具声明派生引用类型到候选工具的反向索引；
- 工具执行前拒绝已知实体类型错配；
- 为所有带实体目标参数的内置工具补齐合同，并收紧相关描述和输入语义；
- 类型错配接入工具失败、Evidence Manifest 和前端状态；
- Agent 策略禁止跨实体类型盲目换工具。

建议提交：

```text
fix(agent): enforce evidence reference tool boundaries
```

##### 阶段 C：证据陈述边界，P1

- 不修改文档 frontmatter，不增加 `applies_to`；
- 检索结果继续完整保留 title、section header 和 chunk text；
- 最终综合提示词增加“不得扩大原文主语、范围和量词”；
- Evidence Manifest 保留来源引用与原始摘录，不新增适用范围状态；
- 复用已有 `service -documented_by-> runbook` 关系处理可确定的服务关联；
- 增加跨主语、局部清单和全局量词的通用回归测试。

建议提交：

```text
fix(agent): preserve evidence statement boundaries
```

#### 12. 测试

##### 12.1 检索单元测试

1. 全库搜索每篇文档只保留一个最佳分块。
2. 指定 `doc_id` 后返回同一文档的多个不同 `chunk_index`。
3. 文档内搜索不会混入其他 `doc_id`。
4. 选取按语义相关度完成，输出按 `chunk_index` 排序。
5. 不存在的 `doc_id` 返回 `runbook_not_found`。
6. 结果超过 `limit` 时设置 `truncated=true`。
7. 语义命中路径不执行无界 `RunbookMetas` 读取。

##### 12.2 工具边界测试

1. runbook 引用调用 `search_runbooks` 成功。
2. runbook 引用调用 `search_code`、`check_docs`、`get_symbol` 均被拒绝。
3. service 引用仍可调用 `check_docs`。
4. symbol 引用仍可调用 `get_symbol` 和 `trace_calls`。
5. 未识别的普通代码查询不被错误拒绝。
6. 类型错误增加 `tool_failure_count` 且不增加证据结果数。
7. 新注册工具只声明自身 `ReferenceInputs` 即可参与校验和候选提示，校验器无需新增工具 ID 分支。
8. Registry Snapshot 删除一个工具后，派生候选中不再出现该工具，不残留静态映射。
9. 扩展 `ReadTool` 与内置工具遵守相同合同，不按工具所有者写特殊分支。

##### 12.3 陈述边界回归测试

测试数据包含两个不同粒度主语的文档片段，不使用网关、菜谱或具体服务名称写专用判断。

1. 原文明示全局主语和完整数量时，可以形成对应的全局结论。
2. 局部主语下的清单不能被改写成全局总量。
3. 只有局部证据时，全局问题保持 `partial`。
4. 不同主语的事实能够在同一回答中分别表达。
5. 未设置 `service` 的文档不会被默认视为全局文档。
6. 不依赖新增 frontmatter 字段或文档 ID 命名规则即可通过测试。

##### 12.4 验证命令

```bash
go test ./internal/agent/ ./internal/retrieval/ ./internal/feature/delivery/
go build ./...
go vet ./...
```

涉及共享检索合同后，再执行：

```bash
go test ./...
```

#### 13. 验收标准

1. 日志中不再出现将已知 runbook ID 传给 `check_docs` 或 `get_symbol` 的成功调用。
2. `search_runbooks(doc_id=...)` 能返回同一文档最多 10 个独立分块。
3. 多个分块不合并到同一个 `chunkText`。
4. 文档内检索不会召回其他文档。
5. 类型错配在 Run 详情、证据清单和前端均显示为工具失败。
6. 新增工具只修改自身定义即可参与引用校验；校验器中不存在按工具 ID 维护的允许关系表。
7. 全局问题不会仅凭局部主语的文档形成全局完整结论。
8. 文档模型、frontmatter 和向量 payload 均未增加 `applies_to`。
9. “平台七个网关”和“菜谱域三个入口网关”能够同时正确表达。
10. 在线检索没有 fetch-all 后切片，也没有循环内线性成员查询。
11. 未新增功能重叠的工具、数据库表、领域分类枚举或持久化状态机。

#### 14. 数据与发布影响

- 阶段 A、B 不需要数据库迁移。
- 阶段 A 使用现有 `doc_id`、`chunk_index`、`section_header` payload。
- 统一响应结构会修改内部 Go 合同和 MCP 工具输出，所有仓库内消费者必须原子升级。
- 阶段 C 不修改文档数据结构或向量 payload，不需要重新嵌入知识文档。
- `ReferenceInputs` 是工具注册合同变更；内置工具与扩展 `ReadTool` 需要在同一版本完成声明。
- 已配置语义后端失败时保持可观察，不静默切换 Provider。

### 方法级调用链闭环评估

> Migrated from Nasuta `docs/design/nasuta-call-chain-closure.zh-CN.md`; incorporated into this module on 2026-07-31.

> 核验日期：2026-07-19
> 核验范围：Nasuta 当前代码、测试以及本地 `.codegraph/codegraph.db` 实际 schema。
> 目标：判断 agent 能否从一个方法或语义命中的代码块出发，拿到可靠的仓内 callers/callees、跨服务下游实现和跨服务上游入口。

#### 实施状态（2026-07-19）

本文 P0 和 Feign 闭环相关的 P1 项已经实现：

- `internal/callchain` 成为 Agent 与 Dashboard 共用的调用链应用服务，HTTP Handler 不再自行拼接调用链。
- `trace_calls` 支持 `file+line`、`query+file/qualified_name`、`direction=both`，并接受 `max_depth/max_nodes/max_fanout` 显式预算。
- CodeGraph adapter 返回完整 `CallHop`，保留重复调用点及 `line/col/confidence/provenance`，截断时返回 `truncated/nextFrontier`。
- 服务归属改为根据 SQLite 中 `repo+module_path` 做最长前缀解析；下游 route resolver 使用目标模块规范路径，不再使用 `%服务名%` 猜测文件路径。
- Feign 支持下游实现桥接和反向上游桥接，桥接后继续做有界方法遍历；Agent 与 Dashboard 使用同一结果。
- 服务边扫描新增 JVM/Python HTTP、gRPC、Dubbo，以及 Kafka producer-topic-consumer 关联。
- agent 策略明确区分方法 `calls`、已验证 `service_route` 和仅服务级依赖，并禁止把 `truncated/unresolved` 表述为完整链路。
- `trace_deps` 与 QA 依赖上下文已迁移到 Ontology Repository；旧内存 Dependency Graph 已删除。
- `trace_calls` 可先通过 `APIEndpoint implemented_by CodeSymbol` 将完整 API 路由解析为 CodeGraph 文件和行号入口。
- 新增 `NASUTA_CODEGRAPH_CONTRACT=1` 门控的外部 builder contract 测试，验证源码到 nodes、calls 和调用点行号；普通测试环境不依赖 CLI。

仍保留一个明确边界：gRPC、Kafka、Dubbo 当前形成服务级依赖边，但尚未形成协议到具体实现方法的符号桥。它属于本文 P1 后续增强，不影响 Feign 方法级双向闭环。

#### 1. 实施前结论（历史基线）

这些问题总体存在，但原评估有三处需要纠正：

1. **agent 方法级调用链仍未闭环。** `trace_calls` 仍是单向、3 跳、总节点最多 8、每个节点最多展开 4 个邻接，并且没有跨服务 route resolver。
2. **Dashboard 有跨服务下游桥接代码，但当前不能称为“接近闭环”。** CodeGraph 已强制使用 `repos/<group>/<project>/...` 规范路径，而 Dashboard 的 `serviceFromPath` 仍按旧目录格式取第一个路径段，实际会把服务解析成 `repos`，导致服务标注和跨服务判断失真。
3. **原文对数据链路有两处事实错误。**
   - 语义命中的 `code_chunk` 不会直接转换成 CodeGraph 节点继续遍历；预检索中的 CodeGraph 证据来自另一条独立 FTS 查询。
   - CodeGraph `edges` 表实际有 `line`、`col`、`provenance`；问题不是 builder 没有保存调用点，而是 Nasuta 的 `queryRelated` 和 `CallChain` 没有读取、返回这些字段。

服务级 `trace_deps` 的**存储和查询机制已经由本体闭环**，但扫描覆盖并不完整，因此不应表述为“所有协议的服务依赖完全闭环”。

#### 2. 实施前数据与执行边界

##### 2.1 服务级依赖图

服务级依赖由 Nasuta 扫描源码后投影为同代际的本体 Fact：

```text
源码扫描器
  -> domain.DependencyEdge
  -> Ontology Projector
  -> .nasuta/index.db ontology_facts
  -> ontology.Service / FindBoundedPaths
  -> trace_deps
```

这条链路不依赖外部 CodeGraph CLI。当前扫描覆盖为：

| 语言/平台 | 已实现依赖扫描 |
|---|---|
| Java | Feign |
| Kotlin | Feign |
| Go | 字面量 HTTP、gRPC client |
| C# | Refit、HttpClient |
| Node.js | axios/fetch 等 HTTP |
| Android | Retrofit/OkHttp |
| iOS | URLSession/Alamofire 等 HTTP |
| Python | 暂无依赖扫描器 |

`EdgeHTTP`、`EdgeGRPC`、`EdgeRPC` 已有模型；Kafka 没有独立边类型。Java/Kotlin 的 RestTemplate、WebClient、gRPC、Dubbo，以及各语言 Kafka producer/consumer 尚未形成完整服务边。

##### 2.2 方法级 CodeGraph

方法级数据由外部 `codegraph` CLI 构建，Nasuta 只读：

```text
codegraph CLI
  -> .codegraph/codegraph.db
  -> codegraph.Open(...?mode=ro)
  -> get_symbol / trace_calls / Dashboard CodeGraph API
```

查询时不依赖 CLI；只有重建索引时 `RebuildGraph -> runCodegraphIndex` 才调用 Docker 内 CLI 或本地 CLI。数据库不存在时 `Open` 返回 nil，工具降级为 `codegraph not indexed`。

当前 adapter 强制所有节点路径满足 `repos/%`。实际 `edges` schema 为：

```sql
edges(
    id,
    source,
    target,
    kind,
    metadata,
    line,
    col,
    provenance
)
```

本地库核验中，绝大多数 `calls` 边已有 `line`，说明调用点数据存在；但 `queryRelated` 只查询目标 Node 和 `metadata`，随后又用 `DISTINCT` 按节点折叠，调用点没有进入返回模型。

#### 3. 实施前三条路径

##### 3.1 agent `trace_calls`

`TraceCalls` 当前流程：

1. `SearchSymbols` 按关键词取候选。
2. 选择第一个非 field/import/file 等节点作为 target。
3. `GetRelatedChain` 按 callers 或 callees 单向 BFS。
4. 返回最多 3 跳、8 个节点，每个节点最多读取 4 个邻接。
5. 每个节点源码最多返回 1500 字符。

确认存在的问题：

- 不支持 `both`。
- 直接 callees 超过 4 个时已经丢失，不只是深层链路被截断。
- 总预算 8 会截断稠密调用树。
- 只遍历 `calls` 边，不执行 `RouteAt` 和 `ResolveDownstreamMethod`。
- query 无 `file`、`qualified_name` 等限定，重名方法可能选错。
- 返回节点，不返回每一跳的 call-site 行列。

##### 3.2 Dashboard `/api/codegraph/endpoint`

Dashboard 当前能力：

1. `GetCallChainByFile(file, line, 30)` 同时读取直接 callers 和 callees。
2. 用结构化依赖边的 Evidence 文件路径查目标服务。
3. 对有目标服务的 route 节点执行 `RouteAt + ResolveDownstreamMethod`。
4. 对下游实现递归展开，默认 depth 3、最大 depth 5，每层最多 20 个直接节点。

但当前有以下断点：

- `serviceFromPath("repos/aiot/speech-proxy/...")` 返回 `repos`，并非真实服务。
- 因 own service 和相邻节点常同时被解析成 `repos`，`crossService` 判断与低置信过滤不能按设计工作。
- `resolveTarget` 仍可能通过 Evidence 精确路径找到 Feign 目标，所以 route 桥接不是全部失效；但服务标签、跨服务过滤和非 Feign drop 结论不可信。
- 只递归 callees，不递归跨服务 callers。
- resolver 基于 HTTP method + path，尚无 gRPC、消息和 RPC 的符号桥。
- 这条能力仍只在 Dashboard HTTP handler 中，agent 工具不可达。
- 当前没有覆盖 `APICodeGraphEndpoint`、`serviceFromPath`、跨服务 enrich/expand 的单元测试。

##### 3.3 预检索中的 CodeGraph

原文“code_chunk 通过 `FindNodeAt` 进入方法遍历”的描述不准确。

当前预检索是两条并行证据路径：

```text
语义搜索 -> code_chunk

关键词 + 服务范围 -> CodeGraph FTS -> Node -> FindNodeAt -> 源码窗口
```

`FindNodeAt` 在 `codeGraphNode` 中用于把 **CodeGraph FTS 已命中的 Node** 再定位到最窄可调用符号，并不是把 semantic code hit 转成调用链起点。索引阶段确实会利用 CodeGraph 方法行号切分 code chunk，但检索阶段没有“code hit -> method node -> callers/callees”的闭环。

#### 4. 实施前问题清单

| # | 问题 | 核验状态 | 优先级 | 说明 |
|---|---|---|---|---|
| 1 | Dashboard 跨服务桥未暴露为 agent 工具 | **存在** | P0 | `builtinTools` 只有 `get_symbol`、`trace_calls` |
| 2 | `trace_calls` 单向且硬限制 3/8/4 | **存在** | P0 | 会丢直接分支和深层节点 |
| 3 | Dashboard 服务路径解析与 `repos/...` 不兼容 | **新增确认** | P0 | 当前 `serviceFromPath` 返回 `repos` |
| 4 | semantic code hit 不能直接进入方法调用链 | **存在，原文判断相反** | P0 | CodeGraph FTS 是独立召回路径 |
| 5 | 跨服务上游无 resolver | **存在** | P1 | 只有 downstream route resolver |
| 6 | 跨服务桥接仅覆盖 HTTP route 类路径 | **存在** | P1 | gRPC/Kafka/Dubbo 无符号级桥接 |
| 7 | Java/Kotlin/Python 等服务边覆盖不完整 | **存在** | P1 | 机制闭环不等于数据覆盖闭环 |
| 8 | 调用点行列未返回 | **存在，但原文根因错误** | P1 | DB 已存 line/col，adapter 丢弃 |
| 9 | 重名符号缺少显式消歧 | **存在** | P2 | 取排序后的首个 callable |
| 10 | prompt 无方法图与服务图的明确拼接协议 | **部分存在** | P2 | 有通用“补齐链路”要求，无具体拼接规则 |
| 11 | 外部 builder 覆盖缺少 contract 验证 | **存在** | P2 | 当前测试只验证 DB adapter 与路径规范 |

#### 5. 修复顺序

##### P0：先修正确性，再扩能力

1. **统一服务归属解析。** 删除 Dashboard 的字符串猜测，基于结构化 SQLite 中的 `repo + module_path` 做最长路径前缀匹配；复用同一套规范路径不变量，并补单仓多模块测试。
2. **抽取共享调用链应用服务。** 不让 agent 调 Dashboard handler；将“节点定位、服务归属、route 桥接、递归预算、返回模型”放到独立业务服务，Dashboard 和 agent 共用。
3. **支持明确起点。** 新工具同时接受：
   - `file + line`：用于 semantic code hit 或 UI 精确起点；
   - `query + optional file/qualified_name`：用于符号搜索和消歧。
4. **支持 direction=both。** 一次返回 callers 和 callees，并分别标记是否截断。
5. **用显式预算替代“无限展开”。** `max_depth`、`max_nodes`、`max_fanout` 可配置且在响应中返回 `truncated`、`next frontier`。调用图必须有界，不能为了“全量”取消所有保护。

##### P1：补跨服务和调用点

1. 设计 `CallHop` 返回模型，至少包含 source、target、edge kind、call-site line/col、confidence 和 provenance。
2. 修改 `queryRelated` 读取 `edges.line/col/provenance`，避免把“同一 caller 在不同位置多次调用同一 callee”静默折叠成无位置节点。
3. 增加跨服务 upstream resolver；至少先闭合 Feign route 的反向查询。
4. 按现有扫描缺口补协议：
   - Java/Kotlin RestTemplate/WebClient、gRPC、Dubbo；
   - Python HTTP/gRPC；
   - Kafka producer/topic/consumer 模型及独立边类型。
5. 协议桥接与服务边抽取分层：边存在不代表可以定位到下游实现方法，两层分别测试。

##### P2：消歧和外部契约

1. `get_symbol`/`trace_calls` 多候选时返回候选及稳定标识，接受 file、kind、qualified name 限定。
2. 在 agent 工具策略中说明方法图与服务图的拼接条件，禁止把未解析的跨服务 hop 当成已验证调用。
3. 增加固定 fixture 的 CodeGraph contract 测试：源码 -> 执行 builder -> 断言 nodes、calls、route、line/col。外部 CLI 不可用时跳过集成测试，但 adapter 单测必须常规执行。

#### 6. 扫描器扩展边界

原方案提出直接全面引入 tree-sitter，但这属于技术选型，不能从当前缺口直接推导为唯一方案。建议按证据复杂度选择：

- 声明式、局部语法足够稳定的模式（Feign、Dubbo、部分 gRPC stub）可以继续使用轻量 scanner，但必须有跨行、别名和多模块 fixture。
- 需要变量绑定、类型追踪或 Bean 来源分析的模式（例如 `@LoadBalanced RestTemplate`）应使用 AST/符号解析，不能靠调用行正则猜测。
- 若引入 tree-sitter，应先以 Java/Kotlin 一个协议做纵向切片，并证明相对现有 scanner 提升了召回和准确率，再扩语言矩阵。
- unresolved target 可以保留为外部逻辑目标，但现有 `DependencyTargetKind` 只有 service/external；增加新状态前必须先定义持久化、查询和晋升规则，不能只加枚举。

#### 7. 验收标准

方法级调用链闭环至少需要以下自动化验收：

1. 同一方法直接调用 6 个不同方法时，默认预算不会静默只返回 4 个；若截断，响应明确给出截断信息。
2. 同一 caller 在不同代码行调用同一 callee 两次，返回两个 call-site。
3. `repos/<group>/<project>/...` 和单仓多模块路径能稳定解析到正确 ServiceRecord。
4. 从 `code_chunk(file,start_line)` 可以显式选择对应 CodeGraph 方法并继续查 both 方向。
5. Feign caller -> client method -> route -> 下游 controller method 能在 agent 和 Dashboard 得到一致结果。
6. 下游服务方法反查跨服务 caller 有明确结果或明确“未解析”，不能伪装成完整链路。
7. 重名方法必须通过 file/qualified name 消歧，不能静默选错。
8. CodeGraph DB 缺失、builder 不可用或某语言无覆盖时均返回可观察的能力缺口。

#### 8. 关键代码索引

| 关注点 | 当前实现 |
|---|---|
| agent 工具注册 | `internal/agent/registry.go`：`get_symbol`、`trace_calls`、`trace_deps` |
| agent 方法调用链 | `internal/agent/tools.go`：`GetSymbol`、`TraceCalls` |
| CodeGraph 只读 adapter | `internal/platform/store/codegraph/db.go` |
| Dashboard 方法链 | `internal/transport/dashboard/tools.go`：`APICodeGraphEndpoint`、`expandDownstream`、`serviceFromPath` |
| 预检索 CodeGraph | `internal/retrieval/collection.go`：`collectCodeGraph`；`internal/retrieval/pipeline.go`：`codeGraphQuery`、`codeGraphNode` |
| 服务依赖扫描 | `internal/indexing/indexer/bootstrap.go`：`ScanCode` |
| 服务依赖查询 | `internal/ontology/service.go`：`TraceDependencies` |
| CodeGraph 外部构建 | `internal/indexing/service.go`：`RebuildGraph`、`runCodegraphIndex` |
