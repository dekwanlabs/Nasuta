# 动态规划多 Agent 调查工作流 v2

状态：设计基线，待实现  
作者：Nasuta Agent Platform Team  
日期：2026-08-22  
适用范围：Nasuta 调查型 QA、多 Agent 调查、MCP 调查任务

## 1. 摘要

本文定义下一代调查型多 Agent workflow 的完整设计。它不是当前 workflow 的兼容性修补，也不是对 `forceConclusion`、`ConclusionRetryMaxTokens`、多层 output recovery 的继续扩展。

v2 将当前的“Agent 自由执行，最后强制总结，失败后层层恢复”改为一个由证据覆盖率驱动的闭环：

```text
用户问题
  -> 调查合同
  -> 动态计划
  -> 并行任务执行
  -> 证据归一化
  -> 声明验证
  -> 缺口评估
  -> 继续调查或合成答案
  -> 交付质量门
  -> 成功、部分成功或明确失败
```

设计目标不是让模型“无论如何都生成一个完整答案”，而是保证：

1. 每个确定性结论都可以追溯到已验证证据；
2. 每个未解决目标都有明确缺口和原因；
3. 动态计划受任务依赖、预算、时间和轮次约束；
4. 模型失败不会导致空答案；
5. `succeeded` 或 `partial` 结果永远包含非空用户可读文本；
6. 最终交付状态由系统质量门决定，而不是由某一个 Agent 自行宣布。

本文批准后，v2 作为新的实现基线。旧 workflow 不保留运行时兼容层；旧设计文档只作为历史记录。

## 2. 背景与根因

### 2.1 当前链路

当前系统已经包含动态任务、Investigator、Verifier、Synthesizer、workflow recovery 和 QA outcome 转换，但这些能力仍然叠加在原始 Agent Loop 之上：

```text
普通 Agent Loop
  -> 动态多 Agent
  -> forced conclusion
  -> reasoning retry
  -> definition recovery
  -> workflow evidence recovery
  -> QA outcome recovery
  -> public output conversion
```

这条链路让同一个概念在多个层次重复表达：

- 执行是否成功；
- 证据是否存在；
- 报告是否有效；
- 用户答案是否存在；
- workflow 是否完成；
- API 是否可以返回成功。

结果可能出现：

```text
模型调用失败
工作流内部标记成功
证据存在
自然语言答案为空
API 返回成功
```

### 2.2 当前空答案问题

典型日志：

```text
turn truncated during reasoning: max_tokens exhausted before any visible content
```

当前的实际行为可能是：

```text
第一次结论调用：max_tokens = 3000
模型全部消耗在 reasoning
可见答案 = ""
第二次重试：max_tokens = 750
模型仍然全部消耗在 reasoning
最终答案 = ""
```

问题不只是重试预算太小。更深层的根因是：

1. 最终答案生成被实现成执行循环结束后的补丁，而不是正式 workflow 阶段；
2. 模型输出、证据结果和公开交付状态共用模糊的 `Outcome`；
3. 证据恢复、结构化输出解析和 QA 状态转换分散在多个包；
4. 没有一个最终交付层对“成功结果必须有非空答案”负责；
5. prompt 中的“不要思考”被当成了 provider 级 reasoning 控制，但它并不具备协议保证。

### 2.3 v2 的根本改变

v2 不再将模型失败当作必须继续重试的信号，而是把它转换为一个可观测的阶段结果：

```text
Composer reasoning 截断
  -> 记录 composition failure
  -> 读取已验证 ClaimLedger
  -> 使用确定性 renderer 生成 partial answer
  -> 交付非空结果
```

模型负责调查、验证和语言组织；系统负责状态、预算、证据、终止和最终交付。

## 3. 设计原则

### 3.1 证据优先

用户答案只能从 `ClaimLedger` 和 `EvidenceLedger` 生成。原始工具输出、Agent 自由文本和未验证 finding 不能直接进入最终答案。

### 3.2 计划由缺口驱动

下一步任务由当前目标覆盖率、证据冲突、任务依赖和预算决定，而不是由固定 Agent 顺序决定。

### 3.3 角色职责单一

Planner 负责计划，Investigator 负责取证，Verifier 负责验证，Composer 负责表达，Delivery 负责交付。任何角色都不能越权承担其他角色的最终职责。

### 3.4 系统掌握终止权

Agent 可以提出继续调查，但只有 Coordinator 根据合同、覆盖率、预算和停止规则决定是否继续。

### 3.5 明确部分成功

证据不足不是空答案，也不是隐式成功。它必须成为显式的 `partial` 交付，并列出已确认内容、未解决目标和限制。

### 3.6 预算一次性管理

所有模型调用、工具调用、任务和轮次使用同一个运行级 `BudgetLedger`。不再由不同层各自派生一套隐式 token 上限。

### 3.7 Provider 能力显式建模

是否可以关闭 reasoning、是否支持结构化输出、是否支持工具调用，必须由 provider/model capability 表达。不能把自然语言 prompt 当作协议级控制。

### 3.8 不为失败样例过拟合

动态规划、证据评分、答案恢复和交付规则必须改善一整类问题，不能针对某个问题、服务名、实体名或 trace ID 写特殊分支。

## 4. 总体架构

```text
┌─────────────────────────────────────────────┐
│              Public API / MCP                │
│        接收问题、发送事件、返回交付结果        │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│          Investigation Coordinator           │
│      管理 Run 生命周期、预算、轮次和终止       │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│              Contract Builder                │
│       将问题转换为目标、范围和成功条件         │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│             Dynamic Planner                  │
│       按证据缺口生成、修订和停止任务计划        │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│          Scheduler / Task Executor           │
│       按依赖和预算并行执行边界清晰的任务        │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│       Evidence Normalizer / Verifier         │
│      归一化证据、验证声明、记录冲突和缺口        │
└──────────────────────┬──────────────────────┘
                       │
                 ┌─────┴─────┐
                 │           │
                 ▼           ▼
          继续调查        Answer Composer
                 │           │
                 └─────┬─────┘
                       ▼
                 Delivery Gate
                       │
          succeeded / partial / failed
```

### 4.1 层职责

| 层 | 负责 | 不负责 |
| --- | --- | --- |
| API/MCP | 请求、鉴权、事件和结果转换 | 计划、证据判定、模型重试 |
| Coordinator | Run 生命周期、预算、轮次、取消和终止 | 直接调用业务工具 |
| Contract Builder | 解析问题和生成调查目标 | 选择具体工具任务 |
| Planner | 生成和修订任务图 | 直接执行工具、生成最终用户答案 |
| Scheduler | 依赖检查、并行度和预算申请 | 修改业务结论 |
| Investigator | 执行一个边界明确的调查任务 | 宣布整个调查完成 |
| Evidence Normalizer | 将工具结果转换成标准证据 | 推断用户结论 |
| Verifier | 验证声明与证据关系 | 生成长篇用户答案 |
| Composer | 组织已验证声明 | 再次调查、补充外部事实 |
| Delivery | 校验、降级渲染、决定公开状态 | 修改证据事实 |

## 5. 核心领域模型

### 5.1 InvestigationRun

一次用户调查是一个持久化 Run：

```go
type InvestigationRun struct {
    ID         string
    Question   string
    Contract   InvestigationContract
    Status     RunStatus
    Round      int
    Plan       PlanSnapshot
    Budget     BudgetState
    Evidence   EvidenceLedger
    Claims     ClaimLedger
    Gaps       []EvidenceGap
    Answer     *DeliveredAnswer
    Failure    *RunFailure
}
```

Run 必须能在进程重启后根据持久化快照恢复。恢复依据是已保存的事实和状态，不依赖某个内存 Agent 是否还存在。

### 5.2 InvestigationContract

Contract 是一次调查的不可变约束：

```go
type InvestigationContract struct {
    Question        string
    Scope           InvestigationScope
    RequiredGoals   []EvidenceGoal
    SuccessCriteria []SuccessCriterion
    AllowedSources  []EvidenceSource
    MaxRounds       int
    MaxTasks        int
    Deadline        time.Duration
}
```

对于“分析 AI 集成方式和技术栈”这类问题，Contract 可以包含：

```text
业务领域
核心流程
AI 入口和调用链
数据与状态
外部依赖
运行与运维
系统边界
技术组件证据
```

每个目标有自己的覆盖状态。一个目标没有证据，不应该使其他目标的有效结果消失。

```go
type EvidenceGoal struct {
    ID              string
    Description     string
    Required        bool
    MinimumCoverage int
    AcceptedSources []EvidenceSource
    DependsOn       []string
}
```

### 5.3 Plan 与 Task

计划是带依赖关系的任务图，而不是 Agent 名单：

```go
type Plan struct {
    Revision int
    Tasks    []TaskSpec
    Edges    []TaskDependency
    StopRule StopRule
}

type TaskSpec struct {
    ID                  string
    Role                TaskRole
    Objective           string
    RequiredGoals       []string
    InputEvidence       []EvidenceRef
    AllowedTools        []ToolID
    OutputSchema        SchemaRef
    MaxTokens           int
    MaxToolCalls        int
    Priority            int
    IndependentlyUseful bool
}
```

Task 是一次受限执行，不是一个拥有全局上下文的通用 Agent。它只能看到 Contract 中允许的目标、输入证据和工具。

### 5.4 EvidenceUnit 与 EvidenceLedger

证据是系统的一等数据：

```go
type EvidenceUnit struct {
    ID          string
    SourceKind  string
    Target      string
    Locator     string
    Summary     string
    ContentHash string
    CapturedAt  time.Time
    TrustTier   int
    Coverage    []string
    Provenance  EvidenceProvenance
}

type EvidenceLedger struct {
    Units     map[string]EvidenceUnit
    Conflicts []EvidenceConflict
    Coverage  map[string]GoalCoverage
}
```

要求：

1. Evidence ID 稳定且可去重；
2. 证据必须有来源、目标和定位信息；
3. 同一证据重复出现时合并，不重复计入覆盖率；
4. 不同版本或不同时间范围的证据不能错误合并；
5. 冲突证据保留两侧，不静默覆盖；
6. Claim 只引用 Evidence ID，不复制大段原始内容。

### 5.5 ClaimLedger

```go
type VerifiedClaim struct {
    ID         string
    Statement  string
    Status     ClaimStatus
    Evidence   []EvidenceRef
    Confidence float64
    Conflicts  []ConflictRef
    Missing    []string
}

type ClaimLedger struct {
    Claims      map[string]VerifiedClaim
    ByGoal      map[string][]string
    Unsupported []UnsupportedClaim
}
```

Claim 状态：

```text
supported
partial
unsupported
conflicted
```

只有 `supported` 和符合交付策略的 `partial` Claim 可以进入最终答案。`unsupported` 和 `conflicted` 只能出现在限制或缺口说明中。

### 5.6 DeliveredAnswer

```go
type DeliveredAnswer struct {
    Text        string
    Status      DeliveryStatus
    Claims      []VerifiedClaim
    References  []EvidenceRef
    Gaps        []EvidenceGap
    Limitations []Limitation
    Failure     *DeliveryFailure
}
```

交付状态：

```text
succeeded
partial
failed
cancelled
```

硬性不变量：

```text
succeeded 或 partial => strings.TrimSpace(Text) != ""
failed 或 cancelled   => 必须存在明确 Failure 或取消原因
```

## 6. Run 生命周期与状态机

这里使用状态机是因为 Run 需要持久化、恢复、超时、取消和并发控制；这些生命周期不能仅由当前字段临时推导。

状态：

```text
created
analyzing
planned
executing
verifying
replanning
composing
delivered
failed
cancelled
timed_out
budget_exhausted
```

允许转换：

```text
created      -> analyzing
analyzing    -> planned | failed
planned      -> executing | composing | failed
executing    -> verifying | timed_out | cancelled | failed
verifying    -> replanning | composing | failed
replanning   -> planned | composing | failed
composing    -> delivered | failed
```

终态：

```text
delivered
failed
cancelled
timed_out
budget_exhausted
```

禁止：

```text
delivered -> executing
failed -> executing
composing -> replanning
```

说明：`executing`、`verifying` 和 `composing` 是真正的阶段状态；“模型是否可用”“是否已重试”“是否已有部分答案”由任务结果、预算和 Ledger 推导，不再持久化为额外状态机。

## 7. 动态规划

### 7.0 Task Template Catalog 的来源与职责

`TaskTemplateCatalog` 不是由 LLM 在每次请求中临时发明的任务列表，也不是一次
retrieval 的结果。它是由平台维护、版本化、启动时注册的能力目录。目录中的每个
模板代表一种可以重复执行、权限可审计、输出可验证的调查动作。

模板不是对用户问题的无限穷举，而是少量可组合的调查积木。目录优先定义通用
动作，例如“搜索”“定位实体”“查看符号”“追踪调用/依赖”“比较证据”“验证声明”，
再用参数绑定到具体问题。用户问题不需要提前出现在目录中，Planner 只需要把问题
拆成这些通用动作的组合。

模板的确定遵循以下规则：

1. 有稳定的 EvidenceGoal 类型，例如 `ai_entrypoint`、`model_provider`、
   `external_dependency`、`runtime_failure_path`；
2. 有明确的证据来源和工具链，能够说明“从哪里取证”；
3. 有固定的输入、输出和失败语义，不能只返回一段无法验证的自由文本；
4. 有最小权限集合、前置条件、停止条件和成本上限；
5. 能通过离线契约测试、Schema 测试和权限测试。

初始模板由平台工程师根据工具注册表、证据来源类型和高频调查动作编写，例如：

```text
code.find_ai_entrypoint
code.trace_call_chain
config.find_model_provider
api.list_external_endpoints
docs.find_ai_integration
runtime.find_failure_path
workspace.resolve_entity
```

线上 trace、失败任务和重复出现的 EvidenceGap 可以作为新增模板的候选来源，
但只能经过人工评审、版本化和测试后进入目录。运行中的 Planner 不能创建新的
`task_type`、工具 ID 或输出 Schema。

如果问题不匹配任何一个专用模板，Planner 可以使用通用的
`investigation.explore` 模板，并把目标、范围和证据来源作为参数传入。它仍然只能
使用工具注册表中已有的工具，并返回统一的 `investigation.report`。如果现有工具
和通用模板都无法覆盖目标，系统应返回“当前能力不足”的明确缺口，而不是让 LLM
临时创造一个没有执行器和校验规则的任务类型。

模板至少包含以下元数据：

```json
{
  "id": "code.find_ai_entrypoint",
  "version": 1,
  "goal_kinds": ["ai_entrypoint"],
  "source_kinds": ["source_code"],
  "required_inputs": ["entity", "scope"],
  "tool_grant": ["search_code", "get_symbol", "trace_calls"],
  "input_schema": {"id": "task.input", "version": 1},
  "output_schema": {"id": "investigation.report", "version": 1},
  "preconditions": ["entity_is_resolved"],
  "cost_profile": {
    "max_input_tokens": 4000,
    "max_output_tokens": 2500,
    "max_tool_calls": 6,
    "max_duration_seconds": 45
  }
}
```

模板是“能力定义”，不是任务实例。Planner 将模板与当前 Contract 中的 goal、
实体、范围和依赖绑定后，才生成 `TaskCandidate`：

```json
{
  "task_id": "task_code_entrypoint_001",
  "template": {"id": "code.find_ai_entrypoint", "version": 1},
  "goal_ids": ["G3"],
  "bindings": {
    "entity": "hsas-aiot-service",
    "scope": "workspace"
  },
  "depends_on": [],
  "allowed_tools": ["search_code", "get_symbol", "trace_calls"],
  "output_schema": {"id": "investigation.report", "version": 1}
}
```

因此，候选任务的拆解位置是：`planning/catalog.go` 负责目录查询，
`planning/planner.go` 负责绑定和排序，`planning/compiler.go` 负责把候选任务
编译成可执行任务。候选任务和被拒绝的候选任务都必须写入对应的
`plan_revision`，不能只存在于 LLM 输出中。

### 7.1 初始规划

Contract Builder 先从用户问题生成 EvidenceGoal，Planner 查询已注册的模板目录，
再绑定实体和参数。初始计划不依赖先做业务 retrieval；检索是任务执行阶段由
Investigator 通过 `search_code`、`get_symbol`、`trace_calls` 等工具完成的。

完整顺序是：

```text
用户问题
  -> Contract
  -> EvidenceGoal
  -> Catalog 查询
  -> 实体/范围绑定
  -> TaskCandidate
  -> PlanCompiler
  -> ExecutableTask
  -> Investigator 调用 retrieval 工具
  -> EvidenceNormalizer / Verifier
```

这里的 Catalog 查询只是查询本地能力注册表，不是从知识库召回业务证据。
如果实体尚未明确，Planner 选择 `workspace.resolve_entity` 或其他发现类模板，
而不是先把整个知识库检索一遍。

初始计划必须满足：

1. 每个 required goal 至少有一个候选任务；
2. 每个候选任务都来自已激活、已版本化的模板；
3. 任务的工具权限属于 Contract 允许范围；
4. 任务依赖无环；
5. 任务总成本不超过当前 Run 和当前阶段的预算预留；
6. 每个任务都有独立的输出 Schema；
7. 任务失败后仍能返回明确 failure，而不是空报告。

### 7.1.1 初始规划与重规划的区别

候选任务有两个产生时机：

```text
初始规划：在第一次 retrieval 之前，根据 Contract 和模板目录生成第一批候选任务
重规划：在前一批任务完成、证据归一化和验证之后，根据新的缺口再次查询模板目录
```

所以答案不是“候选任务都在 retrieval 之后生成”，而是：

```text
第一批候选任务：retrieval 之前
后续候选任务：可以由前一轮 retrieval 暴露的缺口、新实体、冲突触发
```

重规划只能读取压缩后的 EvidenceLedger、ClaimLedger 和 Gap 摘要，不能把无限增长
的原始工具正文重新塞入 Planner。

### 7.1.2 任务不等于 Agent

动态规划产生的是任务节点，不是“每个节点启动一个新 Agent”。Scheduler 根据任务
的 `executor` 选择执行方式：

```text
direct_tool       直接调用一个确定性工具，不启动 Agent
tool_pipeline     按固定顺序调用多个工具，不启动 Agent
investigator      需要多步判断和工具选择时，启动一个 Investigator Agent
verifier          使用规则或一个共享 Verifier 完成证据检查
composer          在最后阶段启动 Composer，生成用户答案
```

例如，五个简单的代码查询任务可以直接并行调用工具，不需要五个 Agent。只有当
任务需要根据中间结果决定下一步、理解复杂调用关系或处理语义冲突时，才启动
Investigator Agent。Agent 数量由并发限制和 RunBudget 决定，不由任务数量直接决定。

因此一轮运行可能是：

```text
2 个 direct_tool 任务
1 个 tool_pipeline 任务
1 个 Investigator Agent
1 个共享 Verifier
1 个 Composer
```

而不是每个任务都创建一个独立 Agent。

### 7.2 重规划输入

每轮 Planner 只读取压缩后的事实：

```text
InvestigationContract
当前 EvidenceLedger 摘要
当前 ClaimLedger 摘要
未覆盖 EvidenceGoal
证据冲突
已失败任务及失败原因
剩余预算、时间和轮次
已执行任务摘要
```

Planner 不直接读取无限增长的原始工具文本。大内容必须先由 Evidence Normalizer 归一化和限量。

### 7.3 重规划输出

```go
type PlanDecision struct {
    Action       PlanAction
    NewTasks     []TaskSpec
    CancelTasks  []string
    MissingGoals []string
    Reason       string
}
```

动作：

```text
execute_tasks
replan
compose
deliver_partial
fail
```

Planner 必须说明每个新增任务：

```text
覆盖哪些目标
依赖哪些证据
预计成本
预期新增信息
失败后的独立价值
```

### 7.4 任务价值评分

任务选择采用显式评分，模型建议不能绕过系统预算：

```text
task_score =
    goal_coverage_value
  + source_reliability
  + dependency_unlock_value
  + independent_usefulness
  - estimated_token_cost
  - duplicate_risk
  - failure_risk
```

低于阈值的任务不启动。评分只用于任务排序和准入，不直接伪造证据覆盖。

### 7.5 重规划触发条件

以下任一条件触发重规划：

1. required goal 仍未覆盖；
2. 新证据发现新的实体、服务或调用边；
3. 发现证据冲突；
4. 任务失败但存在替代来源；
5. 后续任务输入发生变化；
6. 当前计划的预期收益低于继续调查阈值；
7. 剩余预算不足以完成所有目标；
8. 达到最大轮次。

达到停止条件后，不再启动新任务，直接进入合成或部分交付。

## 8. Agent 契约

### 8.1 Planner

输入：Contract、Ledger 摘要、BudgetState、执行历史。

输出：PlanDecision。

允许：

```text
拆分目标
选择任务
调整依赖
取消低价值任务
决定合成或部分交付
```

禁止：

```text
直接调用业务工具
把未验证推断写入 ClaimLedger
直接生成用户答案
绕过预算和停止条件
```

### 8.2 Investigator

Investigator 只执行一个边界清晰的任务：

```go
type InvestigationReport struct {
    TaskID       string
    Findings     []Finding
    Evidence     []EvidenceRef
    Unresolved   []UnresolvedItem
    ToolFailures []ToolFailure
    Status       TaskReportStatus
}
```

`Finding` 必须包含声明和证据引用：

```go
type Finding struct {
    Statement  string
    Evidence   []EvidenceRef
    Confidence float64
    Scope      string
}
```

如果没有找到内容，必须明确返回：

```text
no_evidence
source_unavailable
query_mismatch
out_of_scope
tool_failed
```

不能返回空 JSON 让上层猜测含义。

### 8.3 Evidence Normalizer

Evidence Normalizer 是确定性组件，负责：

```text
工具结果 -> EvidenceUnit
证据去重
内容摘要
稳定 ID
来源和定位标准化
覆盖目标归属
冲突候选识别
```

它不调用 LLM，也不生成用户结论。

### 8.4 Verifier

Verifier 检查：

1. Finding 是否有证据；
2. 证据是否属于当前问题范围；
3. 证据是否满足来源要求；
4. 是否存在冲突或重复；
5. Claim 是否达到目标所需覆盖率；
6. 哪些字段仍然缺失。

Verifier 输出 VerifiedClaim 和 EvidenceGap，而不是长篇答案。

### 8.5 Composer

Composer 只能读取：

```text
InvestigationContract
VerifiedClaim
EvidenceLedger 摘要
EvidenceGap
Limitation
```

Composer 不得：

```text
再次调用调查工具
补充输入中不存在的事实
把 unsupported 或 conflicted claim 写成确定结论
自行决定整个 Run 成功
```

输出：

```go
type AnswerDraft struct {
    Summary      string
    Findings     []AnswerFinding
    Limitations  []Limitation
    MissingGoals []EvidenceGap
    References   []EvidenceRef
}
```

## 9. 调查闭环

完整运行流程：

```text
1. CreateRun
2. AnalyzeQuestion
3. BuildContract
4. InitialPlan
5. ReserveRoundBudget
6. ScheduleReadyTasks
7. ExecuteTasks
8. NormalizeEvidence
9. VerifyClaims
10. UpdateCoverage
11. EvaluateStopRule
12. Replan 或进入 Compose
13. ValidateAnswerDraft
14. DeliveryGate
15. PersistTerminalResult
16. EmitTerminalEvent
```

核心控制伪代码：

```go
func Run(ctx context.Context, request Request) DeliveredAnswer {
    run := coordinator.Create(request)

    contract, err := contractBuilder.Build(ctx, request)
    if err != nil {
        return delivery.RenderFailure(contract, err)
    }
    run.SetContract(contract)

    for run.CanContinue() {
        decision, err := planner.Plan(ctx, PlanInput{
            Contract: contract,
            Evidence: run.Evidence.Summary(),
            Claims:   run.Claims.Summary(),
            Budget:   run.Budget.Snapshot(),
            History:  run.History.Summary(),
        })
        if err != nil {
            run.RecordFailure("planning_failed", err)
            break
        }

        switch decision.Action {
        case PlanExecuteTasks:
            reports := scheduler.Execute(ctx, decision.NewTasks)
            run.Evidence.Merge(normalizer.Normalize(reports))
            run.Claims.Merge(verifier.Verify(ctx, contract, run.Evidence))
        case PlanCompose, PlanDeliverPartial:
            goto compose
        case PlanFail:
            run.RecordFailure("planner_stopped", errors.New(decision.Reason))
            goto compose
        }
    }

compose:
    draft, composeErr := composer.Compose(ctx, ComposeInput{
        Contract: contract,
        Evidence: run.Evidence.Summary(),
        Claims:   run.Claims.Summary(),
        Gaps:     run.Gaps(),
    })

    return delivery.Deliver(DeliveryInput{
        Contract:     contract,
        Evidence:     run.Evidence,
        Claims:       run.Claims,
        Gaps:         run.Gaps(),
        Draft:        draft,
        ComposeError: composeErr,
    })
}
```

伪代码表达生命周期，不代表最终实现必须保留同样的函数名或同步执行方式。

## 10. 预算与资源治理

### 10.1 BudgetLedger

预算不是某个 Agent 自己决定的 token 数，也不是 Planner 可以在运行中随意扩大的
常量。它有四个层次：

```text
PlatformSettings / BudgetPolicy
  -> RunBudget 快照
      -> StageBudget 阶段预留
          -> TaskBudget 任务授予
```

它们的职责不同：

| 层次 | 谁决定 | 作用 | 是否可以在 Run 中扩大 |
| --- | --- | --- | --- |
| `PlatformSettings` | 平台管理员 | 规定系统硬上限、默认 profile 和价格/时间约束 | 否 |
| `RunBudget` | Coordinator 在创建 Run 时 | 把平台策略和本次 profile 固化成一次运行的预算快照 | 否 |
| `StageBudget` | Coordinator 根据 RunBudget 预留 | 为 planning、investigation、verification、composition 和 fallback 留出额度 | 只能在未使用额度内重分配 |
| `TaskBudget` | Scheduler 根据模板成本和剩余额度授予 | 限制一个任务的输入、输出、工具调用和时间 | 否 |

模板里的 `cost_profile` 不是平台预算，也不是任务一创建就自动获得的额度。它只
说明这个任务的预估成本和单任务安全上限；真正授予多少，要由 Scheduler 根据
PlatformSettings、当前 Run 剩余额度和当前阶段剩余额度计算。

Run 创建后必须持久化预算策略版本和快照。这样平台设置之后发生变化，也不会让
同一个 Run 在恢复时获得不同的资源。

预算应按多个维度同时限制，不能只看输出 token：

```go
type BudgetVector struct {
    InputTokens  int64
    OutputTokens int64
    ToolCalls    int
    Duration     time.Duration
    CostMicros   int64
}

type RunBudget struct {
    Caps            BudgetVector
    Reserved        BudgetVector
    Used            BudgetVector
    MaxRounds       int
    MaxTasks        int
    PolicyVersion   string
    Profile         string
}
```

其中 `LLMAnswerMaxTokens` 只能解释为“一次 LLM 调用的输出上限”，不能直接当作
整个 Run 的 `TotalTokens`；`LLMContextWindow` 是单次调用的上下文容量，也不是
可消费预算。当前平台中的 `AgentTimeout`、`AgentMaxToolCalls` 和
`DelegationMaxTotalTokens` 可以作为迁移时的输入，但 v2 应提供语义明确的
`InvestigationMaxDuration`、`InvestigationMaxToolCalls`、
`InvestigationMaxOutputTokens`、`InvestigationMaxRounds`、
`InvestigationMaxTasks` 和 `InvestigationMaxCostMicros` 等设置，避免继续复用
一个字段表达多个生命周期。

因此，答案是：预算由 PlatformSettings 提供上限和默认策略，由系统在 Run 创建时
计算和冻结；不是写死在 Agent 代码里，也不是单个 Agent 的预算。请求方最多只能
选择一个不超过平台硬上限的 profile，不能直接把本次 Run 的上限调大。

任务执行前申请预算：

```go
reservation, err := budget.Reserve(BudgetRequest{
    InputTokens:  estimatedInput,
    OutputTokens: estimatedOutput,
    ToolCalls:    expectedToolCalls,
})
```

任务完成后结算实际使用量：

```go
budget.Settle(reservation, actualUsage)
```

预算申请失败时：

```text
不再启动新任务
尝试使用已验证证据合成
无法合成时使用 deterministic renderer
```

### 10.2 预算预留

最终 Composer 或确定性交付所需的资源必须在 Run 创建时预留，不能等调查任务消耗完所有 token 后再“强行总结”。

预算准入的正确含义是：

```text
sum(active task reservations) + scheduler overhead
    <= min(RunBudget.remaining, current StageBudget.remaining)
```

并且每个任务还必须满足：

```text
actual usage <= TaskBudget
```

初始计划的最低可行预算为：

```text
planning reserve
+ verification reserve
+ composition reserve
+ deterministic fallback reserve
+ 每个 required goal 的最便宜可行候选任务 reservation
```

如果这个总量已经超过 RunBudget，PlanCompiler 不应启动任务，而应直接生成
`budget_insufficient` 的部分交付或失败结果。这样“每个 required goal 至少一个
候选任务”和“不能超预算”不会互相矛盾。

第一版可以提供一个显式的默认 profile，下面的比例只是 profile 的起始策略，
不是 Planner 中的隐藏常量：

```text
问题分析和初始规划：10%
调查任务：55%
验证和缺口分析：15%
最终合成：15%
确定性降级和系统开销：5%
```

其中 composition 和 fallback 是硬预留，调查任务不能借用；planning、
investigation 和 verification 可以在每轮结算后把未使用额度返还到 Run 的未分配
池，再由 Coordinator 根据未覆盖目标和任务价值重新分配。任务模板中的
`cost_profile` 只用于估算和上限约束，Planner 的自然语言“预计成本”不能突破
模板上限。

预算数值不应凭感觉永久写死，初始值按以下顺序确定：

1. 先由产品 SLO 决定交互请求允许的最大时延、最大费用和最大并发；
2. 再由平台管理员把这些 SLO 写成 Run 级硬上限；
3. 根据每个模板的历史 p50/p95 输入 token、输出 token、工具调用和耗时，维护
   `cost_profile`；
4. 用最便宜候选任务覆盖每个 required goal，预留 Composer 和 fallback 后，检查
   初始计划是否可行；
5. 通过真实 Run 的预算消耗、覆盖率和交付质量指标调整 profile，而不是通过不断
   增大 token 或 retry 次数解决问题。

第一版实现可以先只开放 `interactive` 和 `deep` 两个 profile；profile 中的具体
   数值必须落在 PlatformSettings 的硬上限内，并在配置快照中可见。没有经过压测
   的数值只能作为默认起点，不能被当成所有问题都适用的“正确预算”。

实际分配由运行配置和 Coordinator 决定，但必须满足：

1. 每个阶段都有上限；
2. 所有任务共用同一个运行账本；
3. 单个任务不能抢占其他阶段的预留；
4. 重试必须从当前阶段预算中支付；
5. 不能通过隐式默认值扩大总预算。

### 10.3 Reasoning 模型

不再依赖以下 prompt 来关闭模型思考：

```text
Do not think or reason.
```

支持的处理方式按优先级排序：

1. Provider/model 明确支持时使用协议级 thinking/reasoning 参数；
2. 调查阶段使用 reasoning model，Composer 路由到非 reasoning model；
3. Composer reasoning-only 或截断时直接进入 deterministic delivery；
4. 最多允许有限的、预算内的同阶段重试，不允许无限恢复链。

## 11. 交付质量门

### 11.1 质量检查

Delivery Gate 必须检查：

```text
答案文本非空
答案中的 Claim 都存在于 ClaimLedger
Claim 的 Evidence 引用都存在于 EvidenceLedger
引用不超出当前问题 Scope
unsupported/conflicted Claim 没有被表达为确定事实
required goal 的完成状态被正确反映
失败和限制信息没有被丢弃
```

### 11.2 三种公开结果

#### succeeded

```text
required goals 满足
确定性结论都有有效证据
答案非空
```

#### partial

```text
部分目标满足
存在明确未解决目标或限制
答案非空
```

#### failed

```text
没有可以安全交付的证据或答案
存在明确失败原因
```

### 11.3 确定性 Renderer

新增独立的 `AnswerRenderer`：

```go
type AnswerRenderer interface {
    Render(DeliveryInput) (DeliveredAnswer, error)
}
```

渲染策略：

```text
有合法 AnswerDraft：
    校验并交付 draft

Composer 失败但有 supported claims：
    根据 claims 生成部分答案

只有 partial claims：
    生成“当前只能确认以下部分信息”的答案

只有 evidence，没有 claims：
    生成“已获取证据，但尚未形成可验证结论”的答案

没有 evidence：
    生成“当前证据不足”的明确说明
```

示例降级答案：

```text
当前证据不足以确认该问题的可靠结论。

已确认：
- 已检查代码、文档和服务依赖中的相关证据。
- 已保留可追溯的证据引用。

尚未确认：
- 业务领域和核心流程；
- AI 调用入口和完整调用链；
- 数据状态与外部依赖。

因此本次结果只能作为部分调查结果，不能据此推断完整技术栈。
```

这类结果是合法的 `partial` 交付，不是伪造答案，也不是空输出。

## 12. 错误模型

错误按阶段分类：

### Provider 层

```text
provider_unreachable
provider_timeout
rate_limited
authentication_failed
invalid_provider_response
```

### Task 执行层

```text
tool_failed
tool_budget_exhausted
task_timeout
task_cancelled
task_output_invalid
```

### Evidence 层

```text
no_evidence
evidence_conflict
evidence_out_of_scope
unsupported_claim
missing_required_source
```

### Composition 层

```text
composer_timeout
composer_reasoning_truncated
composer_invalid_output
composer_missing_required_field
```

### Delivery 层

```text
empty_answer
invalid_reference
answer_claim_mismatch
delivery_validation_failed
```

内部错误结构：

```go
type RunFailure struct {
    Phase     RunPhase
    Code      string
    Retryable bool
    Message   string
    Cause     error
}
```

公开接口返回聚合后的状态和稳定错误码；完整原因写入 Run trace 和持久化记录。

## 13. 持久化与事件

### 13.1 持久化对象

至少需要持久化：

```text
investigation_runs
investigation_contracts
investigation_plan_revisions
investigation_tasks
investigation_task_attempts
evidence_units
evidence_conflicts
verified_claims
investigation_gaps
delivered_answers
```

原始工具正文不应无界地复制到每张业务表。大内容放入已有 trace/evidence 存储，业务表只保留摘要、哈希、定位和引用。

### 13.2 事件

事件是只追加事实，不是另一个状态机：

```text
run.created
contract.built
plan.created
plan.revised
task.started
task.completed
task.failed
evidence.added
evidence.conflict_detected
claim.verified
gap.updated
run.replanned
composition.started
composition.completed
delivery.fallback
run.delivered
run.failed
run.cancelled
```

每个事件必须包含：

```text
run_id
sequence
timestamp
phase
payload reference
```

同一个 `run_id + sequence` 只能产生一个事件，消费者可以根据 sequence 恢复顺序。

## 14. 包结构

建议的 v2 包结构：

```text
internal/agent/
├── coordinator/
│   ├── run.go
│   ├── lifecycle.go
│   ├── budget.go
│   └── events.go
│
├── contract/
│   ├── contract.go
│   ├── goals.go
│   └── criteria.go
│
├── planning/
│   ├── planner.go
│   ├── plan.go
│   ├── scoring.go
│   └── replanning.go
│
├── scheduling/
│   ├── scheduler.go
│   ├── dependencies.go
│   └── concurrency.go
│
├── investigation/
│   ├── investigator.go
│   ├── task.go
│   └── report.go
│
├── evidence/
│   ├── unit.go
│   ├── ledger.go
│   ├── normalize.go
│   └── conflict.go
│
├── verification/
│   ├── verifier.go
│   ├── claims.go
│   └── coverage.go
│
├── composition/
│   ├── composer.go
│   ├── prompt.go
│   └── draft.go
│
├── delivery/
│   ├── delivery.go
│   ├── renderer.go
│   ├── validation.go
│   └── fallback.go
│
└── persistence/
    ├── runs.go
    ├── plans.go
    ├── evidence.go
    └── claims.go
```

包边界：

1. `coordinator` 依赖各层接口，不依赖具体 LLM provider；
2. `planning` 不依赖 HTTP/MCP transport；
3. `investigation` 通过 Tool interface 访问工具；
4. `evidence` 和 `verification` 不依赖用户会话；
5. `composition` 只处理已验证输入；
6. `delivery` 是公开答案的唯一出口；
7. transport 只负责组装请求、订阅事件和转换结果。

## 15. 测试策略

测试重点从“某次重试使用了多少 token”转向系统不变量。

### 15.1 Contract

```text
问题可以拆成稳定 EvidenceGoal
目标依赖合法且无环
required goal 不会被静默丢弃
```

### 15.2 Planner

```text
无证据时生成初始任务
目标已覆盖时停止继续调查
冲突出现时生成验证任务
预算不足时转 partial delivery
不会重复生成已完成任务
```

### 15.3 Scheduler

```text
依赖未满足的任务不会启动
互不依赖任务可以并行
取消后不会启动新任务
预算不足时不会超发任务
```

### 15.4 Evidence 与 Verifier

```text
重复证据被去重
不同版本证据不会错误合并
冲突证据被保留并标记
无引用 Finding 不能成为 supported Claim
不存在的 Evidence 引用会被拒绝
```

### 15.5 Delivery

```text
Composer 成功 -> succeeded 或 partial，答案非空
Composer reasoning-only -> deterministic partial，答案非空
只有 evidence 没有 claim -> 非空说明
没有 evidence -> 非空 evidence_insufficient 说明
引用不存在 -> failed
成功结果不允许 Text == ""
```

### 15.6 当前故障回归

必须有一个跨层回归测试：

```text
Investigator 产出 verified evidence
Composer 第一次 reasoning-only
Composer 重试仍然 reasoning-only
Delivery fallback

断言：
status == partial
text != ""
output != nil
evidence 保留
gaps 保留
failure.code == composer_reasoning_truncated
```

## 16. 实施计划

这是一次不保留旧 workflow 运行时兼容的重写，按以下阶段实施。

具体的 P0、P1、P2 任务、依赖关系和退出条件见：
[dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md](dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md)。

### Phase 1：领域模型与交付不变量

新增：

```text
InvestigationRun
InvestigationContract
BudgetLedger
EvidenceLedger
ClaimLedger
DeliveredAnswer
DeliveryGate
```

验收：

```text
Run 可以创建、持久化和恢复
状态转换合法
预算结算正确
空答案无法进入 succeeded/partial
```

### Phase 2：Planner 与 Scheduler

实现：

```text
初始计划
任务依赖
并行调度
预算申请
动态 replan
停止条件
```

验收：

```text
计划可以基于新证据增删任务
不会重复任务
不会超预算
预算不足时可以进入 partial delivery
```

### Phase 3：Evidence 与 Verifier

实现：

```text
EvidenceUnit
EvidenceLedger
ClaimLedger
coverage
conflict detection
```

验收：

```text
supported Claim 全部可追溯
冲突结论不会被静默合并
未验证 Claim 不会进入确定性答案
```

### Phase 4：Investigator 与 Composer

实现：

```text
任务级上下文
任务级工具权限
结构化 InvestigationReport
结构化 AnswerDraft
```

验收：

```text
Investigator 不能越权完成其他任务
Composer 不再直接读取原始工具结果
Composer 失败可以被 delivery 捕获
```

### Phase 5：Delivery 与接口切换

实现：

```text
DeliveryGate
DeterministicRenderer
REST API 映射
MCP 映射
SSE 终态事件
```

验收：

```text
所有公开成功结果非空
partial 结果包含 gaps 和 limitations
模型失败原因可观测
```

### Phase 6：删除旧链路

删除：

```text
forceConclusion
ConclusionRetryMaxTokens
分散的 output recovery
多层 Outcome 互相覆盖
publicOutputText 猜测式转换
旧 workflow compatibility adapter
```

旧实现不再作为运行时 fallback。删除前保留必要的历史 trace 和迁移数据读取脚本，但不保留旧执行路径。

## 17. 明确不采用的方案

以下方案不属于 v2：

1. 继续增大 `ConclusionMaxTokens`；
2. 继续增加 reasoning retry 次数；
3. 为每种模型添加 prompt 特殊词；
4. 让所有 Agent 共享一份不断增长的上下文；
5. 通过把失败状态改成成功掩盖空答案；
6. 在多个包中分别实现一套 recovery；
7. 为当前失败问题添加实体名、关键词或表名特殊分支；
8. 用第二套内存依赖图绕过 EvidenceLedger；
9. 使用没有总预算约束的无限动态规划。

## 18. 验收标准

v2 只有满足以下条件才可以替换生产入口：

```text
1. 所有终态都可持久化和恢复；
2. 所有公开 succeeded/partial 结果 Text 非空；
3. 每个确定性 Claim 都能定位到 EvidenceUnit；
4. 任务计划可解释、可重放、可取消；
5. 动态 replan 不会产生循环或无限任务；
6. 单次 Run 的 token、工具调用、轮次和时间不超预算；
7. Composer 失败不会丢失已验证证据；
8. 证据不足时不会伪造完整结论；
9. API、MCP、SSE 返回同一个 DeliveryResult；
10. 运行 trace 可以区分 planning、execution、verification、composition 和 delivery。
```

## 19. 最终决策

v2 的核心不是增加 Agent 数量，而是把调查变成由证据覆盖率驱动的闭环控制系统：

```text
Contract
  -> Plan
  -> Execute
  -> Verify
  -> Coverage
  -> Replan or Compose
  -> Delivery Gate
```

模型负责：

```text
提出计划
执行调查
判断候选声明
组织自然语言
```

系统负责：

```text
状态
预算
依赖
证据
终止
交付
```

只要这个边界成立，`max_tokens exhausted before any visible content` 就不再是可以把整个用户请求变成空答案的致命错误，而只是一个可观测的 Composer 阶段失败：

```text
Composer failed
  -> Evidence and claims retained
  -> Deterministic partial delivery
  -> User receives a non-empty, bounded, traceable result
```
