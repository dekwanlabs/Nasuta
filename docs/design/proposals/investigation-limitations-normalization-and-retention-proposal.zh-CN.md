# Investigation 限制项归一化、排序与详细结果留存提案

状态：草案  
作者：Nasuta Agent Platform  
日期：2026-08-18  
关联事项：`workflow_592090ef0cdc50a3e5f55d88`、trace `3d4ceaafb7b8`、`investigation.answer` Schema v1/v2  
目标版本：待评审

## 1. 摘要

本提案用于解决多 Agent Investigation 流程中 `limitations` 列表重复、冗余、顺序不稳定，以及最终答案无法同时满足“简洁展示”和“完整保留细节”的问题。

当前，多个 investigator 和 verifier 可能分别产生限制项。系统虽然能够把限制项汇总出来，但缺少统一的归一化流程：相同限制可能重复出现，相似限制没有合并，限制项没有按影响程度排序，最终 Synthesizer 只能把大量限制直接写进最终答案。此前 `investigation.answer v1` 只允许 20 条限制项，导致 28 条合法限制项无法通过 Schema 校验。

本提案计划增加一个由服务端负责的限制项整理阶段，将流程从：

```text
多个 investigator 生成限制项
→ verifier 汇总限制项
→ Synthesizer 原样搬运全部限制项
→ 最终答案过长或超过 Schema 上限
→ 任务失败或用户看到大量重复信息
```

调整为：

```text
多个 investigator 生成原始限制项
→ verifier 保留完整限制项及来源
→ limitation normalizer 去重、合并、分级、排序
→ 生成“主要限制”展示列表
→ 全量归一化结果写入 detail artifact
→ Synthesizer 只展示主要限制并引用详细结果
```

最终目标是：**不丢失任何限制信息，同时让用户看到的答案简洁、稳定、按重要程度排序。**

## 2. 背景

### 2.1 业务与技术背景

Investigation 是多 Agent 并行调查流程。不同 investigator 负责代码、运行时、文档、Web 或记忆等不同证据来源，之后由 verifier 合并证据，再由 Synthesizer 生成最终回答。

限制项不是普通的模型附加说明，而是回答可信度的重要组成部分，可能表示：

- 某个结论只有部分证据支持；
- 某个下游服务或能力不可用；
- 某个链路无法继续验证；
- 存在多个实现，但无法证明它们属于同一条链路；
- 某些证据因预算被裁剪；
- 运行时、时间范围或数据新鲜度有限制；
- 证据之间存在冲突或未解决的依赖。

因此，限制项不能简单粗暴地截断。系统需要区分“用户应该立即看到的主要限制”和“仍然必须可追溯的详细限制”。

### 2.2 当前实现

- `internal/agent/catalog/schema.go`：定义 `investigation.verified_bundle` 和 `investigation.answer` Schema；
- `internal/agent/qa/evidence.go`：组织 verifier 输入和已验证证据；
- `internal/agent/workflow/agent_node.go`：执行 investigator、verifier 和 synthesizer 节点；
- `internal/agent/workflow/service_execution.go`：推进工作流节点并处理节点失败；
- `internal/agent/workflow/store_execution.go`：持久化工作流运行状态和 artifact；
- `internal/agent/catalog/defaults_investigation.go`：定义 investigator、verifier、synthesizer 的 Agent Contract；
- `internal/prompts/text/agent/catalog/synthesizer.txt`：约束最终答案的生成格式；
- `internal/agent/run/hub.go`：持久化 Agent step 和 artifact；
- `internal/agent/workflow/investigation.go`：编译 Investigation workflow。

当前已存在的兼容性修复是：

- 保留 `investigation.answer v1` 不变；
- 新增 `investigation.answer v2`，允许最多 100 条限制项；
- 新的 Synthesizer 使用 `investigation.answer v2`。

本提案是在该兼容性修复之上，进一步解决限制项质量、展示和详细结果留存问题。

### 2.3 为什么现在需要修改

本次修改由以下线上失败触发：

- 触发时间：2026-08-18 10:11:36 +08:00；
- 触发标识：trace `3d4ceaafb7b8`；
- 工作流：`workflow_592090ef0cdc50a3e5f55d88`；
- 直接表现：`investigation.answer v1` 校验失败，`limitations` 数量为 28，超过 Schema 上限 20；
- 根因：上游 verified bundle 允许产生更多限制项，下游最终答案直接接收全部限制项，且没有整理、排序和详情留存机制；
- 临时处置：新增 `investigation.answer v2`，将可接收上限提高到 100。

提高 Schema 上限可以避免当前失败，但不能解决以下长期问题：

- 重复限制仍然存在；
- 相似限制仍然占用多个展示位置；
- 用户无法快速判断哪些限制最重要；
- 如果未来限制项超过 100，问题仍会再次出现；
- 全量限制和面向用户的摘要没有明确的职责边界。

### 2.4 范围与非目标

#### 目标

1. 对限制项进行稳定、可解释的精确去重；
2. 对具有相同根因的相似限制进行安全合并；
3. 为每条限制项分配严重程度和排序依据；
4. 最终答案只展示有限数量的主要限制；
5. 将全部归一化限制项、来源和合并关系写入 detail artifact；
6. 保证历史 v1/v2 workflow 和历史 run 仍可读取；
7. 在日志、指标和 artifact 中能够追溯“展示了什么、隐藏了什么、为什么合并”。

#### 非目标

1. 本提案不改变已有历史 Schema v1/v2 的内容；
2. 本提案不通过针对某个 Trace ID、问题关键词或具体业务名称的特例解决问题；
3. 本提案不允许服务端静默丢弃未展示的限制项；
4. 本提案不重新设计 investigator 的证据搜索策略；
5. 本提案不把限制项的完整内容塞回用户答案以规避 artifact 持久化问题；
6. 本提案不把安全、权限或执行预算交给模型决定。

## 3. 问题

### 3.1 问题描述

**期望行为：**

- 相同限制只展示一次；
- 相同根因的限制能够合并，并保留所有来源；
- 高风险、高影响限制排在前面；
- 用户答案只展示主要限制；
- 未展示的限制仍然完整保存在详细 artifact 中；
- 任何合并、隐藏和排序都可以审计和复现。

**实际行为：**

- verifier 输出的限制项直接传给 Synthesizer；
- Synthesizer 需要自己判断是否去重、合并和排序；
- Schema 只约束数组最大数量，不表达限制项之间的关系；
- 超出上限时整个节点失败，而不是生成“主要限制 + 详细结果”；
- 即使没有超过上限，用户也可能看到一长串重复或低价值限制。

**差异：**

当前系统把“完整性存储”和“用户展示”混成了同一个 `limitations` 数组，导致展示上限同时成为数据保存上限。

### 3.2 根因分析

```text
多 Agent 独立产出限制项
→ 限制项没有统一身份、分类、严重程度和来源模型
→ verifier 只做汇总，不做可审计的归一化
→ Synthesizer 被迫承担去重、合并、排序和写作
→ 输出 Schema 只能用 maxItems 做粗粒度保护
→ 限制项多时，出现重复、冗余或 invalid_output
```

机制层根因有四个：

1. **限制项没有结构化身份**：系统只有字符串，没有稳定的 limitation ID、来源集合和合并关系；
2. **归一化职责放错位置**：把确定性的数据整理工作交给了 LLM，而不是由服务端完成；
3. **展示模型和存储模型没有分离**：用户看到的列表和完整审计数据共用一个数组；
4. **Schema 上限承担了业务整理职责**：`maxItems` 只能拒绝超限，不能告诉系统哪些应该展示、哪些应该保存到详情。

### 3.3 影响

- **用户影响：** 最终答案可能包含大量重复限制，或因为输出超过上限而直接失败；
- **业务影响：** 复杂问题的成功率下降，用户无法得到稳定的最终结论；
- **系统影响：** 已经完成的调查和验证结果因为最终展示阶段失败而被整体判定失败；
- **工程影响：** 只能不断提高 `maxItems`，无法保证列表质量，也无法审计模型如何删改限制。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：相同限制来自多个 Agent

- **Given（前置条件）：** code investigator、runtime investigator 和 docs investigator 都发现“无法验证最终设备下发服务”；
- **When（触发行为）：** verifier 汇总多个 investigator 的限制项；
- **Then（期望结果）：** 生成一条合并限制，保留三个来源；
- **But（当前结果）：** 可能生成三条内容相同或近似的限制，占用三个展示位置。

```text
原始限制 1：未能验证最终设备服务的实际响应。
原始限制 2：缺少设备服务下游的运行时证据。
原始限制 3：无法确认指令是否真正到达设备服务。

期望合并：
限制：无法用当前证据验证指令是否最终到达设备服务。
来源：code、runtime、docs
严重程度：high
```

#### 场景 B：限制项很多但重要程度不同

- **Given：** verifier 产生 28 条限制，其中 2 条会影响核心结论，10 条属于部分覆盖，16 条属于低风险细节；
- **When：** Synthesizer 生成最终答案；
- **Then：** 用户优先看到会改变结论可信度的 2 条主要限制；
- **But：** 当前流程要求模型直接输出全部限制，旧 Schema 甚至会因为数量超过 20 而拒绝整个答案。

#### 场景 C：详细限制需要追溯

- **Given：** 最终答案只展示 10 条主要限制；
- **When：** 用户或工程师需要审查为什么某些限制没有展示；
- **Then：** 可以通过 detail artifact 查看完整列表、去重关系、合并来源、排序分数和隐藏原因；
- **But：** 当前没有统一的详细限制 artifact 契约。

### 4.2 边界场景

1. 所有限制项都是重复项；
2. 限制项为空；
3. 限制项超过展示上限但未超过 verified bundle 上限；
4. 限制项超过内部安全上限；
5. 两条限制文本相似但根因不同，不能误合并；
6. 严重程度缺失或不合法；
7. artifact 持久化失败；
8. 同一个 workflow run 重试或重复完成；
9. 历史 run 仍引用 `investigation.answer v1/v2`；
10. 多个并发 workflow 同时写入限制详情。

### 4.3 复现步骤

1. 构造一个包含至少两个 investigator 的 Investigation 请求；
2. 让不同 investigator 分别返回相同或相似的限制项；
3. 让 verifier 返回 28 条限制项；
4. 使用旧的 `investigation.answer v1` 执行 Synthesizer；
5. 观察输出合法 JSON 在 Schema 校验阶段因 `maxItems: 20` 失败；
6. 使用 v2 执行同样输入，观察虽然可以通过，但最终答案会展示全部限制项，缺少归一化和详细结果分离。

## 5. 如何修改

### 5.1 修改原则

1. **完整数据优先**：任何原始或归一化限制项都不能因为展示上限被静默删除；
2. **确定性工作服务端完成**：精确去重、排序、数量截断和 artifact 写入不交给模型；
3. **语义合并必须可追溯**：相似限制只有在有明确共同根因或可信分组依据时才允许合并；
4. **展示与存储分离**：最终答案展示主要限制，artifact 保存完整细节；
5. **Schema 版本不可变**：不修改已发布的 v1/v2 内容，新增后续版本；
6. **失败不伪成功**：详细结果保存失败时必须重试并暴露明确状态，不得假装已经完成完整留存；
7. **旧任务可读，新任务渐进升级**：历史任务继续使用其绑定的旧 Schema 和 workflow 版本，新任务使用新契约。

### 5.2 目标流程

```text
investigator reports
→ verifier 生成完整、带来源的 limitation records
→ limitation normalizer
   ├─ 文本规范化
   ├─ 精确去重
   ├─ 相似限制分组/合并
   ├─ 严重程度归一化
   ├─ 影响分排序
   └─ 生成显示列表和完整详情
→ 先持久化 limitation detail artifact
→ Synthesizer 读取显示列表和 detail_ref
→ 生成主要限制 <= display_limit
→ 服务端校验 answer contract
→ 完成 workflow
```

### 5.3 详细改动

#### 改动一：引入结构化 LimitationRecord

verifier 输出不再只包含字符串，而是逐步升级为结构化限制记录：

```json
{
  "id": "lim_01J...",
  "text": "无法用当前证据验证指令是否最终到达设备服务。",
  "severity": "high",
  "category": "coverage",
  "confidence": 0.91,
  "evidence_refs": ["ev_...", "ev_..."],
  "producer_node_ids": ["investigate.code.1", "investigate.runtime.1"],
  "merge_key": "device-command-final-hop"
}
```

约束：

- `severity` 只允许 `critical`、`high`、`medium`、`low`；
- `category` 使用服务端维护的有限枚举；
- `confidence` 必须在 `[0,1]`；
- `evidence_refs` 和 `producer_node_ids` 必须引用当前 workflow 中已存在的对象；
- `merge_key` 不是用户可控的最终 ID，必须经过服务端校验；
- 缺失或非法字段不能导致整条 workflow 静默成功，必须进入明确的 fallback 或失败路径。

在迁移期，旧的字符串限制项由服务端转换为：

```text
severity = medium
category = unspecified
confidence = 0
merge_key = normalized text hash
```

#### 改动二：精确去重

服务端先进行确定性的文本规范化，再按规范化后的哈希去重：

- 去除首尾空白；
- 统一 Unicode 等价形式；
- 合并连续空白；
- 统一中英文标点的可等价形式；
- 不改变原始展示文本；
- 保留全部原始来源 ID。

精确去重不能只保留第一条记录，而要合并：

```text
同一 normalized_key
→ 选取质量最高的 canonical_text
→ union evidence_refs
→ union producer_node_ids
→ severity 取最高等级
→ confidence 取 max
→ 记录 merged_from_ids
```

#### 改动三：相似限制合并

相似合并不能只依据字符串包含关系，否则容易把不同根因错误合并。建议使用两级策略：

1. **优先按 verifier 提供并经过校验的 `merge_key` 合并**；
2. 没有可信 `merge_key` 时，只允许在以下条件同时满足时合并：
   - category 相同；
   - severity 相同或相邻；
   - evidence_refs 至少存在一个共同证据或共同目标；
   - 服务端相似度超过阈值；
   - 合并后的文本能够覆盖所有输入含义。

无法证明是同一根因时，宁可不合并，也不要为了减少数量而牺牲准确性。

每次合并必须保留：

```json
{
  "canonical_id": "lim_canonical_01",
  "merged_from_ids": ["lim_raw_01", "lim_raw_07", "lim_raw_12"],
  "merge_reason": "same verified final-hop coverage gap",
  "merge_method": "verified_merge_key"
}
```

#### 改动四：按严重程度和影响排序

排序由服务端完成，不交给 Synthesizer 自由决定。建议排序键如下：

```text
severity_rank desc
→ affects_core_conclusion desc
→ confidence asc
→ evidence_count desc
→ first_seen_seq asc
→ canonical_id asc
```

默认严重程度排序：

```text
critical > high > medium > low
```

排序语义：

- `critical`：限制会使核心结论不成立或存在严重安全/数据风险；
- `high`：限制会明显降低核心结论可信度，或阻断关键链路验证；
- `medium`：限制影响部分结论或某个非核心分支；
- `low`：限制属于补充信息、边界细节或低风险缺口。

#### 改动五：主要限制与详细限制分离

服务端根据配置生成两个视图：

```text
all_limitations      = 全量归一化限制项
primary_limitations  = 排序后前 N 条主要限制项
omitted_limitations  = all_limitations - primary_limitations
```

建议初始配置：

```yaml
investigation:
  limitations:
    primary_display_limit: 10
    normalized_max_items: 100
    detail_artifact_required: true
```

`primary_display_limit` 是展示上限，不是数据保存上限。N 应作为配置或服务端策略，而不是由模型决定。

最终答案只携带主要限制，例如：

```json
{
  "answer": "当前证据可以确认 Agent、Google 和 Alexa 的入口与主要控制链路，但无法完整验证最终设备服务的运行时落点。",
  "citations": [],
  "limitations": [
    "无法用当前证据验证指令是否最终到达设备服务。",
    "Google 与 Alexa 的下游控制实现存在证据缺口，不能确认两者完全复用同一条设备链路。"
  ],
  "limitations_detail": {
    "artifact_id": "art_550e8400-e29b-41d4-a716-446655440000",
    "total_count": 28,
    "displayed_count": 2,
    "omitted_count": 26,
    "normalization_version": "limitations-v1"
  }
}
```

最终答案中的 `limitations_detail` 属于新 Schema 版本字段。旧版本仍保持原契约不变。

#### 改动六：写入 detail artifact

在 Synthesizer 执行前，先写入一个不可变 detail artifact，建议 artifact kind 为：

```text
investigation.limitations.detail
```

artifact 内容至少包含：

```json
{
  "schema_id": "investigation.limitations.detail",
  "schema_version": 1,
  "workflow_run_id": "workflow_...",
  "normalization_version": "limitations-v1",
  "raw_count": 28,
  "deduplicated_count": 19,
  "merged_count": 12,
  "displayed_count": 10,
  "omitted_count": 2,
  "limitations": [
    {
      "id": "lim_canonical_01",
      "text": "...",
      "severity": "high",
      "category": "coverage",
      "confidence": 0.91,
      "evidence_refs": ["ev_..."],
      "producer_node_ids": ["investigate.code.1"],
      "merged_from_ids": ["lim_raw_01", "lim_raw_07"],
      "displayed": true,
      "rank": 1
    }
  ]
}
```

artifact ID 必须使用标准短 ID，例如 `art_<UUID>`，不能把完整哈希直接拼到数据库 `artifact_id` 字段中。

#### 改动七：Synthesizer 只负责表达，不负责整理

Synthesizer 的输入应明确区分：

- `primary_limitations`：只能用于最终答案展示；
- `limitations_detail_ref`：用于告知用户详细限制已保存在哪里；
- `all_limitations`：不直接放入模型上下文，避免不必要地撑大上下文；
- `normalization_summary`：提供总数、去重数、合并数和隐藏数。

Synthesizer 必须：

- 不新增限制项；
- 不删除 `primary_limitations`；
- 不改变排序；
- 不把 artifact 中的全部限制复制回答案；
- 只返回 Schema 允许的字段。

#### 改动八：详细结果持久化失败处理

详细 artifact 是完整性要求的一部分，不允许静默忽略。处理顺序：

1. 使用稳定幂等键 `workflow_run_id + normalization_version` 写入 artifact；
2. 持久化失败时按基础设施重试策略重试；
3. 重试仍失败时，workflow 不得标记为完整成功；
4. 记录明确错误码 `limitations_detail_persist_failed`；
5. 允许上层将任务置为可重试状态，而不是返回一份看似完整但无法追溯的答案。

### 5.4 数据结构或接口契约

建议新增以下内部 Schema：

```text
investigation.verified_bundle v2
  limitations: LimitationRecord[]
  maxItems: 100

investigation.limitations.detail v1
  全量归一化限制项和合并审计信息

investigation.answer v3
  answer: string
  citations: Citation[]
  limitations: string[]       // 仅主要限制，maxItems=10（或配置上限）
  limitations_detail: DetailRef
```

兼容关系：

```text
investigation.answer v1：历史任务，最多20条字符串限制，不修改
investigation.answer v2：当前过渡版本，最多100条字符串限制，不修改
investigation.answer v3：新任务，展示主要限制 + detail artifact 引用
```

状态转换：

```text
verified bundle created
→ limitation normalization pending
→ limitation detail persisted
→ primary limitation view ready
→ synthesizer answer generated
→ answer schema validated
→ workflow succeeded
```

失败状态：

```text
normalization invalid
→ invalid_output / normalization_failed

detail artifact persist failed after retry
→ limitation_detail_persist_failed / retryable

answer references missing artifact
→ invalid_output / missing_limitation_detail
```

### 5.5 兼容、迁移与回滚

- **向后兼容：** v1/v2 Schema、历史 workflow 和历史 run 不修改；读取历史答案时继续按旧字段解析；
- **数据迁移：** 不回填历史 artifact。新任务从启用版本开始生成 detail artifact；历史数据可按需离线重算，不影响原始 run；
- **灰度方式：** 先通过配置只启用服务端精确去重和排序，再启用相似合并，最后切换到 answer v3；
- **回滚条件：** 合并误判率、artifact 写入失败率、答案校验失败率或延迟超过验收阈值时回滚；
- **回滚步骤：** 新任务切回 answer v2，保留已经写入的 detail artifact；关闭相似合并开关但保留精确去重和 artifact 写入；必要时停止 v3 workflow 发布。

## 6. 修改伪代码

### 6.1 核心流程

```go
type NormalizedLimitations struct {
    All      []LimitationRecord
    Primary  []LimitationRecord
    Omitted  []LimitationRecord
    Summary  LimitationSummary
}

func NormalizeLimitations(
    ctx context.Context,
    raw []LimitationRecord,
    policy LimitationPolicy,
) (NormalizedLimitations, error) {
    normalized := normalizeTextAndFields(raw)
    exact := deduplicateExact(normalized)
    merged, audit := mergeVerifiedSimilar(ctx, exact, policy)
    ranked := rankLimitations(merged)

    primaryCount := min(policy.PrimaryDisplayLimit, len(ranked))
    return NormalizedLimitations{
        All:     ranked,
        Primary: ranked[:primaryCount],
        Omitted: ranked[primaryCount:],
        Summary: buildSummary(raw, exact, merged, primaryCount, audit),
    }, nil
}
```

### 6.2 关键边界处理

```go
func persistBeforeSynthesis(
    ctx context.Context,
    runID string,
    normalized NormalizedLimitations,
) (DetailRef, error) {
    artifact := BuildLimitationsDetailArtifact(runID, normalized)
    ref, err := artifactStore.PutIdempotent(
        ctx,
        runID,
        "investigation.limitations.detail",
        artifact,
    )
    if err != nil {
        return DetailRef{}, fmt.Errorf(
            "limitations_detail_persist_failed: %w", err,
        )
    }
    return ref, nil
}

func buildAnswerInput(
    normalized NormalizedLimitations,
    detailRef DetailRef,
) SynthesizerInput {
    return SynthesizerInput{
        PrimaryLimitations:  toDisplayStrings(normalized.Primary),
        LimitationsDetail:   detailRef,
        NormalizationSummary: normalized.Summary,
    }
}
```

关键不变量：

```text
len(primary) <= primary_display_limit
len(primary) + len(omitted) == len(all)
每个 all limitation 都能追溯到至少一个 raw limitation
每个 merged limitation 都保留 merged_from_ids
detail artifact 成功持久化后才能进入 Synthesizer
```

### 6.3 修改前后对比

修改前：

```go
verifiedBundle := verifierOutput
synthesizerInput := verifiedBundle
answer := runSynthesizer(synthesizerInput)
validate(answer)
```

修改后：

```go
verifiedBundle := verifierOutput
normalized, err := NormalizeLimitations(ctx, verifiedBundle.Limitations, policy)
if err != nil {
    return fail("limitations_normalization_failed", err)
}
detailRef, err := persistBeforeSynthesis(ctx, runID, normalized)
if err != nil {
    return retryableFail("limitations_detail_persist_failed", err)
}
synthesizerInput := buildAnswerInput(normalized, detailRef)
answer := runSynthesizer(synthesizerInput)
validateAnswerV3(answer)
```

### 6.4 配置或数据库变更

建议先使用配置，不新增数据库表：

```yaml
investigation:
  limitations:
    primary_display_limit: 10
    normalized_max_items: 100
    exact_dedup_enabled: true
    similar_merge_enabled: false
    detail_artifact_required: true
    normalization_version: limitations-v1
```

灰度稳定后，再考虑将策略版本写入 workflow definition 或 catalog，而不是依赖运行时可变配置。

artifact 使用现有 artifact 存储能力，新增的只是 schema/kind 约束，不应绕过现有持久化边界。

## 7. 预期的效果

### 7.1 功能效果

- 相同限制只出现一次；
- 相同根因的限制可以合并并保留来源；
- 主要限制按严重程度和影响排序；
- 最终答案不会因为限制项数量增加而无限膨胀；
- 全量限制仍可从 detail artifact 查询；
- 限制项整理逻辑从 Synthesizer 移到服务端，结果更稳定。

### 7.2 可观测性效果

每个 Investigation run 至少记录：

- 原始限制数；
- 精确去重后数量；
- 相似合并后数量；
- 主要展示数量；
- 隐藏数量；
- 各严重程度数量；
- 合并方法和合并失败原因；
- detail artifact ID 和 content hash；
- artifact 持久化耗时、重试次数和最终状态；
- Synthesizer 实际收到的主要限制数量；
- answer Schema 版本。

### 7.3 量化指标

初始验收目标：

| 指标 | 目标 |
| --- | --- |
| 相同限制重复展示率 | < 1% |
| 主要限制超过展示上限的成功答案 | 100% 通过服务端截取，不因数量失败 |
| 归一化限制的 artifact 可追溯率 | 100% |
| 合并后无法追溯来源的记录 | 0 |
| detail artifact 持久化失败静默成功率 | 0 |
| 原始触发案例 `invalid_output` | 0 |
| 正常路径新增 P95 延迟 | ≤ 100ms，不含外部 LLM 调用 |
| 相似合并误合并率 | < 2%，通过人工抽样评估 |

### 7.4 不应发生的变化

- 不修改历史 `investigation.answer v1/v2` Schema；
- 不改变 investigator 的权限、工具可见范围和执行预算；
- 不因为展示上限而删除 detail artifact 中的限制；
- 不允许模型新增、静默删除或重排服务端已经确定的主要限制；
- 不把所有限制重新注入 Synthesizer 上下文，导致上下文再次膨胀；
- 不引入只针对 `workflow_592090ef0cdc50a3e5f55d88` 或 trace `3d4ceaafb7b8` 的硬编码。

## 8. 测试与验收

### 8.1 单元测试

- 相同文本的限制项被精确去重；
- Unicode、空格和标点差异不会造成不必要的重复；
- 精确去重后所有来源 ID 都保留；
- 合法 `merge_key` 的相似限制被合并；
- 不同根因的相似文本不会被错误合并；
- `critical/high/medium/low` 排序稳定；
- 主要限制数量不会超过配置上限；
- `all = primary + omitted` 不变量成立；
- 空限制、非法严重程度、非法引用能够进入明确失败或 fallback；
- artifact ID 始终符合 `art_<UUID>` 格式；
- artifact 重复写入具有幂等性。

### 8.2 集成测试

- 从 verifier 输出到 normalizer、artifact、Synthesizer、answer 校验的完整链路；
- 28 条限制输入最终生成主要限制和 detail artifact，不发生 invalid_output；
- artifact 写入失败时触发重试或明确的 retryable 状态；
- Synthesizer 只收到主要限制，不收到全量列表；
- 新旧 Schema、旧 workflow 和新 workflow 可以同时运行；
- 同一个 workflow run 重复完成不会生成重复 artifact；
- 并发 workflow 写入不同 artifact 时互不覆盖。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 28 条限制，其中 5 条重复 | answer v3 成功，主要限制 ≤ 10，详情完整 | 集成测试 + DB/artifact 查询 |
| 全部重复 | 20 条相同限制 | 归一化后 1 条，来源数为 20 | 单元测试 |
| 相似但不同根因 | 2 条相近文本、不同证据 | 不误合并 | 单元测试 + 人工样例 |
| 高风险限制优先 | critical/high/medium/low 混合 | critical/high 排在前面 | 单元测试 |
| artifact 写入失败 | 存储连续失败 | 不标记为完整成功 | 故障注入测试 |
| 历史 v1 run | 已绑定 v1 的旧 workflow | 仍可恢复、读取和展示 | 回归测试 |
| 新任务 | 新 catalog/workflow | 使用 answer v3 和 detail artifact | 端到端测试 |

### 8.4 验收标准

1. 原触发案例不再因为限制项数量导致 `invalid_output`；
2. 用户答案中的主要限制不超过配置上限，且按服务端排序；
3. 所有未展示限制都能在 detail artifact 中找到；
4. 每个合并后的限制都能追溯到原始限制和证据来源；
5. detail artifact 持久化失败时没有静默成功；
6. 历史 v1/v2 workflow 不受影响；
7. 日志能够解释限制项被去重、合并、展示或隐藏的原因。

## 9. 风险与控制

| 风险 | 控制措施 |
| --- | --- |
| 相似合并误合并不同根因 | 默认先关闭相似合并；优先使用经过验证的 merge_key；保留 merged_from_ids；抽样评估 |
| 严重程度判断不准确 | 服务端使用有限枚举；缺失时降级为 medium；排序不改变证据内容 |
| artifact 增加存储量 | 设置单 run 大小上限、压缩和生命周期策略；只保存必要的 provenance |
| artifact 写入增加延迟 | 在单次 workflow finalization 中批量写入；使用幂等写入；监控 P95 |
| Synthesizer 仍然输出额外限制 | answer v3 服务端校验，禁止未在 primary 列表中的新增限制；失败进入 invalid_output |
| 新旧 Schema 并存复杂 | Schema 版本不可变；workflow definition 固定绑定版本；增加版本和运行路径日志 |
| 配置变更导致排序变化 | 将 normalization_version 和策略摘要写入 artifact，保证可审计和可复现 |
| 详细结果泄露内部信息 | artifact 按 workflow/run 权限读取；用户答案只返回受控 DetailRef，不直接暴露内部证据 |

## 10. 实施计划

### 阶段 1：最小安全改动

- 增加限制项服务端精确去重；
- 增加稳定排序和 `primary_display_limit`；
- 保留全量归一化列表在内存中并增加日志；
- 退出条件：原触发案例不再因重复或数量导致答案失败，且单元测试通过。

### 阶段 2：详细 artifact 与结构化契约

- 定义 `investigation.limitations.detail v1`；
- 增加 detail artifact 幂等持久化；
- 增加结构化 limitation record 和来源关系；
- 发布新的 answer Schema 版本；
- 退出条件：主要限制和全量详情都可查询，历史 v1/v2 任务保持可读。

### 阶段 3：灰度相似合并

- 默认关闭相似合并，只启用精确去重；
- 使用 verified merge key 开启第一批合并；
- 采集误合并率、合并覆盖率和延迟；
- 退出条件：误合并率低于 2%，且 artifact 追溯率为 100%。

### 阶段 4：全面启用与清理

- 根据评估结果开启安全范围内的相似合并；
- 将 normalization policy 固化到 workflow/catalog 版本；
- 更新 Synthesizer prompt，删除其自行去重、排序的职责；
- 更新 dashboard、运行详情和运维文档；
- 退出条件：连续一个发布周期没有因限制项处理导致的 invalid_output 或静默数据丢失。

## 11. 待决策事项

1. `primary_display_limit` 默认值取 10 还是沿用现有 20；
2. `investigation.answer v3` 是否直接返回 `limitations_detail.artifact_id`，还是只返回受控的 detail reference；
3. 相似合并第一阶段是否只允许 verified `merge_key`，暂不使用语义相似度；
4. detail artifact 的保留期限、读取权限和是否支持用户侧查看；
5. artifact 持久化失败时，是将 workflow 标记为 retryable，还是允许返回 answer v2 作为降级结果；
6. 是否需要在 Schema 中暴露 `hidden_count`，还是只通过 detail reference 查询。

## 12. 决策摘要

本提案建议：

1. 不再让 Synthesizer 直接处理未经整理的全量限制项；
2. 新增服务端 limitation normalizer，负责精确去重、可信相似合并、严重程度排序和展示截取；
3. 最终答案只展示前 N 条主要限制，默认建议 N=10；
4. 全量归一化结果和合并审计信息写入 `investigation.limitations.detail` artifact；
5. 详细 artifact 写入成功是完整成功的必要条件，失败时不得静默成功；
6. 保留 v1/v2 历史契约，通过新的 Schema/workflow 版本逐步启用；
7. 第一阶段只启用精确去重和排序，相似合并先灰度，避免为了减少条数而误合并不同根因。

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
