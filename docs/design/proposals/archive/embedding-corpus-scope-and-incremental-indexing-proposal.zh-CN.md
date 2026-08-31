# Embedding 语料范围与增量索引治理提案

状态：已实施并归档
作者：Nasuta / CodeLoom
日期：2026-08-19
关联事项：workspace embedding corpus 分析；当前无关联 Issue、Trace 或 PR
目标版本：待评审后确定

## 1. 摘要

本提案用于解决 Nasuta workspace 语义索引范围过宽、更新时重复 embedding，以及多来源内容重复占用 retrieval 候选池的问题。

当前 workspace 语义索引由三类 corpus 构成：

1. `code_chunk`：多语言源码，以及部分 SQL、配置、接口和仓库文档；
2. `runbook`：`flow`、`schema`、`module`、`document` 文档；
3. `service`：service name 与可选 summary。

对当前 workspace 使用真实 scanner、过滤规则、codegraph range 和 chunker 进行只读统计后，得到以下基线：

- 发现 180 个扫描目录；
- 42,866 个候选文件；
- 38,095 个实际 eligible 文件；
- 177 个仓库至少产生一个有效 chunk；
- 约 856 万行文本；
- 产生 305,628 个 `code_chunk`；
- 平均每个 eligible 文件约 8.02 个 chunk。

按 chunk 数看，C、Java、Kotlin、Objective-C、Python、Swift、C++、TypeScript 等代码和接口语言约占 97.7%。XML、Markdown、YAML、Shell、SQL 等内容合计只占约 2.3%。因此，单纯删除 Markdown、YAML 或 XML 无法解决主要 embedding 成本。

当前更值得优先处理的问题是：

1. 仓库发生任意提交变化后，整个仓库代码会重新 embedding；
2. repo Markdown、generated module docs 与 DocStore 文档可能重复；
3. code scanner 对 vendor、third-party 和部分生成内容的边界仍偏宽；
4. 所有内部查询默认并行召回 code、runbook 和 service，低价值向量会占用候选位；
5. method-aware chunking 只保留 method/function 范围，文件级上下文存在缺口。

当前流程为：

```text
workspace 扫描
→ 按扩展名、目录和文件大小过滤
→ 按 codegraph method/function 或固定行窗口切 chunk
→ 整个 workspace/repo 批量 embedding
→ code、runbook、service 三路并发召回
→ file dedup、rerank 和 context budget 截断
```

本提案计划调整为：

```text
workspace 扫描
→ 生成带 policy/chunker/model 版本的文件状态
→ 未变化文件复用，变化文件按 chunk hash 复用
→ 按 evidence class 和 canonical source 治理语料
→ 根据 query kind 选择 retrieval source 和预算
→ 记录成本、复用、排除、重复和质量指标
```

本提案建议采用以下顺序：

1. 建立按语料类型和 query slice 的质量、成本基线；
2. 实现文件级增量 embedding，再演进到 chunk 级复用；
3. 治理 vendor、generated、重复文档和低价值配置；
4. 收窄低信息量 service vectors；
5. 按 query kind 选择检索来源和预算；
6. 单独修复 method chunk 的文件级上下文缺口。

核心决策是：

> 不以“只保留源码”作为目标。优先消除重复计算、重复语料和低价值候选，在固定评估集证明不损害召回后再逐步收窄 corpus。

## 1.1 当前实施状态

本提案的首版代码已经落地，但尚未宣称通过完整验收门禁。当前实现覆盖：

- 代码 chunk state 持久化，按 model、dimension、policy 和 chunker 版本判断兼容性；
- repo 增量 embedding：兼容的 logical chunk 复用，变化或新增 chunk 才调用 provider，删除 stale code points；
- 缺失、不可信或版本不兼容的 state 触发 repo full rebuild；provider 返回数量不匹配或部分失败时拒绝发布完整结果；
- BM25 legacy/missing vocab 进入 migration required，禁止不安全的 repo-only 增量路径；
- 默认排除 `vendor`、`third_party` 和 `thirdparty` 路径；保留 SQL、YAML、XML 等仍有检索价值的内容；
- method/function chunk 补充受控的 file-context chunk，避免丢失 package、import、type 和 field 等上下文；
- 无 summary 的 service 不再生成 dense vector，bootstrap/reindex 会清理旧 service vectors；
- 同一批文档输入按 canonical content hash 去重，并按 `schema > flow > module > document` 选择主要来源；
- `QueryCodeReview` 跳过 runbook retrieval，并在 trace 中记录 skipped 状态。

仍待完成或需要灰度验证的内容：

- state 文件与 semantic backend 尚未形成严格原子 snapshot；state 存在不等于 backend point 一定存在，批次后段失败时已 upsert 的 changed points 也可能先于 state 发布而短暂可见；
- 文件移动或行号变化时，当前首版不会仅凭 content hash 从 semantic backend 读取旧向量并复用；
- 文档去重目前限于同一批 embedding inputs，尚未对不同 indexing 入口做跨 backend 查询式去重；
- 除 code review 外，其他 query kind 仍保留 code、runbook、service 三路检索，source-aware retrieval 只完成第一处收窄；
- 完整指标、incremental/full rebuild 等价性、固定 query slices 和 Recall@10 门禁仍需补齐。

本节是当前实现的边界说明；第 5 至第 8 节中的目标流程、伪代码和验收标准仍是后续灰度与完善的依据。

主要落地位置：

| 能力 | 实现位置 |
| --- | --- |
| code state、兼容性与 content hash | `internal/indexing/code_index_state.go` |
| repo chunk 增量 embedding 与 stale cleanup | `internal/indexing/code_embedding.go` |
| provider 批次完整性检查 | `internal/indexing/embedding_runtime.go` |
| corpus path policy 与 file-context chunk | `internal/indexing/indexer/code.go` |
| 文档 exact dedup 与 canonical scope | `internal/indexing/indexer/embeddocs.go` |
| summaryless service vector gate | `internal/indexing/service_embedding.go` |
| BM25 append-only vocab observation | `internal/retrieval/sparse.go` |
| code review retrieval source gate | `internal/retrieval/pipeline.go` |

截至 2026-08-19，以下离线验证已通过：

```bash
GOWORK=off go test ./...
GOWORK=off go test -race -count=1 ./internal/indexing/... ./internal/retrieval/...
GOWORK=off go build ./...
GOWORK=off go vet ./...
git diff --check
```

上述验证不包含 live semantic backend、付费 embedding provider 或生产 workspace 灰度。

## 2. 背景

Nasuta 为 CodeLoom 提供 workspace 索引、语义存储和 retrieval 能力。CodeLoom 当前通过本地 module replacement 使用 Nasuta，workspace 同时包含服务端、移动端、固件和配套文档仓库。

系统的产品目标不是单一语言代码搜索，而是：

- 跨仓库定位代码实现；
- 查询 SQL、schema 和配置；
- 理解服务职责、依赖和业务流程；
- 为 QA、故障分析和代码审查提供可引用证据。

因此，embedding 范围天然会大于普通的“源码向量库”。问题不在于是否存在非源码内容，而在于每类内容是否有明确的检索价值、所有权、可信度和更新策略。

### 2.1 业务与技术背景

当前相关链路为：

```text
workspace / 文档 / codegraph
→ indexer 扫描、过滤和切 chunk
→ embedding provider + Qdrant / BM25
→ retrieval pipeline 合并、去重和 rerank
→ QA、MCP 工具和 dashboard 输出证据
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/indexing/indexer` | 扫描、过滤、文档分段和代码 chunk | workspace、codegraph ranges、文档 | 候选 chunks |
| `internal/indexing` | embedding、向量写入、索引协调 | chunks、model、policy | dense vectors、BM25 状态 |
| `internal/retrieval` | 按 query plan 召回、去重、rerank | query、各类 vectors | evidence candidates |
| `internal/domain/evidence.go` | 统一 evidence class 和 trust 语义 | 来源元数据 | 可信度分类 |
| DocStore / structured store | 保存 curated 文档、service 和结构化事实 | 文档、服务、ontology | 可精确查询的数据 |

### 2.2 当前 corpus

#### 2.2.1 `code_chunk`

入口：

- `internal/indexing/code_embedding.go`
- `internal/indexing/indexer/code.go`

当前 dense embedding 支持的内容包括：

- JVM：Java、Kotlin、Scala、Groovy；
- Python、Go、Rust；
- TypeScript、JavaScript、Vue；
- Swift、Objective-C、Dart；
- C、C++、C#；
- Ruby、PHP、Lua、R、Perl；
- SQL；
- YAML、XML；
- Markdown、Shell、Proto；
- 以及扩展表中定义但被 noise rule 排除的其他格式。

以下扩展名虽然被语言表识别，但当前不会 embedding：

- `.properties`
- `.json`
- `.http`
- `.html`
- `.toml`
- `.vm`
- `.ftl`

以下目录由公共文件 walker 排除：

- `target`
- `.git`
- `node_modules`
- `.venv`、`venv`
- `dist`
- `build`
- `.idea`
- `.claude`
- `.codex`
- `.nasuta`
- `__pycache__`
- `bin`、`obj`
- `.dart_tool`
- `DerivedData`、`.build`
- `.gradle`
- `cmake-build-debug`、`cmake-build-release`

embedding 层还会排除：

- `src/test`、`test`、`tests`、`__tests__`、`testdata`
- fixture、mock、e2e、snapshot 目录
- 典型 generated/minified 文件名
- 超过 256 KiB 的文件
- `.env`、私钥和证书等敏感文件

#### 2.2.2 代码 chunk 规则

存在 codegraph method/function range 时：

- 每个 method/function 形成一个 chunk；
- 超过 200 行的方法再切成 80 行窗口；
- 相邻窗口重叠 15 行；
- 每个 chunk 增加 language、kind、qualified name、signature 和路径行号 header。

没有有效 method/function range 时：

- 整个文件按 80 行窗口切分；
- 相邻窗口重叠 15 行。

进入 embedding runtime 前，chunk 文本最多保留 8,000 bytes。

BM25 sparse corpus 比 dense corpus 更窄，只覆盖源码、SQL、Shell 和 Proto。YAML、XML、Markdown 等当前为 dense-only。

#### 2.2.3 `runbook`

入口：

- `internal/indexing/service_embedding.go`
- `internal/indexing/document_index.go`
- `internal/indexing/indexer/embeddocs.go`
- `internal/indexing/indexer/chunkmd.go`

所有文档向量统一使用 `kind=runbook`，通过 `scope` 区分：

| scope | 来源 | 当前 evidence class | 当前 trust |
|---|---|---|---:|
| `flow` | curated knowledge document | `curated_runbook` | 85 |
| `schema` | curated knowledge document | `curated_schema` | 90 |
| `module` | generated module document | `generated_doc` | 75 |
| `document` | user/generated document | `user_document` | 60 |

Bootstrap 读取 `flow` 和 `schema`。`module` 和 `document` 由文档生成、上传和同步链路另行 embedding。

Markdown chunk 默认规则：

- 按 `##` 至 `####` 标题分段；
- 每块最大 5,000 characters；
- 小于 200 characters 的尾块与前块合并；
- hard split 使用 500 characters overlap；
- 每个 chunk 重复 document title 和 section header。

#### 2.2.4 `service`

每个 service 当前形成一个向量，文本为：

```text
ServiceName
（可选 Summary）
```

payload 还包含 service name、layer、owner、evidence class 和 trust tier。

service metadata 同时存在于结构化存储中，检索端会先进行名称和字段的 lexical scoring，再合并 dense 结果。因此，service dense vector 是补充召回，不是 service 精确识别的唯一来源。

#### 2.2.5 不属于当前 corpus embedding 的结构化内容

以下内容不进入上述三类向量：

- endpoints；
- dependencies；
- ontology entities 和 facts；
- codegraph symbol nodes；
- call graph 和 dependency graph edges。

这些内容分别由 SQLite、ontology 和 codegraph 提供结构化检索。

### 2.3 当前实测规模

#### 2.3.1 测量口径

统计直接复用当前实现中的：

- `DiscoverScanDirs`
- `walkFiles`
- `IsIndexableFile`
- noise rules
- 256 KiB 文件上限
- codegraph method/function ranges
- `chunkByNodes`
- `chunkFile`

统计没有连接 MySQL，因此未应用平台设置中的动态 `VCSExcludeProjects`。生产实际值可能低于本文基线。

#### 2.3.2 总体数据

| 指标 | 数值 |
|---|---:|
| workspace 总体积 | 约 6.1 GB |
| workspace 文件数 | 约 72,710 |
| discovered scan dirs | 180 |
| indexable candidate files | 42,866 |
| eligible files | 38,095 |
| 有有效 chunk 的 repos | 177 |
| eligible bytes | 305,931,404 |
| eligible lines | 8,561,417 |
| code chunks | 305,628 |
| codegraph node-backed files | 21,844 |
| fallback window files | 16,251 |

#### 2.3.3 按语言分布

| 语言 | 文件数 | chunks |
|---|---:|---:|
| C | 20,014 | 160,897 |
| Java | 8,571 | 90,428 |
| Kotlin | 1,301 | 13,964 |
| Objective-C | 833 | 11,306 |
| Python | 950 | 6,184 |
| Swift | 773 | 5,619 |
| C++ | 337 | 4,718 |
| XML | 2,081 | 2,995 |
| TypeScript | 375 | 2,984 |
| Markdown | 1,119 | 2,541 |
| JavaScript | 779 | 1,889 |
| YAML | 491 | 640 |
| Vue | 105 | 517 |
| Shell | 229 | 456 |
| Perl | 44 | 201 |
| SQL | 66 | 143 |
| Rust | 10 | 72 |
| C# | 9 | 66 |
| Proto | 8 | 8 |

#### 2.3.4 已排除内容

本次统计中，embedding 层额外排除了：

| 原因 | 文件数 |
|---|---:|
| empty/unreadable | 146 |
| test 类目录 | 1,830 |
| `.json` | 1,267 |
| `.html` | 981 |
| `.properties` | 210 |
| `.http` | 26 |
| `.ftl`、`.vm`、`.toml` | 28 |
| generated/minified name pattern | 23 |
| 超过 256 KiB | 232 |

#### 2.3.5 规模判断

从 chunk 数看：

- C 和 Java 合计约占 82.2%；
- 前七种主要代码语言约占 95.9%；
- XML、Markdown、YAML、Shell、SQL、Proto 等合计约占 2.3%。

所以：

1. 删除全部 repo Markdown，最多只减少约 0.8% 的 code chunks；
2. 删除全部 XML 和 YAML，最多再减少约 1.2%；
3. 真正的大头是源码数量、chunk 数量和仓库更新时的重复 embedding；
4. 非源码治理更主要的收益是提升候选质量，而不是大幅降低全量向量数。

### 2.4 当前检索如何消费 corpus

#### 2.4.1 三路并发召回

QA 内部 retrieval 对同一个查询向量并发搜索：

- `kind=code_chunk`
- `kind=runbook`
- `kind=service`

默认预算：

| query kind | code | runbook | service | rerank |
|---|---:|---:|---:|---:|
| focused fact / code review | 12 | 8 | 6 | 20 |
| runtime / inventory / comparison | 16 | 12 | 8 | 24 |
| flow | 16 | 8 | 6 | 24 |
| overview | 16 | 16 | 8 | 24 |

当前 query kind 主要改变预算，并不会关闭其中某一路。

#### 2.4.2 候选处理

code search：

- dense 或 dense + BM25 hybrid；
- semantic backend 最多抓取 `max(limit*4, 40)`，上限 64；
- 以 `path` 分组和去重；
- 每个文件最多保留一个候选进入后续 ranking。

runbook：

- 每个文档最多合并三个 chunk；
- 在线正文有独立截断；
- 最终与 code candidates 进入同一个 rerank/code pool。

因此，低价值或重复向量不只是增加离线存储，也可能在 backend fetch limit 内挤占更有价值的候选。

#### 2.4.3 可信度

当前主要 trust tier：

| 内容 | trust |
|---|---:|
| runtime code | 100 |
| curated schema | 90 |
| curated runbook | 85 |
| generated doc | 75 |
| SQL / user document | 60 |
| config | 55 |
| service metadata | 45 |
| repo Markdown | 35 |

repo Markdown 已经被标记为低可信内容，但 trust adjustment 发生在召回之后。大量重复或泛化文档仍可能先进入有限候选集。

### 2.5 为什么现在需要修改

本次提案由 workspace 规模增长和 embedding corpus 分析触发，而不是由单一线上事故触发：

- 触发时间：2026-08-19，Asia/Shanghai；
- 触发标识：当前无关联 Issue、Trace 或 PR；
- 直接表现：只读统计显示约 38,095 个 eligible 文件、305,628 个 code chunks；method-aware 文件仍有约 34.4% 的文件行不在 method/function 范围内；
- 影响范围：workspace daily sync、full bootstrap、semantic store 存储和 QA retrieval；
- 临时处置：当前不改变线上行为，先形成可评审的 corpus、增量和质量治理提案。

### 2.6 范围

#### 目标

1. 只对新增或变化的高价值内容执行 embedding，并复用兼容的文件或 chunk 结果；
2. 通过 canonical ownership 和 path policy 减少重复、低价值和生成内容对候选池的干扰；
3. 保留 runtime code、SQL、核心配置、curated schema/runbook 等高价值检索能力；
4. 用固定 query slices、成本指标和回滚门禁验证收窄是否安全。

#### 非目标

1. 本提案不改变 ontology、codegraph、endpoint、dependency 和 structured store 的职责；
2. 本提案不把系统改成只支持 Java/Python，也不删除 SQL、配置和 runbook 检索；
3. 本提案不替换 semantic provider，不通过提高 token、chunk 或超时上限掩盖根因；
4. 本提案不针对单个 QA 问题加入关键词、路径或实体特例。

## 3. 问题

### 3.1 问题描述

**期望行为：**

系统只对有检索价值且实际发生变化的 workspace 内容执行 embedding；同一事实由一个主要语义来源负责；查询只召回与当前 query kind 有关的 corpus；任何复用、排除、降级和重建都可观测、可验证。

**实际行为：**

当前 corpus 覆盖范围较宽。仓库 HEAD 未变化时能够跳过，但只要 HEAD 变化，就会重新扫描并 embedding 整个仓库的 eligible chunks。相同或近似内容还可能通过 repo Markdown、generated module doc、DocStore 文档等多个入口进入候选池。检索端默认并发查询 code、runbook 和 service 三路，仅通过预算调节，未按 query kind 关闭低相关来源。

**差异：**

索引成本主要由“变化仓库的全量重算”决定，而不是由少量非源码扩展名决定；候选质量则受到重复来源、低信息向量和 source selection 粗粒度影响。当前实现缺少统一的 corpus 生命周期、canonical source 和 retrieval source 治理，无法稳定做到“只计算变化内容、只保留主要证据、只搜索相关来源”。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | embedding 请求量和同步耗时偏高，候选中存在重复或低信息内容 | 当前 workspace 产生 305,628 个 code chunks；一个小提交可触发整个仓库重算 |
| 直接原因 | 缺少 file/chunk hash 复用、跨入口 canonical source 去重和细粒度 corpus policy | `EmbedRepoCode` 以 repo generation 为更新边界；各文档入口独立生成 point |
| 机制根因 | 索引生命周期、语料所有权和 retrieval source 选择分散在不同链路，没有共享的版本契约和质量门禁 | indexing、DocStore 和 retrieval 分别处理局部行为，尚无统一 corpus policy/version |

根因链路：

```text
仓库少量变更或同一文档从多个入口进入
→ repo generation 只能判断仓库变更，不能判断文件和 chunk 复用
→ 各来源独立写入 semantic store，检索端默认搜索三类 corpus
→ 重复计算与重复候选未在边界处消除
→ embedding 成本、同步时延和候选位竞争增加
```

本问题不能只通过删除 Markdown、YAML、XML 或增大 chunk 解决。非源码内容只占 code chunks 的约 2.3%，删除整类文件会损失 SQL、配置和运行手册能力；增大 chunk 会降低代码定位精度，且不会解决仓库级重复 embedding。

#### 3.2.1 更新粒度过粗

Daily sync 会先根据 HEAD SHA 判断仓库是否变化。未变化仓库不会进入 embedding，这是正确的第一层门禁。

但只要仓库 HEAD 变化：

- `EmbedRepoCode` 会重新扫描整个仓库；
- 所有 eligible chunks 都重新调用 embedding；
- 写入新 generation 后再删除旧 generation；
- 没有按文件 hash 或 chunk hash 跳过未变化内容。

一个只修改少量文件的提交，可能重新 embedding 整个大型仓库。

#### 3.2.2 corpus 所有权重复

同一知识可能以不同入口进入向量库：

- repo 中的 `.md` 作为 `code_chunk`；
- DocStore 中的 `flow` 或 `schema` 作为 `runbook`；
- generated module docs 重新描述代码内容；
- user document 可能再次上传相同正文。

当前 point ID 在各自入口内部稳定，但没有跨入口的 canonical source/content hash 去重。

#### 3.2.3 非源码策略只按扩展名和少量路径判断

XML、YAML 和 Markdown 的业务价值高度依赖路径和文件角色：

- deployment/config/schema 通常有价值；
- CI 模板、license、changelog 和生成配置通常价值较低；
- README、architecture、ADR 与普通说明文档的检索价值不同。

当前规则尚未表达这种差异。

#### 3.2.4 service dense 信息量偏低

没有 summary 的 service 只 embedding service name。名称和结构化字段已经可以 lexical 匹配，这类向量提供的增量语义价值有限。

#### 3.2.5 method-aware chunking 存在上下文缺口

codegraph range 只查询 `method` 和 `function` nodes。有 nodes 时，chunker 不再对整文件做 fallback window。

本次统计中：

- node-backed 文件方法范围覆盖 3,921,717 行；
- 同一批文件还有 2,058,025 行不在 method/function 范围内；
- 未覆盖部分约占 node-backed 文件总行数的 34.4%。

这些行可能包含：

- package/import/include；
- class/interface/type 声明；
- 字段和常量；
- 文件顶部说明；
- 注解和模块级配置；
- top-level declarations。

因此，当前状态同时存在“corpus 很大”和“文件上下文不完整”两个问题。不能用进一步删除代码上下文来解决成本问题。

### 3.3 影响

- **用户影响：** 索引更新完成前可能检索到旧证据；重复候选和上下文缺口会降低答案定位精度与引用完整度；
- **业务影响：** workspace 规模继续增长时，daily sync 和 bootstrap 的 embedding 成本、完成时间及故障恢复时间同步增长；
- **系统影响：** 未变化 chunks 被重复发送给 provider，semantic store 和 backend fetch limit 被重复或低价值内容占用；
- **工程影响：** corpus 变化缺少统一指标，过滤策略无法用固定评估集证明，增量结果与 full rebuild 的一致性也无法直接验收。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：大型仓库的小提交

- **Given（前置条件）：** 一个已完成全量索引的大型仓库包含 10,000 个 eligible chunks，本次提交只修改 2 个文件、20 个 chunks；
- **When（触发行为）：** daily sync 发现仓库 HEAD SHA 变化并执行 `EmbedRepoCode`；
- **Then（期望结果）：** 只重新切分变化文件，复用未变化文件和内容未变化的 chunks，并删除失效 points；
- **But（当前结果）：** 整个仓库重新扫描并向 embedding provider 提交全部 eligible chunks。

复现步骤：

1. 对测试仓库执行一次 full bootstrap，记录 embedding batch 和 input token；
2. 只修改一个小文件并提交；
3. 再执行 repo sync；
4. 比较第二次运行的 embedding 输入量与仓库总 chunk 数；
5. 当前实现可观察到输入量更接近全仓，而不是变化文件规模。

#### 场景 B：文档从多个入口重复进入 corpus

- **Given：** repo 中存在一份 Markdown 架构说明，DocStore 同时保存相同正文的 curated flow，generated module doc 又包含其摘要；
- **When：** bootstrap、文档同步和 module generation 分别执行 embedding；
- **Then：** curated source 作为主要语义所有者，其他来源保留引用关系或摘要，不重复占用主要召回；
- **But：** 当前各入口独立生成 vectors，相同知识可能多次进入 backend fetch limit。

#### 场景 C：overview 或 diagnosis 查询默认搜索全部来源

- **Given：** query plan 已将问题识别为 service inventory、SQL/schema 或 runtime diagnosis；
- **When：** retrieval pipeline 构造语义查询；
- **Then：** 按 query kind 和 facet 选择主要来源，为次要来源设置低预算或关闭无关来源；
- **But：** 当前 code、runbook、service 三路默认并发，仅预算不同。

#### 场景 D：method-aware 文件缺少文件级上下文

- **Given：** codegraph 为一个文件返回 method/function ranges，文件同时包含 imports、type declarations、fields 和 constants；
- **When：** `chunkByNodes` 生成 chunks；
- **Then：** methods 与受控的 file context 都能进入检索；
- **But：** 当前有 nodes 时不再执行整文件 fallback，非 method/function 内容可能完全缺失。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 未变化仓库 | HEAD SHA 未变化 | 跳过 repo embedding | 保持现有行为，并记录 skip 指标 |
| 文件移动 | 正文相同、路径变化 | 新 generation 重建全仓 | 复用 dense embedding，更新 metadata，删除旧 point |
| 文件删除 | 旧 generation 中存在 point | generation 切换后统一删除 | 增量 reconciliation 明确删除 stale point |
| model/chunker/policy 升级 | 版本与旧状态不兼容 | 缺少统一复用契约 | 禁止静默复用，触发明确范围的重建 |
| provider 失败 | embedding backend 返回错误 | 当前 run 失败 | 保持 provider 错误可见，不替换 provider，不发布不完整 generation |
| BM25 发布失败 | dense 写入后 sparse state 未持久化 | 可能出现版本窗口 | 记录失败并保持旧可用 snapshot，或中止发布新状态 |
| 并发搜索与重建 | 在线搜索读取 BM25 | 依赖原子 pointer handoff | 保持 snapshot 原子切换，reader 不持有跨调用旧 pointer |
| 旧索引状态缺失 | 首次启用增量机制 | 无 file/chunk state | 执行一次显式 full rebuild 建立基线 |

### 4.3 最小可复现数据集

建立一个包含以下内容的测试 repo：

```text
service-a/
  src/main/java/.../OrderService.java
  src/main/resources/application.yaml
  src/main/resources/mapper/OrderMapper.xml
  schema/order.sql
  README.md
  third_party/copied-sdk/...
docs/
  flow/order.md
```

复现顺序：

1. full rebuild，保存文件状态、semantic points 和 BM25 snapshot；
2. 仅修改 `OrderService.java` 中一个 method；
3. 移动 `README.md`，正文保持不变；
4. 删除 `application.yaml`；
5. 注入一次 embedding provider 失败和一次 BM25 publish 失败；
6. 对比 incremental 与重新 full rebuild 的最终可检索结果、point metadata 和指标。

## 5. 如何修改

### 5.1 修改原则

1. **先降重复计算，再删语料。** 增量 embedding 的质量风险低于直接扩大排除规则。
2. **按检索价值分类，不按是否源码二分。** SQL、部署配置和 curated runbook 不是源码，但具有高价值。
3. **结构化能力优先使用结构化检索。** service name、dependency、endpoint、symbol 不依赖 dense vector 才能工作。
4. **同一内容只有一个主要语义所有者。** curated/generated/repo 文档必须建立优先级。
5. **不以单个失败问题制定规则。** 任何过滤策略必须由一类语料的统计和固定评估集支持。
6. **收窄必须可观测、可回放、可回滚。** 每个 policy 版本都要记录 corpus 变化和质量指标。
7. **复用现有 evidence class。** 不新增只用于改名的平行分类体系。
8. **不通过增大 chunk 粗暴减少数量。** 当前代码 80 行和文档 5,000 characters 已不算细；继续增大会降低定位精度。

### 5.2 目标流程与语料策略

目标流程：

```text
扫描 workspace
→ 在 indexer 入口应用可解释的 corpus policy
→ 计算 file/chunk canonical hash 与版本契约
→ 复用兼容的 dense/BM25 状态
→ 仅对新增或变化内容 embedding
→ reconcile 新增、移动、删除和 stale points
→ 原子发布 semantic/BM25 snapshot
→ retrieval 根据 query kind 选择 source 和预算
→ 输出成本、质量和完整度指标
```

职责所有者：

| 职责 | 所有者 | 责任边界 |
| --- | --- | --- |
| scanner、文件过滤、path policy | `internal/indexing/indexer` | 在切 chunk 前决定文件是否进入 corpus，并记录排除原因 |
| file/chunk hash 与 reconciliation | `internal/indexing` | 判断复用、新增、删除和版本失效，不改变 evidence 语义 |
| 文档 canonical ownership | DocStore / document indexing owner | 决定同一内容的主要来源和引用关系 |
| retrieval source 与预算 | `internal/retrieval` | 根据现有 query plan/facet 选择 code、runbook、service 来源 |
| BM25 snapshot handoff | `internal/indexing` 与 retrieval BM25 handoff | 保证 sparse vocab、coordinates 和 dense snapshot 版本一致 |
| evidence/trust 分类 | `internal/domain/evidence.go` | 复用现有 evidence class 和 trust tier，不新增平行分类 |

目标语料分层如下。

#### 5.2.1 Tier A：始终 embedding

- runtime source code；
- SQL；
- curated `flow`；
- curated `schema`；
- 用户明确上传且通过内容校验的 `document`；
- 被确认参与运行时行为的核心配置和协议文件。

#### 5.2.2 Tier B：条件 embedding

- README；
- architecture/design 文档；
- ADR；
- 部署和运维说明；
- service-specific runbook；
- Spring/MyBatis 等运行时 XML；
- application/bootstrap/deployment 类 YAML；
- generated module summary。

条件可以来自：

- 路径和文件角色；
- content hash 是否与更高优先级来源重复；
- 文档长度和有效文本比例；
- 实际 retrieval 命中与最终 evidence 使用情况。

#### 5.2.3 Tier C：默认排除或摘要化

- vendor/third-party 未修改依赖；
- generated source 的机械产物；
- snapshot；
- license；
- changelog；
- CI boilerplate；
- 无业务信息的模板；
- 与 curated/generated doc 内容重复的 repo Markdown；
- generated module document 中逐段复述原始代码的正文。

#### 5.2.4 Tier D：结构化检索优先

- 没有 summary 的 service metadata；
- endpoint；
- dependency；
- symbol 和 call graph；
- ontology fact。

Tier D 不等于删除数据，而是避免为已经有可靠结构化访问路径的低信息文本增加 dense vector。

### 5.3 详细改动

#### 5.3.1 阶段 0：建立 corpus 与 retrieval 基线

在改变线上 corpus 之前，补齐以下只读统计：

- files/chunks/bytes/tokens by repo、language、extension、evidence class；
- top directories by chunk count；
- vendor/generated/third-party 规模；
- exact/near duplicate chunk ratio；
- 每种 evidence class 的 Top-K 出现率；
- 每种 evidence class 的最终入选率；
- backend fetch 后被 path dedup 丢弃的比例；
- runbook 与 repo Markdown 的重复率；
- service dense 命中后实际扩展为有效 service evidence 的比例。

固定 query slices：

- code location；
- runtime diagnosis；
- code review；
- SQL/schema；
- config/deployment；
- service identity；
- dependency/flow；
- overview；
- runbook lookup。

每个 slice 记录：

- Recall@K；
- MRR；
- nDCG；
- evidence hit rate；
- citation accuracy；
- no-result rate；
- wrong-service rate；
- duplicate candidate ratio。

#### 5.3.2 阶段 1：文件级增量 embedding

第一阶段只解决明显重复计算，不改变 corpus 范围。

为每个 repo 文件记录：

- canonical repo-relative path；
- content hash；
- policy version；
- chunker version；
- embedding model identity；
- indexed revision/time。

处理逻辑：

1. 新文件：正常 chunk 和 embedding；
2. 内容 hash 未变化：复用现有 vectors 和 sparse state；
3. 内容变化：仅重建该文件；
4. 文件删除：删除对应 vectors；
5. 文件移动：
   - 内容相同可以复用 dense embedding；
   - 更新 path、repo、line metadata；
   - 旧 point 必须删除；
6. policy/chunker/model 变化：明确触发所需范围的重建。

第一阶段可以先按文件粒度复用，不要求立即解决 changed file 内的 chunk 复用。

#### 5.3.3 阶段 2：chunk 级复用

对发生变化的文件：

- 为 canonical chunk text 计算 content hash；
- 内容相同的 chunk 复用 dense vector；
- 行号或路径变化只更新 metadata；
- 新增或正文变化的 chunk 才请求 embedding；
- 失效 chunk 明确删除。

chunk identity 不能只依赖内容 hash，因为相同正文可能在多个位置合法出现。建议区分：

- embedding cache key：model identity + canonical text hash；
- semantic point identity：source path + logical range/location；
- reconciliation identity：repo + file + generation/policy version。

这样既能复用 embedding，也不会把两个合法来源折叠成同一个证据位置。

#### 5.3.4 阶段 3：vendor/generated/path policy

在阶段 0 数据支持下，先处理高置信路径：

- `vendor`
- `third_party`
- `thirdparty`
- generated output roots
- snapshots
- copied SDK/source mirrors

规则必须支持显式保留：

- 业务修改过的 vendor code；
- 唯一可用的生成协议/schema；
- 生产环境真实执行的 generated source；
- 产品场景明确需要检索的 SDK adapter。

不建议第一版加入大量模糊文件名规则。路径规则应可解释，并通过统计说明预期减少的 corpus。

#### 5.3.5 阶段 4：文档所有权和去重

文档所有权优先级：

1. curated `schema`；
2. curated `flow`；
3. generated `module` summary；
4. user `document`；
5. repo Markdown。

去重键包含：

- canonical content hash；
- normalized title/section；
- source repo/path；
- document scope。

建议行为：

- 完全相同正文只保留最高优先级来源进入主要召回；
- 低优先级来源可以保留结构化引用，但不必重复建向量；
- generated module doc 只 embedding 摘要、职责、入口、依赖和对外接口；
- repo Markdown 重点保留 README、architecture、ADR、deployment、operations；
- license、changelog、模板默认排除。

#### 5.3.6 阶段 5：配置语料分层

配置文件不应只有“全部 dense”或“全部删除”两种模式。

建议优先保留：

- deployment manifests；
- service runtime config；
- routing/gateway config；
- schema/protocol mapping；
- Spring/MyBatis 等直接影响运行时行为的 XML；
- application/bootstrap profile 配置。

建议默认排除或降低优先级：

- CI-only config；
- generated config；
- dependency lock/metadata；
- repository tooling boilerplate；
- 与多个仓库重复的模板。

配置分类应在 chunk 前完成，避免先读取、切分、embedding 后再在检索端隐藏。

#### 5.3.7 阶段 6：service vector 收窄

建议：

- summary 为空的 service 不建 dense vector；
- service name、layer、owner 继续走结构化和 lexical scoring；
- summary 达到质量门槛的 service 才进入 dense corpus；
- 如果保留 service vectors，文本应包含稳定的 domain、tags、layer、职责摘要，而不是只有名称；
- service-scoped 查询继续优先使用配置或结构化精确匹配。

该调整的绝对成本收益可能不大，但可以减少低信息量候选和错误服务扩展。

#### 5.3.8 阶段 7：按 query kind 选择 retrieval source

当前所有 internal query 默认并行搜索 code、runbook 和 service。

建议目标：

| query kind | 主要来源 | 次要来源 |
|---|---|---|
| code review | runtime code | config、service |
| focused fact | 按 target/facet 选择 | 其余来源低预算 |
| runtime diagnosis | code、config、runbook | service、dependency |
| SQL/schema | SQL、curated schema | code |
| flow | runbook、codegraph、code | service |
| overview | service、module summary、runbook | code |
| inventory | structured service/API/dependency | code、docs |

source selection 必须来自现有 query plan/facet，不引入另一套相互冲突的 intent 模型。

#### 5.3.9 独立质量轨：补充文件级上下文

method-aware chunking 不应只输出 method/function。

可以考虑为 node-backed 文件补充一个受控的 file context chunk，内容限定为：

- package/module/import/include；
- class/interface/type signature；
- fields/constants；
- 文件顶部说明；
- 不在任何 method/function range 内的关键 declarations。

该工作可能增加少量 vectors，但能避免 corpus 收窄后进一步丢失文件语义。它属于质量补强，不计入“减少向量数”的收益。

### 5.4 数据结构和版本边界

增量索引必须能够判断“旧 vector 是否仍可复用”。至少需要区分：

- embedding provider/model；
- embedding dimension；
- canonical text normalization version；
- chunker version；
- corpus policy version；
- sparse tokenizer/vocab version。

以下变化必须显式触发相应重建：

- embedding model 或 dimension 变化；
- chunk 规则变化；
- evidence classification 改变且影响 payload/filter；
- corpus policy 改变；
- sparse vocabulary contract 变化。

不允许在模型或 policy 已变化时静默复用不兼容 vector。

### 5.5 BM25 一致性和失败行为

当前 code retrieval 使用 dense + BM25 sparse hybrid。增量 embedding 不能只考虑 dense。

需要保证：

1. 未变化 chunk 的 sparse coordinates 保持可读；
2. 新词可以追加进入 vocabulary；
3. 删除文档不会导致在线 BM25 统计永久失真；
4. workspace full rebuild 与 repo incremental update 产生一致的搜索语义；
5. vocab 落盘仍保持原子写入；
6. 失败不能留下 semantic vectors 使用了尚未持久化的 sparse token IDs。

如果增量 BM25 的 document frequency 删除语义无法安全维护，第一版可以：

- dense embedding 按文件增量；
- BM25 builder 按明确周期或阈值重建；
- 该降级必须可观测，不能静默。

### 5.6 可观测性

每次 indexing run 应记录：

- run scope：workspace/repo/file；
- scanned files；
- eligible files；
- new/changed/unchanged/deleted files；
- total/reused/embedded/deleted chunks；
- embedding input bytes/tokens；
- embedding batch count；
- dense cache hit ratio；
- corpus count by evidence class；
- policy exclusion count by reason；
- duplicate suppression count；
- embedding、upsert、delete、BM25 各阶段耗时；
- provider/model/policy/chunker version。

Dashboard 至少需要展示：

- 当前 corpus 分布；
- 最近一次索引变更；
- reused 与 newly embedded 比例；
- exclusion reason Top-N；
- semantic store 总 vectors；
- 当前 policy/model/chunker version。

### 5.7 兼容、迁移与回滚

- **向后兼容：** 现有 repo generation 和 full bootstrap 保留为安全路径；没有增量状态的 repo 首次运行时执行一次显式 full rebuild；
- **数据迁移：** file/chunk state 可以从现有 point payload、当前 revision 和一次扫描结果建立，不要求直接修改旧 point 的语义；
- **灰度方式：** 先按 repo 或环境启用增量 reconciliation，旧 generation 与新 snapshot 并行比对，确认一致后扩大范围；
- **回滚条件：** incremental 与 full rebuild 的结果不一致、stale vector 可召回、Recall@10 下降超过门禁、provider/BM25 状态出现不可解释分叉时回滚；
- **回滚步骤：** 关闭增量和新 policy 开关，恢复上一 policy/model/chunker 版本，对受影响 repo 执行明确 full rebuild，再删除不兼容的增量状态。

失败行为必须明确：

- hash/state 不一致时不能静默复用，进入该文件或 repo 的重建队列；
- model、chunker 或 policy 版本不兼容时不能混用 vectors；
- provider 错误必须向 indexing run 返回失败并保留错误上下文，不得静默替换 provider；
- 删除或移动 reconciliation 失败时不得发布“完成”状态，应保留旧 snapshot 并记录 stale cleanup failure；
- BM25 snapshot 未原子发布时，dense 与 sparse 不得被标记为同一个完成 generation；
- 任何可降级行为都必须记录 `partial` 或等价状态，不能用成功状态掩盖不完整索引。

## 6. 修改伪代码

### 6.1 核心增量流程

```go
func IndexRepo(ctx context.Context, repo string, cfg IndexConfig) error {
    state, err := LoadIndexState(repo)
    if err != nil {
        return fmt.Errorf("load index state for %q: %w", repo, err)
    }

    files, err := Scan(repo, cfg.PolicyVersion)
    if err != nil {
        return fmt.Errorf("scan repo %q: %w", repo, err)
    }

    next := state.BeginRun(cfg.Model, cfg.ChunkerVersion, cfg.PolicyVersion)
    for _, file := range files {
        if next.CanReuseFile(file, cfg) {
            next.RecordUnchangedFile(file)
            continue
        }

        chunks, err := Chunk(file, cfg.ChunkerVersion)
        if err != nil {
            next.RecordFileFailure(file, err)
            return fmt.Errorf("chunk %q: %w", file.Path, err)
        }

        for _, chunk := range chunks {
            key := EmbeddingCacheKey(cfg.Model, CanonicalTextHash(chunk.Text))
            cached, ok := next.LookupCompatibleChunk(key, cfg)
            if ok {
                if err := next.ReconcileMetadata(cached, chunk); err != nil {
                    next.RecordChunkFailure(chunk, err)
                    return fmt.Errorf("reconcile chunk %q: %w", chunk.Location, err)
                }
                continue
            }

            vector, err := Embed(ctx, cfg.Provider, chunk.Text)
            if err != nil {
                next.RecordProviderFailure(file, err)
                return fmt.Errorf("embed %q: %w", chunk.Location, err)
            }
            if err := next.UpsertChunk(chunk, vector, cfg); err != nil {
                next.RecordChunkFailure(chunk, err)
                return fmt.Errorf("upsert chunk %q: %w", chunk.Location, err)
            }
            next.RecordEmbeddedChunk(chunk)
        }

        if err := next.DeleteStaleChunks(file); err != nil {
            next.RecordCleanupFailure(file, err)
            return fmt.Errorf("delete stale chunks for %q: %w", file.Path, err)
        }
        next.RecordChangedFile(file)
    }

    if err := next.DeleteMissingFiles(files); err != nil {
        return fmt.Errorf("delete missing files for %q: %w", repo, err)
    }
    if err := next.PublishDenseSnapshot(); err != nil {
        return fmt.Errorf("publish dense snapshot for %q: %w", repo, err)
    }
    if err := PublishBM25Snapshot(next); err != nil {
        next.MarkPartial("bm25_publish_failed")
        return fmt.Errorf("publish BM25 snapshot for %q: %w", repo, err)
    }
    if err := PersistIndexState(next); err != nil {
        return fmt.Errorf("persist index state for %q: %w", repo, err)
    }

    EmitIndexMetrics(next)
    return nil
}
```

### 6.2 复用与版本边界

```go
func (s IndexState) CanReuseFile(file FileRecord, cfg IndexConfig) bool {
    old, ok := s.Files[file.Path]
    if !ok {
        return false
    }
    return old.ContentHash == file.ContentHash &&
        old.PolicyVersion == cfg.PolicyVersion &&
        old.ChunkerVersion == cfg.ChunkerVersion &&
        old.ModelIdentity == cfg.Model.Identity() &&
        old.Dimension == cfg.Model.Dimension
}

func (s IndexState) LookupCompatibleChunk(key CacheKey, cfg IndexConfig) (ChunkRecord, bool) {
    chunk, ok := s.EmbeddingCache[key]
    if !ok || chunk.ModelIdentity != cfg.Model.Identity() {
        return ChunkRecord{}, false
    }
    if chunk.Dimension != cfg.Model.Dimension {
        return ChunkRecord{}, false
    }
    return chunk, true
}
```

### 6.3 发布不变量

```text
旧 snapshot
  ├─ 增量成功且 dense/BM25/state 均持久化 → 原子切换为新 snapshot
  ├─ provider、upsert 或 stale cleanup 失败 → 保留旧 snapshot，run=failed
  └─ BM25 未能发布 → 不标记新 generation=complete，run=partial/failed
```

以下不变量必须成立：

1. 一个 semantic point 的 payload、model、policy 和 chunker 版本相互兼容；
2. 已删除或移动的旧 logical location 不再被召回；
3. 在线 reader 只能看到完整的旧 snapshot 或完整的新 snapshot；
4. `reused`、`embedded`、`deleted`、`failed` 的计数与最终状态一致。

## 7. 预期的效果

### 7.1 功能、成本和时延

实施后：

1. HEAD 未变化的仓库不产生 code embedding 请求；
2. 仓库只修改少量文件时，embedding 输入量与变化文件规模近似相关；
3. 文件移动、删除和重建后的 metadata 与 semantic state 保持正确；
4. 低信息 service name 不再单独制造 dense candidate；
5. query kind 能够优先检索最相关的 corpus，并对次要来源使用受控预算。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `index_files_reused_total` | Counter | 统计未变化文件复用次数 |
| `index_chunks_reused_total` | Counter | 统计 chunk cache 命中次数 |
| `index_chunks_embedded_total` | Counter | 统计实际调用 provider 的 chunks |
| `index_stale_points_deleted_total` | Counter | 统计删除和移动清理的旧 points |
| `index_embedding_tokens` | Histogram | 衡量每次 run 的输入成本 |
| `index_duplicate_suppressed_total` | Counter | 衡量 canonical ownership 去重效果 |
| `index_snapshot_status` | Gauge / status | 区分 complete、partial、failed 和旧 snapshot 保留 |
| `retrieval_duplicate_candidate_ratio` | Gauge | 衡量候选池重复程度 |

日志应至少能够回答：

- 本次 run 使用了哪个 model、policy、chunker 和 vocab 版本；
- 每个文件是 reused、embedded、deleted 还是 failed；
- 哪些 chunks 因 duplicate、vendor、generated 或低价值 policy 被排除；
- 是否发生 provider、upsert、cleanup 或 BM25 publish 失败；
- 最终发布的是新 snapshot、旧 snapshot，还是 partial 结果。

### 7.3 检索质量和数据正确性

固定评估集上：

- runtime code Recall@10 下降不超过 3 个百分点；
- SQL/schema Recall@10 下降不超过 3 个百分点；
- config/deployment Recall@10 下降不超过 3 个百分点；
- runbook Recall@10 下降不超过 3 个百分点；
- wrong-service rate 不上升；
- citation accuracy 不下降；
- no-result rate 不上升；
- duplicate candidate ratio 明显下降。

- 删除文件不会残留可召回旧 vector；
- 文件移动后的引用路径和行号正确；
- embedding model 或 policy 升级不会错误复用旧 vector；
- repo incremental 与 workspace rebuild 最终结果一致；
- embedding/backend 失败保持可观察，不静默切换 provider；
- BM25 vocabulary 和 sparse coordinates 重启后仍一致。

### 7.4 量化目标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 未变化仓库 code embedding 请求 | 0（已有 HEAD 门禁） | 保持 0 | 每次 sync | indexing metrics |
| 未变化文件复用率 | 未观测 | 可观测；稳定仓库接近 100% | 每次 sync | file state |
| 小提交 embedding 请求量 | 接近整个变化仓库 | 相比当前实现减少 90% 以上 | 每次小提交 | provider usage |
| runtime code Recall@10 | 待建立 | 下降不超过 3 个百分点 | 固定评估集 | retrieval eval |
| SQL/schema、config、runbook Recall@10 | 待建立 | 各下降不超过 3 个百分点 | 固定评估集 | retrieval eval |
| duplicate candidate ratio | 待建立 | 相比基线明显下降 | 固定评估集 | retrieval trace |
| stale vector 命中率 | 待建立 | 0 | 删除、移动和重建回归 | semantic store |

## 8. 测试与验收

### 8.1 单元测试

- file content hash 未变化时复用文件状态，不调用 embedding provider；
- chunk text hash 相同且 model/dimension 兼容时复用 dense vector；
- path 或 line metadata 变化时更新 metadata，不折叠合法 evidence location；
- 文件删除、移动和 policy 排除会删除 stale points；
- model、dimension、chunker、policy 或 vocab 版本变化会拒绝复用；
- duplicate document ownership 只保留最高优先级主要来源；
- summary 为空的 service 不生成 dense vector；
- provider、upsert、cleanup、BM25 publish 失败产生明确失败状态和指标。

### 8.2 集成测试

- 增量 repo 与 full rebuild 的最终 points、payload、BM25 查询结果一致；
- workspace 重启后 vocab 和 sparse coordinates 仍可读；
- rebuild 期间并发 search 只看到完整旧 snapshot 或完整新 snapshot；
- provider 配置错误不会静默切换到另一个 provider；
- 旧索引状态缺失时执行一次显式 full rebuild；
- document、module、repo Markdown 的 canonical ownership 和引用关系可追踪。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 大仓库小提交 | 只修改一个 method | 只 embedding 变化 chunks，未变化内容复用 | provider mock + token 统计 |
| 未变化仓库 | HEAD 不变 | 不调用 code embedding | indexing 单测和集成测试 |
| 重复 Markdown/runbook | 相同正文多入口 | 只保留最高优先级主要来源 | corpus ownership 检查 |
| SQL/config/runbook 查询 | 查询对应领域事实 | 主要来源优先且 Recall 不下降 | 固定 query slice |
| method context 查询 | 查询 imports/type/field | file context 可召回 | retrieval 回归 |
| 删除和移动文件 | 删除旧文件、移动同正文文件 | 无 stale point，路径正确 | semantic store 检查 |
| model/policy 升级 | 版本不兼容 | 明确重建，不复用旧 vector | version invalidation 测试 |
| 故障注入 | provider 或 BM25 publish 失败 | 旧 snapshot 保持可用，错误可诊断 | fault injection |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. file/chunk reuse、新增、删除和版本失效行为通过自动化测试；
2. incremental 与 full rebuild 的最终数据和检索语义一致；
3. 小提交的 embedding 请求量相比当前实现减少 90% 以上；
4. 固定评估集的关键 Recall@10 下降不超过 3 个百分点，wrong-service、no-result 和 citation accuracy 不恶化；
5. provider、BM25、stale cleanup 和 snapshot 状态均可观测；
6. 任何失败不会发布伪成功的 complete generation；
7. 灰度期间可以恢复上一 policy/model/chunker 版本并完成重建。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 过滤过宽 | vendor/generated/path policy 误排除业务内容 | 召回下降、证据缺失 | 先做统计和固定 query slice；支持显式保留；按 repo 灰度 | 任一关键 slice Recall@10 下降超过 3 个百分点 |
| stale vector | 删除、移动或 reconciliation 中途失败 | 检索到旧路径或旧内容 | logical location 对账、删除计数、full rebuild 比对 | stale vector 命中率大于 0 |
| cache 版本不兼容 | model、dimension、chunker 或 policy 变化 | 向量空间或 payload 不一致 | cache key 携带完整版本；不兼容时强制重建 | 发现跨版本复用或查询异常 |
| BM25 vocab 不一致 | 增量发布与 vocab 持久化顺序错误 | sparse 检索错误或重启后结果变化 | 原子 vocab 写入；dense/BM25 snapshot 绑定版本 | sparse coordinates 无法恢复 |
| incremental/full 不一致 | 文件状态遗漏或 hash 规则不稳定 | 灰度结果不可解释 | 以 full rebuild 作为 oracle；逐 repo 比对 points 和 query slices | 差异无法在单 repo 内解释 |
| canonical owner 错误 | curated/module/repo 文档优先级判断错误 | 高可信证据被低可信来源覆盖 | 复用 evidence class/trust；保留引用关系；人工抽样 | citation accuracy 下降或错误来源成为主证据 |
| provider 失败被隐藏 | backend error 被降级为另一个 provider | 结果不可预测、成本失控 | explicit dispatcher；失败即记录并终止当前 publish | 发现 provider substitution |

## 10. 实施计划

### Milestone 1：基线与观测

- 固定 query slices；
- 建立 corpus 分布和重复率统计；
- 建立 retrieval quality baseline；
- 明确 vendor/generated/top-path 数据。

### Milestone 2：文件级增量

- 文件状态和版本边界；
- unchanged file skip；
- changed/deleted file reconciliation；
- dense/BM25 一致性测试；
- repo/full rebuild 等价性测试。

### Milestone 3：高置信 corpus 治理

- vendor/third-party/generated policy；
- Markdown canonical ownership；
- exact duplicate suppression；
- service summary quality gate。

### Milestone 4：query-aware retrieval

- 复用现有 query plan/facet；
- source selection；
- source-specific budget；
- 固定评估集回归。

### Milestone 5：文件上下文质量补强

- node residual extraction；
- context chunk 预算；
- declaration/import/type 查询回归；
- 总 vectors 和 Recall 的联合评估。

## 11. 非目标

本提案不做以下事情：

- 不把系统改成只支持 Java/Python；
- 不删除 SQL、配置和 runbook 检索能力；
- 不用更大的 chunk 掩盖 corpus 数量问题；
- 不用单个 QA 失败案例硬编码过滤词；
- 不替换 semantic provider；
- 不改变 ontology、codegraph 和结构化 store 的职责；
- 不引入新的持久化生命周期状态机；
- 不承诺在没有目录和重复率基线前达到某个固定向量缩减比例。

## 12. 待决策事项

实施前需要确认：

1. vendor/third-party 是否存在必须全文检索的正式场景；
2. repo Markdown 与 DocStore 文档谁拥有 canonical source；
3. user `document` 与 generated `module` 当前是否应继续共用 `GeneratedDocKinds`；
4. service summary 的最低质量门槛；
5. query kind 是否允许关闭某一路 retrieval，还是第一版只降低预算；
6. 增量状态放在现有结构化 store 还是 semantic payload 中；
7. BM25 是实现完整增量统计，还是采用增量 dense + 周期 sparse rebuild；
8. corpus policy 是否需要平台级配置，哪些规则必须保持代码默认值。

## 13. 决策摘要

建议批准以下方向进入设计和评估：

1. 先建立 corpus/retrieval 基线；
2. 优先实现文件级增量 embedding；
3. 按证据价值治理 vendor、generated 和重复文档；
4. 保留 runtime code、SQL、curated schema/runbook；
5. 对 XML、YAML、Markdown 使用条件策略，不按扩展名整体删除；
6. summary 为空的 service metadata 默认不建 dense vector；
7. 复用现有 query plan 实现 source-aware retrieval；
8. 将 method chunk 上下文缺口作为独立质量工作处理。

在没有固定评估集证明之前，不建议直接扩大排除范围或删除大类语言支持。

当 incremental/full rebuild 对比、关键 query slice 质量或 snapshot 一致性不满足门禁时，恢复上一 policy/model/chunker 版本，并对受影响范围执行明确的 full rebuild。

## 附录 A：提案提交前检查清单

- [x] 背景说明了 workspace、indexer、semantic store 和 retrieval 的职责；
- [x] 问题以“期望行为—实际行为—差异”描述；
- [x] 包含大型仓库小提交、重复文档、source selection 和文件上下文等可复现场景；
- [x] 已区分表面现象、直接原因和机制根因；
- [x] 修改方案明确了 scanner、indexing、DocStore、retrieval 和 BM25 handoff 的职责所有者；
- [x] 伪代码覆盖复用、新增、删除、版本失效和失败发布；
- [x] 预期效果包含成本、质量、重复候选和 stale vector 的量化指标；
- [x] 已说明兼容、迁移、灰度和回滚方案；
- [x] 测试覆盖增量/full rebuild 等价性、BM25 一致性、故障注入和关键 query slices；
- [x] 未引入只针对单个案例的硬编码特例。
