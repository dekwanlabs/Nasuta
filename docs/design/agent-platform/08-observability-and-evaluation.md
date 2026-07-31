# Observability, Token Accounting, and Evaluation

[English](08-observability-and-evaluation.md) | [中文](08-observability-and-evaluation.zh-CN.md)

> Status: consolidated design; observability is implemented in part, exact provider token accounting is the target metric contract
> Sources: Agent Observability Design; QA LLM Token Accounting

## 1. Purpose

Agent observability answers three different questions through separate channels:

1. **Business record**: what durable steps did the run execute?
2. **Live experience**: what should a connected client see now?
3. **Evaluation and cost**: where were time, tokens, evidence, and context budget spent?

These channels share run and call identifiers, but they must not become one unbounded event log. Telemetry describes execution; it must never grant tool permission, alter evidence, or become a source of business facts.

## 2. Run, Step, and Call Model

`agent_runs` stores one lifecycle with user, session, question, mode, status, timestamps, step counts, and aggregated LLM usage. `agent_steps` stores durable retrieval, reasoning, tool call, tool result, control, and answer steps. `agent_llm_calls` stores one row per physical provider request.

A retry, continuation, forced conclusion, memory extraction, or session summary is a new physical LLM call. Shared LLM clients must not retain mutable “current run” state; correlation is passed through the call context to a lightweight usage recorder.

Large payloads follow the context and tool-result rules in module 04: durable raw content, model-visible content, and display summaries are distinct representations. Observability records which representation was used and whether anything was omitted; it does not silently truncate the evidence itself.

## 3. Live Event Contract

A run hub may broadcast:

```text
phase       transient preprocessing or retrieval status
reasoning   provider-visible reasoning stream, when available
content     visible answer tokens
step        a durable business step
evaluation  opt-in trace telemetry
terminal    the sole run outcome, including status or error
```

Phase hints are ephemeral. Slow subscribers may lose transient stream data under a bounded-buffer policy, but persisted steps and the terminal result remain queryable. One run emits exactly one terminal outcome. User cancellation and connection loss are recorded distinctly from provider, tool, and deadline failures.

## 4. Evaluation Trace

Evaluation is enabled per request or environment and records bounded node-local input metadata, output metadata, status, timing, and error category. Useful nodes include:

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

The trace keeps proposed and effective evidence plans separate. Memory recall is not folded into retrieval. Model-turn timing distinguishes request start, first provider event, reasoning, content, tool deltas, tool calls, and completion. End-to-end TTFT includes preprocessing and retrieval; turn-level TTFT starts at the physical provider call.

Raw hidden reasoning and sensitive provider internals are not a public trace contract. Trace payloads use projections, hashes, counts, and redacted excerpts rather than unrestricted prompts, source files, credentials, or full tool output.

## 5. Token Accounting

Provider-reported usage is the authority. Byte count, rune count, SSE delta count, and legacy character counters must not be labelled as tokens.

```text
call.total_tokens = provider total_tokens
                  or input_tokens + output_tokens

run.total_tokens = sum(call.total_tokens)
run.peak_input_tokens = max(call.input_tokens)
run.peak_reserved_tokens = max(call.input_tokens + call.max_output_tokens)
                           for calls with a known output reservation
```

`reasoning_tokens` is a subdivision of output tokens and is not added twice. `cached_input_tokens` is a subdivision of input usage retained for cost analysis; it is not removed from context occupancy. A zero `max_output_tokens` means the reservation is unknown, not zero.

Recommended phases are `route`, `agent_step`, `continuation`, `forced_conclusion`, `memory_extract`, and `session_summary`. Phase is a cost attribution label, not a lifecycle state.

## 6. Provider Usage Mapping

OpenAI-compatible non-streaming responses read top-level usage. Streaming requests use the provider's supported usage event/final chunk. Anthropic streaming merges the initial input usage with final output usage; cache creation and cache-read input are included in unified input occupancy.

If a configured gateway does not return usage, record the call status with unknown or zero reported usage according to the schema. Do not estimate token counts and do not switch providers. Model context-window size comes from explicit model capability configuration, never from usage inference.

## 7. Persistence and Bounded Reads

A physical-call record includes run ID, monotonic call sequence, phase, provider, model, usage dimensions, output reservation, duration, status, and timestamp. In the same transaction, update the run aggregates and peaks so completed calls remain visible after cancellation or process failure.

Run lists read only aggregate columns. Run detail reads calls for the requested run ordered by call sequence and applies an explicit bound or cursor. Events, logs, traces, and tool payloads each have independent retention and access policies.

## 8. Metrics

Useful metrics include:

- run success, cancellation, timeout, and forced-conclusion rates;
- provider and tool duration, error rate, retry count, and first-token latency;
- input, output, reasoning, cached, total, and peak reserved tokens;
- tool-result size, omission count, archival/refetch rate, and answer-contract retries;
- evidence-plan coverage and required-evidence failure rate;
- subscriber drops and terminal-event delivery;
- background summary and memory cost, reported separately from synchronous answer latency.

Metrics describe system behavior. They cannot be cited as proof of a customer, code, configuration, or runtime fact unless acquired through an authorized evidence tool.

## 9. User Control and Audit

Pause is consumed between agent steps; it cannot interrupt an in-flight provider or tool request. Resume releases a paused run. Nudge injects authenticated guidance at the next safe boundary. Abort cancels the run context and records the actor and reason.

Run status transitions and terminal completion must be concurrency-safe. Audit records include actor, request/run ID, action, old/new status, timestamp, and failure category without storing secrets or unrestricted model internals.

## 10. Acceptance Criteria

1. Every physical LLM request is attributable to one run and phase without cross-run leakage.
2. Provider usage is not replaced by character-based estimates.
3. Run aggregates equal the sum/max of persisted call records.
4. End-to-end and provider-turn TTFT are reported as different metrics.
5. Transient stream loss does not erase durable steps or the terminal result.
6. Trace and metrics do not modify evidence, permissions, or final-answer facts.
7. Tool-result truncation, omission, archival, and contract retries are observable.
8. Online list/detail APIs perform bounded reads at the storage boundary.

## Detailed Consolidated Material

### Agent Observability Design

> Migrated from CodeLoom `docs/design/agent/agent-observability-design.md`; incorporated into this module on 2026-07-31.

Status: Current

Agent observability separates business steps, live stream events, and opt-in evaluation traces. These channels answer different questions and must not be collapsed into one unbounded log.

#### Data Model

`agent_runs` stores one run lifecycle: user, session, question, status, mode, step/token counts, and start/end times. `agent_steps` stores persisted retrieval, model-thinking, tool-call, tool-result, control, and answer steps.

Full tool output may be retained in the step record while `result_summary` and the content returned to the model are bounded separately. The run APIs provide list/detail access and session deletion cleanup.

#### Live Events

`RunHub` fans out:

```text
phase       transient preprocessing/retrieval status
reasoning   streamed provider reasoning
token       visible answer content
step        persisted business step
trace       opt-in evaluation telemetry
terminal    sole run outcome with status/error
```

Phase hints are not persisted. Token/reasoning/trace delivery uses bounded subscriber buffers; slow consumers can delay or lose stream data according to the hub timeout policy, while persisted steps remain queryable.

#### Evaluation Trace

Trace is enabled per QA request and records node-local input, output, status, and duration. Current nodes may include:

```text
query_analysis       evidence_plan       memory_recall
query_rewrite        retrieval_dispatch  retrieval_discover
retrieval_expand     retrieval_assemble  history_compile
agent_model_turn     first_answer_token  force_conclusion
```

`evidence_plan` records proposed and effective plans separately. `memory_recall` stays separate from retrieval. `agent_model_turn` records provider first-event, reasoning, content, tool-delta, and tool-call timings. `first_answer_token` records both turn-level and end-to-end TTFT.

Evaluation traces are broadcast for evaluation/UI use and are not business steps. Sensitive raw provider internals should not be promoted into a public trace contract.

#### User Control

The run-control endpoint supports:

- `pause`: stop before the next loop step and wait;
- `resume`: release a paused run;
- `nudge`: inject user guidance before continuing;
- `abort`: terminate the run.

Signals are polled between Agent steps, so they cannot interrupt an in-flight provider or tool HTTP call. The run/tool contexts still enforce their own deadlines. Pause conditionally persists running to paused when consumed, Resume persists paused to running, and terminal completion atomically accepts either non-terminal state.

#### Invariants

1. Observability does not change `EvidencePlan` or tool permission.
2. Trace nodes map to evaluable functional stages rather than UI-only labels.
3. First-token timing includes preprocessing and retrieval when reported as run-elapsed TTFT.
4. Transient stream loss does not erase persisted run steps.
5. Background summary and memory work are not counted as synchronous answer latency.
