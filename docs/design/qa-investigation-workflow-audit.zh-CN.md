# QA / Investigation Workflow 全流程审计

- 审计日期：2026-08-23
- 审计对象：QA 问答进入动态 Investigation Workflow v2 后的规划、执行、证据验证、交付、父子运行收敛、重启恢复和会话归档
- 审计基线：`docs/design/dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md`、最新运行日志、当前工作区代码和已有测试
- 结论级别：本文是代码审计和修复依据，不把实施任务清单中的“已完成”直接视为生产验收完成

> 说明：实施任务清单是设计/实现基线，属于被审计资料；仓库 `AGENTS.md` / `CLAUDE.md` 才是代码修改约束。本文不新增针对某个问题文本、某个 goal ID 或某个 workflow ID 的特例。

## 1. 总结结论

当前系统不是单一竞态问题，而是三类问题叠加：

1. **目标契约在边界转换时丢失**：QA 层同时拥有 `InvestigationGoals` 和 `EvidenceGoals`，但 app adapter 只把 `EvidenceGoals` 转成 Investigation 层的 `Goals`。最新日志中的 `architecture_overview` 因此被错误地拿去匹配 evidence goal，最终在计划编译阶段失败。
2. **父子运行是跨存储、非事务链路**：QA parent、investigation child snapshot 和 session history 分别持久化，正常路径依赖多个先后写入。任意中间点崩溃、延迟、重复调用或删除数据，都会产生“父运行活动但子运行不存在/父运行未收敛/历史重复”等状态。
3. **本地状态与 durable 状态双轨**：runner 同时维护 `runs map` 和 durable store。当前实现解决了“snapshot 创建之前就返回”的正常启动竞态，但没有解决进程重启后的 child resume、重复 Start、local error 覆盖 durable terminal、tracked-but-not-terminal 阻塞 reconcile 等问题。

因此，当前状态应判断为：

> **v2 核心骨架已存在，正常单进程 happy path 有较多实现和单测；但契约闭环、跨存储恢复、幂等和生产级故障验收尚未完成，不能判定为“整个 workflow 已闭环”。**

## 2. 最新日志的直接故障

最新日志的关键顺序如下：

```text
execution route proposed=single_agent effective=multi_agent
path=durable_workflow tasks=4 independent_tasks=4
investigation snapshot persisted ... status=created
investigation await requested
investigation await started
plan_invalid: investigation plan is invalid:
proposal task "investigate.architecture_overview.service.1"
references unknown investigation goal "architecture_overview"
```

这说明：

- 本次 **snapshot 创建竞态已经被当前修改覆盖**：snapshot 已持久化后才向 QA 返回 Start 成功；
- child workflow 也已经进入 await，失败发生在计划编译，而不是 `LoadTerminal` 找不到 snapshot；
- `architecture_overview` 确实存在于 QA 的 `TaskContract.InvestigationGoals`；
- Investigation compiler 当时只有从 `TaskContract.EvidenceGoals` 构造出的 evidence goal map，所以将 investigation goal 当成 evidence goal 校验并拒绝。

真实链路是：

```text
QA TaskContract.InvestigationGoals
  -> QA planner 生成 TaskSpec.InvestigationGoalIDs
  -> app/contractFromTaskContract
  -> 只转换 EvidenceGoals，丢失 InvestigationGoals
  -> InvestigationContract.Goals 只有 evidence goals
  -> CompileProposal 把 InvestigationGoalIDs 对着 contract.Goals 校验
  -> plan_invalid
```

直接根因不是检索结果，也不是 `architecture_overview` 名称非法，而是**两套目标命名空间没有在 Investigation contract 中保留下来**。

## 3. 当前真实流程

### 3.1 入口到交付

```text
POST /api/qa/ask
  -> 加载 session / 历史上下文
  -> query classification + canonical query plan
  -> evidence plan
  -> route decision
  -> contractFromPreparation
  -> task graph plan / LLM proposal
  -> ScenarioLifecycle.Start 创建 QA parent
  -> prepareEvidence / recordEvidenceLedger
  -> InvestigationRunner.Start
       -> contractFromTaskContract
       -> Coordinator.ExecuteWithProposalReady
       -> acquire lease
       -> 检查已有 child run
       -> RunStore.Create(RunCreated)
       -> onPersisted 回调
       -> plan compile / persist plan
       -> execute tasks
       -> verify claims
       -> compose / DeliveryGate
       -> SaveDelivery / SaveMetrics / terminal
  -> QA Coordinator.Await
       -> AwaitTerminal
       -> converge parent
       -> persist session turn
       -> ScenarioLifecycle.Complete(parent)
  -> SSE / REST 投影结果
```

### 3.2 Investigation child 生命周期

代码定义了以下生命周期：

```text
created
  -> analyzing
  -> planned
  -> executing
  -> verifying
  -> replanning (可重复)
  -> composing
  -> delivered

异常终态：
  -> failed
  -> cancelled
  -> timed_out
  -> budget_exhausted
```

`RunStatus.Terminal()` 只认 delivered 和四类异常终态。child 的 plan、budget、task、result、evidence、claims、report、metrics、delivery 均有独立保存入口；这些保存操作不是一个跨所有阶段的数据库事务。

### 3.3 三套持久化事实

| 事实 | 主要对象 | 所有者 | 与其他事实的关系 |
| --- | --- | --- | --- |
| QA parent | `agent_runs` 中的 `KindQAParent` | `internal/agent/definition` / `internal/agent/run` | 保存用户问题、session、`WorkflowRunID`、QA terminal projection |
| Investigation child | `InvestigationRun` snapshot + event sequence | `internal/agent/investigation` | 保存 contract、plan、task、ledger、report、delivery |
| Session history | session turns / compaction / history index | `internal/memory` | 面向下一轮问答，不是 workflow 状态源 |

三者没有共同事务。`CompleteQAParent` 可以把 parent 状态和 parent terminal event 放在一个事务中，但它不能和 child store、session history 的写入组成原子操作。

## 4. ID 和命名空间审计

当前至少有以下不同语义的 ID：

| ID | 含义 | 典型来源 | 不能替代 |
| --- | --- | --- | --- |
| `parentRunID` | QA 父运行 | `run_*` | child workflow ID、route task ID |
| `workflowRunID` | QA 记录引用的 investigation child | `workflow_*` | parent ID、proposal task ID |
| investigation run ID | child snapshot 的持久化 ID | 当前通常与 `workflowRunID` 相同 | 不应依赖“通常相同”而省略校验 |
| route task ID | QA 业务分解任务 | planner / route | proposal task ID |
| proposal task ID | child plan 中可执行节点 | `TaskSpec.ID` | goal ID |
| investigation goal ID | 最终交付目标 | `InvestigationGoal.ID` | evidence goal ID |
| evidence goal ID | 证据覆盖目标 | `EvidenceGoal.ID` | investigation goal ID |
| facet / kind | 证据维度或目标类型 | `RequiredFacets` / `EvidenceGoal.Facet` | 任意 goal ID |

当前日志中同时出现 `tasks=4`、`independent_tasks=4`、proposal task ID 和 goal ID，但没有统一字段标明各自命名空间，故排错时容易把“4 个独立业务任务”“4 个 proposal 节点”“server verifier 节点”混为一谈。

## 5. Contract 字段流转审计

### 5.1 期望的契约模型

```text
QA TaskContract
  - InvestigationGoals[]
  - EvidenceGoals[]
  - TaskEvidenceAssignments[]
  - Context / Entities / Objective

InvestigationContract
  - InvestigationGoals[]
  - EvidenceGoals[]
  - Task limits / budget / tool grants
  - SeedEvidence

TaskSpec
  - InvestigationGoalIDs[]
  - EvidenceGoalIDs[]
  - RequiredFacets[]
  - InputRefs[]
  - objective / capability / edges / stop policy
```

其中：

- `InvestigationGoalIDs` 只绑定最终需要回答的业务交付目标；
- `EvidenceGoalIDs` 只绑定证据覆盖目标；
- `RequiredFacets` 是 evidence goal 的维度筛选条件；
- 一个 investigation goal 可以依赖多个 evidence goals，不能用字符串相等隐式替代映射。

### 5.2 当前实际流转

| 边界 | 当前行为 | 风险 | 状态 |
| --- | --- | --- | --- |
| `prepare.go -> TaskContract` | 同时生成 investigation goals 和 evidence goals | 上游模型正确 | 已实现 |
| `task_graph_plan.go` | planner proposal 同时输出两类绑定字段 | 结构存在 | 已实现 |
| `app/contractFromTaskContract` | 只转换 `EvidenceGoals` 到 `InvestigationContract.Goals` | 丢失 `InvestigationGoals`，且下游没有独立字段接收 | **P0 未完成** |
| `CompileProposal` goal map | 只有 `contract.Goals` evidence goal map | 对 `InvestigationGoalIDs` 做错空间校验 | **P0 未完成** |
| `validateProposalGoalSelectors` | 校验 `InvestigationGoalIDs`、`RequiredFacets` | 没有校验 `EvidenceGoalIDs` | **P1 未完成** |
| `proposalGoalIDs` | 实际用 `InvestigationGoalIDs` + facets 产出 `TaskCandidate.GoalIDs` | TaskCandidate 只保留 evidence goal IDs，investigation 绑定语义丢失 | **P0/P1 未完成** |
| delivery / report gaps | 当前主要按 evidence goal ID 输出 gap | 无法完整回映射到 investigation goal | **P1 未完成** |
| continuation | 用 verifier 的 unresolved goal ID 同时收缩两类 goal | 两套 ID 不同会停止 continuation 或错收缩 | **P1 未完成** |

### 5.3 需要冻结的结构

推荐将 `InvestigationContract` 改成显式双集合：

```go
type InvestigationContract struct {
    InvestigationGoals []InvestigationGoal
    EvidenceGoals      []EvidenceGoal
    // 其余字段保持现有约束
}
```

同时为 task 的两类绑定保留独立字段，编译时分别执行：

```text
InvestigationGoalIDs -> InvestigationContract.InvestigationGoals
EvidenceGoalIDs      -> InvestigationContract.EvidenceGoals
RequiredFacets       -> EvidenceGoal.Kind / Facets
```

如果最终运行时只需要 evidence goal，仍必须保留 investigation goal 到 evidence goal 的显式映射，不能在 adapter 中静默丢弃。

## 6. 最新竞态修复的效果和边界

上一阶段加入了 `ExecuteWithProposalReady`，并在 `RunStore.Create` 成功后触发 `onPersisted`。`qaInvestigator.Start` 等待该回调后才返回，因此覆盖了以下正常竞态：

```text
旧行为：Start 返回 -> QA Await -> child snapshot 尚未 Create -> investigation_run_missing
当前行为：Create snapshot -> onPersisted -> Start 返回 -> QA Await
```

但这只覆盖“同一进程、Start 正常执行、snapshot Create 成功”的窗口。以下问题仍未闭环：

1. **parent 已创建、child Create 之前进程崩溃**：启动恢复只能看到 parent，不一定能判断 child 是尚未创建、延迟可见还是永久丢失。
2. **child store 和 parent store 跨数据库**：没有事务或 outbox/状态意图记录保证两边一致。
3. **恢复时 child 尚未可见**：`LoadTerminal` 读到 `ErrNotFound` 后当前逻辑直接生成 `investigation_run_missing`，没有重试窗口或“待确认缺失”状态。
4. **child 已存在但仍 active**：`LoadTerminal` 对 tracked state 直接返回“not terminal”；恢复路径没有自动 `Resume` child。
5. **child 被误删**：系统无法区分数据损坏和一次尚未完成的创建。
6. **重复 Start**：`track` 对同一 workflow key 直接覆盖本地 state；没有明确的 same-request idempotency、conflict 或 replay 策略。

因此，当前修复是“消除正常创建竞态”，不是“完成分布式恢复”。

## 7. 本地状态与 durable 状态审计

### 7.1 `qaInvestigator.runs` 的问题

`app/investigation_runner.go` 同时保存：

```text
runner.runs map[workflowRunID]*investigationState
RunStore.Get(workflowRunID)
```

当前行为：

- `AwaitTerminal` 先看 process-local state，再轮询 durable store；
- `LoadTerminal` 如果发现 local state 正在运行，直接返回 not terminal，不再读取 durable store；
- `track` 对相同 workflow ID 直接覆盖；
- terminal state 在 map 中没有清理；
- `complete` 写入 local terminal/error，但不做 duplicate completion 判定。

风险：

| 场景 | 当前风险 |
| --- | --- |
| 本地执行报错，但 durable child 已经 terminal | local error 可能优先于 durable terminal，调用方收到错误 |
| 进程外 worker 完成 child，本进程仍有 running state | `LoadTerminal` 可能返回 not terminal，阻断 parent reconcile |
| 同一 workflow 重复 Start | 新 state 覆盖旧 state，旧 goroutine 完成时可能写入新 state |
| 大量完成 run | `runs` 长期增长，形成内存泄漏/陈旧索引 |
| 取消时 ID 不一致 | `Cancel` 依赖 state.runID；不同入口可能用 workflow ID 或 contract run ID |

建议原则：durable snapshot 是唯一事实源；local state 只能作为等待通知缓存。所有 terminal/read/reconcile 必须最终以 durable 状态为准，local state 只负责减少轮询，不得改变语义。

### 7.2 空 context 和 caller cancellation

`Start` 使用 `context.WithoutCancel(ctx)` 让 snapshot 创建后执行不受请求取消影响，这对 SSE 客户端断开是合理的；但必须同时持久化 parent 的取消意图，避免“请求已取消但 child 继续消耗预算”。当前 cancellation、request disconnect、server shutdown、explicit user cancel 的责任边界仍需在接口层明确。

## 8. 启动恢复和 parent reconcile 审计

当前启动顺序为：

```text
recoverActiveWorkflows
  -> recoverActiveQAParents
```

QA parent 恢复会：

```text
ListActiveQAParents(startedBefore, cursor, pageSize)
  -> Reconcile(parent.ID)
  -> LoadTerminal(parent.WorkflowRunID)
  -> terminal 已存在则 converge
  -> ErrNotFound 则直接 complete(parent, investigation_run_missing)
```

### 8.1 已有优点

- 使用 cutoff + keyset cursor + page size，避免无限扫描；
- 单个 parent 失败后继续扫描其他 parent；
- `CompleteQAParent` 对 parent terminal event 做事务处理，并对重复完成有 replay 逻辑；
- 最新修改已经增加 parent、workflow 和 phase 日志。

### 8.2 未完成的恢复语义

1. **没有 child investigation recovery scan**：启动恢复代码没有对 InvestigationRun 的 active snapshot 执行 `Coordinator.Resume`。注释也明确写着 unfinished snapshot recovery 尚未完全接入。
2. **ErrNotFound 被过早解释为永久丢失**：`Reconcile` / `Await` 会将 missing child 转为 `investigation_run_missing`，没有区分 transient visibility、创建中断和数据删除。
3. **恢复重入缺少 parent lease**：两个进程可以同时 reconcile 同一 active parent；虽然 parent complete 有条件更新，但前置 session 写入和 child 读取可能重复。
4. **恢复与正常 Await 并发**：正常请求和启动 recovery 可同时完成同一个 parent，必须依靠整个流程幂等，而不仅是最后一次 SQL 更新。
5. **恢复错误不会形成下一次可识别的 retry state**：startup report 只记录 errors，parent 仍可能保持 active，下一次启动会再次走同一错误路径。

## 9. 持久化顺序和失败矩阵

| 失败点 | 已写入事实 | 当前可能结果 | 应有行为 |
| --- | --- | --- | --- |
| parent Create 后、child Create 前崩溃 | parent active | 下次启动 child missing，误报 `investigation_run_missing` | 可重试 child 创建，或持久化 `creation_pending` 意图并按幂等键恢复 |
| child Create 失败 | parent active | parent 长期 active 或被恢复标成 missing | 明确 `investigation_start_failed`，可安全重试/终止 |
| child plan compile 失败 | child terminal failed，parent active | 正常 Await 可收敛；重启依赖 parent 可见 | 保留 child failure code/phase，并验证 parent convergence 幂等 |
| child task 执行中进程崩溃 | child non-terminal | 没有自动 Resume，parent active | lease takeover + Resume，从 event/snapshot 继续 |
| child terminal、parent Complete 失败 | child terminal，parent active | 重复 reconcile | parent completion 和 session side effect 必须可重试且不重复 |
| session AppendTurn 成功、parent Complete 失败 | history 已写，parent active | 重试可能重复 session turn | 使用 parent/run/turn idempotency key 或 transactional inbox/outbox |
| parent Complete 成功、history 失败 | parent terminal，history 缺失 | 下一轮上下文缺少本轮答案 | history 异步补偿，不能回滚已完成 parent |
| delivery 成功、metrics/event 保存失败 | child 可能 terminal 或失败 | 结果和 metrics 不一致 | delivery terminal 与指标补写分离，读取以 delivery 为准 |
| child snapshot 被删除 | parent active | 直接标 missing | 进入数据修复/隔离状态，不能把损坏等同业务失败 |
| duplicate Start | 可能已有 child | 本地 state 覆盖、lease conflict 或 terminal replay 不透明 | same ID 相同请求返回已有 run；不同 contract 返回 conflict |
| duplicate Await/Reconcile | child/parent 已 terminal | 可能重复写 history | terminal replay + side effect idempotency |

## 10. Lease、fencing 和 Resume 审计

已有实现包含：

- Lease acquire/release；
- token-aware lease 接口；
- renewal goroutine；
- fenced mutation wrapper；
- MySQL 条件写；
- Memory/SQLite 等测试实现。

但生产闭环仍需确认：

1. 旧 MySQL 实例是否已经执行 fencing migration；
2. 每个 production RunStore mutation 是否都走 token-aware 条件写；
3. renewal 失败是否立即取消执行并将 child 转为可恢复状态；
4. stale worker 的迟到写入是否在数据库层拒绝，而不是只在进程内校验；
5. Release 是否带 owner/token，旧 owner 是否可能释放新 owner 的 lease；
6. Resume 与新 Start 并发时是否只有一个 owner；
7. 非 fencing LeaseStore 是否可能被误用于生产；
8. 重启后 `Resume` 是否能从最后一个 event/snapshot 继续，并且不会重复扣预算、重复执行已成功 task 或重复写 evidence。

当前代码中 `buildInvestigationCoordinator` 在没有共享 store / DB 时会退回 `MemoryRunStore`，且 lease 可能为空。该行为可以用于单元测试，但生产 QA 必须明确禁止或显式降级并报警，否则“durable workflow”在配置错误时会变成进程内 workflow。

## 11. 错误分类和错误码审计

### 11.1 两层错误码

Investigation 层已有：

```text
plan_invalid
execution_failed
schema_invalid
reasoning_truncated
timeout
cancelled
budget_exhausted
tool_unavailable
permission_denied
empty_output
composer_failed
verifier_failed
```

QA / adapter 层还会产生：

```text
investigation_start_failed
investigation_plan_failed
investigation_run_missing
investigation_execution_failed
investigation_delivery_failed
```

当前需要冻结的映射是：

| 失败阶段 | child `FailureCode` | QA parent `ErrorCode` | 是否可重试 |
| --- | --- | --- | --- |
| contract validation | `plan_invalid` 或独立 contract code | `investigation_plan_failed` | 否，修复输入/代码后重试 |
| snapshot Create | start/persistence code | `investigation_start_failed` | 视存储错误而定 |
| proposal compile | `plan_invalid` | `investigation_plan_failed` | 同一 proposal 不应盲重试 |
| task execution | `execution_failed` / tool/provider code | `investigation_execution_failed` | 按 retryable 分类 |
| verifier | `verifier_failed` | `investigation_delivery_failed` 或独立 verify code | 通常受限重试 |
| composer | `composer_failed` | `investigation_delivery_failed` | 可走 deterministic renderer/fallback，但必须可观测 |
| child missing | 不应伪造 child failure | `investigation_run_missing` | 先确认 transient/permanent |
| parent convergence | child 已知 | 独立 parent convergence code | 必须可重试 |
| session history | child/parent 已知 | 不应覆盖业务结果 | 异步补偿 |

当前 `errorCode(run, runErr)` 在 `runErr != nil` 时大部分回落到 `execution_failed`，而 `investigationTerminal`、`investigationOutcome`、QA parent 又会二次映射。需要保证底层 code、阶段、retryable 和 parent projection 不互相覆盖。

## 12. 观测和日志审计

已有日志改善了 start、snapshot persisted、await、missing、reconcile 和 startup report，但还不足以在一条链路上区分所有 ID 和阶段。

关键日志至少应统一包含：

```text
trace_id
parent_run_id
workflow_run_id
investigation_run_id
route_task_id
proposal_task_id
round
phase
status_before
status_after
investigation_goal_ids
evidence_goal_ids
required_facets
lease_owner
fencing_token
attempt
failure_phase
failure_code
retryable
```

特别需要避免：

- 只打印 `run`，不说明它是 parent 还是 child；
- 只打印 `tasks=4`，不说明是 route task、proposal task 还是 executable task；
- 将完整 prompt、证据内容或敏感参数写入日志；
- 把“降级到 fallback planner”记录成成功；
- 将 `ErrNotFound` 直接写成永久数据丢失而没有 `lookup_attempt` / `visibility_age`。

建议增加指标：

```text
qa_investigation_start_total{result}
qa_investigation_plan_total{result,code}
qa_investigation_missing_total{phase,age_bucket}
qa_parent_reconcile_total{result}
qa_parent_reconcile_retry_total
investigation_resume_total{result}
investigation_lease_fenced_total
investigation_duplicate_start_total{result}
qa_session_archive_total{result}
```

## 13. 完成度重新判定

“已完成 / 部分完成 / 未完成”的判断必须从“代码存在”升级为“代码、契约、失败路径、恢复、并发和验收均通过”。按这个标准重新判定：

| 能力 | 重新判定 | 原因 |
| --- | --- | --- |
| v2 Run / Contract / Plan 基础骨架 | 基本完成 | 双目标契约、计划编译、持久化 Contract v2 和恢复版本门禁已闭环；仍需生产环境验收 |
| 正常单进程 snapshot 启动 | 已完成（范围有限） | `ExecuteWithProposalReady` 在 Create 后通知 |
| 动态 proposal 编译 | 已完成 | InvestigationGoalIDs、EvidenceGoalIDs、RequiredFacets 已分命名空间校验并覆盖回归测试 |
| Budget / Ledger | 基本完成 | 核心账本存在，需补跨 Resume 和指标失败注入 |
| Evidence / Claim / Delivery | 基本完成 | evidence/claim/delivery 主链路存在，目标回映射仍有缺口 |
| 多轮 continuation | 基本完成 | 生产 `qaInvestigator` 已实现 durable `LoadRound` / `StartNextRound`；仍需进程级重启回放 |
| child durable Await | 部分完成 | durable polling 已有，但 local state 仍可改变语义 |
| child restart Resume | 基本完成 | child-first keyset 扫描和分阶段 Resume 已接入；仍需真实 MySQL 双实例验证 |
| parent recovery | 部分完成 | 扫描、日志、terminal replay 存在；missing/并发/跨存储恢复不完整 |
| fencing | 部分完成 | 代码和测试存在，生产 migration/双进程验收缺失 |
| session history 一致性 | 部分完成 | 主路径可写，跨 parent completion 的幂等/补偿未闭环 |
| REST/SSE 统一 delivery 读取 | 基本完成 | 读取 child DeliveryResult 的路径存在；MCP 调查发起/读取能力仍不完整 |
| 生产级端到端验收 | 未完成 | 缺少真实 MySQL、重启、双 worker、取消、接口回放矩阵 |

## 14. 修复优先级和实施顺序

### P0：先修会直接导致请求失败的契约错误

1. 在 Investigation contract 中显式保留 `InvestigationGoals` 和 `EvidenceGoals`；
2. 修正 app adapter，禁止静默丢字段；
3. compiler 分别校验 `InvestigationGoalIDs`、`EvidenceGoalIDs`、`RequiredFacets`；
4. 明确 investigation goal -> evidence goal 的映射；
5. 增加 adapter、compiler、原始日志场景的回归测试；
6. 对 proposal contract 做 ingress validation，失败必须带阶段和命名空间。

P0 退出条件：最新日志中的 proposal 可以编译；非法两类 goal ID 仍被拒绝；任务最终能通过 verifier/delivery；没有空的 succeeded/partial。

### P1：修复 durable 状态和恢复闭环

1. 把 child durable snapshot 作为唯一事实源，local map 只作等待优化；
2. 设计 child 创建意图/幂等键，覆盖 parent created -> child created 的崩溃窗口；
3. `ErrNotFound` 增加确认窗口和永久损坏分类；
4. 启动时扫描 non-terminal investigation runs，按 lease/fencing 执行 Resume；
5. parent reconcile 增加 owner/lease 或数据库条件收敛；
6. duplicate Start / Await / Reconcile 形成明确 replay/conflict 规则；
7. session AppendTurn 使用 parent/turn 幂等键或 outbox；
8. 明确取消、超时、客户端断开和 server shutdown 的语义。

P1 退出条件：进程可在任意阶段重启；只有一个 worker 执行；旧 worker 不能写入；parent、child、history 最终可收敛且无重复副作用。

### P2：补观测、成本和生产验收

1. 补齐统一日志字段和指标；
2. 真实 MySQL migration + 双进程 lease takeover；
3. fault injection：Create、plan、task、verify、compose、delivery、metrics、history 每个边界；
4. REST/SSE/MCP 回放和取消验收；
5. 将评估集接入 CI / deployment gate；
6. 删除或隔离旧 workflow compatibility path，避免两套状态源继续增长。

## 15. 回归测试矩阵

### 15.1 契约和计划

- app adapter 保留 investigation goals；
- `architecture_overview` 等非 evidence goal ID 可正常编译；
- 非法 investigation goal ID 失败；
- 非法 evidence goal ID 失败；
- 非法 required facet 失败；
- 两类 goal 同名和不同名时均能正确处理；
- 一个 investigation goal 绑定多个 evidence goals；
- LLM planner 与 deterministic fallback 输出相同契约语义；
- `MaxTasks` 同时覆盖 proposal task、server verifier task、replan task 的定义和上下限。

### 15.2 启动、并发和幂等

- snapshot Create 成功前 Start 不返回；
- Create 失败时 parent 得到 start failure；
- Create 后立即 Await 能读到 child；
- duplicate Start + 相同 contract 返回同一 durable run；
- duplicate Start + 不同 contract 返回 conflict；
- duplicate Await 不重复副作用；
- duplicate Reconcile 不重复 session turn；
- local state 被删除后仍可 durable Await；
- local state 存在但 worker 在进程外完成时仍可 LoadTerminal。

### 15.3 恢复和 fencing

- parent active + child created；
- parent active + child executing；
- parent active + child terminal；
- parent active + child missing（短暂不可见和永久删除分别测试）；
- child snapshot 创建失败；
- child 在每个 phase 崩溃后 Resume；
- 两个 worker 同时 Resume；
- stale worker 晚到 SaveResult / SaveDelivery 被拒绝；
- lease renewal 失败取消执行；
- release 不能释放新 owner 的 lease；
- MySQL migration 后旧 schema 明确失败，而不是静默无 fencing。

### 15.4 交付和历史

- delivery succeeded / partial / evidence insufficient / failed；
- composer 失败后的 deterministic delivery；
- parent Complete 成功但 history 失败，后台可补偿；
- history 成功但 parent Complete 失败，重试不重复；
- metrics/event 保存失败不破坏已经持久化的 delivery；
- REST、SSE、MCP 读取同一个 DeliveryResult；
- 取消、超时、预算耗尽都保留明确 error code 和完整度。

## 16. 验收标准

只有同时满足以下条件，才能把该流程标为“已完成”：

1. **契约**：两套 goal 命名空间在 QA、adapter、compiler、verifier、delivery、continuation 全链路保持一致；
2. **状态**：parent 和 child 的状态转换、terminal replay、非法转换有自动化测试；
3. **恢复**：进程可在每个持久化边界重启，child 能 Resume，parent 能最终收敛；
4. **并发**：双 worker、duplicate Start、duplicate Reconcile、stale worker 都有明确结果；
5. **持久化**：session history、parent terminal、child delivery 的跨存储失败有补偿或可重试方案；
6. **错误**：每个失败阶段都有稳定 code、phase、retryable 和可搜索日志；
7. **交付**：无证据、部分证据、失败、取消、超时不会伪装成成功；
8. **生产**：真实 MySQL migration、重启、取消、接口回放和 race test 通过；
9. **质量**：`GOWORK=off go build ./...`、`go vet ./...`、相关单测和必要 race test 通过；
10. **无过拟合**：修复不依赖单个问题中的具体 goal、关键词、workflow ID 或检索结果。

## 17. 本次审计后的结论

最新问题已经从“创建竞态导致 child 找不到”推进到更深一层：**child 已经创建成功，但 contract 仍然不完整，导致在 plan compile 阶段失败**。这说明此前修复主要集中在时序和日志，而契约模型没有同步冻结。

后续不应继续围绕单条日志增加局部判断。正确顺序是：

```text
先统一双目标契约
  -> 再修 proposal 编译和 coverage 回映射
  -> 再补 child Resume / parent recovery / idempotency
  -> 再做跨存储故障注入和真实环境验收
```

在完成上述步骤前，系统可以继续用于定位问题和开发验证，但不应把“P0 核心机制已完成”理解为“QA investigation workflow 已经生产闭环”。

## 18. 修复落实结果（2026-08-23）

本文第 11～16 节列出的代码级 P0/P1 缺口已按机制修复，而不是按单个 workflow ID 特判：

1. QA 和 Investigation 的 `InvestigationGoals` / `EvidenceGoals` 已分离建模，proposal、task、projection、verification、delivery 和 continuation 均使用明确命名空间；
2. `Start` 以 durable snapshot 为成功边界，重复 active Start 返回冲突，terminal replay 会校验 contract identity；
3. `AwaitTerminal` / `LoadTerminal` 以 durable store 为权威，active snapshot 返回稳定 conflict，missing 经过有界退避重试后才收敛为 `investigation_run_missing`；
4. startup recovery 按 child、通用 workflow、QA parent 顺序运行，active child 使用 `(updated_at, id)` keyset 分页；
5. `created`、`analyzing` 和无 durable plan 的 snapshot 会明确失败，`planned`、`executing`、`verifying`、`replanning`、`composing` 均有确定恢复路径；
6. MySQL lease 使用随机 owner、单调 token、续租、token-aware release、恢复 token 接管和条件写；并发 Resume 只有一个 owner；
7. lease renewal failure 以 `lease_lost` 失败，不再伪装成用户取消；远程取消会先 revoke lease 并推进 fencing token，再写入 cancelled；
8. stale worker 的 result、delivery、budget 和 release 均不能覆盖新 owner；
9. parent terminal replay、并发 Complete 和 session `run_id` 幂等键共同避免重复历史副作用；
10. production runner 已实现 durable continuation round，持久化 actor、round、base depth、entity detail、conversation refs、time range 和 seed context；
11. delivery projection 明确输出 partial/unresolved evidence goals、rejected claims 和 evidence conflicts；
12. 生产环境不再静默退化到 `MemoryRunStore`，缺少 durable store 会直接失败并保留可搜索错误；
13. Dynamic Investigation 已统一为单一当前契约：`task.contract`、`investigation.report`、`investigation.bundle`、`investigation.verified_bundle`、`investigation.answer` 和持久化 `InvestigationContract` 均只发布当前版本；`investigation.verified_bundle` 采用 `evidence_id` + `evidence_lookup` 紧凑交接；未知 Schema、`CompatibleFrom`、字段 alias 和按 ID 忽略版本的兼容判断均已删除；非当前运行必须清理、作废或离线迁移，不能恢复执行；
14. 防回归测试覆盖旧 Schema 无法 Resolve、旧 Contract 无法 Execute/Resume、旧 `task.contract` 无法进入 projection，以及默认目录只发布当前版本。

代码级验证覆盖相关 package 单测、race、vet 和 build。真实 MySQL 双实例、部署迁移、进程级 crash
注入及 REST/SSE 端到端回放仍属于生产发布验收；对应 migration 已补充 `fencing_token` 和
`(updated_at, id)` 恢复索引，发布前必须执行。
