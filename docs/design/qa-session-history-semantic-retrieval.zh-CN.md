# QA 会话历史语义召回与有界上下文设计

## 状态与范围

本文记录 QA 长会话上下文管理的现行实现。系统按轮归档旧历史，保留最近原文，并在每次请求中动态召回与当前问题相关的归档摘要。

本文不替代工作区知识检索、跨会话长期记忆、运行内工具输出压缩，也不改变 `qa_messages` 和 `qa_turns` 作为原始会话事实源的职责。

关联基线设计：[QA 会话高低水位压缩与 JSON 归档](./qa-session-turn-compaction.zh-CN.md)。

## 解决的问题

旧实现把全部滚动摘要注入每次模型请求，提示词大小随已压缩轮次线性增长：

```text
prompt_summary_tokens = O(compacted_turn_count)
```

现行设计把归档规模和单次请求规模分开：

```text
archived_history_size = O(total_turns)
active_prompt_history = O(configured_token_budget)
```

系统不维护额外的会话状态 JSON。当前目标、约束、决定、实体和待办只有在与当前问题相关时，才会通过逐轮摘要召回；最近对话则直接来自未压缩原文。这样避免第二次 state LLM 调用、状态分类歧义、重复信息和无条件提示词占用。

## 设计原则

1. MySQL 是历史事实源，向量命中必须回到 MySQL 按用户、会话和引用复核。
2. 所有已压缩轮次均可归档，但单次请求只注入固定 token 预算内的相关摘要。
3. 最近完整轮次优先保留，至少保留最近 3 轮原文，并以 `clamp(context_window × 12%, 8000, 16000)` 限制近期原文预算。
4. `session_history` 使用 dense+BM25 混合召回，并与 MySQL 精确词项召回并用，兼顾语义改写、常规关键词和错误码、类名、路径、ID 等精确标识符。
5. 配置过的语义后端失败必须可见，不能静默替换 provider。
6. 摘要和详情都是数据，不是可覆盖系统指令的指令来源。
7. 压缩失败必须向用户建议开启新窗口，不能只在服务端记录错误。

## 目标架构

```text
当前问题 + 最近对话
        │
        ▼
Session History Recall
 dense + BM25 + lexical fallback + recency
        │ refs
        ▼
MySQL authoritative revalidation
 user_id + session_id + refs
        │ bounded summaries
        ▼
Prompt Context Builder ◀── 最近未压缩原文
        │
        ▼
Agent / LLM request
        │ exact detail needed
        ▼
get_turn(ref) / find_turns
```

模型侧会话上下文只有两层：

| 层级 | 内容 | 注入方式 |
|---|---|---|
| 最近历史 | 尚未压缩的最近完整轮次 | 直接注入原文 |
| 归档历史 | 与当前问题相关的逐轮摘要 | 动态召回并按 token 预算注入 |

## 数据模型

### qa_sessions

会话行只记录归档计数和压缩边界：

```text
archived_summary_tokens BIGINT NOT NULL DEFAULT 0
compacted_through_turn  INT NOT NULL DEFAULT 0
```

`archived_summary_tokens` 是已归档 `summary_text` 的累计 token，仅用于观测和新会话建议，不直接计入每次提示词。

### qa_turn_contexts

每个已压缩轮次保存一条稳定引用：

```text
ref
session_id
user_id
run_id
turn_number
detail_json
summary_text
summary_tokens
source_tokens
retained_tokens
created_at
```

`summary_text` 用于 dense、BM25 sparse 和精确词项索引。`detail_json` 不进入向量索引，只有模型拿到当前会话内的 `ref` 后才能通过 `get_turn` 读取。

### qa_session_history_terms

词项表是语义后端不可用或异步索引尚未完成时的持久化召回通道。它保存每轮最多 32 个规范词项，优先保留错误码、trace ID、UUID、文件路径、类名、方法名、表名、API 路径和配置键。主键为 `(session_id, term, ref)`，查询同时限定 `user_id` 和 `session_id`。

### 语义索引与 outbox

语义索引使用独立的 `session_history` collection。point ID 从 `ref` 确定性生成，每个 point 同时写入 dense vector 和 BM25 sparse vector，payload 只保存过滤和定位所需元数据。BM25 使用独立的持久化词表 `.nasuta/session_history_bm25_vocab.json`；新增 token 坐标必须先持久化，再写入 point。MySQL 事务写入归档、词项和 `qa_session_history_index_outbox`；后台消费者异步写入向量后端。向量写入失败不会破坏 MySQL 事实源，词项召回仍可工作，失败原因和重试必须可观测。

## 归档流程

归档由两个入口触发：轮末的近期原文预算检查负责常态化控制上下文，轮首的 80% 高水位检查只负责保护即将执行的请求。

### 轮末日常归档

1. 成功回答并原子保存完整轮次后，重新读取 `uncompacted_tokens`。
2. 计算 `recent_token_budget = clamp(context_window × 12%, 8000, 16000)`。
3. 超出预算且存在可归档轮次时，从 `compacted_through_turn + 1` 开始选择连续完整轮次，至少保留最近 3 轮原文。
4. 按归档轮次的 `source_tokens` 累计回收量，直到剩余近期原文不高于预算，或到达 `latest_turn - 3`。
5. 如果最近 3 轮本身超过预算，保持完整轮次并记录明确原因，不按消息截断轮次。

### 轮首高水位保护

1. 使用当前问题、上一轮观测开销和输出预留计算 `projected_before`。
2. 低于窗口 80% 时不执行轮首归档；达到 80% 后，从最旧可归档轮次开始压缩到 60% 目标。
3. 高水位归档不能突破最近 3 个完整轮次。

两个入口随后共用同一条逐轮归档链路：

1. 为选中轮次构造有界 `detail_json` 和稳定 `cmp_...` 引用。
2. 每 4 轮组成一个摘要 LLM 请求；最多 3 个请求并发，更多批次按波次继续。
3. 校验每个请求返回的 item 集合必须与输入一一对应，并把每条摘要截断到 120 token。
4. 在一个事务中写入逐轮归档、词项、语义 outbox，累计 `archived_summary_tokens`，推进 `compacted_through_turn`。
5. 下一次请求只加载压缩边界之后的最近原文，并按当前问题动态召回归档摘要。

不存在 session state 生成、fallback、存储或提示词注入。摘要生成任一批失败时，归档事务不提交。轮首高水位归档失败时，调用方发送：

```text
session_restart_recommended(reason=compaction_failed)
```

然后终止当前请求并建议用户开启新对话。轮末日常归档失败时，本轮回答已经保存，不回滚回答；记录错误并由下一轮重新检查，高水位保护仍然有效。

## 动态召回

召回查询由当前问题和最近对话语境组成，不从额外状态字段扩展实体。候选来自：

- `session_history` dense+BM25 通道，由语义后端完成混合排序；
- MySQL 规范词项通道，用于异步索引间隙、后端故障和精确技术标识符；
- 时间衰减，用于同等相关度下优先较新的轮次。

融合候选后，系统批量回到 MySQL 复核所有权、会话、引用和压缩边界，再按最终 JSON 的实际 token 预算选择结果。不得逐条查询，也不得在内存加载全部历史后切片。

## 提示词格式

召回结果作为有边界的数据注入：

```xml
<retrieved_session_history format="json">
{"version":1,"mode":"hybrid","turns":[...]}
</retrieved_session_history>
```

提示词明确说明该数据可能不完整；当关键证据缺失时，模型可使用 `find_turns` 搜索当前会话归档，再用 `get_turn(ref)` 获取单轮详情。工具必须从运行上下文取得当前用户和会话，不能接受模型自行指定所有权范围。

## 投影与新会话建议

压缩前投影使用：

```text
projected_before = uncompacted_tokens
                 + incoming_tokens
                 + observed_overhead
                 + output_reserve
```

压缩后投影使用剩余未压缩原文，不把全部归档摘要计入活跃提示词：

```text
projected_after = remaining_uncompacted_tokens
                + incoming_tokens
                + observed_overhead
                + output_reserve
```

达到 95% 临界水位，或归档轮次数超过按摘要预算推导的阈值时，发送 `session_restart_recommended`。压缩自身失败统一使用 `compaction_failed`，不做静默降级。

## 失败与降级

| 失败点 | 行为 |
|---|---|
| 轮首摘要 LLM 失败、超时或 JSON 无效 | 不提交归档，发送 `compaction_failed` 新窗口建议 |
| 轮末日常归档失败 | 不提交归档，保留已保存回答和近期原文，记录错误并在下一轮重新检查 |
| MySQL 事务失败 | 整体回滚，发送 `compaction_failed` 新窗口建议 |
| 语义后端未配置 | 明确使用词项召回 |
| 已配置语义后端失败 | 记录失败并进入可观测词项模式，不替换 provider |
| outbox 写向量失败 | 保留 MySQL 归档并重试，词项召回继续可用 |
| 召回无结果 | 仅使用最近原文，不制造历史事实 |
| `get_turn` 越权或引用不存在 | 返回明确错误，不泄露其他会话数据 |

上表中的 `compaction_failed` 适用于轮首高水位保护。轮末日常归档失败只保留原文并记录可观测错误，不把已经完成的回答改为失败。

## 观测

压缩决策日志至少包含窗口、高低水位、未压缩 token、归档 token、可压缩轮次、投影值和决策。压缩结果日志包含轮次范围、回收 token、剩余 token、摘要耗时、事务耗时和新会话建议。日志不再包含 `session_state_tokens`、`state_mode` 或 `state_ms`。

## 迁移

新建 `qa_sessions` 不再创建 state 字段。历史迁移删除旧滚动 `summary`，创建 `archived_summary_tokens`、逐轮摘要 token、词项表和 outbox。

已有部署中的旧 `session_state` 和 `session_state_tokens` 列不在应用启动时自动删除，避免在线 DDL 锁风险；运行时代码停止读写这些列。需要回收列时，应在维护窗口通过显式离线迁移完成。

## 验证

- 摘要请求每批不超过 4 轮，长范围调用次数为 `ceil(turns / 4)`。
- 同时在途摘要请求不超过 3 个。
- 任一摘要批缺项、重复项、越界 item 或空文本均阻止事务提交。
- 压缩提交后边界单调推进，陈旧生成结果不能覆盖新边界。
- 请求提示词只包含近期预算内的最近原文和预算内召回摘要，不含 session state。
- 低于 80% 但近期原文超过 `recent_token_budget` 时，轮末仍归档最旧完整轮次。
- 轮末归档始终保留最近 3 个完整轮次，已归档摘要与近期原文不重叠。
- 轮首高水位归档失败时 SSE 必须先出现 `session_restart_recommended` 且 reason 为 `compaction_failed`；轮末失败不得改写已完成回答。
- 所有历史读取同时限定当前 `user_id` 和 `session_id`。

## 决策摘要

1. 删除 session state 机制及其 LLM 调用、fallback、存储读写、召回扩展和提示词注入。
2. 使用“最近原文 + 动态召回的逐轮摘要”作为唯一会话上下文模型。
3. 逐轮摘要每 4 轮一批，最多 3 批并发，超出部分按波次处理。
4. MySQL 保持事实源，dense、词项和时间信号只负责候选召回。
5. 压缩失败不静默继续，必须建议用户开启新窗口。
