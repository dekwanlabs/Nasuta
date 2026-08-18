# QA 对比问题的实体识别与证据覆盖修复提案

状态：核心机制已实施；离线回放、灰度观测与指标验收待执行  
作者：CodeLoom / Nasuta Agent Platform  
日期：2026-08-18  
关联事项：Trace `9d09b180705c`；用户问题“我们的 Agent 控制设备和 Google、Alexa 有什么区别，链路是什么样的”  
目标版本：当前分支 `feat/multi-agent-platform`

## 0. 实施记录（2026-08-18）

本提案的核心机制已在当前分支实现：

1. canonical query plan 和 TaskContract 保留比较对象、角色、别名与 required source；
2. LLM 任务图增加 entity × facet × source 的确定性覆盖校验，覆盖不足时使用通用 fallback；
3. Investigator 输入按对象、facet 和来源投影，并输出 `projection_empty` / `projection_insufficient`、缺失对象与缺失 facet；
4. Web 搜索结果显式输出 `source_usable` / `source_unusable`，只有成功抓取的正文才生成 canonical `web` evidence unit；
5. Verifier 按 comparison subject 独立计算 facet/source 覆盖，禁止一个对象的证据补另一个对象；
6. verified bundle v2 增加 `subject_coverage`，Synthesis 必须按对象分别组织结论并保留缺口；
7. capability 可执行但 required subject 证据不足时，工作流使用 `evidence_insufficient`，并已贯通运行状态、checkpoint 和 schema。

当前实现有意保持两层分类：`source_unusable` 是 Web 工具边界的来源质量状态；`evidence_insufficient` 是 Verifier/Workflow 的终止原因。这样无需为每个缺失实体或 facet 增加独立状态机，同时仍可从 `subject_coverage` 和 limitations 定位具体缺口。

尚未完成的上线工作：

- 使用原始 Trace `9d09b180705c` 和扩展回放集做离线重放；
- 按具体外部平台配置官方域名策略；通用 Nasuta 核心不硬编码 Google、Alexa 或项目业务域名；
- 建立提案中的 coverage/fallback/Web source 指标并执行 trace-only 灰度；
- 达到验收阈值后清理旧的宽泛状态和更新线上评测基线。

## 1. 摘要

本提案解决调查型 QA 在“多个系统、多个入口、多个来源”的对比问题中，漏掉核心比较对象、错误选择调查任务、证据投影不足，以及把来源不可用误报为能力不可用的问题。

触发案例中，用户实际要求比较三方：

```text
我们的设备控制 Agent
Google
Alexa
```

但查询计划没有将“我们的 Agent”解析为项目中的自有设备控制 Agent，例如
`hsds-aiot-service/device_assistant`、`PlanAgent`、`ExecSchedule` 和 `SetStatusAgent`。任务规划随后选择了 code + web，未稳定覆盖内部 docs 和 runtime。前置检索实际上已经找到部分内部代码、文档和服务信息，但没有被投影到正确的调查节点。Web Agent 虽然进程执行成功，却返回了大量首页、登录页和无关结果，最终被宽泛地归类为 `capability_unavailable`。

这不是某个关键词缺失、某次 Web 搜索失败或某个模型回答不够长导致的单点问题。机制层根因是：

1. 查询计划只识别了答案 facet，没有可靠识别比较对象及其角色；
2. 任务规划把证据来源当作 facet 的简单替代项，没有表达“内部证据必须存在、Web 只能补充”的来源约束；
3. LLM 任务图通过 schema 校验后，缺少实体级和来源级的确定性覆盖校验；
4. 检索结果到调查节点的投影没有对“每个对象、每个 facet、每个来源”建立可诊断的覆盖合同；
5. Web 结果质量和能力状态没有分层，导致 `source_unusable` 被误报成 `capability_unavailable`；
6. 验证器能够识别 partial evidence，但不能把“哪个比较对象完全缺失”作为一等缺口呈现给合成阶段。

本提案计划通过“实体角色解析、来源要求建模、确定性覆盖兜底、实体级证据投影和细化失败分类”改造调查规划链路，将当前流程从：

```text
自然语言问题
→ 识别 facet
→ LLM 选择少量任务
→ 代码和 Web 调查
→ 部分证据进入验证器
→ 输出不完整的两方分析
```

调整为：

```text
自然语言问题
→ 解析比较对象、对象角色和答案 facet
→ 为每个对象建立内部 code/runtime/docs 覆盖要求
→ 校验 LLM 任务图，不满足要求时使用确定性覆盖方案
→ 按实体、facet、source 投影证据，并对空投影补检索
→ 区分能力关闭、来源不可用和证据不足
→ 按三方结构合成，并显式暴露剩余缺口
```

预期实现以下效果：

1. 对比问题不会因为“我们的 Agent”是口语表达而漏掉自有系统；
2. 内部代码、文档和运行时链路不再被 Web 结果替代；
3. 调查节点可以解释自己覆盖了哪些对象、哪些 facet，以及缺少什么；
4. 最终回答能区分已证实、部分证实、推断和未验证内容；
5. 原始 trace 的失败会转化为可回归、可观测、可泛化的机制改进，而不是针对单个问题增加特判。

## 2. 背景

### 2.1 业务与技术背景

CodeLoom/Nasuta 的调查型 QA 用于回答代码、服务拓扑、内部文档和外部资料之间的复杂问题。此类问题通常不是查一个函数，而是要比较多个系统或入口的完整行为：谁接收请求、谁解释意图、谁把意图转换成设备状态、谁调用下游服务、状态如何维护，以及各入口做了哪些兼容。

本次问题属于典型的多对象对比问题：

```text
自有设备控制 Agent
→ 规划与执行设备意图
→ 设备控制工具
→ hsds-device-shadow 或相关下游
→ 设备影子 / 设备执行

Google Skill
→ Google webhook
→ intent / trait handler
→ 设备控制接口
→ hsas-voice / 下游影子服务
→ 设备影子

Alexa Skill
→ Alexa control endpoint
→ directive / namespace handler
→ 产品或标准控制器
→ hsas-voice / 下游影子服务
→ 设备影子
```

注意：上述三条链路是需要调查和对齐的目标结构，不代表每个中间路由在现有 trace 中都已被完整证明。特别是 `hsmf-mobile-gateway /api/voice/**` 到 `hsas-voice /voice/**` 的精确路由映射，在原 trace 中没有得到完整网关路由证据，后续回答必须保留该限制。

### 2.2 当前实现

调查流程由查询解析、证据规划、任务图规划、调查节点、证据 join、verifier 和 synthesis 组成。相关实现主要位于：

- `Nasuta/internal/domain/query_plan.go`：生成 canonical query plan、答案类型和实体候选；
- `Nasuta/internal/domain/entity.go`：规范化和限制实体标识；
- `Nasuta/internal/agent/qa/prepare.go`：准备查询并记录 canonical query plan；
- `Nasuta/internal/agent/qa/task_graph_plan.go`：根据 investigation goals、evidence goals 和 capabilities 请求 LLM 生成任务图；
- `Nasuta/internal/agent/investigation/capability.go`：把 source/facet 映射为调查 capability；
- `Nasuta/internal/agent/workflow/investigation.go`：定义 code、runtime、docs 等调查节点和工作流预算；
- `Nasuta/internal/agent/workflow/investigator_projection.go`：向调查节点投影受限上下文和证据种子；
- `Nasuta/internal/agent/workflow/verifier.go`：将调查报告绑定回 canonical evidence，并区分 supported、partial、unsupported 和 unresolved；
- `Nasuta/internal/agent/qa/route.go`：根据任务数量和并行性选择执行路线。

当前任务图规划规则允许 capability 覆盖多个 evidence goal，也允许多个 source 被视为同一 facet 的替代来源。该规则对“多个来源任选其一”的问题成立，但无法表达本次问题中的来源约束：

```text
内部代码 / 文档 / 服务拓扑：回答项目真实实现，属于必需证据
Google / Alexa 官方资料：解释外部协议，属于可选补充证据
```

### 2.3 触发案例

本次修改由 Trace `9d09b180705c` 触发。

- 触发时间：2026-08-18，具体时间以 `/Users/dequan.mac/agent-workspace/codeloom/logs/all-2026-08-18.log` 为准；
- 问题类型：多对象架构与链路对比；
- 用户目标：比较自有 Agent、Google 和 Alexa 的设备控制链路、指令兼容和差异；
- 规划结果：`kind=comparison`，required facets 包含 `business_domain`、`core_flow`、`data_and_state`、`external_dependency`，但 `entities=0`；
- 执行路线：`proposed=multi_agent`、`effective=multi_agent`、`path=durable_workflow`；
- 实际调查任务：`investigate.code.1`、`investigate.web.1`、`synthesize`；
- 前置检索：找到了 Google、Alexa、hsas-voice、hsds-aiot-assist 和设备控制文档等内容；
- 代码调查：形成了 Alexa、Google 和共享下游的部分链路，但没有形成自有 Agent 的完整证据；
- Web 调查：进程正常结束，但结果包含首页、登录页、无关内容以及 403/抓取失败；
- 验证结果：`supported_claims=0`、`partial_claims=6`，`business_domain` 和 `external_dependency` unresolved，停止原因被归类为 `capability_unavailable`。

### 2.4 范围与非目标

#### 目标

1. 为多对象对比问题建立稳定的实体角色解析和覆盖合同；
2. 保证内部架构问题至少覆盖 code、runtime、docs 三类内部证据；
3. 对 LLM 任务图进行实体级、facet 级和来源级确定性校验；
4. 改善调查证据投影，使每个节点能知道自己缺少哪些对象或 facet；
5. 将 Web 来源质量问题和真正的 Web capability failure 分开；
6. 为 verifier 和 synthesis 提供可解释的完整度信息；
7. 用原始 trace 建立回归测试，但不针对 trace ID、项目名称或单个问句增加硬编码。

#### 非目标

1. 本提案不补齐目标设备固件、第三方云平台内部实现或仓库外不可见的执行细节；
2. 本提案不把自有 Agent、Google 和 Alexa 强行抽象成一条实际不存在的统一链路；
3. 本提案不把 Web 搜索改造成内部代码、文档或服务拓扑的替代来源；
4. 本提案不通过无限提高模型 token、任务数或重试次数掩盖实体解析和证据覆盖缺陷；
5. 本提案不针对“我们的 Agent”“Google”“Alexa”等固定词写专用分支；
6. 本提案不新增持久化状态机；完整度可以由实体覆盖、节点结果和 verifier 结果派生。

## 3. 问题

### 3.1 问题描述

**期望行为：**

对于要求比较多个系统的调查问题，系统应当：

1. 识别所有需要比较的对象，并给出对象角色；
2. 为每个对象建立所需的证据 facet；
3. 选择能覆盖这些对象和 facet 的内部调查任务；
4. 将前置检索结果投影给负责该对象的节点；
5. 如果证据不足，明确指出具体对象、具体 facet 和具体来源缺口；
6. 最终回答按对象对齐，而不是把已查到的一方内容扩展成所有对象的结论。

**实际行为：**

本次执行中，系统识别出比较类型和 facet，但没有识别任何实体。LLM 任务规划器在最多三个调查任务的约束下选择了 code + web。内部 docs 和 runtime 没有成为独立调查任务，前置检索到的相关内部内容也没有完整投影给调查节点。Web Agent 虽然运行结束，但没有返回足够相关、可验证的官方资料。verifier 只能将已有 claim 标记为 partial，最终合成得到 Google/Alexa 的部分说明，无法完成三方比较。

**差异：**

系统把“任务图合法”误当成“调查目标已覆盖”，把“Web Agent 能运行”误当成“Web 来源可用”，又把“某些 facet 有证据”误当成“所有比较对象均有证据”。这三个判断之间缺少确定性边界。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 最终输出只有 Google/Alexa 的部分链路，`business_domain` 和 `external_dependency` unresolved | Trace 最终 verification 输出 |
| 直接原因 | 任务图选择 code + web，没有稳定启动 runtime/docs；Web 结果不可用；自有 Agent 未进入实体列表 | trace 中的 task graph、Web 结果和 `entities=0` 日志 |
| 机制根因一 | query plan 只保留 facet，不保留可供任务图校验的对象角色和对象覆盖关系 | `query_plan` 中实体为空，任务仍然被接受 |
| 机制根因二 | evidence source 在任务规划中主要表现为 facet 的替代选项，不能表达内部来源必须覆盖、Web 仅可选 | `task_graph_prompt` 的 source alternative 规则 |
| 机制根因三 | 任务图校验只验证 capability、goal ID 和任务数量，没有验证 required entity × facet × source 的覆盖矩阵 | `task_graph_plan.go` 的 draft validation |
| 机制根因四 | 前置检索结果投影没有对节点设定最小实体/证据要求，导致“有一个 seed”也可能被视为可执行 | `investigator_projection.go` 的 matched/dropped 统计 |
| 机制根因五 | Web 结果相关性、可抓取性和能力可用性未分层，导致 source unusable 被归类为 capability unavailable | Web Agent 结束状态和 verifier stop reason |
| 机制根因六 | verifier 以 claim/evidence 为中心，没有把缺失比较对象作为一等缺口传给 synthesis | `unresolved_goals` 没有对象维度 |

### 3.3 需要保持的现有能力

本提案不改变以下已有设计：

1. capability 仍由服务端控制，LLM 不能自行扩大工具和权限；
2. canonical evidence ledger 仍是 verifier 的唯一事实来源；
3. 调查节点仍然可以并行执行；
4. 任务数量、上下文 token 和工作流超时仍然受预算控制；
5. Web、内部代码、内部文档和 runtime 仍然是不同 evidence source，不合并成一种来源；
6. partial 和 unsupported claim 仍必须显式保留，不能为了生成完整答案而自动升级。

## 4. 问题出现的场景

### 4.1 多对象对比场景

用户用口语描述自有系统，并同时提到一个或多个外部系统：

```text
我们的 Agent 和 Google、Alexa 有什么区别？
自研助手和第三方语音平台下发设备命令的链路有什么不同？
内部设备控制和 Google Home、Alexa 的兼容层分别在哪里？
```

这些问法的共同结构是：

```text
对象集合 >= 2
答案形态 = comparison
需要横向对齐 = true
```

系统必须先解决“比较谁”，再解决“比较哪些 facet”。

### 4.2 内部链路依赖场景

问题需要说明实际项目中的服务调用、API 路由或存储落点：

```text
这个命令最后调用哪个服务？
Google 和 Alexa 是否复用同一个设备控制接口？
自有 Agent 到设备 shadow 的真实链路是什么？
```

此时内部 code、runtime 和 docs 是必需证据。即使外部 Web 完全不可用，也不能因此丢失项目内部链路。

### 4.3 前置检索已有结果但节点未覆盖场景

检索阶段已经返回相关服务、代码或文档，但任务投影只给某个节点少量 seed，或者因实体/来源匹配规则不一致而丢弃。此时日志可能显示 `code=completed`，但不能证明每个调查节点拿到了足够证据。

需要区分：

```text
检索没有找到
≠ 找到了但没有投影
≠ 投影了但节点没有使用
≠ 节点使用了但证据没有进入 canonical ledger
```

### 4.4 Web 结果不可用场景

Web capability 正常启动，但返回以下结果：

- 首页、登录页、搜索页或无关页面；
- 只有搜索摘要，没有可验证正文；
- 官方页面抓取失败、403 或超时；
- 页面可访问但与问题目标无关。

此时系统应当继续使用内部证据，并将 Web 结果标记为 `source_unusable`，不能把整个调查标记为 Web capability unavailable。

### 4.5 非目标场景

以下场景不在本提案的默认强制范围内：

1. 单一事实查询，例如“某个类在哪个服务中”；
2. 明确只要求外部平台官方文档的调研问题；
3. 没有项目实体、没有内部来源且用户明确要求泛行业回答的问题；
4. 需要实时系统观测才能回答的 incident 调查，这类问题仍需遵循 runtime evidence 的独立策略。

## 5. 如何修改

### 5.1 建立对象角色和实体覆盖模型

#### 5.1.1 增加对象角色

在 query plan / investigation contract 中，为实体增加可选角色。角色不是业务系统的持久化状态，而是本次问题中用于规划和验证的语义标签。

建议角色至少包括：

```text
first_party_agent       自有 Agent 或内部实现
external_adapter        Google、Alexa 等外部协议适配器
shared_service          多个对象共同依赖的服务
runtime_dependency      网关、影子服务、消息服务等运行时依赖
unknown                 尚未解析完成
```

实体结构建议保持轻量：

```go
type EntityRef struct {
    ID       string `json:"id"`
    Label    string `json:"label,omitempty"`
    Role     string `json:"role,omitempty"`
    Aliases  []string `json:"aliases,omitempty"`
    Source   string `json:"source,omitempty"`
}
```

已有 `EntityRef` 如果已经包含其中部分字段，应优先扩展现有结构，不建立平行实体类型。

#### 5.1.2 在查询入口解析实体

实体解析应发生在 canonical query plan 生成阶段。解析来源按可靠性排序：

1. 已识别的服务、仓库、模块和类名；
2. 索引中的服务描述、文档标题和实体别名；
3. 查询中的明确技术名词；
4. 历史上下文中已经确认的实体；
5. 对“自有系统”“内部 Agent”等角色性表达的项目实体候选。

对于“我们的 Agent”这类表达，不在 planner 中增加关键词特判，而是通过项目实体目录和角色映射得到候选：

```text
first_party_agent
→ hsds-aiot-service/device_assistant
→ PlanAgent / ExecSchedule / SetStatusAgent
→ hsds-device-shadow
```

如果存在多个候选，query plan 应保留候选和 unresolved 状态，让后续内部 discovery 任务负责消歧；不能静默选择一个未经证实的实体。

#### 5.1.3 对比问题生成对象覆盖要求

当 `QueryKind=comparison` 且实体数量大于等于两个时，生成对象级要求：

```go
type EvidenceSubject struct {
    EntityID string   `json:"entity_id"`
    Role     string   `json:"role,omitempty"`
    Facets   []string `json:"facets"`
    Sources  []string `json:"sources"`
}
```

最小要求为：

```text
每个比较对象至少覆盖 core_flow
如果问题涉及兼容或状态，则增加 data_and_state
如果问题涉及链路或依赖，则增加 external_dependency
```

共享服务可以作为独立实体验证，但不能替代上游比较对象的覆盖。

### 5.2 将“facet 覆盖”和“来源覆盖”分开建模

当前 `Sources` 容易被解释成“同一 facet 的可替代来源”。本提案将来源要求拆成 required、optional 和 forbidden/unsupported 三类，避免 Web 替代内部证据。

建议结构如下：

```go
type EvidenceSourcePolicy struct {
    Required []agentapi.EvidenceSource `json:"required,omitempty"`
    Optional []agentapi.EvidenceSource `json:"optional,omitempty"`
}

type EvidenceGoal struct {
    ID              string               `json:"id"`
    Facet           string               `json:"facet"`
    Required        bool                 `json:"required"`
    SourcePolicy    EvidenceSourcePolicy `json:"source_policy,omitempty"`
    MinimumCoverage int                  `json:"minimum_coverage,omitempty"`
}
```

对于本类内部对比问题，默认策略为：

| Facet | 内部 Code | 内部 Runtime | 内部 Docs | Web |
| --- | ---: | ---: | ---: | ---: |
| `core_flow` | 必须 | 可选 | 建议 | 不需要 |
| `data_and_state` | 必须 | 可选 | 建议 | 不需要 |
| `external_dependency` | 可选 | 必须 | 建议 | 可选 |
| `business_domain` | 可选 | 可选 | 必须 | 可选 |

这不是要求所有问题都启动全部节点，而是要求 planner 在选择任务时不能用 Web 覆盖掉必需的内部 source。

### 5.3 增加任务图的确定性覆盖校验和兜底

#### 5.3.1 LLM 仍负责选择任务，但不能决定是否覆盖完成

LLM planner 可以在允许的 capability 中选择并行任务，但服务端必须在接收 proposal 后验证：

```text
每个 required entity 是否被至少一个任务覆盖
每个 required facet 是否被至少一个任务覆盖
required source 是否有对应 capability
所有内部比较问题是否覆盖 code/runtime/docs 的必要组合
```

任务图 schema 合法不代表覆盖合法。

#### 5.3.2 默认确定性覆盖方案

当以下任一条件成立时，使用 deterministic evidence cover：

```text
LLM planner 不可用
LLM 返回无效任务图
required entity 未覆盖
required source 未覆盖
required facet 未覆盖
任务节点投影为空
```

内部多对象对比的默认覆盖方案为：

```text
investigate.code
investigate.runtime
investigate.docs
```

Web 作为 optional task，仅在以下条件之一成立时追加：

```text
用户明确要求官方协议或外部资料
external_dependency 的外部协议 facet 被显式要求
内部证据已满足，但需要补充平台语义
```

当前 `maxInvestigationTasks=3` 时，内部 code/runtime/docs 应优先占满三个调查槽位。若业务确实需要同时运行 Web，应采用基于目标数量和预算的显式扩容策略，而不是让 Web 静默替代内部节点。

#### 5.3.3 任务图日志

任务图日志至少记录：

```json
{
  "required_entities": ["..."],
  "required_facets": ["core_flow", "data_and_state"],
  "required_sources": ["internal"],
  "selected_tasks": [
    {"node": "investigate.code", "entities": ["..."], "facets": ["core_flow"]},
    {"node": "investigate.runtime", "entities": ["..."], "facets": ["external_dependency"]},
    {"node": "investigate.docs", "entities": ["..."], "facets": ["business_domain"]}
  ],
  "fallback": true,
  "fallback_reason": "required_internal_coverage_missing"
}
```

日志中不能只记录“任务数量”和“capability 数量”，因为这两个数字无法证明任务覆盖了正确对象。

### 5.4 改进检索结果到调查节点的投影

#### 5.4.1 投影匹配维度

投影应同时检查：

```text
entity
facet
source
reference / target
```

只按单个 seed 或单个关键词命中，不足以判定节点可执行。

#### 5.4.2 设置节点最小证据要求

建议默认最小要求：

| 节点 | 最小要求 |
| --- | --- |
| `investigate.code` | 至少一个目标比较对象的代码证据；多对象问题优先要求每个对象至少一个入口证据 |
| `investigate.runtime` | 至少一个服务、API 路由、调用关系或运行时拓扑证据 |
| `investigate.docs` | 至少一个相关内部文档或 runbook 证据 |
| `investigate.web` | 至少一个通过质量门槛的官方页面证据 |

如果节点没有达到最小要求，状态应为 `projection_insufficient` 或 `projection_empty`，而不是 `matched`。

#### 5.4.3 允许受控实体扩展

当用户实体没有精确命中时，检索可以沿实体目录展开：

```text
hsds-aiot-service/device_assistant
→ hsds-aiot-service
→ device_assistant
→ PlanAgent
→ ExecSchedule
→ SetStatusAgent
→ set_device_shadow
→ hsds-device-shadow
```

扩展必须受实体关系、索引元数据或服务拓扑约束，不能对所有词做无界模糊搜索。

#### 5.4.4 失败时触发补检索

投影发现某个 required entity 或 facet 缺失时，执行一次受控补检索：

```text
第一次：按 canonical entity + facet 精确检索
第二次：按实体别名、服务名、模块名和相关 API 扩展检索
仍无结果：记录 missing entity/facet，进入降级合成
```

补检索不能改变来源要求，也不能把 Web 结果升级为内部实现证据。

### 5.5 增加 Web 来源质量门槛和失败分类

#### 5.5.1 Web 结果质量

Web evidence 至少满足：

1. 域名在该 provider 的官方域名白名单中；
2. 页面标题或正文与目标平台和目标 facet 相关；
3. 页面不是首页、登录页、搜索页或无关跳转页；
4. 页面正文可抓取且非空；
5. 引用能够保存页面标识、抓取状态和内容摘要；
6. 403、超时和解析失败不能生成正常 evidence unit。

Google/Alexa 官方资料属于外部协议补充，不承担项目内部运行时链路的证明责任。

#### 5.5.2 细化状态

建议新增或统一以下分类：

```text
capability_disabled       能力未配置或明确关闭
capability_failed         能力已配置但执行失败
source_unusable           能力执行了，但结果无关、不可抓取或不可验证
evidence_insufficient     来源可用，但证据量或质量不足
required_entity_unresolved 必需比较对象未解析或未找到
required_facet_unresolved  必需 facet 未覆盖
projection_empty          节点没有匹配到证据种子
projection_insufficient   节点有结果但未达到最小覆盖
partial_due_to_coverage   有部分证据，但无法完成所有要求
```

`capability_unavailable` 不再作为所有不完整情况的兜底标签。若保留兼容值，应明确它只表示 capability 边界不可用，而不是来源质量不足。

### 5.6 将实体缺口纳入 verifier 和 synthesis

#### 5.6.1 Verifier 增加 subject coverage

Verifier 除了输出 claim support，还应输出：

```go
type SubjectCoverage struct {
    EntityID       string   `json:"entity_id"`
    CoveredFacets  []string `json:"covered_facets"`
    MissingFacets  []string `json:"missing_facets"`
    Sources        []string `json:"sources"`
    Complete       bool     `json:"complete"`
}
```

最终完整度由以下条件共同决定：

```text
required entity 已解析
∧ required entity 的 required facet 已覆盖
∧ required source 已达到最小覆盖
∧ claim 已绑定 canonical evidence
```

不能因为 Google 和 Alexa 的 core flow 有证据，就把自有 Agent 的 core flow 视为已覆盖。

#### 5.6.2 Synthesis 强制按对象对齐

对于 comparison 问题，合成输入应包含对象覆盖矩阵，并要求回答使用以下结构：

```text
1. 结论
2. 三方链路
3. 指令转换和兼容机制
4. 状态管理
5. 公共下游与差异点
6. 已证实、推断和未验证项
```

如果对象缺少证据，合成器必须在对应对象下写明“未充分验证”，不能用其他对象的证据填充。

## 6. 修改伪代码

### 6.1 查询解析与对象覆盖生成

```go
func resolveInvestigationContract(question string, context QueryContext) (TaskContract, error) {
    plan, err := resolveCanonicalQueryPlan(question, context)
    if err != nil {
        return TaskContract{}, fmt.Errorf("resolve query plan: %w", err)
    }

    entities := resolveEntities(
        question,
        plan.Entities,
        context.IndexedEntities,
        context.PreviousEntities,
    )

    goals := deriveEvidenceGoals(plan)
    subjects := make([]EvidenceSubject, 0, len(entities))
    for _, entity := range entities {
        subjects = append(subjects, EvidenceSubject{
            EntityID: entity.ID,
            Role:     entity.Role,
            Facets:   requiredFacetsForSubject(plan, entity),
            Sources:  requiredSourcesForSubject(plan, entity),
        })
    }

    if plan.Kind == QueryComparison && len(subjects) < 2 {
        // 不猜测缺失对象；保留 unresolved 状态，允许 discovery 任务补全。
        subjects = append(subjects, unresolvedComparisonSubject(question)...)
    }

    return TaskContract{
        Objective:        question,
        Entities:         entities,
        InvestigationGoals: plan.InvestigationGoals,
        EvidenceGoals:    goals,
        EvidenceSubjects: subjects,
    }, nil
}
```

### 6.2 任务图规划与确定性兜底

```go
func planTaskGraph(ctx context.Context, contract TaskContract) (TaskGraphProposal, error) {
    capabilities, err := taskGraphCapabilities(contract)
    if err != nil {
        return deterministicEvidenceCover(contract, "capability_resolution_failed")
    }

    draft, err := planWithLLM(ctx, contract, capabilities)
    if err == nil {
        proposal, bindErr := bindTaskGraphDraft(
            draft,
            capabilities,
            contract,
            maxInvestigationTasks,
        )
        if bindErr == nil && coverageSatisfied(proposal, contract) {
            return proposal, nil
        }
        err = fmt.Errorf("task graph does not satisfy subject/source coverage")
    }

    proposal, fallbackErr := deterministicEvidenceCover(
        contract,
        classifyPlanningFailure(err),
    )
    if fallbackErr != nil {
        return TaskGraphProposal{}, errors.Join(err, fallbackErr)
    }
    return proposal, nil
}

func coverageSatisfied(proposal TaskGraphProposal, contract TaskContract) bool {
    covered := makeCoverageSet(proposal.Tasks)
    for _, subject := range contract.EvidenceSubjects {
        for _, facet := range subject.Facets {
            if !covered.Contains(subject.EntityID, facet) {
                return false
            }
        }
    }
    for _, goal := range contract.EvidenceGoals {
        if !goal.Required {
            continue
        }
        if !covered.ContainsRequiredSource(goal.ID, goal.SourcePolicy.Required) {
            return false
        }
    }
    return true
}

func deterministicEvidenceCover(
    contract TaskContract,
    reason string,
) (TaskGraphProposal, error) {
    tasks := make([]TaskDirective, 0, 3)

    if requiresCapability(contract, "knowledge.code.inspect") {
        tasks = append(tasks, taskFor("knowledge.code.inspect", contract, "code"))
    }
    if requiresCapability(contract, "knowledge.service.trace") {
        tasks = append(tasks, taskFor("knowledge.service.trace", contract, "runtime"))
    }
    if requiresCapability(contract, "knowledge.docs.verify") {
        tasks = append(tasks, taskFor("knowledge.docs.verify", contract, "documentation"))
    }

    if len(tasks) == 0 {
        return TaskGraphProposal{}, fmt.Errorf("no deterministic evidence cover for %q", reason)
    }
    return TaskGraphProposal{
        Tasks: tasks,
        Metadata: map[string]string{
            "fallback": "true",
            "fallback_reason": reason,
        },
    }, nil
}
```

### 6.3 证据投影和补检索

```go
func projectInvestigatorInput(input Handoff, task TaskDirective) (ProjectionResult, error) {
    selected := selectEvidenceUnits(input.EvidenceUnits, func(unit EvidenceUnit) bool {
        return matchesAnyEntity(unit, task.Entities) &&
            matchesAnyFacet(unit, task.RequiredFacets) &&
            matchesAllowedSource(unit, task.AllowedSources)
    })

    missing := missingCoverage(task, selected)
    if len(missing) > 0 {
        extra, err := boundedEntityExpansionSearch(task, missing)
        if err != nil {
            return ProjectionResult{
                Status:             "projection_insufficient",
                MissingCoverage:    missing,
                FailureReason:      "supplemental_search_failed",
            }, nil
        }
        selected = mergeEvidence(selected, extra)
        missing = missingCoverage(task, selected)
    }

    status := "projection_matched"
    if len(selected) == 0 {
        status = "projection_empty"
    } else if len(missing) > 0 {
        status = "projection_insufficient"
    }

    return ProjectionResult{
        Status:          status,
        EvidenceUnits:   selected,
        MissingCoverage: missing,
    }, nil
}
```

### 6.4 Web 结果分类

```go
func classifyWebResult(result WebResult, policy WebSourcePolicy) WebEvidenceStatus {
    if result.CapabilityDisabled {
        return "capability_disabled"
    }
    if result.TransportError != nil {
        return "capability_failed"
    }
    if !policy.AllowedDomains.Contains(result.Domain) {
        return "source_unusable"
    }
    if result.IsHomepage || result.IsLoginPage || result.IsSearchPage {
        return "source_unusable"
    }
    if result.BodyEmpty || !relevantToTarget(result, policy.Target) {
        return "source_unusable"
    }
    return "usable"
}
```

### 6.5 Verifier 输出对象级覆盖

```go
func verifyBundle(bundle EvidenceBundle, contract TaskContract) VerificationResult {
    claims := bindClaimsToCanonicalEvidence(bundle)
    subjectCoverage := make([]SubjectCoverage, 0, len(contract.EvidenceSubjects))

    for _, subject := range contract.EvidenceSubjects {
        covered, missing := coverageForSubject(subject, claims)
        subjectCoverage = append(subjectCoverage, SubjectCoverage{
            EntityID:      subject.EntityID,
            CoveredFacets: covered.Facets,
            MissingFacets: missing.Facets,
            Sources:       covered.Sources,
            Complete:      len(missing.Facets) == 0,
        })
    }

    unresolved := unresolvedGoals(claims, subjectCoverage, contract.EvidenceGoals)
    return VerificationResult{
        Claims:          claims,
        SubjectCoverage: subjectCoverage,
        UnresolvedGoals: unresolved,
        Decision:        decisionFromCoverage(subjectCoverage, unresolved),
        StopReason:      classifyStopReason(subjectCoverage, unresolved),
    }
}
```

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. 对比问题进入任务规划时，系统能够保留比较对象及其角色，而不是只有 facet 数量；
2. 对“自有 Agent + Google + Alexa”类问题，至少能够生成 code、runtime、docs 三类内部调查任务；
3. Web 搜索失败不会使内部代码、文档和运行时调查失去覆盖；
4. 每个调查节点都能报告其覆盖对象、覆盖 facet、使用来源和缺失项；
5. verifier 能够识别“某个对象完全缺失”，而不仅仅是“某个 claim partial”；
6. synthesis 能够按三方结构输出，并保留精确网关路由等未被证据证明的限制。

### 7.2 失败和降级效果

实施后，类似失败应变成：

```text
required_entity_unresolved
或
projection_insufficient
或
source_unusable
```

而不是笼统的：

```text
capability_unavailable
```

当内部证据可用、Web 不可用时，系统仍应回答项目内已证实的链路，并明确说明外部协议补充缺失；不能返回空答案，也不能把外部资料缺失当成内部依赖缺失。

### 7.3 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `investigation.required_entity_count` | Gauge / trace field | 记录本次问题要求比较的对象数 |
| `investigation.unresolved_entity_count` | Counter / trace field | 记录未解析或未找到的对象数 |
| `investigation.coverage_matrix` | 结构化 trace | 记录 entity × facet × source 覆盖关系 |
| `investigation.task_fallback_total` | Counter | 统计确定性任务图兜底次数及原因 |
| `investigation.projection_status` | 结构化 trace | 区分 matched、empty、insufficient |
| `investigation.web_source_status` | Counter / trace field | 区分 capability_failed、source_unusable、usable |
| `investigation.subject_coverage` | 结构化 verifier 输出 | 记录每个比较对象的完整度 |

日志至少应回答：

- 系统识别了哪些比较对象，以及各自角色是什么；
- 每个对象需要哪些 facet 和来源；
- LLM 选择了哪些任务，是否触发确定性兜底；
- 每个节点拿到了哪些证据，缺了哪些证据；
- Web 是未配置、执行失败，还是结果不可用；
- 最终哪些结论有 canonical evidence 支持，哪些仍然是 partial 或 unresolved。

### 7.4 量化指标

初期以回放集建立基线，目标如下：

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 对比问题必需对象识别率 | 未建立 | ≥ 95% | 离线回放集 | query plan trace |
| 必需对象覆盖率 | 本案例为 2/3 或更低 | ≥ 95% | 离线回放集 | verifier subject coverage |
| 内部来源强制覆盖率 | 未建立 | 100% | 调查型对比问题 | task graph trace |
| 节点空投影后可解释率 | 未建立 | 100% | 所有调查运行 | projection trace |
| Web source unusable 误报 capability unavailable | 未建立 | 0 | Web 调查回放 | stop reason 统计 |
| 原始 trace 三方回答完整度 | 未完成 | 三方均有 core_flow；缺口显式列出 | 单案例回放 | 人工验收 + verifier |

这些目标是覆盖和可解释性目标，不要求模型在没有证据的情况下生成更完整的事实。

### 7.5 不应发生的变化

- 不改变单对象代码检索的既有结果和工具权限；
- 不把外部 Web 结果写入内部 runtime evidence；
- 不因增加实体和覆盖字段而允许无限制任务扩张；
- 不通过关键词特判修复原始问句；
- 不把 partial evidence 自动升级为 supported claim；
- 不改变已配置 Provider 的失败语义或静默替换规则。

## 8. 测试与验收

### 8.1 单元测试

#### 查询和实体解析

- “我们的 Agent”“自有设备控制 Agent”“内部设备助手”等表达能够解析到一个或多个 `first_party_agent` 候选；
- 服务名、模块名和类名可以形成稳定实体 ID；
- 多对象比较问题至少保留两个 subject；
- 对象无法确认时返回 unresolved，而不是静默丢弃；
- 单对象问题不被强制扩展成多对象比较。

#### 任务图规划

- LLM 返回合法但漏掉 required entity 的任务图时，服务端拒绝并使用 deterministic cover；
- Web capability 不能覆盖 required internal runtime source；
- 内部比较问题在三个任务预算内优先选择 code、runtime、docs；
- 任务图 fallback 记录明确原因；
- capability 不可用时返回明确错误或受控降级，不静默调用其他 Provider。

#### 证据投影

- entity、facet、source 全部匹配时状态为 `projection_matched`；
- 无任何 seed 时状态为 `projection_empty`；
- 有 seed 但缺少 required entity 或 facet 时状态为 `projection_insufficient`；
- 补检索能够使用受控实体别名扩展；
- 补检索失败不会伪造证据或提升完成度。

#### Web 质量

- 官方页面通过质量门槛；
- 首页、登录页、搜索页和无关页面标记为 `source_unusable`；
- 403/超时区分为 `capability_failed` 或对应 transport failure；
- Web 不可用时内部证据仍然可以完成验证。

#### Verifier

- Google、Alexa 有证据但自有 Agent 无证据时，自有 Agent 被列入 missing subject；
- partial claim 不进入 supported claims；
- subject coverage、facet coverage 和 source coverage 一致；
- stop reason 能区分 entity unresolved、projection insufficient 和 source unusable。

### 8.2 集成测试

1. 使用固定的内部 fixture 模拟自有 Agent、Google Skill、Alexa Skill、hsas-voice 和 hsds-device-shadow；
2. 验证从 query plan 到 task graph、projection、join、verifier、synthesis 的完整链路；
3. 删除 Web fixture，确认内部 code/runtime/docs 仍可回答内部链路；
4. 删除自有 Agent fixture，确认最终答案显式列出自有 Agent 缺口；
5. 只提供 Google/Alexa 代码，确认不能把其中一方的证据投影为另一方；
6. 验证任务预算、上下文预算和并行执行行为不被实体覆盖校验破坏；
7. 使用 GOWORK=off 运行 Nasuta 的窄范围测试、构建和 vet。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | Trace `9d09b180705c` 对应问题 | 识别自有 Agent、Google、Alexa；内部调查覆盖 code/runtime/docs | 回放日志与人工核验 |
| 多对象服务对比 | “订单服务和支付服务的调用链有什么不同” | 每个服务作为独立 subject，不能合并成单个服务结论 | 自动化任务图测试 |
| 内部链路无 Web | 内部架构问题，Web capability 关闭 | 内部证据仍可完成；不返回 capability unavailable | 集成测试 |
| Web 结果无关 | Web 返回首页、登录页、菜谱等结果 | Web source_unusable，内部调查继续 | Web fixture 测试 |
| 实体未解析 | 只说“那个服务”和“另一个服务” | 保留 unresolved，不编造服务 ID | 查询解析测试 |
| 单对象事实 | “DeviceService.deviceControl 在哪里实现” | 不启动不必要的三方比较任务 | 回归测试 |
| 部分证据 | 只找到 Google 入口和共享下游 | Google 部分支持；Alexa、自有 Agent 缺口显式保留 | verifier 测试 |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. 任务图在服务端通过 entity × facet × source 覆盖校验；
2. 原始 trace 回放时不再出现 `entities=0` 且任务图仍被接受的情况；
3. 原始 trace 至少执行或有明确结果记录：code、runtime、docs 三类内部调查；
4. 三个内部节点的投影状态和缺口可从 trace 中解释；
5. Web 无关结果不再被标记为 capability unavailable；
6. verifier 输出 subject coverage，并能指出缺失的自有 Agent 证据；
7. 最终回答不把一个对象的证据扩展到其他对象；
8. 原始 trace 的回归结果包含三方对比结构，未证实的网关路由等内容明确标注限制；
9. 单对象查询、非对比查询和已有工具权限行为不发生回归；
10. 通过 `GOWORK=off go test ./...`、`GOWORK=off go build ./...`、`GOWORK=off go vet ./...` 或记录已知环境阻塞。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| 实体别名误匹配 | 多个服务共享相似名称 | 任务调查错误对象 | 保留候选和角色置信来源；不确定时进入 discovery | 错误对象命中率上升 |
| 任务数增加导致延迟上升 | 内部对比问题强制 code/runtime/docs | 墙钟时间和 token 增加 | 并行执行；按问题 facet 裁剪；保留硬预算 | P95 超过既有上限 |
| 任务 fallback 过于保守 | LLM 任务图频繁被拒绝 | 调查成本增加 | 记录拒绝原因，按回放集调优 coverage contract | fallback 比例持续异常 |
| 投影补检索扩大上下文 | 实体别名过多或关联边过宽 | token 和噪声增加 | 受控别名、层级上限、节点最小证据数和总预算 | 投影噪声或预算超标 |
| Web 质量门槛过严 | 官方页面结构变化 | 外部补充资料减少 | 仅影响 optional Web source；内部证据不受影响 | 官方资料可用率异常下降 |
| verifier 暴露更多缺口 | 旧答案依赖模型推断 | 表面上的“完整率”下降 | 以证据完整度为准，分阶段更新评测基线 | 不能证明的 claim 被错误升级 |
| 应用策略上移 Nasuta | CodeLoom 的具体 Agent 别名进入通用核心 | Nasuta 与应用业务耦合 | Nasuta 只定义角色和 resolver contract，项目实体目录由应用注入 | 出现硬编码项目名或业务分支 |

## 10. 实施计划

### 阶段 1：覆盖校验和确定性兜底（核心实现已完成）

- [x] 明确 comparison subject 和 required source policy 的数据契约；
- [x] 在 `task_graph_plan.go` 增加 entity/facet/source coverage validation；
- [x] 实现内部 comparison 的 deterministic code/runtime/docs cover；
- [x] 记录 fallback 原因和覆盖诊断；
- [x] 添加多对象任务图回归测试；
- [ ] 使用原始 trace 做离线重放验收。

退出条件：LLM 返回漏掉自有 Agent 或内部 runtime/docs 的任务图时，服务端能够拒绝并生成可解释的兜底任务。

### 阶段 2：实体解析和证据投影（核心实现已完成）

- [x] 在 query plan ingress 规范化结构化实体、角色和别名；
- [x] 将实体角色传入 TaskContract；
- [x] 为调查任务设置最小投影要求；
- [x] 增加 `projection_empty` / `projection_insufficient` 状态和缺失对象/facet 诊断；
- [x] 添加节点投影回归测试；
- [ ] 基于应用侧实体目录继续扩充受控实体关系，不在 Nasuta 核心硬编码业务别名。

退出条件：三个内部调查节点能收到与其职责匹配的证据，空投影不再伪装成 matched。

### 阶段 3：Verifier、Synthesis 和 Web 状态治理（核心实现已完成）

- [x] 增加 `subject_coverage`；
- [x] 将 Web `source_unusable` 保留在来源边界，并以 gap/limitation 进入 verifier；
- [x] 将 subject 的实体/facet/source 不足统一归类为 `evidence_insufficient`；
- [x] 更新 comparison synthesis contract；
- [x] 只有成功抓取的 Web 正文生成 canonical evidence，搜索候选本身不能充当已验证证据；
- [ ] 由应用或 provider 策略配置官方域名规则；
- [ ] 建立离线回放集和覆盖率指标。

退出条件：最终回答能按对象对齐，且每个事实都能追溯到 canonical evidence 或显式标记为缺口。

### 阶段 4：灰度和清理（待执行）

- [ ] 对 comparison 查询启用 trace-only 观测；
- [ ] 对比新旧任务图的延迟、token、覆盖率和 fallback 比例；
- [ ] 达到验收阈值后默认启用；
- [ ] 清理仍然存在的宽泛 `capability_unavailable` 归类；
- [ ] 更新 Agent Platform 设计索引和相关运行手册。

退出条件：连续回放和线上观测满足指标，且没有单对象查询回归。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| 自有实体目录归属 | Nasuta 内置项目实体别名 | CodeLoom 通过 resolver 注入实体目录 | B | 避免可复用核心依赖具体业务仓库 |
| 来源要求表达 | 扩展现有 `Sources` 语义 | 增加 required/optional source policy | B | 兼容现有 alternatives，同时能表达内部来源必需 |
| 对比问题默认任务 | code + web | code + runtime + docs，Web 可选 | B | 内部架构事实只能由内部证据证明 |
| 任务图校验方式 | 继续完全信任 LLM proposal | LLM 选择 + 服务端确定性 coverage validation | B | 模型适合选择，服务端负责合同和权限 |
| Web 失败分类 | 继续使用 capability_unavailable | 细分 source_unusable、capability_failed 等 | B | 真实反映失败责任，避免错误诊断 |
| 最大调查任务数 | 固定 3 | 按 required source 和预算显式计算 | B | 三个内部节点是当前最低要求，外部补充需有预算依据 |

## 12. 决策摘要

本提案建议：

1. 将比较对象和对象角色纳入 canonical query/investigation contract；
2. 将 facet 覆盖与 source 覆盖分开建模，内部 code/runtime/docs 不被 Web 替代；
3. 保留 LLM 任务规划，但由服务端执行 entity × facet × source 确定性校验；
4. 规划失败或覆盖不足时使用通用 deterministic evidence cover，而不是针对原始问句增加特判；
5. 在证据投影阶段增加最小覆盖、实体扩展和可解释补检索；
6. 对 Web 结果质量和 capability 状态进行分层；
7. 将 subject coverage 传入 verifier 和 synthesis，禁止用一方证据填充另一方；
8. 先通过 trace-only 和离线回放验证，再逐步启用线上默认行为。

本提案不建议通过“增加 Web 搜索次数”或“把模型 token 上限调大”解决问题。真正需要修复的是：系统在开始调查前没有确认要比较哪些对象，也没有在执行前确认每个对象是否被正确来源覆盖。

## 附录 A：原始 trace 的预期修复后路径

修复后，原始问题的预期执行路径如下：

```text
用户问题
→ QueryKind=comparison
→ 实体解析：first_party_agent、Google adapter、Alexa adapter
→ 生成对象 × facet × source 覆盖矩阵
→ LLM 任务图校验失败或主动选择 code/runtime/docs
→ investigate.code 调查三方入口和指令转换
→ investigate.runtime 调查 hsas-voice、网关和 hsds-device-shadow 依赖
→ investigate.docs 调查业务域、状态和兼容设计
→ Web 可选调查 Google/Alexa 官方协议
→ projection 检查每个节点的实体/facet/source 覆盖
→ evidence join
→ verifier 输出 claim support + subject coverage
→ synthesis 按三方输出链路、兼容和差异
```

如果网关路由、设备执行或外部平台内部行为仍没有直接证据，最终答案应写为：

```text
已证实：代码和文档中明确出现的入口、服务调用和状态转换。
部分证实：由多个相邻证据拼出的调用关系，但缺少完整方法或网关路由证据。
未验证：仓库外平台、设备固件或没有直接代码/运行时证据的最后一跳。
```

## 附录 B：提案提交前检查清单

- [x] 背景说明了多对象调查和内部证据的职责边界；
- [x] 问题以“期望行为—实际行为—差异”描述；
- [x] 包含 Trace `9d09b180705c` 的可复现场景；
- [x] 区分了表面现象、直接原因和机制根因；
- [x] 修改方案明确实体、任务图、投影、Web 和 verifier 的职责；
- [x] 伪代码覆盖正常路径、失败路径和降级路径；
- [x] 预期效果包含可验证指标；
- [x] 说明了测试、灰度、风险和回滚；
- [x] 没有针对单个 trace ID、项目名或用户原句增加硬编码；
- [ ] 实现代码和自动化测试尚未提交；
- [ ] 评审人、目标版本和最终 rollout 配置待确定。
