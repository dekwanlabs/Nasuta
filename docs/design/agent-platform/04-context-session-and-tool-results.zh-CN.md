# 上下文、会话与工具结果

[English](04-context-session-and-tool-results.md) | [中文](04-context-session-and-tool-results.zh-CN.md)

> 状态：已实现
>
> 更新：2026-08-09
>
> 实现：新鲜工具结果无损交付、动态预算、Artifact、Trace、AnswerContract 双向校验与有界读取 API
> 来源：Session Summary、Context Pollution Control、Session History Retrieval、Turn Compaction、Structured Tool-output Compression

## 1. 结论

Nasuta 不会默认截断或压缩新鲜工具证据。只有回答前运行时上下文达到 80% 高水位时，才会为模型生成带覆盖元数据的临时压缩投影；Session、Trace 和 Artifact 中的权威原文保持不变。

核心不变量：

1. 工具执行只产生一份权威结果；
2. 正常大小的新鲜结果完整进入模型；
3. Session、Agent Trace 和审计链路保存完整结果或无损可恢复引用；
4. UI 可以折叠展示，但后端追踪数据不能截断；
5. 单次结果过大时，由工具分页、缩小查询或返回明确交付失败，不能伪装成完整成功结果；
6. 历史压缩只在真实上下文压力下处理旧轮次，不能删除权威原文；
7. 精确回答合同同时校验模型输入和最终答案；
8. 回答前压缩优先删除工具调用叙述、压缩较早工具结果，并为最新证据保留更高预算。

实现已删除 `sessionToolResultLimit = 1_200` 和新鲜结果默认 `tooloutput.Compress` 路径。历史摘要仍可生成有损投影，但完整工具原文已先保存在 Trace 或 Artifact 中。

## 2. 三个不同问题

工具结果治理包含三个独立问题，不能共用一个固定长度：

### 2.1 当前工具结果交付

当前 Run 刚得到的结果是生成答案的一手证据。它默认完整进入模型；只有整个回答上下文达到高水位时，Runtime 才能压缩模型侧副本，而且必须显式声明覆盖范围并保留可恢复原文。

### 2.2 Session 与 Trace 持久化

Session 保持多轮协议连续性，Trace 用于复现、审计和排障。二者都必须能恢复权威结果。

### 2.3 历史上下文维护

旧轮次持续增长后，需要在模型窗口内选择、归档和召回。这里可以生成摘要，但摘要不能替代权威原文。

这三个问题分别由工具边界、执行链路和历史维护机制负责。

## 3. 内容模型

### 3.1 工具公开合同

工具继续通过 `tool.Result` 返回业务结果和每次执行产生的动态合同：

```go
type Result struct {
    Content        string
    References     []Reference
    Coverage       EvidenceCoverage
    AnswerContract AnswerContract
}
```

其中：

- `Content` 是工具权威结果；
- `Coverage` 声明工具查询本身是否完整；
- `AnswerContract` 声明本次结果要求最终答案精确复制的值。

### 3.2 Runtime 执行内容

Runtime 可以区分权威内容和实际模型输入，但不能持久化一个可能被误用的截断摘要：

```go
type ToolExecution struct {
    AuthoritativeContent string
    PromptContent        string

    Coverage       tool.EvidenceCoverage
    AnswerContract tool.AnswerContract

    Arguments  string
    Failed     bool
    DurationMs int
}
```

默认必须满足：

```text
PromptContent == AuthoritativeContent
```

只有以下情况允许不同：

1. 确定性的无损格式转换；
2. 工具执行失败，`PromptContent` 是结构化错误；
3. 结果超出当前模型可用预算，Runtime 明确把本次交付标记为失败，并要求分页或缩小查询。

不允许把有损摘要作为成功的 `PromptContent`。

### 3.3 不再持久化 DisplaySummary

核心执行结构不定义 `DisplaySummary`。如果 UI 列表需要预览，应当在读取或渲染边界临时生成，不能成为业务数据源。

```text
权威内容：持久化、校验、回放、审计
UI 预览：临时计算、可折叠、非权威
```

## 4. 新鲜工具结果交付

目标流程：

```text
tool executes
  -> tool.Result.Content
  -> persist authoritative trace
  -> derive model payload without loss
  -> calculate remaining model budget
     -> fits: send complete role=tool message
     -> does not fit: mark delivery failed and request pagination/narrowing
  -> preflight AnswerContract
  -> append successful tool result to the active run
```

### 4.1 普通结果

普通结果原样发送：

```go
execution.AuthoritativeContent = result.Content
execution.PromptContent = result.Content
```

不能经过：

- query-relative 摘要；
- head/tail 截断；
- 数组条目采样；
- 精确标识符缩写；
- 用 `...` 或 `…` 替换中间内容。

59 条设备记录和完整 SN 属于普通业务结果，只要能进入当前模型窗口，就必须完整交付。

### 4.2 无损格式转换

格式转换必须满足可证明的等价性。例如 JSON 空白格式变化可以接受，但删除字段、改写值或省略数组元素不可以。

如果保留 `formatToolResultForLLM`，每个分支必须满足：

```text
所有权威字段和值仍可从 PromptContent 完整读取
```

声明了 `AnswerContract.RequiredLiterals` 的值必须逐字存在。

### 4.3 模型预算

可用预算由当前请求动态计算：

```text
available tool budget =
    provider context window
  - system and developer messages
  - selected recent atomic turns
  - current user request
  - already accepted current-run evidence
  - reserved final-answer tokens
  - safety margin
```

不能用固定的每工具 1,200 rune 或 10,000 token 代替这个计算。

## 5. 超大工具结果

### 5.1 工具边界优先有界

可能返回大量记录的工具必须在数据源边界支持：

- `limit` 或 `page_size`；
- 稳定 cursor；
- 必要字段投影；
- `total`、`has_more`、`next_cursor`；
- `Coverage.Partial` 和 `OmittedItems`。

存储查询必须在 SQL、搜索请求或外部 API 上执行限制，不能先全量加载再截断。

### 5.2 Runtime 不做静默降级

完整结果超出模型预算时：

1. 完整结果仍写入 Trace/Artifact；
2. 本次模型交付标记失败；
3. 向模型返回结构化错误，要求重新调用带分页或更窄条件的工具；
4. 失败结果不加入最终答案合同；
5. 不把截断内容伪装成完整工具成功。

示例：

```json
{
  "error": "tool_result_exceeds_context_budget",
  "tool": "lookup_customer_devices",
  "result_bytes": 824013,
  "artifact_id": "tool_result_01H...",
  "retry": {
    "page_size": 100,
    "cursor": null
  }
}
```

自动重试只适用于只读或明确幂等的工具。写工具不能因交付失败而被 Runtime 无条件重放。

### 5.3 Artifact

Artifact 保存超大权威结果：

```go
type ToolResultArtifact struct {
    ID          string
    SessionID   string
    RunID       string
    ToolCallID  string
    Content     []byte
    ContentType string
    SHA256      string
    SizeBytes   int64
    CreatedAt   time.Time
}
```

必须先成功保存完整 Artifact，Session 或 Trace 才能写入引用。引用必须包含：

- artifact ID；
- tool call ID；
- 内容类型；
- 大小和摘要哈希；
- coverage；
- 有界读取方式。

Artifact 访问必须按租户、用户和 Session 隔离。

## 6. Session 持久化

### 6.1 最近原子轮完整保存

一个轮次按完整协议保存：

```text
user
assistant(tool_calls)
tool results
assistant(final)
```

不能拆散 call/result，也不能只保存工具结果前缀。

### 6.2 两种无损表示

Session 中的工具结果有两种合法表示：

#### Inline

正常大小结果完整保存在工具消息中。

#### Artifact Reference

异常大或已归档结果保存无损引用。权威原文必须已经存在于 Artifact/Trace 中。

不允许第三种“截断前缀”表示。

### 6.3 固定 1,200 rune 截断已删除

`sessionToolResultContent` 和 `sessionToolResultLimit` 已删除。正常大小的 Session 工具消息保存完整 `PromptContent`，当前实现中它与权威 `Content` 相同；超限交付则保存带 `artifact_id` 的结构化失败消息，完整权威内容先写入 Artifact。

`summaryToolProjectionRunes = 1_200` 只限制旧轮次摘要的模型输入投影，不参与新鲜工具交付、Session 原子轮保存或 Trace 权威存储。

## 7. Agent Trace、UI 与日志

### 7.1 Agent Trace 是权威追踪记录

每次工具执行应保存：

```go
type ToolResultTrace struct {
    TraceID     string
    RunID       string
    ToolCallID  string
    ToolName    string
    Arguments   string

    AuthoritativeContent string
    PromptContent        string

    AuthoritativeSHA256 string
    PromptSHA256        string
    SizeBytes           int64

    Coverage       tool.EvidenceCoverage
    AnswerContract tool.AnswerContract
    Failed         bool
    DurationMs     int
}
```

Trace 必须能够回答：

1. 工具实际返回了什么；
2. 模型实际看到了什么；
3. 两者是否发生变化；
4. 经过了什么转换；
5. 合同为何通过或失败。

正常结果的两个哈希应该相等。若不同，Trace 必须有明确原因，且不能把有损转换标记为成功交付。

如果内容过大，Trace 可以保存 Artifact 引用，但该引用必须无损可恢复。逻辑上仍然保存完整内容。

### 7.2 UI 展示完整结果

工具调用详情页必须能够读取和查看完整参数、完整工具结果、实际模型输入以及合同校验结果。

UI 可以：

- 默认折叠大文本；
- 按需展开；
- 对 Artifact 分页或分片读取；
- 在列表页只读取元数据。

UI 不可以：

- 只保存一个 1,200 字符摘要；
- 把预览字段当成详情内容；
- 因为前端折叠而修改后端权威结果。

如果保留 `ResultSummary`，必须改名为 `ResultPreview` 并明确为非权威字段。更推荐删除持久化预览，由列表 API 按需生成或只返回大小、状态和记录数。

### 7.3 日志

Agent Trace/审计日志必须完整或无损可恢复。普通进程日志可以只记录索引信息，避免在 stdout 重复写入敏感数据：

```text
trace_id, run_id, tool_call_id, tool, bytes, sha256, artifact_id
```

这里的“普通日志不重复 payload”不等于截断权威日志；完整 payload 始终可以通过 Trace ID 找回。

## 8. AnswerContract

### 8.1 合同生成时机

合同由工具在得到本次查询结果后构造：

```go
return tool.Result{
    Content: string(encoded),
    Coverage: tool.EvidenceCoverage{
        Partial:      partial,
        OmittedItems: omitted,
    },
    AnswerContract: tool.AnswerContract{
        RequiredLiterals: requiredDeviceSNs(results),
    },
}, nil
```

`requiredDeviceSNs(results)` 在构造 `tool.Result` 时调用一次，从本次真实结果提取完整 SN。它不是全局注册回调。

### 8.2 合同作用域

只聚合当前 Run 中实际成功交付给模型的工具结果合同：

```text
current run
  -> successful tool result A: no contract
  -> successful tool result B: required literals
  -> failed delivery C: not active
  -> final answer validates contract B only
```

工具注册表不持有跨请求的全局 RequiredLiterals。

### 8.3 模型输入预检

在把工具消息加入成功执行链路前：

```text
for every required literal:
    literal must exist verbatim in PromptContent
```

缺失时：

1. 将本次工具交付标记为失败；
2. 不进入最终答案生成；
3. 尝试恢复完整权威内容，或要求分页重试；
4. 记录缺失值数量和 Trace ID；
5. 不把模型无法看到的值加入最终回答合同。

该预检能够直接发现“原始 SN 完整，但发给模型时变成 `…xxx…`”的问题。

### 8.4 最终答案后检

模型生成答案后，逐字检查所有有效 `RequiredLiterals`：

1. 首次缺失时携带缺失列表重新生成；
2. 重试次数有界；
3. 仍缺失则返回 `ErrAnswerContractViolation`；
4. 残缺答案不能作为成功响应交付。

合同错误不是通过再次压缩、掩码或猜测修复。

## 9. 历史上下文维护

### 9.1 高低水位

历史维护根据实际 Provider 输入 token 触发：

```text
below low watermark
  -> keep selected recent history unchanged

above high watermark
  -> archive stale atomic turns
  -> keep recent atomic tail verbatim
  -> recall only relevant archived history
  -> reduce to low watermark
```

水位比例可配置，不应硬编码为某个工具字符上限。

### 9.2 压缩对象

允许压缩：

- 已完成且已归档的旧轮次模型投影；
- 用于话题定位和结论回顾的摘要；
- 非精确型旧证据的召回表示；
- 回答前达到 80% 高水位后，当前 Run 较早工具结果的模型侧结构化投影。

禁止压缩：

- 当前问题；
- 最近保留的原子轮；
- 尚未归档的权威工具结果；
- 精确标识符的唯一副本。

回答前运行时压缩不改写持久化对象。它先清除非权威的工具调用叙述，再按从旧到新的顺序压缩工具结果，目标回落到上下文窗口约 60%；最新工具结果使用更高保留下限。只有硬窗口仍会溢出时，才进入更激进的应急预算。

每个实际生成的工具结果投影都会在 `answer_context_compaction` Trace 节点记录 Tool Call ID、工具名、压缩策略、来源/压缩前/保留 tokens 及 coverage 字段。压缩完成后 Runtime 还会发送普通 `status` 事件，让回答页面显示本轮已在生成答案前整理上下文。

### 9.3 Summary 合同

Summary 用于任务连续性，保留：

- 用户长期约束；
- 当前目标；
- 已作决策及理由；
- 文件、接口和关键标识符；
- 命令和结果；
- 未完成工作与下一步。

Summary 不承担大规模精确列表的唯一存储。需要完整证据时，从 Trace/Artifact 回放或重新调用工具。

### 9.4 历史选择

在线路径只读取有限候选元数据，并按以下关系选择：

- 用户显式引用某一轮；
- 当前问题依赖上一轮原始证据；
- 实体和 topic 相关度；
- 时间衰减；
- 新旧实体冲突。

依赖原始证据时回放完整原子轮或 Artifact 分片；只依赖结论时可以使用结构化摘要。

## 10. 实现状态

截至 2026-07-31，当前实现如下：

| 位置 | 已实现行为 |
|---|---|
| `internal/agent/loop.go` | 正常新鲜工具结果原样进入模型；每次调用按当前消息、工具定义、答案预留和安全余量动态计算预算；交付失败后不激活答案合同 |
| `internal/agent/tool_delivery.go` | 生成确定性 Trace/Artifact ID、SHA256、大小和结构化交付错误；超限结果保留完整 Artifact，模型只看到明确的分页或缩小查询要求 |
| Session 工具消息 | 删除固定 1,200 rune 截断；正常结果完整 inline，超限结果保存可恢复的 Artifact 引用 |
| `agent_steps` | 保存权威内容、实际模型输入、两个哈希、coverage、AnswerContract、失败原因和 Artifact 引用；不再持久化 `ResultSummary` |
| `agent_tool_result_artifacts` | Artifact 先于 Step 引用在同一事务内写入；内容使用 `LONGBLOB`，按用户和可选 Session 隔离 |
| `internal/agent/answer_contract.go` | RequiredLiteral 先在 `PromptContent` 中预检；最终答案缺失时最多重试两次，仍失败则返回 `ErrAnswerContractViolation`；合同只聚合当前 Run 成功交付的结果 |
| `internal/agent/execution/answer_context_compaction.go` | 每次后工具模型调用及强制结论前检查 80% 高水位；只改写运行时消息副本，优先保留最新证据、协议配对和精确回答合同，并输出逐工具 Trace 与页面状态 |
| `formatToolResultForLLM` | 当前为恒等转换，不删除字段、数组元素、JSON 尾部或精确标识符 |
| `summary.go` / `turn_detail.go` | 只生成旧历史的有损召回投影；权威原文已由 Trace/Artifact 独立保留，投影不是详情数据源 |
| Run/Artifact API | Run 详情先校验用户归属；普通 Trace 返回完整内容，Artifact 返回临时预览并通过有界 API 分片读取；单次最多 256 KiB |
| SSE 与日志 | SSE 只广播临时 `result_preview` 和 Trace 元数据；进程日志记录 Trace ID、Tool Call ID、大小、SHA256、Artifact ID 和失败原因，不把预览当作权威内容 |

数据库升级脚本位于 `docs/sql/migration_agent_tool_result_trace.sql`。它移除 `result_summary`，回填已有 Step 的模型输入与哈希，并创建 Artifact 表。

## 11. 实施结果

### 阶段 A：停止不可恢复丢失

- 已删除 Session 固定截断；
- 已移除新鲜工具结果默认语义压缩；
- `formatToolResultForLLM` 已收敛为无损恒等转换；
- Session 和 Trace 正常路径保存完整结果；
- RequiredLiteral 在模型输入侧预检。

### 阶段 B：清理预览与追踪语义

- `ResultSummary` 已从新 schema 和迁移中删除；
- `ResultPreview` 只在 Run 详情或 SSE 边界临时生成；
- Trace 保存 `PromptContent`、权威内容哈希和模型输入哈希；
- Run 详情、Run Control 和 Artifact 读取均执行用户归属校验。

### 阶段 C：大结果能力

- Runtime 使用当前请求动态预算，不使用固定工具字符上限；
- 超限结果先完整写入 Artifact，再写 Step 引用；
- Artifact API 使用数据库 `SUBSTRING` 有界读取并保持 UTF-8 边界；
- 交付失败返回结构化可重试错误，Runtime 不自动重放写工具。

### 阶段 D：历史维护

- 现有 Session 压缩继续由高低水位触发并保留近期原子轮；
- 回答前先完成预取、记忆召回和证据检索，再按“未压缩历史 + 本轮检索/召回及系统输入 + 输出预留”投影，达到上下文窗口 80% 时触发；
- Agent 工具循环产生新证据后，在下一次模型调用和强制结论前再次按“当前模型输入 + 输出预留”检查 80% 高水位，并将模型侧临时上下文压到约 60%；
- 回答完成后的空闲压缩复用同一 80% 高水位，不再使用独立的低比例历史预算；
- 压缩选择以约 60% 为目标，并始终保留最近 3 个原子轮；
- Summary 和归档详情只承担召回投影；
- 新鲜工具证据已退出通用结构化压缩器默认路径，只保留回答前高水位保护；
- Session 删除时同步删除关联 Artifact，避免孤儿权威内容。

## 12. 验证

当前回归测试覆盖以下行为，主要位于 `internal/agent/tool_delivery_test.go`、`internal/agent/tool_result_store_test.go`、`internal/agent/agent_test.go` 和 `internal/agent/stream_test.go`：

1. 正常工具结果字节级进入模型；
2. 59 个完整 SN 全部存在于模型输入和最终答案；
3. 下一轮仍能读取完整 SN；
4. 任意 RequiredLiteral 不在 `PromptContent` 时，工具交付失败；
5. 任意 RequiredLiteral 不在最终答案时，有界重试后硬失败；
6. JSON 尾部、数组后续元素和 `next_cursor` 不丢失；
7. Session 中 call/result/final 原子关系不被拆散；
8. UI 工具详情可查看完整结果；
9. Trace 能比较权威结果与模型输入的哈希；
10. 超大结果先完整保存 Artifact，再写引用；
11. 未达到上下文高水位时不压缩旧轮次；
12. 历史投影丢失不会删除权威原文；
13. 日志可通过 Trace ID 定位完整结果；
14. 普通小工具不再触发通用语义压缩；
15. 工具循环达到高水位后，在下一次模型调用前压缩较早结果并保留最新结果；
16. 步数耗尽后的强制结论使用压缩后的运行时上下文；
17. 回答前压缩不修改 Session、Trace 或 Artifact 中的权威结果。

## 13. 验收标准

当前实现满足：

- 工具结果不存在不可恢复的固定前缀截断；
- 正常新鲜结果在工具、模型、Session 和 Trace 之间保持一致；
- UI 折叠不改变后端权威内容；
- Agent Trace 完整记录工具实际返回和模型实际输入；
- 超大结果不会被伪装成完整成功；
- AnswerContract 能同时阻止输入侧和输出侧的精确值丢失；
- 历史维护只影响模型投影，不删除可审计原文；
- 所有压缩、归档、恢复和合同失败均可观测。
