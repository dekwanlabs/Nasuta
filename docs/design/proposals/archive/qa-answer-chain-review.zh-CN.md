# QA 回答链路代码质量评审与治理提案

> 状态：已实现并归档，非规范性历史记录
> 创建日期：2026-08-01
> 归档日期：2026-08-01
> 范围：QA 请求、检索、重排、Agent 循环、工具执行、SSE、终态持久化、会话归档
> 关联提案：`qa-sse-protocol-simplification.zh-CN.md`

## 0. 文档生命周期

本文是独立开发提案，不直接修改正式架构文档。

执行顺序：

1. 核验问题和目标行为。
2. 按开发切片修改代码并补齐测试。
3. 通过单元测试、集成测试和人工验收。
4. 将最终实现合并到正式 QA、Agent、检索和 SSE 设计文档。
5. 标记本文为 `Implemented`，记录实际变更和遗留项。

当前状态：实现和自动化验证已完成，提案已归档。正式基线以 `agent-platform` 下的架构、检索、会话和可观测性文档为准。

## 1. 评审范围

当前 QA 主链：

```text
POST /api/qa/ask
  -> 请求解析与会话上下文准备
  -> 查询分析和证据路由
  -> 历史召回、记忆召回、内部检索
  -> 候选截断、去重、rerank、多样性选择
  -> Agent reason -> tool -> observe 循环
  -> RunHub 事件
  -> Dashboard SSE
  -> run.finished
  -> 会话轮次持久化
  -> 异步历史归档
```

评审维度包括：

- 代码正确性与业务逻辑
- 可读性与职责划分
- 可维护性与扩展性
- 性能与资源开销
- 稳定性与异常处理
- 权限与敏感信息风险
- 架构边界与可测试性

## 2. 总体结论

QA 链路已经具备工具权限快照、参数校验、调用超时、结构化终态和会话存储等基础能力，但仍有四类系统性问题：

1. **失败语义失真**：模型中断或答案仍被截断时，可能被记录为成功。
2. **召回运行状态缺少内部观测**：多路召回允许子路失败并按零结果继续，但当前只能依赖分散日志定位失败来源。
3. **持久化职责错位**：会话归档依赖 SSE 连接消费终态，而不是依赖 run 生命周期。
4. **检索召回存在结构性损失**：过早截断、按标题合并文档、只收集一个服务的依赖。

优先级应先保证“成功就是完整成功”，再简化事件和检索机制。

## 3. 已确认问题

### 3.1 P0：部分答案掩盖模型失败

证据：

- `internal/agent/loop.go:287`
- `internal/agent/loop.go:331`
- `internal/agent/loop.go:594`
- `internal/agent/service.go:1595`

模型调用失败、超时或续写失败时，只要已经产生可见文本，`preserveInterruptedAnswer` 就保存文本并让循环进入已回答状态，但不保留原始错误。`outcomeFor` 最终将其判定为 `done`。

示例：模型正在回答“订单创建链路包含 API 层、领域服务和消息投递”，输出到“领域服务写入订单后……”时 Provider 超时。当前实现会保留这半段文本，并产生 `status=done`；前端和会话历史都把它当成完整答案，用户无法知道消息投递部分实际没有生成。

影响：

- 不完整答案被展示和持久化为成功答案。
- 失败答案可能进入记忆提取。
- 运行指标无法反映真实 Provider 失败率。

修改方案：

- 将 `preserveInterruptedAnswer` 改为只负责保存可展示的部分文本，不再通过布尔返回值暗示运行成功。
- 调用处在保留文本的同时把原始 Provider、超时或续写错误写入 `RunResult.Err`。
- `outcomeFor` 保持“存在运行错误就不能是 `done`”的单一判定规则。
- 若产品需要展示部分结果，终态增加明确的 `partial`；否则使用 `failed` 并携带 `answer` 作为失败前已生成内容。
- `memoryExtractionAllowed` 只接受完整的 `done` 结果。

预期效果：

- 前端可以展示已生成文本，同时明确提示回答未完成。
- 会话、记忆和成功率指标不再把 Provider 中断误算成成功。
- 终态判定集中在 `outcomeFor`，减少调用点各自推断成功状态。

### 3.2 P0：续写达到上限仍返回成功

证据：

- `internal/agent/loop.go:643`
- `internal/agent/loop.go:665`

达到 `MaxContinueRounds` 后，如果 `finish_reason` 仍为 `length`，当前逻辑只写 warning 并返回 `nil` error。

示例：一次架构评审需要输出 12 个服务，首次回答只写到第 6 个服务，随后两轮续写又写到第 10 个服务，但最后一次仍返回 `finish_reason=length`。当前逻辑仍以成功结束，缺失的两个服务不会触发错误或不完整状态。

修改方案：

- 增加稳定错误 `ErrAnswerTruncated`。
- `continueIfNeeded` 达到续写上限且仍为 `FinishLength` 时返回当前结果和该错误。
- 调用方保留已生成内容，但按上一问题的终态规则标记为 `partial` 或 `failed`。
- 增加“首次截断、续写成功、续写耗尽、续写调用失败”测试。

预期效果：

- 不完整回答不会静默进入成功路径。
- 日志、run 记录和前端展示使用一致的截断语义。
- 调整 `MaxContinueRounds` 后仍能通过测试验证行为边界。

### 3.3 P2：多路召回子路失败缺少统一观测

证据：

- `internal/retrieval/pipeline.go:200`
- `internal/retrieval/pipeline.go:226`
- `internal/retrieval/pipeline.go:279`
- `internal/retrieval/pipeline.go:297`
- `internal/retrieval/pipeline.go:356`

`discover` 并行执行代码、Runbook 和服务检索。多路召回的产品语义是允许任一子路失败，并对 Agent 表现为该路没有结果：

- 后端正常返回零结果
- 单一来源失败
- 多个来源失败

这三种情况都不应向模型返回工具错误，也不应因为某一路失败而中断回答。当前不足仅在于失败日志分散，运行观测无法快速统计各召回子路的可用性。

示例：一次问答同时执行代码、Runbook 和服务三路召回。代码召回返回 8 条，Runbook 返回 0 条，服务召回因数据库超时失败。模型应只看到代码证据并正常回答，不应收到“服务搜索失败”的错误提示；运维侧则应能看到本轮状态为 `code=completed`、`runbook=empty`、`service=failed`。

修改方案：

- Agent 侧继续将失败子路视为零结果，不注入错误文本。
- 在 `discover` 内部使用固定的来源状态结构汇总代码、Runbook 和服务召回结果。
- 每个 goroutine 只写自己负责的结果和状态，汇总后统一记录 trace、结构化日志或指标。
- `discover` 不因单一或多个召回子路失败而返回业务错误，失败来源的结果保持为空。
- source status 仅用于运行观测，不进入模型上下文，不改变回答终态。
- 只有整个检索组件无法执行、上下文取消或程序协议错误才向调用方返回错误。

预期效果：

- 模型仍按已有证据自然回答，不会被基础设施错误文本干扰。
- 运维可以区分真实零命中与后端失败，并统计每条召回链路的可用率。
- 多路召回继续保持部分可用，不扩大单点失败影响。

### 3.4 P0：会话归档依赖 SSE 连接

证据：

- `internal/transport/dashboard/qa.go:305`
- `internal/transport/dashboard/qa.go:356`
- `internal/transport/dashboard/qa.go:362`
- `internal/transport/dashboard/qa.go:653`

只有 HTTP handler 收到成功的 `run.finished` 且客户端仍保持连接时，才执行 `saveTurnToSession`。客户端断开或传输失败后，run 可以成功，但 `qa_turns` 不一定生成。

示例：用户提交问题后关闭浏览器标签页，Agent 在后台继续运行并把 `agent_runs` 更新为 `done`。由于 SSE handler 已退出，没有代码再调用 `saveTurnToSession`，重新打开该会话时看不到这轮问答，数据库中也出现有 run、无 turn 的不一致状态。

修改方案：

- 将 `saveTurnToSession` 从 `serveAgentSSE` 移到 run completion 所属的应用服务。
- run 完成后先持久化终态和 session turn，再通过 RunHub 发布终态投影。
- SSE 只投影运行事件，不拥有业务归档。
- 使用 `(session_id, run_id)` 唯一约束或等价机制保证幂等。
- 保存失败写入统一错误日志和 run 持久化状态，不能依赖客户端连接重试。
- 客户端断线不得影响 run 结果和会话轮次的一致性。

预期效果：

- 浏览器关闭、网络断开和慢客户端不再导致会话轮次丢失。
- `agent_runs` 与 `qa_turns` 可以通过 `run_id` 稳定关联。
- HTTP handler 只承担传输职责，归档逻辑更容易单元测试。

### 3.5 P1：多服务依赖只保留第一个结果

证据：

- `internal/retrieval/collection.go:198`
- `internal/retrieval/collection.go:236`

`collectDeps` 找到第一个存在边的服务后立即返回。多服务架构和调用链问题无法获得完整依赖上下文。

例如，用户询问：

```text
梳理 Alexa/Google Skill 到设备控制的完整调用链。
```

召回阶段可能识别出以下候选服务：

```text
hsmf-mobile-gateway
hsas-voice
hsmf-device-gateway
```

假设依赖查询分别返回：

```text
hsmf-mobile-gateway -> hsas-voice
hsas-voice -> hsmf-device-gateway
hsmf-device-gateway -> MQTT
```

当前实现查询到 `hsmf-mobile-gateway` 存在依赖边后便立即 `return`，最终上下文可能只包含：

```text
hsmf-mobile-gateway -> hsas-voice
```

模型看不到后两个服务的依赖关系，容易把局部链路回答成完整架构。目标实现应继续查询剩余候选服务，在统一预算内形成：

```text
hsmf-mobile-gateway -> hsas-voice
hsas-voice -> hsmf-device-gateway
hsmf-device-gateway -> MQTT
```

如果不同服务返回了同一条边，应只保留一次。例如两个查询都返回
`hsas-voice -> hsmf-device-gateway`，最终上下文不能重复写入。若总预算为
30 条边，在达到预算后还剩 2 个服务未查询或 12 条边未纳入，则应在内部
trace 中记录截断数量；不需要把这类运行统计注入模型上下文。

修改方案：

- 删除找到第一组边后的 `return`，继续遍历候选服务。
- 使用 `map[dependencyKey]struct{}` 按方向、起点和终点对依赖边去重。
- 将原来的单服务边数限制改为整个 `collectDeps` 调用共享的总边数预算。
- 达到预算后立即停止后续查询或写入，并在 trace 中记录已查询服务数、未查询服务数和省略边数。
- 保留稳定的服务顺序和边顺序，避免相同输入产生随机上下文。

预期效果：

- 多服务架构问题能够获得跨服务的连续依赖链。
- 重复边不再浪费上下文预算。
- 依赖上下文仍有严格上限，不会因服务数量增加而无界膨胀。

### 3.6 P1：Runbook 按标题合并不同文档

证据：

- `internal/retrieval/collection.go:92`
- `internal/retrieval/collection.go:123`
- `internal/retrieval/collection.go:173`

当前以标题为聚合键。不同 `docID` 的同名文档会被混合，引用却使用第一条数据的 `docID`。

示例：知识库中存在两份标题均为“设备控制流程”的文档，一份属于国内环境，内容为“通过 Kafka 下发”，另一份属于海外环境，内容为“通过 MQTT 下发”。当前实现可能把两个 chunk 合并成一个候选，正文同时包含 Kafka 和 MQTT，但引用只指向国内文档，模型可能错误地回答同一链路同时使用两种通道。

修改方案：

- 将 `byTitle`、`seenText` 和顺序集合的 key 全部改为稳定的 `docID`。
- 标题仅保留在聚合值中用于展示，不再承担身份语义。
- 同一 `docID` 内继续按 chunk 文本去重并受 `maxChunksPerRunbook` 限制。
- 输出引用、正文、分数、文档类型和信任等级都取自同一个文档聚合对象。
- 增加“同标题不同 docID”和“同 docID 多 chunk”测试。

预期效果：

- 不同文档的证据和引用不再串线。
- 同一文档的多个相关片段仍可合并，保持现有上下文压缩能力。
- 标题修改或重复不会改变文档身份。

### 3.7 P1：rerank 前截断造成不可恢复的召回损失

证据：

- `internal/retrieval/rerank.go:62`
- `internal/retrieval/pipeline.go:279`

候选先由 `topByRecall` 截断，再交给 reranker；Runbook 初始查询还固定限制为 5 条。初排遗漏的候选无法被重排恢复。

示例：初始召回得到 20 个候选，真正解释系统全貌的概览文档粗排位于第 7；`RerankPool=5` 时它在调用 reranker 前已经被删除。即使远程 reranker 能准确识别该文档最相关，也没有机会对它评分。

修改方案：

- 分开定义 source recall limit、rerank pool 和 final top K，删除同一个数字跨阶段复用。
- 每个来源先在自身读取上限内返回候选，再做统一去重和候选池配额。
- 在进入 reranker 前为代码、Runbook、服务等来源保留最低候选配额，剩余位置按初排分数竞争。
- 仅在候选池超过 reranker 可接受上限时执行粗截断，并在 trace 中记录被截断来源分布。
- 用通用召回指标评估，不对具体问题、文档名或关键词增加特判。

预期效果：

- reranker 有机会纠正初排误差，而不是只能重排已经偏斜的小集合。
- 单一来源或单一服务不容易提前占满候选池。
- 参数含义清晰，可分别调节召回成本、rerank 成本和最终上下文大小。

### 3.8 P1：配置 reranker 失败后隐式改变算法

证据：

- `internal/retrieval/rerank.go:240`
- `internal/retrieval/rerank.go:255`
- `internal/retrieval/rerank.go:258`

远程 reranker 失败后自动使用本地 `denseReranker`。日志可见，但返回数据和 trace 没有表达实际 backend 和 fallback mode。

示例：同一问题上午由 DashScope 将架构总览排到第一，下午 DashScope 超时后本地 substring 算法把包含更多重复关键词的局部代码排到第一。两次运行在 trace 中都表现为“rerank enabled”，排位变化无法从运行记录中解释。

修改方案：

- 保持一个显式 rerank dispatcher，由它选择配置的 backend，不在 Provider 实现内部替换算法。
- 远程调用返回错误时，由检索编排层根据明确配置决定是保持原始 recall 顺序，还是执行已声明的本地 rerank。
- 如果允许本地降级，配置中增加明确开关和策略名，默认行为不得靠代码隐式推断。
- trace 记录 requested backend、actual mode、fallback enabled、duration 和 error。
- 删除“错误但 scores 长度不匹配”与“调用错误”共用模糊日志的做法，分别记录协议错误和调用错误。

预期效果：

- 相同配置下的排序行为可以从 trace 完整解释。
- Provider 故障不会悄悄改变算法。
- 后续新增 reranker 只需增加一个 backend 实现和 dispatcher 分支。

### 3.9 P1：上下文预算单位不一致

证据：

- `internal/retrieval/pipeline.go:139`
- `internal/retrieval/pipeline.go:435`
- `internal/retrieval/pipeline.go:441`
- `internal/agent/context_assemble.go:365`

检索阶段的 `ContextBudget` 按 rune 扣减，Agent 输入预算按 token 估算。配置名称和执行单位不一致。

示例：预算配置为 10,000。检索阶段会把 10,000 个中文字符近似当成 10,000 token 放入上下文，但 Agent 阶段的估算器可能认为它显著超过 10,000 token；结果是前一阶段认为预算充足，真正调用 Provider 前才报上下文超限。英文和代码又会产生不同偏差。

修改方案：

- 将 retrieval `ContextBudget` 明确定义为 token 数，复用统一 token estimator。
- 组装每个 `partial` 前计算一次 token 成本，按剩余预算选择、跳过或截断。
- 提供按 token 近似截断且保持 UTF-8 的公共函数，替换 rune 和 byte 截断。
- 对标题、引用标记和分隔符等固定开销一并计入预算。
- trace 记录 selected tokens、dropped tokens、remaining tokens 和 output reserve。

预期效果：

- 检索组装与 Provider 调用前检查使用相同量纲。
- 中文、英文和代码内容的预算差异缩小。
- 上下文超限更早、更稳定地在组装阶段被控制。

### 3.10 P1：问题文本启发式限制工具步骤

证据：

- `internal/agent/loop.go:122`
- `internal/agent/loop.go:127`
- `internal/agent/loop.go:154`
- `internal/agent/service.go:580`

多数问题被限制为 2 步，架构和代码评审限制为 3 步，只有硬编码关键词或 temporal 工具路由能得到完整预算。

示例：用户问“这个系统从外部请求到设备执行是怎么工作的”，问题没有命中“调用链”“上下游”等关键词，可能只获得 2 个步骤。Agent 第一步查入口、第二步查一个服务后就必须总结，无法继续验证网关、消息通道和设备侧；换成“完整调用链”措辞却能得到更多步骤，调查深度被文字表达而非证据需求决定。

修改方案：

- 删除 `MaxStepsFor` 中按回答类型压缩为 2 或 3 步的分支，以及 `extendedToolLoopSignals` 关键词表。
- `MaxSteps` 只作为所有问题一致的硬上限。
- 每轮工具执行后根据是否产生新证据、是否存在可执行工具调用、是否重复相同调用决定继续或结束。
- 保留现有工具 fingerprint 去重，并增加连续无新证据计数；达到阈值后要求模型基于现有证据总结。
- Web 等需要固定额外阶段的流程通过真实 workflow 需求表达，不通过问题措辞扩步。

预期效果：

- 同一调查任务不会因用户换一种说法而得到不同工具预算。
- 简单问题仍可提前结束，复杂问题可以在硬上限内继续取证。
- 防循环规则建立在实际运行事实上，可测试且更容易解释。

### 3.11 P2：SSE 写入错误不可传播

证据：

- `internal/transport/dashboard/qa.go:224`
- `internal/transport/dashboard/qa.go:245`

`fmt.Fprintf` 和 `Flush` 错误均被忽略，`sseWriter.emit` 无法反馈连接失败。

示例：客户端网络中断后，下一次 `answer.delta` 写入已经返回 broken pipe，但 `emit` 丢弃该错误。handler 仍继续消费 RunHub 事件并尝试 Flush，日志中没有本次 SSE 失败的统一记录，也无法及时停止无效的网络投影。

修改方案：

- 将 `sseWriter.emit` 改为返回 `error`，同时检查 `fmt.Fprintf` 的返回值和 ResponseController 可提供的 Flush 错误。
- heartbeat 和业务事件共用同一写失败处理函数，使用 `sync.Once` 只记录首次失败。
- 写失败后取消 SSE 投影循环并退订 RunHub，避免继续消费和写入事件。
- run 是否继续由独立的 run context 和产品策略决定，不能被 writer error 隐式决定。
- 统一日志包含 request ID、run ID、session ID、event type 和底层错误。

预期效果：

- broken pipe、客户端重置和代理断流能够被统一观察。
- 传输失败后不再持续执行无效 Flush 和事件转发。
- SSE 生命周期与后台 run 生命周期边界清晰。

详细事件协议和慢消费者策略由 `qa-sse-protocol-simplification.zh-CN.md` 负责。

### 3.12 P2：RunHub 满缓冲区任意丢事件

证据：

- `internal/agent/stream.go:370`
- `internal/agent/stream.go:378`

缓冲区满时丢弃任意非终态事件；插入终态前还会移除一个最旧事件。前端虽然会使用终态完整答案覆盖增量文本，但工具状态和诊断事件可能不完整。

示例：某轮产生大量 reasoning 和 trace，填满 512 个事件的缓冲区。随后 `tool.finished` 因缓冲区已满被丢弃，前端只显示工具开始，没有完成状态；终态到来时又移除一个最旧事件，但无法保证被移除的是可丢弃的 trace。

修改方案：

- 将事件按可靠性分为终态、业务状态和 best-effort 诊断三类。
- 终态使用独立槽位或独立 channel，不能通过移除任意旧事件来抢占空间。
- `tool.started` 与 `tool.finished` 作为成对业务状态，不允许只保留开始事件。
- reasoning、trace 和可覆盖的 status 可以在队列拥塞时合并或丢弃。
- 对每类事件记录 dropped、coalesced 和 queue high-water mark 指标。

预期效果：

- 前端不会长期停留在“工具执行中”等不完整业务状态。
- 诊断洪峰不会挤掉终态和关键工具事件。
- 慢消费者问题可以通过指标定位，而不是只依赖偶发 warning。

### 3.13 P2：会话追加缺少 run 幂等性

证据：

- `internal/memory/session.go:451`
- `internal/platform/dbschema/mysql.go:154`

每次 `AppendTurn` 都创建新 `turn_no`，表中没有 `(session_id, run_id)` 唯一约束。

示例：run `run-123` 已成功归档为第 8 轮，调用方因响应超时重试相同归档操作。第二次 `AppendTurn` 会再创建第 9 轮，使同一个问题和答案在会话中出现两次，后续历史召回和 token 统计也被重复放大。

修改方案：

- 先通过迁移识别空 `run_id`、重复 `(session_id, run_id)` 和无法关联的历史数据。
- 对非空 run 增加 `(session_id, run_id)` 唯一约束；空 run 的历史兼容由一次性迁移处理，不在在线读写路径保留永久分支。
- 将 `AppendTurn` 改为幂等接口：写入前或唯一键冲突后读取既有 turn 并返回。
- 校验 run 的 user、session 与归档请求一致，避免错误地把其他会话的 run 关联进来。
- 增加并发调用同一 run 的测试。

预期效果：

- 网络重试、重复事件和并发归档不会产生重复会话轮次。
- `run_id` 成为 run、turn 和归档上下文之间可靠的关联键。
- 历史统计和上下文压缩不会因重复轮次被放大。

### 3.14 P2：查询分析使用无收益 goroutine

证据：

- `internal/agent/service.go:286`
- `internal/agent/service.go:291`
- `internal/agent/service.go:338`

代码只启动一个 goroutine 并立即等待，没有并行收益，却引入跨 goroutine 变量写入和额外同步。

示例：请求线程创建 goroutine 执行 query analysis，紧接着调用 `preWg.Wait()`，期间没有任何其他任务并行执行。运行时间与直接调用相同，但 `cleanQuestion`、`terms`、`decision` 等变量变成跨 goroutine 写入，读者还需要额外确认同步和竞态安全。

修改方案：

- 删除 `preWg`、单一 goroutine 和跨 goroutine 写入的局部变量。
- 在当前请求 goroutine 中直接执行 query analysis，并保持现有 timeout context。
- `planningDuration` 使用调用前后的直接计时。
- 将不同分析分支的共同赋值收敛为一个局部结果对象，减少共享可变变量。

预期效果：

- 删除无收益的同步和调度开销。
- query analysis 控制流可以顺序阅读，不再需要推导 WaitGroup 的 happens-before 关系。
- 后续若增加真正并行任务，可在独立结果对象和明确合并点上重新引入并发。

### 3.15 P2：DashScope 文档按字节截断

证据：

- `internal/retrieval/rerank.go:419`

`body[:rerankDocChars]` 可能截断 UTF-8 字符，破坏中文内容。

示例：限制位置恰好落在汉字“链”的第二个 UTF-8 字节上，截断后的字符串包含非法编码。JSON 编码时可能替换为乱码字符，DashScope 实际评分的文档尾部与本地保存的原文不一致。

修改方案：

- 使用统一的 UTF-8 安全 token 截断函数处理 rerank 文档。
- 截断后保留完整 rune，不允许直接按 byte slice。
- 将变量名从 `rerankDocChars` 改为能够反映真实单位的名称。
- 增加中文、emoji、ASCII 边界和无需截断的测试。

预期效果：

- 发送给 DashScope 的 JSON 始终包含合法 UTF-8。
- 远程 rerank 输入与本地日志、测试预期一致。
- 截断单位不再通过误导性变量名隐藏。

### 3.16 P3：无意义抽象和静默修正

证据：

- `internal/agent/stream.go:43`：`newBufferedStreamPipe` 只是别名。
- `internal/agent/stream.go:84`：`OnToken` 不缓存也不转发。
- `internal/agent/stream.go:88`：最终一次性发布完整答案。
- `internal/agent/loop.go:85`：无效 timeout 配置被静默改写。
- `internal/transport/dashboard/qa.go:184`：忽略 JSON 编码错误。

示例一：`newBufferedStreamPipe` 让调用者以为 token 被暂存并可由 `Flush` 发出，但 `OnToken` 只记录时间，`Flush` 是空操作，最终由 `Publish` 一次发送完整文本。维护者很容易基于错误抽象重复实现缓冲或误判前端已经获得真实流式输出。

示例二：配置 `Timeout=60s`、`AnswerReserve=70s` 本应在配置边界报错，当前却静默改成 `AnswerReserve=30s`。线上实际行为和运维看到的配置不一致，问题只能通过阅读代码推断。

示例三：新增事件误放入 channel、function 等 JSON 不支持的值时，`jsonStr` 返回空字符串且不记录错误，前端只收到空 data，无法判断是编码失败还是协议数据为空。

修改方案：

- 删除 `newBufferedStreamPipe` 别名；根据最终产品行为只保留一种实现。
- 如果答案需要验证后一次发布，将类型命名为 validated answer publisher，并删除无意义的 `Flush`；如果需要真实流式输出，则实现实际 buffer/commit 语义。
- 在配置加载或 Agent 构造边界校验 `Timeout`、`AnswerReserve`、token 上限等关系，非法配置直接返回带字段名的错误。
- 将 `jsonStr` 改为返回 `(string, error)`，或者让 SSE writer 直接接收 typed payload 并在内部编码、记录错误。
- 删除不会发生或没有支持需求的兼容分支，不以 silent default 修复非法规范化数据。

预期效果：

- 类型、方法名和运行行为一致，维护者无需阅读实现才能判断是否真正流式。
- 配置错误在启动或保存配置时暴露，不再形成“配置值与实际值不同”。
- 新增事件的编码错误能在测试和日志中立即发现。

## 4. 安全性评估

当前未发现直接的工具越权路径：

- HTTP 层仅为管理员启用写权限。
- run 开始时固定工具权限快照。
- executor 校验工具是否存在于快照。
- executor 在调用 handler 前校验参数 schema。

相关位置：

- `internal/transport/dashboard/qa.go:299`
- `tool/contract.go:252`
- `tool/executor.go:18`

仍需补充：

- 对工具日志中的参数和结果执行字段级脱敏，而不只是长度截断。
- 验证配置、数据库和日志工具不会返回密钥、Token、Cookie 或个人信息。
- 工具 artifact 下载继续保持 user、session、run 所有权校验。

## 5. 目标架构原则

### 5.1 一个 run 只有一个权威终态

Agent 返回结构化 outcome，run service 负责持久化终态和会话轮次，RunHub 与 SSE 只做投影。

### 5.2 成功必须表示完整成功

Provider 错误、超时、续写耗尽和上下文超窗不能仅靠 warning 表达。多路召回的子路失败是例外：它按零结果继续，只在内部观测中记录。

### 5.3 召回状态只服务于内部观测

每个召回来源可在 trace 或指标中记录：

```text
source
status: completed | empty | failed | skipped
candidate_count
selected_count
error
```

这些字段不进入模型上下文，不改变 run 终态。失败子路对 Agent 等价于零命中。

### 5.4 预算只使用一种单位

检索上下文、历史上下文、工具结果、输入窗口和输出预留统一按 token 计算。

### 5.5 复杂度由真实生命周期驱动

不使用问题关键词模拟调查深度，不使用无收益 goroutine，不增加只改名不拥有行为的 wrapper。

## 6. 开发切片

### Slice 1：修正终态与截断语义

- 修复部分答案掩盖错误。
- 修复续写耗尽仍成功。
- 阻止失败或 partial 结果进入记忆提取。
- 补齐终态测试。

### Slice 2：将会话归档移出 SSE

- run completion 持久化 session turn。
- 增加 run 幂等约束。
- SSE 断线与 run 生命周期解耦。
- 处理历史空 `run_id` 数据。

### Slice 3：统一召回运行观测

- 保持子路失败按零结果继续。
- trace、日志或指标统一记录各来源状态。
- 不向模型注入检索后端错误。
- 仅在整个检索组件无法执行时返回系统错误。

### Slice 4：修复召回与证据聚合

- Runbook 按 `docID` 聚合。
- 收集多服务依赖。
- 扩大并配置初始召回池。
- 统一 rerank backend 结果语义。

### Slice 5：统一 token 预算

- 替换 rune budget。
- 统一历史、检索和工具结果预算。
- 增加中英文、代码和长工具结果测试。

### Slice 6：简化 Agent 控制流

- 删除无收益 goroutine。
- 删除问题关键词驱动的步骤限制。
- 使用无进展和重复调用检测控制循环。
- 删除假 buffered stream 和静默配置修正。

### Slice 7：实施 SSE 专项提案

- 收敛事件合同。
- 传播写入错误。
- 区分业务事件和诊断事件。
- 定义慢消费者和断线行为。

## 7. 验收门禁

### 正确性

- Provider 在输出部分文本后失败，run 不得为 `done`。
- 达到续写上限后，run 不得为完整成功。
- 多服务问题能够保留多个服务的依赖证据。
- 同名不同 `docID` 的文档不会被合并。

### 稳定性

- 客户端在 `run.started` 后断开，run 和 session turn 最终保持一致。
- 同一 `run_id` 重试归档不会产生重复 turn。
- 任一召回子路失败时回答仍可继续，模型只看到该路零结果。
- 召回子路失败可通过内部 trace、日志或指标定位，不改变 `run.finished` 状态。

### 性能

- 不增加无界读取。
- 候选池、依赖边、上下文和事件队列均有明确上限。
- 不因扩大召回池引入 N+1 外部调用。

### 安全

- 写工具仍只对授权 run 可见。
- 工具参数、结果和错误日志不泄露敏感字段。
- artifact 读取继续执行租户和会话所有权校验。

### 测试

至少运行：

```bash
go test ./internal/agent ./internal/retrieval ./internal/memory ./tool
go test ./...
go vet ./...
```

涉及并发、RunHub 或异步归档的切片还应运行：

```bash
go test -race -count=1 ./internal/agent ./internal/memory ./internal/transport/dashboard
```

## 8. 非目标

- 不为某一个问题、服务名、文档名或关键词增加召回特判。
- 不在本提案中重写 LLM Provider 客户端。
- 不将诊断 trace 变成必须可靠投递的业务事件。
- 不为了统一接口引入新的通用依赖容器或状态机。

## 9. 实施记录

实施日期：2026-08-01。

已完成：

- Provider 中断和续写耗尽不再被记录为完整成功，失败结果不进入记忆提取。
- run completion 负责持久化 session turn；`(session_id, run_id)` 幂等，SSE 只做事件投影。
- 多路召回统一记录 `completed | empty | failed`，子路失败继续按零结果处理。
- Runbook 按 `docID` 聚合；依赖边跨候选服务收集、去重并受统一预算约束。
- 候选先去重再截断，rerank 前按来源保留最低配额；远程 reranker 失败保持 recall 顺序。
- 检索上下文和工具输出复用统一 token 估算与 UTF-8 安全截断。
- 删除问题文本驱动的步骤压缩、单 goroutine WaitGroup、假 buffered stream 和 timeout 静默修正。
- 代码、Runbook、服务使用独立的 source recall limit；Runbook 初始召回从 5 扩大到 20。
- SSE payload 在 writer 内编码并传播编码、写入和 Flush 错误；首次失败后停止 heartbeat 和事件投影。
- Agent run 使用独立于 HTTP 连接取消的上下文；客户端断线不再取消后台运行。
- RunHub 使用订阅者内部队列，诊断事件拥塞时可丢弃，答案、工具状态和唯一终态可靠排队。

主要实现位置：

- `internal/agent/loop.go`
- `internal/agent/service.go`
- `internal/agent/stream.go`
- `internal/memory/session.go`
- `internal/retrieval/collection.go`
- `internal/retrieval/pipeline.go`
- `internal/retrieval/rerank.go`
- `internal/tokenestimate/token.go`
- `internal/transport/dashboard/qa.go`
- `internal/platform/dbschema/mysql.go`
- `docs/sql/migration_qa_turns_run_id.sql`

实现偏差：

- 未新增 `partial` 终态，保留部分可见答案时统一使用 `failed`，避免扩展前后端状态合同。
- source recall limit 使用按来源命名的代码常量，没有增加新的在线配置项；rerank pool 和 final top K 仍由平台设置控制。
- RunHub 本轮未增加 dropped/high-water 指标，只保留结构化 warning；指标纳入后续观测治理。
