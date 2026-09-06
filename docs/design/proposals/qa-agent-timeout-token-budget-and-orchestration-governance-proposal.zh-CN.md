# QA Agent 超时、Token 膨胀与多 Agent 收敛治理提案

状态：草案，待评审
作者：Nasuta Agent Platform Team
日期：2026-09-05
关联事项：Run `run_341d789fbca1de96c4710548`；日志附件 `/Users/dequan.mac/.codex/attachments/fe7fafe9-44af-455b-b666-d43d10f8278e/pasted-text.txt`
关联提案：`qa-llm-provider-aware-reasoning-and-budget-governance-proposal.zh-CN.md`、`qa-delegation-async-settlement-proposal.zh-CN.md`、`qa-agent-context-budget-and-cancellation.zh-CN.md`、`investigation-run-budget-and-large-context-governance-proposal.zh-CN.md`
目标版本：待定

> 本提案是针对本次失败运行的综合落地提案，负责把时间预算、模型生成预算、Child 收敛、上下文投影和失败语义收敛到同一条执行链路。不建立第二套 evidence、run budget 或 provider 请求契约；已有专项提案仍是对应领域的详细设计来源。

## 1. 摘要

本提案用于解决 QA Agent 在复杂多业务调查中出现的运行超时、Child 报告失败、上下文和 Token 膨胀，以及最终答案阶段无法正常收敛的问题。

本次运行 `run_341d789fbca1de96c4710548` 的硬失败原因是父 Agent 的 wall-clock deadline 到期，而不是父 Agent 首先耗尽全局 Token 预算。运行总时限约为 `4m52.728s`，其中 `AnswerReserve=30s`，配置 `maxSteps=8`，父 Agent 实际只完成了 4 个 step；最终 synthesis 被取消，QA 层记录为 `cancelled / context deadline exceeded`。同时，4 个 Child Agent 累计约消耗 `218,427` tokens、执行 48 次工具调用，其中一个 Child 因 `reported output=14164` 而 `available_output=14163`，仅超出 1 token 就被判定为失败。

根因不是单一的“预算太小”，而是预算口径和执行职责没有闭合：父 Agent 单次模型调用上限过宽，Child 的生成上限与 report 上限混用，report cap 在生成之后才裁剪；Child 完成后父 Agent 仍可继续轮询状态和检索；`delegation_status` 回填过大；最终答案 reserve 固定且过小；预算核算缺少边界余量；Agent 层 fallback 又清除了部分错误语义。

本提案计划将当前流程从：

```text
父 Agent 大步长探索
→ 并发派发 Child
→ Child 生成长 reasoning/report
→ 父 Agent 回填完整状态和报告
→ Child 已结束但仍允许继续轮询/检索
→ 固定 30s reserve 不足
→ 最终模型调用被 deadline 取消
→ fallback 与上层终态不一致
```

调整为：

```text
请求进入
→ 按任务类型建立共享 Run Budget
→ 按阶段设置真实 provider generation cap
→ 有界派发 Child，并由服务端负责等待和收口
→ 只回填摘要、证据 ID 和完成度
→ Child 全部结束后关闭探索工具，强制进入 synthesis
→ 剩余时间门禁保护 answer reserve
→ 超时/预算耗尽时输出显式 partial/fallback
→ 统一记录 termination reason、usage 和完整度
```

预期实现：复杂调查不再因无效轮询和过宽单轮上限耗尽时间；Child 轻微预算误差不再导致整项失败；模型上下文和总 Token 可控；最终答案拥有真实、可观测、可回放的完成度。

## 2. 背景

### 2.1 业务与技术背景

QA Agent 需要回答涉及多个服务、代码入口、运行边界和业务流程的调查型问题。此类请求通常由一个 Parent Agent 负责拆题、证据汇总和最终回答，并由多个只读 Child Agent 并行调查不同主题。

相关链路为：

```text
QA 请求
→ QA Service 创建 Parent Run
→ Parent Agent 准备上下文、Evidence Plan 和工具集
→ Parent Agent 调用模型决定检索或委派
→ delegate_investigation 并发启动 Child Agent
→ Child Agent 调用知识工具并生成结构化 Investigation Report
→ Parent Agent 接收 Child 结果和证据引用
→ Verifier / Synthesis 生成最终答案
→ QA 层持久化执行状态、答案和使用量
```

各模块主要职责：

| 模块 | 当前职责 | 输入 | 输出 |
| --- | --- | --- | --- |
| `internal/agent/qa` | 创建 QA Run、准备上下文、提交最终结果 | 用户问题、配置、历史和预检索证据 | Parent Run、QA 终态 |
| `internal/agent/execution` | 执行 Parent/Child Agent loop、模型调用、工具调用、答案契约校验 | messages、tools、phase budget | step 结果、答案、usage |
| `internal/agent/delegation` | 计算 Child 预算、派发任务、等待/汇总 Child 结果 | Task Contract、Parent Budget | delegation batch、reports、errors |
| `internal/agent/catalog` | 定义 investigator、verifier、synthesizer 的角色、工具和预算默认值 | Platform Settings | Agent Definition |
| `internal/agent/run` | 保存 Run、预算 reservation、usage 结算和租约 | Run/usage/budget events | durable budget 与审计数据 |
| `internal/llm` | 将逻辑模型调用参数转换为 provider 请求并解析 usage | messages、tools、logical completion limit | provider response、usage、timing |

本提案遵循以下既有架构边界：

1. Provider 请求字段由 `internal/llm` 及其 capability 负责，不由 Parent/Child loop 根据模型字符串散落判断。
2. Run 累计预算由已有 durable budget/ledger 负责，不能由各个 Agent 自己维护一份平行账本。
3. Evidence 的权威内容仍由现有 evidence/tool trace 机制保存；模型上下文只接收有界 projection。
4. Child 的异步传输能力保留，但“等待完成”和“何时进入 synthesis”由服务端/loop 的确定性流程负责。

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/execution/loop.go`：创建 Run deadline 和 loop deadline，当前使用 `runTimeout - AnswerReserve`；
- `internal/agent/execution/loop_turn.go`：执行 Parent/Child 模型回合、继续生成、答案契约校验和 partial answer 保留；
- `internal/agent/execution/model_call.go`：一次物理模型调用的预算 reservation、provider 调用和 usage 结算；
- `internal/agent/delegation/executor.go`：Child budget、委派 batch、Child settle 和 report 约束；
- `internal/agent/catalog/defaults_investigation.go`：Investigator、Verifier、Synthesizer 的默认模型和预算；
- `internal/agent/qa/submission.go`：QA 层对 Agent 结果和取消状态的最终投影；
- `internal/agent/run/store_budget.go`：durable budget、reservation 和 usage 账本；
- `internal/llm/client.go`：provider 请求、`max_tokens` 映射、usage 与 timing 记录。

当前关键行为：

1. Parent Run 使用总 timeout 创建 `runCtx`，再使用 `runTimeout - AnswerReserve` 创建 loop context。`AnswerReserve` 当前为固定 30 秒。
2. Parent 每轮模型调用复用 `agent.cfg.AnswerMaxTokens`；日志中请求显示 `logical_completion_limit=24000`。这是每次物理调用的上限，不是整个 Run 的上限。
3. Investigator 使用 `settings.LLMAnswerMaxTokens` 作为 `Model.MaxOutputTokens`；`MaxReportTokens` 在报告生成后由 `boundReport` 再裁剪。
4. Child 的 input/output/tool usage 会累计到 Parent/Child 预算，但并行 Child 的 wall-clock 由最慢 Child 决定，Token 和成本则对所有 Child 累加。
5. `delegation_status` 会把较大的 reports、claims、edges 和 metadata 回填到 Parent messages；Child 完成后，如果没有服务端强制收口，Parent 仍可能继续轮询或重复检索。
6. tool pruning 当前能够计算潜在节省，但本次日志显示 `applied=false`，完整工具集仍然被发送。
7. 小幅 output 超限在 usage 结算时直接作为 hard failure，尚未区分可保留的 partial report 和不可恢复的 hard failure。

### 2.3 为什么现在需要修改

本次修改由一次可复现的复杂多业务调查超时触发：

- 触发时间：日志记录为 `2026-09-06 01:25:02` 至 `01:29:55`（`+08:00`）；
- 触发标识：`run_341d789fbca1de96c4710548`；
- 用户问题：`帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的`；
- 直接表现：Parent Run 在最终 synthesis 前收到 `context deadline exceeded`，QA 层记录为取消，用户只得到 351 字的确定性 fallback；
- Child 结果：4 个 Child 中 3 个完成、1 个失败；失败 Child 仅超出可用 output 预算 1 token；
- 资源表现：Child 总 usage 约 218,427 tokens、48 次工具调用，Parent 上下文字符数从约 42,801 增长到约 113,616，Child 上下文最高约 139,769 字符；
- 临时处置：系统安装 deterministic conclusion，但该 fallback 没有完整保留 deadline/partial 语义，导致 Agent 日志和 QA 终态表达不一致。

关键证据位于附件：

- `pasted-text.txt:903-906`：Run timeout、`AnswerReserve=30s`、`maxSteps=8`、初始上下文和 tool pruning 未应用；
- `pasted-text.txt:906`：Parent 模型请求使用 `logical_completion_limit=24000`；
- `pasted-text.txt:1620`、`pasted-text.txt:2828`、`pasted-text.txt:11559`：Parent 多轮模型耗时；
- `pasted-text.txt:9390-9391`：Child `reported output=14164`、`available_output=14163`，usage 结算失败但保留了部分答案；
- `pasted-text.txt:9469`：Child settle 为 `status=failed`，错误为 `child output token limit exceeded`；
- `pasted-text.txt:10508-10512`：较大的 `delegation_status` 结果回填和 trace 持久化；
- `pasted-text.txt:11538-11545`、`pasted-text.txt:12653-12658`：Child 已全部结束后仍出现状态回填/重复状态路径；
- `pasted-text.txt:14044-14051`：deadline fallback、Agent 层 `aborted=false err=nil` 与 QA 层 `cancelled/context deadline exceeded` 不一致。

### 2.4 范围与非目标

#### 目标

1. 将 Parent、Child、Verifier、Synthesis 的时间和模型生成预算拆成独立、可结算的阶段预算。
2. 在发起模型或工具调用之前执行剩余时间门禁，保证最终答案阶段有足够的动态 reserve。
3. 将 Child 完成后的等待、回填和 synthesis 收口交给服务端/loop，禁止模型通过轮询浪费 step 和时间。
4. 将 Child report 的“模型实际生成上限”和“交付给 Parent 的报告上限”分离。
5. 对完整报告、摘要、证据 ID 和工具结果使用不同的投影策略，防止上下文无界增长。
6. 对轻微预算误差保留 partial，对真正不可恢复的预算耗尽输出明确失败原因。
7. 使 Agent、QA、持久化和评估调用方共享同一个终止原因、完成度和 usage 解释。
8. 建立普通 QA 与多 Agent 深度调查的预算 profile，并通过固定数据集和 p95/p99 指标校准默认值。

#### 非目标

1. 本提案不针对 RGB、消息中心、菜谱或 TTS 增加关键词、服务名或路径特例。
2. 本提案不把所有任务的 timeout 或 token 上限简单提高，也不把全局 `720k` token budget 当作本次问题的首要修复点。
3. 本提案不通过在生成完成后裁剪 report 来假装减少模型 reasoning/decode 时间。
4. 本提案不重新设计 evidence 数据模型、知识检索算法或 provider API；只定义其在本执行链路中的预算和投影边界。
5. 本提案不把异步 `delegate_investigation` 改成阻塞式传输调用；异步传输保留，服务端负责确定性等待和收口。
6. 本提案不静默切换 provider、删除错误、伪造完成或把 partial answer 标记为完整答案。
7. 本提案不在没有观测数据的情况下承诺固定的行业标准 token/秒比例；具体数值通过本提案的基准和分位数校准。

## 3. 问题

### 3.1 问题描述

**期望行为：**

对于需要多个 Child 调查的 QA 请求，系统应在总 deadline 内按任务复杂度分配时间和 Token，Child 结果应在完成后一次性、有界地交付给 Parent，Parent 应在 answer reserve 内完成可验证的 synthesis。任一预算维度耗尽时，系统应明确说明是时间、provider completion、工具、step、成本还是上下文预算耗尽，并保留可以安全使用的 partial evidence/report。

**实际行为：**

本次 Run 只有 4 个 Parent step 就因 wall-clock deadline 到期而停止。Child 侧有较大 reasoning/output 长尾，报告 cap 是生成后的裁剪；Parent 侧继续承担状态回填、重复检索和大上下文处理；Child 结束后仍允许状态工具路径；最后 30 秒 reserve 无法稳定覆盖 synthesis、contract validation 和 fallback。一个 Child 只因 1-token 边界误差就被判 hard failure，Agent 层清除了部分错误但 QA 层仍记录为取消。

**差异：**

系统当前把“单次模型 output 上限”“可见报告上限”“Run 累计 Token 预算”“Parent 剩余时间”“Child batch wall-clock”和“最终答案保留时间”混在不同层次处理，既没有在调用前做统一准入，也没有在调用后以同一个账本和终止语义结算。因此，增加 `MaxReportTokens` 并不能减少实际生成时间，增加全局 Token 预算也不能阻止 Parent 在 deadline 前继续探索。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | QA 返回未完成的 fallback，执行结果被标记为 cancelled | `pasted-text.txt:14044-14051` |
| 直接原因 | Parent Run 的 wall-clock deadline 到期，最终 synthesis/model call 被取消 | `pasted-text.txt:903`、`pasted-text.txt:14044` |
| 机制根因一 | `AnswerReserve` 固定为 30 秒，且没有基于 phase、上下文和模型尾延迟动态门禁 | `internal/agent/execution/loop.go`；`pasted-text.txt:903` |
| 机制根因二 | Parent 单次调用使用 24k 逻辑 completion limit，Child Investigator 使用全局 answer max；阶段职责没有独立 cap | `internal/agent/execution/loop_turn.go`、`defaults_investigation.go`；`pasted-text.txt:906` |
| 机制根因三 | `MaxReportTokens` 在模型生成后才执行，无法减少 reasoning/decode 时间 | `defaults_investigation.go`、`delegation/executor.go` |
| 机制根因四 | Child 完成后的等待和收口没有完全由服务端接管，Parent 仍可轮询状态或重复检索 | `pasted-text.txt:11538-11545`、`pasted-text.txt:12653-12658` |
| 机制根因五 | `delegation_status` 和 Child context 投影过大，完整 reports/claims/edges 被重复放入 messages | `pasted-text.txt:10508-10512`；上下文字符数观测 |
| 机制根因六 | Token accounting 没有边界余量，轻微超限直接 hard failure | `pasted-text.txt:9390-9391`、`pasted-text.txt:9469` |
| 机制根因七 | tool pruning 只计算未真正应用，且 Parent/Child/预检索存在重复工具职责 | `pasted-text.txt:905` |
| 机制根因八 | fallback 清除 Agent 层错误字段，QA 层仍保留原始 deadline error，终止语义不一致 | `pasted-text.txt:14044-14051` |

根因链路：

```text
复杂问题触发多 Child 调查
→ Child 数量、工具调用和 reasoning 总量增加
→ 单轮 completion cap 过宽，report cap 只能事后裁剪
→ Parent 回填大报告/状态，并在 Child 完成后继续探索
→ 上下文 prefill、模型 decode、工具等待和 Child batch 共同消耗 wall-clock
→ 固定 30s answer reserve 不足
→ 最终 synthesis 被 context deadline 取消
→ deterministic fallback 清除部分错误语义
→ Agent、QA、评估调用方对终态理解不一致
```

本问题不能只通过提高 `720k` 全局 Token 上限、提高 Parent timeout、降低生成后的 `MaxReportTokens`、增加重试或关闭某个案例的工具来解决，因为这些方式不能阻止无效探索、不能减少已发生的 reasoning/decode 时间，也不能保证最终答案阶段在 deadline 内拥有可用的时间窗口。

### 3.3 影响

- **用户影响：** 复杂问题只能得到未整合的 fallback，已收集的证据不能及时转化为最终答案，用户无法判断哪些内容已确认、哪些内容未完成；
- **业务影响：** 多业务流程问题的可用回答率下降，评估侧将底层仍在运行的任务记为超时或取消；
- **系统影响：** Child 的 Token、工具调用、上下文处理和 provider 资源被重复消耗，deadline 到期后仍可能有活动调用；
- **工程影响：** 失败日志无法明确区分 time/token/steps/tool_calls 等预算维度，Agent 与 QA 状态语义不一致，问题难以回放和校准。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：多 Child 调查占满 Parent 的最终答案窗口

- **Given（前置条件）：** 一个 flow/overview 类问题需要 RGB、消息中心、菜谱和 TTS 四个独立主题；Parent timeout 约 5 分钟，`AnswerReserve=30s`，Parent 每次模型调用上限为 24k；
- **When（触发行为）：** Parent 先做预检索，再派发 4 个 Child，Child 并行进行多次工具调用并返回报告；
- **Then（期望结果）：** Child 全部结束后服务端一次性收口，Parent 只接收有界摘要，并在保留的 answer reserve 内完成 synthesis；
- **But（当前结果）：** Child 批次耗时约 2 分钟以上，Parent 继续处理大状态/报告和重复检索，最终 deadline 到期，synthesis 被取消。

示例输入：

```text
帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的
```

当前执行路径：

```text
Parent 预检索
→ delegate_investigation(4 个 Child)
→ Child 报告和状态回填
→ Parent 仍允许 delegation_status/search_runbooks
→ 上下文继续增长
→ deadline 到期
→ fallback
```

关键证据：

```text
总 timeout≈4m52.728s，AnswerReserve=30s，maxSteps=8
Child batch：4 tasks，3 completed，1 failed
Child 总 usage≈218,427 tokens，tool calls=48
Parent contextChars：约 42,801 起步，后续最高约 113,616
```

#### 场景 B：Child 只因 1-token 误差被判定失败

- **Given：** Child 已经生成了大部分有效调查结果，预算结算显示 `reported output=14164`、`available_output=14163`；
- **When：** usage accounting 严格按可用值判定 hard limit；
- **Then：** 系统应保留可解析的 partial report，记录软超限和截断原因，并让 Parent 在 synthesis 时标注该 Child 不完整；
- **But：** 当前 Child 被记录为 `child output token limit exceeded`，从 batch 角度看为 failed。

#### 场景 C：降低 report cap 但模型仍然生成很久

- **Given：** Investigator 的 `MaxOutputTokens` 绑定到约 16k，报告最终上限为 4k；
- **When：** 模型先生成 reasoning 和 output，生成完成后才调用 `boundReport`；
- **Then：** 应在 provider 请求前使用 Investigator 的实际 generation cap，生成量从源头受限；
- **But：** 当前 4k report cap 只影响交付大小，不影响已发生的模型生成时间。

#### 场景 D：Child 已结束但 Parent 继续轮询和检索

- **Given：** delegation batch 已经全部 settle，系统已经有报告、状态和 evidence ID；
- **When：** Parent 仍可调用 `delegation_status` 或 `search_runbooks`；
- **Then：** 应关闭探索工具，直接进入 verifier/synthesis；
- **But：** 当前路径存在重复状态回填和继续搜索，浪费 step、工具时间和上下文容量。

### 4.2 边界场景

| 场景 | 输入或条件 | 当前行为 | 目标行为 |
| --- | --- | --- | --- |
| 正常单 Agent | 单一事实问题、无需 Child | 使用统一的大 output cap，可能产生不必要 reasoning | 使用 normal QA profile，不受多 Agent预算影响 |
| 多 Child 正常完成 | 2～4 个 Child 全部完成 | Parent 需要模型决定是否继续等待/收敛 | 服务端一次性汇总，Parent 直接 synthesis |
| Child 超时 | 单个 Child 超过 batch deadline | 可能继续等待或以失败报告结束 | 取消该 Child，保留其他结果，输出 partial 并标记 child timeout |
| Child 轻微 token 超限 | 超限不超过 headroom | 整个 Child hard failure | 保留 partial report，记录 soft budget exhaustion |
| Parent 剩余时间不足 | 剩余时间小于下一调用 p95 + reserve | 仍可能开始新模型/工具调用 | 通过 admission gate 拒绝新探索，直接 synthesis/fallback |
| 上下文过大 | Parent/Child 达到 compact threshold | 继续携带完整 history/tool result | 主动 compact，只保留摘要、证据 ID 和未解决目标 |
| 工具重复 | 预检索和 Child 使用同一查询/证据 | 重复请求和重复注入 | 通过 evidence ledger 去重，复用已确认结果 |
| tool pruning | 可安全移除的工具存在 | 只计算节省，实际仍发送完整工具集 | 按 phase 应用 pruning，并记录实际工具集合 |
| provider 下游失败 | provider 返回错误或超时 | 可能与预算耗尽混在一起 | 保留 `%w` 原因，分类为 provider/time/tool 等明确错误 |
| fallback | deadline 到期但已有部分证据 | Agent 层可能清除错误，QA 层记录 cancelled | 输出 partial/fallback，统一 `termination_reason` 和 `answer_complete=false` |

### 4.3 复现步骤

1. 使用已有 Nasuta/CodeLoom 本地环境和附件中的固定索引数据，提交问题：`帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的`；
2. 让 QA 选择 Parent Dynamic Delegation，配置 Parent `maxSteps=8`、总 timeout 约 5 分钟、`AnswerReserve=30s`，并启用 4 个 investigator Child；
3. 保留当前工具集、`logical_completion_limit=24000`、Investigator 全局 output 上限和 report 事后裁剪逻辑；
4. 观察 Parent step timing、Child batch timing、`contextChars`、usage、tool calls 和 delegation status；
5. 预期可见：Child batch 结束前后 Parent 仍存在状态/检索路径，最终出现 `context deadline exceeded`，并在 `delegation_status`/report 回填后产生大上下文；
6. 将同一固定运行分别套用本提案的各项开关，比较 deadline timeout、总 Token、Child failure、post-settlement tool calls 和最终答案完整度。

## 5. 如何修改

### 5.1 修改原则

1. **修复机制，不增加案例特例。** 预算和收敛逻辑只依据任务类型、phase、证据增量和剩余资源，不识别 RGB、消息中心、菜谱或 TTS 等具体名称。
2. **每个预算维度只有一个所有者。** `internal/llm` 负责 provider wire limit，Agent phase 负责角色级 cap，Run ledger 负责累计 Token/cost，deadline controller 负责 wall-clock，loop 负责 steps/tool calls。
3. **调用前控制生成，调用后控制交付。** `generation_max_tokens` 必须在 provider 请求前生效；`report_max_tokens` 只限制交付 projection，不能替代 generation cap。
4. **服务端负责异步收口。** `delegate_investigation` 保持非阻塞传输，但 Parent 不通过模型轮询来等待 Child；服务端负责等待、取消、汇总和进入 synthesis。
5. **剩余时间优先保证最终答案。** 时间不足时禁止新的探索调用，宁可输出明确 partial，也不把全部时间耗尽后再 fallback。
6. **失败可诊断、降级不伪成功。** 每次预算耗尽必须标识维度；partial/fallback 必须公开完成度和 termination reason，不得清除原始 deadline 或 provider error。
7. **保持单一事实源。** Parent 只引用 `report_id`、`evidence_id`、摘要和 top claims；权威完整内容保存在 trace/artifact/evidence ledger 中。
8. **先观测再校准。** 初始值是 A/B 起点，不宣称为行业统一标准；用固定评估集和线上 p95/p99 校准，而不是凭单次样例继续放大预算。

### 5.2 目标流程

```text
QA request
→ Normalize task kind and choose budget profile
→ Create shared Run deadline and durable budget ledger
→ Prepare bounded parent context and phase-specific tool set
→ Parent model admission check
   ├─ insufficient time/budget → synthesis or explicit partial fallback
   └─ admitted → one bounded model/tool turn
→ delegate_investigation (non-blocking transport)
→ server-side await batch with child deadline
→ project child results to summary/evidence IDs only
→ close delegation and exploration tools
→ verifier/synthesis with reserved answer window
→ validate answer/flow contract
→ persist result with termination reason, completeness and usage
```

与当前流程相比，关键变化是：

1. 在 Run 创建时冻结共享时间、Token、成本、steps、tool calls 和上下文预算，并将其作为所有 Parent/Child 调用的准入依据；
2. 在每次物理模型或工具调用前，检查“预计耗时 + 动态 answer reserve + 结算开销”是否仍能落在 deadline 内；
3. 将 Parent 的探索、Child 调查、Verifier 和 Final Synthesis 分为不同的 phase profile，不再复用 24k/16k 的宽上限；
4. 将 Child 的 generation cap 与 report projection cap 分开；
5. 将 Child 等待从模型职责移到 delegation executor/loop 的确定性路径；
6. Child batch terminal 后关闭 `delegation_status`、`delegate_investigation` 和已完成目标对应的检索工具；
7. `delegation_status` 只回填 summary、top claims、verified hops、evidence/report ID、状态和错误；
8. deadline、soft/hard budget exhaustion、partial/fallback 和 QA 终态统一使用同一 termination contract。

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 动态 answer reserve | 固定 `30s` | 按 phase、context、模型 p95 和最小保留值动态计算；剩余时间不足时禁止新探索 | `internal/agent/execution/loop.go`、`loop_turn.go` | 默认先 shadow 记录 admission 结果，再灰度启用 |
| Parent phase cap | 各回合复用 `AnswerMaxTokens`，日志为 `24000` | tool-use、continuation、repair、synthesis 分离 cap | `internal/agent/execution`、`config` | 旧配置映射到 normal profile，保留显式 override |
| Child generation/report | Investigator 用全局 `LLMAnswerMaxTokens`，report 事后裁剪 | provider generation cap 先限制，report cap 再做有界 projection | `internal/agent/catalog`、`internal/agent/delegation` | 未配置新字段时使用保守默认值 |
| Child settle | Parent 依赖模型决定是否轮询 | 服务端等待 batch terminal，一次性 handoff | `internal/agent/delegation`、`execution` | 保留 legacy poll API，仅从正常 Parent 路径隐藏 |
| Settled 后工具 | Child 完成后仍可能继续 status/search | 强制 synthesis，关闭已完成委派和重复检索工具 | `internal/agent/execution`、tool snapshot | 仅作用于 managed delegation run |
| Context projection | 完整 reports/claims/edges 回填 messages | 只回填 summary、top claims、evidence/report IDs 和 unresolved gaps | `internal/agent/delegation`、`execution` | 权威完整报告继续持久化，旧 API 读取不变 |
| Child 输入 | 可能携带较大父历史和重复证据 | 任务定向投影，设置 input soft cap，主动 compact/dedup | `internal/agent/delegation`、`qa` | 过大时返回明确 `context_budget_exhausted`，不静默截断 |
| Token accounting | 无 headroom，1-token 超限 hard fail | 预留 headroom，soft overrun 保留 partial，hard overrun 才失败 | `internal/agent/execution/model_budget.go`、`internal/agent/run` | 保留 reported/effective usage 两套字段并记录推导关系 |
| Tool pruning | 只计算潜在节省，`applied=false` | phase-aware pruning 真正应用并记录 offered/effective tools | `internal/agent/execution` | 通过开关灰度，工具权限不扩大 |
| 终止语义 | Agent fallback 可能清除错误，QA 仍记录 cancelled | 统一 `termination_reason`、`fallback_used`、`answer_complete`、`completeness` | `internal/agent/definition`、`qa/submission`、run store | 增加字段时保持旧状态读取兼容 |
| 物理调用观测 | 缺少完整 phase/remaining budget/TTFT 维度 | 逐调用记录 usage、timing、context 和剩余预算 | `internal/llm`、`execution`、`run` | 先增加日志/指标，不改变结果语义 |

#### 改动一：建立共享 Run Budget 和动态 answer reserve

**方案：**

Run 创建时由已有 Run budget owner 生成一个共享账本，至少包含：

```text
TotalDeadline
DynamicAnswerReserve
ParentStepLimit
ParentToolCallLimit
MaxChildCount
MaxChildWallClock
ProviderOutputBudget
MaxTotalTokens
MaxTotalCost
ContextSoftLimit
ContextHardLimit
```

`DynamicAnswerReserve` 不再只是固定常量，而是由以下因素决定：

```text
reserve = max(
    configured_minimum,
    p95(final_synthesis + contract_validation + persistence),
    context_compaction_cost + p95(final_synthesis),
)
```

在没有足够历史分位数数据前，先使用以下 A/B 起点：

| profile | 总 timeout 起点 | answer reserve 起点 | Parent max steps | 适用范围 |
| --- | ---: | ---: | ---: | --- |
| `normal_qa` | 3～5 分钟 | 45～60 秒 | 3～4 | 单一事实、少量工具、无 Child |
| `deep_multi_agent` | 6～8 分钟 | 60～90 秒 | 4～5 | 多主题调查、2～4 个 Child |
| `deep_multi_agent_complex` | 8～10 分钟 | 60～90 秒 | 4～5 | 大上下文、跨服务、多轮验证 |

这些值不是行业标准，只是用于校准的初始 profile。普通 QA 和复杂多 Agent 任务不能共用一组固定值。

**约束：**

- answer reserve 不能被 Parent 探索、Child 派发或新的工具调用借用；
- 任何调用必须在开始前检查剩余 wall-clock、预计调用耗时和结算开销；
- 外层调用方 deadline 更短时，以更早 deadline 为准；
- 不通过延长总 timeout 来绕过 admission gate；
- 只增加运行时派生的 phase 信息，不新增无法从既有 Run facts 推导的持久化状态机。

**失败行为：**

- 剩余时间不足以完成新探索但仍有可用证据时，直接进入 synthesis/partial；
- 剩余时间连 synthesis 最小预算也不足时，输出确定性 partial/fallback，并标记 `termination_reason=deadline_exceeded`；
- admission 拒绝必须记录拒绝维度和估计值；
- 不允许启动一个预计会越过 answer reserve 的新 Child 或模型调用。

#### 改动二：按 phase 分离 Parent、Child 和最终答案的 generation cap

**方案：**

保留内部逻辑字段 `LogicalCompletionLimit`，由 `internal/llm` 根据 provider capability 映射到实际 wire 字段。执行层按 phase 计算 effective cap：

| phase | 初始 generation cap | report/visible 交付 cap | 说明 |
| --- | ---: | ---: | --- |
| Parent tool-use | 4k～8k | 不直接作为用户答案 | 只需要做路由、参数和下一步决策 |
| Parent continuation/repair | 2k～4k | 2k～4k | 只修复已有结果，不重新探索 |
| Child Investigator | 6k～8k | 2k～4k | 生成量和交付报告分开 |
| Delegation Verifier | 4k～6k | 2k～4k | 只处理已有 evidence |
| Final Synthesis | 4k～8k | 4k～8k | 保护最终可见答案 |

这些数值同样是起始配置，不是所有模型和任务的统一标准。对于支持 reasoning 的 provider，generation cap 必须覆盖 provider 实际计费的 completion 语义，不能把它误当成 visible answer cap。

**约束：**

- 一次请求只发送 provider/API 实际支持的一个 completion limit 字段；
- phase cap 在 `callModel` 之前转为 effective provider limit；
- `report_max_tokens` 不得扩大 provider generation cap；
- Child 的角色限制仍由 Agent Definition 所有，预算 profile 不能授予额外工具或权限；
- Final Synthesis 至少保留一个可用模型调用机会，除非剩余时间已不足以发起该调用。

**失败行为：**

- provider 不支持的 reasoning 或 completion 参数不能静默发送；
- provider 失败、预算拒绝和上下文超限必须分别分类；
- Child 生成被截断但已有可解析报告时返回 partial report；
- Final Synthesis 失败时使用受控 deterministic fallback，并保留原始失败原因。

#### 改动三：服务端接管 Child 等待和收口

**方案：**

`delegate_investigation` 继续快速返回 delegation ID，但正常 Parent loop 不再把“是否已经完成”交给模型通过 `delegation_status` 决定。Delegation executor 在共享 batch deadline 内等待 Child terminal，并生成一次有界 handoff：

```text
Child terminal results
→ normalize status
→ settle child reservations
→ persist complete reports/artifacts
→ project summary/evidence IDs
→ mark delegation settled
→ return one handoff to Parent
```

当 batch 中存在失败、超时或取消 Child 时，handoff 仍返回已完成 Child 的结果，并将未完成项作为明确 gaps。

**约束：**

- 传输层仍是异步，不能把 HTTP/MCP 工具调用改成几十秒的阻塞请求；
- await 必须受 Child/batch deadline 和 Parent answer reserve 约束；
- Child 全部 terminal 后，`delegation_status`、`delegate_investigation` 和已完成目标的检索工具从正常 Parent 工具集移除；
- Child 的完整报告只进入权威持久化/trace，Parent 只接收有界 projection；
- 同一 delegation 只能完成一次 handoff，重复 settle 必须幂等。

**失败行为：**

- batch deadline 到期：取消仍运行的 Child，保留已完成结果，返回 `partial`；
- Child provider 错误：保留错误分类和已完成的其他 Child，不把整个 batch 静默标记为成功；
- Parent deadline 先到：停止等待和新调用，使用当前已确认结果生成 partial/fallback；
- settlement 失败：保持 durable reservation 的错误可见，并阻止伪造 `succeeded`。

#### 改动四：采用摘要化 delegation handoff 和上下文治理

**方案：**

`delegation_status` 或服务端 handoff 的模型投影只允许以下内容：

```text
DelegationSummary {
    delegation_id
    status
    task_id
    subject
    report_id
    evidence_ids
    summary
    top_claims
    verified_hops
    unresolved_gaps
    error_code
    completeness
}
```

不直接回填完整 `reports`、全部 `claims`、全部 `edges`、重复 metadata 或同一证据的多个文本副本。完整内容保留在 trace/artifact/evidence ledger，必要时由后续明确的 evidence reference 工具按范围读取。

Child 输入采用任务定向投影：

```text
Parent task contract
+ subject-specific evidence IDs
+ required facets
+ bounded context summary
+ unresolved questions
```

不默认复制完整 Parent history。达到 context soft limit 时主动 compact；达到 hard limit 时拒绝新增上下文或工具调用，并返回明确 partial。

**约束与失败行为：**

- 权威报告和模型 projection 必须通过 ID/版本关联，不能只保留不可追溯的摘要；
- projection 截断必须记录 `omitted=true`、原因和 artifact/report ID；
- context hard limit 触发时不静默删除用户问题、任务目标或证据来源；
- 发现同一 evidence 的重复表示时，保留权威版本并记录 dedup 节省量。

#### 改动五：修复预算 accounting 的边界和 partial 语义

**方案：**

每次物理模型调用使用以下顺序：

```text
estimate request
→ reserve with safety headroom
→ call provider
→ record reported usage
→ normalize effective usage
→ settle reservation
→ classify exact/soft/hard exhaustion
```

建议在 provider usage 结算中预留 `256～512` tokens 或 `5%～10%` 的边界余量，具体值通过 provider 回放校准。`reported_usage`、`effective_usage` 和 `available_before_call` 必须同时记录，避免 1-token rounding 差异不可诊断。

**约束与失败行为：**

- reasoning token 仍计入 provider output、总 Token 和成本账本；
- 轻微超限且输出可解析时，保留 partial，并将状态设为 `partial / soft_budget_exhausted`；
- 超过 headroom、usage 无法解析或结果不可安全使用时，才设为 hard failure；
- 任何 reservation 不能因为 fallback 被静默释放或标记为成功；
- total token/cost、time、steps、tool calls 是不同预算维度，不能互相抵消。

#### 改动六：按 phase 应用 Tool Pruning 和重复检索治理

**方案：**

工具集合由 phase 和未解决的 Evidence Goal 决定：

```text
prepare/parent route
→ 只暴露必要的发现和委派工具
child investigation
→ 只暴露该 capability 需要的只读工具
settled/synthesis
→ 隐藏 delegation/status 和重复检索工具
verifier
→ 只暴露 evidence/reference 读取工具
```

对预检索结果、Child 工具结果和 Parent 复核使用同一 run-local evidence ledger，通过 evidence ID、query scope 和 coverage 判断是否已有足够证据，避免只因模型想“再搜一次”而重复调用。

**约束与失败行为：**

- pruning 不能扩大工具权限，只能移除当前 phase 不需要的工具；
- 仍然必须提供完成任务所需的最小工具集；
- pruning 应记录 offered/effective 工具列表和实际节省，不能只记录 dry-run；
- 如果去重判断不确定，保留一次有界复核机会，不允许无界重复。

### 5.4 数据结构或接口契约

新增或修改的核心字段建议如下。字段应优先复用已有 Run/usage/result 类型；只有现有类型无法表达时才增加字段。

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `budget_profile` | `string` | QA preparation | `normal_qa`、`deep_multi_agent` 等任务预算 profile | 按任务类型派生 | 新增可选字段 |
| `phase` | `string` | execution | 当前物理调用阶段，如 `parent_tool_use`、`child_investigation`、`synthesis` | `unknown` | 日志/账本新增字段 |
| `requested_output_cap` | `int64` | execution | 调用前请求的逻辑 completion 上限 | 现有值 | 保留旧 usage |
| `effective_output_cap` | `int64` | `internal/llm`/execution | capability 映射后实际生效的 provider limit | 同 requested | 新增观测字段 |
| `answer_reserve_ms` | `int64` | deadline controller | 本次 Run 为最终答案保留的毫秒数 | profile 默认 | 新增观测字段 |
| `termination_reason` | `string` | Run/QA result | `completed`、`deadline_exceeded`、`budget_exhausted`、`cancelled`、`provider_error` 等 | `unknown` | 旧状态读取兼容 |
| `fallback_used` | `bool` | execution/QA | 是否使用 deterministic fallback 或 partial fallback | `false` | 新增字段 |
| `answer_complete` | `bool` | QA result | 用户答案是否达到完整输出契约 | `false` | 新增字段 |
| `completeness` | `string` | workflow/result | `complete`、`partial`、`failed`、`unknown` | `unknown` | 复用既有完成度语义 |
| `soft_budget_exhausted` | `bool` | usage ledger | 是否在 headroom 内发生可保留的软超限 | `false` | 新增账本字段 |
| `context_tokens` | `int64` | execution | 本次调用发送前的估算/实际上下文规模 | `0` | 新增观测字段 |

终止语义：

```text
running
  ├─ 正常完成 → completed / answer_complete=true
  ├─ 有可用部分结果但资源耗尽 → partial / answer_complete=false
  ├─ deadline 到期且 fallback 可生成 → deadline_exceeded + fallback_used=true
  ├─ provider/工具不可恢复失败 → failed + termination_reason=<具体错误>
  └─ 显式用户取消 → cancelled
```

说明：这里的 `phase` 和上述结果字段用于描述已有执行生命周期，不引入新的持久化状态机。Child 是否完成、reservation 是否结算和 Run 是否终止仍由现有 Run facts、batch result 和 durable budget 派生；若实现需要增加 phase 字段，必须证明它表达的是实际不同的预算/取消/权限边界。

不变量：

1. `answer_complete=true` 时，不得同时存在 `fallback_used=true` 或未公开的 `termination_reason != completed`。
2. `termination_reason=deadline_exceeded` 时，必须保留原始 context deadline cause，并且 `answer_complete` 只能在契约校验确实通过时为 `true`。
3. `reported_usage` 不得被 `effective_usage` 覆盖；reasoning 不得从 total token/cost 中扣除。
4. Child report 的 `report_id`、evidence ID 和完整度必须可追溯到权威 trace/artifact。
5. Child batch 未 settle 时，Parent 不得释放仍有 active reservation 的 Run lease，也不得伪造 `succeeded`。
6. `MaxReportTokens` 不得大于实际 generation cap 所允许的有效交付；生成 cap 必须在 provider 调用前生效。
7. 已 settled delegation 在正常流程中不得再次产生无界 `delegation_status` 或重复检索调用。

### 5.5 兼容、迁移与回滚

- **向后兼容：** 继续使用现有 `LogicalCompletionLimit` 作为内部逻辑字段；旧配置可映射到 `normal_qa` 或对应 role profile；旧 API 不要求立即暴露全部新字段。
- **数据迁移：** 不新增必须回填的业务数据表。Run/usage 新字段允许旧记录为 `unknown`，历史记录不通过永久 read-time cleanup 修复；如需要历史统计，使用一次性离线回放或 repair job。
- **灰度方式：** 按环境、租户或 feature flag 分阶段开启：先 shadow 记录 admission/pruning/projection，再开启动态 reserve、Child server-side await 和 phase cap。
- **回滚条件：** 固定评估集答案完整度显著下降、正常 QA 成功率下降、provider 错误率升高、P95/P99 延迟超过外层 SLA，或 partial 比例异常升高时回滚新 profile/feature flag。
- **回滚步骤：** 关闭动态 profile 和 phase cap 开关，保留原始 usage/termination 观测；恢复旧 projection 只作为短期应急，不恢复无界完整回填；修复后重新以 shadow 模式验证。
- **配置边界：** 已持久化的 platform settings 优先于代码默认值；改变默认值不能假设会覆盖数据库中的旧配置，必须显式迁移或在入口 canonicalize。

## 6. 修改伪代码

### 6.1 核心流程

```go
func RunQA(ctx context.Context, input QAInput) (QAResult, error) {
    normalized, err := NormalizeAndValidate(input)
    if err != nil {
        return QAResult{TerminationReason: "invalid_input", Completeness: "failed"}, err
    }

    profile := SelectBudgetProfile(normalized)
    budget := FreezeRunBudget(profile, normalized)
    runCtx, cancel := context.WithDeadline(ctx, budget.TotalDeadline)
    defer cancel()

    state := NewRunState(normalized, budget)
    ledger := NewRunLedger(budget)

    for state.CanExplore() {
        if state.HasPendingDelegation() {
            handoff, err := AwaitDelegation(
                runCtx,
                state.PendingDelegation(),
                budget.RemainingChildDeadline(),
            )
            if err != nil {
                state.RecordDelegationFailure(ClassifyDelegationError(err))
            } else {
                state.AttachDelegationProjection(ProjectHandoff(handoff))
            }
            state.CloseSettledDelegationTools()
        }

        phase := state.NextPhase()
        if !AdmissionAllows(phase, budget, state.ContextTokens()) {
            state.RecordBudgetStop(budget.ExhaustionReason(phase))
            break
        }

        callBudget := budget.ForPhase(phase)
        effectiveCap := callBudget.EffectiveProviderOutputCap(
            providerCapability(phase),
            safetyHeadroom(callBudget),
        )
        if effectiveCap <= 0 {
            state.RecordBudgetStop("provider_completion_tokens")
            break
        }

        tools := ToolsForPhase(state, phase)
        result, usage, err := CallModel(
            runCtx,
            state.Messages(),
            tools,
            effectiveCap,
        )
        ledger.Settle(usage, err)

        if err != nil {
            action := ClassifyFailure(err)
            switch action {
            case Retry:
                if budget.RetryAllowed(phase) && AdmissionAllows(phase, budget, state.ContextTokens()) {
                    state.RecordRetry(phase, err)
                    continue
                }
                state.RecordPartialOrFailure(phase, err)
            case Degrade:
                state.RecordPartial(phase, err)
            case Abort:
                state.RecordFailure(phase, err)
            }
            break
        }

        state.RecordModelResult(phase, result)

        if state.DelegationSettled() || state.AnswerReady() || budget.AnswerWindowReached() {
            break
        }
    }

    result := SynthesizeOrFallback(
        runCtx,
        state.EvidenceSummary(),
        state.UnresolvedGoals(),
        budget.FinalAnswerBudget(),
    )
    result = ValidateAndAnnotateTermination(result, state, ledger)
    PersistOutcome(runCtx, state, result)
    EmitRunMetrics(runCtx, state, ledger, result)
    return result, result.ErrorForCaller()
}
```

### 6.2 预算准入和动态 reserve

```go
func AdmissionAllows(phase Phase, budget RunBudget, contextTokens int64) bool {
    remaining := budget.RemainingWallClock()
    reserve := budget.DynamicAnswerReserve(contextTokens, phase)
    estimate := budget.P95Duration(phase) + budget.SettlementOverhead(phase)

    if phase == PhaseSynthesis {
        return remaining >= budget.MinimumSynthesisDuration()
    }
    return remaining > estimate+reserve
}

func DynamicAnswerReserve(contextTokens int64, phase Phase) time.Duration {
    compact := EstimateCompactionDuration(contextTokens)
    final := P95FinalSynthesisDuration(contextTokens, phase)
    validation := P95ContractValidationDuration()
    persistence := P95PersistenceDuration()
    configured := ProfileMinimumReserve()

    return maxDuration(configured, compact+final, final+validation+persistence)
}
```

### 6.3 Child 收口和有界 handoff

```go
func AwaitDelegation(
    ctx context.Context,
    delegation Delegation,
    childDeadline time.Time,
) (DelegationHandoff, error) {
    waitCtx, cancel := context.WithDeadline(ctx, childDeadline)
    defer cancel()

    batch, err := delegation.WaitUntilTerminal(waitCtx)
    if err != nil && !HasUsableCompletedTasks(batch) {
        return DelegationHandoff{}, err
    }

    handoff := DelegationHandoff{
        DelegationID: delegation.ID,
        Status:       NormalizeBatchStatus(batch),
        Reports:      make([]ReportSummary, 0, len(batch.Tasks)),
    }
    for _, task := range batch.Tasks {
        PersistAuthoritativeReport(task.Report)
        handoff.Reports = append(handoff.Reports, ProjectReportSummary(task.Report))
    }
    return handoff, nil
}

func ToolsForPhase(state State, phase Phase) []Tool {
    tools := SelectToolsByPhaseAndGoals(state, phase)
    if state.DelegationSettled() {
        tools = Remove(tools, "delegation_status", "delegate_investigation")
        tools = RemoveSatisfiedRetrievalTools(tools, state.EvidenceLedger())
    }
    return ApplyToolPruning(tools, state.ContextTokens(), state.RemainingBudget())
}
```

### 6.4 Token 结算和 partial 结果

```go
func SettleUsage(usage ProviderUsage, reservation Reservation) Settlement {
    reported := usage.ProviderOutputTokens
    available := reservation.AvailableOutputTokens
    headroom := maxInt64(256, available/20) // 初始实现需由 provider 回放校准

    switch {
    case reported <= available:
        return ExactSettlement(reported)
    case reported <= available+headroom && usage.ContentIsParseable:
        return PartialSettlement{
            Reported: reported,
            Effective: available,
            Reason: "soft_budget_exhausted",
        }
    default:
        return HardFailureSettlement{
            Reported: reported,
            Available: available,
            Reason: "provider_completion_tokens",
        }
    }
}
```

### 6.5 修改前后对比

修改前：

```go
// Parent / Child 多个阶段复用同一个宽上限；report 在生成后才裁剪。
result, err := agent.callModel(ctx, messages, tools, agent.cfg.AnswerMaxTokens)
report = boundReport(result, policy.MaxReportTokens)

// Child 完成后仍把状态工具交给模型决定是否继续。
tools = append(tools, delegationStatusTool, searchRunbooksTool)
```

修改后：

```go
phase := state.NextPhase()
if !AdmissionAllows(phase, budget, state.ContextTokens()) {
    return SynthesizeOrFallback(state, "budget_admission_denied")
}

cap := budget.ForPhase(phase).EffectiveProviderOutputCap(capability)
result, usage, err := agent.callModel(ctx, messages, ToolsForPhase(state, phase), cap)
settlement := SettleUsage(usage, reservation)
state.RecordSettlement(settlement)

if state.DelegationSettled() {
    state.CloseExplorationTools()
    return SynthesizeWithBoundedHandoff(state.DelegationSummaries())
}
```

### 6.6 配置或数据库变更

建议先增加配置 profile，不删除旧字段：

```yaml
qa:
  budget_profiles:
    normal_qa:
      timeout: 5m
      answer_reserve: 60s
      parent_max_steps: 4
      parent_tool_calls: 8
      parent_generation_cap: 8k
      synthesis_generation_cap: 8k
    deep_multi_agent:
      timeout: 8m
      answer_reserve: 90s
      parent_max_steps: 5
      parent_tool_calls: 10
      child_timeout: 150s
      child_max_steps: 3
      child_tool_calls: 8
      child_generation_cap: 8k
      child_report_cap: 4k
      verifier_generation_cap: 6k
      synthesis_generation_cap: 8k
      context_soft_limit: 48k
      context_hard_limit: 64k
      token_headroom: 512
```

配置语义：

- `generation_cap` 控制 provider 请求前的实际生成上限；
- `report_cap` 控制交付给上层的 projection 上限；
- `answer_reserve` 只用于最终答案阶段，不计入探索可用时间；
- `context_soft_limit` 触发 compact，`context_hard_limit` 拒绝新增上下文/工具调用；
- 旧 `LLMAnswerMaxTokens`、`AgentMaxSteps` 等字段先由入口 canonicalize 到 profile，禁止下游重复解释同一配置。

本提案不要求新增数据库表；如 durable budget 已有对应字段，优先扩展既有 schema/迁移和 store contract。任何 schema 变更必须同步 `docs/sql/`、canonical schema、迁移测试和受影响查询。

## 7. 预期的效果

### 7.1 功能效果

实施后：

1. Parent 在剩余时间不足以完成探索时，不再发起新的 Child、状态轮询或重复检索；
2. Child 全部 terminal 后，Parent 能在一次有界 handoff 后直接进入 synthesis；
3. Child 生成上限在 provider 请求前生效，`MaxReportTokens` 不再承担减少模型生成时间的错误职责；
4. Child 轻微 output 超限时保留可解析的 partial report，并向最终答案公开其不完整性；
5. deadline、provider error、tool error、steps/tool calls exhausted 和 total budget exhausted 能被明确区分；
6. deterministic fallback 仍可用于不可避免的 deadline 场景，但不再清除原始错误，也不再被标记成正常完整回答。

### 7.2 可观测性效果

新增或调整以下信号：

| 信号 | 类型 | 目标 |
| --- | --- | --- |
| `agent_model_call_duration_seconds` | Histogram | 按 provider/model/phase 统计 TTFT、总耗时和尾延迟 |
| `agent_model_usage_tokens` | Counter | 分开记录 input、cached input、provider output、reasoning、visible estimate、total |
| `agent_budget_remaining_seconds` | Gauge/日志字段 | 记录调用前后的剩余 wall-clock 和 answer reserve |
| `agent_budget_exhausted_total{dimension}` | Counter | 区分 `time`、`provider_completion_tokens`、`total_tokens`、`cost`、`steps`、`tool_calls`、`context` |
| `agent_delegation_settlement_seconds` | Histogram | 统计 Child batch 等待、排队和 settle 尾延迟 |
| `agent_post_settlement_tool_calls_total` | Counter | 目标为正常 managed delegation run 中为 0 |
| `agent_context_projection_chars` | Histogram | 统计 Parent/Child projection 大小和 compact 次数 |
| `agent_partial_result_total{reason}` | Counter | 统计 partial 的原因和来源 |
| `agent_termination_total{reason}` | Counter | 统一 Agent、QA 和评估侧的终止原因 |
| `answer_complete` | 持久化字段 | 区分完整答案和 fallback/partial |
| `completeness` | 持久化字段 | 表达 `complete`、`partial`、`failed` 或 `unknown` |

每次物理模型调用至少记录：

```text
run_id
agent_id
phase
provider
model
requested_output_cap
effective_output_cap
input_tokens
cached_input_tokens
tool_schema_tokens
reasoning_tokens
visible_output_tokens_estimated
provider_output_tokens
total_tokens
TTFT
total_latency
tool_latency
context_tokens
remaining_wall_clock
answer_reserve
remaining_token_budget
remaining_cost_budget
termination/settlement reason
```

日志不得记录完整 prompt、完整源码、密钥或敏感 payload；完整结果通过 trace/artifact ID 关联。

### 7.3 量化指标

| 指标 | 当前基线 | 目标值 | 统计窗口 | 数据来源 |
| --- | ---: | ---: | --- | --- |
| 原触发 Run 的最终答案 deadline cancellation | 1 次复现 | 固定回放不再在 synthesis 前被取消 | 每次提交/回放 | Run/QA 终态 |
| 正常 managed delegation 的 settled 后探索调用 | 已观察到非零 | `0` | 每个 delegation batch | execution trace |
| Child 因 1～几十 token 边界误差直接 hard failure | 已观察到 | `0`；可解析结果转为 partial | 每周/固定评估集 | usage ledger |
| Parent/Child phase 未配置实际 generation cap | Parent 24k、Child 复用全局值 | 所有 phase 均有 requested/effective cap | 每次模型调用 | model-call metrics |
| Answer reserve 被探索调用侵占 | 固定 30s、无门禁 | `0` 次越过动态 reserve 的新探索调用 | 每个 Run | admission logs |
| deadline timeout rate | 待采集基线 | 在相同任务集上较基线下降至少 50%，并低于业务 SLA | 50～100 次固定回放后 | QA/eval metrics |
| P95/P99 wall-clock | 待采集基线 | 在不降低答案完整度的前提下落入外层 SLA | 生产/评估窗口 | phase timing |
| 总 Token/Run | 本次约含 Child 218,427 tokens | 在固定任务集上较当前基线下降，目标先设为 20% 以上 | 50～100 次回放 | durable ledger |
| `delegation_status` 单次模型投影 | 本次约 20,544 chars | 只包含有界 summary；不超过 phase projection budget | 每个 handoff | trace |
| Agent/QA 终止语义不一致 | 本次存在 | 同一 Run 的 `termination_reason`、`fallback_used`、`answer_complete` 一致 | 每个 Run | Run/QA records |

在阶段 0 完成前，不将“20% Token 降幅”或固定 timeout 视为最终承诺；最终 profile 必须以至少 50～100 次固定数据集回放和线上 p95/p99 观测为准。

### 7.4 不应发生的变化

- 普通单 Agent QA 不因启用多 Agent治理而被强制使用深度调查的长 timeout 或更大上下文；
- provider 的 reasoning token 不从成本或总 Token 账本中剔除；
- Child 的完整权威报告不因 projection 限制而丢失，只是不再默认全部注入 Parent prompt；
- 异步 delegation 的传输契约不变；
- 用户看不到未验证的内部 claims、edges、budget metadata 或 NASUTA 内部 marker；
- 不新增针对单个服务、业务名称、trace ID 或用户问题文本的硬编码；
- 不以降低证据完整度、隐瞒 partial 或伪造 success 换取表面成功率。

## 8. 测试与验收

### 8.1 单元测试

- `DynamicAnswerReserve` 在不同 context size、phase 和 p95 duration 下返回不低于最小 reserve；
- 剩余时间不足时 admission gate 拒绝新工具/Child，允许进入 synthesis 或 fallback；
- Parent tool-use、Child investigation、verifier、synthesis 使用不同 effective output cap；
- provider capability 只映射一个合法 completion limit 字段；
- `MaxReportTokens` 不会影响 provider 已经生成的历史，但实际 generation cap 在请求前生效；
- `reported output=available+1` 且 report 可解析时返回 partial，不返回 hard failure；
- 超过 headroom、不可解析或 provider usage 缺失时返回明确 hard failure；
- `delegation_status` projection 只包含 summary、claims、evidence/report ID 和 gaps，不包含完整重复报告；
- Child terminal 后 tool snapshot 移除 `delegation_status`、`delegate_investigation` 和已满足目标对应的检索工具；
- `termination_reason`、`fallback_used`、`answer_complete`、`completeness` 的不变量在成功、partial、deadline 和取消路径成立；
- reported/effective usage、reasoning、visible estimate 和 total cost 的结算不互相覆盖。

### 8.2 集成测试

- 验证 QA 请求到 Parent、Child batch、handoff、synthesis 和 QA persistence 的完整链路；
- 注入一个慢 Child，验证 batch deadline 到期后其他 Child 结果仍能被汇总；
- 注入一个 Child provider error，验证 Parent 只将该 Child 标为 partial/failed，不丢失其他 Child；
- 模拟 Child 全部完成，验证 Parent 不再调用 `delegation_status` 或重复 `search_runbooks`；
- 模拟 Parent deadline 在 Child 尚未全部完成时到期，验证活动调用被取消或停止，结果不伪造为 completed；
- 验证 Run lease 在所有 Child reservation settle 前不会被释放；
- 验证上下文达到 soft/hard limit 时分别 compact 和拒绝新增交付；
- 验证 tool pruning 实际应用的工具集合与日志一致；
- 验证旧配置、已有 API 客户端和旧 Run 记录仍可读取。

### 8.3 回归场景

| 场景 | 输入 | 期望结果 | 验收方式 |
| --- | --- | --- | --- |
| 原触发案例 | RGB/消息中心/菜谱/TTS 多主题流程问题 | Child 结果一次性收口，最终答案在 deadline 内完成或明确 partial | 固定日志回放 + E2E |
| 单 Agent 正常路径 | 单服务、少工具事实问题 | 使用 `normal_qa` profile，行为和答案契约不回退 | 固定评估集 |
| Child 全部成功 | 2～4 个 Child | 0 次 settled 后 status/search，直接 synthesis | trace 断言 |
| Child 轻微超限 | output 仅超过 available 1～几十 token | partial report 保留，Parent 标注不完整 | usage 单测 + 集成测试 |
| Child 严重超限 | 超过 headroom 或报告不可解析 | 明确 hard failure，其他结果继续汇总 | 故障注入 |
| 大上下文 | 多轮工具结果接近 context soft limit | compact、去重、保留 evidence ID 和目标 | context fixture |
| deadline | 最终阶段前注入慢 provider | 不再开始新的探索，返回 partial/fallback，终态一致 | fake clock/provider |
| 显式取消 | 用户/调用方 abort | 活动模型/工具调用收到取消，持久化为 cancelled | 并发集成测试 |
| provider wire contract | 不同 provider/API capability | 只发送合法字段，usage 解释与 provider 一致 | 请求体 contract test |

### 8.4 验收标准

提案视为完成，必须同时满足：

1. 原触发案例固定回放不再出现 synthesis 前因无界探索导致的 deadline cancellation；若人为缩短 deadline 仍无法完整回答，系统必须返回真实 partial，而不是伪装 success。
2. 所有 Parent/Child/Verifier/Synthesis 物理模型调用都记录 `phase`、requested/effective cap、usage、timing 和剩余预算。
3. Child 全部 terminal 后，正常 managed delegation 路径不再产生新的 `delegation_status`、`delegate_investigation` 或重复检索调用。
4. Child generation cap 在 provider 请求前生效，report projection 不再被当作生成时间控制。
5. 1-token 边界误差不再导致可解析 Child report 整体 hard failure；partial 状态可被 Parent 和最终答案识别。
6. Agent、QA、Run store 和评估调用方对 `termination_reason`、`fallback_used`、`answer_complete` 和 `completeness` 的表达一致。
7. 在固定任务集上，deadline timeout rate、P95/P99 wall-clock、总 Token、Child failure rate 和答案完整度均有可比较的前后基线。
8. 普通 QA 不因深度 profile 引入不可接受的延迟、成本或答案质量回退。
9. `GOWORK=off go test ./...`、`GOWORK=off go vet ./...` 和必要的 race/integration 测试通过；变更文档通过 `git diff --check`。

## 9. 风险与控制

| 风险 | 触发条件 | 影响 | 控制措施 | 回滚条件 |
| --- | --- | --- | --- | --- |
| generation cap 过小 | reasoning-heavy provider 长尾，visible content 尚未产生 | 答案/报告质量下降 | 先 shadow，按 phase 记录 finish reason；保留一次受控 continuation | partial 比例或质量回退超过阈值 |
| reserve 过大 | 过多时间被最终阶段保留，探索被过早截断 | 证据覆盖下降 | 使用 p95/p99 校准，区分 normal/deep profile | coverage/完整度显著下降 |
| reserve 过小 | 最终 synthesis 仍被取消 | 超时率不降 | 动态 reserve 加最小硬门禁，禁止越过 reserve 探索 | 固定回放仍在 synthesis 前失败 |
| Child await 增加阻塞 | batch 等待没有正确受 deadline 约束 | Parent 长时间等待 | await 仅在服务端执行，绑定 Child/batch deadline，支持取消 | P99 等待超过 SLA |
| projection 过度压缩 | summary 丢掉合成所需证据 | 最终答案不完整 | 保留 report/evidence ID 和 unresolved gaps；权威报告可按 ID 取回 | evidence coverage 下降 |
| soft overrun 被滥用 | 大量严重超限被误判 partial | 成本和延迟继续膨胀 | headroom 小且可配置，记录 soft/hard 分布 | soft overrun rate 异常 |
| pruning 误删工具 | phase 判断错误 | Agent 无法完成必要调查 | 工具准入 contract test，保留最小必需集合 | 任务成功率下降 |
| 终态字段迁移不完整 | Agent/QA/store 各自解释字段 | 监控和评估不一致 | 以 Run result/ledger 为事实源，增加跨层集成测试 | 同一 Run 出现矛盾终态 |
| provider usage 语义差异 | reasoning 是否计入 output 不同 | 预算误判、成本统计错误 | capability profile、reported/effective usage 分离 | provider contract test 失败 |
| 只调高并发 | Child/LLM/工具共享资源争抢 | P99 和失败率上升 | 并发保持有界，按真实 provider/tool p95 调整 | 下游限流或尾延迟恶化 |

## 10. 实施计划

### 阶段 1：观测和语义冻结

- 为每次物理模型调用补齐 phase、requested/effective cap、usage、timing、context 和剩余预算；
- 统一 `termination_reason`、`fallback_used`、`answer_complete`、`completeness` 的字段语义；
- 基于固定日志/评估集采集至少 50 次回放基线；
- 退出条件：可以区分 time/token/context/tool/step/cost 预算，且 Agent 与 QA 的终态一致。

### 阶段 2：P0 止血

- 动态 answer reserve 和剩余时间 admission gate；
- Child 全部 terminal 后服务端强制 handoff/synthesis；
- 禁止 settled 后的 `delegation_status` 和重复检索；
- generation cap 与 report cap 分离，并在 provider 请求前生效；
- 预算结算增加 headroom 和 partial report；
- 退出条件：原触发回放不再在 synthesis 前被无界探索拖到 deadline，且 Child 轻微超限可保留 partial。

### 阶段 3：上下文和工具治理

- delegation handoff 改为 summary/evidence ID projection；
- Child 输入按任务和 evidence scope 投影，达到 soft limit 主动 compact；
- 启用 phase-aware tool pruning；
- 建立同一 run 的 evidence dedup 和重复检索门禁；
- 退出条件：上下文大小、单次 handoff 大小和 settled 后工具调用达到验收门禁。

### 阶段 4：预算 profile 与灰度

- 上线 `normal_qa`、`deep_multi_agent`、`deep_multi_agent_complex` profile；
- 通过 feature flag 按环境/租户/比例灰度；
- 比较 timeout rate、P95/P99、总 Token、Child failure、答案完整度和 evidence coverage；
- 退出条件：固定任务集质量不回退，生产分位数满足外层 SLA，且无新的终态不一致。

### 阶段 5：全面启用与清理

- 将已验证的 profile 默认值写入 canonical config 文档；
- 删除只做 dry-run 的 pruning 和旧的无界 projection 路径；
- 清理不再使用的重复配置解释和兼容分支；
- 将稳定合同归并到 `agent-platform/04-context-session-and-tool-results.zh-CN.md`、`agent-platform/08-observability-and-evaluation.zh-CN.md` 和相关 QA 设计文档；
- 退出条件：旧路径无调用者，固定评估集、race、integration 和回放测试全部通过。

## 11. 待决策事项

| 决策项 | 方案 A | 方案 B | 推荐方案 | 原因 |
| --- | --- | --- | --- | --- |
| Parent answer reserve | 固定 30s | 按 profile + p95 动态 reserve，初始 60～90s | B | 30s 已不足以覆盖大上下文 synthesis；动态值可按 workload 校准 |
| Parent tool-use cap | 继续复用 24k | tool-use 4k～8k，synthesis 4k～8k，repair 2k～4k | B | 工具决策不需要与最终答案共用宽上限 |
| Child generation/report | 生成上限和 report cap 共用 | generation 6k～8k，report 2k～4k | B | 只有调用前的 generation cap 才能减少实际 decode 时间 |
| Child 等待职责 | 交给模型轮询 `delegation_status` | 服务端 await，Parent 只接收一次 handoff | B | 模型不适合承担异步完成通知和收口控制 |
| Child 全部结束后的工具 | 继续暴露 status/search | 关闭探索工具，强制 synthesis | B | 防止 settled 后继续浪费 step、Token 和时间 |
| 轻微 output 超限 | 直接 hard failure | 有 headroom 时保留 partial | B | 1-token rounding 不应丢弃已生成的可用证据 |
| 预算 profile | 所有 QA 一组默认值 | normal/deep/complex 分 profile | B | 不同 workload 的 p95/p99 和证据需求差异明显 |
| 全局 token budget | 先从 720k 提高 | 先保持，补齐 phase/run ledger 后再按数据调整 | B | 本次硬失败是 wall-clock deadline，不是全局 Token 先耗尽 |
| 并发策略 | 无界提高 Child 并发 | 保持有界，按下游 p95/限流校准 | B | 并发降低理想 wall-clock，但会累加 Token、成本和资源竞争 |
| 新增持久化状态机 | 新增完整 phase 状态机 | 使用已有 Run facts 派生 phase/终态 | B | 本问题主要是准入和职责边界缺陷，不需要引入新的持久化 FSM |

## 12. 决策摘要

本提案建议：

1. 把本次故障定性为“wall-clock deadline 先耗尽，且由过宽单轮 cap、Child 收口缺失、上下文膨胀和固定 reserve 共同放大”，而不是简单的全局 Token 不足；
2. 优先实施动态 answer reserve、调用前 admission gate、phase-specific generation cap 和 Child 服务端收口；
3. 将 Child generation cap 与 report projection cap 分离，加入 256～512 token 或 5%～10% 的结算 headroom，并对轻微超限保留 partial；
4. 将 delegation handoff 收敛为摘要、证据 ID、报告 ID 和未解决缺口，Child settle 后关闭状态/重复检索工具；
5. 用统一 Run ledger 记录 provider output、reasoning、visible estimate、总 Token、时间、成本、steps 和 tool calls，保留 reported/effective usage 的差异；
6. 采用 normal/deep/complex 三类预算 profile，通过固定回放和 p95/p99 指标校准，不用一个“业内标准 token/秒”硬套全部任务；
7. 当 deadline、质量、成本或完整度指标超过回滚阈值时，关闭新 profile/feature flag，回到可观测的安全路径，而不是恢复无界上下文和伪成功。

## 附录 A：提案提交前检查清单

- [x] 背景足以让非原作者理解系统和改动动机；
- [x] 问题以“期望行为—实际行为—差异”描述；
- [x] 至少包含一个可复现的典型场景；
- [x] 已区分表面现象、直接原因和机制根因；
- [x] 修改方案明确了职责所有者和单一事实源；
- [x] 伪代码覆盖正常路径、失败路径和状态变化；
- [x] 预期效果包含可量化指标，而不只是定性描述；
- [x] 已说明兼容、迁移、灰度和回滚方案；
- [x] 测试可以覆盖原始触发案例和关键边界场景；
- [x] 未引入只针对单个案例的硬编码特例。
