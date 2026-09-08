# QA 推理模型预算与流式渲染修复提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-08
关联事项：诊断日志 `logs/all-2026-09-08.log`（11:12:36 → 11:19:02，单轮 6 分 26 秒）；推理治理提案 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md`；子 Agent 预算提案 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`；回答渲染提案 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`；共享预算治理提案 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案是 2026-09-08 一次多主题 QA 调查的**事故定位与收口文档**。它把四个症状（3 个子 Agent 失败、单轮耗时 6+ 分钟、前端卡顿、答案格式散）逐一映射到既有提案，确认其中三类的根因已被相关提案覆盖；**唯一尚未被既有提案覆盖的新增问题是前端流式渲染卡顿**，本提案给出针对性治理。

核心结论：

1. **推理模型未识别导致结论预算耗尽**（子 Agent partial + 父 Agent 最终答案 0 可见输出）——根因与 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md` 完全一致，本提案补充 09-08 的复现证据与具体数值（`requested_completion_limit=4096` 被 `reasoning_tokens=7033/8192` 吃光），说明该提案落地前问题会持续复现。
2. **子 Agent 累计输入预算耗尽**（3 个 `failed`）——根因与 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md` 及 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md` 一致。
3. **答案格式散**——后端已输出正确分节，属前端渲染保真度问题，与 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md` 一致。
4. **前端流式渲染卡顿**——**本提案新增**：`streamingSegments` 每 80ms 全量重解析（O(n²)）、mermaid 流式重渲染串行排队、`llmTimingTimer` 每 100ms 触发整段重绘。

## 2. 背景与故障事实

一次提问触发 `parent_dynamic_delegation` 路由：父 Agent 识别出 4 个主题（RGB 灯效 / 消息中心 / 菜谱 ChefAI / TTS），派发 4 个子 Agent 并行调查，父 Agent 汇总。日志 `batch finished tasks=4 completed=1 failed=3`。

| 时间 | 事件 | 日志证据 |
| --- | --- | --- |
| 11:12:36 | 父 Run 启动，`maxSteps=8`，`timeout=7m55s` | `loop_execution.go:67` |
| 11:12:40 | 父 step 1，21.6s，首个 content 19.6s 才出现 | `loop_turn.go:190` |
| 11:13:36 | `delegate_investigation` 派发 4 个子任务 | `tool_executor.go:114` |
| 11:13:37 | 4 个子 Agent 并发启动，`maxSteps=6`，`timeout=2m29s` | `loop_execution.go:67` |
| 11:14:05 | 2 个子 Agent `failed`：`input_tokens requested=48731 available=42283` | `executor.go:1550` |
| 11:14:20 | 1 个子 Agent `failed`：`input_tokens requested=39912 available=20766` | `executor.go:1550` |
| 11:14:44 | RGB 子 Agent `partial`：`reasoning_tokens=7033 visible_output_tokens=0 finish_reason=length`（step 3 耗时 54.97s） | `answer_generation.go:373` |
| 11:14:44 | 批次结束 `tasks=4 completed=1 failed=3`，elapsed=1m8s | `executor.go:546` |
| 11:15:49 | 父 step 3，1m04s，首个 content 1m00s 才出现 | `loop_turn.go:190` |
| 11:17:02 | 父最终答案 `reasoning_tokens=8192 visible_output_tokens=0 finish_reason=length`，强制收敛 | `loop_turn.go:358` |
| 11:19:02 | `exact-answer contract unsatisfied violations=2`，answerLen=10592 | `answer_contract.go:812` |

## 3. 根因分析

### 3.1 推理模型未识别 → 结论预算被推理吃光（症状 1 partial / 2 / 4）

`deepseek-v4-flash` 是推理模型，但 [capability.go:102](../../internal/llm/capability.go#L102) 的 `isOpenAIReasoningModel` 只认 `o1/o3/o4/gpt-5` 前缀，`deepseek-v4-flash` 落进默认保守 profile（`max_tokens` + 无推理控制）。答案阶段 [loop.go:108](../../internal/agent/execution/loop.go#L108) 用 `WithoutReasoning()` 试图关推理，但因 wire field 为 `none` 不生效，模型照常推理。

结果：结论预算（`LLMConclusionMaxTokens`，日志反推 4096）全部被不可见推理 token 消耗，`visible_output_tokens=0`、`finish_reason=length`，服务端被迫走 deterministic conclusion。最终答案没有由模型按 exact-answer contract 产出，[answer_contract.go:812](../../internal/agent/execution/answer_contract.go#L812) 记录 2 处违约后走服务端保守恢复。

> 本根因与 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md` 第 4 条结论完全一致（「子 Agent 本次故障主要是 completion 上限被 reasoning 消耗殆尽」）。09-08 复现说明该提案尚未落地或配置值未更新。

### 3.2 子 Agent 累计输入预算耗尽（症状 1 failed）

`DefaultDelegationMaxChildInputTokens = 96000` 是**累计**输入预算。子 Agent 每步重发整段已增长历史 + 冗长工具结果，context 从 4619 字符涨到 124934 字符（约 20~60k token），累计输入近乎二次增长，4~5 步即打穿。错误来自 [model_call.go:147](../../internal/agent/execution/model_call.go#L147)。

> 本根因与 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`（子 Agent 缺预检索喂料、调查预算偏紧）及 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`（child 累计 quota 与 shared ledger 双重限制）一致。

### 3.3 推理模型固有慢 + context 无界增长（症状 2 的剩余部分）

推理模型每步 20~70s 都在产推理 token，且随 context 变重而恶化（父 context 66k→87k 字符，单步从 29s 涨到 70s）。最终答案那 70s 是纯浪费——烧了 8192 推理 token 后 `finish_reason=length`，零可见输出。

### 3.4 前端流式渲染热点（症状 3，本提案新增）

三个叠加因素，独立于后端：

1. **全量重解析 O(n²)**：[qa/index.vue:2248](../../../codeloom/web/src/views/qa/index.vue#L2248) 的 `streamingSegments` 每 80ms 对整段已增长文本重新 `parseMarkdownSegments`（代码注释已自认是「#2 streaming hotspot」）。
2. **mermaid 流式重渲染**：[MermaidDiagram.vue:265](../../../codeloom/web/src/components/MermaidDiagram.vue#L265) 的 `watch(code, render)` 在码块增长期间反复渲染，且所有实例共享一条模块级串行 `renderChain`（[MermaidDiagram.vue:78](../../../codeloom/web/src/components/MermaidDiagram.vue#L78)），4 张图串行排队。
3. **100ms 高频重绘**：[qa/index.vue:1273](../../../codeloom/web/src/views/qa/index.vue#L1273) 的 `llmTimingTimer` 每 100ms 改 `llmTimingNow`，`liveRenderVersion` 又把它算进 streaming turn 的 `v-memo` key，导致整个流式 section 每 100ms 失效重渲。

> 注意：症状 4（答案「看似啥都有但格式散」）后端已输出正确的 `## 1/2/3/4` 分节（见 09-08 日志 answer 段），属前端渲染保真度问题，与 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md` 一致，不重复展开。

## 4. 与既有提案的关系

| 症状 | 根因 | 既有提案 | 本提案角色 |
| --- | --- | --- | --- |
| 子 Agent partial / 父答案 0 可见 | 推理模型未识别、结论预算被 reasoning 吃光 | `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md` | 补充 09-08 复现证据与具体数值（4096 vs 7033/8192） |
| 3 个子 Agent failed | 子 Agent 累计输入预算耗尽 | `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`、`investigation-run-budget-and-large-context-governance-proposal.zh-CN.md` | 确认复现，不重复方案 |
| 答案格式散 | 前端渲染保真度（后端已正确分节） | `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md` | 确认一致，不重复方案 |
| 前端流式卡顿 | 全量重解析 + mermaid 串行重渲 + 100ms 重绘 | **无** | **本提案新增** |

## 5. 方案设计

### 5.1 前端流式渲染治理（本提案新增，P3）

三个正交改动，消除长流式回答期间的卡顿：

1. **增量/分块解析替代全量重解析**：`streamingSegments` 不再对整段已增长文本每 80ms 重解析，改为只解析新增 delta，或按 viewport 懒渲染，消除 O(n²)。
2. **mermaid 码块稳定后再渲染 + 懒渲染**：`watch(code, render)` 改为 debounce（码块停止增长才 render），并懒渲染（滚动到可视区才 render），避免 4 张图串行排队阻塞主线程。
3. **计时显示去高频重绘**：`llmTimingTimer` 不再每 100ms 改 `llmTimingNow` 并把它塞进 `v-memo` key；改为只更新时长文本节点，或显著降低刷新频率。

### 5.2 结论预算数值收口（补充既有推理治理提案，P0）

1. 落地 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md` 的 provider capability 映射（识别 DeepSeek 推理模型 + 阶段化 reasoning）。
2. **结论预算按推理留余量**：`LLMConclusionMaxTokens` 从 4096 提到 16384~32768（推理本身要 7~8k，可见答案再留 8k+）；更稳的做法是结论预算叠加「推理余量」常量，而非复用一个可见答案值。
3. 答案阶段真正降档推理（若网关支持 `reasoning_effort: low`），取代当前无效的 `WithoutReasoning()`。

### 5.3 子 Agent 预算（引用既有提案，P1）

落地 `qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md` 的三项机制（父预检索种子注入、小幅加大调查预算、gap chase 四道闸门），并按 `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md` 统一到 shared ledger 语义。本提案不重复设计。

## 6. 关键改动文件

| 文件 | 改动 | 归属 |
| --- | --- | --- |
| `Nasuta/internal/llm/capability.go` | `isOpenAIReasoningModel` 增加 DeepSeek 推理前缀 | 引用推理治理提案 |
| `Nasuta/internal/agent/execution/loop.go` / `definition/run.go` | 结论预算叠加推理余量 | 引用推理治理提案 |
| `Nasuta/internal/agent/delegation/executor.go` | 子预算 + 预检索种子 | 引用子预算提案 |
| `codeloom/web/src/views/qa/index.vue` | `streamingSegments` 增量解析、计时去高频重绘 | **本提案** |
| `codeloom/web/src/components/MermaidDiagram.vue` | mermaid 稳定后渲染 + 懒渲染 | **本提案** |

## 7. 影响与风险

- **成本**：上调结论预算增加输出 token 成本，需在 settings 表显式确认可接受；与推理治理提案的成本结论一致。
- **兼容性**：识别推理模型只影响已识别的推理模型的 wire 字段，普通模型走原路径。
- **前端**：增量解析与懒渲染需保证最终态与流式中间态的渲染一致性，避免引入「分节在流式中间态丢失」的回归（回归锚点见 `qa-delegation-event-projection-and-answer-rendering-proposal.zh-CN.md`）。

## 8. 实施顺序

1. 读 MySQL settings 表确认 `LLMConclusionMaxTokens` / `LLMAnswerMaxTokens` / `DelegationMaxChildInputTokens` 实际值，验证 DeepSeek-v4 网关推理语义。
2. 落地前端流式渲染治理（本提案新增，独立可交付）。
3. 落地推理治理提案的 capability 映射 + 结论预算余量（P0）。
4. 落地子 Agent 预算提案（P1）。

## 9. 验证与验收

- 复现同一条多主题提问，核对日志不再出现：`finish_reason=length` 且 `visible_output_tokens=0`、`input_tokens requested=... available=...`、`exact-answer contract unsatisfied`。
- 目标结果：`batch finished tasks=4 completed=4 failed=0`，单轮时长显著下降。
- 前端：Performance 面板确认长流期间主线程不再被 markdown/mermaid 重解析长时间占用，且流式中间态与最终态的分节渲染一致。
- 回归：`go build ./... && go vet ./... && go test -race ./...`（Nasuta 侧），codeloom 前端构建通过。
