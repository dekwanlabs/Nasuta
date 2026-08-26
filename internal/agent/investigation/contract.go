package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

var (
	ErrInvalidTransition = errors.New("invalid investigation state transition")
	ErrNotFound          = errors.New("investigation run not found")
	ErrBudgetExceeded    = errors.New("investigation budget exceeded")
	ErrPlanInvalid       = errors.New("investigation plan is invalid")
	ErrCapabilityGap     = errors.New("investigation capability gap")
	ErrEvidenceReference = errors.New("invalid evidence reference")
	ErrEmptyDelivery     = errors.New("investigation delivery is empty")
	ErrNoDelivery        = errors.New("investigation run has no delivery result")
	ErrTaskNotReady      = errors.New("investigation task is not ready")
	ErrTerminalRun       = errors.New("investigation run is already terminal")
	ErrLeaseHeld         = errors.New("investigation run lease is held")
	ErrLeaseFenced       = errors.New("investigation run lease fenced")
)

// RunStatus owns the lifecycle of one investigation. Terminal states cannot transition again.
type RunStatus string

const (
	RunCreated         RunStatus = "created"
	RunAnalyzing       RunStatus = "analyzing"
	RunPlanned         RunStatus = "planned"
	RunExecuting       RunStatus = "executing"
	RunVerifying       RunStatus = "verifying"
	RunReplanning      RunStatus = "replanning"
	RunComposing       RunStatus = "composing"
	RunDelivered       RunStatus = "delivered"
	RunFailed          RunStatus = "failed"
	RunCancelled       RunStatus = "cancelled"
	RunTimedOut        RunStatus = "timed_out"
	RunBudgetExhausted RunStatus = "budget_exhausted"
)

func (status RunStatus) Terminal() bool {
	switch status {
	case RunDelivered, RunFailed, RunCancelled, RunTimedOut, RunBudgetExhausted:
		return true
	default:
		return false
	}
}

// TaskStatus is deliberately separate from RunStatus because a task may fail while the run continues.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskPartial   TaskStatus = "partial"
	TaskFailed    TaskStatus = "failed"
	TaskBlocked   TaskStatus = "blocked"
	TaskCancelled TaskStatus = "cancelled"
)

type FailureCode string

const (
	FailureExecution       FailureCode = "execution_failed"
	FailureSchema          FailureCode = "schema_invalid"
	FailureReasoning       FailureCode = "reasoning_truncated"
	FailureTimeout         FailureCode = "timeout"
	FailureCancelled       FailureCode = "cancelled"
	FailureBudget          FailureCode = "budget_exhausted"
	FailureToolUnavailable FailureCode = "tool_unavailable"
	FailurePermission      FailureCode = "permission_denied"
	FailurePlan            FailureCode = "plan_invalid"
	FailureEmptyOutput     FailureCode = "empty_output"
	FailureComposer        FailureCode = "composer_failed"
	FailureVerifier        FailureCode = "verifier_failed"
	FailureLease           FailureCode = "lease_lost"
)

type ClaimStatus string

const (
	ClaimSupported   ClaimStatus = "supported"
	ClaimPartial     ClaimStatus = "partial"
	ClaimConflicting ClaimStatus = "conflicting"
	ClaimRejected    ClaimStatus = "rejected"
)

type DeliveryStatus string

const (
	DeliverySucceeded            DeliveryStatus = "succeeded"
	DeliveryPartial              DeliveryStatus = "partial"
	DeliveryEvidenceInsufficient DeliveryStatus = "evidence_insufficient"
	DeliveryFailed               DeliveryStatus = "failed"
)

type ExecutorType string

const (
	ExecutorDirectTool   ExecutorType = "direct_tool"
	ExecutorToolPipeline ExecutorType = "tool_pipeline"
	ExecutorInvestigator ExecutorType = "investigator"
	ExecutorVerifier     ExecutorType = "verifier"
	ExecutorComposer     ExecutorType = "composer"
)

// InvestigationGoal is one user-facing deliverable tracked independently from evidence coverage.
type InvestigationGoal struct {
	ID                  string   `json:"id"`
	Objective           string   `json:"objective"`
	IndependentlyUseful bool     `json:"independently_useful"`
	DependsOn           []string `json:"depends_on,omitempty"`
}

type EvidenceGoal struct {
	ID              string                    `json:"id"`
	Kind            string                    `json:"kind"`
	Description     string                    `json:"description"`
	Facets          []string                  `json:"facets"`
	Sources         []agentapi.EvidenceSource `json:"sources,omitempty"`
	RequiredSources []agentapi.EvidenceSource `json:"required_sources,omitempty"`
	Freshness       agentapi.FreshnessPolicy  `json:"freshness,omitempty"`
	Required        bool                      `json:"required"`
	HighRisk        bool                      `json:"high_risk,omitempty"`
	// MinimumCoverage is the number of distinct supporting claims a high-risk
	// goal needs before it may be presented as fully covered.
	MinimumCoverage int `json:"minimum_coverage,omitempty"`
}

const (
	GoalKindSystemBoundary     = "system_boundary"
	GoalKindBusinessDomain     = "business_domain"
	GoalKindEntrypoint         = "entrypoint"
	GoalKindCoreFlow           = "core_flow"
	GoalKindDataAndState       = "data_and_state"
	GoalKindExternalDependency = "external_dependency"
	GoalKindRuntimeOperations  = "runtime_and_operations"
	GoalKindExplore            = "evidence.explore"
)

type InvestigationEntity struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Role    string   `json:"role,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

type InvestigationConversationRef struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Turn      int    `json:"turn,omitempty"`
}

type InvestigationTimeRange struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	ToExclusive bool      `json:"to_exclusive"`
}

type InvestigationContext struct {
	ConversationRefs []InvestigationConversationRef `json:"conversation_refs,omitempty"`
	TimeRange        *InvestigationTimeRange        `json:"time_range,omitempty"`
	SeedMaterial     []agentapi.ContextBlock        `json:"seed_material,omitempty"`
}

const InvestigationContractVersion int64 = 1

type InvestigationContract struct {
	ID                 string                `json:"id"`
	Version            int64                 `json:"version"`
	ParentRunID        string                `json:"parent_run_id,omitempty"`
	TaskID             string                `json:"task_id,omitempty"`
	Round              int                   `json:"round,omitempty"`
	BaseDepth          int                   `json:"base_depth,omitempty"`
	Actor              agentapi.Actor        `json:"actor,omitempty"`
	Entities           []string              `json:"entities,omitempty"`
	EntityDetails      []InvestigationEntity `json:"entity_details,omitempty"`
	Context            InvestigationContext  `json:"context,omitempty"`
	Question           string                `json:"question"`
	InvestigationGoals []InvestigationGoal   `json:"investigation_goals,omitempty"`
	EvidenceGoals      []EvidenceGoal        `json:"evidence_goals,omitempty"`
	AllowedToolIDs     []tool.ToolID         `json:"allowed_tool_ids,omitempty"`
	PrincipalToolIDs   []tool.ToolID         `json:"principal_tool_ids,omitempty"`
	WorkspaceToolIDs   []tool.ToolID         `json:"workspace_tool_ids,omitempty"`
	MaxRounds          int                   `json:"max_rounds,omitempty"`
	MaxTasks           int                   `json:"max_tasks,omitempty"`
	BudgetProfile      string                `json:"budget_profile,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	// SeedEvidence carries identity-only evidence already admitted by the
	// caller. It is not normalized as text and must not be duplicated.
	SeedEvidence []EvidenceUnit `json:"seed_evidence,omitempty"`
}

type TaskTemplateRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type ToolCallSpec struct {
	ToolID tool.ToolID    `json:"tool_id"`
	Args   tool.Arguments `json:"args,omitempty"`
}

type TaskTemplate struct {
	ID        string   `json:"id"`
	Version   int64    `json:"version"`
	GoalKinds []string `json:"goal_kinds,omitempty"`
	// DiscoveryTypes limits replan candidates to normalized discovery shapes.
	DiscoveryTypes []string           `json:"discovery_types,omitempty"`
	SourceKinds    []string           `json:"source_kinds,omitempty"`
	RequiredInputs []string           `json:"required_inputs,omitempty"`
	Provides       []string           `json:"provides,omitempty"`
	ToolGrant      []tool.ToolID      `json:"tool_grant,omitempty"`
	InputSchema    agentapi.SchemaRef `json:"input_schema"`
	OutputSchema   agentapi.SchemaRef `json:"output_schema"`
	Preconditions  []string           `json:"preconditions,omitempty"`
	Executor       ExecutorType       `json:"executor"`
	CostProfile    BudgetVector       `json:"cost_profile"`
	MaxAttempts    int                `json:"max_attempts,omitempty"`
	ToolCalls      []ToolCallSpec     `json:"tool_calls,omitempty"`
	Enabled        bool               `json:"enabled"`
	// ProposalOnly prevents a template from being selected by the generic planner.
	ProposalOnly bool `json:"proposal_only,omitempty"`
	// AllowParallel is the server-owned default for ready Agent nodes.
	AllowParallel bool `json:"allow_parallel,omitempty"`
	Deprecated    bool `json:"deprecated,omitempty"`
}

type TaskCandidate struct {
	ID                   string          `json:"id"`
	Template             TaskTemplateRef `json:"template"`
	Objective            string          `json:"objective"`
	InvestigationGoalIDs []string        `json:"investigation_goal_ids,omitempty"`
	EvidenceGoalIDs      []string        `json:"evidence_goal_ids"`
	// Capability preserves the validated planner capability through compilation.
	Capability    string         `json:"capability,omitempty"`
	EvidenceGoals []EvidenceGoal `json:"evidence_goals,omitempty"`
	Optional      bool           `json:"optional,omitempty"`
	AllowParallel bool           `json:"allow_parallel,omitempty"`
	MaxAttempts   int            `json:"max_attempts,omitempty"`
	Entities      []string       `json:"entities,omitempty"`
	AllowedTools  []tool.ToolID  `json:"allowed_tools,omitempty"`
	InputRefs     []EvidenceRef  `json:"input_refs,omitempty"`
	Dependencies  []string       `json:"dependencies,omitempty"`
	Budget        BudgetVector   `json:"budget"`
}

type TaskBudget struct {
	Limit       BudgetVector `json:"limit"`
	MaxAttempts int          `json:"max_attempts,omitempty"`
}

type ExecutableTask struct {
	ID                   string          `json:"id"`
	Template             TaskTemplateRef `json:"template"`
	Objective            string          `json:"objective"`
	InvestigationGoalIDs []string        `json:"investigation_goal_ids,omitempty"`
	EvidenceGoalIDs      []string        `json:"evidence_goal_ids"`
	// Capability is retained for runtime routing and audit of proposal tasks.
	Capability    string             `json:"capability,omitempty"`
	EvidenceGoals []EvidenceGoal     `json:"evidence_goals,omitempty"`
	Entities      []string           `json:"entities,omitempty"`
	AllowedTools  []tool.ToolID      `json:"allowed_tools,omitempty"`
	InputRefs     []EvidenceRef      `json:"input_refs,omitempty"`
	Dependencies  []string           `json:"dependencies,omitempty"`
	Budget        TaskBudget         `json:"budget"`
	InputSchema   agentapi.SchemaRef `json:"input_schema"`
	OutputSchema  agentapi.SchemaRef `json:"output_schema"`
	Executor      ExecutorType       `json:"executor"`
	ToolCalls     []ToolCallSpec     `json:"tool_calls,omitempty"`
	Optional      bool               `json:"optional,omitempty"`
	AllowParallel bool               `json:"allow_parallel,omitempty"`
	Status        TaskStatus         `json:"status"`
}

// PlanExecutionPolicy freezes planner-provided limits after the server has
// intersected them with coordinator and contract defaults.
type PlanExecutionPolicy struct {
	MaxParallelism    int          `json:"max_parallelism,omitempty"`
	MaxRounds         int          `json:"max_rounds,omitempty"`
	MaxDepth          int          `json:"max_depth,omitempty"`
	MaxDuplicateRatio float64      `json:"max_duplicate_ratio,omitempty"`
	MaxRetries        int          `json:"max_retries,omitempty"`
	Budget            BudgetVector `json:"budget,omitempty"`
}

type PlanRevision struct {
	Revision     int                 `json:"revision"`
	ContractID   string              `json:"contract_id"`
	Tasks        []ExecutableTask    `json:"tasks"`
	CreatedAt    time.Time           `json:"created_at"`
	ProposalHash string              `json:"proposal_hash,omitempty"`
	Policy       PlanExecutionPolicy `json:"policy,omitempty"`
}

type EvidenceCandidate struct {
	SourceKind    string   `json:"source_kind"`
	Target        string   `json:"target"`
	Section       string   `json:"section,omitempty"`
	Content       string   `json:"content"`
	ContentHash   string   `json:"content_hash,omitempty"`
	Facets        []string `json:"facets,omitempty"`
	TrustTier     int      `json:"trust_tier,omitempty"`
	EvidenceClass string   `json:"evidence_class,omitempty"`
	Version       string   `json:"version,omitempty"`
	TimeRange     string   `json:"time_range,omitempty"`
}

type EvidenceUnit struct {
	ID            string   `json:"id"`
	SourceKind    string   `json:"source_kind"`
	Target        string   `json:"target"`
	Section       string   `json:"section,omitempty"`
	Content       string   `json:"content"`
	ContentHash   string   `json:"content_hash"`
	Facets        []string `json:"facets,omitempty"`
	TrustTier     int      `json:"trust_tier,omitempty"`
	EvidenceClass string   `json:"evidence_class,omitempty"`
	Version       string   `json:"version,omitempty"`
	TimeRange     string   `json:"time_range,omitempty"`
	TaskID        string   `json:"task_id,omitempty"`
}

type EvidenceRef struct {
	EvidenceID  string `json:"evidence_id"`
	SourceKind  string `json:"source_kind"`
	Target      string `json:"target"`
	Section     string `json:"section,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Version     string `json:"version,omitempty"`
	TimeRange   string `json:"time_range,omitempty"`
}

type ClaimCandidate struct {
	GoalID       string        `json:"goal_id"`
	Text         string        `json:"text"`
	Status       ClaimStatus   `json:"status"`
	EvidenceRefs []EvidenceRef `json:"evidence_refs"`
	Confidence   float64       `json:"confidence,omitempty"`
	ConflictRefs []EvidenceRef `json:"conflict_refs,omitempty"`
}

type VerifiedClaim struct {
	ID             string        `json:"id"`
	GoalID         string        `json:"goal_id"`
	Text           string        `json:"text"`
	Status         ClaimStatus   `json:"status"`
	EvidenceRefs   []EvidenceRef `json:"evidence_refs"`
	Confidence     float64       `json:"confidence,omitempty"`
	ConflictRefs   []EvidenceRef `json:"conflict_refs,omitempty"`
	VerifierTaskID string        `json:"verifier_task_id,omitempty"`
}

type EvidenceGap struct {
	GoalID         string   `json:"goal_id"`
	Reason         string   `json:"reason"`
	MissingFacets  []string `json:"missing_facets,omitempty"`
	MissingSources []string `json:"missing_sources,omitempty"`
	SuggestedTasks []string `json:"suggested_tasks,omitempty"`
}

type GoalCoverageStatus string

const (
	GoalCovered    GoalCoverageStatus = "covered"
	GoalPartial    GoalCoverageStatus = "partial"
	GoalUnresolved GoalCoverageStatus = "unresolved"
)

type GoalCoverage struct {
	GoalID         string             `json:"goal_id"`
	Required       bool               `json:"required"`
	Status         GoalCoverageStatus `json:"status"`
	ClaimIDs       []string           `json:"claim_ids,omitempty"`
	MissingFacets  []string           `json:"missing_facets,omitempty"`
	MissingSources []string           `json:"missing_sources,omitempty"`
}

type RunFailure struct {
	Code      FailureCode `json:"code"`
	Message   string      `json:"message"`
	Stage     string      `json:"stage,omitempty"`
	TaskID    string      `json:"task_id,omitempty"`
	Retryable bool        `json:"retryable"`
}

// RunFailureError preserves the stable failure code through wrapped execution errors.
type RunFailureError struct {
	Failure RunFailure
}

func (err *RunFailureError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", err.Failure.Code, err.Failure.Message)
}

type InvestigationReport struct {
	Evidence          []EvidenceUnit              `json:"evidence,omitempty"`
	Claims            []VerifiedClaim             `json:"claims,omitempty"`
	Coverage          []GoalCoverage              `json:"coverage,omitempty"`
	Gaps              []EvidenceGap               `json:"gaps,omitempty"`
	Failures          []RunFailure                `json:"failures,omitempty"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts,omitempty"`
}

type AnswerDraft struct {
	Text     string         `json:"text"`
	ClaimIDs []string       `json:"claim_ids,omitempty"`
	Status   DeliveryStatus `json:"status"`
	// Usage is populated by Runtime-backed composers. Custom composers may
	// leave it zero, in which case the coordinator uses a compatibility
	// fallback estimate for composition accounting.
	Usage BudgetVector `json:"usage,omitempty"`
}

type DeliveryResult struct {
	Status    DeliveryStatus      `json:"status"`
	Text      string              `json:"text"`
	Usage     BudgetVector        `json:"usage,omitempty"`
	Report    InvestigationReport `json:"report"`
	Failure   *RunFailure         `json:"failure,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}

type TaskExecutionInput struct {
	Task     ExecutableTask             `json:"task"`
	Evidence []EvidenceUnit             `json:"evidence,omitempty"`
	Claims   []VerifiedClaim            `json:"claims,omitempty"`
	Upstream map[string]json.RawMessage `json:"upstream,omitempty"`
	// RuntimeBudget is the shared hard ceiling for Agent-backed execution. It
	// is separate from Task.Budget.Limit, which also carries admission grants.
	RuntimeBudget BudgetVector   `json:"-"`
	WorkflowRunID string         `json:"-"`
	ParentRunID   string         `json:"-"`
	Actor         agentapi.Actor `json:"-"`
	Attempt       int            `json:"-"`
}

type TaskExecutionResult struct {
	Output             json.RawMessage     `json:"output,omitempty"`
	EvidenceCandidates []EvidenceCandidate `json:"evidence_candidates,omitempty"`
	Claims             []ClaimCandidate    `json:"claims,omitempty"`
	Discoveries        []Discovery         `json:"discoveries,omitempty"`
	Usage              BudgetVector        `json:"usage"`
	Failure            *RunFailure         `json:"failure,omitempty"`
}

// Discovery is a planner-relevant new fact surfaced by a task: either a newly
// resolved entity or a new dependency edge. It is intentionally normalized so
// replanning never depends on raw model prose.
type Discovery struct {
	Type   string `json:"type,omitempty"` // entity | dependency
	Entity string `json:"entity,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

type TaskExecutionRecord struct {
	TaskID      string          `json:"task_id"`
	Status      TaskStatus      `json:"status"`
	Output      json.RawMessage `json:"output,omitempty"`
	Usage       BudgetVector    `json:"usage"`
	Failure     *RunFailure     `json:"failure,omitempty"`
	StartedAt   time.Time       `json:"started_at,omitempty"`
	EndedAt     time.Time       `json:"ended_at,omitempty"`
	Attempts    []TaskAttempt   `json:"attempts,omitempty"`
	Discoveries []Discovery     `json:"discoveries,omitempty"`
}

// TaskAttempt preserves one bounded execution attempt for audit and recovery.
type TaskAttempt struct {
	Attempt   int         `json:"attempt"`
	StartedAt time.Time   `json:"started_at,omitempty"`
	EndedAt   time.Time   `json:"ended_at,omitempty"`
	Status    TaskStatus  `json:"status"`
	Failure   *RunFailure `json:"failure,omitempty"`
}

type InvestigationRun struct {
	ID        string                         `json:"id"`
	Contract  InvestigationContract          `json:"contract"`
	Budget    BudgetSnapshot                 `json:"budget"`
	Metrics   RunMetrics                     `json:"metrics,omitempty"`
	Status    RunStatus                      `json:"status"`
	Plan      PlanRevision                   `json:"plan,omitempty"`
	Tasks     map[string]ExecutableTask      `json:"tasks,omitempty"`
	Results   map[string]TaskExecutionRecord `json:"results,omitempty"`
	Evidence  []EvidenceUnit                 `json:"evidence,omitempty"`
	Claims    []VerifiedClaim                `json:"claims,omitempty"`
	Report    InvestigationReport            `json:"report,omitempty"`
	Delivery  *DeliveryResult                `json:"delivery,omitempty"`
	Failure   *RunFailure                    `json:"failure,omitempty"`
	CreatedAt time.Time                      `json:"created_at"`
	UpdatedAt time.Time                      `json:"updated_at"`
}

// RunMetrics is the bounded operational summary persisted with one run.
type RunMetrics struct {
	Rounds           int                           `json:"rounds,omitempty"`
	Tasks            int                           `json:"tasks,omitempty"`
	AgentTasks       int                           `json:"agent_tasks,omitempty"`
	ToolCalls        int                           `json:"tool_calls,omitempty"`
	InputTokens      int64                         `json:"input_tokens,omitempty"`
	OutputTokens     int64                         `json:"output_tokens,omitempty"`
	CostMicros       int64                         `json:"cost_micros,omitempty"`
	Duration         time.Duration                 `json:"duration,omitempty"`
	ComposerFallback bool                          `json:"composer_fallback,omitempty"`
	StageUsage       map[BudgetStage]BudgetVector  `json:"stage_usage,omitempty"`
	ExecutorCounts   map[ExecutorType]int          `json:"executor_counts,omitempty"`
	TaskUsage        map[string]BudgetVector       `json:"task_usage,omitempty"`
	TemplateUsage    map[string]BudgetVector       `json:"template_usage,omitempty"`
	GoalCoverage     map[string]GoalCoverageStatus `json:"goal_coverage,omitempty"`
}

type Composer interface {
	Compose(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error)
}

type ComposerFunc func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error)

func (fn ComposerFunc) Compose(ctx context.Context, contract InvestigationContract, report InvestigationReport) (AnswerDraft, error) {
	return fn(ctx, contract, report)
}

func evidenceID(candidate EvidenceCandidate) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(candidate.SourceKind))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.Target))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.Section))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.Version))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.TimeRange))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.ContentHash))
	return "evidence_" + hex.EncodeToString(hash.Sum(nil))
}

func claimID(candidate ClaimCandidate) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(candidate.GoalID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(candidate.Text))
	return "claim_" + hex.EncodeToString(hash.Sum(nil))
}

func validateSchemaRef(ref agentapi.SchemaRef) error {
	if strings.TrimSpace(ref.ID) == "" || ref.Version <= 0 {
		return fmt.Errorf("schema reference must contain id and positive version")
	}
	return nil
}
