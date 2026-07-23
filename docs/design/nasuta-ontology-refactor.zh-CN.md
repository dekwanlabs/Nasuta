# Nasuta 本体化重构设计

> 状态：Proposal
>
> 范围：设计与实施计划，不包含本轮代码实现

## 1. 摘要与决策

Nasuta 当前已经拥有 `ServiceRecord`、`EndpointRecord`、`DependencyEdge`、Runbook、代码符号、证据和依赖图。这些结构表达了领域事实，但类型含义、关系约束、实体身份、跨类型查询和证据规则仍分散在索引器、SQLite、内存图和 Agent 工具中，属于“隐式本体”。

本设计将这些隐式语义提升为显式、可验证、可查询的工作区本体，并作出以下决策：

1. 新增 `internal/ontology`，集中拥有实体类型、关系类型、约束、稳定身份、投影和查询语义。
2. 第一阶段只覆盖现有确定性扫描能够稳定产出的概念，不使用 LLM 自动制造事实。
3. 默认使用当前结构化 SQLite 保存本体快照；结构数据和本体数据在同一个临时数据库中校验并原子发布。
4. 通过 `ontology.Repository` 保留 Neo4j Provider 扩展能力，但运行时只允许一个本体后端，不进行 SQLite/Neo4j 双写。
5. Provider 使用一个显式分发器。显式配置的 Neo4j 失败必须可观察，不能静默替换为 SQLite。
6. Qdrant/Milvus 继续负责语义召回，codegraph 继续负责完整的方法调用关系；两者都不是本体事实主存储。
7. 第一阶段新增通用关系查询，不替换 `get_service`、`list_apis`、`trace_deps` 和 `trace_calls`。经过影子校验后，再按专用工具逐个迁移。
8. 本体 Go 契约先保持内部，不增加面向上层应用的公开注册入口。

本次重构的直接收益是统一语义、证据和查询边界，降低后续增加 Database、MessageTopic、Incident 等概念的成本。它不会仅凭引入“本体”或 Neo4j 自动提高抽取准确率；准确率仍由确定性抽取、实体解析和证据质量决定。

## 2. 背景与现状

### 2.1 当前数据能力

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
| 服务依赖遍历 | `internal/platform/graph` |
| 方法调用链 | `internal/platform/store/codegraph`、`internal/callchain` |
| 向量与混合检索 | `internal/semantic`、`internal/retrieval` |
| Agent 专用工具 | `internal/agent` |
| 上层稳定只读能力 | `knowledge.API` |

### 2.2 现有问题

1. `Service`、`API`、`Dependency` 的语义依赖具体字段和工具说明，没有统一 Schema。
2. 相同关系可能分别存在于结构表、内存图、Runbook 和 codegraph 中，缺少共同的事实标识与证据模型。
3. Agent 能分别调用专用工具，但无法表达“服务 → API → Symbol → Runbook”这类跨类型查询。
4. 当前关系字段允许代码构造出语义错误的数据，主要依赖各扫描器自律。
5. 新增概念时容易把抽取、存储、查询和工具输出一起耦合修改。
6. 如果未来引入 Neo4j，而上层直接依赖 SQL 或 SQLite 类型，迁移会扩散到 Agent、Retrieval、Dashboard 和索引生命周期。

### 2.3 命名约定

本设计中的术语：

- **Entity**：具有稳定身份的领域对象，例如 Service、APIEndpoint。
- **Fact**：两个 Entity 之间的一条有向关系，例如 `Service depends_on Service`。
- **Predicate**：Fact 的关系类型，例如 `depends_on`。
- **Evidence**：支持 Entity 或 Fact 的代码/文档位置。
- **Snapshot**：一次索引生成并原子发布的完整实体和事实集合。
- **Direct Fact**：由扫描或受控文档直接支持的事实。
- **Derived Path**：查询时根据多条 Direct Fact 得到的可达路径，不保存为新的直接事实。

## 3. 目标与非目标

### 3.1 目标

1. 给现有结构化知识建立单一的类型和关系语义。
2. 所有实体使用稳定 ID，重复索引不改变身份。
3. 所有事实经过 domain/range、属性白名单和引用完整性校验。
4. 每条非结构性事实携带可回溯证据和置信度。
5. 支持实体解析、一跳邻接、反向邻接和有界路径查询。
6. 本体与结构化索引使用一致的快照，不能跨版本读取。
7. 默认不增加本地部署依赖。
8. 保留可测试的 Neo4j Provider 扩展点。
9. 迁移过程不改变现有专用工具的可观察行为。

### 3.2 非目标

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

## 4. 设计原则

### 4.1 事实来源优先级

```text
AST/结构扫描
  > 明确配置和代码注解
  > 人工维护的受控 Frontmatter
  > 生成文档
  > LLM 候选
```

LLM 候选在未来也必须经过 Schema 校验、实体解析、原始证据确认和人工/确定性规则批准，才能成为正式事实。

### 4.2 规范化只发生在入口

扫描器和文档解析器负责建立规范：

- HTTP Method 大写；
- API Path 具有前导 `/`；
- Repo、ModulePath、文件路径使用既有规范；
- Service 使用已经生成的 `ServiceKey`；
- 外部目标在投影入口规范化；
- 别名在写入前生成规范形式。

进入 `ontology.Entity` 和 `ontology.Fact` 后，下游不再重复 Trim、Lower、兼容旧别名或补默认值。非法数据必须失败并指出来源。

### 4.3 直接事实与推导分离

假设有：

```text
order-service depends_on payment-service
payment-service depends_on mysql
```

系统可以查询到 `order-service → payment-service → mysql`，但不能持久化一条新的直接事实 `order-service depends_on mysql`。查询结果必须标识路径深度和基础 Fact ID。

反向关系也不重复存储。例如 `depended_on_by` 由 `depends_on` 的反向查询得到，避免两个方向漂移。

### 4.4 单 Provider、无静默替换

运行时只能有一个本体 Repository：

```text
provider=sqlite → 只查询和发布 SQLite 本体
provider=neo4j  → 只查询和发布 Neo4j 本体
```

Neo4j 配置错误或不可达时，本体能力应进入明确的 unavailable 状态并记录错误；不能自动改查 SQLite 后声称 Neo4j 正常。

现有专用工具继续读取原有结构化 SQLite/内存图，不属于 Provider 替换，因为它们是尚未迁移的独立能力。

## 5. 第一版本体模型

### 5.1 Entity Class

| Class | 稳定身份 | 来源 | 主要属性 |
|---|---|---|---|
| `repository` | repo | RepositoryRecord | repo、head_sha |
| `service` | 现有 ServiceKey | ServiceRecord | repo、module_path、language、owner、runtime |
| `api_endpoint` | serviceKey + method + path | EndpointRecord | method、path、file、handler |
| `code_symbol` | repo + file + qualified/handler name | EndpointRecord/codegraph 引用 | file、qualified_name、language |
| `external_system` | 规范化 target | DependencyEdge | target、protocol_hint |
| `runbook` | runbook ID | RunbookRecord | title、path、scope、tags |

第一版不创建 Database、DatabaseTable、MessageTopic、Incident、LogEvent、TraceSpan。它们必须在确定性抽取和实际查询需求成熟后单独加入。

### 5.2 Predicate

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

### 5.3 核心类型草案

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

### 5.4 Schema 约束

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

## 6. 稳定身份与去重

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
    return platform.UUIDFromString(
        "external_system\x00" + normalizeExternalTarget(target),
    )
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

## 7. 投影与抽取流水线

### 7.1 不重复扫描

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

    snapshot := builder.Build()
    return snapshot, ValidateSnapshot(snapshot)
}
```

### 7.2 Runbook 模型调整

当前 `RunbookRecord` 未保存 frontmatter 中的 `service:`，投影层不能再次读取 DocStore。应在文档入口扩展：

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

### 7.3 CodeSymbol 边界

第一阶段只为能够从 Endpoint 的 `HandlerMethod` 和文件位置稳定识别的 Symbol 创建引用实体。完整的 `Symbol calls Symbol` 仍由 codegraph 拥有：

```text
Ontology: APIEndpoint implemented_by CodeSymbolRef
Codegraph: CodeSymbol calls CodeSymbol
```

如果 HandlerMethod 为空或身份不稳定，只保留 API Entity 的 handler/file 属性，不生成 `implemented_by` 事实。不能用名称猜测 Symbol。

### 7.4 明确不进入本体的数据

以下内容保留为 Evidence 或专用存储，不创建高基数 Entity：

- 原始代码 Chunk；
- 每条日志；
- 每个 Trace Span；
- 每次 QA Run/LLM Call；
- 每次索引过程事件；
- 向量命中结果。

未来增加 Incident 时，应建 Incident 聚合实体并链接受影响 Service；原始日志和 Span 仍只作为 Incident Evidence。

## 8. 包与依赖边界

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

## 9. Repository 查询契约

查询和发布是两个不同生命周期：Agent 查询只需要 `Repository`；Indexing 发布需要能够同时处理结构快照和本体快照的 `Publisher`。分开定义可以避免查询服务获得重建权限，也解决 SQLite 同库原子发布与 Neo4j 跨存储发布的差异。

查询接口围绕 Nasuta 的业务需求定义，不照搬 SQLite 或 Neo4j API：

```go
type Repository interface {
    Resolve(context.Context, ResolveQuery) ([]Entity, error)
    EntitiesByID(context.Context, EntityQuery) ([]Entity, error)
    Neighbors(context.Context, NeighborQuery) ([]Fact, bool, error)
    FindPaths(context.Context, PathQuery) ([]Path, bool, error)
    Stats(context.Context) (Stats, error)
    Close() error
}
```

`EntitiesByID` 只用于关系查询完成后批量补齐 Entity 名称和 Class，最多接收 200 个 ID。它避免 Tool 层按路径逐个解析形成 N+1 查询，不是通用实体扫描接口。

发布契约：

```go
type WorkspaceSnapshot struct {
    Generation string
    Structure  domain.IndexBundle
    Ontology   Snapshot
}

type Publisher interface {
    PublishWorkspace(context.Context, WorkspaceSnapshot) error
}

type Backend interface {
    Repository
    Publisher
}
```

`Generation` 由排序后的 `repo + HEAD SHA` 集合确定性生成。SQLite Backend 在一个临时数据库中发布两部分；Neo4j Backend 协调结构 SQLite 和 Neo4j 代际快照，并用 Generation 阻止跨版本本体查询。

一次关系查询先从 `Stats` 固定 Generation，随后把 Generation 传给 Resolve、Neighbors 和 EntitiesByID。若发布恰好发生在调用之间，Repository 返回 `ErrStaleSnapshot`，Service 最多从头重试一次，禁止拼接两个快照的数据。

禁止暴露：

```go
ExecuteCypher(...)
QuerySQL(...)
RawSession(...)
```

否则上层会绑定具体后端，Provider 抽象失效。

### 9.1 查询类型

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

type NeighborQuery struct {
    EntityIDs  []string
    Predicates []Predicate
    Direction  Direction
    Limit      int
}

type PathQuery struct {
    StartID    string
    TargetID   string // 可选；为空时返回预算内的展开路径
    Predicates []Predicate
    Direction  Direction
    MaxDepth   int
    MaxNodes   int
    MaxFanout  int
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

### 9.2 实体解析顺序

实体解析按确定性优先：

1. Entity ID 精确匹配；
2. canonical key 精确匹配；
3. class + normalized alias 精确匹配；
4. class + name 前缀/受限模糊匹配；
5. 返回多个候选，让调用者消歧。

第一阶段不调用 LLM 或向量库解析实体。未来如增加语义解析，只能生成候选，最终仍以 Entity ID 为查询入口。

## 10. SQLite 默认 Provider

### 10.1 存储决策

默认继续使用当前结构化 SQLite。它是可重建派生索引，适合本地零依赖运行。Schema 从当前版本升级到新版本时，按既有机制丢弃旧派生库并要求重建，不迁移历史派生数据。

结构化表和本体表写入同一个临时数据库，校验成功后 rename 发布，保证读者只看到完整旧快照或完整新快照。

### 10.2 Schema 草案

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

### 10.3 原子发布

结构 Store 提供两个明确生命周期，避免用 nil 或 mode 参数隐藏行为：

```go
func (store *SQLite) ReplaceStructure(
    ctx context.Context,
    generation string,
    bundle domain.IndexBundle,
) error

func (store *SQLite) ReplaceWorkspace(
    ctx context.Context,
    generation string,
    bundle domain.IndexBundle,
    ontologySnapshot ontology.Snapshot,
) error
```

SQLite Ontology Backend 调用 `ReplaceWorkspace`；Neo4j Backend 调用 `ReplaceStructure` 后发布 Neo4j Snapshot。两个方法共享内部临时数据库写入机制，但对调用方表达不同的不变量，不保留含糊的通用 `ReplaceAll`。

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

## 11. Neo4j 可选 Provider

### 11.1 定位

Neo4j 是本体查询后端的可选实现，不是本体定义，也不立即替代：

- 结构化 SQLite；
- codegraph；
- Qdrant/Milvus；
- MySQL 业务数据。

只有在复杂跨类型路径、高深度遍历或图算法形成真实需求时，Neo4j Adapter 才产生明显收益。

### 11.2 配置

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
ONTOLOGY_PROVIDER=sqlite

# ONTOLOGY_PROVIDER=neo4j
# ONTOLOGY_NEO4J_URI=neo4j://localhost:7687
# ONTOLOGY_NEO4J_USERNAME=neo4j
# ONTOLOGY_NEO4J_PASSWORD=replace-me
# ONTOLOGY_NEO4J_DATABASE=neo4j
```

密码不能出现在日志、Dashboard 返回或错误详情中。

### 11.3 显式分发器

```go
func New(
    cfg Config,
    sqliteDB *store.SQLite,
) (ontology.Backend, error) {
    switch cfg.Provider {
    case "", "sqlite":
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

### 11.4 Neo4j 数据模型

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

### 11.5 Neo4j 快照发布

Neo4j 不具备 SQLite 文件 rename 语义，且结构数据仍在 SQLite，因此使用 Generation 门控的代际快照：

```text
生成 WorkspaceSnapshot Generation
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

### 11.6 禁止双写

不允许：

```text
一次索引同时写 SQLite 和 Neo4j
```

双写会产生半成功、跨版本、删除不一致和重试重复问题。Provider 是单选：

```go
workspace := ontology.WorkspaceSnapshot{
    Generation: generationFor(bundle.Repositories),
    Structure:  bundle,
    Ontology:   snapshot,
}
if err := backend.PublishWorkspace(ctx, workspace); err != nil {
    return fmt.Errorf("publish workspace snapshot: %w", err)
}
```

如需比较 Provider，应在测试或离线影子环境分别运行同一输入，而不是生产双写。

## 12. 运行时组合与降级

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

## 13. Agent 与查询接口

### 13.1 新工具

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
      "direct": true,
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
      "direct": true,
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

### 13.2 与现有工具的关系

| 工具 | 第一阶段数据源 | 策略 |
|---|---|---|
| `get_service` | 现有结构化索引 | 保持不变 |
| `list_apis` | 现有 endpoints | 保持不变 |
| `trace_deps` | 现有内存 Graph | 保持不变，影子对比 |
| `trace_calls` | codegraph/callchain | 保持不变 |
| `search_code` | Semantic + BM25 | 保持不变 |
| `query_relations` | ontology.Service | 新增 |

稳定后可以逐个让专用工具复用本体 Repository，但每次迁移必须有行为特征测试，不能一次性替换全部查询。

### 13.3 Retrieval 集成

第一阶段只把 `query_relations` 作为 Agent read tool，不把全量本体内容注入 Prompt。查询路由可以根据以下信号选择它：

```text
“有哪些关系/关联”
“这个服务暴露的 API 和下游”
“从服务到实现方法”
“哪些 Runbook 描述这个服务”
```

明确 Service 专用依赖问题仍优先 `trace_deps`，方法级调用问题仍优先 `trace_calls`。

## 14. 模块改动清单

| 模块 | 改动 | 边界理由 |
|---|---|---|
| `internal/ontology` | 新增本体核心 | 集中拥有语义和查询约束 |
| `internal/domain/types.go` | Runbook 增加服务关联字段 | 入口规范化后保留事实来源 |
| `internal/indexing/indexer/docs.go` | 解析并规范化 Runbook service | 不允许投影层二次读取 DocStore |
| `internal/indexing/indexer/bootstrap.go` | Canonicalize 后投影 Snapshot | 复用确定性扫描，不重复解析 |
| `internal/indexing/service.go` | 编排本体投影和发布 | Indexing 拥有索引生命周期 |
| `internal/platform/store/sqlite.go` | Schema 升级、本体表、原子写入 | SQLite 只拥有存储机制 |
| `internal/platform/ontologystore` | 新增 Provider 分发 | 唯一后端选择点 |
| `internal/platform/graph` | 第一阶段不改 | 降低行为变更范围 |
| `internal/platform/store/codegraph` | 第一阶段不改 | 完整调用图仍由专用存储拥有 |
| `internal/agent/tools.go` | 精确注入 ontology.Service | 不保留泛型依赖容器 |
| `internal/agent/registry.go` | 条件注册 query_relations | 能力不可用时不伪装可用 |
| `app/platform.go` | 构造并组合 ontology | 根组合点可以知道具体实现 |
| `config`/`.env.example` | 增加 Ontology Provider 配置 | 配置在入口规范化 |
| `knowledge/api.go` | 第一阶段不改 | 不提前固化公开契约 |
| `internal/semantic` | 第一阶段不改 | 向量库不是事实库 |
| Dashboard | 后续可选 | 等查询契约稳定后增加浏览页 |

## 15. 分阶段重构计划

为保持差异可审查，不进行大爆炸重构。

### 阶段 0：行为基线

只增加特征测试：

- 固定 Fixture 的 Service/Endpoint/Dependency 数量；
- `trace_deps` 输出；
- `list_apis` 输出；
- Runbook frontmatter 映射；
- 当前 SQLite 快照原子发布和失败保留旧快照。

### 阶段 1：本体核心

新增：

- Class/Predicate/Entity/Fact/Evidence；
- Schema；
- Stable ID；
- Builder 和 ValidateSnapshot；
- Project(IndexBundle)。

不接数据库、不注册工具。

### 阶段 2：SQLite Provider

- SQLite Schema 升级；
- 本体表写入；
- Resolve/Neighbors/FindPaths；
- 与结构化数据同快照发布；
- Repository 查询合同和 Backend 发布合同测试。

### 阶段 3：运行时组合

- OntologyConfig；
- Provider 分发；
- `ontology.Service`；
- `query_relations` 条件注册；
- Stats/Health。

默认仍使用 SQLite。

### 阶段 4：影子校验

在测试或显式诊断模式比较：

```text
trace_deps vs ontology depends_on
list_apis vs ontology exposes
```

差异只记录，不改变线上回答。需要确认差异来自模型语义、投影错误还是旧工具逻辑。

### 阶段 5：专用工具逐个迁移

只有影子结果稳定后，才分别评估：

1. `trace_deps` 是否改用 ontology Repository；
2. `list_apis` 是否改用 ontology Repository；
3. `get_service` 是否复用 Entity Resolver。

`trace_calls` 不在本阶段迁移。

### 阶段 6：Neo4j Provider（需求驱动）

交付条件至少满足一项：

- 跨类型 5 层以上路径成为高频查询；
- SQLite 路径查询 P95 明确超过目标；
- 图算法成为产品能力；
- 某部署环境明确要求复用 Neo4j。

实现 Neo4j Adapter、代际快照和共享合同测试，不改变上层 Service/Tool。

## 16. 迁移、兼容与恢复

### 16.1 SQLite 派生数据升级

本体数据由结构化索引重新生成，不迁移旧派生 SQLite：

```text
检测旧 schema version
→ 关闭旧数据库
→ 删除派生 db/wal/shm
→ 创建新 Schema
→ 提示/触发明确的结构重建
```

不能在读取路径长期兼容旧 Schema。

### 16.2 API 与工具兼容

- 现有工具 ID、输入 Schema 和输出保持不变；
- 新增工具不会修改旧工具语义；
- `knowledge.API` 不变；
- 本体内部类型不泄漏到公开包；
- Dashboard 若增加本体接口，使用自包含 DTO。

### 16.3 发布失败恢复

SQLite：失败保留旧文件快照。

Neo4j：失败 Snapshot 保持 building/failed，Active 指针不切换；清理任务按 Snapshot ID 有界删除。

进程重启后：

- SQLite 打开当前已发布文件；
- Neo4j 只读取 Active Snapshot；
- 非 Active 的 building Snapshot 标记中断并等待清理；
- 不自动把不完整 Snapshot 设为 Active。

## 17. 可观测性

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

## 18. 测试策略

### 18.1 Schema 单元测试

- 合法 domain/range；
- 非法关系拒绝；
- 未注册属性/Qualifier 拒绝；
- Confidence 边界；
- 缺失引用；
- 稳定 ID；
- Evidence 去重。

### 18.2 投影测试

使用多语言 Fixture 验证：

- 每个 Service 生成唯一实体；
- 每个 Endpoint 生成 `exposes`；
- HandlerMethod 稳定时生成 `implemented_by`；
- Service/External Dependency 正确区分；
- 多来源依赖合并 Evidence；
- unresolved Runbook service 不制造 Service；
- 相同输入不同顺序得到相同实体/Fact 集合。

### 18.3 Repository 与 Backend 合同测试

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

### 18.4 Agent 工具测试

- 输入规范化和预算校验；
- 多候选实体消歧输出；
- direct/path 标识；
- truncated 传播；
- unavailable Provider 行为；
- 不泄漏内部存储错误和凭证；
- 现有工具行为特征测试继续通过。

### 18.5 并发与性能

- 重建发布期间旧快照可读；
- 发布切换后新查询只读新快照；
- 多个查询不持有跨调用可变 Snapshot 指针；
- `go test -race -count=1 ./...`；
- 路径遍历对环和高 Fanout 有明确预算。

## 19. 性能目标

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

## 20. 安全与权限

- 本体查询只读；
- `query_relations` 进入现有 read tool 权限体系；
- Neo4j 使用最小权限账户；
- 凭证只从配置入口读取，不写入 Platform Settings 返回对象之外的日志；
- 查询不接受任意 SQL/Cypher；
- 路径深度、节点、Fanout 和超时均由服务端限制；
- Evidence 输出遵循现有工作区路径授权，不返回文件系统绝对路径；
- 上层应用若未来获得公开查询能力，必须通过自包含 DTO 和认证边界。

## 21. 风险与应对

| 风险 | 后果 | 应对 |
|---|---|---|
| 本体与现有 domain 类型重复 | 双模型漂移 | 本体只做规范化投影，不重写扫描 DTO |
| 过早通用化接口 | 难用且限制 Neo4j | 只支持 Resolve/Neighbors/Bounded Path |
| LLM 事实污染 | 概率猜测变成确定事实 | 第一阶段禁止 LLM 正式写入 |
| 双写不一致 | Provider 结果分裂 | 单 Provider，禁止生产双写 |
| 多跳被当成直接依赖 | 错误影响分析 | direct/path 明确分离 |
| Neo4j 失败静默回退 | 配置与实际行为不一致 | 显式错误和 unavailable，不替换 Provider |
| SQLite 高 Fanout | 查询抖动/内存增长 | 批量 BFS、MaxDepth/Nodes/Fanout |
| Schema 一次扩得过大 | 抽取质量低、维护成本高 | 第一版只覆盖已有稳定数据 |
| 公共 API 过早固化 | 后续难演进 | 先保持 internal，真实第三方需求再公开 |

## 22. 验收标准

完成第一阶段 SQLite 本体化必须满足：

1. 每个 Service 有且只有一个 `service` Entity。
2. 每个 Endpoint 有且只有一个 `Service exposes APIEndpoint` Fact。
3. 每条规范化 Dependency 对应一个 `depends_on` Fact，协议不丢失。
4. 多来源相同 Fact 合并 Evidence，不重复生成逻辑关系。
5. 未解析目标不会自动创建虚假 Service。
6. 相同输入重复索引后 Entity ID、Fact ID 完全稳定。
7. 所有 Fact 通过 Schema、引用和属性白名单校验。
8. 结构数据和本体数据在同一个 SQLite 快照原子发布。
9. Resolve/Neighbors/FindPaths 全部有存储层 Limit 和服务层预算。
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

## 23. 后续演进

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

## 24. 最终建议

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
