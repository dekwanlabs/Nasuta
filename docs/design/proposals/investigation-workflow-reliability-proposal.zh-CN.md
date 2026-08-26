# 调查型 QA 工作流可靠性改进提案

状态：提案，尚未实施  
日期：2026-08-16  
触发案例：Trace `9da1525dd75a`

## 1. 摘要

本提案解决调查型 QA 工作流中“已经找到证据，但最终答案仍不完整”的系统性问题。

触发案例的问题是：

> 设备从“说出指令”到“执行”的完整链路，业务上是怎样设计的，代码里又是怎么实现的？

本次工作流历时约 255 秒，消耗 252,101 tokens，执行 24 次工具调用。工作流终态为
`succeeded`，但 code 和 docs 两个调查节点失败，最终只剩 runtime 报告参与合成，用户得到的
是约 538 字的降级回答。调查过程中实际已经定位 Google、Alexa 和自研 AIoT Assistant 的多个
代码入口与下行控制路径，但其中一份完整报告因 JSON 外层带标准 Markdown fence 被判无效，
另一份报告因长度截断且不允许续写而失败；动态工具发现的新证据又没有进入 canonical evidence
ledger，无法被 verifier 认可。

这不是单个提示词或单个问题的缺陷，而是以下机制之间的契约不完整：

1. 查询语义分类依赖开放式关键词枚举，无法稳定识别等价的自然语言答案形态；
2. 推理步骤预算和工具调用预算被错误等同；
3. 结构化输出只接受裸 JSON，缺少严格的入口规范化；
4. 长结构化报告没有受控续写和可诊断的失败边界；
5. 部分调查工具没有生产 canonical evidence units；
6. InvestigationGoal 数量、Capability 数量和 EvidenceGoal 覆盖被混为同一约束；
7. 多入口系统缺少受信任的分支化合成约束，容易把相邻路径拼成一条链；
8. 工作流终态没有同时投影执行状态和既有 Completeness。

提案的核心目标是建立一条闭合链路：

```text
问题语义
→ 查询分类和覆盖目标
→ 能力规划
→ 有界调查
→ 结构化报告规范化与校验
→ 动态证据入账
→ 基于 canonical evidence 的验证
→ 按入口和证据强度合成
→ 对用户公开真实完成度
```

## 2. 范围

### 2.1 目标

1. 语义上要求有序端到端过程的问题能够进入 flow 检索策略并启用 codegraph 扩展，不依赖自然语言关键词枚举。
2. 调查节点已生成的合法结构化报告不会因标准 JSON fence 被整体丢弃。
3. 长报告在输出截断时至少拥有一次受控续写机会。
4. `MaxSteps` 和 `MaxToolCalls` 分别表达推理轮次和工具调用次数，不再互相替代。
5. 调查期间新发现的工具证据能够形成 canonical evidence units，并被 verifier 和最终引用链使用。
6. 多入口、多实现问题按路径分支回答，不把不同入口、异步任务和相邻依赖拼成一条“完整链路”。
7. 工作流状态、节点失败和最终覆盖度可以从日志及持久化记录中直接判断。
8. 所有改动作用于通用调查机制，不针对本案例的设备、平台、类名或关键词做特例。

### 2.2 非目标

1. 不在本提案中补齐目标业务仓库的设备固件代码或外部语音平台实现。
2. 不保证一次调查能够证明仓库外部的 ASR、NLU、云平台和设备固件内部行为。
3. 不通过提高全部模型 token 上限掩盖检索、预算和结构化输出问题。
4. 不允许 verifier 绕过 canonical evidence，仅凭模型正文接受 claim。
5. 不把 Google、Alexa、自研助手强制抽象成一条实际不存在的统一运行路径。
6. 不引入持久化状态机；本问题可以由现有事实、节点结果和覆盖度直接派生。

### 2.3 与既有设计的关系和收敛约束

本提案是对现有 QA 调查链路的可靠性修正，不建立第二套查询、证据、预算或完成度架构。实施时必须与以下
既有设计保持一致：

- `qa-query-intent-and-facet-model-simplification.zh-CN.md`：请求级语义只保留一个 canonical `QueryKind`，
  Facet 和检索策略由它派生；
- `qa-unified-evidence-acquisition-pipeline.zh-CN.md`：`tool.Result.EvidenceUnits`、run-local ledger、Workflow handoff、
  join 和 verifier 已经是 canonical evidence 主链；
- `qa-agent-context-budget-and-cancellation.zh-CN.md`：模型上下文、输出、工具交付和墙钟预算已有分层所有者；
- `agent-platform/18-task-driven-multi-agent-architecture.zh-CN.md`：Task、Capability、EvidenceGoal 和 Workflow 节点
  各自承担不同职责，不能互相借字段表达。

为避免新的“水多加面、面多加水”，本提案遵守以下约束：

1. **一个概念只有一个事实源。** `QueryKind`、`EvidenceUnit`、有效运行预算和 `Completeness` 不建立平行模型。
2. **优先修生产边界，不优先修模型文本。** 能由服务端规范化、来源适配或预算配置解决的问题，不增加通用
   LLM repair。
3. **来源身份由来源所有者产生。** `Reference` 不能仅凭展示字段自动升级为 authoritative `EvidenceUnit`。
4. **路径归属由任务作用域和证据 provenance 产生。** 不信任 finding 自报的 `path_id`，也不根据相同
   `stage` 名称推导共享实现。
5. **已有链路只补缺口，不重新实现。** 动态证据问题的范围是补齐未产出 `EvidenceUnits` 的工具适配器，
   不是再建设一条 ledger/handoff/verifier 链。
6. **恢复机制必须有删除条件。** continuation 仅处理明确的 `finish_reason=length`；若未来引入其他恢复，
   必须证明不会改写 claim 或伪造 evidence，并用指标决定是否保留。

收敛审查后的判断是：原稿的总体故障分解成立，但通用 schema repair、`Reference → EvidenceUnit` 自动升格、
finding 自报路径、以及新建一套 `NodeBudget/CompletionSummary` 的写法存在重复建模或后置补偿风险。下文方案已
改为复用现有 canonical 类型和链路。

## 3. 触发场景复盘

### 3.1 用户场景

用户希望同时获得两个视角：

- **业务设计**：语音理解、设备能力映射、命令编排、下行执行、状态回报分别由谁负责；
- **代码实现**：HTTP/Skill 入口、控制器、handler、agent/tool、Feign、设备影子和下行通道如何连接。

问题没有限定 Google、Alexa 或自研语音助手。因此正确行为不是假设唯一入口，而是先识别系统存在
多个入口，再分别调查每条路径，最后抽取共享的设备控制层。

### 3.2 实际执行结果

| 节点 | 结果 | Tokens | 工具调用 | 耗时 | 失败或降级原因 |
|---|---:|---:|---:|---:|---|
| `investigate.code` | failed / `invalid_output` | 47,487 | 8 | 119 秒 | 合法 JSON 被包在单一 `json` fence 中 |
| `investigate.docs` | failed / `agent_failed` | 96,278 | 8 | 136 秒 | 报告被长度截断，`MaxContinueRounds=0` |
| `investigate.runtime` | succeeded | 97,709 | 8 | 206 秒 | 首轮 reasoning 无可见内容，no-reasoning 重试后成功 |
| `synthesize` | succeeded | 10,627 | 0 | 40 秒 | 只能使用 runtime 报告合成 |

工作流汇总：

- 时间：18:01:08 至 18:05:23；
- 端到端耗时：约 255 秒；
- 工作流耗时：246 秒；
- 总 tokens：252,101；
- 工具调用：24；
- 工作流状态：`succeeded`；
- 停止原因：`capability_unavailable`；
- 用户可见回答：约 538 字。

### 3.3 本应交付的答案形态

调查已经发现至少三类入口：

```text
Google Assistant
→ /googleHome/webHook
→ GoogleLambdaController.webHook
→ google-dreo-skill#execute
→ GoogleHomeWebHookController.execute/doExecute
→ handler
→ DeviceShadow/设备控制 Feign
→ 下行通道
→ 设备执行与状态回报

Alexa
→ Skill IntentHandler
→ BaseKCMDeviceLogic.handlePowerControl
→ 构造 desired state
→ deviceCommandService.sendCommand
→ 设备命令/影子通道
→ 设备执行

自研 AIoT Assistant
→ voice-assistant-gateway / hsas-aiot-application
→ hsds-aiot-service.dispatch_user_request
→ classic 或 mtp_itt
→ planner / ExecAgent
→ device_operation_agent / FunctionTool
→ 设备控制工具
→ 设备影子或下行通道
→ 设备执行
```

其中“设备命令进入下行通道”以前的部分有较强代码证据；MQTT/device gateway、固件实际执行、
actual state 回报等最后一跳缺少直接代码证据，应作为限制说明，而不是补成确定事实。

最终回答没有交付以上结构，根因不是单纯召回失败，而是调查结果在输出校验、续写、重试、证据入账
和合成阶段连续丢失。

## 4. 问题定义

### 4.1 自然语言 QueryKind 依赖开放式关键词枚举

当前 `ResolveQueryPlan` 使用 `hasFlowSignal` 和 `queryKindSignals` 对用户原文做子串匹配。触发案例没有
命中现有 flow 词表，又因为“实现”命中 `QueryInventory`，最终导致：

- query kind 被识别为 `inventory`；
- required facets 不包含 `core_flow`；
- retrieval policy 不启用 codegraph expansion；
- 日志中的 codegraph keywords 为 0；
- 检索偏向宽泛枚举，而不是沿入口和调用关系追踪。

相关实现：

- `internal/domain/query_plan.go`
- `internal/retrieval/route.go`
- `internal/agent/qa/query.go`
- `internal/retrieval/pipeline.go`

这里的根因不是词表少了“链路、完整链路、流程、流转”几个词。自然语言表达是开放集合，继续扩充
`hasFlowSignal` 只会形成“水多加面、面多加水”的无限兼容：

- 每出现一种新说法就增加一个词或正则；
- 新词会与 inventory、overview、comparison 等已有词表产生新的优先级冲突；
- 中英文、口语、缩写和上下文省略无法通过有限子串列表穷举；
- 测试会逐渐变成历史失败语句集合，而不是稳定契约测试。

精确格式规则仍然有价值，例如带字段名的 trace ID、W3C `traceparent`、Kibana trace URL。这些输入拥有
封闭语法，可以由本地规则可靠识别。任意裸 UUID 不足以证明用户在做运行时诊断，不能单独触发覆盖。
自然语言要求的答案形态则不具备封闭词表，不应继续由关键词规则承担。

#### 场景

下面四个问题要求的答案形态完全相同：

- “订单从提交到扣款成功的完整链路是什么？”
- “一个支付请求发出去以后，是怎么一路落到账务系统的？”
- “用户点确认后，哪些模块依次接棒，最后在哪里改状态？”
- “Walk me through what happens between checkout confirmation and ledger posting.”

它们都要求有序的端到端过程，但没有一个可长期维护的关键词集合能够覆盖所有等价表达。继续追加
“完整链路”“一路”“依次接棒”“what happens between”等词，只会把分类器变成不断增长的兼容表。

#### 需要解决的机制

自然语言 QueryKind 应由现有 model-backed retrieval planner 在同一次预处理调用中输出稳定的结构化
语义枚举，服务端只负责：

1. 校验枚举值；
2. 将 canonical `QueryKind` 映射到 required facets 和 retrieval policy；
3. 对带类型信息的 runtime locator 等封闭格式保留确定性覆盖规则；
4. planner 不可用或输出无效时不终止问答，进入明确的最小资源 fallback，并记录 `origin=fallback`；
5. 不再通过新增自然语言关键词改变 QueryKind。

模型负责理解开放式自然语言，但不直接决定工具、预算或 capability。执行策略仍由服务端根据稳定枚举
确定，因此不会把权限和运行边界交给模型。该语义分析复用现有 retrieval planner 调用，不新增一次 LLM 请求。
`query_semantics` 不增加一个仅用于观测、却会扩大失败面的 confidence 字段；分类质量由离线评估和现有 planner
调用指标衡量。

##### 代码示例（设计草案）

在现有 planner 响应中增加一个稳定的 `query_semantics` contract：

```go
// internal/domain/query_plan.go
type QuerySemantics struct {
    Kind QueryKind
}

const QueryResolutionPlanner QueryResolutionOrigin = "planner"

// internal/retrieval/route.go
type AnalysisResult struct {
    Decision       domain.PlanDecision
    Execution      ExecutionSuggestion
    Question       string
    Terms          QueryTerms
    QuerySemantics *domain.QuerySemantics
    ToolIDs        []string
    Time           TimeExpr
    History        HistoryRelation
}
```

Prompt 描述每种答案形态的语义边界，而不是列举触发词：

```text
Classify the answer shape required by the current question.
- focused_fact: one bounded fact or location.
- inventory: a set of components, changes, or required work without ordered execution.
- flow: an ordered process with an entry, intermediate transitions, and an outcome.
- comparison: independently establish alternatives, then compare them.
- runtime_diagnosis: explain an observed runtime failure using runtime evidence.
- code_review: evaluate implementation quality or risk.
- overview: explain a system broadly across several responsibility dimensions.
Return only one supported kind. Do not select tools or evidence sources.
```

服务端 binder 只接受封闭枚举，但协议错误不能终止整次问答。Binder 保留具体错误，调用边界记录降级并
传入 `nil`，再由 resolver 生成 `origin=fallback` 的最小资源计划：

```go
func bindQuerySemantics(raw map[string]any) (*domain.QuerySemantics, error) {
    value, ok := raw["kind"].(string)
    if !ok {
        return nil, errors.New("query_semantics.kind must be a string")
    }
    kind := domain.QueryKind(value)
    switch kind {
    case domain.QueryFocusedFact,
        domain.QueryOverview,
        domain.QueryFlow,
        domain.QueryComparison,
        domain.QueryInventory,
        domain.QueryRuntimeDiagnosis,
        domain.QueryCodeReview:
    default:
        return nil, fmt.Errorf("unsupported query kind %q", value)
    }

    return &domain.QuerySemantics{Kind: kind}, nil
}

func bindQuerySemanticsOrFallback(raw map[string]any) *domain.QuerySemantics {
    semantics, err := bindQuerySemantics(raw)
    if err != nil {
        log.Warnf("retrieval planner query semantics rejected; using fallback: %v", err)
        return nil
    }
    return semantics
}
```

不能在 binder 内把未知值伪装成合法的 `focused_fact`，否则 trace 会错误显示 planner 分类成功。返回
`nil` 后，统一由 resolver 决定 fallback kind，并记录真实的 `resolution_origin=fallback`。

本地 resolver 不再扫描开放式自然语言词表，只保留封闭格式覆盖和显式 fallback：

```go
var traceIDFieldRe = regexp.MustCompile(
    `(?i)\btrace(?:[_-]?id)?\s*[:=：]\s*[0-9a-f-]{12,64}\b`,
)
var traceparentRe = regexp.MustCompile(
    `(?i)\b[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\b`,
)

func hasTypedRuntimeLocator(question string) bool {
    return traceIDFieldRe.MatchString(question) ||
        traceparentRe.MatchString(question) ||
        hasKibanaTraceURL(question)
}

func ResolveQueryPlan(
    question string,
    semantics *QuerySemantics,
    identifiers []string,
) QueryResolution {
    entities := CanonicalEntityIDs(identifiers)
    if hasTypedRuntimeLocator(question) {
        return QueryResolution{
            Plan:            QueryPlan{Kind: QueryRuntimeDiagnosis, Entities: entities},
            Origin:          QueryResolutionRule,
            MatchedRuleKind: QueryRuntimeDiagnosis,
        }
    }
    if semantics != nil {
        return QueryResolution{
            Plan:   QueryPlan{Kind: semantics.Kind, Entities: entities},
            Origin: QueryResolutionPlanner,
        }
    }
    return QueryResolution{
        Plan:   QueryPlan{Kind: QueryFocusedFact, Entities: entities},
        Origin: QueryResolutionFallback,
    }
}
```

这里的 fallback 不声称理解了问题，只提供受限、可观测的退化路径。后续若发现 fallback 召回范围不足，
应调整 fallback policy 或恢复 planner 可用性，而不是重新建立一套自然语言关键词分类器。

### 4.2 `MaxSteps` 被错误用作 `MaxToolCalls`

当前 investigation budget 直接设置：

```go
maxToolCalls = definition.Budget.MaxSteps
```

相关实现：

- `app/investigation_budget.go`

但一个推理 step 可以并行调用多个工具。触发案例中三个 investigator 都在第 8 次工具调用后收到：

```text
tool-call budget exhausted; forcing conclusion
```

这使得 8 个推理步骤通常只能执行两三轮实际调查。Code investigator 只来得及调用 8 次
`get_symbol`，尚未形成完整的跨服务调用图就被强制收敛。

#### 场景

一个链路调查的单轮可能需要同时查询：

- 入口 controller；
- 下游 service；
- handler/interface 实现；
- 设备影子写入点。

如果一次并行批次包含 4 次调用，8 次工具预算只允许两轮。增加 `MaxSteps` 又会同时扩大模型循环，
无法独立控制外部调用成本。

#### 需要解决的机制

推理轮次与工具调用数是两个不同预算维度，必须独立配置和计量。预算耗尽时还应记录消耗的是哪一维，
不能统一显示为“强制结论”。

##### 代码示例（设计草案）

Agent definition 明确拥有工具预算，不再由 `MaxSteps` 推导：

```go
type BudgetPolicy struct {
    Timeout           time.Duration `json:"timeout"`
    MaxSteps          int           `json:"max_steps"`
    MaxToolCalls      int64         `json:"max_tool_calls"`
    ContextTokens     int           `json:"context_tokens"`
    MaxContinueRounds int           `json:"max_continue_rounds,omitempty"`
}
```

应用组合层只传递已经校验过的预算。要求工具的 investigator 未配置工具预算时直接报错，避免静默使用
另一个字段作为替代值：

```go
func investigatorToolBudget(definition agentapi.Definition, requireTools bool) (int64, error) {
    if !requireTools {
        return 0, nil
    }
    if definition.Budget.MaxToolCalls <= 0 {
        return 0, fmt.Errorf(
            "investigation agent definition %q requires max_tool_calls",
            definition.ID,
        )
    }
    return definition.Budget.MaxToolCalls, nil
}
```

执行循环分别判断两个上限，并返回可观测的耗尽原因：

```go
switch {
case state.step >= agent.cfg.MaxSteps:
    state.forceConclusion("max_steps_exhausted")
case int64(state.result.Evidence.ToolCallCount) >= agent.cfg.MaxToolCalls:
    state.forceConclusion("max_tool_calls_exhausted")
}
```

### 4.3 结构化输出入口过于脆弱

`validatedOutput` 当前尝试：

1. 把完整回答作为裸 JSON 校验；
2. 如果失败，再把完整回答当作 JSON string 校验。

它不接受只有一个标准 `json` code fence 的回答。触发案例的 code 报告在去除 fence 后：

- 可以正常 `json.Unmarshal`；
- 可以通过当前 `investigation.report` Schema；
- 但当前实现将节点标记为 `invalid_output`，并清空公开文本、引用和消息。

相关实现：

- `internal/agent/definition/result.go`

#### 场景

模型被要求返回 JSON，但仍返回：

````text
```json
{"findings": [...]}
```
````

这是一种常见、确定性可规范化的传输包装，不应触发整份报告丢弃，也不需要再次调用模型。

#### 需要解决的机制

结构化输出入口应只做一次严格规范化：

- 接受裸 JSON；
- 接受外层只有一个、无额外正文的 `json` 或无语言 fence；
- 拒绝 fence 前后存在解释文本、多段 fence 或无法唯一确定 payload 的输出；
- 规范化后只对 canonical bytes 做 schema 校验；
- 下游不得重复 trim、去 fence 或尝试猜测 JSON。

##### 代码示例（设计草案）

规范化函数只接受裸 JSON 或唯一的 fenced payload，不从任意正文中猜测 JSON 子串：

```go
func canonicalStructuredOutput(answer string) (json.RawMessage, error) {
    value := strings.TrimSpace(answer)
    if json.Valid([]byte(value)) {
        return json.RawMessage(value), nil
    }

    lines := strings.Split(value, "\n")
    if len(lines) < 3 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
        return nil, errors.New("structured output must be JSON or one JSON fence")
    }
    opener := strings.ToLower(strings.TrimSpace(lines[0]))
    if opener != "```" && opener != "```json" {
        return nil, fmt.Errorf("unsupported structured output fence %q", opener)
    }
    for _, line := range lines[1 : len(lines)-1] {
        if strings.HasPrefix(strings.TrimSpace(line), "```") {
            return nil, errors.New("structured output contains multiple fences")
        }
    }

    payload := strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
    if !json.Valid([]byte(payload)) {
        return nil, errors.New("fenced structured output is not valid JSON")
    }
    return json.RawMessage(payload), nil
}
```

`validatedOutput` 只校验规范化后的 canonical bytes：

```go
func validatedOutput(
    schemas *agentapi.SchemaRegistry,
    ref agentapi.SchemaRef,
    answer string,
) (json.RawMessage, error) {
    raw, err := canonicalStructuredOutput(answer)
    if err != nil {
        return nil, err
    }
    if err := schemas.Validate(ref, raw); err != nil {
        return nil, fmt.Errorf(
            "definition output does not match schema %q version %d: %w",
            ref.ID,
            ref.Version,
            err,
        )
    }
    return append(json.RawMessage(nil), raw...), nil
}
```

### 4.4 长报告截断后没有恢复路径

`continueIfNeeded` 只有在 `MaxContinueRounds > 0` 时才继续生成。触发案例的 docs investigator 返回
`finish_reason=length`，但配置为 0，因此立即返回 `ErrAnswerTruncated`。

相关实现：`internal/agent/execution/answer_generation.go`。

工作流虽然配置了多次 attempt，但长度截断不是重新执行调查工具的合理理由。前 90% 的报告内容可能已经生成，
整节点重跑既昂贵，也会改变证据集合；另一方面，通用 LLM schema repair 又可能改写 claim 和 evidence，不能
作为默认补偿。

#### 场景

文档调查包含多个业务阶段、引用和限制说明，JSON 在最后一个 finding 附近被截断。系统既不补充剩余字节，
也不保存可诊断 artifact，而是丢弃全部公开结果。

#### 需要解决的机制

恢复过程只包含两个不改语义的步骤：

1. `finish_reason=length` 且已有可见内容：进行一次 continuation；
2. 合并后执行严格 JSON canonicalization 和 schema validation；
3. 仍非法或 schema 不满足：节点显式 `invalid_output`，保存有界脱敏 artifact、finish reason 和 validation error；
4. 不做通用 schema repair，不因内容错误重跑整个工具调查节点。

Investigator Definition 为长结构化报告保留一次续写机会：

```go
Budget: agentapi.BudgetPolicy{
    Timeout:           settings.AgentTimeout,
    MaxSteps:          settings.AgentMaxSteps,
    MaxToolCalls:      settings.InvestigatorMaxToolCalls,
    ContextTokens:     settings.AgentContextTokens,
    MaxContinueRounds: 1,
}
```

Provider 临时错误继续使用已有基础设施重试分类；length continuation 只处理明确截断，不升级为通用内容修复器。

### 4.5 动态工具证据生产不完整

触发案例工作流的初始 evidence units 为 27。经过 24 次 investigator 工具调用后，runtime handoff、join、
verifier 和 output 仍然都是 27 个 evidence units。

Verifier 只根据 `source.EvidenceUnits` 构建 evidence index，因此下列动态引用无法绑定：

- `tool:list_apis#google-dreo-skill`；
- `tool:trace_deps#google-dreo-skill-lambda`；
- `tool:trace_deps#hsds-aiot-service`；
- `tool:get_service#tts-proxy`。

相关实现已经具备主链能力：

- `tool.Result` 已包含 `References` 和 `EvidenceUnits`；
- Agent loop 已将工具返回的 `EvidenceUnits` 合并到 run-local ledger；
- `RunResult → NodeResult → Handoff → Join → Verifier` 已传递和去重 evidence units。

实际缺口是部分 code/service/dependency 工具只返回模型可读 `Content` 或展示用 `References`，没有由来源所有者
生成 authoritative `EvidenceUnits`。因此这不是“再建一条动态 ledger”的问题，而是已有 canonical 链路的
producer coverage 不完整。

#### 场景

预检索只知道服务 A。Agent 通过 `trace_deps(A)` 找到服务 B，再通过 `get_symbol(B)` 找到真正的命令写入点。
如果 B 和 symbol 结果只形成 reference，没有形成来源拥有的 evidence unit，最终正文可以描述它们，但
canonical references 永远只能引用最初的 A。

#### 需要解决的机制

1. code/service/dependency/runbook/runtime 等来源适配器在工具成功边界生成 canonical `EvidenceUnits`；
2. `Reference` 只作为 UI、模型投影和最终引用的轻量表示，优先从 evidence unit 派生；
3. 禁止通用层仅根据 `Reference{Type, Target}` 猜测 trust tier、facet、content hash 或 coverage 并自动升格；
4. 复用现有 run-local ledger、handoff、join 和 verifier，不新增第二套动态证据容器；
5. 为每类只读调查工具增加“成功结果必须产出 evidence unit”的合同测试，确实不构成证据的工具显式声明
   `Evidence=false`。

来源适配器至少应提供稳定 identity、source kind、定位字段、coverage、trust tier、evidence class、
content hash/版本和 producer run/node。模型正文自行书写的 `tool:...` 字符串仍不能成为 canonical evidence。

### 4.6 Planner 的任务数量约束与能力覆盖约束冲突

确定性 fallback 使用 `len(investigation_goals)` 作为 capability 选择上限。触发案例只有两个高层交付目标，
但证明它们需要三个互补 capability，于是出现：

```text
cannot cover evidence facets with 2 investigation tasks
```

相关实现：`internal/agent/qa/task_graph_plan.go`。

#### 场景

用户只有一个目标“解释完整链路”，但证明该目标可能需要业务文档、代码调用路径和运行依赖。反过来，某个
EvidenceGoal 即使允许 code/docs/runtime 三种来源，也不代表三种来源都必须执行。当前建模容易在两个方向上
混淆：用用户目标数限制任务数，或把候选 capability 数当成必需任务数。

#### 需要解决的机制

Planner 应覆盖 required `EvidenceGoal`，并显式区分：

- **互补来源**：合同要求多种来源共同证明，可能需要多个任务；
- **替代来源**：任一满足信任/新鲜度要求的来源即可，只选择最小可行集合。

任务上限来自 `MaxInvestigationTasks` 和工作流总预算，不来自 InvestigationGoal 数量。Task ID、
InvestigationGoal ID 和 EvidenceGoal/Facet ID 必须分离；当前 report 的 `goal_ids` 继续表示 EvidenceGoal/Facet，
不能再被复用为用户交付目标或路径身份。

若预算无法覆盖全部 required EvidenceGoals，应生成 partial plan 并列出 uncovered goals，而不是扩大任务数直到
“看起来完整”，也不是返回一个自相矛盾的完整计划。

### 4.7 合成阶段缺少受信任的多入口分支约束

语音控制不是一条天然唯一的链路。Google、Alexa、自研 Assistant 各自拥有不同入口、认证、语义模型和编排
代码，但可能汇入共享的设备控制层。

当前检索同时返回 Google Home、Alexa、自研 AIoT Assistant、shortcut/scene、TTS、AWS IoT/EMQ、Kafka、
定时任务和状态事件。如果 synthesize 只按主题相似度拼接 finding，就可能错误地声称这些组件属于同一条
语音控制路径。

#### 场景

Google 路径证实使用设备影子，另一个定时任务文档证实使用 Kafka。两份证据都与设备状态有关，但不能据此
得出“Google 语音控制必经 Kafka”。同样，两条路径都出现 `dispatch` 阶段，也不能证明它们共享同一个实现。

#### 需要解决的机制

路径归属不能主要依赖 finding 自报字段。短期由 investigator task 的受信任 scope、producer node 和
canonical evidence identity 确定归属；长期如需正式字段，应把 `path_scope` 放在服务端校验的 Task/TaskContract
元数据中，并让 finding 继承，而不是让模型为每个 finding 自由生成 `path_id`。

最终答案必须区分：

- 入口专属步骤；
- 多入口共享的**业务责任层**；
- 被同一 canonical identity 或显式等价关系证明的共享实现；
- 仅在其他场景出现的相邻机制；
- 未证实的最后一跳。

共享实现不能仅通过 `stage` 文本相同或描述相似推导。没有可信 path scope 的 finding 只能进入“未归属证据”，
不能被自动串入任一入口。

### 4.8 终态成功不能表达实质降级

触发案例工作流状态为 `succeeded`，但停止原因为 `capability_unavailable`，三个调查节点中两个失败，多个
required goal unresolved。

这会导致 UI 看起来像完整成功、监控无法统计答案覆盖不足、调试者必须跨多张表查找原因，用户也不知道回答
只是部分调查结果。

#### 需要解决的机制

系统已经存在两类不同事实：

- Workflow run status：执行是否正常结束；
- Handoff/Investigation `Completeness`：证据与答案覆盖是 `complete`、`partial` 还是 `unavailable`。

缺口是终态投影和观测没有同时展示两者，而不是缺少第三套状态机。应从节点终态、预算事件、stop reason 和
verifier coverage 派生只读摘要，包含 supported/partial/unresolved goals、failed nodes 和 degradation reasons。

工作流执行状态可以为 succeeded，同时 Completeness 为 partial；若没有任何可交付证据，应为 unavailable。
API、SSE、日志和 UI 必须同时展示 run status 与既有 Completeness，但不能再持久化一份可独立漂移的
`completion` 状态。

## 5. 方案设计

### 5.1 目标流程

```text
Retrieval planner（复用现有一次调用）
  └─ query_semantics 独立绑定；该 section 失败不丢弃其他有效 section
       ↓
ResolveQueryPlan
  ├─ 封闭格式规则覆盖
  └─ planner / minimal-fallback origin
       ↓
Required EvidenceGoals
       ↓
Capability cover
  ├─ 覆盖 required EvidenceGoal，而不是要求所有候选来源都执行
  └─ Task identity 与用户 InvestigationGoal identity 分离
       ↓
Investigator execution
  ├─ Definition 默认上限
  ├─ Task/Run 只可收紧
  └─ Workflow NodeBudget 由有效上限派生
       ↓
Tool result
  ├─ canonical evidence units（来源所有者产生）
  ├─ model content（有界投影）
  └─ references（由 evidence/provenance 投影）
       ↓
Structured output ingress
  ├─ strict fence normalization
  ├─ bounded length continuation
  └─ JSON/schema validation；失败即显式 partial，不做通用语义 repair
       ↓
现有 run-local ledger / Workflow handoff / Join
       ↓
现有 Verifier
  └─ findings 绑定 canonical evidence
       ↓
Synthesizer
  ├─ 按受信任 task/path scope 分组
  ├─ 区分 supported / partial / unsupported
  └─ 只在证据 identity 支持时声明共享实现
       ↓
现有 Completeness 投影
  └─ complete / partial / unavailable + degradation reasons
```

### 5.2 查询语义：使用稳定 answer-shape contract

#### 方案

移除 `hasFlowSignal` 和 `queryKindSignals` 对自然语言 QueryKind 的生产分类职责，不保留关键词兼容兜底。
将 QueryKind 作为 `query_semantics` 属性加入现有 retrieval planner 的结构化响应，与 route、query terms、
execution、time 和 history relation 在同一次调用中完成。

Planner 只输出稳定答案形态：`focused_fact`、`inventory`、`flow`、`comparison`、`runtime_diagnosis`、
`code_review`、`overview`。服务端严格校验枚举，再通过 `RequiredFacetsFor` 和 `retrievalPolicyFor` 派生行为；
Planner 不能直接开启 codegraph、指定工具、扩大权限或修改预算。

`query_semantics` 必须采用 **section-level binding**：该 section 缺失或非法时，只把语义解析降级为
`origin=fallback`，不得让已经合法的 route、query terms、execution、time 和 history relation 一起失效。
现有 planner 的整包 `Validate` 若仍是任一 section 出错即拒绝整个响应，实施前必须先调整绑定边界，否则只是
把关键词漏判换成“一个新字段拖垮全部预处理”的新脆弱点。

确定性本地规则只保留封闭语法，例如带字段名的 trace ID、W3C `traceparent`、Kibana trace URL；裸 UUID
不触发分类覆盖。Planner 不可用时使用 `QueryFocusedFact` 只是**最小资源 fallback**，不代表系统正确理解了
答案形态；trace、完成度和评估必须保留 `resolution_origin=fallback`，不得把它统计为 focused fact 分类成功。
若生产数据显示该 fallback 系统性导致覆盖不足，应修复 planner 可用性或单独评审 fallback policy，而不是
恢复开放式自然语言词表。

#### 约束

1. QueryKind 是唯一请求级主分类，不新增平行的 answer-shape 类型。
2. Prompt 用答案结构和证据要求描述 kind，不维护中英文触发词表。
3. 复用现有 planner 请求，不增加模型调用次数。
4. Query semantics 的 contract 错误必须局部降级、可观测，不能静默污染其他 planner section。
5. 精确格式规则必须具有封闭语法，不能以“规则兜底”为名重新加入开放式短语列表。
6. `RequiredFacets` 继续由 canonical QueryKind 派生，不作为第二份可独立漂移的请求状态。

#### 预期效果

触发问题经 planner 输出 `query_semantics.kind=flow`、`resolution_origin=planner`，服务端派生
`entrypoint/core_flow/data_and_state/external_dependency` 和 `expandCodeGraph=true`。新的同义表达只扩充语义
评估集，不修改生产 Go 词表。

### 5.3 调查预算：分离维度，但不新建平行预算模型

#### 方案

`MaxSteps` 与 `MaxToolCalls` 必须独立，但应沿用现有预算所有权层次：

```go
type BudgetPolicy struct {
    Timeout           time.Duration `json:"timeout"`
    MaxSteps          int           `json:"max_steps"`
    MaxToolCalls      int64         `json:"max_tool_calls"`
    ContextTokens     int           `json:"context_tokens"`
    MaxContinueRounds int           `json:"max_continue_rounds,omitempty"`
}
```

- Agent Definition 提供该能力的默认硬上限；
- TaskSpec / RunRequest 可以按任务收紧，不能放宽 Definition；
- immutable run snapshot 保存最终有效上限；
- Workflow `NodeBudget` 的 input/output/total tokens、tool calls 和 cost 由有效 Definition/Task 上限派生；
- 不再新增另一个含 `MaxTokens` 的 `NodeBudget` 配置对象。现有 `ModelPolicy.MaxOutputTokens`、
  `ContextTokens`、`UsageCeiling` 和 runtime usage recorder 继续分别承担输出、上下文、理论上限和实际消耗控制。

这样只新增缺失的一个正交维度 `MaxToolCalls`，不复制现有 token/timeout 事实。

#### 预算耗尽行为

当某一预算维度耗尽时：

1. 工具预算耗尽后关闭本轮剩余 tool calls，并保持 provider tool-call 协议完整；
2. 向模型发送一次无工具 force-conclusion，明确耗尽维度；
3. 报告列出尚未验证的 EvidenceGoals；
4. run/node 记录 `max_steps_exhausted`、`max_tool_calls_exhausted`、`token_budget_exhausted` 或
   `deadline_exceeded`，不能统一写成 `capability_unavailable`；
5. 若已有可交付证据，执行可以正常结束，但答案 `Completeness` 必须按 coverage 派生为 partial/unavailable。

#### 约束

- 不允许无界工具调用；
- 不因为提高工具调用预算而提高推理轮次；
- 批量并行调用按每个调用独立计数；
- force-conclusion 不再开放工具；
- 不在 Definition、Task、Workflow 三层保存可互相冲突的独立默认值。

### 5.4 结构化输出：入口一次规范化

#### 方案

在 `validatedOutput` 前增加严格的 `canonicalStructuredOutput`：

```text
输入
├─ trim 后是合法 JSON → 原样进入 schema 校验
├─ trim 后是唯一一段 fenced payload
│    ├─ fence language 为空或 json
│    ├─ fence 前后没有其他非空内容
│    └─ payload 是合法 JSON
│         → 去 fence，进入 schema 校验
└─ 其他情况 → invalid_output
```

规范化后的 JSON bytes 是后续唯一表示。下游 verifier、handoff 和 persistence 不再进行二次清洗。

#### 不接受的情况

- fence 外有解释文字；
- 多段 code fence；
- JSON 前后混入其他内容；
- 通过正则从任意文本中猜测第一个 `{...}`；
- schema 不匹配时静默删除字段或填默认值。

这保证兼容常见传输包装，同时不把宽松解析变成不可预测的“猜答案”。

### 5.5 输出恢复：只保留可证明不改语义的有界恢复

#### 阶段 A：长度续写

仅在 provider 返回 `finish_reason=length` 且已有可见内容时，允许一次 continuation。Continuation 只补充剩余
内容，不重写开头；合并后统一进入 canonicalization 和 schema validation。再次 length、provider error 或无
可见内容时显式失败。

#### 阶段 B：确定性规范化

只去除一个严格的 JSON fence，不调用模型，不改变 JSON 语义。

#### 阶段 C：schema 校验

- JSON 和 schema 都合法：接受 canonical bytes；
- JSON 非法或 schema 不匹配：节点返回 `invalid_output`，保留有界、脱敏的原始 artifact 和 validation error，
  Completeness 标记 partial/unavailable；
- 不在本提案中加入通用 LLM schema repair。

通用 schema repair 看似节省一次节点重跑，但它允许模型重新生成完整 JSON，可能改写 claim、删减 gap、替换
identity 或补造 evidence。对于调查报告，这不是纯格式修复，而是新的内容生成步骤，容易形成“schema 更严
→ repair 更强 → verifier 再加规则”的水多加面循环。

如果上线数据证明存在大量**可判定为表示层错误**且 continuation/canonicalization 无法覆盖，后续应单独评审
一个 format-only renderer。它必须满足：无工具、输入输出 claim/evidence identity 集合哈希一致、不能新增或
删除 finding、失败即停止，并有独立指标和删除条件；不能以通用自由文本 repair 形式进入本阶段。

#### 为什么不让工作流重跑整个节点

结构化包装或长度截断发生在输出边界，不应重新执行昂贵工具。Provider 临时错误可沿用现有基础设施重试；
内容/schema 错误则显式暴露，不通过整节点重跑或通用 repair 掩盖。

### 5.6 动态证据：补齐现有 canonical pipeline 的 producer

#### 方案

现有 `tool.Result.EvidenceUnits → agent run-local ledger → RunResult → NodeResult → Handoff → Join → Verifier`
链路继续作为唯一事实源。本阶段只补齐没有产出 evidence units 的调查工具。

```go
type Result struct {
    Content        string
    References     []Reference
    EvidenceUnits  []EvidenceUnit
    Coverage       EvidenceCoverage
    AnswerContract AnswerContract
}
```

约束如下：

1. code/service/dependency/runbook/runtime 的来源 owner 或 source-specific adapter 生成 `EvidenceUnits`；
2. `References` 优先从 canonical unit/provenance 投影，用于模型展示和最终引用；
3. 通用执行层不得仅凭 reference 的 type/target 猜测 Facet、TrustTier、Coverage 或 ContentHash；
4. 同一来源只实现一次 identity 构造器，工具和 retrieval 复用，避免两套 identity 漂移；
5. 工具成功但结果不构成证据时显式返回空 evidence，并记录原因，不伪造低质量 unit。

一个动态 evidence unit 至少包含稳定 identity、source kind、repository/service/path/symbol 等定位信息、
coverage、trust tier、evidence class、content hash/版本、token cost 和 producer node/run。既有 ledger 按稳定
identity O(n) 去重并保留冲突；无需再增加新的 workflow evidence 容器或二次清洗步骤。

#### 安全约束

- 只有成功完成且通过来源协议校验的结果才能生成 evidence unit；
- 模型正文自行书写的 reference 不能升级为 canonical evidence；
- restricted scope、脱敏和可见范围必须随 provenance 保留；
- verifier 仍只接受 canonical ledger，不降低标准。

### 5.7 Planner：由 required EvidenceGoal cover 决定调查任务

#### 方案

规划顺序调整为：

1. 从 canonical QueryKind 派生 required `EvidenceGoals`；
2. 构造允许的 capability，并明确每个 capability 能覆盖哪些 EvidenceGoal；
3. 将同一 EvidenceGoal 的多个允许 source 视为候选方案，除非合同明确要求来源多样性，不能把所有候选来源
   都当作必须执行；
4. 在 `MaxInvestigationTasks`、token、tool-call 和 timeout 预算内选择最小可行 cover；
5. 再把用户 `InvestigationGoals` 作为交付目标关联到所选任务，允许多对多；
6. 若无法完整覆盖，生成显式 partial plan 和 uncovered EvidenceGoal，不伪造完整计划。

Task identity 不再复用 InvestigationGoal identity。一个用户目标可能需要多个 capability，一个 capability task
也可能服务多个用户目标。建议任务使用服务端 canonical ID，例如 `investigate.code.1`；如果需要保留用户交付
目标关联，使用单独的 `investigation_goal_ids` 元数据。现有 report `goal_ids` 表示 EvidenceGoal/Facet ID，
不得再同时表示用户 InvestigationGoal 或 path ID。

Set cover 的覆盖对象应是 required EvidenceGoal，而不是“所有 allowed capability 的 Facet 并集”。否则只要一个
Facet 允许 code、docs、runtime 三种来源，规划器就会把三者全部当成必选，再次通过增加任务数解决自己制造的
约束。

#### 预期行为

两个业务目标需要 code、docs、runtime 三类互补证据时，可以生成三个任务；若它们只是同一 EvidenceGoal 的
三个替代来源，则只选择预算和信任等级下最合适的最小集合。两种情况由 EvidenceGoal source policy 区分，
不由 goal 数量或 capability 数量猜测。

### 5.8 合成：按受信任路径作用域和证据强度组织答案

#### 路径作用域

第一阶段不修改当前 `investigation.report` 让模型自报 `path_id`、`stage` 或 `support`：

- `support` 已由 verifier 产生，不能在 report 中维护第二份；
- `path_id` 应来自服务端校验的 task scope、producer node 或 canonical entrypoint identity；
- `stage` 可以作为展示标签，但不能作为“共享实现”的证明。

如后续确需正式 path contract，应优先扩展 Task/TaskContract 的受信任 scope，并在 verifier 输出中投影继承值，
而不是让每个 finding 自由声明路径。

#### 合成规则

1. 先根据 task scope 和 canonical entrypoint evidence 判断是否存在多个入口；
2. 多入口时分别列出每条已证实路径；
3. 不把不同 path scope 的依赖串成顺序关系；
4. 两条路径具有相同业务阶段，只能说明业务责任相似，不能说明代码实现共享；
5. 只有相同 canonical evidence identity、明确共享 target，或经服务端等价关系验证的阶段，才能描述为共享实现；
6. partial claim 使用“已确认到……，后续尚缺证据”；unsupported claim 不进入事实正文；
7. 无可信 path scope 的 claim 进入“未归属/相邻证据”，不得自动拼入主链；
8. 明确说明仓库外边界和最后一跳缺口。

#### 面向触发案例的目标答案结构

```text
结论：系统不存在唯一语音入口，共有 Google、Alexa、自研 Assistant 三条已识别入口。

业务责任共性：
语音理解 → 意图/设备识别 → 能力映射 → 命令编排 → desired state/command
→ 下行通道 → 设备执行 → 状态回报

Google 路径：...
Alexa 路径：...
自研路径：...
经相同 canonical target 证实的共享实现：...
仅业务角色相似但实现未证实共享的部分：...
未证实部分：设备网关到固件执行及 actual state 回报的直接代码证据不足。
```

### 5.9 完成度和可观测性：复用现有 Completeness

#### 方案

不新增另一套 completion 状态机或重复保存 `execution_status`。复用现有 Workflow run status、handoff
`Completeness`（`complete/partial/unavailable`）、verifier coverage 和 stop reason，在 API/SSE/UI 边界派生一个
只读摘要：

```json
{
  "completeness": "partial",
  "supported_goals": 1,
  "partial_goals": 1,
  "unresolved_goals": 3,
  "failed_nodes": 2,
  "degradations": [
    "invalid_output",
    "answer_truncated",
    "max_tool_calls_exhausted"
  ]
}
```

Workflow run status 继续表示执行是否正常终止；`Completeness` 表示答案覆盖。SSE 终态可同时携带二者，但
summary 内不再复制一份 status。若无任何可交付证据，应使用已有 `unavailable`，不能强行压成 `partial`。

#### 输出位置

- 结构化日志和 trace；
- workflow/run 的既有结果或可派生投影；
- SSE 终态事件；
- Dashboard QA 运行详情；
- 按 query kind、agent definition 和模型统计 partial/unavailable 比例。

#### 错误持久化

`workflow_node_runs` 或等价运行记录保留具体 error code、有界脱敏 message、attempt、finish reason、
continuation 次数和耗尽的预算维度。不要再新增一套可独立漂移的 degradation 状态；摘要从这些事实和 verifier
coverage 派生。

## 6. 典型场景与目标行为

| 场景 | 当前风险 | 目标行为 |
|---|---|---|
| 用不同措辞询问同一端到端过程 | 关键词规则随案例持续膨胀 | Planner 输出稳定 `flow` 枚举，服务端映射 core flow 和 codegraph |
| 模型返回单一 `json` fence | 合法报告被判 `invalid_output` | 确定性去 fence 后校验通过 |
| 报告因长度在末尾截断 | 全量结果被丢弃 | 一次 continuation 后再校验 |
| 一轮并行调用 4 个工具 | 8 次预算两轮耗尽 | steps 和 tool calls 独立控制 |
| 工具发现预检索外的新代码 | 正文能用，verifier 无法引用 | 来源 adapter 直接产出 evidence unit，reference 仅作投影 |
| 系统有三个入口 | 不同路径被拼成一条 | 按受信任 task/path scope 分支；共享实现要求同一 canonical target |
| 两个目标需要三类互补能力 | fallback 规划失败 | required EvidenceGoal cover 可生成三个任务 |
| 两个调查节点失败、一个成功 | 工作流仍显示完整 succeeded | run succeeded + existing completeness partial |
| 设备固件不在仓库 | 模型补全最后一跳 | 明确边界，标为未证实 |

## 7. 实施切片

提案按“先阻止确定性丢失，再补生产者，最后增强语义”的顺序实施。每阶段都可独立回滚，且不得同时保留新旧
事实源。

### 阶段 1：阻止已生成报告因传输包装或长度截断丢失

1. 严格 JSON fence canonicalization；
2. investigator `MaxContinueRounds=1`；
3. 保存有界原始 artifact、finish reason 和 schema error；
4. 不引入通用 schema repair。

主要代码：`internal/agent/definition/result.go`、`internal/agent/execution/answer_generation.go`、agent definition
组合层、workflow node failure persistence。

### 阶段 2：修复语义和预算所有权

1. `query_semantics` 加入现有 planner，并实现 section-level binding；
2. 删除自然语言 QueryKind 生产词表，保留封闭格式规则；
3. 在 Definition 默认预算中增加 `MaxToolCalls`，Task/Run 只可收紧；
4. Workflow NodeBudget 从有效限制派生，记录具体耗尽维度；
5. capability planner 覆盖 required EvidenceGoal，不把所有候选来源都当成必选。

### 阶段 3：补齐动态 evidence producers

1. 盘点只读调查工具的 EvidenceUnits 产出覆盖；
2. 为 code/service/dependency 等来源建立单一 identity adapter；
3. 复用现有 run-local ledger、handoff、join 和 verifier；
4. 增加工具结果到最终 verifier 绑定的端到端测试；
5. 禁止 Reference 自动升格。

### 阶段 4：多路径合成与完成度投影

1. 先用 producer task scope 和 canonical entrypoint identity 分组；
2. 不修改 report/v1 让 finding 自报 path/support；
3. 共享实现要求相同 canonical target 或显式等价关系；
4. API/SSE/UI 复用现有 Completeness，展示 partial/unavailable 和 degradation reasons；
5. 若任务级 path scope 不足，再单独评审 TaskContract 扩展。

## 8. 测试方案

### 8.1 查询语义契约测试

单元测试不再验证不断增长的自然语言关键词列表，而是验证稳定协议边界：

1. `bindQuerySemantics` 接受所有合法 QueryKind；
2. 未知 kind、自由文本 kind 或缺失 kind 必须被识别为协议错误，但不能终止问答；
3. planner 返回 `flow` 时，`RequiredFacetsFor` 包含 `core_flow`，retrieval policy 启用 codegraph；
4. 带字段名的 trace ID、W3C `traceparent`、Kibana trace URL 等封闭格式可以确定性覆盖为 runtime diagnosis；裸 UUID 不得单独覆盖；
5. planner 不可用或语义响应无效时标记 `resolution_origin=fallback`，不能伪装成 rule/planner 成功；
6. fallback 路径不得重新调用自然语言关键词分类函数。

代码示例：

```go
func TestResolveQueryPlanUsesPlannerSemantics(t *testing.T) {
    semantics := &QuerySemantics{Kind: QueryFlow}
    got := ResolveQueryPlan("任意自然语言表述", semantics, nil)
    if got.Plan.Kind != QueryFlow || got.Origin != QueryResolutionPlanner {
        t.Fatalf("resolution = %#v", got)
    }
}

func TestInvalidPlannerKindFallsBackWithoutFailingQuestion(t *testing.T) {
    semantics, err := bindQuerySemantics(map[string]any{
        "kind": "full_chain_with_code",
    })
    if err == nil || semantics != nil {
        t.Fatalf("binding = %#v, %v", semantics, err)
    }

    got := ResolveQueryPlan("任意自然语言表述", semantics, nil)
    if got.Plan.Kind != QueryFocusedFact || got.Origin != QueryResolutionFallback {
        t.Fatalf("resolution = %#v", got)
    }
}
```

自然语言理解质量通过独立语义评估集验证。评估集应包含同一 answer shape 的多语言和多种改写，但这些
句子是模型分类测试数据，不进入生产 Go 词表。例如：

- “设备从说出指令到执行的完整链路是什么”；
- “一个指令发出去后，是怎么一路落到设备上的”；
- “用户开口以后哪些模块依次接棒”；
- “Walk through what happens after the voice command is accepted.”

以上问题均预期为 `QueryFlow`。新增改写只扩充评估覆盖，不修改生产分类分支。

### 8.2 结构化输出测试

| 输入 | 预期 |
|---|---|
| 裸合法 JSON | 通过 |
| 单一 `json` fence | 去 fence 后通过 |
| 单一无语言 fence | 去 fence 后通过 |
| fence 外有说明 | 拒绝 |
| 两段 fence | 拒绝 |
| 合法 JSON、schema 不符 | 显式失败并保留诊断，不调用通用 repair |
| 非法 JSON | 显式失败，不能猜取子串或重写语义 |

必须验证 canonicalization 只发生在入口一次。

### 8.3 Continuation 测试

1. 第一段 `finish_reason=length`，第二段完成 JSON：成功；
2. 第一段只有 reasoning、无可见内容：保持 `ErrReasoningTruncated` 语义；
3. continuation 再次 length：有界失败；
4. continuation provider error：保留原内容和明确错误；
5. 完成后 schema 不符：显式 `invalid_output`，不触发通用 repair。

### 8.4 预算测试

1. `MaxSteps=8, MaxToolCalls=24` 时，单轮 4 次并行工具不会在第二轮后错误结束；
2. tool calls 达上限但 steps 未达上限时，只报告 tool call budget exhausted；
3. steps 达上限但工具仍有余额时，不允许继续推理循环；
4. 多个并行工具调用必须原子、准确计数；
5. 不因 force conclusion 再开放工具调用。

### 8.5 动态证据测试

1. 初始 ledger 不含 symbol B；
2. `get_symbol` 的 code source adapter 成功返回 B 的 evidence unit；
3. reference 从 canonical provenance 投影，不参与自动升格；
4. join 后 B 仍存在且无重复；
5. verifier 能将 finding 绑定到 B；
6. 最终 reference details 包含 B；
7. 模型仅在正文伪造 B 的 reference 时不得绑定。

### 8.6 Planner 测试

- 2 个 InvestigationGoals、3 个互补 capability 才能覆盖 required EvidenceGoals：应生成 3 个 tasks；
- 同一 EvidenceGoal 有 3 个替代 source：不能默认生成 3 个 tasks；
- capability 无法覆盖某 required EvidenceGoal：返回明确缺失项；
- 超过 `MaxInvestigationTasks`：生成 partial plan 并记录未覆盖项，不输出伪完整计划；
- fallback 和 LLM planner 应通过同一 contract validator。

### 8.7 合成测试

输入三条路径：

- Google → shadow；
- Alexa → command service；
- 定时任务 → Kafka。

预期：

- Google 和 Alexa 分开描述；
- shadow/command 只能抽象为相似业务责任；只有 canonical target 相同或有显式等价证据时才能称为共享实现；
- Kafka 不得被写成 Google/Alexa 的必经步骤；
- 缺少固件证据时必须输出限制说明。

### 8.8 触发案例回归验收

重新执行与 trace `9da1525dd75a` 同语义的问题，至少满足：

1. query kind 为 `flow`；
2. `resolution_origin=planner`，且生产代码没有为该问题新增自然语言关键词；
3. required facets 包含 `core_flow`；
4. codegraph expansion 已启用；
5. code/docs/runtime 任务图不存在 goal 数量与 capability 数量冲突；
6. 单一 JSON fence 不导致 code 节点失败；
7. docs 输出 length 时执行至少一次 continuation；
8. 动态工具结果使 evidence ledger 数量或内容发生可解释变化；
9. verifier 能绑定动态 code/service/dependency evidence；
10. 最终答案分别描述 Google、Alexa 和自研 Assistant；
11. 不把 Kafka、场景、定时任务无证据地拼入语音主链路；
12. 明确指出设备固件最后一跳证据不足；
13. 若任一必要能力仍失败，终态显示既有 `completeness=partial/unavailable`，不能呈现为完整覆盖。

本提案不把固定 token 数或回答字数作为核心验收标准。重点是证据覆盖、路径正确性、失败可恢复性和完成度
透明。性能验收应记录与原 trace 相比的端到端耗时、总 tokens、工具调用数和有效 finding 数，但不能通过
牺牲正确性换取表面下降。

## 9. 风险与控制

### 9.1 Query semantics 误分类或 planner 不可用

控制：QueryKind 使用封闭枚举和严格 binder；模型不直接控制工具、权限或预算；通过多语言改写评估集持续
衡量语义分类质量。Planner 不可用或语义响应未通过校验时进入显式 fallback 并记录降级，不能让问答失败，
也不能静默切换到开放式关键词分类器。

### 9.2 放宽 JSON 接受范围导致误解析

控制：只接受唯一 fence 且 fence 外无正文；不从任意文本中提取 JSON 子串。

### 9.3 Continuation 增加成本

控制：仅 `finish_reason=length` 触发一次；记录额外 usage。通用 schema repair 不在本阶段实施。

### 9.4 工具预算提高导致运行时间增长

控制：独立的 token、timeout 和 payload budget 继续生效；按 capability 设置预算，不全局无限上调。

### 9.5 动态 evidence ledger 膨胀

控制：工具结果本身有界；按稳定 identity O(n) 去重；只保存必要定位和有界摘要；最终输出继续按覆盖与
信任等级筛选。

### 9.6 多路径 scope 增加规划复杂度

控制：第一阶段复用 producer task scope 和 canonical entrypoint identity，不扩展 report/v1；只有证据证明现有 scope 不足时才评审 TaskContract 字段。

### 9.7 `completeness=partial/unavailable` 被误认为执行失败

控制：复用并分离 Workflow run status 与既有 Completeness；Dashboard 同时展示，不互相替代，也不创建第三套状态。

## 10. 成功指标

上线后按 query kind 和 investigator definition 统计：

1. `invalid_output` 率；
2. fenced JSON 确定性恢复率；
3. `ErrAnswerTruncated` 率、continuation 成功率和 continuation 后 schema 失败率；
4. 每个调查节点的 steps/tool calls/token/time 分布；
5. tool-call budget exhausted 比例；
6. 动态 evidence 产生数、去重后数量和 verifier 绑定率；
7. supported/partial/unresolved goal 比例；
8. execution succeeded 但 completeness partial/unavailable 的比例；
9. 每个最终 supported finding 的平均 token 成本；
10. 多路径问题中的跨路径错误拼接回归数。

首要成功标准不是“所有工作流都显示 succeeded”，而是：

- 已找到的有效报告不会无故丢失；
- 已调用工具发现的证据能够被最终验证和引用；
- 答案明确区分事实、部分证据和未知边界；
- 部分完成不会伪装成完整完成。

## 11. 决策摘要

本提案建议接受以下收敛后的设计决策：

1. 自然语言 QueryKind 由现有 retrieval planner 输出稳定枚举；`query_semantics` 采用 section-level binding，
   本地规则只处理封闭格式，不维护开放式关键词兼容表。
2. `MaxSteps` 与 `MaxToolCalls` 独立；`MaxToolCalls` 补入既有 Definition → Task/Run → Workflow 派生层次，
   不新建平行 token/timeout 预算模型。
3. 结构化输出入口只做一次严格 canonicalization，接受单一标准 JSON fence。
4. 仅对明确 length 截断做一次 continuation；本阶段不引入通用 LLM schema repair。
5. 动态证据问题通过补齐 tool source adapter 的 `EvidenceUnits` 产出解决；复用现有 ledger/handoff/join/verifier，
   禁止把轻量 Reference 自动升格为 authoritative evidence。
6. Planner 覆盖 required EvidenceGoal，并区分互补来源与替代来源；Task ID、InvestigationGoal ID、EvidenceGoal ID
   不再互相复用。
7. 多入口问题按受信任 task/path scope 分支合成；相同 stage 只说明业务角色相似，相同 canonical target 或显式
   等价证据才能证明共享实现。
8. 工作流状态与答案覆盖分离，但复用现有 `Completeness=complete/partial/unavailable`，不创建第三套状态机。

因此，答案是：**原稿确实存在局部“水多加面”风险，但不是整体方向错误。** 风险主要来自重复建模和通用后置
repair；收敛后的方案把改动压回四个真正缺口：语义分类边界、独立工具预算、证据 producer coverage、以及
受信任的多路径合成与完成度投影。
