# 多 Agent 调查任务级上下文与证据所有权提案

状态：已实施并归档
日期：2026-08-20
目标：让每个 Investigator 只接收自己任务范围内的上下文和证据，消除兄弟 Agent 的重复证据消费，并保证部分失败时仍能完成可验证的汇总。

## 1. 结论先说

本问题的根因不是 token 预算太小，也不是并发数不够，而是**任务分配没有落到证据边界**：

```text
Planner 只分配 facet/capability
→ 没有给 task 分配 InputRefs
→ 每个 Investigator 从同一份 workflow input 自己过滤
→ InputRefs 为空时退化为“匹配 facet/source 的全部证据”
→ 兄弟 Agent 重复读取、重复推理
→ 某个 Agent 失败后，join 又丢掉原始 seed evidence
→ Verifier/Synthesizer 只能看到 unavailable 元数据
```

正确的数据流是：

```text
QA 准备 seed evidence
→ 根据 task facet + capability/source 选择唯一 owner
→ 将精确 EvidenceRef 写入 task.InputRefs
→ Investigator 只拿自己的投影
→ join 保留 baseline evidence，并合并 task 新证据
→ Verifier/Synthesizer 处理完整且可追溯的 evidence ledger
```

本提案**不通过缩小 `MaxOutputTokens`、`MaxSteps` 或 continuation rounds
来掩盖问题**。这些值继续由现有全局配置驱动；本次修改的是上下文的归属和证据的传递。

## 2. 根因

### 2.1 Planner 分配了职责，没有分配输入

Planner 已经能够生成 task purpose、investigation goal、evidence facet 和 capability，
但没有把已经检索得到的 evidence 绑定到 task。每个 Investigator 随后收到同一份
`task.contract`，projection 再按 facet/source kind 过滤。

这会把“没有指定输入”误解为“所有匹配输入都可以拿”。只要两个兄弟 task 的
facet/source 相同，它们就会重复消费同一批 evidence。

### 2.2 `nil` 与显式空列表过去没有区别

正确语义如下：

| `InputRefs` | 含义 |
| --- | --- |
| `nil` | 旧版/legacy workflow，没有完成 task-level assignment，可使用兼容性的宽匹配 |
| `[]` | 该 task 被明确分配为没有 seed evidence，不得注入任何 seed；它只能使用自己的工具补证 |
| 非空列表 | 只允许引用命中的 canonical evidence identity |

`InputRefs` 在 JSON 中保留显式空数组，避免 definition publish、持久化和 reload 后
退化成 `nil`。

### 2.3 Join 丢失 baseline

`joinHandoffs` 虽然接收 `baselineEvidence`，过去只把它用于 convergence 统计，没有加入
最终 ledger。所有 Investigator unavailable 时，join 只有 unavailable task 元数据；
Verifier/Synthesizer 没有原始检索证据可用。

即使 join 保留 evidence，Verifier 的 payload trimming 也可能因为 seed 没有被 finding
引用而删掉它，导致原始证据在最后一跳消失。

## 3. 设计

### 3.1 在 QA 和 Escalator 边界分配 evidence owner

QA 启动 workflow 前同时拥有完整的 contract、task graph proposal 和 `seedEvidence`，
因此在 `internal/agent/qa/submission.go` 完成 server-owned assignment。工作流
Escalator 也处在同一个边界：它在创建 delegation workflow 的 `task.contract` 时，
必须写入相同格式的 ownership manifest，不能只设置 capability/facet 后把完整输入广播
给所有 Investigator。

manifest 使用稀疏表示：只有真正拥有 seed evidence 或 delegation context 的 task 才写入
assignment；manifest 存在但没有列出的 task 视为显式空输入。这样 Escalator 生成的
`delegation.report` 也只会进入对应 capability 的 Investigator，不会被每个兄弟节点
重复消费。`codegraph` 等可能被多个 capability 使用的 source kind，仍然必须在一次
assignment 中选择唯一 owner。

Planner 不负责猜测 evidence 引用，也不能通过 prompt 自己决定兄弟之间如何共享 evidence。
assignment 使用已经 canonicalize 的 evidence ledger，而不是模型自由文本。

### 3.2 唯一 owner 规则

对每个 seed evidence group：

1. task 的 `RequiredFacets` 必须和 evidence facet 相交；
2. task capability 必须允许该 `SourceKind`；
3. 相同 capability/facet 的多个 task 按已分配 group 数量做确定性均衡；
4. 一个 canonical evidence group 只能属于一个 sibling task；
5. 多 section unit 按整体分配，不能把不同 section 分给不同 task；
6. 找不到 owner 的 evidence 不强行注入任何 Investigator，但仍保留在 baseline ledger。

source boundary：

| capability | 允许的 source kind |
| --- | --- |
| `knowledge.code.inspect` | `code`, `codegraph` |
| `knowledge.service.trace` / `knowledge.runtime.observe` | `service`, `dependency`, `runtime`, `codegraph` |
| `knowledge.docs.verify` | `runbook`, `generated_doc`, `doc`, `docs` |
| `knowledge.web.research` | `web`, `external` |
| `knowledge.memory.recall` | `memory` |

每个 owner task 写入精确 `EvidenceRef`，包括 source、target、section、version、
time range 和 content hash。

### 3.3 Investigator projection

`projectInvestigatorHandoff` 的契约：

- 非空 `InputRefs`：只投影引用命中的 evidence；
- 显式空 `InputRefs`：投影空 seed，Agent 可以使用自己被授权的工具补证；
- nil `InputRefs`：保持 legacy broad matching，兼容未经过新 assignment 的旧 workflow。

Planner proposal 在编译阶段被拒绝时，deterministic fallback 也为 Investigator 写入
显式空 `InputRefs`，避免 fallback 重新广播全部 seed。

### 3.4 Join 使用统一 evidence ledger 和字节预算

Join 顺序为：

```text
baselineEvidence → investigator handoffs → canonical dedup/conflict detection
```

join payload 同时保留 baseline 的正文 evidence 和 canonical identity 元数据：

```json
{
  "baseline_evidence_identities": [
    {
      "source_kind": "code",
      "target": "repo/order.go",
      "section": "L1-L20"
    }
  ]
}
```

`baseline_evidence_identities` 用于下游识别和保护 baseline；正文仍位于统一的
`evidence_units` ledger 中。baseline 和 investigator evidence 使用同一个
`internal/evidence.Ledger` 去重并报告冲突。

join 不再使用固定的 `200` 条 evidence 上限。它使用当前节点已有的
`Budget.MaxHandoffBytes` 做 payload 字节预算：先保护 baseline，再按各 Investigator
的发现以确定性、交错顺序填充剩余空间；超出预算的证据被省略，并记录
`evidence_units_total` / `evidence_units_omitted`。如果仅 protected baseline 就超过
字节预算，join 必须显式失败，不能静默删除 baseline。

### 3.5 Verifier 保护 baseline

Verifier 读取 `baseline_evidence_identities`，在 payload trimming 时把对应 identity
作为 protected evidence。即使 finding 没有引用它，baseline 仍然保留；Verifier 自己的
上下文裁剪也继续服从全局/节点配置的 `MaxInputTokens` 和 `MaxHandoffBytes`，不引入
角色级固定 token 配额。

## 4. 数据流与不变量

### 4.1 数据流

```text
prepared evidence
  ├─ seedMaterial → TaskContract.context.seed_material
  └─ seedEvidence
        ↓
  prepareInvestigationProposal
        ↓
  assignTaskEvidenceOwners
        ↓
  TaskSpec.InputRefs
        ↓
  ProposalCompiler → TaskDirective.InputRefs
        ↓
  projectInvestigatorHandoff
        ↓
  Investigator handoffs
        ↓
  joinHandoffs(baselineEvidence, handoffs)
        ↓
  Verifier protected baseline
        ↓
  Synthesizer
```

Escalator 路径使用同一不变量：

```text
delegation request + seedEvidence
        ↓
workflowEscalationTaskAssignments
        ↓
task.contract.task_evidence_assignments
        ↓
Investigator projection
```

### 4.2 必须保持的不变量

1. 同一个 canonical evidence group 不能被两个 sibling Investigator 接收；
2. 多 section unit 不能被 section 拆分到不同 sibling；
3. 显式空 `InputRefs` 不能匹配任何 seed；
4. join 后 baseline 即使所有 Investigator unavailable 也仍存在；
5. baseline 与 investigator 相同 identity 只保留一份；
6. evidence conflict 仍然可见，不得因为 baseline 优先而隐藏；
7. Verifier trimming 不得删除 protected baseline；
8. Synthesizer 失败时，其他 handoff、baseline 和 unavailable metadata 仍可供下游处理；
9. Investigator、Verifier、Synthesizer 的输出和步骤预算继续跟随全局配置，不由本提案
   固定为角色级常数。

## 5. 实施范围

核心实现位于：

- `internal/agent/qa/submission.go`
- `internal/agent/qa/task_evidence_assignment.go`
- `internal/agent/workflow/investigator_projection.go`
- `internal/agent/workflow/proposal_compile.go`
- `internal/agent/workflow/executor_handoff.go`
- `internal/agent/workflow/evidence.go`
- `internal/agent/workflow/verifier.go`
- `internal/agent/catalog/schema.go`
- `agent/task_graph.go`
- `internal/agent/workflow/model.go`
- `app/investigation.go`

## 6. 验收

已覆盖以下回归测试：

- 两个同 capability/facet 的 sibling task 不共享 seed group；
- 多 section evidence 只归属一个 owner；
- 未匹配 source/facet 的 evidence 不会被注入；
- explicit empty refs 不会重新触发 broad matching；
- 所有 Investigator unavailable 时 baseline 仍在 join ledger；
- baseline 与 investigator evidence canonical dedup；
- Verifier trimming 保留 protected baseline；
- investigation bundle schema 接受新的 baseline identity 字段；
- join 使用 `MaxHandoffBytes` 做字节级裁剪，不再依赖固定 200 条上限；
- Escalator delegation report 按 capability/source ownership 投影到唯一 Investigator；
- 全仓 `go test ./...`。

验收重点不是“每个 Agent 都拿到很多上下文”，而是：

```text
每个 Agent 拿到属于自己的上下文
兄弟之间不重复消费同一证据
失败时下游仍拿到可验证的剩余事实
```

## 7. 非目标与后续工作

本提案不改变 provider 选择、检索排序、capability 权限、global context/output budget
来源或外部 workflow escalator 协议。

后续可以继续优化：

1. 把 task owner 分配结果写入运行 trace，便于直接检查重复率；
2. 对没有 owner 的 seed evidence 记录明确的未分配原因；
3. 用 canonical evidence group 数而不是模型调用次数衡量并行收益；
4. 进一步压缩重复 report metadata，但不能压缩 canonical identity 和 protected baseline。
