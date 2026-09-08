# QA 子 Agent 有界上下文与推理治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-08
关联事项：父 Run `run_afb46980515ef89f6d4b9893`；子 Run `run_child_932bda7930683a9dfb215de7`、`run_child_618f25d8269122cc258f2805`、`run_child_d4e2dac1281bf8ec73503f2a`、`run_child_bb0cbbc6d40dd20afccb4db5`；推理治理提案 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md`；子 Agent 预算提案 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`；共享预算治理提案 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案用于解决 QA 委派调查链路中「子 Agent 因累计输入预算耗尽而崩掉、上下文从不压缩、单次请求上下文无上限」三个相互耦合的机制缺陷，并把推理模型的「可见输出被 reasoning token 吃光」问题按已验证的事实约束收口为最小可落地改动。

当前，子 Agent 每步都重发整段已增长历史与工具结果，`MaxChildInputTokens=96000` 被当作**整个 Run 的累计输入预算**；5～6 步后累计输入打穿该值，3 个子 Agent 报 `input_tokens requested=48731 available=42283` 一类错误直接 `failed`。与此同时，压缩逻辑 `compactContext` 用 `agent.cfg.ContextWindow`（默认 `1_000_000`）计算 80% 高水位，而子 Agent 从未设置单次请求上下文上限 `MaxContextTokens`，导致压缩阈值远高于累计预算、永远不触发；`selectContext`/`defaultSeedContext` 又把「种子注入上限」与「累计输入预算」混用同一个 96000，种子可以一次性塞满累计预算。

本提案计划通过「给子 Agent 设置单次请求上下文上限，并让压缩/种子截断/准入一致地使用该上限」，将流程从「累计预算被每步重发打穿、压缩永不触发、种子与累计预算语义冲突」调整为「单次请求上下文有界 → 接近上限即主动压缩 → 累计预算不再被无限重发击穿」。推理模型部分，本提案**不盲目改 wire 字段**，而是先按已知网关契约证据决定是否把 `deepseek-*` 识别为推理模型，否则保留保守 `max_tokens` profile，只通过「保证结论阶段可见输出余量」缓解。

预期实现：子 Agent 不再因累计输入打穿而 `failed`、上下文在接近单次上限时主动压缩、种子注入与累计预算解耦；同时不改变 LLM 输出文本、不伪造证据、不针对具体业务写特例。

## 2. 背景

### 2.1 业务与技术背景

QA 委派调查链路在遇到跨多主题问题时，父 Agent 派发多个 Investigator 子任务，每个子任务对一个主题做深度调查并产出 `investigation.report`，父 Agent 汇总合成最终答案。子 Agent 是一个完整的小型 Agent loop：每步「思考 → 调用工具 → 读工具结果 → 下一轮模型调用」都会把**全部历史消息与工具结果**重新发送给模型。

当前相关链路为：

```text
主 Agent 预检索
→ delegate_investigation 派发子任务
→ 子 Agent 进入 loop（每步重发全量历史 + 工具结果）
→ 子 Agent 产出 investigation.report
→ 父 Agent 汇总合成
```

各模块主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/agent/delegation/executor.go` | 计算子任务 limits、注入种子上下文、派发/等待 | `ParentContext` + `DelegationTask` | `RunLimits` + `DelegationReport` |
| `internal/agent/execution/answer_context_compaction.go` | 在每次模型调用前压缩瞬时工具上下文 | `compiledLoop` | 压缩后的 `state.messages` |
| `internal/agent/execution/prompt_context.go` | 计算有效上下文窗口与单次请求准入 | `Config` | `effectiveContextWindow` |
| `internal/llm/capability.go` | 解析 provider/model 的能力 profile | `provider, model` | `ModelCapabilityProfile` |
| `platform/config/platform.go` | 定义委派与 LLM 默认值 | 平台设置 | 默认预算 |

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/delegation/executor.go`：`childBudget`（约 2477 行）、`prepareTaskBudget`（约 1035 行）、`childLimitsAt`（约 2620 行）、`gapChaseLimits`（约 2554 行）、`selectContext`（约 2949 行）、`defaultSeedContext`（约 3011 行）；
- `internal/agent/execution/answer_context_compaction.go`：`compactContext`（约 112 行，第 118/130 行用 `agent.cfg.ContextWindow`）；
- `internal/agent/execution/prompt_context.go`：`effectiveContextWindow`（约 238 行）；
- `internal/agent/execution/loop.go`：`Config.MaxContextTokens` 的注释（约 47 行）明确「单次 provider 请求上限」；
- `internal/llm/capability.go`：`DefaultModelCapability`（约 61 行）、`isOpenAIReasoningModel`（约 102 行）；
- `platform/config/platform.go`：`DefaultDelegationMaxChildInputTokens=96000`、`DefaultDelegationMaxChildOutputTokens=8000`、`DefaultLLMContextWindow=1_000_000`。

当前执行逻辑概括：

1. `childLimitsAt` 构造子任务 `RunLimits`，但只设置 `MaxInputTokens/MaxOutputTokens/MaxTotalTokens`，**不设置 `MaxContextTokens`**；
2. `prepareTaskBudget` 用 `childBudget.inputTokens`（96000）作为 `selectContext`/`defaultSeedContext` 的截断上限；
3. 子 Agent 进入 loop 后，`ensureTurnBudget` 每步先调用 `compactAnswerContext`，再调用 `ensureInputBudget`；
4. `compactContext` 用 `agent.cfg.ContextWindow`（默认 1_000_000）计算 80% 高水位，`ensureInputBudget` 才用 `effectiveContextWindow()`（min(ContextWindow, MaxContextTokens)）。

### 2.3 为什么现在需要修改

本次修改由线上多主题 QA 调查故障触发：

- 触发时间：2026-09-08，一次提问派发 4 个子 Agent，`batch finished tasks=4 completed=1 failed=3`；
- 触发标识：父 Run `run_afb46980515ef89f6d4b9893`；
- 直接表现：3 个子 Agent 报 `input_tokens requested=39912/46724/48731 available=20766/6600/42283` 后 `failed`；第 4 个 `partial`，`reasoning_tokens=7033 visible_output_tokens=0 finish_reason=length`；父最终答案 `reasoning_tokens=8192 visible_output_tokens=0 finish_reason=length`；
- 影响范围：所有走「多子 Agent 深度调查」的跨主题 QA 问题；上下文越大、子 Agent 步数越多越易触发；
- 临时处置：无，等待根因修复。

### 2.4 范围与非目标

#### 目标

1. 给子 Agent 设置单次请求上下文上限 `MaxContextTokens`，并从同一推导得到压缩/种子截断/准入三处一致使用；
2. 让 `compactContext` 使用 `effectiveContextWindow()`，使压缩在打穿累计预算前触发；
3. 让种子注入上限与累计输入预算解耦，不再用同一个 96000 当两种语义；
4. 对推理模型可见输出被 reasoning 吃光的问题，按已验证的网关契约做最小适配，未验证前不盲改 wire 字段。

#### 非目标

1. 不改 LLM 输出文本内容，不重排、不改写、不注入编号；
2. 不改变 `investigation.report` 的 schema 与校验语义；
3. 不把 reasoning token 从成本或总 token 账本中剔除；
4. 不针对「rgb/tts/消息中心/菜谱」等具体业务写硬编码特例；
5. 不通过无限增大 `MaxChildInputTokens`/`ChildTimeout` 掩盖根因。

## 3. 问题

### 3.1 问题描述

**问题 A（3 个子 Agent 因累计输入预算耗尽而 failed）：**

- **期望行为：** 子 Agent 每步都受「单次请求上下文上限」约束，接近上限即主动压缩旧工具结果，累计输入在 `MaxChildInputTokens` 内完成调查并产出 report。
- **实际行为：** 子 Agent 每步重发全量历史 + 冗长工具结果，单次请求无上限；`MaxChildInputTokens` 是累计预算，5～6 步后累计输入打穿，`ensureInputBudget`/`limitModelOutputForPhase` 直接拒绝调用，子 Agent `failed`。
- **差异：** 压缩机制从未在打穿前触发，累计预算被无限重发击穿，子 Agent 无法交付 report。

**问题 B（压缩从不触发）：**

- **期望行为：** 当单次请求输入接近「有效上下文窗口」80% 高水位时，主动压缩瞬时工具上下文。
- **实际行为：** `compactContext` 用 `agent.cfg.ContextWindow`（默认 1_000_000）算高水位，子 Agent 从未设置 `MaxContextTokens`，`effectiveContextWindow()` 对压缩无效；高水位 ≈ 800_000，单次请求永远达不到，压缩永不触发。
- **差异：** `effectiveContextWindow()` 只在 `ensureInputBudget` 和 tool admission 使用，未用于压缩，导致「有效窗口」与「压缩阈值」事实源不一致。

**问题 C（96000 同时当累计预算和种子上限）：**

- **期望行为：** 累计输入预算与「单次请求/种子注入上限」是两个独立维度，各自有明确事实源。
- **实际行为：** `prepareTaskBudget` 把 `childBudget.inputTokens`（96000）同时传给 `selectContext`/`defaultSeedContext` 作为种子截断上限，也用于累计预算。种子可以一次性塞满累计预算，导致后续步没有任何输入余量。
- **差异：** 两个不同语义共用一个数字，既浪费累计预算，又让子 Agent 冷启动即接近上限。

**问题 D（推理模型可见输出被 reasoning 吃光）：**

- **期望行为：** 报告/最终答案阶段能产出可见文本；若模型必然产出 reasoning，结论阶段应留足 completion 余量或显式低 reasoning 档。
- **实际行为：** `deepseek-v4-flash` 未被 `isOpenAIReasoningModel` 识别，走保守 `max_tokens` profile；`WithoutReasoning()` 产生的 `mode=disabled/effort="none"` 在 `applyReasoningParameters` 的 `ReasoningWireNone` 分支直接 return，不发送任何控制字段，禁用思考不生效，报告/答案的 completion 全被不可见 reasoning 消耗。
- **差异：** `visible_output_tokens=0 finish_reason=length`，服务端被迫走 deterministic conclusion/fallback，丢失模型自然语言输出。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 3 个子 Agent failed，1 个 partial，父答案 0 可见输出，单轮 6 分多钟 | `input_tokens requested=... available=...`；`reasoning_tokens=7033/8192 visible_output_tokens=0 finish_reason=length` |
| 直接原因 | 子 Agent 每步重发全量历史，累计输入打穿 96000；结论阶段 completion 被 reasoning 吃光 | `model_call.go:147`；`answer_generation.go` |
| 机制根因一 | `childLimitsAt` 未设 `MaxContextTokens`，单次请求上下文无上限 | `executor.go` `childLimitsAt` |
| 机制根因二 | `compactContext` 用裸 `ContextWindow` 而非 `effectiveContextWindow()`，压缩阈值与有效窗口不一致 | `answer_context_compaction.go:118/130` |
| 机制根因三 | 96000 同时当累计预算与种子上限，语义冲突 | `executor.go` `prepareTaskBudget`/`selectContext`/`defaultSeedContext` |
| 机制根因四 | deepseek 不在 reasoning 模型名单，`ReasoningWireNone` 下禁用思考不生效 | `capability.go:102`、`client.go:548-568` |

根因链路：

```text
子 Agent 每步重发全量历史 + 工具结果
→ childLimitsAt 未设 MaxContextTokens（单次请求无上限）
→ compactContext 用 1_000_000 算高水位（压缩永不触发）
→ 96000 又同时被当作种子上限（种子可塞满累计预算）
→ 累计输入在 5~6 步打穿，子 Agent failed
→ 推理模型下结论阶段 reasoning 又吃光 completion
→ visible_output_tokens=0，服务端退化到 deterministic fallback
```

本问题不能只通过「把 96000 调大」解决，因为那只是推迟打穿点，单次请求仍无上限、压缩仍不触发、种子与累计预算仍语义冲突，且会放大成本与推理时延。

### 3.3 影响

- **用户影响：** 子 Agent 崩掉导致回答缺块、格式散、等待时间长；
- **业务影响：** 跨主题 QA 成功率下降，需要人工补查；
- **系统影响：** 推理 token 与重复输入浪费大量成本与时间；
- **工程影响：** 「有效窗口」与「压缩阈值」两个事实源，语义混乱，难以测试与观测。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：跨主题深度调查导致子 Agent 累计输入打穿

- **Given：** 一次提问派发 4 个 Investigator 子任务，每个 `MaxChildInputTokens=96000`、`MaxChildTurns=6`、`MaxChildToolCalls=24`，工具结果冗长（trace 调用、代码片段等）；
- **When：** 子 Agent 每步重发整段历史，累计输入从约 7459 字符增长到 91218~124934 字符；
- **Then：** 应在接近单次请求上限时主动压缩旧工具结果，在 96000 累计预算内完成 report；
- **But：** 单次请求无上限、压缩不触发，累计输入打穿 96000，子 Agent `failed`。

#### 场景 B：报告阶段 completion 被 reasoning 吃光

- **Given：** 模型为 `deepseek-v4-flash`，结论/report 阶段 `WithoutReasoning()` 但 wire 层不发送禁用字段；
- **When：** 子 Agent 产出 report 或父 Agent 产出最终答案，`requested_completion_limit=4096`；
- **Then：** 应产出可见文本；
- **But：** `reasoning_tokens=7033/8192` 吃光 completion，`visible_output_tokens=0 finish_reason=length`，服务端走 deterministic fallback。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 上下文小、步数少 | 正常产出 report | 不受影响 |
| 单次请求接近上限 | 工具结果冗长 | 永不压缩，累计打穿 | 触发压缩，累计不再打穿 |
| 无工具结果可压缩 | 上下文本身已超上限 | `ensureInputBudget` 报错 | 明确「输入超窗」错误，不被累计误判 |
| 种子与累计预算冲突 | 种子内容很大 | 种子塞满累计预算 | 种子按单次上限截断，累计预算保留余量 |
| 推理模型 + 结论阶段 | deepseek + report/answer | 可见输出为 0 | 留足可见输出余量或低 reasoning 档 |
| 旧客户端/旧配置 | 未设 `MaxContextTokens` | 行为不变 | 默认推导一个安全单次上限，兼容回滚 |

### 4.3 复现步骤

1. 配置 `DelegationMaxChildInputTokens=96000`、`MaxChildTurns=6`、`MaxChildToolCalls=24`，模型 `deepseek-v4-flash`；
2. 发起一个跨多主题、工具结果冗长的 QA 问题；
3. 观察子 Agent 每步 context 字符增长（7459 → 91218~124934）与 `input_tokens requested=... available=...` 错误；
4. 可见 3 个子 Agent `failed`，报告/答案 `visible_output_tokens=0`；
5. 重复多主题问题，问题必现或高概率复现。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 单次请求上限、压缩阈值、种子上限三处统一到同一个推导函数，不针对某个子任务写特殊分支。
2. **保持单一事实源。** 单次上下文上限由 `childBudget` 派生一次，`childLimitsAt`/`gapChaseLimits`/`selectContext`/`defaultSeedContext` 共用。
3. **明确职责边界。** `delegation` 负责推导并注入 `MaxContextTokens`；`execution` 负责用 `effectiveContextWindow()` 做压缩与准入；`llm` 负责按 capability 决定 wire 字段。
4. **失败可诊断。** 压缩触发/未压缩、单次窗口、累计预算在日志与 context usage 事件中可区分。
5. **兼容与可回滚。** `MaxContextTokens` 默认推导自现有配置；未配置时行为等价于「无单次上限」，可灰度、可回退。

### 5.2 目标流程

```text
子 Agent 派发
→ childBudget 派生单次上下文上限 contextTokens
→ childLimitsAt/gapChaseLimits 注入 MaxContextTokens
→ selectContext/defaultSeedContext 用 contextTokens 截断种子
→ 子 Agent loop 每步 compactContext 用 effectiveContextWindow() 判高水位
→ 接近单次上限即压缩旧工具结果
→ 累计输入不再被无限重发打穿
→ 报告/答案阶段按 capability 保证可见输出
```

与当前流程相比，关键变化是：

1. 在 `childBudget` 新增 `contextTokens` 派生字段（单次请求上限），并在 `childLimitsAt`/`gapChaseLimits` 写入 `MaxContextTokens`；
2. 将 `compactContext` 的高水位计算从裸 `ContextWindow` 改为 `effectiveContextWindow()`；
3. 将 `selectContext`/`defaultSeedContext` 的截断上限从 `budget.inputTokens` 改为 `budget.contextTokens`；
4. 对 deepseek 推理模型，按已验证的网关契约决定是否识别为 reasoning 模型，未验证前保留保守 profile，仅通过结论阶段余量缓解可见输出问题。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 派生单次上下文上限 | `childBudget` 无 `contextTokens` | `contextTokens = inputTokens / max(1, turns)`，加下限/上限约束 | `delegation/executor.go` | 未配置 turns 时用现有默认 6 |
| 注入 `MaxContextTokens` | `childLimitsAt`/`gapChaseLimits` 不设 | 写入 `RunLimits.MaxContextTokens` | `delegation/executor.go` | 0 时保持无单次上限 |
| 压缩使用有效窗口 | `compactContext` 用 `ContextWindow` | 用 `effectiveContextWindow()`；`window<=0` 提前 return | `execution/answer_context_compaction.go` | 未设 `MaxContextTokens` 时等价旧行为 |
| 种子上限解耦 | `selectContext`/`defaultSeedContext` 用 `budget.inputTokens` | 改用 `budget.contextTokens` | `delegation/executor.go` | 无上下文时仍返回 nil |
| deepseek reasoning 识别 | `isOpenAIReasoningModel` 只认 `o1/o3/o4/gpt-5` | 按网关契约证据决定是否加入 `deepseek-*` | `llm/capability.go` | 未验证前保留保守 profile |

#### 改动一：派生并注入单次上下文上限

**方案：**

在 `childBudget` 结构体新增 `contextTokens int64` 字段，派生规则为 `inputTokens / max(1, turns)`，并施加下限（不小于单次回答/报告所需的最小上下文）与上限（不大于 `inputTokens`）。`childLimitsAt` 与 `gapChaseLimits` 在返回 `RunLimits` 时写入 `MaxContextTokens: budget.contextTokens`。

**约束：**

- `MaxContextTokens` 必须不大于 `definition.Budget.ContextTokens`（由 `validateRequestedLimits` 保证）；
- `gapChaseLimits` 的 contextTokens 与 `childLimitsAt` 一致，只是步数/工具减半；
- 不改变现有 `MaxInputTokens`（累计）语义。

**失败行为：**

- `contextTokens <= 0` 时不设置 `MaxContextTokens`，保持旧行为；
- 派生值超窗时由编译期 `validateRequestedLimits` 返回明确错误，不静默降级。

#### 改动二：压缩使用有效上下文窗口

**方案：**

将 `compactContext` 中 `window := agent.cfg.ContextWindow` 改为 `window := agent.effectiveContextWindow()`，并在 `window <= 0` 时提前 return（现有入口已如此判断 `agent.cfg.ContextWindow <= 0`，改为统一用 effective 值）。

**约束：**

- 压缩阈值（80% 高水位）与 `ensureInputBudget` 的准入窗口必须使用同一个 effective 窗口，避免「压缩不到、准入报错」的脱节；
- 不改变 `state.messages` 之外的任何会话/追踪记录（压缩只作用于模型侧副本）。

**失败行为：**

- 高水位触达但无瞬时消息可压缩时，保持现有「reach high water but no transient messages were compressible」告警，不静默吞错。

#### 改动三：种子上限与累计预算解耦

**方案：**

将 `prepareTaskBudget` 中传给 `selectContext` 与 `defaultSeedContext` 的 `childBudget.inputTokens` 改为 `childBudget.contextTokens`。`estimateTokens(input, candidate.context)` 的校验仍对累计预算 `inputTokens` 生效，但种子截断不再吞满累计预算。

**约束：**

- `defaultSeedContext` 的 facet 过滤逻辑保持不变；
- 种子是数据而非指令，investigator prompt 已声明检索内容非指令性。

#### 改动四：deepseek reasoning 能力（分阶段/待决策）

**方案：**

保持当前保守 profile 不动，除非网关契约已证明 deepseek 接受 `max_completion_tokens` / `reasoning_effort`。最小稳妥做法是：先不把 `deepseek-*` 加入 `isOpenAIReasoningModel`；对报告/结论阶段留足可见输出余量，或为已确认支持 effort 的网关显式配置 profile。此改动需网关契约证据后落地，见「5.5 兼容、迁移与回滚」。

**约束与失败行为：**

- 未验证前不同时发送 `max_tokens` 与 `max_completion_tokens`，不发送网关未知的 reasoning 字段；
- capability 不支持 reasoning 控制时，记录 `unsupported`，退回 prompt + 更小 completion cap + deterministic fallback。

### 5.4 数据结构或接口契约

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `childBudget.contextTokens` | `int64` | `delegation` | 单次 provider 请求上下文上限 | `inputTokens/turns`（turns 默认 6） | 0 表示不限制 |
| `RunLimits.MaxContextTokens` | `int64` | `agent` | 单次 provider 请求上限（已有字段） | 0 保持无上限 | 已有字段，新增赋值 |

不变量：

1. `0 <= MaxContextTokens <= definition.Budget.ContextTokens`；
2. 压缩高水位、准入窗口、tool admission 三处使用同一 `effectiveContextWindow()`；
3. 种子截断上限 ≤ 单次请求上限 ≤ 累计输入预算；
4. `MaxInputTokens` 仍是整个 Run 的累计输入预算，不被种子上限改写。

### 5.5 兼容、迁移与回滚

- **向后兼容：** `contextTokens <= 0` 时不设置 `MaxContextTokens`，旧行为不变；
- **数据迁移：** 无需回填持久化数据；新增派生字段为运行时计算；
- **灰度方式：** 通过 `DelegationMaxChildTurns`/`DelegationMaxChildInputTokens` 间接控制派生值，或后续引入独立配置；
- **回滚条件：** 出现「单次窗口过小导致 report 质量下降」或「压缩过度丢关键工具结果」时回退；
- **回滚步骤：** 将派生上限设 0（不注入 `MaxContextTokens`），恢复旧压缩行为。

## 6. 修改伪代码

### 6.1 childBudget 派生单次上下文上限

```go
type childBudget struct {
    turns         int
    toolCalls     int64
    inputTokens   int64   // 累计输入预算（语义不变）
    outputTokens  int64
    reportTokens  int64
    contextTokens int64   // 单次请求上下文上限（新增）
}

func (executor *Executor) childBudget(parent ParentContext) childBudget {
    b := childBudget{
        turns:        executor.policy.MaxChildTurns,
        toolCalls:    executor.policy.MaxChildToolCalls,
        inputTokens:  executor.policy.MaxChildInputTokens,
        outputTokens: executor.policy.MaxChildOutputTokens,
        reportTokens: executor.policy.MaxReportTokens,
    }
    b.contextTokens = perStepContextTokens(b.inputTokens, b.turns)
    return b
}

func perStepContextTokens(inputTokens int64, turns int) int64 {
    if inputTokens <= 0 {
        return 0
    }
    if turns <= 0 {
        turns = 6 // 与 DefaultDelegationMaxChildTurns 一致
    }
    per := inputTokens / int64(turns)
    if per < minContextTokens {
        per = minContextTokens // 保证单步有最小工作空间
    }
    if per > inputTokens {
        per = inputTokens
    }
    return per
}
```

### 6.2 注入 MaxContextTokens

```go
// childLimitsAt / gapChaseLimits 返回前：
return agentapi.RunLimits{
    Deadline:         deadline,
    MaxSteps:         steps,
    MaxToolCalls:     toolCalls,
    MaxInputTokens:   budget.inputTokens,   // 累计，语义不变
    MaxContextTokens: budget.contextTokens, // 单次，新增
    MaxOutputTokens:  outputTokens,
    MaxTotalTokens:   tokens,
    MaxCostMicros:    cost,
}, nil
```

### 6.3 压缩使用有效窗口

```go
func (agent *Agent) compactContext(state *compiledLoop, tools []llm.ToolDef, phase string) (answerCompactionResult, error) {
    var result answerCompactionResult
    window := agent.effectiveContextWindow() // 改为 effective，而非 cfg.ContextWindow
    if state == nil || window <= 0 {
        return result, nil
    }
    // ... 其余不变：highWater = run.ContextHighWaterTokens(window)
}
```

### 6.4 种子上限解耦

```go
func (executor *Executor) prepareTaskBudget(parent ParentContext, delegationID string, candidate *preparedTask, capability agentapi.Capability) error {
    b := executor.childBudget(parent)
    candidate.inputTokens = b.inputTokens
    candidate.outputTokens = b.outputTokens
    candidate.reportTokens = b.reportTokens
    candidate.context = selectContext(
        parent, candidate.request.EvidenceRefs, candidate.request.FocusFacets,
        b.contextTokens, // 单次上限，而非累计 inputTokens
    )
    if len(candidate.request.EvidenceRefs) == 0 {
        candidate.context = defaultSeedContext(
            parent, capability, candidate.request.FocusFacets, b.contextTokens,
        )
    }
    // estimateTokens(input, candidate.context) > b.inputTokens 仍按累计校验
    ...
}
```

### 6.5 推理能力（待网关证据）

```go
// 仅在网关契约证明 deepseek 接受 reasoning_effort / max_completion_tokens 后才加入。
func isOpenAIReasoningModel(model string) bool {
    for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
        if model == prefix || strings.HasPrefix(model, prefix+"-") {
            return true
        }
    }
    // 待决策：确认网关契约后再加入 "deepseek" 前缀。
    return false
}
```

## 7. 预期的效果

1. **子 Agent 不再因累计输入打穿而 failed：** 单次请求上限 + 主动压缩使累计输入在 `MaxChildInputTokens` 内完成，`input_tokens requested > available` 错误显著下降；
2. **压缩在正确水位触发：** 接近单次上限即压缩旧工具结果，`context usage` 事件与日志可观察到 `compaction_triggered=true`；
3. **种子与累计预算解耦：** 种子按单次上限截断，累计预算保留后续步余量；
4. **报告/答案可见输出恢复（待推理治理落地）：** 结论阶段留足余量，`visible_output_tokens=0 finish_reason=length` 比例下降。

## 8. 验证方式

```bash
cd /Users/dequan.mac/agent-workspace/Nasuta
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
git diff --check
```

行为验证：

1. `childBudget` 派生 `contextTokens` 的单测：给定 `inputTokens=96000, turns=6`，断言 `contextTokens=16000`（或受下限约束）；
2. `childLimitsAt`/`gapChaseLimits` 返回 `MaxContextTokens` 等于派生值；
3. `compactContext` 在 `MaxContextTokens` 小于 `ContextWindow` 时用 effective 窗口触发压缩（复用 `TestEffectiveContextWindowNarrowedByMaxContextTokens` 思路扩展）；
4. `prepareTaskBudget` 的种子截断按 `contextTokens` 而非累计预算截断。
