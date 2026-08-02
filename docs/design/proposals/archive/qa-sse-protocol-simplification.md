# QA SSE Protocol Simplification Proposal

[English](qa-sse-protocol-simplification.md) | [中文](qa-sse-protocol-simplification.zh-CN.md)

> Status: implemented and archived; non-normative historical record
> Date: 2026-08-01
> Archived: 2026-08-01
> Scope: Nasuta QA/Agent/LLM and CodeLoom Dashboard QA
> Consolidation targets: `agent-platform/01-architecture-and-execution` and `08-observability-and-evaluation`

## 1. Document Lifecycle

This document carries the implementation audit, target protocol, delivery slices, and acceptance gates during development. Until implementation is accepted, the `agent-platform` module documents remain authoritative.

Consolidate this proposal into modules 01 and 08 only after both repositories have switched, legacy events and fallbacks are removed, backend/frontend/cross-repository tests pass, and the text is reconciled with the final code. Then remove this proposal or retain only a historical decision and delivery record.

## 2. Scope and Non-goals

This proposal simplifies the QA live-event protocol across the LLM, agent, RunHub, dashboard, and frontend. It does not switch SSE to WebSocket, require browser `EventSource`, refactor retrieval/reranking/diversity/token accounting, add a generic replay log, or expose internal `StepKind` as a public API.

Dashboard QA keeps `POST /api/qa/ask` with `fetch` consuming `text/event-stream`, which supports the required JSON body, authentication, and cancellation.

## 3. Current Path

```text
OpenAI-compatible/Anthropic stream
  -> llm.StreamHandler
  -> agent.StreamPipe
  -> RunHub.SSEEvent
  -> dashboard emitHubEvent
  -> HTTP named SSE
  -> Vue fetch + ReadableStream parser
```

The dashboard currently emits 15 names:

```text
run_start, phase, progress, tool, tool_result,
token, reasoning, llm_timing, trace, context,
run_end, done, error, compaction, session_restart_recommended
```

The handler emits `run_start/context/compaction/session_restart_recommended` directly, while most remaining events are inferred from seven optional `RunHub.SSEEvent` fields. There is no single exhaustive protocol owner.

## 4. Confirmed Problems

1. Final content is represented as tokens, a `think` step, and an `answer` step; success is represented by `run_end`, `done`, EOF, and catch-path buffer commits.
2. `StreamPipe` delivers content before a later tool delta. The frontend clears it on `tool`, while backend `answerText` does not, allowing UI and persistence to diverge.
3. Dashboard remaps internal StepKinds to HTTP names, while broadcast `retrieval` and `answer` steps are silently ignored.
4. Seven optional fields form an untagged union that permits invalid combinations and prevents exhaustive handling.
5. EOF or parse failure can commit a partial buffer as a successful assistant message.
6. Reasoning, LLM timing, and trace diagnostics are mixed into the business completion protocol.
7. Backend `tool_result` sends `result_preview`, while the frontend reads `summary`.
8. The handwritten parser accepts only one-line JSON `data:` and swallows most JSON or dispatch errors without gap detection.

The transport is not the source of complexity. Repeated renaming, duplicate facts, and fallback completion paths are.

## 5. Target Boundaries

```text
provider stream
  -> LLM adapter: content/reasoning/tool-call delta/usage/finish
  -> agent: answer candidate/tool call/business step/RunOutcome
  -> dashboard adapter: public SSE event
  -> frontend dispatcher: UI state
```

The LLM adapter owns provider chunk compatibility. The agent owns answer/tool classification and RunOutcome. One dashboard mapper owns the public contract. The frontend understands only that contract.

## 6. Public Contract Draft

RunHub uses a `Type + Data` tagged event. Dashboard writes `Type` as the SSE `event:` and serializes `Data` as SSE `data:` without remapping:

```json
{
  "step": 4,
  "tool": "search_code",
  "summary": "...",
  "failed": false
}
```

The current single-connection, single-run SSE stream is naturally ordered. It does not add sequence numbers, replay logs, or a reconnect state machine. Those mechanisms require a separate contract if reconnect recovery becomes a real requirement.

| Business event | Purpose |
|---|---|
| `run.started` | establish run identity |
| `status` | replace transient status text |
| `answer.delta` | append confirmed answer content |
| `tool.started` | add a tool call with bounded arguments |
| `tool.finished` | complete the tool with summary, status, and timing |
| `context` | update references and hit count |
| `run.finished` | sole terminal result with status, answer, evidence, and error |

`reasoning.delta`, `trace`, and `llm.call` are optional diagnostics. Session compaction projects to `session.status`; only a genuine new-session requirement emits `session.restart_recommended`.

## 7. Answer and Tool Turns

Content remains candidate text until the model turn finishes because a later chunk may introduce a tool call.

- No tool call: publish candidate content as `answer.delta`.
- Tool call present: retain candidate content only for internal audit and publish no answer delta.
- Persist the final answer step without replaying the same live text.

The frontend never rolls back delivered answer text. A future absolute minimum per-token latency requirement needs a provider-guaranteed answer/tool discriminator.

## 8. Completion and Disconnects

Each run emits exactly one `run.finished`. Commit an assistant message only for non-empty `done` results. Failed and aborted runs do not commit partial answers. Remove `run_end`, `done`, synthesized completion, channel-close success, EOF success, and catch-path success. A disconnect before the terminal event is a transport failure; it does not rewrite business state.

## 9. Delivery Policy

| Priority | Events | Policy |
|---|---|---|
| Terminal | `run.finished` | publish after persistence; never silently drop; queryable by run ID |
| Ordered business | answer, tool, context | preserve in-run order and report gaps |
| Coalescible | `status` | retain only the newest value under pressure |
| Droppable diagnostics | reasoning, trace, LLM call | optional; count drops |

Use bounded per-subscriber buffers. Do not hold a global lock during network waits, block unrelated runs, add unbounded buffers, or introduce a generic replay log.

## 10. Delivery Slices

1. **P0, contract tests:** capture protocol fixtures, fix `result_preview/summary`, and test chunk splits, CRLF, multiple frames, multiline data, invalid JSON, and EOF without terminal.
2. **P1, canonical event:** replace the optional-field union with a `Type + Data` tagged event and forward it unchanged through Dashboard, without speculative sequencing or compatibility dual writes.
3. **P2, frontend:** extract a tested frame parser, dispatch typed payloads, commit only on `run.finished`, and remove rollback/EOF/catch commits.
4. **P3, agent:** prevent tool-turn candidate content from becoming answer deltas, remove duplicate final-answer `think` projection, classify persistence-only steps explicitly, and separate diagnostics.
5. **P4, cleanup:** remove old names, compatibility adapter, optional union, and completion fallbacks. Verify by code search.

## 11. Acceptance Gates

1. Tool preambles never enter `answer.delta`; the frontend never clears delivered answers.
2. Every run has exactly one `run.finished`.
3. EOF, parse failure, and disconnect without terminal never commit success.
4. Failed and aborted runs do not save assistant messages.
5. Tool payload fields and pairing identifiers agree across repositories.
6. Disabling diagnostics does not alter business results.
7. Slow subscribers do not block unrelated runs; terminal state remains queryable.
8. Backend race tests, dashboard handler tests, frontend parser/dispatcher tests, and cross-repository E2E pass.
9. Only after legacy removal and acceptance are the final facts merged into modules 01 and 08.

## 12. Decisions to Confirm During Development

- Default to publishing answer content after full-turn classification, potentially as one or several chunks, prioritizing correctness.
- Use `tool_call_id` as the stable pairing key; step number is display order only.
- Reuse a bounded run-detail query for disconnect recovery. Add only the minimum query capability if absent; do not add a replay log.

## 13. Implementation Result

The backend and frontend switched on 2026-08-01:

- RunHub now owns tagged events; Dashboard removed StepKind remapping and forwards events unchanged.
- Agent output is published only after full-turn classification, so tool-turn preambles never enter the answer stream.
- `run.finished` is the only terminal event and carries the authoritative answer, evidence, and error.
- The frontend removed tool rollback, EOF commit, catch commit, and legacy terminal branches.
- The extracted SSE parser handles chunk splits, CRLF, multiple frames, multiline data, and explicit malformed-JSON failures.
- Hub broadcasting no longer waits under its global mutex; full subscriber buffers produce observable drops.

Agent/dashboard tests, race tests, `go build ./...`, frontend type checking, and the production build pass. A real-browser disconnect E2E has not yet run and remains a non-blocking residual verification item. The implementation is accepted and archived; modules 01 and 08 own the normative contract.
