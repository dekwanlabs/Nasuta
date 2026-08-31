# Investigation 并行预算饥饿、错误码保真与证据抢救提案

状态：已实施并归档
作者：Nasuta Agent Platform Team
日期：2026-08-27
关联事项：`run_inv_46257b6b`、`investigate.core_businesses_detail.code.1`、`investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`
目标版本：已落地，待发布

> 本提案聚焦一个已确认的运行时故障闭环：并行 Investigator 争用单一 Run 输出预算，后入场且上下文较大的 Task 被压缩到不可用的模型输出预算；随后预算错误被错误分类，已经产生的 evidence 又未能投影为 partial。它补充“共享预算与大上下文治理提案”，不替代后者对预算语义、默认值和配置迁移的总体约定。

## 1. 摘要

本提案用于解决 Investigation 多 Agent 并行执行中的预算饥饿、预算错误分类失真和失败路径证据丢失问题。

当前，Coordinator 为多个并行 Investigator 建立一个 Run 级共享预算。每个模型调用都从同一池中预留输出额度，但并行 Task 没有受保护的最低调用预算。于是先发起请求的 Task 可能消耗大部分共享预算，后发起且输入上下文更大的 Task 只能获得极小的 effective output；reasoning 模型会把这部分额度主要用于不可见 thinking，无法产出 report。若该 Task 后续在 turn budget gate 处触发 `ErrBudgetExceeded`，结果映射还会把它归类为 `execution_failed`，并在错误投影路径中丢弃此前工具调用已经产生的 evidence。

本提案计划通过三项机制修复：

1. 让 `ErrBudgetExceeded` 在 Investigation、Agent runtime 和应用包装层保持稳定的 `budget_exhausted` 语义；
2. 让成功工具调用产生的 admissible evidence 在每次观察时就进入结果状态，并在预算失败时继续完成 evidence projection，以便返回有边界的 partial；
3. 在不扩大 Run 硬上限的前提下，为并行 Task 建立受保护的层级化 reservation，必要时排队或降低并发，而不是启动一个注定只能获得不可用输出预算的 Task。

目标流程从：

```text
冻结 Run 共享预算
→ 并行 Task 直接争用同一可用余额
→ 先到 Task 消耗余额，后到 Task 被压缩到极小 output
→ 下一轮预算 gate 抛出 ErrBudgetExceeded
→ 错误被归类为 execution_failed，已有 evidence 丢失
```

调整为：

```text
创建 Run 时冻结不可扩张的硬上限
→ 并行 admission 先保护每个可运行 Task 的最低调用预算
→ 物理模型调用同时受 Task reservation 与 Run ledger 约束
→ 工具 evidence 及时写入并在失败路径继续投影
→ budget_exhausted 按 artifact 完整度返回 partial 或 failed
```

预期实现公平且可解释的并行准入、准确的失败诊断，以及“有证据则可 partial、无证据才 failed”的可靠结果闭环，同时继续保证所有物理模型调用不超过 Run 的累计硬上限。

## 2. 背景

### 2.1 业务与技术背景

Investigation 用于回答需要跨代码、服务拓扑、文档和运行证据进行推理的复杂问题。Coordinator 将问题拆分为多个带 capability 的 Task，Scheduler 并行运行 Investigator，随后由 evidence verifier 和上层 composer 汇总结果。

当前相关链路为：

```text
Investigation request
→ Coordinator 创建并冻结 Run policy / BudgetLedger
→ Scheduler 并行 admission Investigator Task
→ Agent runtime 执行 reason → tool → observe turns
→ 工具结果写入 evidence observations
→ Verifier / composer 消费 Task 结果与 evidence
→ mapResult / application runner 生成外部结果
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/agent/investigation/coordinator.go` | 创建 Run、构建 policy、初始化共享 ledger、编排阶段 | 调查计划、平台设置 | Run 与阶段执行结果 |
| `internal/agent/investigation/budget.go` | 管理预算向量、reservation、usage settlement 和 gate | 预算请求、实际 usage | 预算快照或预算错误 |
| `internal/agent/investigation/scheduler.go` | 并行 Task admission、attempt reservation 和执行调度 | Task、共享 ledger | Task execution result |
| `internal/agent/execution/model_call.go` | 计算 effective output、执行 provider 调用并登记 usage | messages、tools、max tokens、gate | 模型响应与调用 usage |
| `internal/agent/execution/loop_turn.go` | 驱动单轮模型调用、工具执行和 evidence observation | Agent state、tool execution | 下一轮状态或 runtime error |
| `internal/agent/investigation/runtime_executor.go` | 将调查 Task 投影为 Agent run，并生成 Investigator evidence candidates | Task contract、Agent result | Investigator result / candidates |
| `internal/agent/definition/result.go` | 将 Agent run error 和 outcome 映射为稳定结果状态与 failure code | run result、error | 对外结果分类 |
| `app/investigation_runner.go` | 应用层包装 Investigation 结果和错误 | Nasuta Investigation result | API / workflow result |

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/investigation/coordinator.go:841-896`：根据 policy 建立 `NewBudgetLedger`，并将 stage 的 Input/Output/Total 限制统一设置为 Run limit；
- `internal/agent/investigation/budget.go`：实现 `BudgetLedger`、`reservationBudgetGate` 和 `ErrBudgetExceeded`；
- `internal/agent/investigation/scheduler.go:256`：Task admission 使用 `ReserveAdmission(..., soft)`；`agentAdmissionGrant` 在当前路径将 token 维度清零，使 admission 对 token 资源仅具有 advisory 语义；
- `internal/agent/execution/model_call.go:38-52`：`callModel` 先检查预算，再由 `limitModelOutput` 根据可用 reservation 压缩请求的 output 上限；
- `internal/agent/execution/loop_turn.go:350-356`：目前只在满足特定成功 execution 条件时追加 `EvidenceObservations`；
- `internal/agent/definition/result.go:212+`：`mapResult` 对部分 tool-call / run limit 错误有特殊映射，但没有完整识别 Investigation 的 `ErrBudgetExceeded`；
- `internal/agent/investigation/runtime_executor.go:499-516`：`result.Error != nil` 时直接返回，未必经过 Investigator evidence projection；
- `app/investigation_runner.go`：应用层可能再次包装或映射 Investigation failure，必须与内部分类保持一致。

当前关键语义是：Run 级 output hard limit 为共享池；单次模型调用基线为 `AnswerMaxTokens=3000`。并行 Task 并未在启动前取得自己的受保护 token reservation，实际可用额度由并发调用的先后顺序决定。

### 2.3 为什么现在需要修改

本次修改由一条已完成代码和日志梳理的失败运行触发：

- 触发标识：`run_inv_46257b6b`；
- 失败节点：`investigate.core_businesses_detail.code.1`；
- executor：`investigator.code`；
- 并行兄弟：`investigator.docs`、`investigator.runtime`；
- 直接表现：前两个 Investigator 的 step 1 各预留约 3000 output，code Investigator 最终只获得 619 effective output；
- 上下文差异：code Task 的输入约 12117 tokens，context 约 28287 字符，明显高于兄弟 Task；
- 后续表现：step 1 只完成两个工具调用，step 2 的 `ensureTurnBudget` 触发 `ErrBudgetExceeded`；
- 结果表现：日志中出现 `steps=1 answerLen=0 aborted=false err=<nil>`，外部结果为 `execution_failed`，step 1 的 `search_runbooks` evidence 未能形成 partial。

该问题已经超出单个业务问题或单个节点的范围。任何并行数量增加、Task 输入不均衡或 reasoning 模型需要较多隐藏输出的 Investigation 都可能触发同一机制缺陷。

### 2.4 范围与非目标

#### 目标

1. 让 Investigation 预算错误在所有结果映射层保持 `budget_exhausted` 的稳定语义；
2. 让预算失败前已经产生的 admissible evidence 可被安全投影，并按完整度生成 partial 或 failed；
3. 让并行 Task 在 admission 时获得受保护的最低调用预算，且共享 Run 硬上限不可被任务数量、retry 或 replan 放大；
4. 让预算准入、effective output、usage settlement 和终端日志能解释同一个资源事实；
5. 以确定性测试覆盖并发乱序、上下文大小差异、预算耗尽和 evidence salvage。

#### 非目标

1. 本提案不重新设计检索排序、BM25、evidence dedup 或 Synthesizer 的内容质量算法；
2. 本提案不针对 `run_inv_46257b6b`、`core_businesses_detail`、特定服务名或问题关键词增加分支；
3. 本提案不通过按 Agent 数量扩大 Run limit、无限提高 token、无限重试或延长 timeout 掩盖预算分配缺陷；
4. 本提案不自动替换已配置的 LLM provider/model；
5. 本提案不把 partial evidence 当成已验证的最终答案；
6. 本提案不引入仅用于表示一次性执行进度的新持久化 state machine。

## 3. 问题

### 3.1 问题描述

**期望行为：**

Run 创建时冻结一份可审计的共享硬预算。每个并行 Task 在启动前都应获得满足其最小可用模型调用的受保护 reservation；Task 可以使用释放后的闲置容量，但不能侵占已 admission 兄弟的最低保障。物理模型调用必须同时遵守 Task reservation 和 Run ledger。若后续调用无法准入，之前成功工具调用产生的 evidence 仍应被保留，结果应准确反映 `budget_exhausted` 以及 report/artifact 的完成度。

**实际行为：**

所有并行 Investigator 直接争用同一个 Run 级可用余额。先发请求的 Task 预留 output 后，后发请求的 Task 通过 `limitModelOutput` 获得被动压缩的 effective output。对于 reasoning 模型，极小 output 可能只够隐藏 thinking 和少量 tool call，无法形成可见 report。预算错误在结果映射中落入 generic runtime failure；错误路径又绕过 Investigator evidence projection，导致已产生的 evidence 被丢失。

**差异：**

系统把“Task 已被 admission”误认为“Task 一定拥有可用的模型调用预算”，又把“后续模型调用预算耗尽”误认为“Task 没有任何可恢复产物”。这同时破坏了资源公平性、错误分类保真和 evidence 结果契约。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | code Investigator step 1 结束后无 report，整体 workflow `execution_failed` | 运行日志 `run_inv_46257b6b` |
| 直接原因 | 并行兄弟先预留共享 output，code Task 的 effective output 从 3000 被压缩到 619；下一轮 gate 无法继续 | `model_call.go`、`scheduler.go`、日志中的 `shrinking model output budget` |
| 机制根因一 | Run 级共享预算没有 per-task 受保护配额，admission 与真实 token 资源脱节 | `coordinator.go`、`scheduler.go` 的 shared ledger / soft admission 路径 |
| 机制根因二 | 模型单次 output、Run 累计预算、Task 工作量和 context/evidence 边界的语义混用 | `coordinator.go` 的统一 limit 设置、`limitModelOutput` |
| 机制根因三 | `ErrBudgetExceeded` 未在 `mapResult` 中优先映射，generic runtime failure 覆盖了预算错误 | `definition/result.go` |
| 机制根因四 | budget error path 直接返回，未回退到 `projectInvestigatorEvidence` 抢救已有候选 evidence | `runtime_executor.go` |
| 观测缺陷 | 终端日志打印 `state.result.Err` 而非 `runTurns` 返回错误，形成 `err=<nil>` 假成功信号 | `loop_execution.go` / loop finalization path |

根因链路：

```text
并行 Task 同时进入调度
→ 共享 Run 余额按请求先后被预留
→ 上下文较大的后入 Task 获得过小 effective output
→ reasoning 输出耗尽但没有 report
→ 下一轮 gate 抛出 ErrBudgetExceeded
→ 结果映射退化为 execution_failed
→ 错误投影短路，已有 evidence 被丢弃
→ 可恢复的部分结果变成不可闭环的硬失败
```

本问题不能只通过提高 `DefaultInvestigationMaxOutputTokens`、增大模型 context、调整并发启动顺序或对单个 Task 重试解决，因为这些方式不能保证并行公平性、保留 evidence，也不能修复错误码和失败路径的语义。Run 硬上限仍需存在，且必须在共享 ledger 中可验证。

### 3.3 影响

- **用户影响：** 已经找到运行手册或其他证据的调查仍可能返回不可解释的硬失败，无法获得部分结论；
- **业务影响：** 复杂调查成功率、降级率和成本不可预测，管理员无法据错误码判断是预算不足还是 provider/runtime 故障；
- **系统影响：** 并行请求可能 over-reserve 或饥饿，已有 evidence 在错误路径丢失，父级依赖被错误地标记为 blocked；
- **工程影响：** `answerLen=0` 与 `err=<nil>` 误导排障，测试无法只凭稳定 failure code 区分预算、工具和 provider 问题。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：并行 Investigator 争用共享 output

- **Given（前置条件）：** Run output hard limit 为 16000，三个 Investigator 并行执行，单次模型调用请求 `AnswerMaxTokens=3000`；
- **When（触发行为）：** docs、runtime 先完成 admission 并发起 step-1 请求，code Task 最后发起请求且输入 context 更大；
- **Then（期望结果）：** code Task 仍拥有受保护的最小可用 output reservation，或者在无法保障时被排队，不能被静默压缩到不可用额度；
- **But（当前结果）：** code Task 只得到 619 effective output，reasoning 只完成少量 tool call，后续预算 gate 失败。

示例输入与关键观测：

```text
run output limit: 16000
requested max output: 3000
code input tokens: 12117
code context chars: 28287
effective output: 619
```

当前执行路径：

```text
Coordinator shared ledger
→ docs/runtime 先 ReserveCall
→ code 调用 limitModelOutput
→ effective output = 619
→ step 1 tool calls
→ step 2 ensureTurnBudget
→ ErrBudgetExceeded
```

#### 场景 B：预算失败前已有 admissible evidence

- **Given：** Investigator 的 step 1 成功执行 `search_runbooks` 并返回 evidence manifest，但下一轮模型调用无法通过 budget gate；
- **When：** `RunCompiled` 以 error 返回；
- **Then：** evidence observations 和 Investigator candidates 仍被投影，Task 返回 `partial + budget_exhausted`；
- **But：** 当前错误分支在 `result.Error != nil` 时提前返回，evidence candidate 为空，Task 被视为失败。

#### 场景 C：预算不足且没有可用产物

- **Given：** Task 在首个模型调用前就无法完成 admission，或工具没有返回 admissible evidence；
- **When：** 共享 Run ledger 拒绝调用；
- **Then：** Task 返回 `failed + budget_exhausted`，不能伪造 partial，也不能把错误映射成 `runtime_failed`。

#### 场景 D：非预算错误回归

- **Given：** provider 返回配置错误、网络错误或明确的 provider failure，且 Task 可能已有或没有 evidence；
- **When：** runtime 返回非 `ErrBudgetExceeded` 错误；
- **Then：** 保持 provider/runtime failure 的原有分类和 wrapped cause，不得误标为 `budget_exhausted`；有 admissible evidence 时按既有 partial contract 处理，但不能吞掉 provider failure。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常并行 | 多个 Task，Run 余额充足 | 共享池先到先得 | 所有 Task 获得最低保障，闲置容量可借用 |
| 上下文不均衡 | 一个 Task 输入显著更大 | 后入 Task 被压缩 | admission 与 context projection 可解释，不能侵占兄弟最低保障 |
| 预算耗尽但有 evidence | 工具成功后下一轮 gate 失败 | evidence 丢失，整体 failed | `partial + budget_exhausted`，保留 candidates |
| 预算耗尽且无 evidence | 首调用前即拒绝 | 可能 generic failure | `failed + budget_exhausted` |
| provider failure | 配置、网络或响应错误 | 不稳定地混入 usage limit | 保留 provider/runtime 分类与 wrapped cause |
| 预留未用完 | Task 提前结束 | 余额可能长期被占用 | terminal 时释放差额 |
| 并发 reserve/settle | 多个调用同时结算 | 有 overbook 风险 | ledger 原子维护不变量，累计 usage 不超过 Run limit |
| retry/replan/resume | 产生新 attempt 或恢复旧 Run | 可能重复占用或扩张 | 复用冻结 Run ledger，不能创造额外 hard budget |
| sibling evidence | 多 Task 同时产出 evidence | 可能被错误归并 | evidence 只归属产生它的 Task，按 contract 投影 |

### 4.3 复现步骤

1. 使用 Investigation workflow 创建包含 docs、runtime、code 三个并行 Investigator 的计划；
2. 将 Run output hard limit 设为 16000，将单次 `AnswerMaxTokens` 设为 3000，并让 code Task 带入明显更大的 `input_refs`；
3. 让三个 Task 同时启动，控制或记录其模型请求发起顺序；
4. 观察 `requested max output`、`effective output`、`input_tokens`、reservation 和 Run remaining；
5. 在某个 Task 成功返回 evidence 后，让下一轮 admission 触发 `ErrBudgetExceeded`；
6. 检查 Task result、failure code、evidence candidates、父级依赖状态和终端日志；
7. 重复改变启动顺序与上下文大小，确认结果不再依赖 first-wins 竞态。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 预算公平性由通用 reservation 和 scheduler 规则保证，不读取 task name、query token 或具体 evidence 内容来调预算。
2. **Run hard limit 单一事实源。** Run 创建时冻结累计硬上限，Task reservation 是其子资源，不得扩张父级总额。
3. **Admission 必须有资源含义。** 已 admission 的 Task 至少拥有一次可用模型调用所需的受保护预算；做不到时排队或减少并发。
4. **物理调用统一预留和结算。** provider 请求之前原子预留，返回后按实际 usage 结算并释放未使用 reservation。
5. **成功产物与最终完成度分离。** report 是否完成不能决定之前 evidence 是否存在；partial 只表示产物存在但流程未完整。
6. **错误分类优先保真。** 使用 `errors.Is` 识别 canonical sentinel，保留 wrapped cause，禁止 generic runtime error 覆盖预算边界。
7. **可观测但不泄露。** 记录资源数值和标识，不记录完整 prompt、代码上下文或敏感工具 payload。

### 5.2 目标流程

```text
Run 创建
→ 冻结 Run BudgetPolicy 与共享硬上限
→ Scheduler 按并行组计算并保护 Task 最低 reservation
→ 无法保障时排队/降低并发，不发起不可用调用
→ model_call 在 Task + Run 两层 gate 中预留
→ provider 返回后按 usage settlement，释放 reservation 差额
→ 每次成功工具调用立即追加 evidence observation
→ 后续 turn 预算失败时保留 runtime error 与已有 evidence
→ runtime_executor 先投影 evidence，再生成 partial/failed
→ mapResult/application wrapper 输出稳定 budget_exhausted
```

与当前流程相比，关键变化是：

1. 在 parallel admission 处新增受保护的 Task reservation，消除 first-wins 饥饿；
2. 将实际模型调用的资源扣减与 Task reservation 绑定，避免 admission 只是软提示；
3. 将 evidence 观察从“整个 run 成功”解耦，在成功工具调用边界持久化到当前结果状态；
4. 将 `result.Error != nil` 从“跳过投影”的条件改为“保留错误并继续投影已有 admissible evidence”的条件；
5. 在每一层结果映射中优先识别 `ErrBudgetExceeded`，终端日志输出真实的 turn runner error。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 预算错误映射 | Investigation `ErrBudgetExceeded` 落入 generic runtime failure | 使用 `errors.Is` 映射为 `budget_exhausted`，保留 wrapped cause | `definition/result.go`、`app/investigation_runner.go` | 保留已有错误文本，新增稳定分类 |
| 终端错误日志 | 只打印 `state.result.Err`，可能显示 `err=<nil>` | 打印 loop/turn 实际返回 error，并区分 result error | execution loop finalization | 只改变诊断字段，不改变成功路径 |
| evidence observation | 依赖 execution 成功和完整 run 路径 | 成功工具返回 admissible manifest 后立即追加 | `execution/loop_turn.go` | 复用既有 evidence schema 和去重规则 |
| error-path projection | `result.Error != nil` 时直接返回 | 先投影 `publicTerminalEvidence` / Investigator candidates，再返回错误 | `investigation/runtime_executor.go` | 无 evidence 时维持 failed |
| 并行 admission | `ReserveAdmission(..., soft)`，token grant 可能清零 | 为每个 admitted runnable Task 保护最低调用 reservation | `investigation/scheduler.go`、`budget.go` | 不扩大 Run limit，闲置 reservation 可释放 |
| effective output | 可被共享余额压缩到不可用的小值 | 不低于已承诺的 Task minimum；不能保障则不启动该调用/Task | `execution/model_call.go`、budget gate | 保留 context window 和 role max 的更小约束 |
| reservation settlement | 主要按共享余额竞争 | Task 与 Run 两层原子 reserve/settle/release | `investigation/budget.go` | 旧 snapshot 只按显式兼容策略读取 |
| 观测字段 | 难以判断哪个边界耗尽 | 记录 run/task limit、requested/effective、reserved/used/remaining、error class、evidence count | execution/investigation logs | 不记录敏感上下文正文 |

#### 改动一：恢复预算错误的分类与日志保真

**方案：**

- 将 `investigation.ErrBudgetExceeded` 作为 canonical sentinel；所有跨层包装使用 `%w`；
- `mapResult` 对 `runErr` 或对应 outcome 先执行 `errors.Is(err, investigation.ErrBudgetExceeded)`，再进入 generic runtime failure 分支；
- application runner 若再次映射 failure，沿用同一稳定 code，不根据错误文本猜测；
- terminal/finalize log 同时保留 `state.result.Err` 和真实的 loop runner error，至少保证返回错误不会被 nil 字段覆盖；
- 日志输出 budget boundary / dimension / limit / used / reserved / requested 等结构化诊断字段。

**约束：**

- `ErrToolCallBudgetExhausted`、provider failure、context-window mismatch 不能被误归为 `budget_exhausted`；
- 不修改调用者可依赖的错误文本，新增分类字段解决机器识别问题；
- provider error 即使带有 usage，也必须保留 provider failure cause，并按实际 usage settlement。

**失败行为：**

- Investigation 预算耗尽：返回 `budget_exhausted`；
- provider 或配置失败：返回原有 runtime/provider failure；
- 无法识别的 wrapped error：返回 generic failure，但保留完整 cause chain。

#### 改动二：预算失败时抢救已产生的 evidence

**方案：**

- 在成功工具执行产生 admissible evidence manifest 的时点，将 observation 追加到当前 Agent run result；
- `RunCompiled` / runtime executor 收到 execution error 时，不清空已积累的 observations；
- 错误返回前依次执行 public terminal evidence 和 Investigator evidence candidate projection；
- 根据 report/artifact 完整度生成 `partial` 或 `failed`，并附带原始 `budget_exhausted` failure code；
- partial 结果中的 evidence 只能作为未验证材料交给允许消费 partial 的下游，不得直接冒充最终验证结论。

**约束与失败行为：**

- evidence 必须经过现有 admissibility、脱敏、大小上限和去重规则；
- 不因 error path 而放宽 evidence 上限或复制 sibling Task 的 evidence；
- 没有 admissible artifact 时，不能为了避免失败而生成空 candidate 或伪造 report；
- 若 verifier 的输入契约不接受 partial，父级应返回可诊断的 partial/blocked 状态，而不是吞掉原始 budget error。

#### 改动三：并行 Task 的层级 reservation 与公平准入

**方案：**

- Run 创建时只冻结一份共享硬上限；Task 数量、retry、replan、resume 不得扩大它；
- Scheduler 在启动 parallel group 前，按组内可运行 Task 建立受保护的最低调用 reservation；
- reservation 必须在同一个 ledger 不变量下原子登记，至少覆盖一次符合 reasoning 模型要求的可用调用；
- 每次模型调用同时检查 Task reservation 和 Run ledger，按实际 provider usage settlement，释放未使用差额；
- Task 终止、取消或失败时释放其未使用的受保护 reservation；
- 若 Run 余额不足以保护所有待启动 Task，则排队或降低并发，不能先启动后让 `limitModelOutput` 静默压缩到不可用值；
- 任务可以借用明确未被保护的剩余容量，但不能借用兄弟 Task 的最低保障。

**约束与失败行为：**

- 任何时刻 `reserved + settled usage` 都不得突破 Run hard limit；并发 reserve/settle 必须原子；
- role max output、provider context window 和 evidence projection 上限仍可进一步收紧 Task 的实际调用；
- 如果单次调用在所有独立约束下都无法满足，返回明确的 context/budget failure，不以 provider 替换或无限重试补救；
- 不按特定 Task 类型、问题关键词或输入 token 阈值硬编码预算；
- `agentAdmissionGrant` 不能继续把 token 资源清零后宣称 Task 已取得 token admission。

### 5.4 数据结构或接口契约

本提案优先复用现有 `BudgetVector`、`BudgetLedger`、`RunBudgetUsageGate`、`EvidenceObservations`、`EvidenceCandidates` 和 `TaskPartial`，不新增对外 API。内部需要明确以下 canonical 语义：

| 字段或概念 | 类型 | 所有者 | canonical 含义 | 兼容性 |
| --- | --- | --- | --- | --- |
| `Run hard limit` | `BudgetVector` | Investigation Coordinator / Ledger | 整个 Run 的累计硬上限，创建时冻结 | retry/replan/resume 不扩张 |
| `Task reservation` | 内部 reservation | Scheduler / Ledger | admitted Task 的受保护资源额度，可释放、可借用剩余容量 | 不对外暴露为第二个累计总账 |
| `requested output` | `int` | model call | 本次调用按 role/answer contract 请求的最大 output | 保留现有请求语义 |
| `effective output` | `int` | model call gate | 经过 context、role、Task 和 Run 约束后的实际请求值 | 不得低于已承诺最低值；无法满足则拒绝 |
| `EvidenceObservations` | 既有结果字段 | Agent execution | 已成功工具调用产生的 admissible evidence 观察 | error path 继续保留 |
| `EvidenceCandidates` | 既有投影字段 | Investigator executor | 可供下游消费的 Task-owned evidence artifacts | sibling 不串入 |
| `failure code` | 稳定字符串 | Result mapping | `budget_exhausted`、`runtime_failed` 等机器可识别分类 | 保留 wrapped error 和文本 |
| `completion status` | 既有状态 | Result/projector | `succeeded`、`partial`、`failed` | partial 不等于 verified |

状态转换：

```text
admitted/running
  ├─ 完整 report + evidence → succeeded
  ├─ budget_exhausted + admissible evidence → partial
  ├─ budget_exhausted + 无 admissible evidence → failed
  ├─ provider/runtime error + 可消费 artifact → partial（保留原错误）
  └─ provider/runtime error + 无 artifact → failed
```

不变量：

1. Run 的累计 settled usage 与 active reservation 的可计量总额不能超过冻结的 Run hard limit；
2. Task reservation 只能从 Run hard limit 中产生，不能创建新的父级预算；
3. successful evidence observation 一旦进入当前 Task result，后续 turn error 不得清空它；
4. 一个 Task 的 evidence candidate 只能由该 Task 的 observation 投影产生；
5. `errors.Is(returnedErr, ErrBudgetExceeded)` 为真时，所有公开 result mapping 必须使用 `budget_exhausted`；
6. partial 只表示存在 admissible artifact，不表示 verifier 或 synthesizer 已确认结论。

### 5.5 兼容、迁移与回滚

- 保留现有 `ErrBudgetExceeded` 错误文本和 `errors.Is` 行为，新增或统一 failure code，不要求客户端解析日志文本；
- 已持久化 Run 继续使用其创建时冻结的 budget snapshot；不在恢复时重新按当前 Task 数量计算预算；
- 旧的缺失 evidence candidate 只能通过新运行结果修复，不在读取历史结果时永久添加猜测性 fallback；
- 若层级 reservation 在灰度阶段出现误拒绝，应优先将调度策略切换为“保守降低并发/排队”，不能关闭 Run hard limit；
- 回滚只允许回退实现版本或关闭新 admission 分配路径，不能恢复错误码吞并或 error path 丢 evidence 的契约；
- 任何配置默认值、旧持久化 setting repair 和大 context 迁移继续遵循
  `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`，不在本提案中重复定义。

## 6. 修改伪代码

### 6.1 并行组 admission

```go
func admitParallelGroup(ctx context.Context, group []Task, run *RunLedger) ([]Admission, error) {
    runnable := filterRunnable(group)
    minimum := minimumCallBudgetFor(runnable) // based on call contract, not task names

    grants, err := run.ReserveTaskMinimums(ctx, runnable, minimum)
    if err != nil {
        // Do not launch tasks that cannot receive a usable call.
        return queueOrReduceConcurrency(runnable, err)
    }

    admissions := make([]Admission, 0, len(grants))
    for _, grant := range grants {
        admissions = append(admissions, Admission{
            TaskID: grant.TaskID,
            Budget: grant,
        })
    }
    return admissions, nil
}
```

`ReserveTaskMinimums` 必须在一个 ledger 临界区内检查并登记全部最低 reservation，避免逐个 Task 检查导致组内 overbook。若只够启动部分 Task，scheduler 应按既有优先级排队剩余 Task，并在 terminal release 后重新 admission。

### 6.2 物理模型调用

```go
func callModel(ctx context.Context, req ModelRequest, gate RunBudgetUsageGate) (ModelResponse, error) {
    requested := roleMaxOutput(req)
    effective, err := gate.EffectiveOutput(ctx, requested, req.InputTokens)
    if err != nil {
        return ModelResponse{}, fmt.Errorf("admit model call: %w", err)
    }

    reservation, err := gate.ReserveCall(ctx, CallBudget{
        InputTokens:  req.InputTokens,
        OutputTokens: effective,
    })
    if err != nil {
        return ModelResponse{}, fmt.Errorf("reserve model call: %w", err)
    }

    response, callErr := providerCall(ctx, req, effective)
    settlementErr := gate.SettleCall(reservation, UsageFromResponse(response), callErr)
    if callErr != nil {
        if settlementErr != nil {
            return response, errors.Join(callErr, settlementErr)
        }
        return response, callErr
    }
    return response, settlementErr
}
```

`EffectiveOutput` 应遵守以下顺序：

```text
requested output
→ role/definition max
→ provider context available output
→ Task reservation remaining
→ Run unreserved remaining
→ 若低于 Task protected minimum，则拒绝或由 scheduler 延后
```

它不能把一个已经 admission 的 Task 静默压缩到低于最低可用调用预算后继续执行。provider 返回的实际 usage 优先用于 settlement；缺失 usage 时不得伪造精确 token 数。

### 6.3 evidence 观察与错误投影

```go
func runCompiled(ctx context.Context, task Task) (Result, error) {
    result, runErr := runTurns(ctx, task)

    // Observations already belong to this task even when the next turn failed.
    candidates, projectionErr := projectInvestigatorEvidence(result)
    result.EvidenceCandidates = candidates

    if runErr != nil {
        result.Error = runErr
        result.FailureCode = mapFailureCode(runErr)
        if hasAdmissibleArtifacts(candidates) {
            result.Status = TaskPartial
        } else {
            result.Status = TaskFailed
        }
        return result, runErr
    }

    return finalizeSuccessfulResult(result, projectionErr)
}
```

```go
func appendEvidenceAfterTool(execution ToolExecution, state *AgentState) {
    if !execution.Succeeded || !execution.HasEvidenceManifest() {
        return
    }
    state.Result.EvidenceObservations = append(
        state.Result.EvidenceObservations,
        admissibleObservation(execution),
    )
}
```

实际实现必须继续使用现有 evidence admissibility、redaction、size limit 和 dedup 逻辑；伪代码只强调顺序：先保留成功观察，再处理后续错误，最后依据 artifact 完整度决定 partial/failed。

### 6.4 结果映射与终端日志

```go
func mapResult(runErr error, state RunState) Result {
    switch {
    case errors.Is(runErr, investigation.ErrBudgetExceeded):
        return resultWithFailure(state, "budget_exhausted")
    case errors.Is(runErr, ErrToolCallBudgetExhausted):
        return resultWithFailure(state, "tool_call_budget_exhausted")
    case runErr != nil:
        return resultWithFailure(state, "runtime_failed")
    default:
        return resultSuccess(state)
    }
}

func finalizeLoop(state *LoopState, runErr error) {
    logResult(
        state,
        runErr, // do not substitute state.result.Err when the runner returned an error
        state.Result.EvidenceObservations,
    )
}
```

预算错误的 wrapped cause、耗尽维度和 reservation snapshot 应进入内部诊断字段；面向客户端只暴露稳定 failure code 和安全的错误摘要。

## 7. 预期的效果

### 7.1 功能效果

1. 并行 Task 不再由 provider 请求发起顺序决定谁获得可用 output；
2. Run hard limit 继续是不可扩张的累计上限，Task reservation 只是在其内部做公平分配；
3. 预算失败前产生的 admissible evidence 不再因 error path 被丢弃；
4. 有 evidence 但无完整 report 的 Task 返回可消费的 partial，而不是伪造 success 或直接 generic failed；
5. 无 evidence 的预算失败仍然明确失败，不会为了提高 partial 比例产生空 artifact；
6. provider/runtime error 与 budget exhaustion 的 failure code、wrapped cause 和日志含义保持一致。

### 7.2 可观测性效果

每次 Investigation 和每个 Task 至少可关联观察以下安全字段：

```text
run_id
task_id
budget_boundary
budget_dimension
run_limit
run_reserved
run_used
run_remaining
task_reserved
task_used
requested_output
effective_output
input_tokens
failure_code
evidence_candidate_count
completion_status
```

日志不应包含完整 prompt、代码引用正文或工具返回 payload。`answerLen=0` 只能表示没有可见 report，不能被解释为没有错误；真实 runner error 和 result error 必须分开记录。

### 7.3 量化指标

- 相同 Run limit、Task 数和模型调用契约下，改变并行启动顺序不应改变 Task 是否获得最低可用调用预算；
- 任意并发 reserve/settle 测试中，Run settled usage + active reservation 不超过 frozen hard limit；
- 预算错误结果中，`budget_exhausted` 的映射准确率为 100%；
- 成功工具调用后发生预算失败的测试中，admissible evidence candidate 保留率为 100%；
- 无 admissible evidence 的预算失败不得产生 partial candidate；
- 终端日志不再以 `err=<nil>` 覆盖实际返回的预算错误。

### 7.4 不应发生的变化

- 不改变已配置 provider/model，也不增加 provider fallback；
- 不因 Task 数量增加而自动提高 Run hard limit；
- 不把 evidence candidate 变成未经 verifier 确认的 final answer；
- 不改变既有 evidence 脱敏、大小限制和去重契约；
- 不以特定任务、关键词或输入 token 数写入业务特例。

## 8. 测试与验收

### 8.1 单元测试

1. `errors.Is(wrappedErr, ErrBudgetExceeded)` 为真时，`mapResult` 输出 `budget_exhausted`；
2. tool-call budget exhaustion、provider error 和 context-window error 仍映射到各自分类；
3. terminal finalizer 使用真实 runner error，不再把 `state.result.Err=nil` 记录为成功；
4. 成功 evidence-producing tool execution 后发生下一轮预算失败，`EvidenceObservations` 和 `EvidenceCandidates` 均保留；
5. 无 admissible artifact 的 budget failure 仍为 `failed`，不会生成空 partial；
6. partial 状态包含 incompleteness reason，且不会被标记为 verified。

### 8.2 集成测试

1. 构造三个并行 Investigator，令其中一个输入 context 明显更大，验证 admission 不依赖启动顺序；
2. 验证多个 Task 并发 `ReserveCall`、provider usage settlement 和 release 不会突破 Run hard limit；
3. 验证一个 Task 提前 terminal 后释放的 reservation 可被排队 Task 使用；
4. 验证 Task-owned evidence 不会进入 sibling Task 的 candidates；
5. 验证 application runner、workflow result 和 API response 统一保留 `budget_exhausted`；
6. 验证 retry、replan、resume 复用冻结的 Run ledger，不创造额外预算。

### 8.3 回归场景

- 原始三 Investigator 并行案例：不再出现后入 Task 获得不可用 619 output 的静默启动行为；
- 单 Task 大上下文案例：context/evidence 边界单独触发时，返回明确 boundary 与 dimension；
- Run 余额充足案例：Task 可借用未保护的剩余容量，不因固定均分而过早失败；
- Run 余额不足案例：scheduler 排队/降低并发，所有已启动 Task 均满足最低 admission 契约；
- evidence 成功、verifier 失败案例：保留 evidence，同时准确反映 verifier failure；
- provider 配置错误案例：不被误报为 budget exhaustion，也不静默切换 provider。

### 8.4 验收标准

本轮实现已满足以下验收条件：

1. 原始预算失败链路已覆盖：有 admissible evidence 时保留 `partial + budget_exhausted`，无可用产物时保持 `failed + budget_exhausted`；
2. wrapped `ErrBudgetExceeded` 已在 Agent runtime、definition、Investigation 和 application runner 间保持 `budget_exhausted` 分类，不再退化为 `execution_failed`；
3. 预算失败前产生的 evidence observations、Task-owned candidates、claims 和 partial output 已在错误路径及 resume 路径保留；
4. 并行 Task 使用原子最低预算 admission；无法同时满足最低预算时缩小批次，不启动没有可用最低预算的 Task；
5. 模型调用使用 Task reservation 的 call reserve/settle/release，累计资源仍受冻结 Run hard limit 约束，并覆盖并发 race；
6. 覆盖了 provider/runtime 非预算错误回归、tool-call budget 边界、sibling Task ownership、nil Ledger fail-fast、retry/replan/resume 和持久化状态恢复；
7. 以下本轮验证命令通过：
   - `GOWORK=off go test -race -count=1 ./internal/agent/investigation ./internal/agent/execution ./internal/agent/definition ./app ./internal/agent/qa`；
   - `GOWORK=off go build ./...`；
   - `GOWORK=off go vet ./...`；
   - `git diff --check`；
8. 未启动 Docker、Qdrant、网络服务、真实或付费 provider，以及 credential-dependent 测试；未将依赖这些环境的全量测试结果作为本轮回归结论。

## 9. 风险与控制

| 风险 | 表现 | 控制措施 |
| --- | --- | --- |
| 最低 reservation 过大 | 可启动并发数下降，延迟增加 | 最低值来自实际 call contract；闲置 reservation terminal release；监控排队时间 |
| 固定均分造成预算碎片 | 小任务拿到过多、复杂任务仍不足 | 只保护最低值，允许借用未保护余额；不做永久固定配额 |
| projection error 覆盖原始 error | 排障误判为 evidence failure | 原始 `runErr` 与 projection error 分开记录，优先保留主因 |
| 错误映射层不一致 | 内部是 budget，API 仍是 runtime | 为 definition、investigation runner 和 application wrapper 增加端到端断言 |
| 并发 settlement race | 余额负数或 overbook | ledger 内原子检查，补充 `-race` 与高并发 deterministic test |
| partial 被误当 success | 下游输出未经验证 | status、failure code、verification state 分离，API 文案明确未完成 |
| 过度放大 context | 输入成本或 provider 拒绝 | context window 与 Run cumulative budget 分离，projection 在边界处限制 |

## 10. 实施计划

### 阶段 1：先补充契约与失败分类

- 固定 `ErrBudgetExceeded` 的 `errors.Is` 语义和 `budget_exhausted` 映射；
- 修正 terminal log 使用的 error 来源；
- 先加入 wrapped error、非预算错误和日志测试；
- 退出条件：失败结果不再被分类为 generic `execution_failed`。

### 阶段 2：实现 evidence salvage

- 将 admissible evidence observation 的写入边界前移到成功工具调用；
- 在 runtime executor error path 中完成 evidence projection；
- 补充 partial/failed 矩阵测试和 sibling ownership 测试；
- 退出条件：预算失败前已有 evidence 时稳定返回 partial，且无 evidence 时仍 failed。

### 阶段 3：实现并行 Task reservation

- 明确最低可用调用预算的计算来源和 ledger 不变量；
- 改造 scheduler 的 parallel admission，使 reservation 成组原子登记；
- 改造 model-call gate，使 Task reservation 与 Run ledger 共用同一准入事实；
- 加入 terminal release、borrow 和 queue/reduce-concurrency 路径；
- 退出条件：并发测试证明无 overbook，且改变启动顺序不会产生 starvation。

### 阶段 4：灰度与观测

- 在测试环境运行原始场景、上下文不均衡、预算耗尽、provider failure 和 retry/replan 场景；
- 观察 effective output、排队时间、partial 比例、Run remaining、reservation release 和 P95 延迟；
- 若最低保障过高，优先调度降并发或显式配置，不绕过硬上限；
- 退出条件：预算不变量、错误码和 evidence salvage 指标稳定。

### 阶段 5：清理旧路径

- 删除仅用于 first-wins 或 token-admission advisory 语义的代码与测试；
- 删除任何通过错误文本猜测 failure code 的 application fallback；
- 更新与预算、evidence 和结果状态相关的运行手册；
- 退出条件：代码、日志、API 和设计文档使用一致的预算与完成度术语。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 并行 Task 预算 | 继续共享池先到先得 | 受保护最低 reservation + 可借用闲置余额 | B | 防止 admission 后获得不可用 output，同时不浪费未使用容量 |
| Run limit | 按 Task 数量动态扩大 | 创建时冻结，Task 只能在内部 reservation | B | 保持成本上限稳定、可审计 |
| Task 无法获得最低预算 | 先启动再静默压缩 | 排队或降低并发 | B | 不让系统承诺不可用的 admission |
| evidence 失败路径 | 只在 run success 时投影 | 先投影已有 observations，再返回错误 | B | 保留真实产物，允许安全 partial |
| budget failure 映射 | generic `execution_failed` | stable `budget_exhausted` | B | 便于客户端和监控准确处理 |
| child token grant | 继续清零，admission 仅 advisory | token reservation 与物理调用绑定 | B | admission 必须反映真实资源保障 |
| 最低值策略 | 永久按 Agent 数固定均分 | 保护最低值，闲置可借用并在 terminal 释放 | B | 减少预算碎片，不引入静态平均分配 |

## 12. 决策摘要

本提案建议：

1. 保持 Run 共享硬上限为唯一父级累计预算，不因并行 Task、retry、replan 或 resume 扩张；
2. 在 parallel admission 阶段建立 Task-owned、可释放的最低调用 reservation，无法保障时排队或降低并发；
3. 物理模型调用同时受 Task reservation 和 Run ledger 约束，并按实际 usage settlement；
4. 使用 `errors.Is` 贯穿 Investigation 与 application wrapper，将 `ErrBudgetExceeded` 稳定映射为 `budget_exhausted`；
5. 成功工具调用立即写入 evidence observations，budget error path 先投影 evidence 再决定 partial/failed；
6. partial 只表示存在 admissible artifact，不代表 verifier 或 synthesizer 已确认结论；
7. 用 deterministic concurrency、error mapping、evidence salvage、retry/replan 和 provider failure 测试验证机制，而不是对单个事故样本加特例。

本提案与 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md` 的关系是：后者定义预算维度、默认值、Run 冻结和配置迁移的总体治理；本提案定义在共享预算已冻结之后，如何保证并行 Task 的公平 admission、预算错误的结果保真和 evidence 失败恢复。
