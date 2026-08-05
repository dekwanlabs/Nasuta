# Nasuta Agent Platform Design Index

[English](README.md) | [中文](README.zh-CN.md)

> Status: consolidated design entry point
> Updated: 2026-08-05
> Sources: CodeLoom `docs/design/agent` and Nasuta `docs/design`

## 1. Purpose

This directory consolidates the two source design directories into one bilingual, module-oriented set. The source material mixes implementation notes, incident reviews, proposals, and target architecture. Reading it as a flat list creates four recurring problems:

- one capability is split across several files;
- decisions written at different times conflict or supersede one another;
- CodeLoom application policy is mixed with reusable Nasuta platform ownership;
- several documents exist only in Chinese.

The consolidated rules are:

1. each module has one Chinese document and one English document;
2. each module distinguishes implemented behavior, target behavior, deprecated choices, and known gaps;
3. Nasuta owns reusable mechanisms while CodeLoom keeps application policy;
4. former source files are removed after their active decisions are consolidated and verified; this directory becomes the canonical module-level entry point;
5. new design work should update this set first, then add a focused delivery note only when needed.

## 1.1 Completeness and Document Shape

Each module is a consolidated normative design rather than a concatenation of former files:

1. it states current behavior, target behavior, ownership, conflicting decisions, deprecated mechanisms, and migration order;
2. it merges still-valid constraints and removes duplicated, obsolete, or conflicting historical proposals;
3. its Chinese and English versions use the same section structure and active conclusions;
4. the coverage register records historical sources and their destination without requiring line-for-line retention in the normative document.

The migration covers 45 physical Markdown files, 30 logical documents, and 12,254 source lines. See the [Source Coverage Register](SOURCE-COVERAGE.md) for the file-level deletion gate and source mapping. Coverage proves that a source was reviewed and consolidated; it does not make a deprecated historical proposal part of the current contract.

## 2. Reading Order

| Order | Module | English | 中文 |
|---:|---|---|---|
| 1 | Architecture, execution, and run convergence | [01-architecture-and-execution.md](01-architecture-and-execution.md) | [01-architecture-and-execution.zh-CN.md](01-architecture-and-execution.zh-CN.md) |
| 2 | Evidence planning, tools, and runtime investigation | [02-evidence-and-tooling.md](02-evidence-and-tooling.md) | [02-evidence-and-tooling.zh-CN.md](02-evidence-and-tooling.zh-CN.md) |
| 3 | Retrieval, runbooks, and call chains | [03-retrieval-and-knowledge.md](03-retrieval-and-knowledge.md) | [03-retrieval-and-knowledge.zh-CN.md](03-retrieval-and-knowledge.zh-CN.md) |
| 4 | Context, sessions, and tool results | [04-context-session-and-tool-results.md](04-context-session-and-tool-results.md) | [04-context-session-and-tool-results.zh-CN.md](04-context-session-and-tool-results.zh-CN.md) |
| 5 | Indexing, ontology, and source storage | [05-index-ontology-and-storage.md](05-index-ontology-and-storage.md) | [05-index-ontology-and-storage.zh-CN.md](05-index-ontology-and-storage.zh-CN.md) |
| 6 | LLM providers and reliability | [06-llm-providers-and-reliability.md](06-llm-providers-and-reliability.md) | [06-llm-providers-and-reliability.zh-CN.md](06-llm-providers-and-reliability.zh-CN.md) |
| 7 | Long-term memory | [07-memory.md](07-memory.md) | [07-memory.zh-CN.md](07-memory.zh-CN.md) |
| 8 | Observability, tokens, and evaluation | [08-observability-and-evaluation.md](08-observability-and-evaluation.md) | [08-observability-and-evaluation.zh-CN.md](08-observability-and-evaluation.zh-CN.md) |
| 9 | Write safety and approval | [09-write-safety-and-approval.md](09-write-safety-and-approval.md) | [09-write-safety-and-approval.zh-CN.md](09-write-safety-and-approval.zh-CN.md) |
| 10 | Feature Delivery Agent | [10-feature-delivery.md](10-feature-delivery.md) | [10-feature-delivery.zh-CN.md](10-feature-delivery.zh-CN.md) |

## 2.1 Focused Implementation Proposals

The following Chinese proposals build on modules 01–10 and define the next implementation steps for the Agent platform and Feature Delivery. They are focused delivery proposals rather than replacements for the bilingual normative modules above.

| Order | Proposal | Document |
|---:|---|---|
| 11 | Single-Agent Decoupling and Independent Runtime | [11-single-agent-decoupling-proposal.zh-CN.md](11-single-agent-decoupling-proposal.zh-CN.md) |
| 12 | Nasuta Multi-Agent Platform | [12-multi-agent-platform-proposal.zh-CN.md](12-multi-agent-platform-proposal.zh-CN.md) |
| 13 | Multi-Agent Review at Development Stages | [13-development-multi-agent-review-proposal.zh-CN.md](13-development-multi-agent-review-proposal.zh-CN.md) |

## 3. Global Invariants

1. **Explicit evidence boundaries**: only acquired evidence may be stated as fact; partial, truncated, and unavailable states must propagate.
2. **Stable tool visibility**: Registry, configuration, and permissions determine visible tools; probabilistic routing only supplies preferences.
3. **Fresh evidence stays exact**: normal-sized current tool results reach the model verbatim; stale history is rewritten only under real context pressure.
4. **Authoritative traces stay complete**: Session, Agent Trace, and audit records retain complete results or losslessly recoverable references; UI collapse never mutates backend evidence.
5. **Bounded and recoverable**: reads, retrieval, tool output, and replay are bounded, but content removed from hot context must be archived or re-fetchable.
6. **Final-answer guarantees**: tool failure or missing evidence must not silently produce an empty answer; exact-output requirements use deterministic contracts.
7. **No provider substitution**: a configured provider failure remains observable and never silently switches providers.
8. **Inward ownership**: Nasuta owns reusable mechanics; CodeLoom composes business capabilities at the application boundary.
9. **Derive state from facts**: do not persist a state machine without a real transition graph, recovery, or concurrency requirement.
10. **Bound reads at the source**: pagination, cursors, projections, and `LIMIT` belong at the data-source boundary.
11. **Observable failure and degradation**: retries, omissions, compression, fallback, and budget decisions enter traces or metrics.

## 4. Source-to-Module Map

The table covers every logical document in both source directories. Chinese and English files with the same stem count as one logical document.

| Source document | Repository | Consolidated module |
|---|---|---|
| Agent Design Index | CodeLoom | This index |
| Agent End-to-End Execution Flow | CodeLoom | 01 |
| QA Run State Convergence | CodeLoom | 01 |
| Agent Evidence Planning | CodeLoom | 02 |
| Agent Runtime Evidence And Incident Design | CodeLoom | 02 |
| Runtime Investigation Accuracy Remediation | CodeLoom | 02 |
| Agent Web Evidence Design | CodeLoom | 02 |
| Required Evidence Empty-Answer Incident | Nasuta | 02 |
| QA Tool Selection and Multi-turn Evidence | Nasuta | 02 |
| Agent Retrieval Design | CodeLoom | 03 |
| Agent Retrieval Pipeline Simplification Proposal | CodeLoom | 03 |
| Agent Context and Multi-Intent Evidence Quality Remediation | CodeLoom | 03 |
| QA Runbook Retrieval and Tool Boundaries | Nasuta | 03 |
| Method-level Call-chain Closure | Nasuta | 03 |
| Session Summary Design | CodeLoom | 04 |
| QA Context Pollution Control | Nasuta | 04 |
| QA Session History Semantic Retrieval | Nasuta | 04 |
| QA Session High/Low-water Compaction | Nasuta | 04 |
| Structured Tool-output Compression | Nasuta | 04 |
| Agent Index Storage Design | CodeLoom | 05 |
| Nasuta Ontology Refactor | Nasuta | 05 |
| Docs Source-file Storage | Nasuta | 05 |
| LLM Provider Design | CodeLoom | 06 |
| LLM Retry, JSON Repair, and QA Closure Evaluation | CodeLoom | 06 |
| Long-Term Memory System Design | CodeLoom | 07 |
| Agent Observability Design | CodeLoom | 08 |
| QA LLM Token Accounting | Nasuta | 08 |
| Approval-Gated Write Tools | CodeLoom | 09 |
| Nasuta Feature Delivery Agent | Nasuta | 10 |
| Feature Delivery Stage Prompts and Artifacts | Nasuta | 10 |

## 5. Status Labels

This set is the consolidated design baseline as of 2026-07-31; it does not claim that every target is implemented.

- **Implemented**: stable behavior exists in code and tests.
- **Partially implemented**: the main mechanism exists with known gaps.
- **Target design**: intended consolidated behavior.
- **Deprecated / removal required**: historical behavior may still exist in code but is no longer the correct direction.

In particular, `sessionToolResultLimit = 1_200` is a confirmed implementation gap, not a target contract. See module 04.
