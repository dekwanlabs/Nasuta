# QA LLM Token 使用量记录设计

## 目标

Nasuta 必须同时回答两个不同问题：

1. 一次用户 QA Run 实际消耗了多少模型 token；
2. Run 内单次模型调用距离模型上下文窗口还有多少余量。

统计以 Provider 返回的 `usage` 为准，不再把字符串字节数、字符数或 SSE delta 数量称为 token。

## 口径

每次物理 LLM API 调用记录一条明细，包括路由、查询分析、Agent step、续写、强制结论、
长期记忆提取和会话摘要。重试是新的物理调用；Provider 没有返回 usage 时记录状态和零 usage，
不能用字符数伪造。

```text
call.total_tokens = usage.total_tokens
                  或 usage.input_tokens + usage.output_tokens

run.total_tokens = Σ call.total_tokens
run.peak_input_tokens = max(call.input_tokens)
run.peak_reserved_tokens = max(call.input_tokens + call.max_output_tokens)
                           仅统计 max_output_tokens 已知的调用
```

`max_output_tokens = 0` 表示 Provider 请求没有设置可确认的输出上限，此时不参与
`peak_reserved_tokens`；峰值为 0 表示预留量未知，而不是上下文占用为 0。

`reasoning_tokens` 是 `output_tokens` 的细分项，不再重复加到 `total_tokens`。
`cached_input_tokens` 是 `input_tokens` 的细分项，保留用于成本分析，但不从上下文占用中扣除。

## 调用阶段

`phase` 只标识费用来源，不表示生命周期状态：

| phase | 含义 |
|---|---|
| `route` | 证据路由、查询规范化和术语提取 |
| `agent_step` | Agent 工具决策或直接回答 |
| `continuation` | 长答案续写 |
| `forced_conclusion` | 工具步数耗尽后的最终结论 |
| `memory_extract` | 回答完成后的长期记忆提取 |
| `session_summary` | 保存对话后的滚动摘要 |

## Provider 映射

### OpenAI-compatible

非流式响应读取顶层 `usage`。流式请求发送 `stream_options.include_usage=true`，读取最终 SSE
chunk 的 `usage`。兼容网关若拒绝该字段必须返回可观察错误，不能伪造 usage 或切换 Provider。

映射：

```text
prompt_tokens                                  -> input_tokens
prompt_tokens_details.cached_tokens            -> cached_input_tokens
completion_tokens                              -> output_tokens
completion_tokens_details.reasoning_tokens      -> reasoning_tokens
total_tokens                                   -> total_tokens
```

### Anthropic

非流式响应读取顶层 `usage`。流式响应合并 `message_start.usage` 与 `message_delta.usage`。

```text
input_tokens + cache_creation_input_tokens
  + cache_read_input_tokens      -> input_tokens
cache_creation_input_tokens
  + cache_read_input_tokens      -> cached_input_tokens
output_tokens                    -> output_tokens
```

Anthropic 没有独立返回 thinking token 时，`reasoning_tokens` 保持 0；thinking 已包含在输出计费中。
Anthropic 的 `input_tokens` 不包含 cache creation/read，因此统一口径的输入总量必须把三项相加；
否则缓存命中时会低估上下文占用和总量。

## 数据模型

`agent_llm_calls` 保存物理调用明细：

```sql
CREATE TABLE agent_llm_calls (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id              VARCHAR(64) NOT NULL,
    call_seq            INT NOT NULL,
    phase               VARCHAR(32) NOT NULL,
    provider            VARCHAR(32) NOT NULL,
    model               VARCHAR(128) NOT NULL,
    input_tokens        INT NOT NULL DEFAULT 0,
    cached_input_tokens INT NOT NULL DEFAULT 0,
    output_tokens       INT NOT NULL DEFAULT 0,
    reasoning_tokens    INT NOT NULL DEFAULT 0,
    total_tokens        INT NOT NULL DEFAULT 0,
    max_output_tokens   INT NOT NULL DEFAULT 0,
    duration_ms         INT NOT NULL DEFAULT 0,
    status              VARCHAR(16) NOT NULL,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_run_call (run_id, call_seq),
    KEY idx_run (run_id)
);
```

`agent_runs` 增加聚合列：

```text
input_tokens
cached_input_tokens
output_tokens
reasoning_tokens
total_tokens
llm_call_count
peak_input_tokens
peak_reserved_tokens
```

每次写入调用明细时，在同一事务中增量更新 Run 聚合和峰值。这样失败、暂停、进程退出前已经
完成的调用仍然可查询。在线列表只读 `agent_runs`，Run 详情按 `run_id, call_seq` 有界读取明细。

已有库通过显式 SQL migration 增加表和字段；新建库同步更新 `dbschema`。旧的
`token_used`、`token_delta`、`reasoning_tokens` 字段保留兼容，但不再作为真实 token 指标，
也不把历史字符数转换成 token。其中步骤级 `token_delta` 继续记录内容字符数，仅用于旧页面兼容；
真实 token 只能读取 `agent_llm_calls` 和 `agent_runs` 的新增字段。

## 调用关联

QA 在生成 `run_id` 后，把 `run_id`、`phase` 和 usage recorder 放进当前调用上下文。
`internal/llm` 只认识自己的轻量 Recorder 接口，不依赖 Agent 或数据库；RunStore 实现该接口。
共享 LLMClient 不持有可变的当前 Run 状态，因此并发请求不会串账。

后台 `memory_extract` 和 `session_summary` 使用 `context.WithoutCancel` 保留 usage 元数据，
并继续关联触发它们的 `run_id`。

## API

Run 列表返回聚合字段。Run 详情额外返回按 `call_seq` 排序的 `llm_calls`，不得无界读取其他 Run。
前端可展示：调用次数、输入/输出/总 token、缓存输入、推理细分，以及峰值预留上下文。

模型上下文窗口大小不从 usage 推断；若要显示 `peak_reserved_tokens / context_window`，必须由明确的
模型能力配置提供，未知模型只显示峰值，不猜测窗口。

## 验证

- OpenAI 非流式和流式 usage 映射测试；
- Anthropic 非流式和流式 usage 合并测试；
- reasoning/cached token 不重复计入 total；
- RunStore 明细与聚合在同一事务内成功或回滚；
- 并发 Run 不串账，失败调用不伪造 token；
- Run 列表使用聚合列，Run 详情只读取指定 Run 的调用明细；
- Nasuta 与 CodeLoom 构建、测试和 `go vet` 通过。
