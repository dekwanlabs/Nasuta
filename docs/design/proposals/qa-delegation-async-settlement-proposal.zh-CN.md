# QA 委派异步收口与预算治理提案

> 状态：提案，待评审
> 创建日期：2026-09-04
> 最新变更：`MaxConcurrency` 两层统一改为 6；子 Agent 暂不使用 fast model，按本提案修复 ①②③④
> 范围：QA 父 Agent 的 `delegate_investigation` 编排链路、child Agent 生命周期、父 loop 收尾、durable budget 租约释放
> 诊断来源：`logs/all-2026-09-04.log` 请求 `ee3af1f11011`（父 `run_bfb9053635ac82396acfe9c8`，委派批次 `del_37fd3362a17959be27a0d02e`）
> 关联提案：`qa-delegation-async-partial-answer-proposal.zh-CN.md`（2026-09-03，异步派发 + partial answer 的基线）

## 0. 一句话结论

异步派发本身是对的，问题不在"要不要异步"，而在**"子任务跑完后的完成通知 / 等待"被错误地设计成了模型的职责**。
父 Agent 委派后没有别的合法活可干，只能靠 `delegation_status` 一遍遍轮询，把自己 `maxSteps` 烧穿，
又在子任务还没 settle 时就 `Finish`，触发 durable budget 租约释放报错。

修复方向：**保持异步派发（传输层必须），把"等完成"从模型手里收回到服务端（loop 层确定性 await），
模型看到的始终是"委派 → 等结果 → 写 synthesis"的同步闭环。** 顺带取消 flow 子任务的单独压浅预算。

## 1. 本次故障事实

一次 flow 类 QA（"帮我分析一下 rgb 灯效、消息中心、菜谱、tts 这几个业务的流程"），父 Agent 拆成 4 个子任务后出现三个相互关联的问题：

1. 父 Agent 委派后连续调用 6 次 `delegation_status`，每次返回都是 `status:"running"`，零收获，把 `maxSteps=8` 烧穿，被迫 `forcing conclusion`。
2. 父 Agent 在子任务还没全部 settle 前就 `run.Finish(nil)`，报错：

   ```text
   [qa] finish run run_bfb9053635ac82396acfe9c8: release durable budget lease for run "...": durable budget lease still has active reservations
   ```

3. flow 子任务被单独压浅预算，TTS 子任务 `finish_reason=length`、`output_tokens requested=8000 available=0`，输出 token 全部烧在推理上，可见内容为 0，最终空报告。

时间线：

| 时间 | 事件 |
|------|------|
| 15:25:17 | 父 Agent 启动，`maxSteps=8`，总超时约 4m43s |
| 15:25:49 | 委派 4 个子任务（RGB / 消息中心 / 菜谱 / TTS） |
| 15:25:50/50/51 | RGB / 消息中心 / 菜谱 三个子 Agent 同时启动 |
| 15:25:51~15:25:59 | 父 Agent 连打 6 次 `delegation_status`，全部 `running` |
| 15:25:59 | 父 Agent `maxSteps` 耗尽，forcing conclusion |
| 15:26:48/50/53 | 三个子任务 settle（各 52~63s） |
| 15:26:49 | TTS 子 Agent 才启动（在 capability slot 排队约 59s） |
| 15:27:07 | 父 Agent 生成完答案，`run.Finish(nil)` → 报错 |
| 15:27:58 | TTS settle（最后一步 63s，`finish_reason=length`），batch finished |

注意：**没有子 Agent 真正失败**（`batch finished tasks=4 completed=4 failed=0`）。问题全在父 Agent 的"收口"。

## 2. 根因

### 2.1 异步只做了一半

`delegate_investigation` 走 `Dispatch`，工具调用秒回 `running`，子任务在后台 goroutine / 队列里跑——这一步是对的，
也必须异步（否则单个工具调用挂几十秒，MCP/HTTP 会超时，模型 turn 也会被卡死）。

问题在于**派发之后"跑完了"这件事没人接手**：`dispatchInBackground` 起个 goroutine 跑完只打一条日志（`batch finished`）就散了，
没有任何机制回头通知父 loop。父 loop 于是把控制权交回模型。

而委派契约明确写着：父 Agent 不要自己检索这些 subject、不要自己写深挖、深挖交给子 Agent。也就是说
**父 Agent 在这个等待窗口里合法能做的事 = 0**。它手里唯一能用的工具就是 `delegation_status`，而这个工具
`never blocks`、永远返回 `running`。于是模型只能一遍遍轮询，烧光 step。

结论：**把"等子任务完成"设计成了模型的职责，而模型没有别的事可干，只能当"催命鬼"。**

### 2.2 父 loop 收尾没有等子任务闭合

`runCompiled` → `runTurns` 跑完就 `finishLoop`，`qa/submission.go` 随后 `run.Finish(nil)`。
全程没有任何一处保证"子任务的 reservation 已经闭合"。父 Agent 只要自己写完了答案就 Finish，而子任务可能还在跑。

### 2.3 flow 单独预算把子任务压死了

`executor.go` 里 `childBudget` 对 `OutputContract.Kind == "flow"` 做了压缩
（`turns≤2 / toolCalls≤6 / output≤8000 / report≤2000`）。flow 子任务 token 预算过小，输出全部被 reasoning 吃掉，
可见报告一个字都出不来。

## 3. 设计决策（已确认）

### 3.1 保持异步派发，不做成阻塞

`delegate_investigation` 这个工具调用**继续异步**，秒回 `running`。不把工具改成阻塞等待。

理由：传输层必须异步，否则一个工具调用挂几十秒会把请求路径和模型 turn 一起卡死。异步本身是对的，
缺的不是"同步化工具"，而是"异步的收口"。

### 3.2 把"等完成"从模型职责收回到服务端

异步只应该发生在模型看不见的地方；模型看到的仍然是"委派 → 等结果 → 写 synthesis"的**同步闭环**。

- **传输层保留异步**：工具秒回，子任务后台跑。
- **loop 层做服务端确定性 await**：委派这个 turn 结束、下一步模型调用开始之前，服务端自己去等这批子任务 settle，
  截止时间取父 Agent 剩余窗口（`answerDeadline - childAnswerDeadlineSafety`）。等齐后把已完成的报告一次性塞进下一轮模型上下文。
- 这段等待发生在**模型两次 turn 之间**，不占 step、不烧 token、不走模型。

### 3.3 不让父 Agent 在等待窗口做"补充检索"

评估过"允许父 Agent 在等待窗口做非深挖的补充检索"，**否决**。原因：

- 等待窗口墙钟时间免费，但 step / tool-call / token 预算不免费，检索会吃掉留给最终 synthesis 的预算。
- 会往 context 灌入与子报告重复、甚至冲突的证据，触发 conflict notice 与 context 压缩。
- 父 Agent 的下一步决策仍依赖"子任务是否完成"，只要依赖没斩断，轮询必然回归。
- 边界会从黑白变灰，模型会滑回"我也自己查查"，稀释委派架构价值。
- 当前 synthesis 契约极轻（每 subject 一行流程 + 最多 6 个 verified hop），等待窗口做检索的边际收益极低。

结论：**异步的意义 = 父 Agent 能并行做"和子任务不冲突、且不依赖子任务完成状态"的活；
而"等待窗口检索"恰好既冲突、又依赖完成状态、还烧最终答案预算，是收益最低、风险最高的那种活。**

### 3.4 不让主 Agent 自己认领一个深挖任务

评估过"拆 4 个任务，主 Agent 也领一个"，**否决**。业内三种形态：

- **Orchestrator-Worker（编排者不干活）**：最主流。主 Agent 只拆任务、派发、等齐后写 synthesis。
- **Map-Reduce**：Map 阶段所有 worker（包括原主 Agent）各领一个，Reduce 阶段**换独立角色**写合成。
- **Self-participating supervisor**：少见，只在"任务数 > worker 数"或"监督者有子领域专长"时用，且它下场的任务通常不参与自己之后的 synthesis 主笔。

否决理由：若让领任务的主 Agent 回头写 synthesis，会视角污染（自己那份报告最重、输出不均匀）+
预算打架（深挖一个 subject 吃 2 step + 大量推理 token，写完 synthesis 没预算）。本案例 4 个 flow 深挖、synthesis 极轻，
正确的形态是 Orchestrator-Worker：父 Agent 等齐 4 份报告直接合成，别下场深挖。

## 4. 修复方案

| # | 修复点 | 改哪 | 解决什么 |
|---|--------|------|----------|
| ① | 取消 flow 单独预算 | `internal/agent/delegation/executor.go` 删除 `childBudget` 中 `OutputContract.Kind == "flow"` 的压缩分支，及 4 个 `flow*` 常量 | 子任务不再被压浅，有足够 token 产出可见报告，杜绝"烧完预算空手回" |
| ② | 异步收口（服务端 await，不改阻塞） | 父 loop 委派后、下一步模型调用前，加服务端确定性 `await 完成`（复用 100ms ticker + deadline），完成后把报告一次性注入模型上下文 | 模型不再靠 `delegation_status` 轮询烧 step |
| ③ | 收尾前等待 settle | `finishLoop` / `qa/submission.go` 的 `run.Finish` 前复用同一 await 原语 | 释放租约时 reservation 已闭合，消掉 `still has active reservations` ERROR；父 Agent 拿到子任务产出 |
| ④（已定） | 同 capability 并发槽 + 委派 worker 数：`MaxConcurrency` 统一改成 6 | `internal/agent/catalog/defaults_capability.go` 的 `MaxConcurrency: 3 → 6`；`platform/config/platform.go` 的 `DefaultDelegationMaxConcurrent: 3 → 6`（及对应测试） | 4 个同 capability 子任务不再排队，真正并行，缩短总耗时（详见 §6.1 两层并发） |

①②③ 是必须做的（功能 bug），④ 是并发性能调优（本次一并定案为 6），不是 bug。

② 与 ③ 本质是同一个"等待 settle"原语：放在 loop 派发后即 ②（解决轮询浪费），放在收尾前即 ③（解决 ERROR）。

## 5. 关键代码位置

- 父 loop 收尾：`internal/agent/execution/loop_execution.go`
  - `finishLoop`（约 180 行）
  - `shouldForceConclusion`（约 192 行）
  - `mergeDelegatedFlows`（约 164 行）
- 父 loop 主流程：`internal/agent/execution/loop.go`
  - `runCompiled`（约 360~406 行）在 `runTurns` 之后调 `finishLoop`
- 租约释放与 ERROR：`internal/agent/definition/run.go`
  - `Finish`（约 306 行）
  - `releaseFinishLease`（约 395 行）
- 预算租约：`internal/agent/run/store_budget.go`
  - `ReleaseLease`（约 404 行）在仍有 `open/active` reservation 时返回 `budget.ErrLeaseHasReservations`
- 异步派发与轮询：`internal/agent/delegation/executor.go`
  - `Dispatch`（约 382 行）
  - `dispatchInBackground`（约 501 行，只打日志、无完成通知）
  - `waitForQueuedTask`（约 1145 行，已存在 ticker 100ms + deadline 模式，可复用）
  - `childBudget`（约 2358 行，flow 压缩分支在此）
  - flow 常量（约 50~53 行）
- 状态工具：`internal/agent/delegation/tool.go`
  - `StatusTool`（约 318 行，description 含 "it never blocks"）
- capability 并发：`internal/agent/catalog/defaults_capability.go`
  - 7 个 capability 在循环内统一赋 `MaxConcurrency`（约 141 行，本次 3 → 6）
- 委派策略并发（worker 数）：`platform/config/platform.go`
  - `DefaultDelegationMaxConcurrent`（约 30 行，本次 3 → 6）
  - `executor.go` 的 `runTasks` 用 `min(policy.MaxConcurrent, len(tasks))` 决定 worker 数（约 1426 行）
- 并发测试断言：`internal/agent/catalog/catalog_test.go`（约 388 行）与 `platform/config/platform_test.go`（约 91、350~370 行）

## 6. 关于"思考是否串行"的说明

四个子任务里 RGB / 消息中心 / 菜谱 三个是**并行思考**的（step1 时间戳 15:25:53/53/55、step2 时间戳 15:26:48/50/53 相互重叠，各 52~63s）。整体看起来"串行"的原因是：

1. 三个已启动子 Agent 每步都是 52~63s 的长推理，开始点略微错开，叠加后观感像"一个接一个"。
2. TTS 子任务在 capability slot 上排队约 59s（四个任务都命中同一个 `knowledge.service.trace`，其 `MaxConcurrency=3`，只有 3 个能同时跑，第 4 个排队）。

因此"思考本身在 3 个已启动子 Agent 之间是并行的"，不是真正串行。改 6 之后，同 capability 的 4 个子任务不再排队、能同时跑。

### 6.1 两层"并发"的区别（都改成 6）

代码里有**两个**叫 `MaxConcurrency` 的东西，卡点不同，这次一起改成 6：

| 层 | 位置 | 作用 | 本次改动 |
|----|------|------|----------|
| capability 并发槽（硬边界） | `internal/agent/catalog/defaults_capability.go` 约 141 行 | 每个 capability 版本一把信号量 `make(chan struct{}, MaxConcurrency)`，同一 capability 同时最多跑这么多子任务。本次 TTS 排队 59s 就是它卡在 3 | `MaxConcurrency: 3 → 6` |
| 委派策略 worker 数（上限） | `platform/config/platform.go` 约 30 行 `DefaultDelegationMaxConcurrent` | `runTasks` 里 `workers := min(policy.MaxConcurrent, len(tasks))`，决定一个批次最多开几个 worker goroutine | `DefaultDelegationMaxConcurrent: 3 → 6` |

两层是**取交集**的关系：worker 数决定最多同时跑几个，capability 槽决定同一 capability 还能不能再塞。只有两层都 ≥6，拆 4~6 个同 capability 子任务时才真正并行、不排队。改完后 `delegation_max_children = 6`、`delegation_max_concurrent = 6`，`MaxConcurrent ≤ MaxChildren` 的校验仍然成立。

**注意运行时来源**：`DefaultDelegationMaxConcurrent` 只是代码默认值；已配置实例实际读的是 MySQL 平台设置 `delegation_max_concurrent`（日志里就是 3）。改代码默认值不会覆盖已持久化的 3，需要连 DB 一起把 `delegation_max_concurrent` 更新为 6 才生效。

**测试同步**：`internal/agent/catalog/catalog_test.go`（约 388 行 `MaxConcurrency != 3 → != 6`）与 `platform/config/platform_test.go`（约 91 行跟随默认值自动适配；约 352 行的 `TestValidateAgentSettingsChecksDelegationRelationships` 需把 `delegation_max_children` 从 3 提到 6 才能继续通过 `MaxConcurrent ≤ MaxChildren` 校验）。

## 7. 落地顺序

### 第一批（止血）

1. 删除 `childBudget` 的 flow 压缩分支（修复 ①）。
2. 抽一个服务端 await 原语（复用 `waitForQueuedTask` 的 ticker + deadline 模式）。
3. 在 `finishLoop` / `Finish` 前插入 await，保证 reservation 闭合（修复 ③）。

### 第二批（异步收口）

4. 父 loop 委派后、下一步模型调用前插入 await，完成后把报告一次性回填进模型上下文（修复 ②）。
5. 收窄 `delegation_status` 的暴露：正常流程不让模型用它，只在超时降级兜底路径暴露，避免模型自由轮询。

### 第三批（并发调优，本次已定案）

6. `MaxConcurrency` 两层都改成 6（修复 ④）：`defaults_capability.go` 的 capability 槽 + `DefaultDelegationMaxConcurrent` 的 worker 数，并同步两处测试断言。**运行时若数据库已持久化 `delegation_max_concurrent=3`，需连 DB 一起更新为 6。** 上线后观察下游 Qdrant / LLM 的承载与配额。

## 8. 验收标准

- 场景 1：委派后父 Agent 不再出现连续 `delegation_status` 轮询，step 不被白烧。
- 场景 2：父 Agent 在子任务全部 settle 后才 `Finish`，不再出现 `durable budget lease still has active reservations`。
- 场景 3：flow 子任务（尤其长链路的 TTS 类）能产出可见报告，不再出现 `output_tokens requested=... available=0` 的空回答。
- 场景 4：父 Agent 委派后下一轮能直接拿到已完成的报告并写出 synthesis，模型对"异步 / running"无感知。
- 场景 5：拆 4~6 个同 capability 子任务时，4 个同时跑、无 capability 排队（日志里不再出现 TTS 那种 59s 的 slot 等待）。

## 9. 与既有提案的关系

本提案是 `qa-delegation-async-partial-answer-proposal.zh-CN.md`（2026-09-03）的**补充与收敛**，不推翻其"非阻塞派发 + partial answer + 分层 deadline"的基线方向：

- 既有提案解决了"派发不阻塞、child 慢不拖垮父 Agent、最终答案有兜底"。
- 本提案补上它未覆盖的两个具体缺口：**父 Agent 委派后无活可干导致的 `delegation_status` 轮询烧 step**，以及**父 Agent Finish 过早导致的租约释放报错**。
- 两者共同结论：异步派发保留，但"等完成"必须由服务端收口，模型看到的始终是同步闭环。
