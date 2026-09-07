# 委托调查子 Agent 结构化报告生成与恢复治理提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-09-07
关联事项：`run_0ceef70aae0c3cdb4a18bc21`、`del_81765b7dd051ef58c62e207a`；诊断日志 `/Users/dequan.mac/.codex/attachments/2ec402b1-9e6b-4118-af64-2a9f5b2a4962/pasted-text.txt`
相关提案：`qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md`、`investigation-workflow-reliability-proposal.zh-CN.md`、`investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`
目标版本：待评审

## 1. 摘要

本提案用于解决委托调查链路中“子 Agent 已经花费大量时间完成工具调查，却没有产出可采纳的结构化调查报告”的问题。

触发案例中，父 Agent 为回答“分析 RGB 灯效、消息中心、菜谱、TTS 四个业务的流程”这一问题，并发派发了四个 Investigator 子任务。四个子任务都实际完成了工具调用、收集到了真实证据，日志也显示：

```text
tasks=4 completed=4 failed=0
```

但父 Agent 最终只拿到四份近乎为空的 `partial` 报告，摘要全部是固定的降级文案。根因不是工具召回失败，而是调查流程把证据收集与最终 JSON 报告生成挤在同一条完成预算里，模型在最后一轮把 completion 额度全部消耗在 reasoning 上，最终没有任何可见 JSON 内容；随后系统用普通文本兜底，兜底文本回显了任务输入，恢复逻辑又把任务输入 JSON 当作调查报告去校验，最终把“模型没生成报告”这一问题扭曲成了“Schema 校验失败”。

目标执行流程从：

```text
子 Agent 调查与报告共享完成预算
→ 工具结果与 reasoning 持续膨胀
→ 最后一轮 completion 被 reasoning 耗尽
→ visible_output_tokens=0
→ 普通文本兜底并回显任务输入
→ 恢复逻辑抽取任务合同并校验失败
→ 返回空 partial 报告
```

调整为：

```text
调查阶段与报告阶段分离
→ 报告阶段关闭或降低 reasoning，并预留独立可见输出预算
→ 结构化任务失败时生成 Schema 合法的结构化 fallback
→ 恢复器先识别报告形状，拒绝把任务输入当报告
→ 父 Agent 按 task_index 顺序直接输出 1、2、3、4 的分节 Markdown，代码仅校验格式
```

预期实现以下效果：

1. 子 Agent 即使在 reasoning 耗尽 completion 的情况下，也能返回 Schema 合法的 `investigation.report`；
2. 恢复逻辑不再把任务合同误判为调查报告，不再产生误导性的 `additionalProperties` 错误；
3. 调查阶段采集到的证据在报告失败时仍可被父 Agent 观察和采纳；
4. 最终面向用户的回答按 `1、2、3、4` 稳定分条，不再把多个业务主题揉成一段。

## 2. 背景

### 2.1 业务与技术背景

Nasuta 的 QA 调查链路在遇到跨多个业务主题、要求给出流程和依据的问题时，会由父 Agent 动态派发多个 Investigator 子任务。每个子任务接收一份任务合同（task contract），调用受限工具采集证据，最后生成 `investigation.report version 1` 结构化报告，供父 Agent 汇总合成。

当前链路为：

```text
POST /api/qa/ask
→ QA Service 判定并进入 Parent Dynamic Delegation
→ 父 Agent 调用 delegate_investigation
→ 并发启动多个 Investigator Child Agent
→ 每个 Child Agent 调用工具采集证据
→ 每个 Child Agent 生成 investigation.report
→ 父 Agent 汇总 report 与 evidence
→ 父 Agent 生成最终用户答案
```

本次触发案例中，父 Agent 一次派发四个子任务，分别对应 RGB 灯效、消息中心、菜谱、TTS 四个业务。

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/catalog/schema.go`：定义任务合同 Schema 和 `investigation.report version 1` 输出 Schema；
- `internal/agent/catalog/defaults_investigation.go`：定义 Investigator 角色，当前 `investigatorMaxSteps=4`，输出绑定 `InvestigationReportSchemaRef()`；
- `internal/prompts/text/agent/catalog/investigation_report.txt`：约束报告只返回一个 JSON 对象；
- `internal/agent/execution/loop_execution.go`：提供 `deterministicConclusionProse` 普通文本兜底；
- `internal/agent/execution/answer_generation.go`：执行 forced conclusion 与续写；
- `internal/agent/execution/model_call.go`：按阶段限制模型输出预算；
- `internal/agent/definition/result_recovery.go`：失败报告的恢复逻辑；
- `internal/agent/delegation/report.go`：把结构化输出投影为 `DelegationReport`。

当前执行逻辑概括如下：

1. 子 Agent 的前几步主要进行工具调用，最后一步才要求一次性输出完整 JSON 报告；
2. 调查阶段使用的工具结果与推理 token 会持续累积；
3. 报告生成和工具调查共用同一份完成预算，没有为报告预留独立可见输出空间；
4. 报告生成失败后进入 `deterministicConclusionProse`，生成普通文本并把 `state.input.Question` 拼在“问题：”后面；
5. 恢复逻辑从普通文本中抽取到任务合同 JSON，再用 `investigation.report` Schema 校验，触发 `additionalProperties` 错误；
6. 最终生成一份 Schema 合法但内容为空的 `partial` 报告。

### 2.3 为什么现在需要修改

本次修改由线上案例触发：

- 触发时间：`2026-09-06 23:26:26` 至 `23:28:33`（`+08:00`）；
- 触发标识：父 Run `run_0ceef70aae0c3cdb4a18bc21`、委派批次 `del_81765b7dd051ef58c62e207a`；
- 用户问题：`帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的`；
- 直接表现：四个子任务全部 `partial`，父 Agent 只拿到空报告，随后不得不自行补查最薄弱的 TTS 与消息中心入口；
- 影响范围：所有依赖 `investigation.report` 的委托调查子任务，只要模型把 completion 额度消耗在 reasoning 上，就会触发同类降级。

典型日志证据：

```text
provider completion budget exhausted dimension=provider_completion_tokens
requested_completion_limit=4096 output_tokens=4646 reasoning_tokens=4646
visible_output_tokens=0 finish_reason=stop

final-answer generation produced no visible content; forcing conclusion
```

恢复阶段出现：

```text
unexpected additional properties [
  "objective", "focus_facets", "evidence_refs", "max_hops",
  "delegation_id", "parent_run_id", "capability",
  "parent_question_summary", "output_kind", "task_index"
]
```

父 Agent 最终陈述：

```text
四个子调查均未产出可采纳的报告内容
```

### 2.4 范围与非目标

#### 目标

1. 为调查型子 Agent 建立“调查阶段”与“报告阶段”的阶段边界；
2. 为结构化报告建立独立的可见输出预算与 reasoning 控制；
3. 为 `investigation.report` 建立 Schema 合法的确定性 fallback，替代普通文本兜底；
4. 恢复器能够区分任务合同与调查报告，避免误抽取和误导性 Schema 错误；
5. 让证据采集状态与报告生成状态分开可观测；
6. 最终用户答案由父 Agent 按 `task_index` 顺序直接输出为 `1、2、3、4` 的分节 Markdown，代码只校验、不重排。

#### 非目标

1. 不重复设计 `qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md` 已覆盖的 provider 能力映射和四类 token 分层；本提案只定义“结构化输出契约”和“恢复边界”如何使用这些能力。
2. 不通过单纯提高 `MaxOutputTokens`、`MaxSteps` 或 timeout 掩盖根因。
3. 不改变 `investigation.report` 的既有 Schema 字段含义，也不新造平行证据模型。
4. 不在本提案中重新设计父 Agent 的委派调度和批次 deadline。
5. 不为触发案例中的具体业务名、trace 或 delegation ID 写特例。

## 3. 问题

### 3.1 问题描述

**期望行为：**

子 Agent 完成工具调查后，无论成功还是失败，都应返回一个 Schema 合法的 `investigation.report` JSON。必填字段为：

```json
{
  "focus": "code|runtime|docs|web|memory",
  "summary": "string",
  "findings": [],
  "gaps": [],
  "covered_evidence_goals": [],
  "unresolved_evidence_goals": []
}
```

`findings` 中的每一条应包含 `claim`、`evidence_goal_ids`、`evidence`、`confidence`；对于流程类任务还应输出紧凑的 `flow`。报告必须只返回一个 JSON 对象，不能包含 markdown 或正文。

**实际行为：**

模型在最后一轮没有产生任何可见 JSON 内容（`visible_output_tokens=0`），系统随后用普通文本兜底，文本中还回显了原始任务输入；恢复逻辑从该文本中抽出了任务合同 JSON，用报告 Schema 校验后报 `additionalProperties` 错误，最终只返回一份空 `partial` 报告。

**差异：**

1. 期望的报告是模型生成的、带 findings 和证据的结构化结果，实际得到的是系统生成的固定降级文案；
2. 期望的失败结果是 Schema 合法的报告降级，实际却经过了一次“把任务合同当报告”的次生错误；
3. 期望父 Agent 能区分“证据采集完成”与“报告生成失败”，实际只看到一个几乎为空的 `partial` 报告。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 子任务显示 `completed=4 failed=0`，父 Agent 却拿不到可采纳报告 | `tasks=4 completed=4 failed=0` |
| 直接原因 | 最后一轮 completion 额度被 reasoning 耗尽，没有可见 JSON 输出 | `reasoning_tokens=4646`、`visible_output_tokens=0` |
| 机制根因 | 调查与报告共用同一完成预算，报告生成被推迟到最后一步，且没有结构化专用 fallback | `deterministicConclusionProse` 生成普通文本并回显 `Question` |
| 次生根因 | 恢复逻辑从普通文本中抽取任务合同，再按报告 Schema 校验 | `unexpected additional properties [objective, capability, ...]` |

根因链路：

```text
工具结果与 reasoning 累积
→ completion 预算被最后一轮 reasoning 耗尽
→ 模型没有生成可见 JSON
→ 普通文本兜底并回显任务输入
→ 恢复器抽取任务合同 JSON
→ 报告 Schema 校验失败
→ 返回空 partial 报告
```

本问题不能只通过“增加 retry”或“提高 token 上限”解决，因为：

1. 提高 completion 上限后，模型可能继续把新增额度消耗在 reasoning 上，仍然不产生可见 JSON；
2. 普通文本兜底在结构化输出场景下本来就不成立，重试多少次都会回显任务输入；
3. 恢复器缺少“这是不是一份报告”的判定，才是任务合同被误当报告的真正边界缺陷。

### 3.3 影响

- **用户影响：** 四个业务主题都拿不到有依据的流程结论，只能看到模糊降级结果；
- **业务影响：** 调查耗时长、token 成本高，却没有转化为可用的知识输出；
- **系统影响：** 大量工具调用和 reasoning 的投入被浪费，父 Agent 被迫重复补查；
- **工程影响：** `completed` 与“报告已生成”语义混用，状态不可信，失败原因被 Schema 错误掩盖，难以定位。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：reasoning 耗尽 completion，报告没有可见输出

- **Given（前置条件）：** 子 Agent 已执行多轮工具调用，上下文约 2 万～8 万字符，模型支持 reasoning，`investigatorMaxSteps=4`；
- **When（触发行为）：** 子 Agent 到达最后一步，被要求输出 `investigation.report` JSON；
- **Then（期望结果）：** 模型输出可见 JSON，通过 Schema 校验，父 Agent 采纳报告；
- **But（当前结果）：** 模型把 completion 额度消耗在 reasoning 上，`visible_output_tokens=0`，随后进入普通文本兜底。

当前执行路径：

```text
最后一步模型调用
→ completion 额度被 reasoning 消耗
→ visible_output_tokens=0
→ forcing conclusion
→ deterministicConclusionProse 生成普通文本
→ 文本回显 Question（任务输入）
```

关键证据：

```text
requested_completion_limit=4096
output_tokens=4646
reasoning_tokens=4646
visible_output_tokens=0
```

#### 场景 B：恢复器把任务合同误判为调查报告

- **Given：** 兜底文本中同时包含 `问题：{任务输入 JSON}` 和普通说明；
- **When：** 恢复逻辑从文本中抽取到一个 JSON 对象；
- **Then（期望结果）：** 恢复器识别到该 JSON 是任务合同而非报告，放弃修复，直接生成结构化 fallback；
- **But（当前结果）：** 恢复器把任务合同交给报告 Schema 校验，报 `additionalProperties`。

当前执行路径：

```text
抽取文本中的 JSON
→ 得到任务合同
→ 按 investigation.report 校验
→ unexpected additional properties [objective, capability, ...]
→ 生成空 fallback
```

关键证据：

```text
unexpected additional properties [
  "objective", "focus_facets", "evidence_refs", "max_hops",
  "delegation_id", "parent_run_id", "capability",
  "parent_question_summary", "output_kind", "task_index"
]
```

#### 场景 C：父 Agent 无法分条输出四个业务主题

- **Given：** 父 Agent 收到四个子任务的报告，`task_index` 分别为 0、1、2、3；
- **When：** 父 Agent 生成最终用户答案；
- **Then（期望结果）：** 父 Agent 直接输出 `1、2、3、4` 四节，每节独占一个业务主题，且顺序与 `task_index` 一致；
- **But（当前结果）：** 多个主题被揉成一段，或顺序不稳定，用户难以阅读。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 模型正常输出合法报告 JSON | 通过校验，采纳 | 保持不变 |
| 报告输出为空 | `visible_output_tokens=0` | 普通文本兜底，回显输入 | Schema 合法结构化 fallback，不回显输入 |
| 报告被截断 | `finish_reason=length` 且 JSON 不完整 | 按恢复路径处理 | 有限 JSON 修复；修复失败走结构化 fallback |
| 文本中混有任务合同 | 兜底文本含任务输入 JSON | 误抽取并校验失败 | 识别为任务合同，拒绝按报告修复 |
| 部分目标未覆盖 | 部分 evidence goals 未调查 | 可能仍显示 `completed` | 明确 `partial` 并列出未覆盖目标 |
| 下游工具失败 | 部分工具调用失败 | 信息不完整 | fallback 保留 `gaps` 和工具失败摘要 |

### 4.3 复现步骤

1. 准备一个需要跨多个业务主题、要求输出流程的调查问题；
2. 派发支持 reasoning 的 Investigator 子任务，令其执行多轮工具调用并扩大上下文；
3. 观察最后一步模型调用日志；
4. 可见 `visible_output_tokens=0` 或 `finish_reason=length`；
5. 观察恢复日志，可见 `unexpected additional properties` 中包含任务合同字段；
6. 观察父 Agent 最终只收到空 `partial` 报告，随后自行补查。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 所有改动作用于通用委托调查链路的阶段、预算、恢复和输出契约边界。
2. **保持单一事实源。** `investigation.report` Schema 是子 Agent 输出的唯一契约；任务合同 Schema 是输入的契约，二者不能互相替代。
3. **明确职责边界。** 执行器负责阶段推进，预算层负责可见输出预留，恢复器负责识别和降级，父 Agent 负责有序合成。
4. **失败可诊断。** 区分“执行结束”“证据采集完成”“报告生成完成”“Schema 校验通过”“报告被采纳”五个状态。
5. **兼容与可回滚。** 新增能力通过开关灰度，旧路径在未启用时保持不变。

### 5.2 目标流程

```text
任务合同（输入）
→ 调查阶段：调用工具、采集证据、更新覆盖状态
→ 达到覆盖阈值或预算边界，提前进入报告阶段
→ 报告阶段：关闭/降低 reasoning，工具关闭，预留独立可见输出预算
→ 生成 investigation.report JSON
→ Schema 校验
   ├─ 通过 → 采纳
   └─ 失败 → 报告形状识别
             ├─ 是报告 → 有限修复
             └─ 不是报告 → Schema 合法结构化 fallback
→ 父 Agent 按 task_index 顺序直接输出 1、2、3、4 的分节 Markdown
→ 代码校验输出格式，不通过则重试或标记 partial
```

与当前流程相比，关键变化是：

1. 在“调查”和“报告”之间新增阶段门；
2. 将最终报告的可见输出从普通 completion 预算中分离出来；
3. 将普通文本兜底替换为结构化兜底；
4. 在恢复边界增加报告形状识别；
5. 在父 Agent 输出契约中约束直接生成分节 Markdown，代码只校验、不加工文本。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 阶段分离 | `investigatorMaxSteps=4` 混用调查与报告 | 调查与报告分开，报告阶段强制最后执行 | `internal/agent/catalog/defaults_investigation.go`、`internal/agent/execution/loop_turn.go` | 通过开关灰度 |
| 报告预算 | 与调查共用 completion 预算 | 报告阶段预留独立可见输出预算并关闭/降低 reasoning | `internal/agent/execution/model_call.go`、`internal/agent/execution/answer_generation.go` | 复用 provider 能力映射 |
| 结构化兜底 | `deterministicConclusionProse` 普通文本 | 按输出 Schema 选择结构化 fallback | `internal/agent/execution/loop_execution.go` | 仅调查子 Agent 生效 |
| 恢复识别 | 直接抽取 JSON 并校验 | 先识别报告形状，拒绝任务合同 | `internal/agent/definition/result_recovery.go` | 旧路径保留 |
| 状态语义 | `completed` 含义模糊 | 拆分为 settled / report_valid / adopted 等 | `internal/agent/delegation/executor.go`、日志字段 | 新增字段 |
| 分条输出契约 | 模型自由排版、可能揉成一团 | 输出契约与提示词要求模型直接输出 `1、2、3、4` 分节 Markdown；代码仅校验格式，失败重试或标记 partial | 父 Agent 输出契约、synthesizer 提示词 | 输出兼容 |

#### 改动一：调查阶段与报告阶段分离

**方案：**

将子 Agent 的一次运行拆成两个阶段。调查阶段只允许工具调用和证据采集；当满足以下任一条件时进入报告阶段：

1. 所有 required evidence goals 已覆盖；
2. 剩余目标已确认无法通过当前工具覆盖；
3. 达到调查工具调用上限；
4. 达到调查时间预算。

报告阶段必须满足：工具关闭、使用压缩后的任务合同与证据摘要、关闭或降低 reasoning、使用独立可见输出预算。报告阶段不计入普通调查步骤，或始终保留最后一个专用步骤。

**约束：**

- 不能为了填满 `MaxSteps` 而继续调用工具；
- 报告阶段不得重新打开工具；
- 报告阶段的 reasoning 控制必须通过 provider capability 参数优先，prompt 仅作兜底。

**失败行为：**

- 调查阶段失败时，仍携带已采集证据进入报告阶段；
- 报告阶段失败时，进入结构化 fallback；
- 不允许静默把调查失败伪装成报告成功。

#### 改动二：结构化报告独立可见输出预算

**方案：**

在报告阶段使用独立预算，并确保报告阶段不会因 reasoning 消耗完成额度而失去可见输出空间。

概念结构：

```text
InvestigationBudget（调查阶段）
ReportOutputReserve（报告阶段可见输出）
ReportTimeReserve（报告阶段时间）
```

推荐原则：

1. 整个子任务开始时锁定报告预算；
2. 调查阶段不能消费报告预算；
3. 报告阶段至少保留 2K～4K 可见输出空间；
4. 报告阶段关闭 reasoning 或使用最低 reasoning effort；
5. 调查时间不能占满总超时，必须为报告阶段保留固定时间或比例。

**约束与失败行为：**

- 不通过同时发送多个互相竞争的 token 字段提高上限；
- 如果 provider 无法区分 reasoning 与可见 token，报告阶段必须关闭 reasoning；
- 预算不足时返回明确的 `budget.visible_answer_tokens` 或 `budget.report_reserve` 失败维度。

#### 改动三：结构化任务使用 Schema 合法的 fallback

**方案：**

将兜底分为普通问答与结构化输出两类。当输出 Schema 为 `investigation.report version 1` 时，禁止使用 `deterministicConclusionProse`，改为生成 Schema 合法的 fallback。

最低安全结果：

```json
{
  "focus": "docs",
  "summary": "证据采集已执行，但最终报告生成失败，当前未接受未经验证的结论。",
  "findings": [],
  "gaps": ["最终结构化报告未能完成生成。"],
  "covered_evidence_goals": [],
  "unresolved_evidence_goals": ["goal_a", "goal_b"]
}
```

如果已保存可信的中间 finding，可附带：

```json
{
  "focus": "docs",
  "summary": "已完成部分证据核验。",
  "findings": [
    {
      "claim": "已确认的单句结论",
      "entity_ids": ["entity_1"],
      "evidence_goal_ids": ["entry_path"],
      "evidence": [
        {
          "kind": "runbook",
          "reference": "event-flow-device-push",
          "summary": "对应证据摘要",
          "evidence_id": "ev_2035f548f1dc3"
        }
      ],
      "confidence": 0.9
    }
  ],
  "gaps": ["下游运行时链路尚未验证。"],
  "covered_evidence_goals": ["entry_path"],
  "unresolved_evidence_goals": ["runtime_path"]
}
```

**约束：**

- fallback 必须是合法 JSON；
- 不能回显任务输入；
- 不允许为了填充 findings 而制造未经证据支持的结论。

**失败行为：**

- 报告生成失败时返回 `partial`，`report_origin=deterministic_fallback`；
- 不复用普通文本兜底。

#### 改动四：恢复器识别报告形状

**方案：**

恢复器在拿到 JSON 后，先检查是否具有报告特征字段 `focus`、`summary`、`findings`、`gaps`；如果检测到任务合同特征字段 `objective`、`capability`、`delegation_id`、`parent_run_id`、`task_index`，则判定为任务合同回显，不进入报告修复。

示意逻辑：

```text
if 包含任务合同特征字段:
    判定为 echoed_task_contract，不按报告修复

if 不包含 focus/summary/findings:
    判定为 not_investigation_report，不按报告修复
```

**约束与失败行为：**

- 只修复可从任务合同确定推导的字段，如 `covered_evidence_goals`、`unresolved_evidence_goals`、`focus`；
- 不修复模型没有生成的事实结论或 findings；
- 严格结构化输出优先要求整个回答是 JSON 对象，禁止从长篇普通文本中任意寻找第一个 JSON。

### 5.4 数据结构或接口契约

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `report_origin` | `string` | 结果恢复层 | `model` / `repaired` / `deterministic_fallback` | `model` | 新增，旧记录缺失视为 `model` |
| `settled_tasks` | `int` | 委派批次层 | 已结束的子任务数 | — | 新增观测字段 |
| `report_valid_tasks` | `int` | 委派批次层 | 产出 Schema 合法报告的子任务数 | — | 新增观测字段 |
| `adopted_tasks` | `int` | 委派批次层 | 报告被父 Agent 采纳的子任务数 | — | 新增观测字段 |

状态转换：

```text
pending
  ├─ 调查完成 → evidence_collected
  ├─ 报告生成 → report_generated
  ├─ Schema 校验通过 → schema_valid
  ├─ 被父 Agent 采纳 → adopted
  └─ 失败 → partial / failed
```

不变量：

1. `report_generated` 必须晚于 `evidence_collected`；
2. `adopted` 必须晚于 `schema_valid`；
3. `deterministic_fallback` 产出的报告必须仍为 Schema 合法对象；
4. 任务合同字段不得出现在 `investigation.report` 输出中。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 未启用新阶段门时，子 Agent 仍按现有步骤执行；旧报告记录没有 `report_origin`，读取时按 `model` 处理；
- **数据迁移：** 本提案只新增可观测字段和状态语义，不改变既有报告 Schema，不迁移历史数据；
- **灰度方式：** 通过 feature flag 控制阶段分离和结构化 fallback，先在调查子 Agent 子集启用；
- **回滚条件：** 当报告生成成功率下降、报告平均长度异常或成本显著上升时回滚；
- **回滚步骤：** 关闭 feature flag，恢复旧兜底和旧步骤模型，保留新增观测字段供定位。

## 6. 修改伪代码

### 6.1 核心流程

```go
func RunChildInvestigation(ctx Context, input Input, budget Budget) (Report, error) {
    normalized, err := ValidateContract(input)
    if err != nil {
        RecordFailure(ctx, "invalid_input", err)
        return Report{Status: Failed}, err
    }

    state := NewInvestigationState(normalized, budget)

    // 调查阶段
    for state.CanInvestigate() {
        step, ok := state.NextInvestigationStep()
        if !ok {
            break
        }
        output, err := ExecuteTool(ctx, step)
        if err != nil {
            state.RecordToolFailure(step, err)
            continue
        }
        state.RecordEvidence(output)
        if state.RequiredGoalsCovered() {
            break
        }
    }

    // 报告阶段：独立可见输出预算，关闭/降低 reasoning
    report, err := GenerateReport(ctx, state.Summary(), ReportPhaseParams{
        Tools:     nil,
        Reasoning: budget.ReportReasoning,
        MaxVisible: budget.ReportOutputReserve,
    })
    if err != nil || !SchemaValid(report) {
        report = DeterministicReportFallback(state, normalized)
    }

    PersistOutcome(ctx, state, report)
    return report, nil
}
```

### 6.2 关键边界处理

```go
func DeterministicReportFallback(state State, contract Contract) Report {
    return Report{
        Focus:   FocusForAgent(contract.Capability),
        Summary: "证据采集已执行，但最终报告生成失败，当前未接受未经验证的结论。",
        Findings: state.ConfirmedFindings(),      // 只保留有证据的中间 finding
        Gaps:    state.UnconfirmedGaps(),
        CoveredEvidenceGoals:  state.CoveredGoals(),
        UnresolvedEvidenceGoals: state.UnresolvedGoals(),
    }
}

func RecoverInvestigationReport(schemas SchemaRegistry, input Contract, answer string) (Report, bool, error) {
    report, ok := DecodeReportShape(answer)
    if !ok {
        // 不把任务合同或普通文本当报告
        return DeterministicReportFallback(EmptyState(), input), false, nil
    }

    if IsTaskContractShape(report) {
        Record(ctx, "recovery_skipped_echoed_task_contract")
        return DeterministicReportFallback(EmptyState(), input), false, nil
    }

    repaired := RepairCoverageOnly(report, input.RequiredGoals())
    if SchemaValid(repaired) {
        return repaired, true, nil
    }
    return DeterministicReportFallback(EmptyState(), input), false, nil
}
```

### 6.3 修改前后对比

修改前：

```go
// 普通文本兜底，并回显任务输入
if question := strings.TrimSpace(state.input.Question); question != "" {
    parts = append(parts, "问题："+question)
}
parts = append(parts, "由于时间或模型调用不可用，本次未完成完整分析。")
```

修改后：

```go
if outputContract.Ref == InvestigationReportSchemaRef() {
    return StructuredReportFallback(state, contract)
}
return ProseFallback(state)
```

### 6.4 配置或数据库变更

```yaml
feature:
  investigation_report_phase_separation:
    enabled: false
    rollout_percent: 0
  investigation_report_structured_fallback:
    enabled: false
  investigation_report_reasoning_control:
    enabled: false
```

```text
-- 本提案不引入数据库 schema 变更；
-- 仅新增日志字段 report_origin、settled_tasks、report_valid_tasks、adopted_tasks。
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当 reasoning 耗尽 completion 时，子 Agent 仍返回 Schema 合法的 `investigation.report`，不再返回普通文本；
2. 恢复逻辑不再把任务合同误判为调查报告，不再产生误导性的 `additionalProperties` 错误；
3. 调查阶段采集到的证据在报告失败时仍可被父 Agent 观察；
4. 父 Agent 能按 `task_index` 顺序稳定输出 `1、2、3、4`，每个业务主题独占一节；代码不对 LLM 输出做重排或改写。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `report_origin` | 结构化日志字段 | 区分 `model` / `repaired` / `deterministic_fallback` |
| `settled_tasks` | 结构化日志字段 | 记录已结束子任务数 |
| `report_valid_tasks` | 结构化日志字段 | 记录产出合法报告的子任务数 |
| `adopted_tasks` | 结构化日志字段 | 记录报告被采纳的子任务数 |
| `llm.empty_visible_after_length_total` | Counter | 监控无可见内容但有 length 的调用 |
| `agent.structured_fallback_rate` | Counter | 监控结构化兜底频率 |

日志应至少能够回答：

- 子任务处于调查阶段还是报告阶段；
- 报告最终来源是模型、修复还是兜底；
- 哪些 evidence goals 已覆盖、哪些未覆盖；
- 报告是否通过 Schema 校验、是否被父 Agent 采纳；
- 任务是否因 token、时间、steps 或工具调用触发边界。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 子 Agent 结构化报告生成成功率 | 低（本案例 4/4 无真实报告） | 明显提升 | 7 天 | 日志与持久化状态 |
| 空 partial 报告率 | 高 | 下降 | 7 天 | `report_origin=deterministic_fallback` 且 findings 为空 |
| 恢复误抽取任务合同次数 | 出现 | 0 | 7 天 | 恢复日志 |
| 父 Agent 分条回答符合率 | 不稳定 | 100% | 7 天 | 合成层评测 |
| 单子任务调查耗时 | 本案例约 83 秒 | 不高于当前基线 | 7 天 | 运行计时 |

### 7.4 不应发生的变化

- 正常路径下模型报告行为保持不变；
- `investigation.report` Schema 既有字段语义不改变；
- 不降低报告 Schema 的校验严格性；
- 不引入针对具体业务名、trace 或 delegation ID 的硬编码；
- 不通过提高全局 token 或 timeout 上限掩盖问题。

## 8. 测试与验收

### 8.1 单元测试

- 正常模型输出合法报告，返回 `report_origin=model`；
- 模型无可见输出，返回 Schema 合法的结构化 fallback，且不回显任务输入；
- 恢复器接收包含任务合同字段的 JSON，识别为 `echoed_task_contract`，不按报告修复；
- 报告缺失 `covered_evidence_goals` 时，仅从任务合同补全覆盖字段；
- 报告缺少 `focus/summary/findings` 时，判定为 `not_investigation_report`；
- `findings` 仅在具备证据时填充，不允许凭空制造结论。

### 8.2 集成测试

- 验证从派发子任务到父 Agent 合成回答的完整链路；
- 验证 `settled_tasks`、`report_valid_tasks`、`adopted_tasks` 在日志与状态中一致；
- 验证报告阶段关闭工具且不会重新打开；
- 验证最终用户答案由模型直接输出为 `1、2、3、4` 分节 Markdown，且顺序与 `task_index` 一致；
- 验证 feature flag 关闭时旧路径保持不变。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 四个业务主题调查 | 子 Agent 返回合法报告或结构化 partial，父 Agent 分条输出 | 自动化测试 + 人工检查 |
| 正常路径 | 单主题调查 | 保持既有成功报告行为 | 测试 |
| 报告为空 | 无可见输出 | 结构化 fallback，不回显输入 | 测试 |
| 文本含任务合同 | 兜底文本 | 不误抽取为报告 | 测试 |
| 部分目标未覆盖 | 部分 evidence goals | 返回 partial 并列出未覆盖目标 | 测试 |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. `visible_output_tokens=0` 时仍产出 Schema 合法的 `investigation.report`；
2. 恢复日志不再出现任务合同字段导致的 `additionalProperties` 错误；
3. `report_origin` 能区分 `model` / `repaired` / `deterministic_fallback`；
4. 父 Agent 最终答案稳定输出 `1、2、3、4`，顺序与 `task_index` 一致，且代码未对模型文本做重排或改写；
5. feature flag 关闭时旧路径行为保持不变；
6. 原触发案例回归通过，且不引入业务名、trace 或 delegation ID 特例。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 阶段分离导致调查过早结束 | 覆盖阈值或预算设置过严 | 报告变薄，覆盖度下降 | 覆盖阈值可调，先灰度小流量 | 报告覆盖度显著下降 |
| 报告阶段关闭 reasoning 降低质量 | 模型依赖 reasoning 组织报告 | 结论质量下降 | 只关闭结构化报告阶段的 reasoning，保留调查阶段 reasoning | 报告可读性下降 |
| 结构化 fallback 被误认为成功 | fallback 未标记来源 | 父 Agent 误采纳空结论 | 强制 `report_origin` 与 `partial` 标记 | 空报告被采纳率上升 |
| 恢复识别误判合法报告 | 报告字段与任务合同字段重叠 | 合法报告被拒绝 | 仅以明确的报告/合同特征字段判定 | 合法报告丢失 |

## 10. 实施计划

### 阶段 1：最小安全改动

- 将结构化任务的普通文本兜底替换为 Schema 合法结构化 fallback；
- 增加 `report_origin` 标记；
- 退出条件：`visible_output_tokens=0` 时不再产生普通文本兜底。

### 阶段 2：恢复边界修正

- 恢复器增加报告形状识别；
- 拒绝把任务合同当报告；
- 退出条件：恢复日志不再出现任务合同字段的 `additionalProperties` 错误。

### 阶段 3：阶段分离与报告预算

- 分离调查阶段与报告阶段；
- 报告阶段预留独立可见输出预算并控制 reasoning；
- 退出条件：报告阶段不再因 reasoning 耗尽可见输出。

### 阶段 4：父 Agent 分条输出契约与灰度清理

- 通过输出契约和提示词要求父 Agent 直接输出 `1、2、3、4` 分节 Markdown，代码仅校验、失败重试或标记 partial；
- 清理旧兜底逻辑与旧状态语义；
- 退出条件：用户答案稳定分条，feature flag 全面启用后删除旧路径。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 报告阶段 reasoning 控制 | 报告阶段统一关闭 reasoning | 仅在无可见输出时关闭 reasoning | A | 结构化输出确定性优先 |
| 报告独立预算上限 | 固定 4096 可见输出 | 按报告规模动态预留 | B | 兼顾长报告与预算上限 |
| 阶段门粒度 | 全局开关 | 按子任务类型开关 | B | 便于灰度与回滚 |

## 12. 决策摘要

本提案建议：

1. 将调查阶段与报告阶段分离，报告阶段预留独立可见输出预算并关闭/降低 reasoning；
2. 结构化任务失败时使用 Schema 合法的 `investigation.report` fallback，不使用普通文本兜底；
3. 恢复器先识别报告形状，拒绝把任务合同当报告修复；
4. 父 Agent 按 `task_index` 顺序直接输出 `1、2、3、4` 分节 Markdown，代码只校验、不重排、不改写模型文本；
5. 通过 feature flag、观测字段和回归测试验证效果，失败时回滚旧路径。

## 附录 A：提案提交前检查清单

- [x] 背景足以让非原作者理解系统和改动动机；
- [x] 问题以“期望行为—实际行为—差异”描述；
- [x] 至少包含一个可复现的典型场景；
- [x] 已区分表面现象、直接原因和机制根因；
- [x] 修改方案明确了职责所有者和单一事实源；
- [x] 伪代码覆盖正常路径、失败路径和状态变化；
- [x] 预期效果包含可量化指标，而不只是定性描述；
- [x] 已说明兼容、迁移、灰度和回滚方案；
- [x] 测试可以覆盖原始触发案例和关键边界场景；
- [x] 未引入只针对单个案例的硬编码特例。
