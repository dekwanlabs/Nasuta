# Nasuta 工具结果结构化压缩方案

> 状态：P1 `structured-extractive-v1` 已实施，P2 `structured-projection-v2` 待实施  
> 日期：2026-07-21  
> 范围：Agent 工具调用返回给 LLM 的 `role=tool` 消息  
> 不影响：MCP 原始工具响应、工具执行结果持久化、上层工具注册契约

## 1. 结论

Nasuta 应在 Agent Runtime 的工具结果边界统一执行内容压缩：

```text
工具原始结果
  -> 保存完整结果
  -> 估算模型输入 token
  -> 未超预算：原样发送
  -> 超出预算：结构化分块
  -> 根据当前问题和工具参数选择内容
  -> 在统一预算内打包完整分块
  -> 生成一条合法的 role=tool 消息
```

核心约束：

1. 预算由 Nasuta Runtime 统一拥有，默认每次工具结果最多约 `10_000` token。
2. 不按工具 ID 分配预算，不枚举或识别上层工具。
3. 上层工具不能声明、覆盖或探测上下文预算。
4. 未超预算的模型侧工具内容保持原样，不增加 envelope。
5. 超预算时优先保留完整 JSON 元素、完整日志行组或完整文本段落。
6. 压缩结果用 envelope 描述来源、覆盖范围和遗漏情况。
7. 完整原始结果仍用于运行记录和审计，压缩结果只进入 LLM 上下文。
8. 第一阶段不调用额外 LLM，不做生成式摘要，只做确定性的抽取式压缩。

## 2. 背景

当前 `internal/agent/loop.go` 已经在 `toolMessage` 处为所有工具统一设置
`10_000` estimated token 上限。超过预算后采用 UTF-8 安全的头尾保留和中间截断：

```text
HEAD
... [tool output truncated] ...
TAIL
```

它解决了以下问题：

- 上层工具不能无限挤占模型上下文；
- 不再针对 `observe_logs`、`search_runbooks` 等工具写预算分支；
- 成功、错误和运行时附加说明都会经过同一个最终边界；
- 任意上层工具都受到相同规则约束。

但字符串截断仍有明显缺陷：

- JSON 会变成非法 JSON；
- 数组元素或对象可能从中间断开；
- 日志事件、堆栈和代码段可能失去上下文；
- 头尾不一定是与当前问题最相关的内容；
- LLM 无法区分“没有数据”和“数据被省略”；
- 大量重复字段、URL、公共属性会浪费预算。

因此需要把“按字符截断”升级为“按结构选择内容”，同时保留当前统一预算边界。

## 3. 目标与非目标

### 3.1 目标

- 任意工具输出都使用同一套压缩机制。
- JSON、JSON Lines 和普通文本至少有稳定的分块策略。
- 压缩后的 envelope 始终是合法 JSON。
- JSON 数字、长 ID、Unicode 和原始字符串不能因解析而失真。
- 选择依据来自当前问题、工具参数和内容本身，不来自工具名称。
- 最终消息连同 envelope 元数据严格落在统一预算内。
- 被保留内容可通过 JSONPath 或行号追溯到原始结果。
- 被省略内容明确标记为未知，不能让模型误判为不存在。
- 压缩失败时有确定性降级，不能导致工具调用整体失败。

### 3.2 非目标

- 不改变 `tool.Result` 和上层 `ReadTool` 的公共注册契约。
- 不要求工具作者提供 chunk schema、预算或压缩回调。
- 不替代工具自身的存储分页、查询 `LIMIT` 和领域 Top N。
- 不在第一阶段调用快速模型做摘要。
- 不承诺在固定预算内保留任意超大结果的全部细节。
- 不把压缩后的 envelope 返回给 MCP 客户端。

## 4. 职责边界

### 4.1 上层工具

上层工具仍只负责：

- 校验业务参数；
- 在存储或远端查询边界限制读取范围；
- 返回正确、完整且可观察的 `tool.Result`；
- 在结果中表达领域边界，例如 `total`、`truncated`、`next_cursor`。

上层工具不负责：

- 判断 LLM 上下文还剩多少；
- 为自己申请更高预算；
- 根据 Agent 模型类型调整输出；
- 构造 Nasuta 压缩 envelope。

### 4.2 Nasuta Runtime

Nasuta Runtime 负责：

- 保存本次调用的完整 `Result.Content`；
- 统一估算模型输入 token；
- 选择 JSON 或文本压缩器；
- 使用当前问题和工具参数计算相关性；
- 在统一预算内选择、排序和序列化分块；
- 生成最终 `role=tool` 消息；
- 记录压缩指标和降级原因。

### 4.3 MCP

MCP 调用继续返回工具的完整 `Result.Content`。MCP 客户端是否压缩、分页或裁剪，
由客户端自己的模型上下文策略决定。

## 5. 总体数据流

```text
tool.Handler.Execute
  |
  v
tool.Result.Content --------------------------+
  |                                           |
  |                                           +--> StepRecord.Content
  |                                                完整保存和审计
  v
现有模型侧格式化
  |
  v
ToolOutputCompressor.Compress
  |-- 预算内 -------------------------------> 原样内容
  |
  |-- 超预算
       |-- JSON 解析成功 --------------------> JSON 分块
       |-- JSONL 识别成功 -------------------> 按记录分块
       |-- 其他 -----------------------------> 文本分块
                                                |
                                                v
                                           相关性评分
                                                |
                                                v
                                           预算打包
                                                |
                                                v
                                           envelope JSON
                                                |
                                                v
                                           role=tool
```

压缩发生在完整结果保存之后、`llm.Message` 构造之前。这样运行记录和问题复盘不会丢失
原始证据，模型侧上下文也不会失控。

## 6. 统一输入与输出

建议在 `internal/agent/tooloutput` 下建立独立包，避免继续扩大 `loop.go`。

工具执行和模型消息构造应拆成两个内部步骤：

```go
type ToolExecution struct {
    FullContent  string
    ModelContent string
    Arguments    tool.Arguments
    Notices      []string
}
```

`ToolExecutor.Execute` 只返回 `ToolExecution`。Agent loop 先把 `FullContent` 写入
`StepRecord.Content`，再调用 compressor 处理 `ModelContent`：

```go
type Request struct {
    Question  string
    Arguments tool.Arguments
    Content   string
    Notices   []string
    MaxTokens int
}

type Result struct {
    Content         string
    Compressed      bool
    Strategy        string
    OriginalTokens  int
    RetainedTokens  int
    OriginalChunks  int
    RetainedChunks  int
    OmittedChunks   int
    FallbackReason  string
}

func Compress(Request) Result
```

设计要求：

- `Question` 是当前用户问题，不是完整历史。
- `Arguments` 是本次已经校验过的工具参数。
- `Notices` 是 Runtime 生成的收敛提示等附加信息，必须一起计入预算。
- `MaxTokens` 由 Runtime 传入固定值，上层工具无法设置。
- `Result.Content` 是唯一进入 `llm.Message.Content` 的内容。
- `FullContent` 不经过 compressor，继续用于完整运行记录。
- `ModelContent` 承接现有面向模型的必要格式化；领域格式化与预算策略是两个独立步骤。

调用顺序：

```go
execution := executor.Execute(...)
observer.OnStep(StepRecord{Content: execution.FullContent})
compressed := compressor.Compress(Request{
    Question:  question,
    Arguments: execution.Arguments,
    Content:   execution.ModelContent,
    Notices:   execution.Notices,
    MaxTokens: defaultToolOutputTokenLimit,
})
messages = append(messages, toolMessage(call.ID, call.Function.Name, compressed.Content))
```

这样可以从结构上保证完整结果先进入运行记录，压缩失败也不会影响原始证据留存。

## 7. 是否压缩

压缩器先对最终待发送内容估算 token：

```text
estimated_tokens <= max_tokens
    -> 原样返回

estimated_tokens > max_tokens
    -> 进入结构化压缩
```

必须先完成模型侧必要格式化和 Runtime notice 组装，再判断预算，避免压缩后追加文本导致
最终消息再次超限。

错误结果、去重提示等短内容通常会直接原样返回，但仍经过同一个预算检查。

## 8. JSON 分块

### 8.1 解析要求

JSON 解析必须满足：

- 使用 `json.Decoder.UseNumber()` 或 `json.RawMessage` 保留数字精度；
- 不能把 19 位 ID 读成 `float64` 后再序列化；
- 使用原始字节片段保存 chunk 内容；
- 无效 JSON 自动转入文本分块，不向调用方返回压缩错误；
- 整体处理不得出现嵌套遍历导致的 O(n²) 复制。

例如：

```json
{"deviceId":"2039236571084886018"}
```

`deviceId` 必须保持原字符串。即使输入是无引号 JSON number，也不能经过 `float64`
产生精度变化。

### 8.2 通用分块规则

分块只根据 JSON 结构和大小决定，不根据字段名或工具名决定：

1. 整个节点低于目标 chunk 大小时，保留为一个完整 chunk。
2. 大数组按元素拆分，每个元素保持完整。
3. 大对象把标量和小字段提取为 ancestor context，再递归处理大型数组或对象字段。
4. 数组元素自身仍过大时，继续递归拆分它的子结构。
5. 单个字符串仍超过可用预算时，才允许对该字符串做头尾截断。
6. `null`、布尔值、数字和短字符串不单独拆成失去字段名的 chunk。

建议首版目标 chunk 大小约为 `400` 到 `800` estimated token。它只是内部打包粒度，
不是工具预算，也不暴露给上层。

### 8.3 路径

每个 chunk 使用稳定的 JSONPath 风格引用：

```text
$[0]
$[0].roomDeviceItems[0]
$[0].roomDeviceItems[0].devices[6]
```

路径只描述原始位置，不把数组内容复制进 envelope 元数据。

### 8.4 Ancestor context

大型父对象中的标量和小字段只保存一次，子 chunk 通过 `context_refs` 引用：

```json
{
  "contexts": [
    {
      "ref": "$[0]",
      "fields": {
        "familyId": "2007830887593005058",
        "familyName": "brooke.wang的家"
      }
    },
    {
      "ref": "$[0].roomDeviceItems[0]",
      "fields": {
        "id": "2007830887605587970",
        "roomName": "默认房间"
      }
    }
  ]
}
```

同一个 context 只序列化一次。只有至少一个已选择 chunk 引用它时，才把它放进最终
envelope。

## 9. 文本分块

JSON 解析失败后依次尝试 JSON Lines 和普通文本。

### 9.1 JSON Lines

- 每个合法 JSON 行是一个基本 chunk。
- 连续非法行按普通文本规则处理。
- 不因为个别坏行丢弃整份结果。
- `ref` 使用 `lines:start-end`。

### 9.2 普通文本

普通文本按以下边界分块：

1. 优先按空行形成段落。
2. 段落过大时按完整行继续拆分。
3. 缩进续行、堆栈行和紧邻的异常说明尽量与前一行放在同一 chunk。
4. 单行过大时才在行内做 UTF-8 安全的头尾截断。
5. 每个 chunk 保存原始起止行号。

示例：

```json
{
  "ref": "lines:920-955",
  "kind": "text",
  "content": "connection timed out..."
}
```

首版不做自然语言改写，所以被保留文本仍是原始工具结果中的精确内容。

## 10. 相关性评分

相关性只使用以下通用信号：

- 当前用户问题中的词和短语；
- 工具参数中的字符串、数字和布尔值；
- JSON 路径和字段名；
- chunk 原始内容；
- 原始位置和 chunk 大小。

禁止使用：

- 工具 ID 分支；
- CodeLoom 领域字段枚举；
- 针对某个问题关键词写死的特殊规则；
- 上层工具提供的私有评分器。

建议使用确定性的加权评分：

```text
score =
    exact_argument_match
  + exact_question_phrase_match
  + lexical_overlap
  + path_overlap
  + structural_anchor
  - size_penalty
```

实现注意：

- 对问题和参数只构建一次 term set；
- chunk 在生成时单次扫描并计算分数；
- 精确参数值匹配权重最高，例如 trace ID、device ID、服务名；
- 中文等无空格文本可增加字符 bigram/trigram 重叠；
- 排名选择后必须恢复原始 source order 再发送给模型；
- 不得按排序后的相关性顺序打乱日志时间线或数组原始顺序。

若问题和参数没有产生任何有效匹配，采用稳定降级顺序：

1. 保留必要 ancestor context；
2. 保留开头和结尾的少量 anchor chunk；
3. 用剩余预算均匀采样中间 chunk；
4. 明确标记覆盖不完整。

## 11. 两阶段压缩策略

### 11.1 第一阶段：`structured-extractive-v1`

第一阶段只选择完整 chunk：

- 不删除普通 JSON chunk 内的字段；
- 不总结或改写内容；
- 不合并不同位置的事实；
- 只允许对无法容纳的单个超长字符串做局部头尾截断；
- envelope 标记保留和省略的 chunk 数量。

它优先解决非法 JSON、事件断裂和不可追溯问题，实施风险最低。

### 11.2 第二阶段：`structured-projection-v2`

对于超大同构数组，单纯丢弃数组元素可能无法回答“列出全部对象”类问题。第二阶段增加
确定性的紧凑投影：

- 识别数组元素中重复出现的字段集合；
- 把所有元素都相同的字段提升为公共 context；
- 优先保留与问题、参数和路径相关的字段；
- 优先保留短、非空、区分度高的标量字段；
- 降低超长、高熵字符串的优先级，例如不相关的 URL、base64、blob；
- 对所有数组元素使用同一字段投影，避免每行语义不一致；
- envelope 明确列出 `retained_fields` 和 `omitted_fields`；
- 只有投影后能显著提高元素覆盖率时才启用。

该策略仍然只保留原始字段值，不调用 LLM 生成摘要。

压缩器可以同时计算两种候选计划：

```text
计划 A：保留少量完整元素
计划 B：保留更多元素的统一字段投影
```

若存在精确参数命中，优先计划 A；若没有明确单点目标且计划 B 能保留完整数组覆盖，则优先
计划 B。无法保证完整覆盖时必须标记 `item_coverage: "partial"`。

## 12. Envelope 协议

只有内容超预算并实际发生压缩时才生成 envelope。建议格式：

```json
{
  "_nasuta": {
    "version": 1,
    "compressed": true,
    "strategy": "structured-extractive-v1",
    "source_format": "json",
    "original_estimated_tokens": 48320,
    "retained_estimated_tokens": 9720,
    "original_chunks": 86,
    "retained_chunks": 12,
    "omitted_chunks": 74,
    "chunk_coverage": "partial",
    "item_coverage": "partial",
    "field_coverage": "full"
  },
  "contexts": [
    {
      "ref": "$[0]",
      "fields": {
        "familyId": "2007830887593005058",
        "familyName": "brooke.wang的家"
      }
    },
    {
      "ref": "$[0].roomDeviceItems[0]",
      "fields": {
        "id": "2007830887605587970",
        "roomName": "默认房间"
      }
    }
  ],
  "chunks": [
    {
      "ref": "$[0].roomDeviceItems[0].devices[6]",
      "ordinal": 6,
      "kind": "json",
      "context_refs": ["$[0]", "$[0].roomDeviceItems[0]"],
      "content": {
        "owner": true,
        "image": "https://resources.dreo-tech.com/app/preSigned202607/3b5e9adf3b43e4fdd81be86b00228ad37.png",
        "shared": false,
        "productId": "1929721128921149442",
        "name": "雾化风扇",
        "id": "2054913613618057217",
        "variants": [
          {
            "value": "b",
            "key": "color"
          }
        ],
        "deviceId": "2039236571084886018",
        "devicesn": "1929721128921149442-bf1a068730d1b322:001:0000000000b"
      }
    }
  ],
  "notices": [
    "Retained chunks are exact excerpts. Omitted chunks are unknown."
  ]
}
```

字段语义：

| 字段 | 含义 |
|---|---|
| `version` | envelope 协议版本 |
| `strategy` | 本次实际使用的压缩策略 |
| `source_format` | `json`、`jsonl` 或 `text` |
| `original_estimated_tokens` | 原始模型侧内容的估算 token |
| `retained_estimated_tokens` | 最终 envelope 的估算 token |
| `original_chunks` | 原始逻辑 chunk 数 |
| `retained_chunks` | 实际保留的 chunk 数 |
| `omitted_chunks` | 未进入模型上下文的 chunk 数 |
| `chunk_coverage` | 所有格式通用，`full` 或 `partial` |
| `item_coverage` | 数组元素覆盖，`full`、`partial` 或 `not_applicable` |
| `field_coverage` | JSON 字段覆盖，`full`、`partial` 或 `not_applicable` |
| `ref` | 原始 JSONPath 或文本行号 |
| `ordinal` | chunk 在原始结果中的稳定顺序 |
| `context_refs` | chunk 依赖的公共上下文 |
| `content_truncated` | 单个 chunk 内是否发生局部截断 |

Envelope 不包含完整原始内容、用户问题或工具参数，避免重复占用预算和扩大敏感数据暴露面。

## 13. 设备数组示例

用户给出的家庭设备 JSON 在当前规模下大概率不超过 `10_000` token，因此直接原样发送。

如果 `devices` 扩大到数百个并超过预算，第一阶段会形成：

```text
context $[0]
  familyId, familyName, userId, avatarType, avatarValue

context $[0].roomDeviceItems[0]
  room id, roomName, roomNameI18Key, type

chunk $[0].roomDeviceItems[0].devices[0]
chunk $[0].roomDeviceItems[0].devices[1]
...
```

如果问题是：

```text
雾化风扇的 deviceId 是什么？
```

包含“雾化风扇”的设备 chunk 和相关 ancestor context 获得最高分，其他设备可被省略。

如果问题是：

```text
这个家庭有哪些设备？
```

第一阶段只能在预算内保留尽可能多的完整设备对象，并声明部分覆盖。第二阶段可以选择统一
字段投影，例如每个设备只保留：

```json
{
  "name": "雾化风扇",
  "id": "2054913613618057217",
  "deviceId": "2039236571084886018",
  "productId": "1929721128921149442"
}
```

同时把重复的 `owner=true`、`shared=false` 提升到公共 context，把与当前问题无关的长图片
URL 放入 `omitted_fields`，从而尽量保留完整设备列表。

## 14. 预算打包

最终预算必须覆盖：

- `_nasuta` 元数据；
- `contexts`；
- `chunks`；
- Runtime notices；
- JSON 标点和转义开销。

不能先生成超大 envelope，再对序列化字符串做中间截断，否则仍会产生非法 JSON。

建议打包流程：

1. 生成最小 envelope 骨架并估算固定开销。
2. 选出必须保留的 context 和精确参数命中 chunk。
3. 按相关性从高到低尝试加入完整 chunk。
4. 每加入一个 chunk，同时计算新增 context 的成本。
5. 超出预算则跳过该 chunk，继续尝试更小的候选。
6. 选择结束后按 `ordinal` 恢复原始顺序。
7. 序列化完整 envelope 并再次估算。
8. 若仍超预算，从最低优先级 chunk 开始移除并重新序列化。
9. 若没有完整 chunk 能放入，对最高分单个 chunk 的字符串内容做局部头尾截断。

必须设置小于硬上限的内部目标，例如硬上限 `10_000`、打包目标 `9_700` estimated token，
为 JSON 转义、notice 和估算误差保留空间。

## 15. 降级与错误处理

压缩是模型侧优化，不能让一次已经成功的工具调用变成业务失败。

| 场景 | 处理 |
|---|---|
| JSON 解析失败 | 转普通文本分块 |
| JSONL 部分坏行 | 合法行按记录分块，坏行按文本分块 |
| 相关性无有效命中 | 头尾 anchor + 中间均匀采样 |
| 单个 chunk 超预算 | 只截断该 chunk 内的超长字符串 |
| Envelope 序列化失败 | 回退到当前 UTF-8 安全头尾截断 |
| 最终复核仍超预算 | 逐个移除低优先级 chunk；最后回退字符串截断 |
所有降级都应写入日志和 trace，但不能把内部错误堆栈发送给模型。

## 16. Prompt 契约

Agent system prompt 增加一条通用规则：

```text
Compressed tool results contain exact excerpts from the original result.
Use chunk refs as evidence locations. Omitted chunks are unknown; do not
infer their content or treat retained chunks as complete coverage.
```

如果 envelope 的 `item_coverage` 或 `field_coverage` 是 `partial`，模型不得使用“全部”、
“只有”或“没有其他结果”等完整性结论，除非另有完整证据。

## 17. 可观察性

每次发生压缩时记录：

```text
tool_name
source_format
strategy
original_estimated_tokens
retained_estimated_tokens
original_chunks
retained_chunks
omitted_chunks
chunk_coverage
item_coverage
field_coverage
compression_duration_ms
fallback_reason
```

`tool_name` 只用于日志归因和统计，不能参与压缩策略选择。

建议增加 trace 节点：

```text
tool_output_compression
```

日志只记录指标和短摘要，不重复打印完整敏感工具结果。

## 18. 性能约束

- 原始结果已经由工具返回为字符串，压缩器不再额外复制完整内容。
- JSON 解析、chunk 生成和打分应为单次扫描。
- term membership 使用 `map[string]struct{}`。
- context 去重使用路径 map。
- 不做 chunk 两两相似度比较。
- chunk 很多时使用有界候选堆，避免无条件保留全部排序副本。
- 最终 source-order 恢复只对已选择 chunk 排序。
- 整体目标为 O(n) 扫描和 O(k log k) 候选选择，不允许 O(n²) 字符串拼接。
- 使用 `strings.Builder`、`json.Encoder` 或预分配 buffer 组装输出。

其中 `n` 是原始内容长度，`k` 是逻辑 chunk 数量。

## 19. 安全与正确性

- 压缩不能扩大原始结果的权限范围。
- 不把用户问题、工具参数或隐藏 prompt 写入 envelope。
- 不执行工具结果中包含的指令，只把它视为数据。
- JSON 字符串必须正确转义，不能手工拼接 envelope。
- 原始 ID、时间戳、金额和计数不得经过浮点转换。
- 被省略内容必须表达为 unknown，不能表达为 empty。
- Runtime 保存的完整结果和发送给模型的压缩结果应能通过同一 tool call ID 关联。

## 20. 实施拆分

### P0：保持现有统一预算

- 保留当前 `10_000` estimated token 的 Runtime 边界。
- 保留当前头尾截断作为最终 fallback。
- 不恢复任何按工具名分配预算的逻辑。

### P1：抽取式结构化压缩

建议新增：

```text
internal/agent/tooloutput/
  compressor.go
  envelope.go
  json_chunker.go
  text_chunker.go
  rank.go
  token.go
```

实施内容：

1. 把 token estimator 和最终 fallback 移入 `tooloutput`。
2. 把工具执行结果和模型消息构造拆开，先保存 `FullContent`。
3. 接入当前用户问题和已解析工具参数。
4. 实现 JSON、JSONL 和文本分块。
5. 实现确定性相关性评分和 source-order 恢复。
6. 实现合法 envelope 的预算内打包。
7. 把 Runtime notices 纳入同一次预算计算。
8. 添加 system prompt 的压缩结果解释规则。

### P2：同构数组投影

1. 检测数组公共字段和稳定字段集合。
2. 对所有元素应用同一投影。
3. 提升重复常量字段到 context。
4. 比较“完整元素”和“全量投影”两个候选计划。
5. 增加 item/field coverage 测试。

### P3：可选快速模型蒸馏

只有确定性方案无法满足质量要求时再评估：

- 对超大 chunk 分批调用低成本模型摘要；
- 摘要与原文 excerpt 分开标记；
- 摘要失败、超时或未配置时回退到确定性方案；
- 不允许静默切换 LLM provider；
- 需要单独的成本、延迟和提示注入评估。

P3 不属于首版实现范围。

## 21. 测试矩阵

### 21.1 通用边界

- 任意未知工具名使用同一预算和同一策略。
- 上层工具无法设置预算。
- 未超预算内容逐字节保持不变。
- 成功结果、错误结果和 Runtime notice 都不超过预算。
- UTF-8 中文、emoji 和组合字符不会损坏。
- `MaxTokens <= 0` 有确定性结果。

### 21.2 JSON

- 顶层数组按完整元素分块。
- 嵌套数组生成正确 JSONPath。
- ancestor context 只保留一次。
- 19 位整数和数字形式 ID 不丢精度。
- 字符串中的引号、反斜线和换行正确转义。
- 压缩后 envelope 可被 `json.Unmarshal`。
- 最终序列化结果不超过预算。
- 选中 chunk 恢复原始顺序。
- 单个超大字符串只在字段内部截断。

### 21.3 文本

- 普通段落不从中间断开。
- 日志 chunk 保留准确行号。
- 堆栈续行尽量与异常头部同组。
- 单个超长行 UTF-8 安全截断。
- 无关键词命中时稳定保留头尾和均匀样本。

### 21.4 相关性

- 问题中的精确 ID 命中对应 chunk。
- 工具参数中的 trace ID 命中对应 chunk。
- 评分不依赖工具名。
- 相同输入每次产生相同输出。
- 任意上层工具名与内置工具名得到一致行为。

### 21.5 覆盖语义

- 丢弃任意数组元素时 `item_coverage=partial`。
- 删除任意字段时 `field_coverage=partial`。
- 文本结果使用 `item_coverage=not_applicable` 和 `field_coverage=not_applicable`。
- 原始模型侧内容未超预算时不应生成 envelope，而是原样返回。
- 投影模式下所有数组元素使用同一字段集合。

## 22. 验收标准

P1 完成需要满足：

1. 所有 Agent `role=tool` 消息不超过统一 estimated token 上限。
2. 未超预算结果与工具原始模型侧内容完全一致。
3. 超预算 JSON 返回合法 envelope，不再依赖对 envelope 字符串做中间截断。
4. 数组元素、日志行组和文本段落不会无标记地从中间断裂。
5. 每个保留 chunk 都有可追溯 `ref`。
6. 被省略的 chunk 明确标记，模型 prompt 禁止把部分覆盖当成完整覆盖。
7. 完整原始结果仍进入 `StepRecord.Content`。
8. MCP 返回行为不变。
9. 压缩策略与工具名无关。
10. 无法产出预算内合法 envelope 时回退到安全截断，不影响工具调用状态。

P2 完成还需要满足：

1. 类似家庭设备的大型同构数组可以自动提升重复公共字段。
2. 在预算允许时，紧凑投影可以保留全部数组元素。
3. 投影字段来自通用评分，不包含设备、日志或 CodeLoom 专用字段规则。
4. Envelope 能准确表达元素覆盖和字段覆盖。

## 23. 待确认项

实施前只需要确认两个 Runtime 内部参数：

1. P1 的目标 chunk 大小采用 `600` 还是 `800` estimated token。
2. `10_000` 硬上限下，内部打包目标采用 `9_700` 还是更保守的 `9_500`。

它们都是 Nasuta 内部实现参数，不进入上层工具注册协议，也不形成按工具差异化预算。
