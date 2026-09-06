# QA 链路分层收敛与入口归一化重构提案

状态：草案
作者：Codex（AI coding agent）
日期：2026-09-06
关联事项：`feat/multi-agent-platform` 分支；git 提交 `03c2113`（remove QA facade package）、`698c6d4`（simplify tool/agent/multi-agent orchestration complexity）
目标版本：可选

## 1. 摘要

本提案用于解决 Nasuta QA 请求链路中“分层过厚、职责交叉、结果模型重复映射”的工程复杂度问题。

当前，一次 `POST /api/qa/ask` 请求会依次穿过
`dashboard.SSE → qa.Service → definition.Runtime → execution.Agent → run.Hub/run.Store`
五层边界，但其中至少有两层只是转发和别名：`qa` 包不再承担 workflow 晋升，`route.go` 恒返回 `single_agent`，`definition.Runtime` 同时扮演“执行引擎 + 事件总线 + 工具源 + 校验器 + 持久化协调器”五个角色，且结果数据需要经过 `agentapi.RunResult`、`execution.RunResult`、`run.Outcome` 三套高度重叠的模型来回搬运。根因不是某个 bug，而是历史演进遗留：早期“任务图规划 → workflow 晋升 → 多 agent 调查”的完整链路被逐步收敛为“父 agent + `delegate_investigation`”，但删掉的是调用关系，分层与装配没有同步删除，于是 QA 既“看起来支持 workflow”又“实际不走 workflow”。

本提案计划通过“删薄层、合并结果映射、收敛 QA 编排边界、剥离 workflow 假耦合、简化运行时装配”五步，将当前流程从：

```text
HTTP
→ dashboard.Handler.currentQARuntime()（回调 + legacy fallback 双通道）
→ qa.Service（retriever/planner/compactor/memory/router 上帝对象）
→ qa.* 别名/包装函数
→ definition.Runtime（执行 + 事件 + 工具源 + workflow 钩子）
→ execution.Agent
→ run.Hub / run.Store
→ 三套结果模型来回映射
```

调整为：

```text
HTTP/SSE（仅鉴权/解析/session/订阅）
→ QA 应用服务（仅 prepare 编排，产出 RunStart + RunRequest）
→ AgentRuntime（唯一不可变执行边界 Begin/Execute/Finish）
→ execution 循环（LLM loop/tools/answer）
→ EventBus + RunStore（单一事件/持久化事实源，单一结果归一化出口）
```

预期实现：消除 `qa/dependencies.go`、`qa/result.go`、`execution/types.go` 等纯转发薄层；把结果归一化收敛到唯一出口；让 QA 与 workflow 解耦；让运行时装配与热重载不再重复构建 runtime。最终降低 QA 链路的认知负担与维护成本，同时不改变现有对外行为、SSE 事件顺序和持久化语义。

## 2. 背景

### 2.1 业务与技术背景

QA 是 Nasuta 的知识问答入口：用户提出问题后，系统需要经过证据规划、检索、会话历史装配、答案生成等步骤，把结果通过 SSE 流式返回，并将会话轮次、运行步骤、证据与使用量持久化。同时，同一套 Agent 运行时还需要服务 feature delivery 的评审（review/adjudication）以及 incident/product-development 的 durable workflow。

当前相关链路为：

```text
POST /api/qa/ask
→ internal/transport/routes/routes.go:176（qaAskAuth）
→ internal/transport/dashboard/qa.go:74 APIQAAsk
→ internal/transport/dashboard/qa.go:272 serveAgentSSE
→ internal/agent/qa/service.go:169 Ask
→ internal/agent/qa/prepare.go:70 prepare
→ internal/agent/qa/prepare.go:274 prepareSingleRun
→ internal/agent/definition/run.go:64 Begin
→ internal/agent/definition/run.go:204 activeRun.Execute
→ internal/agent/execution/loop*.go（LLM loop / tools / answer）
→ internal/agent/run/hub.go / store*.go（事件与持久化）
→ SSE 投影 + 会话轮次持久化
```

各模块主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/transport/dashboard` | HTTP 解析、鉴权、session 读取、SSE 订阅与投影 | `*http.Request` | SSE 事件流 |
| `internal/agent/qa` | QA 场景编排：证据规划、检索、历史装配、压缩、内存、路由、Run 提交与收尾 | `qa.Request` | `*qa.AskResult` |
| `internal/agent/definition` | 不可变定义解析、RunStart/RunRequest 校验、Begin/Execute/Finish、事件发射、工具源 | `agentapi.RunStart/RunRequest` | `agentapi.RunResult`、`run.Outcome` |
| `internal/agent/execution` | 真实 LLM 循环、工具调用、答案生成、上下文压缩、委托结算 | `execution.Input` | `execution.RunResult` |
| `internal/agent/run` | 事件总线、步骤/证据/预算/生命周期持久化 | `run.Outcome`、事件 | 持久化记录、SSE 事件 |
| `internal/agent/workflow` | DAG workflow 引擎（incident/product-development 的 durable workflow） | `workflow.RunRequest` | `workflow` 结果 |

### 2.2 当前实现

相关实现主要位于：

- `internal/transport/dashboard/qa.go`（945 行）：QA HTTP/SSE 处理器，`serveAgentSSE` 订阅 hub 并调用 `qa.Service.Ask`；
- `internal/transport/dashboard/handler.go`（155 行）：`Handler` 同时持有旧字段 `qa`/`persistentRunStore`/`qaSessions`/`history`/`platform`/`writeAvailable` 与新回调 `qaRuntimeFn`；
- `internal/transport/dashboard/lifecycle.go`（66 行）：`currentQARuntime()` 优先走 `qaRuntimeFn()`，否则回退旧字段；
- `internal/agent/qa/service.go`（190 行）：`Service` 是“agent-facing runtime facade”，`New` 一次性注入 14 个依赖；
- `internal/agent/qa/prepare.go`（582 行）、`context.go`（611 行）、`submission.go`（488 行）、`compaction.go`（264 行）：QA 编排的主要实现；
- `internal/agent/qa/dependencies.go`（151 行）：大量 `type X = ...` 别名；
- `internal/agent/qa/route.go`（216 行）：执行路由，但 `executionPath` 恒为 `single_agent`；
- `internal/agent/qa/result.go`（16 行）：`outcomeFor`/`mergeOutcomeReferences` 纯转发；
- `internal/agent/definition/runtime.go`（286 行）、`run.go`（667 行）、`prepare.go`（832 行）、`result.go`（1129 行）：不可变定义执行运行时与结果归一化；
- `internal/agent/execution/types.go`（44 行）：`Registry`/`ToolPolicy`/`Observer`/`Controller` 等别名；
- `internal/agent/execution/outcome.go`（151 行）：`OutcomeFor`/`MergeOutcomeReferences`；
- `app/qa.go`（779 行）：`buildQARuntime`、`rebuildQARuntimeLocked`、catalog 复用与 workflow 接线；
- `app/server.go:130 recoverStartupRuns`：注释明确“QA requests are ordinary agent runs”。

当前执行逻辑概括如下：

1. HTTP 层解析请求、读取 session、订阅 `run.Hub`；
2. `qa.Service.Ask` 完成 prepare（plan/analyze/route/evidence/context/compaction）；
3. `definition.Runtime.Begin` 先固定 RunStart 并创建 managed run；
4. `qa.Service` 继续 acquireEvidence/compaction，然后 `submitRun` 异步调用 `run.Execute`；
5. `activeRun.Execute` 调用 `execution.Agent`，再由 `definition.result` 把结果映射为 `run.Outcome` 与 `agentapi.RunResult`；
6. `run.Hub` 把事件投递到 SSE 订阅，`run.Store` 持久化结果。

### 2.3 为什么现在需要修改

本次修改由架构演进触发：

- git 提交 `77bcfae`（simplify QA workflow and remove legacy investigation）、`03c2113`（remove QA facade package and consolidate entry points）、`698c6d4`（simplify tool/agent/multi-agent orchestration complexity）已经表明项目正在往“QA 不再走 workflow”的方向收敛；
- 但 `qa` 包仍叫 `qa` 并保留 workflow 暗示，`app/qa.go` 仍在 QA 重建流程里调用 `configureAgentWorkflowRuntime`，`definition.Runtime` 仍混入 workflow 专用事件钩子；
- `recoverStartupRuns` 的注释与 `route.go` 的注释都确认：QA 是普通 agent run，动态调查通过 `delegate_investigation` 在父 run 内完成，不创建也不等待 durable investigation workflow。

直接表现是：新加入的维护者很难判断一次 QA 请求“实际经过哪条路径”，因为多层边界、多套模型和多处“看起来可切换、实际永远固定”的分支。

### 2.4 范围与非目标

#### 目标

1. 删除 QA 链路中的纯转发薄层（别名、包装函数），让依赖关系可一眼看清；
2. 把“执行结果 → 持久化结果 → 公共结果”的归一化收敛到唯一出口；
3. 让 QA 编排与 workflow 引擎在装配层面解耦；
4. 简化 `buildQARuntime`/`rebuildQARuntimeLocked` 的构建与热重载流程，消除重复构建。

#### 非目标

1. 本提案不改变 QA 的对外 HTTP/SSE 契约、事件顺序、持久化 schema 或会话语义；
2. 本提案不重构 feature delivery 的评审执行方式（review/adjudication 仍可继续直接调用 `agentapi.Runtime.Run`）；
3. 本提案不删除 incident/product-development 真正依赖的 durable workflow 引擎；
4. 本提案不通过“提高 token/超时上限”或“针对单个入口写特例”来掩盖复杂度问题。

## 3. 问题

### 3.1 问题描述

**期望行为：**

一次 QA 请求应当在清晰的边界内完成：传输层只负责 HTTP/SSE，QA 应用层只负责编排，运行时层只负责不可变定义执行，执行层只负责 LLM 循环，事件/持久化层只负责结果落库与投递；并且结果只需要一次确定性的归一化。

**实际行为：**

- `qa.Service` 成为“上帝对象”，同时承担检索、规划、历史、压缩、内存、路由、提交；
- `definition.Runtime` 成为另一个“上帝对象”，同时承担执行、事件、工具源、workflow 钩子；
- 两个对象之间通过 `qa.Deps` 注入的 `Runtime/RuntimeTools/PhaseEmitter/ExecutionEvents` 四字段其实指向同一 `definitionRuntime`；
- `qa/dependencies.go`、`execution/types.go` 用别名制造额外的类型层；
- `qa/result.go` 包装 `execution` 的结果映射；
- `app/qa.go` 在 catalog 复用时可能重复 `buildQARuntime` 一次，且每次 QA 重建都重新把 runtime 接进 workflow 编排器。

**差异：**

实际代码的职责边界、类型层数和装配路径，远多于“单一路径”的真实行为，导致代码绕、难测试、难维护。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 维护者无法快速定位一次 QA 请求的真实调用链 | `qa`/`definition`/`execution` 三层边界 + 三套结果模型 |
| 直接原因 | 历史多 agent workflow 路径被删除后，分层、别名和装配未同步清理 | `route.go` 恒 `single_agent`；`recoverStartupRuns` 注释 |
| 机制根因 | 缺少“领域模型 / 公共 DTO / 持久化模型 / 事件总线”的明确单一所有权 | `agentapi.RunResult`、`execution.RunResult`、`run.Outcome` 字段重叠并多次映射 |

根因链路：

```text
早期：QA 需要 workflow 晋升 + 多 agent 调查
→ 引入 qa/definition/execution/run 多层边界与多态路由
→ 后续：QA 收敛为父 agent + delegate_investigation
→ 调用被删，但分层/别名/装配未删
→ 形成“绕来绕去”的薄层与假耦合
```

本问题不能只通过“删掉某个别名”或“合并某两个函数”单独解决，因为薄层是分散在 `qa`、`execution`、`definition`、`app` 多个包里的同源现象；需要以“单一事实源 + 单一边界”为目标做分层收敛，否则还会继续产生新的包装层。

### 3.3 影响

- **用户影响：** 无直接功能回归风险（本次目标是不改变行为），但复杂度会持续拖慢新功能交付；
- **业务影响：** QA 链路是核心入口，理解成本高会降低排查与迭代效率；
- **系统影响：** `rebuildQARuntimeLocked` 重复构建 runtime 造成不必要的资源与状态切换风险；
- **工程影响：** 薄层与别名让测试需要构造同时实现多接口的 fake，`qa/service_test.go`（1355 行）、`definition/result_test.go`（1321 行）等维护成本高，职责混乱。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：定位一次 QA 请求的真实执行路径

- **Given：** 开发者想确认“QA 是否走 workflow”；
- **When：** 顺着 `APIQAAsk → qa.Service.Ask → ...` 阅读代码；
- **Then：** 应当一眼看到单一执行路径与明确边界；
- **But：** 当前会看到 `qa/route.go` 的多态路由、`definition.Runtime` 的 workflow 钩子、`app/qa.go` 的 `configureAgentWorkflowRuntime` 接线，需要再读注释才能确认“其实不走 workflow”。

#### 场景 B：新增一个结果字段

- **Given：** 需要给 QA 结果增加一个新状态字段；
- **When：** 修改结果结构；
- **Then：** 应当只改一个事实源并自动投影；
- **But：** 当前需要同步修改 `execution.RunResult`、`run.Outcome`、`agentapi.RunResult`，并经过 `execution/outcome.go` 和 `definition/result.go` 多处手工映射，极易漏改。

#### 场景 C：平台设置或代码图重建

- **Given：** 用户修改影响 QA 的平台设置，或代码图被重建；
- **When：** `applyStoredPlatformSettings`/`replaceQACodeGraph` 触发 `rebuildQARuntimeLocked`；
- **Then：** 应当一次性构建候选、决定复用、发布并替换；
- **But：** 当前 `reusedCatalog` 分支会再次调用 `buildQARuntime`，同一 runtime 可能被构建两次。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 合法 QA 请求 | 多层转发后执行 `single_agent` | 单一路径直接执行 |
| 空输入或缺失字段 | 非法请求 | `normalizeRequest` 校验失败 | 保持现有拒绝行为 |
| 超时或预算耗尽 | 定义超时/预算触发 | 映射为 `partial`/`failed` | 保持现有状态分类 |
| 下游失败 | 检索/LLM 失败 | `finishRunWithError` 收尾 | 保持现有失败可见性 |
| 重试或重复请求 | runID 重复 | `RunStart` 校验拦截 | 保持现有幂等约束 |
| 并发或乱序 | 多请求并发/热重载 | `qa.mu`/`reload` 保护 | 保持现有并发安全 |
| 兼容旧数据或旧客户端 | 旧 run/会话数据 | 旧字段 fallback | 迁移后删除 fallback，保持 schema 兼容 |

### 4.3 复现步骤

1. 阅读 `internal/agent/qa/dependencies.go`、`internal/agent/qa/result.go`、`internal/agent/execution/types.go`；
2. 观察存在 `type X = ...` 别名和纯转发函数；
3. 阅读 `internal/agent/qa/route.go`，确认 `executionPath` 只有 `single_agent` 一个取值；
4. 阅读 `app/qa.go` 的 `rebuildQARuntimeLocked` 与 `configureAgentWorkflowRuntime`；
5. 可见 QA 实际单一路径，但代码仍保留多态/别名/workflow 装配。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 分层收敛按“删薄层 → 合并映射 → 收敛编排 → 解耦 workflow → 简化装配”顺序，目标是消除产生复杂度的结构，而不是针对某个入口打补丁。
2. **保持单一事实源。** `run.Outcome` 作为持久化/流式的单一终态，`agentapi.RunResult` 作为公共 DTO，`execution.RunResult` 作为执行层私有结果；归一化只允许在一个位置发生。
3. **明确职责边界。** 传输层、应用层、运行时层、执行层、事件/持久化层各只有一个所有者，禁止 pass-through getter 和别名隐藏耦合。
4. **失败可诊断。** 保留现有错误码、`Completeness`、`TerminationReason` 与结构化日志，不在归一化中引入新的静默降级。
5. **兼容与可回滚。** 阶段化提交、`GOWORK=off go test ./...` 逐步验证；每阶段保持行为等价，可在任一阶段回退。

### 5.2 目标流程

```text
[HTTP/SSE 请求]
→ [传输层：解析/鉴权/session/订阅 EventBus]
→ [QA 应用服务：plan → analyze → route(仅观测) → evidence → context → compaction]
→ [产出 RunStart + RunRequest]
→ [AgentRuntime：resolve definition → validate → Begin/Execute/Finish]
→ [execution：LLM loop / tools / answer]
→ [结果归一化唯一出口：execution.RunResult → run.Outcome / agentapi.RunResult]
→ [EventBus 投递 + RunStore 持久化]
→ [SSE 投影]
```

与当前流程相比，关键变化是：

1. 在 `internal/agent/qa` 删除别名与包装函数，依赖关系显式化；
2. 将结果归一化从 `qa`/`execution` 多处转发收敛到唯一出口；
3. 将 `definition.Runtime` 的事件发射与工具源职责剥离，运行时只保留执行与校验；
4. 在 `app/qa.go` 断开 QA runtime 与 workflow orchestrator 的装配，workflow 只服务 incident/product-development 的 durable workflow；
5. 在日志/持久化中继续公开执行路径、步骤失败、最终状态与完成度。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 删除 QA 别名 | `qa/dependencies.go` 大量 `type X = ...` | 直接 import 真实类型 | `internal/agent/qa` | 编译期替换，行为不变 |
| 删除结果包装 | `qa/result.go` 转发 `execution.OutcomeFor`/`MergeOutcomeReferences` | 调用点直接使用 execution 函数 | `internal/agent/qa` | 测试同步迁移 |
| 收敛 execution 别名 | `execution/types.go` 别名 | 合并到使用处或保留极小公共类型 | `internal/agent/execution` | 编译期替换 |
| 收敛结果映射 | `execution/outcome.go` + `definition/result.go` + `qa/result.go` 多层映射 | 唯一归一化出口 | `internal/agent/definition` | 先补对照测试再合并 |
| 收敛 QA 入口 | `qa/route.go` 多态路由 + `Deps` 四字段指向同一 runtime | 显式单一路径 + 合并 `RuntimePort` | `internal/agent/qa` | 保留降级观测事件 |
| 剥离事件/工具源 | `definition.Runtime` 暴露 `Hub`/`ToolsFor`/`Emit*` | 事件总线与工具源由 platform 注入 | `internal/agent/definition` | dashboard 改订阅注入 hub |
| 解耦 workflow | `app/qa.go` `configureAgentWorkflowRuntime` 把 QA runtime 接入 workflow | 断开 QA 装配，workflow 独立归属 | `app` | 保留 incident/product workflow |
| 简化重载 | `rebuildQARuntimeLocked` 复用分支二次 build | 先定 version，再 build 一次 | `app/qa.go` | 保持 active runtime 原子替换与旧 recovery stop |

#### 改动一：删除 QA 链路中的别名与包装层

**方案：**

- 删除 `internal/agent/qa/dependencies.go` 中 `ConversationContext`、`RunResult`、`RunOutcome`、`Tool`、`ExecutionEventEmitter` 等别名，让 `qa` 直接 import `execution`/`run`/`tool`；
- 删除 `internal/agent/qa/result.go` 的 `outcomeFor`、`mergeOutcomeReferences`，调用点直接调用 `execution.OutcomeFor`、`execution.MergeOutcomeReferences`；
- 将 `internal/agent/execution/types.go` 的别名合并到使用处，或只保留一个明确的 `execution.PublicTypes` 说明文件，但禁止 `qa` 与 `definition` 再次 alias。

**约束：**

- 不改变任何运行时行为；
- 不改变对外包导入关系（仅 `internal` 内部调整）；
- 保留测试覆盖，测试 import 改为真实类型。

**失败行为：**

- 该阶段只做编译期替换，若 `GOWORK=off go test ./...` 失败则回退该阶段；
- 不允许通过“保留旧别名”来避免修改测试。

#### 改动二：将结果归一化收敛到唯一出口

**方案：**

- 明确 `execution.RunResult` 为执行层私有结果；
- 明确 `run.Outcome` 为持久化/流式唯一终态；
- 明确 `agentapi.RunResult` 为公共 DTO；
- 让 `internal/agent/definition/result.go` 成为唯一从 `execution.RunResult` 生成 `run.Outcome` 与 `agentapi.RunResult` 的位置；
- 将 `execution/outcome.go` 的 `OutcomeFor` 保持为 `execution.RunResult → run.Outcome` 的机械映射，但禁止再被 `qa` 二次包装；
- 将 `definition/result.go` 中 `mapResult`/`mapPartialResult`/`mapFailedResult`/`mapCancelledResult`/`attemptOutputRecovery` 按“状态分类 → schema 校验 → 公共投影”拆分为三个明确函数，降低单文件复杂度。

**约束：**

- `AnswerComplete`、`FallbackUsed`、`Completeness`、`TerminationReason` 的语义与现有值必须保持一致；
- 保持 partial/failed/cancelled/succeeded 的分类规则不变。

**失败行为：**

- 归一化失败时仍返回明确的 `run.Outcome` 状态与 `agentapi.RunResult` 错误码；
- 不允许静默把失败映射为成功或把 partial 映射为 succeeded。

#### 改动三：收敛 QA 编排边界

**方案：**

- 保留 `Ask → prepare → prepareSingleRun → submitRun` 的 use-case 骨架；
- 将 `route.go` 中“永远 `single_agent`”的事实显式化：删除 `executionPath` 类型与多态分支，只保留降级/观测理由与事件发射；`route_test.go` 随之瘦身；
- 将 `qa/contracts.go` 的 `Deps` 中 `Runtime`/`RuntimeTools`/`PhaseEmitter`/`ExecutionEvents` 合并为一个 `Runtime RuntimePort`，内部按需要断言可选能力，或拆成 `ManagedRuntime` + `PhaseSink` 两个明确接口；
- 将 `runtime.go` 的 `Models/NewModels` 收编到 platform 构建处，与 `definition.NewRuntime` 一起创建，避免 QA service 再持有两个 LLM client 的间接层。

**约束：**

- 保留 route 降级理由、`execution_routed`/`execution_degraded` 事件与 `runtrace` 输出，作为可解释性信号；
- 不破坏 `qa/service_test.go` 与 `test_helpers_test.go` 的既有 fake 边界（通过接口收敛而非删除）。

**失败行为：**

- 当 runtime 未配置时，仍返回明确的 `runtime not configured` 错误，不允许静默成功。

#### 改动四：剥离 `definition.Runtime` 的事件与工具源职责

**方案：**

- 由 platform 持有并注入 `run.Bus`（或复用 `run.Hub`），`definition.Runtime` 只保留生命周期与结果，把 `EmitPhase`/`EmitStatus`/`EmitEvent`/`ProjectToolEvents` 收敛为对注入 Bus 的写；
- 将 `ToolsFor` 从 runtime 拆出为独立 `ScenarioToolProvider`；
- 将 workflow 专用钩子 `ProjectToolEvents` 下沉到 feature/workflow 专属装配（若 audit 确认 feature review 不走 agent node，则直接删除 agent node 桥）。

**约束：**

- 必须保证 hub 生命周期与 runtime 重建一致，避免热重载时 SSE 订阅落到旧 hub；
- 必须保持 `serveAgentSSE` 中“先订阅再 Ask”的事件顺序不变。

**失败行为：**

- hub 不可用时，SSE 以明确状态终止，不允许静默丢事件。

#### 改动五：断开 QA 与 workflow 的装配，简化热重载

**方案：**

- 移除 `app/qa.go` 中 `rebuildQARuntimeLocked` 对 `configureAgentWorkflowRuntime(definitionRuntime)` 的调用，让 QA runtime 不再进入 workflow orchestrator；
- 将 `buildQARuntime` 与 catalog 复用拆为两阶段：
  - 阶段 A：`buildRuntimeCandidate(settings, graph, version)` 只产出候选（runtime + definitions + capabilities），不发布；
  - 阶段 B：`publishOrReuse(candidate)` 决定复用或发布；复用判断不再需要二次 build；
- 去掉 `dashboard.Handler` 的 legacy fallback 字段（`qa`/`persistentRunStore`/`qaSessions`/`history`/`platform`/`writeAvailable`），`currentQARuntime()` 退化为简单转发；
- 收敛 `QARuntime`：从“QA + Hub + RunStore + Sessions + History + Settings + WriteAvailable + CompactionLLM”大杂烩，拆为 `QAApplication` + `RuntimeDeps`。

**约束：**

- 保持“候选未发布前不替换 active runtime”和“旧 recovery worker 正确 Stop”两个既有不变量；
- 保持 `setQARuntimeWriteAvailable` 的运行时免重建更新能力。

**失败行为：**

- 构建/发布失败时返回明确错误，不替换 active runtime；
- 复用判断失败时走全新发布路径，不允许静默复用错误版本。

### 5.4 数据结构或接口契约

新增或调整的核心接口：

| 字段/接口 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `RuntimePort` | `interface` | `internal/agent/qa` | 合并 `ManagedRuntime` 与可选 `PhaseSink` 的能力面 | 空值即未配置 | 过渡接口，逐步拆细 |
| `ScenarioToolProvider` | `interface` | platform | 为 QA prepare 提供工具快照 | 无 | 替代 `definition.Runtime.ToolsFor` |
| `EventBus` | `interface` | platform | 事件投递单一入口 | 无 | 替代 `definition.Runtime.Hub()` |
| `QAApplication` | `struct` | platform | QA 编排 + 依赖的明确聚合 | 无 | 替代 `dashboard.QARuntime` 大杂烩 |

结果状态转换（保持不变）：

```text
execution.RunResult
  ├─ 正常完成 → run.Outcome{Status: done} → agentapi.RunResult{Status: succeeded}
  ├─ 可用但未完成 → run.Outcome{Status: partial} → agentapi.RunResult{Status: partial}
  ├─ 预算/超时 → run.Outcome{Status: failed} → agentapi.RunResult{Status: failed}
  └─ 取消 → run.Outcome{Status: aborted} → agentapi.RunResult{Status: cancelled}
```

不变量：

1. `AnswerComplete`、`FallbackUsed`、`Completeness`、`TerminationReason` 在三处模型中语义一致，只在唯一归一化出口赋值；
2. `run.Outcome` 是持久化/流式的唯一事实源，`agentapi.RunResult` 是其公共投影，不允许反向派生；
3. 热重载过程中，active runtime 的替换必须原子完成，旧 recovery worker 必须在新 runtime 启动前停止；
4. 未配置 runtime 时，QA 请求必须以明确错误终止，不允许静默成功。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 本提案不改变对外 HTTP/SSE 契约、事件顺序、持久化 schema 与会话语义；所有改动限定在 `internal` 与 `app` 内部；
- **数据迁移：** 无需数据库迁移（结果状态语义保持不变）；
- **灰度方式：** 按阶段提交到同一分支，每阶段跑 `GOWORK=off go build ./...`、`GOWORK=off go test ./...`、`GOWORK=off go vet ./...`；不引入运行时 feature flag；
- **回滚条件：** 任一阶段出现编译失败、测试失败或 SSE 事件顺序回归即回滚该阶段；
- **回滚步骤：** `git revert` 对应阶段提交，恢复 active runtime 原子替换与旧 recovery worker 停止逻辑。

## 6. 修改伪代码

### 6.1 核心流程（重构后目标形态）

```go
// 传输层只负责解析、订阅与投影
func ServeAgentSSE(ctx Context, r Request) error {
    qa := runtime.QAApplication()
    events := runtime.EventBus()

    runID := NewRunID()
    sub := events.Subscribe(runID)
    defer events.Unsubscribe(runID, sub)

    result, err := qa.Ask(ctx, qa.Request{
        Question:       r.Question,
        Conversation:   conversation,
        UserID:         userID,
        RunID:          runID,
        EvidencePlan:   r.EvidencePlan,
        WriteAuthorized: writeAuthorized,
        WriteRequested:  r.WriteRequested,
    })
    if err != nil {
        EmitTerminal(sub, runID, failed(err))
        return err
    }

    for event := range sub {
        if event.Terminal() {
            ProjectTerminal(event)
            return nil
        }
        ProjectEvent(event)
    }
    return nil
}

// 应用层只做编排，产出执行边界所需对象
func (s *Service) Ask(ctx Context, req Request) (*AskResult, error) {
    prepared, err := s.prepare(ctx, req, time.Now())
    if err != nil {
        return nil, err
    }

    run, err := s.runtime.Begin(ctx, s.buildRunStart(prepared))
    if err != nil {
        prepared.Close()
        return nil, err
    }

    admitted, err := s.acquireEvidence(run.Context(ctx), prepared, run)
    if err != nil {
        prepared.Fail(run, err)
        return nil, err
    }

    runRequest := s.buildRunRequest(prepared, admitted)
    go s.executeSubmittedRun(ctx, run, prepared, runRequest)

    return &AskResult{RunID: req.RunID, Context: admitted.Retrieved}, nil
}

// 运行时层只做不可变定义执行
func (rt *Runtime) Begin(ctx Context, start RunStart) (ManagedRun, error) {
    prepared, err := rt.prepare(runRequestFrom(start))
    if err != nil {
        return nil, err
    }
    return rt.beginPrepared(ctx, start, prepared)
}

func (run *activeRun) Execute(ctx Context, req RunRequest) (RunResult, error) {
    input := compileInput(req, run.execution)
    result := run.agent.RunCompiled(ctx, run.start.RunID, input, run.execution.toolSnapshot)

    outcome := NormalizeResult(run.start.RunID, result, runErr, usage, refs)
    run.setOutcome(outcome)
    run.runtime.events.EmitTerminal(run.start.RunID, outcome)

    return outcome.PublicResult(), nil
}
```

### 6.2 关键边界处理（唯一归一化出口）

```go
// 唯一出口：execution.RunResult → run.Outcome → agentapi.RunResult
func NormalizeResult(
    runID string,
    result *execution.RunResult,
    runErr error,
    usage Usage,
    refs []Reference,
) (run.Outcome, agentapi.RunResult) {
    if result == nil {
        outcome := run.Outcome{Status: run.StatusFailed, Err: run.ErrEmptyAnswer}
        return outcome, FailedPublic(outcome, usage)
    }

    outcome := run.Outcome{
        Status:              classifyStatus(result, runErr),
        Answer:              result.Answer,
        SessionMessages:     result.SessionMessages,
        Evidence:            result.Evidence,
        References:          MergeReferences(refs, result.References),
        DelegationAdoptions: result.DelegationAdoptions,
        AnswerComplete:      result.AnswerComplete,
        FallbackUsed:        result.FallbackUsed,
        Completeness:        result.Completeness,
        TerminationReason:   result.TerminationReason,
    }

    if result.AnswerComplete || result.Completeness != "partial" {
        return outcome, SucceededPublic(outcome, usage)
    }
    return outcome, PartialPublic(outcome, usage)
}
```

### 6.3 修改前后对比

修改前（别名与二次包装）：

```go
// qa/dependencies.go
type RunResult = execution.RunResult
type RunOutcome = run.Outcome

// qa/result.go
func outcomeFor(result *RunResult, pre []Reference, err error) RunOutcome {
    return execution.OutcomeFor(result, pre, err)
}
```

修改后（直接使用唯一出口）：

```go
// qa 直接 import execution / run，不再提供别名
outcome := execution.OutcomeFor(result, pre, err)
```

修改前（route 多态暗示）：

```go
type executionPath string
const executionPathSingle executionPath = "single_agent"

type executionRouteDecision struct {
    Strategy retrieval.ExecutionStrategy
    Path     executionPath
    ...
}
```

修改后（显式单一路径 + 只保留观测理由）：

```go
type executionRouteDecision struct {
    HighRisk        bool
    RouteReason     string
    DowngradeReason string
    DecisionOrigin  string
}
```

### 6.4 配置或数据库变更

无需配置或数据库变更。结果状态语义与持久化 schema 保持不变。

```yaml
# 无新增配置
```

```sql
-- 如无数据库变更，删除此代码块。
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当合法 QA 请求进入时，仍按 `single_agent` 单一路径完成 prepare → Begin → Execute → 事件/持久化，行为不变；
2. 当异常发生时，仍以 `partial`/`failed`/`cancelled` 明确终止，失败可见；
3. 不再出现 `qa` 包内对 `execution`/`run` 类型的别名转发和 `qa/result.go` 二次包装；
4. 对 QA、feature review、incident/product workflow 三个入口，能够明确区分各自是否经过 workflow 引擎。

### 7.2 可观测性效果

新增或调整以下信号（大部分为既有信号，仅保证不回归）：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `execution_routed` | 事件 | 保留 QA 执行路径选择可解释 |
| `execution_degraded` | 事件 | 保留路由降级原因可解释 |
| `run.finished` | SSE 终端事件 | 保持终端状态与完成度一致 |
| `Completeness` / `TerminationReason` | 持久化字段 | 反映真实完成度与终止原因 |
| `[qa] runtime run ... completed` | 结构化日志 | 定位失败步骤与原因 |

日志应至少能够回答：

- 请求选择了哪条执行路径；
- 哪一步失败，以及失败原因；
- 是否发生降级或终止；
- 最终执行状态和结果完整度分别是什么；
- 哪些输出可以追溯到哪些输入或证据。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| QA 链路的转发/别名薄层文件数 | `qa/dependencies.go`、`qa/result.go`、`execution/types.go` 共约 211 行 | 0（删除或并入使用处） | 一次性 | 代码评审 |
| 结果归一化出口数量 | 3 处（execution/definition/qa） | 1 处 | 一次性 | 代码评审 |
| QA 与 workflow 装配点 | `app/qa.go:404` 每次重建都接线 | 0（QA 不再接 workflow） | 一次性 | 代码评审 |
| `rebuildQARuntimeLocked` 单次重建的 runtime 构建次数 | 复用分支最多 2 次 | 1 次 | 一次性 | 代码评审 |
| 全量测试通过率 | `GOWORK=off go test ./...` 当前基线 | 100% | 每阶段 | CI |
| SSE 事件顺序回归 | 无 | 无回归 | 每阶段 | 集成测试 |

### 7.4 不应发生的变化

- 既有正常 QA 路径的行为保持不变；
- 延迟、成本、token 或资源消耗不因本重构增加；
- 不降低结果的状态分类准确性、可解释性或失败可见性；
- 不引入针对具体入口、ID 或关键词的硬编码特例；
- 不删除 incident/product-development 真正依赖的 durable workflow 引擎。

## 8. 测试与验收

### 8.1 单元测试

- 删除别名后，`qa` 包编译并通过既有 `qa/*_test.go`；
- `execution.OutcomeFor` / `MergeOutcomeReferences` 的测试从 `qa` 迁移到 `execution` 后仍通过；
- 结果归一化唯一出口对 nil result、空 answer、partial、budget exceeded、cancelled 均返回正确状态；
- `route.go` 单一路径仍产生 `execution_routed`/`execution_degraded` 与降级理由；
- 运行时事件发射与工具源剥离后，`definition` 既有测试（`runtime_test.go`、`result_test.go`、`prepare_test.go`）保持通过。

### 8.2 集成测试

- 验证从 `POST /api/qa/ask` 到 SSE 终端的完整链路行为不变；
- 验证 feature review 的 `RuntimeReviewRunner`/`RuntimeAdjudicationRunner` 直接 `Runtime.Run` 仍正常；
- 验证 incident/product workflow 的 durable recovery 仍正常；
- 验证平台设置/代码图热重载后，active runtime 原子替换、旧 recovery worker 停止、SSE 订阅落到新 hub；
- 验证并发请求、重复 runID、超时与预算耗尽行为不回归。

### 8.3 验收标准

1. `GOWORK=off go build ./...`、`GOWORK=off go test ./...`、`GOWORK=off go vet ./...` 全部通过；
2. `qa/dependencies.go`、`qa/result.go`、`execution/types.go` 中的别名/包装被删除或并入使用处；
3. 结果归一化只保留一个出口；
4. `app/qa.go` 不再把 QA runtime 接入 workflow orchestrator；
5. `rebuildQARuntimeLocked` 单次重建不再重复 `buildQARuntime`；
6. 无 SSE 事件顺序、持久化 schema 或对外契约回归。
