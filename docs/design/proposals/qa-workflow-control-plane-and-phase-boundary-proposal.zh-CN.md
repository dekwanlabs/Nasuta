# QA 调查 Workflow 控制面、阶段边界与单一契约收敛提案

状态：草案
作者：Nasuta Agent Platform Team
日期：2026-08-29
关联事项：Trace `32cf05b4ff23`；Workflow `workflow_run_d5f6787e73fa1674d01c2853`；调查日志 `/Users/dequan.mac/.codex/attachments/6706bacf-35bb-4d20-8aea-3634a0b1dbeb/pasted-text.txt`
目标版本：待评审

> 本提案是架构收敛提案，当前只记录问题、目标模型、修改顺序和验收标准，不直接修改生产代码。
> 本提案不替代已有的查询意图、证据采集、预算治理和并行调度提案，而是规定这些机制如何在同一条 workflow 中组合，避免每个提案各自维护一套事实源。

## 1. 摘要

本提案用于解决调查型 QA Workflow 长期出现的“局部修改互相影响、任务状态互相覆盖、资料很多但无法形成可靠结论”问题。

触发案例是用户提问：

> 我们的架构是什么样的，有哪些业务，挑选三个核心的业务详解讲解一下

2026 年 8 月 29 日 17:29:01（`+08:00`）的日志表明，系统识别出这是一个需要先发现业务、再选择业务、最后详细调查的 overview 问题，但实际路由同时创建了 3 个独立并行任务；第一轮调查只要求证明 `business_domain`，第二轮自然语言目标又要求覆盖入口、流程和数据，但结构化 `evidence_goals` 仍将这些 Facet 标记为非必需。随后任务图规划降级、服务身份解析失败、证据冲突未完成裁决、一个调查结果被截断并恢复为 partial，最终同时出现 `status=failed` 和 `status=delivered`。

这不是某个关键词、某个服务别名、某次模型输出截断或某个重试次数的单点问题。机制层根因是：**同一个请求在 Query Plan、Investigation Contract、Task Contract、Evidence Ledger、Result Recovery、Workflow Terminal 和 Delivery 层被重复表达，但没有唯一的控制面和统一的完成标准。** 发现、选择、身份解析、调查、证据裁决和回答合成也没有形成有门槛的阶段边界。

本提案计划通过“单一执行契约、严格阶段门、类型化实体身份、Facet/Claim 级证据覆盖、执行状态与结果质量分离、失败前置校验”收敛当前链路，将当前流程从：

```text
用户问题
→ 解析 Query Plan
→ 提前路由为 3 个并行任务
→ 宽泛检索并发现业务
→ 从发现结果直接挑选并标记 core_business
→ 用不一致的 Task Contract 调查
→ 失败结果继续作为 partial evidence 传播
→ workflow failed 但 delivery succeeded
→ 保存一个无法验证完整度的结果
```

调整为：

```text
用户问题
→ 入口规范化并生成唯一 Query Plan
→ Discovery 阶段只发现候选业务
→ Selection 阶段按显式规则选择业务并保留依据
→ Identity Resolution 阶段绑定业务、服务、仓库和文档
→ 为每个已选业务生成统一的 Facet Contract
→ 校验任务图，契约不一致时在执行前失败
→ 分阶段采集并裁决 Claim/Evidence
→ 通过 Completeness Gate 后才允许合成
→ 分离 workflow、结果质量和 delivery 状态
```

预期实现以下效果：

1. 不再让自然语言目标和机器验收标准表达不同的任务；
2. 不再在业务尚未发现时提前创建业务解释任务；
3. 不再用自由字符串猜测业务对应的服务和文档；
4. partial、failed、succeeded、delivered 各自表达真实含义；
5. 任何最终结论都能追溯到具体实体、Facet、Claim 和证据来源；
6. 后续修改可以沿明确的所有权边界进行，不再通过局部补丁修复跨层契约问题。

## 2. 背景

### 2.1 业务与技术背景

调查型 QA 用于回答不能依赖单个文件或单个函数的问题，例如：

- 系统整体架构如何分层；
- 系统有哪些业务领域；
- 哪些业务是核心业务；
- 某个业务从入口到下游服务、数据和状态的完整链路是什么；
- 结论是否有代码、文档或运行时证据支持。

这类问题天然包含多个阶段，但这些阶段的输入输出不同：

```text
用户问题
→ 问题语义与答案覆盖要求
→ 候选业务发现
→ 核心业务选择
→ 业务身份解析
→ 业务证据调查
→ 证据冲突与覆盖裁决
→ 最终回答合成
```

当前相关链路为：

```text
QA 入口
→ Query Prepare / Retrieval
→ Route
→ Coordinator / Continuation
→ Investigation Planner
→ Durable Runner
→ Agent Runtime / Tools
→ Evidence Ledger
→ Result Recovery
→ Workflow Terminal / Delivery
→ QA Submission / Answer
```

各模块的主要职责和当前问题如下：

| 模块 | 当前职责 | 输入 | 输出 | 当前边界问题 |
| --- | --- | --- | --- | --- |
| `internal/transport/dashboard/qa.go` | 接收用户问题并启动 QA | 用户问题、会话 | QA 请求 | 入口层之后同一语义被多次重新解释 |
| `internal/agent/qa/prepare.go` | 生成 Query Plan、准备检索 | 用户问题、上下文 | Query Plan、初始检索结果 | Query Plan 没有成为后续唯一事实源 |
| `internal/agent/qa/route.go` | 选择执行路径、并行策略 | Query Plan | Route | 在实体未知时提前创建业务任务 |
| `internal/agent/qa/select_then_explain.go` | 处理发现、选择和解释的 continuation | 初始结果、历史证据 | 选中实体、下一轮任务 | 阶段边界和选择依据不清晰 |
| `internal/agent/qa/coordinator.go` | 合并快照、判断 continuation、规划和等待 | Run snapshot、任务结果 | 下一轮 Workflow 或父级结果 | 同时承担总控、状态机、预算、证据合并和恢复 |
| `internal/agent/investigation/contract.go` | 定义调查契约和运行状态 | Query/Task 计划 | Investigation Contract | 运行状态、任务状态、证据要求和结果质量混在一起 |
| `internal/agent/investigation/planning.go`、`proposal_plan.go` | 生成和校验任务图 | Contract、Capability | Task Graph | 自然语言目标和 required Facet 可能不一致 |
| `internal/agent/investigation/runtime_executor.go` | 将 Task 投影为 Agent Run 并收集结果 | Task Contract | Investigator Result | 任务能力和目标 Facet 可能不匹配 |
| `internal/agent/definition/result.go` | 映射运行错误、恢复部分结果 | Agent Result、Error | Result / Partial Report | 失败结果可以继续以可用结果形态传播 |
| `internal/evidence/ledger.go` | 收集、合并和保留证据 | Evidence Observations | Evidence Ledger | 证据数量和冲突数量不等于 Facet 覆盖和结论可用 |
| `app/investigation_runner.go` | Durable Workflow 启动、等待和交付 | Workflow、Run | Terminal、Delivery | `failed` 与 `delivered` 的含义没有在上层统一投影 |
| `internal/agent/qa/submission.go` | 保存 QA Run 并提交后续处理 | 父级结果 | 保存记录、回答输入 | 未见统一的最终 Completeness Gate |

### 2.2 当前实现

本次日志显示的实际执行步骤如下：

1. QA 入口收到 overview 问题，Query Plan 记录 `required_facets=7`，但 `entities=0`；
2. Route 记录 `discover_then_select=true`，同时将任务路由为 3 个独立并行任务，并标记 `evidence_decomposable=false`；
3. 初始检索召回代码、服务和文档混合资料，形成资料池，但没有先形成带证据的候选业务清单；
4. 第一轮 Investigation 只要求 `discover_businesses` 和必需的 `business_domain`；
5. 第二轮从第一轮结果中产生 `Device & IoT`、`Cookbook & Recipe` 和 `Message & Push` 三个实体，并直接赋予 `role=core_business`；
6. 第二轮自然语言 objective 要求入口、流程和数据，但实际下发的任务使用 `knowledge.docs.verify`，每个任务的 required evidence 仍然只有 `business_domain`；
7. Agent 用 `message_push`、`iot-device`、`hsds-cookbook` 等不同名称调用服务或文档工具，多次得到 `service_not_found`，然后回退到语义检索和推断；
8. 任务累计了 `prior_evidence_count=51` 和 `prior_conflict_count=7`，但没有形成按实体和 Facet 的裁决结果；
9. Device 调查的结构化输出被截断，系统将部分结果恢复为 evidence-preserving partial report；
10. Durable Runner 最终同时记录 Investigation `status=failed` 和 execution `status=delivered`，父流程仍然保存 Run。

### 2.3 为什么现在需要修改

本次修改由一次无法形成可靠回答的实际运行触发：

- 触发时间：`2026-08-29 17:28:59` 至 `2026-08-29 17:31:12 +08:00`；
- 触发标识：Trace `32cf05b4ff23`；
- 用户问题：`我们的架构是什么样的，有哪些业务，挑选三个核心的业务详解讲解一下`；
- 父级 Run：`run_d5f6787e73fa1674d01c2853`；
- 第二轮 Workflow：`workflow_run_d5f6787e73fa1674d01c2853_round_2`；
- 直接表现：已检索到较多内部资料，但系统不能验证三个核心业务及其入口、流程和数据，最终只能给出证据不足的保守结论；
- 影响范围：涉及所有需要“先发现对象、再选择对象、再逐对象解释”的调查型 QA 问题；
- 当前临时处置：保留部分失败输出并交付给父流程，但没有形成可验证的完整度结论。

### 2.4 范围与非目标

#### 目标

1. 为 Query Plan、Investigation Contract、Task Contract、Evidence Ledger 和最终合成建立清晰的所有权关系；
2. 将 Discovery、Selection、Identity Resolution、Investigation、Adjudication 和 Synthesis 变成具有输入、输出和门槛的阶段；
3. 让所有机器验收条件来自结构化 Contract，而不是来自自然语言 objective 的隐式理解；
4. 让每个已选实体的 required Facet 覆盖可以被确定性计算；
5. 让业务实体、服务、仓库、文档和 schema 通过类型化身份或注册关系连接；
6. 让失败、部分完成和交付状态分离，并禁止 delivery 被解释为成功；
7. 让契约矛盾在 Agent 启动前被发现，禁止通过静默降级继续执行错误任务图；
8. 在不直接修改代码的当前阶段，先完成设计收敛、日志回放和验收规则定义。

#### 非目标

1. 本提案不针对 `Device & IoT`、`Cookbook & Recipe` 或 `Message & Push` 增加硬编码业务特例；
2. 本提案不通过单纯提高模型 token、超时或重试次数掩盖任务拆分和输出契约问题；
3. 本提案不在当前阶段修改生产代码、数据库 schema 或已有工作区改动；
4. 本提案不要求所有问题都使用多 Agent 或 durable workflow；简单问题应继续使用简单路径；
5. 本提案不保证通过内部代码和文档证明仓库外部系统的全部运行时行为；
6. 本提案不再新增一套重量级 Ontology、事件溯源状态机或与既有 proposal 平行的 Evidence 模型；
7. 本提案不替代以下已有专项提案，而是规定其组合边界：
   - `qa-query-intent-and-facet-model-simplification.zh-CN.md`；
   - `qa-unified-evidence-acquisition-pipeline.zh-CN.md`；
   - `qa-agent-context-budget-and-cancellation.zh-CN.md`；
   - `investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`；
   - `investigation-parallel-budget-starvation-and-evidence-salvage-proposal.zh-CN.md`；
   - `qa-comparison-entity-and-evidence-coverage-proposal.zh-CN.md`。

## 3. 问题

### 3.1 问题描述

**期望行为：**

对于“列出业务并挑选三个核心业务详细解释”的问题，系统应先明确识别这是一个多阶段任务：

```text
发现候选业务
→ 保存每个候选业务的证据
→ 按明确规则选择三个核心业务
→ 为每个业务解析 canonical identity
→ 分别收集入口、边界、流程、数据和依赖证据
→ 校验每个业务的 required Facet 覆盖
→ 只有在达到回答门槛后合成最终答案
```

**实际行为：**

系统在实体为空时已经路由为 3 个独立任务；第一轮只验证业务领域存在；第二轮直接把若干发现结果标为 `core_business`；第二轮的自然语言目标、required Facet 和 capability 不一致；工具使用自由字符串查服务；部分失败结果继续向后传播；最终 workflow 和 delivery 状态同时出现失败和交付。

**差异：**

系统把“有一些相关资料”“找到了业务名称”“生成了一个非空 JSON”“结果已交付”分别当成了不同层面的推进条件，但没有一个统一条件回答：

```text
这三个业务是否已经被证明是核心业务？
每个业务的入口、流程和数据是否都有足够证据？
证据冲突是否已经裁决？
当前结果是否允许合成并对用户作出结论？
```

因此，资料检索可以完成，Workflow 也可以交付结果，但用户问题仍然不能可靠回答。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 最终只能返回“证据不足，不能可靠回答” | 触发案例的最终回答；`pasted-text.txt` 中的保存和 compaction 日志 |
| 表面现象 | 服务和文档查询多次 `service_not_found` | `check_docs` 对 `message_push`、`iot-device`、`hsds-cookbook` 的错误日志 |
| 表面现象 | Agent 结构化输出被截断 | `answer_generation.go:289`：`structured output still truncated` |
| 直接原因 | 第二轮只把 `business_domain` 标为 required，入口、流程、数据均为 optional | 第二轮 Task Contract 中的 `evidence_goals` |
| 直接原因 | 任务图在契约异常时降级生成，而不是阻止执行 | `coordinator.go:471`：`continuation task graph planner degraded` |
| 直接原因 | partial report、workflow failed、delivery delivered 可以同时进入父流程 | `result.go:290`、`investigation_runner.go:251`、`investigation_runner.go:401` |
| 机制根因 | Query Plan、阶段计划、任务契约和结果验收没有单一事实源 | `prepare.go`、`route.go`、`coordinator.go`、`contract.go`、`planning.go` 等共同维护相关语义 |
| 机制根因 | Discovery、Selection、Identity Resolution 和 Investigation 没有硬阶段边界 | `discover_then_select=true` 但同时创建 3 个调查任务；实体直接获得 `core_business` 角色 |
| 机制根因 | 业务实体和服务/文档身份没有统一映射 | `Device & IoT`、`device_iot`、`iot-device`、`hsds-device-*` 等名称在不同工具调用间漂移 |
| 机制根因 | Evidence Ledger 主要承担收集和合并，没有以 Claim × Entity × Facet 完成裁决 | `prior_evidence_count=51`、`prior_conflict_count=7`，但没有对应覆盖矩阵和冲突结论 |
| 机制根因 | Agent 同时承担检索、推理、证据组织和长 JSON 生成，输出预算与任务职责耦合 | `maxSteps=4`、`max_tokens=8192`，最终出现截断和 partial recovery |

根因链路：

```text
用户问题需要多个阶段
→ Query Plan 记录了答案 Facet，但没有成为唯一执行契约
→ Route 在实体未知时提前创建 3 个任务
→ Discovery 只产出 business_domain 证据
→ Selection 直接从资料中赋予 core_business 角色，缺少选择依据
→ 下一轮 objective、required Facet 和 capability 互相矛盾
→ Planner 降级而不是拒绝错误契约
→ Agent 使用非 canonical 名称查服务，产生空结果和推断
→ Evidence 只有数量，没有实体/Facet/Claim 级裁决
→ 长结构化输出截断，失败结果被包装为 partial 并继续传播
→ Workflow、Result、Delivery 状态没有统一投影
→ 最终无法判断哪些结论可以对用户负责
```

本问题不能只通过追加关键词、增加服务别名、提高 token 或超时时间、增加重试、扩大检索召回、针对某个业务写特例解决，因为这些方式只会改变某个局部输入或缓解某个症状，不能解决同一语义被多层重复拥有的问题。

### 3.3 影响

- **用户影响：** 资料可能很多，但最终无法给出稳定、可验证的架构和核心业务解释；不同次运行可能选择不同业务或形成不同链路；
- **业务影响：** 重要业务的选择缺少依据，回答可能把相邻模块、服务或文档误当成业务；
- **系统影响：** 失败结果继续传播，父级 Workflow 无法根据真实完整度决定是否继续；并行任务、续跑、重试和预算互相耦合；
- **工程影响：** 同一规则分散在多个 package，局部修复产生跨模块回归，日志不能回答“哪一步决定了这个结果”；
- **验证影响：** 当前很难为“完整回答”“部分回答”“不能回答”定义可重复的验收条件，也难以从单个 trace 反推出失败责任边界。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：先发现业务、再选择三个核心业务的问题

- **Given（前置条件）：** 用户问题要求列出业务并选择三个核心业务；Query Plan 尚未解析出任何实体；
- **When（触发行为）：** Route 计算 `discover_then_select=true` 并启动调查；
- **Then（期望结果）：** 只创建 Discovery 阶段任务，输出候选业务、每个候选业务的证据和选择所需指标；
- **But（当前结果）：** 日志同时记录 `tasks=3`、`independent_tasks=3`，后续又直接产生三个 `core_business` 实体。

示例输入：

```text
我们的架构是什么样的，有哪些业务，挑选三个核心的业务详解讲解一下
```

当前执行路径：

```text
Query Plan: entities=0
→ Route: discover_then_select=true, tasks=3
→ 宽泛检索
→ business_domain 发现
→ 直接生成三个 core_business EntityRef
```

关键证据：

```text
canonical query plan kind=overview required_facets=7 entities=0
execution route ... tasks=3 ... evidence_decomposable=false discover_then_select=true
```

#### 场景 B：自然语言目标与机器验收条件不一致

- **Given：** Task objective 要求解释入口、流程和数据；
- **When：** 任务被转换成 `InvestigationContract`；
- **Then：** `entrypoint`、`core_flow`、`data_and_state` 至少应按回答策略成为 required，并绑定对应 capability；
- **But（当前结果）：** 只有 `business_domain` 是 required，且执行能力为 `knowledge.docs.verify`；planner 发现异常后使用 deterministic gap cover。

关键证据：

```text
continuation task graph planner degraded; using deterministic gap cover:
task 1 evidence goal "entrypoint" is not required
```

#### 场景 C：业务身份和服务身份不一致

- **Given：** 任务实体是 `Message & Push`，内部 ID 是 `message_push`，实际服务是 `hsds-message-push`；
- **When：** Agent 把业务 ID 直接传给 `check_docs` 或 `get_service`；
- **Then：** 应由 Identity Resolution 返回 canonical service ref，再由工具按 service ref 查询；
- **But（当前结果）：** `message_push`、`iot-device`、`hsds-cookbook` 等名称直接查询失败，Agent 只能换名称重搜或根据资料推断。

关键证据：

```text
check_docs error ... service="message_push" ... service_not_found
check_docs error ... service="iot-device" ... service_not_found
check_docs error ... service="hsds-cookbook" ... service_not_found
```

#### 场景 D：结构化输出截断但已有部分证据

- **Given：** Agent 已成功执行若干工具调用并获得证据，但在生成 `investigation.report` 时达到步骤或输出限制；
- **When：** Agent Runtime 返回截断错误；
- **Then：** 应保留 evidence，但明确标记结果为 partial，并阻止其满足未覆盖的 required Facet；
- **But（当前结果）：** 结果恢复为 evidence-preserving partial report，随后与 workflow failed 和 delivery delivered 一起进入父流程。

关键证据：

```text
structured output still truncated after 0 continuation rounds
preserving partial final answer at step 4
recovered model-output failure ... as an evidence-preserving partial report
```

#### 场景 E：证据数量多但结论仍不可验证

- **Given：** 运行已累计 51 条 evidence，并发现 7 个冲突；
- **When：** Coordinator 继续规划和合并；
- **Then：** 应输出每个实体、每个 Facet 的 coverage，以及每个冲突的裁决状态；
- **But（当前结果）：** 只有总数量和冲突数量，无法判断入口、流程、数据分别是否已覆盖。

关键证据：

```text
prior_evidence_count=51 prior_conflict_count=7
```

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 实体未知 | `entities=0` 且问题要求先选择对象 | 仍可提前生成多个对象调查任务 | 只运行 Discovery，禁止创建对象解释任务 |
| 任务目标与 required Facet 矛盾 | objective 要求入口，但 `entrypoint.required=false` | planner 降级并继续 | 执行前返回 `invalid_contract`，不启动 Agent |
| 业务身份未解析 | 业务名称没有 service/doc 映射 | 工具报 `service_not_found`，Agent 猜名称 | 标记 `identity_unresolved`，按规则补解析或阻断相关结论 |
| 证据缺 required Facet | 有业务领域证据，没有入口或流程证据 | 可能继续当作可用报告 | 结果为 partial/insufficient，不满足完整回答 gate |
| 有证据但 Agent 失败 | 工具成功后输出截断或预算耗尽 | partial 继续传播但边界模糊 | 保留 evidence，结果质量明确为 partial，失败原因保真 |
| 无证据且 Agent 失败 | 首次调用前就失败 | 可能被包装成非明确失败 | `failed`，不能伪造 partial |
| Workflow 失败但结果已交付 | Runner 已传递部分结果 | 上层容易把 delivered 当成功 | delivery 单独记录，合成 gate 仍按 workflow 和 completeness 判断 |
| 证据存在冲突 | 同一 Claim 有多个来源且结论不一致 | 只累计冲突数量 | 按来源优先级和 Claim 身份裁决，未裁决则保留限制 |
| 并行任务上下文不同 | 兄弟 Task 共享预算或证据上下文 | 后启动任务可能截断或受污染 | 沿用预算和上下文专项提案，按 Task 隔离投影并执行 admission |

### 4.3 复现步骤

1. 使用当前版本提交以下问题：

   ```text
   我们的架构是什么样的，有哪些业务，挑选三个核心的业务详解讲解一下
   ```

2. 记录 Trace `32cf05b4ff23`、父级 Run `run_d5f6787e73fa1674d01c2853` 和第二轮 Workflow `workflow_run_d5f6787e73fa1674d01c2853_round_2`；
3. 检查 `prepare.go`、`route.go`、`coordinator.go`、`investigation_runner.go` 和 Agent Runtime 的结构化日志；
4. 验证是否同时看到以下条件：
   - `entities=0` 但 `tasks=3`；
   - `discover_then_select=true` 但已创建业务调查任务；
   - objective 提及入口/流程/数据，但 corresponding evidence goal 非 required；
   - `service_not_found`；
   - `prior_evidence_count` 和 `prior_conflict_count` 有值但无 Facet coverage；
   - `structured output still truncated`；
   - workflow `failed` 与 execution `delivered` 同时存在；
5. 检查最终提交记录是否包含“结果是否完整、哪些 Facet 缺失、哪些 Claim 未裁决”的结构化信息。

## 5. 如何修改

### 5.1 修改原则

1. **一个概念只有一个事实源。** Query Plan 负责请求语义，Investigation Contract 负责当前阶段和执行要求，Task Contract 只是按任务投影，Evidence Ledger 负责证据，Runner 负责执行和 delivery；下游不能重新定义上游语义。
2. **先校验契约，再启动 Agent。** 目标文本不能替代结构化 required Facet；任意 required Facet 没有 capability、source 或验收规则时，返回明确的 `invalid_contract`，不使用静默降级任务图。
3. **严格区分阶段。** Discovery 不能直接完成 Selection，Selection 不能隐式完成 Identity Resolution，Investigation 不能自行改变核心业务选择。
4. **在入口统一身份。** 业务、服务、仓库、文档和 schema 使用类型化引用及显式别名关系；下游工具不接收任意自然语言名称作为唯一身份。
5. **证据按 Claim × Entity × Facet 管理。** evidence 数量只能用于观测，不可作为完成标准；完成度必须从可引用、可追溯、无未裁决冲突的证据推导。
6. **部分产物可以保留，但不能伪装成成功。** partial 是结果质量，不是 workflow 成功；delivery 是传递事实，不是业务验收。
7. **不增加平行架构。** 优先收敛现有 `QueryPlan`、`InvestigationContract`、`TaskContract`、`EvidenceUnit` 和 `Completeness`，不为同一概念再建立第二套结构。
8. **失败原因必须保真。** 契约错误、身份未解析、证据不足、预算耗尽、工具失败和模型输出截断必须有可区分的稳定分类。

### 5.2 目标流程

```text
QA ingress
→ NormalizeAndValidate
→ Build canonical QueryPlan
→ Determine execution phase
→ Discovery（仅当实体未知）
→ Selection（需要显式选择规则和依据）
→ Identity Resolution（业务到服务/仓库/文档的 canonical refs）
→ Build per-entity Facet Contract
→ ValidateTaskGraph
→ Bounded Investigation（代码、文档、服务能力按 Facet 分配）
→ Evidence Ledger merge
→ Claim/Facet adjudication
→ Completeness Gate
→ Synthesis
→ Project workflow/result/delivery status
→ Persist and observe
```

与当前流程相比，关键变化是：

1. 在入口之后冻结一份 canonical Query Plan，后续只允许通过显式阶段转换更新，不允许每层重新推导 required Facet；
2. 在 `entities=0` 且存在 `discover_then_select` 时，Route 只能启动 Discovery 任务，不得提前创建三个业务解释任务；
3. Selection 必须输出选择规则、候选列表、被选实体和选择证据，`role=core_business` 只能由 Selection 阶段产生；
4. Identity Resolution 输出稳定的 `EntityRef`、`ServiceRef`、`RepositoryRef` 和 `DocumentRef`，工具调用使用 canonical ref；
5. Task Graph 根据结构化 Facet Contract 派生，objective 仅用于说明，不参与完成度判断；
6. Evidence Ledger 以实体、Facet 和 Claim 为索引，Verifier 输出 coverage 和 conflict adjudication，而不是只输出 evidence 总数；
7. Agent 输出优先返回受控的 evidence/claim artifact，长篇自然语言由独立 Synthesis 阶段生成；
8. Workflow 执行状态、结果完整度和 delivery 状态分别保存、分别记录、分别参与决策。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| Canonical Query Plan | Query Plan、Task Contract 和 coordinator 各自持有部分语义 | Query Plan 保存请求级不可变语义；阶段和要求由同一 Contract 派生 | `internal/agent/qa/prepare.go`、`internal/agent/investigation/contract.go` | 先影子计算并记录差异，不立即删除旧字段 |
| 阶段控制 | `discover_then_select` 与 3 个任务并存 | 用阶段门控制 Discovery、Selection、Identity Resolution、Investigation、Synthesis | `internal/agent/qa/route.go`、`coordinator.go`、`select_then_explain.go` | 先拒绝明显矛盾的组合，保留旧路径开关用于回滚 |
| Task Contract 校验 | objective 和 required Facet 可能不一致 | 任务启动前确定性校验 Facet、Capability、Source 和完成标准 | `internal/agent/investigation/planning.go`、`proposal_plan.go` | 增加 `invalid_contract` 观测，确认无误后关闭 degraded fallback |
| 实体身份 | 业务 ID、服务名和文档名自由传递 | 使用类型化 Entity/Service/Repository/Document refs 和显式 alias mapping | `internal/agent/catalog`、`internal/agent/qa`、工具适配层 | 旧名称只在入口解析一次，未注册名称不得静默猜测 |
| 业务选择 | 直接给发现结果设置 `core_business` | Selection 保存候选、排序依据、选择理由和证据 | `internal/agent/qa/select_then_explain.go` | 旧结果可读，但没有选择依据的实体不能进入完整调查 |
| Evidence 覆盖 | 统计 evidence 数量和冲突数量 | 生成 Entity × Facet × Claim coverage，区分 direct/inferred/conflicted/partial | `internal/evidence/ledger.go`、Verifier 相关实现 | 先从已有 evidence manifest 派生 coverage，不丢历史证据 |
| 结果恢复 | Agent 失败时将部分结果恢复为 partial 并继续传播 | 保留 evidence，但明确 Result Completeness 和 failure cause；不得满足缺失 required Facet | `internal/agent/definition/result.go`、`runtime_executor.go` | 保留证据抢救能力，移除伪成功语义 |
| 状态投影 | Workflow `failed` 与 Runner `delivered` 混合解释 | Execution Status、Result Completeness、Delivery Status 三列独立投影 | `app/investigation_runner.go`、`internal/agent/qa/submission.go` | 旧状态记录只读兼容，新增结构化字段和迁移映射 |
| Synthesis Gate | 结果保存后即可进入后续处理 | 只有 coverage 满足 Completion Policy 才允许完整回答 | `internal/agent/qa/coordinator.go`、submission/synthesis | 不满足时生成带缺口的受控结果或明确拒答 |
| Agent 输出 | 单次 Agent 生成完整长 JSON 报告 | 调查 Agent 返回 bounded evidence/claim artifacts，Synthesis 生成长文 | `internal/agent/execution`、`runtime_executor.go` | 逐步引入 output schema version，旧 report 仅作兼容输入 |

#### 改动一：建立阶段化的 canonical contract

**方案：**

不新建与现有 Query Plan、Investigation Contract 平行的“大总合同”，而是明确现有对象的所有权：

| 概念 | 唯一所有者 | 下游使用方式 |
| --- | --- | --- |
| 用户问题的回答形态、目标实体是否已知、请求级 Facet | canonical Query Plan | Route 和 Discovery 读取，不重新解释 |
| 当前阶段、已确认实体、阶段输入输出、Completion Policy | Investigation Contract / persisted snapshot | Coordinator 推进阶段，Task Planner 只读 |
| 单个任务要做的 Facet 和允许能力 | Task Contract | Runtime 执行，不改变父级目标 |
| Claim、EvidenceUnit、来源身份和冲突关系 | Evidence Ledger | Verifier 计算 coverage，Synthesis 只能读取已裁决结果 |
| Execution Status、Result Completeness、Delivery Status | 各自对应的运行/结果/传输所有者 | 上层按规则投影，禁止相互替代 |

**约束：**

- 自然语言 objective 是说明字段，不是机器完成条件；
- required Facet 必须在 canonical contract 中定义，并由 Task Contract 继承或显式投影；
- 阶段转换必须记录输入 snapshot、输出 artifact 和失败原因；
- 下游任务不能增加、删除或降低父级 required Facet；若确需改变，必须产生新的显式阶段计划版本。

**失败行为：**

- 如果 `entities=0` 但执行计划包含对象级解释任务，返回 `invalid_contract`；
- 如果 objective 提及的回答内容没有对应结构化 Facet，返回 `invalid_contract`；
- 如果 required Facet 没有可执行 capability 或 source，返回 `capability_contract_invalid`；
- 不使用 deterministic gap cover 掩盖契约错误；确定性 fallback 只能根据一个已经通过校验的 Contract 生成执行顺序。

#### 改动二：建立 Discovery → Selection → Identity Resolution 的阶段门

**方案：**

Discovery 输出候选业务，不输出最终核心业务；Selection 消费候选和选择证据；Identity Resolution 将选中的业务绑定到内部可查询对象；只有三者完成后，Investigate 才能启动。

Selection 的选择依据必须是显式的、可记录的规则或人工提供的优先级。规则可以使用业务重要性、覆盖范围、运行证据、服务数量、用户可见性等输入，但不能只依赖检索结果排序位置。

**约束：**

- 候选业务必须包含 `candidate_id`、展示名称、业务定义、支持证据和不确定性；
- `core_business` 角色只能由 Selection 阶段写入；
- 业务实体和服务实体不能共用同一个无类型字符串字段；
- 无法完成身份解析的实体可以保留为候选，但不能进入需要服务/源码证据的完整调查。

**失败行为：**

- 候选不足三个时，不伪造三个核心业务，返回 `selection_insufficient`；
- 没有选择依据时，不把前三个检索结果自动标记为核心业务；
- 身份无法解析时，返回 `identity_unresolved`，并将受影响的 Facet 标记为缺失，而不是用名称推断补齐。

#### 改动三：让 Evidence Ledger 负责证据裁决，而不是只负责收集

**方案：**

保留现有 canonical `EvidenceUnit` 和 ledger 方向，增加或明确其索引和验证维度：

```text
EntityRef × Facet × Claim
  → Supporting EvidenceUnits
  → Source/Trust metadata
  → Conflict group
  → Adjudication result
  → Coverage result
```

Verifier 至少要输出：

- 每个已选实体的 required Facet 是否覆盖；
- 每个 Facet 由哪些 Claim 支持；
- Claim 是直接证据、合理推断、部分证据还是未裁决冲突；
- 哪些证据仅能证明业务领域，不能支持入口、流程或数据；
- 哪些缺口阻止完整回答。

**约束与失败行为：**

- evidence 总数不得直接作为任务完成条件；
- `inferred` 不能在没有直接证据时自动升级为 `verified`；
- 冲突未裁决时，不得将对应 Claim 作为无条件事实合成；
- Ledger 合并不能改变 evidence 的原始来源、产生任务和实体归属；
- 没有 required Facet 覆盖时，结果可以保留 partial artifact，但不得通过完整回答 gate。

#### 改动四：分离执行状态、结果完整度和交付状态

**方案：**

保留现有执行和交付机制，但不再让一个 `status` 字段承载三种含义：

```text
ExecutionStatus:
  created / running / succeeded / failed

ResultCompleteness:
  complete / partial / insufficient

DeliveryStatus:
  not_delivered / delivered
```

这三个维度分别回答：

- Workflow 是否完成执行；
- 产物是否满足回答要求；
- 产物是否已被传递或保存。

**不变量：**

1. `delivered` 只表示传递成功，不表示执行成功或结果完整；
2. `partial` 必须携带未覆盖 Facet 和失败原因；
3. `complete` 必须满足 Completion Policy，不能仅因为结果非空；
4. 没有证据的失败结果不能伪造为 partial；
5. `failed + delivered` 可以作为“失败结果已被传递”的诊断组合存在，但上层不得将其提升为成功；
6. Synthesis 只能消费通过相应 Completeness Gate 的 evidence bundle。

### 5.4 数据结构或接口契约

以下是目标契约的概念字段，实施时优先复用和收敛现有字段，不直接照搬为一套新的平行类型：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `phase` | enum | Investigation Contract | 当前阶段 | `discovery` 或由 Query Plan 派生 | 旧 Run 可映射为 `legacy`，只读兼容 |
| `target_entity_refs` | typed refs | Contract / Selection | 已确认调查对象 | 空 | 旧字符串在入口解析，无法解析则显式失败 |
| `required_facets` | facet requirements | Query Plan / Contract | 回答必须覆盖的 Facet | 根据 QueryKind 派生 | 旧 optional 字段不能覆盖 canonical 要求 |
| `selection_basis` | structured artifact | Selection | 核心业务选择依据 | 空 | 没有依据不得标记完整选择 |
| `identity_refs` | typed refs | Identity Resolution | 业务对应的服务、仓库、文档等 | 空 | 支持别名读取，但不允许下游继续猜名称 |
| `coverage` | derived matrix | Verifier | Entity × Facet × Claim 覆盖 | 由 Ledger 派生 | 旧 evidence 可回放生成 |
| `execution_status` | enum | Workflow runner | 执行生命周期 | `created` | 与旧 `status` 映射 |
| `result_completeness` | enum | Verifier / Result | 结果完整度 | `unknown` | 旧结果缺失时为 `unknown`，不得默认为 complete |
| `delivery_status` | enum | Application runner | 是否已传递或保存 | `not_delivered` | 从交付事件派生 |
| `failure_code` | stable code | 失败发生层 | 契约、工具、预算、输出等失败类别 | 空 | 保留原始 wrapped cause |

阶段转换：

```text
[discovery]
  ├─ 候选业务充分 → [selection]
  ├─ 候选不足 → [selection_insufficient]
  └─ 不可恢复错误 → [failed]

[selection]
  ├─ 选择依据充分 → [identity_resolution]
  ├─ 核心业务不足 → [selection_insufficient]
  └─ 不可恢复错误 → [failed]

[identity_resolution]
  ├─ required identity 完成 → [investigation]
  ├─ 部分身份可用 → [investigation_partial_scope]
  └─ 无法解析关键身份 → [identity_unresolved]

[investigation]
  ├─ 所有任务完成 → [adjudication]
  ├─ 有证据但任务失败/预算耗尽/输出截断 → [adjudication_with_partial]
  └─ 无可用证据且不可恢复 → [failed]

[adjudication]
  ├─ required Facet 全覆盖且冲突已处理 → [synthesis_allowed]
  ├─ 有可引用部分但存在缺口 → [synthesis_partial_or_blocked]
  └─ 无法形成可引用结论 → [insufficient]

[synthesis_allowed / synthesis_partial_or_blocked]
  └─ 结果传递或保存 → `delivery_status=delivered`
```

这里不引入一个新的大型持久化状态机。阶段、执行状态和完整度分别表示已有的真实生命周期；能从快照、任务结果和 coverage 派生的状态不重复存储，只有跨进程恢复、审计或并发协调确实需要的事实才持久化。

不变量：

1. `discover_then_select=true` 且 `target_entity_refs` 为空时，Task Graph 只能包含 Discovery 任务；
2. 每个 required Facet 必须有唯一的 capability/source/验收定义；
3. Task Contract 只能是 canonical Contract 的投影，不能降低 required Facet；
4. Selection 没有选择依据时，实体不能获得完整的 `core_business` 结论；
5. Evidence 必须保留来源、实体归属、Facet 和 Claim 关系；
6. `result_completeness=complete` 必须由 verifier 根据 coverage 派生，不能由“结果非空”或“已交付”设置；
7. `delivery_status=delivered` 不得改变 `execution_status` 或 `result_completeness`；
8. 契约校验失败不能启动 Agent，也不能以 degraded task graph 代替失败。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 第一阶段只增加 canonical contract、phase、coverage 和三维状态的影子计算，不改变旧回答路径；旧 Run 仍可读取，但缺少完整度时按 `unknown` 处理，不默认成功；
- **数据迁移：** 不立即重写历史 evidence。先通过已有 manifest、Task ID、source metadata 和日志回放生成 coverage；如确认需要持久化新字段，再单独提交 schema、迁移和回填提案；
- **灰度方式：** 以 trace-only / shadow mode 运行新规划校验、Identity Resolution 和 Completeness Gate，比较新旧 task graph、coverage 和最终状态；确认结果后按问题类型逐步启用阶段门；
- **回滚条件：** 新路径造成正常问题类型的执行成功率、P95 延迟、成本或可回答率明显下降，或者出现实体/证据归属错误；
- **回滚步骤：** 关闭新路径开关，保留影子日志和失败样本，恢复旧执行路径；不得删除已产生的 evidence 或覆盖旧结果；
- **收敛条件：** 新路径连续通过回放和线上观测后，删除旧的隐式 fallback、重复字段和未使用的兼容分支，避免永久维护两套控制面。

## 6. 修改伪代码

### 6.1 核心流程

```go
func HandleQuestion(ctx context.Context, input UserQuestion) (Outcome, error) {
    query, err := PrepareCanonicalQuery(input)
    if err != nil {
        return Outcome{ExecutionStatus: Failed, FailureCode: "invalid_query"}, err
    }

    contract, err := BuildInvestigationContract(query)
    if err != nil {
        return Outcome{ExecutionStatus: Failed, FailureCode: "invalid_contract"}, err
    }

    for {
        switch contract.Phase {
        case Discovery:
            candidates, err := RunDiscovery(ctx, contract)
            if err != nil {
                return FailOrPartial(contract, err)
            }
            contract = contract.WithCandidates(candidates)
            if !DiscoverySatisfiesSelection(contract) {
                return Outcome{ExecutionStatus: Succeeded, ResultCompleteness: Insufficient,
                    FailureCode: "selection_insufficient"}, nil
            }
            contract = contract.AdvanceTo(Selection)

        case Selection:
            selection, err := SelectCoreEntities(contract.Candidates, contract.SelectionPolicy)
            if err != nil {
                return Outcome{ExecutionStatus: Succeeded, ResultCompleteness: Insufficient,
                    FailureCode: "selection_insufficient"}, err
            }
            contract = contract.WithSelection(selection).AdvanceTo(IdentityResolution)

        case IdentityResolution:
            identities, err := ResolveCanonicalIdentities(ctx, contract.SelectedEntities)
            if err != nil {
                return Outcome{ExecutionStatus: Succeeded, ResultCompleteness: Partial,
                    FailureCode: "identity_unresolved"}, err
            }
            contract = contract.WithIdentities(identities).AdvanceTo(Investigation)

        case Investigation:
            graph, err := BuildAndValidateTaskGraph(contract)
            if err != nil {
                // 契约错误必须在 Agent 启动前失败，禁止静默降级。
                return Outcome{ExecutionStatus: Failed, FailureCode: "invalid_contract"}, err
            }

            results := ExecuteTasks(ctx, graph)
            ledger := MergeEvidenceWithoutChangingOwnership(results)
            contract = contract.WithEvidence(ledger).AdvanceTo(Adjudication)

        case Adjudication:
            coverage := VerifyEntityFacetClaimCoverage(contract.Evidence, contract.CompletionPolicy)
            if coverage.HasUnresolvedRequiredGaps() {
                return Outcome{
                    ExecutionStatus: Succeeded,
                    ResultCompleteness: Partial,
                    Coverage: coverage,
                    FailureCode: coverage.PrimaryGapCode(),
                }, nil
            }
            contract = contract.WithCoverage(coverage).AdvanceTo(Synthesis)

        case Synthesis:
            answer, err := SynthesizeFromVerifiedEvidence(ctx, contract)
            if err != nil {
                return Outcome{ExecutionStatus: Failed, FailureCode: "synthesis_failed"}, err
            }
            return Outcome{
                ExecutionStatus: Succeeded,
                ResultCompleteness: Complete,
                Answer: answer,
                Coverage: contract.Coverage,
            }, nil
        }
    }
}
```

### 6.2 关键边界处理

```go
func ValidateTaskGraph(contract InvestigationContract, graph TaskGraph) error {
    if contract.Phase == Discovery && len(contract.TargetEntityRefs) != 0 {
        return ErrInvalidContract
    }

    if contract.DiscoverThenSelect && len(contract.TargetEntityRefs) == 0 {
        if graph.ContainsEntityInvestigationTask() {
            return ErrInvalidContract
        }
    }

    for _, facet := range contract.RequiredFacets {
        if !graph.Covers(facet) {
            return ErrRequiredFacetWithoutCapability
        }
    }

    for _, task := range graph.Tasks {
        if task.RequiredFacets.Reduce(contract.RequiredFacets) {
            return ErrTaskCanLowerRequirement
        }
    }

    return nil
}

func EvaluateResult(results []TaskResult, coverage Coverage) ResultCompleteness {
    if coverage.AllRequiredFacetsVerified() &&
        coverage.AllRequiredConflictsAdjudicated() &&
        AllRequiredTasksTerminal(results) {
        return Complete
    }

    if coverage.HasAnyAdmissibleEvidence() {
        return Partial
    }

    return Insufficient
}
```

### 6.3 修改前后对比

修改前：

```go
// 目标文本、Task Contract、Evidence Goal 和运行状态分别影响后续判断。
// Planner 出现矛盾时继续生成 deterministic gap cover。
if result != nil {
    Deliver(result)
}
```

修改后：

```go
plan, err := BuildCanonicalPlan(input)
if err != nil {
    return Fail("invalid_contract", err)
}

graph, err := BuildAndValidateTaskGraph(plan)
if err != nil {
    return Fail("invalid_contract", err)
}

result := ExecuteAndAdjudicate(graph)
result.Completeness = EvaluateResult(result.Tasks, result.Coverage)

// Delivery 只记录传递事实，不能把 failed 或 partial 改成 succeeded 或 complete。
Deliver(result)
return result
```

### 6.4 配置或数据库变更

当前提案阶段不直接引入数据库变更。若实施后确认需要持久化 `phase`、`coverage`、`result_completeness` 或
`delivery_status`，必须将 schema、迁移、回填和迁移测试作为独立变更提交，并遵守“schema 与 migration 同步”的仓库约定。

建议先使用已有 Run snapshot、Evidence manifest 和结构化运行事件完成影子计算；只有跨进程恢复、审计或并发协调无法从既有事实派生时，才增加持久化字段。

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当问题要求先发现业务时，实体未知阶段只执行 Discovery，不提前创建业务解释任务；
2. 当任务目标、required Facet、Capability 或 Source 不一致时，Agent 启动前返回明确的契约错误；
3. 当三个核心业务无法根据证据和选择规则确定时，系统明确返回选择不足，不伪造核心业务；
4. 当业务身份无法解析时，系统暴露 `identity_unresolved`，不通过自由字符串猜测形成事实；
5. 当某个 Agent 输出截断或预算耗尽时，已产生的 evidence 可以保留，但结果明确是 partial，不能满足缺失的 required Facet；
6. 最终回答只使用通过 coverage 和冲突裁决的 evidence；
7. `failed`、`partial`、`complete` 和 `delivered` 的语义可以从结果中直接判断，不再依赖多条日志拼接解释。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `qa.workflow.phase` | 结构化日志字段 | 记录当前阶段和阶段转换原因 |
| `qa.workflow.contract_version` | 结构化日志字段 | 确认同一 Run 使用的 canonical contract 版本 |
| `qa.workflow.invalid_contract_total` | Counter | 统计被阻止的契约矛盾，不让错误计划进入 Agent |
| `qa.workflow.identity_resolution_total` | Counter | 区分已解析、未解析和多候选身份 |
| `qa.workflow.entity_facet_coverage` | Gauge/事件 | 记录每个实体和 Facet 的覆盖状态 |
| `qa.workflow.claim_conflict_total` | Counter | 统计 Claim 级冲突及裁决结果 |
| `qa.workflow.result_completeness` | 结构化结果字段 | 记录 `complete`、`partial`、`insufficient` |
| `qa.workflow.delivery_status` | 结构化结果字段 | 记录传递状态，不覆盖执行和质量状态 |
| `qa.workflow.degraded_planner_total` | Counter | 目标是清理因契约矛盾触发的隐式降级 |

日志至少能够回答：

- 当前 Run 处于 Discovery、Selection、Identity Resolution、Investigation、Adjudication 还是 Synthesis；
- 哪个阶段决定了三个核心业务；
- 每个实体绑定了哪些 canonical service/document refs；
- 哪个 Task 负责哪个 Entity 和 Facet；
- 哪个 Claim 由哪些 EvidenceUnit 支持或冲突；
- Agent 失败时是预算、工具、身份、契约还是输出截断；
- 结果是否完整，哪些 Facet 阻止了完整回答；
- `delivered` 是否只是传递成功，还是同时满足了执行和质量条件。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 契约矛盾进入 Agent 的比例 | 待回放测量 | `0` | 每个 Run | `invalid_contract` 日志和 Task start 事件 |
| `entities=0` 时创建对象解释任务的比例 | 当前案例可复现 | `0` | 每个 Run | Route/Planner 事件 |
| 无选择依据却标记 `core_business` 的比例 | 待回放测量 | `0` | 每个 Run | Selection artifact |
| Delivered 结果缺少 completeness 的比例 | 待回放测量 | `0` | 每个 Run | Delivery/Result 记录 |
| 完整回答的 required Facet 覆盖率 | 当前不可直接计算 | `100%` | 每个完整回答 | Coverage verifier |
| 未裁决 Claim 被合成无条件事实的比例 | 待建立基线 | `0` | 每个完整回答 | Claim/Citation audit |
| 因契约矛盾触发的 planner degraded 次数 | 当前至少 1 次 | `0` | 每个 Run | Planner 日志 |
| 最终回答可追溯 Claim 比例 | 待建立基线 | `100%`（完整回答） | 每个回答 | Evidence/Citation audit |

### 7.4 不应发生的变化

- 不针对具体业务名、服务名或 trace ID 增加硬编码；
- 不通过扩大所有任务的 token、超时和重试预算来掩盖阶段和契约错误；
- 不降低已有 EvidenceUnit 的来源、归属和可追溯性要求；
- 不把 partial 结果默认为 complete；
- 不把 delivery 成功作为 workflow 成功或答案可靠的替代条件；
- 不要求简单问题经过完整的多阶段 durable workflow；
- 不在未完成历史回放和指标校验前删除现有兼容路径。

## 8. 测试与验收

### 8.1 单元测试

- Query Plan 在实体未知且 `discover_then_select=true` 时，只允许生成 Discovery 阶段计划；
- Discovery 结果没有选择依据时，不能产生 `core_business` 完整结论；
- Selection 必须保存候选、选择规则、选择结果和证据引用；
- 业务 EntityRef 不能直接作为 ServiceRef 或 DocumentRef 使用；
- 所有 required Facet 都必须映射到至少一个可执行 capability 和明确 source；
- objective 与 required Facet 矛盾时，Task Graph 校验返回 `invalid_contract`，不启动 Agent；
- Task Contract 不能降低父 Contract 的 required Facet；
- `message_push`、`iot-device`、`hsds-cookbook` 这类未规范化名称不会被工具层静默当成 canonical service；
- 有 evidence 的 Agent 输出截断结果为 partial，保留 evidence 和 failure cause；
- 没有 evidence 的 Agent 失败结果为 insufficient/failed，不能伪造 partial；
- `delivery_status=delivered` 不改变 `execution_status` 和 `result_completeness`；
- coverage 只在每个实体、每个 Facet 和每个 Claim 满足规则后允许 `complete`。

### 8.2 集成测试

- 使用 Trace `32cf05b4ff23` 的原始问题回放完整链路，验证阶段顺序、任务数量、实体选择、身份解析、证据覆盖和最终状态；
- 验证“业务发现不足”“选择依据缺失”“身份无法解析”“required Facet 缺失”“证据冲突未裁决”“Agent 输出截断”“预算耗尽”和“工具失败”分别得到稳定结果；
- 验证 Message Push、Device & IoT、Cookbook & Recipe 的展示名、内部实体 ID、服务名和文档名不会在工具调用间互相替代；
- 验证多个并行 Investigator 的 evidence 只归属产生它的 Task，并按 Entity × Facet 投影；
- 验证执行顺序、并发度、上下文大小变化不会改变 Selection 的核心业务结果或 coverage 语义；
- 验证 workflow failed 但 delivery 已发生时，父 QA 不会把结果提升为完整成功；
- 验证通过 Completeness Gate 的每个最终 Claim 都能回溯到至少一个可引用 EvidenceUnit；
- 验证简单 overview、单实体 flow、比较问题和多实体 discover-then-select 问题分别选择适当路径，不强制全部走多 Agent durable workflow。

### 8.3 回放与上线验收

1. 用当前日志对应的 Trace 建立基线，保存旧 Task Graph、任务输入、Evidence manifest、Result 状态和 Delivery 事件；
2. 以 shadow mode 运行新的阶段和 coverage 计算，比较新旧路径的实体选择、required Facet、任务数量和最终完整度；
3. 对至少一组正常问题、一组契约矛盾问题、一组身份缺失问题、一组预算/截断问题和一组证据冲突问题进行可重复回放；
4. 只有满足以下条件才允许启用新的合成 gate：
   - 契约矛盾不会进入 Agent；
   - 无选择依据不会产生完整核心业务结论；
   - 每个完整回答都有 Entity × Facet coverage；
   - partial 不会被提升为 complete；
   - delivered 不会被解释为 succeeded；
   - 所有失败原因都可以按稳定错误分类定位；
5. 灰度期间持续观察延迟、模型消耗、工具调用数量、完整回答率和拒答率；达到目标后，删除重复的隐式 fallback 和旧状态解释路径。

## 9. 实施顺序与评审决策

本提案建议按以下顺序实施，避免再次出现“先改局部，再被迫补偿”的循环：

1. **先冻结设计：** 评审 Query Plan、Investigation Contract、Task Contract、Evidence Ledger 和三维结果状态的唯一所有权；
2. **先做观测不做行为切换：** 影子计算阶段、实体身份和 Entity × Facet coverage，确认当前线上真实分布；
3. **先加执行前校验：** 阻止明显的 `entities=0 + object investigation`、required Facet 无 capability 和 objective/Facet 矛盾；
4. **再切换阶段门：** Discovery、Selection 和 Identity Resolution 通过后，才允许 Investigation；
5. **再收敛结果状态：** 将 partial recovery、workflow terminal 和 delivery 统一映射为三维状态；
6. **最后切换 Synthesis Gate：** 只允许使用已裁决、满足 Completion Policy 的 evidence bundle；
7. **清理旧逻辑：** 在回放和灰度达标后移除重复字段、隐式 fallback 和不可解释的兼容分支。

评审时需要明确回答以下问题：

- 哪个对象是 Query Plan 的唯一所有者，哪些字段允许下游派生；
- Selection 的“核心业务”标准由谁定义、如何版本化；
- Entity/Service/Repository/Document 的 canonical identity 从哪里产生；
- 哪些 Facet 是所有类似问题的必需项，哪些只在特定 QueryKind 下必需；
- partial 结果允许回答到什么程度，哪些缺口必须阻止合成；
- 是否保留当前 degraded planner，若保留，它允许处理什么类型的异常；
- 哪些现有专项提案作为本提案的实现切片，哪些重复字段和逻辑应在最终收敛时删除。

在这些问题获得明确答案前，不建议继续对单个日志症状追加局部补丁。
