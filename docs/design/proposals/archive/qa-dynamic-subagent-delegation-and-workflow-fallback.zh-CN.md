# QA 动态子 Agent 委派与 Workflow 降级提案

> 状态：Implemented，已归档
> 创建日期：2026-08-15
> 实施日期：2026-08-16
> 范围：QA Agent 工具循环、调查任务委派、Agent Runtime、权限与预算、EvidenceUnit、RunStore、Workflow 编排及运行事件
> 关联提案：`qa-agent-context-budget-and-cancellation.zh-CN.md`、`qa-query-intent-and-facet-model-simplification.zh-CN.md`、`qa-evidence-convergence-and-retrieval-governance.zh-CN.md`、`qa-retrieval-latency-and-progress-governance.zh-CN.md`
> 关联正式文档：`../agent-platform/01-architecture-and-execution.zh-CN.md`、`../agent-platform/02-evidence-and-tooling.zh-CN.md`、`../agent-platform/04-context-session-and-tool-results.zh-CN.md`、`../agent-platform/08-observability-and-evaluation.zh-CN.md`、`../agent-platform/18-task-driven-multi-agent-architecture.zh-CN.md`

## 0. 文档生命周期

本文记录 QA 多 Agent 从“入口预规划 Task Graph + 独立 Workflow DAG”扩展为
Single、Dynamic Delegation 与 Durable Workflow 三条路径的实施结果。协议、
Runtime/Store、权限预算、证据报告、显式 escalation、恢复、事件和 shadow 观测均已
落地；按风险触发的 semantic verifier、parent 最终答案采用事实及其 Step/terminal/event
投影也已完成。默认 feature flag 仍关闭，生产收益门槛尚待真实流量验证。

本文的核心判断是：

1. 当前 Task Contract、Capability、Agent Definition、PermissionPolicy、EvidenceUnit、Run Runtime 和持久化 Workflow 都解决了真实问题，不应因执行入口调整而重建或整体删除。
2. 当前主要问题不是“缺少更多 Planner 或更多 Workflow 节点”，而是调查拆解在主 Agent 看到实际检索结果之前就被固定，并将同一份调查合同广播给所有 investigator。
3. 普通交互式问答应默认由主 Agent 保持自己的工具循环，在获得证据后按需委派窄调查任务；子 Agent 只返回有界、带引用的报告。
4. Durable Workflow 仍然适合长任务、强依赖、高风险、人工审批和必须恢复重放的场景，但不应继续作为普通多 Agent 调查的唯一执行形态。
5. `join`、`verify`、`risk gate` 和 `synthesize` 不应一次性全部删除；其中确定性校验应保留为普通代码，高成本语义验证应根据冲突和风险按需触发。
6. 当前通用 tool call 执行是顺序语义。第一版应在一个批量委派工具内部实现受限并发，不应直接将所有工具调用改成并行。

### 0.1 实施结果与 rollout 门槛

评审要求的 Runtime 前置和闭环观测已经完成：

1. **统一 Capability 协议**：增加 `Capability.Role`，不引入 alias/FamilyID；Capability、Agent Definition、Tool ID 和 WorkflowBinding 各自只有一个职责，上层工具由应用注册。
2. **锁定 deterministic validator 边界**：只有显式冲突和 ClaimPolicy 覆盖的结构化冲突可确定性发现；自由文本跨报告合并必须按规则触发 semantic verifier。
3. **补齐 Runtime/Store 模型**：run-level limits、parent-owned budget reservation/settlement、delegation relation、状态投影和 report artifact 必须先落地。
4. **定义 Workflow escalation 协议**：通过独立、immutable WorkflowBinding 和幂等 `WorkflowEscalator` 启动；Capability/Tool 不携带也不推导 Workflow node ID。
5. **实现按需 semantic verifier**：高风险、关键冲突、自由文本跨报告合并、关键引用/
   comparator 缺失和报告截断由 validator 稳定原因码自动触发；verifier 复用 child
   admission、预算、settlement、artifact、事件、重放和恢复协议。
6. **记录最终采用事实**：delegate tool 为每个 delegation 注册可采用 report ID 集；
   parent 最终答案必须显式声明采用子集，Runtime 校验并从可见内容剥离，再把 typed
   adoption facts 写入 answer Step、terminal/public result 和独立事件。

代码完成不等于生产切流完成。当前仍保持：

- `DelegationEnabled=false`、`DelegationShadowEnabled=false`、
  `DelegationWorkflowEscalationEnabled=false`；
- 固定 Workflow verify/risk gate/synthesize 尚未因本提案删除；
- `has_conflicts=false` 只表示未发现显式或受 `ClaimPolicy` 覆盖的结构化冲突，
  不表示多份自由文本报告语义一致；
- 默认路由和旧 stage 淘汰必须等待第 14.4 节生产分桶基线与 shadow 数据。

## 1. 背景

### 1.1 实施前形态与保留的 Workflow 路径

实施前 QA 多 Agent 调查，以及当前保留的 Durable Workflow，大致经过以下链路：

```text
Evidence Planner
  -> ExecutionSuggestion
  -> single / multi execution route
  -> TaskGraphProposal / deterministic proposal
  -> ProposalCompiler
  -> Workflow DAG
       -> investigator.code / runtime / docs / web / memory / observe
       -> evidence.join
       -> evidence.verify
       -> evidence.risk
       -> synthesize
```

当前实现已经具备以下能力：

- `internal/agent/qa/investigation.go` 定义 canonical `TaskContract`；
- `app/investigation.go` 将 Task Contract 编码后启动调查 Workflow；
- `internal/agent/workflow/proposal.go` 和相关 compiler 将 Task Graph proposal 编译为受约束 Workflow；
- Agent Definition、Capability、ToolScope 和 PermissionPolicy 约束 investigator 的工具、权限和预算；
- `run.Execute(ctx, RunRequest)` 可运行独立 Agent 并保存运行事实；
- EvidenceUnit、citation、conflict 和 completeness 可承载结构化调查结果；
- Workflow 负责持久化、事件投影、取消、终态和可重放执行。

这些基础件已继续复用。普通交互式调查现在由 parent 工具循环按需调用
`delegate_investigation`；上述 Workflow 链路保留给 durable/high-risk/strong-dependency
路径和显式 escalation，没有另建一套 Agent Runtime。

### 1.2 实施前问题的本质

实施前的多 Agent 不是主 Agent 在 reason→act→observe 循环中按实际发现创建子任务，
而是入口编排链先决定是否进入多 Agent，再提前生成一张 Task Graph。主 Agent 当时无法
在自己的检索结果基础上决定：

- 是否还需要子 Agent；
- 需要调查哪一个新暴露的方向；
- 哪些初始方向已经没有必要；
- 两个方向能否并行；
- 是否只需追加一个窄 verifier。

与此同时，Workflow 的 `TaskContract` 调用方式将其作为所有 investigator 共享的
canonical 输入。即使 `marshalInvestigationContract` 已经对 seed material 做预算和
截断，合同的 question、objective、entities、evidence goals、constraints 和 seed
metadata 仍会被多个 Workflow investigator 接收；Dynamic Delegation 已改为窄
objective/facets/evidence refs。

因此，延迟和上下文膨胀来自三个层次：

```text
控制层
  提前路由并固定 Task Graph

输入层
  将完整 Task Contract 广播给多个 investigator

收敛层
  固定执行 join -> verify -> risk gate -> synthesize
```

## 2. 目标与非目标

### 2.1 目标

1. 普通 QA 请求默认保留在主 Agent 工具循环中。
2. 主 Agent 在获得检索证据后，按需创建一个或多个窄调查任务。
3. 子 Agent 只接收当前任务目标、受控 evidence refs 和服务端派生约束。
4. 子 Agent 完整轨迹继续持久化，主 Agent 只接收有界调查报告。
5. 服务器端统一控制 Agent Definition、ToolScope、PermissionPolicy、预算、并发和递归深度。
6. 普通路径移除固定的多次 LLM 收敛尾部，保留确定性验证和按需语义验证。
7. Durable Workflow 保留为长任务、高风险、强依赖和恢复重放路径。
8. 新旧路径可独立观测、灰度、对比和回退。

### 2.2 非目标

1. 不允许模型调用任意 Agent ID 或任意 Tool ID。
2. 不允许子 Agent 自由创建下一层子 Agent。
3. 不将所有 tool call 一次性改成并行执行。
4. 不将写工具开放给普通调查委派。
5. 不删除 Workflow Runtime、RunStore、Task Contract 或 EvidenceUnit。
6. 不在第一版支持 child-to-child 通信或共享可变上下文。
7. 不用新状态机复制已有 Run 或 Workflow 生命周期。
8. 不为了单个评估问题、服务名或关键词增加专用委派规则。
9. 不在没有基线数据的情况下直接删除现有 verifier、risk gate 或 synthesizer。

## 3. 总体方案

### 3.1 普通交互路径

```text
User Question
  -> Query Analysis / Evidence Plan
  -> Parent Agent tool loop
       -> ordinary retrieval tools
       -> observe returned evidence
       -> decide whether a narrow investigation is needed
       -> delegate_investigation(batch)
            -> bounded child runs in parallel
            -> bounded child reports
            -> deterministic report validation
       -> resolve conflicts or optionally request verification
       -> answer in the parent context
```

### 3.2 Durable Workflow 路径

```text
User Question / Parent Agent escalation
  -> Workflow eligibility and risk policy
  -> TaskGraphProposal
  -> ProposalCompiler
  -> durable Workflow DAG
       -> investigators
       -> evidence convergence
       -> verification / risk gate
       -> synthesis
```

### 3.3 控制权划分

```text
Parent Agent
  动态拆解、是否委派、最终综合、显式冲突表达

Child Agent
  在窄目标和受限工具内完成独立调查

Server Policy
  Capability 映射、权限交集、预算、并发、深度、输出上限

Run Runtime / RunStore
  child 执行、轨迹、使用量、终态和报告持久化

Workflow Runtime
  长任务、强依赖、审批、恢复、重放和高风险编排
```

## 4. 问题、场景与修改方式

### 4.1 P0：Task Graph 在看到证据前被提前确定

#### 问题

入口层根据初始问题和 Evidence Planner 输出选择 single 或 multi，再生成 Task Graph。调查方向在首次检索之前就被锁定。

#### 场景

用户询问某接口返回 500 的原因。入口可能提前安排代码、运行时、文档和配置调查，但第一次 `search_code` 已经找到明确空指针路径，其余 investigator 仍会继续运行。

反向场景是入口只安排代码调查，但代码证据随后指向配置中心或下游运行状态。主 Agent 无法基于新发现自然追加一个窄 runtime 或 observe 调查，只能依赖旧 Workflow 的兜底规划。

#### 修改方式

1. 普通请求默认进入主 Agent 工具循环。
2. `ExecutionSuggestion` 从强制执行路由降级为 eligibility 和风险提示。
3. 主 Agent 先使用普通检索工具，看到结果后再调用 `delegate_investigation`。
4. `TaskGraphProposal` 和 `ProposalCompiler` 只在 Durable Workflow 路径使用。
5. 明确 Workflow 触发条件，禁止新旧路径按相同条件重复启动调查。

#### 预期结果

```text
当前：Question -> fixed Task Graph -> execute all planned tasks
目标：Question -> retrieve -> observe -> create only necessary tasks
```

### 4.2 P0：完整 Task Contract 被广播给所有 investigator

#### 问题

当前 `TaskContract` 同时携带 question、objective、entities、investigation goals、evidence goals、constraints 和 context。不同 investigator 接收相同合同，仅通过 Agent Definition 和 Capability 改变执行行为。

#### 场景

代码 investigator 只需要确认调用链和错误处理，却同时接收 runtime、docs、web、memory 等证据目标及 seed metadata。多个 investigator 重复理解相同背景，增加输入 token，并使无关证据进入不必要的权限和上下文边界。

#### 修改方式

新增窄 `DelegationTask`：

```json
{
  "capability": "knowledge.code.inspect",
  "objective": "确认 payment-service 状态更新是否存在可达的空指针路径",
  "focus_facets": ["core_flow", "data_and_state"],
  "evidence_refs": ["ev_code_123", "ev_api_456"]
}
```

服务端负责：

1. 校验 capability；
2. 解析 evidence refs；
3. 过滤 parent 无权访问或与 capability 不兼容的证据；
4. 在一个 ingress 边界完成标准化、长度校验和去重；
5. 从 capability policy 派生 Definition、工具、权限和预算；
6. 只生成当前 child 需要的运行输入，不再复制完整 Task Contract。

#### 预期结果

```text
当前：TaskContract x N investigators
目标：N 个独立、窄化、不可扩权的 DelegationTask
```

### 4.3 P0：固定 join、verify、risk gate 和 synthesize 形成串行尾部

#### 问题

所有 Workflow 调查都需要经过固定收敛阶段，即使报告一致、引用完整且风险很低，也要支付多次串行 LLM 调用。

#### 场景

两个代码 investigator 已经返回结构化、无冲突且引用完整的结果，但系统仍需要依次 join、verify、risk gate 和 synthesize 后才能返回答案。低风险问题的尾部延迟可能超过实际调查时间。

#### 修改方式

将收敛拆成两类：

**确定性校验，普通路径始终执行：**

- report schema 校验；
- citation 是否存在；
- evidence refs 是否属于 parent 可访问集合；
- claim 是否至少有一个 citation；
- EvidenceUnit 去重；
- facet 覆盖统计；
- report 字节和 token 上限；
- completeness 汇总；
- 显式 conflicts 和 uncertainties 汇总；
- child 权限、预算和终态检查。

**语义验证，仅按风险触发：**

- 两份报告对同一事实给出互斥结论；
- runtime 与 code 证据不一致；
- 关键 claim 缺少直接证据；
- 高风险或写操作场景；
- 用户明确要求独立验证；
- 系统风险策略要求 verifier。

普通回答由 parent 综合。独立 verifier 和 synthesizer 继续保留给 Workflow 或显式升级路径。

#### 预期结果

```text
低风险：children -> deterministic validation -> parent answer
高风险：children -> validation -> optional verifier/risk gate -> answer
```

### 4.4 P0：当前多个 tool calls 按顺序执行，动态委派不会自动并行

#### 问题

`internal/agent/execution/loop_turn.go` 的 `executeToolTurn` 按 tool call slice 顺序调用 executor。若模型连续返回多个 delegate tool calls，child 仍会串行启动和等待。

#### 场景

主 Agent 同时需要 code、runtime 和 docs 调查。如果发出三个独立 delegate 调用，实际执行可能是 code 完成后才开始 runtime，再开始 docs，延迟可能比现有并行 Workflow 更高。

直接并行所有工具也会改变现有语义：写工具顺序、共享预算、消息顺序、取消、错误处理和观察结果排序都需要重新定义。

#### 修改方式

第一版只增加一个支持 batch 的工具：

```json
{
  "tasks": [
    {"capability": "knowledge.code.inspect", "objective": "确认代码调用链"},
    {"capability": "knowledge.runtime.observe", "objective": "确认最近错误发生位置"}
  ]
}
```

`delegate_investigation` executor 内部：

1. 先校验全部任务并预留预算；
2. 使用固定上限的 worker pool 执行 child；
3. 默认最多两个 child 并发；
4. 每个 child 独立 timeout；
5. parent context 取消时取消所有活动 child；
6. 单个 child 失败不隐式取消其他 child；
7. 结果按请求顺序返回，避免非确定完成顺序污染 prompt；
8. 不修改其他工具的顺序执行合同。

### 4.5 P0：Fork 数量、递归和成本可能失控

#### 问题

主 Agent 可以多轮调用 delegate。若没有服务器端 admission control，模型可能重复创建相似任务；如果 child 也能继续 delegate，还会形成递归扩张。

#### 场景

parent 第一轮创建三个 child，报告标记 partial 后又创建三个；每个 child 再创建自己的 child，最终请求成本、延迟和上下文不可预测。

#### 修改方式

增加运行级 `DelegationPolicy`，由服务器配置，模型不能扩大：

```go
type DelegationPolicy struct {
    MaxDepth             int
    MaxChildren          int
    MaxConcurrent        int
    MaxChildTurns        int
    MaxChildToolCalls    int
    MaxChildInputTokens  int
    MaxChildOutputTokens int
    MaxReportTokens      int
    MaxTotalTokens       int
    ChildTimeout         time.Duration
}
```

第一版建议：

| 限制 | 默认建议 |
|---|---:|
| 最大深度 | 1 |
| 每个 parent 最大 child 数量 | 3 |
| 最大并发 child 数量 | 2 |
| child 是否允许继续 delegate | 否 |
| 单 child report | 800～1200 tokens |
| 单 child timeout | 不超过 parent 剩余执行时间 |
| 总 token/cost | 计入 parent run 总预算 |

预算不足必须返回结构化 `rejected` 或 `partial`，不能静默超额，也不能伪装成“未找到证据”。

### 4.6 P0：通用 delegate 可能扩大权限边界

#### 问题

如果模型可以指定 Agent ID、Definition、Tool IDs、PermissionPolicy 或 Provider，委派就会成为扩权入口。

#### 场景

父 Agent 只有代码读取权限，却请求 runtime child 查询未经授权的客户日志；或者模型直接选择一个带写工具的 Definition。

#### 修改方式

模型只允许选择服务端派生 allowlist 中的 capability。该 allowlist 不是硬编码清单，而是在 `delegate_investigation` 工具注册时从合并 Capability Registry 派生：只暴露 `SideEffects == none` 且 `Enabled` 的 capability ID。上层应用通过 `AgentCatalogContribution` 动态注册的只读能力（例如 codeloom 的 `knowledge.runtime.observe` 及其唯一工具 `observe_logs`）会自动进入枚举，Nasuta 无需认识上游工具的名字。

```text
base catalog + AgentCatalogContribution
  -> 合并 Capability Registry
  -> filter(SideEffects == none && Enabled)
  -> capability allowlist（模型可见枚举）
```

服务端按引用解析每个 capability，不维护手工映射：

```text
capability ID
  -> Capability.Agent.DefinitionRef -> Definition
  -> ToolIDs / RestrictVisible -> 子 Agent 可见工具
  -> PermissionScope -> 权限上界
  -> Definition.Budget -> 默认预算
  -> OutputSchema -> Report schema
```

未知、禁用或 `SideEffects == write` 的 capability 必须显式拒绝，不得静默替换或降级。

child 最终权限必须满足：

```text
EffectivePermission
  = ParentPermission
  ∩ CapabilityPermission
  ∩ RequestDataScope
```

模型不得指定：

- 任意 Agent ID 或 Definition version；
- 任意 Tool IDs；
- PermissionPolicy；
- Provider 或 credential；
- 更大的预算；
- 未授权数据源。

第一版仅开放读取能力，写操作继续走现有授权和审批路径。

### 4.7 P1：子 Agent 报告仍可能撑大 parent 上下文

#### 问题

即使 child 输入已经收窄，如果每个 child 返回完整 reason→act→observe 轨迹、大段代码和日志，parent 上下文仍会膨胀。

#### 场景

三个 child 各返回数千 token，并重复引用原始代码和日志。parent 下一轮需要重新发送所有报告，动态委派反而把原 Workflow 的上下文问题搬进主循环。

#### 修改方式

定义强约束 `DelegationReport`：

```json
{
  "run_id": "run_child_123",
  "report_id": "report_123",
  "capability": "knowledge.code.inspect",
  "status": "completed",
  "completeness": "partial",
  "summary": "状态更新前未检查 order 是否为空。",
  "findings": [
    {
      "id": "claim_1",
      "statement": "UpdateStatus 调用前存在可达空值路径",
      "structured_claim": {
        "schema": "knowledge.code.assertion.v1",
        "subject": "symbol:UpdateStatus",
        "predicate": "reachable_null_dereference",
        "value": true,
        "scope": {"revision": "commit_abc"}
      },
      "confidence": "high",
      "citations": ["ev_123", "ev_456"],
      "facets": ["core_flow", "data_and_state"]
    }
  ],
  "conflicts": [],
  "uncertainties": ["无法确认生产请求是否执行该分支"],
  "usage": {
    "tool_calls": 3,
    "input_tokens": 4200,
    "output_tokens": 800
  }
}
```

报告规则：

1. 不返回隐藏推理或完整执行轨迹；
2. 不重复大段 authoritative evidence；
3. claim 必须关联 citation；
4. findings、conflicts 和 uncertainties 数量受限；
5. 截断必须显式标记 completeness，不能静默丢失；
6. 完整轨迹和原始结果保存在 RunStore/Artifact；
7. parent 只接收 report 投影和 report ID。

### 4.8 P1：并行报告可能给出相互冲突的结论

#### 问题

多个 child 具有隔离上下文，可能对同一事实给出不同结论。简单拼接文本无法稳定发现和处理冲突。

#### 场景

code child 认为失败来自数据库空值，runtime child 认为实际故障来自下游 HTTP timeout。parent 若忽略时间、环境和证据范围，可能把两者错误合并成确定结论。

#### 修改方式

增加确定性 `DelegationValidation`，但明确它的能力来自结构化合同，而不是对自由文本做语义理解：

```json
{
  "report_ids": ["report_1", "report_2"],
  "citation_coverage": 0.91,
  "structured_claim_coverage": 0.67,
  "conflicts": [
    {
      "kind": "structured_value_mismatch",
      "claim_key": "incident.failure_cause@prod/2026-08-15T10:00Z",
      "claim_ids": ["report_1/claim_1", "report_2/claim_2"]
    }
  ],
  "unverified_semantic_overlap": true,
  "requires_verification": true,
  "verification_reasons": ["critical_structured_conflict", "unstructured_cross_report_merge"]
}
```

规则：

1. child 必须显式返回 `conflicts` 和 `uncertainties`，但 child 的自报冲突只是一类输入，不是完整冲突发现能力；
2. finding 可以带 `structured_claim`；其 schema 和比较策略由 Capability 的 OutputSchema 或应用注册的 `ClaimPolicy` 声明；
3. aggregator 只对 schema、引用、稳定 claim key 和已注册 comparator 做确定性处理，不对自由文本 statement 猜测同义、反义或因果关系；
4. 发现关键结构化冲突后向 parent 返回独立 conflict observation；
5. 多份报告之间若需要合并自由文本结论，或 claim 没有可比较的结构化表示，必须标记 `unverified_semantic_overlap`，并按第 8.3 节触发语义 verifier；
6. parent 不得把未消解冲突表达为已确认事实；无法消解时保留不确定性和下一步验证建议。

因此，普通路径可以移除固定的 LLM join，但不能据此声称 deterministic validator 能发现任意语义冲突。它替代的是机械聚合和合同校验；跨报告的自由文本语义判断仍由按需 verifier 负责。

### 4.9 P1：动态 child 必须保留持久化、取消和重放能力

#### 问题

如果 delegate 只在内存 goroutine 中执行，连接断开、parent 取消或进程退出时可能产生孤儿 child、报告丢失和使用量无法归属。

#### 场景

两个 child 已经启动，parent SSE 连接中断。其中一个 child 已完成但报告尚未返回，另一个仍在调用外部工具。若没有 parent-child run 关系，就无法可靠取消、恢复、审计或复用已完成报告。

#### 修改方式

所有 child 继续通过现有 Agent Runtime 执行，并记录：

```text
ParentRunID
ChildRunID
DelegationID
CapabilityID/Version/ContentHash
Depth
RunLimits
ReportArtifactID
Usage
```

生命周期规则：

1. child 启动前持久化 admission、exact Capability snapshot identity 和预算 reservation；
2. child run 通过 `ParentRunID`、`DelegationID` 和稳定 task index 关联 parent；
3. child 完成后先持久化 bounded report 并完成预算 settlement，再向 parent 返回 observation；
4. parent 取消向活动 child 传播 cancellation；
5. parent 已取消但 child 已完成时保留报告用于审计；
6. 重试携带稳定 DelegationID，避免重复启动同一任务；
7. 不新建与现有 Run/Workflow 重复的持久化状态机；
8. 需要跨进程恢复的长任务仍升级到 Durable Workflow。

### 4.10 P1：Single、Delegate 和 Workflow 三条路径边界不明确

#### 问题

新增 delegate 后会同时存在纯 single Agent、parent + delegate 和 Workflow。如果没有适用规则，相同请求可能随机进入不同路径，且失败降级不可预测。

#### 场景

一个普通代码问题既可能被入口 `ExecutionSuggestion` 路由到 Workflow，也可能进入 single Agent 后再调用 delegate。两套调查重复执行，成本增加，指标也无法比较。

#### 修改方式

建立明确 eligibility：

**保持纯 Single Agent：**

- 单次或少量普通工具调用即可回答；
- 不需要独立上下文；
- 没有明显可并行任务；
- 委派成本预计高于直接检索。

**使用 Parent + Dynamic Delegation：**

- 需要一个到三个独立调查方向；
- 子任务可以使用隔离上下文；
- parent 能在当前 run 内完成综合；
- 不涉及写操作、审批或跨进程长时间运行。

**使用 Durable Workflow：**

- 子任务具有强依赖顺序；
- 需要暂停、恢复、重放或人工审批；
- 预计执行时间超过普通 QA run；
- 子任务数量超过动态委派上限；
- 风险策略要求独立 verifier/risk gate；
- incident analyze/fix 等已有场景合同要求 Workflow。

入口与 parent 不得同时隐式启动两种多 Agent 路径。升级到 Workflow 必须产生可观测原因码。

### 4.11 P1：现有事件和 UI 以 Workflow 节点为中心

#### 问题

现有事件可以投影 investigator、`evidence.join`、risk gate 和 synthesize 节点，但动态 child 不是 Workflow node。若不增加事件，用户只能看到 parent 长时间等待。

#### 场景

parent 启动 code 和 runtime 两个 child，其中 runtime timeout。UI 需要展示正在调查什么、哪个任务成功、哪个任务超时、使用了多少预算以及最终是否采用该报告。

#### 修改方式

新增统一 delegation 事件：

```text
delegation.created
delegation.started
delegation.completed
delegation.failed
delegation.cancelled
delegation.rejected
```

事件至少包含：

```text
parent_run_id
child_run_id
delegation_id
capability
objective_summary
status
duration_ms
usage
report_id
error_code
```

要求：

- objective summary 必须截断和脱敏；
- 不记录完整敏感 prompt；
- budget rejection、timeout、permission denial 和 execution failure 使用不同错误码；
- Workflow 和 delegate 使用量进入同一运行计量；
- UI 将其展示为 child investigation，不伪装成 Workflow DAG node。

### 4.12 P1：直接替换旧链路无法定位质量回归

#### 问题

若同时删除 Task Graph 路由、full contract、join、verify、risk gate 和 synthesize，出现质量变化时无法确认根因，也无法快速回退。

#### 场景

新路径答案 citation coverage 下降。原因可能是 child 输入过窄、report 截断、冲突验证未触发或 parent 综合提示不足。如果一次性替换全部阶段，评估无法隔离变量。

#### 修改方式

采用 feature flag、shadow 和分阶段迁移：

1. 先建立旧 single/multi 路径基线；
2. 只在 single-agent 路径注册 delegate，不改变旧 multi 路由；
3. 开启 shadow，对比旧 Workflow 和新 delegate，但不影响用户答案；
4. 先迁移低风险 code/docs 调查；
5. runtime/observe 和高风险请求继续走 Workflow；
6. 数据证明某个 LLM stage 无额外价值后，再单独删除该 stage；
7. 为新旧路径保留清晰 route reason 和 fallback reason。

## 5. Delegate 工具协议

### 5.1 工具选择

第一版采用一个 typed、服务端受控、支持 batch 的工具：

```text
delegate_investigation
```

不采用完全开放的 generic delegate，也不在第一版向模型暴露大量能力专用工具。原因是：

- 一个工具可以集中执行 schema 校验、预算预留、并发和结果聚合；
- capability 仍然显式，便于服务端映射不同 Definition 和工具集合；
- 避免扩大模型可见工具数量；
- 避免在 `executeToolTurn` 中引入通用并发语义；
- 后续若某种能力出现显著不同的输入、生命周期或风险合同，再考虑拆成独立工具。

### 5.2 请求合同

```json
{
  "tasks": [
    {
      "capability": "knowledge.code.inspect",
      "objective": "调查订单状态更新失败的代码路径",
      "focus_facets": ["core_flow", "external_dependency"],
      "evidence_refs": ["ev_123", "ev_456"]
    }
  ]
}
```

字段所有权：

| 字段 | 所有权 | 规则 |
|---|---|---|
| `capability` | 模型选择，服务端校验 | 仅允许 allowlist |
| `objective` | 模型提供 | 必须是一个窄、可完成目标 |
| `focus_facets` | 模型提供，Catalog 校验 | 只保留 canonical facet |
| `evidence_refs` | 模型提供，服务端过滤 | 必须来自 parent 可访问 evidence ledger |
| Definition | 服务端 | 模型不可指定 |
| Tool IDs | 服务端 | 模型不可指定 |
| PermissionPolicy | 服务端 | 模型不可指定 |
| Budget | 服务端 | 模型不可扩大 |
| Provider/Credential | 服务端 | 模型不可指定 |

### 5.3 返回合同

```json
{
  "delegation_id": "del_123",
  "results": [
    {
      "run_id": "run_child_123",
      "report_id": "report_123",
      "capability": "knowledge.code.inspect",
      "status": "completed",
      "completeness": "complete",
      "summary": "发现一个可达的空指针路径。",
      "findings": [
        {
          "id": "claim_1",
          "statement": "UpdateStatus 在 order 为空时会解引用 order.ID",
          "structured_claim": {
            "schema": "knowledge.code.assertion.v1",
            "subject": "symbol:UpdateStatus",
            "predicate": "reachable_null_dereference",
            "value": true,
            "scope": {"revision": "commit_abc"}
          },
          "confidence": "high",
          "citations": ["ev_789"],
          "facets": ["core_flow", "data_and_state"]
        }
      ],
      "conflicts": [],
      "uncertainties": [],
      "usage": {
        "tool_calls": 2,
        "input_tokens": 3800,
        "output_tokens": 620
      }
    }
  ],
  "validation": {
    "citation_coverage": 1.0,
    "structured_claim_coverage": 1.0,
    "has_conflicts": false,
    "unverified_semantic_overlap": false,
    "requires_verification": false,
    "verification_reasons": []
  }
}
```

### 5.4 状态与部分失败

child 状态至少包括：

```text
completed
partial
failed
timeout
cancelled
rejected
interrupted
```

batch 中单个 child 失败不应覆盖其他成功结果。例如：

```text
knowledge.code.inspect: completed
knowledge.runtime.observe: timeout
overall: partial
```

parent 根据已有证据决定继续回答、补充调查、显式说明证据不足或升级 Workflow。

## 6. 服务端能力映射

### 6.1 Capability Policy

Capability、Agent Definition 和 Tool ID 是三个不同层次，不能通过 alias 或额外的 FamilyID 重复表达：

```text
Capability ID
  对外的 canonical 能力身份，模型请求、权限审计和 Workflow binding 使用

Capability Role
  服务端策略分类，例如 investigator、verifier、synthesizer

Agent Definition
  具体执行合同，包含 prompt、model、budget、权限上限和工具上限

Capability ToolIDs
  该 Capability 在 Definition 工具上限内进一步暴露的工具集合

Tool ID
  实际工具身份，可以由上层应用注册，Nasuta 不需要理解其业务语义
```

建议在 `Capability` 中增加服务端声明的 `Role` 字段：

```go
type Capability struct {
    ID      string           `json:"id"`
    Version int64            `json:"version"`
    Role    CapabilityRole   `json:"role"`
    Agent   DefinitionRef    `json:"agent"`
    ToolIDs []string         `json:"tool_ids,omitempty"`
    // other existing fields...
}
```

`Role` 只用于服务端过滤和策略判断，不由模型提交，也不作为模型请求 capability 的替代名称。第一版至少支持：

```text
investigator
verifier
synthesizer
```

Capability Registry 的关系如下：

```text
knowledge.code.inspect
  -> Role: investigator
  -> Agent: investigator.code
  -> ToolIDs: application/catalog supplied

knowledge.service.trace
  -> Role: investigator
  -> Agent: investigator.runtime
  -> ToolIDs: application/catalog supplied

knowledge.docs.verify
  -> Role: investigator
  -> Agent: investigator.docs
  -> ToolIDs: application/catalog supplied

knowledge.runtime.observe
  -> Role: investigator
  -> Agent: investigator.observe
  -> ToolIDs: [observe_logs]
```

其中：

- `knowledge.runtime.observe` 是 Codeloom 注册的 canonical Capability ID；
- `investigator.observe` 是 Codeloom 注册的 Agent Definition ID；
- `observe_logs` 是 Codeloom 注册的 Tool ID；
- Nasuta 不需要认识 `observe_logs` 的业务含义，只需校验工具存在、属于当前 Tool Snapshot，并且没有超过 Definition 和 Capability 的工具范围。

模型只能请求 canonical Capability ID，例如：

```json
{
  "tasks": [
    {
      "capability": "knowledge.runtime.observe",
      "objective": "确认最近一次请求失败时的运行时错误位置",
      "focus_facets": ["runtime_and_operations"],
      "evidence_refs": ["ev_runtime_123"]
    }
  ]
}
```

不允许模型提交：

```text
code
observe
investigator.observe
observe_logs
```

这些值分别属于能力别名、Definition 身份或 Tool 身份，不能替代 Capability ID。

`Capability.ToolIDs` 与 `Definition.Tools.VisibleToolIDs` 不是同一层的重复字段。`ToolIDs` 是 canonical 精确 allowlist；空集合表示该 Capability 不暴露工具，不表示继承 Definition 的全部工具，也不支持 `*` 或名称前缀匹配：

```text
EffectiveTools
  = Definition tool ceiling
  ∩ Capability tool scope
  ∩ Run ToolScope
```

例如 Definition 可以允许：

```text
[observe_logs, observe_traces, observe_metrics]
```

而 `knowledge.runtime.observe` 只暴露：

```text
[observe_logs]
```

这样可以复用同一个 Definition，同时由不同 Capability 对工具、权限、输入输出 Schema 和预算做进一步收窄。

`ClaimPolicy` 同样不是 Family 或 Capability alias。它是某个 OutputSchema 的确定性比较插件，最小合同为：

```go
type ClaimPolicy struct {
    Schema       SchemaRef `json:"schema"`
    ComparatorID string    `json:"comparator_id"`
    KeyFields    []string  `json:"key_fields"`
    ScopeFields  []string  `json:"scope_fields"`
}
```

`KeyFields` 和 `ScopeFields` 在 policy 注册 ingress 固化，report validator 只消费 canonical structured claim。没有 ClaimPolicy 的 OutputSchema 仍可返回自由文本 finding，但只能参与 citation/schema 校验，不能进入确定性语义冲突检测。

Capability 的通用约束：

1. Capability ID、Role、Tool ID 和 Permission Scope 在注册 ingress 完成 canonical 校验；
2. Capability 必须绑定 immutable、pinned 的 Agent Definition；
3. Capability ToolIDs 必须是 Definition 可见工具的子集；
4. `delegate_investigation` 只暴露 `Role == investigator`、`SideEffects == none` 且 `Enabled` 的 Capability；
5. Capability ToolIDs 可以引用上层应用注册的工具，Nasuta 不维护上层工具名称清单；
6. QueryKind、EvidencePlan 或 focus facet 只能收窄执行，不得扩大 Capability 的工具或权限范围；
7. Capability 失败必须返回明确错误，不得静默替换 provider 或其他 Capability。

Workflow node ID 不属于 Capability。Workflow 升级使用独立的应用组合层 binding：

```text
WorkflowBinding
  Capability ID/version
    -> application-owned Workflow kind/builder
```

没有 WorkflowBinding 的上层 Capability（例如仅注册 `observe_logs` 的 Codeloom observe 能力）可以执行 Dynamic Delegation，但不能升级到 Workflow。升级时必须返回 `workflow_unavailable`，不得猜测 Workflow node，也不得把它替换为其他能力。

Nasuta 的基础 Capability 和上层 `AgentCatalogContribution` 都进入同一个 immutable registry snapshot。Nasuta 只负责通用结构、版本、权限、工具和副作用校验；Capability 的业务语义、上层工具语义和可选 Workflow binding 由注册方负责。

### 6.2 Child 输入构造

child 输入应包含：

- 一个窄 objective；
- parent question 的必要摘要，而非完整会话；
- 与 objective 直接相关的 evidence refs 和有界投影；
- 服务端派生的 allowed tools、constraints 和 report schema；
- parent run、delegation 和 trace metadata。

child 输入不应包含：

- parent 完整 message history；
- 全部 Task Contract；
- 无关 EvidenceGoal；
- 其他 child 的完整报告；
- parent 的隐藏推理；
- 未授权数据源内容。

## 7. 执行、预算与并发

### 7.1 执行入口

child 继续复用现有：

```text
run.Execute(ctx, RunRequest)
```

新增的是一个负责构造受限 `RunRequest`、执行 admission control、并发调度和报告投影的 delegate executor。它不应复制 Agent loop、模型调用、工具执行、RunStore 或 cancellation 机制。

### 7.2 Batch 并发

第一版并发仅存在于 delegate executor 内：

```text
validate all tasks
  -> reserve aggregate budget
  -> start at most MaxConcurrent workers
  -> execute child runs
  -> collect reports
  -> deterministic order
  -> validate
  -> return one tool result
```

要求：

- 不使用无界 goroutine；
- 不共享可变 child message history；
- 共享预算使用单一 owner，避免并发超扣；
- parent 取消后不再启动排队中的 child；
- 已取消 child 的结果不得作为 completed 报告返回；
- 完成顺序不决定返回顺序；
- 外部 provider 并发仍受现有或新增 provider 限流约束。

### 7.3 预算归属与 Runtime 合同

```text
Parent Run Budget Account
  -> ordinary parent model/tool usage
  -> delegation reservations
       -> child 1 effective limits -> actual usage -> settlement
       -> child 2 effective limits -> actual usage -> settlement
  -> protected parent answer reserve
```

实施前 `Definition.Budget` 只有 definition 级 `Timeout`、`MaxSteps`、
`ContextTokens` 和 continuation 上限，`RunRequest` 不能表达一次 child grant。
当前实现已增加 run 级**有效执行上限**，并将最终值固化到 `RunSnapshot`，而不是只在
delegate executor 外层计数：

```go
type RunLimits struct {
    Deadline       time.Time `json:"deadline"`
    MaxSteps       int       `json:"max_steps"`
    MaxToolCalls   int64     `json:"max_tool_calls"`
    MaxTotalTokens int64     `json:"max_total_tokens"`
    MaxCostMicros  int64     `json:"max_cost_micros"`
}

type RunRequest struct {
    // existing fields...
    Limits RunLimits `json:"limits"`
}
```

`RunPolicy.MaxToolCalls` 作为计算 `RunLimits.MaxToolCalls` 的一个输入，child 路径不再
另建第二个工具调用计数；Definition/Capability policy 的 token、cost ceiling 在注册
ingress 一次规范化。`RunLimits` 只表示本次 Run 的最终有效值。

`RunLimits` 由服务端 preparation 计算，模型不能提交。Runtime 必须校验：

```text
Effective child limits
  = intersection of every configured ceiling
    (pinned Definition budget, RunPolicy, Capability policy,
     parent remaining delegation grant, provider/runtime hard limits)
```

`RunSnapshot` 固化最终 `RunLimits`，实际执行循环按它限制 deadline、step、tool call、token 和 cost。若某一维当前 provider 无法在调用前硬停止，也必须在调用后立即结算并阻止后续 step，不能把“仅记录 usage”描述为预算执行。

创建 child 前同时检查：

- parent 剩余 wall-clock time和 child deadline；
- parent 剩余 token/cost；
- parent 最终回答 reserve；
- child 最大 step 和工具调用次数；
- report 投影预算；
- batch aggregate reservation 是否超过 parent account。

### 7.4 预留、结算与并发一致性

parent run 是预算唯一 owner。batch admission 先按 task index 确定 grant，再一次性预留 aggregate budget；worker 不直接修改共享计数。每个 child 完成后用 Runtime 的 authoritative `Usage` 结算：

```text
reserve(parent, delegation, child, grant)
  -> execute child with exact RunLimits
  -> settle(reservation, actual usage)
  -> release unused amount
```

约束：

1. reservation 必须幂等，key 为 `(parent_run_id, delegation_id, task_index)`；
2. 同一个 key 重试只能返回原 reservation，不能重复扣减；
3. actual usage 大于 grant 是 Runtime invariant violation，child 以 `budget_accounting_violation` 失败并禁止继续调用；
4. rejected、未启动和取消的 task 释放未消费 reservation；
5. parent answer reserve 不进入可委派余额；
6. parent 终态前必须完成所有已启动 child 的 settlement，或将未决项记为 interrupted 并由启动恢复过程释放；
7. 普通 dynamic delegation 不跨进程恢复执行；进程中断后 parent 和活动 child 沿用现有 recovery 规则结束，只有 Durable Workflow 可以恢复调度。

## 8. 证据、验证与综合

### 8.1 Authoritative evidence 与模型投影

沿用现有提案中的职责分离：

```text
Authoritative child artifacts
  完整工具结果、EvidenceUnit、trace、审计和回放

DelegationReport projection
  有界 findings、citations、conflicts、uncertainties 和 usage
```

parent 不应通过 report 文本重新发明一份证据事实。citation 必须解析到 authoritative evidence。

### 8.2 确定性验证与冲突算法

validator 输入 child reports、Capability OutputSchema/ClaimPolicy 和 parent evidence ledger，按以下固定顺序执行：

1. **合同校验**：验证 report schema、枚举、数量/字节上限和 completeness；无效 report 不进入 claim 聚合。
2. **引用校验**：逐个解析 citation，确认 evidence 存在、属于 parent/child 可访问 ledger、内容哈希有效且没有越权；计算 citation coverage。
3. **终态校验**：将 child Run 终态、错误码、EvidenceStatus 和 report completeness 映射为第 9.2 节的 delegation status，拒绝“Run 失败但 report completed”一类不变量冲突。
4. **精确去重**：优先以 `(structured claim key, canonical value, citation set)` 去重；无 structured claim 时仅以 statement hash 和 citation set 去重，不做近义文本聚类。
5. **显式冲突汇总**：校验 child `conflicts[].claim_ids` 都存在且有引用，然后按稳定 key 合并；不能解析的显式冲突使 report partial。
6. **结构化冲突检测**：仅对具有同一 `schema + subject + predicate + canonical scope`，且 ClaimPolicy 声明可比较的 claims 分组。validator 调用注册 comparator；例如 `exclusive_scalar` 在 canonical value 不同时产生冲突，`boolean_assertion` 在 `true/false` 对立时产生冲突。没有注册 comparator 时不得自行比较。
7. **覆盖与风险判定**：计算 facet coverage、structured claim coverage、truncation、missing refs 和关键 claim 的验证状态。
8. **生成决定**：输出 `requires_verification` 和稳定 reason codes。

第一版稳定 reason codes 至少包括：

```text
critical_explicit_conflict
critical_structured_conflict
unstructured_cross_report_merge
missing_critical_citation
unknown_claim_comparator
report_truncated
high_risk_policy
```

`requires_verification` 的确定性规则为：

```text
true, if
  policy requires semantic verification
  OR any unresolved critical explicit/structured conflict exists
  OR the final conclusion must merge claims from multiple reports and
     at least one participating critical claim lacks a comparable structured form
  OR a critical citation/claim comparator is missing
otherwise false
```

能力边界必须写死：

- validator **能**发现 child 显式声明的冲突，以及 ClaimPolicy 覆盖范围内的结构化值冲突；
- validator **不能**发现任意自由文本之间的同义、反义、时序、因果或范围矛盾；
- 两段 statement 即使看起来讨论同一事实，只要没有稳定 claim key 和 comparator，validator 也不能宣称“无冲突”；它只能输出 `unverified_semantic_overlap`；
- `has_conflicts=false` 表示“确定性算法未发现已覆盖类型的冲突”，不表示“所有语义一致”；
- citation coverage 只证明引用存在、可访问且完整性校验通过，不证明引用内容在语义上蕴含 claim；structured claim 也仍是 child 输出，不会因为结构化就自动成为 authoritative fact。

因此普通路径移除固定 LLM join 的前提是：机械汇总由本 validator 完成；任何依赖自由文本跨报告合并的关键结论仍走第 8.3 节 verifier。低风险、单报告或无需跨报告合并的请求可以不调用 verifier。

### 8.3 按需语义 verifier

Dynamic Delegation 的 verifier 已实现为服务端拥有的
`evidence.semantic.verify` Capability。它不是普通委派的固定尾部；validator 只有在
`requires_verification=true` 时才自动启动一个 tool-free、read-only verifier child。
触发原因来自第 8.2 节稳定 reason codes，包括高风险 policy、关键显式/结构化冲突、
自由文本跨报告合并、关键 citation/comparator 缺失和 report truncation。

verifier 输入收窄为：

- 待验证 claim；
- 冲突 claims；
- 相关 evidence refs；
- 明确判定问题；
- 有界输出 schema。

不得再次传入完整 report、整份 Task Contract、完整 child trace、tool transcript 或
parent 会话。verifier 只获得其声明 evidence refs 对应的有界 Context View，且
`ToolScope.RestrictVisible=true`、可见工具为空、`MaxToolCalls=0`。

verifier 继续复用 delegation Runtime/Store 不变量：

1. 使用稳定 child run/verification/artifact identity；
2. 进入 parent-owned admission、answer reserve 保护和 usage settlement；
3. 预算不足返回 `rejected/delegation_budget_insufficient`，不启动 Runtime；
4. durable verification artifact 先于结果返回写入；
5. 重试读取已 settlement 的 artifact，不重复调用 verifier Runtime；
6. 启动恢复遇到 admitted 但未 settlement 的 verifier 时，结算为
   `failed/interrupted`，保留可审计 artifact；
7. 发布 `delegation.verification_started/done/failed/rejected` 生命周期事件，事件携带
   verification ID、trigger reasons、status、error code、duration 和 usage。

### 8.4 Parent 综合责任

parent 最终回答必须：

1. 只将有证据支持的 claim 表达为事实；
2. 保留 child 报告中的关键 uncertainty；
3. 明确说明无法消解的冲突；
4. 使用 citation 指向 authoritative evidence；
5. 不把 child 的 `confidence=high` 当成独立事实证明；
6. 在 report 被截断或 child partial 时调整答案完整性声明。

delegate tool 同时向最终 `AnswerContract` 注册：

```text
delegation_id -> allowed report_ids
```

parent 候选答案最后一行必须包含隐藏的显式采用元数据：

```text
[NASUTA_DELEGATION_ADOPTION] {"delegations":[{"delegation_id":"del_...","adopted_report_ids":["report_..."]}]}
```

每个已登记 delegation 必须恰好出现一次，`adopted_report_ids` 必须是该 delegation
允许 report 集的无重复子集；verifier ID 不是 report ID，不能声明为 adopted。空数组
投影为 `not_adopted`，非空投影为 `adopted`。marker 缺失、重复、未知字段、delegation
缺项/重复或 report 越权会触发有界答案修复，不能作为成功答案交付。

校验成功后 marker 必须从 token stream、answer Step content、public result 和 Session
消息中剥离；采用事实作为结构化字段独立保存。parent 失败、取消、没有可交付答案或
output schema validation 失败时，所有已登记 delegation 分别投影为
`unknown/parent_run_failed`、`unknown/parent_cancelled`、
`unknown/final_answer_unavailable` 或 `unknown/invalid_output`，不得沿用候选
`adopted` 结论。

## 9. 持久化、取消与可观测性

### 9.1 落到现有 Run Runtime 的持久化模型

`agent_runs` 持久化 `run_kind`、`parent_run_id`、Definition snapshot identity、
Workflow correlation、状态和 usage；child 写 `run_kind=agent`，`parent_run_id`
指向 QA parent。当前实现已补齐 DelegationID、Capability version/hash、depth、
run-level limits、预算 reservation 和 report artifact，未把这些事实只放在内存事件中。

parent answer Step 通过 `agent_steps.delegation_adoptions_json` 持久化最终采用事实，
Terminal/Outcome 与 public `RunResult` 使用同一 typed projection。升级脚本为
`docs/sql/migration_delegation_adoptions_20260816.sql`；可见 answer content 不保存
隐藏 marker。

采用“现有 Run 是生命周期事实 + delegation relation 是关联/预算事实”的模型：

```text
agent_runs
  existing run identity/status/usage
  + capability_id
  + capability_version
  + capability_content_hash
  + delegation_id
  + delegation_depth
  + run_limits_json
  + capability_registry_revision (audit only)

agent_delegation_tasks
  parent_run_id
  delegation_id
  task_index
  child_run_id
  capability_id/version/content_hash
  objective_hash
  admitted / rejection_code
  reservation_json
  settled_usage_json
  report_artifact_id
  created_at / settled_at

agent_run_artifacts (若实现时已有通用 runtime artifact store，则复用)
  artifact_id
  run_id
  kind = delegation_report
  schema_id/version
  content_hash
  content
  created_at
```

`agent_delegation_tasks` 不拥有第二套 child 生命周期状态：

- `rejected` 来自 admission fact，且没有已启动 child；
- `running/done/failed/aborted/paused` 继续以 `agent_runs.status` 为唯一事实；
- `timeout/cancelled/partial/completed` 是 API 投影，由 Run 状态、稳定 error code、EvidenceStatus 和 report completeness 推导；
- task 已 admitted 但进程中断前没有 child Run 时，依据 parent terminal fact 投影为 `interrupted` 并释放 reservation，不恢复执行。

必须建立唯一约束：

```text
(parent_run_id, delegation_id, task_index)
child_run_id
report_artifact_id for kind=delegation_report
```

写入顺序固定为：

```text
persist admission + reservation
  -> create/start child Run
  -> persist child terminal usage
  -> persist and hash bounded report artifact
  -> settle reservation and link report artifact
  -> emit terminal projection to parent
```

parent 只在 report artifact 和 settlement 均成功后收到 `completed/partial` observation。若 child 已成功但 report 持久化失败，则返回 `report_persistence_failed`，不能把内存报告伪装为 durable completed。

### 9.2 状态映射

对外 delegation 状态不直接扩展现有 `run.Status`：

| Delegation status | Existing Run fact / admission fact |
|---|---|
| `rejected` | admission rejected，无 child Run |
| `completed` | child `done`，report complete，settlement complete |
| `partial` | child `done`，但 EvidenceStatus/report completeness 为 partial |
| `failed` | child `failed`，错误码不是 timeout |
| `timeout` | child `failed` 或 `aborted`，error code=`child_timeout` |
| `cancelled` | child `aborted`，error code=`parent_cancelled` 或 `client_cancelled` |
| `interrupted` | parent 被 recovery 终结，accepted task 无可完成的 child/report |

这避免 `agent_runs.status` 与 delegation status 双写后产生冲突。API、事件和 UI 都必须调用同一 projection 函数。

### 9.3 取消语义

- parent 显式取消：取消活动 child，停止启动排队 child；
- child timeout：仅标记该 child timeout，其他 child 可继续；
- parent 超时：传播取消，并保留已落库报告；
- client 断开是否取消 run：继续遵守现有 QA run 合同，不由 delegate 私自决定；
- Workflow 升级：由现有 Workflow cancellation 和 terminal 语义接管。

### 9.4 事件

新增 delegation 事件并统一进入现有 execution event/RunHub 投影。至少记录：

- 路由选择和 route reason；
- delegation created/started/terminal；
- capability；
- child run ID；
- duration；
- token、tool-call 和 cost usage；
- report size/completeness；
- validation result；
- verifier trigger reason；
- verifier started/done/failed/rejected、verification ID、duration 和 usage；
- `delegation.adoption_evaluated`，包含 delegation ID、adopted report IDs、status 和
  unknown reason；
- Workflow escalation reason。

## 10. 路由与降级规则

### 10.1 默认决策顺序

```text
Can ordinary tools answer within current run?
  yes -> single Agent
  no

Are there <= MaxChildren independent, read-only, bounded investigations?
  yes -> dynamic delegation
  no

Does the task require durability, dependency graph, approval or high-risk gates?
  yes -> Durable Workflow
  no -> return bounded partial result or explicit unsupported status
```

### 10.2 Workflow 升级条件

使用稳定原因码：

```text
strong_task_dependencies
durable_execution_required
human_approval_required
high_risk_verification_required
child_limit_exceeded
parent_time_budget_insufficient
scenario_requires_workflow
```

升级必须显式，不允许 delegate 失败后静默切到 Workflow，也不允许配置的 capability 失败后切换到其他 provider。普通的 provider error、未知 capability、permission denial 或某个 child timeout 默认返回当前路径的明确失败；只有 route policy 已声明对应 reason 可升级，且存在 binding 时，才提交 escalation。

### 10.3 Workflow escalation 服务端协议

Workflow escalation 是应用服务端 handoff，不是 Capability Registry 推导出的 Workflow node，也不是模型直接提交 `workflow_id`。模型或 QA parent 最多提出 capability、objective 和 reason；binding、Workflow Definition、ScenarioPermission 和预算由服务端解析。

公开合同建议放在 Nasuta application surface：

```go
type WorkflowEscalationReason string
type WorkflowEscalationStatus string

const (
    EscalationAccepted       WorkflowEscalationStatus = "accepted"
    EscalationAlreadyStarted WorkflowEscalationStatus = "already_started"
    EscalationRejected       WorkflowEscalationStatus = "rejected"
)

type WorkflowEscalationRequest struct {
    RequestID      string                   `json:"request_id"`
    ParentRunID    string                   `json:"parent_run_id"`
    DelegationID   string                   `json:"delegation_id,omitempty"`
    Capability     agent.CapabilityRef      `json:"capability"`
    Reason         WorkflowEscalationReason `json:"reason"`
    Objective      string                   `json:"objective"`
    FocusFacets    []string                 `json:"focus_facets,omitempty"`
    EvidenceRefs   []string                 `json:"evidence_refs,omitempty"`
    ReportRefs     []string                 `json:"report_refs,omitempty"`
}

type WorkflowEscalationReceipt struct {
    RequestID       string `json:"request_id"`
    WorkflowRunID   string `json:"workflow_run_id,omitempty"`
    BindingID       string `json:"binding_id,omitempty"`
    BindingVersion  int64  `json:"binding_version,omitempty"`
    Status          WorkflowEscalationStatus `json:"status"`
    ErrorCode       string `json:"error_code,omitempty"`
}

type WorkflowEscalator interface {
    Escalate(context.Context, WorkflowEscalationRequest) (WorkflowEscalationReceipt, error)
}
```

该结构是 **server-to-server contract**，不是模型工具参数原样反序列化的结构。`RequestID`、`ParentRunID`、`DelegationID` 和 exact CapabilityRef 均由 QA runtime 生成或解析；模型侧如果开放 escalation tool，只能提交 capability ID、objective、focus facets 和建议 reason，且 reason 仍需 route policy 复核。

`Actor`、parent effective PermissionPolicy、remaining policy budget 和 session/trace correlation 必须由服务端根据 `ParentRunID` 加载，不接受模型回传。`EvidenceRefs` 和 `ReportRefs` 只携带引用；服务端解析为 bounded handoff 和 `SeedEvidence`，不能把完整 child 轨迹重新塞进 Workflow input。

应用组合层注册 immutable binding：

```text
WorkflowBinding
  Binding ID/version
  Capability ID/version
  allowed escalation reasons
  Workflow Definition resolver/builder
  Scenario
  ScenarioPermission ceiling
  input Schema/version
```

它与 Capability Registry 分离。Codeloom 可以为 `knowledge.runtime.observe` 注册自己的 binding；如果只注册 Capability/Tool 而没有 binding，delegation 仍可用，但 escalation 返回 `workflow_unavailable`。

模型提交的 capability 只有 ID；delegate/escalation ingress 必须立即解析并固化 exact `Capability ID + Version + ContentHash`。Registry revision 只用于审计和刷新派生工具 schema，不能替代 immutable capability identity，也不能在重放时重新解析“latest”。

服务端执行顺序：

```text
validate request + load parent
  -> verify exact Capability version/hash resolved at delegation/escalation ingress
  -> resolve exact WorkflowBinding version
  -> intersect parent/capability/scenario permissions
  -> validate evidence/report ownership and hashes
  -> build bounded Workflow input + SeedEvidence
  -> derive stable WorkflowRunID from RequestID
  -> call workflow.Service.Start(StartRequest)
  -> persist escalation receipt/event
```

协议约束：

1. `RequestID` 是幂等键；唯一范围为 `(parent_run_id, request_id)`。重复提交返回同一 `WorkflowRunID` 和 `already_started`，不得启动第二个 Workflow；
2. accepted 后，当前 parent 不再为同一目标启动新的 child；可以返回 Workflow handle，也可以按现有 QA 合同等待/投影，但两种行为必须由 scenario 固定，不能临时切换；
3. `ParentRunID` 传给现有 `workflow.StartRequest.ParentRunID`，Workflow 内部 node IDs 由 binding 的 builder/Definition 决定；
4. escalation 不能扩大权限。若 Workflow 需要写权限或人工审批，必须由现有 authorized start/approval 边界处理；
5. 已完成 child 的 report/evidence 可以作为 seed，但必须保留原 provenance，Workflow node 不得冒充为新发现；
6. Workflow start 成功后使用现有 durable cancellation、recovery、event 和 terminal 语义；start 失败则 parent 仍拥有当前 QA run，并收到明确错误；
7. 至少定义以下稳定错误码：

```text
workflow_unavailable
workflow_binding_not_found
workflow_reason_not_allowed
workflow_permission_denied
workflow_invalid_handoff
workflow_budget_insufficient
workflow_start_conflict
workflow_start_failed
```

### 10.4 Workflow 保留范围

明确保留：

- incident analyze/fix/notify；
- 需要人工审批的写操作；
- 具有强顺序依赖的 Task Graph；
- 需要暂停、恢复和跨进程重放的任务；
- 风险策略要求独立 verifier/risk gate 的任务；
- 超出普通 QA run 时限和 child 数量上限的调查。

## 11. 现有组件处理清单

### 11.1 保留并复用

| 组件 | 处理方式 |
|---|---|
| `run.Execute(ctx, RunRequest)` | 作为 child 执行入口 |
| Agent Definition | 定义不同调查能力和 prompt |
| Capability Registry | 解析能力对应 Agent 与工具 |
| ToolScope | 限制 child 可见工具 |
| PermissionPolicy | 与 parent 权限取交集 |
| EvidenceUnit / citations | 继续作为证据事实 |
| RunStore / artifacts | 保存 child Run；补充 delegation relation、run limits 和 report artifact |
| Workflow Runtime | 保留 durable/high-risk 路径 |
| Task Contract | 保留 Workflow canonical input |
| Workflow verifier/risk gate | 保留高风险和显式升级场景 |

### 11.2 降级为非默认能力

| 组件 | 新定位 |
|---|---|
| `ExecutionSuggestion` 的强制 multi 路由 | eligibility、风险和建议输入 |
| `TaskGraphProposal` | Durable Workflow 使用 |
| `ProposalCompiler` | 只编译持久化 Workflow |
| 全量 `TaskContract` 广播 | Workflow 内保留，delegate 改为窄任务 |
| LLM join | 普通路径改为确定性 aggregation |
| LLM verify | 按冲突和风险触发 |
| LLM risk gate | 高风险场景保留 |
| 独立 synthesizer | Workflow 保留，普通路径由 parent 综合 |

### 11.3 新增

- `delegate_investigation` tool contract；
- `DelegationTask` 和 `DelegationReport`；
- capability delegation policy；
- admission controller；
- bounded batch executor；
- deterministic report validator 和 ClaimPolicy/comparator；
- parent-owned budget account、run-level limits 和 reservation settlement；
- delegation relation 与 report artifact；
- WorkflowBinding Registry 和 WorkflowEscalator 服务端合同；
- delegation events 和 route reason；
- feature flag、shadow 和评估指标。

## 12. 实际代码边界

实现沿用既有 owner，并将 Dynamic Delegation 的专有编排集中在
`internal/agent/delegation`，没有复制 Agent loop、RunStore 或 Workflow Runtime。

### 12.1 Agent API / Domain Contract

已落地职责：

- 定义 `DelegationTask`、`DelegationReport` 和 validation result；
- 定义 capability allowlist 或 registry-facing ref；
- 定义 parent-child metadata、RunLimits 和 budget grant/usage 合同；
- 定义 WorkflowEscalationRequest/Receipt 和稳定错误码；
- 保持 schema 可版本化和可持久化。

约束：只有需要跨 public Nasuta surface 的合同才进入 outward package；纯 QA runtime 细节留在 `internal/agent`。

### 12.2 QA Composition

已落地：

- 在 QA answerer 可见工具中按配置注册 delegate；
- 将 `ExecutionSuggestion` 从强制 multi route 逐步降级；
- 增加 single/delegate/workflow route reason；
- 为 Workflow escalation 提供显式 `WorkflowEscalator` 入口和幂等 receipt。

### 12.3 Execution Runtime

已落地：

- 不修改 `executeToolTurn` 的通用顺序语义；
- delegate executor 内实现批量受限并发；
- 复用 `run.Execute` 创建 child；
- 由 parent budget account 原子预留并向 child 传入 exact RunLimits；
- 结算 authoritative usage，保护 parent answer reserve；
- 持久化 delegation relation 和 bounded report 后再返回一个 tool result。

### 12.4 Investigation Workflow

已落地与保留边界：

- 保留 `app/investigation.go` 和 ProposalCompiler；
- 将其从普通 multi 默认路径改为 durable escalation；
- 后续根据评估结果决定是否收缩固定节点；
- 不在第一阶段删除现有 Workflow 测试和事件投影。

### 12.5 Evidence 与 Tool Delivery

已落地：

- report citations 解析到 parent/child authoritative evidence；
- report 投影遵守现有 context/tool delivery 预算；
- 重复 evidence 使用 canonical identity 去重；
- 超限使用 explicit partial/omitted/artifact reference。

## 13. 迁移计划

### 阶段 0：建立基线（指标合同完成，生产数据待积累）

实现已具备记录旧 single、Workflow、delegate 和 shadow 路径所需的事件与 usage 字段。
以下生产基线仍需在真实流量中积累：

- 总延迟及各 stage 延迟；
- LLM 调用次数；
- 输入/输出 token；
- investigator 数量；
- citation coverage；
- 无引用关键 claim 比例；
- conflict 发现率；
- timeout/cancel/failure 比例；
- 单次运行成本；
- 最终答案质量。

### 阶段 1：协议、Runtime/Store 基础和单 Child（已完成）

1. 增加 `Capability.Role`，定义请求、报告、ClaimPolicy 和 validation schema；
2. 增加 run-level `RunLimits`、parent budget reservation/settlement 和持久化 migration；
3. 增加 delegation relation、report artifact 和统一状态 projection；
4. 定义 WorkflowBinding/WorkflowEscalator 合同，但暂不改变默认路由；
5. 注册 feature-gated delegate 工具，只允许一次调用启动一个 child；
6. child 通过 `run.Execute` 执行；
7. 完整轨迹落库，parent 只接收 bounded report；
8. 完成权限交集、预算、幂等和取消测试。

边界、持久化、启动恢复、权限、预算、幂等和取消回归均已完成。

### 阶段 2：Batch Fan-out（已完成）

1. 支持一个工具调用提交多个窄任务；
2. 增加有界 worker pool；
3. 增加聚合预算预留；
4. 增加部分失败合同；
5. 增加 deterministic validation；
6. 增加 delegation 运行事件。

### 阶段 3：Single 路径灰度（实现完成，默认开关关闭）

1. 只在现有 single-agent 路径开放 delegate；
2. 不改变旧 multi-agent route；
3. 先开放低风险 code/docs；
4. runtime/observe 使用更严格 allowlist；
5. **已完成**：通过严格 final-answer metadata 记录每个 delegation 的 adopted/
   not_adopted/unknown，剥离隐藏 marker，并持久化到 answer Step、terminal/public
   result 和 `delegation.adoption_evaluated` 事件。

### 阶段 4：Shadow 对比（观测实现完成，生产对照待积累）

同一请求对比：

```text
旧路径：TaskGraph Workflow
新路径：Parent + Dynamic Delegation
```

Shadow 结果不影响用户答案。对比延迟、成本、引用、冲突和质量，按 QueryKind、capability 和风险等级分桶，不能只比较全局平均值。

`delegation.shadow_evaluated` 已提供 query kind、duration、usage、reference count 和
conflict count 等结构化字段；当前 shadow flag 默认关闭，尚无足够生产数据证明收益。

### 阶段 5：调整默认路由（可配置代码完成，生产切换待门槛）

1. 低风险、短、目标明确的 code/docs 调查默认使用 delegate；
2. 高风险、长任务、runtime 强依赖继续走 Workflow；
3. `ExecutionSuggestion` 从强制路由改为建议；
4. Workflow 保留显式 escalation，不做静默 fallback；
5. 每次只淘汰一个被数据证明无价值的固定 LLM stage。

路由、Capability allowlist、显式 escalation 和 feature flag 已实现。低风险请求默认
切换到 delegate，以及任何旧 verifier/risk/synthesize stage 的淘汰，仍必须等待
第 14.4 节生产门槛。

### 阶段 6：正式文档收敛（已完成）

截至 2026-08-16：

1. 更新正式 Agent Architecture、Evidence、Context 和 Observability 文档；
2. 标记本提案为 Implemented；
3. 记录实际 policy 参数、路由阈值和遗留项；
4. 旧路由或 stage 的删除尚未执行，因为生产数据尚未证明可淘汰。

## 14. 测试与验收

### 14.1 单元测试

- capability 到 Definition/ToolScope/PermissionPolicy 映射；
- parent/child 权限交集；
- 未授权 evidence ref 拒绝；
- duplicate task 去重或拒绝；
- child 数量、深度、token、tool-call 和 report 上限；
- batch 结果保持请求顺序；
- completed/partial/failed/timeout/cancelled/rejected/interrupted 投影；
- citation coverage 和 missing ref；
- explicit conflict 与 ClaimPolicy comparator 的 deterministic conflict 汇总；
- 无 comparator/free-text overlap 时不误报“无语义冲突”；
- reservation 幂等、settlement 和 answer reserve 保护；
- verifier 高风险/多报告触发、输入窄化、预算拒绝、artifact settlement、重放、恢复和
  生命周期事件；
- final-answer adoption metadata 的缺项、重复、越权、剥离及失败/取消/invalid-output
  unknown 投影；
- Workflow escalation reason、binding 缺失和 receipt 幂等。

### 14.2 并发和取消测试

- MaxConcurrent 不被突破；
- budget reservation 不超扣；
- parent 取消停止排队和活动 child；
- 一个 child timeout 不取消无关 child；
- child 完成与 parent 取消竞争时终态唯一；
- report 先持久化后投影；
- 无 goroutine 或活动 provider call 泄漏。

### 14.3 集成测试

- parent 普通检索后动态创建 code child；
- code/docs 两个 child 并行并返回有界报告；
- runtime capability 缺失时明确 rejected，不静默替换；
- 两个报告需要自由文本合并或存在关键冲突时触发 verifier；
- 高风险请求在 Dynamic Delegation 路径自动触发 verifier；
- adopted/not_adopted/unknown 能从 answer Step、terminal/public result 和事件读取，
  marker 不进入 SSE、Session 或可见答案；
- 高风险请求通过 binding 升级 Workflow，重复 RequestID 不重复启动；
- 上层 Capability 无 binding 时返回 `workflow_unavailable`；
- Workflow 仍支持现有持久化、事件、终态和回放；
- SSE/UI 能展示 child 生命周期。

### 14.4 性能与质量门槛

以下是 rollout 目标，不是截至 2026-08-16 已经达成的结果：

- 适合动态委派的请求 P95 总延迟至少下降 20%；
- 平均输入 token 至少下降 25%；
- 平均 LLM 调用次数至少减少 1 次；
- 无需委派的问题不得新增额外 LLM 调用；
- citation coverage 不低于旧路径；
- 无引用关键 claim 比例不增加；
- 冲突漏检率不高于旧路径；
- 高风险请求不得绕过现有风险策略；
- child 失败、预算拒绝和 provider 错误不得静默隐藏。

当前代码与测试只证明协议、安全边界、持久化、恢复、事件和路由行为正确。生产数据
尚未证明 P95 延迟下降 20%、输入 token 下降 25%、LLM 调用减少或 citation/质量不退化；
在这些结论按 QueryKind、capability 和风险等级分桶成立前，不改变默认 flag，也不删除
旧 Workflow stage。

## 15. Feature Flag 与配置

实际配置 owner 为 `platform/config.PlatformSettings`。截至 2026-08-16 的默认值：

```text
DelegationEnabled=false
DelegationShadowEnabled=false
DelegationWorkflowEscalationEnabled=false
DelegationCapabilities=[]                # 显式 allowlist
MaxDepth=1                               # 应用构造的固定 delegation policy
DelegationMaxChildren=3
DelegationMaxConcurrent=2
DelegationChildTimeout=90s
DelegationMaxChildTurns=4
DelegationMaxChildToolCalls=8
DelegationMaxChildInputTokens=12000
DelegationMaxChildOutputTokens=1200
DelegationMaxReportTokens=1000
DelegationMaxTotalTokens=48000
DelegationParentAnswerReserve=4000
DelegationMaxTotalCostMicros=0            # 未配置费用硬上限
```

启用 delegation 时，`DelegationCapabilities` 必须最终解析出至少一个 enabled、
read-only、`Role=investigator` 的 Capability；否则配置校验失败。Shadow 和 Workflow
escalation 都依赖主 delegation flag。并发不能超过 child 数量，child timeout 必须大于
parent answer reserve，聚合 token 上限必须覆盖全部 child grant 和 parent reserve。

配置在 ingress/store 边界完成规范化和校验，运行时只消费 canonical policy，不新增
分散环境变量或读取时兼容逻辑。

灰度维度可以包括：

- 用户或租户；
- QueryKind；
- capability；
- 风险等级；
- 请求采样比例。

## 16. 风险与应对

| 风险 | 应对 |
|---|---|
| parent 过度委派 | child 数量、深度、预算和并发硬限制 |
| 改造后延迟反而增加 | batch fan-out，不依赖顺序 tool calls |
| child 报告过长 | 固定 schema、token/byte 上限、artifact reference |
| 权限扩大 | capability 服务端映射，权限取交集 |
| 子报告冲突 | 显式/结构化 deterministic conflict 汇总；自由文本重叠按需 verifier |
| validator 误称语义安全 | 输出 structured coverage 和 unverified semantic overlap，不将 `has_conflicts=false` 等同于语义一致 |
| 并发预算超扣 | parent-owned reservation、exact RunLimits、authoritative settlement |
| 失去持久化 | child 继续走 Run Runtime，并补齐 delegation relation/report artifact |
| parent 取消产生孤儿任务 | parent-child 关联和 cancellation propagation |
| 新旧路径重复执行 | 唯一路由 owner、显式 route reason、幂等 escalation receipt |
| 上层 Capability 被错误映射到 Workflow | 独立 WorkflowBinding；无 binding 返回 `workflow_unavailable` |
| 永久双轨增加维护成本 | 设定阶段退出条件，数据验证后删除无调用方旧路径 |
| 删除 verifier 导致质量下降 | 先改为条件触发，不一次性删除 |
| 新类型继续膨胀 | 复用现有 Capability、EvidenceUnit、RunRequest，不建第二套 registry |

## 17. 决策清单

本提案建议确认以下决策：

1. 普通 QA 是否以主 Agent 工具循环为默认控制面：**是**。
2. 动态委派是否采用一个 typed batch tool：**是**。
3. 模型是否可以指定任意 Agent、工具或权限：**否**。
4. child 是否继续通过现有 `run.Execute` 和 RunStore：**是**。
5. child 是否接收完整 Task Contract：**否**。
6. child 是否允许继续 fork：**第一版否**。
7. 是否直接并行所有 tool calls：**否**。
8. 是否直接删除 Workflow DAG：**否**。
9. 是否直接删除 join/verify/risk gate/synthesize：**否**。
10. 普通路径是否由 parent 完成最终综合：**是**。
11. 确定性 evidence/report 校验是否保留：**是**。
12. deterministic validator 是否声称发现任意自由文本语义冲突：**否**，只覆盖显式冲突和注册 ClaimPolicy 的结构化冲突。
13. 多报告关键结论需要合并自由文本时是否触发语义 verifier：**是**。
14. run-level child limits、parent budget reservation 和 settlement 是否在实现前落到 Runtime/Store：**是**。
15. delegation status 是否另建独立生命周期状态机：**否**，由 admission + 现有 Run 状态 + report completeness 投影。
16. Workflow escalation 是否通过独立 WorkflowBinding 和幂等服务端协议：**是**。
17. Workflow node ID 是否属于 Capability 或可从 Tool ID 猜测：**否**。
18. Workflow 是否保留为 durable/high-risk 路径：**是**。
19. 是否先 shadow 和灰度再改变默认路由：**是**。
20. parent 是否必须显式记录每个 delegation 的 report 采用情况：**是**，通过严格、
    隐藏且在交付前剥离的 final-answer metadata。
21. verifier output 是否可以伪装成 adopted report：**否**，verification ID 不属于
    delegation report allowlist。

## 18. 最终结论

本提案不是将现有 Workflow 改名为 delegate，也不是再增加一套并列的多 Agent 平台。目标是重新分配现有能力的控制权：

```text
当前
  入口 Planner 拥有拆解
  Workflow 拥有调查、验证和综合
  investigator 接收完整 Task Contract

目标
  Parent Agent 拥有动态拆解和最终综合
  Child Run 拥有隔离的窄调查
  Server Policy 拥有权限、预算和并发控制
  RunStore 拥有完整轨迹和报告持久化
  Workflow 保留长任务、高风险和可恢复编排
```

推荐实施顺序是：

```text
先定义窄协议和运行边界
  -> 单 child 验证权限、预算、持久化和报告
  -> batch executor 实现受限并发
  -> single 路径灰度
  -> shadow 对比旧 Workflow
  -> 调整默认路由
  -> 最后删除被数据证明无价值的固定阶段
```

该方向可以直接解决当前两个主要问题：

- **上下文大**：不再向每个 investigator 广播完整 Task Contract，parent 只接收有界报告；
- **时间长**：不再让所有普通请求固定经过 Task Graph 预规划和多次串行 LLM 收敛阶段。

同时，它复用现有权限、Definition budget、Agent Runtime、EvidenceUnit、RunStore 和 Durable Workflow，并明确补齐 dynamic delegation 所缺少的 run-level budget、关联持久化和 escalation 协议，不以降低延迟为理由牺牲安全边界、运行事实或高风险任务的可恢复性。
