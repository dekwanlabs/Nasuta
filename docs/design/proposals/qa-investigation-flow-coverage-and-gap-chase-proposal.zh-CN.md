# QA 调查证据流程图缺失与缺口未追查治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-08
关联事项：`trace_id=617f4839cd2f4a5f8bbfcce103fb6457`、`run_2c9876b5b3290ea363128ede`、`del_86d7f17e77cd310a4f379467`；诊断日志 `/Users/dequan.mac/.codex/attachments/0aed3123-ba35-4d4c-acb0-e13dbac216ea/pasted-text.txt`
相关提案：`qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`（委派事件投影断线）、`qa-investigation-report-structured-output-and-recovery-proposal.zh-CN.md`（结构化输出与恢复）
目标版本：待评审

## 1. 摘要

本提案用于解决 QA 调查链路中「调查证据流程图缺失」和「已标出的缺口本可查却未继续追查」两个相互关联的问题。

在 `run_2c9876b5b3290ea363128ede` 这次四业务（RGB 灯效 / 消息中心 / 菜谱 / TTS）调查中，父 Agent 正确派发了 4 个 Investigator 子任务，每个子任务也都用检索工具采集到了证据（日志记录 `tool_calls` 分别为 8 / 14 / 12 / 6 次），但最终结果出现两种缺陷：

1. **没有流程图**：4 份子报告全部以 `partial` 收尾，`summary` 是确定性回退文案「Evidence collection completed, but the final report could not be generated」，`findings`、`flow`、`covered_evidence_goals` 全部为空。父 Agent 因此拿不到任何 `FlowIR`，`mergeDelegatedFlows` 无流可合并，最终回答只有「已核验环节 + 缺口」文字，没有可渲染的流程图。
2. **缺口未追查**：父 Agent 最终回答在每条业务后明确写「缺口：xxx 未见实现证据 / 无代码或路由证据 / 未核验到实现证据」。这些缺口所对应的证据（服务落点实现类、库表读写、调用方向、路由映射、外部依赖）恰恰是子 Agent 本应能用 `search_code` / `trace_calls` / `trace_deps` / `list_apis` 等工具查到的内容，但系统没有针对 `partial` / `unresolved_evidence_goals` 发起任何补查。

本提案的机制层根因不是「模型没能力」，而是两点结构性缺陷：

- **子调查的输出预算与「最后一步一次性输出 JSON」机制叠加，导致结构化报告在承载流程图时频繁截断，且确定性回退把已采集证据整体丢弃**；
- **系统没有「partial → 缺口补查」的收敛闭环**：重试只针对硬失败，不针对「部分完成但留有可查缺口」，且父 Agent 在委派 settle 后立即移除委派工具，无法二次派发。

目标流程从：

```text
子 Agent 最多 3 步取数 + 1 步一次性吐 JSON
→ 报告因 flow + findings 超出输出预算而截断
→ 确定性回退丢弃 flow/findings
→ 父 Agent 拿到空报告，无流程图
→ partial 报告的 unresolved 缺口被原样写进回答
→ 无任何补查，链路中后段永久是开放断点
```

调整为：

```text
子 Agent 在预算内完成取数，结构化报告可被可靠产出
→ 输出截断时回退保留已采集的 findings/flow/缺口
→ 父 Agent 拿到带 FlowIR 的报告，渲染流程图
→ partial 且存在可查缺口时触发有界补查（第二遍委托或子任务续查）
→ 缺口被补齐或明确标注「已尝试仍不可得」，而非静默留白
```

预期实现：流程类问题能产出流程图；被标出的缺口要么被补上、要么有明确的「已尝试但证据不存在」的结论，而不是「能查却没查」。

## 2. 背景

### 2.1 业务与技术背景

Nasuta 的 QA 调查链路在遇到「分析多个业务主题的流程」这类问题时，父 Agent 会先做一轮预检索以隔离主题（`prepare.go` 的 pre-retrieve 命中 16 个候选证据），再通过 `delegate_investigation` 动态派发多个只读 Investigator 子任务。每个子任务独立采集证据，返回一份符合 `investigation.report` schema 的结构化报告，父 Agent 汇总合成最终回答，并把子报告里的 `flow` 合并成服务端拥有的流程图（`flow_renderer.go` + `delegation.MergeFlowIRs`）。

当前链路：

```text
POST /api/qa/ask
→ qa.Service 预检索 + 准入
→ definition.Runtime.Run 执行父 Agent loop
→ 父 Agent 调 delegate_investigation 派发 N 个子任务
→ delegation.Executor 并发运行子 Investigator
→ 子 Agent 取数 → 最后一步输出 investigation.report JSON
→ 子报告投影为 DelegationReport（含 FlowIR）
→ 父 Agent awaitDelegationSettlement 回填报告
→ 父 Agent 合成回答 + mergeDelegatedFlows 渲染流程图
→ run.finished 经 SSE 广播
```

各模块职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `qa.Service` | 预检索、准入、编排入口 | 用户问题 | 父 run |
| `delegation.Executor` | 派发子任务、预算授予、报告投影 | 委派任务列表 | `DelegationReport`（含 `FlowIR`） |
| 子 `investigator.*` | 用只读工具采集证据、产出结构化报告 | 单一主题 objective | `investigation.report` JSON |
| `execution.loop` | 步进控制、最后一步收敛、确定性回退 | 消息 + 工具 | 结构化 JSON 或回退文案 |
| 父 loop + `mergeDelegatedFlows` | 合成回答、合并子流程图 | 子报告 | 最终 answer + Mermaid |

### 2.2 当前实现

关键实现与预算约束：

- `platform/config/platform.go`：`DefaultDelegationMaxChildTurns = 4`、`MaxChildToolCalls = 16`、`MaxChildInputTokens = 96000`、`MaxChildOutputTokens = 8000`、`MaxReportTokens = 4000`、`ChildTimeout = 150s`；
- `internal/agent/catalog/defaults_investigation.go`：`investigatorMaxSteps = 4`、`roleMaxContinueRounds = 1`，`investigatorSteps = boundedRoleLimit(settings.AgentMaxSteps, 4)`；
- `internal/agent/delegation/executor.go:childLimitsAt`：`MaxSteps = min(definition.Budget.MaxSteps, budget.turns)`，即子 Agent 最多 4 步；
- `internal/agent/execution/loop_execution.go`：`reservesLastStepForAnswer()` 对结构化输出恒为 true，`toolsForStep()` 在 `step >= stepLimit` 时返回 nil，即第 4 步被强制收敛为「只输出 JSON、不可再调工具」；
- `internal/agent/execution/loop_turn.go`：`remindStructuredLastStep` 在最后一步注入 `structured_last_step.txt`（「Tools are closed for this turn. Return exactly one schema-valid JSON object now」）；
- `internal/agent/execution/loop_execution.go:structuredConclusionFallback` 与 `internal/agent/definition/result_recovery.go:recoverInvestigationReport`：结构化输出截断/失败时，确定性回退产出 `findings=[]`、`covered_evidence_goals=[]` 的「空报告」，`flow` 不产出；
- `internal/agent/delegation/report.go:salvageCollectedChildReport`：在「子 run 未成功但已采集证据」时，尽量从 `result.Output` 解码保留 findings/flow，但只有当 `decodeInvestigationOutput` 成功时才保留，否则回退为空报告；
- `internal/agent/delegation/executor.go:retryOwnedAttempt / retryableChildResult`：仅当 `result.Status == RunFailed` 且 `Retryable` 时才重试，`partial`（`RunPartial`）不在重试范围；
- `internal/agent/execution/loop_execution.go:effectiveToolsForStep`：`delegationSettled` 后把 `delegate_investigation` 与 `delegation_status` 从工具面移除，父 Agent 只能进入合成，无法二次派发；
- `internal/prompts/text/agent/qa/parent_delegation.txt`：要求「一次一个 batch 覆盖所有需要深挖的主题」「Do not split leftovers into a second batch」「After child reports return, write a scannable synthesis」，没有任何「对 partial/unresolved 报告做二次补查」的指令。

### 2.3 为什么现在需要修改

本次修改由线上案例 `run_2c9876b5b3290ea363128ede` 触发：

- 触发时间：2026-09-07 23:03 ~ 23:06（`+08:00`）；
- 触发标识：`trace_id=617f4839cd2f4a5f8bbfcce103fb6457`、`del_86d7f17e77cd310a4f379467`；
- 直接表现：最终回答有 4 条业务分节与「已核验环节 / 缺口」文字，但无流程图，且每条业务的「缺口」都是可用检索工具补全的内容；
- 影响范围：流程/架构类问题（要求产出 `flow` 的场景），属于高频、高价值问答场景；
- 临时处置：无。

日志中的硬证据：

```text
run run_child_0d7880bf94c1ff8bee9e8f68 start: (maxSteps=4 configured=4 timeout=2m29.98s)
...
[agent] run run_child_... reserved last step for structured output; requiring JSON report
[agent] shrinking model output budget requested=8000 effective=6949 input_tokens=27054

DELEGATION_SETTLED 回填的 4 份报告全部为：
"summary":"Evidence collection completed, but the final report could not be generated; no unverified conclusion was accepted."
"status":"partial","completeness":"partial"
"tool_calls": 8 / 14 / 12 / 6
"output_tokens": 7885 / 8000 / 7182 / 8000
```

四个子任务的 `output_tokens` 均逼近或打满 8000，其中两个恰好为 8000（达到 provider 输出上限）；两个出现 `shrinking model output budget`（effective 被进一步压到 6949 / 7352）。这直接说明「报告在承载 `flow` + `findings` 时超出了输出预算而被截断」。

### 2.4 范围与非目标

#### 目标

1. 流程类调查的最终回答能够产出流程图（父 Agent 能从子报告拿到 `FlowIR`）；
2. 子报告为 `partial` 且存在可查缺口时，系统能触发有界补查，而不是把缺口原样留给用户；
3. 确定性回退在无法产出 schema 合法 JSON 时，尽量保留已采集的 `findings` / `flow` / 缺口，而不是丢弃。

#### 非目标

1. 本提案不解决委派事件投影断线导致前端不显示委派（见 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`）；
2. 本提案不改变回答分条契约（`parent_delegation.txt` 已要求 `1、2、3、4` 按 `task_index` 分节，且本次日志确认模型已输出 `## 1.` … `## 4.`）；
3. 本提案不通过「无限提高 token / 步数上限」掩盖问题，补查必须有界、可观测、可回滚。

## 3. 问题

### 3.1 问题描述

**问题 A（证据流程图缺失）：**

- **期望行为：** 流程类调查的子报告包含 `flow` 对象，父 Agent 合并后渲染出每个业务的流程图。
- **实际行为：** 4 份子报告全部为 `partial`，`summary` 为确定性回退文案，`findings` 与 `flow` 均为空，父 Agent 无 `FlowIR` 可合并，最终回答无流程图。
- **差异：** 子 Agent 已经用工具采集到证据（`tool_calls` 6~14 次），但结构化报告在最后一步生成失败，回退逻辑把已采集证据整体丢弃，导致「有证据却无流程图」。

**问题 B（缺口可查却未追查）：**

- **期望行为：** 子报告标出 `unresolved_evidence_goals` / `open_hops` / `gaps` 后，系统对「仍可用检索工具补全」的缺口发起有界补查；补不上的明确标记为「已尝试但证据不存在」。
- **实际行为：** 缺口被原样写进父 Agent 最终回答（「未见实现证据 / 无代码或路由证据 / 未核验到实现证据」），没有任何补查发生。
- **差异：** 缺口对应的证据（落点服务实现类、库表读写、调用方向、路由映射、外部依赖）在本系统的检索工具能力范围内，属于「能查却没查」，而非「证据不存在」。

### 3.2 根因分析 A（流程图缺失）

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 最终回答无流程图 | 最终 answer 只有 `## 1.` … `## 4.` 文字分节 |
| 直接原因 | 4 份子报告 `partial`，`flow` / `findings` 为空 | `DELEGATION_SETTLED` 回填报告 `summary=... could not produce a schema-valid report` |
| 机制根因 | 输出预算（8000，effective 一度压到 ~6950）无法承载「findings + flow(≤32 节点/≤32 边/≤16 open_hops) + gaps + goals」，结构化报告在唯一一次输出中截断；截断后确定性回退产出空报告 | `loop_execution.go:structuredConclusionFallback`、`result_recovery.go:recoverInvestigationReport`、`defaults_investigation.go:investigatorMaxSteps=4`、`executor.go:childLimitsAt` |

根因链路：

```text
子 Agent 只有 4 步，第 4 步被保留为「一次性输出 JSON」
→ 第 4 步要同时承载 findings + flow + gaps + goals
→ 输出预算 8000（且被总预算压到 ~6950）不足以装下完整 flow
→ FinishReason=length / schema 校验失败
→ structuredConclusionFallback / recoverInvestigationReport 回退为 findings=[] + 无 flow
→ 父 Agent 拿不到 FlowIR → 无流程图
```

关键点：`investigation.report` 的 `flow` 字段是 schema 的 `optional`（`flow` 非 `required`），而回退路径「宁可空也不伪造」。这本身是安全设计（不伪造证据），但当输出截断的真实原因是「预算装不下 flow」时，代价是把本已采集的 `findings` 也一并丢弃，放大成了「无证据、无流程图」。

### 3.3 根因分析 B（缺口未追查）

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 回答里写了「缺口」，但缺口可查却未查 | 最终 answer 四条业务的「缺口」段 |
| 直接原因 | 系统没有对 partial 报告触发补查 | 无任何 `retry` / 二次 `delegate` 日志 |
| 机制根因 | 重试语义只覆盖「硬失败」；委派 settle 后父 Agent 立即移除委派工具；父提示词禁止拆第二批 | `retryableChildResult`、`effectiveToolsForStep`、`parent_delegation.txt` |

根因链路：

```text
子报告 partial + unresolved_evidence_goals/open_hops
→ retryableChildResult: Status != RunFailed → 不重试
→ delegationSettled = true → effectiveToolsForStep 移除 delegate_investigation/delegation_status
→ parent_delegation.txt 指示「不要拆第二批、直接写 synthesis」
→ 父 Agent 把缺口原样写进回答
→ 无补查，链路中后段永久开放
```

需要特别澄清：`extendEvidenceStepLimit`（`tool_executor.go:369`）只会在「边界证据产生时」额外 +1 步且只扩一次，不是为了「追查已有缺口」而设计；`retryOwnedAttempt` 的重试对象是「执行失败且可重试的错误」，不是「报告部分完成但留有可查缺口」。因此两者都无法覆盖问题 B。

### 3.4 影响

- **用户影响：** 流程类回答缺少流程图，且中后段关键环节被标为「缺口」，用户拿不到完整、可信的业务链路；
- **业务影响：** 高价值流程问答的可解释性与完整度下降，用户需要人工二次追问才能补全；
- **系统影响：** 已采集证据因回退被丢弃，浪费了子 Agent 的检索成本与 token（本次 4 个子任务合计约 26 万 token）；
- **工程影响：** 「能查却没查」被静默容忍，缺少 `partial → 补查 → 判定` 的状态机，难以观测「完整度」的真实来源。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：流程类调查无流程图

- **Given：** 用户提问「分析一下 rgb 灯效、消息中心、菜谱、tts 这几个业务的流程」，父 Agent 派发 4 个 `knowledge.service.trace` / `knowledge.code.inspect` 子任务，每个子任务 objective 要求产出「端到端主流程 + 可核验跳数」；
- **When：** 子 Agent 用工具采集到服务、端口、依赖方向、库表等证据后，进入最后一步输出 `investigation.report` JSON；
- **Then（期望）：** 报告含 `flow`（subject/status/nodes/edges/open_hops），父 Agent 合并渲染出每个业务的 Mermaid 流程图；
- **But（当前）：** 报告因输出预算不足而截断，回退为空报告，无 `flow`，最终回答只有文字无流程图。

关键证据：

```text
[agent] run run_child_... reserved last step for structured output; requiring JSON report
[agent] shrinking model output budget requested=8000 effective=6949 input_tokens=27054
"output_tokens": 8000 / 7885 / 8000 / 7182
"summary":"... could not produce a schema-valid report ..."
```

#### 场景 B：缺口可查却未追查

- **Given：** 子报告 `partial`，`unresolved_evidence_goals` 含 `core_flow` / `data_and_state` / `external_dependency` 等 facet，`open_hops` 标出「provider 落点实现类、库表读写、路由映射、外部 TTS 服务地址」等缺口；
- **When：** 父 Agent `awaitDelegationSettlement` 回填报告后进入合成；
- **Then（期望）：** 系统对这些仍可检索的缺口发起有界补查（第二遍子任务或子任务续查），把缺口补上或标记「已尝试但证据不存在」；
- **But（当前）：** 无补查，缺口被原样写进回答：「缺口：`hsds-scene-provider` 侧 Feign 落点服务实现类……未见实现证据」。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常流程调查 | 证据充分 | 可能仍因预算截断丢失 flow | 产出带 flow 的完整报告 |
| 证据真实不存在 | 检索后无结果 | 缺口原样留白 | 标记「已尝试、证据不存在」，不再无意义重试 |
| 输出截断 | 报告超出输出预算 | 回退为空报告 | 回退保留已采集 findings/flow/缺口 |
| partial + 可查缺口 | 有未解决 goal 且预算/轮次剩余 | 不重试、不补查 | 触发有界补查，耗尽后标记终态 |
| partial + 不可查缺口 | goal 无法用当前工具满足 | 不重试 | 直接标记终态，不浪费重试 |
| 预算/轮次耗尽 | 补查预算用尽 | — | 保留已补部分，标记「预算耗尽」终态 |
| 并发多子任务 | N 个子任务同时 partial | 各自独立结束 | 各自独立补查，互不阻塞 |

### 4.3 复现步骤

1. 准备一个含多个业务主题、且每个主题链路较长（需要产出 `flow`）的问题；
2. 执行 QA 调查，观察子 Agent 最后一步输出与 `DELEGATION_SETTLED` 回填报告；
3. 可见 4 份报告 `status=partial`、`output_tokens` 逼近/打满 8000、`flow` 为空；
4. 观察父 Agent 最终回答：有文字分节与「缺口」，但无流程图，且缺口可被检索工具补全；
5. 该行为在「流程类 + 多主题 + 链路较长」输入下稳定复现，概率趋近必现。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 不针对「rgb/tts/消息中心」写死任何服务名或缺口清单；补查机制对任意 partial 报告生效。
2. **保持单一事实源。** 流程图的事实源是子报告的 `FlowIR`（服务端拥有、`flow_renderer.go` 渲染），不是模型手写的 Mermaid；缺口的事实源是 `unresolved_evidence_goals` / `open_hops` / `gaps` 字段。
3. **明确职责边界。** 子 Agent 负责「采集证据 + 产出结构化报告」，delegation executor 负责「判定 partial 是否可补查并授予补查预算」，父 Agent 负责「按补查结果收敛回答」，确定性回退负责「只降级、不伪造」。
4. **失败可诊断。** 报告截断、回退、补查触发、补查结果都要有结构化日志与状态字段，避免再次「静默留白」。
5. **兼容与可回滚。** 新机制默认关闭（feature flag），通过配置开启；旧配置下行为不变。

### 5.2 目标流程

```text
子 Agent 采集证据 → 最后一步输出结构化报告
→ 报告合法：投影 findings + flow
→ 报告截断/失败：回退保留已采集 findings/flow/缺口（不丢证据）
→ 父 Agent 合并 FlowIR → 渲染流程图
→ 若 partial 且存在可查缺口：触发有界补查
   ├─ 补查补齐缺口 → 更新 covered_evidence_goals / 收敛 flow
   ├─ 补查确认证据不存在 → 缺口标记「已尝试、不可得」
   └─ 补查预算/轮次耗尽 → 标记「预算耗尽」终态
→ 父 Agent 合成回答，缺口不再静默留白
```

与当前流程相比，关键变化是：

1. 在「输出截断/失败」的确定性回退处，改为「证据保留型回退」，从已采集证据重建 findings / flow / 缺口；
2. 在 delegation executor 增加「partial 报告缺口判定」：存在 `unresolved_evidence_goals` 或 `open_hops` 且仍有补查预算时，重试/续查，目标收窄为这些缺口；
3. 在父 Agent 委派 settle 后，不立即移除委派工具，而是保留一次「缺口补查」派发窗口，直到缺口耗尽或预算/轮次上限触发；
4. 在日志 / 状态中公开「补查触发、补查结果、最终完整度」。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 证据保留型回退 | `structuredConclusionFallback` / `recoverInvestigationReport` 产出空 findings、无 flow | 回退时从 `result.EvidenceUnits` / `EvidenceObservations` / 已采集的 flow 证据重建 findings 与 flow，缺口保留为 `unresolved_evidence_goals` | `execution/loop_execution.go`、`definition/result_recovery.go` | 仅当 schema 校验失败时触发，正常路径不变 |
| partial 缺口补查 | `retryableChildResult` 只对 `RunFailed && Retryable` 重试 | 新增：`partial` 且含 `unresolved_evidence_goals`/`open_hops` 且补查预算剩余时，可续查（目标收窄为缺口） | `delegation/executor.go` | feature flag 默认关闭 |
| 父 Agent 补查窗口 | `delegationSettled` 后立即移除 delegate/status 工具 | settle 后保留一次「缺口补查」派发，缺口耗尽或上限触发后移除 | `execution/loop_execution.go` | 仅在存在 partial 报告时生效 |
| 补查状态与观测 | 无补查状态 | 新增 `gap_chase` 状态/日志字段（triggered/retrieved/unavailable/budget_exhausted） | `delegation/*`、`run.Hub` | 纯新增，不破坏既有事件 |

#### 改动一：证据保留型回退

**方案：**

当子 Agent 的结构化输出在校验失败或截断后进入确定性回退时，不再产出「空报告」，而是基于已采集的证据（`result.EvidenceUnits`、`result.EvidenceObservations`、以及已解码的部分 `flow`）重建：

- `findings`：从 evidence observations 映射为「claim + evidence_refs」的最小 finding；
- `flow`：若在截断前已解码到部分 `flow`（`decodeInvestigationOutput` 成功拿到 `FlowIR` 的一部分），保留之；否则从 `trace_deps` / `trace_calls` 的证据单元构建节点与边（`evidence_state` 一律标 `inferred`/`unresolved`，绝不伪造为 `verified`）；
- `covered_evidence_goals` / `unresolved_evidence_goals`：按证据单元实际覆盖的 facet 划分，未覆盖的保留为 unresolved；
- `gaps`：保留「报告生成未完成」与「未覆盖 facet」两类缺口，不合并、不静默。

**约束：**

- 不得把 `inferred` 升为 `verified`，不得伪造 `evidence_refs`；
- 回退仍必须通过 `investigation.report` schema 校验，校验失败则退回现状（空报告 + 明确错误），不能因「保留型回退」引入非法输出；
- 保留型回退只作用于「已采集到证据」的场景；零证据时维持空报告。

**失败行为：**

- 当证据不足以重建 flow/findings 时，返回明确状态 `gap_chase=unavailable`，并保留「报告生成未完成」缺口；
- 不允许静默吞掉「回退失败」；记录结构化日志。

#### 改动二：partial 报告缺口补查（子任务续查）

**方案：**

在 delegation executor 的收尾逻辑中新增「缺口判定」：当子报告 `status=partial`、`unresolved_evidence_goals` 或 `open_hops` 非空、且补查预算（新增 `MaxGapChaseRounds`，默认 0，配置开启后默认 1）未耗尽时，将该子任务的 objective 收窄为「仅补齐这些缺口」，复用现有 child run 机制发起一次续查；续查结果合并回原报告，重复的缺口被去重，新覆盖的 goal 从 unresolved 移入 covered。

**约束：**

- 续查必须有界：`MaxGapChaseRounds` 上限（建议 1），单次续查仍受 `ChildTimeout` / `MaxChildTurns` / `MaxChildToolCalls` 约束；
- 续查目标必须是原报告的缺口字段（`unresolved_evidence_goals` / `open_hops`），不得凭空扩大调查范围；
- 幂等：重复触发续查不得产生重复 findings/evidence。

**失败行为：**

- 续查仍 partial 或证据不存在时，把缺口标记为 `unavailable`（已尝试、不可得），不再重试；
- 补查预算耗尽时标记 `budget_exhausted` 终态，保留已补部分。

#### 改动三：父 Agent 补查窗口

**方案：**

`effectiveToolsForStep` 在 `delegationSettled` 后，若存在 partial 报告且含未解决缺口，则保留 `delegate_investigation`（或专用补查工具）一次派发窗口；父提示词 `parent_delegation.txt` 同步补充「若回填报告为 partial 且缺口仍可查，先对缺口发起一次有界补查，再写 synthesis」。缺口耗尽或补查上限触发后，再移除委派工具进入收敛。

**约束与失败行为：**

- 补查窗口必须有明确上限，避免父 Agent 无限循环派发；
- 补查派发失败或超时时，父 Agent 仍必须收敛回答（保留现状的确定性收敛兜底），不得卡死。

### 5.4 数据结构或接口契约

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `DelegationReport.gap_chase` | object/string | delegation | 补查状态：`none` / `triggered` / `retrieved` / `unavailable` / `budget_exhausted` | `none` | 新增，可选 |
| `Policy.MaxGapChaseRounds` | int | delegation | partial 报告缺口补查的最大轮次 | `0`（关闭） | 新增配置 |
| `unresolved_evidence_goals` | []string | 子报告 | 未覆盖 facet（补查目标来源） | 既有字段 | 不变 |

状态转换：

```text
report.status
  ├─ complete            → 无补查
  └─ partial
       ├─ 无 unresolved/open_hops      → gap_chase=none，直接收敛
       ├─ 有缺口且预算剩余             → gap_chase=triggered
       │    ├─ 补查成功                → gap_chase=retrieved，goal 移入 covered
       │    ├─ 证据确认不存在          → gap_chase=unavailable，缺口保留
       │    └─ 预算/轮次耗尽           → gap_chase=budget_exhausted，保留已补部分
       └─ 有缺口但预算耗尽             → gap_chase=budget_exhausted
```

不变量：

1. `covered_evidence_goals` 与 `unresolved_evidence_goals` 必须构成对 task contract 要求 facet 的完整划分，不得有 facet 同时出现在两边，也不得遗漏；
2. 补查不得把 `unresolved` 改为 `verified` 而不附带新的 `evidence_refs`；
3. 补查轮次不得超过 `MaxGapChaseRounds`；
4. 任何状态下，最终回答必须能收敛（有确定性兜底），不得因补查卡死。

### 5.5 兼容、迁移与回滚

- **向后兼容：** `MaxGapChaseRounds` 默认 0，关闭补查，旧行为不变；`gap_chase` 为新增可选字段，旧消费方忽略即可；
- **数据迁移：** 无需 schema 迁移（若引入 `gap_chase` 持久化字段，按 `docs/sql/` 规范补迁移；本方案建议仅作为运行时/日志字段，不落库）；
- **灰度方式：** 通过 `delegation_gap_chase_rounds` 配置灰度，先对流程类问题开启；
- **回滚条件：** 补查导致延迟显著上升或成本超预算时回滚；
- **回滚步骤：** 将 `MaxGapChaseRounds` 置 0，恢复「回退为空报告 + 不补查」的旧行为。

## 6. 修改伪代码

### 6.1 核心流程

```go
func FinalizeChildReport(result RunResult, evidence []EvidenceUnit, schema SchemaRef) (Report, error) {
    report, err := DecodeAndValidate(result.Output, schema)
    if err == nil {
        return report, nil
    }

    // 证据保留型回退：不丢已采集证据
    if len(evidence) > 0 {
        report, ok := BuildEvidencePreservingFallback(evidence, schema)
        if ok {
            report.gapChase = "none"
            return report, nil
        }
    }

    // 零证据或重建失败：退回确定性空报告（现状）
    return EmptyReport(), fmt.Errorf("report generation failed: %w", err)
}

func (executor *Executor) CloseOutChild(parent ParentContext, task preparedTask, report Report) Report {
    report.gapChase = "none"

    if report.Status != Partial {
        return report
    }
    gaps := append(report.UnresolvedGoals, report.OpenHops...)
    if len(gaps) == 0 {
        return report
    }

    if executor.policy.MaxGapChaseRounds <= 0 || !executor.gapBudgetRemaining(task) {
        report.gapChase = "unavailable"
        return report
    }

    // 有界补查：目标收窄为缺口
    report.gapChase = "triggered"
    chased, err := executor.chaseGaps(ctx, task, gaps)
    switch {
    case err == nil && chased.Covered > 0:
        report = mergeChased(report, chased)   // 去重、goal 移入 covered
        report.gapChase = "retrieved"
    case chased.TriedAll && chased.Covered == 0:
        report.gapChase = "unavailable"        // 已尝试、证据不存在
    default:
        report.gapChase = "budget_exhausted"
    }
    return report
}
```

### 6.2 关键边界处理

```go
func BuildEvidencePreservingFallback(evidence []EvidenceUnit, schema SchemaRef) (Report, bool) {
    findings := findingsFromEvidence(evidence)   // 每条 evidence → 一个 finding
    flow := flowFromEvidence(evidence)           // 依赖/调用证据 → nodes/edges，state 仅 inferred/unresolved

    // 绝不把推断升级为 verified
    for i := range flow.Edges {
        if flow.Edges[i].EvidenceState == "verified" {
            flow.Edges[i].EvidenceState = "inferred"
        }
    }

    covered, unresolved := PartitionGoals(evidence, requiredGoals)
    report := Report{
        Findings: findings, Flow: flow,
        Covered: covered, Unresolved: unresolved,
        Gaps: append(gapsFromEvidence(evidence), "report generation was truncated"),
    }

    if !SchemaValid(report, schema) {
        return Report{}, false   // 重建失败，退回空报告
    }
    return report, true
}
```

### 6.3 修改前后对比

修改前：

```go
// 结构化输出截断后：直接丢弃已采集证据
func structuredConclusionFallback(state *compiledLoop) string {
    fallback := map[string]any{
        "summary":  "...could not produce a schema-valid report...",
        "findings": []any{},        // 丢证据
        "gaps":     []string{"...report generation ended before a schema-valid report..."},
        "covered_evidence_goals":    []string{},
        "unresolved_evidence_goals": []string{},
    }
    return json.Marshal(fallback)
}

// 重试只看硬失败，partial 永不补查
func retryableChildResult(result RunResult, ...) bool {
    if result.Status != RunFailed { return false }
    return result.Error != nil && result.Error.Retryable
}
```

修改后：

```go
func structuredConclusionFallback(state *compiledLoop) string {
    if report, ok := BuildEvidencePreservingFallback(state.result.EvidenceUnits); ok {
        return marshal(report)      // 保留 findings/flow/缺口
    }
    return marshal(emptyReport())   // 零证据时才退为空报告
}

func retryableOrChasableChildResult(result RunResult, policy Policy) bool {
    if result.Status == RunFailed {
        return result.Error != nil && result.Error.Retryable
    }
    if result.Status == RunPartial &&
       policy.MaxGapChaseRounds > 0 &&
       len(result.UnresolvedGoals)+len(result.OpenHops) > 0 {
        return true                 // 有可查缺口 → 有界补查
    }
    return false
}
```

### 6.4 配置或数据库变更

```yaml
feature:
  delegation_gap_chase:
    enabled: false
    max_rounds: 1          # 默认 0 关闭；开启后建议 1
```

```sql
-- 如无数据库变更，删除此代码块。
-- 本提案建议 gap_chase 仅作为运行时/日志字段，不落库。
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 流程类调查的子报告能稳定产出 `flow`，父 Agent 合并后渲染出每个业务的流程图；
2. 当结构化报告截断时，回退保留已采集的 `findings` / `flow` / 缺口，不再整体丢弃；
3. partial 报告存在可查缺口时，系统触发有界补查，把缺口补齐或标记「已尝试、证据不存在」；
4. 补查有明确轮次上限，不会无限循环或显著拉长时延。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `report.gap_chase` | 状态字段 | 反映缺口补查是否触发及结果 |
| `delegation_gap_chase_triggered` | Counter | 统计补查触发次数 |
| `report_preserved_from_fallback` | Counter | 统计证据保留型回退生效次数 |
| 结构化日志 `gap_chase` 字段 | 日志字段 | 定位「能查却没查」与「已尝试不可得」 |

日志应至少能够回答：

- 子报告为何 partial（截断 / 校验失败 / 预算耗尽）；
- 回退是否保留证据、保留了哪些；
- 是否触发补查、补查目标是什么、补查结果如何；
- 最终缺口是「未尝试」还是「已尝试但证据不存在」。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 流程类调查产出流程图比例 | ~0（本次 0/4） | ≥90% | 7 天 | SSE 最终态 + 评测 |
| partial 报告缺口补查覆盖率 | 0（无补查） | ≥80% 可查缺口被补上或判定 | 7 天 | 结构化日志 + 评测 |
| 回退丢证据率 | 100%（回退即空） | <10%（保留型回退生效） | 7 天 | 日志 `report_preserved_from_fallback` |
| 单次调查增量成本 | 基线 | 补查增加 ≤1 轮子任务 | 7 天 | token 计量 |

### 7.4 不应发生的变化

- 正常（非截断、非 partial）路径的行为不变；
- 模型输出的最终回答文本不被改写、重排或注入编号（分条仍由 LLM 输出，见关联提案）；
- 不得伪造 `verified` 证据或 `evidence_refs`；
- 不引入针对「rgb/tts/消息中心」等具体业务的硬编码。

## 8. 测试与验收

### 8.1 单元测试

- 结构化输出截断且有证据时，回退保留 findings / flow / 缺口，且通过 schema 校验；
- 零证据时回退仍为空报告，不回退伪造；
- `retryableOrChasableChildResult` 对 `RunFailed && Retryable` 返回 true、对 `RunPartial && 有缺口 && 预算剩余` 返回 true、对 `RunPartial && 无缺口` 返回 false；
- 补查合并去重：重复 evidence 不重复计数，goal 从 unresolved 正确移入 covered；
- `gap_chase` 状态转换在「triggered → retrieved / unavailable / budget_exhausted」各分支正确。

### 8.2 集成测试

- 派发一个流程类子任务，验证最终 `DelegationReport.Flow != nil`，父 Agent 能渲染 Mermaid；
- 模拟 partial + 有可查缺口，验证触发一次补查且结果正确合并；
- 验证补查预算耗尽时以 `budget_exhausted` 终态收敛，父 Agent 仍能合成回答；
- 验证 feature flag 关闭时行为与现状完全一致。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 四业务流程调查 | 4 条业务有流程图，缺口被补上或标注已尝试 | 自动化 + 人工 |
| 正常流程 | 证据充分 | 产出带 flow 的完整报告，无补查 | 测试 |
| 证据不存在 | 检索无结果 | 缺口标记 unavailable，不无限重试 | 测试 |
| 输出截断 | 报告超出预算 | 回退保留证据，不丢 flow/findings | 测试 |
| flag 关闭 | 默认配置 | 行为与现状一致 | 测试 |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. 流程类调查最终回答包含流程图（父 Agent 拿到并合并 `FlowIR`）；
2. partial 报告存在可查缺口时触发有界补查，结果可观测；
3. 证据保留型回退在截断时保留 findings / flow / 缺口，且不伪造 verified；
4. 补查有轮次上限，不显著拉长时延；
5. feature flag 关闭时旧行为不变；
6. 原触发案例回归通过。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 回退伪造证据 | 重建 flow/findings 时把推断当事实 | 回答可信度下降 | 强制 `evidence_state` 只允许 `inferred`/`unresolved`，schema 校验兜底 | 出现伪 verified 证据 |
| 补查无限循环 | 缺口判定/轮次上限失效 | 时延与成本失控 | `MaxGapChaseRounds` 硬上限 + 预算闸门 | 单次调查补查 > 上限 |
| 补查扩大范围 | objective 未收窄为缺口 | 调查越界 | 续查 objective 仅取缺口字段，去重合并 | 发现越界检索 |
| 回退非法输出 | 重建报告未通过 schema | 下游解析失败 | 重建后强制校验，失败退回空报告 | 出现 schema 校验失败 |
| 延迟上升 | 补查增加一轮子任务 | 回答变慢 | 默认关闭 + 灰度 + 单轮上限 | P95 延迟显著上升 |

## 10. 实施计划

### 阶段 1：证据保留型回退（最小安全改动）

- 修改 `structuredConclusionFallback` / `recoverInvestigationReport`，在「有已采集证据」时从证据重建 findings / flow / 缺口；
- 保持 schema 校验兜底，重建失败退回现状；
- 退出条件：截断场景回退保留证据，单测通过。

### 阶段 2：partial 缺口补查机制

- 新增 `MaxGapChaseRounds` 配置（默认 0）与 `gap_chase` 状态；
- 在 executor 收尾逻辑实现有界补查，父 Agent 保留补查派发窗口，更新 `parent_delegation.txt`；
- 退出条件：partial + 可查缺口能触发一次补查并正确收敛。

### 阶段 3：灰度与观测

- 对流程类问题开启 `delegation_gap_chase_rounds=1`；
- 观测流程图产出比例、补查覆盖率、增量成本与 P95 延迟；
- 退出条件：量化指标达标且无伪证据、无失控循环。

### 阶段 4：全面启用与清理

- 根据灰度结果固化默认值或保持 feature flag；
- 更新相关设计文档与评测集；
- 退出条件：验收标准全部满足。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 补查位置 | 子任务续查（delegation executor 收窄 objective 重跑） | 父 Agent 二次委派（settle 后保留一次 delegate） | A | 复用现有 child run 与重试机器，作用域可控、观测集中 |
| 回退重建 flow 的确定性 | 只保留已解码的部分 flow，缺边标 open_hops | 从依赖/调用证据单元重建 nodes/edges | B | 从证据重建能覆盖更多场景，但必须约束为 inferred/unresolved |
| 补查轮次默认值 | 默认 0（关闭） | 默认 1（开启） | A | 先灰度、可回滚，避免直接改变线上成本/时延 |
| gap_chase 是否落库 | 仅运行时/日志字段 | 落库作为报告状态字段 | A | 最小改动，先可观测，不引入 schema 迁移 |

## 12. 决策摘要

本提案建议：

1. 将结构化输出的确定性回退从「空报告」改为「证据保留型回退」，在截断/校验失败时保留已采集的 findings / flow / 缺口，且绝不伪造 `verified` 证据；
2. 引入有界「缺口补查」机制：partial 报告存在 `unresolved_evidence_goals` / `open_hops` 且补查预算剩余时，复用 child run 以收窄 objective 续查，结果去重合并；
3. 新增 `MaxGapChaseRounds`（默认 0 关闭）与 `gap_chase` 状态字段，灰度开启并观测流程图产出比例、补查覆盖率、增量成本与 P95 延迟；
4. 父 Agent 在委派 settle 后保留一次缺口补查派发窗口，并在 `parent_delegation.txt` 补充补查指令；
5. 当补查预算耗尽、证据确认不存在或 feature flag 回滚时，恢复到受控的旧行为，不引入针对具体业务的硬编码。

## 附录 A：提案提交前检查清单

- [x] 背景足以让非原作者理解系统和改动动机；
- [x] 问题以「期望行为—实际行为—差异」描述；
- [x] 至少包含一个可复现的典型场景；
- [x] 已区分表面现象、直接原因和机制根因；
- [x] 修改方案明确了职责所有者和单一事实源；
- [x] 伪代码覆盖正常路径、失败路径和状态变化；
- [x] 预期效果包含可量化指标，而不只是定性描述；
- [x] 已说明兼容、迁移、灰度和回滚方案；
- [x] 测试可以覆盖原始触发案例和关键边界场景；
- [x] 未引入只针对单个案例的硬编码特例。
