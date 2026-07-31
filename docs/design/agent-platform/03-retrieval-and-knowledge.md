# Retrieval, Knowledge Documents, and Call Chains

[English](03-retrieval-and-knowledge.md) | [中文](03-retrieval-and-knowledge.zh-CN.md)

> Status: hybrid retrieval implemented; budgeting, score semantics, and multi-intent coverage are still converging
> Sources: Retrieval Design, Retrieval Simplification, Context Evidence Quality, Runbook Boundaries, Call-chain Closure

## 1. Goal

Retrieval converts the current question into bounded, traceable evidence grouped by source. It does not generate the answer and does not present ranking scores as factual confidence.

```text
canonical query
  -> bounded source retrieval
  -> source-local normalization
  -> deduplication
  -> rerank/fusion
  -> diversity and facet coverage
  -> token-budgeted evidence assembly
```

## 2. Canonical Query

A request has one canonical query. A complete question is not concatenated with prior-turn text. Only clearly referential, elliptical, or short follow-ups may use validated query rewriting.

Derived data may include terms, rare tokens, entities, a small facet set, and time/environment constraints. These values support recall and coverage; they do not create a second drifting query truth.

## 3. Hybrid Retrieval

- Dense retrieval handles semantic similarity.
- BM25/sparse retrieval handles exact tokens, identifiers, and error text.
- Results normalize within each source before an explicit fusion strategy such as RRF.
- Reranking is optional; absence disables only the capability, while configured failure remains visible.
- Vocabulary and index snapshots publish atomically; online reads do not retain mutable pointers.

Score fields remain distinct:

- raw dense/sparse score;
- fusion score;
- rerank score;
- trust/provenance adjustment;
- final selection score.

Different semantics must not reuse one field.

## 4. Deduplication and Diversity

Deduplicate by stable identity, not rendered text:

- code: repository + path + symbol/chunk identity;
- runbook: document + section/chunk;
- service/API: ontology identity;
- call edge: caller + callee + location + provenance.

Use map-based O(n) deduplication before the unavoidable sort. Diversity first covers different sources, documents, and facets, then fills remaining budget with high-scoring items.

## 5. Runbook Retrieval

`search_runbooks` returns chunk/section evidence rather than only the best chunk per document.

```json
{
  "items": [],
  "total": 0,
  "truncated": false,
  "next_cursor": "",
  "coverage": {
    "documents": "complete|partial",
    "sections": "complete|partial"
  }
}
```

Rules:

- one document may retain several relevant sections;
- cover selected documents before adding more sections from the same document;
- every chunk enters the evidence pool independently with source location;
- keyword fallback and semantic score are recorded separately;
- `truncated=true` means more matches were not returned, not that omitted facts do not exist;
- storage uses `limit+1` or cursors instead of loading full bodies and slicing.

## 6. Multi-intent and Facet Coverage

A compound question cannot be represented by one globally high-scoring result. The assembler tracks a bounded facet set:

```text
requested: [code, runbook, dependency]
covered:   [code, runbook]
missing:   [dependency]
```

At most one targeted supplemental retrieval is allowed within budget. No open-ended retry loop and no query-specific keyword special cases.

## 7. Call Chains

Method-level call chains distinguish:

1. static call sites with caller, callee, file, line, column, and language;
2. verified service routes closed through entry points, clients, and configuration;
3. service-level `depends_on`, which expresses dependency but is not a method call.

Call-chain results retain duplicate call sites, location, confidence, provenance, unresolved nodes, `truncated`, `next_frontier`, and bounds such as `max_depth`, `max_nodes`, and `max_fanout`.

A service dependency must not be promoted to a method-level call, and a truncated traversal must not be described as complete.

## 8. Budgeted Assembly

Retriever rune/character limits are not the model context window. The target split is:

- bound candidate count and item size at the source;
- assemble with a provider-aware token budget;
- prioritize the current question and fresh tool evidence;
- add evidence items atomically instead of breaking JSON or code in the middle;
- propagate coverage and omitted counts into the agent.

## 9. Failure and Degradation

| Condition | Behavior |
|---|---|
| Dense capability absent | Use explicitly enabled sparse capability and record state |
| Configured dense backend fails | Visible error; no silent substitution |
| Rerank absent | Preserve fused ordering |
| Configured rerank fails | Record error and follow documented failure policy |
| Runbook output truncated | Return cursor and partial coverage |
| Call traversal budget exhausted | Return frontier; do not claim closure |
| No hits | State that this query found none; do not prove non-existence |

## 10. Acceptance Criteria

1. One canonical query survives the pipeline.
2. Deduplication uses stable identities in O(n).
3. A runbook may contribute multiple sections.
4. Every score meaning is traceable.
5. Multi-intent questions report missing facets.
6. Call chains distinguish method calls, service routes, and dependencies.
7. Every read and traversal has explicit bounds and continuation.

## Detailed Consolidated Material

### Agent Retrieval Design

> Migrated from CodeLoom `docs/design/agent/agent-retrieval-design.md`; incorporated into this module on 2026-07-31.

Status: Current baseline with evaluated backlog

Agent retrieval combines structured records, dense vectors, sparse lexical matching, dependency graphs, and budgeted evidence assembly. `EvidencePlan` decides whether this pipeline may run; this document defines how Internal evidence is indexed and retrieved.

#### Pipeline

```text
one canonical query + query terms
  -> retrieval dispatch
  -> dense/sparse discovery
  -> hit-gated structured and CodeGraph expansion
  -> deduplication and reranking
  -> rank-preserving layer/service diversity
  -> per-item context-budget assembly
```

The trace separates `retrieval_dispatch`, `retrieval_discover`, `retrieval_expand`, and `retrieval_assemble` so recall, enrichment, ranking, and context construction can be evaluated independently.

#### Dense And Sparse Retrieval

Code retrieval blends Qdrant dense vectors with the named sparse vector `bm25`. Dense search covers semantic similarity; sparse search preserves exact identifiers, service names, paths, error text, and terminology.

Sparse tokenization:

1. splits CJK and non-CJK segments;
2. segments Chinese and splits non-CJK word boundaries;
3. splits camelCase and normalizes case;
4. filters stop words, pure/noisy numbers, UUIDs, long hexadecimal values, excessive digit ratios, and oversized terms;
5. preserves document repetition for term frequency while deduplicating query tokens.

Only source/interface files create sparse vectors. Configuration and documentation still receive dense vectors but do not pollute the code vocabulary.

#### Stable Vocabulary And Incremental Indexing

Sparse token IDs are Qdrant coordinates and are append-only. Published IDs are never reassigned or reused.

```json
{
  "version": 2,
  "tokenizer_version": 1,
  "next_id": 52936,
  "tokens": {"order":10,"payment":25}
}
```

The vocabulary is saved with temp-file replacement. Repository indexing clones the current vocabulary, allocates new IDs, persists the extended vocabulary, writes a new repository generation, and deletes old generation points only after successful upsert. Failure may leave unused IDs but cannot leave Qdrant coordinates unknown after restart.

Qdrant applies collection-level IDF. Stored document weights use saturated term frequency with `b=0`, avoiding a corpus-wide average-length rewrite after every repository update. Queries send value `1` for known tokens.

An indexing lock serializes vocabulary allocation and repository replacement. The complete builder is published to live Agent tools through an atomic pointer, so readers never observe a partially built vocabulary. Legacy vocabulary detection blocks incremental updates until a full migration runs.

#### Evidence Ownership And Diversity

Code and service records carry runtime-form layers such as `app`, `server`, and `front`; runbook evidence uses `docs`. CodeGraph results derive layer from repository/service paths when vector payload is unavailable.

Rerank diversity counts by `layer + service` but selects in one global-rank-preserving pass; it never round-robins groups to promote lower-ranked evidence. Layer is a presentation label and formatting never reorders evidence. Unknown layers remain visible rather than being coerced into a known category.

Online Runbook evidence prefers matched chunks, merges only a bounded number of deduplicated chunks per document, and applies a rune limit. Full document bodies belong only to explicit full-read flows.

Each reranked item enters budget assembly independently. Only evidence actually written to context contributes references, and `HitCount` means the number of unique visible references. Truncation occurs per item rather than after merging independent sources.

Dependency persistence uses `dependency_edges` and includes scanner-produced HTTP/gRPC/RPC relationships plus explicitly declared runbook edges. Repository replacement removes stale edges using repository ownership.

#### Failure Rules

- Qdrant upsert failure retains the previous generation.
- Empty repository scans remove only that repository's code vectors.
- Failed vocabulary persistence prevents vector publication.
- A missing semantic capability disables semantic retrieval observably; it is not silently replaced by another backend.
- Rerank provider failure follows the documented retrieval fallback and remains logged/traced.

#### Open Quality Work

##### C/C++ Runtime Form

A dedicated project-level C/C++ indexer is needed to classify firmware repositories as `mcu` or `module` from build/SDK structure. It must not guess per file or hardcode known repositories.

##### Event-Driven Dependencies

MQTT, Kafka, RabbitMQ, and WebSocket relationships are not comprehensively extracted. New extraction must use a general edge contract with source evidence and confidence.

##### CodeGraph Fallback

Measure dependency coverage before adding a fallback for empty structured walks. A fallback must improve a general missing-edge class without duplicating or contradicting existing edges.

##### Rewrite Evaluation

Standalone rewrites now receive deterministic validation for newly introduced technical identifiers, and complete questions no longer receive recent-turn prefixes. A multilingual anaphora dataset is still needed for Chinese entities, short acronyms, and cross-language rewrites without token-specific heuristics.

#### Evaluation

Use labels for relevant chunks, services, dependency edges, and runtime forms. Measure Recall@K, MRR/nDCG, context precision, duplicate rate, source/layer diversity, unsupported rewrite entities, latency, and final-answer groundedness. A mechanism is accepted only when it improves a general slice without regressing unrelated slices.

### Agent Retrieval Pipeline Simplification Proposal

> Migrated from CodeLoom `docs/design/agent/agent-retrieval-simplification-proposal.md`; incorporated into this module on 2026-07-31.

Status: Phases 1-3 implemented; Phase 4 ownership split pending

Implementation status (2026-07-17): canonical queries, matched Runbook chunks, rank-preserving diversity, per-item budget assembly, visible-reference accounting, demand-driven CodeGraph expansion, and question-sensitive Agent step limits are implemented. Splitting `AskAgentWithContext` remains a later structural change.

This proposal addresses a mechanism where Internal evidence is recalled correctly but loses priority during reranking, formatting, and budget assembly. It also reduces accidental complexity in query rewriting, retrieval expansion, and Agent follow-up lookup. No rule in this design is keyed to a specific question, service, or token.

#### 1. Background

The current path provides multi-source retrieval, external reranking, structural expansion, context budgeting, and Agent tool lookup. Those capabilities are valid, but structured evidence is repeatedly reordered and flattened before it reaches the model:

```text
question and conversation
  -> evidence planning, cleaning, term extraction, optional standalone rewrite
  -> parallel Code / Runbook / Service discovery
  -> Service / Dependency / CodeGraph expansion
  -> shared pool deduplication and reranking
  -> layer + service round-robin diversity
  -> fixed server/app/front/docs regrouping
  -> partial priority inferred from Markdown headings
  -> whole-block context truncation
  -> up to five more Agent tool steps
```

Two observed failure classes share the same structural cause:

1. A complete short question is unconditionally prefixed with a long previous turn, so retrieval follows the previous topic.
2. A top-ranked flow document is moved behind lower-ranked code by layer formatting; a monolithic evidence block is then truncated, displacing the main flow with branch code and producing zero references.

#### 2. Goals

1. Produce one canonical query for Internal retrieval.
2. Preserve source, rank, trust, and reference metadata until final serialization.
3. Never let diversity or presentation grouping destroy global rerank order.
4. Apply the context budget per evidence item rather than per merged string.
5. Use matched Runbook chunks by default instead of implicit full-document reads.
6. Define `HitCount` as the number of unique references actually included in context.
7. Let simple questions perform one retrieval and at most one targeted follow-up while retaining multi-hop Agent behavior.
8. Keep selection and assembly bounded, observable, single-pass, and independent of case-specific keywords.

#### 3. Non-goals

- Replacing Qdrant, BM25, DashScope Rerank, CodeGraph, or storage backends.
- Redesigning indexed payloads or requiring a full reindex.
- Removing Observe, Memory, or Web capability boundaries.
- Solving every answer-quality issue in one change.
- Hiding selection defects by increasing the context budget.
- Adding special rules for any observed failing query.

#### 4. Root causes

##### 4.1 Multiple competing query representations

A request may have the original question, `CleanQuestion`, a conversation-prefixed query, a term-analysis query, and a standalone rewrite. When no rewrite is produced, recent conversation can still be appended to an already complete question and dominate dense and sparse recall.

##### 4.2 Premature evidence flattening

`codeDoc` contains source, layer, rerank score, trust, and location. `formatCodePool` collapses the entire ranked pool into one `partial{text, refs}`, losing per-item budget and reference ownership.

##### 4.3 Rank is destroyed twice

Round-robin diversity across `layer + service` can promote a lower-ranked group. Fixed formatting by `server -> app -> front -> mcu -> module -> docs` then moves documents behind code regardless of rerank score.

##### 4.4 Unbounded Runbook reads

Search returns relevant chunks, but expansion prefers full `Record.Text` and only falls back to `ChunkText`. A bounded chunk lookup becomes an online read of up to 8,000 characters and can merge the same full text repeatedly.

##### 4.5 Whole-block truncation corrupts references

Many sources are merged into one partial. If that partial exceeds the remaining budget, the assembler truncates and exits before collecting references. Moving all references before the break would over-report evidence that never became visible.

##### 4.6 Pre-retrieval overlaps Agent lookup

Pre-retrieval searches code, Runbooks, services, dependencies, and CodeGraph, while the Agent may repeat equivalent searches for up to five steps. Simple queries pay for both paths and can repeatedly hit the same index after a weak seed.

#### 5. Principles and invariants

##### 5.1 One canonical query

```text
anaphora or omission -> validated standalone rewrite
complete question    -> CleanQuestion
```

Conversation remains available to planning and answering but is not appended directly to a complete vector/BM25 query.

##### 5.2 Preserve structure to the serialization boundary

Retrieval, deduplication, reranking, diversity, and budgeting operate on structured items. Markdown is produced only when context is injected into the LLM.

##### 5.3 Relevance order comes first

Trust can break ties or enforce an explicit authority policy. Diversity can cap monopolies. Neither may arbitrarily reorder ranked evidence.

##### 5.4 Bound reads at each source

Code windows, Runbook chunks, dependency edges, and service records have source-level count and size limits. Full reads require an explicitly named full-document operation.

##### 5.5 References describe visible evidence

An item contributes a reference only after at least part of its text enters context. Fully dropped items never appear in `References` or `HitCount`.

#### 6. Target architecture

```text
Question + Conversation
          |
          v
QueryResolver ---------> canonicalQuery
          |
          v
EvidencePlanner -------> EvidencePlan
          |
          v
Backend Retrieval
  Code / Runbook / Service / Observe
          |
          v
[]EvidenceItem
          |
          v
Dedup -> Rerank -> Rank-preserving diversity -> Budget selection
          |
          v
ContextAssembler ------> text + included references + selection stats
          |
          v
Answerer --------------> final answer or one targeted evidence-gap lookup
```

##### 6.1 Unified evidence model

```go
type EvidenceItem struct {
    Source        EvidenceSource
    Layer         string
    Service       string
    Text          string
    Reference     Reference
    RecallScore   float64
    RerankScore   float64
    TrustTier     int
    EvidenceClass string
    Rank          int
}
```

This is an internal retrieval contract and must not spread into domain or transport packages. Existing `codeDoc` can evolve into it or convert after reranking.

##### 6.2 Query resolution

`QueryResolver` determines whether anaphora resolution is required, creates a standalone question only when needed, validates that the rewrite introduced no unsupported identifiers, and emits one `canonicalQuery`.

##### 6.3 Backend retrieval

`EvidencePlan` continues to select capability boundaries. Internal retrieval chooses only the generally relevant backends:

- architecture and flows: Runbook, Service, Dependency, then supporting code;
- symbols and call chains: Code and CodeGraph, then Service as needed;
- runtime incidents: Observe and relevant Runbooks, with code for known implementations;
- entity lookup: exact Service or Symbol lookup without automatic graph fanout.

Selection uses general response intent and structured terms, not business keyword tables. Uncertain cases may fan out, but expansion remains hit-gated.

##### 6.4 Runbook chunk reads

Use `ChunkText` and `SectionHeader` by default. Group by title, keep a bounded number of top chunks, deduplicate identical text with a map, cap characters per document, and use `Record.Text` only for explicit full-document requests.

##### 6.5 Deduplication, reranking, and diversity

```text
bounded candidates -> O(n) map dedup -> one rerank -> threshold -> rank-preserving diversity
```

Rank-preserving diversity performs one ordered scan and accepts an item only while its `layer + service` group remains under the cap. Non-strict top-K fill uses skipped items in original rank order. It never round-robins groups or sorts again during formatting.

##### 6.6 Budget assembly

For each selected item in order, render a source label, apply its source-level cap, write `min(item length, remaining budget)`, register the reference after writing, and stop when the budget is exhausted. Adjacent items of the same layer may share a heading; non-adjacent items are never moved together.

Keep the existing `RetrievedContext` API and add Trace statistics for retrieved, reranked, included, dropped, and truncated counts plus included characters. `HitCount` means `len(unique included references)`.

##### 6.7 Agent follow-up policy

- Simple lookup and architecture overview: answer directly, with at most one targeted lookup.
- Symbol or dependency tracing: two to three steps.
- Genuine multi-hop incident and write-target tracing: retain the current maximum.

Every lookup names one concrete evidence gap. Add a general answer constraint: when evidence contains separate paths, label the main path, bypass, fallback, or active refresh separately instead of merging them.

#### 7. Delivery phases

##### Phase 1: assembly correctness

Limit changes to `internal/retrieval`: emit per-item evidence, use rank-preserving diversity, remove layer reordering, budget per item, and correct `HitCount` and Trace semantics. Do not change retrieval providers, indexed data, or Agent prompts.

##### Phase 2: query and read boundaries

Introduce `canonicalQuery`, remove history prefixes from complete questions, prefer Runbook chunks, configure source-level item caps, and record query and selection counts.

##### Phase 3: demand-driven expansion and convergence

Run Dependency and CodeGraph expansion only for relevant general intents, limit simple questions to one follow-up, preserve multi-hop behavior, and add the separate-path answer constraint.

##### Phase 4: ownership split

After behavior stabilizes, split `AskAgentWithContext` into QueryResolver, EvidencePlanner, RetrievalCoordinator, ContextAssembler, and AgentRunner. QA Service retains orchestration and capability degradation only. Do not combine this refactor with the earlier behavior changes.

#### 8. Testing

Mechanism tests use generic cross-layer fixtures, not tokens from observed failures.

Unit coverage includes rank preservation across layers, diversity caps without promotion, UTF-8-safe truncation, visible-only references, nonzero `HitCount` for evidence context, bounded and deduplicated Runbook chunks, history-free complete queries, standalone rewrites for anaphora, and hard context limits.

Integration coverage includes mixed flow documents and multilingual code, topology queries without irrelevant CodeGraph symbols, call-chain queries that still expand CodeGraph, and deterministic local ordering when the configured reranker fails.

Regression evaluation covers a standalone short question after a long turn, main and bypass paths, multi-service architecture, exact symbol lookup, multi-hop writes, and Observe incidents. Track Recall@K, MRR/nDCG, context precision, duplication, visible-reference ratio, truncation, groundedness, first-answer latency, and tool steps.

#### 9. Observability

Record canonical query length; retrieved, deduped, reranked, selected, and included counts; context budget and included characters; included rank/source/layer/trust/size/truncation; and drop reasons. Do not log full sensitive evidence.

Alert on these invariants:

- `included_count > 0 && hit_count == 0`;
- nonempty context with no references;
- a top-ranked eligible item not included for any reason other than threshold or budget;
- standalone rewrite identifiers absent from both question and conversation.

#### 10. Acceptance criteria

1. The first eligible reranked item enters context first.
2. Formatting never changes global evidence order.
3. No monolithic partial contains multiple independent sources.
4. `HitCount` matches unique visible references.
5. Complete questions are not polluted by previous turns.
6. Online Runbook evidence uses bounded relevant chunks.
7. Simple questions average no more than one follow-up lookup.
8. Multi-hop and Observe behavior has no functional regression.
9. Main and bypass paths remain distinct without increasing context budget.

#### 11. Rollout and rollback

Deliver each phase as an independent commit and run focused retrieval and Agent tests, `go build ./...`, and `go vet ./...`. Phase 1 may briefly retain the old formatter/assembler behind a release-comparison flag, but the flag must be removed after stabilization. Run old and new selection over a fixed evaluation set without duplicating final-answer LLM calls. Roll back by phase rather than masking regressions with larger budgets or lower thresholds. Phases 1 through 3 require no index rebuild.

#### 12. Expected complexity reduction

Retain capability planning, parallel backend recall, one reranker, bounded assembly, multi-hop tool use, and observable configured-backend degradation.

Remove competing query representations, post-rank round-robin and layer sorting, business priority inferred from Markdown headings, full-document merge before truncation, repeated search for simple questions, and downstream repair of reference counts.

Outside the reranker's single sort, selection and assembly are O(n) passes with map-based membership and explicit source bounds.

### Agent Context and Multi-Intent Evidence Quality Remediation

> Migrated from CodeLoom `docs/design/agent/agent-context-evidence-quality-remediation.md`; incorporated into this module on 2026-07-31.

Status: Phase 1 in progress; Phases 2 and 3 pending

Implementation status (2026-07-17): the current worktree contains semantic multi-section Runbook candidates, document/per-document/global count bounds, independent section evidence, and candidate diagnostics. Configuration, complete tests, and build acceptance remain unfinished. Model-token ledgers, prompt convergence, facet coverage, and the complete score contract are not implemented. Query-context changes also drift from the single canonical-query invariant.

This document follows the [retrieval simplification proposal](03-retrieval-and-knowledge.md). The earlier work established canonical queries, rank-preserving evidence, per-item assembly, and visible-reference accounting. This proposal addresses the remaining quality gaps: incomplete coverage for compound questions, document-level collapse of Runbook chunks, rune budgets disconnected from the model context window, oversized permanent prompts, and ambiguous hybrid-search scores.

No production rule may key off an observed question, service, or token. Online reads and selection must remain bounded.

#### 1. Confirmed baseline

- `ContextBudget` is consumed in runes, not model tokens. Reaching it is a local assembler event, not an LLM context-window error.
- The base and Agent-mode prompts contain roughly 20,000 English characters and repeat grounding, call-chain, evidence-gap, and tool-use rules.
- The current worktree now keeps multiple semantic Runbook sections and makes them independently budgetable, but limits are hard-coded and score qualification remains ambiguous.
- The current worktree also appends prior-turn text to otherwise complete retrieval queries after removing validated standalone rewrite, reintroducing topic-drift risk.
- Qdrant RRF responses retain `FusionScore` but not the dense prefetch component. `dense=0` means unavailable, not zero cosine relevance.

#### 2. Goals

1. Cover every critical facet of a compound question with qualified evidence or an explicit gap.
2. Keep self-contained questions isolated from prior turns and resolve only genuine references with grounded entities.
3. Allow multiple distinct sections from one Runbook without filling a fixed quota.
4. Bound Runbook documents, chunks per document, total chunks, per-item tokens, and total tokens independently.
5. Calculate evidence space from an explicit model token window and validate every provider turn.
6. Observe prompt, tools, history, evidence, tool results, output reserve, and actual provider usage separately.
7. Give dense, fusion, local rerank, external rerank, blended, trust, and final-rank scores stable meanings.
8. Preserve O(n) aggregation and bounded sorting; never restore implicit full-document reads.

#### 3. Non-goals

- Increasing the evidence budget to conceal selection defects.
- Adding business-keyword branches for one failure.
- Inferring a public context limit from a private model alias.
- Running duplicate dense and sparse searches on every production request only for diagnostics.
- Replacing Qdrant, Voyage, BM25, or DashScope Rerank.
- Replacing constrained reference resolution with raw history concatenation.

#### 4. Model-aware token budget

Add explicit settings:

```text
llm_context_window_tokens
retrieval_evidence_cap_tokens
agent_tool_result_reserve_tokens
llm_context_safety_margin_tokens
```

Reuse the existing `llm_max_tokens`, `llm_answer_max_tokens`, and `agent_conclusion_max_tokens`. Reserve the larger of the answer and conclusion limits. These values belong to MySQL platform settings and must be validated at the settings write boundary.

Private aliases such as `deepseek-v4-pro` require an explicit window. Missing capability data must produce a clear configuration error rather than silently borrowing another model's defaults. The rune-based `context_budget` cannot be copied 1:1 into a token field; migrate it from an evaluated model profile, record old/new values, then remove the legacy key.

The QA orchestration boundary owns the complete prompt, tool policy, and output reserve before retrieval starts. It computes an immutable request budget and passes the evidence allowance explicitly to Retriever; it must not mutate shared `PlatformSettings`.

```text
fixed_input_tokens =
    system_prompt_tokens
  + identity_and_plan_tokens
  + tool_schema_tokens
  + conversation_tokens
  + question_tokens
  + message_envelope_tokens

available_evidence_tokens =
    model_context_window_tokens
  - fixed_input_tokens
  - output_reserve_tokens
  - agent_tool_result_reserve_tokens
  - llm_context_safety_margin_tokens

effective_evidence_tokens = min(
    retrieval_evidence_cap_tokens,
    available_evidence_tokens
)
```

An effective budget at or below zero fails before retrieval with a component breakdown.

Before every provider turn, validate `estimated_current_input + current_turn_max_output + safety_margin <= model_context_window`. Tool results consume the reserved budget at the ToolExecutor boundary. Once exhausted, the Agent stops calling tools. Any compaction keeps tool-call/result messages as complete pairs.

Use the model tokenizer when available. Until the gateway exposes the actual DeepSeek tokenizer, use an observable conservative estimator: `ceil(ASCII bytes / 3)`, two tokens per non-ASCII rune, plus a 10% message overhead. Extend `ChatStreamResult` with optional actual input/output/reasoning usage. Reasoning-delta counts remain diagnostics and never masquerade as provider usage.

For a confirmed 128K window, an initial configuration is:

```text
llm_context_window_tokens          = 131072
llm_answer_max_tokens              = 8192
agent_conclusion_max_tokens        = 8192
retrieval_evidence_cap_tokens      = 10000
agent_tool_result_reserve_tokens   = 16000
llm_context_safety_margin_tokens   = 12000
```

For a 64K window, use a 6K-8K evidence cap, 10K-12K tool reserve, and at least 6K safety margin. Do not expand evidence merely because the model window is larger.

#### 5. Prompt convergence

Keep only permanent invariants in the always-on prompt: evidence authority, verified call chains for behavior, separation of client entry points and branches, noise rejection, requested-facet coverage, one targeted lookup for a critical gap, user-language output, and non-disclosure of internal blocks.

Move Mermaid details, long examples, and response templates behind `ResponseMode`. Tool-mode text owns tool choice and stopping rules but does not repeat base grounding policy.

Before final output, produce a structured `covered | missing` status for independently requested facets. If a critical facet is missing and a step remains, perform one targeted lookup for that facet. Do not rephrase and repeat the original broad search. The status is control data, not exposed chain of thought.

#### 6. Bounded multi-section Runbook selection

Top three is a maximum, not a fill target:

```text
runbook_max_documents       = 5
runbook_max_chunks_per_doc  = 3
runbook_max_chunks_total    = 10
runbook_max_chunk_tokens    = 1200
runbook_min_semantic_score  = migrated existing threshold
```

Validate positive values, `max_documents <= max_chunks_total`, and a global hard ceiling.

Qualification uses raw Qdrant `semantic_score`. A bounded trust bonus may affect cross-document rank but cannot rescue a chunk below the semantic threshold. Within one document, order by raw semantic score. The current worktree still mixes adjusted `Score` and `SemanticScore`; Phase 1 must close that contract gap.

Deduplicate by `document_key + "\x00" + section_key`; prefer Runbook ID, then canonical path, and prefer normalized section header, then stable chunk ID. Legacy data may use a stable text hash, never full text as a map key.

Select in two bounded passes:

1. Without facets, compute `doc_score = max(section.final_rank_score)` and retain at most the configured document count.
2. Give each selected document its best section.
3. Put each document's remaining second and third sections into one score-ordered expansion pool.
4. Fill until per-document or global chunk limits are reached.

Aggregation and membership are map-based. Sorting is limited to the bounded candidate set. Selected chunks remain independent evidence items; never merge them into one document string that lets the first long section truncate later qualified sections.

Query preprocessing may emit at most three semantically independent facets without introducing new entities. Facet claims participate before the document top-N cut so a facet-only document cannot be discarded early. Without reliable facets, selection falls back to section deduplication and the two-pass algorithm rather than keyword heuristics.

Keyword fallback reports `score_kind=keyword` and `semantic_score=n/a`. It promises document-level recall only, reads bounded summaries or explicit passages after metadata filtering, and never claims section/facet coverage or shares cosine thresholds.

#### 7. Hybrid-score observability

Trace fields have stable contracts:

```text
recall_score_kind
dense_score           // null / n/a when unavailable
fusion_score
keyword_score
rerank_kind
local_rerank_score
external_rerank_raw_score
external_rerank_normalized_score
blended_score
trust_bonus
final_rank_score
threshold_field
selected
drop_reason
```

RRF output with no retained dense component reports `dense_score=n/a`, never zero. Production does not issue an extra query to fill diagnostics. A sampled diagnostic mode may run dense and sparse searches separately and join ranks by point ID.

Keep the rerank stages explicit:

```text
external_rerank_normalized = external_rerank_raw / max_external_rerank_raw
normalized_recall = recall / max_recall
blended_score = 0.7 * external_rerank_normalized
              + 0.3 * normalized_recall
final_rank_score = blended_score + trust_bonus
```

Local fallback has `rerank_kind=local` and never populates external fields. Threshold configuration must name the field it evaluates. Drop reasons include `min_score`, `document_cap`, `section_cap`, `facet_cap`, `diversity_cap`, `top_k`, `duplicate`, and `context_budget`.

#### 8. Delivery

##### Phase 1: finish the Runbook foundation

Already present in the worktree:

- Multiple distinct semantic Runbook sections.
- Document, per-document, and global chunk bounds.
- Independently budgetable chunks.
- Pre-collapse candidate diagnostics.

Remaining: restore the single canonical-query invariant, correct qualification and rank fields, move bounds to platform settings, add selector/fallback tests, pass package/build/vet acceptance, and replace permanent per-candidate info logs with sampled structured traces.

This phase does not add facets or change the model budget or index schema.

##### Phase 2: prompt and token budget

- Compress base and Agent prompts.
- Add model-window and reserve settings.
- Introduce one TokenEstimator boundary.
- Compute request evidence space at the QA orchestration boundary.
- Validate the ledger before every provider turn.
- Expose optional provider usage.
- Migrate and remove rune-based context and per-Runbook caps.

##### Phase 3: multi-intent retrieval and scores

- Emit bounded query facets.
- Merge multi-query recall with facet metadata.
- Apply facet claims before document top-N selection.
- Add coverage state and at most one targeted lookup.
- Split recall, rerank, blend, trust, and final score fields.
- Sample estimated versus actual provider token usage.

Phase 3 depends on the Phase 2 tool-result reserve; the coverage gate does not belong in Phase 1.

#### 9. Tests

- Multiple qualified sections from one document survive; duplicate windows from one section do not.
- Low-scoring sections never enter merely to fill three slots.
- Document coverage occurs before section expansion, and all hard limits hold.
- Self-contained questions do not inherit prior turns; reference-dependent questions introduce only grounded conversation entities.
- Two facets matching different sections of one document both reach context.
- Facet-only documents survive document selection; missing and budget-dropped facets differ.
- A missing facet remains an explicit gap; single-intent queries are not forcibly split.
- Prompt, tools, history, evidence, tool results, answer reserve, and margin never exceed the model window.
- RRF logs `dense=n/a`; dense-only logs cosine similarity.
- Keyword fallback stays document-level.
- External raw, normalized, blended, local, trust, and final rank scores are independently testable.

#### 10. Acceptance criteria

1. Critical-facet coverage improves without lowering context precision.
2. Self-contained retrieval is unaffected by a long prior turn, while genuine references still resolve correctly.
3. Multiple sections from one Runbook remain bounded in reads, candidates, and tokens.
4. Relevant sections lost to document collapse are recovered without increasing the evidence cap.
5. Always-on prompt tokens fall by at least 50% with no grounding regression.
6. Every provider turn can explain allocation across prompt, tools, history, evidence, tool results, output, and margin.
7. Unavailable dense components are never represented as zero.
8. Invalid model limits or Runbook bounds fail at the settings boundary.
9. Groundedness, facet coverage, and visible-reference rate improve on a fixed evaluation set without material latency or tool-step regression.
10. Semantic-backend failure degrades only to explicit document-level keyword results.
11. Relevant tests, `go build ./...`, and `go vet ./...` pass; shared concurrency changes also pass targeted race tests.

Deliver phases independently. Shadow-evaluate old and new Runbook evidence sequences before Phase 1 release, confirm the actual DeepSeek V4 Pro gateway limits before Phase 2, and sample detailed score diagnostics before enabling them broadly.
