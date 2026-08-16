package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

type WorkflowEscalationReason string

const (
	EscalationStrongTaskDependencies       WorkflowEscalationReason = "strong_task_dependencies"
	EscalationDurableExecutionRequired     WorkflowEscalationReason = "durable_execution_required"
	EscalationHumanApprovalRequired        WorkflowEscalationReason = "human_approval_required"
	EscalationHighRiskVerificationRequired WorkflowEscalationReason = "high_risk_verification_required"
	EscalationChildLimitExceeded           WorkflowEscalationReason = "child_limit_exceeded"
	EscalationParentTimeBudgetInsufficient WorkflowEscalationReason = "parent_time_budget_insufficient"
	EscalationScenarioRequiresWorkflow     WorkflowEscalationReason = "scenario_requires_workflow"
)

type WorkflowEscalationStatus string

const (
	EscalationAccepted       WorkflowEscalationStatus = "accepted"
	EscalationAlreadyStarted WorkflowEscalationStatus = "already_started"
	EscalationRejected       WorkflowEscalationStatus = "rejected"
)

const (
	WorkflowUnavailable        = "workflow_unavailable"
	WorkflowBindingNotFound    = "workflow_binding_not_found"
	WorkflowReasonNotAllowed   = "workflow_reason_not_allowed"
	WorkflowPermissionDenied   = "workflow_permission_denied"
	WorkflowInvalidHandoff     = "workflow_invalid_handoff"
	WorkflowBudgetInsufficient = "workflow_budget_insufficient"
	WorkflowStartConflict      = "workflow_start_conflict"
	WorkflowStartFailed        = "workflow_start_failed"
)

type WorkflowEscalationRequest struct {
	RequestID      string                   `json:"request_id"`
	ParentRunID    string                   `json:"parent_run_id"`
	DelegationID   string                   `json:"delegation_id,omitempty"`
	Capability     CapabilityRef            `json:"capability"`
	CapabilityHash string                   `json:"capability_content_hash"`
	Reason         WorkflowEscalationReason `json:"reason"`
	Objective      string                   `json:"objective"`
	FocusFacets    []string                 `json:"focus_facets,omitempty"`
	EvidenceRefs   []string                 `json:"evidence_refs,omitempty"`
	ReportRefs     []string                 `json:"report_refs,omitempty"`
}

type WorkflowEscalationReceipt struct {
	RequestID      string                   `json:"request_id"`
	WorkflowRunID  string                   `json:"workflow_run_id,omitempty"`
	BindingID      string                   `json:"binding_id,omitempty"`
	BindingVersion int64                    `json:"binding_version,omitempty"`
	Status         WorkflowEscalationStatus `json:"status"`
	ErrorCode      string                   `json:"error_code,omitempty"`
}

type WorkflowEscalator interface {
	Escalate(context.Context, WorkflowEscalationRequest) (WorkflowEscalationReceipt, error)
}

// WorkflowDefinitionRef pins the immutable Workflow selected by an application binding.
type WorkflowDefinitionRef struct {
	ID          string `json:"id"`
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
}

// WorkflowBinding is application-owned metadata. Executable builders are registered
// separately so ContentHash covers only canonical, serializable facts.
type WorkflowBinding struct {
	ID                  string                     `json:"id"`
	Version             int64                      `json:"version"`
	Capability          CapabilityRef              `json:"capability"`
	CapabilityHash      string                     `json:"capability_content_hash"`
	AllowedReasons      []WorkflowEscalationReason `json:"allowed_reasons"`
	Workflow            WorkflowDefinitionRef      `json:"workflow"`
	Scenario            string                     `json:"scenario"`
	ScenarioPermissions PermissionPolicy           `json:"scenario_permissions"`
	InputSchema         SchemaRef                  `json:"input_schema"`
	BuilderID           string                     `json:"builder_id"`
	ContentHash         string                     `json:"content_hash"`
}

// WorkflowEscalationBudget is the server-loaded budget still available to a parent.
type WorkflowEscalationBudget struct {
	MaxTotalTokens int64     `json:"max_total_tokens,omitempty"`
	MaxCostMicros  int64     `json:"max_cost_micros,omitempty"`
	Deadline       time.Time `json:"deadline,omitempty"`
}

// WorkflowEscalationParent contains only authoritative parent facts loaded by the server.
type WorkflowEscalationParent struct {
	RunID       string                   `json:"run_id"`
	Question    string                   `json:"question"`
	Actor       Actor                    `json:"actor"`
	Permissions PermissionPolicy         `json:"permissions"`
	Correlation Correlation              `json:"correlation"`
	Limits      RunLimits                `json:"limits"`
	Remaining   WorkflowEscalationBudget `json:"remaining"`
}

// WorkflowEscalationReport is a bounded, content-addressed child projection.
type WorkflowEscalationReport struct {
	Ref         string          `json:"ref"`
	RunID       string          `json:"run_id"`
	Schema      SchemaRef       `json:"schema"`
	ContentHash string          `json:"content_hash"`
	Payload     json.RawMessage `json:"payload"`
}

// WorkflowEscalationBuildRequest is supplied only after ownership, hash, permission,
// and budget checks have succeeded.
type WorkflowEscalationBuildRequest struct {
	Request    WorkflowEscalationRequest  `json:"request"`
	Parent     WorkflowEscalationParent   `json:"parent"`
	Capability Capability                 `json:"capability"`
	Binding    WorkflowBinding            `json:"binding"`
	Evidence   []tool.EvidenceUnit        `json:"evidence,omitempty"`
	Reports    []WorkflowEscalationReport `json:"reports,omitempty"`
}

type WorkflowEscalationBuildResult struct {
	Input        json.RawMessage     `json:"input"`
	SeedEvidence []tool.EvidenceUnit `json:"seed_evidence,omitempty"`
}

type WorkflowEscalationInputBuilder interface {
	BuildWorkflowEscalation(
		context.Context,
		WorkflowEscalationBuildRequest,
	) (WorkflowEscalationBuildResult, error)
}

type WorkflowEscalationInputBuilderFunc func(
	context.Context,
	WorkflowEscalationBuildRequest,
) (WorkflowEscalationBuildResult, error)

func (build WorkflowEscalationInputBuilderFunc) BuildWorkflowEscalation(
	ctx context.Context,
	request WorkflowEscalationBuildRequest,
) (WorkflowEscalationBuildResult, error) {
	return build(ctx, request)
}
