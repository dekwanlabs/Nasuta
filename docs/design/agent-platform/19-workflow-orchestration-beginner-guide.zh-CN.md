# Workflow 编排内核——新手完全指南

> **状态：当前实现**
> **更新日期：2026-08-31**
> 本文讲的是 Nasuta 的通用 Workflow DAG 内核，以及它和 QA 委派链路的边界。
>
> **重要边界：QA 已不再启动 durable investigation workflow。** 当前 QA 始终先执行一个普通 Agent Run；需要并行只读调查时，由父 Run 调用 `delegate_investigation`，委派执行器负责创建、限流、持久化和验证子 Run。`internal/agent/workflow` 仍然是通用的持久化 DAG 引擎，主要服务于 Feature Delivery 等 Workflow 场景。

## 第 0 章：先记住一句话

Workflow 是服务端持久化和执行一张**预先固定的有向无环图（DAG）**：

```text
Workflow Definition
  -> Prepare / 校验 / 计算 ContentHash
  -> Service.Start 持久化 Run 与输入
  -> Executor 按依赖分 wave 调度节点
  -> 节点输出形成 Handoff
  -> 后继节点消费 Handoff
  -> 成功、失败、取消或人工审批等待
  -> 通过 checkpoint / recovery 恢复
```

模型可以产生结构化输出，但不能在运行时偷偷添加节点、扩大权限或绕过预算。图、节点合同、Schema、权限、预算和 Retry Policy 都由服务端的 Definition 决定。

## 第 1 章：Workflow 和 QA 委派不是一回事

这是阅读代码前最重要的区分：

| 能力 | 当前实现 | 持久化事实 | 入口 |
|---|---|---|---|
| 普通 QA | 一个 `qa.answerer` Agent Run | `agent_runs`、步骤、LLM 调用、证据和 terminal | `internal/agent/qa/service.go` |
| QA 并行只读调查 | 父 QA Run 调用 `delegate_investigation`，创建有界 child Run | `agent_delegation_tasks`、delegation artifact、证据 ledger | `internal/agent/delegation/` |
| 通用 Workflow | 多个有依赖节点的 durable DAG | `workflow_runs`、`workflow_node_runs`、handoff 和事件 | `internal/agent/workflow/` |

QA 的路由结果现在是**父 Run 的 advisory metadata**，不是把 QA 提升成另一种 Workflow：

```text
QA prepare
  -> evidence/query plan
  -> server route assessment
  -> 普通 qa.answerer Run
       └─ 可选 delegate_investigation
            ├─ investigator child Runs（只读）
            ├─ server Validator 校验报告、claim 和引用
            └─ 结果回到父 Run，父 Agent 继续回答
```

因此，下面关于 `Workflow Service`、DAG、wave、checkpoint 的章节，不应被理解为 QA 的 durable investigation 生命周期。

## 第 2 章：一张 Workflow DAG 长什么样

```text
        ┌──────────────┐
        │ agent: inspect│
        └──────┬───────┘
               │
        ┌──────▼───────┐
        │ agent: review │  （两个节点可以并行）
        └──────┬───────┘
               │
        ┌──────▼───────┐
        │ join / verifier│
        └──────┬───────┘
               │
        ┌──────▼───────┐
        │ gate / approval│
        └───────────────┘
```

图由两类对象组成：

- `Definition.Nodes`：节点 ID、节点种类、输入/输出 Schema、Agent/Capability、权限、预算、超时、Retry 和是否 optional。
- `Definition.Edges`：节点依赖关系，以及 required/optional 语义。

`Definition.ContentHash` 是整张图的指纹。一个已持久化的 Run 恢复时必须继续使用相同版本和相同哈希；不能因为 Catalog 已经发布新版本，就让进行中的 Run 漂移到新图。

## 第 3 章：模型里的核心对象

### 3.1 Workflow Definition

文件：`internal/agent/workflow/model.go`

```go
type Definition struct {
    ID            string
    Version       int64
    InputSchema   agentapi.SchemaRef
    OutputSchema  agentapi.SchemaRef
    Nodes         []NodeDefinition
    Edges         []EdgeDefinition
    Permissions   agentapi.PermissionPolicy
    Budget        Budget
    FailurePolicy FailurePolicy
    ContentHash   string
}
```

调用 `Prepare()` 后，服务端会执行规范化、Schema 连续性检查、节点/边校验、权限和预算限制校验，并计算内容哈希。准备完成后的 Definition 是执行快照，不是给模型自由修改的草稿。

### 3.2 NodeDefinition 和 TaskDirective

当前节点种类由 `NodeKind` 定义：

| Kind | 作用 |
|---|---|
| `agent` | 用固定 Agent Definition 执行一次受限 Agent Run |
| `join` | 合并前驱的 payload 或 evidence view |
| `verifier` | 用确定性规则检查输出、证据、引用和冲突 |
| `gate` | 根据服务端策略允许继续、拒绝或要求澄清 |
| `transform` | 由服务端注册的 dispatcher 做确定性转换 |
| `human_approval` | 持久化为等待人工状态，审批后继续 |

`TaskDirective` 只承载经过服务端校验的任务语义，例如 purpose、investigation/evidence goal IDs、required facets、输入证据引用和并行组。它不是旧的 proposal compiler 输入；当前代码中不存在 QA proposal 编译链。

### 3.3 Handoff 和证据

节点之间不共享可变的聊天上下文，而是通过 `Handoff` 传递：

- payload 及其 Schema；
- producer node、source run 和序列信息；
- 有界的 evidence units；
- evidence conflict、completeness 和 omission metadata。

证据去重和引用绑定使用 `internal/evidence` 的 canonical identity 和 content hash。下游只应消费已经进入 Handoff/ledger 的证据，不能把模型自由声称的文件、URL 或引用当成事实。

## 第 4 章：Workflow 如何启动

核心实现：`internal/agent/workflow/service_execution.go`。

`Service.Start` 的关键顺序是：

1. 校验调用者、场景、Workflow Definition 和输入 Schema。
2. 为 Run 生成或确认 ID，保存 Definition/Selection 快照、输入摘要、权限和预算。
3. 先持久化 `workflow_runs` 及启动事件。
4. 使用 detached context 启动后台执行；客户端断开不会自动删除已创建的 Run。
5. 由 Executor 读取持久化状态并按依赖调度节点。

这套顺序保证恢复器能区分“尚未启动”“正在运行”和“已产生 terminal”。启动失败不会伪造成功结果。

## 第 5 章：执行器怎样一波一波派活

核心实现：

- `internal/agent/workflow/executor_orchestration.go`
- `internal/agent/workflow/executor_handoff.go`
- `internal/agent/workflow/executor_attempt.go`

执行器维护已完成节点、失败节点、输出 Handoff 和当前 attempts。每一轮大致做以下事情：

1. 找出所有前驱已满足的 `readyNodes`。
2. 形成一个 dispatch wave；同一 wave 内没有未满足的 required 依赖。
3. 按全局并发、Capability 并发和节点预算限制启动 attempts。
4. 成功节点写入输出和 Handoff；失败节点按 Retry Policy 分类。
5. wave 结束后更新 checkpoint，再计算下一 wave。
6. 所有节点完成，或 Failure Policy 判定无法继续时，写入 Workflow terminal。

### 5.1 Required 和 optional

- required 前驱失败，通常会阻塞后继或触发 fail-fast。
- optional 节点失败可以在 `collect_available` 策略下保留部分结果，但必须把失败和缺失显式传给下游。
- “没有结果”不能被当成“成功但为空”。

### 5.2 Retry

Retry 由 Definition 的 `RetryPolicy` 和 `RetrySafe` 控制。每次 attempt 都有自己的持久化记录；恢复时不能重复扣除已经结算的用量，也不能无限重试不可安全重放的写操作。

## 第 6 章：预算是 admission control，不是事后统计

核心实现：`internal/agent/workflow/executor_budget.go`。

预算可能包含：

- input / output / total tokens；
- tool calls；
- cost；
- retry 或其他节点级限制。

并发节点开始前先做 admission/reservation，模型调用结束后按真实用量 settle，失败或取消则释放尚未使用的 reservation。这样两个并行节点不会同时消费同一份余额而造成超卖。

公共 `agent` 包通过 `RunBudgetGate`、`RunBudgetUsageGate` 等接口向 Runtime 暴露共享预算边界，不依赖任何已经删除的 durable investigation package。

## 第 7 章：Agent 节点、Verifier 和 Gate

### 7.1 Agent 节点

`internal/agent/workflow/agent_node.go` 会：

1. 解析输入 Handoff；
2. 根据 `TaskDirective` 做有界 context projection；
3. 计算 actor、scenario、workflow、node 权限的交集；
4. 以固定 Definition/Schema/ContentHash 调用 Definition Runtime；
5. 将 Agent 输出和证据转换为 NodeResult/Handoff；
6. 对报告型输出执行必要的 completeness 和 evidence 约束。

### 7.2 Verifier

`internal/agent/workflow/verifier.go` 是服务端的确定性边界。它会检查：

- output Schema 是否正确；
- required/high-risk 目标是否覆盖；
- 引用是否指向已准入、身份匹配的证据；
- 同一 canonical target 是否产生冲突；
- payload、findings、uncertainties 和 omissions 是否超限。

### 7.3 Gate

`internal/agent/workflow/evidence_risk_gate.go` 和通用 Gate 逻辑根据验证结果选择继续、拒绝、部分交付或澄清。Gate 的结果必须进入事件和 Run 状态，不能只写日志。

## 第 8 章：取消、审批和恢复

核心实现：

- `service_checkpoint.go`
- `service_recovery.go`
- `store_checkpoint.go`
- `store_execution.go`

恢复流程：

1. 启动时扫描未完成 Workflow Run。
2. 读取完整 Run、节点和 Handoff checkpoint。
3. 校验 Definition version/content hash 和输入身份。
4. 将上次进程中断时仍为 running 的 attempt 转成可解释状态：重试、失败或等待审批。
5. 保留已结算 usage，重新计算 ready nodes。
6. 从最近 checkpoint 继续，不重放已经成功且已持久化的节点。

取消、预算耗尽、审批拒绝、Schema 错误和恢复冲突都应有明确 status/error code。恢复器不能因为某个 child 或节点暂时不可见，就把整个结果静默标成成功。

## 第 9 章：权限不允许越权

一个节点的有效权限只能收窄：

```text
actor permissions
  ∩ scenario permissions
  ∩ workflow permissions
  ∩ node permissions
  ∩ agent/capability permissions
```

QA 动态委派还会额外强制：

- 最大深度为 1；
- child 只能使用 allowlist 中的 enabled investigator capability；
- capability 必须 `RoleInvestigator`、read-only、`SideEffectNone`；
- child 的工具、输入、输出、并发、token、cost 和 timeout 均受 `DelegationPolicy` 限制；
- verifier 只处理 bounded reports/evidence，不为 child 追加新权限。

## 第 10 章：当前 QA 委派链路的正确阅读顺序

如果目标是排查 QA workflow 遗留，按以下顺序读：

1. `internal/agent/qa/service.go`：确认 QA 始终走普通 Agent Run。
2. `internal/agent/qa/prepare.go`：看检索、上下文、Run limits 和提交。
3. `internal/agent/qa/route.go`：看 advisory route 和降级原因。
4. `internal/agent/qa/submission.go`：看 parent context 如何注入 `delegate_investigation`。
5. `internal/agent/delegation/tool.go`：看工具 Schema、任务上限和返回合同。
6. `internal/agent/delegation/executor.go`：看 child 创建、并发、重放、settle 和 artifact。
7. `internal/agent/delegation/validator.go`：看 claim/citation/conflict 验证。
8. `internal/agent/run/store_delegation.go`、`store_evidence.go`：看当前持久化事实。

如果目标是排查通用 Workflow：

1. `internal/agent/workflow/model.go`：Definition、Node、Edge 和 Prepare。
2. `internal/agent/workflow/service_execution.go`：Start 和后台执行。
3. `internal/agent/workflow/executor_orchestration.go`：wave、ready nodes 和 terminal。
4. `internal/agent/workflow/executor_attempt.go`、`executor_budget.go`：attempt、预算和重试。
5. `internal/agent/workflow/agent_node.go`、`evidence.go`、`verifier.go`：节点输出与证据。
6. `internal/agent/workflow/service_recovery.go`：checkpoint 和恢复。

补充：通用 Workflow 的 Catalog 读取仍有一条有明确边界的存量数据兼容路径，用于校验并安全执行缺少 `max_rounds` / `max_depth` 的历史 Definition；它不属于 QA 链路。新 Definition 必须提供这两个字段，待存量 Definition 完成重新发布或终止后即可移除该路径。

## 第 11 章：本次 cleanup 删除了什么

本次收敛明确删除以下不再属于当前 QA 链路的实现：

- `internal/agent/investigation/` 整个 durable investigation coordinator/scheduler/lease/persistence/planning/delivery 包；
- `internal/agent/workflow/proposal*.go` 的旧 Proposal Compiler 链；
- QA 专属的旧 TaskGraph/Proposal 数据结构和旧 Investigation 配置；
- `investigation_runs`、`investigation_events`、`investigation_leases` 以及旧 `workflow_escalations` 的启动 schema/迁移遗留；
- 只服务上述路径的 run terminal 字段、测试 fixture、预算 helper 和无调用 API。

已部署环境还需要执行：

`docs/sql/migration_remove_legacy_investigation_workflow_20260831.sql`

它会删除历史旧表和 `investigation_*` settings 行。历史 SQL migration 和历史设计提案仍保留，原因是它们记录过去已经执行过的数据库/设计变更；它们不代表当前运行时。

## 第 12 章：一句话总结

> **当前 QA = 普通 Agent Run + 可选的有界 `delegate_investigation`；当前 Workflow = 独立的通用持久化 DAG 引擎。**
>
> 不要再把 QA 的动态委派理解成旧的 durable investigation workflow，也不要在当前代码中寻找已经删除的 Proposal Compiler。
