# 子调查种子 Token 计量与压缩下限治理（单位统一 · 抽取器可用下限 · 准入可辨识）

状态：草案
作者：wangdequan
日期：2026-09-10
关联事项：续接 [`qa-multi-entity-evidence-chain-governance.zh-CN.md`](qa-multi-entity-evidence-chain-governance.zh-CN.md) 的阶段 3（预算与压缩边界）；触发运行 `run_0267ea7ecd24c54d3d85b6c4`，日志 `codeloom/logs/all.log`（同日 `all-2026-09-10.log` 第 144312-144471 行为同一 run）
目标版本：可选

## 1. 摘要

本提案是 `qa-multi-entity-evidence-chain-governance` 阶段 3 的**定位修正与落地补充**。同一天的另一次运行（`run_0267ea7ecd24c54d3d85b6c4`，同样是「rgb 灯效、消息中心、菜谱、tts」四业务提问）暴露了阶段 3 尚未覆盖的五个缺陷，其中两个与该提案 §3.5 的既有诊断**不同**。

关键差异：本次运行中 `ErrBudgetExceeded` 出现 **0 次**，`RequireWithin` 的 1-token 严格比较**未被触发**。菜谱子 Agent 的失败不是输出预算判定，而是 `ensureInputBudget` 在首次 provider 调用前的**输入超窗**（`steps=0 answerLen=0`）。其机制根因是种子裁剪按**字节**估算、准入校验按**token**计量，两把尺子不一致，中文内容下字节启发式严重低估。

本提案计划：统一种子裁剪与准入校验的计量单位、把工具结果压缩下限抬到结构化抽取器可工作的阈值、让 `deny_budget` 与真空结果可辨识，并修正两处无标记截断。预期实现多实体提问下无子 Agent 因输入超窗零步失败、已获取证据不再被压缩到 1.7% 保留率、"未检索"与"检索无果"在下游可区分。

## 2. 背景

### 2.1 业务与技术背景

CodeLoom 通过 QA agent 回答跨服务链路问题。父 Agent 识别实体后 `delegate_investigation` 派发子调查，子 Agent 在受限窗口内做深挖并回报结构化 `investigation.report`，父 Agent 归并为最终答案与流程图。

多实体提问是该链路压力最大的场景：父层预检索的证据要按实体切分喂给 N 个子 Agent，每个子 Agent 的单次请求窗口独立受限（`DefaultDelegationMaxChildContextTokens = 51200`），远小于父层的 256000。

### 2.2 当前实现

- 种子注入：`defaultSeedContext` / `selectContext`（`internal/agent/delegation/executor.go`）按 `remainingBytes := int(maxTokens * 4 / 2)` 裁剪；
- 准入校验：`ensureInputBudget`（`internal/agent/execution/prompt_context.go:253`）按 `estimateInputTokens` 判定；
- 工具准入：`admitToolCallDecision`（`internal/agent/execution/tool_admission.go`）在执行前按剩余 token 放行/收窄/拒绝；
- 上下文压缩：`compactAnswerContext` → `compressToolResults`（`internal/agent/execution/answer_context_compaction.go`），floor 常量 `96 / 256 / 16`。

### 2.3 为什么现在需要修改

`qa-multi-entity-evidence-chain-governance` 阶段 3 的退出条件是「1-token 误判归零、结构化保留率达标」。本次运行显示：即使该阶段完成，四业务提问仍会失败——因为失败路径不经过 `RequireWithin`，而阶段 3 的改动四也未触及种子裁剪的单位换算。若不补齐，阶段 3 上线后同类问题仍会复现，且现象与既有诊断不符，会造成二次误判。

### 2.4 范围与非目标

**范围：** 子调查种子裁剪的计量单位；工具结果压缩下限；`deny_budget` 结果的可辨识性；memory 抽取路径与 answer 日志的截断标记。

**非目标：**

- 不调整 `outputReserve = 18000`。该值占 51200 窗口 35% 看似偏高，但注释已说明推理模型在可见输出前消耗大量隐式思考 token；调整需先有推理 token 实测数据，不在本提案凭判断改动；
- 不改 `RequireWithin` 输出容差。那是 `qa-multi-entity-evidence-chain-governance` 改动四的范围，本次运行未触发，保持该提案的结论不变；
- 不处理 CodeLoom 侧 `list_apis` 对 `hsds-scene` 返回空的索引覆盖缺口。该问题与本提案的预算/压缩根因完全独立（详见 §3.6），应单独立项。

## 3. 问题

### 3.1 问题描述

用户提问「帮我分析一下 rgb 灯效、消息中心、菜谱、tts 这几个业务的流程是什么样的」。父 Agent 正确识别 4 实体并派发 4 个子调查，但最终答案中：

- 菜谱：完全无内容，答案原文「该主题的深度调查在执行中未完成」；
- RGB 灯效：仅剩「服务存在 + 实体类命中」，端到端链路全部标注未解决；
- RGB 流程图：12 个节点中 6 个是同义重复，10 条 `open_hops` 每条重复两遍；
- 消息中心、TTS：链路可用，但关键跳转（推送通道、云端合成入口）标注未解决。

### 3.2 根因一：种子裁剪按字节、准入校验按 token（菜谱零步失败）

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 菜谱子 Agent `steps=0 answerLen=0`，无任何产出 | `all.log:4985` |
| 直接原因 | `ensureInputBudget` 判定 `37314+18000+2560 = 57874 > 51200` | `prompt_context.go:264` |
| 机制根因 | 种子裁剪用 `maxTokens*4/2` 字节启发式，与 token 计量不一致 | `executor.go:3030`、`executor.go:3109` |

根因链路：

```text
seedContextTokens(51200, 18000) = 51200-18000-2560 = 30640 tokens   ← 预算算对
remainingBytes = 30640 * 4 / 2 = 61280 bytes                        ← 换算错（假设 2 bytes/token）
菜谱种子实际注入 50234 chars → 落在 61280 以内 → 放行
estimateInputTokens 实测 = 37314 tokens > 30640                     → 超 6674 → 首次 provider 调用前失败
```

`tooloutput.runeTokenUnits` 自身即声明非 ASCII 字符的 token 成本是 ASCII 的 6 倍（`nonASCIITokenUnits = 66` vs `asciiTokenUnits = 11`，`tokenUnits = 30`）。中文为主的证据内容下，2 bytes/token 是系统性低估。

四个子 Agent 的初始上下文实测，可见风险随注入量单调上升：

| 子任务 | contextChars | 结果 |
| --- | --- | --- |
| RGB 灯效 | 4520 | 跑完，预算第 2 步耗尽 |
| TTS | 33509 | 跑完，`remainingToolTokens` 降至 858 |
| 消息中心 | 34467 | 跑完，`remainingToolTokens` 降至 865 |
| 菜谱 | **50234** | **steps=0，超窗崩溃** |

补充：`estimateInputTokens` 还包含 system prompt 与 tool schema，而 `seedContextTokens` 只按证据内容算预算。因此单位统一后仍需一道派发前断言，二者不可互相替代。

### 3.3 根因二：压缩下限 96 tokens 低于结构化抽取器可工作阈值

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 已成功检索的证据在答案中消失 | `all.log:5187` `5567->96`、`5370` `8719->96` |
| 直接原因 | `oldToolResultFloorTokens = 96`，触发 `head-tail-fallback` | `answer_context_compaction.go:17` |
| 机制根因 | 96 token 预算下 `chunkTarget` 被 clamp 到 64，`pack` 装不进任何 chunk | `compressor.go:125-134` |

根因链路：

```text
budget = 96
chunkTarget(96): 96/3 = 32 → clamp 到 64
buildJSONChunks(content, 64) 的 chunk + 信封 > 96 → pack 失败
→ fallbackResult → truncate 头尾各 48 token 硬切
→ 保留率 96/5567 = 1.7%
```

这不是"压缩上限调小了"，而是**结构化抽取器在该预算下没有工作空间**，必然退化为自由文本头尾截断。`ChunkCoverage: "partial"` 已被正确计算（`compressor.go:92`）但无人消费，下游无从得知这条证据只剩碎片。

实际损失可核验：三次成功的 `search_code` 命中了 Android 路由常量 `RequestPathUtil.kt`（含 `/device/rgb-effect/all/list` 与编辑路径）与 `DeviceRgbEffect.java`/`DeviceRgbEffectDto.java`；唯一成功的 `search_runbooks`（4575 B）返回 `doc-bce0279146a8fc5e`（`event-flow-rgb-effect.md`，`trustTier=85`），其排障表点名 `hs-iot-hsmf-mobile-gateway-*` 与 `/device/rgb-effect/edi…`。而最终答案把「App/后台灯效入口及其保存/发布接口」标为未解决——**证据已在上下文中，被压缩丢弃**。

### 3.4 根因三：`deny_budget` 与真空结果不可辨识

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 大量跳转标注"未取证" | 答案 `未解决` 条目 |
| 直接原因 | 26 次工具调用中 17 次被 `deny_budget` 拒绝 | `tool_admission.go:95` |
| 机制根因 | 拒绝信封与真空结果都是 `evidence:[]`，子 Agent 无法区分 | `tool_admission.go:189-199` |

本次运行的权威口径是 26 条 `loop_turn.go:602` trace-persist 记录（`tool_calls=` 日志行存在重复打印，不能用于计数）：

| | 数量 |
| --- | --- |
| 总调用 | 26 |
| `deny_budget` 拒绝（未触及检索层） | **17（65%）** |
| 执行且有数据 | 8 |
| 真·索引空结果 | 1 |

按工具的空率：

| 工具 | 调用 | 空 | 有数据 | 空率 |
| --- | --- | --- | --- | --- |
| `list_apis` | 5 | 5 | 0 | **100%** |
| `trace_deps` | 1 | 1 | 0 | **100%** |
| `get_symbol` | 1 | 1 | 0 | **100%** |
| `search_runbooks` | 5 | 4 | 1 | 80% |
| `get_service` | 6 | 3 | 3 | 50% |
| `search_code` | 6 | 3 | 3 | 50% |

`trace_deps` 与 `get_symbol` 各自唯一一次调用均被拒——**依赖图与符号级证据在本次问答中完全缺席**，而这两者恰是"逐跳验证跨服务调用"最需要的工具。从未被调用：`trace_calls`、`check_docs`、`index_stats`、`observe_logs`。

拒绝信封（17 条中的典型，171 字节）：

```json
{"action":"deny_budget","declaredMaxTokens":4096,"evidence":[],"reason":"declared_result_exceeds_budget","remainingToolTokens":1702,"scope":{"source_kind":"","target":""}}
```

字节大小差异（170/171/198/220/221）纯为 `remainingToolTokens` 与 `declaredMaxTokens` 的位数变化，全部是同一种拒绝。`remainingToolTokens` 在 run 内单调递减（1702→1635→1568→1501；1279→1212→1145→1059），而 `declaredMaxTokens` 恒为 4096——**一旦剩余降到 4096 以下，后续所有调用无条件被拒**，与它们本可检索到什么无关。

子 Agent 已自行识别该状况并写入 `uncertainties`（原文「后续所有 get_symbol、list_apis、search_code 调用因检索预算耗尽被拒绝（declared_result_exceeds_budget）」），但该信息未进入 `evidence_state`，最终仍渲染为"未解决"。

### 3.5 根因四：两处无标记截断

**4a — memory 抽取路径静默熔接 JSON。** `submission.go:279` 用 `tooloutput.TruncateContent(result.Text, 2000)`，其内部 `truncateWithoutMarker`（`token.go:70`）头尾各半拼接、不插标记。切口落在 JSON 字符串值内部：

```
{"id":"node_63019c8ce5af1523","label":"RGB 灯；服务端合成音频回流设备并进入播放队列的时序与协议。
```

`"RGB 灯效设备"` 被切断并焊接到 TTS 散文上，RGB 与 TTS 两个 flowir 块被静默熔接。下游 memory 抽取 LLM 无任何信号可判断这是伪影。本次产出的 memory 恰好无害（`user:current-focus`），但机制对任何超 2000 token 的答案都不成立。

注：`TruncateContent` 的文档注释写明适用于「coverage metadata is stored separately」的场合，而 memory 抽取路径并无另存的 coverage 元数据——属于误用。

**4b — answer 日志 4000 runes 上限妨碍事后归因。** `loop_execution.go:344` 用 `platform.TruncateForLog(answer, 4000)`：

| Run | 日志 | 实际 `answerLen` | 丢失 |
| --- | --- | --- | --- |
| RGB | 4000 | 5343 | ~25% |
| 消息中心 | 4000 | 9798 | **~59%** |
| TTS | 4000 | 10506 | **~62%** |
| RGB chase | 4000 | 4521 | ~11% |

四条均精确截至 4003 runes（含 `...`）。同日 `all-2026-09-10.log` 为同样截断，磁盘无完整副本。后果：本次归因中「子 Agent 原始 flow 有几条边」无法查证——`DELEGATION_SETTLED`（`all.log:7508`）保留 `summary`/`flow`/`open_hops`/`uncertainties`，但**丢弃 `findings[]`**，每条 claim 的 `evidence.reference` 仅存在于被截断的 answer 行中。

### 3.6 与既有提案的关系与差异

`qa-multi-entity-evidence-chain-governance` §3.5 的诊断为「严格 `>` 比较 + head-tail 截断」，根因链路为「输出 16603 tokens、可用 16602 → `RequireWithin` 严格 > → `ErrBudgetExceeded` → status=failed」。

本次运行（`run_0267ea7ecd24c54d3d85b6c4`）核验结果：

- `ErrBudgetExceeded` / `budget exceeded` 出现 **0 次**；
- 唯一失败是 `QA context exceeds configured window before provider call`（输入维度，非输出维度）；
- 该提案触发运行 `run_e6c2ffa9f9a112c2e0088a3c`、`run_2c5be2f356a2ebdfa162acf3` 在本日志中出现 0 次——是不同运行。

结论：两份诊断**互补而非冲突**。输出维度容差（该提案改动四）仍有必要；但输入维度的单位不一致是独立缺陷，且是本次四业务提问失败的实际路径。压缩问题双方都识别到，本提案补充了"96 tokens 使抽取器失去工作空间"这一机制层解释——该提案改动四的"按工具类型保关键字段"若在 96 预算下实施仍会失败，因为信封本身即超预算。

另有一项独立于本提案根因的发现：`list_apis` 对 `service="hsds-scene"` 在 3.8ms 内返回 `{"matches":[]}`，而 `get_service` 同时确认该服务存在（`hsds-scene-provider`、spring-boot、端口 4015、入口 `HsdsSceneApplication.java`、confidence 0.9）。3.8ms 响应说明索引被查询且确实为空——CodeLoom 侧 API 端点索引对该 repo 未落数据。属索引覆盖缺口，应单独立项，不在本提案范围。

### 3.7 影响

- **用户影响：** 多实体提问中弱信号实体完全无答案（菜谱），已检索到的入口/路由证据被渲染为"未解决"，答案可信度低于实际证据水平；
- **业务影响：** 流程图出现同义节点重复（RGB 12 节点中 6 个重复）与 `open_hops` 翻倍，图不可直接用于交付；
- **系统影响：** 65% 的工具调用消耗了调度与准入开销却从未触及检索层；成功检索的结果以 1.7% 保留率进入模型；
- **工程影响：** 计量单位在种子层与准入层不一致，缺陷无法通过任一侧的单元测试发现；日志截断使同类事故的事后归因存在不可查证区间。

## 4. 问题出现的场景

### 4.1 典型场景

一次提问点名 N≥3 个业务，父层预检索产出较大证据集，其中某实体的证据显著多于其他实体：

1. 父层按实体切分种子，强信号实体（本次为菜谱，50234 chars）的种子最大；
2. `defaultSeedContext` 按 61280 字节上限判定"够"，放行；
3. 子 Agent 编译请求，`estimateInputTokens` 计入 system prompt + tool schema + 种子 = 37314 tokens；
4. `ensureInputBudget` 判定超窗，返回错误，`steps=0`；
5. 父层收到 `status=failed`，最终答案该实体段落为空。

其余实体虽未超窗，但种子已占据大部分窗口，`remainingToolTokens` 迅速降至 4096 以下，后续工具调用全部被 `deny_budget` 拒绝。

### 4.2 边界场景

- **纯 ASCII 内容：** 字节启发式接近正确（英文约 4 bytes/token，`*4/2` 留 2 倍余量），缺陷不显现——这解释了为何该 bug 长期未被发现；
- **中文/CJK 内容：** 系统性低估，注入量越大越危险；
- **单实体提问：** 种子无需切分、通常远小于窗口，不触发；
- **恰好压到 96 tokens：** 若工具结果原本就 ≤96 tokens，`toolResultCandidates` 因 `tokens <= emergencyToolResultFloor` 跳过，不受影响；只有大结果被压到 floor 时才退化。

### 4.3 复现步骤

1. 构造一个中文为主、单实体证据超过 30640 tokens 的多实体提问；
2. 观察 `[agent] run <child> request compiled ... contextChars=` 是否显著超过 `seedContextTokens * 2`；
3. 观察是否出现 `QA context exceeds configured window before provider call`，且 `steps=0`；
4. 观察 `answer context tool result compacted ... tokens=N->96`，确认 `strategy=head-tail-fallback`；
5. 统计 `deny_budget` 出现次数与 `loop_turn.go:602` trace 行数之比。

## 5. 如何修改

### 5.1 修改原则

- **一个概念一把尺子：** 种子裁剪与准入校验必须使用同一个 token 估算器，不得各自换算；
- **下限必须让机制可工作：** 压缩 floor 不是"尽量小"，而是"抽取器能产出结构化结果的最小值"；
- **拒绝必须可辨识：** 未执行的检索与执行后无果，在下游必须能区分；
- **截断必须留痕：** 任何送入模型的截断都要带标记，日志截断不得使关键归因不可查证。

### 5.2 目标流程

```text
父层实体切分
  → seedContextTokens 计算 token 预算
  → 按 token 裁剪（同一估算器）           ← 改动一
  → 派发前断言 input+reserve+safety ≤ window ← 改动一
  → 子 Agent 执行
      → 工具准入：拒绝时标注 retrieved=false ← 改动三
      → 压缩：floor ≥ 抽取器工作阈值        ← 改动二
      → coverage=partial 传递到 evidence 侧  ← 改动二
  → 回报 / 归并
memory 抽取：带标记截断                      ← 改动四
answer 日志：全文可查                        ← 改动五
```

### 5.3 详细改动

#### 改动一：种子裁剪改用 token 计量 + 派发前断言

**方案：**

`defaultSeedContext` 与 `selectContext`（`executor.go:3030`、`executor.go:3109`）删除 `remainingBytes := int(maxTokens * 4 / 2)`，改为按 token 记账：

```go
remainingTokens := int(maxTokens)
// ...
filtered.Content = tooloutput.Truncate(filtered.Content, remainingTokens)
remainingTokens -= tooloutput.EstimateTokens(filtered.Content)
```

使用带标记的 `Truncate` 而非 `TruncateContent`，使子 Agent 能感知种子被裁剪（与改动四同源）。

派发前增加断言：种子组装完成后，按子 Agent 的实际 messages + tools 跑一次 `input + outputReserve + safety ≤ window` 校验；不通过则继续裁剪种子直至通过，而非让子 Agent 在 step 0 失败。

**约束与失败行为：** 裁剪到最小仍不通过时（理论上仅当 system prompt + tool schema 本身超窗），记录 `child_seed_unfittable` 并派发无种子的子 Agent——比零步失败保留更多能力。

#### 改动二：压缩 floor 抬到抽取器可工作阈值 + coverage 传递

**方案：**

`answer_context_compaction.go:17-19` 调整：

```go
oldToolResultFloorTokens    = 384  // 96  -> 384
recentToolResultFloorTokens = 768  // 256 -> 768
```

依据：`chunkTarget` 需 budget ≥ 1800 才不被 clamp 到 600；退一步，装入一个 64-token chunk 加信封的硬下限约 256，384 才有实际抽取价值。

同时让 `compressToolResults` 在 `compressed.Strategy == strategyFallback` 时把该事实写入证据元数据，使下游能区分"完整证据"与"仅存头尾碎片"。`ChunkCoverage` 已计算，只需接通消费侧。

**约束与失败行为：** 抬高 floor 会使 `needed` 更难满足，压不到 target 时沿用现有 `hardOverflow` → `emergencyToolResultFloor` 降级路径。**本改动必须在改动一之后实施**，否则只是把失败点从压缩挪到准入校验。

#### 改动三：`deny_budget` 结果可辨识

**方案：**

`toolAdmissionExecution`（`tool_admission.go:189`）payload 增加不可误读字段：

```go
payload := map[string]any{
    "action":    decision.Action,
    "reason":    decision.Reason,
    "retrieved": false,  // 检索层从未被调用
    "retryable": true,   // 收窄参数后可重试
    "hint":      "refused before retrieval; narrow the query or request fewer items",
    // ... 原有字段
}
```

下游：`retrieved:false` 的调用不计入"已尽力检索"判定，避免把"未检索"渲染为"检索无果"。

**约束与失败行为：** 仅新增字段，不改 `action`/`reason` 语义，对未消费新字段的既有代码无影响。

#### 改动四：memory 抽取路径改用带标记截断

**方案：**

`submission.go:279`：

```go
answer := tooloutput.Truncate(result.Text, 2000)  // 原 TruncateContent
```

`Truncate` 已存在且行为正确（`token.go:22`），插入 `... [tool output truncated: original ~N tokens, M lines] ...`。

**约束与失败行为：** 标记本身占 token，`truncate` 已处理 `markerTokens > maxTokens` 的退化路径。建议同时审查 `TruncateContent` 其余调用点是否存在同类误用。

#### 改动五：answer 日志上限

**方案：**

`loop_execution.go:344` 的 4000 runes 装不下 schema-valid 的 `investigation.report`。两个选项：

- 简单：上限提至 16000（覆盖实测最大 10506）；
- 推荐：INFO 级只留摘要 + `answerLen`，全文降至 debug 级单独打印。

倾向后者——生产日志不应常态输出 10k 字符，但排查时必须能取到全文。

**约束与失败行为：** 纯日志改动，无运行时行为影响。

### 5.4 数据结构或接口契约

| 字段 | 位置 | 变更 | 兼容性 |
| --- | --- | --- | --- |
| `retrieved` | 工具准入 payload | 新增 bool | 向后兼容（新增字段） |
| `retryable` | 工具准入 payload | 新增 bool | 向后兼容 |
| `hint` | 工具准入 payload | 新增 string | 向后兼容 |
| `oldToolResultFloorTokens` | 压缩常量 | 96 → 384 | 行为变更，需回归 |
| `recentToolResultFloorTokens` | 压缩常量 | 256 → 768 | 行为变更，需回归 |

无数据库变更，无配置项新增（floor 暂保持常量；若需灰度再提为配置）。

### 5.5 兼容、迁移与回滚

改动一、四、五为缺陷修正，无兼容问题。改动二改变压缩行为，回滚即恢复常量值。改动三仅新增字段。五项改动相互独立，可分别回滚。

## 6. 修改伪代码

### 6.1 核心流程（改动一）

```go
func defaultSeedContext(parent ParentContext, capability agentapi.Capability,
    task agentapi.DelegationTask, maxTokens int64) []agentapi.ContextBlock {

    remainingTokens := int(maxTokens)          // 原: remainingBytes = maxTokens*4/2
    var blocks []agentapi.ContextBlock
    for _, block := range parent.Context {
        if !isSeedBlock(block) || !claimedByTask(block, task) {
            continue
        }
        if remainingTokens <= 0 {
            break
        }
        claimed := cloneContextBlock(block)
        claimed.Content = tooloutput.Truncate(claimed.Content, remainingTokens)
        claimed.ContentHash = hashBytes([]byte(claimed.Content))
        remainingTokens -= tooloutput.EstimateTokens(claimed.Content)
        blocks = append(blocks, claimed)
    }
    return blocks
}
```

### 6.2 关键边界处理（改动一的派发前断言）

```go
// 种子组装后、派发前
for attempt := 0; attempt < maxSeedShrinkAttempts; attempt++ {
    if err := childAgent.ensureInputBudget(messages, tools); err == nil {
        break
    }
    if len(blocks) == 0 {
        log.Warnf("[delegation] child_seed_unfittable task=%d; dispatching without seed", task.index)
        break   // 无种子派发，优于 steps=0 失败
    }
    blocks = shrinkSeed(blocks)   // 减半重裁
    messages = recompile(blocks)
}
```

### 6.3 修改前后对比

| 维度 | 修改前 | 修改后 |
| --- | --- | --- |
| 种子裁剪单位 | 字节（`maxTokens*4/2`） | token（同 `estimateInputTokens`） |
| 超窗子 Agent | `steps=0 answerLen=0` | 裁剪至可派发，或无种子派发 |
| 压缩保留率 | 5567→96（1.7%） | ≥384 tokens，抽取器可工作 |
| 压缩策略 | 必然 `head-tail-fallback` | `structured-extractive-v1` 可生效 |
| 拒绝可辨识 | `evidence:[]`，同真空结果 | `retrieved:false` 明确区分 |
| memory 截断 | 静默熔接 JSON | 带 `[truncated]` 标记 |
| answer 日志 | 4000 runes（丢 62%） | 全文可查（debug 级） |

### 6.4 配置或数据库变更

无。`DefaultDelegationMaxChildContextTokens = 51200` 与 `outputReserve = 18000` 均保持不变（见 §2.4 非目标）。

## 7. 预期的效果

### 7.1 功能效果

- 多实体提问中不再出现子 Agent 因输入超窗零步失败；
- 已成功检索的证据以可用粒度进入模型，入口/路由类证据不再被渲染为"未解决"；
- 下游能区分"未检索"与"检索无果"，`open_hops` 文案可反映真实原因。

### 7.2 可观测性效果

- 种子裁剪日志给出 token 而非字节，与准入校验同维度可比；
- `strategy=head-tail-fallback` 成为异常信号而非常态；
- `retrieved=false` 计数可直接度量准入拒绝对答案完整性的影响。

### 7.3 量化指标

| 指标 | 当前 | 目标 |
| --- | --- | --- |
| 子 Agent 输入超窗零步失败率 | 1/4（本次运行） | 0 |
| 工具结果压缩保留率 | 1.7%（96/5567） | ≥ 384 tokens 或 ≥ 20% |
| `head-tail-fallback` 占压缩次数比 | 4/5（本次运行） | < 20% |
| `deny_budget` 占工具调用比 | 65% | 观测项（改动一后应自然下降） |

### 7.4 不应发生的变化

- 单实体提问的种子注入量与答案质量不应变化；
- 纯 ASCII 内容的裁剪结果不应显著变化（字节启发式在该场景本就接近正确）；
- `RequireWithin` 输出维度判定不应改变（属既有提案范围）。

## 8. 测试与验收

### 8.1 单元测试

- `defaultSeedContext` / `selectContext`：给定中文内容与 token 预算，断言 `EstimateTokens(结果) ≤ maxTokens`（当前实现会失败）；
- `chunkTarget` / `Compress`：断言 budget=384 时 `Strategy != strategyFallback`，budget=96 时记录当前退化行为作为回归基线；
- `toolAdmissionExecution`：断言拒绝 payload 含 `retrieved:false`；
- `TruncateContent` vs `Truncate`：断言后者输出含标记。

### 8.2 集成测试

- 构造中文为主、单实体证据 > 30640 tokens 的四实体提问，断言 4 个子 Agent 均 `steps > 0`；
- 断言每个被点名实体在最终答案中有非空段落。

### 8.3 回归场景

- 单实体提问（无委派）；
- 纯 ASCII 证据的多实体提问；
- 工具结果本身 ≤ 96 tokens（不应进入压缩候选）；
- `qa-multi-entity-evidence-chain-governance` 阶段 1、2 的既有回归集。

### 8.4 验收标准

1. §8.1 全部单元测试通过，且改动前对应测试确实失败（证明测到了真缺陷）；
2. §8.2 集成场景 4/4 子 Agent 有产出；
3. `go build && go vet && go test -race ./...` 全绿；
4. 复现 `run_0267ea7ecd24c54d3d85b6c4` 的同类提问，菜谱段落非空、RGB 段落含至少一条 verified 跳转。

## 9. 风险与控制

| 风险 | 等级 | 控制 |
| --- | --- | --- |
| 抬高压缩 floor 后上下文更易压不下去，失败点从压缩挪到准入 | 高 | **必须在改动一之后实施**；保留 `hardOverflow` → emergency floor 降级路径 |
| 种子按 token 裁剪后注入量下降，弱信号实体证据变少 | 中 | 派发前断言采用"裁剪至可派发"而非"直接放弃"；观测各实体段落非空率 |
| 派发前断言引入重编译开销 | 低 | 限制 `maxSeedShrinkAttempts`；仅在首次校验失败时触发 |
| 新增 payload 字段被既有下游忽略 | 低 | 纯新增，不改既有字段语义 |
| answer 日志降级后排查信息减少 | 低 | INFO 级保留 `answerLen` 与摘要，debug 级可开全文 |

## 10. 实施计划

五项改动存在依赖，不可乱序。

### 阶段 1：零风险机械修正

- 改动四（`Truncate` 换函数，1 行）、改动五（日志上限）；
- 退出条件：`go test -race` 绿；memory 抽取输入不再出现无标记熔接。

### 阶段 2：根因修正（单位统一）

- 改动一（种子按 token 裁剪 + 派发前断言）；
- 退出条件：§8.1 种子测试、§8.2 集成测试通过；四业务提问无零步失败。

### 阶段 3：准入可辨识

- 改动三（`retrieved`/`retryable`/`hint`）+ 下游消费；
- 退出条件：`open_hops` 能反映"因预算被拒"与"检索无果"的差异。

### 阶段 4：压缩下限（依赖阶段 2）

- 改动二（floor 384/768 + coverage 传递）；
- 退出条件：`head-tail-fallback` 占比 < 20%，且无新增准入失败。

按 `docs/refactor.md` 的 boundary discipline：五项改动为五个独立 commit，一个 concern 一个 commit，不合并。

## 11. 待决策事项

1. **压缩 floor 的具体值。** 384/768 是按 `chunkTarget` 反推的下限。若要让抽取器完全不受 clamp 影响需 budget ≥ 1800，代价是压缩能力大幅下降。需在"保留率"与"可压缩性"之间取值，建议先按 384/768 实测再定。
2. **floor 是否提为配置项。** 目前建议保持常量以便回滚；若需按 provider/窗口分档再提为配置。
3. **改动二的 coverage 传递范围。** 最小实现是写入日志与证据元数据；是否进一步影响 `evidence_state` 的取值，需与 `qa-multi-entity-evidence-chain-governance` 改动三（FlowIR `EntityID`）协调，避免两处同时改 flow 契约。
4. **answer 日志采用"提上限"还是"降 debug 级"。** 倾向后者，但会改变现有排查习惯，需确认。

## 12. 决策摘要

- 本提案是 `qa-multi-entity-evidence-chain-governance` 阶段 3 的定位修正与落地补充，不取代该提案；
- 该提案 §3.5 的输出维度诊断（`RequireWithin` 严格比较）在本次运行未触发，保持其结论；本提案补充输入维度的单位不一致缺陷，是本次四业务提问失败的实际路径；
- 五项改动按 §10 顺序实施，改动二严格依赖改动一；
- `outputReserve = 18000`、`MaxChildContextTokens = 51200`、`list_apis` 索引缺口均为明确非目标。

## 附录 A：提案提交前检查清单

- [x] 根因链路给出层次分解（表面现象 / 直接原因 / 机制根因）与代码位置；
- [x] 每条结论有可核验的日志行号或代码行号；
- [x] 与既有提案的重叠与差异已显式说明（§3.6）；
- [x] 非目标已列明，避免范围蔓延（§2.4）；
- [x] 改动间依赖关系已标注，实施顺序有依据（§10）；
- [x] 待决策事项未在提案内自行拍板（§11）；
- [ ] 评审确认压缩 floor 取值与 coverage 传递范围。

## 附录 B：本次运行核验数据

| 项 | 值 | 来源 |
| --- | --- | --- |
| 父 run | `run_0267ea7ecd24c54d3d85b6c4`，`window=256000`，`steps=2`，`answerLen=12020` | `all.log:2829`、`7513` |
| 子 run 窗口 | `window=51200`，`outputReserve=18000`，`safety=2560` | `all.log:4985` |
| 菜谱失败 | `input=37314`，`37314+18000+2560=57874 > 51200`，`steps=0` | `all.log:4985` |
| 工具调用 | 26 条 trace；17 `deny_budget`；8 有数据；1 真空 | `loop_turn.go:602` 行 |
| 压缩实测 | `5567->96`、`8719->96`，`strategy=head-tail-fallback` | `all.log:5187`、`5370` |
| answer 截断 | 4 条均 4003 runes；实际 5343/9798/10506/4521 | `all.log:5308`、`5323`、`5329`、`5466` |
| `list_apis` 真空 | `service="hsds-scene"` → `{"matches":[]}`，3.8ms | `all.log:5349` |
| gap-chase run | `run_child_chase_8deeae378cdbf12619e84d1c`，`steps=3`，`answerLen=4521` | `all.log:5309`、`5464` |
