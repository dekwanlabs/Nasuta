# 多 Agent 调查延迟与上下文收敛提案（历史版本）

状态：已被任务级上下文与证据所有权提案替代  
作者：Nasuta Agent Platform Team  
日期：2026-08-20  
关联事项：用户运行 trace `run_fd5c1d1029781f11cd80b76022a4559c`、`investigator-scoped-context-projection-proposal.zh-CN.md`、`qa-agent-context-budget-and-cancellation.zh-CN.md`  
目标版本：历史记录

> 注意：本文件最初把问题归因到角色级 token 预算，并建议缩小
> Investigator/Verifier/Synthesizer 的预算。该方向不是当前修复方案。
> 当前方案见 `task-scoped-evidence-ownership-proposal.zh-CN.md`，重点是任务分配、
> evidence owner、`InputRefs` 和 baseline evidence 保留。

## 1. 摘要

本提案解决多 Agent 调查工作流“并行了但仍然很慢、多个 Agent 重复推理、最终总结又把所有结果重新读一遍，以及总结失败后按原预算重跑”的问题。

大白话说，当前不是每个 Agent 只查一个小问题，而是多个 Agent 各自完整查资料、完整写报告，最后再让 Synthesizer 把所有报告重新理解一遍。若 Synthesizer 把 token 都花在不可见的 reasoning 上，没有输出可见答案，系统还会用同样大的预算再调用一次。并行只能减少等待总和，不能减少最慢节点的工作量，也不能消除 Synthesizer 的二次推理。

用户 trace 显示：一次运行从 `19:12:12` 左右开始，到 `19:17:05` 结束，约 293 秒；中间出现 `contextChars=46728`、`12000 reasoning tokens` 且没有可见内容，随后 forced conclusion 仍使用 `max_tokens=12000`。这证明上下文和推理预算已经明显放大，但不能据此声称 Provider 一定返回了硬性的 context-window error。

本提案先实施 P0：

```text
并行 Investigator
→ 每个角色拥有独立的输出、步骤和 continuation 预算
→ Synthesizer 使用自己的较小且明确的预算
→ forced conclusion 首次按正常预算执行
→ reasoning 截断或空响应时只允许一次小预算重试
→ 失败原因和预算边界保持可观测
```

后续 P1 再处理任务准入、按新增证据提前停止、Synthesizer 输入压缩和整体并行截止时间。提案不通过无限增加 token、并发数或重试次数来掩盖根因。

## 2. 背景

### 2.1 业务与技术背景

调查型 QA 需要同时回答代码实现、服务拓扑、内部文档、历史记忆和外部资料等不同 facet。系统将这些工作拆给多个 Investigator，再经过 verifier 和 Synthesizer 汇总：

```text
QA 请求
→ Query Plan / Task Graph
→ 多个 Investigator 并行
→ Investigator 报告和证据交接
→ Verifier 处理冲突
→ Synthesizer 生成最终答案
```

不同角色的职责本来就不同：

| 角色 | 主要职责 | 是否获取新证据 | 目标输出 |
| --- | --- | --- | --- |
| Code Investigator | 查代码、符号和调用路径 | 是 | 短的结构化调查报告 |
| Runtime Investigator | 查服务拓扑、依赖和入口 | 是 | 短的结构化调查报告 |
| Docs Investigator | 查运行手册和内部文档 | 是 | 短的结构化调查报告 |
| Web Investigator | 查配置的外部来源 | 是 | 短的结构化调查报告 |
| Memory Investigator | 评估已准入的历史证据 | 否 | 短的结构化调查报告 |
| Verifier | 只处理已有证据中的冲突 | 否 | 有限的冲突结论 |
| Synthesizer | 汇总已验证的报告 | 否 | 面向用户的最终答案 |

### 2.2 当前实现

相关实现主要位于：

- `internal/agent/catalog/defaults_investigation.go`：创建 Investigator、Verifier 和 Synthesizer 的定义；
- `internal/agent/definition/run.go`：把定义预算接入共享执行循环；
- `internal/agent/execution/loop.go`：保存执行循环和结论生成预算；
- `internal/agent/execution/answer_generation.go`：执行 forced conclusion、continuation 和答案修复；
- `app/investigation_budget.go`：根据不可变 Agent Definition 计算工作流节点预算；
- `internal/agent/workflow/`：并行调度、handoff、验证和汇总。

当前关键行为是：

1. 多个 Investigator 并行执行，但各角色默认共享 `LLMAnswerMaxTokens` 和 `AgentMaxSteps`；
2. 每个 Investigator 可能进行多轮工具调用和完整报告生成；
3. Synthesizer 在 Investigator 完成后再次读取 handoff、证据和目标，重新进行一轮完整推理；
4. 主循环耗尽或异常结束后，forced conclusion 首次使用 `ConclusionMaxTokens`；
5. 若首次 forced conclusion 因 reasoning 截断或空响应失败，第二次仍使用同一个 `ConclusionMaxTokens`；
6. 协议修复、答案契约修复等后续路径可能继续增加模型调用。

### 2.3 为什么现在需要修改

本次修改由用户提供的运行 trace 触发：

- 触发时间：2026-08-20 19:12:12～19:17:05（Asia/Shanghai）；
- 触发运行：`run_fd5c1d1029781f11cd80b76022a4559c`；
- 直接表现：运行耗时约 293 秒，最终输出虽然存在，但中间发生长时间等待、上下文增长和 reasoning 耗尽；
- 关键日志：

```text
19:12:13 request provider=openai model=deepseek-v4-flash max_tokens=12000 messages=7 tools=3
19:14:32 request compiled ... messages=5 contextChars=46728
19:15:54 empty visible content: 12000 reasoning tokens, finish_reason=length
19:15:54 final-answer generation produced no visible content; forcing conclusion
19:15:54 force conclusion request ... max_tokens=12000 messages=6 tools=0
19:17:05 run end ... steps=1 answerLen=24037
```

### 2.4 范围与非目标

#### 目标

1. 让 Investigator、Verifier、Synthesizer 使用角色级预算，而不是所有角色共享回答预算；
2. 让 forced conclusion 的失败重试使用独立的小预算，最多重试一次；
3. 保证现有正常结论、协议修复和答案契约行为不被无关改动；
4. 用测试和定义快照验证预算边界，避免预算再次无意放大。

#### 非目标

1. 本提案不重新设计已有的 Investigator scoped context projection；
2. 本提案不改变检索排序、工具权限、Provider 选择或证据内容；
3. 本提案不通过增加全局 token、Agent 数量、并发度或重试次数解决延迟；
4. 本提案不针对某个问题、服务名、实体名或 trace ID 写特殊分支；
5. 本提案不新建持久化状态机或第二套 token ledger。

## 3. 问题

### 3.1 问题描述

**期望行为：**

每个调查角色只为自己的职责消耗有限预算。短报告的 Investigator 不应拿到最终回答级别的输出预算；Verifier 和 Synthesizer 不应继承 Investigator 的全部步骤预算。结论失败时，系统应使用有限的小预算快速尝试收口，不能再次启动一个同等规模的完整 reasoning 过程。

**实际行为：**

角色职责虽然不同，预算却基本相同；多个角色各自完成重型调查后，Synthesizer 又对全部 handoff 进行二次推理。首次 forced conclusion 没有可见输出后，系统再次用 `12000` token 预算调用模型。

**差异：**

任务已经拆分，资源没有随任务拆分。结果是“工作并行了，重复推理没有减少”；失败恢复还会把一次预算不足放大成第二次同等规模的调用。

### 3.2 根因分析

| 层次 | 说明 | 证据 |
| --- | --- | --- |
| 表面现象 | 延迟高、上下文变大、模型耗尽 reasoning 后没有可见答案 | trace 中 `contextChars=46728`、`12000 reasoning tokens`、约 293 秒 |
| 直接原因 | Investigation 角色共享 `LLMAnswerMaxTokens`/`AgentMaxSteps`；forced conclusion retry 继续使用 `ConclusionMaxTokens` | `defaults_investigation.go`、`definition/run.go`、`answer_generation.go` |
| 机制根因 | 预算模型按“Agent 类型”配置，没有按“任务职责和恢复阶段”分层；重试路径没有独立的资源上限 | 当前 Definition 和 execution Config 契约 |

根因链路：

```text
多个 Investigator 并行
→ 每个角色都可以做接近完整回答级别的推理
→ 等待最慢 Investigator
→ Synthesizer 重新读取全部 handoff 并完整推理
→ reasoning token 用尽，没有可见答案
→ forced conclusion 用同等预算再次调用
→ 延迟、成本和上下文压力继续放大
```

本问题不能只通过增大上下文窗口、提高全局 token、增加并发或增加重试解决，因为这些方式只提高上限或重复执行次数，不能减少重复信息、重复推理和最慢节点的工作量。

### 3.3 影响

- **用户影响：** 复杂问题需要等待很久，答案可能在超时边缘才出现；
- **系统影响：** Provider 输入、输出和 reasoning 成本增加，多个并行节点争抢同一运行时间；
- **可靠性影响：** Synthesizer 或 forced conclusion 失败时，后续恢复可能再次耗尽剩余时间；
- **工程影响：** 从 trace 很难判断时间花在调查、汇总还是恢复重试上，预算边界也容易被默认值悄悄放大。

## 4. 问题出现的场景

### 4.1 典型场景

#### 场景 A：多个角色各自完整调查

- **Given：** 一个问题同时需要代码、拓扑、文档和外部资料，系统创建多个 Investigator；
- **When：** Investigator 使用工具检索并生成报告；
- **Then：** 每个角色应在自己的短预算内完成最小充分证据；
- **But：** 当前每个角色共享全局回答级 token 和步骤预算，最慢角色可能持续做无边界的额外检索。

#### 场景 B：Synthesizer 二次重推理

- **Given：** Investigator 已经产出 handoff、证据和结构化报告；
- **When：** Synthesizer 接收完整汇总输入；
- **Then：** Synthesizer 应只做有限的证据归并和答案表达；
- **But：** 当前仍可能用与 Investigator 相同的模型输出预算，对全部材料重新推理。

#### 场景 C：forced conclusion 同预算重试

- **Given：** 主循环结束，第一次结论调用使用 `ConclusionMaxTokens`；
- **When：** Provider 返回 `finish_reason=length` 且没有可见内容，或正常结束但没有可见内容；
- **Then：** 系统应最多再进行一次小预算、明确要求直接作答的尝试；
- **But：** 当前第二次调用仍使用 `ConclusionMaxTokens`，可能重复消耗大块 reasoning 预算。

### 4.2 边界场景

| 场景 | 当前行为 | 目标行为 |
| --- | --- | --- |
| 正常 forced conclusion | 使用正常结论预算，一次完成 | 保持不变 |
| reasoning 截断 | 同预算重试 | 仅一次小预算重试 |
| 空模型响应 | 同预算重试 | 仅一次小预算重试 |
| 协议泄漏 | 执行协议修复 | 保持现有行为 |
| 答案契约不满足 | 执行已有契约修复 | 保持现有行为 |
| 全局回答预算较小 | 角色预算可能超过合理职责 | 各角色预算不超过全局回答预算 |
| Investigator 没有工具 | 仍继承完整 Agent 步骤预算 | 仍使用角色上限，实际由一次输出自然结束 |

### 4.3 复现步骤

1. 配置一个支持 reasoning 的模型，并将回答预算设置为较大值；
2. 提交同时需要多个证据 facet 的调查型 QA；
3. 观察多个 Investigator 的模型调用和最慢节点结束时间；
4. 观察 Synthesizer 是否重新接收完整 handoff；
5. 让一次结论调用以 `finish_reason=length` 且无可见内容结束；
6. 对比 forced conclusion 两次请求的 `max_tokens`、耗时和 reasoning token。

## 5. 如何修改

### 5.1 修改原则

1. **按职责分配预算。** Investigator 负责找证据和写短报告，Verifier 负责裁决，Synthesizer 负责收敛和表达；
2. **恢复预算独立。** 首次结论和失败后的直接收口不是同一种任务，不能共享同一个大预算；
3. **保持单一事实源。** Agent Definition 继续拥有调查角色的预算事实，工作流预算继续从 Definition 派生；
4. **失败可诊断。** 保留 `ErrReasoningTruncated`/`ErrEmptyModelResponse` 分类，并记录进入小预算重试；
5. **不增加总调用次数。** 本轮只改变角色预算和重试预算，不增加默认重试次数。

### 5.2 目标流程

```text
Definition Catalog
→ 为 Investigator / Verifier / Synthesizer 生成角色级输出和步骤预算
→ Workflow 从不可变 Definition 派生节点预算
→ Investigator 并行执行有限调查
→ Verifier 和 Synthesizer 使用更小的专属预算完成收敛
→ forced conclusion 首次正常预算
→ reasoning 截断或空响应时最多一次小预算直接作答
→ 记录真实终态和预算事件
```

### 5.3 详细改动

| 改动项 | 当前实现 | 修改后 | 涉及模块 | 兼容策略 |
| --- | --- | --- | --- | --- |
| 结论失败重试 | 第二次仍使用 `ConclusionMaxTokens` | 新增 `ConclusionRetryMaxTokens`，默认约为结论预算四分之一且不超过 1024 | `internal/agent/execution` | 未配置时由执行层派生 |
| Investigator 输出 | 直接使用 `LLMAnswerMaxTokens` | 按全局预算约 `1/4` 分配，最低 1024、最高 4096，并受全局回答预算约束 | `internal/agent/catalog` | 定义版本/hash 随定义变化 |
| Verifier 输出 | 直接使用 `LLMAnswerMaxTokens` | 按全局预算约 `1/6` 分配，最低 768、最高 3072，并受全局回答预算约束 | `internal/agent/catalog` | 定义版本/hash 随定义变化 |
| Synthesizer 输出 | 直接使用 `LLMAnswerMaxTokens` | 按全局预算约 `1/2` 分配，最低 2048、最高 6144，并受全局回答预算约束 | `internal/agent/catalog` | 定义版本/hash 随定义变化 |
| 角色步骤 | 全部使用 `AgentMaxSteps` | Investigator 最多 3 步，Verifier/Synthesizer 最多 1 步 | `internal/agent/catalog` | 小于全局值时取全局值 |
| continuation | Investigator 固定 1，Verifier/Synthesizer 跟随全局 | 各角色最多 1 轮 | `internal/agent/catalog` | 全局值更小时取全局值 |

#### 改动一：forced conclusion 小预算重试

`Config.ConclusionMaxTokens` 继续控制第一次 forced conclusion。新增的
`Config.ConclusionRetryMaxTokens` 只用于 `ErrReasoningTruncated` 或
`ErrEmptyModelResponse` 的一次 no-reasoning 重试。默认值为：

```text
min(ConclusionMaxTokens, max(1, ConclusionMaxTokens / 4), 1024)
```

如果第二次仍失败，不再因为同一原因继续重试；协议修复和答案契约修复保留现有路径。

#### 改动二：调查角色预算分层

`DefaultInvestigators` 继续生成不可变 Definition，但按角色设置输出预算：

- Investigator：按全局预算的约四分之一分配，最低 1024，最高 4096，最多 3 步；
- Verifier：按全局预算的约六分之一分配，最低 768，最高 3072，最多 1 步；
- Synthesizer：按全局预算的一半分配，最低 2048，最高 6144，最多 1 步；
- 所有角色的实际值不超过 `settings.LLMAnswerMaxTokens` 和 `settings.AgentMaxSteps`；
- continuation 最多 1 轮，不创建新的全局配置字段。

最低值只在全局预算足够时生效，最终值仍会取
`min(计算值, settings.LLMAnswerMaxTokens)`。例如全局预算为 `512` 时，
三个角色都最多使用 `512`；全局预算为 `12000` 时，实际值分别为
`3000`、`2000` 和 `6000`。这样保留了 `2048` 配置下的原有行为，
同时避免高预算环境把调查报告压缩到过小的输出窗口。

### 5.4 数据结构或接口契约

新增执行层字段：

| 字段 | 类型 | 所有者 | 含义 | 默认值 | 兼容性 |
| --- | --- | --- | --- | --- | --- |
| `ConclusionRetryMaxTokens` | `int` | `execution.Config` | forced conclusion 的一次小预算重试上限 | 由 `ConclusionMaxTokens` 派生 | 旧调用方不填时自动派生 |

不变量：

1. `ConclusionRetryMaxTokens > 0` 时不大于 `ConclusionMaxTokens`；
2. Investigator/Verifier/Synthesizer 的角色预算不超过全局回答预算；
3. forced conclusion 的 no-reasoning retry 最多发生一次；
4. 首次正常结论、协议修复和答案契约修复不改变原有预算语义。

### 5.5 兼容、迁移与回滚

- **向后兼容：** `ConclusionRetryMaxTokens` 是执行层内部可选字段，旧调用点继续使用默认派生；
- **数据迁移：** 不涉及持久化数据和数据库 schema；
- **灰度方式：** 先通过单元测试和固定 trace 回放观察，角色 Definition 版本/hash 自动反映定义变化；
- **回滚条件：** 若调查报告普遍因输出预算不足而无法形成结构化结果，回调角色上限，但仍保留 forced conclusion 的独立 retry budget；
- **回滚步骤：** 回退角色预算常量或恢复上一版定义，避免修改运行时状态和历史记录。

## 6. 修改伪代码

### 6.1 结论生成

```go
func (a *Agent) forceConclusion(ctx context.Context, messages []Message) Result {
    result, err := generate(messages, a.cfg.ConclusionMaxTokens)

    if errors.Is(err, ErrReasoningTruncated) ||
       errors.Is(err, ErrEmptyModelResponse) {
        messages = append(messages, noReasoningInstruction)
        result, err = generate(messages, a.cfg.ConclusionRetryMaxTokens)
    }

    // Existing protocol repair and answer-contract validation stay bounded
    // by their existing policy.
    return result, err
}
```

### 6.2 角色预算

```go
investigatorOutput := proportional(settings.LLMAnswerMaxTokens, 4, 1024, 4096)
verifierOutput := proportional(settings.LLMAnswerMaxTokens, 6, 768, 3072)
synthesizerOutput := proportional(settings.LLMAnswerMaxTokens, 2, 2048, 6144)

investigatorSteps := min(settings.AgentMaxSteps, 3)
singleStep := min(settings.AgentMaxSteps, 1)

investigator := Definition{
    Model:  ModelPolicy{MaxOutputTokens: investigatorOutput},
    Budget: BudgetPolicy{MaxSteps: investigatorSteps, MaxContinueRounds: 1},
}
verifier := Definition{
    Model:  ModelPolicy{MaxOutputTokens: verifierOutput},
    Budget: BudgetPolicy{MaxSteps: singleStep, MaxContinueRounds: 1},
}
synthesizer := Definition{
    Model:  ModelPolicy{MaxOutputTokens: synthesizerOutput},
    Budget: BudgetPolicy{MaxSteps: singleStep, MaxContinueRounds: 1},
}
```

## 7. 测试与验收

### 7.1 自动化测试

1. `withDefaults` 会在未配置时派生正数 retry budget，且不超过正常结论预算和 1024；
2. reasoning 截断后，第二次 HTTP 请求的 `max_tokens` 小于第一次；
3. no-reasoning retry 最多执行一次；
4. 正常 forced conclusion 不触发 retry；
5. Investigator、Verifier、Synthesizer 的定义输出和步骤预算分层，且不超过全局设置；
6. 现有协议修复、结构化 continuation 和答案契约测试继续通过。

### 7.2 验收指标

P0 验收至少满足：

- forced conclusion retry 的 `max_tokens` 不再等于正常结论预算；
- Investigator、Verifier、Synthesizer 的输出预算按角色比例分配，并分别受
  `4096`、`3072`、`6144` 的角色上限约束；
- Verifier 和 Synthesizer 不再与 Investigator 共享同一个输出/步骤预算；
- 默认调用次数不增加；
- 在相同 trace 输入下，失败恢复阶段不会再次消耗一个完整结论预算。

P1 需要另行验收：

- 多 Agent 运行的 p95/p99 延迟；
- 最慢 Investigator 等待占比；
- Synthesizer 输入 token 与 handoff 总量；
- 证据覆盖率、结构化报告成功率和最终答案完整度。

## 8. 预期的效果

### 8.1 直接效果

1. forced conclusion reasoning 截断后的重试从“再做一次完整推理”变成“用小预算直接收口”；
2. 调查 Agent 的输出和步骤预算与其职责匹配，减少短报告阶段的过度推理；
3. Verifier/Synthesizer 不再继承 Investigator 的资源规模；
4. 延迟和成本的主要放大器变得可见、可测、可调。

### 8.2 不保证的效果

本 P0 不保证所有多 Agent 请求立即达到目标 p95，也不保证 Synthesizer 不再读取大量 handoff。后两项分别需要 P1 的整体截止时间和输入收敛机制。P0 的职责是先阻止预算配置和失败恢复继续把问题放大。

### 8.3 后续 P1

1. 按任务新增证据和未满足 goal 设置 Investigator 提前停止条件；
2. 在 Synthesizer 前使用 claim/evidence manifest，避免重复传递完整报告正文；
3. 为并行节点增加父工作流级剩余时间门禁，不能无限等待最慢节点；
4. 将每次调用的输入、输出、reasoning、retry 和等待时间纳入同一份运行观测；
5. 用固定评测集验证“延迟下降不能以证据覆盖下降为代价”。
