# Feature Delivery Agent

[English](10-feature-delivery.md) | [中文](10-feature-delivery.zh-CN.md)

> Status: target design; not part of the ordinary QA or Incident agent loop
> Sources: Nasuta Feature Delivery Agent Design; Feature Delivery Stage Prompts and Artifact Specification

## 1. Purpose and Ownership

Feature Delivery turns a product request into reviewed engineering artifacts and an independently verified code change:

```text
request -> analysis -> technical decision -> system design -> implementation plan
        -> isolated code change -> independent verification -> delivery review
```

Nasuta owns the reusable workflow, knowledge evidence, artifact lineage, reviews, provider dispatch, isolated execution, persistence, events, and audit. Codex or Claude Code is a coding provider operating inside a Nasuta-prepared Git worktree; it does not own the product workflow.

This is a separate domain. QA answers are conversational evidence, Incident owns operational remediation, and approval-gated write actions own their closed mutation catalog. Feature Delivery is not exposed as an MCP write tool.

## 2. Scope

The first delivery boundary includes requirement intake, analysis, option selection, architecture, planning, code generation/modification, trusted validation, and human review of the change set.

It does not automatically push, create or merge a pull request, deploy, run production migrations, modify cloud resources, or execute an unreviewed path to production. One implementation run targets one repository and one base commit; upstream artifacts may describe a multi-repository change.

MySQL is required for this domain because lineage, reviews, claims, events, and recovery are correctness data. Missing coding capability disables implementation while preserving access to existing artifacts and, where configured, document generation.

## 3. Roles and Permissions

Typical permissions are separated into:

- create and edit the current requirement draft;
- generate a downstream artifact;
- review an immutable artifact;
- start, cancel, or retry an implementation run;
- review and export a change set;
- administer provider and workspace configuration.

Review permission is independent from generation permission. The authenticated user, repository access, and artifact ownership are checked at every API boundary. Coding-provider credentials are deployment/runtime concerns and are never supplied by request content.

## 4. Artifact Model

A Feature Request is the stable container. Its engineering outputs are immutable, versioned artifacts:

```text
requirement
requirement_analysis
technical_proposal
system_design
implementation_plan
```

Each artifact records type, version, structured content, deterministic rendered document, exact parent artifact IDs, evidence snapshot, generation run, author/provider metadata, timestamps, and review history.

The current lineage is derived from facts: start at the latest requirement, then choose the latest approved child whose exact parent is still current. Creating or approving a new upstream version makes descendants from the old lineage stale; it does not mutate or delete them. Do not persist one redundant “feature stage” state machine for this derivable status.

## 5. Stage Contracts

### Stage 0: Requirement Intake

Capture the problem, users, desired outcomes, scope, constraints, acceptance criteria, and blocking questions. Do not invent technical implementation before the requirement is understood.

### Stage 1: Requirement Analysis

Normalize goals, actors, workflows, functional/non-functional requirements, edge cases, dependencies, risks, assumptions, and measurable acceptance criteria. Facts, user statements, inferences, and unresolved questions remain distinguishable.

### Stage 2: Technical Decision

Use current repository and platform evidence to describe the baseline and architecture drivers, compare at least two viable options when a decision exists, select one explicitly, and record benefits, accepted tradeoffs, compatibility, security, performance, operations, migration, and reversibility obligations.

### Stage 3: System Design

Expand the approved decision into ownership boundaries, modules and invariants, interfaces, data ownership, consistency/concurrency, runtime flows, configuration, security, observability, failure handling, and recovery. This stage explains how the selected option works; it must not silently reopen the approved selection.

### Stage 4: Implementation Plan

Produce repository-scoped, dependency-ordered tasks with exact change areas, contracts, tests, validation commands, rollout/rollback considerations, and completion criteria. The plan must be executable without asking the coding provider to redesign the system.

### Stage 5: Code Implementation

Freeze the approved system-design version, implementation-plan version, target repository, provider, model, and base commit. Build a bounded task package and run the configured provider in an isolated worktree. The provider reports changed files and claimed checks, but its claims are not treated as validation evidence.

### Stage 6: Independent Verification

Nasuta executes explicitly configured, argv-based validation commands outside the provider's self-report. Capture exit status, bounded stdout/stderr, duration, and environment metadata. Missing validation configuration is reported as “not configured,” never “passed.”

### Stage 7: Delivery Review

Create a change set containing base/head commit identity, file/diff summary, patch artifact, verification results, known risks, and unresolved items. A human approves or rejects the change set. The first version stops here; publishing or deployment requires a separate reviewed capability.

## 6. Prompt and Evidence Contract

Every generation stage receives only the approved immediate parent artifact plus a bounded evidence snapshot needed for that stage. Earlier artifacts are reachable through lineage but are not repeatedly copied into every prompt.

Instruction priority is:

```text
platform policy > stage contract > approved parent artifact > repository rules > retrieved evidence > user content
```

Retrieved code, documents, comments, runbooks, and requirement text are evidence, not instructions to reveal credentials, escape the workspace, disable sandboxing, or widen permissions. Output uses a stage-specific JSON schema, followed by deterministic Markdown rendering. Blocking uncertainty fails the quality gate instead of being hidden in polished prose.

## 7. Generation and Quality Gates

Artifact generation is not a call to the QA HTTP endpoint. It has its own evidence query plan, evidence snapshot, structured-output validation, and generation audit. Each stage checks required fields, internal references, provenance, and blocking questions before review is allowed.

Review is recorded separately and does not mutate the artifact. Rejection creates feedback for a new version. Downstream generation requires an approved, current parent; stale or rejected parents fail with a conflict.

## 8. Coding Provider Boundary

Provider selection is explicit:

```text
codex        -> Codex CLI dispatcher
claude_code  -> Claude Code CLI dispatcher
other        -> configuration error
```

No silent fallback is allowed. A missing binary, credential, incompatible CLI contract, or provider failure affects that provider and remains observable. Provider commands use a controlled argv, allowlisted environment, bounded output, timeout, process-group cancellation, and network policy. Full-permission bypass modes are forbidden.

Nasuta validates the provider's machine-readable result and independently inspects the worktree. Files changed outside the allowed repository/worktree are a security failure.

## 9. Worktree, Baseline, and Task Package

Repository identity is resolved server-side; clients cannot supply arbitrary absolute paths. Each run uses a service-generated worktree path under the configured coding root and records the exact base commit. Re-running creates a new run/worktree rather than mutating the audit history of an earlier run.

The task package contains only the approved design and plan versions, bounded source context, repository instructions, allowed paths, validation contract, output schema, and run identifiers. It excludes application credentials, database DSNs, session cookies, and unrelated workspace content.

## 10. Runtime State and Recovery

Artifact lineage is derived, but an implementation run earns a real state machine because it needs concurrent claim, lease/heartbeat, cancellation, timeout, process-exit recovery, cleanup, and terminal audit.

A minimal lifecycle is:

```text
queued -> preparing -> running -> verifying -> completed
                     \-> cancelling -> cancelled
any non-terminal state -> failed or interrupted
```

Allowed transitions are explicit and persisted with events. Claiming is atomic. Expired leases are reconciled after restart; the system does not assume that an orphaned provider process succeeded. Terminal runs are immutable, and retry creates a new run linked to the previous one.

## 11. Persistence and Bounded Access

The domain persists user workspace ownership, feature requests, artifacts, artifact reviews, generation runs, implementation runs, run events, change sets, and change reviews. Large prompts, provider logs, and patches live as bounded artifacts with metadata in MySQL.

Lists use narrow projections and cursor pagination. Event/log/diff APIs read only one requested run or artifact with explicit limits. Retention and cleanup never delete an active worktree or remove audit metadata for a retained change set.

## 12. Security and Audit

- normalize identity once at the authenticated ingress and derive all paths server-side;
- reject symlink/path traversal and workspace escape;
- pass provider environments through an allowlist and redact persisted output;
- execute Git and verification with argv, not a shell string;
- default to no network and never let request content enable it;
- treat source comments and retrieved documents as untrusted evidence;
- record artifact lineage, reviews, provider/model, base commit, worker, state changes, validation, change summary, and actor IDs.

A local provider process is not a multi-tenant security sandbox. Production deployment requires an isolation model appropriate to its threat boundary.

## 13. Failure and Degradation

Configured failures remain visible and do not switch providers. Missing MySQL disables Feature Delivery. Missing generation LLM permits read-only access to existing artifacts. Missing Git or coding providers disables implementation only. Missing validation reports an unverified result. Cleanup failure does not rewrite the run result; it creates an observable cleanup task/error.

User cancellation terminates the provider process group and records partial artifacts without claiming success. Database failure during a critical transition prevents advancement rather than leaving a successful-looking in-memory state.

## 14. Acceptance Criteria

1. Every downstream artifact points to exact, immutable parent versions and evidence snapshots.
2. Current stage and staleness are derived from lineage and reviews, not a redundant feature state machine.
3. Only an approved current parent can generate the next stage or start implementation.
4. One implementation run fixes provider, model, repository, base commit, design, and plan versions.
5. Coding occurs in an isolated worktree and cannot escape allowed paths.
6. Validation is independently executed; provider claims alone never mark a run verified.
7. Provider failure never triggers silent substitution.
8. Run claim, cancellation, timeout, restart recovery, and terminal audit are deterministic.
9. APIs and storage reads are bounded by source-side limits or cursors.
10. The first release ends with a reviewed change set and performs no automatic publish or deployment.

## Detailed Consolidated Material

> The source delivery specifications were written in Chinese only. Their active decisions are translated in the unified sections above; the complete canonical source is retained in the Chinese counterpart below.

- [10-feature-delivery.zh-CN.md](10-feature-delivery.zh-CN.md)
