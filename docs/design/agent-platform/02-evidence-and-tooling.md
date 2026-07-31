# Evidence Planning, Tools, and Runtime Investigation

[English](02-evidence-and-tooling.md) | [中文](02-evidence-and-tooling.zh-CN.md)

> Status: core behavior implemented; exact-answer contracts and runtime-investigation quality are being strengthened
> Sources: Evidence Planning, Runtime Evidence, Runtime Investigation, Web Evidence, Required Evidence Incident, Tool Selection and Multi-turn Evidence

## 1. Core Model

Evidence planning answers where a turn may acquire evidence. The tool registry answers what the model can actually call. These decisions remain separate.

```text
Question
  -> EvidencePlan      // sources and preferences
  -> Registry Snapshot // available tools
  -> Model decision    // whether and how to call
  -> Execution guard   // permission and argument validation
  -> Evidence ledger  // success, failure, partial, omitted
```

## 2. EvidencePlan

`EvidencePlan.Sources` is a capability bit set:

| Source | Authoritative scope |
|---|---|
| Memory | Stable preferences, identity, and confirmed long-lived facts |
| Internal | Code, docs, services, APIs, ontology, and call chains |
| Runtime | Logs, configuration, traces, alerts, and live state |
| Web | External pages, current public information, and official sources |

A planner may return preferred tool IDs and diagnostics, but it must not:

- hide tools permitted by the registry snapshot;
- promote probability into a required call;
- replace execution-boundary permission checks;
- treat memory as current runtime truth;
- silently switch evidence providers after planning failure.

## 3. Tool Visibility and Selection

Visible tools are the intersection of:

```text
Registry definitions ∩ runtime configuration ∩ request permissions
```

The snapshot is immutable within a run. The model may follow routing preferences, choose another visible tool, answer directly from sufficient evidence, or re-query when time, parameters, or targets change.

In ordinary QA, ignoring a routing preference is not failure. A required call only comes from a deterministic caller contract such as an explicitly required prefetch.

## 4. Tool Execution Contract

Every tool provides:

- stable ID, description, and JSON Schema;
- read-only or write/side-effect classification;
- argument validation and canonicalization boundary;
- bounded query semantics;
- result coverage: complete/partial, omitted count, and next cursor;
- observable errors without hiding configured-backend failure.

Recommended result shape:

```go
tool.Result{
    Content: authoritativeContent,
    Coverage: tool.EvidenceCoverage{
        Partial:      partial,
        OmittedItems: omitted,
    },
    AnswerContract: tool.AnswerContract{
        RequiredLiterals: requiredValues,
    },
}
```

An AnswerContract belongs to the specific execution result. Tools without exact-output requirements register no contract.

## 5. Runtime Evidence

Runtime evidence remains distinct from code knowledge:

- logs prove only events observed in a stated window;
- configuration includes environment, namespace, and redaction state;
- traces retain trace/span identity and time range;
- alerts prove what the alerting system observed;
- zero results do not automatically mean the condition never existed.

```text
identify target and time range
  -> locate runtime endpoint
  -> execute bounded query
  -> validate coverage and timestamps
  -> correlate logs/config/trace
  -> state conclusion with confidence and gaps
```

Expected behavior in source code cannot substitute for production evidence, and configuration from one environment cannot be attributed to another.

## 6. Web Evidence

Web capability separates search from fetch:

1. Search returns candidate URLs, titles, snippets, and provenance.
2. Fetch reads page content within size, media-type, and timeout boundaries.
3. Local passage ranking selects high-signal sections.
4. Answers distinguish page content, model inference, and time-sensitive facts.

A configured web provider failure remains visible and never silently switches providers. Search snippets are not page bodies; load-bearing facts require fetch or an authoritative page.

## 7. Multi-turn Evidence

A completed turn persists the real protocol sequence:

```text
user
assistant(tool_calls)
tool result × N
assistant(final)
```

The next turn may reuse still-valid recent evidence, re-query time-sensitive or changed conditions, recover archived exact content through history/artifacts, or call another tool. It must not infer prior tool calls from keywords in the final answer.

Tool calls and results replay as pairs; a bounded history window cannot start with an orphan tool result.

## 8. Required-Evidence Empty-Answer Incident

A historical implementation promoted routed candidates to required tools. It discarded an already-generated answer when a tool was not called, then skipped finalization when the step budget ended, producing an empty answer.

Consolidated rules:

- routing expresses preference only;
- required calls come only from explicit deterministic contracts;
- an unmet required call may extend attempts but cannot bypass finalization;
- a tool error counts as a real attempt and must be disclosed;
- except for cancellation or final-LLM failure, missing evidence must not produce an empty run.

## 9. Exact-Answer Contracts

For serial numbers, order IDs, device IDs, and other values that cannot be abbreviated:

```text
Tool result
  -> collect required literals
  -> provide exact evidence to model
  -> generate candidate answer
  -> deterministic validation
  -> repair retry
  -> reject if still invalid
```

The contract does not replace evidence. If model-visible content lost a required literal, the runtime must restore it, page the tool, or return a retryable tool-delivery failure instead of asking the model to guess.

Large sets should use a server-side validator backed by an artifact rather than injecting thousands of literals into the prompt.

## 10. Acceptance Criteria

1. Router failure does not change the permission-approved tool snapshot.
2. An unused preferred tool does not automatically block an answer.
3. Every required call has a deterministic source.
4. Runtime and web conclusions include time, environment, and coverage.
5. Tool error, partial, and omitted states constrain the final answer.
6. Multi-turn replay preserves call/result pairing.
7. Exact-contract failure cannot deliver abbreviated or incomplete output.

## Detailed Consolidated Material

### Agent Evidence Planning

> Migrated from CodeLoom `docs/design/agent/agent-evidence-planning.md`; incorporated into this module on 2026-07-31.

Status: Current

Evidence planning is the Agent's sole policy for deciding which evidence capabilities may execute. It is independent of answer presentation.

#### Two Independent Decisions

```text
question
  -> ResponseMode: how the answer should be structured
  -> EvidencePlan: which evidence capabilities may execute
```

`ResponseMode` is a local answer-shape hint such as `bug_analysis`, `requirements_analysis`, `architecture_review`, `code_review`, or `codebase_qa`. It never grants evidence or tools.

`EvidencePlan.Sources` is a bit set:

| Source | Authority |
| --- | --- |
| `Memory` | Durable user identity, preferences, work habits, and historical context |
| `Internal` | Current indexed workspace, services, APIs, code, configuration, schemas, call chains, and runbooks |
| `Web` | Current external documentation, standards, products, and public facts |

Scenario evidence such as logs and traces is not a core source bit. The scenario composition publishes read tools with RoutingSpec through the restricted registrar; the same planning call selects core sources and matched read ToolIDs before definitions and handlers are pinned in the Run snapshot.

`Sources == 0` means Direct. Any source combination is valid; a combination needs no additional Mixed enum.

#### Plan And Diagnostics

```go
type EvidencePlan struct {
    Sources EvidenceSources
}

type PlanDecision struct {
    Plan       EvidencePlan
    Confidence float64
    Origin     DecisionOrigin // model, rule, explicit, fallback
}
```

The plan answers only what may execute. Confidence and origin are observable diagnostics and must not independently enable tools.

#### Planning Protocol

In automatic mode, the fast LLM returns the core source decision and, when gated reads exist, a candidate-validated ToolID selection:

```json
{"route":{"sources":["internal","web"],"confidence":0.92},"tools":{"tool_ids":["observe_logs"]}}
```

The planner receives the current question and bounded conversation context. It selects every independently required authority even when a runtime capability is unavailable. Availability is checked afterward so a missing prerequisite stays visible instead of silently changing the evidence basis.

Explicit API selection is parsed into a fixed plan with `origin=explicit`. Canonical selections are `direct`, `memory`, `internal`, `web`, and `all`; `auto` requests model planning at the API boundary.

Meta questions that are fully answerable from supplied context may use a rule-origin Direct plan. Planner failure uses an observable Internal fallback. A low-confidence model Direct decision also falls back to Internal because an unsupported workspace answer is riskier than an extra internal lookup.

#### Execution Semantics

| Selection | Execution |
| --- | --- |
| `Memory` | Recall a bounded memory set and inject it as system context |
| `Internal` | Run workspace retrieval before the loop |
| `Web` | Enable `web_search` and `web_fetch`; Web remains Agent-driven |
| Direct | Skip core pre-retrieval; always-visible built-in reads remain callable on demand |

Memory remains a distinct Trace node. Web is iterative because search results must be inspected before a page is fetched. The candidate snapshot is pinned before planning; unmatched RoutingSpec reads are removed from the Run snapshot. Matched reads remain callable and may be explicitly prefetched through a trusted ToolPlan. Core Retrieval never invokes a scenario service directly.

The effective plan selects core prompt and retrieval scope. The derived immutable snapshot independently constrains registered tools, so a hidden tool name cannot resolve. Write capability remains outside EvidencePlan and scenario read registration; it requires the closed platform catalog, request authorization, and human approval.

#### Memory Truth Boundary

Memory is not a project knowledge cache. A mutable statement such as “the user-center service is X” must be resolved through Internal. An old memory may explain history but cannot override current indexed evidence. Runtime claims require a registered scenario tool; external claims require Web.

Until versioned memory exists, Memory should be selected only for durable user context. See [Long-term memory](07-memory.md).

#### Observability And Evaluation

The `evidence_plan` trace node records response mode, proposed/effective plans, sources, confidence, origin, and planning/fallback errors. `memory_recall` is separate so its latency and quality remain independently measurable.

Evaluate:

- exact source-set match and per-source precision/recall;
- false-Direct rate for questions requiring evidence;
- required-but-unavailable source visibility;
- invoked retrieval/tools being a subset of the effective plan;
- evidence recall, precision, latency, and budget;
- groundedness, completeness, citation correctness, and first-token latency.

Datasets label the required authority set rather than one coarse route, so multi-source questions remain directly evaluable.

#### Invariants

1. `EvidencePlan` is the only core Memory/Internal/Web execution policy; RoutingSpec plus the pinned snapshot govern scenario reads.
2. Direct is an empty source set, not a source.
3. Multi-source is a combination, not a mode.
4. Diagnostics never grant capability.
5. Required-but-unavailable sources remain visible.
6. Write authorization remains independent.
7. Memory never outranks current evidence for mutable facts.

### Agent Runtime Evidence And Incident Design

> Migrated from CodeLoom `docs/design/agent/agent-runtime-evidence-design.md`; incorporated into this module on 2026-07-31.

Status: Current for named sources, Kibana logs, SkyWalking traces, and the basic incident workflow

Observe supplies time-bounded runtime evidence to the Agent. Incident management consumes the same Observe capability to persist investigations and, when explicitly approved, initiate a fix workflow. They share evidence but remain separate authorization domains.

#### End-To-End Relationship

```text
observe_sources (MySQL)
  -> canonical source configuration
  -> ObservePlan
  -> provider-neutral query
  -> Kibana logs / SkyWalking traces
  -> normalized runtime evidence
       ├─ QA pre-retrieval and observe_logs tool
       └─ incident investigation
            -> persisted analysis
            -> optional notification
            -> closed platform write proposal and human approval
            -> branch/commit workflow
```

`EvidencePlan` containing Observe authorizes runtime reads for QA. It does not authorize incident mutation or repository writes.

#### Named Observe Sources

A source contains a stable source key, display name, endpoints/index patterns, enabled/default/priority flags, service patterns, authentication, trace connection, and `fields_config`.

Gateway candidates are source-owned configuration persisted in the existing `fields_config` JSON, without adding a database column. Every candidate owns a required index scope. When candidates exist, the general index pattern does not participate in `observe_logs` gateway queries and cannot serve as a fallback after failed resolution. Sources without gateway candidates continue to use their general index patterns. The current scope does not register gateway or endpoint management tools.

`fields_config` keeps each stable Observe field's Agent description, provider
path, query behavior, and extraction rule together with the log/trace provider
identifiers. Stable names such as `timestamp`, `url`, `trace_id`, and `user_id`
remove the previous role and provider-binding indirection. Query analysis and
`QueryTerms` remain one fixed, code-owned contract in Nasuta.

See the `CodeLoom internal/config/observe_fields.example.md`
for each property, stable field, and parsing rule.

Create/update canonicalizes and validates the configuration at ingress. Runtime code consumes the canonical model directly. List responses never return credentials; the authenticated edit contract returns only secret state required by that edit flow.

#### Planning And Source Resolution

Observe executes only when selected by the effective Agent evidence plan.

For a question containing an email or customer user ID, the customer directory returns both the canonical user ID and owning region, which Observe maps to exactly one source. Kibana is never used to probe geography. An unconfigured directory source or conflict with an explicit source fails visibly. Otherwise resolution follows an explicit source, a strict service-pattern match, or configured defaults; a non-empty unmatched service cannot fall back to the default source.

The fast LLM produces code-owned `QueryTerms` with fixed `DomainTerms` and `Identifiers` fields during evidence planning. `observe_logs` derives each filter branch from configured field descriptions, types, and operators. Invalid filters fail visibly and cannot inject arbitrary physical provider fields.

#### QA Execution Entrances

##### Pre-retrieval

QA builds an `ObservePlan` from source, configured filters, full text, service, email/user ID, and time window. `Retriever.RetrievePlan` runs Observe alongside Internal retrieval and assembles normalized evidence into the initial Agent context.

##### Agent Tool

`observe_logs` exposes the configured field schema and accepts `email` and `user_id` directly at the tool root, matching the dashboard request contract. Each Source can declare gateway candidates with independent index scopes. The tool presents candidates by Source, and the model may select `gateway` only from retrieved architecture or call-chain evidence, never from URL shape. Without an explicit gateway, only `service + url_groups` can trigger Config Center resolution; failure stops before Kibana. A query accesses only the selected gateway, never the general index scope, and does not retry another gateway after an empty result. URL, configured filters, non-zero response codes, and trace IDs are preferred structured scopes. `full_text` is a message-field fallback only when none is available and cannot be combined with them. Tool arguments are parsed into the same `ObservePlan` and use the same source resolution and direct field configuration. It does not enter a Run snapshot unless its RoutingSpec matches. Every execution starts with Kibana. Non-zero normalized response codes are automatically enriched from SkyWalking, while successful or untyped logs are not; an explicit `trace` object forces enrichment regardless of code. Optional `trace.id` must pass provider validation, only filters Kibana, and cannot bypass logs to query SkyWalking directly. Bounded log hits retain the normalized `request`, `response`, and `message` names and expose the effective `query_scope` instead of introducing an Agent-only payload wrapper.

Relative time is normalized by the multilingual routing model into a grounded language-neutral expression; the model never calculates absolute dates. Nasuta resolves that expression from one server-time anchor in the service process's local time zone and passes the authoritative half-open interval to time-aware tools. `recent` means 24 hours, and an unnumbered equivalent of "recent days" means 5 rolling days. Explicit tool timestamps cannot override a range resolved from the user's wording. Without a relative or explicit range, Observe defaults to 24 hours at its ingress; Kibana has no independent time fallback.

Pre-retrieval does not call the Agent tool; both paths share the Observe domain service below their adapters.

#### Provider Boundary

`LogProvider` accepts `ProviderLogQuery`; `TraceProvider` accepts trace identifiers and bounded windows. Current explicit dispatch supports Kibana for logs and SkyWalking for traces. Unknown or unavailable providers return errors and are never emulated by another backend.

Kibana executes provider filters, retrieves bounded hits, extracts normalized fields, and aggregates API latency/error summaries. Automatic enrichment sends only non-zero-code hits with provider-validated trace IDs to SkyWalking; explicit trace enrichment may send all matching hits. Unless `trace.limit` is explicit, at most 5 traces are queried. When enrichment is required, a missing trace provider, absent valid trace IDs, or failed lookup is surfaced through `trace_error`. Ordinary successful or untyped log queries do not report a Trace failure. Agent-facing output is compacted to bounded hit and trace summaries rather than returning complete payload and span collections.

Future Loki, Elasticsearch-direct, Jaeger, or Tempo support requires explicit provider implementations and dispatcher cases. Reusable field profiles shared across sources are also not implemented.

#### Incident Workflow

```text
alert webhook or manual report
  -> normalize alert and affected services
  -> deduplicate against open incidents
  -> persist analyzing state
  -> analyze bounded alert window
       ├─ reuse supplied logs or query Observe
       ├─ fetch bounded traces
       ├─ derive deterministic analysis baseline
       ├─ optional configured-LLM analysis with code hints
       └─ build analysis Markdown
  -> persist open state
  -> optional Feishu/WeCom/HTTP notification
```

MySQL stores the alert payload, evidence, affected services, analysis, assignment, fix branches, and lifecycle timestamps. Open incidents deduplicate by normalized title, services, and alert window.

The deterministic baseline identifies trace errors or aggregated slow APIs. Optional LLM analysis may refine root cause and solution, but failure retains the readable deterministic report. Code search supplies bounded evidence hints and never authorizes a write.

#### Incident API And Write Boundary

Current APIs provide secret-checked alert ingress, manual creation, list/detail/delete, direct authenticated fix start, and fix confirmation.

Agent-initiated `propose_branch` and `propose_commit` calls from the closed platform catalog create pending actions. Human approval dispatches the exact persisted action; upper-layer scenarios cannot register new write contracts. See [Approval-gated write tools](09-write-safety-and-approval.md).

The fix implementation currently:

1. requires a clean shared worktree;
2. fetches the base branch and creates/resets a fix branch;
3. writes `.nasuta-fix.md` with the analysis and recommendation;
4. on confirmation, commits current changes, pushes, and optionally creates a GitLab MR.

This is not an autonomous code-fixing agent. Direct incident UI endpoints remain authenticated operations separate from pending-action approval.

#### Security, Degradation, And Limits

- Query windows, hit counts, trace counts, provider calls, and tool execution are bounded.
- Missing Observe disables runtime evidence only; selected unavailability remains logged and traced.
- LLM absence leaves the deterministic incident analysis intact.
- Notification delivery is best effort and has no durable retry queue.
- Incident fix uses the shared checkout; isolated per-action worktrees are not implemented.
- Concurrent approval exactly-once execution and durable background jobs remain future work.
- Incident listing is bounded rather than cursor-paginated.

#### Evaluation

For Observe, measure source-selection accuracy, identity routing, field configuration correctness, time-window correctness, provider latency/error, normalized-field completeness, trace join rate, and answer grounding.

For incidents, measure alert deduplication, relevant evidence capture, root-cause groundedness, missing-capability behavior, notification outcome, approval audit completeness, and mutation idempotency.

#### Invariants

1. Observe reads require an Observe evidence plan in QA.
2. Observe evidence does not grant incident or write authorization.
3. Configured providers are explicit and never substituted.
4. Missing evidence produces a visible gap, not fabricated certainty.
5. Agent-initiated mutations require pending-action approval.
6. A dirty worktree blocks fix-branch creation.

### Agent Web Evidence Design

> Migrated from CodeLoom `docs/design/agent/agent-web-evidence-design.md`; incorporated into this module on 2026-07-31.

Status: Current

Web evidence uses two Agent read tools. `web_search` finds candidate pages; `web_fetch` converts a selected page into bounded, question-relevant evidence. The tools are exposed only when Web is selected by `EvidencePlan` and the capability is configured.

#### Search

`NASUTA_WEB_SEARCH_ENABLED` controls tool registration and `NASUTA_WEB_SEARCH_MCP_ENABLED` independently controls MCP exposure. `NASUTA_WEB_SEARCH_ENGINE` selects a registered provider. Nasuta registers DuckDuckGo, Brave, and Bing by default; built-in credentialed providers read `NASUTA_WEB_SEARCH_API_KEY` and return an explicit error when it is missing. Applications can add or replace providers through `RegisterWebSearchProvider` without changing the central dispatcher.

```text
query
  -> engine dispatcher
  -> provider request and parser
  -> at most 10 title/URL/snippet candidates
```

Search snippets are leads, not sufficient support for material claims. The Agent must fetch authoritative pages before relying on details.

The dispatcher resolves the canonical name through the provider registry. Unknown names return a configuration error instead of silently substituting another provider.

#### Fetch Boundary

`web_fetch` accepts absolute HTTP(S) URLs and uses an SSRF-safe client. The request has a 15-second timeout and the response body is byte-limited.

```text
HTTP body
  -> determine charset from BOM, Content-Type, or document metadata
  -> convert once to valid UTF-8
  -> remove scripts, styles, navigation, headers, footers, and side content
  -> convert readable HTML to Markdown
  -> split headings and paragraphs into candidates
  -> reject duplicates and link-heavy blocks
  -> rank locally against the current question
  -> select diverse passages within 8,000 characters
```

Charset conversion is the untrusted ingress boundary. Downstream extraction, ranking, logging, and model input can trust valid UTF-8. Truncation is rune-safe.

#### Local Passage Ranking

Each page becomes a temporary passage corpus:

```text
score = body_bm25
      + 0.8 * section_heading_overlap
      + 0.3 * page_title_overlap
```

The selector uses a bounded top-candidate heap, limits repeated passages under one heading, and stops at the character budget. Complexity is linear apart from `O(n log k)` top-K maintenance with bounded `k`.

Passage selection uses no embedding or LLM call. This keeps latency, cost, failure behavior, and evaluation deterministic. A semantic reranker is justified only if a passage dataset reveals a general paraphrase recall gap.

Without a query, fetch returns a rune-safe prefix of the cleaned document. The Agent tool schema normally supplies the current question so QA receives ranked passages.

#### Agent Loop Behavior

Web is not prefetched because result inspection and page selection are iterative. The loop tracks fetched evidence and adds convergence hints when repeated searches or pages stop producing useful progress. Web-only uses the compact research prompt; mixed plans can combine Web tools with Internal or Observe evidence.

#### Evaluation And Invariants

Measure provider latency/errors, invalid-engine handling, fetch success, invalid UTF-8 count, extraction failures, passage `Recall@3/5`, selected characters, duplicate rate, and final-answer groundedness.

1. Providers are explicitly dispatched and never substituted.
2. Network input becomes valid UTF-8 once at ingress.
3. Page content is bounded before model injection.
4. Snippets do not replace fetched evidence.
5. QA capability and MCP exposure remain independent controls.
