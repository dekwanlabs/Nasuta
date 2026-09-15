# 记忆系统分类与提取设计

## 1. 背景

`internal/memory` 已实现一套完整的用户级长期记忆：提取（extract）→ 固化（consolidate）→ 召回（recall）→ 注入 QA 上下文，存储为 MySQL 真源 + Qdrant 向量 + BM25 稀疏。骨架是健壮的。

但当前**提取出来的记忆太乱、没有分类、内容含糊**。对线上 `qa_memories` 表的实际抽样暴露了五类问题：

1. **去重失败**：同一件事实被存成多条，且 `fact_key` 各不相同。例如"纯 BERT 槽位直执行路径已上线"这一件事，被存成 7 条不同的 active 记忆（`workspace:bert-slot-direct-execution:status`、`workspace:bert-slot-direct-path:user-claim`、`workspace:device-assistant:bert-slot-direct-path-status`、`workspace:pure-bert-slot-direct-execution-path:status` 等）。根因是 `workspace:<entity>:<attribute>` 的 `fact_key` 完全由 LLM 自由生成，没有归一化，唯一约束 `uniq_user_factkey_active` 因此失效。
2. **分类混乱**：`user:current-focus` 同一个 key 被分别标成 `work_context`、`profile`、`episode` 三种 kind；同为 role 的记忆，`user:role:value-consumer` 标 `profile`、`user:role:iot-risk-control` 标 `work_context`、`user:role:cookbook-system` 标 `assistant_inference`。
3. **内容含糊**：大量残句、中英文混杂碎片、甚至乱码（`"???? OauthService ? token ????"`、`"????????"`、`"???? R100 ????"`）。`user:response-language` 被存了 6 次以上，措辞各不相同（`chinese`/`Chinese`/`zh`/`Respond in Chinese`）。
4. **信噪比低**：`user:current-focus` 成了"什么都往里装"的垃圾桶，大量一次性查询意图（"在查某 API"、"在查某字段"）被当成长期记忆存下，随即被下一条 focus supersede，产生大量 churn，真正的偏好被淹没。
5. **系统事实混入**：`workspace:*` 命名空间诱导 LLM 把"系统/代码库的事实"（架构分层、技术栈、服务命名）当成用户记忆存下，违反了记忆系统"存用户偏好"的初衷（提取 prompt 本就写明 "Do not save current workspace/service/config claims as user facts"，但未被执行）。

**根因判断**：乱的不是 LLM 不够聪明，而是**约束太松**——`fact_key` 自由生成、kind 无判定边界、content 无模板、提取立场是"凑数"（prompt 第一句 "Consolidate at most 5 durable memories"）。因此本方案的主体是**收紧约束 + 反转立场 + 受控分类**，而非更换模型或重写存储。

## 2. 目标与非目标

### 2.1 目标

1. 提取的记忆**分类清晰**：kind、fact_key、content 都从受控集合/模板产出，LLM 只做"归类 + 填槽"，不做"发明"。
2. 提取的记忆**内容自包含**：每条都是脱离上下文也能理解的完整陈述句。
3. **去重可靠**：同一事实必然落到同一 `fact_key`，单 active 约束真正生效。
4. **默认不记、例外才记**：大多数 turn 产出空，只有明确命中判定标准才提取，提升信噪比。
5. 覆盖**身体/习俗**等敏感维度：类别存在但不凑数，用户明确说了才记，标记敏感、可查看可删。
6. 提取质量**可观测**：每次提取/固化/召回的决策可查询、可度量，能验证改进效果。

### 2.2 非目标

1. 不重写存储架构（MySQL + Qdrant + BM25 保留）。
2. 不建记忆知识图谱、不做实体关系图（关联最多到"演化链 + 主题标签"）。
3. 不引入 LLM 重排（召回重排用确定性公式，不为每次召回多花一次 LLM 调用）。
4. 不做跨用户/代码库记忆（服务对象仍为单用户）。
5. 系统/代码库事实不进入用户记忆（走代码索引/runbook，不属于本方案）。

## 3. 设计总则

1. **默认不记，例外才记**：提取立场从"尽量凑满 N 条"反转为"默认输出 `[]`，只有明确命中某一类判定标准才提取"。
2. **分类是受控的，不是自由生成的**：kind、fact_key、content 均来自受控集合/模板。
3. **敏感维度存在但不凑数**：身体/习俗类只有用户第一人称明确陈述才记，标记敏感，可查看可删。
4. **系统/代码库事实不属于用户记忆**：`workspace:*` 命名空间整体移除。

## 4. 记忆分类体系

### 4.1 顶层两轴

记忆按"关于什么"分两大类，每类映射到现有 `kind`：

| 大类 | 含义 | 稳定性 | 映射 kind |
|---|---|---|---|
| **A. 用户作为人（Person）** | 跨场景、与具体任务无关的用户属性 | 稳定/缓慢变化 | `preference` / `profile` |
| **B. 用户作为工作者（Work）** | 与当前任务/系统相关的工作状态 | 易变 | `work_context` / `episode` |
| **C. 助手的未验证推测** | 非用户明说、assistant 推断 | 待验证 | `assistant_inference` |

### 4.2 完整分类表

| # | 类别 | kind | fact_key（受控词表） | 能提取到什么（真实例子） | 敏感度 | 提取触发条件 |
|---|---|---|---|---|---|---|
| A1 | 回答语言 | `preference` | `user:response-language` | "用中文回答" | 普通 | 用户明确说，或用某语言持续交流 |
| A2 | 回答风格/形式 | `preference` | `user:response-style` | "要流程图"、"要对比结论"、"没证据就明说" | 普通 | 用户明确要求回答形式 |
| A3 | 生活/工具偏好 | `preference` | `user:preference:<topic>` | "性价比导向"、"用 SVG 画图"、"要 4K" | 普通 | 用户表达对某类事物的稳定偏好 |
| A4 | 身份/角色 | `profile` | `user:role` | "后端工程师，在做 agent 项目" | 普通 | 用户透露职业/角色/在做的项目 |
| A5 | 身体/健康 | `profile` | `user:health` | 身体状况、作息、健康约束 | **敏感** | 仅当用户第一人称明确陈述 |
| A6 | 习俗/文化 | `preference` | `user:culture` | 语言习惯、文化禁忌、饮食/节日习俗 | **敏感** | 仅当用户第一人称明确陈述 |
| A7 | 环境/关系 | `profile` | `user:environment` | 所在城市、团队、设备/运行环境 | 普通 | 用户透露工作/生活环境 |
| B1 | 持续工作焦点 | `work_context` | `user:current-focus` | "在梳理 RGB/消息/菜谱/TTS 流程"、"排查线上错误码" | 普通 | **同主题跨会话出现 ≥2 次** |
| B2 | 纠正/历史决定 | `episode` | `user:correction:<topic>` | "纠正过遗漏 schedule RGB 链"、"要求落到服务实现" | 普通 | 用户明确纠正 agent 错误 |
| C1 | 助手推测 | `assistant_inference` | `user:profile-inference` | 从行为推断的用户特征（未验证） | 普通 | 仅当高置信且长期有用，少用 |

### 4.3 对比现状的关键变化

- **删除** `workspace:*` 整个命名空间（系统/代码库事实）。
- **删除** `user:role:<domain>` 的 `<domain>` 后缀，收敛成单一 `user:role` 槽位，避免自由发挥。
- **新增** A5 `user:health`、A6 `user:culture`（身体/习俗），标记敏感。
- **新增** A3 `user:preference:<topic>`（生活偏好，从 response-style 拆出）。
- **新增** B2 `user:correction:<topic>`（纠正，从 episode 独立，authority 最高）。
- **B1 `user:current-focus`** 增加"跨会话 ≥2 次"硬门槛，堵住一次性查询。

## 5. 分类判定决策树

LLM 按**顺序**判断一条候选记忆属于哪类，取第一个命中的：

```
Q0: 这是关于"系统/代码库/服务/架构"的事实吗？
    → 是：DISCARD（不属于用户记忆，走代码索引）

Q1: 这是用户明确纠正 agent 的错误吗？
    → 是：B2 correction（episode, authority 100）

Q2: 这是用户明确说"希望怎么被回答"吗？（语言/风格/形式/详略）
    → 是：A1 或 A2（preference）

Q3: 这是用户透露的"作为人的属性"吗？
    ├─ 身体/健康/作息 → A5（敏感）
    ├─ 习俗/文化/禁忌 → A6（敏感）
    ├─ 职业/角色/项目 → A4
    ├─ 生活/工具偏好  → A3
    └─ 城市/团队/环境 → A7

Q4: 这是用户"当前在做的工作主题"，且【跨会话已出现 ≥2 次】吗？
    → 是：B1 work_context
    → 只出现 1 次：DISCARD（一次性查询，不记）

Q5: 以上都不命中，但 assistant 有【高置信且长期有用】的推测吗？
    → 是：C1 assistant_inference（authority 30，少用）
    → 否则：DISCARD
```

配套硬规则：

- 每个 kind 给 **2 个正例 + 2 个反例**（few-shot）写进 prompt。
- `source_type` 与 kind 强一致：A/B 类（用户说的）→ `user_stated` 或 `explicit_user`；C 类 → `assistant_inference`。
- 敏感类（A5/A6）额外规则：只有用户**第一人称明确陈述**才记，禁止从第三方或推断得出；content 不含具体病情/身份细节，只记"约束"层面（如"用户作息倾向早起"，不记"用户有某病"）。

## 6. content 模板（自包含完整句）

每个 kind 一个 content 模板，强制自包含——任何人脱离上下文读到都能懂：

| kind | content 模板 | 好例 | 坏例（现状乱象） |
|---|---|---|---|
| preference | `用户希望{回答/事物}具备{什么特征}。` | "用户希望回答使用中文。" | `chinese` |
| profile | `用户是{角色}，{负责/在做}{领域}。` | "用户是后端工程师，在做多 agent 项目。" | `open platform architecture` |
| work_context | `用户近期持续在{做什么}。` | "用户近期在梳理 RGB/消息/菜谱/TTS 四大业务流程。" | `?? OauthService ? token ???` |
| episode | `用户曾于{时间}{纠正/决定}{什么}。` | "用户曾纠正 assistant 遗漏 schedule RGB 执行链。" | `????????` |
| health（敏感） | `用户在{健康/作息}上有{约束}。` | "用户提到自己作息倾向早起。" | — |
| culture（敏感） | `用户在{文化/习俗}上偏好{什么}。` | "用户提到饮食上不吃辣。" | — |

代码层校验（提取后、入库前，位于 `canonicalizeRecord`）：

- content 必须是完整句：长度 ≥ 12 字符（中文 ≥ 6 字）、含谓语、无 `?`/乱码/占位符。
- 单语言：统一成用户主语言，做归一化。
- 不含敏感正则（保留现有 password/token/JWT 过滤）。
- 敏感类（A5/A6）额外：必须含第一人称标记，否则 discard。
- 不合格 → discard，并记录到可观测事件。

## 7. 去重与归一化

现状去重失败的根因是 `fact_key` 由 LLM 自由生成。改进：

1. **fact_key 受控词表**（见 4.2 表）：LLM 只能从词表选 key，不能造新 key，同一事实必然落到同一 key。
2. **归一化在代码层做**：提取后对 content 做语言归一 + 大小写/空白/标点归一，再算 fact_key canonical form。`Chinese`/`chinese`/`zh` → 同一条。
3. **单 active 约束**（数据库已有 `uniq_user_factkey_active`）：同一 user 同一 fact_key 只有一条 active，新的走 refresh/replace。
4. **语义去重兜底**：保留 consolidation 阶段已有的 dense ≥ 0.78 召回比对，用于发现"换了说法但同义"的候选，触发 refresh 而非 add。

## 8. 敏感维度（身体/习俗）专门设计

| 维度 | 原则 |
|---|---|
| 不主动挖掘 | 提取 prompt 不诱导找身体/习俗信息；只有用户明确说才记 |
| 第一人称限定 | 必须用户第一人称陈述（"我…"），禁止从第三方/推断得出 |
| 只记约束层面 | 记"约束/偏好"，不记具体病情/身份/隐私细节 |
| 敏感标记 | 数据库加 `sensitive` 标志；召回时默认**不注入**普通 QA 上下文，除非与当前问题直接相关 |
| 可查看可删 | 用户能查看自己所有敏感记忆、能单条删除（扩展现有 management 列表/删除接口） |
| TTL | 敏感记忆默认带较长但有限的 TTL，到期需用户确认仍有效 |

## 9. 提取流程改造

**现状**：prompt 第一句 "Consolidate at most 5 durable memories"，诱导凑数。

**改造后立场**：

```
Most turns produce NO durable memory. Default to output [].
Only extract when a candidate clearly matches one of the categories below
AND is durably useful across future conversations.
When in doubt, output [].
```

**提取数量**：从"最多 5"降到"**最多 2**"，宁缺毋滥。

**提取 prompt（`prompts/memory/extract.txt`）重写结构**：

1. 立场（默认 `[]`）。
2. Q0–Q5 决策树（第 5 节）。
3. 各类 content 模板 + 正/反例（第 6 节）。
4. fact_key 受控词表（第 4.2 节）。
5. 敏感类特殊规则（第 8 节）。
6. 输出 JSON schema（沿用现有 `extractedEntry`，action 默认 discard）。

## 10. 可观测性

新增 `qa_memory_events` 表（或复用 observe 体系），记录每次提取/固化/召回决策：

| 字段 | 用途 |
|---|---|
| turn_id / user_id | 关联 |
| operation | extract / consolidate / recall |
| fact_key / kind | 分类分布 |
| action | add / refresh / replace / reject / discard |
| filtered_reason | 被 discard/reject 的原因（残句/一次性/非第一人称/系统事实） |
| content_len / lang | 内容质量分布 |
| created_at | 时间 |

关键指标：

- **提取率**（每 turn 平均产出条数）：应从 ~1 降到 ~0.2。
- **discard 率及原因分布**：看"一次性查询"和"系统事实"是否被有效拦截。
- **入库记忆 `use_count > 0` 占比**：衡量"记的都是有用的"，应升到健康水平。
- **各类 kind 分布**：看是否还有分类混乱。

## 11. 数据迁移（清洗存量乱记忆）

现有 `qa_memories` 已有大量乱记忆，需一次性清洗（离线脚本或 bootstrap migration step，不在线跑）：

1. **删除 `workspace:*` 全部记录**（系统事实，非用户记忆）。
2. **合并重复**：同一 user 同一语义的记忆（如 6 条 response-language、7 条 bert-slot），按新词表归一化成一条，保留 authority 最高/最新的。
3. **重标 kind**：`user:current-focus` 被错标成 profile/episode 的，统一回 work_context。
4. **删除残句**：content 含 `???`、长度低于阈值、无意义的，直接删。
5. **删除一次性 current-focus**：只出现一次的"在查某 API/字段"，删。

## 12. Schema / 代码改动点清单

| 改动 | 位置 | 内容 |
|---|---|---|
| fact_key 词表 | `internal/memory/model.go` `validFactKey` | 重写：删 `workspace:*`，删 `user:role:<domain>` 后缀，新增 `user:health`/`user:culture`/`user:preference:<topic>`/`user:correction:<topic>` |
| kind 校验 | `internal/memory/model.go` `validKind` | 保留 5 种（health/culture 复用 profile/preference，不新增 kind） |
| 敏感标记 | `internal/memory/model.go` MemoryRecord + `internal/platform/dbschema/mysql.go` | 新增 `sensitive` 字段，召回默认不注入 |
| content 校验 | `internal/memory/model.go` `canonicalizeRecord` | 加完整句/单语言/第一人称（敏感类）校验 |
| 归一化 | `internal/memory/consolidation.go` 或新增 | content 语言/大小写归一，fact_key canonical |
| 提取 prompt | `internal/prompts/memory/extract.txt` | 全文重写（立场反转 + 决策树 + 模板 + 词表 + 敏感规则） |
| 提取数量 | `internal/memory/extract.go` / prompt | 最多 5 → 最多 2 |
| B1 持续焦点门槛 | `internal/memory/consolidation.go` | current-focus 需"跨会话 ≥2 次"才 add（查历史 turn） |
| 可观测事件 | 新增 `qa_memory_events` + 写入点 | extract/consolidate/recall 各写一条 |
| 召回过滤敏感 | `internal/memory/recall.go` | sensitive=1 默认不注入普通上下文 |
| 数据迁移 | 一次性脚本 | 清洗存量乱记忆（第 11 节） |

## 13. 落地顺序

| 阶段 | 内容 | 收益 |
|---|---|---|
| **P0** | 重写提取 prompt（立场反转 + 决策树 + 模板 + 词表）+ fact_key 词表校验 + content 校验 | 立刻止住新产生的乱记忆，成本最低 |
| **P0** | 数据迁移（删 `workspace:*` + 合并重复 + 删残句） | 清掉存量噪声 |
| **P1** | 敏感维度（health/culture）+ 敏感标记 + 召回过滤 + 可查看可删 | 覆盖身体/习俗，且安全 |
| **P1** | B1 持续焦点"跨会话 ≥2 次"门槛 | 堵住一次性查询 |
| **P2** | 可观测事件表 + 指标 | 验证改进效果，持续调优 |

**P0 先行**：改 prompt + 校验 + 迁移，不动架构，见效快，新提取的记忆立刻变干净。敏感维度和可观测性随后。
