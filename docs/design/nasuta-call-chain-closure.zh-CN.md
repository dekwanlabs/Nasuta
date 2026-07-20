# 方法级调用链闭环评估

> 核验日期：2026-07-19  
> 核验范围：Nasuta 当前代码、测试以及本地 `.codegraph/codegraph.db` 实际 schema。  
> 目标：判断 agent 能否从一个方法或语义命中的代码块出发，拿到可靠的仓内 callers/callees、跨服务下游实现和跨服务上游入口。

## 实施状态（2026-07-19）

本文 P0 和 Feign 闭环相关的 P1 项已经实现：

- `internal/callchain` 成为 Agent 与 Dashboard 共用的调用链应用服务，HTTP Handler 不再自行拼接调用链。
- `trace_calls` 支持 `file+line`、`query+file/qualified_name`、`direction=both`，并接受 `max_depth/max_nodes/max_fanout` 显式预算。
- CodeGraph adapter 返回完整 `CallHop`，保留重复调用点及 `line/col/confidence/provenance`，截断时返回 `truncated/nextFrontier`。
- 服务归属改为根据 SQLite 中 `repo+module_path` 做最长前缀解析；下游 route resolver 使用目标模块规范路径，不再使用 `%服务名%` 猜测文件路径。
- Feign 支持下游实现桥接和反向上游桥接，桥接后继续做有界方法遍历；Agent 与 Dashboard 使用同一结果。
- 服务边扫描新增 JVM/Python HTTP、gRPC、Dubbo，以及 Kafka producer-topic-consumer 关联。
- agent 策略明确区分方法 `calls`、已验证 `service_route` 和仅服务级依赖，并禁止把 `truncated/unresolved` 表述为完整链路。
- 新增 `NASUTA_CODEGRAPH_CONTRACT=1` 门控的外部 builder contract 测试，验证源码到 nodes、calls 和调用点行号；普通测试环境不依赖 CLI。

仍保留一个明确边界：gRPC、Kafka、Dubbo 当前形成服务级依赖边，但尚未形成协议到具体实现方法的符号桥。它属于本文 P1 后续增强，不影响 Feign 方法级双向闭环。

## 1. 实施前结论（历史基线）

这些问题总体存在，但原评估有三处需要纠正：

1. **agent 方法级调用链仍未闭环。** `trace_calls` 仍是单向、3 跳、总节点最多 8、每个节点最多展开 4 个邻接，并且没有跨服务 route resolver。
2. **Dashboard 有跨服务下游桥接代码，但当前不能称为“接近闭环”。** CodeGraph 已强制使用 `repos/<group>/<project>/...` 规范路径，而 Dashboard 的 `serviceFromPath` 仍按旧目录格式取第一个路径段，实际会把服务解析成 `repos`，导致服务标注和跨服务判断失真。
3. **原文对数据链路有两处事实错误。**
   - 语义命中的 `code_chunk` 不会直接转换成 CodeGraph 节点继续遍历；预检索中的 CodeGraph 证据来自另一条独立 FTS 查询。
   - CodeGraph `edges` 表实际有 `line`、`col`、`provenance`；问题不是 builder 没有保存调用点，而是 Nasuta 的 `queryRelated` 和 `CallChain` 没有读取、返回这些字段。

服务级 `trace_deps` 的**存储和查询机制闭环**，但扫描覆盖并不完整，因此不应表述为“所有协议的服务依赖完全闭环”。

## 2. 实施前数据与执行边界

### 2.1 服务级依赖图

服务级依赖由 Nasuta 扫描源码后写入结构化 SQLite，再由 `reloadDependencyGraph` 加载到内存图：

```text
源码扫描器
  -> domain.DependencyEdge
  -> .nasuta/index.db
  -> Graph.Rebuild
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

### 2.2 方法级 CodeGraph

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

## 3. 实施前三条路径

### 3.1 agent `trace_calls`

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

### 3.2 Dashboard `/api/codegraph/endpoint`

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

### 3.3 预检索中的 CodeGraph

原文“code_chunk 通过 `FindNodeAt` 进入方法遍历”的描述不准确。

当前预检索是两条并行证据路径：

```text
语义搜索 -> code_chunk

关键词 + 服务范围 -> CodeGraph FTS -> Node -> FindNodeAt -> 源码窗口
```

`FindNodeAt` 在 `codeGraphNode` 中用于把 **CodeGraph FTS 已命中的 Node** 再定位到最窄可调用符号，并不是把 semantic code hit 转成调用链起点。索引阶段确实会利用 CodeGraph 方法行号切分 code chunk，但检索阶段没有“code hit -> method node -> callers/callees”的闭环。

## 4. 实施前问题清单

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

## 5. 修复顺序

### P0：先修正确性，再扩能力

1. **统一服务归属解析。** 删除 Dashboard 的字符串猜测，基于结构化 SQLite 中的 `repo + module_path` 做最长路径前缀匹配；复用同一套规范路径不变量，并补单仓多模块测试。
2. **抽取共享调用链应用服务。** 不让 agent 调 Dashboard handler；将“节点定位、服务归属、route 桥接、递归预算、返回模型”放到独立业务服务，Dashboard 和 agent 共用。
3. **支持明确起点。** 新工具同时接受：
   - `file + line`：用于 semantic code hit 或 UI 精确起点；
   - `query + optional file/qualified_name`：用于符号搜索和消歧。
4. **支持 direction=both。** 一次返回 callers 和 callees，并分别标记是否截断。
5. **用显式预算替代“无限展开”。** `max_depth`、`max_nodes`、`max_fanout` 可配置且在响应中返回 `truncated`、`next frontier`。调用图必须有界，不能为了“全量”取消所有保护。

### P1：补跨服务和调用点

1. 设计 `CallHop` 返回模型，至少包含 source、target、edge kind、call-site line/col、confidence 和 provenance。
2. 修改 `queryRelated` 读取 `edges.line/col/provenance`，避免把“同一 caller 在不同位置多次调用同一 callee”静默折叠成无位置节点。
3. 增加跨服务 upstream resolver；至少先闭合 Feign route 的反向查询。
4. 按现有扫描缺口补协议：
   - Java/Kotlin RestTemplate/WebClient、gRPC、Dubbo；
   - Python HTTP/gRPC；
   - Kafka producer/topic/consumer 模型及独立边类型。
5. 协议桥接与服务边抽取分层：边存在不代表可以定位到下游实现方法，两层分别测试。

### P2：消歧和外部契约

1. `get_symbol`/`trace_calls` 多候选时返回候选及稳定标识，接受 file、kind、qualified name 限定。
2. 在 agent 工具策略中说明方法图与服务图的拼接条件，禁止把未解析的跨服务 hop 当成已验证调用。
3. 增加固定 fixture 的 CodeGraph contract 测试：源码 -> 执行 builder -> 断言 nodes、calls、route、line/col。外部 CLI 不可用时跳过集成测试，但 adapter 单测必须常规执行。

## 6. 扫描器扩展边界

原方案提出直接全面引入 tree-sitter，但这属于技术选型，不能从当前缺口直接推导为唯一方案。建议按证据复杂度选择：

- 声明式、局部语法足够稳定的模式（Feign、Dubbo、部分 gRPC stub）可以继续使用轻量 scanner，但必须有跨行、别名和多模块 fixture。
- 需要变量绑定、类型追踪或 Bean 来源分析的模式（例如 `@LoadBalanced RestTemplate`）应使用 AST/符号解析，不能靠调用行正则猜测。
- 若引入 tree-sitter，应先以 Java/Kotlin 一个协议做纵向切片，并证明相对现有 scanner 提升了召回和准确率，再扩语言矩阵。
- unresolved target 可以保留为外部逻辑目标，但现有 `DependencyTargetKind` 只有 service/external；增加新状态前必须先定义持久化、查询和晋升规则，不能只加枚举。

## 7. 验收标准

方法级调用链闭环至少需要以下自动化验收：

1. 同一方法直接调用 6 个不同方法时，默认预算不会静默只返回 4 个；若截断，响应明确给出截断信息。
2. 同一 caller 在不同代码行调用同一 callee 两次，返回两个 call-site。
3. `repos/<group>/<project>/...` 和单仓多模块路径能稳定解析到正确 ServiceRecord。
4. 从 `code_chunk(file,start_line)` 可以显式选择对应 CodeGraph 方法并继续查 both 方向。
5. Feign caller -> client method -> route -> 下游 controller method 能在 agent 和 Dashboard 得到一致结果。
6. 下游服务方法反查跨服务 caller 有明确结果或明确“未解析”，不能伪装成完整链路。
7. 重名方法必须通过 file/qualified name 消歧，不能静默选错。
8. CodeGraph DB 缺失、builder 不可用或某语言无覆盖时均返回可观察的能力缺口。

## 8. 关键代码索引

| 关注点 | 当前实现 |
|---|---|
| agent 工具注册 | `internal/agent/registry.go`：`get_symbol`、`trace_calls`、`trace_deps` |
| agent 方法调用链 | `internal/agent/tools.go`：`GetSymbol`、`TraceCalls` |
| CodeGraph 只读 adapter | `internal/platform/store/codegraph/db.go` |
| Dashboard 方法链 | `internal/transport/dashboard/tools.go`：`APICodeGraphEndpoint`、`expandDownstream`、`serviceFromPath` |
| 预检索 CodeGraph | `internal/retrieval/collection.go`：`collectCodeGraph`；`internal/retrieval/pipeline.go`：`codeGraphQuery`、`codeGraphNode` |
| 服务依赖扫描 | `internal/indexing/indexer/bootstrap.go`：`ScanCode` |
| 服务依赖图装载 | `internal/indexing/service.go`：`reloadDependencyGraph` |
| CodeGraph 外部构建 | `internal/indexing/service.go`：`RebuildGraph`、`runCodegraphIndex` |
