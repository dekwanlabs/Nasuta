# 原文覆盖清单

[English](SOURCE-COVERAGE.md) | [中文](SOURCE-COVERAGE.zh-CN.md)

> 为 2026-07-31 文档归并生成。下表每一项都已进入新模块，随后才删除旧文件。

## 覆盖规则

- 下表覆盖两个旧目录的全部 45 个 Markdown 文件和 30 个逻辑文档。
- 每个来源都已被审阅，其仍有效的约束、问题和决策已归入目标模块。
- 目标模块是整理后的规范设计，可以删除重复、过时和相互冲突的旧方案，不要求逐行保留原文。
- 历史来源只有单一语言时，两种语言的规范文档仍应表达相同的有效结论；这里保证语义双语归并，不声称逐行翻译。
- 原始文件名、语言、行数和目标文档保留在本清单中，用于追溯来源和检查删除范围。
- 原 CodeLoom `agent-index` 已由新的双语平台索引替代。

## 物理文件清单

| 来源仓库 | 原文件 | 语言 | 行数 | 模块 | 新权威文档 |
|---|---|---:|---:|---:|---|
| CodeLoom | `agent-context-evidence-quality-remediation.md` | English | 237 | 03 | [03-retrieval-and-knowledge.md](03-retrieval-and-knowledge.md) |
| CodeLoom | `agent-context-evidence-quality-remediation.zh-CN.md` | zh-CN | 475 | 03 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| CodeLoom | `agent-evidence-planning.md` | English | 103 | 02 | [02-evidence-and-tooling.md](02-evidence-and-tooling.md) |
| CodeLoom | `agent-evidence-planning.zh-CN.md` | zh-CN | 103 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| CodeLoom | `agent-execution-flow.md` | English | 128 | 01 | [01-architecture-and-execution.md](01-architecture-and-execution.md) |
| CodeLoom | `agent-execution-flow.zh-CN.md` | zh-CN | 128 | 01 | [01-architecture-and-execution.zh-CN.md](01-architecture-and-execution.zh-CN.md) |
| CodeLoom | `agent-index-storage-design.md` | English | 363 | 05 | [05-index-ontology-and-storage.md](05-index-ontology-and-storage.md) |
| CodeLoom | `agent-index-storage-design.zh-CN.md` | zh-CN | 363 | 05 | [05-index-ontology-and-storage.zh-CN.md](05-index-ontology-and-storage.zh-CN.md) |
| CodeLoom | `agent-index.md` | English | 31 | 索引 | [README.md](README.md) |
| CodeLoom | `agent-index.zh-CN.md` | zh-CN | 32 | 索引 | [README.zh-CN.md](README.zh-CN.md) |
| CodeLoom | `agent-llm-providers.md` | English | 49 | 06 | [06-llm-providers-and-reliability.md](06-llm-providers-and-reliability.md) |
| CodeLoom | `agent-llm-providers.zh-CN.md` | zh-CN | 49 | 06 | [06-llm-providers-and-reliability.zh-CN.md](06-llm-providers-and-reliability.zh-CN.md) |
| CodeLoom | `agent-memory-design.md` | English | 244 | 07 | [07-memory.md](07-memory.md) |
| CodeLoom | `agent-memory-design.zh-CN.md` | zh-CN | 244 | 07 | [07-memory.zh-CN.md](07-memory.zh-CN.md) |
| CodeLoom | `agent-observability-design.md` | English | 62 | 08 | [08-observability-and-evaluation.md](08-observability-and-evaluation.md) |
| CodeLoom | `agent-observability-design.zh-CN.md` | zh-CN | 62 | 08 | [08-observability-and-evaluation.zh-CN.md](08-observability-and-evaluation.zh-CN.md) |
| CodeLoom | `agent-retrieval-design.md` | English | 96 | 03 | [03-retrieval-and-knowledge.md](03-retrieval-and-knowledge.md) |
| CodeLoom | `agent-retrieval-design.zh-CN.md` | zh-CN | 96 | 03 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| CodeLoom | `agent-retrieval-simplification-proposal.md` | English | 255 | 03 | [03-retrieval-and-knowledge.md](03-retrieval-and-knowledge.md) |
| CodeLoom | `agent-retrieval-simplification-proposal.zh-CN.md` | zh-CN | 393 | 03 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| CodeLoom | `agent-run-state-convergence.md` | English | 166 | 01 | [01-architecture-and-execution.md](01-architecture-and-execution.md) |
| CodeLoom | `agent-run-state-convergence.zh-CN.md` | zh-CN | 267 | 01 | [01-architecture-and-execution.zh-CN.md](01-architecture-and-execution.zh-CN.md) |
| CodeLoom | `agent-runtime-evidence-design.md` | English | 134 | 02 | [02-evidence-and-tooling.md](02-evidence-and-tooling.md) |
| CodeLoom | `agent-runtime-evidence-design.zh-CN.md` | zh-CN | 133 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| CodeLoom | `agent-runtime-investigation-remediation.zh-CN.md` | zh-CN | 464 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| CodeLoom | `agent-summary-design.md` | English | 43 | 04 | [04-context-session-and-tool-results.md](04-context-session-and-tool-results.md) |
| CodeLoom | `agent-summary-design.zh-CN.md` | zh-CN | 43 | 04 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |
| CodeLoom | `agent-web-evidence-design.md` | English | 70 | 02 | [02-evidence-and-tooling.md](02-evidence-and-tooling.md) |
| CodeLoom | `agent-web-evidence-design.zh-CN.md` | zh-CN | 70 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| CodeLoom | `agent-write-tools-design.md` | English | 50 | 09 | [09-write-safety-and-approval.md](09-write-safety-and-approval.md) |
| CodeLoom | `agent-write-tools-design.zh-CN.md` | zh-CN | 50 | 09 | [09-write-safety-and-approval.zh-CN.md](09-write-safety-and-approval.zh-CN.md) |
| CodeLoom | `llm-retry-json-repair-evaluation.md` | English | 286 | 06 | [06-llm-providers-and-reliability.md](06-llm-providers-and-reliability.md) |
| Nasuta | `docs-source-file-storage.zh-CN.md` | zh-CN | 398 | 05 | [05-index-ontology-and-storage.zh-CN.md](05-index-ontology-and-storage.zh-CN.md) |
| Nasuta | `feature-delivery-stage-prompts.zh-CN.md` | zh-CN | 550 | 10 | [10-feature-delivery.zh-CN.md](10-feature-delivery.zh-CN.md) |
| Nasuta | `nasuta-call-chain-closure.zh-CN.md` | zh-CN | 228 | 03 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| Nasuta | `nasuta-feature-delivery-agent.zh-CN.md` | zh-CN | 1833 | 10 | [10-feature-delivery.zh-CN.md](10-feature-delivery.zh-CN.md) |
| Nasuta | `nasuta-ontology-refactor.zh-CN.md` | zh-CN | 1350 | 05 | [05-index-ontology-and-storage.zh-CN.md](05-index-ontology-and-storage.zh-CN.md) |
| Nasuta | `qa-context-pollution-control.zh-CN.md` | zh-CN | 381 | 04 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |
| Nasuta | `qa-llm-token-accounting.zh-CN.md` | zh-CN | 152 | 08 | [08-observability-and-evaluation.zh-CN.md](08-observability-and-evaluation.zh-CN.md) |
| Nasuta | `qa-required-evidence-failure-remediation.zh-CN.md` | zh-CN | 57 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| Nasuta | `qa-runbook-retrieval-and-tool-boundaries.zh-CN.md` | zh-CN | 580 | 03 | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| Nasuta | `qa-session-history-semantic-retrieval.zh-CN.md` | zh-CN | 230 | 04 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |
| Nasuta | `qa-session-turn-compaction.zh-CN.md` | zh-CN | 296 | 04 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |
| Nasuta | `qa-tool-selection-and-multiturn-evidence.zh-CN.md` | zh-CN | 100 | 02 | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| Nasuta | `tool-output-structured-compression.zh-CN.md` | zh-CN | 810 | 04 | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |

**合计：**45 个物理文件、30 个逻辑文档、12,254 行原文。

## 删除校验

本次删除已完成以下校验：每行都有目标文档；新目录相对链接全部有效；十个模块均有中英文版本；来源的有效结论已进入对应规范文档；并且没有覆盖仓库中与本任务无关的未提交修改。
