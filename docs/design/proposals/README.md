# QA 设计提案索引

本目录收录 QA 链路（检索 → 委派 → 子 Agent → 预算 → 渲染）的设计提案。已实施完成或被新提案取代的文档移入 [`archive/`](archive/README.md)。

状态约定：`草案`=待评审；`待实现/未实施`=方案已定、尚未落地；`实施中`=部分切片已落地；`已实施`=完成、待归并正式文档。

## 主题一 · QA 预算 / 超时 / 推理治理

三份 canonical 提案，分别覆盖共享预算、超时/token、推理控制：

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`investigation-run-budget-and-large-context-governance.zh-CN.md`](investigation-run-budget-and-large-context-governance.zh-CN.md) | 08-25 | 草案 | Run 共享预算与大上下文治理（四类限制分层） |
| [`qa-agent-timeout-token-budget-and-orchestration-governance.zh-CN.md`](qa-agent-timeout-token-budget-and-orchestration-governance.zh-CN.md) | 09-05 | 草案 | 超时、Token 膨胀、多 Agent 收敛 |
| [`qa-llm-provider-aware-reasoning-and-budget-governance.zh-CN.md`](qa-llm-provider-aware-reasoning-and-budget-governance.zh-CN.md) | 09-05 | 草案 | 推理控制、父子预算治理（`max_tokens` 字段语义） |

与预算正交的结果契约侧：

| [`investigation-limitations-normalization-and-retention.zh-CN.md`](investigation-limitations-normalization-and-retention.zh-CN.md) | 08-18 | 草案 | limitations 归一化、排序与结果留存 |

已归档：`qa-agent-context-budget-and-cancellation.zh-CN.md`（08-05，待实现 → 被上述 09-05 两份提案覆盖）。

## 主题二 · QA 委派 / 编排 / 控制面

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`qa-delegation-async-partial-answer.zh-CN.md`](qa-delegation-async-partial-answer.zh-CN.md) | 09-03 | 待评审 | 异步派发、流式回填、partial answer |
| [`qa-delegation-async-settlement.zh-CN.md`](qa-delegation-async-settlement.zh-CN.md) | 09-04 | 待评审 | 异步收口与预算（09-03 的修正续篇：把「等完成」收回服务端 loop） |
| [`qa-workflow-control-plane-and-phase-boundary.zh-CN.md`](qa-workflow-control-plane-and-phase-boundary.zh-CN.md) | 08-29 | 草案 | Workflow 控制面、阶段边界、单一契约 |
| [`qa-chain-layer-collapse.zh-CN.md`](qa-chain-layer-collapse.zh-CN.md) | 09-06 | 草案 | 链路职责收敛、运行时边界（入口已收敛，剩余跨层组合待清理） |

已归档：`investigation-workflow-reliability-proposal.zh-CN.md`（08-16，未实施 → legacy `investigation` 工作流已被 delegation 重构取代）。

## 主题三 · 子 Agent 调查治理

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`qa-investigation-report-structured-output-and-recovery.zh-CN.md`](qa-investigation-report-structured-output-and-recovery.zh-CN.md) | 09-07 | 草案 | 结构化报告生成与恢复 |
| [`qa-investigation-flow-coverage-and-gap-chase.zh-CN.md`](qa-investigation-flow-coverage-and-gap-chase.zh-CN.md) | 09-08 | 草案 | 流程图缺失、缺口二次追查（gap chase） |
| [`qa-child-investigator-evidence-seeding-and-bounded-budget.zh-CN.md`](qa-child-investigator-evidence-seeding-and-bounded-budget.zh-CN.md) | 09-08 | 草案 | 子 Agent 预检索喂料、有界调查预算（是 gap-chase 提案的续篇） |
| [`investigator-scoped-context-projection.zh-CN.md`](investigator-scoped-context-projection.zh-CN.md) | 08-17 | 草案 | 按任务范围投影子 Agent 上下文 |

## 主题四 · 检索 / 证据链路

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`qa-unified-evidence-acquisition-pipeline.zh-CN.md`](qa-unified-evidence-acquisition-pipeline.zh-CN.md) | 08-09 | 待实现 | Retrieval 与 Tool Calling 统一证据链路 |
| [`qa-evidence-convergence-and-retrieval-governance.zh-CN.md`](qa-evidence-convergence-and-retrieval-governance.zh-CN.md) | 08-09 | 部分实现 | 证据收敛、历史相关性、工具准入 |
| [`qa-retrieval-latency-and-progress-governance.zh-CN.md`](qa-retrieval-latency-and-progress-governance.zh-CN.md) | 08-10 | 实施中 | 首屏时延、进度反馈 |
| [`qa-query-intent-and-facet-model-simplification.zh-CN.md`](qa-query-intent-and-facet-model-simplification.zh-CN.md) | 08-15 | 未实施 | 查询意图、Facet 收敛为 canonical QueryPlan |
| [`qa-comparison-entity-and-evidence-coverage.zh-CN.md`](qa-comparison-entity-and-evidence-coverage.zh-CN.md) | 08-18 | 核心已实施 | 对比问题实体识别与证据覆盖 |

已归档：`retrieval-current-chain.zh-CN.md`（08-15，现状梳理基线、非提案）。

## 主题五 · 前端 / 渲染

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`qa-delegation-event-projection-and-answer-rendering.zh-CN.md`](qa-delegation-event-projection-and-answer-rendering.zh-CN.md) | 09-07 | 草案 | 委派事件投影断线、回答分条渲染保真度 |
| [`qa-reasoning-model-budget-and-streaming-render.zh-CN.md`](qa-reasoning-model-budget-and-streaming-render.zh-CN.md) | 09-08 | 草案 | 09-08 事故收口：推理预算映射 + 前端流式渲染性能（新增点） |
| [`qa-child-context-window-and-streaming-render-governance.zh-CN.md`](qa-child-context-window-and-streaming-render-governance.zh-CN.md) | 09-08 | 草案 | 落地补充：子 Agent 单次窗口解耦 + 预喂料压限（后端）+ 前端渲染治理 P0~P4 |

> 注：`qa-reasoning-model-budget-and-streaming-render` 是 2026-09-08 事故的定位/收口文档；`qa-child-context-window-and-streaming-render-governance` 是它的落地补充，给出后端窗口解耦（`perStepContextTokens` 混用维度）与前端 21 个渲染热点（P0 thinking 卡死 / P1 O(n²) / P2 渲染风暴 / P3 mermaid / P4 Monaco）的具体改动清单。后者同时扩展主题一的「四类限制分层」与主题三的「预喂料有界预算」。

## 主题六 · 评审 / 审计（非 QA 专项）

| 提案 | 日期 | 状态 | 范围 |
| --- | --- | --- | --- |
| [`builtin-tools-review.zh-CN.md`](builtin-tools-review.zh-CN.md) | 08-01 | 实施中 | 内置工具实现简化 |
| [`ontology-review.zh-CN.md`](ontology-review.zh-CN.md) | 08-02 | 评审草稿 | 本体使用场景与链路评审 |
| [`qa-evaluation-findings-improvement.zh-CN.md`](qa-evaluation-findings-improvement.zh-CN.md) | 08-03 | 实施中 | QA 评估问题改进 |

## 归档

已归档提案见 [`archive/README.md`](archive/README.md)。归档条件：实施完成并归并正式文档，或已出现明确取代它的新提案。
