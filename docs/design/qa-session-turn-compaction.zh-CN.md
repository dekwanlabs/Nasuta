# QA 会话逐轮压缩与详情回查设计

## 目标

长会话的模型上下文与持久化记录采用不同生命周期：

- `qa_messages` 保存原始会话消息，`agent_steps` 保存完整工具执行证据；
- 模型上下文达到窗口的 80% 后，压缩最近三轮之前的每个完整轮次；
- 每轮生成独立、稳定的 `ref`，禁止一个引用覆盖多轮；
- Rolling summary 只保留逐轮短摘要和对应 ref；
- 详情工具只接受 ref，并且一次只返回该轮有明确上限的压缩详情；
- 压缩失败可观测，但不破坏原始消息，也不允许旧任务覆盖新进度。

该机制解决跨轮次上下文增长，不替代单次工具输出限制和运行内工具结果维护。

## 轮次边界

一次问答是一个逻辑轮次：

```text
user
assistant(tool_calls)
tool result × N
assistant final answer
```

`saveTurnToSession` 在一个事务内为整组消息分配相同的 `turn_no`，并记录产生该轮的 `run_id`。工具调用与结果按 `seq` 排序并保持协议配对。现有数据通过显式迁移按每个 `user` 消息开始新轮次完成回填；运行时不保留兼容推断。

## 数据模型

### qa_sessions

保留：

- `summary`：由逐轮 `ref/text` 行组成的Rolling summary主体；
- `compacted_through_turn`：已连续压缩到的最后轮次。

不再保存单一 `summary_ref`，因为不存在覆盖多轮的累计压缩码。

### qa_turns

每轮一行，保存 `turn_no`、`run_id`、`first_seq`、`last_seq` 和入口处计算的 `token_estimate`。它负责轮次边界，不重复保存消息正文。

### qa_turn_contexts

每个已压缩轮次一行：

- `ref`：由 `session_id + turn_number` 确定生成；
- `session_id`、`user_id`、`run_id`、`turn_number`；
- `text`：该轮有上限的压缩详情；
- `summary_text`：注入Rolling summary的短摘要；
- `source_tokens`、`retained_tokens`；
- `created_at`。

唯一键 `(session_id, turn_number)` 保证一轮只有一个 ref。对外详情对象为：

```json
{
  "ref": "cmp_xxx",
  "sessionId": "session_xxxx",
  "userId": "1",
  "text": "该轮经过压缩的详情内容",
  "turnNumber": 1
}
```

`sessionId` 是QA会话ID；Agent运行ID单独存为 `run_id`，禁止混用。

## 单轮详情压缩

详情压缩是确定性的结构化裁剪，不再调用LLM自由改写事实。原始消息和完整工具证据继续保留，模型只读取压缩详情。

每轮详情最多约2400 token，结构固定为：

```text
TURN N

USER
<question>

TOOL CALLS
<tool name + JSON args>

TOOL RESULTS
<bounded evidence/error excerpts>

ASSISTANT
<final answer>
```

处理规则：

1. 删除系统提示词、隐藏推理、阶段提示和其他运行噪声；
2. 用户问题优先完整保留，超限时最多400 token；
3. 工具调用保留名称、参数JSON和顺序，合计最多600 token；
4. 工具结果使用现有结构化压缩器，错误信息优先，合计最多1000 token；
5. 最终回答优先保留结论、标识符和首尾，最多400 token；
6. 所有省略均写入原始大小、保留大小和省略标记，不能把未覆盖内容解释为不存在。

每轮的 `summary_text` 由LLM生成，最大120 token。一次压缩多个旧轮次时使用一个批量摘要调用，并严格校验输出中的 ref 集合与输入完全一致。无效响应会导致本次压缩失败，不静默切换模型或生成无来源摘要。

## 触发和滚动

模型上下文窗口由平台设置 `llm_context_window` 明确配置，默认128000。轮次写入后，满足以下任一条件时开始压缩：

- 当前运行的真实 `peak_input_tokens >= context_window × 80%`；
- 尚未压缩轮次的持久化 token 估算达到同一阈值。

首次压缩时，为 `latest_turn - 3` 之前的每轮分别生成 ref。压缩一旦启动，此后每新增一轮就压缩刚离开最近三轮窗口的那一轮，使原文窗口始终保持最近三轮。

摘要生成和详情压缩在数据库事务外执行。提交时锁定 session，并要求 `compacted_through_turn` 仍等于任务看到的旧边界；过期任务不允许覆盖更新结果。所有新轮次上下文记录和session摘要在一个事务内发布。

## Rolling summary格式

当前上下文固定为：

```text
<rolling_summary>
ref=cmp_001, text=第1轮确认了客户端标识必须使用clientId:userId完整格式。
ref=cmp_002, text=第2轮确认ES URL过滤必须同时包含链接主体和参数条件。
instruction=Use get_session_turn_details only when exact prior wording, identifiers, tool arguments, or evidence are necessary and this summary is insufficient.
</rolling_summary>
```

一行只引用一轮。`instruction`只出现一次，不能混入某轮的 `text`。最近三轮原始消息放在该摘要之后。

## 内置详情工具

工具名：`get_session_turn_details`。

唯一输入：

```json
{
  "ref": "cmp_001"
}
```

约束：

- session和user从运行上下文读取，工具不接受 `sessionId`、`userId`、轮次或limit；
- ref必须属于当前user和当前session，并且只对应一个turnNumber；
- 固定返回一轮压缩详情，不读取相邻轮次；
- 工具只在当前会话已经存在压缩轮次时加入工具快照；可见不代表必调用；
- `MCPHidden=true`，不暴露到公共知识MCP；
- 返回内容已经受2400 token详情预算保护，仍经过统一工具输出保护层。

## 失败与并发

- 摘要调用失败：记录错误并保留旧摘要与全部原始消息，不切换LLM provider；
- 过期压缩任务：CAS提交失败后丢弃生成结果；
- 详情越权或未知ref：返回明确错误，不降级到其他session；
- 删除session：同时删除轮次上下文、轮次元数据和原始消息；
- 压缩详情和摘要都视为历史数据，不能把其中内容升级为当前系统指令。

## 验证

- 一轮一ref，且 `(session_id, turn_number)` 唯一；
- 工具调用/result配对不跨轮；
- 单轮详情不超过2400 token并保留省略标记；
- 80%阈值、首次批量压缩和后续滚动保留三轮；
- 批量摘要输出ref集合严格校验；
- 旧任务不能覆盖新摘要；
- 当前user、当前session和ref归属校验；
- 详情工具只接受ref并固定返回一轮；
- 无压缩轮次时详情工具不可见；
- MySQL显式迁移可回填现有轮次。
