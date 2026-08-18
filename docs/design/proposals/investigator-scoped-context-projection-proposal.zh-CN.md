# Investigator 按任务范围投影上下文提案

状态：草案  
作者：Nasuta Agent Platform Team  
日期：2026-08-17  
关联事项：trace `f5fa50d9beac`、多 Agent 初始上下文过大问题  
目标版本：待评审后确定

## 1. 摘要

本提案用于解决 Nasuta 多 Agent 调研工作流中“每个 Investigator 都收到同一份完整任务合同和完整 Seed Material，导致初始化上下文过大、工具预算被无关证据消耗、任务被迫提前总结”的问题。

当前，当一个问题被拆分为多个 Investigator 时，入口只序列化一次完整的 `task.contract`，再把这份输入作为工作流初始输入分发给多个节点。虽然每个节点在工作流中拥有不同的 `purpose`、`required_facets`、能力和工具，但初始输入没有按节点任务进行投影。于是，代码 Agent 也收到文档和外部依赖材料，文档 Agent 也收到大量代码材料，所有 Agent 都需要先处理与自己职责无关的内容。

在 trace `f5fa50d9beac` 中，三个 Investigator 的初始上下文分别约为 38,556、38,612 和 38,635 字符；其中一个 Agent 后续扩大到 227,794 字符，另一个 Agent 因工具调用预算耗尽而被迫总结。根因不是单纯的模型上下文窗口太小，而是“完整合同作为所有节点共享输入”的分发契约错误。

本提案计划在 Workflow 层增加确定性的 **Investigator Scoped Context Projection**：先由工作流输入保存完整任务事实，再依据每个节点的 `required_facets`、`evidence_goals`、`capability`、`source kind` 和 `trust tier` 生成独立的 Scoped Contract；初始上下文只传递任务目标、必要约束、证据摘要、引用和内容哈希，原始证据由 Agent 通过已有知识工具按需获取。投影失败时执行明确的受控降级，不静默伪造“完整”状态。

当前流程：

```text
完整 task.contract + 完整 Seed Material
→ workflow.input 序列化一次
→ 同一份输入复制给所有 Investigator
→ 各 Agent 在无关证据中检索和筛选
→ 上下文膨胀、工具预算消耗、部分任务提前总结
```

目标流程：

```text
完整 task.contract + Seed Material
→ workflow.input 校验并建立共享事实源
→ 按 Investigator 节点生成 Scoped Contract
→ 仅注入目标 Facet 的摘要、引用和哈希
→ Agent 通过能力受限工具按需获取原始证据
→ 以 covered / partial / unavailable 反映真实完成度
→ Synthesizer 汇总带来源的结果
```

预期实现以下效果：

1. 降低每个 Investigator 的初始上下文和重复内容；
2. 让每个 Agent 只处理与自身任务匹配的证据范围；
3. 保留原始证据可追溯性，并让工具按需补全证据；
4. 在无匹配证据、预算不足或工具失败时产生可诊断、可回滚的结果，而不是伪装成完整成功。

## 2. 背景

### 2.1 业务与技术背景

Nasuta 的 QA 调研流程需要回答涉及多个方面的问题，例如业务领域、核心流程、数据状态和外部依赖。为了提高并行度，系统会将一个问题拆成若干个 Investigator 任务，并为不同任务配置不同能力和工具：

- Code Investigator：查看代码、符号和调用关系；
- Runtime Investigator：追踪服务和运行链路；
- Docs Investigator：核对内部文档、运行手册和生成文档；
- Web Investigator：研究外部平台、协议和当前公开资料；
- Memory Investigator：复用历史证据或已有记忆；
- Synthesizer：汇总各 Investigator 的结构化报告。

相关链路为：

```text
QA 请求
→ Query Plan / Task Graph
→ investigation workflow
→ workflow.input
→ 多个 Investigator Agent Node 并行执行
→ Investigator 报告与证据交接
→ Synthesizer
→ QA 最终回答
```

各模块的主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| QA Planner | 解析问题、生成 Evidence Goals、分配能力和 Facet | 用户问题、查询计划 | Task Graph Proposal、Task Contract |
| `app/qaInvestigator` | 准备调查工作流、序列化任务合同、启动运行 | Task Contract、Seed Evidence | Workflow Run |
| `workflow.input` | 将入口输入转换为工作流可消费的交接数据 | 完整合同、Seed Material | 初始 Handoff |
| Investigator Agent Node | 组装 `RunRequest`，执行模型和工具循环 | Handoff、节点任务、工具策略 | Investigation Report、Evidence Units |
| Retrieval / Tool Layer | 按查询和工具权限获取代码、文档、服务、Web 证据 | Agent 查询、Facet、引用 | 有来源的证据单元 |
| Synthesizer | 汇总并验证多个 Investigator 的结果 | Reports、Evidence Units、Conflicts | 最终回答 |

### 2.2 当前实现

相关实现主要位于：

- `/Users/dequan.mac/agent-workspace/Nasuta/app/investigation.go`：在启动工作流前调用 `marshalInvestigationContract`，并将同一份输入传给工作流；
- `/Users/dequan.mac/agent-workspace/Nasuta/app/investigation_input.go`：对完整合同和 Seed Material 做整体预算控制、摘要和内容裁剪；
- `/Users/dequan.mac/agent-workspace/Nasuta/app/investigation_budget.go`：根据 Agent 定义计算 Investigator Payload Budget；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/qa/task_graph_plan.go`：生成 Task Planner、Evidence Goals 和各节点的 `required_facets`；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/workflow/agent_node.go`：把节点任务和 Handoff 组装为 `RunRequest`；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/workflow/evidence.go`：处理工作流 Handoff、引用和证据单元；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/answer_context_compaction.go`：运行时上下文达到高水位后的压缩兜底；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/tool_admission.go`、`tool_delivery.go`：工具调用和工具结果的预算控制；
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/catalog/defaults_investigation.go`：定义 Investigator 的能力、可见工具和预算。

当前执行逻辑概括如下：

1. Planner 生成完整 `TaskContract`，其中包含所有 `evidence_goals`、`investigation_goals` 和 `context.seed_material`；
2. `app/investigation.go` 根据工作流预算调用一次 `marshalInvestigationContract`；
3. `marshalInvestigationContract` 对整个 Seed Material 做统一的元数据压缩和按比例内容裁剪，但不识别具体 Investigator 节点；
4. `workflow.input` 产生包含全部 Seed Evidence 的 Handoff；
5. 每个 Agent Node 只在任务指令中看到自己的 `purpose` 和 `required_facets`，但仍继承完整 Handoff；
6. Agent 通过工具继续搜索，工具结果被追加到自己的上下文；达到运行时高水位时再尝试压缩；
7. 工具调用预算或时间预算耗尽时，Agent 被迫基于已收集材料总结。

### 2.3 为什么现在需要修改

本次修改由真实调研失败和上下文资源异常触发：

- 触发时间：2026-08-17 08:23:02～08:25:58（Asia/Shanghai）；
- 触发标识：`f5fa50d9beac`；
- 用户问题：`我们的agent控制设备和google、alexa有什么区别，这两个对设备指令的下发做了哪些兼容，链路是什么样的，差异点在哪里`；
- 直接表现：最终 QA 会话记录为 `evidence_status="unavailable"`，最终回答没有形成可验证的完整证据链；
- 影响范围：一次多 Agent comparison 调研链路，涉及代码、文档和 Web 等多个证据来源；
- 临时处置：现有运行时上下文压缩、工具预算和强制总结机制仍然生效，但只能防止单次运行无限增长，不能消除重复初始化输入。

### 2.4 范围与非目标

#### 目标

1. 在 Workflow 层为每个 Investigator 生成独立的 Scoped Contract，而不是把完整合同复制给所有节点；
2. 使 Seed Material 按 Facet、来源类型、能力和信任等级确定性过滤；
3. 初始上下文只传递必要任务信息和证据摘要，原始内容通过工具按需获取；
4. 记录投影前后大小、裁剪数量、证据覆盖率和降级原因；
5. 支持旧工作流输入兼容、灰度启用和快速回滚。

#### 非目标

1. 本提案不重新设计 Query Planner、Evidence Goal 语义或 Investigator 的业务职责；
2. 本提案不改变现有知识工具的权限边界、检索算法或外部 Web 供应商；
3. 本提案不通过无条件增大 Context Window、提高 Tool Call 上限或延长超时时间掩盖问题；
4. 本提案不把原始证据永久删除，也不以模型二次摘要替代可追溯的证据引用；
5. 本提案不针对 trace `f5fa50d9beac`、特定关键词或特定仓库名称增加硬编码规则。

## 3. 问题

### 3.1 问题描述

**期望行为：**

每个 Investigator 应只接收完成自身任务所必需的上下文。例如，Code Investigator 主要接收 `core_flow`、`data_and_state` 相关的代码摘要和引用；Web Investigator 主要接收 `business_domain`、`external_dependency` 相关的外部资料摘要。所有 Agent 可以通过受权限控制的工具获取缺失原始证据，但不应在初始化阶段继承其他 Agent 的全部证据。

**实际行为：**

入口把一个完整的 `task.contract` 和 Seed Material 作为统一工作流输入。虽然下游节点任务不同，但 `workflow.input` 的完整 Handoff 被多个 Investigator 共享。每个 Agent 初始上下文包含大量与自身 Facet 不匹配的代码、文档和 Web 材料。

**差异：**

任务分配是按 Agent 维度拆分的，输入分发却仍是全量广播，导致“逻辑上并行、数据上重复”。运行时压缩发生在 Agent 已经接收和处理大量内容之后，无法降低初始化成本，也不能保证工具预算用于目标 Facet。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 多个 Agent 的初始上下文约 38K 字符，部分 Agent 的运行中上下文继续膨胀，最终任务被迫提前总结或返回不可用结果 | trace `f5fa50d9beac` 中三条 `request compiled` 日志：`38556`、`38612`、`38635`；另有 `context size after step 4: 227794` |
| 直接原因 | 每个 Agent 都接收同一个完整 Handoff，随后工具结果继续追加到已有大上下文 | `app/investigation.go` 只调用一次 `marshalInvestigationContract`；`app/investigation_input.go` 只做整体裁剪；`agent_node.go` 将输入 Handoff 直接组装进 `RunRequest` |
| 机制根因 | 工作流输入契约没有定义“节点可见的上下文投影”，完整任务事实、节点任务指令和节点专属证据范围没有分层 | Task Graph 已经拥有 `required_facets` 和能力分配，但这些信息没有参与初始 Seed Material 的投影和分发 |

根因链路：

```text
完整 TaskContract / Seed Material
→ 入口只做一次全局预算裁剪
→ workflow.input 形成完整 Handoff
→ 所有 Investigator 共享完整 Handoff
→ Agent 先处理无关证据，再进行工具检索
→ 初始上下文重复 + 工具结果继续累积
→ 上下文膨胀、预算耗尽、证据覆盖不足
→ 最终结果可能为 partial 或 unavailable
```

本问题不能只通过增大 Context Window、提高工具调用次数或增加重试解决，因为这些措施只提高了可容纳或可执行的上限，并没有减少每个节点接收的重复输入；更大的窗口还可能让 Agent 在更多无关材料中迷失，增加延迟和成本。也不能只缩短 `purpose`，因为真正占用空间的是共享 Handoff 中的 Seed Material。

### 3.3 影响

- **用户影响：** 比较类问题无法稳定获得完整、可验证的答案；回答可能只覆盖部分 Facet，或者因证据状态不可用而无法作答；
- **业务影响：** 多 Agent 并行没有带来等比例的质量收益，调研成功率下降，用户需要重复提问或人工核对；
- **系统影响：** 相同 Seed Material 被多个 Agent 重复送入模型，增加输入 Token、模型延迟、工具调用和成本；单个 Agent 的上下文过大还会挤占输出和工具预算；
- **工程影响：** 当前压缩、预算和强制总结机制承担了本不属于它们的分发职责，问题难以从日志中快速区分为“证据不足”还是“输入广播过量”。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：比较类问题拆分为代码、文档和 Web 调研

- **Given（前置条件）：** QA Planner 生成 4 个 required evidence goals：`business_domain`、`core_flow`、`data_and_state`、`external_dependency`；同时生成代码、文档和 Web 等能力；Seed Material 中包含多个代码片段、Runbook 和历史证据；
- **When（触发行为）：** 系统将该 TaskContract 启动为多 Agent 工作流，并行运行 Investigator 节点；
- **Then（期望结果）：** 每个节点只收到与其 `required_facets` 和能力匹配的摘要、引用和哈希；需要原文时通过工具获取；Synthesizer 能追溯每条结论的来源；
- **But（当前结果）：** 三个 Investigator 初始化上下文分别约 38,556、38,612 和 38,635 字符；Docs Agent 的上下文曾达到约 227,794 字符，Code Agent 因 Tool Call budget exhausted 被迫总结。

示例输入：

```text
我们的agent控制设备和google、alexa有什么区别，这两个对设备指令的下发做了哪些兼容，链路是什么样的，差异点在哪里
```

当前执行路径：

```text
QA 请求
→ comparison Query Plan
→ 完整 TaskContract（4 个 evidence goals + 完整 Seed Material）
→ marshalInvestigationContract 一次性全局序列化
→ workflow.input 生成完整 Handoff
→ Code / Docs / Web Investigator 分别接收同一份 Handoff
→ 各自继续检索并追加工具结果
→ 上下文增长、工具预算耗尽或结果状态 unavailable
```

关键证据：

```text
2026-08-17T08:23:10+08:00 ... request compiled ... contextChars=38556
2026-08-17T08:23:10+08:00 ... request compiled ... contextChars=38612
2026-08-17T08:23:10+08:00 ... request compiled ... contextChars=38635
2026-08-17T08:23:49+08:00 ... context size after step 4: 227794 chars
2026-08-17T08:24:30+08:00 ... tool-call budget exhausted; forcing conclusion with collected evidence
```

#### 场景 B：某个 Facet 没有匹配的 Seed Material

- **Given：** 节点被分配 `external_dependency`，但预检索 Seed Material 只有内部代码证据，或者所有相关证据都低于允许的 Trust Tier；
- **When：** Projection Node 为该节点生成 Scoped Contract；
- **Then：** 节点收到明确的 `seed_status=empty`、待完成的 Evidence Goals 和可使用的工具范围，并尝试按需检索；
- **But：** 若把完整合同原样传入，Agent 会把无关内部材料误认为外部依赖证据，或者在大上下文中浪费工具调用。

#### 场景 C：投影或工具获取失败

- **Given：** Seed Material 结构非法、投影规则版本不兼容，或 Code/Web 工具在执行期间超时；
- **When：** 节点准备或运行阶段遇到错误；
- **Then：** 记录明确失败原因，并将该 Facet 标记为 `unavailable` 或 `partial`，保留其他节点的有效结果；
- **But：** 当前只能依赖通用运行时错误和强制总结，无法区分是输入过大、证据不匹配还是下游工具失败。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常路径 | 每个 Facet 都有可用 Seed 和工具 | 所有节点接收全量 Seed | 每个节点接收专属摘要和引用，按需取原文 |
| 空输入或缺失字段 | 没有 Seed Material 或没有 required facets | 仍可能创建大合同，或由 Agent自行判断范围 | 生成最小合同；缺失任务字段在入口明确失败 |
| 超时或预算耗尽 | 工具调用达到节点上限 | 强制总结，完成度容易与上下文状态混淆 | 保留已验证证据，明确 `partial` 和 `budget_exhausted` |
| 下游失败 | 检索、Web 或代码工具失败 | Agent 继续尝试无关工具或返回泛化结论 | 只在允许能力内重试；失败原因进入状态和指标 |
| 重试或重复请求 | 同一 Workflow Run 重试 | 可能重复注入同一 Seed 内容 | 以 `projection_hash`、`content_hash` 去重，重试保持幂等 |
| 并发或乱序 | 多个 Investigator 同时完成 | 依赖共享全量输入，难以判断来源边界 | 每个节点独立 Handoff，按 Producer Node ID 合并 |
| 兼容旧数据或旧客户端 | 旧工作流输入没有 projection 字段 | 新代码无法理解或直接全量广播 | 兼容旧格式并进入 legacy 模式，记录告警，可开关回退 |

### 4.3 复现步骤

1. 使用 2026-08-17 的本地日志和 trace `f5fa50d9beac` 作为基线；
2. 提交与场景 A 相同的比较问题，并使 Planner 生成多个 Investigator 任务；
3. 在 `workflow.input` 中放入包含代码片段、Runbook、历史证据和外部资料的 Seed Material；
4. 观察 `internal/agent/execution/loop_execution.go` 的 `request compiled` 日志，记录每个 Agent 的 `contextChars`；
5. 观察 `loop_turn.go` 的 `context size after step N`、工具调用计数和预算状态；
6. 在旧实现下，应能看到三个 Agent 的初始上下文接近，且至少一个 Agent 在多轮工具调用后显著膨胀；
7. 切换到投影实现后，比较初始 Token、重复率、Facet 覆盖率和最终报告质量。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 投影规则只依赖节点任务契约和证据元数据，不依赖 trace ID、问题关键词或特定仓库名称。
2. **保持单一事实源。** 完整 `TaskContract` 由 workflow input 作为事实源保存；Scoped Contract 是针对节点的只读视图，不复制或修改事实源。
3. **明确职责边界。** Planner 负责分配 Facet 和能力；Projection 负责裁剪可见上下文；Agent 负责按需获取证据和形成报告；Synthesizer 负责跨节点汇总与一致性校验。
4. **失败可诊断。** 每次投影必须记录规则版本、输入/输出 Token、选中与裁剪数量、缺失 Facet、降级原因和 `projection_hash`。
5. **兼容与可回滚。** 以 feature flag 灰度启用；旧输入可以进入 legacy 全量模式；投影失败不允许静默标记为 complete。
6. **确定性优先。** Facet、Capability、Source Kind 和 Trust Tier 的过滤由代码完成；模型摘要只能压缩展示，不拥有证据选择的最终解释权。

### 5.2 目标流程

```text
完整 TaskContract + Seed Material
→ Contract 校验与规范化
→ workflow.input 保存完整事实源和 Seed 索引
→ Projection Node 读取节点 Task Directive
→ Facet / Goal / Capability / Source Kind / Trust Tier 过滤
→ 摘要 + Reference + Content Hash 生成 Scoped Handoff
→ Investigator 运行并按需调用可见工具
→ 结果带 Evidence Units / Conflicts / Completeness 回传
→ Synthesizer 合并、校验和输出
```

与当前流程相比，关键变化是：

1. 在 `workflow.input` 与 Investigator Node 之间新增按节点投影机制，用于把共享事实源转换为最小可用上下文；
2. 将“所有节点共享完整 Seed”的职责从入口序列化逻辑移动到 Workflow Projection；
3. 将 `required_facets`、`evidence_goals` 和 `capability` 从仅用于任务描述，提升为上下文可见性策略的显式输入；
4. 在无匹配 Seed、投影预算不足和工具失败边界增加 `empty`、`partial`、`unavailable` 等明确状态；
5. 在日志和运行记录中公开投影比例、重复率、覆盖率、预算消耗和降级原因。

推荐架构：

```text
workflow.input（完整事实源）
├── scope.code    → investigator.code
├── scope.runtime → investigator.runtime
├── scope.docs    → investigator.docs
├── scope.web     → investigator.web
└── scope.memory  → investigator.memory
```

如果当前工作流引擎不适合新增独立节点，可以在 `agent_node.go` 组装 `RunRequest` 前调用同一个纯函数 Projection Service；但投影职责和数据契约必须保持独立，不能重新退化为入口全量序列化。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 节点输入生成 | `app/investigation.go` 只生成一份完整合同 | 生成共享事实源，并由每个节点生成 Scoped Contract | `app/investigation.go`、`internal/agent/workflow` | 旧开关关闭时保留 legacy 全量输入 |
| Seed 裁剪 | `investigation_input.go` 对全部 Seed 做整体按比例裁剪 | 按 Facet、Goal、Capability、Source Kind、Trust Tier 过滤后再预算裁剪 | `app/investigation_input.go` 或新的 `internal/agent/investigation` | 旧字段缺失时使用 conservative legacy projection |
| Agent 任务约束 | Task Directive 和实际可见证据范围分离 | `required_facets` 同时约束任务和上下文投影 | `internal/agent/qa/task_graph_plan.go`、workflow | 不改变已有 Planner 输出字段 |
| 原始证据获取 | 初始输入包含较多原始内容 | 初始只放摘要/引用/哈希，原文由工具按需取得 | `internal/agent/tools`、`internal/retrieval` | 工具接口保持向后兼容 |
| 完成度 | 主要依赖 Agent 报告和通用运行时状态 | 增加 projection completeness 与 evidence availability | `internal/agent/workflow/evidence.go`、运行记录 | 旧结果映射为 `unknown` |
| 观测 | 只能看到总上下文和工具预算 | 记录投影前后大小、选中/裁剪数、重复率和失败原因 | `internal/agent/execution`、run events | 新字段可选，不阻塞旧消费者 |

#### 改动一：建立节点级 Scoped Contract

**方案：**

在工作流输入中保留完整任务事实，但不再把完整 `context.seed_material` 直接交给每个 Agent。根据当前节点的 Task Directive 生成 `ScopedContract`：

- `task_id`、`objective`、实体和通用安全约束保留；
- `evidence_goals` 只保留该节点负责的 Goal；
- `investigation_goals` 只保留与节点 Facet 有交集的目标；
- `required_facets` 作为可见证据的硬过滤条件；
- `seed_material` 只保留匹配来源的 `summary`、`references`、`content_hash`、`source_kind`、`trust_tier` 和截断后的必要片段；
- 通过 `tool_policy` 声明该节点可使用的工具和最大按需获取预算；
- 通过 `projection` 记录规则版本、输入输出大小和缺失 Facet。

**约束：**

- Scoped Contract 必须是完整合同的只读投影，不得新增原合同不存在的事实；
- 任何证据引用必须保留稳定 ID、来源和内容哈希；
- Projection 不得扩大 Agent 原有工具权限；
- 投影后的证据数量和字符数必须受节点独立预算约束；
- 同一输入、同一节点任务和同一规则版本必须生成相同的 `projection_hash`。

**失败行为：**

- 节点任务缺少 `required_facets` 时，按显式安全策略处理：若是旧格式则进入 legacy 模式并告警；若是新格式则返回 `invalid_projection_scope`；
- 找不到匹配 Seed 时生成 `seed_status=empty`，不把无关证据伪装成匹配证据；
- 投影结果仍超过预算时，按优先级裁剪原始片段，保留摘要、引用和哈希；若最小合同仍超限，则节点以 `projection_failed` 终止或受控降级；
- 投影过程中不得吞掉解析错误或把失败结果标记为 `complete`。

#### 改动二：按证据元数据执行确定性过滤

**方案：**

为 Seed Material 建立统一元数据判定：

1. `Facet` 与节点 `required_facets` 相交；
2. `EvidenceGoal` 与节点负责的 Goal 相交；
3. `Capability` 与来源类型匹配，例如 Code Investigator 优先代码和调用链，Docs Investigator 优先文档，Web Investigator 优先 Web 来源；
4. `Source Kind` 和 `Trust Tier` 满足节点策略；
5. 同一 `content_hash` 只保留一个展示单元，避免跨引用重复注入；
6. 对匹配证据按“高风险/必需 Goal、摘要、引用、最短必要片段”的顺序分配预算。

**约束与失败行为：**

- 过滤不能改变证据的原始来源和可信等级；
- 不能因为摘要为空而删除引用和哈希；
- 无法判定 Facet 的证据只能进入显式的 `unclassified` 区域，并默认不注入 Investigator；
- Source Kind 不匹配时不得由模型自行猜测为匹配来源；
- 若节点负责多个 Facet，必须保证每个 required Facet 都得到最低预算或明确标记缺失。

#### 改动三：原始证据改为按需获取

**方案：**

初始 Scoped Contract 只传递足以帮助 Agent 规划检索的证据摘要和引用。Agent 需要原文时，使用已有的知识工具，并在工具请求中携带 `task_id`、`node_id`、`required_facets` 和引用 ID。工具层继续执行权限、预算、来源和结果大小控制。

工具结果应返回结构化 Evidence Unit，而不是把任意大段文本无界追加到上下文。结果至少包含：证据 ID、来源、Facet、摘要、内容哈希、可选截断内容、是否完整、截断原因和获取状态。

**约束与失败行为：**

- 工具只能访问节点允许的能力和来源；
- 单次工具结果和节点累计工具结果均必须有上限；
- 工具超时或下游不可用时，仅对可重试错误执行有限重试；
- 重试仍失败时，回传 `evidence_unavailable`，并保留已收集的有效证据；
- Agent 不得使用没有引用或哈希的工具结果形成“已验证”结论。

#### 改动四：统一任务目的和 Facet 分配

**方案：**

Planner 生成任务时，对 `purpose`、`required_facets`、`evidence_goal_ids` 和 `capability` 做一致性校验。若一个任务的 purpose 描述外部依赖，但 required Facets 只有其他领域，必须在编译阶段修正或拒绝，而不是让 Projection 和 Agent 自行猜测。

**失败行为：**

- Facet 与 Goal 不一致时返回 `task_scope_mismatch`；
- 重复分配同一个 Goal 时保留显式 owner 或标记为共享 Goal；
- 无法确定 owner 时不宣称完整覆盖，由 Synthesizer 将其标记为 unresolved。

### 5.4 数据结构或接口契约

建议新增或扩展以下内部结构。字段名可在实现阶段根据现有命名规范调整，但语义必须保持一致：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `projection_version` | string | Workflow Projection | 投影规则版本 | `"v1"` | 新字段可选 |
| `node_id` | string | Workflow | 当前目标节点 | 空 | 新格式必填 |
| `required_facets` | `[]string` | Planner / Task Directive | 节点负责的证据 Facet | 空 | 旧格式缺失时 legacy |
| `evidence_goal_ids` | `[]string` | Planner | 节点负责的 Goal ID | 空 | 旧格式可由 Facet 推导，但需告警 |
| `seed_status` | enum | Projection | `matched` / `empty` / `partial` / `failed` | `empty` | 旧 Handoff 映射为 `unknown` |
| `seed_material` | `[]ScopedSeed` | Projection | 节点专属摘要、引用和哈希 | 空数组 | 不破坏原 Seed 结构 |
| `tool_policy` | object | Workflow / Catalog | 节点可见工具和按需预算 | 继承节点定义 | 不扩大权限 |
| `input_tokens` | int | Projection | 投影前估算输入 Token | 0 | 观测字段 |
| `projected_tokens` | int | Projection | 投影后输入 Token | 0 | 观测字段 |
| `dropped_seed_count` | int | Projection | 被 Facet 或预算裁剪的 Seed 数 | 0 | 观测字段 |
| `projection_hash` | string | Projection | 输入范围和规则的稳定哈希 | 空 | 用于幂等和诊断 |
| `degradation_reason` | string | Workflow | 降级或失败原因 | 空 | 旧消费者可忽略 |

建议的 Scoped Seed：

```json
{
  "id": "evidence-123",
  "source_kind": "code",
  "trust_tier": "verified",
  "facets": ["core_flow", "data_and_state"],
  "goal_ids": ["command_delivery_chain_comparison"],
  "summary": "设备控制入口将标准化意图转换为设备指令，并通过服务链路下发。",
  "references": ["repos/example/.../AgentFunctionImpl.java:L40-L118"],
  "content_hash": "sha256:...",
  "content": "",
  "content_complete": false
}
```

状态转换：

```text
[unprojected]
  ├─ 校验通过且有匹配 Seed → [matched]
  ├─ 校验通过但无匹配 Seed → [empty]
  ├─ 有部分 Facet 或 Goal 可用 → [partial]
  ├─ 预算不足但保留最小合同 → [partial]
  └─ 结构、规则或权限错误 → [failed]

[matched/empty/partial]
  ├─ 工具补证成功 → [complete 或 partial]
  ├─ 工具不可用 → [unavailable 或 partial]
  └─ 节点预算耗尽 → [partial]
```

不变量：

1. `scoped_seed_material` 中的每个证据都必须满足节点的 Facet 和来源策略，或明确标记为 `unclassified`；
2. `projection_hash` 相同意味着输入合同、节点范围和投影规则版本相同；
3. `complete` 只能表示 required Goals 已被报告明确分类且有可追溯证据，不能仅表示 Agent 正常退出；
4. 投影不会扩大 `VisibleToolIDs` 或节点权限；
5. 被裁剪的原始内容仍可通过 `content_hash` 和引用定位，不得伪造摘要为原文。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 保留现有完整 `TaskContract` 的解析能力；旧工作流输入没有投影字段时进入 `legacy_full_context` 模式，并记录告警和上下文大小；新节点优先读取 Scoped Contract。
- **数据迁移：** 不需要迁移历史证据数据；只需在运行记录或 Handoff 中增加可选投影元数据。历史运行回放时按旧格式解释。
- **灰度方式：** 增加 `investigator_scoped_projection` 开关，支持按环境、租户或百分比启用；灰度阶段同时记录 legacy 与 projection 的大小、覆盖率和质量对比。
- **回滚条件：** 若关键 Facet 覆盖率下降超过 5 个百分点、原触发场景成功率下降、工具错误率上升超过 2 个百分点，或 P95 延迟/成本增加超过 20%，关闭开关回到 legacy 模式。
- **回滚步骤：** 关闭投影开关；停止创建新的 Projection Handoff；保留原完整输入路径；检查是否存在被错误标记为 complete 的结果；不删除证据和运行记录。
- **规则升级：** `projection_version` 向前兼容；新规则先以 Shadow 方式计算但不改变 Agent 输入，确认覆盖率和大小指标后再生效。

## 6. 修改伪代码

### 6.1 核心流程

```go
func StartInvestigation(ctx context.Context, request InvestigationRequest) error {
    normalized, err := NormalizeAndValidateContract(request.Contract)
    if err != nil {
        RecordProjectionFailure(ctx, request.WorkflowRunID, "invalid_contract", err)
        return fmt.Errorf("validate investigation contract: %w", err)
    }

    budget, err := LoadInvestigatorBudgets(ctx, normalized)
    if err != nil {
        RecordProjectionFailure(ctx, request.WorkflowRunID, "budget_unavailable", err)
        return err
    }

    // workflow.input 保存完整事实源，但不把全部 Seed 作为每个节点的直接输入。
    inputHandoff := BuildFullFactSourceHandoff(normalized, request.SeedEvidence)
    if err := StartWorkflowInput(ctx, request.WorkflowRunID, inputHandoff); err != nil {
        return fmt.Errorf("start workflow input: %w", err)
    }

    for _, node := range InvestigatorNodes(normalized) {
        scoped, err := ProjectInvestigatorContext(
            normalized,
            request.SeedEvidence,
            node.Task,
            node.Capability,
            budget.PayloadTokens(node.ID),
        )
        if err != nil {
            if IsLegacyCompatibleInput(request.Contract) {
                scoped = BuildLegacyScopedContext(normalized, node)
                scoped.DegradationReason = "projection_failed_legacy_fallback"
            } else {
                return fmt.Errorf("project context for %s: %w", node.ID, err)
            }
        }

        if err := StartInvestigatorNode(ctx, node, scoped); err != nil {
            MarkNodeUnavailable(ctx, node.ID, err)
            // 不影响其他独立 Investigator；由 Synthesizer 处理缺失 Facet。
        }
    }
    return nil
}
```

### 6.2 投影与预算伪代码

```go
func ProjectInvestigatorContext(
    contract TaskContract,
    seeds []SeedMaterial,
    task TaskDirective,
    capability Capability,
    maxTokens int,
) (ScopedContract, error) {
    if task.NodeID == "" || len(task.RequiredFacets) == 0 {
        return ScopedContract{}, ErrInvalidProjectionScope
    }
    if maxTokens <= 0 {
        return ScopedContract{}, ErrProjectionBudget
    }

    goals := SelectGoals(contract.EvidenceGoals, task.EvidenceGoalIDs, task.RequiredFacets)
    if len(goals) == 0 {
        return ScopedContract{}, ErrTaskScopeMismatch
    }

    matched := make([]ScopedSeed, 0)
    dropped := 0
    seen := map[string]struct{}{}
    for _, seed := range seeds {
        if !FacetIntersects(seed.Facets, task.RequiredFacets) ||
           !CapabilityAllows(capability, seed.SourceKind) ||
           !TrustTierAllows(task, seed.TrustTier) {
            dropped++
            continue
        }
        if _, duplicate := seen[seed.ContentHash]; duplicate {
            dropped++
            continue
        }
        seen[seed.ContentHash] = struct{}{}
        matched = append(matched, SummarizeAsReference(seed))
    }

    scoped := ScopedContract{
        TaskID: contract.TaskID,
        NodeID: task.NodeID,
        Objective: BoundedSummary(contract.Objective),
        InvestigationGoals: SelectInvestigationGoals(contract, task),
        EvidenceGoals: goals,
        RequiredFacets: append([]string(nil), task.RequiredFacets...),
        SeedMaterial: matched,
        ToolPolicy: BuildToolPolicy(capability, task),
        Projection: ProjectionMeta{
            Version: "v1",
            InputTokens: EstimateTokens(contract, seeds),
            DroppedSeedCount: dropped,
        },
    }

    scoped = FitToBudget(scoped, maxTokens)
    if EstimateTokens(scoped) > maxTokens {
        return ScopedContract{}, ErrProjectionBudget
    }
    scoped.SeedStatus = SeedStatusFor(scoped, task.RequiredFacets)
    scoped.Projection.Hash = StableHash(contract.TaskID, task, scoped.SeedMaterial)
    return scoped, nil
}
```

### 6.3 Agent 按需取证与失败处理

```go
func RunInvestigator(ctx context.Context, scoped ScopedContract) (Report, error) {
    report := NewReportFor(scoped.RequiredFacets)
    report.Metadata.ProjectionHash = scoped.Projection.Hash
    report.Metadata.SeedStatus = scoped.SeedStatus

    for _, goal := range scoped.EvidenceGoals {
        if !HasUsableSeed(scoped, goal.ID) {
            result, err := AcquireEvidenceOnDemand(ctx, EvidenceRequest{
                TaskID: scoped.TaskID,
                NodeID: scoped.NodeID,
                GoalID: goal.ID,
                Facets: scoped.RequiredFacets,
                ToolPolicy: scoped.ToolPolicy,
            })
            if err != nil {
                if IsRetryable(err) {
                    result, err = RetryBounded(ctx, result, err)
                }
            }
            if err != nil {
                report.MarkUnresolved(goal.ID, "evidence_unavailable", err)
                continue
            }
            report.AddEvidence(result)
        }
    }

    if ToolBudgetExhausted(ctx) || ContextNearHighWater(ctx) {
        report.MarkUnresolvedRemaining("budget_exhausted")
        report.Status = Partial
    }
    if report.HasVerifiedEvidence() {
        report.Status = FinalizeCompleteness(report, scoped.RequiredFacets)
    } else if report.AllGoalsUnresolved() {
        report.Status = Unavailable
    }
    return report, nil
}
```

### 6.4 结果合并与可观测性伪代码

```go
func MergeInvestigationResults(results []NodeResult) (FinalEvidence, error) {
    merged := NewEvidenceAccumulator()
    for _, result := range results {
        merged.Add(result.Handoff.EvidenceUnits, result.Handoff.EvidenceConflicts)
        RecordMetric("investigator_projected_tokens", result.Usage.ProjectedTokens)
        RecordMetric("investigator_dropped_seed_count", result.Usage.DroppedSeedCount)
        RecordMetric("investigator_duplicate_ratio", result.Usage.DuplicateRatio)
    }

    final, err := VerifyAndFinalize(merged)
    if err != nil {
        RecordFailure(context.Background(), "synthesis_verification_failed", err)
        return FinalEvidence{}, err
    }
    return final, nil
}
```

### 6.5 配置或数据库变更

```yaml
investigation:
  scoped_context_projection:
    enabled: false
    rollout_percent: 0
    projection_version: v1
    max_seed_summary_tokens: 1024
    max_seed_content_tokens: 0
    allow_legacy_fallback: true
    duplicate_hash_dedupe: true
```

本提案不要求数据库迁移。若运行记录需要持久化新指标，可增加可选字段或使用现有事件扩展机制；不得以破坏旧运行记录读取为代价。

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 当节点具有明确的 Facet 和 Goal 分配时，Agent 初始只收到自己的 Scoped Contract；
2. 当 Seed Material 没有匹配项时，Agent 能明确知道“当前没有已注入证据”，并通过允许的工具补证；
3. 当工具失败、上下文接近高水位或 Tool Call budget 耗尽时，报告被标记为 `partial` 或 `unavailable`，不会被误判为完整成功；
4. Synthesizer 能按 Goal、Facet、Producer Node、引用和哈希追溯结论；
5. 代码、文档、运行时、Web 和记忆 Agent 的输入范围与其职责一致。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `investigator_input_tokens_before_projection` | Histogram | 衡量共享完整输入大小 |
| `investigator_projected_input_tokens` | Histogram | 衡量每节点实际初始化输入 |
| `investigator_projection_ratio` | Histogram | 衡量投影压缩比例 |
| `investigator_seed_dropped_total` | Counter | 统计 Facet 不匹配和预算裁剪 |
| `investigator_seed_duplicate_ratio` | Histogram | 统计重复内容比例 |
| `investigator_facet_coverage` | Gauge / Histogram | 衡量节点和最终结果的 Facet 覆盖率 |
| `investigator_projection_failures_total` | Counter | 统计投影失败和降级原因 |
| `investigator_degradation_reason` | 结构化日志字段 | 区分 empty、budget、tool failure 和 legacy fallback |
| `projection_hash` | 运行事件字段 | 支持幂等、审计和复现 |

日志应至少能够回答：

- 每个节点被分配了哪些 Facet、Goal 和 Capability；
- 投影前后分别有多少 Token、Seed 和引用；
- 哪些 Seed 因 Facet、来源、信任等级、重复或预算被裁剪；
- Agent 是否通过工具获取了原始证据，以及是否失败或重试；
- 最终每个 Goal 是 covered、partial、unavailable 还是 unresolved；
- 最终结论可以追溯到哪些 Evidence Unit 和内容哈希。

### 7.3 量化指标

以下以 trace `f5fa50d9beac` 的观测作为初始基线，正式目标应在灰度前用同类请求样本重新统计：

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| Investigator 初始上下文字符数 | 38,556～38,635 chars | P95 ≤ 12,000 chars 等价 Token 预算，具体以模型 Token 估算为准 | 同类 comparison 请求，7 天 | `request compiled` 日志 |
| 初始输入重复率 | 接近全量广播 | ≤ 20%（仅公共元数据重复） | 7 天 | `content_hash` 集合比较 |
| 单节点投影失败率 | 未单独统计 | < 0.5% | 7 天 | Projection counter |
| required Facet 覆盖率 | 原案例为 unavailable | ≥ 95% 的可用 Goal 被正确分类 | 评测集 + 线上 7 天 | Report / Evidence Unit |
| Tool Call budget 提前耗尽率 | 原案例至少 1 个节点触发 | 下降 ≥ 50% | 同类请求，7 天 | Agent run events |
| Investigator 输入 Token 成本 | 全量输入 | 下降 ≥ 40% | 7 天 | LLM usage |
| P95 调研延迟 | 以灰度前基线为准 | 不高于基线 10% | 7 天 | Workflow metrics |
| 最终回答证据可追溯率 | 原案例无完整可用证据 | ≥ 98% 的事实性结论有引用或 Evidence Unit | 评测集 | Synthesizer verifier |

### 7.4 不应发生的变化

- 不改变 Planner 对正常问题的拆分语义和 Agent 工具权限；
- 不因为投影而丢失可通过稳定引用重新获取的原始证据；
- 不把 `empty`、`partial` 或 `unavailable` 错报为 `complete`；
- 不以更高的全局 Context Window、Tool Call 上限或超时掩盖输入广播问题；
- 不引入针对某个 trace、问题文本、Agent 名称或仓库路径的硬编码；
- 不降低现有知识读取的权限和审计能力。

## 8. 测试与验收

### 8.1 单元测试

- 给定完整合同和多个 Seed，Code / Docs / Web 节点生成的 Seed 集合互不包含明显不匹配的来源；
- `required_facets`、`evidence_goal_ids`、Capability 和 Source Kind 过滤结果符合确定性规则；
- 相同输入、节点和规则版本生成相同 `projection_hash`；
- 重复内容哈希只生成一个 Scoped Seed；
- 无匹配 Seed 返回 `seed_status=empty`，不返回 `complete`；
- 投影预算不足时优先保留摘要、引用、哈希，并返回 `partial` 或明确错误；
- 非法 Contract、Facet 与 Goal 不一致、非法 Trust Tier 被拒绝；
- 工具超时、非重试错误和 Tool Call budget 耗尽分别产生正确的 `unavailable` / `partial` 状态；
- 旧格式输入可进入 legacy fallback，并记录告警。

### 8.2 集成测试

- 验证从 QA 请求、Task Graph、workflow.input、Projection、Investigator 到 Synthesizer 的完整链路；
- 验证多个 Investigator 并发时每个节点只读取自己的 Scoped Handoff；
- 验证按需工具请求携带 `task_id`、`node_id`、Goal 和 Facet，并受到原有权限和预算限制；
- 验证工具返回的 Evidence Unit 能被报告和 Synthesizer 正确引用；
- 验证投影失败不影响独立节点，并由 Synthesizer 正确处理缺失 Facet；
- 验证旧工作流输入、新投影输入、重复运行、乱序完成和超时行为；
- 验证日志、指标、运行事件和最终 Completeness 一致。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | 2026-08-17 trace `f5fa50d9beac` 对应比较问题 | 三个 Investigator 初始输入显著缩小，最终能区分 covered / partial / unavailable | 固定回放 + 评测答案与证据检查 |
| 正常路径 | 仅有一个 Facet 的简单问题 | 仍能快速完成，不额外引入复杂交互 | 集成测试 + 延迟检查 |
| 大 Seed 输入 | 代码、文档、Web 混合且内容重复 | 每节点按范围投影，初始输入不超过节点预算 | 属性测试 + Token 断言 |
| 空匹配 | Web 节点没有 Web Seed | 标记 empty，使用 Web 工具按需取证；工具失败则 unavailable | 故障注入 |
| 预算耗尽 | 工具调用达到上限 | 保留已验证证据并标记 partial，不伪造完整 | 单元 + 集成测试 |
| 旧格式回放 | 没有 projection 元数据的旧 Handoff | legacy 模式可运行并产生告警 | 兼容性回放 |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. 对任意具备明确 `required_facets` 的 Investigator，实际 `RunRequest` 只包含其 Scoped Contract 和允许的公共元数据；
2. 同类多 Agent 请求的初始输入 P95 不超过 12,000 Token，且较旧实现的输入 Token 成本下降至少 40%；
3. 投影、工具失败和预算耗尽均能产生可区分的结构化状态和日志；
4. 至少 95% 的可用 Evidence Goals 被正确覆盖或明确标记为 unresolved / unavailable；
5. 事实性结论的证据可追溯率达到 98% 以上；
6. 旧格式工作流可以运行，且关闭 feature flag 后可回到旧行为；
7. trace `f5fa50d9beac` 回放不再因共享全量上下文导致初始输入广播和无关证据膨胀；
8. 单元、集成、故障注入和回归测试全部通过，且不引入权限扩大或数据丢失。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 过滤过严导致有效证据被裁剪 | Facet 或 Source Kind 元数据不完整 | 回答覆盖率下降 | 保留引用和哈希；Shadow 对比；支持节点级白名单和人工审计 | 覆盖率下降超过 5 个百分点 |
| 摘要不足导致 Agent 不知道如何检索 | Seed 摘要质量低或引用不稳定 | 工具调用增加、延迟上升 | 摘要只做导航；工具支持按引用取原文；监控按 Goal 的补证率 | P95 延迟增加超过 20% |
| Projection 逻辑与 Planner 分配不一致 | Task Directive 缺少 Goal 或 Facet | 节点失败或证据重复 | 编译期校验；使用 `task_scope_mismatch`；不允许静默推断 | 任务失败率超过基线 |
| 旧工作流兼容不完整 | 旧 Handoff 无投影字段 | 回放或重试失败 | 保留 legacy parser 和 feature flag；兼容测试 | 任一核心旧客户端不可运行 |
| 投影自身增加延迟 | Seed 数量过大或重复哈希计算耗时 | 调研 P95 延迟上升 | 先做元数据过滤；限制单次 Seed 数；缓存稳定 hash | P95 超过基线 10% |
| Agent 仍无界追加工具结果 | 工具层只限制单次结果 | 上下文再次膨胀 | 同时设置单次、累计和高水位预算；结果摘要化 | 上下文 P95 重新超过目标 |
| 状态误报为 complete | 只依据 Agent 正常退出 | 用户获得不可信结论 | Synthesizer 验证 Goal 分类、Evidence Unit 和哈希 | 发现任何无证据 complete |

## 10. 实施计划

### 阶段 1：最小安全改动

- 定义 `ScopedContract`、`ScopedSeed`、Projection Metadata 和状态枚举；
- 增加纯函数过滤、去重、预算裁剪和稳定哈希的单元测试；
- 增加投影前后大小、Facet、Goal 和降级原因的观测字段；
- 退出条件：不改变 Agent 行为的 Shadow Projection 能稳定生成结果，且测试覆盖正常、空匹配和预算不足场景。

### 阶段 2：核心机制修改

- 在 Workflow 层接入 Projection Service；
- 让 `workflow.input` 保存完整事实源，让 Investigator Node 消费 Scoped Handoff；
- 将按需取证请求与节点身份、Goal 和 Facet 绑定；
- 保持现有工具权限、上下文压缩和 Tool Call budget 作为兜底；
- 退出条件：端到端测试显示节点输入范围正确，旧格式仍可运行。

### 阶段 3：灰度与观测

- 对代表性问题集执行 legacy / projection Shadow 对比；
- 小比例启用 feature flag，观察输入 Token、覆盖率、延迟、成本、工具错误率和结果质量；
- 回放 trace `f5fa50d9beac`，确认不再出现全量 Seed 广播和无关证据重复；
- 退出条件：量化指标达到第 7.3 节目标，且无高优先级数据完整性或权限问题。

### 阶段 4：全面启用与清理

- 将投影作为默认路径，保留 legacy fallback 一段观察期；
- 完善运行记录、运维面板和设计文档；
- 在确认所有旧版本工作流都完成迁移后，删除不再需要的全量广播路径；
- 退出条件：连续一个发布周期无回滚条件触发，且所有验收标准满足。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| Projection 放置位置 | `app/investigation.go` 入口提前为每个节点生成输入 | Workflow 层在 `workflow.input` 与 Agent Node 之间投影 | B | 与节点职责和运行时 Handoff 边界一致，避免入口知道所有节点细节 |
| 原始证据传递方式 | 初始输入继续放截断原文 | 初始放摘要/引用/哈希，按需工具取原文 | B | 最大限度降低初始化上下文，同时保留可追溯性 |
| 过滤决策方式 | 让模型根据 purpose 自行挑选 | 代码按 Facet / Goal / Capability / Source Kind 确定性过滤 | B | 可测试、可观测、可复现，避免模型误判证据范围 |
| 无匹配 Seed 的处理 | 将其他来源材料作为近似上下文 | 显式 `empty`，由允许工具补证 | B | 不把无关证据伪装成目标证据，状态更可信 |
| 旧格式兼容 | 直接拒绝旧 Handoff | legacy full-context fallback + 告警 | B | 降低发布风险，同时推动迁移 |
| 初始上下文上限 | 统一一个非常大的全局上限 | 节点独立预算，Web 4K～6K、Docs 8K～10K、Code 8K～12K tokens | B | 不同节点职责和证据密度不同，避免一个节点挤占其他节点资源 |

## 12. 决策摘要

本提案建议：

1. 在 Workflow 层建立节点级 Context Projection，完整 TaskContract 只作为共享事实源，不作为所有 Agent 的直接输入；
2. 根据 `required_facets`、`evidence_goal_ids`、Capability、Source Kind 和 Trust Tier 确定性过滤 Seed Material，并按 `content_hash` 去重；
3. 初始上下文只传摘要、引用和哈希，原始证据通过能力受限工具按需获取；
4. 为投影、证据获取和最终报告分别记录 `matched`、`empty`、`partial`、`unavailable` 和 `failed` 状态；
5. 以 Shadow、灰度、量化指标和 feature flag 验证效果；当覆盖率、延迟、成本或错误率达到回滚阈值时恢复 legacy 模式。

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
