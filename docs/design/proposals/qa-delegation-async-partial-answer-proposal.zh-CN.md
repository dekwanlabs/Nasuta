# QA Delegation 异步派发、流式回填与 Partial Answer 提案

> 状态：提案，待评审
> 创建日期：2026-09-03
> 分支：feat/multi-agent-platform
> 范围：QA 父 Agent 的 `delegate_investigation` 编排链路、child Agent 生命周期、最终答案交付
> 诊断来源：`/Users/dequan.mac/.codex/attachments/c4de6f6f-b245-4491-87e6-090e9e99b054/pasted-text.txt`

## 0. 文档生命周期

本文是针对 2026-09-03 运行失败（父 Agent 同步等待 delegation 导致 `answerLen=0`）的独立开发提案，
不直接修改正式架构基线。执行顺序：

1. 先落地“委派异步化 + partial answer + 分层 deadline”的最小闭环。
2. 补测试与观测。
3. 灰度验证后，再合并到正式架构文档与既有提案。

## 1. 一句话结论

问题不是“时间配小了”，而是编排器存在一个不可中断的同步长调用（`delegate_investigation`），
它继承父 Agent 的 deadline，一旦 child 慢就会把父 Agent 拖到超时，最终 `answerLen=0`。

修复方向：**非阻塞派发 + 结果流式回填 + 分层硬 deadline + 确定性 partial answer 兜底**。
不再通过反复调大 timeout 解决。

## 2. 本次故障事实

- 父 Agent 总预算约 `4m55s`，扣除最终答案预留 `30s` 后，循环预算约 `4m25s`。
- 父 Agent 在 `17:53:12` 发起一次 `delegate_investigation`，到 `17:56:59` 返回，
  耗时 `3m49s`，几乎耗尽循环预算。
- 委派返回时 semantic verification 已报 `context deadline exceeded`。
- 父 Agent 在 step 3 开始前 loop budget 已耗尽，进入 forced conclusion。
- 最终答案在剩余约 30 秒内再次超时，结果为 `answerLen=0`。

## 3. 核心设计原则

1. **派发与等待解耦**：父 Agent 发起委派后立即拿回 `delegation_id`，不原地等结果。
2. **循环内只有短操作**：每个 loop step 不允许出现分钟级的同步阻塞调用。
3. **结果流式回填**：child 完成后结果通过 `delegation_id` 回填，不是一次性聚合返回。
4. **分层硬 deadline**：父 Agent、child、answer 各有独立 deadline，谁超时只影响谁。
5. **答案只有一份，但有版本**：draft → 回填更新 → deadline 交付一次。

## 4. 目标状态机

### 4.1 Answer 状态

```text
running -> partial（可交付） -> final
```

### 4.2 Task 状态

```text
running -> completed / failed / timeout
```

### 4.3 Child 在主流程结束后的状态

```text
completed_in_time -> 回填并释放 reservation
orphaned          -> 主流程已结束，结果落库为 supplement，不覆盖已交付答案
timeout           -> 强制终止并释放 reservation
```

## 5. 输出契约

### 5.1 发起委派（立即返回）

```json
{
  "delegation_id": "dg_20260903_001",
  "status": "dispatched",
  "tasks": [
    {"task_id": "t_rgb", "subject": "RGB", "status": "running"},
    {"task_id": "t_msg", "subject": "消息中心", "status": "running"}
  ]
}
```

### 5.2 父 Agent 先写 draft（内部，不交付）

```json
{
  "answer": {
    "status": "partial",
    "conclusion": "RGB 和消息中心已有初步结论，菜谱和 TTS 仍在调查。"
  }
}
```

### 5.3 查询委派进度（流式回填）

```json
{
  "delegation_id": "dg_20260903_001",
  "tasks": [
    {"task_id": "t_rgb", "subject": "RGB", "status": "completed", "report": "..."},
    {"task_id": "t_msg", "subject": "消息中心", "status": "completed", "report": "..."},
    {"task_id": "t_recipe", "subject": "菜谱", "status": "running"},
    {"task_id": "t_tts", "subject": "TTS", "status": "running"}
  ]
}
```

### 5.4 deadline 时交付（用户只看到这一次）

```json
{
  "answer": {
    "status": "partial",
    "conclusion": "以下为已确认信息。",
    "completed": ["RGB", "消息中心"],
    "pending": ["菜谱", "TTS"],
    "evidence_refs": ["ev_001", "ev_002"]
  }
}
```

## 6. 分层 deadline 预算

```text
run deadline    = 5分钟（整个请求总上限）
answer deadline = 预留 75秒（答案必须交付的硬线）
child deadline  = 配置上限，例如 60~90秒（单个 child 硬线）
```

发起委派时动态计算 child 能拿到的最大时间：

```text
child_budget = min(child 配置上限, answer_deadline - 当前已用时间 - 安全余量)
```

- child 不继承父 Agent 的 deadline。
- child 到 deadline 就终止，返回 partial report 或 failed report，并释放自己的 reservation。
- 父 Agent 到 answer deadline 强制交付 partial answer，不再重试慢模型。

## 7. 预算与 reservation 处理

1. 每个 child 独立申请 reservation，独立 tracking token/time，独立 deadline。
2. child 完成 → 回填结果并释放 reservation。
3. child 超时 → 终止执行并释放 reservation。
4. 主流程提前交付 → 未完成 child 进入 orphaned，由后台 reclaim 回收并释放。
5. root lease 释放失败时，必须输出具体 reservation ID，不能静默忽略。

## 8. 最终答案三层兜底

1. 正常：剩余时间充足时用正常模型总结。
2. 快速：剩余时间不足时切换 fast/no-reasoning 模型，较小输出。
3. 确定性：模型调用失败或时间不足时，直接用已有 evidence + partial report 渲染答案。

## 9. 需要修改的代码点

- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/tool.go:47`
  - child 不再 `InheritCallerDeadline`，改为独立 deadline。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/tool.go:84-109`
  - `executor.Execute` 同步阻塞改为 `ExecuteAsync` + `Poll`。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/executor.go:281-365`
  - child 独立 deadline 与终止。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/executor.go:1065-1100`
  - `wg.Wait()` 改为“谁完成谁回填”。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/executor.go:523-545`
  - semantic verification 超时标记 unavailable，不阻塞结果返回。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/loop.go:362-381`
  - 父 Agent 分层预算：run / loop / answer 独立。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/loop_execution.go:252-294`
  - 最终答案不再只依赖 `forceConclusion` 慢模型，增加确定性兜底。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/answer_generation.go:19-84`
  - final answer 的 continuation/retry/protocol repair 增加 deadline 感知。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/execution/loop_turn.go:491-552`
  - 父 Agent 识别 partial result，而不是把它当整个工具调用失败。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/budget/durable.go:304-340`
  - `ReleaseLease` 失败时输出 reservation ID 并告警。
- `/Users/dequan.mac/agent-workspace/Nasuta/internal/agent/delegation/executor.go:1362-1371`
  - 不再静默忽略 `Release()` 错误。

## 10. 落地顺序

### 第一批（止血）

1. delegation 拆成 async + poll，取消同步阻塞。
2. 超时返回 partial result，而不是只返回 error。
3. semantic verification 改为 best-effort。
4. 最终答案增加 deterministic fallback。

### 第二批（降低耗时与失败概率）

1. 降低 child reasoning / step / tool call 预算。
2. 保证 child 最小结构化报告。
3. 限制 child 与 parent 上下文。
4. 启用工具裁剪。
5. 降低 delegation prompt 的强制一次性委派约束。

### 第三批（真正异步与回收）

1. `delegation_status` 查询与 supplement 落库。
2. orphaned child 回收与 reservation 清理。
3. reclaim 监控与告警。

## 11. 验收标准

- 场景 1：一个 child 很慢 → delegation 到点返回 partial，父 Agent 仍能输出答案。
- 场景 2：child reasoning 用光 token → 至少返回 partial report，不出现 `answerLen=0`。
- 场景 3：semantic verifier 超时 → `verification.status=unavailable`，报告仍返回。
- 场景 4：最终总结模型超时 → 自动 fast model，再失败则 deterministic renderer。
- 场景 5：父流程取消 → 所有 reservation 可追踪、可 settlement，无长期 active 残留。

## 12. 与旧方案的区别

| 旧方案 | 新方案 |
|---|---|
| child 慢 → 增加 timeout | child 慢 → 不影响父 Agent |
| 全部完成才返回 | 完成多少返回多少 |
| 验证超时 → 拖垮父 Agent | 验证超时 → 标记 unavailable，继续 |
| 最终答案靠模型再试 | 最终答案有确定性兜底 |
