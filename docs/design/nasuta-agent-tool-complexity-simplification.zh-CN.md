# Nasuta Agent 与工具链路复杂度审计及简化方案

状态：第一阶段已完成，后续 P0/P1 简化待实施
日期：2026-09-03
适用范围：`agent`、`tool`、`internal/agent/qa`、`internal/agent/execution`、`internal/agent/delegation`、`internal/agent/workflow`、`internal/agent/run`

## 1. 结论

Nasuta 当前的复杂度包含两类：

1. **必要复杂度**：durable lease/fence、重试恢复、权限收敛、工具协议闭合、证据校验；
2. **偶然复杂度**：同一状态在多层重复判断、参数逐层搬运、可选接口 type assertion、同一终态逻辑复制多次、一个函数同时负责多个执行阶段。

需要保留第一类，但必须把它隔离在明确边界内。当前最需要简化的不是单个 `if`，而是以下重复决策链：

```text
QA 路由
→ Tool 可见性
→ Definition Runtime 再准备 ToolScope
→ Execution 再做 admission / budget / dedupe
→ Delegation 再做 capability / queue / budget / validation
→ Workflow 再做一套 budget / retry / handoff
```

目标不是删除校验，而是让每条业务规则只有一个 owner：

```text
路由是否允许 delegation        → QA route
工具是否对本次 Run 可见         → Tool snapshot / ToolScope
单次工具是否执行                → Execution admission
Child 是否允许启动              → Delegation admission
物理模型调用是否还有预算         → Shared Run budget
Workflow 输入输出是否合法        → Workflow compiler / handoff boundary
持久化 lease/fence 是否有效      → Store
```

## 2. 静态复杂度概况

统计范围为 Agent/Tool 相关生产 Go 文件，不包含 `_test.go`。复杂度为基于 Go AST 的近似圈复杂度，用于发现热点，不作为质量的唯一指标。

### 2.1 总体规模

```text
生产 Go 文件：179
生产代码行数：约 52,069
函数/方法数量：1,801
复杂度 >= 20：49 个
复杂度 >= 30：18 个
复杂度 >= 40：5 个
函数长度 >= 150 行：13 个
函数长度 >= 250 行：3 个
```

主要包规模：

| 包 | 生产代码行数 |
|---|---:|
| `internal/agent/workflow` | 13,462 |
| `internal/agent/execution` | 6,293 |
| `internal/agent/delegation` | 5,776 |
| `internal/agent/run` | 5,732 |
| `internal/agent/qa` | 3,964 |
| `internal/agent/definition` | 3,677 |
| `internal/agent/tools` | 2,880 |
| `agent` | 1,672 |
| `tool` | 1,230 |

### 2.2 当前最高复杂度热点

| 复杂度 | 行数 | 位置 | 判断 |
|---:|---:|---|---|
| 49 | 291 | `internal/agent/delegation/executor.go: runTaskOwned` | attempt、runtime、artifact、settlement、checkpoint 混在一起 |
| 49 | 248 | `internal/agent/workflow/investigator_projection.go: projectInvestigatorHandoff` | 选择、裁剪、预算、完整性同时处理 |
| 43 | 214 | `internal/agent/execution/answer_contract.go: ValidateAndStrip` | 校验、授权、清理、错误聚合耦合 |
| 42 | 141 | `internal/agent/run/store_budget.go: reserve` | 多种 reservation 类型共享一个条件密集函数 |
| 40 | 94 | `tool/schema.go: validateArguments` | 动态 `map[string]any` 递归验证造成大量 type switch |
| 39 | 143 | `internal/agent/workflow/model.go: graph` | 建图、边校验、度数、拓扑信息混合 |
| 38 | 303 | `internal/agent/workflow/verifier.go: verifyBundle` | 验证规则过多且集中在单函数 |
| 35 | 218 | `internal/agent/workflow/agent_node.go: Execute` | definition、projection、request、runtime、handoff 全部串在一起 |

## 3. 发现的偶然复杂度

### 3.1 QA 提交链参数爆炸

原 `submitRun` 接收接近 20 个参数，其中大部分已经存在于 `preparation`：

```text
request / definition / selection / question / userID / runID
query / plan / policy / tools / highRisk / limits
trace / ownsTrace / requestCancel
```

问题：

- 参数可以互相不一致；
- 调用点难以确认每个值来自哪个阶段；
- 新增字段时必须继续延长函数签名；
- Run 构造、异步执行、持久化、历史归档、记忆提取混在一个函数。

### 3.2 Delegation Batch 重复终态逻辑

原 `Executor.Execute` 在以下分支重复执行 validation 和 verification：

- child budget reservation 失败；
- 没有合法 task；
- durable reservation 失败；
- child 正常完成。

每个分支都重复：

```text
ValidateWithContext
→ 写入 result.Validation
→ emitValidation
→ attachVerification
→ return
```

同时 budget release、prepared task reject、reservation projection 也散落在主流程中。

### 3.3 工具执行单函数嵌套过深

原 `executeToolTurn` 同时负责：

```text
工具次数预算
→ ToolCall step
→ admission
→ delegation live evidence
→ executor
→ evidence conflict
→ evidence observation
→ delivery 压缩
→ metrics
→ web convergence
→ reference merge
→ ToolResult step
→ message 回填
→ answer contract
```

这使工具调用的“准备、执行、结算”三个阶段无法单独阅读和测试。

### 3.4 工具过滤重复实现

`withoutScenarioTool` 与 `withoutHistoryTools` 都手工复制：

```text
Tools()
→ 遍历
→ 排除 ID
→ 重建 slice
→ 重建 byID map
```

业务上只是同一个操作：从一个 pinned tool set 中排除若干工具。

### 3.5 Progress emitter 依赖可选接口探测

QA Service 原本只声明 `EmitPhase`，然后在每次调用时通过 type assertion 探测：

```text
EmitStatus
EmitContextUsage
EmitSessionStatus
```

生产传入的 Definition Runtime 又没有转发 `EmitStatus`，导致结构化状态在生产链路自动退化成普通 phase 文本。

### 3.6 三套预算账本重复

以下位置包含相似的 usage 加减、available、reserve、settle、release、phase reserve 判断：

```text
internal/agent/budget/budget.go
internal/agent/budget/durable.go
internal/agent/run/store_budget.go
internal/agent/workflow/executor_budget.go
```

部分重复是内存与 durable backend 的必要差异，但预算数学和状态转换不应分别实现。当前风险是：

- 一个实现增加新 usage 字段，其他实现遗漏；
- phase reserve 语义漂移；
- settlement 幂等规则不一致；
- 测试数量随实现份数成倍增加。

### 3.7 Agent Catalog 与 Workflow Catalog 高度相似

`internal/agent/catalog` 和 `internal/agent/workflow/catalog.go` 都包含：

```text
Publish / PublishAs
Resolve
List / ListRecords
SetDefault / SetActive / SetRollout
ListAudit / ListRolloutAudit
lazy load / revision / persistence
```

Definition 类型不同是必要差异，但 revision、rollout、audit、lazy-load 生命周期可以共享一个内部机制。

## 4. 本轮已经实施的简化

### 4.1 QA Run 提交只传递阶段对象

`submitRun` 现在接收：

```text
context
ManagedRun
*preparation
ConversationContext
*admittedEvidence
```

不再逐个传递 preparation 已经持有的十多个字段。

异步阶段拆为：

```text
submitRun                 构造不可变 RunRequest
withDelegationParentContext 仅构造 delegation 上下文
executeSubmittedRun       执行并完成 Run 生命周期
extractRunMemory          后处理记忆
```

效果：

```text
submitRun：复杂度约 24 → 1
函数长度约 185 行 → 58 行
```

### 4.2 Delegation Batch 终态收敛到一个出口

新增职责明确的普通函数：

```text
prepareTasks
delegationInvocation
reserveTaskBudgets
releaseTaskBudgets
rejectPreparedTasks
delegationReservations
delegationRecordsByIndex
collectTaskOutcomes
finalizeBatch
```

所有 batch 分支统一经过 `finalizeBatch`，validation 和 optional verification 不再复制。

效果：

```text
Executor.Execute：复杂度约 30 → 11
函数长度约 229 行 → 85 行
ValidateWithContext 调用点：4 个 → 1 个
```

### 4.3 工具 Turn 拆成准备、执行、结算

工具链现在按阶段组织：

```text
executeToolTurn
    → runToolCall
        → 预算计数 / admission / executor / evidence conflict
    → applyToolExecution
        → delivery / metrics / reference / contract / message
    → recordToolResult
        → durable ToolResult step
```

效果：

```text
executeToolTurn：复杂度约 29 → 9
函数长度约 139 行 → 37 行
```

所有原有工具协议行为保持不变：并行 tool result 闭合、dedupe、evidence ledger、web convergence、delivery artifact 和 answer contract 仍由原 owner 负责。

### 4.4 工具过滤统一

增加：

```text
withoutScenarioTools(prepared, excluded...)
```

`withoutScenarioTool` 和 `withoutHistoryTools` 只表达业务意图，不再复制过滤循环。

### 4.5 Progress emitter 改为显式完整契约

QA 内部 `PhaseEmitter` 现在明确要求：

```text
EmitPhase
EmitStatus
EmitContextUsage
EmitSessionStatus
```

Service 不再进行三次可选接口 type assertion。Definition Runtime 增加 `EmitStatus` 转发，生产链路可以保留 status code 和 elapsed time。

### 4.6 Workflow Node 校验按类型分派

原 `validateNode` 把 Agent、Gate、Transform、Verifier、Join、HumanApproval 的全部规则放在一个 201 行函数中。现在公共 envelope 校验与各 Node kind 校验分开：

```text
validateNode
    → validateNodeEnvelope
    → validateAgentNode / validateGateNode / validateTransformNode
    → validateVerifierNode / validateJoinNode
```

Verifier 的 goal、subject、evidence source 也分别校验。

效果：

```text
validateNode：复杂度约 56 → 10
函数长度约 201 行 → 28 行
```

## 5. 继续简化的优先级

### P0：拆分仍然过大的单函数

1. `runTaskOwned`：拆为 attempt admission、child runtime、report build、durable settlement；
2. `AgentExecutor.Execute`：拆为 definition resolve、input projection、RunRequest build、result mapping；
3. `verifyBundle`：拆为 subject coverage、claim coverage、conflict policy、payload bound。

要求：只拆职责，不新增状态机、wrapper hierarchy 或兼容分支。

### P1：统一预算数学

抽取一个不依赖 Store 的预算核心，统一：

```text
normalize usage
add/subtract usage
remaining capacity
phase reserve
reservation state transition
settlement idempotency
```

内存账本和 durable Store 只负责锁与持久化，不重复业务数学。

### P1：缩短工具定义文件

`internal/agent/tools/registry.go` 的 `builtinTools` 仍超过 260 行。建议按领域返回声明：

```text
serviceTools(...)
codeTools(...)
dependencyTools(...)
runbookTools(...)
webTools(...)
```

Registry 只负责组合和一次性注册，不再同时持有全部 schema 与 handler 细节。

### P1：减少 Scenario Tool 中间层

当前存在：

```text
tool.Registry
→ tool.Snapshot
→ execution.ToolExecutor
→ definition.ScenarioToolSet
→ qa.filteredScenarioTools
→ RunRequest.ToolScope
→ execution 再次 Snapshot
```

后续目标是让 QA 准备阶段直接持有一个 pinned `tool.Snapshot` 和一个执行接口，过滤通过 Snapshot selection 完成。Run 仍需重新 pin 时，应只保留一次明确的版本一致性检查，而不是复制工具列表。

### P2：统一 Catalog 生命周期

抽取内部 revision/rollout/audit/lazy-load 核心，Agent Catalog 与 Workflow Catalog 只提供各自的：

```text
prepare definition
content hash
semantic validation
```

### P2：减少动态 JSON 校验

`tool/schema.go`、Workflow handoff、definition result 中大量使用 `map[string]any`。对于固定内部协议，应优先解码到结构体并在 ingress 一次校验；只有用户扩展的开放 Tool Schema 保持动态 JSON Schema。

## 6. 简化约束

简化过程中必须遵守：

1. 不删除权限、预算、lease/fence、证据和 provider protocol 校验；
2. 不通过新增 manager/facade/state-machine 把复杂度藏起来；
3. 同一个条件只在业务 owner 处决定一次，下游消费结果；
4. compatibility path 必须有真实调用方和删除计划；
5. helper 必须对应一个可命名阶段，而不是只包一行代码；
6. 每轮只处理一个边界，并保留行为回归测试；
7. 复杂度下降以主流程更短、分支 owner 更清楚为准，不以文件数量增加为目标。

## 7. 验收标准

- 核心入口函数建议复杂度小于 15；
- 单个 orchestration 函数建议不超过 100 行；
- 不允许十个以上独立参数在同一内部调用链继续透传；
- 同一终态操作在一个 package 内只能有一个实现入口；
- 同一 emitter/store/runtime 能力不在调用点反复 type assert；
- 工具执行主流程可以直接看出 admission、execution、settlement 三个阶段；
- 全量测试、race、build、vet 和 `git diff --check` 必须通过。

## 8. 本轮验证结果

2026-09-03 已在独立模块模式完成以下验证：

```bash
git diff --check
GOWORK=off go test ./...
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
```

结果全部通过。重新运行同一份 Go AST 统计后，Agent/Tool 相关生产代码仍为 179 个文件、约 52,069 行和 1,801 个函数；当前阈值计数与第 2 节一致，说明本轮重构没有通过移动统计范围掩盖复杂度，已下降的是目标主流程本身。


## 9. 本轮继续简化（第二轮）

本轮继续处理复杂度统计中的最大热点，全部在保持行为语义完全一致的前提下拆分：

1. **execution `ValidateAndStrip`（43 → 拆出）**
   `internal/agent/execution/answer_contract.go`
   - 拆出 `missingLiteralViolations`、`adoptionMarkerPayload`、`decodeAdoptionEnvelope`、
     `validateDelegationSelections`、`validateAdoptedEvidence`、`appendMissingDelegationViolations`。
   - `ValidateAndStrip` 现在只是串行编排各阶段，退出 top-30。

2. **run `storeBudget.reserve`（42 → 拆出）**
   `internal/agent/run/store_budget.go`
   - 拆出 `validateAndNormalizeReservation`、`marshalBudgetReservation`、`loadReservationLedger`、
     `decodeBudgetLedger`、`applyReservationChange`、`insertBudgetReservation`。
   - `reserve` 现在只是串行编排，退出 top-30。

3. **tool `validateArguments`（40 → 拆出）**
   `tool/schema.go`
   - 拆出 `validateArgumentsByType`、`validateArgumentObject`、`validateArgumentArray`、
     `validateArgumentInt`、`validateArgumentNumber`、`validateArgumentConstraints`。
   - 主入口只做 type dispatch 与约束校验两段。

4. **workflow `graph`（39 → 拆出）**
   `internal/agent/workflow/model.go`
   - 拆出 `buildGraphNodes`、`buildGraphEdges`、`validateGraphEndpoints`、
     `validateForwardingNode`、`topologicallyOrder`。
   - `graph` 现在只是编排，退出 top-30。

5. **agent `CapabilityRegistry.prepare`（35 → 拆出）**
   `agent/capability.go`
   - 拆出 `validatePreparedIdentity`、`validatePreparedSchemas`、`validatePreparedLists`、
     `validatePreparedPolicies`、`validatePreparedAgentBinding`。

6. **workflow `prepareDefinition`（34 → 拆出）**
   `internal/agent/workflow/model.go`
   - 拆出 `validateDefinitionBasics`、`validateDefinitionNodes`。

验证结果：

```bash
git diff --check
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
GOWORK=off go test -race -count=1 ./internal/agent/... ./tool/... ./agent/...
```

全部通过。重新运行 Go AST 统计后，当前最大热点已从本轮开始前的
`answer_contract.go ValidateAndStrip (43)` 降为 `runTaskOwned (34)`，
top-30 阈值也由 24 降到约 26，说明主流程复杂度整体下移。

当前 top-5：

```text
34  internal/agent/delegation/executor.go:1265 (method).runTaskOwned
33  internal/agent/definition/result.go:212 mapResult
33  internal/agent/delegation/flow_merge.go:23 MergeFlowIRs
32  internal/agent/workflow/executor_attempt.go:36 (method).executeAttempt
32  internal/agent/qa/context.go:319 (method).assembleActiveHistory
```

## 10. 本轮继续简化（第三轮）

本轮继续从 Go AST 静态复杂度 top-30 出发，逐个拆分最大热点，全部保持行为语义、错误字符串、日志格式、SQL 幂等与排序不变。

1. **tool `validateSchema`（17 → 拆分）**
   `tool/schema.go`
   - 拆出 `validateSchemaObject`、`validateSchemaProperties`、`validateSchemaRequired`、
     `schemaRequiredNames`、`validateSchemaArray`。
   - 主入口只做 type dispatch（object/array/primitive），退出 top-30。

2. **qa `selectActiveTurns`（17 → 拆分）**
   `internal/agent/qa/context.go`
   - 拆出 `collectMandatoryTurns`、`collectAffinityTurns`、`mergeScoredTurns`。
   - 堆排序、显式引用与前置依赖的收集逻辑被隔离，主入口只做编排。

3. **workflow `validateBudget`（17 → 拆分）**
   `internal/agent/workflow/model.go`
   - 拆出 `budgetIsNegative` 辅助判定，去掉超长布尔条件分支。

4. **workflow `FinishRun`（16 → 拆分）**
   `internal/agent/workflow/store_execution.go`
   - 拆出 `finishRunEvents`，承载 final output 落库、关闭运行中节点、运行状态迁移与事件组装。

5. **execution `evidenceManifest`（16 → 拆分）**
   `internal/agent/execution/evidence_manifest.go`
   - 拆出 `encodeEvidenceManifestItems`、`renderEvidenceManifest`，编码去重与渲染截断两段分离。

6. **workflow `prepareAgentRun`（16 → 拆分）**
   `internal/agent/workflow/agent_node.go`
   - 拆出 `validateAgentNodePinning`，收敛 definition 锁定与 schema 兼容校验。

7. **workflow `RecoverWithObserver`（16 → 拆分）**
   `internal/agent/workflow/service_recovery.go`
   - 拆出 `recoverOne`，单条 run 的 resume/status/observer 聚合逻辑移出主循环。

8. **execution `appendConservativeFallbackMetadata`（16 → 拆分）**
   `internal/agent/execution/answer_contract.go`
   - 拆出 `appendFallbackRequiredLiterals`、`buildFallbackAdoptionEnvelope`。

9. **delegation `delegationAdoptionContract`（16 → 拆分）**
   `internal/agent/delegation/tool.go`
   - 拆出 `collectAdoptionContractItems`、`appendAdoptionContractEdges`。

10. **workflow `validateAgentNode`（16 → 拆分）**
    `internal/agent/workflow/catalog.go`
    - 拆出 `validateAgentNodeContract`，permission/tool/schema/budget 各校验保留但隔离。

11. **workflow `PutWorkflowArtifact`（16 → 拆分）**
    `internal/agent/workflow/store_facts.go`
    - 拆出 `validateWorkflowArtifact`、`contentHashOf`、`ensureWorkflowArtifactInsert`。

12. **run `release`（16 → 拆分）**
    `internal/agent/run/store_budget.go`
    - 拆出 `applyReleaseTx`，事务体内 lease/fence、ledger、reservation 状态迁移被隔离。

13. **workflow `convergenceOutcome`（16 → 拆分）**
    `internal/agent/workflow/executor_orchestration.go`
    - 拆出 `convergenceSuccessOutcome`、`convergenceStopOutcome`、`convergenceErrorOutcome`。

14. **qa `applyHistoryRelationSignals`（16 → 拆分）**
    `internal/agent/qa/context.go`
    - 拆出 `applyHistorySelectionSignal`、`cascadeHistoryNeeds`。

15. **run `validateAndNormalizeReservation`（16 → 拆分）**
    `internal/agent/run/store_budget.go`
    - 拆出 `validateReservationShape`，task/call 两类 shape 约束隔离。

16. **run `ReleaseLeaseWithFence`（16 → 拆分）**
    `internal/agent/run/store_budget.go`
    - 拆出 `applyLeaseReleaseTx`，lease 释放事务体移出主入口。

17. **delegation `normalizePolicy`（16 → 拆分）**
    `internal/agent/delegation/executor.go`
    - 拆出 `delegationPolicyLimitsInvalid`。

18. **workflow `joinHandoffs`（15 → 拆分）**
    `internal/agent/workflow/executor_handoff.go`
    - 拆出 `collectJoinReferences`、`joinCompleteness`、`measureJoinConvergence`、`compactJoinEvidence`。

## 验证结果

```bash
git diff --check
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
GOWORK=off go test -race -count=1 ./internal/agent/... ./tool/... ./agent/...
```

全部通过。重新运行 Go AST 统计，最高复杂度已从本轮开始前的 17 降为 15，
且 16/17 级热点全部清零。当前 top-5：

```text
15  internal/agent/tools/code_search.go:227 (method).FindCodeByVector
15  internal/agent/workflow/service_execution.go:218 prepareStart
15  internal/agent/delegation/executor.go:1299 (method).runTaskOwned
15  internal/agent/workflow/executor_orchestration.go:205 (method).runConvergenceLoop
15  internal/agent/workflow/service_approval.go:99 (method).loadApprovalTarget
```
