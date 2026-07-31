# 写操作安全与审批

[English](09-write-safety-and-approval.md) | [中文](09-write-safety-and-approval.zh-CN.md)

> 状态：当前平台机制；Catalog 有意保持封闭且目前聚焦 Incident，扩展动作需独立设计
> 来源：Approval-Gated Write Tools

## 1. 边界

读工具用于获取证据；写操作会修改仓库、记录、外部系统或用户长期状态。二者必须使用不同的 Registry、权限、执行路径和审计合同。

上层应用可以注册读工具，但不能动态注入、替换或删除平台写操作。写操作不进入 MCP，也不进入普通 ReadToolRegistrar。EvidencePlan 和读工具选择永远不意味着拥有写权限。

## 2. 封闭的 Write Catalog

Nasuta 拥有编译期 `writeaction` Catalog。每项定义：

- 稳定 Action ID 和描述；
- 规范化参数 Schema；
- 授权与影响等级；
- 审批展示内容；
- 显式 Dispatcher 分支；
- 幂等与并发合同；
- 审计与结果 Schema。

当前实现只覆盖 Incident 修复类动作，例如分支和提交提案。新增副作用必须修改并评审平台代码；场景配置不能把任意工具变成写工具。

## 3. 提案合同

模型工具调用只能创建 Proposal。Proposal 保存经过身份认证的请求人、规范化参数、理由、影响、相关资源/版本快照、创建时间、过期时间和 Pending 状态。工具结果只能说明“等待审批”，不能声称写操作已经完成。

参数在 Proposal 入口只规范化和校验一次。审批和执行使用持久化后的规范表示，不能重新解释模型文本，也不能从审批请求接收一套替换参数。

## 4. 审批流程

```text
有权限的 Agent Proposal
  -> 持久化 Pending Action 和不可变执行 Payload
  -> 有权限的 Dashboard 列表/详情
  -> Approve 或 Reject
       ├─ Reject：终态和原因
       └─ Approve：原子 Claim Pending Action
                    -> 分发持久化的精确 Action 和参数
                    -> 保存 Done 或 Failed
```

审批必须绑定规范参数、目标身份和预期版本/Base Snapshot。如果目标已经变化且动作要求新鲜性，执行应返回冲突，并要求重新提案。

## 5. 授权

提案权限、审批权限和实际执行能力是三个独立检查。当前 Incident 流程使用管理员权限；通用扩展应定义按 Action 的 RBAC，不能从角色名称或工具可见性推断权限。

请求人和审批人都必须持久化。Action Policy 可以禁止自我审批。未知 Action、Handler 缺失、Manager 不可用、Proposal 过期和权限失败都必须 Fail Closed。

## 6. 幂等与并发

审批必须原子 Claim `pending -> executing`，避免两个并发审批请求重复执行副作用。下游支持时，Dispatcher 传递稳定 Action/Idempotency ID。

只有 Action 合同能证明重试安全时才允许自动重试。Provider、网络或下游结果不确定时，不能未经再次确认就重放副作用；失败或不确定结果应保持可见，交由操作者处理。

## 7. Dry Run 与影响

有实质风险的 Action 应提供确定性 Dry Run/Preview，展示影响资源、校验错误、预期 Diff 和不可逆后果。Preview 是审批证据，本身不构成执行授权。

影响等级可决定审批权限、双人审批、新鲜性窗口或某环境是否禁用执行。这些是平台策略，不能委托给 LLM 自行判断。

## 8. 执行与审计

执行通过显式 Switch 或等价的封闭 Dispatcher 调用一个已知 Action。配置的后端失败必须直接返回，平台不能替换 Provider 或改走另一条写路径。

审计记录 Proposal ID、Action ID、请求人、审批人、规范参数 Hash、目标快照、状态转换、时间、执行结果、错误分类和外部关联 ID。敏感值在入口脱敏，不进入日志和模型可见结果。

## 9. 与 Agent Run 的关系

创建 Proposal 是一个持久化副作用，但不是用户要求的业务变更。原 Agent Run 可以以“等待审批”结束，不需要无限期 Pause。后续请求查询 Action 状态并重新获取执行后的证据。

长期 Memory 写入同样会改变用户持久状态。即使由不同领域服务实现，也应遵循候选明确、Scope 明确、用户审批、可审计和禁止静默写入的原则。

## 10. 当前差距与目标方向

当前限制包括：在 HTTP 请求中立即执行、没有通用持久化执行队列、Exactly-once Claim 仍需加强、管理员级授权较粗、Catalog 仅覆盖少量 Incident Action。这些限制必须保持可见。

只有动作确实需要 Lease、进程退出恢复、取消或延迟执行时，才引入队列；不能为了给线性审批流程换名而增加状态机。

## 11. 验收标准

1. 模型调用不能直接执行副作用；
2. 读工具注册和 Evidence Routing 不能暴露或授权写操作；
3. 审批只执行持久化的 Action、规范参数和绑定目标快照；
4. 并发审批不能重复执行；
5. 未知、过期、陈旧、无权限或能力不可用时 Fail Closed；
6. 副作用重试按 Action 单独设计，默认不自动重试；
7. Proposal、审批、拒绝、执行和失败全部可审计；
8. 新写操作必须明确 Catalog、授权、影响、幂等和恢复设计。

## 详细归并材料

### 需要审批的写工具

> Migrated from CodeLoom `docs/design/agent/agent-write-tools-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现，仅限事件修复操作

写工具允许 QA Agent 提议变更，但不能在模型调用中直接执行。写 Tool Contract 由平台封闭目录拥有，场景上层只能注册读工具。人工审批是独立且经过认证的 Dashboard 操作。

#### 当前范围

`internal/writeaction` 当前预定义：

- `propose_branch`：申请创建事件修复分支；
- `propose_commit`：申请提交修复分支并生成下一事件修复结果。

调用任一工具都会在 MySQL `pending_actions` 中创建记录，包含参数、理由、影响、申请人、状态、时间戳和 24 小时过期时间。工具结果只报告等待审批。

Nasuta 在初始化 Incident 时同时创建 Approval Service、`pending_actions` 存储和封闭写目录。上层不能提供 Proposer、ToolID、Description、Schema 或 Handler。只有当前请求用户是管理员时，写工具才加入本轮 QA Snapshot。`EvidencePlan` 和读工具意图门永远不能授权写能力。MCP 始终使用 ReadPolicy，写目录同时标记为 MCPHidden。

#### 审批流程

```text
已认证管理员的 Agent 提案
  -> Pending Action
  -> Dashboard 列表/详情
  -> 管理员批准或拒绝
       ├─ 拒绝：保存原因和终态
       └─ 批准：将精确工具分发到 Nasuta Incident Manager
                    -> 保存 done/failed 结果
```

平台 Catalog 只支持编译期定义的 Tool Contract，审批执行只支持显式 Dispatcher Case。未知工具名失败关闭。Incident Manager 不可用时禁止执行，不能替换成其他写路径。提案和审批分别记录真实 `requested_by` 与 `approver`。

#### 当前限制

- 已保存过期时间，但本文所述实现没有通用过期 Worker。
- 审批在 HTTP 请求内立即执行，没有持久任务队列或重试编排。
- 执行前检查 Pending 状态，但要保证并发审批 Exactly-Once，还需要更强的事务 Claim。
- 当前写权限只有管理员粒度，尚未细分为每个 Action 的 RBAC Permission。
- 两个 Action 都针对事件，不是上层可扩展的通用代码编辑框架。
- Agent Pause/Resume 不用于异步等待审批；后续请求查询 Action 结果。

#### 安全不变量

1. 模型工具调用只能创建提案，不能直接执行变更。
2. 上层 ReadToolRegistrar 不能注册、替换或删除写工具。
3. 写权限与证据选择、读工具意图选择独立。
4. 批准后只执行持久化的工具和参数。
5. Provider 或 Incident 能力失败必须可见。
6. 新变更类型必须修改并评审平台 Catalog、显式 Dispatcher Case、影响契约和幂等设计，不能通过场景配置动态注入。
