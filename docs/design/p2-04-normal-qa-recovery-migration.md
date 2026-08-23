# P2-04：普通单 Agent QA 非调查 recovery 迁移方案

状态：待实施（高风险，需要独立分支和回归基线）
关联：dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md
日期：2026-08-22

## 1. 为什么不能直接删

普通单 Agent QA 仍然依赖 `forceConclusion`、`ConclusionRetryMaxTokens`、
`generateWithContinue` 和 `publicOutputText` 生成最终答案。它们不是调查路径，
而是普通问答在 reasoning 截断、空输出或长度截断时的兜底。直接删除会把普通 QA
变成空答案或错误路径。

调查型 QA 已经走 v2 DeliveryGate，因此本迁移只影响普通单 Agent 路径。

## 2. 目标

用明确的最终答案渲染边界替代当前多层 output recovery：

1. 保留主模型正常答案生成。
2. 只有长度截断时允许一次 bounded continue。
3. reasoning-only 或空输出不再进入下一层模型猜测，改为确定性失败/降级结果。
4. 删除 `publicOutputText` 对结构化输出做猜测式转换的路径。

## 3. 实施步骤

1. 在 `internal/agent/execution` 引入 `AnswerRenderer`，只接受已通过 schema 的
   结构化输出，不再从任意 RawMessage 猜测 public text。
2. 将 `ConclusionRetryMaxTokens` 替换为单一 bounded continue，保留
   `ConclusionMaxTokens` 作为硬预算。
3. 在 loop 层增加 `LegacyAnswerRecoveryEnabled` 开关，默认开启并逐步灰度关闭。
   关闭后，空答案直接返回 `ErrEmptyAnswer`，由上层场景给出明确错误而不是再次
   调用模型猜测。
4. 删除 `forceConclusion` 的多层 fallback，只保留一次 continue。
5. 清理 `publicOutputText` 及依赖它的测试。

## 4. 回归要求

- 普通单 Agent QA 的空答案、reasoning-only、长度截断、结构化输出四类测试；
- 多 Agent 调查路径不得调用任何 legacy recovery；
- `go test ./internal/agent/execution ./internal/agent/qa ./app` 全绿；
- 部署前在 staging 打开开关灰度和 A/B 验证。

## 5. 不做的范围

- 不迁移历史 run 的旧答案；
- 不改变 v2 调查路径的 DeliveryGate；
- 不把普通 QA 强制路由到 v2 调查。
