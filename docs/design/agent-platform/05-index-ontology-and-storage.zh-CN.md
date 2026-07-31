# 索引、本体与源文件存储

[English](05-index-ontology-and-storage.md) | [中文](05-index-ontology-and-storage.zh-CN.md)

> 状态：核心存储与本体方向已确定；迁移和 Provider 适配分阶段实施
> 来源：Agent Index Storage Design、Nasuta Ontology Refactor、Docs Source-file Storage

## 1. 统一数据面

Nasuta 使用三个相互配合但职责不同的数据面：

| 数据面 | 职责 |
|---|---|
| Structure Store | 服务、仓库、文件、符号、API、文档和任务等结构记录 |
| Semantic Store | chunk 向量、稀疏表示和语义检索 payload |
| Ontology Snapshot | 稳定实体、类型关系和 `depends_on` 等可遍历事实 |

三者共享稳定身份，不复制第二套业务图。SQLite/MySQL 等结构存储是权威记录，语义索引和本体快照是可重建投影。

## 2. 稳定身份

实体 ID 由规范化事实确定，例如：

- repository + relative path；
- service logical name；
- language + qualified symbol；
- HTTP method + normalized route；
- document source + version + chunk locator。

入口边界完成 trim、大小写、路径和默认值规范化；下游不得重复“防御性清洗”。去重和关系连接使用稳定 ID，不使用易变的展示名称。

## 3. 索引流水线

```text
workspace/source
  -> discover
  -> parse/extract
  -> canonical entities and chunks
  -> structure transaction
  -> semantic batch upsert
  -> ontology projection
  -> atomic snapshot publish
```

要求：

- 扫描和读取都有文件大小、数量、语言和路径边界；
- 批量写入，避免 N+1；
- 重建写入临时版本，成功后原子切换；
- 中断不能留下半个词表或半个本体快照；
- 配置的后端失败保持可见，不替换 Provider；
- 删除和重命名通过 generation/version 清理陈旧投影。

## 4. Structure Store

结构库保存可审计字段：

- stable ID、类型、来源和版本；
- repo/path/service/symbol/API 等结构属性；
- 内容摘要和 source locator；
- 索引 generation 与更新时间；
- provenance 和解析状态。

在线查询只选择所需列并使用 `LIMIT`/cursor。需要完整数据集的重建流程使用明确命名的 full-read API。

## 5. Semantic Store

语义 payload 至少携带：

- stable entity/chunk ID；
- source type 和 locator；
- service/repository ownership；
- section/symbol metadata；
- index generation；
- 可用于权限过滤的 scope。

Dense 和 Sparse 可以由不同组件生成，但检索结果必须能回到同一结构记录。Embedding 未配置时只禁用语义能力；显式配置后端失败不能被 BM25 静默掩盖。

## 6. Ontology

第一版本体保持小而稳定：

- 实体：Service、Repository、File、Symbol、API、Document、Config 等；
- 关系：contains、defines、exposes、calls、depends_on、documents、configured_by；
- 关系带 provenance、confidence 和 source locator；
- 只有真实存在的关系进入快照，不持久化可从现有事实直接推导的状态。

依赖查询、QA 上下文和 `trace_deps` 共用同一 Ontology Repository，不维护第二张内存依赖图。

## 7. Docs 源文件存储

上传或外部文档保存原始源文件与抽取文本：

```text
source object
  -> immutable storage key
  -> metadata record
  -> parser/extractor
  -> normalized text/chunks
  -> semantic and ontology projections
```

存储 key 不使用不可信文件名直接拼接。系统限制：

- 单文件和总上传大小；
- 压缩包展开大小、文件数和目录深度；
- 页数、图片数和像素；
- 解析时间；
- MIME/文件签名一致性；
- Zip Slip 和压缩炸弹。

原文件下载经过鉴权和内容类型控制；索引删除与源文件删除分开定义保留策略。

## 8. Provider 与迁移

存储和语义后端使用显式 dispatcher：

```text
provider config
  -> one switch
  -> provider-specific constructor
  -> clear unavailable/error result
```

迁移遵循：

1. 新 schema 先兼容读取；
2. 批量回填稳定 ID 和 generation；
3. 双写只在明确迁移窗口内存在；
4. 验证计数、引用和检索一致性；
5. 切换读取；
6. 删除旧路径。

不得永久保留读时兼容清洗。

## 9. 验收标准

1. 每个检索 chunk 能追溯到结构记录和源文件；
2. 重建失败不会发布不完整快照；
3. 本体依赖只有一个权威 Repository；
4. 稳定身份支持重复构建幂等去重；
5. 在线查询在存储边界有界；
6. 源文件上传防止路径穿越和资源炸弹；
7. 配置后端失败不触发静默 Provider 替换。

## 详细归并材料

### Agent 索引存储设计

> Migrated from CodeLoom `docs/design/agent/agent-index-storage-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：提案，尚未实现

本文重新定义 Internal 证据链路中的 CodeGraph 和 CodeLoom 结构化 SQLite。项目尚未上线，目标实现不迁移旧表、不保留兼容字段，现有派生数据库由源代码重新构建。

#### 1. 目标与边界

本方案解决四类问题：

1. CodeGraph 符号查询使用 `%LIKE%` 全表扫描，服务名又被错误地当成符号名查询。
2. 结构库同时保存标量列和完整对象 JSON，仓库版本状态重复，多服务仓库被压缩成单一服务。
3. 依赖关系把逻辑边和第一条证据塞在同一行，无法完整表达多证据，也缺少调用方和被调用方索引。
4. 增量清理依赖不可靠的 `repo` 和路径猜测，产生旧数据、空归属和读取期兼容逻辑。

本文不重新设计 MySQL、Qdrant 或外部 CodeGraph 索引器的全部领域模型。它只定义 CodeLoom 对这些能力的所有权边界、结构库目标模型，以及 CodeGraph 必须满足的读取契约。

#### 2. 存储所有权

```text
Workspace source
  ├─ CodeGraph provider DB
  │    files / nodes / edges / nodes_fts
  │    符号、源码位置和符号关系
  ├─ CodeLoom structure DB
  │    repositories / services / endpoints
  │    dependencies / dependency_evidence
  │    服务、API 和服务级依赖
  ├─ Qdrant
  │    代码、文档和长期记忆的语义向量
  └─ MySQL
       用户、设置、文档、会话、运行轨迹、记忆和事件
```

CodeGraph 数据库由外部 provider 生产，CodeLoom 只读，不复制其物理模型。CodeLoom 结构库是自己的派生读模型，由 `internal/indexing` 生成，由 `internal/platform/store` 提供存储适配。

同一事实只能有一个所有者：

- 仓库版本只在 `repositories.head_sha` 保存。
- 服务归属由 `services.repo + services.module_path` 表达。
- 上下游关系只从 `dependencies` 推导，不写回服务 JSON。
- Runbook 正文和元数据属于 MySQL 文档库，语义召回属于 Qdrant，不进入结构库。
- 方法级调用关系属于 CodeGraph；服务级依赖属于结构库。

#### 3. 当前数据库审计

##### 3.1 CodeGraph provider DB

| 表 | 作用和数据 | 当前索引 | 评估 |
| --- | --- | --- | --- |
| `files` | 文件路径、内容哈希、语言、大小、修改/索引时间、节点数和解析错误 | 主键 `path`，`language`、`modified_at` | 合理；用于文件存在性和索引健康检查 |
| `nodes` | 符号 ID、类型、名称、限定名、文件位置、签名和语言属性 | 主键 `id`，`kind`、`name`、`qualified_name`、`file_path`、`(file_path,start_line)` 等 | 结构合理；`%LIKE%` 不能利用名称索引，问题在查询方式 |
| `edges` | 符号间关系，包含 source、target、kind、位置、来源和 metadata | `(source,kind)`、`(target,kind)`、`kind`、`provenance` | 与调用方/被调用方查询匹配，合理 |
| `nodes_fts` | `name`、`qualified_name`、`docstring`、`signature` 的 FTS5 倒排索引 | FTS5 内部索引，触发器随 `nodes` 更新 | 已具备但 CodeLoom 当前未使用，应成为符号搜索入口 |
| `unresolved_refs` | 尚未解析到目标节点的引用及候选项 | `from_node_id`、`reference_name`、组合索引和 `file_path` | provider 的诊断/后处理数据；CodeLoom 不直接依赖 |
| `project_metadata` | provider 项目级键值元数据 | 主键 `key` | provider 自有元数据 |
| `schema_versions` | provider Schema 版本记录 | 主键 `version` | provider 自有迁移元数据 |
| `nodes_fts_*` | SQLite FTS5 自动维护的 shadow tables | SQLite 内部索引 | 不是业务表，CodeLoom 不得直接查询 |
| `sqlite_sequence`、`sqlite_stat1` | SQLite 自增和查询规划内部状态 | SQLite 管理 | 不是业务表 |

本地样本约 43.5 万节点、157 万条边。`name LIKE '%x%'` 单次查询约 3.1 至 3.7 秒，而现有 FTS 查询约 0 至 10 毫秒。该数字只说明当前瓶颈方向，不作为跨机器性能承诺。

##### 3.2 CodeLoom 结构库

| 当前表 | 原用途 | 问题 | 目标动作 |
| --- | --- | --- | --- |
| `repos` | 仓库和索引时间 | `last_commit` 与 `repo_index_state.head_sha` 重复，且样本中全部为空 | 用 `repositories` 替代 |
| `repo_index_state` | 仓库 SHA | 与 `repos` 拆分了同一个生命周期 | 删除，合并到 `repositories` |
| `services` | 服务结构记录 | 标量列与完整 `json` 重复；`module_path` 只能从 JSON 读取 | 重建为规范化 `services` |
| `endpoints` | API 路由 | 重复保存 repo、service name 和完整 `json`；服务模糊匹配无法利用索引 | 通过 `service_key` 关联服务 |
| `dependency_edges` | 服务级依赖和第一条证据 | 多证据丢失；调用方/目标方缺索引；存在空 repo；`interface_method` 无有效数据 | 拆为依赖和依赖证据 |
| `feign_edges` | 旧 Feign 专用关系 | 已被通用依赖模型替代，仍残留数据 | 删除 |
| `runbooks` | 旧 Runbook 结构数据 | 当前职责已迁移到 MySQL 文档库和 Qdrant | 删除 |
| `sqlite_sequence` | SQLite 自增内部状态 | 非业务表 | 由 SQLite 管理 |

结构库当前约 5 MB，适合优先采用全量快照重建。没有必要为这组派生数据引入复杂的行级迁移和读取期兼容。

#### 4. 目标结构库

##### 4.1 关系模型

```text
repositories 1 ─── n services 1 ─── n endpoints
                         │
                         ├── 1 ─── n dependencies (caller)
                         │                │
                         │                └── 1 ─── n dependency_evidence
                         │
                         └── 0 ─── n dependencies (service target)
```

目标库只有五张业务表。所有字符串在索引写入边界完成 trim、大小写和路径规范化；Store 读取不再修复数据。

##### 4.2 `repositories`

一行表示一个规范仓库及其当前结构索引版本。

```sql
CREATE TABLE repositories (
  repo       TEXT PRIMARY KEY,
  head_sha   TEXT NOT NULL,
  indexed_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX idx_repositories_indexed_at
  ON repositories(indexed_at DESC);
```

- `repo` 是相对 workspace 的规范仓库标识，例如 `group/name`，不能包含 `repos/` 前缀。
- `head_sha` 是该快照对应的源码版本；无版本的输入在写入前失败，不能用空串表示未知。
- `indexed_at` 使用 Unix 毫秒，避免文本时间格式分歧。

##### 4.3 `services`

一行表示仓库内一个可独立识别的服务或运行模块。一个仓库可以有多个服务。

```sql
CREATE TABLE services (
  service_key          TEXT PRIMARY KEY,
  repo                 TEXT NOT NULL REFERENCES repositories(repo) ON DELETE CASCADE,
  module_path          TEXT NOT NULL,
  service_name         TEXT NOT NULL,
  layer                TEXT NOT NULL,
  language             TEXT NOT NULL,
  runtime              TEXT NOT NULL DEFAULT '',
  scope                TEXT NOT NULL DEFAULT '',
  owner                TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL DEFAULT '',
  summary              TEXT NOT NULL DEFAULT '',
  confidence           REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  tags_json            TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tags_json)),
  docs_json            TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(docs_json)),
  source_of_truth_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_of_truth_json)),
  entrypoints_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(entrypoints_json)),
  ports_json           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(ports_json)),
  UNIQUE (repo, module_path)
);

CREATE INDEX idx_services_name
  ON services(service_name);
CREATE INDEX idx_services_repo_name
  ON services(repo, service_name);
```

- `service_key` 从规范 `repo + module_path` 确定性生成；调用方不拼接其格式。
- `module_path` 是相对仓库根目录的规范路径，根模块使用 `.`，不能用空串表达。
- JSON 只用于不参与 SQL 条件和关联的有界数组，不再保存完整 `ServiceRecord`。
- `upstreams` 和 `downstreams` 不存储，从依赖表查询。
- 同仓库同模块只能对应一个服务；文档和代码扫描结果必须在索引入口合并后再写入。

##### 4.4 `endpoints`

一行表示一个服务对外暴露的规范 API。

```sql
CREATE TABLE endpoints (
  endpoint_id    INTEGER PRIMARY KEY,
  service_key   TEXT NOT NULL REFERENCES services(service_key) ON DELETE CASCADE,
  method        TEXT NOT NULL,
  path          TEXT NOT NULL,
  handler       TEXT NOT NULL DEFAULT '',
  handler_method TEXT NOT NULL DEFAULT '',
  file_path     TEXT NOT NULL,
  line          INTEGER NOT NULL CHECK (line >= 0),
  source_kind   TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  confidence    REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (service_key, method, path)
);

CREATE INDEX idx_endpoints_service_path
  ON endpoints(service_key, path, method);
```

- `method` 统一为大写，`path` 统一为以 `/` 开头的规范路由。
- `file_path` 是规范 workspace 相对路径；文档没有源码位置时允许空串，不能伪造路径。
- 结构化 API 按精确服务查询，路径条件使用精确值或前缀；服务名不再使用 `%LIKE%`。
- 模糊语义发现由检索层完成，结构库不为几千条路由引入重复全文索引。

##### 4.5 `dependencies`

一行表示一条去重后的逻辑依赖，证据单独保存。

```sql
CREATE TABLE dependencies (
  dependency_id     INTEGER PRIMARY KEY,
  caller_service_key TEXT NOT NULL REFERENCES services(service_key) ON DELETE CASCADE,
  target_kind       TEXT NOT NULL CHECK (target_kind IN ('service', 'external')),
  target_service_key TEXT REFERENCES services(service_key) ON DELETE CASCADE,
  external_target   TEXT,
  protocol          TEXT NOT NULL,
  confidence        REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  CHECK (
    (target_kind = 'service' AND target_service_key IS NOT NULL AND external_target IS NULL) OR
    (target_kind = 'external' AND target_service_key IS NULL AND external_target IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_dependencies_service
  ON dependencies(caller_service_key, target_service_key, protocol)
  WHERE target_kind = 'service';
CREATE UNIQUE INDEX uq_dependencies_external
  ON dependencies(caller_service_key, external_target, protocol)
  WHERE target_kind = 'external';
CREATE INDEX idx_dependencies_caller
  ON dependencies(caller_service_key, protocol);
CREATE INDEX idx_dependencies_target
  ON dependencies(target_service_key, protocol)
  WHERE target_service_key IS NOT NULL;
```

- `protocol` 使用领域枚举，例如 `feign`、`http`、`grpc`、`rpc`，但表名和 API 不再把所有依赖称为 Feign。
- 已解析目标必须引用 `service_key`；无法解析到 workspace 服务的目标明确保存为 `external_target`。
- 仓库归属由 caller service 推导，不重复保存 `repo`。
- 两个 partial unique index 分别约束内部服务和外部目标，避免 SQLite 的 NULL 唯一性语义放过重复边。

##### 4.6 `dependency_evidence`

一行表示逻辑依赖的一处证据，同一依赖可以有多处证据。

```sql
CREATE TABLE dependency_evidence (
  evidence_id   INTEGER PRIMARY KEY,
  dependency_id INTEGER NOT NULL REFERENCES dependencies(dependency_id) ON DELETE CASCADE,
  file_path     TEXT NOT NULL,
  line          INTEGER NOT NULL CHECK (line >= 0),
  symbol        TEXT NOT NULL DEFAULT '',
  source_kind   TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  UNIQUE (dependency_id, file_path, line, symbol, source_kind)
);

CREATE INDEX idx_dependency_evidence_dependency
  ON dependency_evidence(dependency_id, file_path, line);
CREATE INDEX idx_dependency_evidence_file
  ON dependency_evidence(file_path, line);
```

这张表保留所有有效证据，不再只取 `Evidence[0]`。`symbol` 替代没有实际数据的 Feign 专用 `interface_method`，可表示任意协议的源码锚点。

#### 5. CodeGraph 读取契约

CodeLoom 不应让上层直接拼 CodeGraph SQL。适配器只公开四类能力：

```go
SearchSymbols(ctx, SymbolQuery)
FindNodeAt(ctx, canonicalFilePath, line)
FindRelated(ctx, nodeID, direction, edgeKinds, limit)
FindFile(ctx, canonicalFilePath)
```

`SymbolQuery` 包含规范词项、可选 kind 集合、可选 `module_path` 路径范围和 limit。规则如下：

1. 符号搜索使用 `nodes_fts MATCH`，再连接 `nodes.rowid` 取完整字段。
2. kind 和路径范围在 `LIMIT` 前过滤，不能先截取再在 Go 中筛选。
3. 多个词项合并成一次受转义的 FTS 查询，不按关键词启动 goroutine 或重复扫描。
4. 服务名只用于从结构库解析 `repo + module_path`，再转成 CodeGraph `file_path` 前缀；不能查询 `nodes.name`。
5. 文件定位只接受入口已规范化的精确路径。后缀 `%LIKE%` 和 `serviceFromPath` 猜测不属于正常路径。
6. 所有 SQL 使用 `QueryContext`/`QueryRowContext`；取消请求必须中止数据库工作。
7. 返回节点必须能解析到当前 workspace 文件。文件缺失是索引完整性错误，不能返回空 `source` 继续回答。

CodeGraph provider 必须保留以下能力索引：

| 查询 | 必需索引 |
| --- | --- |
| 符号全文检索 | `nodes_fts` 及其同步触发器 |
| 文件内符号定位 | `nodes(file_path, start_line)` |
| 下游关系 | `edges(source, kind)` |
| 上游关系 | `edges(target, kind)` |
| 文件精确定位 | `files(path)` 主键 |

`nodes(name)` 可以服务精确/前缀名称查询，但不能优化前置通配符。CodeLoom 不再把它当模糊搜索索引。

连接管理必须允许只读请求并发，并保证 Refresh 不关闭仍被查询使用的旧连接。新 generation 完成校验后原子发布，旧 generation 在在途 reader 释放后关闭。

#### 6. 写入和重建流程

两个 SQLite 都是可丢弃的派生数据，采用快照发布：

```text
扫描源代码
  -> 在入口规范化并校验领域记录
  -> 写入临时数据库
  -> 校验 Schema、外键、重复项、路径和版本
  -> 关闭并 fsync 临时数据库
  -> 原子 rename 发布
  -> 刷新只读连接
```

结构库构建阶段启用 `PRAGMA foreign_keys = ON`，使用单个事务和 prepared statements 批量写入。临时库使用单文件 journal 模式，关闭后再 rename，避免遗漏 WAL sidecar。

发布前至少执行：

- `PRAGMA integrity_check` 返回 `ok`；
- `PRAGMA foreign_key_check` 无结果；
- 每个 service 的 repo 存在，每个 endpoint/依赖的 service 存在；
- `repo`、`module_path`、`file_path` 均为规范值；
- 没有重复服务、API、逻辑依赖或证据；
- 快照中的 `head_sha` 与本次扫描输入一致；
- CodeGraph `nodes_fts` 行数和 `nodes` 同步，节点路径只来自当前 `repos/` workspace。

校验失败时保留正在服务的旧 generation，并记录明确错误；不能发布部分数据，也不能静默退回其他 provider。

#### 7. 领域和查询 API 调整

实施时同步删除以下复杂度，而不是在新表外增加兼容层：

- 删除 `repos`、`repo_index_state`、`feign_edges`、`runbooks` 和旧 `dependency_edges`。
- 删除结构记录的完整对象 `json` 列。
- 删除 `IndexBundle.Feigns` 和 Feign 专用持久化路径，统一输出 `Dependencies`。
- 删除 `ServiceRecord.Upstreams/Downstreams` 持久字段，按依赖查询生成。
- 将 `IndexBundle` 限定为结构快照；Runbook 继续走文档库和 Qdrant。
- 索引入口完成 doc/code service 合并，Store 只接收唯一规范服务；删除读取期 `MergeServices`。
- 用 `ServicesByRepos(repos) -> map[repo][]ServiceRef` 替代单值 `RepoSvcMap`，保留多服务仓库。
- 通过最长 `module_path` 前缀把 CodeGraph 文件归属到服务；删除 Dashboard 和检索层各自的 `serviceFromPath` 猜测。
- `ListApis` 先精确解析 service，再按 `service_key` 查询；不对 `service_name` 使用 `%LIKE%`。
- Dashboard/API 将统计名从 `feigns` 改为 `dependencies`。
- 删除 CodeGraph `%LIKE%` 符号搜索和逐关键词 fan-out。

Store 方法接收 `context.Context`。批量读取使用一个 `IN` 查询和 map 聚合，不产生 N+1；上层只依赖领域查询，不依赖表名或 JSON 布局。

#### 8. 实施切片

用户批准本提案后，按可独立验证的切片实施：

1. 定义结构领域模型、目标 Schema 和 Store 契约测试。
2. 在索引入口实现仓库、模块、服务、API、依赖和证据的规范化与去重。
3. 切换结构库写入、读取和内存依赖图构建，删除旧模型。
4. 实现 CodeGraph FTS/context 适配器和模块路径范围查询。
5. 切换 Agent、检索、Dashboard/API/UI 的服务解析和 `dependencies` 术语。
6. 实现临时库校验、原子发布和连接 generation 刷新。
7. 删除全部旧表、兼容方法和不可达代码，执行全量重建。

每个切片至少运行相关 package tests 和 `go build ./...`；最终运行 `go test ./...` 与 `go vet ./...`。本项目未上线，因此不编写旧库迁移脚本，启动/Bootstrap 检测到旧 Schema 时直接要求重建。

#### 9. 验收标准

##### 正确性

- 一个仓库中的多个服务都能独立查询，并通过 `module_path` 正确映射 CodeGraph 节点。
- 相同逻辑依赖只存一行，所有证据均可返回。
- 删除或重命名仓库/模块后，全量发布不残留旧节点和结构记录。
- Runbook、向量、方法调用和服务结构各自只有一个数据所有者。
- 正常读取路径没有 trim、大小写修复、旧字段 fallback 或路径后缀猜测。

##### 查询计划

- 符号搜索的 `EXPLAIN QUERY PLAN` 使用 `nodes_fts`，没有 `SCAN nodes`。
- API 列表先使用 `service_key` 索引缩小范围。
- 上下游依赖分别使用 caller/target 索引。
- 文件内符号定位使用 `(file_path,start_line)`，关系遍历使用 `(source,kind)` 或 `(target,kind)`。
- 没有逐关键词数据库调用、逐仓库 service 查询或其他 N+1 路径。

##### 性能基线

在当前约 43.5 万 CodeGraph 节点的数据规模上：

- 单次符号查询以 p95 小于 100 ms 为工程目标；
- 结构库精确服务、API 和一跳依赖查询以 p95 小于 50 ms 为目标；
- 首词链路中 CodeGraph 搜索不再出现秒级 `%LIKE%` 扫描；
- 指标必须分别记录数据库时间、源码读取时间和证据组装时间，不能只记录总耗时。

性能目标需由固定数据集和实际 QA trace 验证；本地 FTS 的 0 至 10 ms 测量不能直接当作线上 SLA。

### Nasuta 本体化重构设计

> Migrated from Nasuta `docs/design/nasuta-ontology-refactor.zh-CN.md`; incorporated into this module on 2026-07-31.

> 状态：SQLite 第一阶段及 `trace_deps` 迁移已实现；Neo4j Adapter 按需求驱动暂缓
>
> 范围：已实现的 SQLite 本体化边界、实施决策与后续 Neo4j 计划

#### 1. 摘要与决策

Nasuta 已经拥有 `ServiceRecord`、`EndpointRecord`、`DependencyEdge`、Runbook、代码符号和证据。这些结构表达了领域事实，但类型含义、关系约束、实体身份、跨类型查询和证据规则曾分散在索引器、SQLite、内存图和 Agent 工具中，属于“隐式本体”。

本设计将这些隐式语义提升为显式、可验证、可查询的工作区本体，并作出以下决策：

1. 新增 `internal/ontology`，集中拥有实体类型、关系类型、约束、稳定身份、投影和查询语义。
2. 第一阶段只覆盖现有确定性扫描能够稳定产出的概念，不使用 LLM 自动制造事实。
3. 默认使用当前结构化 SQLite 保存本体快照；结构数据和本体数据在同一个临时数据库中校验并原子发布。
4. 通过 `ontology.Repository` 保留 Neo4j Provider 扩展能力，但运行时只允许一个本体后端，不进行 SQLite/Neo4j 双写。
5. Provider 使用一个显式分发器。显式配置的 Neo4j 失败必须可观察，不能静默替换为 SQLite。
6. Qdrant/Milvus 继续负责语义召回，codegraph 继续负责完整的方法调用关系；两者都不是本体事实主存储。
7. 通用关系查询与 `trace_deps` 统一读取 Ontology Repository；`trace_calls` 使用本体解析 API 入口后继续交给 CodeGraph。`get_service`、`list_apis` 仍保留完整详情读取模型。
8. 本体 Go 契约先保持内部，不增加面向上层应用的公开注册入口。

本次重构的直接收益是统一语义、证据和查询边界，降低后续增加 Database、MessageTopic、Incident 等概念的成本。它不会仅凭引入“本体”或 Neo4j 自动提高抽取准确率；准确率仍由确定性抽取、实体解析和证据质量决定。

#### 2. 背景与现状

##### 2.1 当前数据能力

当前结构化索引已经表达以下事实：

```text
Repository 包含 Service
Service 暴露 API Endpoint
Service 依赖 Service 或 External Target
API Endpoint 关联 HandlerMethod
Runbook frontmatter 声明 Service 依赖
Codegraph 保存 Symbol calls Symbol
```

主要归属如下：

| 能力 | 当前归属 |
|---|---|
| Service、Endpoint、Dependency 数据结构 | `internal/domain` |
| 多语言确定性扫描和规范化 | `internal/indexing/indexer` |
| 结构化快照 | `internal/platform/store.SQLite` |
| 服务依赖遍历 | `internal/ontology.Service`、`ontology.Repository` |
| 方法调用链 | `internal/platform/store/codegraph`、`internal/callchain` |
| 向量与混合检索 | `internal/semantic`、`internal/retrieval` |
| Agent 专用工具 | `internal/agent` |
| 上层稳定只读能力 | `knowledge.API` |

##### 2.2 现有问题

1. `Service`、`API`、`Dependency` 的语义依赖具体字段和工具说明，没有统一 Schema。
2. 相同关系曾分别存在于结构表、内存图、Runbook 和 codegraph 中，缺少共同的事实标识与证据模型。
3. Agent 能分别调用专用工具，但无法表达“服务 → API → Symbol → Runbook”这类跨类型查询。
4. 当前关系字段允许代码构造出语义错误的数据，主要依赖各扫描器自律。
5. 新增概念时容易把抽取、存储、查询和工具输出一起耦合修改。
6. 如果未来引入 Neo4j，而上层直接依赖 SQL 或 SQLite 类型，迁移会扩散到 Agent、Retrieval、Dashboard 和索引生命周期。

##### 2.3 命名约定

本设计中的术语：

- **Entity**：具有稳定身份的领域对象，例如 Service、APIEndpoint。
- **Fact**：两个 Entity 之间的一条有向关系，例如 `Service depends_on Service`。
- **Predicate**：Fact 的关系类型，例如 `depends_on`。
- **Evidence**：支持 Entity 或 Fact 的代码/文档位置。
- **Snapshot**：一次索引生成并原子发布的完整实体和事实集合。
- **Direct Fact**：由扫描或受控文档直接支持的事实。
- **Derived Path**：查询时根据多条 Direct Fact 得到的可达路径，不保存为新的直接事实。

#### 3. 目标与非目标

##### 3.1 目标

1. 给现有结构化知识建立单一的类型和关系语义。
2. 所有实体使用稳定 ID，重复索引不改变身份。
3. 所有事实经过 domain/range、属性白名单和引用完整性校验。
4. 每条非结构性事实携带可回溯证据和置信度。
5. 支持实体解析、一跳邻接、反向邻接和有界路径查询。
6. 本体与结构化索引使用一致的快照，不能跨版本读取。
7. 默认不增加本地部署依赖。
8. 保留可测试的 Neo4j Provider 扩展点。
9. 迁移过程不改变现有专用工具的可观察行为。

##### 3.2 非目标

第一阶段不做以下工作：

- 不引入 OWL、RDF、SPARQL 或通用三元组推理引擎。
- 不把每条日志、Trace Span、代码 Chunk 或 LLM 调用建成实体。
- 不把向量相似结果当作本体事实。
- 不让 LLM 直接写入正式本体。
- 不立即用本体替换 codegraph 的完整 Symbol 调用图。
- 不进行 SQLite 和 Neo4j 双写。
- 不实现任意 Cypher/SQL 执行接口。
- 不在没有真实消费者时公开第三方本体注册入口。
- 不承诺第一阶段支持通用图算法、中心性或社区发现。

#### 4. 设计原则

##### 4.1 事实来源优先级

```text
AST/结构扫描
  > 明确配置和代码注解
  > 人工维护的受控 Frontmatter
  > 生成文档
  > LLM 候选
```

LLM 候选在未来也必须经过 Schema 校验、实体解析、原始证据确认和人工/确定性规则批准，才能成为正式事实。

##### 4.2 规范化只发生在入口

扫描器和文档解析器负责建立规范：

- HTTP Method 大写；
- API Path 具有前导 `/`；
- Repo、ModulePath、文件路径使用既有规范；
- Service 使用已经生成的 `ServiceKey`；
- 外部目标在结构索引入口规范化；
- 别名在写入前生成规范形式。

进入 `ontology.Entity` 和 `ontology.Fact` 后，下游不再重复 Trim、Lower、兼容旧别名或补默认值。非法数据必须失败并指出来源。

##### 4.3 直接事实与推导分离

假设有：

```text
order-service depends_on payment-service
payment-service depends_on mysql
```

系统可以查询到 `order-service → payment-service → mysql`，但不能持久化一条新的直接事实 `order-service depends_on mysql`。查询结果必须标识路径深度和基础 Fact ID。

反向关系也不重复存储。例如 `depended_on_by` 由 `depends_on` 的反向查询得到，避免两个方向漂移。

##### 4.4 单 Provider、无静默替换

运行时只能有一个本体 Repository：

```text
provider=sqlite → 只查询和发布 SQLite 本体
provider=neo4j  → 只查询和发布 Neo4j 本体
```

Neo4j 配置错误或不可达时，本体能力应进入明确的 unavailable 状态并记录错误；不能自动改查 SQLite 后声称 Neo4j 正常。

`trace_deps` 与 QA 依赖上下文只读取当前 Ontology Provider。结构化详情、向量检索和 CodeGraph 仍是独立能力，不属于 Provider 替换。

#### 5. 第一版本体模型

##### 5.1 Entity Class

| Class | 稳定身份 | 来源 | 主要属性 |
|---|---|---|---|
| `repository` | repo | RepositoryRecord | repo、head_sha |
| `service` | 现有 ServiceKey | ServiceRecord | repo、module_path、language、owner、runtime |
| `api_endpoint` | serviceKey + method + path | EndpointRecord | method、path、file、handler |
| `code_symbol` | repo + file + qualified/handler name | EndpointRecord/codegraph 引用 | file、qualified_name、language |
| `external_system` | 规范化 target | DependencyEdge | target |
| `runbook` | runbook ID | RunbookRecord | title、path、scope、tags |

第一版不创建 Database、DatabaseTable、MessageTopic、Incident、LogEvent、TraceSpan。它们必须在确定性抽取和实际查询需求成熟后单独加入。

##### 5.2 Predicate

| Predicate | Subject | Object | Qualifier | 是否传递 |
|---|---|---|---|---|
| `contains` | Repository | Service | 无 | 否 |
| `exposes` | Service | APIEndpoint | 无 | 否 |
| `implemented_by` | APIEndpoint | CodeSymbol | 无 | 否 |
| `depends_on` | Service | Service/ExternalSystem | protocol | 否 |
| `documented_by` | Service | Runbook | scope | 否 |

`depends_on` 的协议保留现有枚举：

```text
feign | http | grpc | rpc | kafka | runbook
```

##### 5.3 核心类型草案

以下代码仅描述目标契约：

```go
package ontology

type Class string

const (
    ClassRepository     Class = "repository"
    ClassService        Class = "service"
    ClassAPIEndpoint    Class = "api_endpoint"
    ClassCodeSymbol     Class = "code_symbol"
    ClassExternalSystem Class = "external_system"
    ClassRunbook        Class = "runbook"
)

type Predicate string

const (
    PredicateContains      Predicate = "contains"
    PredicateExposes       Predicate = "exposes"
    PredicateImplementedBy Predicate = "implemented_by"
    PredicateDependsOn     Predicate = "depends_on"
    PredicateDocumentedBy  Predicate = "documented_by"
)

type Entity struct {
    ID         string
    Class      Class
    Key        string
    Name       string
    Properties map[string]string
    Aliases    []string
    Confidence float64
}

type Fact struct {
    ID         string
    SubjectID  string
    Predicate  Predicate
    ObjectID   string
    Qualifiers map[string]string
    Confidence float64
    Evidence   []Evidence
}

type Evidence struct {
    Path   string
    Line   int
    Symbol string
    Source string
}

type Snapshot struct {
    SchemaVersion int
    Entities      []Entity
    Facts         []Fact
}
```

`Properties` 和 `Qualifiers` 仅允许 `map[string]string`，并由 Class/Predicate Schema 限定白名单；不允许任意 `map[string]any` 穿过领域层。

##### 5.4 Schema 约束

```go
type RelationDef struct {
    SubjectClasses map[Class]struct{}
    ObjectClasses  map[Class]struct{}
    Qualifiers     map[string]struct{}
}

var relationSchema = map[Predicate]RelationDef{
    PredicateExposes: {
        SubjectClasses: classSet(ClassService),
        ObjectClasses:  classSet(ClassAPIEndpoint),
    },
    PredicateDependsOn: {
        SubjectClasses: classSet(ClassService),
        ObjectClasses:  classSet(ClassService, ClassExternalSystem),
        Qualifiers:     stringSet("protocol"),
    },
    PredicateImplementedBy: {
        SubjectClasses: classSet(ClassAPIEndpoint),
        ObjectClasses:  classSet(ClassCodeSymbol),
    },
}
```

校验至少覆盖：

- Entity Class 是否已注册；
- Entity ID、Key、Name 是否完整；
- Fact 的 Subject/Object 是否存在；
- Predicate 是否允许对应的 Subject/Object Class；
- Properties/Qualifiers 是否属于白名单；
- Confidence 是否在 `[0,1]`；
- Evidence 是否使用规范路径和非负行号；
- ID 是否与规范化 Seed 一致；
- 同一逻辑事实是否被确定性去重。

错误示例必须被拒绝：

```text
Runbook exposes APIEndpoint
APIEndpoint depends_on Repository
Service implemented_by ExternalSystem
```

#### 6. 稳定身份与去重

所有 ID 使用确定性生成，重新索引不能改变：

```go
func RepositoryID(repo string) string {
    return platform.UUIDFromString("repository\x00" + repo)
}

func ServiceID(service domain.ServiceRecord) string {
    return service.ServiceKey
}

func APIEndpointID(endpoint domain.EndpointRecord) string {
    seed := strings.Join([]string{
        "api", endpoint.ServiceKey, endpoint.Method, endpoint.Path,
    }, "\x00")
    return platform.UUIDFromString(seed)
}

func ExternalSystemID(target string) string {
    return platform.UUIDFromString("external_system\x00" + target)
}

func FactID(fact Fact) string {
    seed := strings.Join([]string{
        "fact", fact.SubjectID, string(fact.Predicate), fact.ObjectID,
        canonicalQualifiers(fact.Qualifiers),
    }, "\x00")
    return platform.UUIDFromString(seed)
}
```

同一 Fact 来自多个扫描器或多个文件时，保留一个 Fact，合并 Evidence，并取最高 Confidence。去重使用 `map` 记录 Fact ID 到结果位置，复杂度为 O(n)。

#### 7. 投影与抽取流水线

##### 7.1 不重复扫描

本体投影只消费已经 Canonicalize 的 `domain.IndexBundle`，不能重新解析 Java/Python 或重复解析路径：

```go
func Project(bundle domain.IndexBundle) (Snapshot, error) {
    builder := NewBuilder(CurrentSchemaVersion)

    for _, repository := range bundle.Repositories {
        builder.AddRepository(repository)
    }
    for _, service := range bundle.Services {
        builder.AddService(service)
        builder.AddRepositoryContainsService(service)
    }
    for _, endpoint := range bundle.Endpoints {
        builder.AddEndpoint(endpoint)
        builder.AddServiceExposesEndpoint(endpoint)
        builder.AddEndpointImplementation(endpoint)
    }
    for _, dependency := range bundle.Dependencies {
        builder.AddDependency(dependency)
    }
    for _, runbook := range bundle.Runbooks {
        builder.AddRunbook(runbook)
        builder.AddRunbookRelations(runbook)
    }

    return builder.Build()
}
```

Projector 只负责确定性投影和合并；Publisher 是正式发布边界，在写入完整 Workspace 快照前统一调用 `ValidateSnapshot`，避免生成阶段和发布阶段重复校验同一份 Snapshot。

##### 7.2 Runbook 模型调整

当前 `RunbookRecord` 未保存 frontmatter 中的 `service:`，投影层不能再次读取 DocStore。文档入口一次读取同一批 DocStore 记录，同时生成 Runbook 和声明依赖；读取失败必须中止本次发布，不能当成空知识库清除上一代事实。模型扩展为：

```go
type RunbookRecord struct {
    ID          string
    Repo        string
    Title       string
    Path        string
    Scope       string
    ServiceName string
    Tags        []string
    Text        string
    Confidence  float64
}
```

Frontmatter：

```yaml
---
service: order-service
depends_on:
  - payment-service
called_by:
  - gateway-service
---
```

投影：

```text
order-service documented_by order-runbook
order-service depends_on payment-service
gateway-service depends_on order-service
```

无法解析 `service` 时保留 Runbook Entity，但不创建虚假 Service，也不生成 `documented_by`。未解析项进入构建统计和日志。

##### 7.3 CodeSymbol 边界

第一阶段只为能够从 Endpoint 的 `HandlerMethod` 和文件位置稳定识别的 Symbol 创建引用实体。完整的 `Symbol calls Symbol` 仍由 codegraph 拥有：

```text
Ontology: APIEndpoint implemented_by CodeSymbolRef
Codegraph: CodeSymbol calls CodeSymbol
```

如果 HandlerMethod 为空或身份不稳定，只保留 API Entity 的 handler/file 属性，不生成 `implemented_by` 事实。不能用名称猜测 Symbol。

##### 7.4 明确不进入本体的数据

以下内容保留为 Evidence 或专用存储，不创建高基数 Entity：

- 原始代码 Chunk；
- 每条日志；
- 每个 Trace Span；
- 每次 QA Run/LLM Call；
- 每次索引过程事件；
- 向量命中结果。

未来增加 Incident 时，应建 Incident 聚合实体并链接受影响 Service；原始日志和 Span 仍只作为 Incident Evidence。

#### 8. 包与依赖边界

目标包结构：

```text
internal/ontology/
├── model.go
├── schema.go
├── identity.go
├── projector.go
├── validate.go
├── repository.go
├── publisher.go
├── query.go
├── service.go
└── contract/
    └── repository.go

internal/platform/ontologystore/
├── config.go
├── provider.go
├── sqlite/
│   └── backend.go
└── neo4j/
    └── backend.go   # Provider 真正交付时加入
```

依赖方向：

```text
agent/retrieval ────────┐
                       ▼
                ontology.Service
                       ▼
                ontology.Repository
                       ▲
          ┌────────────┴────────────┐
          │                         │
SQLite Adapter              Neo4j Adapter
```

约束：

- `internal/ontology` 不导入 SQLite、Neo4j Driver、HTTP 或配置实现。
- Repository 接口定义在消费者 `internal/ontology`。
- Adapter 可以导入 ontology，ontology 不能导入 Adapter。
- Agent Service 保存精确的 `*ontology.Service`，不能保存通用 Provider 容器。
- Provider 分发只存在于 `internal/platform/ontologystore/provider.go`。
- `knowledge.API` 第一阶段不增加本体方法，避免内部模型过早成为公共兼容负担。

#### 9. Repository 查询契约

查询和发布是两个不同生命周期：Agent 查询只需要 `Repository`；Indexing 发布需要能够同时处理结构快照和本体快照的 `Publisher`。分开定义可以避免查询服务获得重建权限，也解决 SQLite 同库原子发布与 Neo4j 跨存储发布的差异。

查询接口围绕 Nasuta 的业务需求定义，不照搬 SQLite 或 Neo4j API：

```go
type Repository interface {
    Resolve(context.Context, ResolveQuery) (ResolveResult, error)
    EntitiesByID(context.Context, EntityQuery) ([]EntityRef, error)
    Neighbors(context.Context, NeighborQuery) ([]Fact, bool, error)
    Stats(context.Context) (Stats, error)
}
```

`EntitiesByID` 只用于关系查询完成后批量补齐 Entity 名称和 Class，最多接收 200 个 ID。它避免 Tool 层按路径逐个解析形成 N+1 查询，不是通用实体扫描接口。
有界路径遍历是 `internal/ontology` 基于 `Neighbors` 提供的通用算法，不要求每个 Provider 重复实现。

发布契约：

```go
type WorkspaceSnapshot struct {
    Structure domain.IndexBundle
    Ontology  Snapshot
}

type Publisher interface {
    PublishWorkspace(context.Context, WorkspaceSnapshot) (generation string, err error)
}

type Backend interface {
    Repository
    Publisher
    Close() error
}
```

`Generation` 对规范化后的完整 Structure 与 Ontology Snapshot 做确定性摘要，忽略 `IndexedAt` 这类刷新时间，不作为第二份可变字段保存。这样 Runbook、结构元数据或本体事实变化都会切换 Generation，而相同内容的重复构建保持稳定。Publisher 返回实际发布的 Generation，避免调用方和存储层重复计算。SQLite Backend 在一个临时数据库中发布两部分；Neo4j Backend 协调结构 SQLite 和 Neo4j 代际快照，并用 Generation 阻止跨版本本体查询。

一次关系查询由 `Resolve` 返回命中的实体和本次读取的 Generation，随后把 Generation 传给 Neighbors 和 EntitiesByID。若发布恰好发生在调用之间，Repository 返回 `ErrStaleSnapshot`，Service 最多从头重试一次，禁止拼接两个快照的数据。Stats 只服务健康与统计，不参与普通关系查询。

禁止暴露：

```go
ExecuteCypher(...)
QuerySQL(...)
RawSession(...)
```

否则上层会绑定具体后端，Provider 抽象失效。

##### 9.1 查询类型

```go
type Direction string

const (
    DirectionOutgoing Direction = "outgoing"
    DirectionIncoming Direction = "incoming"
    DirectionBoth     Direction = "both"
)

type ResolveQuery struct {
    Text    string
    Classes []Class
    Limit   int
}

type ResolveResult struct {
    Generation string
    Entities   []EntityRef
}

type EntityQuery struct {
    IDs        []string
    Generation string
}

type NeighborQuery struct {
    EntityIDs  []string
    Predicates []Predicate
    Direction  Direction
    Limit      int
    Generation string
}

type PathQuery struct {
    StartID    string
    TargetID   string // 可选；为空时返回预算内的展开路径
    Predicates []Predicate
    Direction  Direction
    MaxDepth   int
    MaxNodes   int
    MaxFanout  int
    Generation string
}
```

所有读取必须有界：

- Resolve Limit：1～20；
- Neighbor Limit：1～200；
- Path MaxDepth：1～5；
- Path MaxNodes：1～500；
- Path MaxFanout：1～100；
- 存储查询使用 `limit+1` 判断 `truncated`；
- 路径遍历每层批量查询 frontier，不能 N+1；
- visited 使用 `map[string]struct{}`；
- 达到任一预算立即停止并返回 `truncated=true`。

##### 9.2 实体解析顺序

实体解析按确定性优先：

1. Entity ID 精确匹配；
2. canonical key 精确匹配；
3. class + normalized alias 精确匹配；
4. class + name 前缀/受限模糊匹配；
5. 返回多个候选，让调用者消歧。

第一阶段不调用 LLM 或向量库解析实体。未来如增加语义解析，只能生成候选，最终仍以 Entity ID 为查询入口。

#### 10. SQLite 默认 Provider

##### 10.1 存储决策

默认继续使用当前结构化 SQLite。它是可重建派生索引，适合本地零依赖运行。Schema 从当前版本升级到新版本时，按既有机制丢弃旧派生库并要求重建，不迁移历史派生数据。

结构化表和本体表写入同一个临时数据库，校验成功后 rename 发布，保证读者只看到完整旧快照或完整新快照。

##### 10.2 Schema 草案

```sql
CREATE TABLE workspace_snapshot (
  singleton_id            INTEGER PRIMARY KEY CHECK (singleton_id = 1),
  generation              TEXT NOT NULL,
  ontology_schema_version INTEGER NOT NULL
);

CREATE TABLE ontology_entities (
  entity_id       TEXT PRIMARY KEY,
  class           TEXT NOT NULL,
  canonical_key   TEXT NOT NULL,
  name            TEXT NOT NULL,
  properties_json TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(properties_json)),
  confidence      REAL NOT NULL
    CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (class, canonical_key)
);

CREATE INDEX idx_ontology_entities_class_name
  ON ontology_entities(class, name, entity_id);

CREATE TABLE ontology_aliases (
  entity_id        TEXT NOT NULL
    REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  normalized_alias TEXT NOT NULL,
  source           TEXT NOT NULL,
  PRIMARY KEY (entity_id, normalized_alias)
);

CREATE INDEX idx_ontology_alias_lookup
  ON ontology_aliases(normalized_alias, entity_id);

CREATE TABLE ontology_facts (
  fact_id          TEXT PRIMARY KEY,
  subject_id      TEXT NOT NULL
    REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  predicate       TEXT NOT NULL,
  object_id       TEXT NOT NULL
    REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  qualifiers_json TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(qualifiers_json)),
  confidence      REAL NOT NULL
    CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (subject_id, predicate, object_id, qualifiers_json)
);

CREATE INDEX idx_ontology_facts_out
  ON ontology_facts(subject_id, predicate, object_id);

CREATE INDEX idx_ontology_facts_in
  ON ontology_facts(object_id, predicate, subject_id);

CREATE TABLE ontology_fact_evidence (
  fact_id     TEXT NOT NULL
    REFERENCES ontology_facts(fact_id) ON DELETE CASCADE,
  file_path   TEXT NOT NULL,
  line        INTEGER NOT NULL CHECK (line >= 0),
  symbol      TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  PRIMARY KEY (fact_id, file_path, line, symbol, source_kind)
);

CREATE INDEX idx_ontology_evidence_path
  ON ontology_fact_evidence(file_path, line, fact_id);
```

##### 10.3 原子发布

结构 Store 提供两个明确生命周期，避免用 nil 或 mode 参数隐藏行为：

```go
func (store *SQLite) ReplaceStructure(
    ctx context.Context,
    generation string,
    bundle domain.IndexBundle,
) error

func (store *SQLite) ReplaceWorkspace(
    ctx context.Context,
    bundle domain.IndexBundle,
    ontologySnapshot ontology.Snapshot,
) (generation string, err error)
```

SQLite Ontology Backend 调用 `ReplaceWorkspace`，由 Store 从完整 Workspace 派生并返回 Generation；Neo4j Backend 调用 `ReplaceStructure` 后发布 Neo4j Snapshot。两个方法共享内部临时数据库写入机制，但对调用方表达不同的不变量，不保留含糊的通用 `ReplaceAll`。

发布流程：

```text
Validate IndexBundle
→ Validate Ontology Snapshot
→ 创建临时 SQLite
→ 写 repositories/services/endpoints/dependencies
→ 写 ontology entities/aliases/facts/evidence
→ PRAGMA foreign_key_check
→ 校验数量和孤儿引用
→ close
→ rename 原子发布
→ 打开新数据库
→ reload 内存依赖图和 Agent 缓存
```

任何一步失败都保留当前在线快照，并删除临时文件。

#### 11. Neo4j 可选 Provider

##### 11.1 定位

Neo4j 是本体查询后端的可选实现，不是本体定义，也不立即替代：

- 结构化 SQLite；
- codegraph；
- Qdrant/Milvus；
- MySQL 业务数据。

只有在复杂跨类型路径、高深度遍历或图算法形成真实需求时，Neo4j Adapter 才产生明显收益。

##### 11.2 配置

建议配置结构：

```go
type OntologyConfig struct {
    Provider string
    Neo4j    Neo4jConfig
}

type Neo4jConfig struct {
    URI      string
    Username string
    Password string
    Database string
}
```

环境配置建议与现有 Semantic Provider 风格一致：

```text
NASUTA_ONTOLOGY_PROVIDER=sqlite

### NASUTA_ONTOLOGY_PROVIDER=neo4j
### NASUTA_ONTOLOGY_NEO4J_URI=neo4j://localhost:7687
### NASUTA_ONTOLOGY_NEO4J_USERNAME=neo4j
### NASUTA_ONTOLOGY_NEO4J_PASSWORD=replace-me
### NASUTA_ONTOLOGY_NEO4J_DATABASE=neo4j
```

密码不能出现在日志、Dashboard 返回或错误详情中。

##### 11.3 显式分发器

```go
func New(
    cfg Config,
    sqliteDB *store.SQLite,
) (ontology.Backend, error) {
    switch cfg.Provider {
    case "sqlite":
        return sqlite.New(sqliteDB), nil
    case "neo4j":
        return neo4j.New(sqliteDB, cfg.Neo4j)
    default:
        return nil, fmt.Errorf(
            "unsupported ontology provider %q", cfg.Provider,
        )
    }
}
```

Neo4j Backend 需要结构 SQLite 的精确引用，只用于 `PublishWorkspace` 发布结构部分和读取当前 Generation；它不能通过该引用执行本体查询或作为 Neo4j 的回退后端。

不能实现：

```go
neo4j, err := openNeo4j(...)
if err != nil {
    return sqlite.New(...) // 禁止静默替换
}
```

##### 11.4 Neo4j 数据模型

Entity 节点：

```cypher
(:OntologyEntity:Service {
  snapshot_id: "snapshot-1",
  entity_id: "svc-uuid",
  canonical_key: "commerce/order\u0000.",
  name: "order-service",
  confidence: 0.95
})
```

Fact 关系：

```cypher
(order)-[:DEPENDS_ON {
  snapshot_id: "snapshot-1",
  fact_id: "fact-uuid",
  protocol: "feign",
  confidence: 0.9
}]->(payment)
```

Evidence 可以作为受 Fact ID 约束的独立节点或关系属性列表。Adapter 必须向上层映射回统一的 `ontology.Fact`，不能泄漏 Neo4j Driver 类型。

##### 11.5 Neo4j 快照发布

Neo4j 不具备 SQLite 文件 rename 语义，且结构数据仍在 SQLite，因此使用 Generation 门控的代际快照：

```text
从 WorkspaceSnapshot.Structure 派生 Generation
→ 原子发布结构 SQLite（不写 ontology_* 事实）
→ 在 Neo4j 创建 building snapshot
→ 带 snapshot_id/generation 批量写 Entity 和 Fact
→ 校验数量、孤儿引用和 Schema Version
→ 一个短事务切换 workspace ACTIVE 指针
→ 查询确认 Neo4j generation 与结构 SQLite generation 一致
→ 异步有界批量删除旧 Snapshot
```

结构 SQLite 发布成功但 Neo4j 发布失败时，旧 Neo4j Snapshot 仍然保留，但其 Generation 与当前结构快照不一致。本体能力必须报告 `stale/unavailable`，不能返回旧图，也不能自动查询 SQLite 本体。现有结构化专用工具仍可读取新结构快照。

查询开始时读取一次 Active Snapshot 和 Generation，并在一次查询中固定使用，不能在多层遍历过程中重新读取 Active 指针。

##### 11.6 禁止双写

不允许：

```text
一次索引同时写 SQLite 和 Neo4j
```

双写会产生半成功、跨版本、删除不一致和重试重复问题。Provider 是单选：

```go
workspace := ontology.WorkspaceSnapshot{
    Structure: bundle,
    Ontology:  snapshot,
}
if err := backend.PublishWorkspace(ctx, workspace); err != nil {
    return fmt.Errorf("publish workspace snapshot: %w", err)
}
```

如需比较 Provider，应在测试或离线影子环境分别运行同一输入，而不是生产双写。

#### 12. 运行时组合与降级

`app.Platform` 在构造期完成组合。Indexing 只持有 `ontology.Publisher`，Agent 只持有 `ontology.Repository`，不能共享一个可下钻的 Backend 容器：

```text
indexing.Build
→ ontology store dispatcher 返回 Backend
→ Publisher 精确注入 indexing.Service
→ Repository 注入 ontology.Service
→ ontology.Service 注入 agent.NewTools
→ 根据 Enabled 注册 query_relations
```

建议状态：

| 场景 | 行为 |
|---|---|
| 默认/显式 SQLite 正常 | 本体能力启用 |
| 显式 Neo4j 正常 | 本体能力启用，只访问 Neo4j |
| 显式 Neo4j 配置缺失 | 返回清晰构造错误或禁用本体能力并 Error 日志 |
| 显式 Neo4j 运行中失败 | 当前请求返回 unavailable，健康状态异常，不改查 SQLite |
| 本体能力不可用 | 现有专用工具继续工作，`query_relations` 不注册或明确不可用 |

是否因本体 Provider 失败终止进程由能力级别决定：第一阶段本体属于可选能力，推荐禁用本体能力并保持现有 QA 可用；但配置失败必须进入健康检查和错误日志，不能只输出 Debug/Warn 后伪装正常。

#### 13. Agent 与查询接口

##### 13.1 新工具

建议新增内部构建工具：

```text
query_relations
```

输入：

```json
{
  "entity": "order-service",
  "entity_class": "service",
  "relations": ["exposes", "depends_on", "documented_by"],
  "direction": "outgoing",
  "max_depth": 2,
  "max_nodes": 50,
  "max_fanout": 20
}
```

输出：

```json
{
  "root": {
    "id": "svc-uuid",
    "class": "service",
    "name": "order-service"
  },
  "entities": [
    {
      "id": "api-uuid",
      "class": "api_endpoint",
      "name": "POST /orders"
    },
    {
      "id": "payment-uuid",
      "class": "service",
      "name": "payment-service"
    }
  ],
  "facts": [
    {
      "id": "fact-api",
      "subject": "order-service",
      "predicate": "exposes",
      "object": "POST /orders",
      "depth": 1,
      "confidence": 0.95,
      "evidence": [
        {
          "path": "repos/commerce/order/OrderController.java",
          "line": 42
        }
      ]
    },
    {
      "id": "fact-dependency",
      "subject": "order-service",
      "predicate": "depends_on",
      "object": "payment-service",
      "depth": 1,
      "qualifiers": {"protocol": "feign"},
      "confidence": 0.9,
      "evidence": [
        {
          "path": "repos/commerce/order/PaymentClient.java",
          "line": 18
        }
      ]
    }
  ],
  "truncated": false
}
```

工具说明必须明确：

- 返回的是当前索引支持的关系，不是运行时实时状态；
- 多跳路径表示可达性，不表示新的直接依赖；
- 空结果不证明关系不存在，只说明当前快照没有证据；
- `truncated=true` 时结果不完整。

##### 13.2 与现有工具的关系

| 工具 | 第一阶段数据源 | 策略 |
|---|---|---|
| `get_service` | 现有结构化索引 | 保持不变 |
| `list_apis` | 现有 endpoints | 保持不变 |
| `trace_deps` | ontology.Service | 已迁移，复用同代际 `depends_on` 路径查询 |
| `trace_calls` | ontology.Service + codegraph/callchain | API 入口由本体解析，方法遍历仍由 CodeGraph 完成 |
| `search_code` | Semantic + BM25 | 保持不变 |
| `query_relations` | ontology.Service | 新增 |

专用工具只迁移本体能够完整表达的部分。Service/API 完整详情、语义内容和方法调用边不复制进通用关系查询。

##### 13.3 Retrieval 集成

第一阶段只把 `query_relations` 作为 Agent read tool，不把全量本体内容注入 Prompt。查询路由可以根据以下信号选择它：

```text
“有哪些关系/关联”
“这个服务暴露的 API 和下游”
“从服务到实现方法”
“哪些 Runbook 描述这个服务”
```

明确 Service 专用依赖问题优先 `trace_deps`，方法级调用问题优先 `trace_calls`；两者都通过本体获得跨层关系入口。

#### 14. 模块改动清单

| 模块 | 改动 | 边界理由 |
|---|---|---|
| `internal/ontology` | 新增本体核心 | 集中拥有语义和查询约束 |
| `internal/domain/types.go` | Runbook 增加服务关联字段 | 入口规范化后保留事实来源 |
| `internal/indexing/indexer/docs.go` | 解析并规范化 Runbook service | 不允许投影层二次读取 DocStore |
| `internal/indexing/indexer/bootstrap.go` | Canonicalize 后投影 Snapshot | 复用确定性扫描，不重复解析 |
| `internal/indexing/service.go` | 编排本体投影和发布 | Indexing 拥有索引生命周期 |
| `internal/platform/store/sqlite.go` | Schema 升级、本体表、原子写入 | SQLite 只拥有存储机制 |
| `internal/platform/ontologystore` | 新增 Provider 分发 | 唯一后端选择点 |
| `internal/platform/graph` | 删除 | `depends_on` 已由 Ontology Repository 统一查询，不保留第二份运行时图 |
| `internal/platform/store/codegraph` | 第一阶段不改 | 完整调用图仍由专用存储拥有 |
| `internal/agent/tools.go` | 精确注入 ontology.Service | 不保留泛型依赖容器 |
| `internal/agent/registry.go` | 条件注册 query_relations | 能力不可用时不伪装可用 |
| `app/platform.go` | 构造并组合 ontology | 根组合点可以知道具体实现 |
| `config`/`.env.example` | 增加 Ontology Provider 配置 | 配置在入口规范化 |
| `knowledge/api.go` | 第一阶段不改 | 不提前固化公开契约 |
| `internal/semantic` | 第一阶段不改 | 向量库不是事实库 |
| Dashboard | 后续可选 | 等查询契约稳定后增加浏览页 |

#### 15. 分阶段重构计划

为保持差异可审查，不进行大爆炸重构。

##### 阶段 0：行为基线

只增加特征测试：

- 固定 Fixture 的 Service/Endpoint/Dependency 数量；
- `trace_deps` 输出；
- `list_apis` 输出；
- Runbook frontmatter 映射；
- 当前 SQLite 快照原子发布和失败保留旧快照。

##### 阶段 1：本体核心

新增：

- Class/Predicate/Entity/Fact/Evidence；
- Schema；
- Stable ID；
- Builder 和 ValidateSnapshot；
- Project(IndexBundle)。

不接数据库、不注册工具。

##### 阶段 2：SQLite Provider

- SQLite Schema 升级；
- 本体表写入；
- Resolve/Neighbors，以及基于 Neighbors 的通用 FindBoundedPaths；
- 与结构化数据同快照发布；
- Repository 查询合同和 Backend 发布合同测试。

##### 阶段 3：运行时组合

- OntologyConfig；
- Provider 分发；
- `ontology.Service`；
- `query_relations` 条件注册；
- Stats/Health。

默认仍使用 SQLite。

##### 阶段 4：影子校验（已完成）

在测试或显式诊断模式比较：

```text
trace_deps vs ontology depends_on
list_apis vs ontology exposes
```

差异只记录，不改变线上回答。需要确认差异来自模型语义、投影错误还是旧工具逻辑。

##### 阶段 5：专用工具逐个迁移（部分完成）

1. `trace_deps`、QA 依赖上下文和 REST 依赖追踪已改用 Ontology Repository，并删除内存 Graph。
2. `trace_calls` 已使用 `APIEndpoint implemented_by CodeSymbol` 解析 API 起点，完整调用边仍由 CodeGraph 查询。
3. `list_apis` 和 `get_service` 保留结构化详情查询；当前本体未承载它们的全部返回字段，不做有损替换。

##### 阶段 6：Neo4j Provider（需求驱动）

交付条件至少满足一项：

- 跨类型 5 层以上路径成为高频查询；
- SQLite 路径查询 P95 明确超过目标；
- 图算法成为产品能力；
- 某部署环境明确要求复用 Neo4j。

实现 Neo4j Adapter、代际快照和共享合同测试，不改变上层 Service/Tool。

#### 16. 迁移、兼容与恢复

##### 16.1 SQLite 派生数据升级

本体数据由结构化索引重新生成，不迁移旧派生 SQLite：

```text
检测旧 schema version
→ 关闭旧数据库
→ 删除派生 db/wal/shm
→ 创建新 Schema
→ 提示/触发明确的结构重建
```

不能在读取路径长期兼容旧 Schema。

##### 16.2 API 与工具兼容

- 现有工具 ID、输入 Schema 和输出保持不变；
- 新增工具不会修改旧工具语义；
- `knowledge.API` 不变；
- 本体内部类型不泄漏到公开包；
- Dashboard 若增加本体接口，使用自包含 DTO。

##### 16.3 发布失败恢复

SQLite：失败保留旧文件快照。

Neo4j：失败 Snapshot 保持 building/failed，Active 指针不切换；清理任务按 Snapshot ID 有界删除。

进程重启后：

- SQLite 打开当前已发布文件；
- Neo4j 只读取 Active Snapshot；
- 非 Active 的 building Snapshot 标记中断并等待清理；
- 不自动把不完整 Snapshot 设为 Active。

#### 17. 可观测性

每次投影和发布记录：

```text
schema_version
provider
entities_total
facts_total
entities_by_class
facts_by_predicate
evidence_total
unresolved_entities
dropped_facts
validation_errors
projection_duration_ms
publish_duration_ms
active_snapshot_id/generation
```

查询记录：

```text
provider
operation
resolved_candidates
predicates
depth
nodes_visited
facts_returned
truncated
duration_ms
status
```

不得记录 Neo4j 密码、完整代码正文、用户问题中的敏感内容或任意大规模路径结果。

建议在 `index_stats` 增加本体能力状态，但不能把 Provider unavailable 伪装成 0 条事实：

```json
{
  "ontology": {
    "enabled": false,
    "provider": "neo4j",
    "status": "unavailable",
    "error": "connection refused"
  }
}
```

#### 18. 测试策略

##### 18.1 Schema 单元测试

- 合法 domain/range；
- 非法关系拒绝；
- 未注册属性/Qualifier 拒绝；
- Confidence 边界；
- 缺失引用；
- 稳定 ID；
- Evidence 去重。

##### 18.2 投影测试

使用多语言 Fixture 验证：

- 每个 Service 生成唯一实体；
- 每个 Endpoint 生成 `exposes`；
- HandlerMethod 稳定时生成 `implemented_by`；
- Service/External Dependency 正确区分；
- 多来源依赖合并 Evidence；
- unresolved Runbook service 不制造 Service；
- 相同输入不同顺序得到相同实体/Fact 集合。

##### 18.3 Repository 与 Backend 合同测试

共享测试包分别验证查询语义和发布生命周期：

```go
func RunRepository(
    t *testing.T,
    newRepository func(*testing.T) ontology.Repository,
) {
    t.Run("resolve_alias", ...)
    t.Run("outgoing_neighbors", ...)
    t.Run("incoming_neighbors", ...)
    t.Run("bounded_path", ...)
    t.Run("cycle_does_not_loop", ...)
    t.Run("evidence_is_preserved", ...)
}

func RunBackend(
    t *testing.T,
    newBackend func(*testing.T) ontology.Backend,
) {
    t.Run("publish_is_atomic_or_generation_gated", ...)
    t.Run("old_snapshot_is_not_visible", ...)
    t.Run("generation_mismatch_is_unavailable", ...)
}
```

SQLite 在普通测试中运行。Neo4j 合同测试需要显式环境开关和已提供的实例，普通测试不得自动启动 Docker。

##### 18.4 Agent 工具测试

- 输入规范化和预算校验；
- 多候选实体消歧输出；
- depth 与基础 Fact 的路径语义；
- truncated 传播；
- unavailable Provider 行为；
- 不泄漏内部存储错误和凭证；
- 现有工具行为特征测试继续通过。

##### 18.5 并发与性能

- 重建发布期间旧快照可读；
- 发布切换后新查询只读新快照；
- 多个查询不持有跨调用可变 Snapshot 指针；
- `go test -race -count=1 ./...`；
- 路径遍历对环和高 Fanout 有明确预算。

#### 19. 性能目标

第一阶段目标基于本地/常规工作区规模：

| 操作 | 目标 |
|---|---|
| Entity ID/alias 精确解析 P95 | < 30ms |
| 一跳 Neighbor P95 | < 50ms |
| 3 层、100 节点 Path P95 | < 200ms |
| 最大返回 Fact | 200 |
| 最大路径节点 | 500 |
| 重复索引 ID 稳定率 | 100% |
| 跨快照混读 | 0 |

这些是验收基线，不是用例特化阈值。若真实数据不满足，应先通过索引、批量查询和边界收敛优化，再以证据决定是否需要 Neo4j。

#### 20. 安全与权限

- 本体查询只读；
- `query_relations` 进入现有 read tool 权限体系；
- Neo4j 使用最小权限账户；
- 凭证只从配置入口读取，不写入 Platform Settings 返回对象之外的日志；
- 查询不接受任意 SQL/Cypher；
- 路径深度、节点、Fanout 和超时均由服务端限制；
- Evidence 输出遵循现有工作区路径授权，不返回文件系统绝对路径；
- 上层应用若未来获得公开查询能力，必须通过自包含 DTO 和认证边界。

#### 21. 风险与应对

| 风险 | 后果 | 应对 |
|---|---|---|
| 本体与现有 domain 类型重复 | 双模型漂移 | 本体只做规范化投影，不重写扫描 DTO |
| 过早通用化接口 | 难用且限制 Neo4j | 只支持 Resolve/Neighbors/Bounded Path |
| LLM 事实污染 | 概率猜测变成确定事实 | 第一阶段禁止 LLM 正式写入 |
| 双写不一致 | Provider 结果分裂 | 单 Provider，禁止生产双写 |
| 多跳被当成直接依赖 | 错误影响分析 | 用 depth 和基础 Fact 明确表达路径 |
| Neo4j 失败静默回退 | 配置与实际行为不一致 | 显式错误和 unavailable，不替换 Provider |
| SQLite 高 Fanout | 查询抖动/内存增长 | 批量 BFS、MaxDepth/Nodes/Fanout |
| Schema 一次扩得过大 | 抽取质量低、维护成本高 | 第一版只覆盖已有稳定数据 |
| 公共 API 过早固化 | 后续难演进 | 先保持 internal，真实第三方需求再公开 |

#### 22. 验收标准

完成第一阶段 SQLite 本体化必须满足：

1. 每个 Service 有且只有一个 `service` Entity。
2. 每个 Endpoint 有且只有一个 `Service exposes APIEndpoint` Fact。
3. 每条规范化 Dependency 对应一个 `depends_on` Fact，协议不丢失。
4. 多来源相同 Fact 合并 Evidence，不重复生成逻辑关系。
5. 未解析目标不会自动创建虚假 Service。
6. 相同输入重复索引后 Entity ID、Fact ID 完全稳定。
7. 所有 Fact 通过 Schema、引用和属性白名单校验。
8. 结构数据和本体数据在同一个 SQLite 快照原子发布。
9. Resolve/Neighbors 有存储层 Limit，FindBoundedPaths 有服务层深度、节点和 Fanout 预算。
10. 多跳结果携带深度和基础事实，不伪装为直接事实。
11. `query_relations` 返回 Evidence、Confidence 和 truncated。
12. Provider 配置只有一个显式分发点，无静默后端替换。
13. 本体不可用时现有 QA 专用工具继续工作，状态可观察。
14. `get_service`、`list_apis`、`trace_deps`、`trace_calls` 行为不回归。
15. `go build ./...`、`go vet ./...`、相关单元/合同/集成测试和 `-race` 通过。

Neo4j Provider 交付还必须满足：

1. 运行与 SQLite 相同的 Repository 查询合同和 Backend 发布合同测试；
2. 使用代际 Snapshot，发布过程中不跨代读取；
3. 显式 Neo4j 失败不访问 SQLite 本体；
4. 查询结果在统一排序、预算和 Evidence 语义上与 SQLite 一致；
5. 不要求生产双写即可运行和恢复。

#### 23. 后续演进

本设计稳定后，可以按真实需求扩展：

1. `Database`、`DatabaseTable` 和 `reads_from/writes_to`；
2. `MessageTopic` 和 `publishes_to/consumes_from`；
3. `Incident affects Service`、`resolved_by Runbook`；
4. 通过 codegraph ID 加强 `APIEndpoint implemented_by CodeSymbol`；
5. 实体语义候选解析，但最终仍解析为稳定 Entity ID；
6. Dashboard 本体浏览、路径和证据视图；
7. 在明确第三方需求出现后，设计公开只读 Ontology API；
8. 有性能或图算法证据后启用 Neo4j Provider。

任何扩展都必须先明确新的 Class、Predicate、domain/range、身份、证据来源、抽取入口和查询消费者；不能只增加一个字符串关系或在下游临时兼容。

#### 24. 最终建议

Nasuta 的本体化应从“统一现有确定性知识的语义和证据”开始，而不是从“引入图数据库”开始。默认 SQLite 能以最低部署成本完成第一阶段目标；Repository 边界和合同测试为未来 Neo4j 保留了可验证的替换能力。

推荐实施顺序：

```text
行为基线
→ 本体 Schema/Identity/Projection
→ SQLite 原子快照和 Repository
→ query_relations
→ 影子对比
→ 专用工具逐个迁移
→ 真实需求驱动 Neo4j Adapter
```

这一路线将本体定义、数据抽取、存储 Provider、Agent 查询和公开边界拆开，使每一阶段都可以独立验证、回滚和评审。

### Docs 源文件存储设计

> Migrated from Nasuta `docs/design/docs-source-file-storage.zh-CN.md`; incorporated into this module on 2026-07-31.

#### 1. 状态与范围

- 状态：设计稿
- 范围：Docs 源文件的保存、可访问性校验、读取、重新解析和删除
- 支持：本地 workspace、S3/MinIO、阿里云 OSS
- 核心原则：系统只配置一个当前存储，一份文档只写入一个位置

`documents.content` 继续保存标准化后的 UTF-8 Markdown，并作为文档详情、切块、Embedding 和检索的输入。源文件独立保存，用于下载、重新解析和解析器升级。

#### 2. 设计约束

1. 存储配置只存在于应用配置文件或对应环境变量中，不允许用户在数据库中维护多套存储配置。
2. 运行实例同一时刻只有一个当前存储提供方。
3. `document_sources` 不保存 `storage_config_id`，只保存文件类型、位置和上传时的可访问性校验结果。
4. 本地文件固定保存到 `<workspace>/document/asserts`，并按日期分目录。
5. 云端保存稳定对象 URL，不保存会过期的 presigned URL。
6. 切换配置只影响切换后的上传，不迁移、不复制历史文件。
7. 云端源文件不随 Docs 文档删除而物理删除；本地源文件可以删除。
8. 配置错误或存储不可达必须明确报错，禁止静默切换提供方。

#### 3. 目标与非目标

##### 3.1 目标

- 保存 Markdown、PDF、DOCX、粘贴文本、URL 导入和系统生成内容的源文件。
- 每次上传完成前验证最终路径确实可读取。
- 本地路径在容器重启后仍可通过 workspace 挂载解析。
- 支持 S3、MinIO 和 OSS 的单提供方部署。
- 上传和读取流式处理，并设置明确的资源上限。

##### 3.2 非目标

- 不保存多套用户存储配置。
- 不通过数据库保留历史云 endpoint 或历史凭据。
- 不保证云存储配置切换后，旧云文件仍能被当前实例读取。
- 不支持同一文件同时写入多个提供方。
- 不设计存储迁移、复制或自动回填到新提供方。
- 首期不永久保存 PDF/DOCX 中提取出的每一张图片。

#### 4. 当前存储配置

存储配置由应用配置入口一次性规范化，运行时业务代码直接信任配置不变量。配置结构只表达一个当前提供方，不使用配置数组。

本地示例：

```yaml
document_storage:
  type: local
```

S3 或 MinIO 示例：

```yaml
document_storage:
  type: s3
  endpoint: https://minio.example.com
  region: us-east-1
  bucket: docs
  prefix: documents
  path_style: true
  access_key: ${DOCUMENT_STORAGE_ACCESS_KEY}
  secret_key: ${DOCUMENT_STORAGE_SECRET_KEY}
```

OSS 示例：

```yaml
document_storage:
  type: oss
  endpoint: https://oss-cn-hangzhou.aliyuncs.com
  region: cn-hangzhou
  bucket: docs
  prefix: documents
  access_key: ${DOCUMENT_STORAGE_ACCESS_KEY}
  secret_key: ${DOCUMENT_STORAGE_SECRET_KEY}
```

实际键名应在实现时遵循 Nasuta 的配置命名规范。密钥只能通过环境变量或密钥挂载注入，不能写入数据库、日志或前端响应。

启动校验包括：

- `type` 只能是 `local`、`s3`、`oss`。
- 本地模式不接受云端专用字段。
- S3/OSS 必须提供 endpoint、bucket 和凭据。
- prefix 规范化后不能以 `/` 开头，也不能包含目录穿越片段。
- 显式配置的存储初始化失败时，相关上传能力不可用并记录明确错误。

#### 5. 源文件定位模型

每份文档只保存以下定位信息：

```text
storage_type  local | s3 | oss
source_path   workspace 相对路径或稳定对象 URL
```

示例：

```text
local  document/asserts/2026/07/26/aaa-01j3....pdf
s3     https://minio.example.com/docs/documents/2026/07/26/aaa-01j3....pdf
s3     https://docs.s3.us-east-1.amazonaws.com/documents/2026/07/26/aaa-01j3....pdf
oss    https://docs.oss-cn-hangzhou.aliyuncs.com/documents/2026/07/26/aaa-01j3....pdf
```

`source_path` 在上传成功后不可修改。本地保存相对路径，避免绑定机器绝对路径；云端保存不带临时签名参数的稳定 URL。

数据库不保存 `is_accessible`。可访问性是实时属性，持久化布尔值会很快失真。系统只记录最近一次成功校验时间 `access_verified_at`，并在每次实际读取失败时返回当前错误。

#### 6. 日期目录与文件名

本地根目录固定为：

```text
<workspace>/document/asserts
```

本地物理路径格式为：

```text
<workspace>/document/asserts/YYYY/MM/DD/<filename>
```

数据库保存的相对路径为：

```text
document/asserts/YYYY/MM/DD/<filename>
```

例如：

```text
<workspace>/document/asserts/2026/07/26/aaa-01j3....pdf
```

规则如下：

1. 日期取服务端统一配置时区中的上传时间，同一次上传只计算一次。
2. 文件名保留清洗后的原始主文件名，并追加文档 ID 或同等强度的唯一后缀。
3. 扩展名规范为小写，原始文件名单独保存到 `original_filename`。
4. 去除路径分隔符、控制字符和 `.`、`..` 等危险片段，客户端不能决定目录。
5. 云端 object key 使用相同的 `documents/YYYY/MM/DD/<filename>` 日期规则。
6. 写入使用不覆盖语义；若唯一后缀仍发生冲突，则重新生成后重试。

#### 7. 存储接口与分发

存储能力保持最小接口：

```go
type SourceStore interface {
	Put(ctx context.Context, objectKey string, body io.Reader, size int64, contentType string) (string, error)
	Open(ctx context.Context, sourcePath string) (io.ReadCloser, error)
	Stat(ctx context.Context, sourcePath string) (SourceInfo, error)
}
```

`Put` 返回最终 `source_path`，`Stat` 用于确认对象存在、大小正确且当前配置可访问。云端物理删除不属于通用接口；本地文件删除由本地实现单独提供。

提供方通过一个显式 dispatcher 分发：

```text
local -> put/open/statLocal
s3    -> put/open/statS3
oss   -> put/open/statOSS
其他值 -> 明确报错
```

S3 和 MinIO 共享 S3 协议实现，OSS 使用独立实现。不得在某个提供方失败后尝试其他提供方。

#### 8. 上传与可访问性校验

上传流程：

1. 在 HTTP 入口规范化文件名、MIME、扩展名和大小。
2. 读取唯一的当前存储配置，生成日期 object key。
3. 流式写入当前存储，同时计算 SHA-256 和实际字节数。
4. 使用同一个 provider 对返回的 `source_path` 执行 `Stat`。
5. 校验对象大小与实际上传大小一致；需要时抽样打开文件验证读取权限。
6. 解析源文件并生成规范化 Markdown。
7. 在数据库事务中写入 `documents`、`document_sources` 和索引任务。

只有第 4、5 步成功，上传才算成功，`access_verified_at` 记录此次校验时间。校验失败时返回明确的 provider、路径和原因，不创建可见文档记录。

上传入口必须使用 `io.LimitReader` 或等价机制设置硬上限，禁止无上限 `io.ReadAll`。同时校验客户端声明大小、实际写入大小和存储端返回大小。

数据库事务失败时，可以在当前请求仍持有当前存储客户端期间清理本次新写入对象。该补偿只处理尚未形成有效文档的上传，不改变“云端文档源文件不随 Docs 文档删除”的语义。

#### 9. 本地存储

本地写入流程：

1. 生成 `document/asserts/YYYY/MM/DD/<filename>` 相对路径。
2. 将相对路径与规范化后的 `WorkspaceRoot` 安全拼接。
3. 确认最终路径仍位于 `<workspace>/document/asserts` 内。
4. 在目标目录创建临时文件，流式写入并计算 SHA-256。
5. 同目录原子重命名为最终文件名。
6. 重新打开最终文件完成可访问性校验。

读取时使用 `<workspace>/<source_path>`。本地根目录固定，因此当前云配置如何变化都不影响本地历史文件的定位。

#### 10. S3 与 MinIO

`s3` 提供方同时覆盖 AWS S3 和兼容 S3 API 的 MinIO。`Put` 完成后生成不带签名参数的稳定 HTTPS 对象 URL，`Stat` 使用当前 S3 配置和签名请求确认对象可访问。

私有 bucket 不得为了得到可直接访问 URL 而改成公开读。下载时使用当前凭据读取并由 Docs API 流式返回；如需临时直链，在请求时动态签发，不能覆盖数据库中的稳定 `source_path`。

#### 11. 阿里云 OSS

OSS 使用独立的 `oss` provider 和 SDK，不通过 S3 实现伪装。`Put` 返回稳定 OSS 对象 URL，`Stat` 使用当前 OSS 配置验证对象存在、大小正确且可读取。

OSS 故障必须直接暴露，不允许静默改用 S3 或本地存储。

#### 12. 数据模型

只新增源文件表，不新增存储配置表：

```sql
CREATE TABLE document_sources (
  document_id         VARCHAR(64)   NOT NULL,
  storage_type       VARCHAR(16)   NOT NULL,
  source_path        VARCHAR(2048) NOT NULL,
  original_filename  VARCHAR(512)  NOT NULL,
  source_origin      VARCHAR(32)   NOT NULL,
  mime_type          VARCHAR(255)  NOT NULL,
  extension          VARCHAR(32)   NOT NULL,
  sha256             CHAR(64)      NOT NULL,
  size_bytes         BIGINT        NOT NULL,
  parser_version     VARCHAR(64)   NULL,
  source_url         VARCHAR(2048) NULL,
  access_verified_at DATETIME(6)   NOT NULL,
  created_at         DATETIME(6)   NOT NULL,
  updated_at         DATETIME(6)   NOT NULL,
  PRIMARY KEY (document_id),
  CONSTRAINT fk_document_sources_document
    FOREIGN KEY (document_id) REFERENCES documents(id),
  CONSTRAINT chk_document_sources_storage
    CHECK (storage_type IN ('local', 's3', 'oss')),
  CONSTRAINT chk_document_sources_size CHECK (size_bytes >= 0)
);
```

`source_origin` 使用 `upload`、`paste`、`url_import`、`generated`、`legacy_content`。它只描述源文件如何产生，不参与存储路由。

表中不包含：

- `storage_config_id`
- endpoint、bucket、region 或凭据
- 容易失真的 `is_accessible` 布尔状态

#### 13. 配置切换后的行为

配置文件变更并重启后，新上传只使用新的当前配置。历史 `document_sources` 不修改，也不会被复制到新存储。

读取规则：

```text
local
  -> 始终按 WorkspaceRoot + source_path 读取

s3 / oss
  -> source_path 的 provider 与当前配置一致时，使用当前凭据读取
  -> provider 不一致或当前凭据无权访问时，返回源文件不可用
```

系统不保存历史 endpoint 和凭据，因此不能承诺切换后继续读取旧云对象。稳定对象 URL 仍保留在数据库中，但其实际可访问性取决于当前网络、对象是否存在以及当前配置是否拥有权限。

如果业务未来重新要求“切换后旧云文件必须可读”，就必须在“保留历史配置”或“迁移旧文件”中选择一种；仅保存 URL 无法为私有对象恢复旧凭据。该能力不属于本设计。

#### 14. 各类来源处理

| 来源 | 保存的源文件 | `source_origin` |
|---|---|---|
| Markdown 上传 | 原始 `.md` | `upload` |
| PDF/DOCX 上传 | 原始二进制文件 | `upload` |
| 粘贴文本 | 生成 UTF-8 `.md` | `paste` |
| URL 导入 | 原始响应体，保留识别出的类型 | `url_import` |
| 系统生成 Markdown | 生成的 `.md` | `generated` |
| 历史文档回填 | 从 `documents.content` 生成 `.md` | `legacy_content` |

URL 导入额外保存原始 URL 到 `source_url`。它描述内容来源，不替代源文件的 `source_path`。

#### 15. PDF、DOCX 与图片

PDF/DOCX 原文件永久保存在上传时的目标位置。解析时将源文件流式写入受控临时目录，完成文本和图片提取后删除临时文件。

首期图片策略：

1. 提取出的图片仅用于本次解析、OCR 或多模态理解。
2. 不为每张图片创建永久对象，也不将图片二进制写入数据库。
3. Markdown 中不得写入即将失效的临时文件路径。
4. 后续若需要文档内图片预览，应另行设计图片资产表和稳定引用。

源 PDF/DOCX 保留后，未来仍可使用新解析器重新提取图片。

#### 16. 下载、重新解析与删除

##### 16.1 下载

下载接口读取 `document_sources`：

- 本地文件按固定 workspace 根目录打开。
- 云文件仅在当前 provider 和凭据可以访问该 URL 时打开。
- 下载响应使用 `original_filename`，不从物理路径反推展示名称。

每次读取失败都返回实时错误，不使用 `access_verified_at` 代替实际访问。

##### 16.2 重新解析

重新解析从 `source_path` 读取源文件。成功后更新 `documents.content`、`parser_version`、切块和向量索引，但不修改源文件位置。无法访问历史云文件时明确失败，不回退到其他存储。

##### 16.3 删除

删除 Docs 文档时：

- 本地源文件可以随文档删除。
- 云端源文件不执行物理删除，只删除 Docs 数据、切块和向量索引。

本地删除采用数据库事务内写删除 outbox、事务外删除文件的方式，避免数据库与文件系统不一致。对象不存在视为幂等成功，其他错误按退避策略重试。

#### 17. 本地删除 Outbox

删除 outbox 只承担本地文件清理，不保存云配置：

```sql
CREATE TABLE document_source_delete_outbox (
  id               BIGINT        NOT NULL AUTO_INCREMENT,
  document_id      VARCHAR(64)   NOT NULL,
  source_path      VARCHAR(2048) NOT NULL,
  status           VARCHAR(16)   NOT NULL,
  attempt_count    INT           NOT NULL DEFAULT 0,
  next_attempt_at  DATETIME(6)   NOT NULL,
  last_error       TEXT          NULL,
  created_at       DATETIME(6)   NOT NULL,
  updated_at       DATETIME(6)   NOT NULL,
  PRIMARY KEY (id),
  KEY idx_document_source_delete_due (status, next_attempt_at)
);
```

任务按有界批次领取。worker 只接受 `document/asserts/` 下经过边界校验的相对路径，拒绝其他目标。

#### 18. 安全与资源限制

1. 所有外部文件名、MIME、扩展名和 URL 在入口只规范化一次。
2. 本地路径必须经过 `filepath.Clean` 和根目录边界检查，拒绝绝对路径、软链接逃逸和目录穿越。
3. 云端上传返回的 URL 必须匹配当前配置的 endpoint、bucket 和 object key，不能接受客户端提供的对象 URL。
4. 下载权限沿用 Docs 访问控制，不能因数据库保存了 URL 而绕过鉴权。
5. 云 bucket 默认私有，临时下载 URL 必须短时有效。
6. 日志不得输出凭据、签名参数或完整敏感 URL 查询串。
7. 限制单文件大小、解析文本大小、压缩包展开大小、页数、图片数量、图片像素和解析时长。
8. 校验文件签名与声明类型，DOCX 必须防止 Zip Slip 和压缩炸弹。
9. SHA-256 用于完整性校验和审计，不默认用于跨文档复用或删除对象。

#### 19. 历史数据回填

历史 `documents` 没有源文件时，从 `documents.content` 生成 UTF-8 Markdown，并写入执行回填时的当前存储：

```text
source_origin = legacy_content
original_filename = <document-title>.md
```

回填任务按主键游标分页，单批有界，逐条执行上传和 `Stat` 校验。已有 `document_sources` 的文档跳过，失败记录原因并允许继续。

#### 20. 测试与验收标准

##### 20.1 单元测试

- 配置入口只接受一个当前 provider，并拒绝不完整配置。
- 日期目录在月末、年末和指定时区下正确生成。
- 同名上传生成不同路径，原始文件名仍可用于下载。
- 路径穿越、绝对路径和软链接逃逸被拒绝。
- dispatcher 对 `local`、`s3`、`oss` 路由正确，对未知类型明确报错。
- `Put` 后 `Stat` 失败时不创建可见文档。
- 云 URL 不接受 presigned 参数或与当前 endpoint/bucket 不一致的地址。
- 文件大小边界、哈希和流式复制行为正确。

##### 20.2 集成测试

- 本地文件写入 `workspace/document/asserts/YYYY/MM/DD`，重启后仍可下载和重新解析。
- S3、MinIO、OSS 分别完成上传、`Stat`、下载和失败反馈。
- 配置切换后新文件只进入新存储，不复制或修改历史记录。
- 历史云文件与当前配置不匹配时返回明确不可用错误。
- 删除本地文档会清理本地文件，删除云文档不会删除云对象。
- PDF/DOCX 解析完成后临时文件被清理，源文件保持可用。

##### 20.3 验收结论

满足以下条件即完成：

1. 系统不创建存储配置表，也不在数据库保存云凭据。
2. `document_sources` 不包含 `storage_config_id`。
3. 本地路径符合 `workspace/document/asserts/YYYY/MM/DD/<filename>`。
4. 每次上传只有通过最终路径可访问性校验后才成功。
5. 配置切换只影响新上传，不迁移、不复制历史文件。
6. 云文件不随 Docs 删除，本地文件可通过受控 outbox 删除。
