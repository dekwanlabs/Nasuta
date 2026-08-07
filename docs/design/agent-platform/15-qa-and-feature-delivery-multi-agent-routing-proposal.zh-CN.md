# QA 与研发任务多 Agent 路由方案

[返回设计索引](README.zh-CN.md)

> 状态：目标设计，待实施
> 更新日期：2026-08-07
> 适用范围：Nasuta QA、Feature Delivery、通用 Agent Workflow
> 前置方案：[Nasuta 多 Agent 平台方案](12-multi-agent-platform-proposal.zh-CN.md)
> 关联方案：[研发节点多 Agent 评审方案](13-development-multi-agent-review-proposal.zh-CN.md)

## 1. 结论

多 Agent 是 QA 和研发任务内部的执行策略，不是要求调用方选择的独立业务入口。

目标产品入口保持稳定：

- 用户问答统一进入 `POST /api/qa/ask`；
- 研发任务统一进入 Feature Delivery；
- 场景服务根据问题复杂度、任务风险和可并行边界，选择单 Agent 或固定多 Agent Workflow；
- 前端只消费父 Run 的事件和结果，不直接创建 Agent、选择 DAG 或切换到 Investigation API。

当前 `POST /api/investigations` 只显式启动固定 Delegated Investigation，普通 QA 不会调用它。因此它证明了多 Agent Workflow 能力已经存在，但没有完成 QA 业务接入。目标状态下，该接口最多保留为管理员调试和 Workflow 验证入口，不承担产品分流职责。

```text
QA / Feature Delivery API
  -> 场景分析与执行路由
  -> Single Agent 或固定 Multi-Agent Workflow
  -> 统一 Parent Run / Child Run
  -> 统一 SSE、Session、Artifact、Gate、Trace 和 Evaluation
```

## 2. 目标与非目标

### 2.1 目标

1. QA 自动判断问题复杂度，并在单 Agent 与 Delegated Investigation 之间选择。
2. 多 Agent QA 与普通 QA 共享入口、会话、引用、最终答案和事件流。
3. Feature Delivery 在需求、方案、设计、计划、编码和验证阶段按风险启用多 Agent。
4. 复用现有 `internal/agentworkflow`、Agent Catalog、Reviewer Panel 和 Feature Pipeline。
5. 路由决策可解释、可审计、可评估，并固定到 Run Snapshot。
6. Agent 数量、权限、工具和预算由服务端 Policy 决定，不能由模型自由扩张。

### 2.2 非目标

- 不建设自由群聊式 Agent 网络；
- 不允许模型创建 Agent、修改 DAG 或扩大权限；
- 不要求所有问题和研发任务都使用多 Agent；
- 不用多 Agent 数量替代证据质量、测试、确定性 Gate 或人工批准；
- 不把 Reviewer 多数票当作批准依据；
- 不在第一阶段实现跨进程 Worker 或任意动态任务图。

## 3. 现状与差距

### 3.1 QA

当前 QA 已在一次结构化分析中完成：

- 证据源选择；
- Tool 路由；
- Query Terms 提取；
- 相对时间解析；
- 会话依赖分析。

但分析结果不包含执行策略。路由完成后，QA 始终解析并执行一个 `qa.answerer` Definition。

主要代码边界：

- `internal/retrieval/route.go`：问题分析和证据路由；
- `internal/agent/service.go`：QA Run 创建、预检索和单 Agent 提交；
- `internal/transport/dashboard/qa.go`：QA SSE 投影；
- `internal/agentworkflow/investigation.go`：固定 Delegated Investigation DAG；
- `internal/investigation/service.go`：独立 Investigation 场景入口。

### 3.2 Feature Delivery

当前 Feature Pipeline 固定为串行十节点：

```text
Requirement Analysis
  -> Human Approval
  -> Technical Proposal
  -> Human Approval
  -> System Design
  -> Human Approval
  -> Implementation Plan
  -> Human Approval
  -> Coding
  -> Validation
```

Artifact 生成仍是单次 Generator 调用。Parallel Review Panel 已经实现，但主要通过独立 Review Round 创建和执行，没有成为每个 Pipeline 阶段的内嵌质量 Gate。Coding 也没有按可并行实施任务拆分多个 Agent。

## 4. 统一执行路由

### 4.1 路由输出

扩展现有问题分析结果，在同一次结构化 LLM 调用中返回执行建议：

```json
{
  "route": {
    "sources": ["internal"],
    "confidence": 0.94
  },
  "tools": {
    "tool_ids": ["search_code", "trace_deps"]
  },
  "execution": {
    "strategy": "multi_agent",
    "complexity": 0.82,
    "confidence": 0.91,
    "reasons": [
      "requires_cross_service_analysis",
      "requires_independent_evidence_validation"
    ]
  }
}
```

第一阶段只接受两种策略：

```text
single_agent
multi_agent
```

模型只提出建议。服务端结合场景 Policy、权限、能力可用性和置信度计算最终策略。模型不能返回 Agent ID、Workflow ID、Provider、工具权限或并行度。

### 4.2 路由依据

使用通用结构特征判断复杂度，不按具体问题关键词硬编码：

| 特征 | 单 Agent 倾向 | 多 Agent 倾向 |
|---|---|---|
| 子问题数量 | 一个聚焦问题 | 至少两个可独立调查的子问题 |
| 证据维度 | 单一来源或单次查找 | 代码、拓扑、文档等需要交叉验证 |
| 影响范围 | 单 Symbol、单 API、单服务 | 跨模块、跨服务、跨阶段 |
| 不确定性 | 目标和答案合同明确 | 存在冲突、歧义或多个根因候选 |
| 风险 | 解释、摘要、普通查询 | 架构、安全、迁移、可靠性或交付风险 |
| 可并行性 | 子任务强依赖或修改范围重叠 | 子任务可隔离执行并稳定 Join |

多 Agent 必须同时满足：

1. 场景 Policy 允许多 Agent；
2. 至少存在两个有独立产出的并行职责；
3. 对应 Workflow 和 Agent Definition 可用；
4. 调用者权限覆盖 Workflow 最小权限；
5. 预算足以完成必需节点；
6. 路由置信度达到配置阈值。

任一条件不满足时使用单 Agent，并记录最终决策原因。

### 4.3 场景 Policy

路由器不拥有业务策略。每个场景声明允许的执行方式：

```go
type ExecutionPolicy struct {
    AllowMultiAgent       bool
    MultiAgentWorkflow    WorkflowRef
    MinComplexity         float64
    MinConfidence         float64
    MaxParallelism        int
    FallbackToSingleAgent bool
}
```

该结构表示真实的两种执行行为，不是持久化状态机。发布后应进入不可变场景配置快照，并记录到父 Run。

## 5. QA 多 Agent 链路

### 5.1 目标流程

```text
POST /api/qa/ask
  -> 创建 QA Parent Run
  -> Query Analysis
  -> 计算最终执行策略
  -> single_agent
       -> qa.answerer
       -> Tool Loop
  -> multi_agent
       -> investigator.code
       -> investigator.runtime
       -> investigator.docs
       -> evidence.join
       -> synthesizer
  -> 统一 QA Outcome
  -> 持久化 Session Turn
  -> run.finished
```

QA 调用方不感知 `/api/investigations`，也不提交执行模式。显式证据源选择只约束证据需求，不直接强制多 Agent。

### 5.2 多 Agent 适用范围

第一阶段复用固定 Delegated Investigation，只处理只读的内部技术调查：

- 跨服务调用链和依赖分析；
- 代码、服务拓扑和文档一致性；
- 修改影响面和关联入口分析；
- 实现位置、职责边界和文档覆盖调查；
- 需要独立证据视角后再综合的问题。

以下请求保持单 Agent：

- 普通对话、改写、翻译和用户提供内容的总结；
- 单一 Symbol、API 或配置项查询；
- 主要依赖 Web、Memory 或实时日志的问题；
- 写操作和需要审批的动作；
- 子任务不可独立并行的问题。

后续若要支持实时故障调查，应新增拥有实时证据能力的固定 Workflow，而不是让现有 Runtime Topology Investigator 虚构实时状态。

### 5.3 Parent/Child Run

QA Parent Run 负责业务生命周期，Workflow Run 和 Agent Run 作为子 Run：

```text
qa_run_id
  -> workflow_run_id
       -> code_agent_run_id
       -> runtime_agent_run_id
       -> docs_agent_run_id
       -> synthesizer_run_id
```

所有子 Run 必须记录：

- Parent QA Run ID；
- Workflow Run ID 和 Node ID；
- Agent Definition ID、Version 和 Hash；
- Tool Snapshot；
- Actor、权限和预算；
- Usage、错误和最终 Handoff。

### 5.4 QA 输出收敛

Synthesizer 输出直接转换为标准 QA Outcome：

```json
{
  "answer": "最终结论",
  "citations": [],
  "limitations": []
}
```

不应默认再调用一次 `qa.answerer` 重写结论，否则会增加成本并可能破坏引用。QA 场景层只做确定性格式映射、Session 持久化和事件投影。

### 5.5 SSE 事件

现有 QA SSE 增加以下事件：

```text
execution.routed
workflow.started
agent.started
agent.completed
evidence.joined
execution.degraded
run.finished
```

事件至少包含：

```json
{
  "run_id": "qa parent run",
  "workflow_run_id": "optional child workflow",
  "node_id": "optional node",
  "strategy": "single_agent or multi_agent",
  "status": "running or completed or failed"
}
```

前端仍只订阅 QA SSE，不额外订阅 Workflow SSE。

### 5.6 QA 失败策略

- 路由分析失败：按现有保守策略进入单 Agent，并记录 `route_degraded`；
- Workflow 不可用：发送 `execution.degraded`，明确回退单 Agent；
- 配置的 LLM Provider 失败：不替换 Provider；
- 可选调查节点失败：Join 生成明确 Gap，Synthesizer 写入 `limitations`；
- 必需节点失败且没有足够证据：QA Run 明确失败，不返回伪完整答案；
- 客户端断开不应取消已经进入持久化生命周期的父 Run。

为支持部分证据收敛，Delegated Investigation 应从简单 `FailFast` 调整为可表达必需/可选调查职责的 `CollectAvailable` Policy，并通过结构化 Gap 保留缺失原因。

## 6. 研发任务多 Agent 链路

### 6.1 根流程

Feature Delivery 仍拥有业务制品、审批和 Gate。通用 Workflow 只负责调度：

```text
Feature Request
  -> Requirement Analysis Stage
  -> Technical Proposal Stage
  -> System Design Stage
  -> Implementation Plan Stage
  -> Coding Stage
  -> Validation Stage
  -> Delivery Gate
```

每个文档阶段统一为：

```text
Generate Artifact
  -> Reviewer Panel
  -> Adjudication
  -> Deterministic Gate
  -> Human Approval
```

Reviewer Panel 不再是旁路操作，而是进入下游阶段前的必经质量检查。人工批准必须绑定当前 Artifact Hash、Review Round 和 Gate Result。

### 6.2 阶段 Reviewer

| 阶段 | 默认 Reviewer | 主要职责 |
|---|---|---|
| 需求分析 | Architecture、Reliability | 范围、领域规则、验收可测试性、阻塞问题 |
| 技术方案 | Architecture、Security、Reliability | 方案比较、兼容性、安全、可靠性、可逆性 |
| 系统设计 | Architecture、Security、Reliability | 边界、合同、数据所有权、并发、恢复、观测 |
| 实施计划 | Architecture、Reliability | 修改范围、依赖顺序、测试、迁移、回滚 |
| Change Set | Architecture、Security、Reliability | 正确性、越权修改、回归、安全和复杂度 |
| Validation | Architecture、Reliability | 覆盖度、失败分析、行为合同和残余风险 |
| Delivery | Architecture、Security、Reliability | 发布准备、监控、回滚和未决风险 |

默认 Reviewer 继续由版本化 Review Policy 决定。风险事实可以选择已发布的更严格 Policy，但模型不能临时创建 Reviewer。

### 6.3 研发复杂度路由

Feature Delivery 在两个层级做决策：

1. **阶段是否启用多 Reviewer**：由 Subject Kind 和 Review Policy 决定，生产阶段默认启用；
2. **Coding 是否并行**：由 Implementation Plan 的结构化任务和文件所有权决定。

Coding 只有同时满足以下条件才能多 Agent 并行：

- 实施计划已经批准；
- 任务依赖形成有效无环图；
- 每个任务声明清晰的模块和文件所有权；
- 并行任务的写集合不重叠；
- 公共合同修改先于依赖任务完成；
- 每个 Coding Agent 使用独立 Worktree；
- 集成前有确定性冲突和基线校验。

无法证明写边界独立时保持单 Coding Agent。不能为了体现多 Agent 而并行修改同一文件集合。

### 6.4 Coding 多 Agent 目标流程

```text
Approved Implementation Plan
  -> Task Planner 生成有界任务 DAG
  -> Ownership Validator 校验写集合和依赖
  -> 多个 Coding Agent 在独立 Worktree 执行
  -> Integration Transform 按依赖顺序收敛
  -> Change Set Review Panel
  -> Independent Validation
  -> Validation Review Panel
  -> Delivery Gate
```

第一阶段不实现任意动态 DAG。Task Planner 只能从批准的 Implementation Plan 生成受 Schema 约束的任务：

```json
{
  "tasks": [
    {
      "id": "task-1",
      "depends_on": [],
      "owned_paths": ["internal/example"],
      "acceptance_checks": ["go test ./internal/example/..."]
    }
  ]
}
```

服务端必须验证：

- Task ID 唯一；
- 依赖无环且引用存在；
- `owned_paths` 不重叠；
- 路径位于允许的 Repository；
- 验证命令来自批准计划或受控命令策略；
- 任务数量、并行度和预算有界。

### 6.5 研发失败策略

- 必需 Reviewer 失败：Round 为 `incomplete` 或 `failed`，不能进入人工批准；
- Optional Reviewer 失败：保留 Coverage Gap，由 Gate 决定是否需要人工；
- Gate 为 `revise`：创建新 Artifact 或 Change Set 版本，不修改旧制品；
- Coding Agent 失败：依赖任务不启动，已完成 Worktree 保留供审计，不自动集成；
- 集成冲突：进入明确失败或人工处置，不让模型直接覆盖冲突；
- Validation 失败或未配置：禁止 Delivery Gate 通过；
- Provider 失败：不切换其他 Provider。

## 7. 权限与安全

### 7.1 QA

Delegated Investigation 固定为 `knowledge.read`。子 Agent 权限是 Actor、QA 场景、Workflow、Node 和 Agent Definition 权限的交集。

### 7.2 Feature Delivery

- Reviewer 和 Adjudicator 只读；
- Coding Agent 只能写自己的隔离 Worktree 和批准路径；
- Integration 只能消费已完成任务的不可变 Change Artifact；
- Gate 不执行写操作；
- 人工审批不能由 Agent 代替；
- 发布、合并和部署继续使用现有审批与 Write Action 边界。

## 8. 可观测性与评估

### 8.1 Trace

父 Run Trace 至少包含：

- 路由输入特征和输出；
- 建议策略与最终策略；
- 置信度、阈值和覆盖的 Policy 版本；
- 单/多 Agent Run 关系；
- 每个节点的耗时、Token、工具调用、成本和错误；
- Join、Adjudication 和 Gate 结果；
- 降级原因和缺失证据。

### 8.2 关键指标

QA：

- 单/多 Agent 路由比例；
- 路由人工标注准确率；
- 答案有证据率和限制披露率；
- 多 Agent 相对单 Agent 的质量增益；
- P50/P95 延迟和成本；
- Workflow 部分失败率和降级率。

Feature Delivery：

- 每阶段 Finding 数量和严重度；
- 人工采纳率、误报率和漏检率；
- Gate 的 `pass/revise/human_required/incomplete` 分布；
- 返工轮次和跨阶段逃逸缺陷；
- 并行 Coding 的关键路径缩短比例；
- 合并冲突、验证失败和回滚率。

多 Agent 只有在质量增益能够覆盖成本和延迟时才应保持启用。Evaluation 必须按精确 Route Policy、Workflow、Agent Definition 和模型版本对比。

## 9. API 与兼容策略

### 9.1 QA API

`POST /api/qa/ask` 请求保持兼容，不要求新增 `multi_agent` 参数。服务端自动路由。

可在响应事件中增加：

```json
{
  "event": "execution.routed",
  "strategy": "multi_agent",
  "complexity": 0.82,
  "confidence": 0.91
}
```

### 9.2 Investigation API

`POST /api/investigations` 的迁移顺序：

1. QA 内部接入同一 Workflow；
2. Dashboard 不再新增或依赖 Investigation 产品入口；
3. 将接口标记为管理员调试用途；
4. 确认没有生产调用方后，再决定保留、收缩到内部路由或移除。

### 9.3 Feature Delivery API

现有 Feature、Pipeline、Review Round 和 SSE API 保持兼容。Pipeline 内部创建和执行 Review Round，前端不再要求管理员逐轮手工启动必需评审，但仍可进入 Review Round 页面查看详情、取消和处理人工决策。

## 10. 实施计划

### 阶段一：QA 自动路由

1. 为 `retrieval.AnalysisResult` 增加执行建议。
2. 扩展路由 Prompt、JSON 校验、Trace 和 Evaluation。
3. 在 QA 场景注入受限的 Investigation Runner。
4. 建立 QA Parent Run 与 Workflow Child Run 关联。
5. 将 Synthesizer 结果映射为 QA Outcome。
6. 将 Workflow/Agent 事件投影到 QA SSE。
7. 保留显式可观测的单 Agent 降级。

完成标准：

- 简单问题只创建一个 QA Agent Run；
- 复杂内部技术问题创建一个 Workflow Run 和四个 Agent Run；
- 两种路径写入同一 QA Session；
- 前端始终只调用 `/api/qa/ask`；
- 路由、降级、成本和引用可查询。

### 阶段二：Reviewer Panel 内嵌 Pipeline

1. 在四个 Artifact Generation 节点后接入 Review Workflow。
2. Gate 通过或明确人工处置后才进入 Human Approval。
3. 审批继续绑定 Subject Hash、Round 和 Gate。
4. Pipeline SSE 投影 Review Round 和 Reviewer 节点进度。
5. Change Set、Validation 和 Delivery 保持必需 Review Gate。

完成标准：

- Pipeline 不能绕过必需 Reviewer；
- Reviewer 失败不会被当作通过；
- Artifact 变化使旧 Round 和旧审批失效；
- 人工可以从父 Pipeline Run 下钻 Reviewer 报告和证据。

### 阶段三：Coding 多 Agent

1. 定义实施任务 Schema 和 Task Planner。
2. 增加 DAG、路径所有权、预算和命令策略校验。
3. 为独立任务创建隔离 Coding Run 和 Worktree。
4. 建立确定性 Integration 和冲突处理。
5. 接入 Change Set Review、Validation 和 Delivery Gate。

完成标准：

- 只有写集合不重叠的任务并行；
- 每个修改可追踪到批准计划和 Coding Run；
- 集成失败不会产生可交付 Change Set；
- 独立验证失败时 Delivery 明确阻断。

### 阶段四：评估和灰度

1. 建立 QA 单/多 Agent 对照集。
2. 建立不同研发阶段的 Finding 和 Gate 标注集。
3. 按稳定分桶灰度 Route Policy 和 Workflow 版本。
4. 设定质量、成本、延迟和失败率回退阈值。
5. 根据 Evaluation 调整复杂度阈值和 Review Policy。

## 11. 测试策略

### 11.1 单元测试

- 执行路由 JSON 校验；
- 模型建议与服务端 Policy 合并；
- 低置信度和能力不可用降级；
- Parent/Child Run 关联；
- Synthesizer 到 QA Outcome 的确定性映射；
- Coding Task DAG 和路径所有权校验。

### 11.2 集成测试

- QA 简单问题只执行单 Agent；
- QA 复杂问题执行三个 Investigator 和 Synthesizer；
- 一个调查节点失败时限制信息完整；
- Pipeline 每阶段自动创建 Review Round；
- Gate 未通过时下游节点不启动；
- 多 Coding Agent 只在不重叠 Worktree 中执行；
- Provider、Workflow Store 和 SSE 中断时终态一致。

### 11.3 回归测试

- 现有 `/api/qa/ask` 请求和 SSE 事件继续兼容；
- 显式 Evidence Plan 不被执行路由误解为强制多 Agent；
- QA Session 历史和引用不丢失；
- Feature Delivery 旧 Artifact、Review 和 Approval 审计仍可读取；
- 设置热加载只影响后续 Run，不修改进行中 Snapshot。

## 12. 验收标准

1. 产品侧只有 QA 和 Feature Delivery 两类多 Agent 业务入口。
2. QA 调用方不需要知道 Investigation Workflow。
3. 多 Agent 决策由一次结构化分析提出并由服务端 Policy 校验。
4. 简单问题不承担多 Agent 固定成本。
5. 复杂 QA 的每个调查方向使用独立 Agent Run 和类型化 Handoff。
6. QA 单/多 Agent 输出进入同一 SSE、Session 和结果合同。
7. Feature Pipeline 的必需 Reviewer 成为阶段 Gate，而不是旁路操作。
8. Coding 仅在任务依赖和写边界可证明独立时并行。
9. 所有降级、缺失证据、Provider 失败和 Gate 决策可观察。
10. Evaluation 能按精确版本比较单 Agent 与多 Agent 的质量、成本和延迟。

## 13. 代码改造边界

第一阶段预计涉及：

| 包 | 改造内容 |
|---|---|
| `internal/retrieval` | 执行策略分析合同与绑定校验 |
| `internal/agent` | QA 单/多 Agent 分流、结果收敛、Session 持久化 |
| `internal/investigation` | 从独立场景服务抽出可供 QA 调用的受限 Runner |
| `internal/agentworkflow` | Parent Run 关联、部分调查失败收敛 |
| `internal/transport/dashboard` | QA SSE 投影多 Agent 事件 |
| `internal/featurepipeline` | Artifact 阶段嵌入 Review Workflow |
| `internal/featurereviewworkflow` | 保持 Reviewer、Join、Adjudication 和 Gate 实现 |
| `app` | 场景 Policy、Runner 和 Workflow Runtime 组合 |
| `web` | 展示执行策略和子 Agent 进度，不增加入口选择 |

依赖方向保持不变：通用 Workflow 机制由 Nasuta 拥有，QA 和 Feature Delivery 分别拥有场景路由、结果合同和业务 Gate。
