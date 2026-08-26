# 多 Agent 工作流编排 —— 新手完全指南

> 本文是 18 号文档《Task-Driven Multi-Agent Architecture》的**白话翻译版**。
> 目的:让第一次接触这个项目的人,能从头到尾看懂「一堆 agent 是怎么被组织起来一起干活的」。
> 每一个环节都对应到真实代码文件,看完本文再去读源码,不会迷路。

---

## 第 0 章 一句话:这到底是个什么东西

你问一个问题,比如「这个服务的支付流程为什么会挂?」,系统不是让**一个** agent 吭哧吭哧查半天,而是:

1. 先判断:这个问题**能不能拆**成几件互不干扰的小事?
2. 能拆 → 派**好几个** agent 同时去查(一个查代码、一个查文档、一个查线上日志)。
3. 每个 agent 查完交「作业」,作业必须**附证据**。
4. 有个**不用 AI 的裁判**挨个核对:你说的话,证据到底在不在账本里?
5. 裁判通过了,才由一个「汇总 agent」把所有人的结论写成最终答案。

这套「拆活 → 派活 → 收活 → 验活 → 汇总」的机制,就叫 **workflow 编排(编排 = orchestration)**。

---

## 第 1 章 一个生活化的比喻:装修公司

把整个系统想象成一家装修公司接了一个活(用户的问题):

| 现实世界 | 本系统里的叫法 | 对应的 Go 文件 |
|---|---|---|
| 业主的需求(要装修) | 用户的问题 / QA 请求 | `internal/agent/qa/` |
| 项目经理判断「这活要不要分包」 | 路由(routing) | `internal/agent/qa/route.go` |
| 项目经理画「拆成哪几项活」 | 任务规划(task graph plan) | `internal/agent/qa/task_graph_plan.go` |
| 每项活该由哪个工种干(电工/木工/油漆) | 能力(capability) | `agent/capability.go` |
| 盖了章的施工图(谁先谁后、谁管谁) | 工作流定义(WorkflowDefinition) | `internal/agent/workflow/model.go` |
| 把草图审一遍、画成正式施工图 | 编译+校验(ProposalCompiler) | `proposal_validate.go` / `proposal_compile.go` |
| 工人进场,能并行的并行 | 执行内核(Orchestrator) | `executor_orchestration.go` |
| 工人交的每个结论都附小票/照片 | 证据(Evidence) | `internal/agent/workflow/evidence.go` |
| 监理(不看人情,只看票) | 验证器(Verifier) | `internal/agent/workflow/verifier.go` |
| 预算会计(多少钱、多少材料) | 预算账本(BudgetAccount) | `executor_budget.go` |

记住这个比喻,后面每个环节都套得上。

---

## 第 2 章 总览:一个请求的完整旅程

下面这张图是整个系统的主干。**请先花一分钟只看这张图**,有个整体印象,后面逐段展开。

```
用户问了一个问题
        │
        ▼
① 准备 QA(收集上下文、识别涉及的实体)
        │  internal/agent/qa/prepare.go
        ▼
② 决定走不走多 agent
        │  internal/agent/qa/route.go —— decideExecutionRoute
        │  (判断:有没有 ≥2 个能并行的独立任务?)
        ▼
③ 用 AI 规划「拆成哪几件活」
        │  internal/agent/qa/task_graph_plan.go —— planTaskGraph
        │  (AI 只准说:活叫什么、归哪个工种、要查哪几方面)
        ▼
④ 服务端把 AI 的草图编译成正式施工图
        │  internal/agent/workflow/proposal_validate.go (校验)
        │  internal/agent/workflow/proposal_compile.go (编译)
        │  model.go Prepare() (盖章 + 算哈希)
        ▼
⑤ 持久化 + 后台开始执行
        │  internal/agent/workflow/service_execution.go —— Start
        ▼
⑥ 执行内核:一轮一轮地派活(能并行的并行)
        │  internal/agent/workflow/executor_orchestration.go
        │  internal/agent/workflow/executor_attempt.go
        │  internal/agent/workflow/agent_node.go
        ▼
⑦ 把所有人的作业收上来,合并证据
        │  internal/agent/workflow/evidence.go / executor_handoff.go
        ▼
⑧ 监理(不用 AI)逐个核对证据
        │  internal/agent/workflow/verifier.go
        ▼
⑨ 风险闸门(有冲突/高风险就拦下)
        │  internal/agent/workflow/evidence_risk_gate.go
        ▼
⑩ 汇总 agent 写最终答案
        │  (synthesize 节点)
        ▼
把结果还给用户
```

**核心思想(贯穿全文,务必记住):**

> **AI 只负责「拆活」这一个环节;后面「怎么执行、怎么验证、预算多少、谁有权限」全是服务端写死的代码决定的。**
> 换句话说:模型出主意,服务器当法官,代码当执行者。模型想乱来(比如超预算、多要权限),服务器直接拒绝。

---

## 第 3 章 五个核心概念(用大白话)

在进入细节前,先把五个反复出现的名词讲清楚。

### 3.1 任务合同 TaskContract

一次调查要达成的**目标清单**。比如「查清楚支付流程为什么会挂」会被拆成几个**证据目标(EvidenceGoal)**:

- 目标 A:代码里支付流程的实现(来源 = 代码)
- 目标 B:支付相关的文档(来源 = 文档)
- 目标 C:线上报错日志(来源 = 运行时)

每个目标都写明「要从哪个来源查、要覆盖哪几个方面(facet)」。

> 文件:`internal/agent/qa/investigation.go` —— `TaskContract` / `EvidenceGoal` 类型。

### 3.2 能力 Capability

一个「工种」的完整说明书。它回答:

- 这个工种是干嘛的?(名称 + 描述)
- 它**实际调用哪个 agent 模型**(`Agent.ID` / `Agent.Version`,叫「钉死 pinned」)
- 它能用哪些工具(`ToolIDs`)
- 它**能读不能写**?(`SideEffectClass` = none 还是 write)
- 它最多能同时开几个工位(`MaxConcurrency`)
- 它需要什么权限(`PermissionScope`)

**关键:能力把「AI 能看到的抽象名字」和「真正要跑的 agent 定义」绑在一起。** 规划阶段 AI 只知道「有个叫 `knowledge.code.inspect` 的工种」,它不知道、也管不着这工种背后是哪个模型、能用什么工具——那些是服务端注册时定死的。

> 文件:`agent/capability.go` —— `Capability` 结构、`CapabilityRegistry`。
> 能力从哪来:`internal/agent/catalog/defaults_capability.go`(默认注册表)。

### 3.3 工作流定义 WorkflowDefinition

一张**盖了章、不可改的施工图**。一次运行就绑定一张图。它包含:

- 有哪些节点(`Nodes`):谁先谁后
- 节点之间的连线(`Edges`):谁依赖谁
- 总预算(`Budget`)
- 失败策略(`FailurePolicy`):一个工人没干成,是全体停工,还是继续?
- **一个哈希值(`ContentHash`)**:这张图的「指纹」,改一个字哈希就变

**为什么要有哈希?** 因为系统要「断电重启后接着干」。重启时它重新读施工图,如果哈希对不上,说明图被改过了,拒绝继续——保证**同一个活,从头到尾用同一张图**。

> 文件:`internal/agent/workflow/model.go` —— `WorkflowDefinition` / `Prepare()`。

### 3.4 证据 Evidence

工人(agent)交作业时,不能只说「我认为是这样」,必须说「我依据的是哪条证据」。每条证据都有个**身份证号(identity)**,由五样东西组成:

```
来源类型(source_kind) + 目标(target) + 小节(section) + 版本(version) + 时间范围(time_range)
```

比如:「代码文件 `payment.go` 第 45 行的那个函数、版本 abc123、某个时间段」。

**这套身份证号是整个系统的灵魂。** 它让系统能:

- 判断两个 agent 是不是交了**同一条**证据(去重)
- 判断两个 agent 对同一件事说了**相反**的话(冲突)
- 判断一个 agent 的结论**到底有没有证据支撑**(验证)

> 文件:`internal/agent/workflow/evidence.go`,证据的 key 定义在 `internal/evidence`。

### 3.5 预算 Budget

钱/材料不能无限花。预算分**六个维度**一起管:

1. 输入 token 数
2. 输出 token 数
3. 总 token 数
4. 工具调用次数
5. 花费(钱)
6. 重试次数

**关键机制:先预留、后结算。** 一个工人进场前,会计先「冻结」他可能用掉的最大额度,确认超没超;干完活,会计按实际用量「结算」,把多冻结的还回去。这样**并行的工人之间永远不会互相超卖预算**。

> 文件:`internal/agent/workflow/executor_budget.go` —— `workflowBudgetAccount`。

---

## 第 4 章 分步详解:每一步在哪个文件、干了什么

下面把第 2 章那张图,逐格放大。

### 4.1 准备 QA(prepare)

用户的问题进来后,系统先做「热身」:

- 识别问题里提到的**服务**、**实体**(canonical entities)
- 确定需要哪些**方面(facet)** 的证据

这些会成为 `TaskContract` 的原材料。

> 文件:`internal/agent/qa/prepare.go`。

### 4.2 路由:要不要走多 agent?(route)

这是**第一道分水岭**。函数 `decideExecutionRoute`(`route.go:149`)按顺序检查,任何一条不满足就直接「降级」成单个 agent:

| 检查项 | 不满足时的原因(降级原因) |
|---|---|
| 策略允许多 agent 吗? | `policy_disallows_multi_agent` |
| 用户是不是要求**写**东西(改代码)? | `write_requested` —— **写操作硬性降级**,多 agent 只读不写 |
| workflow 引擎可用吗? | `workflow_unavailable` |
| 独立任务数 ≥ 2，或证据合同本身可拆分吗? | `insufficient_independent_tasks` |
| 这些任务**能并行**吗? | `tasks_not_parallelizable` |

**判断「能不能拆」的依据,是任务结构本身**,不是「问题来自哪个来源」。具体看 `assessExecution`(`route.go:174`):它从 `TaskContract` 的证据目标,数出有几个「独立的能力」,再判断有没有「必须串行」的理由。

> 大白话:通常要有 ≥2 个互不干扰、能同时查的小问题才上多 agent；如果模型把一个宽问题错误合并成一个任务，但服务端已经确认它需要多个证据目标和多个独立能力覆盖，也会交给 workflow planner 再拆一次。写操作一律不让多 agent 碰。

### 4.3 AI 规划任务(task graph plan)

这一步**是唯一让 AI 参与规划的地方**,而且把 AI 的手脚绑得很死。

`planTaskGraph`(`task_graph_plan.go:107`)调用一个**快速模型**(fastLLM),给它一份**极窄的系统提示词**(`:16-26`),规则大致是:

- 每个允许的能力,**恰好**建一个任务,不多不少
- 任务要查的方面(facets),**必须原样复制**,不许自由发挥
- 任务之间**不许写依赖关系**(`depends_on` 必须为空)
- **禁止**自己加「汇总节点、工具、预算」这些字段

AI 输出后,`validateTaskGraphDraft`(`:218`)在服务端**再查一遍白名单**:

- 任务数量 == 允许能力数量(一一对应)
- 任务 ID 合法、不是保留字(`synthesize`、`evidence.join` 等)
- 能力在允许集合内、不重复
- 每个任务要查的方面 == 该能力能查的方面(完全一致)
- 依赖关系为空(当前是单轮,不搞多轮)

最后 `bindTaskGraphDraft`(`:284`)补上服务端定死的字段:`Optional=true`、`MaxAttempts=2`、输出是报告,并追加一个**唯一的 `synthesize` 汇总节点**。

**如果 AI 这一步出错了怎么办?** 直接回退到「确定性映射」(`investigation.go` 里的 `DelegatedInvestigationProposalForGoals`),按固定规则把目标映射到能力,不依赖 AI。**系统永远不会因为 AI 抽风而挂掉。**

### 4.4 编译 + 校验:把草图变成施工图

这是「模型出主意、服务器当法官」体现得最彻底的一环。

`ProposalCompiler.Compile` → 三步走:`validate` → `compile` → `Prepare`。

**第一步 validate(校验,`proposal_validate.go`)**:把 AI 的草图逐条审:

- 能力存在吗?启用了吗?
- 输出格式对吗?
- 要查的方面,是不是能力的子集?
- 权限是不是只少不多?
- 预算:AI 写的上限,**只能比服务端默认值更紧,不能放宽**(`tightenInt` / `tightenRatio`)
- 并行分组:同组的并发会不会超?会不会有**写冲突**?组内有没有依赖?
- 必须有且只有一个「终点」节点
- 每个必查目标(RequiredGoal)都得有任务覆盖

**第二步 compile(编译,`proposal_compile.go`)**:把草图落成正式的节点图:

- 每个任务 → 一个 `agent` 节点,钉死它的 agent 定义 + 能力 + 工具白名单
- 有多个前驱的目标 → 插入一个 `join`(合并)节点
- 配置了验证器 → 在合并后插入 `verifier` 节点
- 配置了风险闸门 → 再插一个 `gate` 节点
- 重新接好所有连线

**注意:join / verifier / gate 这些节点的 ID、格式、权限、超时,全部来自服务端的 `CompilationPolicy`,AI 一个都碰不到。**

**第三步 Prepare(盖章,`model.go:227`)**:做最终校验并**计算哈希**。哈希一旦算出,这张图就冻结了。

### 4.5 启动 + 持久化(service)

`Service.Start`(`service_execution.go:53`)做的事:

1. 先把「这次运行 + 输入」**写到数据库**(持久化,万一挂了能恢复)
2. 再 `go func()` 起一个**后台 goroutine** 去执行,用**脱离的 context**(detached context)

> 关键:**用户断开连接,后台任务继续跑**,不会半途而废。

### 4.6 执行内核:一轮一轮派活(executor)

这是整个系统**最核心的循环**,在 `executor_orchestration.go`。

**主循环的条件**(`:181`):

```go
for len(outputs)+len(failedOptional) < len(definition.Nodes) {
    // 只要「已完成的节点数 + 已失败的选做节点数」还没达到节点总数,就继续
}
```

每一轮:

1. **`readyNodes()`(`executor_handoff.go:32`)**:按拓扑顺序找「现在能开工」的节点。一个节点能开工,当且仅当它每个前驱:
   - 已经成功 → 通过
   - 是「失败的选做任务」,且这条边**不是必须的**(required)→ 放行
   - 还没干完也没失败 → 等下一轮

2. **`dispatchWave()`(`:480`)**:把这一批能开工的节点**同时**派出去(一个「波次」wave),两层限流:
   - 全局信号量 `MaxParallelism`:整个工作流最多同时开几个
   - 每个能力的并发限制器 `capabilityLimiters`(`:558`):同一个工种最多同时开几个工位

3. 每个节点在自己的 goroutine 里跑,完成/失败后把结果折回主循环。

**单个节点怎么跑?** `executeNode`(`:616`)→ 带重试的循环:

```go
for attempt := 1; attempt <= 节点最大尝试次数; attempt++ {
    执行一次(executeNodeAttempt)
    如果成功 或 不可重试 或 已经到头 → 返回
    否则 sleep 重试等待时间(backoff)
}
```

**单次尝试 `executeNodeAttemptUntraced`(`executor_attempt.go:36`)的骨架:**

```
1. 会计预留预算(Reserve)           —— 先冻结额度
2. 通知观察者「节点开始了」         —— 写日志/落库
3. 校验输入合法(validateNodeInputs)
4. 按节点类型分发(switch node.Kind)  —— 见第 5 章
5. 归一化输出(PrepareHandoff)
6. 会计结算(Settle)                 —— 按实际用量结账
7. 失败 → 判断能不能重试(retryableNodeFailure)
```

**哪些失败可以重试?**(`executor_attempt.go:119`)

- 只限 `agent` 节点(或标了 `RetrySafe` 的 transform)
- **排除**有写副作用的(副作用不能重做)
- **排除**「没预算」「预算耗尽」「被取消」「超时」这几种(重试也没用)
- 必须实现了 `Retryable()` 方法的错误才算

### 4.7 agent 节点:把活交给真正的 agent 模型(agent_node)

`AgentNodeExecutor.Execute`(`agent_node.go`):

1. 解析节点钉死的 agent 定义,**精确到 ID + Version**
2. 校验输入/输出格式兼容
3. **收窄权限**(`IntersectPermissions`)
4. 生成一个**子运行 ID**(child run)
5. 组装请求:
   - 上下文 = 前驱的作业(handoff)+ 任务指令(TaskDirective)
   - 工具白名单 = 能力声明的工具(`RestrictVisibleTools=true`)
   - 最大工具调用次数 = 节点预算
6. 调 `runtime.Run`,作为一个**子运行**去真正调 AI
7. 成功回来后:
   - 如果输出是调查报告,检查「覆盖的目标 + 未解决的目标」是否和要求的方面**完全对得上**(`investigationReportCompleteness`,`:209`)
   - 把子运行的证据合并进前驱

### 4.8 合并证据 + 度量收敛(join / evidence)

多个 agent 干完,他们的作业要先**合并**。

`aggregateHandoffs` → `joinHandoffs`(`executor_handoff.go:144`):

- 证据合并(`mergeHandoffEvidence`):**同身份证号去重,不同身份证号但说法冲突 → 记冲突**
- 完整度折叠规则:`不可用 > 部分 > 完整`(有一个不可用,整体就不可用)

然后 `measureEvidenceConvergence`(`evidence.go:102`)计算「收敛度」:

- 以**工作流输入时的基线(baseline)**为参照
- 数「新增的证据身份」有多少、重复提交的有多少、重复比例多大
- 这些数字是后面判断「没有新证据」「证据重复超标」的**事实依据**

### 4.9 监理:确定性验证(verifier)

`verifyInvestigationEvidence`(`verifier.go:177`)——**这一步完全不碰 AI,是纯代码的裁判**:

1. 把合并后的证据账本读进来
2. 如果配置了「冲突就拒绝」,且发现有冲突 → **直接拒绝**(`:201`)
3. 对每个 agent 的每条结论(finding),把它的证据引用**绑定(bind)到账本里的正式身份证号**:
   - 能精确对上 → **supported(有支撑)**
   - 部分对上 → **partial(部分支撑)**
   - 一条都对不上 → **unsupported(无支撑)**,踢出,不算数(`:254`)
4. 数「必查目标」被覆盖的情况:
   - 全被完整支撑 → **Complete(完整)**
   - 一个都没有 → **Unavailable(不可用)**
   - 介于中间 → **Partial(部分)**
5. 推导**停止原因(StopReason)**(`:444`):是「目标都查到了」?还是「没有新证据」?还是「证据重复超标」?还是「能力不可用」?

**为什么用代码不用 AI?** 因为「这句话有没有证据支撑」是**确定性的判断**(身份证号在不在账本里,非黑即白),不该交给会犯迷糊的 AI。这让验证结果**可复现、可审计、零幻觉**。

### 4.10 风险闸门(gate)

`EvidenceRiskGateEvaluator.Evaluate`(`evidence_risk_gate.go:23`):

- 如果发现**证据冲突**,或**高风险的目标只有部分支撑** → 决定 = `needs_clarification`(需要澄清)
- 这个决定会**拦住汇总**,不让它拿着残缺/冲突的证据硬写答案,转而走人工澄清。

### 4.11 汇总(synthesize)

走到最后,证据都验完了,才让**汇总 agent** 把 supported 的结论、partial 的结论、未解决的目标、局限(limitations)一起,写成最终答案。

> 汇总 agent 有硬性约束:它的最大工具调用次数必须是 **0**(它只能综合,不能再自己去查)。

---

## 第 5 章 六种节点类型(NodeKind)

节点是施工图里的「最小执行单元」。共六种,定义在 `model.go:21-30`:

| 类型 | 干什么 | 谁实现 |
|---|---|---|
| `agent` | 真正调一个 AI agent 去干活 | `agent_node.go` |
| `join` | 把多个前驱的作业合并成一个 | `executor_handoff.go` |
| `verifier` | 纯代码核对证据(不用 AI) | `verifier.go` |
| `gate` | 风险闸门,决定放行还是拦下 | `evidence_risk_gate.go` |
| `human_approval` | 需要人工点头,直接返回「等人」 | `executor_attempt.go` |
| `transform` | 纯数据变换(不调 AI) | `dispatcher.Execute` |

---

## 第 6 章 预算系统(为什么并行不会超卖)

`workflowBudgetAccount`(`executor_budget.go`)是一个**带锁的账本**,三个操作:

1. **Reserve(预留,`:17`)**:工人进场前,先冻结他「可能用掉的最大额度」。冻结时对六个维度逐个查「已用 + 已冻结 + 新增 ≤ 上限」,超了就返回 `ErrNoAffordableTask`。
   - 注意:这是**派发前**就发现的「没预算」,和干到一半才超支是两码事(前者 → `no_affordable_task`,后者 → `budget_exhausted`)。
2. **Settle(结算,`:52`)**:干完活,释放冻结、累加实际用量,再查一次「实际 ≤ 冻结」和「总量 ≤ 上限」。
3. **Release(释放,`:46`)**:失败了要回滚,把冻结的额度还回去。

因为所有并行工人共享同一个带锁账本,**预留时冻结 + 结算时核销**,所以无论开多少个并发,预算都绝不会被花超。

---

## 第 7 章 断电了怎么办:持久化与恢复

系统假设「随时可能挂」,所以每一步关键状态都落库,挂了能续上。

**启动时**:`Start` 先把运行和输入写库,再后台执行(见 4.5)。

**恢复时**(`service_recovery.go` + `service_checkpoint.go`):

1. `LoadFullRunState`:读完整的检查点
2. `takeOverRunningAttempts`(`:184`):把上次中断时还显示「运行中」的节点标记为失败;把 human_approval 标回「等人」;把可重试的标成「重启重试」,并**预留中断时已花的额度**
3. `workflowProgressFromState`(`service_checkpoint.go:11`):从库里的节点状态重建进度:
   - 成功的 → 记为已产出
   - 可重试失败且没到上限 → 重建「下一次尝试」的进度
   - 选做的失败 → 记为「可跳过的失败」
   - 其他 → 记为终态错误(不假装成功)
4. **校验施工图哈希没变**(`:153`),变了拒绝继续

---

## 第 8 章 权限:四层交集,只收窄不扩大

一个节点真正能用的权限,是**四层取交集**(`model.go:421` `IntersectPermissions`):

```
actor(谁在操作) ∩ scenario(什么场景) ∩ workflow(这张图) ∩ node(这个节点)
```

交集的结果,**永远比任何单层更小或相等**——委派(delegation)只会收窄,绝不会扩大。这是安全的核心不变式:一个被拆出去的小任务,不可能拿到比它来源更大的权限。

---

## 第 9 章 贯穿全程的设计原则(为什么这么设计)

1. **不可变 + 哈希**:图、能力、作业全带哈希,改动即被察觉。这是恢复、去重、审计的基石。
2. **模型规划 / 服务端执行分离**:AI 只在「拆活」时说话,其余全是代码。AI 想越界,服务器拒绝。
3. **失败是显式结果,不是隐藏的成功**:节点失败、证据缺口,都会被记录成 `partial` / `unavailable` 和明确的停止原因,而不是静默假装成功。
4. **证据靠身份证号,不靠语义**:去重、冲突、验证,全靠那五元组身份证号,不靠「看起来像不像」或「多数票」。
5. **能并行的并行,但有硬上限**:波次 + 双层限流 + 预算预留,并行但不失控。

---

## 第 10 章 当前还没做完的部分(诚实交代)

读到这里,你已经理解了**当前已实现**的部分。但 18 号文档里明确标注了还没做的(阶段 3/4 的 P0):

1. **执行器绑定静态节点集**:主循环的终止条件是 `len(outputs)+len(failedOptional) < len(definition.Nodes)`(`executor_orchestration.go:181`),也就是「节点数是开工前就定死的」。所以**运行中不能临时加节点**——「第一轮查完发现新线索,再派新 agent」这种**自适应扩展(adaptive expansion)**还没落地。目前是**单轮 fan-out**(一次拆活、一次派完)。
2. **跨轮累计预算账本**:每一轮是一次独立运行,还没有把「多轮的 token/工具/花费」汇总到一个总上限里。
3. **通用的人工闸门策略**没接全:目前只有 `evidence.risk` 这一个 gate。
4. **写任务**没复用这套机制:写操作目前一律降级成单 agent。

---

## 第 11 章 建议的阅读顺序(给想读源码的你)

按「数据流方向」读,而不是按文件名读:

1. `internal/agent/qa/route.go` —— 先看「什么情况才多 agent」
2. `internal/agent/qa/task_graph_plan.go` —— 再看「AI 怎么被限制着拆活」
3. `agent/capability.go` —— 理解「能力」长什么样
4. `internal/agent/workflow/proposal_validate.go` → `proposal_compile.go` —— 看「草图怎么变施工图」
5. `internal/agent/workflow/model.go` 的 `graph()` 和 `Prepare()` —— 看「施工图怎么被校验、盖章」
6. `internal/agent/workflow/executor_orchestration.go` —— 看「怎么一轮轮派活」(核心)
7. `internal/agent/workflow/executor_attempt.go` + `executor_budget.go` —— 看「单次尝试 + 预算」
8. `internal/agent/workflow/agent_node.go` —— 看「怎么真正调 AI」
9. `internal/agent/workflow/evidence.go` + `verifier.go` + `evidence_risk_gate.go` —— 看「证据 + 裁判 + 闸门」
10. `internal/agent/workflow/service_recovery.go` + `service_checkpoint.go` —— 最后看「断电恢复」

---

> 一句话总结:**这个系统把「多 agent 协作」这件最不可靠的事,拆成了「AI 只出主意、代码只当法官、服务器只认哈希和预算」三件可靠的事。**
> 你看懂这三件事怎么咬合,就理解了这个项目的核心。
