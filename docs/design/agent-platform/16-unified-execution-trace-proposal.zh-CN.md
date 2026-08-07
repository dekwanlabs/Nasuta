# QA、研发任务与多 Agent 统一 Execution Trace 方案

[返回设计索引](README.zh-CN.md)

> 状态：已实施（批次 A–D，保留 v1 兼容桥）
> 更新日期：2026-08-07
> 适用范围：Nasuta QA、Feature Delivery、Agent Workflow、Tool、LLM 与多 Agent 子 Run
> 关联设计：[可观测性、Token 统计与评估](08-observability-and-evaluation.zh-CN.md)
> 关联方案：[QA 与研发任务多 Agent 路由](15-qa-and-feature-delivery-multi-agent-routing-proposal.zh-CN.md)

## 1. 结论

本改动属于**中等偏大的跨模块重构**，不是简单替换 `RecordTrace` 调用。

推荐目标是：

1. Recorder 在统一 Run 生命周期中创建、挂载和关闭；
2. QA、Workflow、Tool、LLM 和多 Agent 的正式执行边界由拦截器自动记录；
3. 业务代码不再负责 `TraceEnabled`、计时、状态推导和事件发送；
4. 节点名及 Input/Output 评估字段由稳定 Trace Spec 投影；
5. 保持当前 QA SSE 和 MCP `_trace` JSON 协议兼容；
6. 少量没有执行边界的瞬时决策允许保留显式 `Record`，但必须是例外。

按一名熟悉现有代码的工程师估算：

| 实施档位 | 范围 | 预计投入 | 风险 |
|---|---|---:|---|
| 最小收敛 | 统一 Recorder、Run 生命周期和 Exporter，不迁移全部业务节点 | 3–5 人日 | 中低 |
| 推荐方案 | 增加执行拦截器，迁移 QA、Retrieval、Tool、LLM、Workflow 主要节点 | 12–18 人日 | 中等 |
| 深度节点化 | QA 内部阶段全部节点化，追求业务代码零显式 Trace，并覆盖研发任务和多 Agent | 20–30 人日 | 中高 |

推荐采用第二档，分 4 个可独立验证的批次完成。若将研发任务和多 Agent 完整接入也计入同一期，按 **16–24 人日**准备更稳妥。以上不包含等待评审、联调环境和全量评估运行时间。

## 2. 问题定义

当前评估 Trace 是业务代码主动记录：

```go
traceEnabled := domain.TraceEnabled(ctx)
started := time.Now()

result, err := executeBusiness(ctx, request)

if traceEnabled {
    domain.RecordTrace(ctx, domain.EvaluationTrace{
        Node:       "business_node",
        DurationMS: time.Since(started).Milliseconds(),
        Input:      traceInput(request),
        Output:     traceOutput(result, err),
    })
}
```

它导致以下问题：

- Trace 生命周期由 QA、MCP 等 Transport 分别管理；
- 业务函数重复执行 Enabled 判断、计时、状态判断和 Map 构造；
- 相同节点的评估协议与业务控制流耦合；
- 普通日志与评估 Trace 重复描述同一个执行阶段；
- 新增 QA、研发任务或多 Agent 节点时容易漏记；
- Recorder 的并发、Sequence、缓冲和导出逻辑存在重复实现。

目标不是把上述代码缩短为另一个 `trace.Record`，而是让 Trace 成为执行框架能力：

```go
result, err := svc.evidencePlanner.Execute(ctx, request)
```

调用方只执行业务能力。装配时注册的 Decorator 或 Executor Middleware 在调用前后自动通知当前 Run Scope。

## 3. 当前规模基线

截至 2026-08-07，Nasuta 当前实现具有以下规模：

| 项目 | 数量 |
|---|---:|
| 生产代码中的 `domain.RecordTrace` | 39 处 |
| 生产代码中的 `domain.TraceEnabled` | 11 处 |
| 直接包含 Trace 逻辑的生产文件 | 9 个 |
| Trace 相关生产与测试文件 | 15 个 |
| 已出现的稳定节点名 | 33 个 |
| QA、Agent、Tool、Retrieval 等主要受影响文件总行数 | 约 8,300 行 |

主要分布：

```text
internal/agent/service.go
internal/agent/loop.go
internal/agent/tools.go
internal/retrieval/collection.go
internal/retrieval/pipeline.go
internal/retrieval/rerank.go
internal/sessionhistory/service.go
internal/transport/dashboard/qa.go
internal/transport/mcp/server.go
```

当前 Recorder 有两套 Transport 实现：

- QA SSE：`internal/transport/dashboard/qa.go`；
- MCP：`internal/transport/mcp/server.go`。

评估协议还被 sibling `codeloom-eva` 直接消费：

- QA 客户端固定发送 `"trace": true`；
- MCP 客户端固定发送 `"_trace": true`；
- Runner 直接识别 `evidence_plan`、`vector_search`、`retrieval_assemble`、`candidate_rerank` 和 `agent_model_turn`；
- 路由评估直接读取 `proposed_sources`、`effective_sources` 和 `effective_confidence`。

因此改动的主要风险不是代码编译，而是评估协议发生无意漂移。

## 4. 目标架构

```text
QA / Feature Delivery / Workflow API
                 │
                 ▼
          ManagedRuntime.Begin
                 │
                 ▼
       Execution Trace Run Scope
                 │
      ┌──────────┼───────────┐
      ▼          ▼           ▼
  QA Executor  Workflow    Multi-Agent
      │        Executor      Child Run
      └──────────┼───────────┘
                 ▼
       Middleware / Decorator
                 │
                 ▼
             Recorder
                 │
       ┌─────────┼──────────┐
       ▼         ▼          ▼
      SSE       MCP      Evaluation
    Exporter  Exporter     Exporter
```

架构拆成四个职责：

1. **Run Scope**：管理一次执行的 Trace 生命周期和关联标识；
2. **Execution Interceptor**：在正式执行边界前后自动回调；
3. **Trace Spec**：定义节点名、字段投影和业务状态；
4. **Exporter**：保持 QA SSE、MCP 和后续持久化输出彼此独立。

## 5. Run Scope

### 5.1 生命周期归属

Recorder 不再由 QA Handler 或 MCP Handler 创建。它归属于已经存在的 Managed Run 边界：

```text
Begin
  -> 创建 Scope
  -> Context 挂载 Scope
  -> Execute
  -> Finish
  -> Flush / Close
```

`definitionManagedRun.Context` 当前已经统一挂载 Usage Recorder 和 LLM Call Lifecycle Observer，Execution Trace 应在同一位置接入。这样 QA、Workflow Agent 节点和后续多 Agent 子 Run 共用一个生命周期机制。

建议概念接口：

```go
type Scope interface {
    Enabled() bool
    Begin(Operation, Fields) Completion
    Record(Event)
    Finish(error) error
}

type Completion func(Outcome)
```

接口表达真实生命周期，不要求业务代码直接调用。绝大多数 `Begin/Completion` 由 Executor Middleware 使用。

### 5.2 Trace Mode

Transport 只负责把外部请求转换成统一 Trace Mode：

```text
trace: false / 无 _trace  -> disabled
trace: true / _trace      -> evaluation
```

第一阶段不需要引入复杂状态机。Mode 只表示本次 Run 是否创建评估 Recorder。后续若确有采样、审计或持久化行为差异，再增加有明确语义的模式。

### 5.3 父子 Run

多 Agent 子 Run 继承：

```text
trace_id
parent_run_id
workflow_run_id
```

并拥有自己的：

```text
agent_run_id
workflow_node_id
```

Sequence 只要求在同一 Scope 内稳定递增。跨并行子 Run 的全局顺序不能依赖 goroutine 完成时序，聚合查看时使用父子关系和节点时间排序。

## 6. 自动回调机制

### 6.1 已有统一 Executor 的节点

以下节点可以做到业务调用方零显式 Trace：

- `ManagedRun.Execute`；
- Workflow `NodeExecutor.Execute`；
- Tool Executor；
- LLM 物理调用；
- Multi-Agent Dispatch 和 Child Run；
- Feature Delivery 中拥有明确 Executor 的节点。

以 Workflow 为例，当前已经有：

```text
NodeStarted
NodeSucceeded
NodeFailed
```

增加组合式 `TraceRunObserver` 即可，无需在每个 Workflow 业务节点中调用 Trace。

### 6.2 拥有业务接口的能力

Planner、Retriever、Memory 等通过装配层 Decorator 接入：

```go
planner := NewTracedEvidencePlanner(realPlanner, EvidencePlanSpec)
retriever := NewTracedRetriever(realRetriever, RetrievalSpec)
```

业务调用保持不变：

```go
plan, err := svc.planner.Execute(ctx, request)
context, err := svc.retriever.RetrievePlan(ctx, plan)
```

Decorator 内部可使用统一 Invoke Helper：

```go
return executiontrace.Invoke(ctx, EvidencePlanSpec, request, next.Execute)
```

计时、Panic、默认状态、错误分类和 Recorder 调用都由 `Invoke` 负责。

### 6.3 QA 大函数中的内部阶段

当前 QA 准备流程在 `internal/agent/service.go` 中混合了分析、历史组装、证据规划、时间解析、Memory 和检索分发。Go 运行时无法自动知道普通函数片段对应哪个评估节点。

要移除这些位置的显式 Trace，有两个选择：

1. 将有独立输入、输出和失败语义的阶段提炼为正式执行节点；
2. 对不值得节点化的瞬时结果保留少量 `Scope.Record`。

建议优先提炼：

```text
QueryAnalyzer.Execute
ContextAssembler.Execute
EvidencePlanner.Execute
MemoryRecaller.Execute
Retriever.Execute
```

不建议通过反射、函数名、日志文本或 goroutine 调用栈推断节点。该方式无法稳定表达 `effective_sources` 等业务字段，重命名函数也会破坏评估合同。

## 7. Trace Spec

### 7.1 作用

Trace Spec 集中声明：

- 内部 Operation ID；
- 对外 v1 Node 名；
- Input 字段投影；
- Output 字段投影；
- `completed`、`degraded`、`failed` 等状态推导；
- 字段脱敏、截断和数量上限。

概念示例：

```go
var EvidencePlanSpec = Spec[PlanRequest, PlanResult]{
    Operation: "qa.evidence_plan",
    Node:      "evidence_plan",
    Input:     projectEvidencePlanInput,
    Output:    projectEvidencePlanOutput,
    Status:    evidencePlanStatus,
}
```

Spec 应按领域放在对应 Decorator 附近，由 Composition 统一装配；不建议建立一个包含所有业务类型的全局大注册文件。

### 7.2 状态不是简单的 error 映射

通用执行器只能提供默认规则：

```text
err == nil -> completed
err != nil -> failed
panic      -> failed，记录后重新抛出
```

业务降级必须由 Spec 显式投影。例如 Reranker 失败后保留 Recall 顺序，整个节点应是 `degraded`，不能被通用执行器误判为 `failed` 或 `completed`。

### 7.3 延迟投影

Input/Output 投影必须延迟执行。Trace 未开启时：

- 不创建 `map[string]any`；
- 不复制候选列表；
- 不排序或截断仅供 Trace 使用的数据；
- 不读取完整 Prompt、代码或工具结果。

## 8. 协议兼容

迁移期间继续输出 Trace v1：

```json
{
  "sequence": 1,
  "node": "evidence_plan",
  "status": "completed",
  "elapsed_ms": 10,
  "duration_ms": 5,
  "input": {},
  "output": {}
}
```

必须保持：

1. QA SSE 的 `event: trace`；
2. MCP 对象结果中的 `_trace`；
3. 现有节点名；
4. `codeloom-eva` 已读取的字段类型和含义；
5. Trace 关闭时现有业务响应不变。

可以新增 `schema_version`，但第一阶段应由 Exporter 默认产生 v1，不能要求所有业务节点同时修改。

建议建立跨仓库 Golden Contract Test，至少固定：

```text
evidence_plan
vector_search
retrieval_assemble
candidate_rerank
agent_model_turn
```

以及：

```text
proposed_sources
effective_sources
effective_confidence
```

## 9. 与 slog 和其他事件的边界

Execution Trace 不与 `slog` 合并门面。

| 类型 | 回答的问题 | 典型内容 |
|---|---|---|
| Operational Log | 系统为什么失败或降级 | Provider 错误、配置缺失、重试、告警 |
| Execution Trace | 一次 Run 执行了哪些评估节点 | 节点状态、耗时、受控输入输出字段 |
| Product Event | 用户当前应该看到什么 | SSE Phase、回答增量、工具进度 |
| Evaluation Fact | 如何统计、比较和审计 | Token、成本、评分、Review Label |

四者共享 `trace_id`、`run_id`、`workflow_run_id` 和 `agent_run_id`，但不共享生命周期、Schema 或 Exporter。

迁移完成后，普通日志应只保留运维价值。与 Trace 完全重复的 `[qa]` INFO 日志可以删除或降级，错误和明确降级日志继续保留。

## 10. 改动范围

### 10.1 Nasuta 新增能力

建议新增一个边界清晰的包：

```text
internal/executiontrace/
  scope.go
  recorder.go
  invoke.go
  exporter.go
  protocol.go
```

预计新增 800–1,300 行生产代码和测试，具体取决于 Exporter 是否同批迁移、是否使用泛型 Helper，以及字段限制策略的测试深度。

### 10.2 Nasuta 修改模块

| 模块 | 主要修改 | 规模 |
|---|---|---|
| `internal/domain` | 迁移或兼容现有 EvaluationTrace 合同 | 小 |
| `internal/agent/definition_runtime.go` | Scope 生命周期接入 | 中 |
| `internal/transport/dashboard` | 删除 QA Recorder 创建，保留 SSE Exporter | 中 |
| `internal/transport/mcp` | 删除 MCP Recorder 重复实现，保留结果投影 | 中 |
| `internal/agent/service.go` | QA 准备阶段节点化并移除显式 Trace | 大 |
| `internal/agent/loop.go` | Model Turn、预算和结论节点迁移 | 中到大 |
| `internal/agent/tools.go` | Tool Executor 拦截 | 中 |
| `internal/retrieval` | Pipeline 与 Rerank 节点迁移 | 大 |
| `internal/sessionhistory` | Recall 节点迁移 | 小 |
| `internal/agentworkflow` | Trace Observer、父子 Run 关联 | 中 |
| `internal/featuredelivery` | 研发任务节点接入 | 中，后续批次 |

预计直接修改 15–25 个生产文件、10–20 个测试文件。若只完成 Recorder 收敛，不做 QA 节点化，影响可控制在 8–12 个文件。

### 10.3 CodeLoom

CodeLoom 通过 Nasuta 公共 Surface 使用能力，原则上不应引入 Execution Trace 业务实现。可能修改：

- 应用 Composition 中的 Exporter 或 Factory 装配；
- 当前协议文档链接；
- 场景级验收测试。

预计 2–5 个文件。

### 10.4 codeloom-eva

生产解析逻辑应保持不变，主要增加或调整：

- Trace v1 Golden Contract Test；
- `tracecontract.EventV1` 是 Trace v1 的公共字段与 JSON 契约，Eva 使用同一类型覆盖 QA SSE 与 MCP `_trace` 消费；
- QA SSE 与 MCP `_trace` 兼容测试；
- 多 Agent 父子节点聚合测试。

预计 3–6 个测试文件。若协议保持兼容，不需要同步重写 Runner。

## 11. 分批实施与工作量

### 批次 A：协议冻结与公共 Recorder

工作内容：

- 固定 Trace v1 节点和字段；
- 建立 `internal/executiontrace`；
- 合并 QA/MCP Recorder 的 Sequence、Elapsed、并发和缓冲逻辑；
- 保持 SSE 与 MCP 输出不变。

预计：**3–5 人日**。

验收：现有 QA/MCP Trace 测试通过，`codeloom-eva` 无需修改解析代码。

### 批次 B：Run 生命周期与稳定执行器

工作内容：

- `ManagedRun` 自动创建、挂载和关闭 Scope；
- Tool Executor、LLM Call 和 Workflow Node 接入拦截器；
- 建立字段限制、错误分类、Panic 和并发测试。

预计：**3–5 人日**。

验收：这些执行边界的业务调用方不再出现 Trace 代码，Trace 关闭时不执行字段投影。

### 批次 C：QA 与 Retrieval 迁移

工作内容：

- 将分析、上下文组装、证据规划和 Memory 提炼为可执行能力；
- 迁移 Retrieval discover、expand、assemble 和 rerank；
- 迁移 Agent Model Turn 和结论节点；
- 删除重复 INFO 日志和旧 Trace 分支。

预计：**6–8 人日**。

验收：QA 和 Retrieval 主要业务路径不再显式调用 `TraceEnabled` 或 `RecordTrace`，现有节点和评估字段保持一致。

### 批次 D：研发任务与多 Agent

工作内容：

- Feature Delivery 节点接入 Scope；
- Multi-Agent Dispatch、Child Run 和 Aggregate 节点接入；
- 父子 Run Trace 关联和并行顺序测试；
- `codeloom-eva` 增加多 Agent 聚合评估测试。

预计：**4–6 人日**。

验收：单 Agent 与多 Agent 使用同一 Trace 机制，调用方不切换独立评估链路。

### 汇总

| 交付范围 | 累计投入 |
|---|---:|
| 只完成批次 A | 3–5 人日 |
| 完成 A+B | 6–10 人日 |
| 完成 A+B+C，推荐 QA 范围 | 12–18 人日 |
| 完成 A+B+C+D | 16–24 人日 |
| 继续追求所有内部瞬时事件零显式记录 | 20–30 人日 |

多人并行不能按人日线性缩短，因为协议冻结、Runtime 生命周期和 QA 节点边界有先后依赖。合理方式是一个人负责公共机制，另一个人在批次 B 稳定后并行迁移 Retrieval 或 Feature Delivery。

## 12. 风险与控制

### 12.1 协议漂移

风险最高。通过 v1 Golden Test、迁移前后事件快照对比和 `codeloom-eva` 回归控制。

### 12.2 过度抽象

不同业务接口签名不同，不应为了一个通用 Decorator 引入反射或复杂动态类型系统。公共包只统一生命周期和 Invoke 机制，各领域保留薄的类型安全 Adapter。

### 12.3 QA 行为回归

提炼执行节点时必须保持原有顺序、Context、Timeout、Fallback 和错误传播。每次只迁移一个阶段，迁移前后运行同一组 QA 测试和评估样本。

### 12.4 并发顺序误解

并行 Retrieval 和多 Agent 子任务不能承诺按业务定义顺序完成。Trace 使用 Scope Sequence 表示实际记录顺序，使用 Parent/Child 和 Operation ID 表达结构关系。

### 12.5 Trace 成本

关闭时不得构建字段；开启时限制单事件字段、候选数量、字符串长度、事件数和总字节数。达到上限时记录一次明确的 truncated 状态，不能无界增长。

### 12.6 日志被错误删除

迁移只删除与 Trace 重复且没有运维价值的 INFO 日志。Provider 错误、能力不可用、Fallback 和数据异常仍进入 Operational Log。

## 13. 验收标准

1. QA Handler 和 MCP Handler 不再创建各自 Recorder；
2. `ManagedRun` 统一拥有 Scope 生命周期；
3. Tool、LLM、Workflow 和 Multi-Agent 正式执行节点自动记录；
4. QA 与 Retrieval 主要业务路径不再显式判断 Trace Enabled；
5. Trace 关闭时字段投影函数不会执行；
6. Panic、错误、降级和取消都产生正确状态，且不改变业务错误传播；
7. QA SSE 与 MCP `_trace` 保持 v1 兼容；
8. `codeloom-eva` 现有路由、检索、Rerank 和 Agent 指标继续可计算；
9. 并发执行通过 Race Test，不串 Run、不重复 Sequence；
10. 普通日志、Product Event、Evaluation Trace 和持久化事实职责清晰。

## 14. 推荐决策

建议批准 **A+B+C** 作为第一期，预算 **12–18 人日**：

- 先解决 Recorder 和生命周期散落；
- 再覆盖最稳定的 Tool、LLM 和 Workflow 执行边界；
- 最后迁移 QA 与 Retrieval 的高频节点；
- 不把“所有瞬时事件零显式记录”作为第一期硬指标。

批次 D 与 QA 多 Agent 路由一起落地，预算增加 **4–6 人日**。这样既能让单 Agent 与多 Agent 从第一天共享同一个 Trace 基础，又不会让 Trace 重构阻塞 QA 业务链路的分阶段交付。

## 15. 实施结果

本方案已按批次 A–D 落地。当前实现与验收标准的对应关系如下：

| 验收项 | 实现位置 | 结果 |
|---|---|---|
| 统一 Scope 生命周期与 Trace ID | `internal/executiontrace/scope.go`、`internal/executiontrace/capture.go`、`internal/agent/definition_runtime.go`、`internal/agentworkflow/executor.go` | 已完成 |
| Tool、LLM、Workflow 与多 Agent 执行边界自动记录 | `tool/execution.go`、`internal/llm/client.go`、`internal/agentworkflow/executor.go` | 已完成 |
| QA、Retrieval、Memory、Feature Delivery 节点迁移 | `internal/agent/`、`internal/retrieval/`、`internal/sessionhistory/`、`internal/featurepipeline/`、`internal/featurereviewworkflow/` | 已完成 |
| 父子 Run、Workflow Node 与并行顺序关联 | `internal/executiontrace/scope.go`、`internal/agentworkflow/execution_trace.go` | 已完成 |
| QA SSE `event: trace` 与 MCP `_trace` 兼容 | `internal/transport/dashboard/qa.go`、`internal/transport/mcp/server.go` | 已完成 |
| Trace 字段投影、错误/Panic/取消状态与容量截断 | `internal/executiontrace/invoke.go`、`internal/executiontrace/scope.go` | 已完成 |
| 旧业务 Trace 调用收敛 | `internal/executiontrace/invoke.go` 保留唯一兼容桥，其余业务包不直接调用 | 已完成 |

实现阶段保留 `domain.EvaluationTrace`、`domain.TraceEnabled` 和 `domain.RecordTrace` 作为 Trace v1 兼容合同；它们不再承担各业务线的生命周期管理。后续若要移除兼容桥，应先完成协议 v2 或所有外部消费者迁移，再删除 `internal/executiontrace/invoke.go` 中的桥接。
