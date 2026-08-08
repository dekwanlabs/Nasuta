# Nasuta Agent 平台设计索引

[English](README.md) | [中文](README.zh-CN.md)

> 状态：统一设计入口
> 更新日期：2026-08-07
> 来源：CodeLoom `docs/design/agent` 与 Nasuta `docs/design`

## 1. 文档定位

本目录将两个原设计目录按模块归并为一套中英文文档。原文同时包含现状说明、历史事故、实施提案和目标架构，直接并列阅读容易出现以下问题：

- 同一能力分散在多篇文档中，例如工具选择、运行时证据和多轮证据；
- 不同时间的设计结论互相覆盖，例如工具结果压缩与会话证据回放；
- CodeLoom 应用策略与 Nasuta 可复用平台职责混在一起；
- 部分文档只有中文，无法形成稳定的双语入口。

统一后的规则是：

1. 每个模块只有一份中文文档和一份英文文档；
2. 模块文档同时说明已实现状态、目标状态、已废弃做法和待修复差距；
3. Nasuta 只拥有通用机制，CodeLoom 业务策略留在应用边界；
4. 旧文件的有效结论归并并校验后删除，本目录成为唯一模块级权威入口；
5. 新增设计优先更新本目录，再决定是否需要独立的专项实施文档。

## 1.1 完整性与文档结构

每个模块文档是一份整理后的规范设计，而不是旧文件的简单拼接：

1. 统一说明当前行为、目标行为、职责归属、冲突结论、废弃机制和迁移顺序；
2. 合并同一模块中仍然有效的约束，删除重复、过时和相互冲突的历史方案；
3. 中英文保持同一章节结构和同一有效结论；
4. 历史来源及其归并目标由覆盖清单记录，不要求在规范文档中逐行保留旧正文。

本次迁移覆盖 45 个 Markdown 物理文件、30 个逻辑文档和 12,254 行原文。文件级删除门禁和来源去向见[原文覆盖清单](SOURCE-COVERAGE.zh-CN.md)。覆盖清单证明来源已被审阅和归并，不表示已废弃的历史方案仍是当前合同。

## 2. 阅读顺序

| 顺序 | 模块 | 中文 | English |
|---:|---|---|---|
| 1 | 架构、执行流与 Run 收敛 | [01-architecture-and-execution.zh-CN.md](01-architecture-and-execution.zh-CN.md) | [01-architecture-and-execution.md](01-architecture-and-execution.md) |
| 2 | 证据规划、工具与运行时调查 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) | [02-evidence-and-tooling.md](02-evidence-and-tooling.md) |
| 3 | 检索、Runbook 与调用链 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) | [03-retrieval-and-knowledge.md](03-retrieval-and-knowledge.md) |
| 4 | 上下文、会话与工具结果 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) | [04-context-session-and-tool-results.md](04-context-session-and-tool-results.md) |
| 5 | 索引、本体与源文件存储 | [05-index-ontology-and-storage.zh-CN.md](05-index-ontology-and-storage.zh-CN.md) | [05-index-ontology-and-storage.md](05-index-ontology-and-storage.md) |
| 6 | LLM Provider 与可靠性 | [06-llm-providers-and-reliability.zh-CN.md](06-llm-providers-and-reliability.zh-CN.md) | [06-llm-providers-and-reliability.md](06-llm-providers-and-reliability.md) |
| 7 | 长期记忆 | [07-memory.zh-CN.md](07-memory.zh-CN.md) | [07-memory.md](07-memory.md) |
| 8 | 可观测性、Token 与评估 | [08-observability-and-evaluation.zh-CN.md](08-observability-and-evaluation.zh-CN.md) | [08-observability-and-evaluation.md](08-observability-and-evaluation.md) |
| 9 | 写工具、审批与安全 | [09-write-safety-and-approval.zh-CN.md](09-write-safety-and-approval.zh-CN.md) | [09-write-safety-and-approval.md](09-write-safety-and-approval.md) |
| 10 | Feature Delivery Agent | [10-feature-delivery.zh-CN.md](10-feature-delivery.zh-CN.md) | [10-feature-delivery.md](10-feature-delivery.md) |

## 2.1 专项实施方案

以下文档基于 01–10 的统一设计基线，面向后续 Agent 平台演进和 Feature Delivery 落地。当前先提供中文版本，不改变上述双语模块的规范地位。

| 顺序 | 方案 | 文档 |
|---:|---|---|
| 11 | 单 Agent 解耦与独立 Runtime | [11-single-agent-decoupling-proposal.zh-CN.md](11-single-agent-decoupling-proposal.zh-CN.md) |
| 12 | Nasuta 多 Agent 平台 | [12-multi-agent-platform-proposal.zh-CN.md](12-multi-agent-platform-proposal.zh-CN.md) |
| 13 | 研发节点多 Agent 评审 | [13-development-multi-agent-review-proposal.zh-CN.md](13-development-multi-agent-review-proposal.zh-CN.md) |
| 14 | Agent 平台实施差距审计 | [14-agent-platform-implementation-gap-assessment.zh-CN.md](14-agent-platform-implementation-gap-assessment.zh-CN.md) |
| 15 | QA 与研发任务多 Agent 路由 | [15-qa-and-feature-delivery-multi-agent-routing-proposal.zh-CN.md](15-qa-and-feature-delivery-multi-agent-routing-proposal.zh-CN.md) |
| 16 | QA、研发任务与多 Agent 统一 Execution Trace | [16-unified-execution-trace-proposal.zh-CN.md](16-unified-execution-trace-proposal.zh-CN.md) |
| 17 | Nasuta Core、Feature 与 CodeLoom 拆分 | [17-nasuta-core-feature-codeloom-split-proposal.zh-CN.md](17-nasuta-core-feature-codeloom-split-proposal.zh-CN.md) |

截至 2026-08-07，差距审计剩余 12 项：P1 已清零，P2 9 项，后置分布式能力 3 项。具体证据、边界和建议顺序以方案 14 为准。

## 3. 全局不变量

所有模块共同遵守以下约束：

1. **证据边界明确**：回答只能把实际取得的证据表述为事实；partial、truncated 和 unavailable 必须显式传播。
2. **工具集合稳定**：Registry、配置和权限决定可见工具；概率路由只提供偏好，不决定权限或强制调用。
3. **新鲜证据优先完整**：正常大小的当前工具结果原样进入模型；只在真实上下文压力下处理陈旧历史。
4. **权威追踪完整**：Session、Agent Trace 和审计记录保存完整结果或无损可恢复引用；UI 折叠不能修改后端证据。
5. **有界但可恢复**：存储读取、检索、工具输出和历史回放都有界；被移出热上下文的原文必须可归档读取或重新获取。
6. **最终回答有保障**：工具失败和证据不足不能导致静默空答案；精确输出要求通过确定性合同校验。
7. **Provider 不替换**：配置的 Provider 失败必须可见，不能静默换成其他 Provider。
8. **职责向内收敛**：Nasuta 提供通用机制，CodeLoom 只在应用层组合业务能力。
9. **状态从事实推导**：没有真实转换图、恢复或并发约束时，不增加持久化状态机。
10. **读取边界有界**：分页、游标、字段投影和 `LIMIT` 在数据源边界执行。
11. **失败可观测**：降级、压缩、重试、遗漏和预算决策都必须进入 trace 或指标。

## 4. 原文归并映射

下表覆盖两个来源目录中的全部逻辑文档；中英文同名文件视为同一逻辑文档。

| 原逻辑文档 | 来源 | 统一模块 |
|---|---|---|
| Agent Design Index | CodeLoom | 本索引 |
| Agent End-to-End Execution Flow | CodeLoom | 01 |
| QA Run State Convergence | CodeLoom | 01 |
| Agent Evidence Planning | CodeLoom | 02 |
| Agent Runtime Evidence And Incident Design | CodeLoom | 02 |
| Agent 运行时调查准确性修复提案 | CodeLoom | 02 |
| Agent Web Evidence Design | CodeLoom | 02 |
| QA 必需证据导致空答案事故与修复 | Nasuta | 02 |
| QA 工具选择与多轮证据设计 | Nasuta | 02 |
| Agent Retrieval Design | CodeLoom | 03 |
| Agent Retrieval Pipeline Simplification Proposal | CodeLoom | 03 |
| Agent Context and Multi-Intent Evidence Quality Remediation | CodeLoom | 03 |
| QA 知识文档检索与工具类型边界设计 | Nasuta | 03 |
| 方法级调用链闭环评估 | Nasuta | 03 |
| Session Summary Design | CodeLoom | 04 |
| QA 上下文污染控制设计 | Nasuta | 04 |
| QA 会话历史语义召回与有界上下文设计 | Nasuta | 04 |
| QA 会话高低水位压缩与 JSON 归档设计 | Nasuta | 04 |
| Nasuta 工具结果结构化压缩方案 | Nasuta | 04 |
| Agent Index Storage Design | CodeLoom | 05 |
| Nasuta 本体化重构设计 | Nasuta | 05 |
| Docs 源文件存储设计 | Nasuta | 05 |
| LLM Provider Design | CodeLoom | 06 |
| LLM 重试、JSON 修复与 QA 链路闭环评估 | CodeLoom | 06 |
| Long-Term Memory System Design | CodeLoom | 07 |
| Agent Observability Design | CodeLoom | 08 |
| QA LLM Token 使用量记录设计 | Nasuta | 08 |
| Approval-Gated Write Tools | CodeLoom | 09 |
| Nasuta 需求到代码交付 Agent 设计 | Nasuta | 10 |
| Feature Delivery 节点提示词与产物规范 | Nasuta | 10 |

## 5. 状态说明

本目录是截至 2026-07-31 的统一设计基线，不代表所有目标都已实现。模块文档使用以下标签：

- **已实现**：代码和测试中已有稳定行为；
- **部分实现**：主体存在，但仍有已知差距；
- **目标设计**：统一后应达到的行为；
- **已废弃/待移除**：历史方案仍可能存在于代码中，但不再作为正确方向。

特别注意：`sessionToolResultLimit = 1_200` 属于已确认的实现差距，不是统一后的目标合同。详见模块 04。
