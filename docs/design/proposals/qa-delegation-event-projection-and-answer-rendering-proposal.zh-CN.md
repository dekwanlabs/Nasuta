# QA 委派事件投影断线与回答分条渲染治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-07
关联事项：`trace_id=617f4839cd2f4a5f8bbfcce103fb6457`、`run_2c9876b5b3290ea363128ede`、`del_86d7f17e77cd310a4f379467`；诊断日志 `/Users/dequan.mac/.codex/attachments/0aed3123-ba35-4d4c-acb0-e13dbac216ea/pasted-text.txt`
相关提案：`qa-chain-layer-collapse-proposal.zh-CN.md`、`qa-investigation-report-structured-output-and-recovery-proposal.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案用于解决 QA 调查链路上两个相互独立、但常被一并报告的问题：

1. **回答分条展示**：用户反馈最终回答"文字太乱、没有按 1、2、3、4 分条列出"。
2. **委派进度不显示**：父 Agent 实际派发了 Investigator 子 Agent，但前端进度追踪区没有显示委派步骤。

针对问题 1，本提案首先澄清：后端模型已经输出正确的分节 Markdown（`## 1. RGB 灯效`、`## 2. 消息中心`、`## 3. 菜谱（Chef AI）`、`## 4. TTS`），说明上一提案的"父 Agent 分条输出契约 + `parent_delegation.txt` 提示词"已经生效。剩余风险集中在**前端渲染保真度**（流式中间态与最终态是否都保留 `h2` 层级），需要做验证与轻量加固，而不是改后端重排模型文本。

针对问题 2，本提案定位到机制层根因：`refactor(agent): collapse QA chain thin layers`（commit `40a5041`）在收敛 `definition.Runtime` 时删除了 `EmitEvent`、`ProjectToolEvents` 等事件转发方法，而 `app/qa.go` 注入 delegation executor 时仍通过类型断言依赖这些接口。断言失败后 `Events=nil`，导致 `delegation.created/started/completed/...` 与子 Agent 工具事件被静默丢弃，前端因此看不到委派。

目标流程从：

```text
delegation executor 持有 Events=nil
→ delegation.* / child tool 事件被静默跳过
→ 父 QA 的 SSE 广播里没有委派事件
→ 前端 applyDelegationEvent 收不到任何 delegation.* 事件
→ 用户看不到委派步骤
```

调整为：

```text
definition.Runtime 重新实现 EventEmitter / toolEventProjector
→ delegation.* / child tool 事件写入父 run.Hub
→ 父 QA 的 SSE 广播携带委派事件
→ 前端 applyDelegationEvent 渲染委派步骤卡片
→ 用户可见委派进度
```

预期实现：委派步骤在前端进度追踪区可见、可展开，且最终回答保持 `1、2、3、4` 分节清晰展示，代码不对 LLM 输出做任何重排或改写。

## 2. 背景

### 2.1 业务与技术背景

Nasuta 的 QA 调查链路在遇到跨多个业务主题的问题时，父 Agent 会动态派发多个 Investigator 子任务，每个子任务采集证据并返回 `investigation.report`，父 Agent 再汇总合成最终回答。用户既关心最终答案是否分条清晰，也关心运行过程中是否能看到委派子 Agent 的进度。

当前链路：

```text
POST /api/qa/ask
→ dashboard 订阅 run.Hub 的 SSE
→ qa.Service 准备与准入
→ definition.Runtime.Run 执行父 Agent loop
→ delegation.Executor 派发子任务（child run）
→ child 报告回填
→ 父 Agent 合成最终 answer
→ run.finished 经 SSE 广播给前端
```

### 2.2 当前实现

事件链路的关键实现：

- `internal/agent/run/hub.go`：`Hub` 是事件事实源，`EmitEvent` 把任意事件广播给订阅的 run，`ProjectToolEvents` 把子 Agent 工具生命周期投影到父 run；
- `internal/agent/definition/runtime.go`：`Runtime` 持有私有 `hub`；
- `internal/agent/delegation/executor.go`：`Executor` 通过 `Events EventEmitter` 发射 `delegation.*` 事件，通过 `runtime.(toolEventProjector)` 投影子工具事件；
- `app/qa.go`：`runtimeEventEmitter(runtime)` 做 `runtime.(delegation.EventEmitter)` 类型断言，为 executor 注入事件发射器；
- `web/src/views/qa/index.vue`：`applyDelegationEvent` / `delegationStepKind` 已实现 `delegation.*` 事件的解析与步骤卡片渲染。

关键缺陷在 commit `40a5041`：该提交把 `definition.Runtime` 的 `EmitEvent`、`ProjectToolEvents`、`EmitToolStarted`、`EmitToolFinished`、`Hub` 等方法一并删除，但 `app/qa.go` 的注入逻辑与 `delegation.Executor` 的接口断言没有同步修改。

### 2.3 为什么现在需要修改

- 线上反馈：委派子 Agent 已实际运行（日志有 `run_child_*`、`del_86d7f17e77cd310a4f379467`、4 份 report 生成），但前端看不到委派步骤；
- 回归风险：这是重构引入的功能回归，不是新需求；委派事件投影是既有契约，应恢复而非重新设计；
- 回答分条：模型输出已正确，但用户仍感知"乱"，需要确认前端渲染是否忠实保留分节层级，避免"后端修好了、前端仍显示乱"的断层。

### 2.4 范围与非目标

#### 目标

1. 恢复 `definition.Runtime` 对 `delegation.EventEmitter` 与 `toolEventProjector` 两个接口的实现，使委派事件重新进入父 SSE；
2. 为委派事件投影补回归测试（delegation 开始/完成、child tool 投影到 parent）；
3. 验证并加固前端回答渲染，确保 `1、2、3、4` 分节在流式与最终态都清晰可见。

#### 非目标

1. 不改后端 LLM 输出的文本内容，不重排、不改写、不注入编号；
2. 不改变 `investigation.report` 的 schema 与校验语义；
3. 不新增委派事件类型，仅恢复既有事件的投递；
4. 不通过提高 token/timeout 上限掩盖问题。

## 3. 问题

### 3.1 问题描述

**问题 A（回答分条）：**

- **期望行为：** 最终回答按 `1、2、3、4` 分节，每个业务主题独占一节，顺序与 `task_index` 一致，前端清晰渲染层级。
- **实际行为：** 模型已输出 `## 1.` 至 `## 4.` 的分节 Markdown，但用户仍反馈"文字太乱、没有分条"。
- **差异：** 模型侧已经符合契约，需要定位是流式中间态、最终态渲染，还是前端 CSS/解析导致层级丢失。

**问题 B（委派不显示）：**

- **期望行为：** 父 Agent 派发子 Agent 时，前端进度追踪区出现委派步骤卡片，可展开查看子任务。
- **实际行为：** 委派实际执行了，但前端没有显示任何委派步骤。
- **差异：** 后端事件投影断线，`delegation.*` 事件从未到达前端。

### 3.2 根因分析

#### 问题 A 根因

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 用户看到回答"揉在一起" | 用户反馈 |
| 直接原因 | 疑似前端渲染未保留分节层级 | 待验证 |
| 机制根因 | 模型输出已正确，风险集中在前端渲染保真度，而非后端文本 | 日志 `run_2c9876b5b3290ea363128ede` 最终答案为 `## 1. RGB 灯效` … `## 4. TTS` |

最新日志中父 Agent 最终答案（`loop_turn.go:281 done at step 6 (final answer)`）为：

```text
## 1. RGB 灯效
**主线**：…
## 2. 消息中心
**主线**：…
## 3. 菜谱（Chef AI）
**主线**：…
## 4. TTS
**主线**：…
```

说明后端提示词修复已经生效。前端 `parseMarkdownSegments → markdown-it` 渲染，CSS 已含 `:deep(h2)`、`:deep(ul/ol)` 等规则，理论上能正确显示。需要验证的是：流式过程中 `breaks:true` 让单换行变 `<br>` 的中间态，以及最终态是否真实保留了 `h2` 层级。

#### 问题 B 根因

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 前端看不到委派步骤 | 用户反馈 |
| 直接原因 | `Events=nil`，`executor.emit` 开头 `if executor.events == nil { return }` 静默丢弃事件 | `executor.go:2687`、`executor.go:2712` |
| 机制根因 | `definition.Runtime` 不再实现 `delegation.EventEmitter` 与 `toolEventProjector`，`runtimeEventEmitter` 类型断言失败返回 nil | `app/qa.go:732`、`internal/agent/definition/runtime.go`（无 `EmitEvent`/`ProjectToolEvents`） |

根因链路：

```text
commit 40a5041 删除 Runtime.EmitEvent / ProjectToolEvents
→ app/qa.go runtimeEventEmitter 类型断言失败
→ ExecutorConfig.Events = nil
→ executor.emit / verifier 开头 nil 检查直接 return
→ delegation.* 与 child tool 事件从不写入 hub
→ 父 SSE 无委派事件 → 前端无显示
```

前端 `applyDelegationEvent` 等处理逻辑已存在，问题不在前端解析，而在后端事件投影断线。

### 3.3 影响

- **用户影响：** 看不到委派进度，误以为系统没有并行调查，也影响对回答可信度的判断；
- **业务影响：** 委派能力对用户"不可见"，削弱多 Agent 编排的可解释性；
- **系统影响：** 事件被静默丢弃，无错误日志，问题难以发现；
- **工程影响：** 接口契约与实现脱节，重构后无编译期保护，回归只能靠人工发现。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：委派子 Agent 前端不可见

- **Given：** 父 Agent 派发 `delegate_investigation`，产生 4 个 `run_child_*` 子任务；
- **When：** 子任务开始、执行、结束；
- **Then（期望）：** 前端进度区出现委派步骤，显示"委派调查"及子任务状态；
- **But（当前）：** 前端无任何委派步骤，运行过程看起来像单 Agent。

关键证据（日志）：

```text
run run_child_041e8cea1681f1d9ed3a3d4a model step 3 timing: total=7.57s ...
run run_child_285d53e0a5967f8fcb7cde5c model step 3 timing: total=8.51s ...
DELEGATION_SETTLED: 委派调查已全部结束 …
```

#### 场景 B：回答分条渲染保真

- **Given：** 父 Agent 输出 `## 1.` 至 `## 4.` 的分节 Markdown；
- **When：** 前端流式接收并最终提交消息；
- **Then（期望）：** 四个 `h2` 分节清晰展示，编号 1/2/3/4 可见；
- **But（当前）：** 用户仍反馈"揉在一起"，需验证流式中间态与最终态的渲染结果。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常委派 | 1 个子任务 | 无事件 | delegation.started/completed 均广播 |
| 并发委派 | 4 个子任务 | 无事件 | 每个 child 的事件独立投影到 parent |
| 子任务 partial | 报告未产出合法 JSON | 无事件 | 仍广播 delegation.completed，状态 partial |
| 子任务失败 | child run 报错 | 无事件 | 广播 delegation.failed |
| 回答无分节 | 模型输出纯文本 | 保持原样 | 不注入编号，保持模型原文 |
| 流式中间态 | `## 1.` 尚未闭合 | 可能显示不完整层级 | 最终态完整，中间态尽量稳定 |

### 4.3 复现步骤

1. 在 QA 页面提出一个跨多业务主题的问题（如"分析 rgb 灯效、消息中心、菜谱、tts 的流程"）；
2. 观察后端日志，可见 `delegate_investigation` 与 `run_child_*` 子任务；
3. 观察前端进度追踪区，委派步骤缺失；
4. 观察最终回答分节渲染是否保留 `h2` 层级。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 恢复接口实现，而非针对本次 trace 写特例。
2. **保持单一事实源。** `run.Hub` 仍是事件唯一事实源，`Runtime` 只做转发。
3. **明确职责边界。** `Runtime` 持有 hub 并对外暴露窄接口；`app/qa.go` 只做装配；`delegation.Executor` 只负责发射。
4. **失败可诊断。** 事件发射失败应可观测，不静默吞错（至少在 nil 注入时打日志）。
5. **兼容与可回滚。** 恢复方法不影响现有 `hub.EmitEvent` 的直接调用，可灰度、可回滚。

### 5.2 目标流程

```text
definition.Runtime.EmitEvent / ProjectToolEvents 恢复
→ delegation.Executor.Events 非 nil
→ delegation.* / child tool 事件写入父 hub
→ 父 SSE 广播
→ 前端 applyDelegationEvent 渲染
```

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 恢复 `EmitEvent` | 无该方法 | 转发到 `runtime.hub.EmitEvent` | `internal/agent/definition/runtime.go` | 纯增量，不影响现有调用 |
| 恢复 `ProjectToolEvents` | 无该方法 | 转发到 `runtime.hub.ProjectToolEvents` | 同上 | 纯增量 |
| 恢复 `Hub` / `EmitToolStarted` / `EmitToolFinished`（可选，建议一并恢复） | 无该方法 | 转发到 hub | 同上 | 保持 Runtime 完整事件边界 |
| 事件注入失败可观测 | 断言失败静默返回 nil | nil 时记录结构化 warning | `app/qa.go` | 仅日志，不改行为 |
| 前端渲染保真验证 + 轻量加固 | `breaks:true`、CSS 已有 h2 规则 | 确认最终态保留 h2；必要时增强分节视觉分隔 | `web/src/views/qa/index.vue`、`web/src/utils/markdown.ts` | 不改模型文本 |

#### 改动一：恢复 `definition.Runtime` 的事件转发方法

**方案：**

在 `internal/agent/definition/runtime.go` 重新添加被删除的方法，使 `*Runtime` 同时满足 `delegation.EventEmitter`（`EmitEvent(EventType, ExecutionEvent)`）与 `toolEventProjector`（`ProjectToolEvents(string, string, string, string) func()`）。`EmitToolStarted` / `EmitToolFinished` / `Hub` 一并恢复，保持事件边界完整。

**约束：**

- 所有方法对 nil receiver / nil hub 做防御性处理；
- 不改变 `run.Hub` 的既有语义。

**失败行为：**

- nil receiver 或 nil hub 时返回空操作，不 panic；
- 不静默吞掉真实发射错误（hub 内部已处理）。

#### 改动二：事件注入失败可观测

**方案：**

`runtimeEventEmitter` 在类型断言失败时记录一条结构化 warning，便于后续发现同类接口契约脱节。

**约束与失败行为：**

- 不影响委派执行的降级行为（委派仍会运行，只是事件不可见）；
- 只增日志，不改事件语义。

#### 改动三：前端回答分条渲染保真

**方案：**

验证最终提交消息（`turn.assistant.content`）经 `parseMarkdownSegments` 后确实保留 `h2`；若确认存在层级丢失，修正渲染路径；必要时为分节增加轻量视觉分隔（不修改模型文本）。

**约束与失败行为：**

- 代码不重排、不改写、不注入编号；
- `1、2、3、4` 的编号与顺序完全由模型输出决定。

### 5.4 数据结构或接口契约

本提案不新增数据结构。恢复的接口契约与既有一致：

| 字段/方法 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `EmitEvent` | `func(EventType, ExecutionEvent)` | `definition.Runtime` | 转发事件到 hub | 转发 | 恢复既有契约 |
| `ProjectToolEvents` | `func(string, string, string, string) func()` | `definition.Runtime` | 子工具事件投影到父 run | 返回 no-op | 恢复既有契约 |

### 5.5 兼容、迁移与回滚

- **向后兼容：** 纯增量恢复方法，不影响现有 `hub.EmitEvent` 直接调用与既有测试；
- **数据迁移：** 无；
- **灰度方式：** 无 feature flag 需求，直接恢复既有契约；
- **回滚条件：** 若恢复后事件量异常或重复，可仅保留 `EmitEvent`/`ProjectToolEvents` 最小集合；
- **回滚步骤：** revert 相关方法即可。

## 6. 修改伪代码

### 6.1 核心流程

```go
// internal/agent/definition/runtime.go

// EmitEvent 实现 delegation.EventEmitter，把委派事件转发到父 run.Hub。
func (runtime *Runtime) EmitEvent(eventType run.EventType, event run.ExecutionEvent) {
    if runtime != nil && runtime.hub != nil {
        runtime.hub.EmitEvent(eventType, event)
    }
}

// ProjectToolEvents 实现 toolEventProjector，把子 Agent 工具生命周期投影到父 run。
func (runtime *Runtime) ProjectToolEvents(
    childRunID, parentRunID, workflowRunID, nodeID string,
) func() {
    if runtime == nil || runtime.hub == nil {
        return func() {}
    }
    return runtime.hub.ProjectToolEvents(childRunID, parentRunID, workflowRunID, nodeID)
}

// 可选：恢复完整事件边界
func (runtime *Runtime) Hub() *run.Hub {
    if runtime == nil {
        return nil
    }
    return runtime.hub
}

func (runtime *Runtime) EmitToolStarted(runID string, event run.ToolStartedEvent) {
    if runtime != nil && runtime.hub != nil {
        runtime.hub.EmitToolStarted(runID, event)
    }
}

func (runtime *Runtime) EmitToolFinished(runID string, event run.ToolFinishedEvent) {
    if runtime != nil && runtime.hub != nil {
        runtime.hub.EmitToolFinished(runID, event)
    }
}
```

```go
// app/qa.go —— 事件注入失败可观测

func runtimeEventEmitter(runtime agentapi.Runtime) delegation.EventEmitter {
    emitter, ok := runtime.(delegation.EventEmitter)
    if !ok {
        log.Warn("qa delegation event emitter unavailable; delegation progress will not be broadcast",
            "runtime_type", fmt.Sprintf("%T", runtime))
    }
    return emitter
}
```

### 6.2 关键边界处理

```go
// delegation/executor.go 的 emit 路径保持既有防御：
func (executor *Executor) emit(...) {
    if executor.events == nil {
        // 修复后正常情况下不会命中；保留防御避免历史存量实例 panic。
        return
    }
    ...
    executor.events.EmitEvent(eventType, details)
}
```

### 6.3 修改前后对比

修改前：

```go
// app/qa.go
Events: runtimeEventEmitter(runtime), // runtime 类型断言失败 → nil
```

修改后：

```go
// definition.Runtime 已实现 EmitEvent / ProjectToolEvents
Events: runtimeEventEmitter(runtime), // 断言成功 → 非 nil
```

### 6.4 配置或数据库变更

无配置、无数据库变更。

## 7. 预期的效果

### 7.1 功能效果

1. 委派派发时，`delegation.created/started` 事件进入父 SSE，前端显示委派步骤卡片；
2. 子任务结束时，`delegation.completed/failed` 等事件到达前端，卡片状态更新；
3. 子 Agent 工具事件投影到父 run，前端可嵌套查看子调用；
4. 回答 `1、2、3、4` 分节在最终态清晰渲染，模型文本不被改写。

### 7.2 可观测性效果

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| 事件发射器注入失败 warning | 日志 | 发现接口契约脱节 |
| `delegation.*` 事件进入 SSE | 事件流 | 前端可见委派进度 |

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 委派事件前端可见率 | 0（事件被丢弃） | 100% | 7 天 | SSE 事件 + 前端渲染 |
| 回答分条最终态保留率 | 待验证 | 100% | 7 天 | 前端渲染快照 |

### 7.4 不应发生的变化

- 委派执行本身行为不变（派发、报告回填、合成照常）；
- 模型输出文本不被重排、改写或注入编号；
- `investigation.report` schema 与校验语义不变；
- 单 Agent 路径（无委派）事件行为不变。

## 8. 测试与验收

### 8.1 单元测试

- `definition.Runtime` 实现 `delegation.EventEmitter` 与 `toolEventProjector`（接口编译期断言）；
- nil receiver / nil hub 时 `EmitEvent` / `ProjectToolEvents` 不 panic；
- `runtimeEventEmitter` 在非 EventEmitter 时返回 nil 并记录 warning；
- delegation 开始/完成时 `hub` 收到对应事件。

### 8.2 集成测试

- 派发 1 个与 4 个子任务，验证 `delegation.created/started/completed` 均进入父 SSE；
- child tool 事件被投影到 parent run；
- 前端 `applyDelegationEvent` 能解析并渲染委派步骤卡片。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 四业务主题调查 | 前端显示 4 个委派步骤，回答分条输出 | 自动化 + 人工 |
| 单 Agent 路径 | 无需委派 | 无委派事件，回答正常 | 测试 |
| 子任务 partial | 报告未产出合法 JSON | 委派卡片显示 partial | 测试 |
| 子任务失败 | child 报错 | 委派卡片显示失败 | 测试 |

### 8.4 验收标准

1. 委派事件重新进入父 SSE，前端可见委派进度；
2. `definition.Runtime` 同时满足 `EventEmitter` 与 `toolEventProjector`；
3. 事件注入失败有结构化日志，不静默；
4. 回答分条在最终态清晰渲染，代码不改写模型文本；
5. 单 Agent 路径行为不变。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 事件重复 | 多处注入同一 hub | 前端重复渲染 | 单一注入点 + 回归测试 | 出现重复卡片 |
| 事件量激增 | 子工具事件过多 | SSE 压力增大 | 保持既有投影语义，不新增事件 | SSE 延迟显著上升 |
| 前端渲染改动误伤 | CSS 选择器过宽 | 其他消息样式受影响 | 限定 `.qa-content` 作用域 | 样式回归失败 |

## 10. 实施计划

### 阶段 1：恢复委派事件投影（最小修复）

- 恢复 `definition.Runtime` 的 `EmitEvent` 与 `ProjectToolEvents`；
- 在 `runtimeEventEmitter` 增加断言失败 warning；
- 补接口编译期断言与单测；
- 退出条件：委派事件进入父 SSE，前端可见。

### 阶段 2：回答分条渲染保真验证

- 验证最终提交消息经 `parseMarkdownSegments` 后保留 `h2`；
- 若有层级丢失，修正渲染路径；否则仅做视觉加固；
- 退出条件：`1、2、3、4` 分节在最终态清晰展示。

### 阶段 3：回归与灰度

- 跑通原触发案例，确认前端委派步骤与分条回答都正常；
- 退出条件：单 Agent 路径无回归，委派路径事件完整。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 恢复方法范围 | 仅恢复 EmitEvent + ProjectToolEvents | 一并恢复 Hub/EmitToolStarted/Finished | B | 保持 Runtime 完整事件边界 |
| 事件注入失败处理 | 仅返回 nil | 返回 nil 并记录 warning | B | 可观测、不改变行为 |
| 前端分节加固 | 仅验证不修改 | 验证 + 视觉分隔加固 | 视验证结果 | 优先不引入不必要改动 |

## 12. 决策摘要

本提案建议：

1. 恢复 `definition.Runtime` 的 `EmitEvent`、`ProjectToolEvents`（及 `Hub`/`EmitToolStarted`/`EmitToolFinished`），使委派事件重新进入父 SSE；
2. 事件注入失败增加结构化 warning，避免接口契约脱节再被静默掩盖；
3. 补回归测试，覆盖委派开始/完成与 child tool 投影；
4. 验证并轻量加固前端回答分条渲染，保证 `1、2、3、4` 分节清晰展示，代码不改写模型文本。

## 附录 A：提案提交前检查清单

- [x] 背景足以让非原作者理解系统和改动动机；
- [x] 问题以"期望行为—实际行为—差异"描述；
- [x] 根因区分表面现象、直接原因与机制层根因；
- [x] 场景可复现，改动有归属与失败行为；
- [x] 预期效果可验证，非目标明确；
- [x] 不引入业务名、trace 或 delegation ID 特例。
