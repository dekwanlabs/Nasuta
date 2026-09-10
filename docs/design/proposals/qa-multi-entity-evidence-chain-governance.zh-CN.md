# 多实体问答证据链路治理（逐实体检索 · 种子实体隔离 · 流程图确定性归并）

状态：草案
作者：wangdequan
日期：2026-09-10
关联事项：续接 [`qa-comparison-entity-and-evidence-coverage.zh-CN.md`](qa-comparison-entity-and-evidence-coverage.zh-CN.md)、[`qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md`](qa-child-investigator-evidence-seeding-and-bounded-budget-proposal.zh-CN.md)、[`qa-investigation-flow-coverage-and-gap-chase-proposal.zh-CN.md`](qa-investigation-flow-coverage-and-gap-chase-proposal.zh-CN.md)、[`qa-child-agent-bounded-context-and-reasoning-governance.zh-CN.md`](qa-child-agent-bounded-context-and-reasoning-governance.zh-CN.md)；触发运行 `run_e6c2ffa9f9a112c2e0088a3c`、`run_2c5be2f356a2ebdfa162acf3`，日志 `codeloom/logs/all-2026-09-10.log`
目标版本：可选

## 1. 摘要

本提案治理多实体问答（一次点名多个业务）证据链路中的一条根因链：从检索层的实体坍缩，到委派子调查的种子污染，再到流程图的主体不可信，以及子调查执行期的预算/压缩边界。

当前，当用户一次问及多个业务（例如「分析 rgb 灯效、消息中心、菜谱、tts 的流程」）时，planner 能识别出 4 个实体，但证据链路逐层丢失实体维度：检索层把整句问题 embed 成一个向量做单次召回，代码命中 100% 坍缩到信号最强的 rgb、服务检索为空；委派子调查又只按 facet 过滤父预检索证据，把 rgb 种子注入消息中心/菜谱/tts 子 Agent；子 Agent 生成的流程图 subject 由模型自由书写，被污染时写出「RGB灯效下发链路」的第二张 RGB 图，服务端按该字符串归并导致一模块多图、另一模块无图；最后，预算判定差 1 token 即判失败、结构化工具结果被压缩到 96 tokens，已获取证据在边界处被丢弃。

本提案计划沿同一实体维度打通全链路：检索按 `QueryPlan.Entities` 逐实体召回、种子按实体隔离、流程图按服务端实体 id 归并，并修正预算容差与压缩保字段两个边界。预期实现每个被点名业务都召回自己的证据、每个业务恰好一张流程图、逼近预算上限时仍保留已获取证据。

## 2. 背景

### 2.1 业务与技术背景

QA 链路为「planner 产出查询计划 → 检索层召回 → 委派子调查 → 合成回答」。多实体问题（比较、并列、总览）在 planner 层被建模为 `domain.QueryPlan{Kind, Entities}`，实体列表是贯穿全链路的关键维度：检索应逐实体召回，子调查种子应按实体隔离，流程图应按实体归并。

当前相关链路为：

```text
QA 请求
→ planner 产出 QueryPlan{Kind=comparison, Entities=[4]}
→ RetrievePlan（丢弃 Entities，单向量召回）
→ 委派 delegate_investigation（子任务各带 objective/实体）
→ defaultSeedContext（只按 facet 过滤种子）
→ 子 Agent 产出 FlowIR（subject 由模型生成）
→ MergeFlowIRsBySubject（按 subject 字符串归并）
→ canonicalFlowAnswer（按 subject 插到正文小节）
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/retrieval` | 单向量召回 + 组装 | searchQuery | RetrievedContext |
| `internal/agent/delegation` | 拆子任务、注入种子、收口报告 | QueryPlan、父上下文 | DelegationReport |
| `internal/agent/execution` | 归并 FlowIR、渲染、预算/压缩 | delegatedFlows | 最终回答 |
| `internal/agent/budget` | 预算记账与判定 | Usage | ErrBudgetExceeded |

### 2.2 当前实现

- `internal/retrieval/pipeline.go:247` `RetrievePlan`：对 `searchQuery` 调用一次 `EmbedQuery`，`QueryPlan.Entities` 只用于选策略、不用于召回；
- `internal/retrieval/pipeline.go:182` `retrievalPolicyFor`：`QueryComparison` 下 `expandCodeGraph=false`；
- `internal/agent/delegation/executor.go:3016` `defaultSeedContext`：按 `evidenceMatchesFacets` 过滤种子，无实体维度；
- `internal/agent/delegation/report.go:217`：`FlowIR.subject` 随模型 JSON 进入；`flow_merge.go:44` `MergeFlowIRsBySubject` 按模型 subject 字符串归并；
- `internal/agent/execution/evidence_fallback.go:288`：兜底图 subject 硬编码 `"Evidence-derived flow"`；
- `internal/agent/budget/budget.go:414` `RequireWithin`：严格 `>` 判定；`internal/agent/execution/answer_context_compaction.go:246`：head-tail-fallback 压缩。

### 2.3 为什么现在需要修改

由 2026-09-10 的四业务提问触发：

- 触发时间：2026-09-10 00:08 / 09:41（+08:00）；
- 触发标识：`run_e6c2ffa9f9a112c2e0088a3c`、`run_2c5be2f356a2ebdfa162acf3`；
- 直接表现：`kind=comparison entities=4` 但 `anchor` 服务全为 rgb/scene、`service=empty`；消息中心/菜谱/tts 子报告自述「种子几乎全部是 RGB」；消息中心子 Agent 的 flow 标题为「RGB灯效下发链路」；RGB 子 Agent 因 `output=16603 vs available_output=16602` 差 1 token `status=failed`；list_apis 压缩 `8875->96 tokens`；
- 影响范围：所有 `entities>1` 的比较/并列类问题；
- 临时处置：无。

### 2.4 范围与非目标

#### 目标

1. 检索层按 `QueryPlan.Entities` 逐实体召回，多实体不再坍缩为单实体；
2. 委派种子按业务实体隔离，弱实体子 Agent 不再拿到强实体种子；
3. 流程图按服务端实体 id 归并，一实体一图；
4. 修正输出预算容差与结构化压缩保字段，边界下不丢证据。

#### 非目标

1. 本提案不改变子调查预算的总量分配策略；
2. 本提案不解决流程图「缺失」与缺口追查（已由 [`qa-investigation-flow-coverage-and-gap-chase-proposal.zh-CN.md`](qa-investigation-flow-coverage-and-gap-chase-proposal.zh-CN.md) 覆盖）；
3. 本提案不通过「放大 topN / 调大预算 / 提高超时」等容量手段掩盖问题。

## 3. 问题

### 3.1 问题描述

**期望行为：**

多实体问题的每个被点名业务都能召回自己的证据、每个业务恰好一张流程图；逼近预算上限时仍保留已获取证据。

**实际行为：**

检索坍缩到最强实体、弱实体子调查被污染、流程图一模块多图/标题错位、边界下证据被丢弃。

**差异：**

planner 已识别出的实体列表在检索/委派/渲染三层逐一丢失，形成「识别到 4 个实体」与「只产出 1 个实体的证据」的断裂。

### 3.2 根因一：检索层多实体坍缩

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 四业务问题只有 rgb 拿到完整证据链 | 最终回答「只有 RGB 灯效能闭环」 |
| 直接原因 | `RetrievePlan` 单向量召回，代码命中 100% rgb、service 空 | 日志 L76-92 代码命中全 rgb；L95 `service=empty` |
| 机制根因 | 检索入口不使用 `QueryPlan.Entities`；comparison 策略关闭 `expandCodeGraph` | `pipeline.go:247`、`retrievalPolicyFor` |

根因链路：

```text
多实体问题 → planner 产出 Entities=4
→ RetrievePlan 丢弃 Entities，单向量 EmbedQuery
→ dense 分数挤窄带 + BM25 稀疏项被高频 token 主导
→ code 命中坍缩、service 空、codegraph 禁用
→ anchor 服务全为最强实体
```

本问题不能只通过「追加关键词 / 调大 topN / 提高 rerank 预算」解决，因为坍缩发生在检索入口的实体粒度：只要入口仍把多实体揉成一个向量，弱信号实体就竞争不过强信号实体。

### 3.3 根因二：委派种子只按 facet 隔离

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 消息中心/菜谱/tts 子报告均称种子是 rgb | 子报告 summary 原文 |
| 直接原因 | `defaultSeedContext` 只按 facet 过滤，facet 与主题正交 | `executor.go:3086` `evidenceMatchesFacets` |
| 机制根因 | 种子注入阶段缺少实体维度契约，与投影阶段的按实体过滤断层 | `defaultSeedContext` vs `investigator_projection.go` |

根因链路：

```text
父预检索被强实体主导
→ defaultSeedContext 只按 facet 过滤
→ 强实体的 entrypoint/core_flow 证据通过 facet 匹配
→ 注入弱实体子 Agent → 方向被带偏
```

本问题不能只通过「让子 Agent 自己再检索一遍覆盖污染」解决，因为种子是子 Agent 冷启动起点，污染会持续带偏其后续检索与报告生成。注意此处存在跨提案断层：`qa-comparison-entity-and-evidence-coverage` 的按实体过滤只作用在投影阶段，而后续新增的 `defaultSeedContext` 退回到只按 facet。

### 3.4 根因三：流程图主体由模型生成

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | RGB 两张图、消息中心无图、兜底图无意义 | 最终回答 flowir 块 |
| 直接原因 | `MergeFlowIRsBySubject` 按模型 subject 字符串归并，同名图不合并 | `flow_merge.go:44` |
| 机制根因 | FlowIR.subject 由模型生成，缺乏服务端确定性身份 | `report.go:217`、`evidence_fallback.go:288` |

根因链路：

```text
委派任务（有实体标识）
→ 子 Agent 模型生成 flow.subject（可能写错/不一致）
→ MergeFlowIRsBySubject 按 subject 字符串归并
→ 同实体不同 subject 不合并 → 一模块多图
→ 兜底图 subject 硬编码 → 无意义图
```

本问题不能只通过「在 prompt 里要求模型写对标题」解决，因为模型标题是自由文本，无法保证确定性；身份键必须由服务端持有。

### 3.5 根因四：子调查预算与压缩边界

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | RGB 子 Agent failed、菜谱 API 详情丢失 | 日志 `status=failed`、`8875->96` |
| 直接原因 | 严格 `>` 比较 + head-tail 截断 | `budget.go:414`、`answer_context_compaction.go:246` |
| 机制根因 | 预算记账无容差、压缩策略未区分结构化/自由文本 | `remaining`、`tooloutput.Compress` |

根因链路：

```text
子调查最后一步输出 16603 tokens，可用 16602
→ RequireWithin 严格 > 判定 → ErrBudgetExceeded → status=failed
（并行的另一缺陷）
list_apis 返回 8875 tokens → 超窗 → head-tail 压缩 → 96 tokens 详情丢失
```

本问题不能只通过「四舍五入 available」或「调大压缩上限」解决，因为需要机制级修正：预算判定应允许最后一步完成，压缩应识别结构化结果的字段结构。

### 3.6 影响

- **用户影响：** 并列/比较类问题只回答最强实体、弱实体答案不完整或张冠李戴；流程图标题错位、一模块多图；
- **业务影响：** 知识服务对多业务总览类问题不可信，用户需逐业务追问；
- **系统影响：** 下游委派浪费预算在无关主题深挖上；已消耗预算因 1-token 边界作废；
- **工程影响：** 实体列表作为计划层产物在检索/委派/渲染三层被静默丢弃，契约断裂，难以测试。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：四业务流程并列提问（检索坍缩）

- **Given：** 索引中存在 hsds-message-center、tts-proxy、hsds-aiot-recipe、hsds-scene；用户点名 4 业务；
- **When：** planner 判定 `comparison/entities=4`，进入 `RetrievePlan`；
- **Then：** 4 实体各召回各自代码/服务/runbook；
- **But：** 单向量召回后代码命中 100% rgb、service 空、anchor 全为 rgb/scene。

关键证据：

```text
[qa] canonical query plan kind=comparison required_facets=4 entities=4
[qa] retrieval sources: code=completed runbook=completed service=empty
[qa] codegraph keywords (cleaned): 0 []
[qa] anchor: services=6 codeHits=16 runbooks=12 first=[com.airone.hsdevice hsas-scene hsds-scene-provider hsds-aiot-service hsds-scene-api lvglgdb]
```

#### 场景 B：消息中心子 Agent 拿到 rgb 种子（种子污染）

- **Given：** 父预检索上下文被 rgb 主导；委派拆出「消息中心」子任务，focusFacets 含 entrypoint/core_flow；
- **When：** `defaultSeedContext` 按 facet 过滤父上下文；
- **Then：** 只注入与消息中心实体相关的证据，否则空种子；
- **But：** rgb 的 entrypoint/core_flow 证据通过 facet 匹配，被注入消息中心子 Agent。

关键证据：

```text
子报告 subject："任务要求梳理「消息中心」主流程，但本次返回的种子证据几乎全部是 RGB 灯效（RgbEffect）链路"
子报告 summary："本任务的目标是菜谱业务主流程，但可用的种子与检索证据主要集中在 RGB 灯效与依赖索引"
```

#### 场景 C：消息中心子 Agent 生成 RGB 标题（流程图主体）

- **Given：** 消息中心子 Agent 因污染生成 flow subject「RGB灯效下发链路（种子证据实际覆盖；消息中心链路证据不足）」；
- **When：** `MergeFlowIRsBySubject` 归并四张 flow；
- **Then：** 消息中心图以「消息中心」命名，与 RGB 图区分；
- **But：** 两张 RGB 图 subject 字符串不同，不合并 → RGB 两张图、消息中心无图。

关键证据：

```text
flow[RGB 子任务]    = { subject: "RGB 灯效端到端主链路（查询/编辑到 CBOR 下发）" }
flow[消息中心子任务] = { subject: "RGB灯效下发链路（种子证据实际覆盖；消息中心链路证据不足）" }
flow[菜谱子任务兜底] = { subject: "Evidence-derived flow" }
```

#### 场景 D：最后一步差 1 token 判失败（预算/压缩边界）

- **Given：** RGB 子 Agent 最后一步输出 16603 tokens，可用 16602；
- **When：** `account model call usage` 记账；
- **Then：** 允许最后一步完成并产出报告；
- **But：** 严格 `>` 判 `budget exceeded`，`status=failed`。

关键证据：

```text
[agent] ... err=agent step 6: account model call usage: agent budget exceeded: ... output=16603 available_output=16602 ...
[agent] answer context tool result compacted phase=model_step_2 tool=list_apis strategy=head-tail-fallback tokens=8875->96
```

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 单实体问题 | `entities<=1` | 单向量召回，正常 | 保持既有路径，不回归 |
| 实体无向量命中 | 某实体召回为空 | 无证据 | 标记 `entity_insufficient` |
| 子任务无实体标识 | objective 无实体 | 退回 facet-only | 退回 facet-only |
| 显式 evidence_refs | 父传了 ev_ 句柄 | 走 selectContext | 保持既有行为 |
| 预算恰好超 1 token | output=avail+1 | failed | 容差内允许完成 |
| 结构化工具结果 | list_apis 大结果 | head-tail 截断 | 保字段压缩 |

### 4.3 复现步骤

1. 准备含多业务的索引（hsds-scene / hsds-message-center / tts-proxy / hsds-aiot-recipe）；
2. 提问「分析 rgb 灯效、消息中心、菜谱、tts 的流程」；
3. 观察 `[qa] anchor:`、`[qa] retrieval sources:` 日志，可见 `service=empty` 且 anchor 全为单一业务；
4. 观察子报告 summary，可见弱实体子 Agent 自述 rgb 种子污染；
5. 观察最终回答 flowir 块，可见一模块多图或通用兜底图；
6. 观察 `budget exceeded` 与 `tokens=X->Y` 压缩日志，可见边界证据丢失。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 逐实体召回、实体隔离、服务端 subject、预算容差与保字段压缩均基于实体维度的通用机制，对任意多实体问题通用；
2. **保持单一事实源。** 实体列表的唯一事实源是 `QueryPlan.Entities` / 委派任务的 `EntityID`；检索、种子、归并只消费不重复推导；
3. **明确职责边界。** 检索层负责逐实体 fan-out；委派层负责种子实体匹配；渲染层负责按实体归并；预算/压缩层负责边界容差与保字段；
4. **失败可诊断。** 逐实体结果、空种子、无实体 id、容差触发均记录结构化日志；
5. **兼容与可回滚。** 单实体路径与无实体标识路径完全不变；各改动可分别开关。

### 5.2 目标流程

```text
RetrievePlan → 逐实体 EmbedQuery + 逐实体 discover → 合并去重、标记实体来源
→ delegate_investigation（子任务携带 EntityID/Label）
→ defaultSeedContext（facet + 实体双层过滤）
→ 子 Agent 产出 FlowIR → 服务端按 EntityID 覆盖 subject
→ MergeFlowIRsBySubject 按 EntityID 归并 → 一实体一图
→ 预算容差 + 结构化保字段压缩 → 边界不丢证据
```

与当前流程相比，关键变化是：

1. 检索层从「单向量」改为「逐实体 fan-out + 合并」；
2. 种子注入从「只按 facet」改为「facet + 实体」双层过滤；
3. 流程图身份从「模型 subject」改为「服务端 EntityID」；
4. 预算判定引入容差、结构化压缩改为保字段。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 逐实体召回 | 单次 `discover` | `comparison && entities>1` 逐实体 `discover` 后合并 | `internal/retrieval/pipeline.go` | 单实体路径不变 |
| codegraph 扩展 | comparison 下 false | 逐实体召回后 true | `internal/retrieval/pipeline.go:182` | 仅多实体 |
| 种子实体过滤 | 只按 facet | facet + 实体双层过滤 | `internal/agent/delegation/executor.go` | 无实体标识退回 facet |
| 任务实体标识 | 子任务仅带 objective | 子任务携带 EntityID/Label | `internal/agent/delegation` | 新增字段，默认空 |
| 流程图归并 | 按模型 subject | 按服务端 EntityID 归并 | `internal/agent/delegation/flow_merge.go` | 无 EntityID 退回 subject |
| 兜底图命名 | 硬编码 "Evidence-derived flow" | 按实体命名 | `internal/agent/execution/evidence_fallback.go` | 无实体用通用名 |
| 预算容差 | 严格 `>` | `> available + tolerance` | `internal/agent/budget/budget.go` | 容差可配 |
| 结构化压缩 | head-tail 统一 | 结构化结果保字段 | `internal/agent/execution/answer_context_compaction.go` | 自由文本不变 |

#### 改动一：检索层逐实体召回

**方案：**

在 `RetrievePlan` 中，当 `query.Kind == domain.QueryComparison && len(query.Entities) > 1` 时，为每个实体构造实体级查询（实体名 + 别名 + 该 kind 的 facet 描述词），逐实体执行 `discover`，再按实体合并 anchor 与命中并去重，`codeHit`/`anchor` 携带实体 id。合并结果交给现有 `expand`/`assemble`。`comparison` 策略的 `expandCodeGraph` 置为 true，让逐实体召回后依赖图可扩出弱实体上下游。

**约束：**

- 单实体问题完全走既有路径；
- 逐实体召回复用共享向量路径，避免重复 embed 全句；实体数设上限。

**失败行为：**

- 某实体召回为空时标记 `entity_insufficient`/`entity_missing` 并继续；
- embed 失败整体失败并返回明确错误，不静默退化为单向量。

#### 改动二：委派种子按实体隔离

**方案：**

`DelegationTask` 增加 `EntityID`/`EntityAliases`；`defaultSeedContext` 在 `evidenceMatchesFacets` 之上叠加 `evidenceMatchesEntity`（证据单元所属实体命中任务实体或别名）。有实体标识但无匹配时降级为空种子、子 Agent 冷启动，而非退回 facet-only。

**约束与失败行为：**

显式 `evidence_refs` 路径不变；子任务无实体标识时退回 facet-only 并记录 `seed_entity_unavailable`；有标识无匹配时返回空种子并记录 `seed_empty`。

#### 改动三：流程图服务端确定性归并

**方案：**

`FlowIR` 增加 `EntityID`；`projectReport` 用委派任务实体 label 覆盖 `flow.Subject`；`MergeFlowIRsBySubject` 改为按 `EntityID` 归并（`flowNodeKey` 用 `EntityID` 替代 subject 段）；兜底图 `fallbackFlow` 以任务实体命名。

**约束与失败行为：**

无 `EntityID` 时退回按模型 subject 归并并记录 `flow_entity_unavailable`；实体 label 缺失时保留模型 subject。

#### 改动四：预算容差与结构化保字段压缩

**方案：**

`RequireWithin` 对输出维度引入可配置容差 `outputTolerance`（`output ≤ available + tolerance` 通过，超出仍失败），输入/总 token 维度保持严格；`compressToolResults` 按工具类型识别结构化结果（如 `list_apis`），压缩时保留关键字段（路径/方法/摘要），自由文本保持 head-tail。

**约束与失败行为：**

容差默认保守、仅作用输出维度；无法识别结构化类型时退回 head-tail；保字段压缩后仍超预算再降级。

### 5.4 数据结构或接口契约

新增或修改的核心字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `codeHit.entityID` | `string` | retrieval | 命中所属实体 | 空 | 新增 |
| `RetrievedContext.EntityCoverage` | `map[string]bool` | retrieval | 实体是否有证据 | 空 | 新增 |
| `DelegationTask.EntityID` | `string` | delegation | 子任务所属实体 id | 空 | 新增 |
| `DelegationTask.EntityAliases` | `[]string` | delegation | 实体别名 | 空 | 新增 |
| `FlowIR.EntityID` | `string` | delegation | flow 所属实体 id（身份键） | 空 | 新增 |
| `outputTolerance` | `int64` | budget | 输出维度容差 token 数 | 保守值 | 新增配置 |

状态转换：

```text
[entity discover]
  ├─ 命中 → entity_covered
  ├─ 命中低于阈值 → entity_insufficient
  └─ 无命中 → entity_missing
```

不变量：

1. 有 `EntityID` 时，种子证据单元必须命中该实体；flow 按 `EntityID` 归并为一张；
2. 有 `EntityID` 时 `flow.Subject` 由 `EntityLabel` 派生；
3. 输出预算判定 `output ≤ available + tolerance`，输入/总 token 保持严格。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 单实体问题、非 comparison 问题、无实体标识的 flow、显式 evidence_refs、自由文本压缩均保持旧行为；新增字段默认空、可配容差默认 0；
- **数据迁移：** 无 schema 变更；
- **灰度方式：** 各改动独立开关，先对双实体问题灰度逐实体召回与种子隔离，再推广到流程图归并与边界容差；
- **回滚条件：** 多实体召回延迟/成本超预算、空种子导致召回下降、错误合并、预算失控；
- **回滚步骤：** 关闭对应开关即回到旧路径，无需数据回滚。

## 6. 修改伪代码

### 6.1 核心流程

```go
func (r *Retriever) RetrievePlan(ctx, searchQuery, terms, plan, query) (*RetrievedContext, error) {
    if query.Kind != domain.QueryComparison || len(query.Entities) <= 1 {
        return r.retrieveSingle(ctx, searchQuery, terms, plan, query) // 既有路径
    }
    merged := newAnchor()
    coverage := map[string]bool{}
    for _, entity := range query.Entities {
        a := r.discover(ctx, buildEntityQuery(entity, query), nil, false, vecFor(entity), true, query)
        if a.empty() { coverage[entity.ID] = false; continue }
        coverage[entity.ID] = true
        merged.merge(a, entity.ID)
    }
    rc := r.assemble(ctx, r.expand(ctx, merged, terms, plan, query), searchQuery, query)
    rc.EntityCoverage = coverage
    return rc, nil
}
```

### 6.2 关键边界处理

```go
// 种子注入：facet + 实体双层过滤
func defaultSeedContext(parent ParentContext, capability Capability, focusFacets []string, task DelegationTask, maxTokens int64) []ContextBlock {
    facetSet := toSet(focusFacets, capability.InputFacets)
    var blocks []ContextBlock
    for _, block := range parent.Context {
        if !isSeedBlock(block) { continue }
        filtered := cloneContextBlock(block)
        for _, rawUnit := range block.Evidence {
            unit, ok := canonicalContextEvidenceUnit(rawUnit)
            if !ok || !evidenceMatchesFacets(unit, facetSet) || !evidenceMatchesEntity(unit, task) { continue }
            filtered.Evidence = append(filtered.Evidence, unit)
        }
        if len(filtered.Evidence) > 0 { blocks = append(blocks, filtered) }
    }
    if len(blocks) == 0 && task.EntityID != "" {
        log.InfofCtx(nil, "[delegation] seed empty for entity=%s; child starts cold", task.EntityID)
    }
    return blocks
}

// 流程图归并：按服务端实体 id
func MergeFlowIRsBySubject(flows []FlowIR) ([]*FlowIR, error) {
    groups := map[string][]FlowIR{}
    for _, flow := range flows {
        key := flow.EntityID
        if key == "" { key = "subject:" + normalizeFlowText(flow.Subject) } // 退化
        groups[key] = append(groups[key], flow)
    }
    out := make([]*FlowIR, 0, len(groups))
    for _, g := range groups {
        merged, err := MergeFlowIRs(g)
        if err != nil { return nil, err }
        out = append(out, merged)
    }
    return out, nil
}

// 预算容差
func RequireWithin(request, available Usage, subject string) error {
    if request.InputTokens > available.InputTokens ||
        request.OutputTokens > available.OutputTokens+outputTolerance ||
        request.TotalTokens > available.TotalTokens ||
        request.CostMicros > available.CostMicros {
        return fmt.Errorf("%w: %s ...", ErrBudgetExceeded, subject, ...)
    }
    return nil
}
```

### 6.3 修改前后对比

修改前：

```go
// 单向量：多实体揉成一个向量，弱信号实体被淹没
queryVector, _ = vectorTools.EmbedQuery(ctx, searchQuery)
a = discover(ctx, searchQuery, nil, false, queryVector, true, query)

// 种子只按 facet：同 facet 异实体的证据混入子 Agent
if ok && evidenceMatchesFacets(unit, facetSet) { append(filtered.Evidence, unit) }

// 身份键 = 模型 subject：写错/不一致就不合并
key := strings.ToLower(normalizeFlowText(flow.Subject))

// 严格 > 判定：差 1 token 即失败
if request.OutputTokens > available.OutputTokens { return ErrBudgetExceeded }
```

修改后：

```go
// 逐实体召回
a, coverage := discoverPerEntity(ctx, query) // 逐实体 discover + 合并

// 种子按 facet + 实体双层过滤
if ok && evidenceMatchesFacets(unit, facetSet) && evidenceMatchesEntity(unit, task) { append(...) }

// 身份键 = 服务端实体 id：同实体必合并
key := flow.EntityID
if key == "" { key = "subject:" + normalizeFlowText(flow.Subject) }

// 容差判定：允许最后一步完成
if request.OutputTokens > available.OutputTokens+outputTolerance { return ErrBudgetExceeded }
```

### 6.4 配置或数据库变更

```yaml
retrieval:
  per_entity_fanout:
    enabled: true
    max_entities: 6
delegation:
  seed_entity_scope: true
flow:
  server_owned_subject: true
budget:
  output_tolerance_tokens: 8
compaction:
  structured_keep_fields: true
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 每个被点名业务各召回自己的代码/服务/文档，弱实体不再颗粒无收；
2. 每个子 Agent 只拿自身主题的种子，方向不再被带偏；
3. 每个业务恰好一张流程图，标题由服务端确定；
4. 逼近预算上限时仍能产出报告，结构化工具结果压缩后保留关键字段。

### 7.2 可观测性效果

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `EntityCoverage` | RetrievedContext 字段 | 每实体证据覆盖 |
| `seed_empty` / `seed_entity_unavailable` | 结构化日志 | 记录空种子/无实体退回 |
| `flow_entity_unavailable` | 结构化日志 | 记录无实体 id 退回 |
| `budget_tolerance_applied` | 结构化日志 | 记录容差触发 |

日志应至少能够回答：检索是否逐实体、每实体召回多少、种子是否按实体匹配、图是否按实体归并、预算/压缩边界是否触发。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 多实体 anchor 覆盖实体数 | 1 / 4 | 4 / 4 | 每次 | 日志 `anchor:` |
| 弱实体子报告跨主题污染 | 3 / 3 | 0 / 3 | 每次 | 子报告 summary |
| 图数量 = 实体数 | 否 | 是 | 每次 | 最终回答 flowir 块 |
| 1-token 边界误判失败 | 出现 | 0 | 每次 | `budget exceeded` 日志 |

### 7.4 不应发生的变化

- 单实体与非 comparison 问题行为保持不变；
- 大幅超预算仍判失败、自由文本压缩行为不变；
- 不引入针对「rgb/tts/消息中心/菜谱」等具体业务名的硬编码。

## 8. 测试与验收

### 8.1 单元测试

- 双实体 comparison 逐实体召回，合并后 anchor 覆盖两者；
- 种子按实体匹配、无匹配返回空种子；
- 同实体不同 subject 按 EntityID 合并；无 EntityID 退回 subject 归并；
- 预算 output=avail+容差内通过、大幅超失败；
- 结构化结果保字段、自由文本 head-tail 不变。

### 8.2 集成测试

- 多业务索引下提问四业务并列问题，断言 anchor 覆盖 4 业务、各子 Agent 种子无污染、最终 flowir 块数量=4 且标题各对应业务。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 四业务流程提问 | 四业务各召回、一实体一图 | 集成测试 |
| 单实体 | 单业务提问 | 行为不变 | 单测 |
| 显式 refs | 带 ev_ 句柄 | 行为不变 | 单测 |
| 大幅超预算 | 输出远超 | 仍失败 | 单测 |

### 8.4 验收标准

1. 四业务案例 anchor 覆盖 4 业务、service 检索不再为空；
2. 弱实体子报告不再自述 rgb 种子污染；
3. 图数量 = 实体数，兜底图以实体命名；
4. 1-token 边界误判归零、结构化压缩保留关键字段；
5. 单实体与显式 refs 路径无回归。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 召回延迟/成本放大 | 实体数多 | 首屏变慢 | 实体数上限 + 共享向量路径 | 延迟/成本超预算 |
| 空种子召回下降 | 实体标识过严 | 子调查证据变少 | 空种子冷启动 + 自检索兜底 | 召回显著下降 |
| 错误合并 | 实体 id 填错 | 跨实体合并 | 拆分侧校验 | 出现错误合并 |
| 预算失控 | 容差过大 | 累计超预算 | 容差保守 + 可配 | 预算失控 |

## 10. 实施计划

### 阶段 1：检索逐实体召回

- `RetrievePlan` 增加逐实体 fan-out；comparison 启用 `expandCodeGraph`；
- 退出条件：四业务 anchor 覆盖 4 业务。

### 阶段 2：种子与流程图实体化

- `DelegationTask`/`FlowIR` 增加 `EntityID`；种子双层过滤；归并按实体 id；兜底图按实体命名；
- 退出条件：无跨主题污染、一实体一图。

### 阶段 3：预算与压缩边界

- 输出容差 + 结构化保字段压缩；
- 退出条件：1-token 误判归零、结构化保留率达标。

### 阶段 4：灰度与清理

- 各开关独立灰度，观察覆盖率/延迟/成本/污染率；
- 退出条件：指标达标、单实体回归通过；清理退化分支。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 逐实体召回粒度 | code+service+runbook 三路逐实体 | 仅 code 逐实体 | A | service 空正是逐实体需修复的点 |
| 实体标识来源 | 拆分侧显式填充 | objective 提取 | A | 单一事实源在拆分侧 |
| 无种子匹配策略 | 空种子冷启动 | 退回 facet-only | A | 避免再次引入跨主题噪声 |
| 归并键 | 实体 id | 实体 id + subject 归一化 | A | 单一身份，避免字符串漂移 |
| 预算容差形式 | 固定 token 数 | 百分比 | A | 可预测、可观测 |

## 12. 决策摘要

本提案建议：

1. 检索层对 `comparison && entities>1` 逐实体召回，comparison 启用 codegraph 扩展；
2. `DelegationTask`/`FlowIR` 增加实体标识，种子按实体隔离、流程图按实体归并、subject 由服务端派生；
3. 输出预算引入保守容差、结构化工具结果保字段压缩；
4. 各改动独立开关、分别灰度，通过覆盖率/污染率/图数量/误判率验证；
5. 单实体与无实体标识路径保持旧行为，出现延迟/成本/召回/合并异常时逐项回滚。

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
