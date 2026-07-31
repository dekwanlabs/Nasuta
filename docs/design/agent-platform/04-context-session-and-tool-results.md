# Context, Sessions, and Tool Results

[English](04-context-session-and-tool-results.md) | [中文](04-context-session-and-tool-results.zh-CN.md)

> Status: target design; current fixed truncation and default tool compression conflict with this design and must be removed through the migration below
> Updated: 2026-07-31
> Sources: Session Summary, Context Pollution Control, Session History Retrieval, Turn Compaction, Structured Tool-output Compression

## 1. Decision

Nasuta must not manage fresh tool evidence through silent truncation or generic semantic compression.

Core invariants:

1. A tool execution produces one authoritative result;
2. A normal-sized fresh result reaches the model in full;
3. Session, Agent Trace, and audit paths retain the complete result or a losslessly recoverable reference;
4. The UI may collapse content visually, but backend trace data is never truncated;
5. If one result is too large, the tool must paginate, narrow the query, or return an explicit delivery failure instead of pretending a partial result is complete;
6. Historical compaction applies only to old turns under real context pressure and never deletes authoritative source content;
7. Exact-answer contracts validate both model input and final output.

The current `sessionToolResultLimit = 1_200`, default `tooloutput.Compress`, and summary-style history replay violate these invariants.

## 2. Three Separate Problems

Tool-result governance contains three separate problems and cannot use one fixed length for all of them.

### 2.1 Current Tool-result Delivery

A result produced in the active run is first-party evidence for the answer. It requires high fidelity and is not compressed by default.

### 2.2 Session and Trace Persistence

Session preserves multi-turn protocol continuity. Trace supports reproduction, audit, and diagnosis. Both must recover the authoritative result.

### 2.3 Historical Context Maintenance

Old turns eventually exceed the model window and require selection, archival, and recall. Summaries are valid here, but they never replace authoritative source content.

These problems belong to the tool boundary, execution path, and history-maintenance path respectively.

## 3. Content Model

### 3.1 Public Tool Contract

Tools continue to return business data and per-execution dynamic contracts through `tool.Result`:

```go
type Result struct {
    Content        string
    References     []Reference
    Coverage       EvidenceCoverage
    AnswerContract AnswerContract
}
```

- `Content` is the authoritative tool result;
- `Coverage` states whether the tool query itself is complete;
- `AnswerContract` declares values that this execution requires the final answer to copy exactly.

### 3.2 Runtime Execution Content

The runtime may distinguish authoritative content from actual model input, but it must not persist a truncated summary that downstream code could mistake for evidence:

```go
type ToolExecution struct {
    AuthoritativeContent string
    PromptContent        string

    Coverage       tool.EvidenceCoverage
    AnswerContract tool.AnswerContract

    Arguments  string
    Failed     bool
    DurationMs int
}
```

The default invariant is:

```text
PromptContent == AuthoritativeContent
```

They may differ only when:

1. A deterministic transformation is lossless;
2. Tool execution failed and `PromptContent` carries a structured error;
3. The result exceeds the currently available model budget, so the runtime explicitly marks delivery failed and requests pagination or a narrower query.

A lossy summary is never successful `PromptContent`.

### 3.3 No Persisted Display Summary

The core execution model does not define `DisplaySummary`. If a UI list needs a preview, it derives that preview at the read or rendering boundary. A preview is never a business data source.

```text
authoritative content: persistence, validation, replay, audit
UI preview: transient, collapsible, non-authoritative
```

## 4. Fresh Tool-result Delivery

Target flow:

```text
tool executes
  -> tool.Result.Content
  -> persist authoritative trace
  -> derive model payload without loss
  -> calculate remaining model budget
     -> fits: send complete role=tool message
     -> does not fit: fail delivery and request pagination/narrowing
  -> preflight AnswerContract
  -> append successful result to the active run
```

### 4.1 Normal Results

Normal results are delivered unchanged:

```go
execution.AuthoritativeContent = result.Content
execution.PromptContent = result.Content
```

They do not pass through:

- query-relative summarization;
- head/tail truncation;
- array sampling;
- exact-identifier abbreviation;
- replacement of middle content with `...` or `…`.

A 59-record device result with complete serial numbers is a normal business result. If it fits the model window, all values must be delivered intact.

### 4.2 Lossless Formatting

Formatting is acceptable only when equivalence is demonstrable. JSON whitespace changes are acceptable; deleting fields, rewriting values, or omitting array elements is not.

If `formatToolResultForLLM` remains, each branch must guarantee:

```text
all authoritative fields and values remain completely readable from PromptContent
```

Every `AnswerContract.RequiredLiterals` value must occur verbatim.

### 4.3 Model Budget

The available budget is calculated for the current request:

```text
available tool budget =
    provider context window
  - system and developer messages
  - selected recent atomic turns
  - current user request
  - already accepted current-run evidence
  - reserved final-answer tokens
  - safety margin
```

A fixed 1,200-rune or 10,000-token per-tool limit is not a substitute for this calculation.

## 5. Oversized Tool Results

### 5.1 Bound at the Tool Boundary

A tool that may return many records must support source-level bounds:

- `limit` or `page_size`;
- a stable cursor;
- required-field projection;
- `total`, `has_more`, and `next_cursor`;
- `Coverage.Partial` and `OmittedItems`.

The SQL query, search request, or external API request enforces the bound. The runtime must not load an unbounded result and truncate it afterward.

### 5.2 No Silent Runtime Degradation

If a complete result exceeds the model budget:

1. The complete result is still written to Trace/Artifact;
2. Model delivery is marked failed;
3. The model receives a structured error requesting a paginated or narrower call;
4. The failed delivery does not activate its final-answer contract;
5. Truncated content is never presented as a complete successful tool result.

Example:

```json
{
  "error": "tool_result_exceeds_context_budget",
  "tool": "lookup_customer_devices",
  "result_bytes": 824013,
  "artifact_id": "tool_result_01H...",
  "retry": {
    "page_size": 100,
    "cursor": null
  }
}
```

Automatic retries apply only to read-only or explicitly idempotent tools. The runtime must not replay a write tool unconditionally after a delivery failure.

### 5.3 Artifact

Artifacts retain oversized authoritative results:

```go
type ToolResultArtifact struct {
    ID          string
    SessionID   string
    RunID       string
    ToolCallID  string
    Content     []byte
    ContentType string
    SHA256      string
    SizeBytes   int64
    CreatedAt   time.Time
}
```

The complete artifact must be persisted before Session or Trace stores a reference. The reference contains:

- artifact ID;
- tool-call ID;
- content type;
- size and digest;
- coverage;
- a bounded retrieval method.

Artifact access is isolated by tenant, user, and Session.

## 6. Session Persistence

### 6.1 Preserve Recent Atomic Turns

A turn is stored as a complete protocol unit:

```text
user
assistant(tool_calls)
tool results
assistant(final)
```

Call/result pairs are not separated, and tool messages are not reduced to prefixes.

### 6.2 Two Lossless Representations

A Session tool result has two valid representations.

#### Inline

A normal-sized result is stored completely in the tool message.

#### Artifact Reference

An oversized or archived result uses a lossless reference after the source has been persisted in Artifact/Trace.

There is no third “truncated prefix” representation.

### 6.3 Remove Fixed 1,200-rune Truncation

The current `sessionToolResultContent` truncates every Session tool message to 1,200 runes:

```go
return string(runes[:sessionToolResultLimit]) +
    "\n[truncated for session replay]"
```

This deletes JSON tails, later array entries, cursors, stack-trace tails, and exact identifiers. The mechanism must be removed, not merely assigned a larger fixed number.

## 7. Agent Trace, UI, and Logs

### 7.1 Agent Trace Is Authoritative

Every tool execution records:

```go
type ToolResultTrace struct {
    TraceID     string
    RunID       string
    ToolCallID  string
    ToolName    string
    Arguments   string

    AuthoritativeContent string
    PromptContent        string

    AuthoritativeSHA256 string
    PromptSHA256        string
    SizeBytes           int64

    Coverage       tool.EvidenceCoverage
    AnswerContract tool.AnswerContract
    Failed         bool
    DurationMs     int
}
```

Trace must answer:

1. What the tool actually returned;
2. What the model actually saw;
3. Whether the two changed;
4. Which transformation was applied;
5. Why contract validation passed or failed.

For normal results, both hashes should match. If they differ, Trace records an explicit reason, and a lossy transformation cannot be marked as successful delivery.

An oversized trace may store an Artifact reference, but that reference must recover the full payload losslessly. The trace still retains the complete result logically.

### 7.2 The UI Exposes Complete Results

The tool-call detail view must be able to retrieve complete arguments, complete authoritative output, actual model input, and contract-validation results.

The UI may:

- collapse large text by default;
- expand it on demand;
- read Artifact content in bounded pages or chunks;
- load metadata only for list views.

The UI must not:

- persist only a 1,200-character summary;
- use a preview field as detail content;
- mutate backend authoritative content to implement visual collapse.

If `ResultSummary` remains temporarily, it must be renamed `ResultPreview` and explicitly treated as non-authoritative. Prefer removing persisted previews and deriving them in the list API, or returning only size, status, and item count.

### 7.3 Logs

Agent Trace and audit logs are complete or losslessly recoverable. Ordinary process logs may store only index fields to avoid duplicating sensitive payloads in stdout:

```text
trace_id, run_id, tool_call_id, tool, bytes, sha256, artifact_id
```

Not duplicating payloads in process logs is not truncating the authoritative log. The complete payload remains retrievable through the Trace ID.

## 8. AnswerContract

### 8.1 Contract Construction

A tool constructs the contract after obtaining the execution-specific result:

```go
return tool.Result{
    Content: string(encoded),
    Coverage: tool.EvidenceCoverage{
        Partial:      partial,
        OmittedItems: omitted,
    },
    AnswerContract: tool.AnswerContract{
        RequiredLiterals: requiredDeviceSNs(results),
    },
}, nil
```

`requiredDeviceSNs(results)` runs once while constructing `tool.Result` and extracts complete serial numbers from the actual result. It is not a global registration callback.

### 8.2 Contract Scope

Only contracts from results successfully delivered to the model in the active run are aggregated:

```text
current run
  -> successful result A: no contract
  -> successful result B: required literals
  -> failed delivery C: not active
  -> final answer validates contract B only
```

The tool registry does not retain global `RequiredLiterals` across requests.

### 8.3 Model-input Preflight

Before a tool message joins the successful execution chain:

```text
for every required literal:
    literal must exist verbatim in PromptContent
```

If a literal is missing:

1. Mark tool delivery failed;
2. Do not start final-answer generation;
3. Restore complete authoritative content or request a paginated retry;
4. Record the missing count and Trace ID;
5. Do not activate values that the model could not see.

This preflight directly detects the case where the raw serial number is complete but model input contains `…xxx…`.

### 8.4 Final-answer Postflight

After generation, validate every active `RequiredLiterals` value verbatim:

1. On the first failure, regenerate with the explicit missing list;
2. Bound the retry count;
3. Return `ErrAnswerContractViolation` if values remain missing;
4. Never deliver the incomplete candidate as a successful response.

A contract failure is not repaired by further compression, masking, or guessing.

## 9. Historical Context Maintenance

### 9.1 High/Low Watermarks

History maintenance is triggered by actual provider input tokens:

```text
below low watermark
  -> keep selected recent history unchanged

above high watermark
  -> archive stale atomic turns
  -> keep the recent atomic tail verbatim
  -> recall only relevant archived history
  -> reduce to the low watermark
```

Watermark ratios are configurable and are not expressed as per-tool character limits.

### 9.2 Valid Compression Targets

Allowed:

- model projections of completed, archived old turns;
- summaries used for topic location and conclusion recall;
- recall representations of old non-exact evidence.

Forbidden:

- the current question;
- fresh evidence from the active run;
- retained recent atomic turns;
- authoritative tool results that have not been archived;
- the only copy of an exact identifier.

### 9.3 Summary Contract

A summary preserves:

- durable user constraints;
- current goals;
- decisions and rationale;
- files, interfaces, and important identifiers;
- commands and results;
- unfinished work and next steps.

A summary is never the sole storage for a large exact-value list. Complete evidence is replayed from Trace/Artifact or obtained by rerunning the tool.

### 9.4 History Selection

The online path reads bounded candidate metadata and selects by:

- explicit turn references;
- dependency on previous raw evidence;
- entity and topic relevance;
- time decay;
- conflicts between old and new entities.

Raw-evidence dependencies replay a complete atomic turn or Artifact chunks. Conclusion-only dependencies may use a structured summary.

## 10. Current Implementation Gaps

As of 2026-07-31:

| Location | Current behavior | Target behavior |
|---|---|---|
| `internal/agent/loop.go` | Fresh results enter `tooloutput.Compress(..., 10_000)` by default | Normal results reach the model unchanged |
| `sessionToolResultContent` | Each Session tool result is truncated to 1,200 runes | Complete inline content or lossless Artifact reference |
| Step `ResultSummary` | A 1,200-rune preview is persisted | Full `Content` is authoritative; previews are derived at presentation boundaries |
| `summary.go` | Tool evidence is reduced to 1,200 runes for summary input | Summary references Trace/Artifact and is not authoritative evidence |
| `turn_detail.go` | Archived detail is lossy-compressed | A recall projection may be lossy only after complete source content is in Trace/Artifact |
| `formatToolResultForLLM` | Ordinary tools may undergo field transformation | Only audited lossless transformations remain |
| `AnswerContract` | Final-answer validation and repair exist | Add model-input preflight and delivery-failure handling |

Step currently stores both `Content` and `ResultSummary`, so complete content may already exist in the database. Migration must still audit every reader so Session, UI detail, and model paths never consume `ResultSummary` as evidence.

## 11. Migration

### Phase A: Stop Irrecoverable Loss

1. Remove fixed truncation from `sessionToolResultContent`;
2. Bypass default `tooloutput.Compress` for normal results;
3. Audit and constrain `formatToolResultForLLM`;
4. Add AnswerContract model-input preflight;
5. Use complete tool output in Session and Trace;
6. Make UI detail read complete `Content`.

### Phase B: Clarify Preview and Trace Semantics

1. Remove persisted `ResultSummary`, or migrate and rename it to non-authoritative `ResultPreview`;
2. Make list storage queries load metadata only and detail queries load complete content;
3. Persist actual model input and its digest;
4. Add authorization, retention, and sensitive-data policy for Trace payloads.

### Phase C: Support Oversized Results

1. Add pagination and cursors to large-result tools;
2. Calculate remaining model budget dynamically;
3. Add Artifact storage and bounded reads;
4. Return retryable delivery errors for read tools that exceed the budget;
5. Forbid implicit replay of write tools.

### Phase D: Maintain History

1. Retain recent atomic turns in full;
2. Archive old turns only after the actual high watermark is reached;
3. Persist complete source content to Trace/Artifact first;
4. Generate structured recall projections for old history;
5. Remove the generic structured compressor from the fresh-evidence default path.

## 12. Verification

Required tests:

1. A normal tool result reaches the model byte-for-byte;
2. All 59 complete serial numbers occur in model input and final output;
3. The next turn can still retrieve all complete serial numbers;
4. Missing any RequiredLiteral from `PromptContent` fails tool delivery;
5. Missing any RequiredLiteral from the final answer causes bounded retries and then a hard failure;
6. JSON tails, later array elements, and `next_cursor` survive;
7. Session preserves call/result/final atomic relationships;
8. Tool-call UI detail exposes the complete result;
9. Trace can compare authoritative and model-input digests;
10. An oversized result is fully persisted to Artifact before a reference is written;
11. Old turns are not compressed below the high watermark;
12. Losing a historical projection does not delete authoritative source content;
13. Logs can locate the complete result through Trace ID;
14. Normal small tools no longer invoke generic semantic compression.

## 13. Acceptance Criteria

The completed system satisfies:

- No tool result is reduced to an irrecoverable fixed prefix;
- Normal fresh results remain consistent across tool, model, Session, and Trace;
- UI collapse never changes authoritative backend content;
- Agent Trace records both what the tool returned and what the model saw;
- Oversized results are not disguised as complete success;
- AnswerContract prevents exact-value loss at both input and output boundaries;
- History maintenance changes only model projections and never deletes auditable source content;
- Compression, archival, recovery, and contract failures are observable.
