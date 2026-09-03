package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

// Actor identifies the principal whose permissions and ownership apply to a Run.
type Actor struct {
	UserID   int64  `json:"user_id"`
	TenantID string `json:"tenant_id,omitempty"`
}

// Correlation links an Agent Run to its session, parent, and Workflow node.
type Correlation struct {
	SessionID     string `json:"session_id,omitempty"`
	ParentRunID   string `json:"parent_run_id,omitempty"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
}

// ContextBlock carries admitted context together with its evidence provenance.
type ContextBlock struct {
	Source            string              `json:"source"`
	Title             string              `json:"title"`
	Content           string              `json:"content"`
	References        []Reference         `json:"references,omitempty"`
	Evidence          []tool.EvidenceUnit `json:"evidence,omitempty"`
	EvidenceConflicts []EvidenceConflict  `json:"evidence_conflicts,omitempty"`
	// Complete distinguishes authoritative context from a partial retrieval result.
	Complete bool `json:"complete"`
	// ContentHash binds the admitted content to its provenance metadata.
	ContentHash string `json:"content_hash"`
}

type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Target string `json:"target"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolScope narrows one run below the immutable Definition capability ceiling.
type ToolScope struct {
	// AllowWrite cannot grant writes beyond the Definition permission ceiling.
	AllowWrite bool `json:"allow_write"`
	// RestrictVisible makes VisibleToolIDs an allowlist instead of descriptive metadata.
	RestrictVisible bool     `json:"restrict_visible"`
	VisibleToolIDs  []string `json:"visible_tool_ids,omitempty"`
	// OfferedToolIDs records the pre-pruning surface for auditability.
	OfferedToolIDs []string `json:"offered_tool_ids,omitempty"`
	// PruneApplied records that runtime selection narrowed the offered tool surface.
	PruneApplied bool `json:"prune_applied"`
}

// RunPolicy carries execution semantics that are independent of a scenario's planner types.
// RunOutputMode describes whether a Run is serving a user directly or is a
// structured node inside a Workflow. The execution loop is identical in both
// modes; the mode only documents the output contract for orchestration.
type RunOutputMode string

const (
	RunOutputStandalone     RunOutputMode = "standalone"
	RunOutputWorkflowNode   RunOutputMode = "workflow_node"
	RunOutputEvidenceWorker RunOutputMode = "evidence_worker"
)

// ErrBudgetExceeded identifies a hard budget boundary across the public
// runtime and workflow packages. Callers may wrap it without losing its code.
var ErrBudgetExceeded = errors.New("agent budget exceeded")

// RunBudgetGate is an optional shared hard-limit check supplied by a Workflow
// coordinator. It is deliberately defined in the public agent package so the
// standalone and delegated runtimes can enforce a parent Run limit without
// importing an orchestration implementation.
type RunBudgetGate interface {
	Check() error
}

// RunBudgetCallReservation accounts one physical model call inside a task
// reservation. The estimate stays reserved until the call reports usage.
type RunBudgetCallReservation interface {
	Settle(Usage) error
	Release() error
}

// RunBudgetUsageGate extends the shared run gate with in-flight call accounting.
// It is optional so standalone runtimes can keep using the simpler gate.
type RunBudgetUsageGate interface {
	RunBudgetGate
	ReserveCall(Usage) (RunBudgetCallReservation, error)
}

// RunBudgetTaskReservation owns one admitted child budget. Child model calls
// must reserve through this handle so they cannot consume a sibling's grant.
type RunBudgetTaskReservation interface {
	RunBudgetUsageGate
	RunBudgetAvailability
	Release() error
}

// RunBudgetTaskGate admits bounded child budgets from a shared root ledger.
type RunBudgetTaskGate interface {
	RunBudgetUsageGate
	ReserveTask(Usage) (RunBudgetTaskReservation, error)
}

// RunBudgetPhase identifies whether a physical model call may consume the
// protected parent-answer reserve.
type RunBudgetPhase string

const (
	RunBudgetPhaseDefault  RunBudgetPhase = "default"
	RunBudgetPhaseVerifier RunBudgetPhase = "verifier"
	RunBudgetPhaseAnswer   RunBudgetPhase = "answer"
)

// RunBudgetPhasedGate lets a root ledger protect answer capacity while the
// agent is still reasoning, without changing the legacy ReserveCall contract.
type RunBudgetPhasedGate interface {
	RunBudgetUsageGate
	ReserveCallForPhase(Usage, RunBudgetPhase) (RunBudgetCallReservation, error)
}

// RunBudgetPhasedAvailability reports phase-aware capacity for output sizing.
type RunBudgetPhasedAvailability interface {
	RunBudgetAvailability
	AvailableForPhase(RunBudgetPhase) Usage
}

// RunBudgetTaskGateFromContext returns a hierarchical budget gate, if any.
func RunBudgetTaskGateFromContext(ctx context.Context) RunBudgetTaskGate {
	if ctx == nil {
		return nil
	}
	gate, _ := ctx.Value(runBudgetGateContextKey{}).(RunBudgetTaskGate)
	return gate
}

// WithRunBudgetPhase marks physical calls made by a workflow phase. The
// default phase preserves protected downstream capacity; verifier and answer
// phases may consume the reserve assigned to the phase they activate.
func WithRunBudgetPhase(ctx context.Context, phase RunBudgetPhase) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runBudgetPhaseContextKey{}, phase)
}

// RunBudgetPhaseFromContext returns the current budget phase. Unknown or empty
// values fall back to the conservative default phase.
func RunBudgetPhaseFromContext(ctx context.Context) RunBudgetPhase {
	if ctx == nil {
		return RunBudgetPhaseDefault
	}
	phase, ok := ctx.Value(runBudgetPhaseContextKey{}).(RunBudgetPhase)
	if !ok || phase == "" {
		return RunBudgetPhaseDefault
	}
	return phase
}

// RunBudgetAvailability reports the budget that a physical model call may add
// without exceeding the shared run or its current task reservation. Runtimes
// use it to shrink a requested output ceiling before reserving the call.
type RunBudgetAvailability interface {
	Available() Usage
}

// RunBudgetMinimum reports output tokens still protected for the current task
// admission. A call must not be silently shrunk below this floor.
type RunBudgetMinimum interface {
	MinimumOutputTokens() int64
}

type runBudgetGateContextKey struct{}
type runBudgetPhaseContextKey struct{}

// WithRunBudgetGate attaches a shared Workflow budget gate to a Runtime call.
func WithRunBudgetGate(ctx context.Context, gate RunBudgetGate) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, runBudgetGateContextKey{}, gate)
}

// RunBudgetGateFromContext returns the shared Workflow gate, if any.
func RunBudgetGateFromContext(ctx context.Context) RunBudgetGate {
	if ctx == nil {
		return nil
	}
	gate, _ := ctx.Value(runBudgetGateContextKey{}).(RunBudgetGate)
	return gate
}

// RunBudgetUsageGateFromContext returns the call-accounting gate, if any.
func RunBudgetUsageGateFromContext(ctx context.Context) RunBudgetUsageGate {
	if ctx == nil {
		return nil
	}
	gate, _ := ctx.Value(runBudgetGateContextKey{}).(RunBudgetUsageGate)
	return gate
}

// RunOutputContract describes a request-specific answer shape enforced by the
// runtime in addition to the definition's output schema. It is intentionally
// small and transport-neutral so callers can pin it before execution begins.
type RunOutputContract struct {
	Kind           string   `json:"kind,omitempty"`
	RequireMermaid bool     `json:"require_mermaid,omitempty"`
	Subjects       []string `json:"subjects,omitempty"`
	MaxHops        int      `json:"max_hops,omitempty"`
}

type RunPolicy struct {
	// OutputMode distinguishes direct user delivery from a structured Workflow
	// node. It does not create a second execution runtime.
	OutputMode RunOutputMode `json:"output_mode,omitempty"`
	// OutputContract pins the required user-facing answer shape for this run.
	OutputContract RunOutputContract `json:"output_contract,omitempty"`
	// EvidenceRequired forbids an unsupported successful conclusion.
	EvidenceRequired bool `json:"evidence_required"`
	// EvidenceSeeded records that admitted evidence existed before tool execution.
	EvidenceSeeded  bool  `json:"evidence_seeded"`
	WebResearch     bool  `json:"web_research"`
	MaxToolCalls    int64 `json:"max_tool_calls"`
	RedactSensitive bool  `json:"redact_sensitive"`
}

// RunLimits contains the final effective limits for one prepared Run.
// Callers may only narrow the pinned Definition ceiling.
type RunLimits struct {
	Deadline       time.Time `json:"deadline,omitempty"`
	MaxSteps       int       `json:"max_steps,omitempty"`
	MaxToolCalls   int64     `json:"max_tool_calls,omitempty"`
	MaxInputTokens int64     `json:"max_input_tokens,omitempty"`
	// MaxContextTokens limits one provider request; MaxInputTokens remains the
	// cumulative input budget for the whole Run.
	MaxContextTokens int64 `json:"max_context_tokens,omitempty"`
	// MaxOutputTokens narrows the model output ceiling for every provider call.
	// Zero keeps the Definition model ceiling. It is not a cumulative budget.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int64 `json:"max_total_tokens,omitempty"`
	MaxCostMicros   int64 `json:"max_cost_micros,omitempty"`
	// ParentAnswerReserve protects a final user-facing answer from child and
	// reasoning calls. It is a root-only token reserve and is not cumulative
	// output quota for a child Run.
	ParentAnswerReserve int64 `json:"parent_answer_reserve,omitempty"`
}

// RunDelegation pins the immutable capability identity and parent relation for
// one dynamic child Run.
type RunDelegation struct {
	DelegationID               string        `json:"delegation_id"`
	Depth                      int           `json:"depth"`
	Capability                 CapabilityRef `json:"capability"`
	CapabilityContentHash      string        `json:"capability_content_hash"`
	CapabilityRegistryRevision uint64        `json:"capability_registry_revision"`
}

// DefinitionSelection records the rollout decision that selected a definition.
type DefinitionSelection struct {
	RuleVersion           int64  `json:"rule_version,omitempty"`
	RuleHash              string `json:"rule_hash,omitempty"`
	CandidateVersion      int64  `json:"candidate_version,omitempty"`
	BucketBasisPoints     int    `json:"bucket_basis_points,omitempty"`
	PercentageBasisPoints int    `json:"percentage_basis_points,omitempty"`
	// StableKeyHash permits rollout auditing without persisting the raw routing key.
	StableKeyHash string `json:"stable_key_hash,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// RunRequest contains the fully prepared, execution-ready input for one Agent Run.
type RunRequest struct {
	RunID string        `json:"run_id"`
	Agent DefinitionRef `json:"agent"`
	// DefinitionHash rejects execution if the pinned version no longer matches its content.
	DefinitionHash string              `json:"definition_hash"`
	Selection      DefinitionSelection `json:"selection,omitempty"`
	Input          json.RawMessage     `json:"input"`
	Messages       []Message           `json:"messages,omitempty"`
	Context        []ContextBlock      `json:"context,omitempty"`
	// Permissions is the already-intersected effective policy for this Run.
	Permissions PermissionPolicy `json:"permissions"`
	ToolScope   ToolScope        `json:"tool_scope"`
	Policy      RunPolicy        `json:"policy"`
	Limits      RunLimits        `json:"limits"`
	Delegation  RunDelegation    `json:"delegation,omitempty"`
	Actor       Actor            `json:"actor"`
	Correlation Correlation      `json:"correlation"`
}

// RunStart fixes the identity and capability ceiling before scenario preparation.
type RunStart struct {
	RunID          string              `json:"run_id"`
	Agent          DefinitionRef       `json:"agent"`
	DefinitionHash string              `json:"definition_hash"`
	Selection      DefinitionSelection `json:"selection,omitempty"`
	Input          json.RawMessage     `json:"input"`
	Permissions    PermissionPolicy    `json:"permissions"`
	ToolScope      ToolScope           `json:"tool_scope"`
	Policy         RunPolicy           `json:"policy"`
	Limits         RunLimits           `json:"limits"`
	Delegation     RunDelegation       `json:"delegation,omitempty"`
	Actor          Actor               `json:"actor"`
	Correlation    Correlation         `json:"correlation"`
}

// RunSnapshot pins all mutable control-plane choices before execution starts.
type RunSnapshot struct {
	RunID             string              `json:"run_id"`
	AgentID           string              `json:"agent_id"`
	DefinitionVersion int64               `json:"definition_version"`
	DefinitionHash    string              `json:"definition_hash"`
	Selection         DefinitionSelection `json:"selection,omitempty"`
	Provider          string              `json:"provider"`
	Model             string              `json:"model"`
	ModelParameters   map[string]any      `json:"model_parameters,omitempty"`
	// ToolSnapshotID pins tool definitions and handlers for the lifetime of the Run.
	ToolSnapshotID      string   `json:"tool_snapshot_id"`
	VisibleToolIDs      []string `json:"visible_tool_ids"`
	InputSchemaVersion  int64    `json:"input_schema_version"`
	OutputSchemaVersion int64    `json:"output_schema_version"`
	// PromptHash and ContextHash make replay drift observable.
	PromptHash  string           `json:"prompt_hash"`
	ContextHash string           `json:"context_hash"`
	Budget      BudgetPolicy     `json:"budget"`
	Limits      RunLimits        `json:"limits"`
	Delegation  RunDelegation    `json:"delegation,omitempty"`
	Permissions PermissionPolicy `json:"permissions"`
	Actor       Actor            `json:"actor"`
	Correlation Correlation      `json:"correlation"`
	CreatedAt   time.Time        `json:"created_at"`
}

// RunStatus is terminal because in-progress lifecycle state is owned by the runtime.
type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CostMicros      int64 `json:"cost_micros"`
}

type RunError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type EvidenceSummary struct {
	Status string `json:"status"`
	// ForcedConclusion marks an answer produced outside the normal completion path.
	ForcedConclusion   bool `json:"forced_conclusion"`
	ToolCallCount      int  `json:"tool_call_count"`
	ResultCount        int  `json:"result_count"`
	ToolFailureCount   int  `json:"tool_failure_count"`
	PartialResultCount int  `json:"partial_result_count"`
	OmittedItemCount   int  `json:"omitted_item_count"`
}

// EvidenceIdentity identifies one independently coverable evidence unit.
type EvidenceIdentity struct {
	SourceKind string `json:"source_kind"`
	Target     string `json:"target"`
	Section    string `json:"section,omitempty"`
	Version    string `json:"version,omitempty"`
	TimeRange  string `json:"time_range,omitempty"`
}

// EvidenceConflict preserves competing authoritative versions for one identity.
type EvidenceConflict struct {
	Identity       EvidenceIdentity  `json:"identity"`
	Current        tool.EvidenceUnit `json:"current"`
	Incoming       tool.EvidenceUnit `json:"incoming"`
	CurrentOrigin  string            `json:"current_origin,omitempty"`
	IncomingOrigin string            `json:"incoming_origin,omitempty"`
}

// RunResult is the durable public outcome of one Agent Run.
type RunResult struct {
	RunID                string                `json:"run_id"`
	Status               RunStatus             `json:"status"`
	Output               json.RawMessage       `json:"output,omitempty"`
	Text                 string                `json:"text,omitempty"`
	Evidence             EvidenceSummary       `json:"evidence"`
	EvidenceUnits        []tool.EvidenceUnit   `json:"evidence_units,omitempty"`
	EvidenceObservations []EvidenceObservation `json:"evidence_observations,omitempty"`
	EvidenceConflicts    []EvidenceConflict    `json:"evidence_conflicts,omitempty"`
	References           []Reference           `json:"references,omitempty"`
	Messages             []Message             `json:"messages,omitempty"`
	DelegationAdoptions  []DelegationAdoption  `json:"delegation_adoptions,omitempty"`
	Usage                Usage                 `json:"usage"`
	Error                *RunError             `json:"error,omitempty"`
}

// EvidenceObservation is the bounded content projection a workflow worker
// hands to downstream verification. ContentHash, when present, hashes Summary;
// full tool payloads and their source hashes remain in the execution trace.
type EvidenceObservation struct {
	SourceKind    string   `json:"source_kind"`
	Target        string   `json:"target"`
	Section       string   `json:"section,omitempty"`
	Summary       string   `json:"summary"`
	ContentHash   string   `json:"content_hash,omitempty"`
	Facets        []string `json:"facets,omitempty"`
	TrustTier     int      `json:"trust_tier,omitempty"`
	EvidenceClass string   `json:"evidence_class,omitempty"`
	Version       string   `json:"version,omitempty"`
	TimeRange     string   `json:"time_range,omitempty"`
}

// Runtime executes one already-compiled request against a pinned definition.
type Runtime interface {
	Run(ctx context.Context, request RunRequest) (RunResult, error)
}

// ManagedRun keeps preparation accounting and the scenario commit boundary on one Run.
type ManagedRun interface {
	Context(context.Context) context.Context
	Execute(context.Context, RunRequest) (RunResult, error)
	Finish(*RunError) error
}

// ManagedRuntime starts Runs before a scenario performs model-backed preparation.
type ManagedRuntime interface {
	Runtime
	Begin(context.Context, RunStart) (ManagedRun, error)
}
