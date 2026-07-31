# Agent Architecture, Execution Flow, and Run Convergence

[English](01-architecture-and-execution.md) | [中文](01-architecture-and-execution.zh-CN.md)

> Status: consolidated baseline; core flow implemented, state representation still converging
> Sources: Agent End-to-End Execution Flow, QA Run State Convergence

## 1. Responsibility

This module defines the complete QA run lifecycle, from request ingress to final-answer persistence, and the facts from which run state is derived. It does not own retrieval algorithms, business tools, or long-term memory policy.

```text
HTTP/SSE request
  -> session and permission boundary
  -> question analysis and evidence plan
  -> bounded context assembly
  -> immutable tool snapshot
  -> reason -> act -> observe loop
  -> reserved final-answer phase
  -> stream, persist, trace
```

## 2. Ownership Boundaries

- `internal/transport`: request parsing, authentication, SSE, and DTO conversion; no agent policy.
- `internal/agent`: orchestration, tool loop, timeouts, final answer, and session messages.
- `internal/retrieval`: bounded evidence candidates, not final-answer decisions.
- `tool.Registry`: the visible tool snapshot for one run.
- Session/store: atomic persistence of user messages, tool calls, tool results, and final answers.
- CodeLoom: application tools and business evidence sources without moving policy into Nasuta.

## 3. Ingress and Immutable Snapshot

A run fixes the following at ingress:

- user and session identity;
- current question and attachments;
- registry version and tool snapshot;
- permissions and write policy;
- provider, context window, and answer-token settings;
- EvidencePlan and ResponseMode.

Routing results, a tool failure, or the previous turn's tool choice must not mutate tool definitions during the run. Hot configuration changes affect the next run only.

## 4. Question Analysis

Question analysis produces two independent values:

```text
ResponseMode: how the answer should be organized
EvidencePlan: which evidence sources may be acquired
```

ResponseMode is presentation guidance and grants no permission. EvidencePlan describes availability and priority across Memory, Internal, Runtime, and Web sources; it does not force a particular tool call.

## 5. Context Assembly

Model input is assembled once in priority order:

1. system contracts and current user question;
2. fresh tool evidence from the current run;
3. strongly related recent atomic turns;
4. on-demand archived-history recall;
5. internal retrieval evidence for the current question;
6. admitted long-term memories.

Assembly uses a provider-aware token budget. Each partition has a cap, and the total budget is decremented in one pass instead of repeatedly rescanning and trimming.

## 6. Tool Loop

```text
LLM response
  -> final answer: validate and finish
  -> tool calls: execute against the same snapshot
       -> append paired assistant/tool messages
       -> update evidence and coverage
       -> continue
```

Invariants:

- assistant tool calls pair with results by `tool_call_id`;
- tool errors remain visible and are never represented as success;
- each call records duration, content, coverage, and failure independently;
- the step limit bounds execution but never justifies skipping finalization;
- current-run tool evidence outranks historical summaries.

## 7. Timeout and Final Answer

```text
Tool-loop deadline = Timeout - AnswerReserve
Final-answer deadline = reserved remainder
```

When the loop budget ends, the agent enters a tool-free finalization phase. The final answer must:

- identify acquired evidence;
- disclose failures, omissions, and uncertainty;
- never claim an uncalled tool was executed;
- never become empty merely because required evidence is missing;
- pass exact-output contracts when active;
- recover length truncation through continuation or return an explicit error.

## 8. Run State

Run state is derived from facts rather than persisted as an independent state machine:

- whether a final answer exists;
- cancellation or timeout;
- unanswered tool calls;
- provider `finish_reason`;
- answer-contract result;
- persistence result.

UI states such as `running`, `completed`, `failed`, and `aborted` are projections. Linear labels such as “planning,” “summarizing,” or “waiting for tool” do not justify recoverable persistent states.

## 9. Streaming and Persistence

- reasoning, tool calls, and final content use distinct events;
- once a tool-call delta starts, preceding candidate answer text is no longer user-deliverable;
- only a validated answer is persisted as the final deliverable;
- one logical turn is atomically stored as `user -> assistant(tool_calls) -> tool results -> assistant final`;
- completed tool results remain persisted after cancellation so pairing is preserved;
- session reads are bounded at storage with `LIMIT`, never fetch-all-then-slice.

## 10. Failure Semantics

| Failure | Behavior |
|---|---|
| Optional capability absent | Warn and disable only that capability |
| Configured provider unavailable | Fail clearly; never substitute providers |
| Tool execution failure | Return as tool result and constrain conclusions |
| Tool-loop budget exhausted | Use AnswerReserve to finalize |
| Empty model response | Bounded retry, then explicit error |
| Length-truncated response | Continue, or fail if unrecoverable |
| Answer contract violation | Repair retries, then reject incomplete output |
| Session persistence failure | Observable error; never claim persistence succeeded |

## 11. Acceptance Criteria

1. Tool schemas and permissions remain stable within a run.
2. Tool failure and step exhaustion cannot bypass finalization.
3. Tool call/result pairs always satisfy provider protocols.
4. A contract-invalid candidate is neither streamed nor persisted as final.
5. Run status can be reconstructed from persisted facts.
6. Online session reads remain bounded.
7. Traces reconstruct ingress, planning, tools, finalization, and persistence.

## Detailed Consolidated Material

### Agent End-to-End Execution Flow

> Migrated from CodeLoom `docs/design/agent/agent-execution-flow.md`; incorporated into this module on 2026-07-31.

Status: Current

This is the canonical request sequence for dashboard QA. MCP tool calls share the registry and stores but do not enter this conversational orchestration path.

#### 1. Sequence

```text
POST /api/qa/ask
  -> normalize question and source_mode
  -> load session summary + six recent messages
  -> create run ID and subscribe to SSE events
  -> analyze question
       ├─ ClassifyResponseMode (local answer-structure hint)
       ├─ decide EvidencePlan (model/rule/explicit/fallback)
       ├─ configured query preprocessing and terms
       └─ optional standalone rewrite in parallel
  -> recall Memory when selected
  -> pre-retrieve Internal/Observe when selected
  -> build EvidencePlan-derived prompt and tool policy
  -> reason -> act(tool) -> observe loop
  -> stream reasoning, steps, references, answer tokens, and trace
  -> persist the user/assistant turn
  -> asynchronously regenerate session summary
  -> asynchronously extract long-term memory after a successful run
```

#### 2. Ingress And Session Context

The dashboard boundary trims the question, canonicalizes `source_mode`, validates explicit evidence selection, and rejects an unavailable QA service before starting SSE.

For a stored session, the database query returns the summary and latest six messages directly. It does not load the full session and slice it in application memory. Caller-supplied history is only a fallback when no stored session is available.

The run subscribes to the hub before synchronous planning, otherwise early phase events would be lost.

#### 3. Question Analysis

##### Response Mode

`ClassifyResponseMode` uses local signals to choose the expected answer structure:

```text
bug_analysis | requirements_analysis | architecture_review | code_review | codebase_qa
```

It does not grant tools or choose evidence.

##### Evidence Plan

An explicit API plan bypasses model routing. Automatic planning asks the fast LLM for a set of evidence sources plus confidence. Planning, configured Observe preprocessing, and query-term extraction share one structured helper response where enabled.

A short-circuited meta question records a rule-origin Direct plan. Planning failure and low-confidence Direct use an observable Internal fallback. Required source availability is checked after the decision.

The full policy and evaluation contract is defined in [Agent evidence planning](02-evidence-and-tooling.md).

##### Standalone Rewrite

When recent context exists and the question contains anaphora, standalone rewriting runs concurrently with evidence analysis. Retrieval uses the rewrite when it resolves the reference; otherwise it combines the clean question with bounded prior context. The session summary is routing context, not automatically appended to every retrieval query.

#### 4. Evidence Acquisition

Evidence execution follows the effective plan exactly:

- Memory recall retrieves at most three semantic candidates scoped to the current user and global user `0`.
- Internal/Observe enter `Retriever.RetrievePlan`, which performs dispatch, discovery, expansion, and budgeted assembly.
- Web is not prefetched. The agent receives `web_search` and `web_fetch` only when Web is selected.
- Direct and Memory-only plans skip pre-retrieval.

An unavailable selected source is logged and represented in context/trace where applicable. The system does not silently switch to another provider.

Source-specific mechanics are defined in [Internal retrieval](03-retrieval-and-knowledge.md), [Web evidence](02-evidence-and-tooling.md), and [Runtime evidence and incidents](02-evidence-and-tooling.md).

#### 5. Prompt And Tool Loop

The agent compiles system instructions, persistent summary, recent verbatim messages, recalled memory, retrieved context, role prompt, and the current question. The platform pins a permission-filtered candidate snapshot, then derives the Run snapshot from RoutingSpec intent selection; model definitions and execution use that same immutable view.

Each loop turn:

1. checks pause, resume, nudge, or abort control;
2. streams one model turn with the full answer token budget;
3. records timing and the model/step output;
4. returns the answer when no tool call exists; or
5. executes permitted tools, records bounded results, and appends observations for the next turn.

Repeated identical tool calls are deduplicated. Search-result overlap produces a convergence hint. Tool results sent to the model are bounded independently of the full result retained in run steps.

#### 6. Timeout And Final Answer

The whole run uses `AgentConfig.Timeout`. The iterative loop uses `Timeout - AnswerReserve`, preserving time for `forceConclusion` if tools consume the loop budget or maximum steps.

The first visible content event records `first_answer_token` with both turn TTFT and run-elapsed TTFT. A length-truncated answer may continue. Unrecoverable reasoning truncation is surfaced as an error instead of returning an empty successful answer.

#### 7. Streaming And Persistence

SSE emits run lifecycle events, phases, reasoning, tool calls/results, answer tokens, references, trace nodes, and terminal status. Agent runs and steps are persisted in MySQL when the run store is configured.

After streaming finishes, the dashboard saves the user and assistant messages. It starts a bounded background task that reloads the session and regenerates one persistent summary. Separately, a successful non-aborted Agent run may extract cross-session memories in a bounded background context.

Session summary and long-term Memory are distinct:

- summary preserves progress for one session;
- Memory attempts to preserve reusable user context across sessions.

The current Memory limitations and target stale-fact handling are documented in [Long-term memory](07-memory.md).

#### 8. Evaluation Nodes

The QA trace can include:

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

Each node owns its input, output, status, and duration. Node-level evaluation can therefore distinguish slow planning, weak retrieval, repeated tool use, model TTFT, and final-answer quality.

#### 9. Invariants

1. Session context is bounded at the data layer.
2. Evidence planning and standalone rewriting may run concurrently, but retrieval waits for both.
3. `EvidencePlan` is immutable for the run and controls both pre-retrieval and tool permission.
4. Answer reserve cannot be spent by the iterative tool loop.
5. Background summary and memory extraction do not delay the streamed answer.

### QA Run State Convergence

> Migrated from CodeLoom `docs/design/agent/agent-run-state-convergence.md`; incorporated into this module on 2026-07-31.

Status: P0-P2 implemented (2026-07-18)

This document audits state across Dashboard QA request handling, evidence planning, retrieval, the Agent tool loop, SSE delivery, sessions, and long-term memory. It separates real state machines from immutable policy, event classification, and concurrency coordination.

#### 1. Conclusion

The ordinary QA path does not need more state machines. It needs only:

1. the model -> tool -> model -> answer execution loop;
2. the persisted `running -> done | failed | aborted` Run lifecycle;
3. optional pause/resume/abort/nudge control;
4. the independent Memory `active -> superseded` lifecycle.

LLM continuation is a provider-local protocol. WriteGate and Incident own separate domain lifecycles and must not be merged into read-only QA Runs.

The pre-change complexity came mainly from expressing the same Run terminal state, pause state, and completion signal through several variables, channels, stores, and UI projections. Ownership should be converged without adding facet-coverage, evidence-completeness, or retrieval-phase state machines.

Implemented result:

- `RunOutcome` derives done/failed/aborted once and rejects empty success;
- `RunStore.Complete` atomically moves only running/paused Runs to one terminal state;
- `RunHub` is the sole token/reasoning/step/trace/terminal channel;
- the unwritten `AskResult.TokenCh`, `ErrCh`, and transport fallback were removed;
- pause/resume use conditional persistence and abort releases a paused wait;
- startup recovery converges stale `running/paused` Runs to `aborted` once per process;
- duplicate unused `RunRecord` and `StepKind` domain types were removed.

#### 2. Classification

| Expression | Classification | Direction |
| --- | --- | --- |
| `EvidencePlan` | immutable per-run capability policy | keep as a value |
| `ResponseMode` | answer presentation classification | keep as a value |
| discover -> expand -> assemble | sequential retrieval pipeline | do not persist phases |
| `StepKind` | event/audit category | do not treat as Run state |
| SSE `Phase` | transient UI hint | do not persist |
| Evaluation Trace | evaluation telemetry | do not drive transitions |
| Agent loop | execution state machine | keep |
| `RunStatus` | persisted lifecycle | converge ownership |
| Run control | runtime control protocol | keep only if product-required |
| LLM continuation | provider-local protocol | encapsulate in generation |
| Memory status | independent domain lifecycle | keep |
| Web convergence and Trace buffering | bounded local coordination | keep local |

#### 3. Pre-change Problems

##### 3.1 Pause has two sources of truth

`RunStatus` and the UI declare `paused`, but production code does not call `RunStore.SetStatus`. Pause exists only in the in-memory `RunHub.paused` channel map. The database may remain `running`, the UI may never expose Resume, and restart loses pause state.

##### 3.2 Terminal state is expressed four times

Completion is represented by `RunResult`, persisted `RunStatus`, `RunHub` Done, and the Dashboard `hubSentDone` fallback. The fallback proves there is no single terminal publisher.

##### 3.3 The answer has two live channels

`AskResult` exposes `TokenCh` and `ErrCh`, while tokens, reasoning, steps, and Done also travel through `RunHub`. The QA-created `TokenCh` has no writer. Transport still merges both channels and carries their convergence state.

##### 3.4 Agent terminal outcome is derived from several booleans

`answered`, `Aborted`, `Err`, and context errors jointly determine completion. `answered` is loop-local, never persisted, and only gates `forceConclusion`. `FinishReason` is not among them — it lives on `llm.ChatStreamResult`, is consumed locally inside `continueIfNeeded`, and never surfaces on `RunResult` (it belongs to the provider-local continuation protocol in section 2).

This does not require another state enum, but terminal normalization must reject invalid combinations. Before this change, the loop could return `err==nil && answer=="" && !aborted` (`loop.go:303-316`, when the loop budget broke early or `forceConclusion` returned empty content), so empty success was an observed reachable defect, not a hypothetical case.

##### 3.5 Lifecycle types are duplicated

`internal/domain/agent.go` defines `RunRecord` and `StepKind`, while the Dashboard QA path uses another set in `internal/agent/service.go`. Only one package should own the lifecycle model.

#### 4. Target Model

```text
EvidencePlan   immutable capability policy
Agent loop     sole model/tool execution loop
RunStore       authoritative queryable lifecycle
RunHub         real-time projection, not a second lifecycle owner
Memory,
WriteGate,
Incident       independent domain lifecycles
```

The basic Run transitions are:

```text
running -> done
running -> failed
running -> aborted
```

Terminal Runs cannot transition again. Pause must choose one product contract:

1. persist `running <-> paused` through one lifecycle service; or
2. declare pause process-local and remove it from persisted RunStatus and UI status filtering.

`RunHub` should be the only live event channel for phase, reasoning, token, step, trace, and one terminal outcome. Phase, StepKind, and trace status remain projections, not Run states.

#### 5. States Not To Add

Do not add persisted state for:

- facet coverage;
- strong/weak/empty evidence;
- retrieval pending/completed;
- answer drafting/finalizing;
- UI phases;
- step kinds;
- trace nodes;
- Runbook selection phases;
- prompt-level chain completeness.

Chain completeness remains an answer-time evidence rule: perform one targeted lookup when possible, then name the exact remaining break.

#### 6. Implemented Slices

##### P0: remove pseudo-state (complete)

- decide whether pause is persisted or process-local;
- complete or remove `RunStatusPaused` and `SetStatus`;
- validate allowed Run transitions;
- define recovery for stale running/paused Runs after restart.

##### P0: use one live channel (complete)

- make `RunHub` the sole token/reasoning/step/terminal channel;
- remove the unwritten `AskResult.TokenCh`;
- carry asynchronous failure in the terminal outcome;
- remove transport dual-channel merging and `hubSentDone`.

##### P1: create one completion boundary (complete)

- normalize `RunResult + run error` into one `RunOutcome`;
- persist status, counts, and end time once;
- broadcast one terminal event after persistence;
- make duplicate completion idempotent;
- clean control and pause resources in the same boundary.

##### P2: remove duplicate models (complete)

After verifying callers, merge or delete the unused `RunRecord` and `StepKind` definitions in `internal/domain/agent.go`.

#### 7. Invariants

1. A Run reaches one terminal status once.
2. Every terminal Run has `ended_at`.
3. QA cannot record `done` with an empty answer.
4. An aborted Run cannot be recorded as done.
5. Only the completion boundary publishes terminal events.
6. SSE disconnect does not independently rewrite business state.
7. Phase, Step, and Trace do not modify RunStatus.
8. Pause semantics agree across storage, Hub, UI, and restart behavior.
9. Background Summary and Memory work do not change a completed Run.
10. Control signals cannot expand the immutable EvidencePlan.

#### 8. Acceptance

- one queryable Run lifecycle source;
- one real-time event channel and terminal publisher;
- no `hubSentDone` or unwritten Token channel;
- consistent pause semantics or no persisted pause state;
- no new facet, retrieval-phase, or evidence-completeness state machine;
- focused Agent/transport tests, `go build ./...`, and relevant race tests pass.

Deliver terminal normalization, live-channel convergence, and pause/type cleanup as separate reversible slices. Do not preserve permanent dual writes or compatibility states to hide unresolved ownership.
