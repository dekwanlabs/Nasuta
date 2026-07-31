# 证据规划、工具与运行时调查

[English](02-evidence-and-tooling.md) | [中文](02-evidence-and-tooling.zh-CN.md)

> 状态：主体已实现，精确答案合同与运行时调查质量正在补强
> 来源：Evidence Planning、Runtime Evidence、Runtime Investigation、Web Evidence、Required Evidence Incident、Tool Selection and Multi-turn Evidence

## 1. 核心模型

证据规划回答“本轮允许从哪里取证”，工具 Registry 回答“模型实际能调用什么”，二者不能合并。

```text
Question
  -> EvidencePlan      // sources and preferences
  -> Registry Snapshot // available tools
  -> Model decision    // whether and how to call
  -> Execution guard   // permission and argument validation
  -> Evidence ledger  // success, failure, partial, omitted
```

## 2. EvidencePlan

`EvidencePlan.Sources` 是能力位集合：

| 来源 | 权威范围 |
|---|---|
| Memory | 稳定用户偏好、身份和已确认长期事实 |
| Internal | 代码、文档、服务、API、本体和调用链 |
| Runtime | 日志、配置、Trace、告警和实时系统状态 |
| Web | 外部网页、当前公开资料和官方来源 |

规划器可以返回 preferred tool IDs 和诊断信息，但不得：

- 隐藏 Registry 中权限允许的工具；
- 把概率判断升级为 required tool；
- 替代执行边界的权限检查；
- 把 Memory 当作当前运行时事实；
- 因规划失败而静默切换证据 Provider。

## 3. 工具可见性与选择

一次 Run 的工具集合由三项交集决定：

```text
Registry definitions ∩ runtime configuration ∩ request permissions
```

该快照在 Run 内不可变。模型可以：

- 使用路由偏好中的工具；
- 选择其他可见工具；
- 已有证据足够时直接回答；
- 参数、时间或目标改变时重新调用工具。

普通 QA 中，路由偏好未执行不构成失败。真正的 required 调用只能来自确定性的调用方合同，例如显式 required prefetch。

## 4. 工具执行合同

每个工具必须提供：

- 稳定 ID、描述和 JSON Schema；
- read-only 或 write/side-effect 属性；
- 参数验证和规范化边界；
- 有界查询语义；
- 结果 coverage：complete/partial、omitted count、next cursor；
- 可观察错误，不隐藏配置或后端失败。

推荐结果：

```go
tool.Result{
    Content: authoritativeContent,
    Coverage: tool.EvidenceCoverage{
        Partial:      partial,
        OmittedItems: omitted,
    },
    AnswerContract: tool.AnswerContract{
        RequiredLiterals: requiredValues,
    },
}
```

`AnswerContract` 是本次调用结果专属的最终输出要求。没有精确输出要求的工具可以不注册。

## 5. 运行时证据

运行时证据与代码知识证据必须分开：

- 日志只能证明观察窗口内出现的事件；
- 配置读取必须标注环境、命名空间和脱敏状态；
- Trace 必须保留 trace/span 身份和时间范围；
- 告警只证明告警系统观察到的状态；
- 无结果不能自动解释为“问题不存在”。

运行时调查流程：

```text
identify target and time range
  -> locate runtime endpoint
  -> execute bounded query
  -> validate coverage and timestamps
  -> correlate logs/config/trace
  -> state conclusion with confidence and gaps
```

不得用源码中的预期行为替代生产运行时证据，也不得把另一个环境的配置当作目标环境配置。

## 6. Web 证据

Web 能力分成搜索和抓取：

1. Search 返回候选 URL、标题、摘要和来源；
2. Fetch 在响应大小、媒体类型和超时边界内读取正文；
3. 本地 passage ranking 选择高信号段落；
4. 最终答案区分网页原文、模型推断和时间敏感信息。

配置了某个 Web Provider 后，其失败必须直接可见，不能静默换另一个 Provider。搜索摘要不是网页正文，关键事实需要 fetch 或权威页面确认。

## 7. 多轮证据

一轮成功后持久化真实协议消息：

```text
user
assistant(tool_calls)
tool result × N
assistant(final)
```

下一轮根据当前问题选择：

- 原样复用仍然有效的近期证据；
- 对时间敏感或条件变化的事实重新查询；
- 通过 history/artifact 恢复被归档的精确信息；
- 不根据最终答案关键词猜测上一轮调用了什么工具。

工具 call/result 必须成对回放；有界窗口不能从孤立 tool result 开始。

## 8. Required Evidence 空答案事故结论

历史实现曾把路由候选误当 required tool：模型已经生成回答，但因为工具未调用而丢弃答案，步数耗尽后又跳过最终总结，最终返回空答案。

统一后的规则：

- 路由只表示偏好；
- required 只来源于显式确定合同；
- required 未满足时继续争取调用，但不能跳过最终总结；
- 工具执行报错算真实尝试，并必须在答案中披露；
- 除取消或最终 LLM 失败外，Run 不得因证据不足返回空答案。

## 9. 精确答案合同

对于 SN、订单号、设备 ID、迁移清单等不允许缩写的输出，工具可注册合同：

```text
Tool result
  -> collect required literals
  -> provide exact evidence to model
  -> generate candidate answer
  -> deterministic validation
  -> repair retry
  -> reject if still invalid
```

合同不替代工具证据。若模型侧内容已丢失 required literal，Runtime 必须先恢复、分页或将工具调用标记为可重试失败，不能只靠合同提示模型猜测。

大量值应使用服务端 validator + artifact，而不是把成千上万个 literal 再注入 prompt。

## 10. 验收标准

1. Router 失败不会改变权限允许的工具快照；
2. preferred tool 未调用不会自动阻塞回答；
3. required 调用有明确的确定性来源；
4. Runtime/Web 结论包含时间、环境和 coverage；
5. 工具错误、partial 和 omitted 状态进入最终答案约束；
6. 多轮回放保持 call/result 配对；
7. 精确合同失败时不会交付缩写或残缺答案。

## 详细归并材料

### Agent 证据规划

> Migrated from CodeLoom `docs/design/agent/agent-evidence-planning.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现

证据规划是 Agent 决定哪些证据能力可以执行的唯一策略，与答案如何呈现相互独立。

#### 两个独立决策

```text
问题
  -> ResponseMode：答案应如何组织
  -> EvidencePlan：允许执行哪些证据能力
```

`ResponseMode` 是本地计算的答案结构提示，例如 `bug_analysis`、`requirements_analysis`、`architecture_review`、`code_review` 或 `codebase_qa`，它不能授权证据或工具。

`EvidencePlan.Sources` 是位集合：

| 来源 | 权威范围 |
| --- | --- |
| `Memory` | 持久的用户身份、偏好、工作习惯和历史上下文 |
| `Internal` | 当前已索引工作区、服务、API、代码、配置、Schema、调用链和 Runbook |
| `Web` | 当前外部文档、标准、产品能力和公开事实 |

日志和 Trace 等场景证据不是核心来源位。运行保障场景通过受限注册器发布带 `RoutingSpec` 的只读工具；同一规划调用选择核心来源和命中的读 ToolID，工具定义和 Handler 在 Run 的 Tool Snapshot 中固定。

`Sources == 0` 表示 Direct。来源可任意组合，不需要额外的 Mixed 枚举。

#### 执行计划与诊断信息

```go
type EvidencePlan struct {
    Sources EvidenceSources
}

type PlanDecision struct {
    Plan       EvidencePlan
    Confidence float64
    Origin     DecisionOrigin // model, rule, explicit, fallback
}
```

Plan 只回答“允许执行什么”。置信度和来源只用于可观测诊断，不能独立开启工具。

#### 规划协议

自动模式下，快速 LLM 返回核心来源决策；存在意图门工具时，同时返回经过候选白名单校验的 ToolID：

```json
{"route":{"sources":["internal","web"],"confidence":0.92},"tools":{"tool_ids":["observe_logs"]}}
```

规划器接收当前问题和有界会话上下文。即使某项运行能力不可用，也要选择回答问题所必需的全部权威来源；可用性在决策后检查，使缺失前置条件保持可见，而不是悄悄改变证据基础。

API 显式选择会解析成 `origin=explicit` 的固定 Plan。规范值为 `direct`、`memory`、`internal`、`web` 和 `all`；`auto` 只在 API 边界表示使用模型规划。

完全可由现有上下文回答的元问题可以使用规则产生的 Direct Plan。规划失败时使用可观测的 Internal fallback。模型以低置信度选择 Direct 时也回退到 Internal，因为缺乏证据的工作区答案比额外一次内部检索风险更高。

#### 执行语义

| 选择 | 执行行为 |
| --- | --- |
| `Memory` | 召回有界记忆并作为系统上下文注入 |
| `Internal` | 循环前执行工作区检索 |
| `Web` | 开放 `web_search` 和 `web_fetch`，由 Agent 迭代使用 |
| Direct | 跳过核心证据预检索；始终可见的内置读工具仍可由 Agent 按需调用 |

Memory 保持独立 Trace 节点。Web 采用迭代方式，因为必须先检查搜索结果再选择页面抓取。候选 Snapshot 在规划前固定，未命中 `RoutingSpec` 的场景读工具会从本轮 Snapshot 删除；命中的工具可按需调用，也可通过可信 `ToolPlan` 显式预取。核心 Retrieval 不直接调用场景 Service。

有效 Plan 控制核心提示词和检索范围；派生后的固定 Tool Snapshot 独立约束注册工具。即使模型生成隐藏工具名，Executor 也只能在该 Snapshot 内解析。写能力不属于 `EvidencePlan` 或场景读工具注册，必须来自平台封闭目录并经过请求权限与人工审批。

#### Memory 事实边界

Memory 不是项目知识缓存。“用户中心服务是 X”这类可变事实必须由 Internal 解析；旧记忆最多解释历史，不能覆盖当前索引证据。运行时事实需要注册的场景工具，外部事实需要 Web。

在版本化记忆实现前，只应为持久用户上下文选择 Memory。参见[长期记忆](07-memory.zh-CN.md)。

#### 可观测性与评估

`evidence_plan` Trace 节点记录 ResponseMode、建议/有效 Plan、来源、置信度、决策来源以及规划/fallback 错误。`memory_recall` 单独记录，使记忆延迟和质量可以独立评估。

评估项：

- 来源集合精确匹配及每来源 Precision/Recall；
- 必须取证问题的错误 Direct 比例；
- 必需但不可用来源是否可见；
- 实际检索和工具是否为有效 Plan 的子集；
- 证据 Recall、Precision、延迟和预算；
- Groundedness、完整性、引用正确率和首词延迟。

数据集标注所需权威来源集合，而不是一个粗粒度路由，因此多来源问题可以直接评估。

#### 不变量

1. `EvidencePlan` 是核心 Memory/Internal/Web 的唯一执行策略；RoutingSpec 和固定 Snapshot 是场景读工具的执行策略。
2. Direct 是空来源集合，不是一个来源。
3. 多来源是组合，不是模式。
4. 诊断信息不能授权能力。
5. 必需但不可用的来源必须保持可见。
6. 写授权始终独立。
7. 对可变事实，Memory 永远不能高于当前证据。

### Agent 运行时证据与事件设计

> Migrated from CodeLoom `docs/design/agent/agent-runtime-evidence-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：命名数据源、Kibana 日志、SkyWalking Trace 和基础事件工作流已实现

Observe 为 Agent 提供有时间边界的运行时证据。事件管理复用同一 Observe 能力保存调查过程，并在明确批准后发起修复流程。二者共享证据，但属于独立授权域。

#### 端到端关系

```text
observe_sources (MySQL)
  -> 规范化数据源配置
  -> ObservePlan
  -> provider 无关查询
  -> Kibana 日志 / SkyWalking Trace
  -> 规范化运行时证据
       ├─ QA 预检索和 observe_logs 工具
       └─ 事件调查
            -> 持久化分析
            -> 可选通知
            -> 平台封闭写目录提案和人工审批
            -> 分支/提交工作流
```

`EvidencePlan` 包含 Observe 时，只授权 QA 读取运行时证据，不能授权事件变更或仓库写操作。

#### 命名 Observe 数据源

一个数据源包含稳定 Source Key、显示名称、Endpoint/Index Pattern、启用/默认/优先级标志、服务 Pattern、认证、Trace 连接和 `fields_config`。

候选网关作为 Source 配置的一部分复用现有 `fields_config` JSON，不增加数据库列。每个候选网关必须配置自己的索引范围；存在候选网关时，通用 Index Pattern 不参与 `observe_logs` 网关查询，也不能作为解析失败后的回退。没有候选网关的普通 Source 继续使用通用 Index Pattern。当前不注册网关或 Endpoint 管理工具。

`fields_config` 将每个稳定 Observe 字段的 Agent 说明、Provider 路径、查询行为、
抽取规则和日志/Trace Provider 标识放在一起。`timestamp`、`url`、`trace_id`、
`user_id` 等稳定字段名取代原来的角色映射和 Provider Binding。查询分析和
`QueryTerms` 继续使用 Nasuta 中代码拥有的唯一固定契约。

每个属性、稳定字段和解析规则参见
`CodeLoom internal/config/observe_fields.example.md`。

创建/更新在入口完成规范化和校验。运行时代码直接消费规范模型。列表响应永不返回凭据；经过认证的编辑契约只返回该编辑流程需要的 Secret 状态。

#### 规划与数据源解析

只有有效 Agent Plan 选中 Observe 时才执行 Observe。

问题包含邮箱或客户 User ID 时，客户目录同时返回规范 User ID 和命中区域，Observe 将区域映射为唯一 Source。Kibana 不参与区域探测；目录 Source 未配置或与显式 Source 冲突时查询失败。其他情况按显式 Source、严格服务 Pattern 或配置的默认规则解析，非空 service 未匹配时不能回退默认 Source。

快速 LLM 在证据规划调用中生成代码拥有的 `QueryTerms`，其结构固定为 `DomainTerms` 和 `Identifiers`。`observe_logs` 的字段级 Filter Schema 从所选数据源的 description、type 和 operators 派生；非法 Filter 会明确失败，不能注入任意物理查询字段。

#### QA 执行入口

##### 预检索

QA 根据 Source、配置 Filter、全文条件、服务、邮箱/用户 ID 和时间窗口构建 `ObservePlan`。`Retriever.RetrievePlan` 将 Observe 与 Internal 一起执行，并将规范化证据组装进 Agent 初始上下文。

##### Agent 工具

`observe_logs` 暴露已配置字段 Schema，并在工具顶层直接接收 `email` 和 `user_id`，与 Dashboard 请求契约一致。每个 Source 可以声明候选网关及其独立索引范围；工具按 Source 展示候选，模型只能依据已检索的架构或调用链证据选择 `gateway`，不能从 URL 形状推断。未显式提供网关时，只有 `service + url_groups` 可以触发 Config Center 自动解析；解析失败会在查询 Kibana 前终止。查询只访问选中的网关，不能访问通用索引，空结果也不会触发其他网关重试。URL、配置 Filter、非零响应码和 Trace ID 是优先的结构化范围；`full_text` 只是没有这些条件时的消息字段兜底，不能与结构化条件混用。工具参数解析为同一个 `ObservePlan`，并复用相同的数据源解析和直接字段配置。未命中该工具的 `RoutingSpec` 时，它不会进入本轮 Snapshot。工具执行时总是先查询 Kibana；规范化 `response.code != 0` 的日志自动补充 SkyWalking，成功日志或没有 code 的日志默认不查，显式传入 `trace` 时则不受 code 限制。可选 `trace.id` 必须先通过 Trace provider 校验，只能作为 Kibana 过滤条件，不能绕过日志直接查询 SkyWalking。返回的有界日志继续使用规范化的 `request`、`response` 和 `message` 字段，并通过 `query_scope` 标明实际查询范围。

相对时间由多语言路由模型归一化为经过原文校验的语言无关表达式，模型不计算绝对日期。Nasuta 使用一次带服务进程本地时区的时间锚点计算绝对半开区间，并通过上下文强制传给时间感知工具。“最近”固定为 24 小时；没有数字的“最近几天”固定为滚动 5 天。用户原文解析出的权威时间会覆盖工具调用中由模型生成的时间。既无相对时间也无显式范围时，Observe 在入口统一使用最近 24 小时；Kibana 不再独立兜底时间。

预检索不会调用 Agent 工具；两个入口只在 Adapter 下方共享 Observe 领域服务。

#### Provider 边界

`LogProvider` 接收 `ProviderLogQuery`；`TraceProvider` 接收 Trace 标识符和有界时间窗口。当前显式分发支持 Kibana 日志和 SkyWalking Trace。未知或不可用 provider 返回错误，不能由其他后端模拟。

Kibana 编译物理 Filter，检索有界 Hit，抽取规范字段并聚合 API 延迟/错误摘要。自动补充只把非零 code 日志中通过 provider 校验的 traceId 交给 SkyWalking；显式 Trace 查询可以使用全部命中日志。未显式配置 `trace.limit` 时默认最多查询 5 条 Trace。需要补充 Trace 时，Provider 缺失、日志没有合法 traceId 或查询失败都会写入 `trace_error`；普通成功日志或无 code 日志不会报告 Trace 失败。返回 Agent 的结果只保留有界日志、Trace 摘要和截断后的关键 Payload，不返回完整 Hit 与 Span 集合。

未来增加 Loki、Elasticsearch Direct、Jaeger 或 Tempo 时，必须实现独立 provider 和分发 case。跨数据源共享的可复用语义 Profile 目前也未实现。

#### 事件工作流

```text
告警 Webhook 或手工报告
  -> 规范化告警和受影响服务
  -> 与未关闭事件去重
  -> 保存 analyzing 状态
  -> 分析有界告警时间窗口
       ├─ 复用传入日志或查询 Observe
       ├─ 获取有界 Trace
       ├─ 生成确定性分析基线
       ├─ 可选地使用已配置 LLM 和代码提示优化分析
       └─ 构造分析 Markdown
  -> 保存 open 状态
  -> 可选飞书/企业微信/HTTP 通知
```

MySQL 保存告警 Payload、证据、受影响服务、分析、负责人、修复分支和生命周期时间。未关闭事件按规范化标题、服务和告警窗口去重。

确定性基线识别 Trace 错误或聚合慢 API。可选 LLM 分析可以改进根因和方案，但 LLM 失败时仍保留可读的确定性报告。代码检索只提供有界证据提示，不能授权写操作。

#### 事件 API 与写边界

当前 API 提供带 Secret 校验的告警入口、手工创建、列表/详情/删除、直接认证的修复启动和修复确认。

Agent 调用平台封闭目录中的 `propose_branch` 和 `propose_commit` 只创建 Pending Action。人工审批后分发已持久化的精确 Action；上层场景不能注册新的写 Tool Contract。参见[需要审批的写工具](09-write-safety-and-approval.zh-CN.md)。

当前修复实现：

1. 要求共享 Worktree 干净；
2. Fetch 基础分支并创建或重置修复分支；
3. 将分析和建议写入 `.nasuta-fix.md`；
4. 确认后提交当前变更、Push，并可选创建 GitLab MR。

这不是自主修复代码的 Agent。事件 UI 的直接 Endpoint 仍是经过认证的操作，与 Pending Action 审批分离。

#### 安全、降级与限制

- 查询时间窗、Hit 数、Trace 数、provider 调用和工具执行都有限制。
- Observe 缺失只关闭运行时证据；被选中但不可用的状态保留日志和 Trace。
- 没有 LLM 时仍保留确定性事件分析。
- 通知为尽力而为，没有持久重试队列。
- 事件修复使用共享 Checkout，尚未实现每 Action 独立 Worktree。
- 并发审批的 Exactly-Once 执行和持久后台任务仍是后续工作。
- 事件列表只有数量限制，尚未使用 Cursor 分页。

#### 评估

Observe 度量数据源选择准确率、身份路由、语义到物理字段绑定、时间窗口正确性、provider 延迟/错误、规范字段完整性、Trace 关联率和答案 Grounding。

事件流程度量告警去重、相关证据捕获、根因 Groundedness、能力缺失行为、通知结果、审批审计完整性和变更幂等性。

#### 不变量

1. QA 读取 Observe 必须由 Observe EvidencePlan 授权。
2. Observe 证据不能授权事件或写操作。
3. 已配置 provider 必须显式执行，不能替换。
4. 证据缺失应产生可见缺口，不能伪造确定性。
5. Agent 发起的变更必须经过 Pending Action 审批。
6. Worktree 不干净时禁止创建修复分支。

### Agent 运行时调查准确性修复提案

> Migrated from CodeLoom `docs/design/agent/agent-runtime-investigation-remediation.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：P0-P3 已于 2026-07-21 实施，P4 可观测性与离线评测、P5 运行时错误概览待办

已落地：Trace ID 入口校验、默认 Trace 上限、严格 service 路由、客户目录单区域解析、工具 `identity` 参数、字段级 Filter Schema、结构化 QueryTerms，以及业务标识符的 CodeGraph 扩展保护。

待完成：补充 Source 解析依据、逻辑 Filter 和 Trace 跳过原因的统一 Trace 节点，并用历史调查样本建设离线质量评测。

本文针对运行时调查中已经观察到的一类通用失真：用户给出了身份和业务标识符，但 Agent 错选代码入口、跨区域反复查询、把业务标识符当成 Trace ID，并在步骤耗尽后基于无关证据得出结论。

本文是[运行时证据与事件设计](02-evidence-and-tooling.zh-CN.md)的专项修复提案。它不改变 Observe 的 provider 边界，也不为“删除设备”等具体业务增加动作枚举、资源枚举、意图状态机或硬编码关键词。

#### 1. 结论

修复应收敛为四项通用机制：

1. `QueryTerms` 继续只包含 `DomainTerms` 和 `Identifiers`，但由现有 query-analysis LLM 输出中文领域短语和原始标识符。
2. `observe_logs` 从已配置语义字段自动生成可理解、可校验的 Filter Schema，使模型能把业务标识符放进正确字段。
3. 客户目录一次返回规范 User ID 和区域，Observe 只查询对应区域，不能再通过查询多个 Kibana 数据源猜测区域。
4. Trace provider 在入口校验 Trace ID；业务标识符不能进入 `trace.id`，SkyWalking 只能接收日志命中中提取出的 Trace ID 或显式合法 Trace ID。

目标调用链：

```text
用户问题
  -> query-analysis
       -> DomainTerms: 设备删除、解绑设备等领域短语
       -> Identifiers: 邮箱、设备 SN 等原始标识符
  -> Internal 检索定位相关设备流程
  -> observe_logs 结构化参数
       -> email
       -> filters[device_sn]
  -> CustomerDirectory
       -> canonical userId + source
  -> 单区域 Kibana
       -> identity + device_sn 过滤
  -> 从命中日志提取完整 Trace ID
  -> 同区域 SkyWalking
  -> 运行时证据与实现证据共同支撑结论
```

#### 2. 观察证据

本次分析样本为 `logs/all-2026-07-21.log` 中的 Trace `24a26fd6df22`。

| 现象 | 证据 | 直接影响 |
| --- | --- | --- |
| Internal 预检索选中账号删除代码 | 日志第 437 行包含 `hsas-app-user/UserController.deleteUser` 和 `DynoUserServiceImpl.deleteUserInfo` | Agent 将设备删除理解为账号删除 |
| 首次 Observe 调用把设备 SN 放入 `trace.id` | 日志第 891 行 | Kibana 以错误 Trace ID 过滤，返回 0 条 |
| 首次调用未指定 Source，使用默认 NA | 日志第 889 行查询 `hs-iot-hsmf-mobile-gateway-pro.*` | 在身份区域确定前已经查询错误区域 |
| 后续又显式查询 EU | 日志第 1284、6693、6714 行 | 同一调查跨两个区域反复搜索 |
| 截断业务 ID 后产生超宽查询 | 日志第 1295-1296 行返回 `889092` 条匹配 | 无关产品目录日志进入上下文并触发无关 Trace 扩展 |
| 错误服务提示仍落到默认 gateway 索引 | 日志第 7119、7133、7551 行及相邻 Kibana 结果 | `service` 看似选中了应用服务，实际没有改变物理索引 |
| 五步后强制结束 | 日志第 7997 行 | 尚未找到设备删除请求就被迫总结 |

这些现象不是五个独立的 Prompt 问题。它们共同说明结构化语义没有贯穿 query-analysis、工具参数、区域路由和 provider 执行。

#### 3. 当前根因

##### 3.1 中文业务语义没有进入 QueryTerms

当前 `AnalyzeEvidence` 使用 `ExtractTechTerms` 初始化 `QueryTerms`。该路径主要从英文技术词中提取 Token，不能稳定得到“设备删除”这样的中文领域短语。

因此，原始问题中的邮箱和长设备 SN 在向量/BM25 查询中占据较大权重，而“删除”的高相似账号代码更容易成为锚点。一旦预检索把 `hsas-app-user` 放在上下文前部，Agent 后续工具调用会沿着错误假设继续搜索。

##### 3.2 Tool Schema 丢失字段语义

旧版 `observe_logs` 只给 `filters[].field` 提供字段名枚举，并使用一段通用描述。字段说明以及字段与 Operator 的对应关系没有进入每个 Filter 分支。

配置里即使增加了 `device_sn`，模型也只能看到一个缺少解释的字段名，无法稳定判断长复合标识符应放入 `filters.device_sn`，而不是 `trace.id` 或 `full_text`。

##### 3.3 Agent 工具入口拿不到身份路由元数据

`ObservePlan` 已有 `Email` 和 `UserID`，但它们没有进入 Agent 工具契约。问题中的邮箱没有变成 `plan.Email`，`ResolveIdentitySource` 因而无法执行身份解析。

没有身份时，空 Source 会落到默认数据源；模型发现 0 条后又自行猜测 EU，从而形成跨区域搜索。

##### 3.4 区域解析依赖日志探测

当前身份解析会对多个 Observe Source 执行有界 Kibana 探测，以确定身份属于哪个区域。即使 Agent 正确传入身份，这种机制仍可能对多个区域产生日志请求。

客户目录客户端实际已经按区域调用 Backstage，但返回值丢弃了命中的 endpoint 名称，只保留 User ID。区域事实因此没有被传给 Observe。

##### 3.5 `service` 未匹配时静默回退

`resolveSource` 在 service pattern 未命中时继续选择默认数据源。工具参数中的 `service="hsas-app-user"` 看起来像限定应用服务，实际仍查询默认 gateway 索引。

这违反“配置失败必须可见”的边界，也让 Agent 无法从 0 条结果判断是关键词不存在、服务不受支持，还是查询了错误索引。

##### 3.6 `trace.id` 是无约束字符串

`ParseToolPlan` 只读取并 Trim `trace.id`，没有调用 Trace provider 的标识符校验。任何邮箱、SN、订单号或请求路径都能进入 Trace 过滤。

同时，`ExecuteTool` 默认要求 Trace 扩展，并把日志 Limit 转换为最高 20 条 Trace 查询。一次宽泛日志命中可能因此触发不相关 SkyWalking fan-out。

#### 4. 设计约束

1. 不增加 `action`、`resource`、`typed_identifiers` 或业务动作状态机。
2. 不为“删除设备”、特定接口、邮箱、SN 格式或服务名增加生产硬编码。
3. 数据源 JSON 只描述语义字段、角色和物理绑定；通用提取与校验机制属于代码。
4. 身份与区域在可信入口规范化一次；下游不得重复猜测或静默回退。
5. `service`、`source` 和 provider 的实际执行范围必须一致且可见。
6. SkyWalking 只接受 Trace provider 能验证的标识符。
7. 所有日志、Trace 和目录读取都必须有时间、数量和并发上限。

#### 5. 目标设计

##### 5.1 QueryTerms 保持两个维度

稳定结构不变：

```go
type QueryTerms struct {
    DomainTerms []string
    Identifiers []string
}
```

query-analysis 的 JSON 增加代码拥有的固定部分：

```json
{
  "query_terms": {
    "domain_terms": ["设备删除", "解绑设备"],
    "identifiers": [
      "user@example.com",
      "business-identifier-from-question"
    ]
  }
}
```

约束：

- `domain_terms` 最多 5 个，保留用户语言中的高区分度领域短语；
- `identifiers` 最多 5 个，只复制问题中存在的字面值；
- 不要求模型给标识符分类；
- `normalize` 继续做 Trim、大小写无关去重、噪声过滤和数量上限；
- 解析失败时保留现有确定性 `ExtractTechTerms` 降级并记录降级原因。

这部分服务 Internal 召回，不直接生成 Kibana 物理查询。

##### 5.2 从配置字段生成 Filter Schema

`ToolSpec` 不再生成一个全局 field enum 和全局 operator enum，而是为每个声明了 `operators` 的字段生成一个 `oneOf` 分支：

```json
{
  "oneOf": [
    {
      "properties": {
        "field": {
          "const": "device_sn",
          "description": "Device serial number or composite device identifier"
        },
        "operator": {
          "enum": ["match_phrase"]
        },
        "value": {
          "type": "string"
        }
      },
      "required": ["field", "operator", "value"]
    }
  ]
}
```

字段描述从直接字段配置的 `description`、`type` 和 `operators` 自动生成。配置仍是唯一语义来源，工具 Schema 不维护第二份业务词表。

执行时继续通过 `compileFilters` 校验字段和 Operator，并由 Provider 直接使用字段的 `path` 和查询配置。Schema 提示用于提高参数准确率，服务端校验仍是最终边界。

##### 5.3 增加代码拥有的身份路由参数

`observe_logs` 增加顶层参数：

```json
{
  "email": {"type": "string", "format": "email"},
  "user_id": {"type": "string"}
}
```

`email` 和 `user_id` 是 Observe 的路由元数据，不是 Kibana 物理字段，也不要求每个数据源重复配置邮箱定义。工具入口将它们直接写入规范化的 `ObservePlan.Email/UserID`。

当问题同时包含身份和其他业务标识符时，目标工具参数为：

```json
{
  "email": "user@example.com",
  "filters": [
    {
      "field": "device_sn",
      "operator": "match_phrase",
      "value": "business-identifier-from-question"
    }
  ],
  "limit": 50,
  "trace": {
    "limit": 5,
    "window_minutes": 10
  }
}
```

不预设 API 路径。实际删除接口应由这次日志命中的 URL 和 Trace 发现，而不是从错误代码锚点推断 `/api/user/info`。

##### 5.4 CustomerDirectory 返回 User ID 和 Source

消费方接口由 Observe 定义：

```go
type ResolvedIdentity struct {
    UserID string
    Source string
}

type CustomerDirectory interface {
    ResolveIdentity(context.Context, string, string) (ResolvedIdentity, error)
}
```

Backstage adapter 在按区域查找客户时保留实际命中的 endpoint 名称，并在返回前映射为规范 Source Key。Observe 验证 Source 已配置后固定执行该数据源。

路由顺序调整为：

```text
身份存在
  -> CustomerDirectory.ResolveIdentity
  -> canonical userId + source
  -> 校验 source 已配置
  -> 固定 source

身份不存在
  -> 显式 source
  -> 严格 service pattern
  -> 配置默认 source
```

身份存在但目录无法确定区域时返回明确错误，不能回退默认 NA。显式 Source 与目录 Source 冲突时同样返回错误。

完成该改造后，删除以 Kibana 探测身份区域的 `probeSourceIdentity` 生产路径。Backstage 可能按配置顺序访问多个客户目录 endpoint，但 Kibana 和 SkyWalking 只访问最终命中的一个区域。

##### 5.5 严格 service 语义

`service` 只表示 Source 路由提示，不表示动态切换物理索引。

规则：

- 非空 service 必须命中某个配置的 `service_patterns`；
- 未命中时返回 `service is not mapped to an observe source`；
- 不得继续选择默认 Source；
- Tool 描述明确说明 service 不会创建索引过滤；
- 结果和诊断日志记录最终 Source 与物理 index pattern。

若未来确实需要按服务查询不同索引，应在数据源配置中建立可验证映射，不能让任意 service 字符串隐式修改 provider 查询。

##### 5.6 Trace provider 校验和有界扩展

Trace provider 增加输入校验行为：

```go
type TraceProvider interface {
    ValidateTraceID(string) error
    QueryTrace(context.Context, string, time.Time, int) (*TraceResult, error)
    QueryTraces(context.Context, []LogHit, int, int) ([]*TraceResult, error)
}
```

约束：

- 显式 `trace.id` 在编译 Kibana Filter 前校验；
- 校验失败时不发送 Kibana 或 SkyWalking 请求；
- 错误提示调用方改用已配置字段 Filter 查询业务标识符；
- 已完成的 `TID:` 去除和点分段保留逻辑继续使用；
- 未显式配置 `trace.limit` 时默认最多扩展 5 条，不再跟随日志 Limit 放大到 20；
- 只有 Kibana 命中包含合法 Trace ID 时才自动查询 SkyWalking；
- 过宽查询、没有结构化 Filter 或没有 Trace ID 的命中必须产生可见的跳过原因。

SkyWalking ID 的具体语法属于 SkyWalking adapter，不能散落在 Plan、Kibana 和 Agent Prompt 中。

#### 6. 最小数据源配置

现有配置只需要增加一个直接字段；数据源配置不包含 `preprocess` 或 `query_terms`。

```json
"device_sn": {
  "path": "info_message",
  "description": "Device serial number or composite device identifier",
  "type": "text",
  "query": "match_phrase",
  "operators": ["match_phrase"]
}
```

不在 JSON 中增加动作、资源、接口或 Trace 类型配置。工具 Schema、身份路由和 provider 校验由代码从稳定契约派生。

#### 7. 实施切片

##### P0：入口防错

- 为 `trace.id` 增加 provider 校验；
- 将默认 Trace 扩展上限收敛到 5；
- service pattern 未命中时停止默认 Source 回退；
- 增加对应单元测试和 provider 零调用断言。

P0 直接阻止最危险的错误查询，不改变检索结构。

##### P1：单区域身份路由

- CustomerDirectory 返回 User ID 与 Source；
- Backstage adapter 保留命中 endpoint；
- `observe_logs` 接收 `identity`；
- 身份路由固定 Source，并移除 Kibana 区域探测路径；
- 显式 Source 冲突时失败。

P1 完成后，同一身份调查只能访问一个 Kibana 和一个 SkyWalking Source。

##### P2：配置派生 Tool Schema

- 按配置了 `operators` 的字段生成 Filter `oneOf`；
- 输出 description、type 和字段级 operators；
- 保留执行期 `compileFilters` 校验；
- 为 Schema 快照和解析增加回归测试。

P2 让新增字段自动进入 Agent 工具能力，不需要同步修改 Prompt。

##### P3：QueryTerms 中文语义

- query-analysis 输出固定 `query_terms`；
- 绑定到现有 `QueryTerms` 两个切片；
- 保留确定性降级；
- 调整 Internal 查询，使领域短语主导流程检索，字面业务标识符不能单独决定服务锚点。

P3 修复设备操作被账号操作吸附的通用检索问题。

##### P4：可观测性与评测

- 记录规范领域词和标识符数量，不在普通日志输出完整敏感值；
- 记录 Source 解析依据：identity、explicit、service 或 default；
- 记录逻辑 Filter 名称、最终 index pattern、Trace 扩展数和跳过原因；
- 将历史失败问题加入离线评测集，但生产逻辑不得引用样本中的具体值。

不通过提高全局 Agent MaxSteps 解决该问题。结构化首次查询准确后，现有步骤预算应足够；若仍耗尽，再基于工具无进展度量设计通用收敛机制。

#### 8. 测试方案

##### 8.1 Nasuta

1. query-analysis 能解析中文领域短语和问题中的字面标识符。
2. 非法 JSON、超量术语和重复术语被稳定降级或规范化。
3. 业务标识符不能因长度或数字特征成为唯一服务锚点。
4. 工具路由仍只选择已注册白名单 Tool ID，不扩大权限。

##### 8.2 CodeLoom Observe

1. ToolSpec 的 `device_sn` 分支包含 description、type 和唯一合法 Operator。
2. 顶层 `email` 和 `user_id` 正确进入 `ObservePlan`。
3. Backstage 返回 EU 后，NA LogProvider 调用次数为 0。
4. 最终 Kibana 查询同时包含规范 User ID 与 `device_sn` Filter。
5. 设备 SN 进入 `trace.id` 时失败，Kibana/SkyWalking 调用次数均为 0。
6. Kibana 命中中的完整点分 Trace ID 原样传给 SkyWalking。
7. 未映射 service 返回错误，不能访问默认 gateway 索引。
8. 宽泛全文查询不会自动扩展 20 条 Trace。

##### 8.3 端到端验收

同类问题必须满足：

- Internal 首屏证据包含设备删除或解绑流程，不以账号删除为首锚点；
- 首次 Observe 调用使用 `identity + device_sn`，不使用 `trace.id=SN`；
- 一个 QA Run 只查询最终解析出的一个日志区域；
- 不预设 `/api/user/info`，以日志命中的实际 URL 为准；
- SkyWalking 查询参数来自 Kibana 日志的真实 Trace ID；
- 最终根因明确区分运行时事实、代码事实和推断。

#### 9. 验证命令

每个切片先运行最窄测试，再运行完整检查：

```bash
go test ./internal/observe ./internal/customer/backstage ./internal/transport
go build ./...
go test ./...
go vet ./...

(cd ../Nasuta && go test ./internal/retrieval ./internal/agent)
(cd ../Nasuta && go build ./... && go test ./... && go vet ./...)
```

涉及并发 Trace 扩展或运行时共享状态时，补充：

```bash
go test -race -count=1 ./...
(cd ../Nasuta && go test -race -count=1 ./...)
```

不得在默认测试中启动 Docker、访问真实 Kibana/SkyWalking/Backstage 或调用付费 LLM。集成行为使用有界 `httptest` provider 和调用计数断言验证。

#### 10. 完成标准

本提案只有在以下条件全部满足后才能改为“已实施”：

1. P0-P3 的行为测试全部通过。
2. 三个历史调查样本在离线评测中不再出现账号删除吸附、跨日志区域、SN 充当 Trace ID。
3. 数据源配置不包含动作、资源、`preprocess` 或 `query_terms` Schema。
4. 不存在以失败样本具体词、SN、邮箱、接口或服务名为条件的生产分支。
5. 运行日志能够解释 Source、Filter 和 Trace 的选择依据。
6. CodeLoom 与 Nasuta 均通过 build、test 和 vet。

#### 11. P5：运行时错误概览请求

状态：待办。以下记录来自 2026-07-27 的一次线上样本，仅用于设计和回归评测；本文描述的 P5 行为尚未实施。

##### 11.1 观察样本

样本 Trace ID：`350200307451`。

用户提问：

> 看一下线上今天有哪些报错的请求接口

| 现象 | 日志证据 | 影响 |
| --- | --- | --- |
| 路由选择 `internal` | 13:57:01 的 evidence plan 为 `internal`，置信度 1.00 | 请求先做代码和 Runbook 预检索，而非直接查询当前日志。 |
| 无关静态上下文进入主 Agent | 13:57:03 的预检索返回 26 个命中、14,440 字符；主模型编译上下文为 68,031 字符 | 主模型首步耗时 11.89 秒，且上下文包含与当前问题无关的证书对话和代码证据。 |
| 日志工具没有成功执行 | 13:57:15 两次 `observe_logs` 分别传入 `source=eu`、`source=na`，均因缺少 `gateway` 在本地校验失败 | 没有访问 Kibana，也没有任何真实错误接口可供回答。 |
| 回答将工具参数错误表述为范围不足 | 13:57:23 的最终回答要求用户补充区域和入口 | 用户无法区分“未查询成功”与“查询后没有结果”。 |
| 会话归档占用请求收尾 | 13:57:24 后的归档总耗时 10.16 秒，其中摘要调用 9.877 秒 | 答案已生成，但 SSE 请求收尾可能继续等待归档完成。 |

本样本不是特定区域、网关或接口的规则输入。生产逻辑不得以该 Trace ID、`eu`、`na` 或问题中的具体词作为分支条件。

##### 11.2 根因和边界

“当前有哪些报错接口”属于运行时事实查询。它需要的权威证据是 Observe 返回的日志，不是代码检索结果。当前 `EvidencePlan` 只表达 Memory、Internal 和 Web，`observe_logs` 仅作为可选工具候选；路由模型可以同时选择 Internal，导致预检索在工具调用前执行。

现有 Source 的候选网关隔离是正确约束：缺少 `gateway` 时不得默认选择、猜测或在空结果后依次重试网关。P5 不能通过删除该校验解决问题。

##### 11.3 建议目标设计

1. 路由协议应区分“静态知识证据”和“运行时权威工具”。当问题请求当前日志、Trace、告警或实时状态时，路由选择对应的权威工具并跳过 Internal 预检索；只有同一问题还要求代码实现或根因解释时，才在获得运行时命中后补充 Internal 证据。
2. 对“今天有哪些报错”这类没有单一网关条件的汇总请求，Observe 应提供一个明确、受限的错误聚合操作，而不是复用空 `gateway` 的单网关 `ObservePlan`。该操作只能查询已配置的范围、只能用于 `errors_only`、必须限制总返回数和扇出并发，并在结果中报告已查询范围及失败范围。
3. 聚合操作不自动扩展 SkyWalking Trace。用户选择某条日志后，再以该日志的真实 Trace ID 查询 Trace，避免网关聚合进一步放大外部调用。
4. 若运行时工具因范围不足而没有返回数据，最终回答必须明确写出“未查询日志”和缺失条件；不得把参数校验失败描述为日志查询结论。
5. 主 Agent 的原始近期消息必须有独立预算。与当前问题不相关的完整历史应由已召回的相关摘要替代，不能与预检索证据一起无限累积。
6. 保存 Turn 后的历史归档应进入按 Session 去重的后台任务。归档失败只记录可观测错误，不能阻塞已发送答案的 SSE 收尾。

##### 11.4 实施约束

- 聚合范围只能来自已启用的 Observe Source 和 Gateway 配置；不能接受模型构造的任意索引、区域或网关名。
- 必须保留单网关查询的现有严格行为；聚合是独立、显式且有边界的操作。
- 已配置的 Kibana/SkyWalking 后端失败必须在聚合结果中可见，不能静默跳过或替换为其他 provider。
- 聚合应先按每个范围的有界配额读取，再按时间合并并裁剪到全局上限；不得把所有日志读到内存后再分页。
- 历史裁剪和异步归档不改变会话持久化顺序，且要防止同一 Session 并发产生重复摘要任务。

##### 11.5 验收与回归

1. 给定“看今天线上有哪些报错接口”及无关的前序对话，路由选择 Observe 工具且不调用 Internal Retriever。
2. 错误概览工具只发起受配置约束的范围查询，最大并发、每范围读取数和全局返回数均可断言。
3. 返回内容包含接口、错误码、时间、区域/网关、总命中数、已查询范围和失败范围。
4. 工具因缺少范围而失败时，最终回答明确说明未获得日志数据；不得声称日志范围过广或暗示已经完成查询。
5. 错误概览查询不触发 SkyWalking；用户选择具体日志后才允许有界 Trace 查询。
6. 主模型编译上下文不包含无关的完整历史；相关召回内容仍可用于真正的追问。
7. SSE 最终答案发送后，会话归档不再延长请求完成时间；同一 Session 的并发归档不会重复消费模型调用。

### Agent Web 证据设计

> Migrated from CodeLoom `docs/design/agent/agent-web-evidence-design.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现

Web 证据由两个 Agent 只读工具提供。`web_search` 查找候选网页，`web_fetch` 将选中的页面转换为有界、与问题相关的证据。只有 `EvidencePlan` 选中 Web 且能力已配置时才开放这些工具。

#### 搜索

`NASUTA_WEB_SEARCH_ENABLED` 控制工具注册，`NASUTA_WEB_SEARCH_MCP_ENABLED` 独立控制 MCP 暴露。`NASUTA_WEB_SEARCH_ENGINE` 选择已注册的 Provider；Nasuta 默认注册 DuckDuckGo、Brave 和 Bing，需要凭据的内置 Provider 通过 `NASUTA_WEB_SEARCH_API_KEY` 读取凭据，并在缺失时返回明确错误。上层可以通过 `RegisterWebSearchProvider` 注册或替换 Provider，无需修改中心分发逻辑。

```text
查询
  -> 引擎分发器
  -> provider 请求和解析
  -> 最多 10 条标题/URL/摘要候选
```

搜索摘要只是线索，不能单独支撑重要结论。依赖页面细节前，Agent 必须抓取权威页面。

分发器按规范化名称查询 Provider Registry；未知名称直接返回配置错误，不会静默替换为其他 Provider。

#### 抓取边界

`web_fetch` 只接受绝对 HTTP(S) URL，并使用防 SSRF 客户端。请求超时 15 秒，响应体按字节限长。

```text
HTTP Body
  -> 根据 BOM、Content-Type 或文档元数据判断字符集
  -> 一次性转换为合法 UTF-8
  -> 删除脚本、样式、导航、页头、页脚和侧栏内容
  -> 将可读 HTML 转为 Markdown
  -> 按标题和段落生成候选
  -> 删除重复和链接密度过高的区块
  -> 按当前问题本地排序
  -> 在 8,000 字符预算内选择多样段落
```

字符集转换是不可信输入的入口边界。后续抽取、排序、日志和模型输入可以信任合法 UTF-8。截断按 Rune 执行，不破坏字符。

#### 本地段落排序

每个页面形成一个临时段落语料库：

```text
score = body_bm25
      + 0.8 * section_heading_overlap
      + 0.3 * page_title_overlap
```

选择器使用有界 Top-K 堆，限制同一标题下重复段落，并在字符预算耗尽时停止。除有界 `k` 的 `O(n log k)` Top-K 维护外，其余复杂度为线性。

段落选择不调用 Embedding 或 LLM，因此延迟、成本、失败行为和评估是确定性的。只有段落数据集证明存在通用的释义召回缺口时，才值得引入语义重排器。

没有 Query 时，Fetch 返回清理后文档的 Rune 安全前缀。Agent 工具 Schema 通常会提供当前问题，因此 QA 获得的是排序后的段落。

#### Agent 循环行为

Web 不预取，因为搜索结果检查和页面选择是迭代过程。循环跟踪已抓取证据；重复搜索或页面不再产生有效进展时，加入收敛提示。仅 Web 的 Plan 使用紧凑研究提示词；混合 Plan 可以将 Web 工具与 Internal 或 Observe 证据组合。

#### 评估与不变量

度量 provider 延迟/错误、非法引擎处理、抓取成功率、非法 UTF-8 数量、抽取失败、段落 `Recall@3/5`、选中字符数、重复率和最终答案 Groundedness。

1. Provider 必须显式分发，不能替换。
2. 网络输入只在入口转换一次合法 UTF-8。
3. 页面内容进入模型前必须限长。
4. 搜索摘要不能替代抓取证据。
5. QA 能力和 MCP 暴露保持独立开关。

### QA 必需证据导致空答案事故与修复

> Migrated from Nasuta `docs/design/qa-required-evidence-failure-remediation.zh-CN.md`; incorporated into this module on 2026-07-31.

#### 事故摘要

2026-07-24，trace `f9ef79031b03` 的 QA 运行在模型已经生成回答后，最终以空答案失败：

```text
answerLen=0
required evidence tools not attempted before step limit: observe_logs
```

这不是 `observe_logs` 执行失败。该工具从未被模型调用，Agent 丢弃了两次提前回答，并在步数耗尽后跳过最终总结。

#### 引入时间线

- `b8b49b8`（2026-07-21 20:03）在上下文追问没有选择具体工具时，将全部路由候选工具补回工具快照。此时补回只表示工具可见，不会阻止回答。
- `ca28a58`（2026-07-24 13:53）将路由工具同时作为必需工具：模型未尝试必需工具时丢弃回答，步数耗尽后直接返回错误。
- `f9ef79031b03`（2026-07-24 15:21）同时命中两个条件：上下文补回 `observe_logs`，随后它被错误升级为必需工具，最终没有可见答案。

#### 根因

原始目标是防止模型绕过 evidence router。对于当前用户数、线上错误或实时日志等问题，路由器明确选择运行时证据后，模型必须至少真实尝试对应工具，不能使用代码或文档冒充运行时结果。

实现混淆了三个不同事实：

1. 工具是否对本轮模型可见；
2. 路由器是否明确要求本轮调用该工具；
3. 工具未调用或执行失败后，回答能够声明哪些结论。

上下文补回的工具只满足第一个事实，却被当作第二个事实。第三个事实又被实现成终止运行，因此证据质量约束破坏了最终回答保障。

#### 第一阶段修复

- 路由器本轮明确选择的工具是 required；上下文自动补回的工具只 available。
- required 工具未尝试时，循环仍拒绝未经约束的提前结论并继续争取工具调用。
- 达到步数上限后，不再以空答案结束。Agent 在无工具的最终总结提示中列出未尝试工具，要求明确说明证据缺口，并禁止声称工具已运行或数据已被观察。
- 工具实际执行报错仍算一次尝试。错误结果保留在模型上下文中，最终回答必须报告错误和结论限制。
- 缺失的 `agent_answer_reserve` 在平台设置入口规范化为 `30s`；非正数配置不允许保存。最终总结因此拥有独立时间预算。

该阶段先消除了空答案，但仍让概率型路由决定 required tool。后续对齐发现这项职责划分仍然过重，最终设计已由 [QA 工具选择与多轮证据设计](02-evidence-and-tooling.zh-CN.md) 取代：路由只提供偏好，Registry 决定可见性，普通 QA 不再从路由产生 required tool。

#### 长期不变量

1. 除用户主动取消或最终 LLM 本身失败外，QA 运行不得因为工具未调用或工具执行失败而返回空答案。
2. 概率型路由不裁剪工具快照，也不产生 required tool。
3. required 状态只来源于显式、确定性的调用方契约。
4. 缺失或失败的证据必须对用户和运行日志可见，回答不得伪造成功证据。
5. 工具循环不能占用完整运行超时，必须为最终回答保留正数时间预算。

#### 回归覆盖

- 带路由元数据的读工具不因本轮未命中而从工具快照消失。
- 本轮路由命中只形成偏好提示；模型可以根据完整会话直接回答，也可以改用其他可见工具。
- 工具执行失败后，错误作为 tool result 进入下一次模型调用；即使随后耗尽步数，仍执行最终总结。
- 多轮会话持久化配对完整的 tool call/result，读取窗口不会向模型发送孤立工具消息。
- 显式 `ToolPlan.Prefetch.Required` 失败仍按调用方契约返回错误。
- `agent_answer_reserve` 缺失时使用 `30s`，零值、负值和非法 duration 在写入边界被拒绝。

### QA 工具选择与多轮证据设计

> Migrated from Nasuta `docs/design/qa-tool-selection-and-multiturn-evidence.zh-CN.md`; incorporated into this module on 2026-07-31.

#### 目标

QA Agent 必须同时满足三项约束：

1. 模型始终能看到当前配置和权限允许的稳定工具集合；
2. 路由分析可以提示更相关的工具，但不能把概率判断升级为权限或强制调用；
3. 多轮追问能够看到上一轮真实的工具调用和有界结果，而不是依靠猜测或补回全部候选工具。

该设计参考 DeepSeek-Reasonix 的职责划分：Registry 决定可用能力，模型结合会话历史决定是否调用，权限在执行边界检查，工具失败和步数耗尽都不能跳过最终回答。

#### 职责边界

##### 工具可见性

工具 Registry、运行配置和请求权限共同生成一次不可变快照。快照在单次 Agent 运行期间保持稳定，所有允许的读工具都对模型可见；写工具仍由明确的写权限控制。

问题语义、路由置信度和上一轮是否使用某个工具都不改变可见性。这样可以避免路由漏选造成能力消失，也能保持工具 schema 稳定。

##### 路由偏好

Evidence Router 返回的 `tool_ids` 表示本轮可能有用的工具，进入 Agent 时只形成一条临时偏好提示：

- 模型可以在确有关键证据缺口时调用；
- 已有上下文或上一轮结果足够时可以直接回答；
- 模型可以选择其他已注册工具；
- 偏好工具未调用不阻止回答。

路由输出不得写入 required 集合，不得裁剪快照，也不得成为权限判断依据。

##### 必需调用

必需调用只来自确定性的调用方契约。目前保留 `ToolPlan.Prefetch.Required`：上层场景明确声明某个预取是请求前置条件时，预取不可用或失败应返回错误。

普通 QA 的 LLM 路由不是确定性契约，因此不产生 required tool。

#### 多轮工具证据

成功完成一轮后，会话按顺序持久化：

1. 用户问题；
2. assistant 的结构化 tool calls；
3. 每个 tool result 的有界内容；
4. 最终 assistant 回答。

工具参数在写入前规范化为 JSON，工具结果限制长度。会话仍按用户隔离，在线读取仍使用存储层 `LIMIT`。

下一轮 Agent 会收到配对完整的 tool call/result。若有界窗口从一组工具结果中间开始，缺少调用方的孤立结果会被丢弃，避免向 OpenAI 或 Anthropic 发送非法消息序列。

强关联追问不自动继承上一轮工具。模型根据真实历史决定：

- 复用已有结果直接回答；
- 时间、过滤条件或目标变化时再次调用同一工具；
- 关注点变化时选择其他工具；
- 上一轮失败时报告限制或改变方法。

#### 失败与收尾

- 工具执行错误作为 tool result 返回模型，错误即一次真实尝试；
- 工具失败不终止 Agent，也不允许伪造成功证据；
- 工具循环达到步数或时间预算后，使用保留的回答预算执行无工具最终总结；
- 除用户取消或最终 LLM 本身失败外，QA 不得返回空答案。

#### 不采用的方案

- 不根据单轮路由结果隐藏工具；
- 不把路由命中自动升级为 required；
- 不在省略式追问时补回全部候选工具；
- 不无条件复用上一轮工具；
- 不从最终回答中的关键词反推上一轮调用了什么工具。

#### 可观测性

`evidence_plan` trace 分别记录：

- `preferred_tool_ids`：路由建议；
- `available_tool_ids`：实际模型可见工具；
- `planning_error`：路由降级原因。

两组 ID 必须分开，避免再次混淆“建议”和“可用”。

#### 数据迁移

`qa_messages` 增加：

- `tool_calls_json`：assistant tool calls；
- `tool_call_id`：tool result 对应的调用 ID；
- `tool_name`：tool result 的工具名。

新安装由平台 schema 直接创建这些列；已有安装执行 `docs/sql/migration_qa_message_tools.sql`。

#### 验收标准

1. 路由只命中一个工具时，其他权限允许的读工具仍在模型工具列表中；
2. 模型忽略路由偏好并直接给出有证据约束的回答时，运行正常结束；
3. 工具失败后模型能看到错误并给出最终回答；
4. 下一轮请求包含上一轮配对完整的 tool call/result；
5. 会话读取不会产生孤立 tool result；
6. 显式 required prefetch 的失败语义保持不变。
