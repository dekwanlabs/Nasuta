# QA SSE 协议简化开发提案

[English](qa-sse-protocol-simplification.md) | [中文](qa-sse-protocol-simplification.zh-CN.md)

> 状态：已实现并归档，非规范性历史记录
> 日期：2026-08-01
> 归档日期：2026-08-01
> 影响范围：Nasuta QA/Agent/LLM；CodeLoom Dashboard QA
> 最终归并目标：`agent-platform/01-architecture-and-execution` 与 `08-observability-and-evaluation`

## 1. 文档生命周期

本文用于开发期间记录现状审计、目标协议、实施顺序和验收门禁，不代表当前代码合同。开发完成前，`agent-platform` 中的模块文档仍是权威基线。

只有同时满足以下条件，本文的有效结论才可归并到模块 01 和 08：

1. Nasuta 后端和 CodeLoom 前端完成切换；
2. 旧事件、旧字段和完成兜底已经删除，而非长期兼容；
3. 后端、前端、跨仓库合同测试和手工断流测试通过；
4. 实际实现与本文重新核对，文档按最终代码修正；
5. 归并后删除本文，或改为只保留决策与实施结果的历史记录。

## 2. 范围与非目标

本提案只简化 QA 回答在 LLM、Agent、RunHub、Dashboard 和前端之间的实时事件协议。

本次不做：

- 不把 SSE 改成 WebSocket；
- 不强制使用浏览器 `EventSource`；
- 不重构检索、Rerank、多样性或 Token 统计算法；
- 不新增通用消息队列、Replay Log 或持久事件溯源；
- 不把内部 `StepKind` 直接定义为公共 API。

Dashboard 继续使用 `POST /api/qa/ask`，通过 `fetch` 消费 `text/event-stream`。JSON Body、鉴权和取消控制使这种方式比原生 `EventSource` 更适合当前请求。

## 3. 当前链路

```text
OpenAI-compatible/Anthropic stream
  -> llm.StreamHandler
  -> agent.StreamPipe
  -> RunHub.SSEEvent
  -> dashboard emitHubEvent
  -> HTTP named SSE
  -> Vue fetch + ReadableStream parser
```

| 层 | 当前表达 | 转换行为 |
|---|---|---|
| LLM | Token、Reasoning、ToolCallDelta、ToolCall、Usage、FinishReason | Provider Chunk 归一化为回调和 `ChatStreamResult` |
| Agent Stream | Token、Reasoning、ToolCallDelta | Content 立即转发，遇到 Tool Delta 后才停止后续 Content |
| RunHub | Step、Token、Reasoning、LLMCall、Trace、Phase、Terminal 七个可选字段 | `SSEEvent` 充当 Untagged Union |
| Dashboard | StepKind Switch 与可选字段判断 | 将内部投影再次命名为 HTTP Event |
| Frontend | `curEvent` 字符串分支 | 分别修改回答、工具、状态、诊断和完成状态 |

Dashboard 当前发送 15 种命名事件：

```text
run_start, phase, progress, tool, tool_result,
token, reasoning, llm_timing, trace, context,
run_end, done, error, compaction, session_restart_recommended
```

`run_start/context/compaction/session_restart_recommended` 由 Handler 直接发送，其余多数从 `RunHub.SSEEvent` 转换，因此不存在一个完整、可穷尽检查的协议拥有者。

## 4. 问题清单

### 4.1 同一事实被重复表达

一段最终 Content 同时成为 Token 流、`think` Step 和 `answer` Step。一次成功同时由 `run_end`、`done`、HTTP EOF 和异常路径中的 Buffer 提交表达。调用方必须自行收敛本应唯一的答案和终态。

### 4.2 Tool Turn 先交付再回滚

`StreamPipe` 在首个 Tool Delta 到达前已经发布 Content。前端收到 `tool` 后清空 `streamingText`，但后端 `answerText` 不同步清空。因此 UI、Session 持久化候选和 Agent 最终答案可能不一致。

### 4.3 内部 Step 分类泄漏到 HTTP

Dashboard 根据 `StepKindThink/ToolCall/ToolResult` 再映射成 `progress/tool/tool_result`。`retrieval` 和 `answer` Step 虽被 Hub 广播，却被 HTTP Mapper 静默忽略。新增 StepKind 也没有编译期消费检查。

### 4.4 Untagged Union 允许非法组合

`SSEEvent` 使用七个可选字段表示事件。类型无法保证一次只有一种 Payload，也无法保证 Dashboard 对新增字段进行映射。

### 4.5 终态没有唯一权威

前端会在 `done`、无 `done` 的 EOF、以及 Catch 路径提交 Buffer。网络中断或协议解析失败可能被表现成成功的 Assistant Message。

### 4.6 业务流和诊断流混合

Reasoning、LLM Timing 和 Trace 与答案、工具及终态使用同一公共分发模型。普通聊天客户端被迫理解 Agent 内部观测概念。

### 4.7 前后端字段已经漂移

后端 `tool_result` 发送 `result_preview`，前端读取 `summary`，工具结果摘要可能为空。字符串事件和匿名 Map 无法在编译期发现这种错配。

### 4.8 手写 Parser 承担过多职责

前端 Parser 只处理单行 JSON `data:`，同时维护 Frame、JSON 和业务分发状态。除 `error` 事件外的 JSON 或分发异常通常被忽略，没有协议错误、序号或 Gap 检测。

## 5. 目标边界

事件只允许经过两个语义转换边界：

```text
Provider stream
  -> LLM adapter: content/reasoning/tool-call delta/usage/finish
  -> Agent: answer candidate/tool call/business step/RunOutcome
  -> Dashboard adapter: public SSE event
  -> Frontend dispatcher: UI state
```

- LLM Adapter 独占 Provider Chunk 兼容逻辑，不向上泄漏原始 Chunk。
- Agent 独占 Answer Turn、Tool Turn 和 RunOutcome 判定。
- Dashboard Adapter 独占公共协议映射，不从可选字段组合或 `StepKind` 猜测事件。
- Frontend 只理解公共协议，不使用内部 StepKind 推导生命周期。

## 6. 公共协议草案

Hub 使用 `Type + Data` 的 Tagged Event。Dashboard 不再转换事件，只把 `Type` 写入 SSE `event:`，把 `Data` 写入 SSE `data:`：

```json
{
  "step": 4,
  "tool": "search_code",
  "summary": "...",
  "failed": false
}
```

当前单连接、单 Run 的 SSE 天然有序，不增加 `seq`、Replay Log 或重连状态机。只有出现真实的断线恢复需求时，再单独设计序号与重放合同。

### 6.1 业务事件

| 事件 | 必要数据 | 前端行为 |
|---|---|---|
| `run.started` | Run ID、Mode | 记录当前 Run |
| `status` | Text、可选状态码 | 替换瞬时状态文案 |
| `answer.delta` | Text | 追加已确认的回答文本 |
| `tool.started` | Step、Name、Args | 添加工具调用 |
| `tool.finished` | Step、Name、Summary、Failed、Duration | 完成对应工具调用 |
| `context` | References、HitCount | 更新证据展示 |
| `run.finished` | Status、Answer、Evidence、Error | 唯一提交或失败入口 |

Session 压缩统一为 `session.status`；只有确实需要新会话时才发送 `session.restart_recommended`。

### 6.2 诊断事件

`reasoning.delta`、`trace` 和 `llm.call` 是可选诊断事件。关闭或丢弃诊断事件不得改变工具执行、答案、终态或前端提交行为。

## 7. Answer Turn 与 Tool Turn

一轮结束前，Content Delta 只是候选文本，因为后续 Chunk 仍可能出现 Tool Call。

- 无 Tool Call：按顺序发布候选文本为 `answer.delta`；
- 有 Tool Call：候选文本只进入内部审计投影，不发布 `answer.delta`；
- 最终 `answer` Step 只负责持久化和审计，不重播相同实时文本。

前端不得在 `tool.started` 后回滚已展示答案。若未来要求绝对最低的逐 Token 延迟，必须先获得 Provider 可保证的 Answer/Tool 判别信号，不能继续使用“先展示再清空”。

## 8. 完成与断流语义

每个 Run 恰好产生一个 `run.finished`：

- 仅 `status == "done"` 且 `answer` 非空时提交 Assistant Message；
- `failed` 和 `aborted` 不提交残缺答案；
- `run_end`、`done`、Error 后补发完成、Channel Close 和 EOF 成功兜底全部删除；
- `run.finished` 前断流属于传输失败，前端展示失败或按 Run ID 查询最终结果；
- SSE 断连本身不修改 Run 的业务终态。

## 9. 交付与慢订阅者

| 优先级 | 事件 | 策略 |
|---|---|---|
| 终态 | `run.finished` | 落库后发布，不可静默丢弃，可按 Run ID 查询 |
| 有序业务流 | Answer、Tool、Context | 保持 Run 内顺序；丢失时报告 Gap |
| 可合并瞬时流 | `status` | Buffer 压力下只保留最新值 |
| 可丢诊断流 | Reasoning、Trace、LLM Call | 可关闭或丢弃并计数 |

Hub 使用每订阅者有界 Buffer。广播不得持有全局锁等待网络写入；慢订阅者不能阻塞 Agent 或其他 Run。无法接收不可丢业务事件时，断开该订阅并记录原因，以持久化 Run、Step 和 Terminal 作为恢复来源。不要引入无限 Buffer 或通用 Replay Log。

## 10. 开发切片

### P0：建立合同测试

1. 固化当前事件样本和跨仓库测试入口；
2. 修复 `result_preview/summary` 错配；
3. 覆盖 Chunk 拆分、CRLF、多 Frame、多行 `data:`、非法 JSON 和无 Terminal EOF。

### P1：建立 Canonical Event

1. 用 `Type + Data` Tagged Event 替代七字段 Untagged Union；
2. Dashboard 原样转发 Hub 事件，不保留 StepKind Mapper；
3. 不增加当前流程不需要的序号和重放机制；
4. 前后端一次切换，不长期双写旧协议。

### P2：切换前端

1. 提取独立、可测试的 SSE Frame Parser；
2. 按 `type` 分发 Typed Payload；
3. 只在 `run.finished` 提交消息；
4. 删除 Tool 到达后的答案清空、EOF 提交和 Catch 提交。

### P3：收敛 Agent 输出

1. Tool Turn 的候选 Content 不进入 `answer.delta`；
2. 删除最终答案的重复 `think` 实时投影；
3. 明确 `retrieval/answer` Step 为持久化专用，或为真实 UI 需求定义独立事件；
4. 诊断事件从业务完成逻辑中移除。

### P4：删除兼容逻辑

删除 `token/tool/tool_result/run_end/done` 旧映射、兼容 Adapter、可选字段 Union 和所有完成兜底。代码搜索必须确认旧事件名已从生产分发路径消失。

## 11. 验收门禁

1. Tool Call 前言从不进入 `answer.delta`；
2. 前端不会因为后续工具事件清空已交付答案；
3. 每个 Run 恰好一个 `run.finished`；
4. 无 Terminal EOF、解析错误和断连均不能提交成功答案；
5. `failed/aborted` 不保存 Assistant Message；
6. Tool Started/Finished 可按 ID 或 Step 稳定配对，字段名完全一致；
7. 禁用全部诊断事件不改变业务结果；
8. 慢订阅者不阻塞其他 Run，Terminal 可在断连后查询；
9. Backend Race Test、Dashboard Handler Test、Frontend Parser/Dispatcher Test 和跨仓库 E2E 通过；
10. 旧事件和兜底删除后，再将最终实现归并到模块 01 和 08。

## 12. 待开发时确认

- `answer.delta` 是在完整 Turn 判定后一次发布，还是按已确认区间分批发布；默认采用完整 Turn 判定后的单次或分块发布，优先正确性。
- Tool 配对使用 `tool_call_id` 还是 Step Number；默认以 `tool_call_id` 为稳定主键，Step Number 只用于展示顺序。
- 断连后的查询端点是否已有足够的有界 Run Detail；若没有，只补最小查询能力，不引入 Replay Log。

## 13. 实施结果

2026-08-01 已完成后端与前端切换：

- Hub 改为 Tagged Event，Dashboard 删除 StepKind 映射并原样转发；
- Agent 在完整 Turn 判定后才发布答案，Tool Turn 前言不再进入答案流；
- `run.finished` 成为唯一终态，携带权威 Answer、Evidence 和 Error；
- 前端删除 Tool 回滚、EOF 提交、Catch 提交及旧终态分支；
- SSE Parser 已独立，支持 Chunk 拆分、CRLF、多 Frame、多行 Data，并显式报告非法 JSON；
- Hub 广播不再持锁等待订阅者，慢订阅者满载时记录丢弃事件。

已通过 `go test ./internal/agent/... ./internal/transport/dashboard/...`、对应 Race Test、`go build ./...`、前端 Typecheck 和 Production Build。真实浏览器断流 E2E 未执行，作为非阻断的残余验证记录；实现已验收并归档，正式合同以模块 01 和 08 为准。
