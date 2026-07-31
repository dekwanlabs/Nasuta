# LLM Provider 与可靠性

[English](06-llm-providers-and-reliability.md) | [中文](06-llm-providers-and-reliability.zh-CN.md)

> 状态：OpenAI/Anthropic 分发已建立；错误分类和协议修复持续完善
> 来源：LLM Provider Design、LLM 重试/JSON 修复/QA 闭环评估

## 1. Provider 分发

支持的 Provider 通过一个显式 dispatcher 创建：

```text
PlatformSettings.LLMProvider
  -> openai constructor
  -> anthropic constructor
  -> unsupported provider error
```

禁止：

- OpenAI 配置失败后自动使用 Anthropic；
- Anthropic endpoint 不可用时改用兼容网关；
- 凭证缺失时伪装成空回答；
- 在业务层按 Provider 类型散布分支。

Provider 差异封装在 `llm` 公共合同后面。

## 2. 共同合同

共同能力包括：

- 普通聊天和流式聊天；
- tool schema 和 tool call；
- `tool_call_id` 配对；
- max output tokens；
- usage 和 finish reason；
- cancellation/timeout；
- Provider 原始错误到稳定错误分类的映射。

Anthropic 连续 tool result 可能需要合并到同一 user turn；OpenAI 使用独立 tool role。适配器负责协议差异，Agent 只构造规范内部消息。

## 3. 错误分类

重试只适用于可证明的瞬时错误：

| 类型 | 示例 | 默认行为 |
|---|---|---|
| Transient | 429、部分 5xx、连接重置 | 指数退避 + 抖动，有界重试 |
| Timeout | 请求/读取超时 | 受剩余 Run 预算约束重试 |
| Invalid request | schema、参数、上下文超限 | 不盲重试，返回明确错误 |
| Authentication | 401/403 | 不重试，不替换 Provider |
| Content policy | provider block | 返回限制 |
| Protocol | 非法 tool call 或流事件 | 有界修复或明确失败 |

重试日志记录 Provider、attempt、等待时间、分类和最终结果，但不记录密钥或未脱敏 payload。

## 4. JSON 与 Tool-call 修复

修复顺序从最确定到最昂贵：

1. 严格 JSON 解析；
2. 仅修复已知传输噪声，例如代码围栏；
3. 使用 schema 定位缺失/类型错误；
4. 向同一 Provider reprompt，要求仅返回合法参数；
5. 达到上限后返回工具协议错误。

不得用激进正则把任意文本“猜成”JSON，也不得在修复时改变业务值。

Provider 消息发送前执行 call/result 配对检查：

- 删除或修复孤立结果；
- 中断调用生成明确的 interrupted result；
- 不重排用户消息；
- 不把旧 tool result 配到新的 call ID。

## 5. Streaming 可靠性

- 首 token、首 reasoning、首 content 和结束时间独立记录；
- 流在任何可见内容前失败时可以按错误分类重试；
- 已经向用户发送内容后不透明重放整段，避免重复；
- tool-call delta 出现后停止转发候选回答；
- `finish_reason=length` 进入 continuation；
- 多次 continuation 仍未完成时返回 `ErrReasoningTruncated` 或等价错误。

## 6. 空响应与最终收敛

空 `stop` 响应与 reasoning 被截断分开处理。Agent 可以进行有限 reprompt，但必须消耗同一 Run 预算。最终阶段不能因中间 Provider 空响应永久丢失已有工具证据。

## 7. Token 配置

模型窗口、单次回答上限和工具输出安全预算是三个不同配置：

- context window：输入总容量；
- answer max tokens：可见输出和 reasoning 预算；
- tool-result fuse：单条异常工具结果保护。

所有 Agent turn 使用足够的 answer max tokens，避免 reasoning 模型把配额全部消耗在不可见推理。输入预算检查在 Provider 调用前统一执行。

## 8. 验收标准

1. 每个 Provider 只有一个构造/分发路径；
2. 配置失败不会静默替换 Provider；
3. 只有瞬时错误自动重试；
4. JSON 修复不改变业务值；
5. tool call/result 在 Provider 发送前合法配对；
6. 已流式发送的内容不会因重试重复；
7. 空响应、长度截断和协议错误具有不同可观察错误。

## 详细归并材料

### LLM Provider 设计

> Migrated from CodeLoom `docs/design/agent/agent-llm-providers.zh-CN.md`; incorporated into this module on 2026-07-31.

状态：当前实现

CodeLoom 通过统一的 `LLMClient` 支持 OpenAI Compatible Chat Completions 和 Anthropic Messages。Agent 循环使用共同的 Message、Tool、Streaming、Reasoning、Finish Reason、Retry 和结构化 JSON 契约。

#### 分发

非流式文本、结构化 JSON 和流式工具调用分别进入一个显式 provider Switch：

```text
provider=openai    -> OpenAI Compatible 实现
provider=anthropic -> Anthropic 实现
其他值              -> 分发阶段返回 unsupported-provider 错误
```

Provider 来自 MySQL 平台设置 `llm_provider`，通过 `NewLLMClientWithHTTPAndProvider` 传入。运行时 URL、Key、Model 和 Token 设置也来自平台设置。索引文档生成使用独立的 OpenAI 格式客户端，不支持 Anthropic。

#### 共同契约

Agent 只看到：

- 带 system/user/assistant/tool Role 的 `Message`；
- 参数使用 JSON Schema 的 `ToolDef`；
- 参数为 JSON 字符串的 `ToolCall`；
- 流式 Content、Reasoning、工具 Delta 和完成的工具调用；
- 规范化的 `stop`、`length`、`tool_calls` Finish Reason。

OpenAI Compatible provider 使用 Chat Completion Message 和 SSE Delta。Anthropic 适配将 System 内容移动到顶层 System 字段，将助手调用映射为 `tool_use`，将工具结果映射为用户 `tool_result` Block，并把 Anthropic 事件转换为共同 Stream Callback。

#### 可靠性

公共调用 Wrapper 分类网络、HTTP 状态、响应 Envelope、空输出、非法 JSON 和截断错误。Retry 和 JSON Repair 位于 provider 特定 HTTP 协议之上，因此两个 provider 遵循同一调用方契约。

Reasoning 模型可能耗尽 Token 预算而没有可见内容。该情况返回 `ErrReasoningTruncated`，不能视为空答案成功。Agent 可以在剩余循环预算内续写因长度截断的可见答案。

#### 配置缺口

当前构造器会把除 `anthropic` 外的所有 provider 值规范成 `openai`，导致非法配置可能在显式分发器看到之前被隐藏。目标不变量是在入口严格校验 `openai|anthropic`，下游不再提供别名或默认值。代码修复前，平台配置必须限制该字段。

#### 不变量

1. Provider 特定线协议不能泄漏到 Agent 循环。
2. 配置为 Anthropic 时绝不能走 OpenAI 路径。
3. Provider 错误必须保留类别和原因。
4. 文档生成只支持 OpenAI 的限制必须保持明确。
5. 增加 Provider 时，每种操作都要有独立实现和显式分发 Case。
