# QA 链路职责收敛与运行时边界重构提案

状态：草案
作者：Codex（AI coding agent）
日期：2026-09-06
关联事项：`feat/multi-agent-platform` 分支；当前 HEAD `40a5041`（`refactor(agent): collapse QA chain thin layers and converge entry points`）；相关提交 `03c2113`、`698c6d4`、`77bcfae`
目标版本：可选

> 本提案只描述架构修改方案。本轮已完成代码盘点和验证，暂不修改生产代码。

## 1. 摘要

本提案用于解决 Nasuta QA 链路中仍然存在的职责交叉、运行时所有权不清、生命周期协调分散和薄层复生风险。当前已经完成了一轮入口收敛：QA 不再创建或等待 durable investigation workflow，旧的 QA facade/result/types 薄层已经删除，普通 QA 固定运行 parent agent loop，必要时只把 `delegate_investigation` 作为能力暴露给 parent loop。但剩余代码仍保留了较多跨层组合和重复协调。

当前一次 `POST /api/qa/ask` 请求大致经过：

```text
POST /api/qa/ask
→ routes.qaAskAuth
→ dashboard.APIQAAsk
→ dashboard.serveAgentSSE
→ qa.Service.Ask
→ qa.prepare / prepareSingleRun
→ definition.Runtime.Begin
→ definition.activeRun.Execute
→ execution.Agent / loop
→ run.Hub / run.Store
→ SSE 与会话投影
```

问题不在于调用层数本身，而在于多个层同时拥有同一条链路的部分状态：`dashboard.QARuntime` 同时聚合 QA service、Hub、RunStore、Session、History 和设置；`qa.Service` 同时负责准备、准入、提交、收尾和后置副作用；`definition.Runtime` 同时负责 definition、tool、run、事件、usage、recovery；Dashboard 还需要自行协调 `Ask()` 返回和 `run.finished` 两套信号。

本提案建议将 QA 生命周期明确拆成：

```text
传输适配
→ QA Application.Start
→ Prepare
→ Admission / Begin
→ Execute
→ Finalize
→ PostEffects
→ EventBus / RunStore / SSE 投影
```

同时保留不同结果模型各自的边界，但规定唯一的终态归一化责任；将 `RuntimePort` 拆成运行启动与场景工具源两个端口；将 `definition.Runtime` 内部拆成 compiler、executor、finalizer、recovery coordinator；将 app reload 拆成 assemble、stage、publish、activate、reconcile；让 QA 普通请求和 durable workflow 的边界在装配层可直接看懂。

预期效果是：入口更少、所有权更清晰、失败终态只有一个责任方、SSE 不再承担应用生命周期协调，后续新增功能不再依靠新的 facade、alias 或跨层 callback 来“接线”。

## 2. 背景

### 2.1 业务与技术背景

QA 是 Nasuta 的知识问答入口。用户提交问题后，系统需要完成请求规范化、查询分析、证据规划、检索、会话上下文装配、工具准入和答案生成，并通过 SSE 返回进度和终态，同时持久化运行步骤、证据、使用量、会话轮次和最终结果。

同一套 agent runtime 还被其他能力使用，但边界不同：

- feature delivery 的 review/adjudication 使用 agent runtime 生成评审结果，并可由 feature workflow 驱动；
- incident/product-development 依赖 durable workflow 的恢复、暂停和人工协作能力；
- 普通 QA 请求是普通 agent run，不创建 durable investigation workflow；动态调查是在 parent run 内通过 `delegate_investigation` 能力完成。

### 2.2 当前实现

相关实现主要位于：

- `internal/transport/routes/routes.go`：注册和鉴权 `qaAskAuth`；
- `internal/transport/dashboard/qa.go`：`APIQAAsk`、`serveAgentSSE`、session/run 查询和控制；
- `internal/transport/dashboard/handler.go`、`lifecycle.go`：保存并读取当前 `QARuntime`；
- `internal/agent/qa/service.go`：QA 服务状态、依赖装配和 `Ask` 入口；
- `internal/agent/qa/prepare.go`、`context.go`：规划、分析、历史、上下文、检索和工具准备；
- `internal/agent/qa/submission.go`：创建 RunRequest、异步执行、session/history/memory 后置处理；
- `internal/agent/qa/route.go`：根据规划结果决定是否开放 delegation capability，同时记录 route/degraded 事件；
- `internal/agent/qa/dependencies.go`：`EventSink`、`RuntimePort` 等 QA 边界；
- `internal/agent/definition/runtime.go`、`run.go`、`result.go`：definition runtime、managed run、结果和恢复逻辑；
- `internal/agent/execution/loop.go`、`outcome.go`：LLM loop 和 execution 层结果映射；
- `internal/agent/run/model.go`、`hub.go`、`store*.go`：运行终态、事件和持久化；
- `app/qa.go`：QA runtime 构建、catalog staging/publish/reuse、dynamic delegation、feature review 配置和 reload；
- `app/feature_delivery.go`：feature delivery 与 agent workflow 的配置；
- `app/server.go`：启动恢复。当前注释明确 QA 普通请求不参加 workflow startup recovery。

当前模块职责和边界如下：

| 模块 | 当前主要职责 | 当前复杂度来源 |
| --- | --- | --- |
| `dashboard` | HTTP 参数、鉴权、session 读取、Hub 订阅、SSE 投影、run 控制 | 持有跨层 `QARuntime`，并在 SSE 中协调提交信号和终态事件 |
| `agent/qa` | 规范化、规划、检索、上下文、route、RunStart/RunRequest、异步提交、session/history/memory 收尾 | `Service` 覆盖准备到后置副作用多个生命周期；`RuntimePort` 跨两个能力边界 |
| `agent/definition` | definition resolve、schema/tool 快照、Begin/Execute/Finish、事件/usage/recovery | `Runtime` 集中了编译、执行、结果、持久化和恢复协调 |
| `agent/execution` | LLM loop、tool call、答案和 execution 结果 | 结果需要被 definition 和 run 层再次归一化，但责任边界未充分显式化 |
| `agent/run` | Hub 事件投递、运行状态、步骤/证据/预算/终态持久化 | 既是终态事实源，又由上层直接读取和驱动 SSE |
| `agent/workflow` | incident/product-development durable workflow；feature review 的 workflow 能力 | 领域边界是合理的，但 app reload 和 feature review 配置容易与 QA 构建混在一起 |

### 2.3 当前 HEAD 已经完成的收敛

以下事项已经在当前 HEAD 完成，不应再作为本提案的未来目标：

1. `internal/agent/qa/result.go` 已删除；
2. `internal/agent/execution/types.go` 已删除；
3. QA legacy investigation workflow 路径已删除；
4. QA 已收敛为 parent agent loop，加上可选的 `delegate_investigation` capability；
5. `EventSink` 已合并原先重复的 phase/event emitter 边界；
6. Dashboard legacy QA fallback 已删除，`currentQARuntime()` 现在只有 callback 读取路径；
7. route 的旧 `Strategy` 字段已删除；
8. definition result 实现已拆分到独立文件；
9. `rebuildQARuntimeLocked` 当前一次 reload 只调用一次 `buildQARuntime`，catalog reuse 通过重写和复用已准备的 definition snapshot 完成；
10. QA 不参与 `recoverStartupRuns` 的 durable workflow 恢复。

本提案针对的是上述收敛之后仍然存在的结构性复杂度，而不是重复删除已经不存在的文件或路径。

### 2.4 为什么现在需要修改

前一轮重构已经证明：单纯删除旧 workflow 调用关系，可以降低行为复杂度，但如果不同时调整所有权和生命周期，复杂度会以另一种形式留下来：

- `QARuntime` 仍由 dashboard transport 定义，却由 app 组装并持有大量 agent/platform 依赖；
- QA 的准备、准入、运行、终态和后置副作用仍集中在同一个 `Service`；
- `Ask()` 的返回语义是“已提交/准备完成”，SSE 的终态语义却来自 Hub，两个契约没有在 application 层合成；
- runtime 的工具源能力和 managed run 生命周期被合并为一个 `RuntimePort`；
- app reload 仍然同时处理 settings、catalog、runtime、delegation、review、worker 和 index；
- `definition.Runtime` 仍然是较大的 facade，后续很容易继续往其中添加能力。

如果现在不明确边界，下一次新增能力很可能重新引入 wrapper、匿名 type assertion、跨层 callback 或“看起来支持多条路径”的状态字段。

### 2.5 范围与非目标

#### 目标

1. 明确 QA 普通请求的 application-level 生命周期和终态契约；
2. 将 `dashboard.QARuntime` 的跨层 ownership 移出 transport；
3. 将 QA service 拆成可独立测试的 Prepare、Admission、Execute、Finalize、PostEffects 阶段；
4. 将 `RuntimePort` 拆为 `RunStarter` 与 `ScenarioToolSource`；
5. 在不粗暴合并公共 DTO、execution 结果和持久化终态的前提下，建立唯一 finalization 责任；
6. 将 `definition.Runtime` 内部职责拆开，但暂时保留 facade 以控制改动面；
7. 将 app reload 的 assemble、stage、publish、activate、reconcile 生命周期显式化。

#### 非目标

1. 不改变 `POST /api/qa/ask` 的 HTTP/SSE 对外契约、事件名称和事件顺序；
2. 不改变 run/session/history/memory 的持久化 schema 和既有状态语义；
3. 不删除 `agentapi.RunResult`、`execution.RunResult`、`run.Outcome` 这三个天然属于不同边界的模型；
4. 不删除 feature review 使用 agent runtime 的能力；
5. 不删除 incident/product-development 真正依赖的 durable workflow；
6. 不把所有逻辑强行塞进一个“大 service”或一个全局 EventBus；
7. 不在本轮修改业务代码。

## 3. 问题

**期望行为：**

普通 QA 请求应由一个清晰的 application contract 启动，并在 Prepare、Admission、Execute、Finalize、PostEffects 之间单向流动。Dashboard 只负责传输适配和事件投影；运行时只负责 definition/run 生命周期；终态、SSE terminal 和公共结果应来自同一个 finalization 事实源。

**实际行为：**

当前普通 QA 虽然已经固定走 parent agent loop，但 Dashboard、`qa.Service`、`definition.Runtime` 和 app reload 仍分别持有同一条链路的部分生命周期。`Ask()`、Hub terminal event、`ManagedRun.Finish()`、session persistence 和 memory extraction 由不同层交叉协调。

**差异：**

真实行为已经是单一路径，代码边界却仍然表达为多个跨层组合对象和隐式契约。结果是入口看似收敛，维护时仍需要在 transport、QA、definition、execution、run、app 和 workflow 之间来回确认 ownership，新增能力容易重新产生薄层。

### 3.1 `dashboard.QARuntime` 的 ownership 仍然跨层

当前 `dashboard.QARuntime` 包含：

- `*qa.Service`；
- `*run.Hub`；
- `*llm.LLMClient`；
- `*run.Store`；
- `*memory.SessionStore`；
- `session.History`；
- `*config.PlatformSettings`；
- `WriteAvailable`。

它由 `app` 组装，通过 `qaRuntimeFn` 交给 Dashboard，再由 Dashboard 按字段拆开使用。这不是旧 fallback 双通道问题——当前 fallback 已删除——而是 transport 包仍然定义并持有跨层组合对象，导致 runtime ownership 无法从类型上判断。

### 3.2 `qa.Service` 跨越完整请求生命周期

`qa.Service` 当前同时承担：

1. 请求规范化和默认值处理；
2. evidence planning 和 query analysis；
3. 历史上下文发现与 session context 组装；
4. retrieval、memory recall、工具准入和 delegation admission；
5. `Runtime.Begin`；
6. RunRequest 构造和异步提交；
7. `managedRun.Execute`；
8. `Outcome()` 读取、session turn 持久化、history archive、memory extraction；
9. phase/status/event 投影。

这使得“准备失败”“运行失败”“终态持久化失败”“后置 memory 失败”都在一个对象里通过不同分支处理，测试需要构造大量无关依赖。

### 3.3 conversation 可能被组装两次

`prepareConversation` 先以 service 默认的 context/output reserve 调用 `assemblePreparedConversation`；`prepareSingleRun` 随后解析 definition budget，再根据 definition 的 `ContextTokens` 和 `MaxOutputTokens` 重新计算窗口。如果与 service 默认值不同，会再次调用 `reassembleConversation`。

这不是当前必然错误，但它让 preparation 结果具有隐式可变性：前面步骤看到的 conversation 与最终 RunRequest 使用的 conversation 可能不是同一个版本，也使上下文相关的调试和测试需要理解“第一次组装”和“重组”的关系。

### 3.4 `RuntimePort` 同时代表两个不相同的能力边界

当前接口为：

```go
type RuntimePort interface {
    agentapi.ManagedRuntime
    definition.ScenarioToolSource
}
```

`ManagedRuntime` 表示 Begin/Run 的生命周期；`ScenarioToolSource` 表示根据 `tool.Policy` 获取准备阶段工具快照。两者目前都由 `definition.Runtime` 实现，但“由同一个具体类型实现”不等于“应该是同一个业务端口”。继续使用组合接口会让 fake、测试和未来替换都被迫同时满足两个边界。

### 3.5 `definition.Runtime` 仍然是过大的 facade

当前 runtime 同时拥有：

- definition resolver 和 schema registry；
- tool registry、tool executor 和 scenario tool source；
- run store、usage recorder 和 Hub；
- run 编译和 `Begin`；
- execution loop 驱动和 `activeRun.Execute`；
- result mapping、终态持久化和 `Finish`；
- delegation awaiter；
- durable recovery worker 的启动、停止和 generation 管理。

该类型作为临时 facade 是可以接受的，但内部职责没有显式分组，导致新增能力很容易继续直接添加字段或方法。

### 3.6 结果模型不是“重复到可以直接删掉”，但 finalization 责任不唯一

当前存在三个层次：

- `agentapi.RunResult`：公共 API/跨 package 的 durable public outcome；
- `execution.RunResult`：execution loop 的内部结果；
- `run.Outcome`：事件、SSE 和持久化消费的终态事实。

这三个模型分别属于不同边界，不能简单合并成一个 struct。真正的问题是映射和读取责任分散：`definition` 负责部分结果归一化，`execution/outcome.go` 提供公共映射，QA submission 又通过匿名接口断言：

```go
outcomeRunner, ok := managedRun.(interface{ Outcome() run.Outcome })
```

这里的匿名 type assertion 暗示 `ManagedRun` 的完成契约不完整，QA 必须猜测 managed run 是否额外提供 durable outcome。

### 3.7 Dashboard SSE 仍然承担 application lifecycle 协调

`serveAgentSSE` 当前需要：

1. 从 runtime 取得 QA service 和 Hub；
2. 先订阅 Hub，避免丢失 prepare 阶段事件；
3. 发出 `run.started`；
4. 异步调用 `qa.Service.Ask()`；
5. 处理 Ask 返回的 retrieved context；
6. 同时等待 Ask 失败和 Hub 的 `run.finished`；
7. 在无 Hub 时使用 Ask 返回作为结束条件。

这段逻辑解决的是应用层的“启动并等待一条 QA run”问题，不是纯 transport 投影问题。将它留在 Dashboard，会让 HTTP/SSE 层知道 QA service 的提交时机、Hub 生命周期和终态来源。

### 3.8 app reload 仍然把多个生命周期绑在一起

当前 `rebuildQARuntimeLocked` 已经不会重复调用 `buildQARuntime`，但一次 reload 仍然依次处理：

- settings snapshot；
- agent definitions 和 capability catalog staging；
- catalog publish/reuse；
- definition runtime 构造；
- dynamic delegation 配置；
- feature review runtime 配置；
- active runtime 原子替换；
- 旧 recovery worker 停止、新 recovery worker 启动；
- index/platform 更新。

其中有些步骤必须在同一 reload 锁下完成，但不应继续通过一个长函数表达所有 ownership。当前应修复的是生命周期的可见性和失败边界，而不是声称存在已经修复的“双重 runtime 构建”。

### 3.9 route 的语义仍容易让读者误认为是 workflow route

当前 route 的实际行为是：

- QA 主执行路径固定为普通 parent agent loop；
- 当规划、配置和工具条件满足时，才把 `delegate_investigation` 暴露给 parent；
- 不创建、不等待 durable investigation workflow；
- 仍保留 route/degraded 事件和原因，用于观测与解释。

因此这里的对象本质上是 delegation capability admission，而不是 workflow execution route。名称和结构若不进一步收敛，维护者仍需阅读注释才能理解真实行为。

### 3.10 影响

- **维护影响：** 新成员需要同时阅读 transport、QA、definition、execution、run 和 app 才能判断一次请求的真实生命周期；
- **测试影响：** fake 需要满足跨边界接口，异步提交、终态读取和后置副作用难以分别测试；
- **故障影响：** Ask 返回、Hub 终态、Finish 失败、session 持久化失败可能由不同层观察，错误归属不够直接；
- **演进影响：** runtime reload、feature review 和 delegation 的变更容易互相影响；
- **性能风险：** conversation 重组和不必要的跨层编排会增加准备阶段的隐性成本，需通过指标确认，而不是凭经验判断。

## 4. 问题出现的场景

### 4.1 场景一：定位 QA 是否经过 workflow

- **Given：** 开发者需要判断 QA 请求是否经过 durable workflow；
- **When：** 从 `APIQAAsk` 顺着调用链阅读；
- **期望：** 可以直接看到“普通 QA run + 可选 delegation capability”；
- **当前：** 还要结合 `route.go`、`app/qa.go`、`app/feature_delivery.go` 和 `recoverStartupRuns` 的注释才能排除 workflow 误解。

### 4.2 场景二：增加终态字段

- **Given：** 增加 `TerminationReason` 或新的完整度字段；
- **When：** 修改 execution 结果、run 终态和公共结果；
- **期望：** 明确由一个 finalizer 负责状态分类和投影；
- **当前：** 需要确认 execution、definition、run 和 QA submission 各自在哪一步读取或补写该字段，容易出现事件、持久化和公共响应不一致。

### 4.3 场景三：为 QA 增加一种准备阶段工具

- **Given：** 只想给 QA 的 prepare 阶段增加工具能力；
- **When：** 修改 `RuntimePort` 或 definition runtime；
- **期望：** 只依赖 ScenarioToolSource；
- **当前：** QA service 的 runtime 端口同时要求 ManagedRuntime，测试替身也必须实现完整 run 生命周期。

### 4.4 场景四：QA 请求结束后的持久化失败

- **Given：** agent loop 已返回结果，但 session turn 持久化失败；
- **When：** `executeSubmittedRun` 读取 outcome、持久化 session、再调用 `Finish`；
- **期望：** finalization 明确区分“agent execution 已完成”和“业务后置持久化失败”；
- **当前：** QA service 需要自行读取匿名 `Outcome()` 并决定是否再次以错误调用 `Finish`，终态责任分散。

### 4.5 场景五：设置或代码图热重载

- **Given：** 平台设置或代码图变更触发 QA reload；
- **When：** 执行 `rebuildQARuntimeLocked`；
- **期望：** 候选构建失败不影响 active runtime，发布和 worker 切换有明确边界；
- **当前：** 同一长流程同时处理 catalog、delegation、feature review、active runtime 和 worker，失败回滚点不够直观。

### 4.6 可复现场景

1. 阅读 `internal/transport/dashboard/handler.go` 的 `QARuntime`；
2. 阅读 `internal/transport/dashboard/qa.go` 的 `serveAgentSSE`；
3. 阅读 `internal/agent/qa/service.go`、`prepare.go`、`submission.go`；
4. 阅读 `internal/agent/definition/runtime.go`、`run.go` 和 `result.go`；
5. 阅读 `app/qa.go` 的 `buildQARuntime` 和 `rebuildQARuntimeLocked`；
6. 画出依赖图，即可观察到“普通 QA 单一路径”与“跨层对象/多阶段责任”之间的不匹配。

## 5. 如何修改

### 5.1 修改原则

1. **先定义 ownership，再移动代码。** 不以“少几个文件”为唯一目标，每个状态和副作用必须有唯一责任方。
2. **保留合理的模型分层。** 公共 DTO、execution 内部结果和 run 终态不强行合并；只收敛映射和 finalization。
3. **传输层只做适配。** Dashboard 负责权限、参数、SSE 编码和事件投影，不负责启动/等待 QA 生命周期。
4. **QA application 持有用例流程。** Prepare、Admission、Execute、Finalize、PostEffects 由应用层按阶段编排。
5. **运行时 facade 先保留、内部拆分。** 先拆职责，再决定是否删除 facade，避免一次重构改变太多公共入口。
6. **失败必须可见。** 构建、Begin、Execute、Finalize、持久化和 recovery 失败都要有明确错误和状态，不用静默 fallback 掩盖 ownership 问题。
7. **先补契约测试再改结构。** 先锁定 SSE 顺序、终态、重复 runID、预算和热重载不变量。

### 5.2 目标流程

```text
HTTP/SSE
→ Dashboard Adapter（鉴权、解析、session 读取、订阅、投影）
→ QAApplication.Start
   → Prepare（规范化、规划、分析、检索、conversation、tool admission）
   → Admission（解析 definition、计算预算、Begin）
   → Execute（RunStarter 返回的 ManagedRun 执行）
   → Finalize（唯一完成契约、状态分类、Outcome/Public projection）
   → PostEffects（session/history/memory 等明确的异步副作用）
→ EventBus / RunStore
→ SSE projection
```

并行的其他边界保持：

```text
feature delivery → feature review runner → agent Runtime
incident/product-development → durable workflow
普通 QA → agent Run（不进入 durable investigation workflow）
```

### 5.3 改动一：把 QARuntime ownership 从 dashboard 移出

建议在 `app` 或专门的 application composition 包中定义内部 runtime bundle。Dashboard 不再定义“QA + Hub + Store + Session + Settings”的组合对象，而只依赖明确的 QA application port、event subscription port、run query/control port 和 session query port。

建议形态：

```go
type QAApplication interface {
    Start(context.Context, StartRequest) (StartedRun, error)
    CompactionStatus(string) run.SessionStatusEvent
}

type StartedRun struct {
    RunID   string
    Context *retrieval.RetrievedContext
}

type QAEventStream interface {
    Subscribe(string) (<-chan run.SSEEvent, func())
}
```

迁移时可以先保留 app 内部 bundle，再逐步让 Dashboard 接收窄接口；不要把 bundle 再换一个名字继续暴露所有依赖。

**失败边界：** application 未配置时，Start 返回明确的 service unavailable；事件流不可用时，Dashboard 输出明确终态，不伪造成功结果。

### 5.4 改动二：拆分 QA 生命周期

不要求一次性拆成五个 package，先在 `qa` 内按阶段定义内部对象和函数边界：

```text
PrepareResult
  = normalized request
  + planning/analysis
  + assembled conversation
  + retrieval context
  + tool admission
  + resolved definition/budget

AdmissionResult
  = ManagedRun
  + immutable RunStart
  + run limits

ExecutionResult
  = public execution result
  + durable outcome

PostEffects
  = session turn / history archive / memory extraction
```

建议责任：

| 阶段 | 负责内容 | 不负责内容 |
| --- | --- | --- |
| Prepare | 输入规范化、规划、检索、上下文和 capability admission | Begin、Finish、session 最终写入 |
| Admission | definition resolve、预算计算、Begin、RunRequest 固化 | 读取终态、memory extraction |
| Execute | 调用 ManagedRun.Execute | session/history/memory 业务副作用 |
| Finalize | 统一完成契约、状态分类、Finish、终态投影 | HTTP/SSE 编码 |
| PostEffects | session turn、history、memory 等可观测异步副作用 | 改写已完成的 run 事实 |

### 5.5 改动三：conversation 只组装一次

先解析 definition 及其 budget，再计算 context window 和 output reserve，之后只调用一次 conversation assembler。若 definition resolve 必须依赖前置 planning，则把“默认 budget”改为显式的 preliminary budget，并禁止在后面隐式重组；如果确实发生预算变更，必须返回一个新的不可变 `PreparedConversation`，而不是修改同一个 preparation 对象。

目标是不再出现：

```text
prepareConversation(default window)
→ resolve definition
→ compare budget
→ maybe reassemble
```

而是：

```text
prepare planning
→ resolve definition + budget
→ assemble conversation once
→ build RunRequest
```

### 5.6 改动四：拆分 RunStarter 与 ScenarioToolSource

建议把当前组合接口拆成：

```go
type RunStarter interface {
    Begin(context.Context, agentapi.RunStart) (agentapi.ManagedRun, error)
}

type ScenarioToolSource interface {
    ToolsFor(tool.Policy) definition.ScenarioToolSet
}
```

QA prepare 只依赖 `ScenarioToolSource`；QA admission/execute 只依赖 `RunStarter`。如果某个实现同时提供两者，可以在 app composition 处组合，而不在业务端口处强制组合。

`EventSink` 已经完成合并，本提案不再重新引入 phase/event 两套同义接口。

### 5.7 改动五：引入显式 completion/finalization contract

不建议删除三个结果模型，而是让 managed run 或 application finalizer 显式返回完成信息：

```go
type CompletedRun struct {
    Public  agentapi.RunResult
    Outcome run.Outcome
}

type RunCompletion interface {
    Complete(context.Context, *agentapi.RunError) (CompletedRun, error)
}
```

建议最终由 definition finalizer 负责：

1. 接收 execution result 和 execution error；
2. 分类 succeeded/partial/failed/cancelled；
3. 合并 preparation evidence、dynamic references 和 usage；
4. 生成唯一 `run.Outcome`；
5. 从 `run.Outcome` 生成 `agentapi.RunResult`；
6. 持久化并发出终态事件；
7. 返回 `CompletedRun` 给 QA application。

QA 不再通过匿名 `interface{ Outcome() run.Outcome }` 猜测 managed run 是否具备终态。

**重要约束：** `execution.RunResult` 仍是 execution 内部结果；`run.Outcome` 仍是事件/持久化事实源；`agentapi.RunResult` 仍是公共投影。变化的是映射责任和调用契约，而不是把三者粗暴合并。

### 5.8 改动六：在 definition.Runtime 内部拆分职责

先保留 `definition.Runtime` 对外 facade，在内部引入以下私有组件：

```text
RunCompiler
  - resolve definition
  - validate schema / policy
  - pin tool snapshot
  - build preparedExecution

RunExecutor
  - invoke execution.Agent / loop
  - record usage and checkpoints

RunFinalizer
  - classify result
  - merge preparation evidence
  - build run.Outcome and agentapi.RunResult
  - persist terminal state / emit terminal event

RecoveryCoordinator
  - start/stop recovery worker
  - manage generation and cancellation
```

`ScenarioToolSource` 可以作为独立的工具 provider 由 composition 层注入。暂时不要求删除 `Runtime`，但禁止新代码继续向 facade 添加与上述职责无关的字段。

### 5.9 改动七：拆分 app reload 生命周期

将当前 `rebuildQARuntimeLocked` 的内部过程整理为：

```text
assembleQARuntimeCandidate
→ stageCatalog
→ publishOrReuseCatalog
→ configureDelegation
→ configureFeatureReview
→ activateRuntimeAtomically
→ reconcileRecoveryWorker
→ publishIndexSettings
```

建议引入 app 内部 `qaRuntimeBundle`，包含 candidate、definition runtime、Hub、catalog version 和需要的 application ports，但不暴露给 dashboard。

需要保留的现有不变量：

- candidate 构建或 catalog 校验失败时，不替换 active runtime；
- active runtime 替换是原子的；
- 旧 recovery worker 在新 worker 启动前停止；
- catalog reuse 不触发第二次 runtime 构建；
- `WriteAvailable` 可在不重建整个 runtime 的情况下更新；
- feature review 和 durable workflow 的配置继续保留，但不把普通 QA 请求变成 workflow。

### 5.10 改动八：将 route 重命名为 capability admission

不再用“execution route”暗示存在多个 QA workflow。可将内部结构重命名为 `delegationAdmission` 或 `capabilityAdmission`，保留：

- 是否满足 delegation 条件；
- `delegate_investigation` 是否加入 immutable RunRequest tool scope；
- `execution_routed`、`execution_degraded` 事件；
- route reason、downgrade reason、decision origin 等可观测字段。

主执行 loop 仍然只有 parent agent loop，动态 delegation 仍是该 loop 的工具能力。

### 5.11 兼容、迁移与回滚

- **对外兼容：** HTTP/SSE、事件名、终态字段和持久化 schema 保持不变；
- **数据库迁移：** 无需迁移；
- **实施顺序：** 先补契约测试，再拆 application port，再拆 QA 生命周期，最后拆 definition/runtime 和 app reload；
- **灰度策略：** 每阶段独立提交和验证，不引入仅为重构服务的 runtime feature flag；
- **回滚条件：** SSE 事件丢失/乱序、终态不一致、旧 worker 未停止、session schema 回归或全量测试失败；
- **回滚方式：** 按阶段回退对应提交，不回退当前 HEAD 已完成的 legacy workflow 删除，除非发现明确的兼容性问题。

## 6. 修改伪代码

### 6.1 目标的 Dashboard/Application 边界

```go
func (h *Handler) APIQAAsk(w http.ResponseWriter, r *http.Request) {
    request, err := parseQAAskRequest(r)
    if err != nil {
        writeBadRequest(w, err)
        return
    }

    stream, err := newSSEWriter(w)
    if err != nil {
        writeError(w, err)
        return
    }

    conversation, err := h.sessions.LoadContext(r.Context(), request.SessionID)
    if err != nil {
        stream.Finish(failed(err))
        return
    }

    started, events, err := h.qaApplication.Start(r.Context(), qa.StartRequest{
        Question: request.Question,
        UserID:   currentUserID(r),
        Session:  conversation,
        Policy:   request.Policy,
    })
    if err != nil {
        stream.Finish(failed(err))
        return
    }

    stream.Emit("run.started", started.RunID)
    if started.Context != nil {
        stream.Emit("context", started.Context)
    }
    for event := range events.ForRun(started.RunID) {
        stream.Project(event)
        if event.Terminal() {
            return
        }
    }
}
```

Dashboard 只知道 application 的启动结果和 event stream，不再自己协调 `Ask()` channel 与 Hub terminal channel。

### 6.2 QA Application 生命周期

```go
func (app *QAApplication) Start(ctx context.Context, req StartRequest) (StartedRun, error) {
    prepared, err := app.prepare.Prepare(ctx, req)
    if err != nil {
        return StartedRun{}, err
    }

    admitted, err := app.admission.Begin(ctx, prepared)
    if err != nil {
        prepared.Close()
        return StartedRun{}, err
    }

    go func() {
        completion, executeErr := app.executor.Execute(
            admitted.Run.Context(ctx), admitted.RunRequest,
        )
        if err := app.finalizer.Finalize(
            context.WithoutCancel(ctx), admitted, completion, executeErr,
        ); err != nil {
            app.events.EmitTerminal(admitted.RunID, failed(err))
            return
        }
        app.postEffects.Enqueue(admitted, completion)
    }()

    return StartedRun{
        RunID:   admitted.RunID,
        Context: prepared.Retrieved,
    }, nil
}
```

### 6.3 唯一 finalization 出口

```go
func (f *RunFinalizer) Finalize(
    ctx context.Context,
    admitted AdmissionResult,
    result *execution.RunResult,
    executeErr error,
) (CompletedRun, error) {
    outcome := f.classifier.Classify(
        admitted.RunID,
        result,
        executeErr,
        admitted.PreparationEvidence,
    )

    if err := admitted.Run.CommitOutcome(ctx, outcome); err != nil {
        return CompletedRun{}, err
    }

    public := f.projectPublicResult(outcome, admitted.Usage)
    return CompletedRun{Public: public, Outcome: outcome}, nil
}
```

`CommitOutcome` 必须保证：终态分类、持久化和 terminal event 使用同一个 `run.Outcome`，不允许 QA 再次通过匿名接口读取并重写终态。

### 6.4 conversation 单次组装

```go
func (p *Preparer) Prepare(ctx context.Context, req StartRequest) (Prepared, error) {
    normalized := normalize(req)
    plan, err := p.plan(ctx, normalized)
    if err != nil {
        return Prepared{}, err
    }
    definition, selection, err := p.definitions.ResolveFor(p.agent, normalized.StableKey)
    if err != nil {
        return Prepared{}, err
    }

    limits := limitsFrom(definition.Budget, definition.Model)
    conversation, err := p.context.Assemble(ctx, ContextInput{
        Source:        normalized.Conversation,
        History:       plan.History,
        ContextWindow: limits.ContextWindow,
        OutputReserve: limits.OutputReserve,
    })
    if err != nil {
        return Prepared{}, err
    }

    return Prepared{Definition: definition, Selection: selection,
        Conversation: conversation, Limits: limits}, nil
}
```

### 6.5 capability admission

```go
func (s *DelegationAdmission) Admit(input AdmissionInput) AdmissionDecision {
    decision := AdmissionDecision{Origin: "server_assessment"}
    if input.WriteRequested {
        decision.Reason = "write_requested"
        return decision
    }
    if !input.SuggestionWantsFanout {
        decision.Reason = "single_agent_suggestion"
        return decision
    }
    if !input.ToolReady || input.MaxConcurrent < 2 || input.ParallelTasks < 2 {
        decision.Reason = "delegation_unavailable_or_not_worthwhile"
        decision.Degraded = true
        return decision
    }
    decision.EnableDelegationTool = true
    decision.Reason = "parent_dynamic_delegation"
    return decision
}
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 普通 QA 请求仍然执行 parent agent loop，不创建或等待 durable investigation workflow；
2. delegation capability 仍按现有条件准入，route/degraded 事件继续可观测；
3. session、history、memory 等后置副作用不改变对外结果语义；
4. feature review 和 incident/product-development 的 workflow 能力继续保留；
5. runtime 未配置、Begin 失败、Execute 失败、Finalize 失败都会产生明确的失败终态。

### 7.2 可观测性效果

需要保持或明确以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `run.started` | SSE | 标识 application 已接受 run |
| `execution_routed` | run event | 记录 delegation admission 的理由 |
| `execution_degraded` | run event | 记录降级或 capability 未启用原因 |
| `run.finished` | SSE/run event | 由唯一 finalization 结果驱动 |
| `Completeness` / `TerminationReason` | 终态字段 | 区分完成、部分完成、预算/超时和 provider failure |
| `runtime run completed` | 结构化日志 | 记录 execution 与 post-effects 的边界 |

日志和运行记录应能够回答：

- 请求是否启用了 delegation capability，以及原因是什么；
- failure 发生在 Prepare、Begin、Execute、Finalize 还是 PostEffects；
- `run.Outcome`、SSE terminal 和 `agentapi.RunResult` 是否来自同一次 finalization；
- active runtime 切换时旧 recovery worker 是否停止；
- conversation 使用的 definition budget 是什么。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | --- | --- | --- | --- |
| QA transport 直接依赖的跨层 runtime 字段数 | `QARuntime` 8 个聚合字段 | 仅保留窄 application/event/query ports | 一次性 | 代码评审 |
| QA lifecycle 中负责终态的代码路径 | `Execute`、QA submission、`Finish` 多处协作 | 1 个明确 finalization owner | 一次性 | 代码评审 |
| QA → workflow 的业务调用路径 | 普通 QA 不进入 durable workflow；feature/workflow 仍独立 | 边界可由调用图直接验证 | 一次性 | 代码评审 |
| conversation 组装次数 | 当前可能 1 次或按 definition budget 二次组装 | 正常路径 1 次 | 每次请求 | trace/单测 |
| `RuntimePort` 所需能力面 | ManagedRuntime + ScenarioToolSource | 分离为两个窄端口 | 一次性 | 类型检查/代码评审 |
| 全量测试通过率 | 当前基线 100% | 100% | 每阶段 | CI |
| SSE 终态事件丢失或乱序 | 当前无已知回归 | 0 | 每阶段 | 集成测试 |

### 7.4 不应发生的变化

- 不改变正常 QA 请求的答案、状态、事件名称和事件顺序；
- 不把 partial、failed、cancelled 静默映射为 succeeded；
- 不让失败的 candidate 替换 active runtime；
- 不让旧 recovery worker 在新 worker 启动后继续消费；
- 不删除 feature review 或 durable workflow 的真实能力；
- 不通过新增全局容器、万能 callback 或兼容 alias 重新制造薄层。

## 8. 测试与验收

### 8.1 当前基线验证

本轮分析已完成：

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

两项均通过。提案实施后还需补跑：

```bash
GOWORK=off go build ./...
GOWORK=off go test -race -count=1 ./...
```

### 8.2 单元测试

- `Prepare` 在默认 budget 和 definition budget 下只生成一个最终 conversation；
- `DelegationAdmission` 在 write requested、single-agent suggestion、tool unavailable、并发度不足、任务不足和可 delegation 条件下返回正确决策；
- `RunFinalizer` 对 nil result、空答案、partial、budget exceeded、deadline、cancelled、provider error 返回正确 `run.Outcome` 和 public result；
- finalizer 对 evidence/reference/usage 的合并只执行一次；
- `RunStarter` fake 不需要实现 ScenarioToolSource，ScenarioToolSource fake 不需要实现 ManagedRun 生命周期；
- runtime 未配置、Begin 失败、Finish 重复调用和重复 runID 都有明确错误；
- app reload 在 candidate、catalog、worker 或 feature review 配置失败时不替换 active runtime。

### 8.3 集成测试

- 从 `POST /api/qa/ask` 到 SSE terminal 的完整链路；
- 验证“先订阅再启动”不会丢失 prepare/retrieval 早期事件；
- 验证 Ask 返回的 context 和 terminal event 的顺序；
- 验证 Execute 成功但 session persistence 失败时的终态和日志；
- 验证 execution result、run.Outcome、public RunResult 的字段一致性；
- 验证 feature review 仍可通过 `RuntimeReviewRunner`、`RuntimeAdjudicationRunner` 工作；
- 验证 incident/product-development workflow 的 durable recovery 不受 QA 重构影响；
- 验证设置/代码图热重载下 active runtime 原子替换、旧 worker 停止、新 worker 启动；
- 验证并发请求、超时、预算耗尽、取消、重复 runID 和 SSE 客户端提前断开。

### 8.4 验收标准

1. `GOWORK=off go build ./...`、`GOWORK=off go test ./...`、`GOWORK=off go vet ./...` 和必要时的 race test 全部通过；
2. Dashboard 不再依赖跨层 `QARuntime` 大聚合对象，而是依赖窄 application/event/query ports；
3. QA application 具有明确的 Start/StartedRun 契约，Dashboard 不再协调 Ask channel 与 terminal channel；
4. conversation 正常路径只组装一次；
5. `RunStarter` 与 `ScenarioToolSource` 已分离；
6. 不再通过匿名 `Outcome()` type assertion 获取 durable final outcome；
7. `definition.Runtime` 的 compiler、executor、finalizer、recovery coordinator 责任可独立测试；
8. app reload 的 assemble、stage、publish、activate、reconcile 失败边界和回滚条件有测试；
9. 普通 QA 仍不进入 durable investigation workflow，feature review 和 incident/product workflow 不回归；
10. 无 SSE 事件顺序、终态字段、持久化 schema 或会话语义回归。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 事件订阅时机改变 | application Start 后才订阅 | 丢失早期 phase/retrieval 事件 | Start 前绑定 run event stream，或由 application 返回已绑定 stream | 任一早期事件丢失 |
| finalization 语义改变 | 将 Finish、Outcome、public projection 重排 | terminal 状态不一致 | 先建立字段矩阵和 golden tests，再迁移调用点 | 任一状态映射变化 |
| runtime reload 竞态 | active runtime 与 Hub/worker 切换不同步 | 请求落到旧 Hub 或 worker 重复消费 | 保持 reload lock、原子发布和 worker stop-before-start 不变量 | 并发/热重载测试失败 |
| feature workflow 误解绑 | 将 QA 与所有 workflow 一并拆除 | feature review 或 incident 流程不可用 | 只移除普通 QA 的 durable workflow 语义，保留 feature/incident 装配 | workflow recovery 或 review 回归 |
| PostEffects 失败被隐藏 | 异步化后只记录日志 | session/history/memory 数据不一致 | 为 post-effects 保留独立状态、日志和重试策略 | 无法定位或重复写入 |
| 过度抽象 | 新增 generic container、万能 port 或 facade | 复杂度再次上升 | 每个新增接口必须对应独立生命周期或能力边界 | 代码评审无法说明 ownership |

## 10. 实施计划

### 阶段 0：基线与契约冻结

- 补齐 QA → SSE 的端到端生命周期测试；
- 固化事件顺序、终态字段、重复 runID、预算和热重载不变量；
- 退出条件：基线测试稳定通过。

### 阶段 1：application port 与 Dashboard 迁移

- 定义 `QAApplication.Start`、`StartedRun` 和 event stream contract；
- 将 `QARuntime` bundle 移入 app composition；
- Dashboard 只保留 transport、SSE 和 query/control adapter；
- 退出条件：SSE 集成测试无行为变化。

### 阶段 2：QA 生命周期拆分

- 拆出 Prepare、Admission、Execute、Finalize、PostEffects 内部边界；
- 先保留旧 Service 作为 facade，再逐步减少其字段和分支；
- 消除 conversation 隐式二次组装；
- 退出条件：单元测试覆盖每个阶段的成功/失败路径。

### 阶段 3：runtime 与结果契约收敛

- 分离 `RunStarter` 与 `ScenarioToolSource`；
- 引入显式 completion/finalization contract；
- 在 definition.Runtime 内拆 compiler、executor、finalizer、recovery coordinator；
- 退出条件：删除 QA 匿名 outcome type assertion，结果字段一致性测试通过。

### 阶段 4：app reload 生命周期拆分

- 拆分 assemble、stage、publish/reuse、activate、reconcile；
- 增加失败回滚和 worker 切换测试；
- 明确 feature review、dynamic delegation、durable workflow 的独立装配责任；
- 退出条件：热重载、并发和 recovery 回归通过。

### 阶段 5：清理与文档同步

- 删除只剩转发意义的内部函数和字段；
- 更新调用链文档、测试 fake 和维护指南；
- 退出条件：全量 build/test/vet/race 按适用范围通过，代码评审确认没有新薄层。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| `QARuntime` 替代方式 | 在 app 内定义 bundle，再向 transport 注入窄 port | 直接把 dashboard Handler 改成持有多个 callback | A | 保留 app ownership，避免 callback 数量爆炸 |
| SSE 启动/订阅契约 | `Start` 返回已绑定 event stream | Dashboard 先拿 runID 再单独订阅 | A | 能保持 prepare 早期事件不丢失 |
| 结果模型 | 合并成一个 struct | 保留三层模型，收敛 finalization | B | 三个模型分别服务公共、执行和持久化边界 |
| `definition.Runtime` | 一次性删除 facade | 保留 facade，内部拆私有组件 | B | 降低迁移风险，先改变 ownership 再删除入口 |
| route 命名 | 保留 execution route | 改为 capability/delegation admission | B | 与当前“parent loop + optional delegation”真实行为一致 |
| PostEffects 失败语义 | 直接改写已完成 run 为 failed | 保留 execution terminal，单独记录 post-effect failure | B | 不污染 agent execution 的事实终态，便于重试和诊断 |

## 12. 决策摘要

本提案建议：

1. 不再继续围绕已经删除的 QA facade、legacy investigation workflow 和重复 alias 做重构；
2. 将 `QARuntime` ownership 从 dashboard transport 移回 app/application composition；
3. 用 `QAApplication.Start` 统一 QA 启动契约，让 Dashboard 不再协调两套完成信号；
4. 将 QA 生命周期拆成 Prepare、Admission、Execute、Finalize、PostEffects；
5. 保留三种结果模型，但把最终状态分类、持久化和公共投影收敛到一个 finalizer；
6. 拆分 `RunStarter`/`ScenarioToolSource`，并在 definition facade 内拆 compiler/executor/finalizer/recovery；
7. 保留 feature review 和 incident/product-development 的 workflow 能力，只明确普通 QA 不进入 durable workflow；
8. 通过阶段化契约测试、热重载测试和全量验证控制回归风险。

## 13. 提案提交前检查清单

- [x] 背景足以让非原作者理解 QA、agent runtime 和 workflow 的关系；
- [x] 问题以期望行为、实际行为和差异描述；
- [x] 已包含可复现的典型场景和边界场景；
- [x] 已区分当前 HEAD 已完成事项与本提案未来改动；
- [x] 已核对 QA 不创建 durable investigation workflow；
- [x] 已核对当前 `currentQARuntime()` 只有 callback，没有 legacy fallback；
- [x] 已核对 `rebuildQARuntimeLocked` 当前不会重复调用 `buildQARuntime`；
- [x] 已通过 `GOWORK=off go test ./...`；
- [x] 已通过 `GOWORK=off go vet ./...`；
- [x] 补齐 QA → SSE 端到端生命周期契约测试；
- [x] 将 `QARuntime` 从 dashboard transport ownership 中移出；
- [x] 引入 `QAApplication.Start` / `StartedRun` contract；
- [x] 拆分 QA Prepare、Admission、Execute、Finalize、PostEffects；
- [x] 消除 conversation 二次组装；
- [x] 拆分 `RunStarter` 与 `ScenarioToolSource`；
- [x] 引入显式 Run completion/finalization contract，移除匿名 `Outcome()` type assertion；
- [x] 在 `definition.Runtime` 内拆分 compiler、executor、finalizer、recovery coordinator；
- [x] 拆分 app reload 的 assemble、stage、publish、activate、reconcile 生命周期；
- [x] 将 QA route 重命名或收敛为 delegation capability admission；
- [x] 补充并发、超时、预算耗尽、重复 runID、热重载和 PostEffects 失败回归测试；
- [x] 完成全量 build/test/vet/race 验收并确认 SSE、持久化 schema、状态语义无回归。
