# QA 子 Agent 上下文窗口与前端流式渲染治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-08
关联事项：诊断日志 `logs/all.log`（15:05:07 → 15:10:51，`batch finished tasks=4 completed=2 failed=2`）；预算分层提案 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`；子 Agent 喂料提案 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`；推理预算与流式渲染收口 `qa-reasoning-model-budget-and-streaming-render-proposal.zh-CN.md`；回答渲染提案 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案解决 2026-09-08 15:05 运行暴露的两类独立问题，并给出具体落地改动（含代码点）：

1. **子 Agent 上下文窗口超限**（后端）：`perStepContextTokens` 把「累计输入预算 ÷ 步数」当成「单次请求窗口」，混用了两个本应独立的维度；且预喂料种子用整个窗口当额度，没给「输出预留 + 安全线」留余量，导致 code-inspect 子任务首次调用前即失败。
2. **前端流式渲染卡顿/卡死**（前端）：共定位 21 个渲染热点，其中最严重的是「思考阶段对整段 thinking 文本按 token 级频率做全量 markdown 重解析」，这是「卡死」而非「卡顿」的根源。

本提案是 `qa-reasoning-model-budget-and-streaming-render-proposal.zh-CN.md`（收口文档）的落地补充：该文档把四类症状映射到既有提案并标出「前端流式渲染性能是新增点」，本文给出后端窗口解耦与前端渲染的具体改动清单。

## 2. 背景与故障事实

一次四业务提问（RGB 灯效 / 消息中心 / 菜谱 / TTS）触发 `parent_dynamic_delegation`，派发 4 个子 Agent。日志 `batch finished tasks=4 completed=2 failed=2`：

| task | 主题 | capability | 结果 | 错误 |
| --- | --- | --- | --- | --- |
| 0 | RGB 灯效 | docs.verify | failed | MySQL `write tcp [::1]:3306: no buffer space available` |
| 1 | 消息中心 | docs.verify | partial | 推理耗尽 `reasoning_tokens=6957 visible=0 finish_reason=length` |
| 2 | ChefAI 菜谱 | docs.verify | partial | 推理耗尽 `reasoning_tokens=6905 visible=0 finish_reason=length` |
| 3 | TTS | code.inspect | failed | `context exceeds configured window: input=15893 output_reserve=8000 safety=1024 window=16000` |

父 Agent 也在 15:08:49 推理耗尽（`reasoning_tokens=8192 visible=0`），最终靠 deterministic conclusion 兜底（answerLen=8696）。

## 3. 根因分析

### 3.1 子 Agent 失败一：context window 超限（后端，结构性）

两条代码路径叠加：

1. **单次窗口被「累计 ÷ 步数」错误推导**。[executor.go:2515](../../internal/agent/delegation/executor.go#L2515) 的 `perStepContextTokens(inputTokens=96000, turns=6) = 16000` 把累计输入预算均摊成单次请求窗口。这混用了两个维度——正是 `investigation-run-budget-and-large-context-governance-proposal` 要拆开的「单请求上下文能力 ≠ 累计预算」。

2. **预喂料种子用整个窗口当额度**。[executor.go:1053](../../internal/agent/delegation/executor.go#L1053) 在父 Agent 未传显式 `evidence_refs` 时调用 `defaultSeedContext(..., childBudget.contextTokens)`，把 16000 整个窗口当种子额度；[defaultSeedContext](../../internal/agent/delegation/executor.go#L3047) 里 `remainingBytes = maxTokens * 2`。种子被允许塞满窗口，但「输出预留 8000 + 安全线 1024」也要挤进同一个窗口。

结果：code-inspect 子任务（预喂料内联 33824 字符 ≈ 15893 token）首次调用前 `15893 + 8000 + 1024 = 24917 > 16000` 即失败，压缩机制救不了（`model_step_1` 全是永久内容，无 transient 可压）。

### 3.2 子 Agent 失败二：MySQL ENOBUFS（后端，瞬时）

`queue claim` 落 MySQL 时 `write tcp [::1]:52293->[::1]:3306: write: no buffer space available`（`ENOBUFS`），发生在 [executor.go:1139](../../internal/agent/delegation/executor.go#L1139) 的队列认领持久化。4 子 + 父并发打环回，瞬时耗尽 socket 缓冲。仅 task=0 一条连接中招、其余正常，判定为**孤立瞬时事件**，非代码缺陷。

### 3.3 前端流式渲染卡顿/卡死（前端）

排查定位 21 个热点，按严重度归为五类：

| 组 | 严重度 | 根因 |
| --- | --- | --- |
| P0 | **freeze（卡死）** | 思考阶段 `renderMarkdown(thinkingText)`（[index.vue:324](../../../codeloom/web/src/views/qa/index.vue#L324)）按 **token 级频率**对整段已增长思考文本做全量同步 `md.render`（含 KaTeX + linkifyCodeRefs），无节流无缓存、O(n²)。推理阶段后端长达 20~70s，前端每来一个 reasoning token 就全量重解析一次，主线程被锁死 |
| P1 | jank | `flushStreamingSegments`（[index.vue:2241](../../../codeloom/web/src/views/qa/index.vue#L2241)）每 80ms 全量 `parseMarkdownSegments`；segment 用索引 `:key` 全量 DOM 替换；`cleanWorkspacePaths` 每 flush+commit 两次全量正则；`segCache` 整段文本作 key 造成 O(n²) 内存 |
| P2 | jank | 100ms `llmTimingTimer`（[index.vue:1451](../../../codeloom/web/src/views/qa/index.vue#L1451)）经 `liveRenderVersion` 塞进 `v-memo` key，导致整个 section 每 100ms 空转重渲；ANSWER_REVEAL 28ms 定时器 + `getBoundingClientRect` 布局抖动；ResizeObserver + `pinLiveOutput` 高频 scrollTop 写 |
| P3 | jank | mermaid 每 80ms 重渲染（[MermaidDiagram.vue:265](../../../codeloom/web/src/components/MermaidDiagram.vue#L265) 无 debounce），模块级串行 `renderChain`（[MermaidDiagram.vue:78](../../../codeloom/web/src/components/MermaidDiagram.vue#L78)）队头阻塞；大 SVG `v-html` 全量插入 |
| P4 | jank | 流结束所有码块同 tick 从 `<pre>` 切到 `vue-monaco-editor`（[CodeBlock.vue:22](../../../codeloom/web/src/components/CodeBlock.vue#L22)），N 个实例同时初始化 |

## 4. 方案设计

### 4.1 后端：单次窗口解耦 + 取消累计限制 + 预喂料压限

**改动一：单次窗口独立配置（解耦）。** 新增 `DelegationMaxChildContextTokens`（默认 32768），`childBudget.contextTokens` 直接取该值，删除 `perStepContextTokens(inputTokens/turns)` 推导及 `minChildContextTokens` 常量。

**改动二：取消 per-child 累计输入限制。** `DelegationMaxChildInputTokens` 默认改 0（禁用），删除 [executor.go:1062](../../internal/agent/delegation/executor.go#L1062) 的 `estimateTokens > inputTokens` 检查。总量护栏改由批级 `DelegationMaxTotalTokens`（默认 720000）+ `DelegationMaxTotalCostMicros` 承担——**需确认批级总账在 delegation 实现中是硬约束而非仅记录**。

**改动三：预喂料种子按「窗口 − 输出预留 − 安全线」截断。** `defaultSeedContext` 的 `maxTokens` 参数从「整个窗口」改为「`contextTokens − outputTokens − safety`」，保证 `种子 + 输出预留 + 安全线 ≤ 窗口` 恒成立。

参数建议（需对照 settings 表确认）：

| 参数 | 现值 | 建议 | 说明 |
| --- | ---: | ---: | --- |
| `DelegationMaxChildContextTokens` | 16000（推导） | **32768** | 独立配置 |
| `DelegationMaxChildInputTokens` | 96000 | **0** | 取消 per-child 累计 |
| 预喂料种子额度 | = 窗口 | **= 窗口 − 8000 − 1024 ≈ 23744** | 改动三 |
| `DelegationMaxChildOutputTokens` | 8000 | 暂不变 | 可后续按调查/报告阶段区分 |

> 说明：窗口取 25600 也能装下本次种子（15893 + 8000 + 1024 = 24917，余 683），但零工具结果增长余量；取 32768 更稳。

### 4.2 前端：五组渲染治理

**P0 治卡死——thinking 文本节流 + 缓存。** `renderMarkdown(thinkingText)` 复用 `SEG_THROTTLE_MS` 节流，或按文本 memo 缓存，或只渲染增量后缀；消除 token 级全量重解析。

**P1 治 O(n²)——增量解析 + 稳定 key。** `streamingSegments` 只解析新增 delta 拼进数组；segment 用内容 hash 作 key；`cleanWorkspacePaths` 只对 delta；`segCache` 按字符量上限并清理过期 key。

**P2 治渲染风暴——解耦高频触发源。** `llmTimingTick` 从 `liveRenderVersion`（v-memo key）拆出，计时显示用独立 ref；reveal 的滚动/布局读与 28ms 定时器解耦；ResizeObserver/pinLiveOutput 滚动写合并去重；`usagePollTimer` 指标区单独 memo。

**P3 治 mermaid——debounce + latest-wins。** `watch(code, render)` 加 150~300ms trailing debounce + 版本号丢弃过期快照；`renderChain` 改 latest-wins 合并避免队头阻塞；大 SVG 复用 DOM / 离屏渲染，modal 不二次插入。

**P4 治 Monaco——分批懒加载。** 流结束后码块用 `requestIdleCallback` / `IntersectionObserver` 可见才挂，限制同时挂载实例数。

## 5. 关键改动文件

| 文件 | 改动 |
| --- | --- |
| `Nasuta/platform/config/platform.go` | 新增 `DelegationMaxChildContextTokens`；`DelegationMaxChildInputTokens` 默认改 0 |
| `Nasuta/internal/agent/delegation/executor.go` | `childBudget` 用独立 contextTokens；删 `perStepContextTokens`；`defaultSeedContext` 种子额度改 `窗口−输出−安全`；删累计检查 |
| `Nasuta/internal/agent/qa/service.go` | 新字段接进 delegation policy |
| `codeloom/web/src/views/qa/index.vue` | thinking 节流（P0）、增量解析+稳定 key（P1）、解耦 100ms/28ms 触发源（P2） |
| `codeloom/web/src/components/MermaidDiagram.vue` | debounce + latest-wins（P3） |
| `codeloom/web/src/components/CodeBlock.vue` | Monaco 分批懒加载（P4） |

## 6. 影响与风险

- **成本**：取消 per-child 累计后，整体消费由批级总账（720000）+ 成本上限兜底；需确认这两层是硬约束，否则失去总量护栏。
- **窗口取值**：32768 与预喂料种子额度（23744）联动；若预喂料后续再增大，需同步上调窗口或收紧种子额度。
- **前端回归面**：增量解析 + 稳定 key 需保证「流式中间态与最终态渲染一致」，回归锚点见 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`；thinking 节流不能丢失「思考中」的可视反馈。
- **MySQL ENOBUFS**：本提案不改代码，仅建议观察复现频率；若频发再考虑幂等重试 + 落库限流。

## 7. 实施顺序

1. **后端改动一~三**：单次窗口解耦 + 取消累计 + 预喂料压限（同一模块，一并落地）。
2. **前端 P0**：thinking 节流（单点改动，直接治卡死）。
3. **前端 P2**：解耦 100ms/28ms 触发源（改动小，消除空转重渲）。
4. **前端 P1**：增量解析（治 O(n²)，改动中等）。
5. **前端 P3 + P4**：mermaid 与 Monaco（独立并行）。
6. 观察 MySQL ENOBUFS 是否复现，决定是否加幂等重试/落库限流。

## 8. 验证与验收

- 复现四业务提问，核对日志不再出现 `context exceeds configured window`；目标 `tasks=4 completed=4 failed=0`（或仅瞬时 ENOBUFS 被重试吸收）。
- 确认预喂料种子被截断到「窗口 − 输出 − 安全」以内；确认取消 per-child 累计后批级总账仍是硬护栏。
- 前端：Performance 面板确认思考阶段主线程不再被 `renderMarkdown(thinkingText)` 长时间占用；长流式回答期间无持续卡顿，流式中间态与最终态渲染一致。
- 回归：`go build ./... && go vet ./... && go test -race ./...`（Nasuta 侧），codeloom 前端构建通过。
