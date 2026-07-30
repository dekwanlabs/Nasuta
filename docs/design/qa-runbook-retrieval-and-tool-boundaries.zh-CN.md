# QA 知识文档检索与工具类型边界设计

## 1. 状态与结论

- 状态：设计完成，待实施
- 所属模块：Nasuta QA、知识文档检索、Agent 工具执行
- 适用入口：`/api/qa/ask`、MCP `search_runbooks`、内部 `knowledge.Reader`
- 关联设计：`qa-tool-selection-and-multiturn-evidence.zh-CN.md`、`qa-context-pollution-control.zh-CN.md`

本文解决以下同一类问题：

1. 预检索已命中 `flow-system-overview`，Agent 却把 runbook ID 传给 `search_code`。
2. Agent 把 runbook ID 作为服务名传给 `check_docs`。
3. Agent 把 runbook ID 作为代码符号传给 `get_symbol`，进而命中 Android/iOS 中无关的 `SYSTEM` 枚举。
4. 系统总览只注入了一个片段，完整网关清单没有进入最终上下文。
5. 菜谱域的“三个入口网关”被错误提升为平台整体架构结论，而平台实际有七个网关。

核心决定如下：

1. 不新增 `get_knowledge_doc`，复用并增强现有 `search_runbooks`。
2. `search_runbooks` 增加可选 `doc_id`，支持在已知文档内检索多个相关分块。
3. 返回结构始终按文档分组，每个 `chunkText` 只对应一个 `chunkIndex`。
4. 预检索引用保留 `runbook`、`service`、`symbol` 类型；每个工具在自身定义中声明目标参数可接受的
   引用类型，执行器从工具定义读取约束，不维护中央允许关系表。
5. 类型错误属于工具调用失败，不能作为证据，也不能记录为成功调用。
6. 不新增 `applies_to` 类型。最终回答不得扩大证据原文中的主语、范围和量词；已有 `service` 关联
   和 ontology 关系继续用于可确定的服务级约束。
7. 不新增数据库表；已有向量 payload 已包含文档内检索所需的 `doc_id` 和分块字段。

## 2. 根因

### 2.1 两类文档使用了不同索引

`search_code` 实际查询 `kind=code_chunk`。代码仓库中的 Markdown 会作为代码分块进入该索引，
因此工具描述中的“docs”容易被理解成全部知识文档。

平台知识库中的架构、流程、Schema、模块和运维文档使用 `kind=runbook`。它们由
`search_runbooks` 查询，与仓库 Markdown 不是同一个检索集合。

因此，用以下调用读取已知知识文档在语义上是错误的：

```text
search_code("flow-system-overview architecture gateway")
```

该调用只能从 `code_chunk` 中召回相似仓库文件，不能精确补全 `flow-system-overview`。

### 2.2 `search_runbooks` 只能发现文档，不能补全已知文档

当前全库搜索会把同一文档的多个向量命中按文档 ID 合并，只保留最佳分块。这适合发现相关
文档，但不适合回答“这篇文档中的完整网关清单是什么”。

Agent 已经知道文档 ID，却没有参数把后续检索限定在该文档内，只能继续尝试其他工具。

### 2.3 工具执行只校验 JSON Schema

`check_docs(service="flow-system-overview")` 和
`get_symbol(query="flow-system-overview")` 都满足字符串参数的 JSON Schema，因此工具层会执行。
调用虽然没有产生有效证据，却仍被记录为工具成功。

### 2.4 回答扩大了证据原文的陈述边界

现有 `scope=flow` 表示文档种类。菜谱域文档原文中的主语是菜谱域，但最终综合忽略了该主语，把
“菜谱域涉及三个入口网关”改写成“整个平台只有三个网关”。问题是证据使用时扩大了原文的主语和
量词，不是缺少一个预先枚举的平台/领域类型。

为当前案例新增 `applies_to=system|domain/*|service/*` 会形成第二套业务分类：每出现一个新领域或
新的组织层级就要扩展约定，还会与已有 `service`、tags 和 ontology 关系产生重复或冲突。因此本文
不引入该字段。

## 3. 目标与非目标

### 3.1 目标

1. 已知文档 ID 时，只在该文档内补充证据。
2. 同一文档可返回多个独立、可追溯的分块。
3. 已知 runbook ID 不得进入服务或代码符号工具。
4. 工具边界错误在 Run、证据清单和前端中均可见。
5. 最终结论不超出证据原文明确表达的主语、范围和量词。
6. 所有读取有硬上限，检索、去重和组装采用最低实际时间复杂度。
7. 现有 QA、MCP 和 Feature Delivery 使用同一个 `search_runbooks` 合同。

### 3.2 非目标

- 不增加第二个功能重叠的知识文档读取工具。
- 不把所有普通搜索词预先分类成 runbook、service 或 symbol。
- 不维护“引用类型 → 工具 ID 列表”的中央静态映射。
- 不靠一个特殊网关关键词修复单个问题。
- 不新增 `applies_to` 字段、封闭枚举或另一套领域分类体系。
- 不返回或注入无界的完整 Markdown 正文。
- 不通过永久兼容分支同时维护两套返回结构。

## 4. `search_runbooks` 合同

### 4.1 输入

```json
{
  "query": "平台网关清单和接入层架构",
  "doc_id": "flow-system-overview",
  "limit": 3
}
```

| 字段 | 必填 | 约束 | 含义 |
| --- | --- | --- | --- |
| `query` | 是 | 规范化后非空 | 当前需要查证的事实，不是文档标题的重复描述 |
| `doc_id` | 否 | 规范化文档 ID | 不为空时只检索该文档 |
| `limit` | 否 | 默认 3，范围 1 至 10 | 全库检索时限制文档数，文档内检索时限制分块数 |

输入规范化只在工具入口执行一次。下游检索、分组和响应组装信任已建立的规范值，不重复执行
`TrimSpace`、大小写兼容或空值回退。

### 4.2 统一输出

```json
{
  "matches": [
    {
      "docId": "flow-system-overview",
      "title": "System Overview",
      "path": "docs/knowledge-base/flows/flow-system-overview.md",
      "docKind": "flow",
      "evidenceClass": "curated_flow",
      "trustTier": 85,
      "chunks": [
        {
          "chunkIndex": 2,
          "sectionHeader": "Gateway Layer",
          "chunkText": "...七个网关...",
          "semanticScore": 0.81
        },
        {
          "chunkIndex": 3,
          "sectionHeader": "Service Layers",
          "chunkText": "...服务分层...",
          "semanticScore": 0.76
        }
      ]
    }
  ],
  "semantic": true,
  "docScoped": true,
  "truncated": false
}
```

输出不变量：

1. `matches` 始终按文档分组，不因是否传入 `doc_id` 改变结构。
2. 一个 `chunkText` 只对应一个 `chunkIndex`。
3. 不直接拼接不相邻分块，不伪造连续原文。
4. `chunks` 在选取完成后按 `chunkIndex` 升序排列。
5. `semanticScore` 属于分块，`evidenceClass` 和 `trustTier` 属于文档证据来源。
6. `truncated=true` 表示还有匹配分块未返回，不表示文档中的事实不存在。

现有内部 `knowledge.RunbookSearchResult`、Feature Delivery 消费者、Dashboard API 和 MCP 输出在
同一变更中切换到该结构，不保留旧平铺 `matches` 的运行时兼容分支。

## 5. 检索行为

### 5.1 全库检索

未传 `doc_id` 时，目标是发现最相关的不同文档：

```text
embed(query)
  -> semantic search where kind=runbook
  -> 按 doc_id 保留最佳分块
  -> 最多 limit 篇文档
  -> 每篇文档通常包含一个 chunks 元素
```

去重使用 `map[string]int` 保存文档 ID 到结果位置，单次扫描完成，时间复杂度为 O(n)。不在循环
中使用 `slices.Contains`。

语义命中的 payload 已包含文档 ID、标题、路径、分块和证据字段。语义检索成功时直接从有界
命中构建结果，不再先读取全部 runbook 元数据后做内存关联。

### 5.2 文档内检索

传入 `doc_id` 时，目标是补齐一篇已知文档中的相关证据：

```text
RunbookMetaByID(doc_id) 用窄字段查询精确确认文档存在
  -> embed(query)
  -> semantic search where kind=runbook AND doc_id=<doc_id>
  -> 按 chunk_index 去重
  -> 请求 limit+1 个分块并据此计算 truncated
  -> 选取最多 limit 个分块
  -> 按 chunk_index 排序后返回
```

分块去重使用 `map[int]struct{}`，选择阶段为 O(n)。最终排序仅作用于硬上限 `k <= 10` 的结果，
复杂度为 O(k log k)，用于恢复文档阅读顺序。

`RunbookMetaByID` 只读取 ID、标题、文件路径和文档种类，不读取 `content`。语义路径直接使用向量
payload 中的分块正文，不允许为了确认文档存在或做结果关联而加载整篇 Markdown。文档内语义查询的
点 ID 已按 `doc_id + chunk_index` 唯一，因此读取 `limit+1` 条即可准确判断是否还有结果，时间与空间
复杂度均为 O(limit)。

文档不存在时返回明确错误：

```json
{
  "code": "runbook_not_found",
  "docId": "flow-system-overview"
}
```

不得移除 `doc_id` 后静默退化为全库搜索。

### 5.3 后端失败

- 未配置语义检索时，使用已记录且有界的关键词检索能力。
- 已配置语义后端但调用失败时，返回可观察错误，不静默替换成另一 Provider。
- 关键词路径同样必须在存储边界设置 `LIMIT`，不能读取全部文档后在内存切片。
- 文档内关键词降级只读取目标文档；正文读取受文档上传大小上限约束，匹配采用单次扫描，输出仍受
  `limit` 和单分块字符预算约束。

## 6. 工具职责边界

工具描述修改为以下语义。

### `search_runbooks`

搜索知识库中的系统架构、业务流程、模块、Schema、业务说明和运维文档。已知文档 ID 时通过
`doc_id` 限定范围，并在该文档中检索相关分块。知识文档描述设计或预期行为，不自动证明当前
运行时状态。

### `search_code`

搜索 `code_chunk` 类型的源码、配置、SQL 和代码仓库 Markdown。它不搜索知识库 runbook，
也不能用于读取预检索已知的知识文档。

### `check_docs`

检查一个规范服务名的文档、入口点、API、下游依赖和 source-of-truth 覆盖情况。它不读取、
校验或补全某篇知识文档。

`check_docs` 不再模糊接受任意关键词。调用方先通过 `get_service` 获得规范服务名，再执行覆盖
检查。无法精确解析服务时返回 `service_not_found`，不能返回容易误解的 `missing: service-card`。

### `get_symbol`

查询函数、方法、类或接口的代码图定义。参数描述中删除 `service keyword`，明确禁止传入服务
名、文档标题或 runbook ID。

## 7. 工具自描述的引用约束

### 7.1 引用类型

预检索引用使用受控常量：

```go
type ReferenceType string

const (
    ReferenceRunbook ReferenceType = "runbook"
    ReferenceService ReferenceType = "service"
    ReferenceSymbol  ReferenceType = "symbol"
)
```

引用继续携带规范目标：

```json
{
  "type": "runbook",
  "label": "flow-system-overview",
  "target": "flow-system-overview"
}
```

每次 Agent Run 在开始时对有界引用单次扫描，建立：

```go
map[string]ReferenceType{
    "flow-system-overview": ReferenceRunbook,
    "hsmf-mobile-gateway":  ReferenceService,
}
```

后续成员判断为 O(1)。该索引只属于当前 Run，不持久化为新的会话状态。

### 7.2 约束归工具定义所有

不在校验器中维护以下形式的中央映射：

```go
map[ReferenceType][]tool.ToolID
```

工具在注册时与描述、输入 Schema 一起声明哪个参数承载目标引用，以及该参数接受哪些已有引用
类型。概念合同如下：

```go
type ReferenceInput struct {
    Argument string
    Accepts  []string
}

type Tool struct {
    // 现有字段省略。
    ReferenceInputs []ReferenceInput
}
```

例如，`check_docs` 自己声明 `service` 参数接受 `service` 引用；`get_symbol` 自己声明 `query` 参数接受
`symbol` 引用；`search_runbooks` 的 `doc_id` 参数接受 `runbook` 引用。`search_code.query` 是自由检索
入口，但若其中出现当前 Run 已知的规范引用，其合同可以接受 `service`、`symbol`，而不接受
`runbook`。这些知识与工具定义同处一处，不散落到执行器或提示词中的工具 ID switch。

带实体目标参数的扩展工具通过 `ReadTool` 注册时同时声明自己的 `ReferenceInputs`；没有实体目标的
工具保持为空。注册表快照基于这些声明派生反向索引，供错误提示查找候选工具：

```go
map[string][]tool.ToolID // reference type -> tools derived from the snapshot
```

该索引是工具目录的派生数据，不是另一份人工配置。新工具只修改自己的定义，无需修改校验器。
构建复杂度为 O(T×A)，其中 T 是本次快照中的工具数，A 是每个工具的目标参数数；每个 Run 复用
同一快照和索引。

### 7.3 校验边界

执行器只检查两类已经有可靠类型的信息：

1. 预检索产生并携带类型的规范引用；
2. 工具调用目标参数中以完整 token 边界出现的规范引用。

校验器单次扫描目标参数，并通过当前 Run 的引用索引做 O(1) 查找。它不分类所有自然语言词语，
不根据网关、菜谱等业务关键词猜类型，也不限制没有命中规范引用的普通自由检索。

`ReferenceInputs` 为空表示该工具没有可执行的实体类型约束，而不是接受或拒绝全部引用。工具输入
仍由 JSON Schema 完成结构校验，两者职责不重叠。

### 7.4 类型错误

以下调用在实际 Handler 执行前拒绝：

```text
search_code(query="flow-system-overview architecture gateway")
check_docs(service="flow-system-overview")
get_symbol(query="flow-system-overview")
```

统一错误：

```json
{
  "code": "entity_type_mismatch",
  "entity": "flow-system-overview",
  "actualType": "runbook",
  "tool": "get_symbol",
  "candidateTools": ["search_runbooks"]
}
```

`candidateTools` 从当前 Registry Snapshot 的反向索引实时派生，不在错误处理代码中写死
`search_runbooks`。没有候选工具时返回空数组，不进行跨 Provider 或跨工具的静默替换。

该结果必须满足：

- `ToolExecution.Failed=true`；
- `tool_failure_count` 增加；
- 不增加证据结果数；
- Evidence Manifest 记录失败工具和错误码；
- 最终证据状态至少为 `partial`，不存在其他证据时为 `unavailable`；
- `tool_result` 事件携带该次调用的 `failed=true`，前端在对应工具输出卡片原位显示红色“失败”并
  展示错误内容；不在最终回答下方增加“工具调用失败 N 次”的汇总标签。

## 8. Agent 策略

Agent 工具策略增加：

```text
预检索引用带实体类型时，选择自身 ReferenceInputs 合同接受该类型的工具。

已知 runbook ID 且现有片段不足时，调用：
search_runbooks(query=<当前缺失事实>, doc_id=<runbook ID>)。

文档内检索失败后，不得切换到自身合同不接受 runbook 引用的工具。
一次有针对性的文档内检索仍不足时，以 partial 状态回答并指出缺失证据。
```

现有“自由文本失败后切换精确工具”的规则增加前提：候选工具必须来自当前 Registry Snapshot 中
接受该引用类型的工具定义。提示词负责引导，执行校验负责兜底；两者都不维护工具 ID 白名单。

## 9. 证据陈述边界

### 9.1 不新增 `applies_to`

文档继续使用现有元数据：

```yaml
id: flow-cookbook-architecture
scope: event-driven
tags: [event, cookbook, architecture]
```

其中 DocStore 的 `kind` 是文档种类，`tags` 只参与召回，已有 `service` 字段在能够精确解析时继续
投影为 ontology 中的 `service -documented_by-> runbook`。这些字段都不被重新解释为一套封闭的
`system/domain/service` 适用范围枚举。

不增加 `applies_to` 的原因：

1. 平台、领域、子域、服务、租户和地区不是稳定的单层分类；封闭枚举会持续膨胀。
2. 一篇文档可以同时包含不同粒度的陈述，文档级标签无法准确限定每个段落。
3. 新字段会与原文主语、`service`、tags 和 ontology 关系形成多个 source of truth。
4. 当前错误可由“回答不得扩大原文陈述边界”普遍解决，无需为菜谱案例新增分类。

### 9.2 综合规则

每个返回 chunk 必须保留文档标题、章节标题和原始 chunk 文本。最终综合遵守以下通用规则：

1. 结论中的主语不得比证据原文的主语更宽。
2. “全部、仅、共有、唯一”等量词必须由同一主语下的明确原文支持，不能从局部清单推导。
3. 已有 `service -documented_by-> runbook` 关系可以证明文档与服务的关联，但不能把服务事实提升为
   平台事实。
4. 未关联服务的文档保持其原文陈述边界；“没有 service 关联”不等于“适用于全平台”。
5. 多个来源描述不同主语时分别陈述，不因某个来源细节更多就覆盖另一个来源。
6. 用户问题要求全局结论但证据只明确覆盖局部主语时，证据状态为 `partial`，并指出缺失的全局
   证据。

这套规则只比较问题、结论和证据文本中的陈述边界，不识别特定领域名，也不新增持久化状态。

因此正确结论应为：

```text
平台共有七个网关；其中菜谱域主要涉及 mobile、AI 和 backstage 三个入口网关。
```

## 10. 正确执行链

```text
用户：我们的架构是什么样的
  -> 预检索命中 runbook:flow-system-overview
  -> 当前片段没有完整网关清单
  -> search_runbooks(
       query="平台网关清单和接入层架构",
       doc_id="flow-system-overview",
       limit=3
     )
  -> 返回同一文档中多个独立 chunks
  -> 原文以整个平台为主语，明确列出七个网关
  -> 菜谱文档原文以菜谱域为主语，补充其三个入口
  -> 分别保留两个主语输出，不把局部清单改写成平台总量
```

不得再出现：

```text
search_code -> check_docs -> get_symbol -> query_relations
```

这种连续换工具但没有获得同类型新证据的路径。

## 11. 实施范围

### 阶段 A：文档内分块检索，P0

- `search_runbooks` 增加 `doc_id`；
- DocStore 增加不读取正文的 `RunbookMetaByID`；
- 语义查询增加 `doc_id` filter；
- 全库按文档去重，文档内按 `chunk_index` 去重；
- 文档内检索读取 `limit+1` 条并准确设置 `truncated`；
- 输出改成文档加 `chunks` 的统一结构；
- 同步升级 `knowledge.Reader`、Feature Delivery、Dashboard 和 MCP 消费者；
- 补充结果上限、截断和后端失败信息。

建议提交：

```text
feat(runbook): support document-scoped chunk search
```

### 阶段 B：工具类型边界，P0

- 引用类型改为受控常量；
- 为当前 Run 构建规范引用类型索引；
- 在 `Tool` 和 `ReadTool` 定义中增加 `ReferenceInputs`；
- Registry Snapshot 从工具声明派生引用类型到候选工具的反向索引；
- 工具执行前拒绝已知实体类型错配；
- 为所有带实体目标参数的内置工具补齐合同，并收紧相关描述和输入语义；
- 类型错配接入工具失败、Evidence Manifest 和前端状态；
- Agent 策略禁止跨实体类型盲目换工具。

建议提交：

```text
fix(agent): enforce evidence reference tool boundaries
```

### 阶段 C：证据陈述边界，P1

- 不修改文档 frontmatter，不增加 `applies_to`；
- 检索结果继续完整保留 title、section header 和 chunk text；
- 最终综合提示词增加“不得扩大原文主语、范围和量词”；
- Evidence Manifest 保留来源引用与原始摘录，不新增适用范围状态；
- 复用已有 `service -documented_by-> runbook` 关系处理可确定的服务关联；
- 增加跨主语、局部清单和全局量词的通用回归测试。

建议提交：

```text
fix(agent): preserve evidence statement boundaries
```

## 12. 测试

### 12.1 检索单元测试

1. 全库搜索每篇文档只保留一个最佳分块。
2. 指定 `doc_id` 后返回同一文档的多个不同 `chunk_index`。
3. 文档内搜索不会混入其他 `doc_id`。
4. 选取按语义相关度完成，输出按 `chunk_index` 排序。
5. 不存在的 `doc_id` 返回 `runbook_not_found`。
6. 结果超过 `limit` 时设置 `truncated=true`。
7. 语义命中路径不执行无界 `RunbookMetas` 读取。

### 12.2 工具边界测试

1. runbook 引用调用 `search_runbooks` 成功。
2. runbook 引用调用 `search_code`、`check_docs`、`get_symbol` 均被拒绝。
3. service 引用仍可调用 `check_docs`。
4. symbol 引用仍可调用 `get_symbol` 和 `trace_calls`。
5. 未识别的普通代码查询不被错误拒绝。
6. 类型错误增加 `tool_failure_count` 且不增加证据结果数。
7. 新注册工具只声明自身 `ReferenceInputs` 即可参与校验和候选提示，校验器无需新增工具 ID 分支。
8. Registry Snapshot 删除一个工具后，派生候选中不再出现该工具，不残留静态映射。
9. 扩展 `ReadTool` 与内置工具遵守相同合同，不按工具所有者写特殊分支。

### 12.3 陈述边界回归测试

测试数据包含两个不同粒度主语的文档片段，不使用网关、菜谱或具体服务名称写专用判断。

1. 原文明示全局主语和完整数量时，可以形成对应的全局结论。
2. 局部主语下的清单不能被改写成全局总量。
3. 只有局部证据时，全局问题保持 `partial`。
4. 不同主语的事实能够在同一回答中分别表达。
5. 未设置 `service` 的文档不会被默认视为全局文档。
6. 不依赖新增 frontmatter 字段或文档 ID 命名规则即可通过测试。

### 12.4 验证命令

```bash
go test ./internal/agent/ ./internal/retrieval/ ./internal/featuredelivery/
go build ./...
go vet ./...
```

涉及共享检索合同后，再执行：

```bash
go test ./...
```

## 13. 验收标准

1. 日志中不再出现将已知 runbook ID 传给 `check_docs` 或 `get_symbol` 的成功调用。
2. `search_runbooks(doc_id=...)` 能返回同一文档最多 10 个独立分块。
3. 多个分块不合并到同一个 `chunkText`。
4. 文档内检索不会召回其他文档。
5. 类型错配在 Run 详情、证据清单和前端均显示为工具失败。
6. 新增工具只修改自身定义即可参与引用校验；校验器中不存在按工具 ID 维护的允许关系表。
7. 全局问题不会仅凭局部主语的文档形成全局完整结论。
8. 文档模型、frontmatter 和向量 payload 均未增加 `applies_to`。
9. “平台七个网关”和“菜谱域三个入口网关”能够同时正确表达。
10. 在线检索没有 fetch-all 后切片，也没有循环内线性成员查询。
11. 未新增功能重叠的工具、数据库表、领域分类枚举或持久化状态机。

## 14. 数据与发布影响

- 阶段 A、B 不需要数据库迁移。
- 阶段 A 使用现有 `doc_id`、`chunk_index`、`section_header` payload。
- 统一响应结构会修改内部 Go 合同和 MCP 工具输出，所有仓库内消费者必须原子升级。
- 阶段 C 不修改文档数据结构或向量 payload，不需要重新嵌入知识文档。
- `ReferenceInputs` 是工具注册合同变更；内置工具与扩展 `ReadTool` 需要在同一版本完成声明。
- 已配置语义后端失败时保持可观察，不静默切换 Provider。
