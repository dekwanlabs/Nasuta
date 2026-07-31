# Write Safety and Approval

[English](09-write-safety-and-approval.md) | [中文](09-write-safety-and-approval.zh-CN.md)

> Status: current platform mechanism with a deliberately closed, incident-focused catalog; broader actions require explicit design
> Source: Approval-Gated Write Tools

## 1. Boundary

Read tools acquire evidence. Write actions mutate repositories, records, external systems, or durable user state. They must use different registries, permissions, execution paths, and audit contracts.

Upper-layer applications may register read tools. They cannot dynamically inject, replace, or remove platform write actions. Write actions are absent from MCP and from the ordinary read-tool registrar. Evidence planning and read-tool selection never imply write permission.

## 2. Closed Write Catalog

Nasuta owns a compile-time `writeaction` catalog. Each entry defines:

- stable action ID and description;
- canonical argument schema;
- authorization and impact class;
- approval presentation;
- explicit dispatcher case;
- idempotency and concurrency contract;
- audit and result schema.

The current implementation is limited to incident fix actions such as branch and commit proposals. Adding a mutation requires reviewed platform code; scenario configuration cannot turn an arbitrary tool into a write.

## 3. Proposal Contract

A model tool call may only create a proposal. The proposal stores the authenticated requester, normalized arguments, rationale, impact, relevant resource/version snapshot, creation time, expiry, and pending status. The tool result says that approval is required; it does not claim that the mutation happened.

Canonicalization and validation occur once at proposal ingress. Approval and execution consume the persisted canonical representation. They must not reinterpret model text or accept replacement parameters from the approval request.

## 4. Approval Flow

```text
authorized Agent proposal
  -> persist pending action and immutable execution payload
  -> authorized dashboard list/detail
  -> approve or reject
       ├─ reject: terminal status plus reason
       └─ approve: atomically claim the pending action
                    -> dispatch exact persisted action and arguments
                    -> persist done or failed result
```

Approval binds the approver to the exact normalized arguments, target identity, and expected version/base snapshot. If the target changed and the action contract requires freshness, execution fails with a conflict and a new proposal is required.

## 5. Authorization

Proposal permission, approval permission, and execution capability are distinct checks. The current incident flow uses administrator authorization; a broader design should define per-action RBAC rather than infer permission from role names or tool visibility.

The requester and approver are both persisted. Self-approval may be forbidden by an action's policy. Unknown action IDs, missing handlers, unavailable managers, expired proposals, and permission failures fail closed.

## 6. Idempotency and Concurrency

Approval must atomically claim `pending -> executing` so concurrent approval requests cannot execute the same side effect twice. The dispatcher passes a stable action/idempotency ID to downstream systems when supported.

Automatic retry is allowed only when the action contract proves that retry is safe. Provider, network, or downstream ambiguity must not trigger an unreviewed replay of a side effect. Failed or uncertain outcomes remain visible for operator resolution.

## 7. Dry Run and Impact

Actions with meaningful risk should provide a deterministic dry-run or preview that reports affected resources, validation errors, expected diff, and irreversible consequences. A preview is evidence for approval, not execution authorization by itself.

Impact classes may determine required approver permission, second approval, freshness window, or whether execution is disabled in an environment. These are platform policy checks, not instructions delegated to the LLM.

## 8. Execution and Audit

Execution dispatches one known action through one explicit switch or equivalent closed dispatcher. A configured backend failure is returned; the platform never substitutes another provider or mutation path.

Audit records include proposal ID, action ID, requester, approver, canonical argument hash, target snapshot, status transitions, timestamps, execution result, error category, and external correlation IDs. Sensitive values are redacted at ingress and are not emitted in logs or model-visible results.

## 9. Relationship to Agent Runs

A proposal is a durable side effect but not the requested business mutation. The originating Agent Run may finish with “pending approval.” Approval does not require keeping that run paused indefinitely; later requests query the action status and resulting evidence.

Long-term Memory writes also mutate durable user state. They follow the same principles—explicit candidate, scoped payload, user approval, audit, and no silent write—even if implemented by a separate domain service.

## 10. Current Gaps and Target Direction

Current limitations include immediate HTTP execution, no general durable execution queue, incomplete exactly-once claiming, coarse administrator authorization, and a small incident-only catalog. These limits must remain explicit.

A future queue is justified only when actions need leases, recovery after process exit, cancellation, or delayed execution. It must not be introduced merely to rename a linear approval flow.

## 11. Acceptance Criteria

1. A model call cannot directly perform a mutation.
2. Read-tool registration and evidence routing cannot expose or authorize writes.
3. Approval executes only the persisted action, canonical arguments, and bound target snapshot.
4. Concurrent approvals cannot execute the action twice.
5. Unknown, expired, stale, unauthorized, or unavailable actions fail closed.
6. Side-effect retries are action-specific and never automatic by default.
7. Every proposal, approval, rejection, execution, and failure is auditable.
8. New write types require an explicit catalog, authorization, impact, idempotency, and recovery design.

## Detailed Consolidated Material

### Approval-Gated Write Tools

> Migrated from CodeLoom `docs/design/agent/agent-write-tools-design.md`; incorporated into this module on 2026-07-31.

Status: Current, limited to incident fix actions

Write tools let the QA Agent propose a mutation without executing it in the model call. Their contracts belong to a closed platform catalog; upper-layer scenarios may register read tools only. Human approval is a separate authenticated dashboard operation.

#### Current Scope

`internal/writeaction` currently defines:

- `propose_branch`: request incident fix-branch creation;
- `propose_commit`: request committing a fix branch and creating the next incident fix outcome.

Calling either tool creates a MySQL `pending_actions` record with arguments, rationale, impact, requester, status, timestamps, and a 24-hour expiry. The tool result only reports that approval is pending.

Nasuta constructs the Approval Service, `pending_actions` persistence, and closed write catalog together with Incident initialization. The upper application cannot provide a proposer, IDs, descriptions, schemas, or handlers. Writes enter a Run snapshot only when the request user is an administrator. EvidencePlan and read-tool routing never grant write permission. MCP always uses a read policy and write entries are also MCP-hidden.

#### Approval Flow

```text
authenticated administrator Agent proposal
  -> pending action
  -> dashboard list/detail
  -> administrator approve or reject
       ├─ reject: persist reason and terminal status
       └─ approve: dispatch exact tool to the Nasuta Incident Manager
                    -> persist done/failed result
```

The platform catalog contains only compile-time contracts and approval supports only explicit dispatcher cases. Unknown tool names fail closed. An unavailable incident manager prevents execution rather than substituting another write path. Proposal and approval persist the real requester and approver IDs.

#### Current Limitations

- Expiry is stored but no general expiry worker is documented here.
- Approval executes immediately in the HTTP request; there is no durable job queue or retry orchestration.
- The implementation checks pending status before execution, but stronger transactional claiming is needed to make concurrent approvals exactly-once.
- Current authorization is administrator-level and is not yet a per-action RBAC permission model.
- The two actions are incident-specific, not an upper-layer-extensible code-edit framework.
- Agent pause/resume is not used as an asynchronous approval continuation; a later request observes the action result.

#### Safety Invariants

1. A model tool call can create a proposal, never directly perform the mutation.
2. The upper-layer ReadToolRegistrar cannot register, replace, or remove writes.
3. Write permission is independent of evidence and read-tool intent selection.
4. The approved action executes only its persisted tool and arguments.
5. Provider or incident capability failure remains visible.
6. New mutation types require a reviewed platform-catalog change, explicit dispatcher case, impact contract, and idempotency design; scenario configuration cannot inject them dynamically.
