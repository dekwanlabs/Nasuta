# Agent 平台实施差距审计

[返回设计索引](README.zh-CN.md)

> 状态：当前实现审计
> 审计日期：2026-08-07
> 审计分支：`feat/multi-agent-platform`
> 审计范围：方案 11、12、13 及其对应代码、存储和 HTTP 接口
> 说明：本审计包含当前工作树中的未提交实现，完成度为工程估算，不是交付承诺

## 1. 结论

三个方案尚未全部形成生产闭环：

| 方案 | 估算完成度 | 当前阶段 | 核心判断 |
|---|---:|---|---|
| 11 单 Agent 解耦 | 94%–97% | Runtime 与 Evaluation 合同已闭合 | QA 与 Reviewer 已共享通用 Runtime、Schema Registry、Scope 词表、Provider 模型参数合同和 Definition 版本指标，剩余缺口仅为 Agent 灰度选择、统一控制面 UI 和后置 Worker |
| 12 多 Agent 平台 | 92%–96% | 阶段 F 主体已完成 | 三种首期模式均已通过 Workflow 端到端运行，Catalog 正文持久化、历史恢复和统一 Trace/Evaluation 已接入；剩余缺口仅为 Workflow 灰度选择、统一控制面 UI 和后置 Worker |
| 13 多 Agent 评审 | 96%–98% | Policy 灰度生产闭环已落地 | 评审链路已具备动态风险 Panel、四类 Resolution、Round 预算、Policy 稳定灰度、跨 Round Report 显式复用、统一 Evaluation、完整运营入口和跨重启恢复；剩余边界仅为统一控制面 UI 和后置 Worker |

本次重新对齐逐项复核了 Runtime 合同、Agent/Workflow/Review Policy Catalog、Feature Delivery 编排、统一 Evaluation、Review Store/Service/HTTP 以及上层 CodeLoom Web 入口。除此前关闭的 G12-16、G13-02、G13-03、G13-04、G13-05、G13-12、G11-04、G11-05、G13-01、G13-06 外，G11-07、G12-13、G13-07、G13-08、G13-09、G13-10、G13-13 也已达到完成条件：Workflow Trace 可统一下钻到 Node、Agent Run、Model、Tool Snapshot、Event 和 Usage；Agent、Workflow、Review Policy 支持跨版本指标比较；Review Evaluation 已覆盖 Precision、Recall、Unique Yield、重复率、冲突率和采纳率；风险事实、规则版本、动态 Panel 及其 Hash 已固定到 Round Snapshot；Review Policy 灰度命中已固定到新 Round；Review Round 有界列表和 CodeLoom 运营页面已经接通；跨 Round 复用会显式固定来源 Report 和审计事实。当前仍有 **5 项**未完善能力，其中 P1 已清零、P2 2 项、后置能力 3 项。

当前最大的结构性缺口已经从执行编排、权限、预算、Evaluation 和产品运营入口收敛到 Agent/Workflow 版本灰度选择与统一控制面 UI。三类 Catalog 均已具备正文持久化、启动恢复、有界版本列表、默认版本、停用、版本比较、审计和通过重新设定旧版本完成回滚的后端能力；Review Policy 还已建立按稳定规则选择非默认版本并固定到新 Round 的真实灰度流量合同，但 Agent、Workflow 尚未闭合等价合同，也没有覆盖三类 Catalog 的统一运营 UI。因此，当前状态仍不能等同于“多 Agent 平台已经完成”。

QA Runtime 所有权拆分、统一 Schema Registry、Scope 与模型参数合同、Workflow 执行基础设施、三种首期生产 Workflow 模式、Human Approval 幂等续跑、节点级重试、跨重启恢复、总预算、四类 Resolution、通用 Workflow API/SSE、统一 Trace/Evaluation、动态风险 Panel、Review Policy 灰度、Review Round/Panel 运营入口、跨 Round Report 显式复用和 Review 统一脱敏均已完成。下一步只需闭合 Agent/Workflow 灰度选择和三类 Catalog 的统一控制面 UI；远程 Worker 继续后置。

### 1.1 当前仍未完善功能总表

| 优先级 | 数量 | 编号 | 当前实施重点 |
|---|---:|---|---|
| P0 | 0 | — | 当前没有 P0 缺口 |
| P1 | 0 | — | 当前没有功能闭环阻塞项 |
| P2 | 2 | G11-06、G12-12 | 在现有持久化版本控制面之上补齐 Agent/Workflow 灰度流量选择和三类 Catalog 的统一运营 UI |
| 后置 | 3 | G11-08、G12-14、G13-11 | 仅在独立扩缩容、多实例调度或异构执行环境需求成立后建设 Remote Worker 和 Claim/Lease |

这里的“未完善”不表示控制面从零开始。Agent、Workflow 和 Review Policy 已具备持久化版本、精确历史解析、活动默认版本、停用、比较、回滚和有界审计 API；Review Policy 的稳定灰度合同也已闭合。P2 仅保留 Agent/Workflow 尚未落地的灰度选择合同，以及三类 Catalog 共用的统一控制面 UI。G11-07、G12-13、G13-07、G13-08、G13-09、G13-10 和 G13-13 已闭合，不再计入缺口。

本轮复核的关键实现断点：

- Agent、Workflow 和 Review Policy 的正文及版本状态已经持久化，精确历史版本即使停用也可恢复；未指定版本只能解析活动默认版本。
- Review Policy 已提供稳定灰度规则、规则审计和 Round 选择快照；Agent/Workflow 尚无等价的灰度选择规则与 Run Snapshot，三类 Catalog 均尚无统一运营 UI。
- Workflow Trace 和 Agent/Workflow/Review Policy Evaluation 已生产装配；指标查询在存储边界使用时间窗和上限。
- Review Round 有界列表、详情 API 和 CodeLoom Review Panel 页面已经接通，可下钻 Assignment、Report、Finding、Resolution 和 Event。
- 动态 Panel 会固定风险事实、规则版本、Reviewer 集合及 Hash；跨 Round Report 复用会记录来源 Report、复用状态和审计事实。

按方案统计：

| 方案 | 未完善数量 | 本轮复核变化 | 主要剩余边界 |
|---|---:|---:|---|
| 11 单 Agent 解耦 | 2 | 关闭 G11-07 | Agent 灰度/UI、Remote Worker |
| 12 多 Agent 平台 | 2 | 关闭 G12-13 | Workflow 灰度/UI、通用 Worker |
| 13 多 Agent 评审 | 1 | 关闭 G13-07、G13-08、G13-09、G13-10、G13-13 | Reviewer Worker；统一 Catalog UI 由剩余 P2 共同覆盖 |

## 2. 审计口径

### 2.1 状态定义

- **已完成**：代码已进入生产组装路径，具备对应测试或明确的确定性行为。
- **部分完成**：领域模型或内核存在，但未进入生产路径，或缺少恢复、权限、存储、API 中的一部分。
- **未完成**：目标合同、生产入口或关键行为不存在。
- **后置能力**：方案中明确应在真实跨进程需求出现后再建设，不作为当前 P0 阻塞项。

### 2.2 判断原则

1. 类型、表结构或单元测试存在，不代表生产能力已经完成。
2. 编排必须同时闭合执行、持久化、恢复、权限、预算、事件和 API。
3. SchemaRef 只能标识合同，不能替代 JSON Schema 内容校验。
4. 进程内取消不能替代跨重启恢复或跨进程取消。
5. Feature Delivery 专用编排可以作为试点，但不能替代通用 Workflow 平台验收。

## 3. 方案 11：单 Agent 解耦

### 3.1 已完成

- 公共 `agent.Definition`、`RunRequest`、`RunResult`、`RunSnapshot` 和 `Runtime` 合同已经建立。
- Agent Definition 支持不可变版本、Content Hash、Provider、Model、Tool、Permission 和 Budget 快照。
- `DefinitionRuntime` 可解析精确 Definition 版本并执行独立 Run。
- Agent Run 已记录 Definition、Provider、Model、Tool Snapshot、Schema 和 Workflow 关联信息。
- Feature Delivery Reviewer 已通过通用 Runtime 执行，不再复制一套 Reviewer 专用模型循环。
- `app.Platform` 已统一发布 Catalog 和 Runtime，设置热加载只影响后续 Run。
- QA 已迁移到 `ManagedRuntime`，场景准备、执行和会话持久化共享同一 Run 生命周期。
- QA 只保留 Evidence、检索、会话和记忆职责，执行 LLM、Agent Loop、ToolExecutor、RunHub 和 RunStore 由 `DefinitionRuntime` 持有。
- 公共 `agent.SchemaRegistry` 已支持不可变版本、内容 Hash、批量原子发布、精确解析、Payload 校验和显式单向兼容声明。
- QA 与 Reviewer Definition 由同一 Registry 校验输入和输出；普通 QA 文本编码为 JSON string，结构化 Reviewer 输出保持 JSON object，Schema 不匹配统一失败。
- G11-04 共享 Scope 合同已注册 `knowledge.read`、`knowledge.write` 和 `feature.delivery`，并提供元数据、成员判断、子集校验和副作用判断；Agent Runtime 拒绝领域拥有的 `feature.delivery`，对应能力只由 Feature Delivery Transform 执行。
- Delegation、Workflow Actor/Scenario、Node 和 Agent Definition 的有效 Scope 逐层求交，Run 入口拒绝未注册 Scope，子 Agent 不能扩大上游权限。
- G11-05 Model Parameters 已进入 Definition 与 Run Snapshot；OpenAI 和 Anthropic 分别使用显式白名单及类型/范围校验，合法参数进入 Provider 请求，非法或跨 Provider 参数明确失败。
- G11-07 Agent Definition 版本指标已进入统一 Evaluation：成功率、证据完整率、Token、成本和 P95 延迟均按有界时间窗聚合，并支持两个精确版本的稳定对比。

### 3.2 未完善功能

| 编号 | 优先级 | 缺口 | 当前证据 | 完成条件 |
|---|---|---|---|---|
| G11-06 | P2 | Agent 灰度选择和统一控制面 UI 不完整 | Definition 正文、版本列表、活动默认版本、停用、比较、回滚和审计均已持久化并提供 API；精确历史版本可恢复，但运行入口尚未按稳定灰度规则选择非默认版本，上层也没有统一 Catalog UI | 建立可审计的灰度规则和确定性选择，将命中版本固定到 Run Snapshot，并在统一控制面展示版本、默认、停用、比较、回滚和审计 |
| G11-08 | 后置 | Remote Worker 尚未实现 | 当前只有进程内 Runtime | 出现独立扩缩容、故障域或异构环境需求后，在相同 Runtime 合同上增加远程适配器 |

### 3.3 验收标准映射

| 方案 11 验收项 | 状态 | 说明 |
|---:|---|---|
| 1. QA 不再创建执行基础设施 | 已完成 | 执行 LLM、Agent Loop、ToolExecutor、RunHub 和 RunStore 统一由 `DefinitionRuntime` 创建并持有 |
| 2. Runtime 不依赖 QA 场景语义 | 已完成 | 公共 Runtime 只接收消息、上下文、工具范围和执行策略，QA 的 Evidence Plan 与会话类型不进入公共合同 |
| 3. QA 独立拥有 Evidence、检索、会话和记忆 | 已完成 | QA 负责场景准备与会话提交，并通过 `ManagedRuntime` 执行固定请求 |
| 4. Run 固定 Definition、Provider、Model 和 Tool Snapshot | 已完成 | 已进入 Run Snapshot 和持久化 |
| 5. 应用统一通过 Platform 获取场景服务 | 已完成 | Platform 原子发布 Catalog、Runtime 和 QA 场景服务，应用入口不再自行组装执行内核 |
| 6. 配置重载不改变已开始 Run | 已完成 | 已启动 Run 固定 Runtime 和 Definition Snapshot |
| 7. Provider 和工具失败可见 | 已完成 | 未发现静默替换 Provider 的路径 |
| 8. QA API、SSE 和 Run 行为兼容 | 已完成 | 当前兼容链路仍保留 |
| 9. 存储列表和事件读取有界 | 已完成 | 相关在线读取已有分页或上限 |
| 10. 第二个专用 Agent 无需复制 QA | 已完成 | Reviewer 已通过 DefinitionRuntime 运行 |
| 11. Agent 输入输出使用版本化 Schema | 已完成 | QA 与 Reviewer 共享 Registry，输入、文本输出和结构化输出均执行真实 JSON Schema 校验 |
| 12. Scope 注册、所有权和非扩张 | 已完成 | 共享词表、执行器所有权、Run 入口校验和委托求交已进入生产路径 |
| 13. Provider 模型参数固定 | 已完成 | 合法参数按 Provider 白名单校验并进入请求及 Run Snapshot，非法参数明确失败 |
| 14. Definition 运行指标和版本对照 | 已完成 | 统一 Evaluation 已提供成功率、证据完整率、Token、成本、P95 延迟和精确版本比较 |

## 4. 方案 12：多 Agent 平台

### 4.1 已完成

- `internal/agentworkflow` 已定义 Workflow、Node、Edge、Budget、Failure Policy 和内容 Hash。
- DAG 可执行性校验、稳定拓扑顺序和并行 Wave 已实现。
- Agent、Transform、Join、Gate、Human Approval 节点类型已经建模。
- Handoff 支持内容 Hash、大小限制、Completeness 和稳定 Join。
- Orchestrator 可计算 Actor、Scenario、Workflow 和 Node 的权限交集。
- Actor/Scenario Scope 在 Run 入口校验，Agent Definition 与 Delegation 只能收缩有效 Scope；领域 Scope 由拥有该资源的 Transform Executor 执行。
- Workflow Run、Node Run、Handoff、Gate、Event 及其 Store 已建立。
- Event 和 Handoff 的列表读取在存储边界有界。
- Workflow Catalog 和 Orchestrator 显式依赖统一 `agent.SchemaRegistry`，发布时校验 Workflow/Node Schema 引用、入口、边和终点兼容性。
- Orchestrator 运行时校验 Workflow 输入、节点消费 Payload、Agent/Transform/Gate/Join 输出、Handoff 和最终 Workflow 输出；Handoff 在 Schema 解码前执行字节上限。
- `app.Platform` 已生产装配 Workflow Catalog、Store、Service、Orchestrator 和 Agent Node Executor；MySQL 或 LLM 不可用时对应能力显式降级，Runtime 热加载只影响后续 Workflow Run。
- Agent Node Adapter 会重新解析固定 Definition，为每个节点创建独立 Agent Run，并记录 Workflow Run、Node、Actor、Definition Hash、Tool Snapshot 和有效权限关联。
- Actor、Scenario、Workflow、Node 和 Agent Definition 权限已取交集，交由通用 Runtime 固定 Tool Snapshot 并执行期授权，委托不会扩大权限。
- Workflow Service 已固定 Definition Snapshot，并将 Run、Node Attempt、Handoff、Gate 和单调 Event 序列按事务闭合；取消、超时、失败和等待人工均写入明确终态。
- `NodeDefinition.Retry` 已进入不可变 Workflow Definition 和内容 Hash；零值规范化为单 Attempt，单节点最多 10 次 Attempt，退避最多 30 秒，节点 Timeout 覆盖全部 Attempt 和退避。
- Orchestrator 只重试显式实现 `Retryable() bool` 的 Agent 基础设施失败；取消、Deadline、Human Approval、Transform、Gate、Join 和 Scope 元数据标记为具备副作用的节点不会由通用机制自动重试。
- 每个 Attempt 都独立写入开始、失败或成功事实，并为 Agent Node 创建独立 Agent Run；持久化失败不会触发下一 Attempt，历史 Attempt 由现有复合主键保留。
- Workflow Service 已提供同 Run single-flight 的通用 `Resume`，从完整持久化检查点恢复并跳过成功节点；必需节点的失败、取消、超时和 Attempt 耗尽会稳定进入对应 Workflow 终态。
- 遗留 `running` Attempt 已具备明确接管规则：Human Approval 转为 `waiting_human`，仍有 Attempt 的只读 Agent 进入下一 Attempt，写权限 Agent 和非 Agent 节点不自动重试。
- Store 以启动时刻为上界、按 `started_at,id` 有界 keyset 分页扫描活动 Run；节点首次 Attempt 时间由数据库历史派生，跨重启继续约束节点总 Timeout 和剩余 Backoff。
- `app.Platform` 在执行 Runtime 可用时异步恢复启动前的活动 Run，恢复汇总和失败均可观察，且不阻塞 HTTP 启动；Catalog 已持久化历史版本，恢复时精确解析原版本并校验 Content Hash。
- G12-05 Human Approval 已持久化不可变审批事实，支持批准、拒绝、同决定幂等和不同决定冲突。
- 批准后可从完整持久化检查点续跑，跳过成功节点，保留原 Actor、权限和起始时间；同一 Wave 的多个 Human Node 可独立等待和逐个审批。
- 固定只读 Delegated Investigation 已接入生产组装：Code、Runtime Topology、Documentation 三个 Investigator 并行执行独立 Agent Run，经稳定 Evidence Join 后由显式零工具的 Synthesizer 生成结构化答案。
- `POST /api/investigations` 已作为认证后的窄场景入口接入；请求执行固定 Workflow 和活动 Definition 版本，不开放动态 Agent 或 DAG 创建能力。
- Investigation Request、Report、Bundle 和 Answer 均使用版本化 JSON Schema；Bundle 固定校验 `code`、`docs`、`runtime` 各一份报告，并与 Join 的稳定顺序一致。
- G12-11 通用 Workflow API 已接入认证路由，提供 Definition 发布与列表、Run 启动与查询、Node/Event/Handoff 有界读取、取消和 Human Approval；发布仅允许管理员，普通用户只能启动只读 Workflow，并且 Run 操作只允许 owner 或管理员。
- Workflow、Node 和 Handoff 列表使用默认 20、最大 100 的有界分页；Node/Handoff 使用稳定 Base64 JSON 游标，Event 使用单调 `after_seq`，不会在在线路径加载完整历史后再切片。
- Workflow SSE 支持 `Last-Event-ID`，按持久化回放、订阅、二次回放补窗、实时事件和序列缺口补洞的顺序发送；定时补洞覆盖进程内通知丢失，`waiting_human` 保持可续跑状态而不是关闭事件流。
- Workflow API 将输入错误、越权、资源不存在、状态冲突和能力不可用稳定映射为 400、403、404、409 和 503；审批在 Service 与事务锁内双重校验，同时保留同决定幂等和不同决定冲突。
- G12-10 Workflow 总预算已进入不可变 Definition 和 Content Hash，覆盖输入 Token、输出 Token、Tool Call、Cost 和 Retry；启用某类 Workflow 预算时，Agent Node 必须声明对应的单 Attempt 预留上限。
- Orchestrator 在 Attempt 启动前通过并发安全账户原子预留节点上限，结束后按真实 Usage 结算；超出节点或 Workflow 上限统一返回 `workflow_budget_exhausted`，`CollectAvailable` 可稳定跳过预算不足的 Optional Node。
- Agent Runtime 已执行节点级 `MaxToolCalls`，并按 Definition 固定的输入、输出 Token 单价向上取整计算微成本；启用 Cost 预算的 Workflow 在发布时要求 Agent Definition 固定有效价格。
- Node Attempt 和 Workflow Run 已持久化输入、输出、推理、总 Token、Tool Call、Cost 和 Retry Usage，节点终态与 Workflow 累计在同一事务完成。
- Resume 从持久化 Usage 继续预算核算；跨重启接管遗留 `running` Attempt 时按完整 Node Budget 保守计费，避免重启绕过总预算。
- G12-15 Sequential Artifact Pipeline 已以固定 `feature.delivery.pipeline@1` 接入生产组装，Definition 固定十个节点和九条必需边：Requirement Analysis、Technical Proposal、System Design、Implementation Plan 各自生成后等待 Human Approval，再进入 Coding 和 Validation。
- Pipeline Request、State 和 Result 使用严格版本化 JSON Schema，拒绝未知字段；每个节点消费和生产的 Handoff 均由统一 Schema Registry 校验，State 只传递有界制品摘要、实现摘要和最终验证结果。
- Pipeline Transform 复用 Feature Delivery 的制品生成、Implementation 和 Validation 领域能力，业务制品、血缘、审批规则、代码工作区和验证结果仍由 Feature Delivery 拥有，Workflow 只拥有顺序、检查点、Attempt、Handoff 和终态。
- `POST /api/features/{id}/pipeline` 已作为管理员窄入口接入，只接受实现选项并固定 Path Feature ID、当前管理员 Actor、Workflow ID、Version 和节点图；客户端不能指定或替换 Workflow、Feature 或 Node 字段。
- Artifact Review 与 Workflow Human Approval 已由协调器统一推进：先持久化领域 Review，再提交 Workflow 审批；相同事实重试可收敛，不同事实冲突，直接调用通用审批 API 也不能绕过 Artifact Review。
- 审批协调器会核对 Workflow 版本、Run Input Feature、制品 Kind/Version/Parent/Hash、生成 Node、Generation Run 和 Handoff 的三方绑定；关联查询和 Handoff 读取均在存储或服务边界有界。
- G12-16 Parallel Review Panel 已由不可变 Review Policy、Round 风险事实和固定后的 Reviewer 集合派生 Workflow Definition：Reviewer Node 并行执行，稳定 Join 汇总成功 Report，Adjudication 和 Gate 由独立 Transform Node 驱动。
- Review Request、Report、Report List 和 Gate 使用严格版本化 JSON Schema；Review Transform 与 Pipeline Transform 通过显式分发器隔离，未知或未配置的 Transform 会明确失败。
- Review HTTP Execute/Cancel 已委托 Review Coordinator；Coordinator 固定 `review.<round_id>` Run、Policy 派生的 Definition ID/Version，并保持 Workflow 与 Review Round 的启动、恢复、取消和终态收敛。
- G12-12 Workflow Definition 正文、版本状态和审计事实已经持久化；启动时恢复 Catalog，活动 Run 可按精确 ID、Version 和 Content Hash 解析已停用历史版本，未指定版本只能选择活动默认版本。
- G12-13 统一 Workflow Trace 已关联 Workflow、Node、Agent Run、Model、Tool Snapshot、Event 和 Usage；Evaluation 支持成功率、恢复率、Token、成本、P95 延迟、质量结果和精确 Workflow 版本对比。

### 4.2 未完善功能

| 编号 | 优先级 | 缺口 | 当前证据 | 完成条件 |
|---|---|---|---|---|
| G12-12 | P2 | Workflow 灰度选择和统一控制面 UI 不完整 | Definition 正文、版本列表、活动默认版本、停用、比较、回滚、审计和精确历史恢复均已闭合；尚未建立按 Actor、场景或稳定分桶选择非默认版本的灰度合同，也没有统一 Catalog UI | 建立可审计、可回放的灰度选择并将结果固定到 Workflow Run Snapshot；在统一控制面完成版本运营 |
| G12-14 | 后置 | 跨进程 Claim/Lease 和 Worker 未实现 | 通用 Workflow 当前为单进程执行和启动恢复；Feature Delivery Implementation Run 虽有专用 Claim/Lease，但没有进入通用 Workflow Node 调度合同 | 真实长任务或多实例调度出现后，再引入通用 Claim、Lease、心跳和远程取消 |

### 4.3 三种工作流模式状态

| 模式 | 状态 | 说明 |
|---|---|---|
| Delegated Investigation | 已完成 | 固定三角色只读调查通过独立 Agent Run 并行执行，稳定 Join 后由零工具 Synthesizer 综合，并由认证窄场景 API 启动 |
| Sequential Artifact Pipeline | 已完成 | 固定十节点 Workflow 以严格 Schema Handoff 驱动四类制品、四个人工审批、Coding 和 Validation，Feature Delivery 保留领域所有权 |
| Parallel Review Panel | 已完成 | 不可变 Policy 和 Round 风险快照派生并行 Workflow，Reviewer、Join、Adjudication 和 Gate 使用统一 Node、Attempt、Handoff、Budget、Event 和 Resume 合同，Review Service 只保留 Policy、Subject、Report 和 Gate 领域规则 |

### 4.4 验收标准映射

| 方案 12 验收项 | 状态 | 说明 |
|---:|---|---|
| 1. 不可变 Workflow Definition 和 DAG | 已完成 | Kernel 已具备版本与 Hash 合同 |
| 2. 每个 Agent Node 有独立 Run | 已完成 | 生产 Agent Node Executor 为每个节点创建独立 Run，并固定 Definition 与 Workflow/Node 关联 |
| 3. Agent 仅通过 Schema Handoff 通信 | 已完成 | Delegated Investigation、Sequential Artifact Pipeline 和 Parallel Review Panel 的请求、中间制品与最终结果均执行版本化 Schema 校验，节点只消费声明的 Handoff |
| 4. Orchestrator 决定流程 | 已完成 | 并行 Investigation、顺序 Artifact Pipeline 和并行 Review Panel 均由确定性 Orchestrator 调度 |
| 5. 并行上下文隔离和稳定 Join | 已完成 | Investigator 和 Reviewer 均使用独立 Run；Investigation 与 Review Join 都按 Producer Node 稳定排序，不依赖并发完成顺序 |
| 6. 子 Agent 权限不扩大 | 已完成 | Actor、Scenario、Workflow、Node、Agent Definition 取交集，并参与 Tool Snapshot 与执行期授权 |
| 7. Provider 和工具失败可见 | 已完成 | Agent Run 保留原始失败，Workflow/Node 同步进入明确失败终态，不静默替换 Provider |
| 8. 多 Agent 通过 Evaluation 证明收益 | 已完成 | 统一 Evaluation 已提供成功率、恢复率、成本、延迟、质量和单/多 Agent 版本对照；样本是否足够由时间窗和样本量显式呈现 |
| 9. Feature Delivery 保留领域所有权 | 已完成 | Pipeline 和 Review Transform 复用领域 Service，制品、血缘、Policy、Subject、Report、Gate、审批、Implementation 和 Validation 规则未上移到通用 Workflow |
| 10. 写动作不扩大副作用面 | 已完成 | Pipeline 仅允许管理员通过固定入口启动，Coding 复用 Feature Delivery 既有工作区和验证边界；普通用户不能通过通用 Workflow API 启动写 Workflow |
| 11. Workflow API 有界读取 | 已完成 | 认证 API 已提供 Run、Node、Event 和 Handoff 查询，所有在线列表均使用有界 limit 和稳定游标 |
| 12. Workflow 到 Model/Tool 完整追踪 | 已完成 | 统一 Trace 可从 Workflow 下钻 Node、Agent Run、Model、Tool Snapshot、Event 和 Usage，并标记有界查询是否截断 |
| 13. Workflow 总预算治理 | 已完成 | Token、Tool Call、Cost 和 Retry 在 Attempt 前预留、结束后结算，恢复沿用持久化 Usage，超限使用稳定错误码 |

## 5. 方案 13：研发节点多 Agent 评审

### 5.1 已完成

- Feature Delivery 八类 Subject 已形成版本绑定的 Review Round。
- 默认固定 Panel、Review Policy 和精确 Agent Definition 绑定已实现。
- Reviewer 使用独立通用 Runtime 并行运行，第一轮互相不可见。
- Report、Finding、Evidence、服务端 Fingerprint 和确定性 Gate 已实现。
- 必需 Reviewer、Validation 缺失或失败不会被当成通过。
- Severity 冲突可触发只读 Adjudicator，并持久化不可变裁决。
- Subject Hash 漂移会阻止继续执行或复用旧审批。
- 人工审批绑定 Subject Hash、Review Round 和 Gate Result。
- Round、Assignment、Report、Finding、Resolution、Adjudication、Gate、Event 和 Cancel API 已接入。
- Report 通过授权 Round 下的 Assignment 唯一读取，Store 使用 `round_id + assignment_id + LIMIT 1`，并核对正文与身份列一致。
- Event 先持久化再广播，列表读取采用稳定游标和 Limit。
- Review Live SSE 在一次授权后支持持久化分页回放、`Last-Event-ID`/`after_seq` 续传、订阅后二次回放、实时 Seq 缺口补洞和终态稳定退出。
- 每个 Review Policy 都可派生固定并行 Workflow，Reviewer、稳定 Join、Adjudication 和 Gate 已进入生产组装及统一 Transform 分发器。
- Assignment 可按通用 Retry Policy 创建后续 Attempt，失败历史不会覆盖；执行快照按 Reviewer 读取最新 Attempt。
- 已成功的同 Round Report 在重试或恢复时会直接复用，不会创建多余 Attempt 或重复执行 Reviewer。
- Review Round 固定绑定 `review.<round_id>` Workflow Run；服务恢复会跳过成功 Node 和 Report，并可从 `running`、`evaluating` 或已完成 Gate 继续收敛。
- G13-01 四类 Resolution 已全部进入领域 Service 和 HTTP：`fixed`、`waived`、`invalidated`、`superseded` 具备独立校验，替换型处置要求同类且不同的 Replacement Subject，`fixed` 还要求 Replacement 已有完成且通过的 Review Round。
- Resolution 审计读取复用 Finding 所属 Feature 的所有权授权，使用稳定 Cursor 和存储层 `LIMIT`；通用 `GET/POST .../resolutions` 与兼容 Waiver 入口同时保留。
- G13-06 Review Policy 已固定输入/输出/总 Token、Tool Call、Cost、Retry、Timeout 和 Optional Reviewer 处置，并派生 Workflow 总预算与 Reviewer Node Budget。
- Workflow Account 在 Attempt 前原子预留、结束后按真实 Usage 结算；Node/Workflow Usage 与终态事务化持久化，恢复后继续累计，超限统一使用 `workflow_budget_exhausted`。
- Reviewer 和 Adjudicator 显式启用 `RedactSensitive`；Prompt、Context、Step、Result、Report、Finding、Evidence 文本、Adjudication 和 Event Detail 在 Hash、日志或持久化前统一脱敏，脱敏后的 Context/Step/Report/Finding/Adjudication Hash 会重新计算，原始 Evidence Hash 保持不变。
- G13-08 Review Policy 可声明版本化风险规则；系统从 Artifact、Change Set 和 Validation 事实派生规范风险事实，稳定选择附加 Reviewer，并将 Risk Facts、Risk Hash、Rule Version、Reviewer 集合和 Panel Hash 固定到 Round Snapshot。
- G13-09 Review Policy 版本指标已进入统一 Evaluation，覆盖 Precision、Recall、Unique Yield、重复率、冲突率、采纳率、成本和 P95 延迟；Review TP/FP/FN 标签使用追加式有界存储。
- G13-10 Review Store/Service/HTTP 已提供按 Feature、状态和稳定 Cursor 的 Round 有界列表；CodeLoom 已接入 `评审面板` 路由、RBAC 菜单、筛选、执行/取消和 Assignment、Report、Finding、Resolution、Event 下钻。
- G13-13 跨 Round 成功 Report 复用已显式建模：只有 Subject、Policy、Reviewer Definition 和规范身份完全匹配时才能复用，并记录来源 Report、`reused` Assignment 状态、复用原因和审计事实，Gate 输入保留来源身份。
- G13-07 Review Policy 灰度已闭合：规则按 Subject Kind 版本化并追加审计，使用 SHA-256 将用户优先、Subject ID 兜底的稳定键映射到 `0..9999`，按 `0..10000` BPS 选择活动且 Subject Kind 匹配的候选 Policy；新 Round 固定规则版本、规则 Hash、稳定键 Hash、分桶、命中原因及最终 Policy，恢复和执行不重新分桶，显式 Policy 版本绕过灰度。

### 5.2 未完善功能

| 编号 | 优先级 | 缺口 | 当前证据 | 完成条件 |
|---|---|---|---|---|
| G13-11 | 后置 | 跨进程 Reviewer 调度未实现 | Reviewer 已由通用 Workflow 在单进程内调度和恢复，但没有 Node Claim、Lease、Heartbeat 或远程取消合同 | 只有在多实例和长任务需求成立后引入 Claim/Lease |

### 5.3 验收标准映射

| 方案 13 验收项 | 状态 | 说明 |
|---:|---|---|
| 1–10 | 已完成 | Subject、独立 Reviewer、Schema、Gate、Adjudicator、权限、漂移和人工绑定已形成首轮闭环 |
| 11. Finding 四类 Resolution 可追溯 | 已完成 | 四类领域规则、Replacement 约束、授权 API 和追加式有界审计读取已闭合 |
| 12. Code/Validation 使用精确材料 | 已完成 | Change Set、Validation Bundle 和 Delivery Bundle 已绑定不可变事实 |
| 13. Provider 失败可见 | 已完成 | Runtime 不静默替换 Provider |
| 14. 审计查询有界并复用授权 | 已完成 | Assignment、Report、Finding、Adjudication 和 Event 已在存储边界限制 |
| 15. 历史样本证明 Panel 收益 | 已完成 | Review Evaluation 已提供标签、Precision、Recall、Unique Yield、重复率、冲突率、采纳率、成本、延迟和版本比较，样本量不足会显式呈现 |

方案正文中的跨 Round 成功 Report 显式引用复用（G13-13）与 Prompt、Report、日志落库前脱敏（G13-12）均已完成。

## 6. 跨方案依赖

以下缺口不能孤立处理：

```text
QA Runtime 所有权拆分
  -> 统一 JSON Schema Registry
  -> Workflow Agent Node Runtime Adapter
  -> Workflow 执行与持久化闭环
  -> 首个生产 Workflow
  -> Workflow 总预算
  -> Feature Delivery Sequential Artifact Pipeline
  -> Parallel Review Panel 迁入 Workflow
  -> Review 重试与恢复闭环
  -> Review 统一脱敏边界
  -> Review API、Resolution 和预算闭环
  -> 统一 Trace、Evaluation、动态 Panel、运营入口和显式复用
  -> Agent/Workflow 灰度选择与三类 Catalog 统一控制面 UI
```

关键依赖关系：

1. G11-01、G11-02 是通用 Agent Node 的所有权基础。
2. G11-03 和 G12-08 已通过同一个 `agent.SchemaRegistry` 完成，Agent 与 Workflow 不各自维护校验器。
3. G12-02、G12-03、G12-04、G12-05、G12-06、G12-07、G12-10、G12-11、G12-15、G12-16 已完成，三种首期生产 Workflow 模式均已进入统一 Orchestrator。
4. G13-02、G13-03 已复用 G12-06 的 Retry/Attempt 语义和 G12-07 的恢复机制，不再维护第二套 Review 调度状态。
5. G13-06 已复用 G12-10 的预算预留、Usage 结算和超限错误语义，并将 Review Policy 派生为 Workflow/Node Budget。
6. G13-12 已建立统一脱敏边界；G13-13 已基于该规范表示和稳定 Hash 建立显式跨 Round 复用。
7. G11-07、G12-13 和 G13-09 共用统一 Evaluation；G13-08 的动态 Panel 与 G13-13 的显式复用均已固定可回放事实。
8. Review Policy 灰度已固定到新 Round；剩余 Agent/Workflow 灰度和统一控制面 UI 建立在稳定运行合同上，不改变已开始 Run 或 Round 的固定版本。

## 7. 建议实施顺序

当前建议按以下顺序串行推进：

1. G11-06：为 Agent Definition 增加可审计的稳定灰度选择，并接入统一 Catalog 控制面 UI。
2. G12-12：为 Workflow Definition 增加可回放的灰度选择，并接入同一控制面。
3. 在同一控制面补齐 Review Policy 版本、灰度规则和审计的运营入口；其后端灰度合同已完成，不再单独计入缺口。
4. G11-08、G12-14、G13-11：仅在真实多实例或异构执行需求出现后建设分布式 Worker。

### 阶段 A：完成单 Agent 解耦

状态：**主体与 Runtime 扩展合同已完成（2026-08-07）**。

范围：

- 完成 G11-01、G11-02；
- 建立统一 JSON Schema Registry，完成 G11-03；
- **已完成**：G11-04，共享 Scope 词表、执行器所有权、委托非扩张和副作用元数据；
- **已完成**：G11-05，Provider 模型参数白名单、显式校验和 Run Snapshot 固定；
- 保持现有 QA API、SSE、Run、Step、Usage 和取消兼容。

完成定义：

- QA 不再创建 LLM、Agent Loop、ToolExecutor 和 RunHub；
- QA 只负责编译场景输入并调用固定 Runtime；
- QA 和 Reviewer 使用同一个 Schema 校验入口；
- Scope 与模型参数均通过注册/白名单合同进入不可变 Run Snapshot；
- `GOWORK=off go build ./...`、`GOWORK=off go test ./...` 和 `GOWORK=off go vet ./...` 通过。

### 阶段 B：建立首个 Workflow 垂直链路

状态：**首个生产垂直链路已完成（2026-08-06）**。

范围：

- **已完成**：G12-01、G12-02、G12-03、G12-09；
- 复用已完成的 G12-08 Schema Registry 和 Handoff 校验合同；
- **已完成**：以固定只读 Delegated Investigation 接入首个低风险生产场景；
- **已完成**：接入 Workflow Run、Node Run、Handoff、Gate 和 Event 持久化；
- **已完成**：建立认证后的最小场景执行入口；通用查询、取消、审批和事件 API 后续已在阶段 D 完成。

完成定义：

- 一个生产请求可以从 Workflow Definition 启动；
- 每个 Agent Node 有独立 Run 和固定 Snapshot；
- Workflow 可从 Run 追踪到 Node、Agent Step、Model 和 Tool Call；
- Provider、Tool、Store 和 Node 失败均有明确状态及错误码。

### 阶段 C：补齐 Workflow 重试和恢复

状态：**已完成（2026-08-06）**。

范围：

- **已完成**：G12-05 Human Approval 审批事实、幂等命令和检查点续跑；
- **已完成**：G12-06 Retry Policy、错误分类、退避和多 Attempt 历史；
- **已完成**：G12-07 活动 Run 有界扫描、遗留 Attempt 接管、通用 Resume 和跨重启恢复；
- 短时同步任务先基于数据库事实恢复，不提前引入远程 Worker。

完成定义：

- **已完成**：Human Approval 可进入 `waiting_human`，记录不可变审批事实并在批准后幂等续跑；
- **已完成**：显式可重试的只读 Agent 失败生成新 Attempt，每个 Attempt 独立持久化，取消、超时、人工节点和写权限节点不自动重试；
- **已完成**：服务重启后按启动时刻上界发现并恢复活动 Workflow，启动后新 Run 不会被误接管；
- **已完成**：取消、超时、剩余 Backoff、Attempt 耗尽和同 Run 并发恢复具备测试。

### 阶段 D：补齐 Workflow API 和预算

状态：**已完成（2026-08-06）**。

范围：

- **已完成**：G12-11 开放受控的 Workflow 执行、查询、取消、审批、事件和制品入口；
- **已完成**：G12-10 统一 Token、Tool Call、Cost 和 Retry 预算；
- **已完成**：所有在线事件和制品读取在存储边界有界。

完成定义：

- **已完成**：调用方不依赖固定 Investigation Handler 也能管理已发布 Workflow；
- **已完成**：SSE 可从持久化序列回放并无缝切换到实时事件；
- **已完成**：节点启动前完成预算预留，结束后按 Usage 结算；
- **已完成**：预算耗尽、取消和超时均产生稳定错误码与终态。

### 阶段 E：接入 Feature Delivery 生产模式并完善 Review 闭环

状态：**已完成（2026-08-07）**。

范围：

- **已完成**：G12-15，将 Sequential Artifact Pipeline 接入 Workflow；
- **已完成**：G12-16、G13-02、G13-03，将 Parallel Review Panel 接入 Workflow，并复用 Retry、Attempt、检查点和恢复语义；
- **已完成**：G13-12，在扩大 Review 使用前建立统一落库脱敏边界；
- **已完成**：G13-04，接通 Review SSE 的持久化回放、断点续传、切换补窗、序列补洞和终态退出；
- **已完成**：G13-05，接通 Review Report 独立查询；
- **已完成**：G13-01，补齐四类 Finding Resolution、Replacement 约束、通用 API 和审计读取；
- **已完成**：G13-06，补齐 Policy 派生的 Round 级预算、Optional Reviewer 处置和 Usage 汇总。

完成定义：

- Feature Delivery 阶段交接由 Workflow 固定顺序和版本化 Handoff 驱动；
- **已完成**：Reviewer、Adjudication 和 Gate 使用 Workflow Node、Attempt、Handoff、Event 和 Resume 合同；
- **已完成**：Finding 可完整追溯到四类 Resolution；
- API/SSE 可实时观察 Round、Assignment 和 Gate；
- **已完成**：授权用户可从 Assignment 下钻完整 Report，越权或跨 Round 资源稳定隐藏；
- **已完成**：Review Prompt、Context、Step、Result、Report、Finding、Evidence 文本、Adjudication、Event 和日志只以脱敏后的规范内容进入 Hash 和持久化；
- **已完成**：预算不足时按 Policy 稳定停止或跳过 Optional Reviewer；
- **已完成**：服务重启后可跳过已完成 Assignment 并继续 Evaluating 或 Gate；
- 相同 Subject、Policy 和 Report 产生可回放的相同 Gate。

### 阶段 F：建设控制面、运营入口与 Evaluation

状态：**主体已完成，剩余 Agent/Workflow 灰度选择和统一控制面 UI（2026-08-07）**。

范围：

- **主体已完成**：Agent、Workflow 和 Review Policy 的正文持久化、版本列表、默认版本、停用、比较、回滚和审计；
- **已完成**：Review Round 有界列表和 CodeLoom Panel 运营入口；
- **已完成**：统一 Workflow Trace 和 Agent/Workflow/Review Policy Evaluation；
- **已完成**：版本化风险事实、动态 Panel 选择和 Round Snapshot 固定；
- **已完成**：Review Policy 稳定灰度、规则审计和新 Round 选择快照；
- **已完成**：成功 Report 跨 Round 显式复用；
- **剩余**：Agent/Workflow Catalog 的真实灰度流量选择和三类 Catalog 的统一控制面 UI。

完成定义：

- **部分完成**：版本发布、默认选择、停用、比较、回滚和审计已闭合；Review Policy 灰度选择及命中快照已完成，Agent/Workflow 灰度尚未建立；
- **已完成**：跨 Round 复用显式固定来源 Report、Subject、Policy、Reviewer Definition 和内容 Hash；
- **已完成**：Round 有界列表、详情和运营 UI 明确区分 Agent Gate、Adjudication、Human Decision 和 Resolution；
- **已完成**：Panel 收益可通过 Precision、Recall、Unique Yield、重复率、冲突率、采纳率、成本和延迟衡量；
- **已完成**：Evaluation 明确呈现样本量和版本差异，默认版本调整仍由可审计控制面命令完成。

## 8. 当前不建议提前建设

以下能力不是当前平台闭环的前置条件：

- 独立 Agent 微服务；
- 外部工作流引擎；
- 跨进程 Remote Runtime；
- 通用消息队列调度；
- 多实例 Claim/Lease；
- 模型自主创建 Agent 或修改 DAG；
- 无结构的 Agent 间长对话共享。

只有出现独立扩缩容、跨进程恢复、异构执行环境或明确吞吐瓶颈后，才应增加对应部署机制。当前优先级是完成代码所有权、合同、生产装配、持久化和恢复，而不是扩大运行拓扑。

## 9. 下一阶段交付清单

建议下一阶段只承诺以下尚未完成结果，并按顺序交付：

1. Agent Definition 灰度选择：稳定分桶、规则版本、命中原因和 Run Snapshot 固定均可审计。
2. Workflow Definition 灰度选择：按 Actor/场景的非默认版本选择可回放，恢复仍精确解析原版本。
3. 统一 Catalog 控制面 UI：集中展示三类版本、默认、停用、比较、回滚、灰度和审计，其中 Review Policy 直接复用已完成的灰度后端 API。
4. 三类分布式 Worker 继续按真实部署需求后置，不纳入当前 P2。

方案 11 的 Runtime 与 Definition Evaluation 已完成；方案 12 已覆盖三种可恢复生产 Workflow、持久化 Catalog 和统一 Trace/Evaluation；方案 13 已闭合动态 Panel、Policy 灰度、运营入口、质量评估和显式复用。完成上述 2 个 P2 控制面能力后，平台才具备完整的版本运营闭环；三类分布式 Worker 继续按真实部署需求后置。
