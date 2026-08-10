# 内置工具实现简化开发提案

> 状态：实施中，核心风险与明确冗余已处理，非规范性基线
> 日期：2026-08-01
> 影响范围：Nasuta `tool/`、`internal/agent`、`internal/transport/dashboard`、`internal/transport/mcp`、`internal/transport/routes`、`platform/httputil`
> 最终归并目标：验收后归并到工具架构、QA 执行链路与错误处理的正式文档

## 0. 文档生命周期

本文是在开发前独立维护的现状审计与实施方案，不代表当前代码已经具备文中描述的目标行为。开发期间以本文记录问题、范围、实施切片和验收结果；当前代码与现有正式架构文档仍是权威基线。

只有同时满足以下条件，本文结论才可归并到正式文档：

1. 对应代码切片已经完成且不存在长期兼容分支；
2. Agent、MCP、Dashboard 三个入口的工具合同测试通过；
3. 配置后端失败、能力未配置、成功零结果和部分结果可以被明确区分；
4. 工具结果字段和错误协议与最终实现重新核对；
5. 归并后删除本文，或仅保留决策与实施记录。

本提案遵循仓库 `AGENTS.md` 的简洁性、边界纪律、显式错误、入口规范化和最低时间复杂度约定。

### 0.1 实施进度

截至 2026-08-01，已完成以下代码切片：

- `get_service` 不再吞 semantic embed/search error，空 embedding 作为 provider contract error 返回；
- `FindAPIs`、源码读取和 Dashboard 工具入口不再把失败包装成 HTTP 200 成功 payload；
- 删除 Agent 对工具结果 JSON 的 overlap 反解析、identity formatter、未消费参数副本和重复 chunk 状态；
- malformed session tool arguments 保留原始审计值，超限参数写入明确 omitted marker；
- runbook 只评分一次；`search_code` 只组装并执行一次 semantic query；score 零值兼容回退已删除；
- `trace_calls` 只返回 `nodes`，并暴露 requested/resolved target 与明确 ambiguity；
- `trace_deps` 直接序列化 typed domain 结果，不再维护第三套手写 map；
- 删除 MCP nil-registry、QA 空 registry、通用 Registry replace/unregister 和死 `GetSymbol` 包装；
- `ToolPolicyForPlan` 已改为与真实行为一致的 `ToolPolicyForRun`，history 参数不再使用双层 variadic；
- `index_stats` 通过 `runbookIndex.status` 区分未配置能力与真实零计数；
- 已补 semantic 失败、空 embedding、源码缺失、malformed/oversized arguments 和 Dashboard 非 2xx 合同测试。
- HTTP 入口统一记录 4xx/5xx 的 method、path、status、耗时和 trace ID；`httputil` 产生的原始 error 由同一入口记录，不再要求 handler 分散打日志。

仍保留或后续单独处理：

- 公共 `RunWithPlan`/`RunWithContext`/`RunWithSnapshot` 属于可复用模块 API，本次不按仓内调用关系删除；
- `domain`/`knowledge` 公共边界继续保留；零差异 DTO 是否改为别名需要单独评估兼容性；
- 源码路径自动补 `repos/` 和默认行范围仍属于待确认合同；
- 旧 `map[string]any` 便利包装仍被 application self-test 使用，需先迁移该入口再删除；
- Snapshot 深拷贝、trace 数量和更大范围 typed result 收敛不是本轮行为风险修复的前置条件。

## 1. 结论速览

**链路与流程：通的。** MCP 与 Agent 两条调用链从注册到执行每一跳都接得上，没有生产路径上的断链。写动作隔离是真实的纵深防御（policy 快照 + `MCPHidden` + `IsAdmin` 门 + executor 快照拒绝），不会被 MCP 或普通读回路触达。

**复杂度：参差。** 一半工具简洁（`list_apis`、`index_stats`、`check_docs`、`trace_calls`、`web_search`、写动作目录、`observe_logs`），另一半偏复杂（`search_code`、`get_service`、`trace_deps`、`search_runbooks`、`get_symbol`）。复杂的根因不是业务逻辑本身，而是三层横切问题：

1. **`Xxx → XxxResult → FindXxx → SearchXxx → toXxxResult` 的多层中转**，其中 `Xxx`（吞错的 `map[string]any` 包装）和 `toXxxResult`（手写 map 转换）两层是纯样板，重复 JSON 标签已能完成的工作，且制造字段漂移风险面。
2. **`domain.*` 与 `knowledge.*` 双类型层级本身是合理的**（`knowledge` 是被 `delivery` 真实消费的公共契约），但 runbook 两侧字段逐字段相同，转换器是零差异拷贝；而 `trace_deps` 在 domain/knowledge 之外又叠加了第三个 map 表示，且与 dashboard 直连序列化的形状不一致。
3. **入口间重复校验**（`clampInt`/方向默认在两个入口各做一次）与若干**死代码 / 防御性兜底**。

**死路径/防御性兜底（违反"无静默兜底"约定）：** MCP nil-registry 兜底、`QA.toolExecutor()` 的空 registry 分支、`tool.Registry` 中无生产调用方的通用 `Replace/Unregister` API、`GetSymbol` 死方法——正常生产构造中不可达，部分若被触达会静默降级。`ReadRegistry.Reconcile` 及其 revision 驱动的 MCP 热更新是活路径，不属于清理范围。

### 1.1 代码核验状态

本文问题已按 2026-08-01 的代码重新核验，后续实施必须区分以下三类，不能把"可简化"直接等同于"可删除"：

| 分类 | 已确认事项 |
|---|---|
| 真实行为风险 | `get_service` 吞 semantic embed/search error；空 embedding 被当作成功；`FindAPIs` 吞 `nil page, nil error` 合同异常；`readNodeSource` 吞文件错误；Dashboard 多个工具以 HTTP 200 返回 payload `"error"`；malformed tool arguments 被改写为 `{}` |
| 真实冗余 | runbook 重复评分；Agent 反解析工具 JSON 计算 overlap；`trace_calls` 同时返回 `nodes`/`hops`；`formatToolResultForLLM` identity；`ToolExecution.Arguments` 未消费；`GetSymbol` 无调用方 |
| 策略或公共合同问题 | `referenceMismatch` 扫描自由文本是已有测试固化的策略；公共 `RunWithPlan`/`RunWithContext`/`RunWithSnapshot` 可能属于模块 API；`domain`/`knowledge` 类型隔离有公共边界价值 |
| 待合同确认 | score 零值是否允许；源码路径是否必须带 `repos/`；未配置 doc store 应表达 unavailable 还是计数 0 |

核验结论约束实施方式：真实行为风险可以直接进入修复切片；策略或公共合同问题必须先明确目标行为和兼容承诺；待确认项必须先补合同测试，不能仅凭当前分支形状删除。

### 1.2 HTTP 错误日志核验

接口错误不可观察的问题真实存在：`platform/httputil` 的错误 helper 只写 JSON 响应，原有 `TraceMiddleware` 只注入 trace ID，二者之间没有统一日志边界。大量 handler 因此即使返回 4xx/5xx，后端也可能没有对应日志；少量自行记录的错误又造成日志策略分散。

目标合同：

- 全局 HTTP 中间件统一记录所有 4xx/5xx，4xx 使用 WARN，5xx 使用 ERROR；
- 日志包含 method、path、status、duration 和 trace ID，不记录 raw query；
- `httputil.WriteErrStatus` 只把原始 error 交给响应记录器，不自行打第二条日志；
- 普通 `http.Error` 仍可按 status 被观察，但没有可用的原始 error 时不虚构详情；
- 响应包装器保留 `http.Flusher`，不改变 QA、Feature SSE 和 MCP Streamable HTTP 协议；
- SSE 建连后以事件表达的运行失败不属于 HTTP 4xx/5xx，继续由 Agent/Run 领域日志负责。

## 2. 工具清单

注册入口 [internal/agent/registry.go](../../internal/agent/registry.go) 的 `builtinTools`，共 13 个读工具（9 核心 + 4 条件追加），加上 1 个受限读工具和 2 个写动作：

| # | 工具 | Kind | 条件 | MCP 可见 | 复杂度 |
|---|---|---|---|---|---|
| 1 | `get_service` | Read | 始终 | 是 | 过复杂 |
| 2 | `trace_deps` | Read | 始终 | 是 | 过复杂 |
| 3 | `list_apis` | Read | 始终 | 是 | 简洁 |
| 4 | `search_code` | Read | 始终 | 是 | 过复杂 |
| 5 | `get_symbol` | Read | 始终 | 是 | 过复杂（轻） |
| 6 | `trace_calls` | Read | 始终 | 是 | 可接受 |
| 7 | `search_runbooks` | Read | 始终 | 是 | 过复杂 |
| 8 | `check_docs` | Read | 始终 | 是 | 可接受 |
| 9 | `index_stats` | Read | 始终 | 是 | 简洁 |
| 10 | `web_search` | Read | web 搜索启用 | 是 | 简洁 |
| 11 | `get_turn` | Read | `sessions != nil` | 否（`MCPHidden`） | 简洁 |
| 12 | `find_turns` | Read | `history != nil` | 否（`MCPHidden`） | 简洁 |
| 13 | `trace_relations` | Read | `ontology != nil` | 是 | 简洁 |
| 14 | `observe_logs` | Read | 经 `ReadRegistry` 发布 | 是 | 简洁 |
| 15 | `propose_branch` | Write | 事件流启用 | 否（`MCPHidden`） | 简洁 |
| 16 | `propose_commit` | Write | 事件流启用 | 否（`MCPHidden`） | 简洁 |

> 注：`AGENTS.md` 只列出 9 个核心读工具；`web_search`、`get_turn`、`find_turns`、`trace_relations` 是条件追加工具，不应混入核心固定清单，但正式文档应说明其注册条件。

## 3. 链路与流程评估

### 3.1 两条调用链均连通

**MCP 链：** 注册 [registry.go:41](../../internal/agent/registry.go#L41) `RegisterAll(builtinTools(...))` → 组装 [app/platform.go:109](../../app/platform.go#L109) `agent.NewRegistry` → [:293](../../app/platform.go#L293) `mcp.NewDynamicHandler` → 快照 [internal/transport/mcp/server.go:92](../../internal/transport/mcp/server.go#L92) `Snapshot(tool.ReadPolicy())` → [:100-109](../../internal/transport/mcp/server.go#L100) `AddTool` 回调 → `executor.Execute` → [tool/executor.go:18-35](../../tool/executor.go#L18) `snapshot.Get` → `candidate.Handler.Execute` → 闭包调用 `svc.ServiceLookupResult(...)`。热更新由 [server.go:51-57](../../internal/transport/mcp/server.go#L51) `refresh()` 在 `Revision()` 变化时重建。

**Agent 回路链：** [internal/agent/service.go:261-262](../../internal/agent/service.go#L261) `toolExecutor()` + `Snapshot(toolPolicy)` → [:815](../../internal/agent/service.go#L815) `runWithSnapshot` → [internal/agent/loop.go:229](../../internal/agent/loop.go#L229) `Definitions` / [:405](../../internal/agent/loop.go#L405) `Execute`。

两链均无生产断链。

### 3.2 写动作隔离：真实且纵深

写动作经 `RegisterAll` 进入共享 `*Registry`，但被三道独立闸门挡在所有执行面之外：

- **MCP**：`BuildMCP` 用 `ReadPolicy()` 快照（[tool/contract.go:274-276](../../tool/contract.go#L274)），`Policy.Allows` 对 `KindWrite` 返回 false；写工具另设 `MCPHidden: true`（[internal/writeaction/catalog.go:80](../../internal/writeaction/catalog.go#L80)）。
- **Agent 回路**：`toolPolicy` 仅在 `writeAvailable && AllowWrite && IsAdmin` 时放开（[service.go:260](../../internal/agent/service.go#L260)、`dashboard/qa.go:300`）。
- **Executor**：`snapshot.Get(id)` 对不在快照中的工具直接拒绝（[tool/executor.go:19-22](../../tool/executor.go#L19)），模型即便发出写调用也会被只读快照拒绝。
- **`ReadRegistry`**：`ReadTool.tool()` 强制 `Kind: KindRead`，`reconcileReadSet` 拒绝非读工具（[tool/registry.go:98-100](../../tool/registry.go#L98)）——场景代码无法把写工具塞进上层发布器。

> 文档措辞修正：`AGENTS.md` 称写动作不进入上层 registrar。实际它们进入共享 `*Registry`，只是不进入 `ReadRegistry`。真正隔离的是 policy 快照、`MCPHidden` 和 `IsAdmin`，而非共享注册表的成员资格。

### 3.3 死路径与静默兜底（需清理）

| 位置 | 问题 | 严重度 |
|---|---|---|
| [server.go:30-32](../../internal/transport/mcp/server.go#L30), [:89-91](../../internal/transport/mcp/server.go#L89) | MCP nil-registry 兜底：生产从不触达（`platform.registry` 在 [:109](../../app/platform.go#L109) 已非 nil）；若触达则静默丢 `web_search`/`get_turn`/`find_turns`，且**新建一个与 Agent 回路不同的 registry**，违反 [:82-83](../../internal/transport/mcp/server.go#L82) 的"单一事实源"A DR。同时迫使 `tools *agent.Service` 这个仅供兜底使用的参数穿透 `NewDynamicHandler`/`BuildMCP`。 | 中 |
| [service.go:661-669](../../internal/agent/service.go#L661) | `QA.toolExecutor()` 三路 nil 防御：正常 `NewQA` 构造中 `svc.executor` 总在 [:160](../../internal/agent/service.go#L160) 设置；测试存在直接构造 `&QA{agent: ...}` 的路径，因此分支 2 是测试兼容而非绝对死代码。分支 3 返回空 registry，若触达则**静默以零工具运行**。 | 低-中 |
| [tool/registry.go:88-227](../../tool/registry.go#L88) | `Replace`/`ReplaceAll`/`Unregister`/`UnregisterAll`：零生产调用方（仅自测引用），约 50 行未使用的通用单工具变更 API。owner-scoped `ReadRegistry.Reconcile` 是生产热更新路径，必须保留。 | 中 |
| [tools.go:1146-1148](../../internal/agent/tools.go#L1146) | `GetSymbol`：1 行包装 `GetSymbolFiltered(...,"","")`，**零调用方**，死代码。 | 低 |
| [loop.go:184-196](../../internal/agent/loop.go#L184) | 公共 `RunWithPlan`/`RunWithContext`/`RunWithSnapshot` 未被当前生产 QA 链调用，当前链走私有 `runWithSnapshot`。Nasuta 是可复用模块，删除前必须确认这些方法是否属于受支持的公共 API，不能只按仓内调用判死。 | 低 |
| [tool/registry.go:366](../../tool/registry.go#L366),[:372](../../tool/registry.go#L372),[:382](../../tool/registry.go#L382) | 快照按契约不可变，但每次 `Get`/`Tools`/`MCPTools` 仍对每个工具 `cloneTool` 深拷贝。 | 低 |

### 3.4 空快照疑问澄清

`loop.go:108` 的 `NewToolExecutor(tool.NewRegistry())` 是 `NewAgent` 的 nil 默认值，**仅测试**（`agent_test.go` 构造无工具 agent）触达，循环无工具直接 force-conclude，属测试便利。`service.go:668` 同款在正常构造中不可达，是防御分支；`service.go:664` 的 agent executor 分支仍被部分直接构造 `QA` 的测试依赖。两者均非生产 bug，但空 registry 分支违反"无静默兜底"。

## 4. 复杂度评估

### 4.1 横切问题一：多层中转

每个工具普遍呈 4–5 层结构：

| 层 | 签名 | 消费方 | 评价 |
|---|---|---|---|
| `Xxx` | `map[string]any`（吞错进 map） | REST dashboard + 冒烟 | **样板**：所有工具重复同款 4 行 `if err != nil { return map{...,"error":...} }; return result` |
| `XxxResult` | `(map[string]any, error)` | MCP registry | 多数仅 `clampInt` + 包一层，价值低 |
| `FindXxx`/`TraceXxx` | `domain.*` | 内部检索 `retrieval/pipeline.go` | 合理 |
| `SearchXxx`/`TraceDependencies` | `knowledge.*` | 场景工具 `delivery/generation.go` | **合理**：真实公共契约消费方 |
| `toXxxResult` | `domain.* → knowledge.*` | 桥接两类型世界 | 部分机械字段裁剪，部分零差异拷贝 |

`knowledge.*` 层**有正当性**：`delivery/generation.go` 真实导入 `knowledge.*`，且 `knowledge.ServiceRecord` 注释明确"只携带对外有用的身份与元数据"。问题不在这一层，而在 `Xxx`（吞错 map）与 MCP 专用的第三个 map 表示。

### 4.2 横切问题二：`map[string]any` 与字段漂移

domain 结构已带 json 标签，多数手写 map 是在重造序列化：

- [tools.go:361-376](../../internal/agent/tools.go#L361) `CodeSearchResult` 手敲 15 个字符串键镜像 `domain.CodeSearchHit`，字段重命名会静默失配——正是 `result_preview`/`summary` 那类漂移 bug 的温床。
- [registry.go:410-434](../../internal/agent/registry.go#L410) `dependencyTraceToMap`/`dependencyEdgeToMap`：手写 map 转换，既重复 json 标签，又产生与 dashboard 直连序列化**不一致**的线上形状（MCP 丢 `callerServiceKey/targetKind`，dashboard 保留）。
- [tools.go:231-234](../../internal/agent/tools.go#L231) `RunbookSearchResult` 的 map 与 `domain.RunbookSearchResult`（已带 json 标签）1:1 相同，纯冗余。

### 4.3 横切问题三：双/三类型层级

- **ontology → domain**（[tools.go:740](../../internal/agent/tools.go#L740) `dependencyTrace`）：**合理**，真实语义转换（三元组 → 依赖边、`Object.Class==ExternalSystem` → `TargetKind`）。
- **domain → knowledge**（`toDependencyResult`/`toServiceSearchResult` 等）：**合理但样板**，机械裁剪字段做公共契约隐藏。
- **runbook 两侧逐字段相同**：`toRunbookSearchResult` 是零差异拷贝，但 `knowledge` 作为公共 API 边界仍有所有权价值。应优先让内部实现直接构造公共结果、复用共享结构或使用受控别名，而不是仅凭字段相同删除公共契约。
- **domain → map**（MCP 专用）：**不合理**，叠加在已合理的 ontology→domain→knowledge 链上的第三表示。

### 4.4 逐工具结论

| 工具 | 层数 | 结论 | 关键证据 |
|---|---|---|---|
| `search_code` | 4+转换 | 过复杂 | [FindCode:506-650](../../internal/agent/tools.go#L506) 143 行，dense `Search` 调用重复 3 次（[:554](../../internal/agent/tools.go#L554)/[:560](../../internal/agent/tools.go#L560)/[:570](../../internal/agent/tools.go#L570)），两 dense 分支仅日志不同；`scoreKind`/`fusionScore`/`semanticScore` 泄漏检索内部概念（[:369-373](../../internal/agent/tools.go#L369)、[:622-634](../../internal/agent/tools.go#L622)）；`max(limit*3,limit)` 冗余（[:536](../../internal/agent/tools.go#L536)）；11 段 trace + 8 行 log 淹没主流程 |
| `get_symbol` | 3 | 过复杂（轻） | `GetSymbol` 死代码（[:1146](../../internal/agent/tools.go#L1146) 零调用方）；`readNodeSource` 读整文件再切片（[:1344](../../internal/agent/tools.go#L1344)）；魔数 `+40`/`-3`/`4000` |
| `search_runbooks` | 4+转换+双打分 | 过复杂 | **重复打分**：`scoreRunbooks` 已打分+过滤+排序（[:307](../../internal/agent/tools.go#L307)），`FindRunbooks` 又对返回的 seed 重新 `scoreRunbook`+过滤+排序（[:314-318](../../internal/agent/tools.go#L314)）；`toRunbookSearchResult` 零差异拷贝 |
| `check_docs` | 2 | 可接受 | 最简洁之一，缺失项检查为独立 `if`，无状态机 |
| `get_service` | 5+5 helper | 过复杂 | 5 层 + `scoreServices`/`scoreService`/`mergeServiceMatches`/`semanticServiceNames`/`traceServiceNames`；`clampInt(1,100)` 在 `ServiceLookupResult` 与 `SearchServices` 各做一次；`mergeServiceMatches` 每次从缓存集重建 `byName`（[:1023](../../internal/agent/tools.go#L1023)）；`services()`/`AllServices()`/`ServiceModules` 三取器缓存边界不一致 |
| `trace_deps` | 3 类型+3 转换 | 过复杂 | ontology→domain→knowledge 链合理，但叠加 `dependencyTraceToMap` 第三表示且与 dashboard 形状不一致；`TraceDeps`/`TraceDependencies` 重复方向默认与 depth clamp |
| `trace_calls` | 3 | 核心算法可接受，交付层需简化 | API target 解析能力有业务价值，但 `resolveAPICallTarget` 会静默覆盖 `query/file/line/qualifiedName`，响应未暴露原始与解析后目标；每方向同时发 `nodes` 与 `hops`（[:1305](../../internal/agent/tools.go#L1305)）；ambiguity 又混入成功 payload 的 `"error"` 字段；`readNodeSource` 逐跳读整文件无缓存 |
| `list_apis` | 3 | 简洁 | 薄而规整，`[]domain.EndpointRecord` 直接序列化，基线良好 |
| `index_stats` | 2 | 简洁，但能力语义待统一 | ontology 缺失 → `unavailable`、执行失败 → 显式 `%w` 是正确的；`docStore==nil` → `0,nil` 会把"未配置"与"真实零 runbook"合并，和本文目标结果模型冲突，应先明确外部合同；`runbookCount` 可内联 |
| `web_search` | — | 简洁 | [internal/agent/web.go](../../internal/agent/web.go) 显式分发，每 provider 一函数（`searchDuckDuckGo`/`searchBrave`/`searchBing`）；**无静默兜底**：未知 engine 报错、Brave 缺 key 报错不退回 DDG |
| `get_turn`/`find_turns`/`trace_relations` | 1 | 简洁 | 真实后端（`memory`/`sessionhistory`/`ontology`），各 20–35 行委派 |
| `propose_branch`/`propose_commit` | — | 简洁 | [catalog.go](../../internal/writeaction/catalog.go) 94 行完整非桩；`writeTool` helper 干净；统一走 `proposer.Propose` |
| `observe_logs`/`ReadRegistry` | — | 简洁 | ~30 行；"受限"= 强制 `KindRead` 写安全闸，非 MCP 隐藏；`observe_logs` 经设计有意对 MCP 可见 |

### 4.5 时间复杂度

无 `slices.Contains`-in-loop 违规（去重均用 `map[string]struct{}`），`strings.Contains` 用法均在 k≤10 字段上（符合例外）。小问题：

- `scoreServices`[:932](../../internal/agent/tools.go#L932)、`scoreRunbooks`[:985](../../internal/agent/tools.go#L985) 缺预分配：`var scored []` → `make([]T, 0, len(all))`。
- `mergeServiceMatches`[:1023-1028](../../internal/agent/tools.go#L1023) `byName`/`seen`/`out` 未预分配，且 `byName` 每次从缓存集重建。
- `readNodeSource`[:1344](../../internal/agent/tools.go#L1344) 逐跳读整文件无按路径缓存（受 `MaxNodes` 约束，轻微）。
- `symbolQueryTokens`[:1336](../../internal/agent/tools.go#L1336) `len([]rune(f)) >= 3` 分配 rune 切片，可用 `utf8.RuneCountInString`。

## 5. 目标设计

目标不是重写工具框架，而是让每个工具保持一条可追踪的主路径：

```text
不可信入口
  -> 一次参数规范化与 schema 校验
  -> typed 工具服务
  -> typed 结果或 error
  -> Agent/MCP/Dashboard 各自做一次传输序列化
```

必须明确区分四种结果：

| 结果 | 表达 |
|---|---|
| 可选能力未配置 | capability unavailable；只有文档明确允许时才返回可观察的领域 fallback |
| 已配置后端执行失败 | 返回包装后的 error，不静默切换路径 |
| 执行成功但零结果 | 成功结果，集合为空 |
| 部分结果 | 返回结果和明确 notices/coverage，不能伪装成完整成功 |

工具服务不返回带 `"error"` 字段的成功 map。Dashboard 使用 HTTP 状态表达失败；Agent 和 MCP 使用统一 executor error。自由文本 query 不承担结构化实体类型校验。

## 6. 开发切片

### Slice 1：修复静默错误和错误协议

1. `FindServices` 在语义能力已配置后，不再吞掉 embed/search error。若保留关键词降级，必须记录 warning 并显式标识 degraded。
2. `semanticServiceNames` 将空 vector 视为 provider contract error。
3. `FindAPIs` 将 `page == nil && err == nil` 视为 store contract error。
4. `readNodeSource` 改为返回 `(string, error)`；`get_symbol` 和 `trace_calls` 明确表达源码缺失或读取失败。
5. Dashboard 改用返回 `(result, error)` 的工具服务，并通过 HTTP 错误响应失败；删除把 error 塞入 map 的兼容 wrapper。
6. `index_stats` 明确区分 doc store 未配置与真实 runbook 数量为零，不再用同一个 `0` 表达两种事实。
7. 在 HTTP 入口统一记录 4xx/5xx；错误响应 helper 通过窄接口传递原始 error，handler 不再重复承担接口日志职责。

验收重点：配置故障不能表现成零结果或 HTTP 200；源码索引漂移必须可见；接口失败必须带 trace ID 在统一日志中可检索。

### Slice 2：删除字段反解析和隐式策略

1. 删除 `formatToolResultForLLM` identity wrapper。
2. 删除未被消费的 `ToolExecution.Arguments`。
3. 删除 `overlapKeys` 对工具 JSON 的反解析以及 `path/startLine`、`file/line` 双字段猜测。
4. 保留精确 tool fingerprint 去重；是否需要跨查询收敛，应由明确的 Agent 策略或结构化 evidence key 决定。
5. 先明确 `referenceMismatch` 的产品行为：若实体类型保护只适用于结构化引用，则只检查 `doc_id`、`service`、`ref` 等字段并修改现有策略测试；不得在未更新合同和测试前直接删除自由文本检查。
6. `canonicalSessionToolCalls` 不再把 malformed arguments 改写为 `{}`；持久化明确的 malformed/omitted 原因。

验收重点：工具展示 JSON 不再被 Agent 当成内部协议重新解析；自由文本中的实体名称不会阻断合法工具调用。

### Slice 3：收敛工具结果类型和重复转换

1. Agent、MCP、Dashboard 共用 typed 工具服务结果。
2. 删除 `Xxx -> XxxResult` 中只负责吞错或手工组 map 的层。
3. 删除零差异 runbook DTO 拷贝，但保留 `knowledge` 公共 API 的所有权边界；可由内部实现直接构造公共结果、复用共享结构或采用受控别名。
4. `trace_deps` 只保留一个外部结果形状，删除第三套 map 表示。
5. `trace_calls` 只返回一套 hop/node 表达；自动解析 API target 时同时保留 `requested_target` 与 `resolved_target`。
6. ambiguous symbol 使用明确结果字段，不在成功 payload 中混入 `"error"`。

验收重点：三个入口的同一工具输出字段一致，字段变更由 typed contract 和测试发现。

### Slice 4：简化检索工具内部流程

1. `search_runbooks` 只做一次评分、排序和截断。
2. `search_code` 合并重复 dense search 调用。
3. 先通过 semantic contract test 固化 `ScoreKind` 与 `DenseScore`/`FusionScore` 的字段不变量，再移除以零值判断字段缺失的兼容逻辑。
4. 参数默认、范围和规范化在每个真实入口只执行一次。
5. 源码读取统一 rune-safe 截断，并取消 4000/1500 两次裁剪。
6. 删除确认无生产调用方且不属于受支持公共 API 的死方法、死分支和 registry 变更 API；保留 `ReadRegistry.Reconcile`。调整依赖直接构造 `QA` 的测试后，再删除 executor 测试兼容分支。

验收重点：相同输入的检索语义不变；排序只执行一次；合法零分不会被替换。

### Slice 5：整理注册与能力表达

1. `ToolPolicyForPlan` 要么真正使用 `EvidencePlan`，要么改成与实际行为一致的命名和签名。
2. `NewRegistry`/`builtinTools` 不再连续使用 variadic 参数表达单个 optional history。
3. 未配置、可用、执行失败不再由 `enabled/status/error/semantic/Evidence` 多组字段重复表达。
4. 删除 MCP nil-registry 和 QA 空 registry 静默运行兜底，构造不变量被破坏时显式失败。
5. 公共 Agent 执行方法先确认模块兼容承诺；保留受支持入口，删除仅内部重复的转发层，避免按当前应用调用关系误删公共 API。

## 7. 明确保留的复杂度

以下机制有真实生命周期或安全合同，不在简化范围内：

- Registry Snapshot：保证模型看到的 schema 与实际 handler 属于同一不可变快照。
- `tool.Executor` 的 schema 校验和 per-tool timeout。
- `AuthoritativeContent` 与 `PromptContent`：分别服务审计持久化和模型交付。
- AnswerContract、context budget 与 ArtifactID：防止工具证据无法无损交付时仍被当作成功；ArtifactID 有真实持久化和读取链路。
- 写工具的 policy、`MCPHidden` 与管理员授权隔离。
- 可选能力缺失时已有文档约定且可观察的领域 fallback。

这些机制可以简化字段表达，但不能通过删除校验、审计或失败状态来换取表面上的代码减少。

## 8. 验收门禁

每个切片至少执行：

```bash
go test ./internal/agent/... ./tool/... ./internal/transport/dashboard/... ./internal/transport/mcp/...
go build ./...
```

最终验收增加：

```bash
go test ./...
go vet ./...
```

必须补充或保留以下合同测试：

1. configured semantic backend error 不会静默回退；
2. optional capability absent 与 backend failure 可区分；
3. Dashboard 工具失败返回非 2xx；
4. source read error 不会返回无提示的空 source；
5. malformed session tool arguments 不会变成合法 `{}`；
6. Agent 不依赖工具 JSON 字段名计算重叠；
7. runbook 关键词结果只评分一次且顺序稳定；
8. `trace_calls` 歧义与 resolved target 对调用方可见；
9. Agent、MCP、Dashboard 的公共工具字段合同一致；
10. `referenceMismatch` 的结构化字段范围与目标产品行为一致；
11. `ScoreKind` 对应 score 字段可取合法零值且不触发兼容替换；
12. doc store 未配置与真实 runbook 数量为零可区分；
13. Registry 热更新继续通过 owner-scoped `ReadRegistry.Reconcile` 生效；
14. 直接构造 `QA` 的测试迁移后，正常执行链不再依赖 executor nil 兜底。
15. HTTP 4xx/5xx 分别产生 WARN/ERROR，成功响应不产生日志，raw query 不进入日志；
16. `httputil` 原始 error 可由统一入口记录，响应包装后 SSE 仍支持 `http.Flusher`。

## 9. 非目标

- 不重写 Registry、Executor 或 MCP Streamable HTTP。
- 不新增状态机、provider 抽象或通用结果总线。
- 不改变 rerank、多样性和 token 算法；本提案只处理工具实现及其交付边界。
- 不为历史非法数据库行增加永久读时兼容。
- 不针对单个查询、实体或工具结果做硬编码修复。

## 10. 总结

工具调用链整体连通，写动作隔离和结果交付审计也有真实价值，不需要推倒重建。需要处理的是叠加在主路径上的第二套错误协议、匿名 map、JSON 反解析、重复评分、隐式字段改写和静默兜底。

实施应先恢复错误可观察性，再删除字段反解析与兼容包装，随后收敛 typed contract，最后进行局部性能和死代码清理。每个切片独立验收，全部通过后再把最终合同归并到正式架构文档。
