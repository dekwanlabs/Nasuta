# QA 评估问题改进提案

> 状态：实施中（3.1、3.2 核心修复已完成）
> 创建日期：2026-08-03
> 范围：QA 检索、API 提取、工具选择、歧义处理、上下文预算与运行稳定性
> 关联评估：任务 ID `20260803T084131.566214000Z`

## 0. 文档生命周期

本文是独立开发提案，不直接修改正式架构文档。

执行顺序：

1. 使用可复现的评估记录核验问题和目标行为。
2. 按开发切片修改 Nasuta，并为通用机制补齐测试。
3. 在 CodeLoom 中重新构建、重启和重建受影响索引。
4. 运行固定数据集和补充回归用例，比较分数、延迟和方差。
5. 将稳定实现合并到正式 QA、Agent、检索和索引设计文档。
6. 标记本文为 `Implemented`，记录实际变更和遗留项。

当前状态：CodeLoom Eva 已修正评估侧引用明细和稀疏 ranking 评分语义；Nasuta 已完成 3.1 canonical 引用 target 和 3.2 Java/Python endpoint 抽取的核心修复，其余切片等待评审和实施。endpoint 索引统计、解析失败指标及 CodeLoom 历史索引重建仍待完成。

## 1. 评审范围

本次评估涉及以下链路：

```text
源码
  -> 多语言结构索引和 API 抽取
  -> SQLite 结构记录 / Qdrant 语义索引 / BM25
  -> list_apis / search_code / trace_deps
  -> Agent 查询分析和工具选择
  -> reason -> tool -> observe 循环
  -> 最终回答、引用和运行 trace
  -> CodeLoom Eva 评分
```

评审维度包括：

- 引用身份是否稳定且可回溯
- API 清单是否完整
- 语义检索是否具有足够召回和跨文件覆盖
- 服务级依赖问题是否选择权威工具
- 同名实体是否在取证前完成消歧
- 工具结果和上下文是否受统一预算约束
- 重复运行的超时、得分和工具调用数是否稳定
- 修复是否改善通用机制，而不是适配单个评估用例

## 2. 总体结论

评估暴露的主要问题不在被索引业务服务，而在 Nasuta 的索引、检索和 Agent 编排机制：

1. **引用展示值和引用身份曾混用**：缩短后的路径适合展示，但不能作为持久化和评估使用的 canonical target。
2. **结构化召回存在缺口**：API 抽取遗漏会直接导致 `list_apis` 无法返回完整路由。
3. **语义检索缺少多意图、多文件覆盖保障**：单一查询同时要求多个实现时，候选池可能被局部高分结果占满。
4. **权威工具选择仍依赖模型概率**：明确的服务级依赖问题可能直接基于预检索上下文作答，没有调用 `trace_deps`。
5. **同名实体没有在入口处形成稳定的消歧门禁**：Agent 会在歧义未解除时持续搜索，并把候选片段误当作目标实体。
6. **工具循环按轮次累加完整结果**：上下文可增长到十几万甚至三十多万字符，引起高延迟和 120 秒超时抖动。

这些问题应分别在其所有权边界修复：索引器负责完整抽取，检索负责候选覆盖，查询分析负责意图和歧义判定，Agent 循环负责预算和收敛，引用组装负责 canonical identity。

## 3. 已确认问题

### 3.1 P0：引用 target 不能使用缩短路径

实施状态：已完成。

证据：

- `internal/retrieval/pipeline.go` 的 `formatCodePool` 同时生成展示标题和 `Reference`。
- 评估引用期望使用完整 `repos/...` 路径定位文件。
- 缩短路径依赖服务映射和展示规则，可能丢失仓库前缀，不能作为稳定身份。
- CodeLoom Eva 已能保存并展示完整引用明细，因此 Nasuta 必须提供未压缩的 target。

影响：

- 评估无法按 canonical path 匹配相关文件。
- 不同仓库中的同名相对路径可能发生冲突。
- 前端点击、后续代码定位和历史报告回放缺少稳定目标。

修改方案：

- `Reference.Target` 始终保存原始 canonical `d.filePath`。
- `Reference.Label` 可以继续使用 `shortPath` 生成紧凑展示文本。
- code、codegraph 和其他文件型引用遵守同一约束。
- 增加测试，分别验证“label 可缩短”和“target 必须保留完整路径”。
- 引用 target 的 canonical 规则在检索组装边界建立，下游不得再次猜测或补全路径。

预期效果：

- 评估、前端和后续工具共享同一文件身份。
- 展示可读性与机器可定位性不再互相牺牲。
- 引用明细可在历史运行中稳定回放。

### 3.2 P0：API 抽取遗漏导致 `list_apis` 返回不完整

实施状态：核心抽取修复和回归测试已完成；索引阶段统计及 CodeLoom 历史数据重建待完成。

证据：

- `GLD-RET-007`（问题：“检索 hsds-aiot-embedding 服务中默认 Embedding 能力的完整 API 路由。”）调用 `list_apis`，HTTP 状态正常，但未返回期望的完整路由，得分为 `66.67`。
- `internal/agent/tools.go` 的 `FindAPIs` 只读取结构化 endpoint store；工具本身不会从源码临时推导路由。
- 多语言 endpoint 由 `internal/indexing/indexer` 下的语言索引器抽取。任何抽取遗漏都会成为结构化清单的永久缺口，直到重新索引。

影响：

- `list_apis` 的“权威完整路由”契约与实际数据不一致。
- Agent 可能退回 `search_code` 并从局部装饰器或注解拼接路由。
- 运行日志、调用链和事故分析无法获得可靠 endpoint scope。

修改方案：

- 为 Java、Python 及其他已支持语言建立表驱动的 endpoint 抽取契约测试。
- Python 覆盖 router 前缀、装饰器路径、空路径、多个 method decorator、跨行参数和常见 FastAPI/APIRouter 写法。
- Java 覆盖类级与方法级 mapping 合并、数组路径、空方法路径和常见 shortcut annotation。
- 抽取结果在写入结构化 store 前完成 canonical path 拼接、method 规范化和同一 endpoint 去重。
- bootstrap trace 记录每个仓库、语言和文件的 endpoint 数量及解析失败数。
- 修改索引器后通过显式全量重建或受影响仓库重建修复历史数据，不增加在线读取兼容分支。

预期效果：

- `list_apis` 返回可验证的完整路由，而不是依赖 Agent 推理。
- 解析能力扩展由通用 fixture 驱动，不针对某个服务或路径特判。
- endpoint 数量异常可在索引阶段发现。

### 3.3 P0：语义检索缺少跨文件覆盖和可恢复候选池

证据：

- `GLD-RET-009`（检索问题：“设备语音网关校验 access token 并调用设备认证服务的 Feign 实现”）和 `GLD-RET-017`（检索问题：“设备助手 大厨助手 灯效生成 三个流式对话入口的 FastAPI 路由”）的 `search_code` 结果均为 `0` 分。
- 前一个问题要求定位一个具体实现，但目标文件没有进入有效 ranking。
- 后一个问题要求在一次查询中找到三个并列入口，单一全局 top K 没有保证三个子意图都获得候选。
- `internal/agent/tools.go` 的 `CodeSearchResult` 执行 dense/hybrid 搜索后按文件去重并截断；候选一旦在前置阶段丢失，后续无法恢复。

影响：

- rerank 只能调整已有候选，不能挽回未召回目标。
- 多目标问题容易被一个词频高或 chunk 多的文件占满候选池。
- 相同实现的不同表述可能出现得分剧烈变化。

修改方案：

- 分开配置 backend recall limit、融合候选池、rerank pool 和最终 top K。
- 进入最终排序前先按 canonical file 聚合 chunk，保留每个文件的最佳分数和有限证据片段。
- 对可拆分的并列子意图执行有上限的多查询召回，再以 `map[path]bestCandidate` 单次聚合。
- 候选池为不同查询意图和检索来源保留最低覆盖配额，剩余名额按统一分数竞争。
- rerank 输入记录 query、source、candidate count、selected count 和被截断的来源分布。
- 以 Recall@K、MRR、NDCG 和跨文件覆盖率验收，不添加服务名、类名、路径或问题关键词特判。

预期效果：

- 单目标实现的召回率提高。
- 多目标问题可以在有限 top K 内覆盖多个文件。
- reranker 获得足够候选纠正粗排误差。

### 3.4 P1：服务级依赖问题未稳定选择 `trace_deps`

证据：

- `GLD-TOOL-006`（问题：“hsas-user-message 直接依赖哪些下游服务？只看服务级依赖。”）明确要求查询服务级下游依赖。
- 三次运行均未调用工具，得分稳定为 `45`。
- `internal/agent/registry.go` 已明确声明 `trace_deps` 是服务级上下游依赖的权威工具。
- 当前工具说明虽然正确，但实际选择仍由模型在通用工具集合中自行决定。

影响：

- Agent 可能依据预检索片段、文档描述或服务标签推断依赖。
- 回答缺少 ontology edge 证据，无法证明方向、深度和完整性。
- 同一明确意图的行为受模型波动影响。

修改方案：

- 在查询分析结果中表达结构化 dependency intent：目标服务、方向和深度。
- 当问题已经提供 canonical service 且明确要求服务级依赖时，生成 `trace_deps` 的首选工具路由候选。
- 路由只约束“权威证据来源”，不硬编码任何具体服务名。
- 预检索上下文不能替代该权威查询；只有 ontology capability 未启用时才明确降级并记录原因。
- 增加上游、下游、双向、直接依赖和多层 blast-radius 的表驱动测试。

预期效果：

- 明确的服务依赖问题稳定使用 ontology，而不是自由文本搜索。
- 工具参数方向与用户意图一致。
- 依赖问答的完整性和可解释性提高。

### 3.5 P0：同名实体歧义未在取证前阻断

证据：

- `GLD-TOOL-011`（问题：“帮我分析 UserController 的实现。”）、`GLD-TOOL-012`（问题：“OauthService 的 token 校验逻辑是什么？”）和 `GLD-CON-022`（问题：“UserController 的所有接口有哪些？”）都要求在同名实体无法唯一定位时先澄清。
- 实际运行仍调用了多个工具。其中，结论阶段的 UserController 接口问题每次调用约 `9` 至 `11` 个工具，得分稳定为 `30`。
- OauthService token 校验问题的单次运行调用 `8` 个工具并耗时约 `113.9` 秒。
- 当前 Agent prompt 要求谨慎使用证据，但没有一个在工具循环前执行的歧义门禁。

影响：

- Agent 可能混合多个仓库或服务中的同名类。
- 工具调用越多，错误候选越容易被累积成看似充分的证据。
- 应当在一轮内完成的澄清变成长时间检索，增加成本和超时概率。

修改方案：

- 查询分析阶段识别“裸实体名”输入：只有类、接口、方法等标识符，没有 service、repository、canonical path 或其他唯一限定。
- 使用有界的结构化符号候选查询判断基数，不使用开放式多轮语义搜索完成消歧。
- 候选为 `0` 时说明未找到并请求补充信息；候选为 `1` 时继续；候选大于 `1` 时直接返回澄清问题和有限候选摘要。
- 歧义结果在进入通用 Agent loop 前处理，因此不会先调用其他业务工具。
- 用户补充 service 或 path 后，把该限定作为后续工具调用的 canonical scope。
- 使用通用重复类名 fixture 测试，不以评估中的具体类名建立规则。

预期效果：

- 同名类不会被跨服务拼接成一个虚假实现。
- 歧义问题快速返回可操作的澄清项。
- 工具调用数和无效上下文显著下降。

### 3.6 P0：工具结果累加导致上下文无界膨胀

证据：

- `internal/agent/loop.go` 在每轮工具执行后把 `execution.PromptContent` 追加到 messages。
- `ensureInputBudget` 只在下一次模型调用前检查总输入，没有在每个工具结果进入上下文时执行共享预算分配。
- 评估日志出现约 `125,863`、`217,232` 和 `350,096` 字符的上下文；同一轮次还可观察到从约 `171,225` 增长至 `334,179` 字符。
- 工具 trace 中存在大段 SQL、配置或代码结果，重复调用会持续放大历史消息。

影响：

- 模型输入成本和首 token 延迟随轮次快速增长。
- 重要证据被大量低相关内容稀释。
- 接近 Provider 上下文上限时，运行可能在很晚阶段失败。
- 相同问题因工具返回规模不同而产生高方差。

修改方案：

- 为整个 run 建立统一 token 预算，明确分配 seed evidence、conversation、tool definitions、tool results 和 final answer reserve。
- 工具执行保留完整 authoritative artifact，但进入模型的 `PromptContent` 必须经过通用预算器。
- `prepareToolDelivery` 按结构化结果优先保留身份、摘要、top items、coverage 和 artifact reference；正文按剩余 token 截断。
- 同一 canonical 证据只进入上下文一次，使用 map-keyed dedup，不能通过遍历历史反复查重。
- 新工具结果进入上下文前计算边际 token 成本；预算不足时返回 partial coverage 和 omitted count，而不是先追加再报错。
- trace 记录 authoritative tokens、delivered tokens、omitted tokens、deduped items 和 remaining budget。

预期效果：

- 完整结果仍可审计，模型输入保持严格上限。
- 工具轮次增加不会导致上下文近似无界增长。
- 延迟、成本和超时率下降。

### 3.7 P1：工具循环缺少基于证据增量的稳定收敛

证据：

- `GLD-CON-023`（问题：“仅根据代码仓库和设计文档，判断当前线上 hsds-aiot-service 的 BERT 路由是否健康、过去十分钟是否有超时。”）三次尝试中，一次约 `120` 秒超时，另外两次为 `100` 分。
- `GLD-TOOL-012`（问题：“OauthService 的 token 校验逻辑是什么？”）也出现接近 `120` 秒的长运行。
- 同一类问题在不同尝试中工具调用数可从 `0` 增长到 `15` 或 `17`。
- `internal/agent/loop.go` 已有调用 fingerprint 去重和 step limit，但“换参数继续搜索”仍可能产生低增量循环。

影响：

- 平均分不能反映运行可靠性，单次用户请求仍可能超时。
- 高分回答与超时回答来自同一问题，说明流程方差大于内容难度。
- 工具调用数增加不一定带来新证据，却持续消耗上下文和总超时。

修改方案：

- 每轮汇总新增 canonical evidence 数、重复证据数、失败工具数和 coverage 增量。
- 连续一轮没有新增证据时要求模型基于现有证据作答；连续达到明确阈值时强制结束工具阶段。
- 对运行时事实问题，若所需 capability 未启用或查询失败，应尽早说明无法确认，不通过代码和文档搜索替代运行时证据。
- 将 per-tool timeout、loop deadline 和 answer reserve 纳入同一预算计算，工具阶段不得消耗最终回答保留时间。
- 对重复运行记录 P50、P95、最大工具调用数、超时率和 score standard deviation。

预期效果：

- Agent 在证据不再增长时及时收敛。
- 运行时证据不可用时快速、明确地 abstain。
- 同一问题的延迟和得分方差下降。

## 4. 安全性评估

本提案不新增写工具或权限范围，但实施时需要保持以下边界：

- 歧义候选只能返回允许展示的 service、repository、symbol 和 canonical path，不暴露凭证或隐藏配置。
- authoritative tool artifact 与模型 prompt 分离后，artifact 下载继续执行 user、session 和 run 所有权校验。
- 工具结果压缩不能删除 coverage、error、omitted count 等不确定性标记。
- 运行时 capability 未配置或调用失败时必须可观察，不得静默切换到代码、文档或其他 provider 并声称获得实时结论。
- 引用 target 只允许 workspace canonical path 或受控 URI，不接受模型生成的任意本地路径。

## 5. 目标架构原则

### 5.1 展示标签与稳定身份分离

`Label` 面向人类阅读，`Target` 面向定位、去重、持久化和评估。所有路径缩短只发生在展示层。

### 5.2 权威结构数据在索引边界建立

API 路由、服务依赖和符号身份由索引器与 ontology 建立。Agent 不从片段重新拼接完整结构事实。

### 5.3 检索先保证覆盖，再进行排序

backend recall、融合池、rerank pool 和 final top K 是不同阶段。排序不能弥补候选已经丢失的问题。

### 5.4 歧义是输入条件，不是搜索策略

同名实体无法唯一定位时，系统应请求用户提供 scope。增加工具轮次不能把多个候选自动变成唯一事实。

### 5.5 完整审计与有限模型上下文分离

工具原始结果进入 artifact 和 trace；模型只接收在统一 token 预算内选择的证据投影。

### 5.6 循环继续必须由新证据证明

工具步骤只在产生新 canonical evidence 或解决明确缺口时继续。重复、失败和低增量调用触发收敛。

## 6. 开发切片

### Slice 1：稳定引用身份

- 统一文件型 `Reference.Target` 为 canonical path。
- 保留紧凑 `Label`。
- 增加 code 和 codegraph 引用测试。

### Slice 2：补齐 endpoint 抽取

- 建立多语言 endpoint fixture。
- 修复通用抽取和 canonicalization。
- 增加 bootstrap endpoint 统计。
- 重建受影响索引并验证 `list_apis`。

### Slice 3：提高语义召回覆盖

- 分离各阶段候选上限。
- 按文件聚合并保留最佳候选。
- 增加有界多意图召回和覆盖配额。
- 建立 Recall@K、MRR、NDCG 回归。

### Slice 4：增加权威工具路由

- 查询分析输出 dependency intent。
- 将明确服务级依赖路由到 `trace_deps`。
- 增加方向、深度和 capability 缺失测试。

### Slice 5：增加歧义门禁

- 识别缺少 scope 的裸实体。
- 使用有界结构查询判断候选基数。
- 多候选时在 Agent loop 前返回澄清。
- 增加通用同名 symbol 回归。

### Slice 6：统一工具上下文预算

- authoritative artifact 与 prompt projection 分离。
- 工具结果按 token 预算选择和截断。
- canonical evidence 去重。
- 增加 coverage、omitted 和 budget trace。

### Slice 7：降低运行方差

- 计算每轮证据增量。
- 无增量时结束或强制总结。
- 保护 final answer reserve。
- 增加重复运行稳定性门禁。

## 7. 验收门禁

### 7.1 自动化测试

```bash
GOWORK=off go test ./internal/indexing/indexer/...
GOWORK=off go test ./internal/retrieval/...
GOWORK=off go test ./internal/agent/...
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

必须新增或保留以下回归：

- 引用 label 缩短但 target 保持完整
- Python 和 Java 完整路由抽取
- 同一查询覆盖多个目标文件
- 明确依赖意图选择 `trace_deps`
- 同名 symbol 多候选时零业务工具调用
- 大工具结果按预算投影且 artifact 完整
- 连续无新证据后稳定收敛

### 7.2 CodeLoom 集成验证

Nasuta 修改合入后：

1. 重新构建并重启 CodeLoom。
2. 对 endpoint 抽取变化执行全量或受影响仓库重建。
3. 确认 Qdrant、SQLite ontology snapshot 和 BM25 来自同一次有效 bootstrap。
4. 使用同一数据集和配置重跑任务，不在验证期间修改用例期望。

### 7.3 质量指标

- `GLD-RET-007`（问题：“检索 hsds-aiot-embedding 服务中默认 Embedding 能力的完整 API 路由。”）返回完整 endpoint。
- `GLD-RET-009`（检索问题：“设备语音网关校验 access token 并调用设备认证服务的 Feign 实现”）的目标文件进入 top K。
- `GLD-RET-017`（检索问题：“设备助手 大厨助手 灯效生成 三个流式对话入口的 FastAPI 路由”）的三个目标文件均进入 top K。
- 明确服务级依赖问题稳定调用一次 `trace_deps`，参数方向正确。
- 裸同名实体多候选时不进入通用工具循环。
- 单次 run 的 delivered tool tokens 和总 input tokens 不超过配置预算。
- 重复三次运行不得出现 120 秒超时。
- 同类用例 score standard deviation 显著下降。

### 7.4 非回归门禁

- 不降低已有唯一实体、单文件检索和简单 API 查询用例分数。
- 不把 optional backend 故障静默替换成其他 provider。
- 不丢失工具完整 artifact、错误和 coverage 信息。
- 不增加无界存储读取或 O(n²) 去重。

## 8. 非目标

- 不修改被索引的业务仓库来适配评估。
- 不修改 Qdrant 服务或其部署配置。
- 不在 CodeLoom 根 `internal/*` 复制 Nasuta 的检索、索引或 Agent 逻辑。
- 不为评估中的服务名、类名、路径或问题文本增加特判。
- 不通过放宽所有断言掩盖真实的召回和工具选择问题。
- 本轮不实施 3.3 至 3.7 的 Nasuta 运行时代码修改。

## 9. 实施记录

### 2026-08-03：评估侧先行修复

CodeLoom Eva 已完成：

- 保存和展示完整引用明细。
- 为尝试记录增加独立引用 JSON 持久化。
- 稀疏 relevance 标注默认不把未标注结果判为无关。
- ranking 评分按实际可用指标重新归一化。
- 修复两个过度依赖固定否定措辞的结论断言。

Nasuta：

- `internal/retrieval/pipeline.go` 已将 code 和 codegraph 的 `Reference.Target` 统一为原始 canonical path，紧凑路径仅用于 `Reference.Label`。
- `internal/retrieval/pipeline_test.go` 已增加 canonical target 回归测试。
- `internal/indexing/indexer/python.go` 已支持多行 `APIRouter(prefix=...)`、多个 router、跨行 route decorator、空路径、堆叠 decorator 和命名 `path` 参数。
- `internal/indexing/indexer/java.go` 已支持类级与方法级 mapping 路径组合、路径数组、空方法路径、shortcut annotation 和多个 `RequestMethod`。
- `internal/indexing/indexer/indexer_test.go` 已增加 FastAPI 和 Spring endpoint 抽取契约测试。
- 本轮未实现 bootstrap 的按仓库、语言、文件 endpoint 数量及解析失败数指标，不将其标记为已完成。
- CodeLoom 部署新版本后必须执行全量或受影响仓库的结构索引重建，历史 endpoint 记录不会通过在线读取自动修复。
