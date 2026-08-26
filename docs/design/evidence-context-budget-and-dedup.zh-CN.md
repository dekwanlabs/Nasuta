# 证据上下文预算与去重计费设计

状态：草案
作者：待补充
日期：2026-08-23
关联事项：[multi-agent-orchestration-simplification-proposal.zh-CN.md](./multi-agent-orchestration-simplification-proposal.zh-CN.md)

> 使用说明：本提案覆盖 Nasuta Investigation / QA 多 Agent Workflow 的证据流转、输入预算与计费去重。方括号中的内容需要在实际启动实施前补充。

## 1. 摘要

本提案用于解决 Investigation / QA 多 Agent Workflow 中证据全文被重复计 token 的问题。

当前，当调查产生大量证据且下游 Verifier / Composer 消费这些证据时，同一份证据全文会分别出现在 Investigator 输出、Verifier 输入以及 Composer 输入中；其中 Composer 路径下还会因一条证据被多条 claim 引用而重复出现 K 次。由此导致 Run 的 InputTokens 被证据全文吃穿，任务以 `budget exceeded` 失败。其根因不是单次结算逻辑错误，而是证据上下文缺少独立预算边界，且下游输入仍以内嵌全文而不是引用方式传递。

本提案计划通过三项机制层修改，将当前流程从：

```text
Investigator 输出证据全文
→ Verifier 接收全量 unit.Content
→ Composer 在每条 claim 内嵌同一 evidence 全文
→ 重复计费、InputTokens 被吃穿
```

调整为：

```text
证据全文一次 Run 内只序列化一次
→ Verifier 接收有界摘要与证据引用
→ Composer 通过 evidence_lookup 引用唯一 summary
→ 独立证据上下文预算计量、超限确定性降级并留审计
```

预期实现证据 token 只计一次、证据上下文与答案输出预算解耦，以及超限时可审计、可回滚。

## 2. 背景

### 2.1 业务与技术背景

Nasuta 的多 Agent Investigation 将问题拆解为调查、验证与综合三个环节。Investigator 产出 EvidenceLedger 中的证据全文，Verifier 消费证据并形成 claim，Composer 最终把被 claim 支持的证据投影进最终答案。该链路的价值在于让最终答案可追溯到权威证据；但在缺少输入边界和引用模型时，追溯性以重复传输全文为代价。

当前相关链路为：

```text
Investigator
→ EvidenceLedger（证据全文，按 evidence ID 去重）
→ Verifier（每条 unit.Content 全文进入验证输入）
→ ClaimLedger（claim + evidence refs）
→ Composer / Synthesizer（每条 claim 内嵌 evidence[].summary 全文）
→ 最终答案
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `Investigator` | 调查并产出候选证据 | 问题、目标、检索结果 | EvidenceLedger 证据全文 |
| `Verifier` | 验证证据是否支持 claim | claim 候选、证据全文 | ClaimLedger 中带证据引用的 claim |
| `Composer / Synthesizer` | 综合 claim 与证据生成答案 | verified_bundle，其中含证据全文 | 最终答案、citations、limitations |
| `EvidenceLedger` | 按 evidence ID 去重保存权威证据 | 候选证据 | 去重后的 EvidenceUnit 集合 |
| `ClaimLedger` | 保存 claim 与 evidence refs 的关系 | 验证结果 | VerifiedClaim + EvidenceRef |

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/investigation/runtime_executor.go`：负责构造 Verifier 输入，`verifierInput` 把 `unit.Content` 全文赋给 claim statement；
- `internal/agent/investigation/agent_composer.go`：负责 `marshalVerifiedBundle` 与 `supportedClaim`，把 `unit.Content` 全文写入每条 claim 的 `evidence[].summary`；
- `internal/agent/investigation/evidence.go`：负责证据规范化、按 ID 去重和 `PruneUnreferencedEvidence`；
- `internal/agent/catalog/schema.go`：定义 `investigation.verified_bundle` schema 与 `limitations_detail` 审计字段；
- `platform/config/platform.go`：定义 `llm_context_window`、`investigation_max_input_tokens` 等运行预算配置；
- [关联文档](./multi-agent-orchestration-simplification-proposal.zh-CN.md)：规定 Run 级共享预算与 Single-Agent Runtime 复用。

当前执行逻辑概括如下：

1. Investigator 产出 EvidenceUnit，证据按 `evidenceID` 进入 EvidenceLedger，账本内只保留一份；
2. Verifier 执行前，`verifierInput` 遍历 `input.Evidence`，把每条 `unit.Content` 全文作为 statement 发送给模型；
3. Verifier 验证后形成 claim 和 evidence refs；
4. Composer 执行前，`PruneUnreferencedEvidence` 裁掉未被引用的证据；
5. `supportedClaim` 对单条 claim 内部做 identity 去重，但跨 claim 不去重，每个被引用证据的全文都会被写入对应 claim 的 `evidence[].summary`；
6. 证据与答案共用 InputTokens / OutputTokens 预算，没有独立的证据上下文审计维度。

### 2.3 为什么现在需要修改

本次修改由证据量大但答案短的 Run 出现 `budget exceeded` 触发：

- 触发时间：待补充具体运行时间；
- 触发标识：待补充 `run_id` / `trace_id`；
- 直接表现：输入预算被证据全文耗尽，而不是被最终答案耗尽；
- 影响范围：所有证据数量大、跨 claim 重复引用高的 Investigation / QA Run；
- 临时处置：暂无可回滚的自动降级，只保留 `PruneUnreferencedEvidence` 和 schema `maxItems` 限制。

### 2.4 范围与非目标

#### 目标

1. 证据全文在一次 Run 内只序列化一次，下游使用引用加短摘要传递；
2. 证据上下文拥有独立、可观测的预算上限，与答案输出预算解耦；
3. 证据上下文超限时确定性降级并记录审计，不静默丢弃；
4. 复用现有 `tooloutput.EstimateTokens`、`limitations_detail` 与 `omissions` 审计机制，不新增第二套运行时。

#### 非目标

1. 本提案不解决检索排序、证据质量或 claim 正确性本身的问题；
2. 本提案不改变 Single-Agent Runtime 的模型循环与 Step / Retry / Replan 语义；
3. 本提案不通过单纯提高 `investigation_max_input_tokens` 或新增 `investigation_max_total_tokens` 掩盖重复计费；
4. 本提案不针对某个具体服务名、证据 ID 或问题关键词写特例。

## 3. 问题

### 3.1 问题描述

**期望行为：**

同一份权威证据只应在需要全文检索或审计时保留全文；进入模型的 Verifier / Composer 输入应以引用和受限摘要为主，并且证据上下文应有独立预算边界。超限时应明确降级并记录省略项。

**实际行为：**

同一份证据全文至少出现三次：Investigator 输出一次，Verifier 输入一次，Composer 输入中每条引用它的 claim 再重复一次。Composer 路径没有跨 claim 去重，也没有证据上下文 token 上限。

**差异：**

实际行为把同一证据的全文重复计入多个输入预算，导致输入预算与答案大小解耦失败；当证据很大而答案很小时，系统仍可能耗尽预算，且无法从现有 `InputTokens` / `OutputTokens` 中区分是哪类内容超限。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 证据量大、答案短的 Run 以 `budget exceeded` 失败 | 运行日志中的预算耗尽错误，待补充具体 trace |
| 直接原因 | Verifier 输入使用 `unit.Content` 全文，Composer 在每条 claim 的 `evidence[].summary` 中重复写入全文 | `runtime_executor.go` 的 `verifierInput`；`agent_composer.go` 的 `supportedClaim` |
| 机制根因 | 数据流没有“一次序列化 + 引用”的契约；证据上下文没有独立预算桶；跨 claim 去重只发生在单条 claim 内部 | `marshalVerifiedBundle` 和 `supportedClaim` 的 `seenIdentity` 作用域过窄；配置中只有 Run 输入/输出预算 |

根因链路：

```text
证据被多条 claim 引用
→ Composer 以 claim 为容器重复内嵌全文
→ 单 claim 去重无法覆盖跨 claim 重复
→ 证据 token 与答案 token 共用 Input/Output 预算
→ 预算耗尽时无法判断是证据还是答案超限
→ Run 失败且缺少可恢复的降级路径
```

本问题不能只通过提高 `investigation_max_input_tokens`、给证据字段增加 `maxItems` 或针对某个运行增加特例解决，因为这些方式只放宽总量或压制局部形状，没有消除“同一证据被重复计 token”和“证据与答案预算混算”的机制缺陷。

### 3.3 影响

- **用户影响：** 证据充分但最终答案本应很短的调查任务仍可能失败或超时；
- **业务影响：** QA / Investigation 成功率下降，人工复核成本上升；
- **系统影响：** 输入 token 和成本被重复证据浪费，预算无法反映真实任务规模；
- **工程影响：** 证据投影、计费和审计职责混在 Composer 序列化中，难以测试和定位。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：同一条证据被多条 claim 引用

- **Given（前置条件）：** EvidenceLedger 中有证据 `evt_01`，其 `unit.Content` 约 8,000 字符；三条 claim 均引用 `evt_01`；
- **When（触发行为）：** Composer 调用 `marshalVerifiedBundle` 构造 `verified_bundle`；
- **Then（期望结果）：** `evt_01` 的摘要只出现一次，三条 claim 只带 `evidence_id` 引用；
- **But（当前结果）：** 三条 claim 各自内嵌同一份 `evt_01` 全文，`evt_01` 被重复序列化三次。

示例输入：

```text
question = "这个调用链为什么超时？"
evidence = [
  {id: "evt_01", content: "<8,000 字符调用链证据>"},
]
claims = [
  {id: "c1", evidence_refs: ["evt_01"]},
  {id: "c2", evidence_refs: ["evt_01"]},
  {id: "c3", evidence_refs: ["evt_01"]},
]
```

当前执行路径：

```text
marshalVerifiedBundle
→ PruneUnreferencedEvidence 保留 evt_01
→ 遍历 claims
→ supportedClaim 对每条 claim 单独维护 seenIdentity
→ evt_01 全文写入 c1、c2、c3
→ 输入 token 翻倍增长
```

关键证据：

```text
supportedClaim 中的 seenIdentity 在每次调用时重新创建，
只能消除单条 claim 内部重复，不能消除跨 claim 重复。
```

#### 场景 B：Verifier 接收无界证据全文

- **Given：** 一次调查产出 200 条候选证据，每条 `unit.Content` 数千字符；
- **When：** `verifierInput` 将全部 `input.Evidence` 转为 claim statement；
- **Then：** Verifier 只接收候选 claim 与受限摘要，证据上下文有明确上限；
- **But：** Verifier 输入与 `unit.Content` 成正比，没有摘要截断和 token 闸门。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 证据数量适中，跨 claim 引用少 | 全文进入 Verifier / Composer | 引用 + 单份摘要进入下游 |
| 空输入或缺失字段 | 无证据可验证 | `verifierInput` 返回错误 | 仍返回明确错误 |
| 超时或预算耗尽 | 证据上下文超预算 | Run 可能失败 | 确定性降级并记录 omissions |
| 下游失败 | Composer / Verifier 失败 | 预算已消耗但审计不完整 | 保留已消耗 usage 和裁剪审计 |
| 重试或重复请求 | 同一 Run 重试 | 可能再次重复计费 | 引用模型保证原文只计一次 |
| 并发或乱序 | 多证据并发产出 | 按到达顺序进入 ledger | 按稳定 identity 去重与排序 |
| 旧数据或旧客户端 | 不属于当前运行时契约 | 不再进入新 Run | 旧 snapshot 显式清理、作废或离线迁移 |

### 4.3 复现步骤

1. 准备一次 Investigation Run，令多个 Investigator 产出大量证据，且至少 3 条 claim 引用同一条大证据；
2. 执行调查到 Composer 阶段；
3. 观察 `marshalVerifiedBundle` 序列化后的输入 token；
4. 可见同一条证据全文在 supported_claims 中重复出现；
5. 重复运行多轮后，证据量大、答案短的 run 出现 `budget exceeded` 的概率随证据大小和引用次数上升。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 统一改为“一次序列化 + 引用”，而不是针对某条证据或 claim 去重。
2. **保持单一事实源。** EvidenceLedger 继续持有权威证据；Composer 只生成引用视图和受限摘要，不再复制权威全文为唯一消费路径。
3. **明确职责边界。** Verifier 负责构造有界验证输入；Composer 负责引用投影和预算降级；预算/审计负责记录实际证据 token 和省略项。
4. **失败可诊断。** 超限、省略、预算余量都必须出现在 `limitations_detail` 和结构化日志中。
5. **单一当前契约。** 当前唯一 `investigation.verified_bundle` 使用紧凑引用模型；不保留完整证据内嵌的兼容分支，也不通过 feature flag 在两种 wire format 之间切换。

### 5.2 目标流程

```text
EvidenceLedger 权威证据
→ 证据上下文预算器估算 token
→ 确定性裁剪与去重（按 required goal / trust tier / 引用次数）
→ Verifier 输入 = claim 候选 + 引用 + 短摘要
→ Composer 输入 = evidence_lookup 单份摘要 + claim 证据引用
→ 审计 omissions / limitations_detail
→ 输出答案 + 可追踪证据视图
```

与当前流程相比，关键变化是：

1. 在 `verifierInput` 处新增摘要截断和 token 闸门，用于阻止无界全文进入模型；
2. 将证据全文的序列化位置从每条 claim 移动到 `evidence_lookup`，用于消除跨 claim 重复；
3. 将证据 token 与答案 token 分开记账，用于区分超限来源；
4. 在证据上下文超限处增加确定性降级，用于保留审计而非静默成功；
5. 在 `limitations_detail` 和 omissions 中公开裁剪依据与实际 token。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| Verifier 输入裁剪 | `Statement: unit.Content` 全文 | claim statement 使用受限摘要，并限制条数与 token | `runtime_executor.go` | 不改变失败时的明确错误语义 |
| Composer 引用模型 | 每条 claim 内嵌 `evidence[].summary` 全文 | claim 只带 `evidence_id`，全文集中于 `evidence_lookup` | `agent_composer.go` | 当前唯一 compact schema |
| 证据上下文预算 | 证据与答案共用 Input/Output | 新增 evidence 维度审计计数，估算并累计证据 token | 预算、`tooloutput`、账本 | 不新增 total token 配置 |
| 降级与审计 | 仅有 `maxItems` 和局部 omissions | 按优先级裁剪，记录 token、ID、原因和依据 | `verifier.go` / `agent_composer.go` | 复用 `limitations_detail`、`omissions` |

#### 改动一：Verifier 输入裁剪

**方案：**

在 `verifierInput` 构造 claim 时，不再直接把 `unit.Content` 作为 statement。改为：

1. 按 stable evidence identity 对输入证据去重；
2. 为每条证据生成受限摘要，长度不超过 `E`；
3. 先保留 required goal、高 trust tier、被多次引用的证据；
4. 用 `tooloutput.EstimateTokens` 累计，直到达到证据上下文预算；
5. 若仍超限，按 5.3 改动三的降级顺序处理，并返回 omissions。

**约束：**

- 不得静默截断：所有被省略项都要出现在 omissions；
- 不得改变 Verifier 输入 schema 的 required 字段；
- 摘要截断必须确定性，不依赖另一次 LLM 调用。

**失败行为：**

- 当输入无证据时，继续返回明确错误；
- 当证据上下文超限时，返回 partial 输入并附带 omitted evidence，而不是整次失败或伪成功；
- 预算耗尽时记录 `evidence_budget_exhausted`，不得把证据 token 悄悄归零。

#### 改动二：Composer 引用模型

**方案：**

在 `verified_bundle` 中新增 `evidence_lookup`，claim 的 `evidence` 项改为引用型：

```text
evidence_lookup: {
  "evt_01": { "summary": "截断后的唯一摘要", "content_hash": "...", ... }
}
supported_claims[].evidence[] = { "evidence_id": "evt_01", "kind": "code", "reference": "..." }
```

`marshalVerifiedBundle` 先构造全局 `evidence_lookup`，再让 `supportedClaim` 只输出 `evidence_id`。跨 claim 去重由全局 map 完成，而不是每条 claim 单独维护 `seenIdentity`。

**约束：**

- 当前唯一 consumer 通过 `evidence_lookup[evidence_id]` 解析摘要；
- `supported_claims[].evidence[]` 只允许 `evidence_id`，不再接受内嵌证据对象；
- 摘要长度和 `evidence_lookup` 总 token 都受同一证据上下文预算约束；
- 不能因引用模型丢失 `content_hash` 和 traceability。

**失败行为：**

- 当某个 evidence ID 无法解析时，返回 schema / contract 错误；
- 当 `evidence_lookup` 超预算时，按优先级裁剪，并保留 omissions；
- 不允许多条 claim 引用同一个 ID 却出现不同摘要。

#### 改动三：证据上下文预算桶与降级审计

**方案：**

新增 `EvidenceContextBudget` 作为 InputTokens 的子预算 / 审计维度，不新增 `investigation_max_total_tokens`：

```text
Run 输出预算（现有 OutputTokens）
    = llm_answer_max_tokens × Agent 数量

Run 输入预算（现有 InputTokens）
    = 所有喂进模型的内容，含 evidence、prompt、工具结果

证据上下文预算（新增审计维度）
    = 证据在 Verifier / Composer 消费时累计的 token
```

实现要点：

- 构造 `verifierInput` 和 `marshalVerifiedBundle` 时，用 `tooloutput.EstimateTokens` 估算证据文本 token；
- 单次输入上限继续使用 `llm_context_window`，Run 级输入上限继续走 `InvestigationMaxInputTokens`；
- 在预算账本中增加 evidence 维度计数，不改变现有 OutputTokens / InputTokens 的结算语义。

**约束：**

- 证据上下文预算必须可观测，至少记录预算上限、实际 token、去重后 token、omitted token；
- 降级顺序固定且可测试；
- 不得把证据上下文超限伪装成答案输出超限。

**失败行为：**

- 当证据上下文超限时，按顺序降级：截断摘要 → 去重引用 → 按 goal / trust tier / 引用次数裁剪条目 → 只保留 required goal → 记录 omissions 并继续；
- 若最终仍超限，记录不可恢复状态并继续，但不能静默成功；
- 审计字段应能还原每一次裁剪决策。

### 5.4 数据结构或接口契约

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `evidence_lookup` | `map[string]evidence_summary` | Composer | 以 evidence ID 为键的唯一下游摘要 | 空 map | 当前唯一 compact schema |
| `evidence_id` | `string` | Composer | claim 对 evidence_lookup 的引用 | 无 | 当前唯一 compact schema |
| `evidence_context_tokens` | `int` | 预算 / 审计 | 证据被 Verifier / Composer 消费时累计的 token | 0 | 新增审计字段 |
| `omitted_evidence` | `[]omitted_evidence` | Composer / Verifier | 被裁剪的证据 ID、原因、优先级依据 | 空 | 复用 omissions 结构扩展 |

状态转换：

```text
[evidence_context_within_budget]
  ├─ 成功 → [complete]
  ├─ 可降级 → [partial]，记录 omitted_evidence
  └─ 不可恢复 → [failed]，记录 evidence_budget_exhausted
```

不变量：

1. 同一 evidence ID 在 `evidence_lookup` 中只出现一次；
2. claim 中的 `evidence_id` 必须能在 `evidence_lookup` 中解析，或属于显式省略项；
3. `evidence_context_tokens` 必须等于去重后实际交付的摘要 token，加省略 token 之和；
4. 成功且完整的交付不得出现 `omitted_evidence`，partial 交付必须出现。

### 5.5 兼容、迁移与回滚

- **当前契约：** `investigation.verified_bundle` 只发布一个当前 schema，claim 的 `evidence` 只能是 `evidence_id` 引用；摘要统一放在 `evidence_lookup`；
- **数据迁移：** 不在读取路径兼容旧 payload；历史 snapshot 由发布流程显式清理、作废或离线迁移；
- **发布方式：** 新 Run 直接使用 compact schema，不通过 feature flag 维护两套 wire format；
- **回滚条件：** 若 compact handoff 导致 schema 解析错误、答案质量下降或 omissions 审计异常，则停止发布并修复当前契约；
- **回滚步骤：** 回滚整个应用版本或离线迁移数据，不在运行时恢复完整证据内嵌路径。

## 6. 修改伪代码

### 6.1 核心流程

```go
func BuildEvidenceContext(
	ctx context.Context,
	units []EvidenceUnit,
	claims []VerifiedClaim,
	budget EvidenceContextBudget,
) (EvidenceContext, error) {
	ordered := DeduplicateAndRank(units, claims, budget)

	lookup := make(map[string]EvidenceSummary)
	var used int
	var omitted []OmittedEvidence

	for _, unit := range ordered {
		summary := TruncateSummary(unit.Content, budget.MaxSummaryLen)
		cost := tooloutput.EstimateTokens(summary)

		if used+cost > budget.MaxEvidenceContextTokens {
			omitted = append(omitted, OmittedEvidence{
				EvidenceID: unit.ID,
				Reason:     "evidence_context_budget_exhausted",
			})
			continue
		}

		lookup[unit.ID] = EvidenceSummary{
			Summary:     summary,
			ContentHash: unit.ContentHash,
		}
		used += cost
	}

	context := EvidenceContext{
		Lookup:               lookup,
		EvidenceContextTokens: used,
		Omitted:              omitted,
	}
	if err := PersistLimitationsDetail(ctx, context); err != nil {
		return EvidenceContext{}, err
	}
	return context, nil
}
```

### 6.2 关键边界处理

```go
func DeduplicateAndRank(
	units []EvidenceUnit,
	claims []VerifiedClaim,
	budget EvidenceContextBudget,
) []EvidenceUnit {
	refCount := make(map[string]int, len(units))
	for _, claim := range claims {
		for _, ref := range claim.EvidenceRefs {
			refCount[ref.EvidenceID]++
		}
	}

	byID := make(map[string]EvidenceUnit, len(units))
	for _, unit := range units {
		if _, exists := byID[unit.ID]; exists {
			continue
		}
		byID[unit.ID] = unit
	}

	ranked := make([]EvidenceUnit, 0, len(byID))
	for id, unit := range byID {
		unit.rank = RankEvidence(id, unit, refCount[id])
		ranked = append(ranked, unit)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		return ranked[i].ID < ranked[j].ID
	})
	return ranked
}
```

### 6.3 修改前后对比

修改前：

```go
// 当前实现把证据全文同时用于 Verifier 输入与每条 claim 的 summary。
for _, unit := range input.Evidence {
	claims = append(claims, verificationClaim{
		ID:        unit.ID,
		Statement: unit.Content,
		Citations: []string{unit.ID},
	})
}
```

修改后：

```go
for _, unit := range input.Evidence {
	if evidenceCtx, ok := lookup[unit.ID]; ok {
		claims = append(claims, verificationClaim{
			ID:        unit.ID,
			Statement: evidenceCtx.Summary,
			Citations: []string{unit.ID},
		})
	}
}
```

### 6.4 配置或数据库变更

```yaml
feature:
  evidence_reference_model:
    enabled: false
    rollout_percent: 0
  evidence_context_budget:
    enabled: false
    max_summary_tokens: 1024
    max_evidence_context_tokens: 0
```

```sql
-- 如无数据库变更，删除此代码块。
-- 本提案优先复用现有 limitations_detail / omissions 审计字段；
-- 只有在需要跨重启持久化 evidence 预算账本时才新增表。
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当同一证据被多条 claim 引用时，序列化后的全文只出现一次；
2. 当证据上下文超预算时，系统能够按优先级裁剪并返回 partial 审计；
3. 不再出现“答案很小但证据很大”仍把 InputTokens 吃穿且无法定位的问题；
4. 对 Verifier 和 Composer 两个下游入口能够分别施加有界、可观测的输入限制。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `evidence_context_tokens` | Gauge / 日志字段 | 反映证据实际消耗 |
| `evidence_context_budget` | Gauge / 日志字段 | 反映证据预算上限 |
| `omitted_evidence` | 持久化字段 | 反映裁剪决策 |
| `evidence_budget_exhausted` | Counter / 状态字段 | 定位证据超限 |
| `limitations_detail` | 持久化字段 | 还原 total / displayed / omitted |

日志应至少能够回答：

- 证据是否只走 compact 引用模型，以及 `evidence_lookup` 的命中情况；
- 哪一环节触发裁剪，以及裁剪依据；
- 是否发生降级，以及最终证据上下文 token；
- 最终答案的 complete / partial 状态分别是什么；
- 哪些输出可以追溯到哪些 evidence ID 和 content hash。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 证据重复序列化次数 | 同证据最多被引用次数 + 2 | 1 | 单 Run | 序列化审计 |
| 证据超大但答案短的 Run 失败率 | 待测 | 显著下降 | 周 | Run 日志 |
| 证据上下文 token 可观测率 | 0 | 100% | 单 Run | 预算账本 |
| omitted evidence 审计完整率 | 待测 | 100% | 单 Run | limitations_detail |

### 7.4 不应发生的变化

- 当前 synthesizer 只消费 compact `verified_bundle`，不恢复完整证据内嵌路径；
- 正常小规模证据路径的答案质量和延迟不因新增机制显著劣化；
- 不降低证据可追溯性：`content_hash`、evidence ID 和引用关系必须保留；
- 不引入针对具体服务、证据 ID 或问题关键词的硬编码。

## 8. 测试与验收

### 8.1 单元测试

- 同一证据被 3 条 claim 引用时，`evidence_lookup` 只包含一份摘要；
- `verifierInput` 在证据无内容时返回明确错误，而不是构造空 statement；
- 证据上下文超过预算时，按优先级裁剪且 omissions 记录完整；
- 跨 claim 去重使用全局 map，单 claim 内重复 identity 不影响结果；
- `evidence_context_tokens` 等于去重后实际交付 token 与省略 token 之和；
- `supported_claims[].evidence` 只包含 `evidence_id`，摘要统一从 `evidence_lookup` 解析。

### 8.2 集成测试

- 验证从 EvidenceLedger 到 Verifier、Composer、最终答案的完整链路；
- 验证当前唯一 compact consumer 的 schema、引用解析和 omissions 一致；
- 验证日志、指标、`limitations_detail` 和持久化 omissions 一致；
- 验证证据上下文超限时的 partial 降级不会吞掉最终答案；
- 验证并发、重复请求、乱序证据到达时的去重与排序稳定。
