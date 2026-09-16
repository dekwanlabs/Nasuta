# 流程图生成治理提案：从"强制/兜底"到"有才画、没就不画"

## 1. 背景

QA 答案中的 FlowIR 流程图当前存在三类问题，它们表面不同，根因相同。

### 1.1 三个可复现的症状

**症状 A：对外部实体硬画空壳流程图（trace `4e742e7c5977`）。** 提问"盐城师范和吉林工程技术师范学院对比，哪个好"——一个与代码库完全无关的外部常识问题。答案本身是诚实的（"未拿到权威证据，无法下结论"），但仍被塞进 3 个 FlowIR 块，且全是空壳：`nodes` 里只有一个 `kind:"unknown"` 的占位节点（`label:"yanchengshifan (unresolved subject — no indexed component identified)"`），`open_hops` 全是 "Unresolved"，`confidence:"low"`。

**症状 B：偶尔仍用 trace_deps 依赖图冒充流程图。** 部分答案的流程图是扁平的服务依赖图（无 `label` 语义、节点 `kind:"service"`、边 `sync_mode:"unknown"`、`evidence_state:"inferred"`、`subject:"Evidence-derived flow"`），而非模型调查后写出的语义化流程。

**症状 C：子 agent 调查缺口（gaps/open_hops）普遍偏多。** 对索引覆盖不了的主题，子 agent 报告里大量"未解决/未闭合"项，并被 `gap_chase` 进一步放大观感。

### 1.2 共同根因：只会"按规矩办事"，不会"看人下菜"

三个症状指向同一个结构性缺陷：**系统对"该不该画流程图"缺乏语义判断——它只看 query kind 和实体列表，由分类器一刀切地强制生成，而不让 LLM 根据"这个问题是否适合画流程图、画它靠索引证据还是通用知识"来决定。** 流程图被当成"无论如何都得有一张"的硬性产出，而不是"调查到位后的自然产物"。于是对学校这种"实体对比"（本就不该画）硬画空壳，对 kafka 这种"概念架构"（该画却被证据要求卡死）反而画不好。

具体落在两个机制上：

1. **`completeMissingFlows` 强制制造流程图**（症状 A、C 的直接来源）。
2. **`BuildEvidencePreservingReport` 强制兜底成 trace_deps 图**（症状 B 的直接来源）。

## 2. 根因分析

### 2.1 症状 A/C：kind 驱动的强制流程图补全

强制链如下：

```
query kind (comparison / flow / overview ...)
  → QueryNeedsFlow(kind)：kind 的 RequiredFacets 含 FacetCoreFlow / FacetEntrypoint?
  → outputContractForQuery：是 → 设 OutputContract.Subjects = [全部实体]
  → completeMissingFlows：Subjects 中缺 flow 的 → 强制派 ServiceTraceCapabilityID 子 agent 补 flow
  → enforceFlowContract：把 flow 装进答案
```

- `QueryNeedsFlow`（`internal/domain/query_plan.go`）只看 kind 是否含 CoreFlow/Entrypoint facet。`comparison` 的 RequiredFacets 含 `FacetCoreFlow`，所以"对比两个学校"与"对比两个业务流程"被一视同仁。
- `completeMissingFlows`（`internal/agent/execution/loop_execution.go`）把 `OutputContract.Subjects` 当成"必须补全"的契约目标，缺一个补一个，**不判断该 subject 是否可调查、是否适合画流程图**。对学校这种索引里根本没有的实体，照样派子 agent 去"trace 端到端流程"，子 agent 在代码库搜不到任何东西，最终 `gap_chase rejected`，产出空壳 FlowIR。

**关键矛盾：这违背了自己的设计本意。** `enforceFlowContract`（`internal/agent/execution/flow_contract.go`）的注释明确写道：

> "their presence — **not the query-intent classifier** — is the authoritative signal that the answer should carry structured flow diagrams … **the model's own answer is used as-is when no FlowIR exists**."

即设计本意是"**有没有真实产出 flow，才是该不该画的信号；没有 flow 就用模型自己的答案，不画**"。但 `completeMissingFlows` 在 `enforceFlowContract` 之前先用 kind 推出的 Subjects 强制补 flow，把"有才画"架空成了"kind 说要画就硬造"。

### 2.2 症状 B：校验失败后的 trace_deps 兜底

`recoverInvestigationReport`（`internal/agent/definition/result_recovery.go`）是三级兜底，每级都再过一次 schema 校验：

```
模型 answer → validatedOutput 校验（含 normalizeOutputForSchema 宽容剔除）
  ✓ 通过 → 用模型的 flow（正常）
  ✗ 失败 → 路径① repairInvestigationGoalCoverage（只补 covered/unresolved goals）
              ✓ 通过 → 保留模型 flow（正常）
              ✗ 失败 → 路径② BuildEvidencePreservingReport  ★ fallbackFlow = trace_deps 扁平图
                        ✗ 再失败 → 路径③ 空壳（不画图，安全）
```

`fallbackFlow`（`internal/agent/execution/evidence_fallback.go`）只在路径②触发：它**丢弃模型写的全部内容**，用证据单元的依赖边拼一张扁平图（`subject:"Evidence-derived flow"`、节点 `label:id, kind:service`、边 `sync_mode:unknown, evidence_state:inferred`）。

**会触发路径②的格式错误（即"还有哪些格式不符合会触发兜底"）**：

- **A. 缺 required 字段**：根级缺 `focus/summary/findings/gaps/covered_evidence_goals/unresolved_evidence_goals`；finding 缺 `claim/evidence_goal_ids/evidence/confidence`；flow 缺 `subject/status/nodes/edges/open_hops/confidence`；flow_node 缺 `id/label/kind`；flow_edge 缺 `from/to/evidence_state`；evidence 缺 `kind/reference/summary`。
- **B. 类型/枚举/长度错误**：`confidence` 非 0-1 数字；`status` 非 `complete/partial`；flow `confidence` 非 `low/medium/high`；edge `sync_mode`/`evidence_state` 枚举错误；`focus` 非 `code/runtime/docs/web/memory`；数组超 `maxItems`（findings>50、nodes>32、edges>32、open_hops>16、evidence>20）；字符串超 `maxLength` 或低于 `minLength`；`evidence_id` 不合 `^ev_[a-f0-9]{13}$` 且无法被剔除。
- **C. 多余字段**：已被 P0 的 normalize 宽容剔除覆盖，**不再触发兜底**。
- **D. answer 非合法 JSON / 是任务契约回显**（`isTaskContractShape`）/ 截断多 fence。

**路径①失败、从而落到路径②的典型情形**：模型写了一份几乎完整的 report（findings、covered/unresolved goals 都有），仅 flow 内部某个小错（如 `flow.confidence` 写成 `"high "` 带空格、某 flow_node 缺 `kind`）。此时 `validatedOutput` 失败；路径①因 `covered`/`unresolved` 已存在而跳过（`result_recovery.go:297`）；落到路径②，**一个 flow 字段的小错换来整张 trace_deps 图**。

### 2.3 两个问题的同一哲学

问题 A/C 与问题 B 是同一"强制/兜底"哲学的两个表现：

- `completeMissingFlows`：流程图被**强制制造**（kind 说要画就硬补）。
- `BuildEvidencePreservingReport`：流程图被**强制兜底**（校验失败就拿 trace_deps 拼）。

共同点是"**宁可给一张错图/空图，也不给'没有图'**"。正确方向一致：**流程图该是"有才画、没就不画"，不该是"无论如何都得有一张"**。

## 3. 目标与非目标

### 3.1 目标

1. **该不该画流程图由 LLM 根据问题语义决定，不再由 kind 强制**：对"实体对比"类问题（学校、股票、汽车对比），LLM 判断不适合画 → 不画，答案里不出现空壳 FlowIR。
2. **区分两种图**：证据驱动的业务流程图（RGB/消息中心）靠索引证据，没证据不画；知识驱动的概念架构图（kafka/MySQL 架构）靠 LLM 通用知识，不需要索引证据也能正常产出——不被子 agent 的 `EvidenceRequired` 卡死。
3. 流程图是**调查的自然产出，不是契约任务**：`completeMissingFlows` 不再按 kind 强制补 flow；`enforceFlowContract` 的"有才画、没就不画"成为唯一决定。
4. 兜底要**有尊严地失败**：`BuildEvidencePreservingReport` 在没有可靠依赖证据时不画流程图，而非硬拼一张误导性的 trace_deps 扁平图。
5. 子 agent 调查缺口（症状 C）随之收敛：不再对索引覆盖不了、也不适合画概念图的主题强行调查，缺口自然减少。

### 3.2 非目标

1. 不改变 `enforceFlowContract` 的既有行为（有 flow 才装进答案）——它本来就是对的。
2. **不干掉 kind**——kind 保留检索预算、检索分区、证据 facet、路由等它擅长的配置职责，仅剥离"要不要画流程图"这项错位职责（详见 4.2）。
3. 不按"索引里有没有内容"判断该不该画——那会误杀"kafka 架构图"这类靠通用知识就能画的合理请求（详见 4.0）。
4. 不动 investigation.report 的 schema 本身。

## 4. 提案设计

### 4.0 前置澄清：为什么不能按"索引里有没有"判断

一个直观的错误方向是"索引/代码库里查不到内容就不画流程图"。这是错的——用户可能问"帮我输出一个 kafka 架构图"，kafka 不在我们的代码索引里，但这是完全合理、应该输出架构流程图的请求。按"索引有没有"判断会把这类合理请求误杀。

正确的区分不是"索引里有没有"，而是**流程图的两种类型 + 决定权归属**：

| 维度 | 证据驱动的业务流程图（RGB/消息中心/菜谱） | 知识驱动的概念架构图（kafka/MySQL/Redis 架构） |
|---|---|---|
| 内容来源 | **必须靠代码索引证据** | **靠 LLM 通用知识即可** |
| 没有索引证据时 | 画出空壳（症状 A） | 依然能画好 |
| 用户意图 | 理解"我们系统怎么实现的" | 理解"某技术/概念的结构" |
| 当前机制 | 适用（证据驱动） | **不适用**（被子 agent 的 `EvidenceRequired:true` 卡死） |

而"学校对比"两种都不是——它既不是"我们系统的业务流程"，也不是"某技术的架构"，而是"两个外部实体的对比判断"，**根本不该有流程图**。

因此"该不该画"是三个递进的判断，且**都该由 LLM 在理解问题后决定，而非 kind 分类器强制**：

1. **这个主题有流程/架构可画吗？**（kafka、业务链路 → 有；两所学校、两支股票 → 没有，这是实体对比）
2. **如果有，画它靠索引证据还是通用知识？**（我们的业务链路 → 靠索引证据，没证据就是空壳不画；kafka 架构 → 靠通用知识，不需要索引证据）
3. **用户要"图"作为产出吗？**（"帮我输出 kafka 架构图" → 图就是产出；"学校对比哪个好" → 图不是产出，不该硬塞）

### 4.1 方向 A：把"该不该画"的决定权从 kind 交给 LLM（治症状 A、C）

**核心：流程图由 LLM 根据问题语义决定，不再由 kind 强制。**

1. **取消 kind 对流程图的强制**：`completeMissingFlows` 不再按 `QueryNeedsFlow(kind)` 推出的 `OutputContract.Subjects` 强制补 flow。流程图的有无由 LLM 在合成答案时根据"这个问题是否适合画流程图"自行决定——适合（kafka 架构、业务流程）就画，不适合（学校对比）就不画，答案里不出现空壳 FlowIR。
2. **区分两种图的产出路径**：
   - **证据驱动的业务流程图**：仍走子 agent 调查，有证据才画、没证据不画。
   - **知识驱动的概念架构图**：LLM 凭通用知识直接画（如 kafka 架构），**不走 `EvidenceRequired:true` 的子 agent 调查**——kafka 不在索引里，派子 agent 找证据只会产出空壳。这要求放开子 agent 的 `EvidenceRequired` 一刀切，或允许父级 LLM 在无证据时直接产出概念性 flow。
3. **实体对比类问题**（学校/股票/汽车对比）：LLM 判断"不适合画流程图"→ 不画。

### 4.2 方向 B：kind 不是干掉，而是瘦身（治症状 A 的机制基础）

**核心：kind 保留它擅长的检索/调查配置职责，只把"画不画流程图"这项错位的职责交出去。**

`QueryKind`（7 种）在整个链路里承接 6 项业务逻辑，不能简单干掉：

| # | 消费点 | kind 决定什么 | 是否该剥离 |
|---|---|---|---|
| 1 | `RequiredFacetsFor(kind)` | 收集哪些证据 facet | 保留（调查完整性） |
| 2 | `retrievalPolicyFor(kind)` | 检索预算、是否搜 runbook、是否展开 codegraph | 保留（工程权衡） |
| 3 | `PartitionsByEntity(kind)` | 多实体是否分区检索 | 保留（检索质量） |
| 4 | `QueryNeedsFlow(kind)` | 要不要画流程图 | **剥离 → 交给 LLM** |
| 5 | `AnswerInstructionFor(kind)` | 答案组织方式 | 保留为软信号（prompt 本就允许 LLM 覆盖） |
| 6 | 路由（focused_fact 特殊路径） | 检索路由 | 保留 |

**结论**：kind 是 QA 链路的"总配置开关"，检索预算、分区、证据收集这些骨架靠它撑着，不能拆。但"要不要画流程图"是它管不好的错位职责（学校对比和 kafka 架构它分不清），应剥离给 LLM。**这不是干掉 kind，是给 kind 瘦身，把错位的职责还回去。**

### 4.3 方向 C：兜底有尊严地失败（治症状 B）

**核心：`BuildEvidencePreservingReport` 没有可靠依赖证据时不画流程图。**

当前 `fallbackFlow` 只要能从证据单元提取出依赖边就拼一张图，不管这张图是否有意义。改为：
- `fallbackFlow` 只在**存在带命名调用点（symbols）的依赖边**时才生成（现有逻辑已部分如此：`len(symbols)==0` 的边被跳过）。
- 若提取出的有效边为 0（或节点全是无 label 的 unknown），**不生成 flow 字段**——让路径②的 report 不含 flow，而不是塞一张扁平依赖图。
- 更进一步：路径② `BuildEvidencePreservingReport` 本就是"保留证据"的兜底，它的职责是**保留 findings/gaps**，不该承担"再造一张流程图"的职责。流程图在兜底阶段应**直接缺省**，由 `enforceFlowContract` 的"没就不画"自然处理。

**效果**：校验失败时，答案里要么有模型自己写的 flow（路径①保留），要么没有 flow（路径②/③），**不再出现 trace_deps 冒充的扁平图**。

## 5. 改动点清单（映射到代码）

| 方向 | 位置 | 改动 |
|---|---|---|
| A | `internal/agent/execution/loop_execution.go` `completeMissingFlows` | 不再按 `OutputContract.Subjects` 强制补 flow；流程图有无由 LLM 决定 |
| A | `internal/agent/qa/submission.go` `outputContractForQuery` / `internal/domain/query_plan.go` `QueryNeedsFlow` | `QueryNeedsFlow` 不再驱动 `OutputContract.Subjects` 强制补全，降级为提示 LLM 的软信号 |
| A | 子 agent `EvidenceRequired`（`internal/agent/delegation/executor.go`） | 放开一刀切：知识驱动的概念架构图（kafka 等）允许凭通用知识产出 flow，不强制索引证据 |
| B | `internal/domain/query_plan.go` / 各消费点 | kind 职权收窄：保留检索预算/分区/证据 facet/路由，仅剥离 `QueryNeedsFlow` 的流程图强制 |
| C | `internal/agent/execution/evidence_fallback.go` `fallbackFlow` / `BuildEvidencePreservingReport` | 无有效依赖边/节点全 unknown 时不生成 flow 字段；兜底不承担再造流程图职责 |

## 6. 落地顺序

| 阶段 | 内容 | 收益 |
|---|---|---|
| **P0** | 方向 C：兜底不硬拼 trace_deps 图 | 立刻消除"trace_deps 冒充流程图"（症状 B），改动小、风险低 |
| **P1** | 方向 A：`completeMissingFlows` 不再强制补 flow + `QueryNeedsFlow` 降级为软信号 | 消除空壳流程图（症状 A）；知识驱动的概念架构图（kafka）也能正常产出 |
| **P2** | 方向 A 配套：放开子 agent `EvidenceRequired` 一刀切，支持知识驱动的概念架构图 | kafka/MySQL 架构等概念图不再被证据要求卡死 |
| **P2** | 方向 B：kind 职权收窄的文档化与边界确认 | 明确 kind 只管检索/调查，不管流程图 |

**P0 先行**：方向 C 最独立、风险最低，先消除症状 B。方向 A 是核心哲学调整（决定权交给 LLM），随后。放开 `EvidenceRequired` 和 kind 瘦身是配套，可在 A/C 稳定后推进。

## 7. 验证方式

1. **症状 A（不再硬画）**：重跑"盐城师范和吉林工程技术师范学院对比"，断言答案里**不出现**空壳 FlowIR（无 `kind:"unknown"` 占位节点、无全 Unresolved 的 flowir 块）。
2. **症状 B（不再冒充）**：构造一个 flow 字段有小错（如 `flow.confidence` 带空格）但 findings 完整的子 agent report，断言最终答案**不含** `subject:"Evidence-derived flow"` 的扁平依赖图。
3. **症状 C（缺口收敛）**：对一个索引覆盖不了的实体提问，断言不派 trace 子 agent、报告缺口收敛。
4. **正向（kafka 架构图不被误杀）**：提问"帮我输出一个 kafka 架构图"，断言**能产出**一张概念架构流程图（知识驱动，不依赖代码索引证据），证明"该不该画"的决定权交给 LLM 后合理请求不被误杀。
5. **回归（业务流程图不受影响）**：对正常业务流程问题（如"RGB 灯效/消息中心/菜谱/TTS 流程"），断言流程图仍正常生成且为模型语义化流程，不受影响。
