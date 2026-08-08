# 本体（Ontology）使用场景与链路评审

> 状态：评审草稿
> 创建日期：2026-08-02
> 范围：本体构建、存储、查询链路及其在 Agent / 检索 / 文档生成中的消费
> 相关提案：`05-index-ontology-and-storage.zh-CN.md`（本体化重构设计）

## 0. 文档生命周期

本文是对现有本体实现（`internal/ontology`）的现状梳理与评审，不直接修改正式架构文档。它记录当前本体的使用场景、数据链路、实际作用与不足，作为后续补齐本体能力的输入。

结论先给：本体作为**确定性依赖图投影**的地基是扎实的，核心能力（稳定身份、Schema 校验、原子快照、有界遍历、单一事实来源）均已落地。当前真实差距不在骨架，而在**抽取覆盖率、运行观测和人工纠错入口**三个面，且这三面均被 `05-index-ontology-and-storage.zh-CN.md` 列为已知边界或待补项，不是被意外遗漏的缺陷。

## 1. 本体是什么、在哪里

本体是 Nasuta 内部一个显式、可验证、可查询的工作区知识图，代码在 `internal/ontology`。它把原本分散在索引器、SQLite、内存图和 Agent 工具里的「隐式领域语义」提升为单一的事实模型。它**不是**知识库内容（那些在 CodeLoom），也不是 CodeGraph 的符号调用图。

设计蓝图在 `docs/design/agent-platform/05-index-ontology-and-storage.zh-CN.md`，核心原则是「**投影，而非主存储**」：本体是结构快照的可重建投影，SQLite 结构库才是权威记录。

### 1.1 模型规模

| 维度 | 内容 | 位置 |
|---|---|---|
| 实体 | `repository / service / api_endpoint / code_symbol / external_system / runbook` | `internal/ontology/model.go:5-12` |
| 关系 | `contains / exposes / implemented_by / depends_on / documented_by` | `internal/ontology/model.go:14-22` |
| 关系 Schema | subject/object 类 + qualifier 白名单，`implemented_by: APIEndpoint -> CodeSymbol` | `internal/ontology/schema.go:23-46` |
| 稳定身份 | 确定性种子哈希（service 直接用 `ServiceKey`） | `internal/ontology/identity.go` |
| 证据 | 每条 Fact 带 `confidence` + 可回溯 `Evidence`（文件/行/符号） | `internal/ontology/model.go:41-56` |

### 1.2 边界

- 本体不重新解析 Java/Python，只消费已规范化的 `domain.IndexBundle`。
- 本体不拥有方法级调用关系，那是 CodeGraph + `internal/callchain` 的职责。
- 本体不做向量相似召回，那是 Semantic Store 的职责。

## 2. 完整链路

```text
索引侧（构建）
  indexer.BuildBundle（扫描 Java/Python/runbook frontmatter）
    -> domain.IndexBundle（规范化结构记录）
    -> ontology.Project(bundle)          确定性投影：生成实体+事实+证据    internal/ontology/projector.go:10
    -> builder 按稳定 ID 去重合并、合并 Evidence、取最高 Confidence        internal/ontology/builder.go
    -> ValidateSnapshot                  Schema 校验（类白名单/引用完整性/ID 一致性） internal/ontology/validate.go
    -> ReplaceWorkspace                  temp SQLite + 校验 + 原子 rename 发布  internal/platform/store/sqlite.go
    -> 返回 Generation                   对全部规范化内容做确定性摘要

运行时（查询）
  agent/knowledge API -> ontology.Service（统一解析+遍历，ErrStaleSnapshot 自动重试一次）
    -> ontology.Repository（只读契约）    internal/ontology/repository.go:88
    -> SQLite Adapter（ontologystore/sqlite）
    -> 实体解析：ID / canonical key / alias / name 前缀四段优先级；歧义通过 Candidates 返回，由调用方消歧
    -> 有界路径遍历 FindBoundedPaths：BFS，MaxDepth<=5 / Node<=500 / Fanout<=100，
       每层批量 Neighbors，limit+1 判 truncated                            internal/ontology/path.go:9
```

## 3. 使用场景与消费方

| 场景 | 调用方 | 走了什么 |
|---|---|---|
| Agent 依赖工具 `trace_deps` | `internal/agent/registry.go:75-90` → `tools.go:702` | 查一个服务，上/下游各跑一遍 `depends_on` 遍历，转成证据型 `DependencyTrace`（含 `Truncated`） |
| Agent 跨类型关系工具 `query_relations` | `internal/agent/registry.go:310` | `service -> api -> symbol -> runbook` 这类跨类型多跳查询，用 `implemented_by / exposes / documented_by` |
| API 定位辅助 | `internal/agent/tools.go:1249` | `resolveAPICallTarget` 用 `QueryRelations(implemented_by)` 把 controller 名解析成 API 入口 |
| QA 依赖证据 | `internal/retrieval/collection.go:219` `collectDeps` | 对每个候选服务调 `TraceDeps`，去重+预算后拼进模型上下文 |
| 离线文档/项目生成 | `internal/feature/delivery/generation.go:214` | 对每个服务并发 `TraceDependencies`，把依赖边作为证据源 |

### 3.1 本体是 QA 依赖证据的唯一事实来源

QA 检索阶段的 `collectDeps` 直接调用 Agent 服务的 `TraceDeps`（`internal/retrieval/collection.go:219`），后者内部就是 `ontology.Service.TraceDependencies`。即 QA 上下文里的依赖链、`trace_deps` 工具、feature delivery 全部读取同一 `ontology.Repository`，不再维护第二张内存依赖图。这与设计文档「依赖查询、QA 上下文和 `trace_deps` 共用同一 Ontology Repository」的验收标准一致。

### 3.2 已有的人工知识入口

Runbook frontmatter 是当前唯一的人工受控来源，可声明 `service / depends_on / called_by`，解析后生成 `documented_by` 与反向 `depends_on` 边（`internal/indexing/indexer/docs.go:101-125`）。它已部分兑现设计文档「AST/结构扫描 > 明确配置和代码注解 > 人工维护的受控 Frontmatter」的优先级金字塔第二层。

## 4. 本体起到的作用

1. **单一权威依赖事实**：QA、`trace_deps`、文档生成共享一个只读 Repository，无第二张依赖图。
2. **Schema 强制防错**：`ValidateSnapshot` 从生成到发布前强校验，`Runbook exposes APIEndpoint` 这类语义错误直接被拒，不靠扫描器自律。
3. **稳定身份幂等**：重复索引不变 ID，证据合并、confidence 取最大，支持无脑全量重建。
4. **原子快照 + generation 门控**：临时库写+校验+rename，读者只看到完整旧或完整新快照；跨版本读取返回 `ErrStaleSnapshot`，Service 自动重试一次，禁止拼接两个代际的数据。
5. **查询天然有界**：`Resolve<=20 / Neighbors<=200 / Path depth<=5` 在契约层校验，避免 QA 上下文爆炸。

## 5. 不足与风险

### 5.1 符号桥存在但覆盖窄：只到 endpoint handler

`implemented_by` 确实是已实现的边：Schema 定义 `APIEndpoint implemented_by CodeSymbol`（`schema.go:32`），投影为每个带 handler 的 endpoint 创建 symbol 并落事实（`projector.go:36-43`），Agent 已用它把 controller 名解析成调用入口（`tools.go:1253`），设计文档亦明确规定此边（`05-…md:919`）。**准确缺口是**：符号桥只覆盖 endpoint handler，不覆盖任意代码符号（函数、类、字段），本体层也没有 `Symbol calls Symbol` 关系——完整调用链仍由 CodeGraph 单独拥有，本体符号层与 CodeGraph 调用图之间没有桥。因此「从 API 到设备执行的完整链路」的符号级后半程在本体上不可见，但这属于第一版「符号只建引用实体、调用关系留给 CodeGraph」的既定边界（`05-…md:919-928`）。

### 5.2 投影来源单一，人工纠错/覆盖不足

- 已存在：Runbook frontmatter 能声明 `service / depends_on / called_by`（`docs.go:101`），并生成反向依赖边。
- 缺少的是：**独立于文档的人工纠错/覆盖来源**（例如手工维护的依赖清单、纠错记录），以及**多来源冲突时的明确优先级**。当前多来源合并在 `builder.go` 里表现为「同 ID 取最高 confidence、合并证据」，没有面向冲突语义的分级规则。
- 重要边界：人工纠错也必须作为**权威输入进入索引/投影管线**，而不是直接修改派生的 ontology snapshot——否则会被下次全量重建冲掉。这与设计文档「可重建派生索引」的定位一致。

### 5.3 事件/消息实体缺失是既定排除，不是遗漏

缺少 `MessageTopic / Database` 等实体确实存在，但它们本就是第一版明确排除的内容（`05-…md:667-680`）。当前服务级依赖已覆盖三种来源：结构性扫描（Feign/HTTP/gRPC）、Runbook frontmatter、以及 **Kafka producer/consumer 按 topic 匹配生成的依赖**（`protocol_dependencies.go:69`）。「跨服务调用链不完整」若要归因，应归到「消费侧能否用现有事实拼出链路」而非「实体种类不够」；这一点与更早 QA 评审里的 `collectDeps` 缺陷**不是同一根因**——那个问题（找到第一个有边的服务就 return）已在 `collection.go:202` 修复为遍历全部候选服务并共享预算去重。

### 5.4 孤立 Runbook 是有意行为，但缺失统计与日志

无法解析 `service` 的 Runbook 被保留为孤立实体、不生成 `documented_by`，是有意设计——避免制造虚假 Service（`05-…md:917-918`，测试 `projector_test.go:53`）。**真实缺口**：设计文档要求 unresolved Runbook「进入构建统计和日志」（`05-…md:917`），但当前投影只是直接 `continue`（`projector.go:61-64`），既不计数也不记录。`repository` 参与 `contains`、`code_symbol` 参与 `implemented_by`，不存在「空类/空谓词」；本节的缺口是 unresolved 计数，不是 Schema 层缺约束。

### 5.5 查询能力窄，且歧义处理靠调用方

只有 resolve / neighbors / 路径遍历三类；`FindBoundedPaths` 的 `TargetID` 目标可达查询从未被上层调用；无聚合（如「哪些服务依赖外部系统 X」）、无相似解析。解析歧义**没有**被静默丢弃——`queryResolvedRelations` 把多个候选放进 `Candidates` 返回（`ontology/service.go:89-94`），由调用方决定如何处理，只是当前没有上层真正利用该消歧结果。

### 5.6 观测与运维不足（成立）

`Stats`（`internal/ontology/repository.go:74`）有 `ByClass / ByPredicate` 数量统计，投影成功后也记录了 generation、entity、fact 数量（`indexing/service.go:1130`）。但缺少：**unresolved Runbook 数量**（见 5.4）、**stale 重试计数**、**路径截断率**、**消歧频次**、**投影耗时/规模**。这些正是判断「本体抽取质量够不够」最需要的信号。

### 5.7 Neo4j 是路线图差距，不是当前缺陷

当前 dispatcher 对 `neo4j` 明确返回 "not available in this build"（`provider.go:20-24`），不存在运行到不完整后端、也无静默回退 SQLite 的情况。准确表述是：**配置与正式设计保留了 Neo4j 扩展承诺，但适配器尚未交付**。这属于路线图一致性问题，处理方式二选一：明确标记为 `planned` 并列出交付条件，或在实现前收缩配置暴露面。

## 6. 结论

本体的设计骨架（稳定身份 / Schema 校验 / 原子快照 / 有界遍历 / 单一 Provider）执行得很扎实，符号桥（`implemented_by`）、人工入口（frontmatter）、协议依赖（Kafka 等）均已落地。它作为**确定性依赖图投影**是可靠的权威事实层。当前不足集中在三个可度量、可逐步改进的面：

1. **抽取覆盖率**：符号桥只到 endpoint handler，事件/消息/数据库实体是第一版既定排除；
2. **运行观测**：缺 unresolved、stale、截断、消歧、投影耗时等聚合指标；
3. **人工纠错**：缺独立纠错来源与多来源冲突优先级。

这三面都不动摇「本体是唯一权威依赖事实层」的结论，也不构成对旧 QA 缺陷的因果归因。

## 7. 后续改进方向（候选）

1. 补 unresolved Runbook 数量进投影统计与日志（兑现 `05-…md:917` 的明确要求）。
2. 给本体增加覆盖率与截断观测：unresolved / stale retry / 截断率 / 消歧频次 / 投影耗时。
3. 增加独立的人工纠错/覆盖来源，并定义多来源冲突优先级；纠错作为权威输入进入投影管线，不直接改派生快照。
4. 在符号层与 CodeGraph 调用图之间建桥（例如「服务的入口符号集合」），把完整调用链在本体侧连通——按需求驱动，不违背第一版边界。
5. 明确 Neo4j 的路线图状态：标记 `planned` 并列出交付条件，或在实现前收缩配置暴露面。

## 8. 非目标

- 不引入 OWL / RDF / SPARQL 或通用图推理引擎。
- 不让 LLM 直接写入正式本体。
- 不把向量相似结果当作本体事实。
- 不用本体替换 CodeGraph 的完整符号调用图。
