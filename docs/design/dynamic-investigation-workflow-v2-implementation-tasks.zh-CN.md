# 动态规划多 Agent Workflow v2 实施任务清单

状态：P0 核心机制已完成；P1 大部分完成，P1-06 已接入 token-aware fencing 但仍需生产迁移和重启验收；P2 仍为收尾阶段
关联设计：[dynamic-investigation-workflow-v2.zh-CN.md](dynamic-investigation-workflow-v2.zh-CN.md)
日期：2026-08-22

## 1. 文档目的

本文把 v2 设计拆成可以进入迭代计划的 P0、P1、P2 任务。

这不是对旧 workflow 的修补清单。v2 使用新的 Run、Contract、Plan、Task、
EvidenceLedger、ClaimLedger 和 DeliveryResult。旧的 `forceConclusion`、多层
recovery 和旧 workflow 不作为 v2 的运行时依赖。

## 1.1 当前实现对齐状态（2026-08-22）

以下为与当前代码逐项核对后的实际完成度。已完成表示有实现和单测；部分完成表示
核心路径存在，但仍有验收项缺失；未完成表示当前代码中还没有对应能力。

### 完成度对照

| 任务 | 完成度 | 缺口 |
| --- | --- | --- |
| P0-01 领域契约与 Schema | 已完成 | 无 |
| P0-02 Run 生命周期与持久化 | 部分完成 | 已有 event log、Replay、TaskAttempt、续租和 token-aware 条件写；Execute/Resume 均记录结构化 task/delivery event；旧 MySQL 实例仍需执行 fencing migration，Memory/非 token store 仍是写前校验 |
| P0-03 BudgetLedger | 已完成 | 无 |
| P0-04 Evidence/Claim Ledger | 已完成 | 多来源合并和冲突识别较浅，但账本约束已落地 |
| P0-05 DeliveryGate / Renderer | 已完成 | 无 |
| P0-06 Template Catalog | 已完成 | 无 |
| P0-07 PlanCompiler / DAG | 已完成 | 权限交集和 DAG 环校验已落地 |
| P0-08 Scheduler | 已完成 | Agent/Tool 并发上限未拆分，共用一个 MaxParallelism |
| P0-09 Investigator/Verifier/Composer | 已完成 | Composer 无调查工具权限，verified bundle 已接入 |
| P0-10 最小端到端回归 | 已完成 | 无 |
| P1-01 Gap 驱动 Replan | 基本完成 | 已完成 discovery 类型匹配、确定性收益评分、预算约束下的候选选择和依赖闭合；仍未接入历史数据驱动的失败概率/证据重复率校准 |
| P1-02 更多模板 | 已完成 | 配置/API/文档/运行时方向模板已注册 |
| P1-03 多执行器并行 | 已完成 | Scheduler 已分离 Agent/Tool 并发上限，平台配置值暂沿用全局 MaxParallelism |
| P1-04 完整证据验证与冲突 | 部分完成 | 已完成 claim 跨任务 provenance union、冲突证据自动降级、confidence/引用 canonicalization、MissingFacets/MissingSources coverage 和 conflict refs 保留；仍缺少来源权威性/优先级合并、事实级审计和更完整的冲突公开展示 |
| P1-05 REST/MCP/SSE 统一交付 | 部分完成 | REST、SSE、MCP 结果读取已统一；MCP 没有调查发起入口 |
| P1-06 Run 恢复/超时/取消/幂等 | 部分完成 | 已接入续租、单调 fencing token、MySQL 条件写、Resume metrics/budget 持久化和 durable AwaitTerminal；仍需真实 MySQL migration、重启并发 Resume、取消和接口回放验收 |
| P1-07 预算 profile 和运行指标 | 已完成 | run 内维度齐备，并新增 AggregateRunMetrics 计算 p50/p95、预算不足率和 Composer fallback 率 |
| P1-08 生产级故障注入 | 部分完成 | 新增 tool unavailable、reasoning truncation、provider timeout 专项；重启并发恢复仍未成体系 |
| P2-01 成本模型优化 | 部分完成 | Replan 已使用模板成本做确定性排序扣分，且已有 CalibrateTemplateCosts 按 p95 使用量校准模板成本；仍未把历史成本、失败率和重复证据率接入运行时 catalog 更新 |
| P2-02 并发/上下文优化 | 部分完成 | 新增 PruneUnreferencedEvidence 减少 Composer 上下文；provider/model 路由仍缺 |
| P2-03 模板生命周期治理 | 部分完成 | 模板废弃、List/Resolve 过滤和 append-only audit 已实现；版本发布、Schema 兼容、权限/成本审计仍缺 |
| P2-04 历史迁移和旧链路删除 | 部分完成 | escalation/delegated.investigation 已删；DisableLegacyAnswerRecovery 已接入；publicOutputText 由 AnswerRenderer 替代；outcomeFromPublicResult 已删除，普通 QA 直接读取 durable outcome |
| P2-05 评估集和持续回归 | 部分完成 | 新增 EvaluateDelivery 和 JSON 持久化 EvaluationSuite；尚未接入 CI/部署回放 |

### 已完成项

- v2 `InvestigationRun`、Contract、Budget、Plan、Scheduler、Evidence/Claim Ledger、DeliveryGate 和确定性 Renderer；
- v2 动态 replan、任务执行器注册表，以及 Investigator、Verifier、Composer 三类执行边界；
- QA 多 Agent 调查入口统一创建 `workflow_*` v2 Run，Run ID 可跨进程恢复；
- Composer 只接收 verified bundle，不持有调查工具权限；
- Evidence/Claim Ledger 只向 Composer 暴露经过准入的证据：同一 Claim 的独立来源会合并 provenance，显式或 identity 冲突会降级为 conflicting，coverage gap 会携带缺失 facet/source；
- REST `GET /api/qa/runs/{id}` 与 QA SSE `run.finished` 从同一个 v2 `DeliveryResult` 投影；
- 旧 escalation API、存储绑定、schema、shadow event 和配置项已删除；
- 旧 `delegated.investigation` 固定 DAG 实现及其测试已删除。

### 当前明确收尾项

- MCP 当前没有独立的 QA 调查结果读取端点，调查工具本身已经通过 v2 执行器完成；
- 普通单 Agent QA 仍保留其非调查路径的答案生成 recovery，不能误删后破坏普通问答；
- 需要在真实 MySQL/部署环境完成一次重启恢复、取消和接口回放验证；本地真实 workspace 的 callchain/MCP fixture 测试仍受索引数据缺失影响。

实现时可以暂时在新的包边界中开发，最后切换入口并删除旧链路；这属于代码迁移
顺序，不代表保留旧 workflow compatibility adapter。

## 2. 优先级定义

### P0：必须先完成

P0 解决两个生产阻断问题：

```text
1. workflow 没有统一的状态、预算和证据模型；
2. Composer 或模型失败时可能返回空答案。
```

P0 完成后，必须有一个最小端到端 v2 调查流程，即使只有一轮、少量工具和一个
Investigator，也不能返回空的 `succeeded` 或 `partial`。

### P1：形成完整动态多 Agent 能力

P1 增加动态规划、重规划、任务组合、更多证据来源、并行调度、权限治理和对外
接口切换，使 v2 可以替代旧入口。

### P2：优化和清理

P2 处理性能、成本校准、评估、历史数据迁移和旧链路删除。P2 不改变 P0/P1 的核心
契约，只优化实现和运营能力。

## 3. 总体依赖关系

```text
P0-01 领域契约与 Schema
  ├─> P0-02 Run 生命周期与持久化
  ├─> P0-03 BudgetLedger
  ├─> P0-04 EvidenceLedger / ClaimLedger
  ├─> P0-05 DeliveryGate / DeterministicRenderer
  └─> P0-06 TaskTemplateCatalog / Executor

P0-03 + P0-06
  └─> P0-07 PlanCompiler / DAG 校验
        └─> P0-08 Scheduler / 单轮执行
              └─> P0-09 Investigator / Verifier / Composer 接入
                    └─> P0-10 最小端到端回归

P0 全部通过
  └─> P1-01 动态 Replan
        ├─> P1-02 通用任务组合与更多模板
        ├─> P1-03 多执行器并行调度
        ├─> P1-04 完整证据验证与冲突处理
        ├─> P1-05 REST / MCP / SSE 切换
        └─> P1-06 生产观测与恢复

P1 稳定
  └─> P2 优化、迁移和删除旧链路
```

## 4. P0 任务

### P0-01：冻结 v2 领域契约和 Schema

目标：先把 workflow 中所有关键对象定型，禁止后续各层重新发明一套 Outcome 或
Result。

实现内容：

```text
InvestigationRun
InvestigationContract
EvidenceGoal
PlanRevision
TaskCandidate
ExecutableTask
InvestigationReport
EvidenceUnit / EvidenceRef
VerifiedClaim
EvidenceGap
AnswerDraft
DeliveryResult
RunFailure
```

必须冻结：

```text
对象 ID 和版本规则
TaskStatus
RunStatus
FailureCode
SchemaRef
EvidenceRef
ClaimStatus
DeliveryStatus
```

建议新包：

```text
internal/agent/investigation/contract
internal/agent/investigation/schema
```

验收标准：

```text
所有跨阶段数据只使用 v2 contract；
每个任务输出都有 SchemaRef；
失败、无证据、部分成功和取消都有明确状态；
不再用空字符串判断任务是否成功。
```

依赖：无。

### P0-02：实现 Run 生命周期和持久化模型

目标：让一次调查可以创建、恢复、取消、超时并进入唯一终态。

实现内容：

```text
Run 创建和状态转换
Contract 快照
PlanRevision 快照
Task 和 TaskAttempt 记录
终态 DeliveryResult
事件 sequence
```

状态只保留真实生命周期：

```text
created -> analyzing -> planned -> executing -> verifying -> composing -> delivered
```

异常终态：

```text
failed
cancelled
timed_out
budget_exhausted
```

建议新包：

```text
internal/agent/investigation/coordinator
internal/agent/investigation/persistence
```

验收标准：

```text
非法状态转换被拒绝；
Run 重启后可以从最后一个持久化事件恢复；
终态只能写入一次；
REST、MCP 和 SSE 最终读取同一个 DeliveryResult。
```

依赖：P0-01。

### P0-03：实现 PlatformSettings 到 RunBudget 的预算治理

目标：明确预算来源，保证最终答案和兜底输出始终有资源。

实现内容：

```text
InvestigationBudgetPolicy
RunBudget 快照
StageBudget 预留
TaskBudget 授予
Reserve / Settle
输入 token、输出 token、工具调用、耗时和成本计量
```

平台配置负责硬上限和 profile；模板只提供单任务成本参考及安全上限。

建议新增明确的配置语义：

```text
InvestigationMaxInputTokens
InvestigationMaxOutputTokens
InvestigationMaxToolCalls
InvestigationMaxDuration
InvestigationMaxRounds
InvestigationMaxTasks
InvestigationMaxCostMicros
InvestigationBudgetProfile
```

计划准入必须满足：

```text
活动任务预留 + 调度开销
    <= Run 剩余预算
    <= 当前阶段剩余预算
```

并且要先预留：

```text
Composer 预算
DeterministicRenderer 预算
```

验收标准：

```text
Run 创建时保存预算快照；
任务不能自行扩大预算；
预留不足时不启动任务；
实际使用少于预留时可以返还；
预算耗尽时仍可生成非空部分结果。
```

依赖：P0-01。

### P0-04：实现 EvidenceLedger 和 ClaimLedger

目标：建立最终答案唯一可信来源。

实现内容：

```text
EvidenceUnit 稳定 ID
EvidenceRef 定位
证据去重
证据来源和范围
Claim 状态
Goal Coverage
EvidenceGap
ConflictRef
```

规则：

```text
Investigator 只能提交 evidence_candidates；
Normalizer 才能创建 EvidenceUnit；
Verifier 才能创建 supported Claim；
Composer 只能读取 VerifiedClaim。
```

建议新包：

```text
internal/agent/investigation/evidence
internal/agent/investigation/verification
```

验收标准：

```text
无 EvidenceRef 的 Finding 不能成为 supported Claim；
不存在的引用被拒绝；
冲突证据被保留，不被静默覆盖；
每个 required goal 都能得到 covered、partial 或 unresolved 状态。
```

依赖：P0-01。

### P0-05：实现 DeliveryGate 和确定性兜底输出

目标：从机制上消灭空答案。

实现内容：

```text
AnswerDraft Schema 校验
Claim/Evidence 引用校验
Scope 校验
空文本校验
supported/partial/unresolved 状态映射
DeterministicRenderer
```

Composer 失败时的固定路径：

```text
有 supported claims -> 生成部分答案
只有 partial claims -> 生成受限答案
只有 evidence -> 说明尚未形成结论
没有 evidence -> 返回 evidence_insufficient
```

验收标准：

```text
succeeded.text != ""；
partial.text != ""；
Composer reasoning-only 不会导致空响应；
Composer 超时、Schema 错误和 Provider 失败都能走兜底；
失败原因、gaps 和 limitations 不丢失。
```

依赖：P0-01、P0-04。

### P0-06：实现通用 TaskTemplateCatalog 和任务执行器注册表

目标：避免无限穷举任务，也避免 LLM 临时发明任务能力。

第一批只实现通用能力：

```text
workspace.resolve_entity
investigation.explore
code.search
code.inspect_symbol
code.trace_calls
runtime.trace_dependencies
evidence.compare
evidence.verify
```

每个模板必须有：

```text
输入 Schema
输出 Schema
goal kind
工具能力要求
前置条件
执行器类型
成本上限
```

执行器类型：

```text
direct_tool
tool_pipeline
investigator
verifier
composer
```

任务模板不是 Agent 定义。一个 `direct_tool` 或 `tool_pipeline` 任务不启动 Agent。

建议新包：

```text
internal/agent/investigation/planning/catalog
internal/agent/investigation/execution
```

验收标准：

```text
Planner 只能引用已注册模板；
未知问题可以由通用模板组合；
没有匹配能力时返回 capability_gap；
模板版本和成本上限可审计；
简单任务不创建多余 Agent。
```

依赖：P0-01、P0-03。

### P0-07：实现 PlanCompiler、权限交集和 DAG 校验

目标：把 Planner 建议变成系统认可的可执行计划。

工具权限使用交集：

```text
PrincipalCapabilities
∩ WorkspacePolicy
∩ ContractPolicy
∩ TemplateToolGrant
∩ ToolRegistryAvailability
```

PlanCompiler 必须校验：

```text
模板存在且激活
输入绑定完整
工具权限合法
输出 Schema 存在
依赖无环
任务没有重复实例
预算预留可行
```

建议新包：

```text
internal/agent/investigation/planning
```

验收标准：

```text
A -> B -> A 被拒绝；
无权限工具被拒绝；
没有输出 Schema 的任务被拒绝；
required goal 没有可行候选任务时不启动执行；
PlanRevision 可重放。
```

依赖：P0-03、P0-06。

### P0-08：实现单轮 Scheduler

目标：先跑通一轮稳定的任务执行，不急着做无限动态规划。

实现内容：

```text
按 DAG 找 ready task
并行执行互不依赖任务
任务级超时和取消
预算 Reserve / Settle
失败任务记录 failure
完成后统一进入 Normalize -> Verify
```

必须区分：

```text
Task 数量
Agent 数量
Tool Call 数量
```

验收标准：

```text
依赖未满足的任务不启动；
互不依赖任务可以并行；
预算不足不超发；
取消后不启动新任务；
任务失败不会被当作空成功。
```

依赖：P0-02、P0-03、P0-07。

### P0-09：接入最小 Investigator、Verifier 和 Composer

目标：在新契约上实现最小三个 Agent 角色。

边界：

```text
Investigator：只取证，输出 InvestigationReport
Verifier：只验证 Claim 和 Evidence，不生成用户答案
Composer：只组织已验证内容，不调用调查工具
```

Investigator 的上下文必须只包含：

```text
TaskInput
Contract 允许范围
该任务允许的工具
必要的 SeedEvidence
TaskBudget
```

验收标准：

```text
Investigator 不能访问未授权工具；
Composer 没有调查工具；
Composer reasoning 截断可以回到 DeliveryGate；
所有输出通过 Schema 校验。
```

依赖：P0-04、P0-05、P0-06、P0-08。

### P0-10：完成最小端到端回归场景

目标：证明 v2 的核心不变量真实成立。

至少覆盖：

```text
正常找到证据 -> succeeded
只有部分证据 -> partial 且非空
没有证据 -> evidence_insufficient 且非空
Composer 第一次 reasoning-only，第二次仍失败 -> deterministic partial
预算不足 -> 不启动超额任务并返回非空结果
工具失败 -> 保留 failure 和已确认证据
```

验收标准：

```text
所有公开 succeeded/partial 结果 Text 非空；
每个 supported Claim 可追溯到 EvidenceUnit；
Run trace 可以区分 planning、execution、verification、composition 和 delivery；
测试不依赖真实付费 Provider 或 live Docker 服务。
```

依赖：P0-01 至 P0-09。

### P0 退出条件

```text
新的 v2 Run 可以独立创建和执行；
至少一个代码调查场景可以端到端完成；
空答案在 DeliveryGate 被系统性阻断；
预算超发、权限越界和 DAG 环被系统性阻断；
旧 workflow 不参与 v2 测试和运行。
```

## 5. P1 任务

### P1-01：实现基于 Gap 的动态 Replan

目标：让下一轮任务由证据缺口驱动，而不是固定 Agent 顺序。

触发条件：

```text
required goal 未覆盖
发现新实体或新依赖边
证据冲突
任务失败但存在替代来源
当前任务已无新增价值
```

实现要求：

```text
Planner 只读取 Ledger 摘要；
产生新 PlanRevision；
不重复已完成任务；
不把跨轮结果重新构造成循环依赖；
每轮都有最大任务数和最大轮次。
```

依赖：P0 全部。

当前落地补充（2026-08-22）：

- `TaskTemplate` 增加服务端维护的 `DiscoveryTypes` 能力声明，只允许 `entity` 和 `dependency` 两类归一化发现形状；
- Replan 不再把所有 discovery 一律生成 `investigation.explore`，会优先选择声明了对应 discovery type 且覆盖未解决 Goal 的模板；
- 默认目录已为 `code.find_ai_entrypoint`、`code.trace_call_chain`、`api.list_external_endpoints`、`runtime.trace_dependencies` 和通用 explore fallback 声明适用形状；
- discovery 输入在候选生成边界再次建立规范形状，未知类型、缺少实体端点的记录被忽略；重复 discovery 只生成一个候选，候选按稳定 ID 排序，避免并发执行顺序影响 PlanRevision；
- entity discovery 保留单个实体，dependency discovery 保留 from/to 两端和 kind，并将 kind 纳入任务目标描述；
- Replan 现在按确定性 bounded score 选择候选：required/high-risk/多覆盖 Goal 优先，模板声明的 `SourceKinds` 与 Goal 的 `RequiredSources/Sources` 匹配会增加收益，`Provides/RequiredInputs` 可解锁的后续候选会增加依赖解锁分，独立任务优先，模板 `CostProfile` 作为成本扣分；未掌握的失败概率和证据重复概率保持为 0，不伪造统计结论；
- `maxTasks` 超限时不再整轮放弃，而是使用贪心 set-cover 选择收益最高的候选；每个候选会先展开未执行依赖，只有任务数和 `StageExecution` 剩余 BudgetVector 都能容纳且能覆盖全部 unresolved required Goal 时才进入下一版 Plan；已执行依赖视为满足，不会被错误地重新加入；
- 候选选择完成后按稳定 Task ID 输出，保证同分、并发和 map 遍历顺序不改变 PlanRevision；已经执行的候选被过滤，剩余声明了不同来源边界的模板可作为失败后的替代来源；
- 该轮仍不把运行时 duplicate evidence ratio 或失败概率写入评分，重复证据由 Evidence admission/policy 负责，历史成本和失败率校准保留给 P2-01。

### P1-02：扩充通用模板和模板组合

目标：覆盖代码、配置、API、文档和运行时调查，不按用户问题无限增加模板。

新增方向：

```text
config.find_model_provider
api.list_external_endpoints
docs.find_ai_integration
runtime.find_failure_path
code.find_ai_entrypoint
code.trace_call_chain
```

模板应优先表达可组合动作和参数，而不是一个问题一个模板。

依赖：P0-06、P1-01。

### P1-03：实现多执行器和并行调度

目标：让简单任务不启动 Agent，复杂任务才使用 Investigator。

实现内容：

```text
DirectToolExecutor
ToolPipelineExecutor
InvestigatorExecutor
VerifierExecutor
ComposerExecutor
```

调度器需要支持：

```text
按执行器类型路由
任务并发上限
Agent 并发上限
工具调用并发上限
任务取消传播
单个 Agent 失败不影响其他独立任务
```

依赖：P0-08、P0-09、P1-01。

### P1-04：完善 Evidence 验证、冲突和覆盖率

目标：从“找到文本”提升到“可以确认结论”。

实现内容：

```text
多来源证据合并
证据冲突识别
Claim 支持等级
高风险 Goal 的最低证据要求
Coverage 计算
Unsupported / Partial / Supported 分层
```

依赖：P0-04、P1-01。

当前落地补充（2026-08-22）：

- `EvidenceLedger` 的冲突分组使用完整 identity：`SourceKind + Target + Section + Version + TimeRange`；不同来源、版本或时间范围不会被错误归并为冲突；
- `EvidenceUnit` 的稳定 ID 同样包含版本和时间范围，内容相同但 identity 不同的 seed/工具证据保持独立；
- `EvidenceRef` 在 `SourceKind`、`Target`、`Section`、`Version`、`TimeRange` 和 `ContentHash` 任一已提供字段不匹配时拒绝，避免 claim 通过错误 provenance 引用证据；
- 新增测试覆盖独立 identity、同 identity 冲突、同 identity 去重、seed identity 和完整引用校验；
- Claim ingress 会 trim GoalID/Text，拒绝越界或 NaN confidence，校验并稳定去重 EvidenceRef/ConflictRef；同一 `GoalID + Text` 的重复 Claim 会 union 两次验证任务的 provenance，并保守合并状态与 confidence；
- Claim 标为 `supported` 但带有显式冲突引用，或同一完整 evidence identity 下出现不同 ContentHash 时，会自动降级为 `conflicting`；跨多次 Admit 合并 provenance 后会重新检查冲突，`partial` 不会被错误升级或降级；
- EvidenceRef 准入后会从 admitted EvidenceUnit 回填完整 identity 和 ContentHash，Evidence facet 也会在 ingress trim、去重并稳定排序；Coverage 会根据 admitted supporting evidence 聚合 Facets 和 SourceKind，输出 `MissingFacets`、`MissingSources`；`BuildReport` 和 Composer limitations 会把这些缺口公开给 REST/SSE/MCP 读取的统一 DeliveryResult；PruneUnreferencedEvidence 同时保留 support/conflict 引用；
- 这里的“多来源”仍表示保留独立 provenance 并参与校验，不等同于已经完成带来源优先级、合并规则和审计输出的跨来源事实合并。

### P1-05：接入 REST、MCP 和 SSE 的统一交付结果

目标：所有对外入口读取同一个 `DeliveryResult`，不再各自转换 Outcome。

当前实现：REST run detail 和 SSE terminal 已接入 v2 delivery；MCP 没有独立 QA run read API，避免新增第二套结果转换。

实现内容：

```text
REST /api/qa/ask
MCP 调查调用
SSE 过程事件和终态事件
错误码和 limitations 映射
引用证据的公开格式
```

验收标准：

```text
三个出口的状态、文本、gaps、references 一致；
SSE 最后一条终态事件和 REST/MCP 查询结果一致；
任何出口都不能把空文本映射为 succeeded/partial。
```

依赖：P0-02、P0-05、P1-01。

### P1-06：实现 Run 恢复、超时、取消和幂等

目标：让动态计划在服务重启、客户端断开和任务超时时仍然可控。

实现内容：

```text
事件重放
未完成 TaskAttempt 恢复策略
租约或 fencing
取消传播
阶段超时
终态幂等写入
```

依赖：P0-02、P0-08、P1-01。

### P1-07：实现预算 profile 和运行指标

目标：让预算从拍脑袋默认值变成可观测、可调整的策略。

实现内容：

```text
interactive / deep profile
按模板统计 p50 / p95 成本
按 Goal 统计覆盖率和失败率
按阶段统计预算消耗
按执行器统计 Agent 使用率
预算不足率和 Composer fallback 率
```

依赖：P0-03、P1-01、P1-03。

### P1-08：生产级端到端测试和故障注入

目标：验证 v2 在真实失败条件下仍遵守不变量。

覆盖：

```text
Provider timeout
Provider reasoning truncation
工具不可用
权限不匹配
Schema invalid
DAG cycle
预算不足
证据冲突
Run 重启
并行任务取消
```

依赖：P1-01 至 P1-07。

### P1 退出条件

```text
初始计划和动态 Replan 都能工作；
简单任务不创建多余 Agent；
复杂任务可以使用 Investigator；
所有出口返回统一 DeliveryResult；
Run 可以恢复、取消和超时；
生产级故障注入测试通过；
旧 workflow 已不再是默认入口。
```

## 6. P2 任务

### P2-01：任务价值和成本模型优化

目标：提高在固定预算下的有效覆盖率。

实现内容：

```text
按历史结果校准模板成本
按 Goal 覆盖收益排序
重复证据风险估计
失败概率估计
依赖解锁价值估计
```

约束：评分只能影响排序和准入，不能伪造 Coverage 或 Claim。

依赖：P1-07。

### P2-02：并发、延迟和上下文优化

目标：减少等待和无效上下文，不增加无限重试。

实现内容：

```text
按 DAG 层级最大化并行
限制大工具结果进入 Agent 上下文
跨任务 EvidenceRef 复用
早停和无收益任务取消
Provider/model 能力路由
```

依赖：P1-03、P1-07。

### P2-03：模板生命周期和能力治理

目标：让模板目录长期可维护。

实现内容：

```text
模板版本发布和废弃
Schema 兼容性检查
工具权限变更审计
成本上限变更审计
模板离线回放评估
```

依赖：P0-06、P1-08。

### P2-04：历史数据迁移和旧链路删除

目标：完成从旧 workflow 到 v2 的最终清理。

删除或替换：

```text
forceConclusion
ConclusionRetryMaxTokens
分散的 output recovery
多层 Outcome 转换
publicOutputText 猜测式转换
旧 workflow compatibility adapter
```

保留：

```text
历史 trace 查询脚本
必要的历史数据只读迁移工具
旧结果的离线审计数据
```

依赖：P1 全部通过，且 v2 已经稳定运行一段完整观测周期。

### P2-05：评估集和持续回归

目标：防止后续模板、模型或预算配置变化重新产生空答案和无证据结论。

实现内容：

```text
问题类型评估集
Goal Coverage 指标
Evidence Traceability 指标
Partial Answer 可用性指标
Fallback 触发率
预算超发率
Provider 故障回归集
```

依赖：P1-08、P2-01。

### P2 退出条件

```text
旧 workflow 代码和运行入口已删除；
模板、预算和 Provider 变更都有回归门禁；
核心指标可以按 Run、Goal、Task 和 Agent 维度追踪；
新增问题不需要为每个问题新增一个专用 task_type。
```

## 7. 第一批开发顺序

第一轮不要同时实现所有 Agent 和所有工具，按一个最小纵向切片推进：

```text
P0-01 领域契约
  -> P0-03 预算
  -> P0-04 证据账本
  -> P0-05 DeliveryGate
  -> P0-06 通用模板目录
  -> P0-07 PlanCompiler
  -> P0-08 单轮 Scheduler
  -> P0-09 最小 Investigator / Verifier / Composer
  -> P0-10 空答案和预算故障回归
```

第一批只需要支持一个代码调查闭环：

```text
问题：查找某服务中的 AI 调用入口

任务：
1. workspace.resolve_entity
2. code.search
3. code.inspect_symbol
4. code.trace_calls
5. evidence.verify
6. compose / deterministic delivery
```

完成这个闭环后，再扩展配置、文档、API 和运行时任务。不要在第一批开发中同时
接入所有 Provider、所有工具和所有历史 workflow 分支。

## 8. 每个开发任务的提交要求

每个任务完成时必须同时提交：

```text
代码
单元测试
必要的 Schema 或迁移
运行 trace 示例
失败路径测试
更新后的设计或接口文档
```

任务不得以“模型大概率会返回正确 JSON”作为验收条件，必须验证系统边界和不变量。

## 9. 总体验收

```text
P0：可运行、可持久化、可预算、可交付，绝不返回空成功结果
P1：可动态重规划、可并行、可恢复、可通过 REST/MCP/SSE 统一交付
P2：可优化、可评估、可删除旧链路，长期维护成本可控
```
