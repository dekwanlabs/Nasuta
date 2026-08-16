package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

const qaWorkflowEscalationBuilderID = "qa.investigation.task_contract"

type workflowEscalationParentLoader struct {
	runs *run.Store
}

func (loader workflowEscalationParentLoader) LoadWorkflowEscalationParent(
	ctx context.Context,
	parentRunID string,
) (agentapi.WorkflowEscalationParent, error) {
	if loader.runs == nil {
		return agentapi.WorkflowEscalationParent{}, workflow.ErrUnavailable
	}
	record, err := loader.runs.GetWorkflowEscalationParent(ctx, parentRunID)
	if err != nil {
		return agentapi.WorkflowEscalationParent{}, err
	}
	if record.RunKind != run.KindQAParent ||
		(record.Status != run.StatusRunning && record.Status != run.StatusPaused) {
		return agentapi.WorkflowEscalationParent{}, run.ErrNotActive
	}
	if record.UserID <= 0 || strings.TrimSpace(record.Question) == "" {
		return agentapi.WorkflowEscalationParent{}, fmt.Errorf(
			"workflow escalation parent identity is invalid",
		)
	}
	return agentapi.WorkflowEscalationParent{
		RunID:    record.ID,
		Question: record.Question,
		Actor:    agentapi.Actor{UserID: record.UserID},
		Permissions: agentapi.PermissionPolicy{
			Scopes: []string{scope.KnowledgeRead},
		},
		Correlation: agentapi.Correlation{
			SessionID: record.SessionID, ParentRunID: record.ParentRunID,
			WorkflowRunID: record.WorkflowRunID,
		},
		Limits: record.RunLimits,
		Remaining: agentapi.WorkflowEscalationBudget{
			MaxTotalTokens: remainingEscalationBudget(
				record.RunLimits.MaxTotalTokens,
				record.TotalTokens,
			),
			MaxCostMicros: remainingEscalationBudget(
				record.RunLimits.MaxCostMicros,
				record.CostMicros,
			),
			Deadline: record.RunLimits.Deadline,
		},
	}, nil
}

func remainingEscalationBudget(limit, used int64) int64 {
	if limit <= 0 {
		return 0
	}
	if used >= limit {
		return -1
	}
	return limit - used
}

type workflowEscalationHandoffResolver struct {
	runs    *run.Store
	schemas *agentapi.SchemaRegistry
}

func (resolver workflowEscalationHandoffResolver) ResolveWorkflowEscalationHandoff(
	ctx context.Context,
	parent agentapi.WorkflowEscalationParent,
	request agentapi.WorkflowEscalationRequest,
) (workflow.WorkflowEscalationHandoff, error) {
	if len(request.EvidenceRefs) == 0 && len(request.ReportRefs) == 0 {
		return workflow.WorkflowEscalationHandoff{}, nil
	}
	if resolver.runs == nil {
		return workflow.WorkflowEscalationHandoff{}, fmt.Errorf(
			"workflow escalation handoff is unavailable",
		)
	}
	handoff := workflow.WorkflowEscalationHandoff{}
	if len(request.EvidenceRefs) > 0 {
		resolved, err := resolver.runs.ResolveWorkflowEscalationEvidence(
			ctx,
			parent.RunID,
			request.DelegationID,
			request.EvidenceRefs,
		)
		if err != nil {
			return workflow.WorkflowEscalationHandoff{}, err
		}
		handoff.Evidence = make(
			[]workflow.ResolvedWorkflowEscalationEvidence,
			0,
			len(resolved),
		)
		for _, evidence := range resolved {
			handoff.Evidence = append(
				handoff.Evidence,
				workflow.ResolvedWorkflowEscalationEvidence{
					Ref:  evidence.Ref,
					Unit: evidence.Unit,
				},
			)
		}
	}
	if len(request.ReportRefs) == 0 {
		return handoff, nil
	}
	if resolver.schemas == nil ||
		strings.TrimSpace(request.DelegationID) == "" {
		return workflow.WorkflowEscalationHandoff{}, fmt.Errorf(
			"delegation report handoff is unavailable",
		)
	}
	reports := make(
		[]agentapi.WorkflowEscalationReport,
		0,
		len(request.ReportRefs),
	)
	for _, ref := range request.ReportRefs {
		artifact, err := resolver.runs.GetDelegationReport(
			ctx,
			parent.RunID,
			request.DelegationID,
			ref,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workflow.WorkflowEscalationHandoff{}, fmt.Errorf(
					"delegation report %q does not belong to the parent handoff",
					ref,
				)
			}
			return workflow.WorkflowEscalationHandoff{}, err
		}
		if err := resolver.schemas.Validate(
			artifact.Schema,
			artifact.Content,
		); err != nil {
			return workflow.WorkflowEscalationHandoff{}, fmt.Errorf(
				"validate delegation report %q: %w",
				ref,
				err,
			)
		}
		reports = append(reports, agentapi.WorkflowEscalationReport{
			Ref: ref, RunID: artifact.RunID, Schema: artifact.Schema,
			ContentHash: artifact.ContentHash,
			Payload:     append(json.RawMessage(nil), artifact.Content...),
		})
	}
	handoff.Reports = reports
	return handoff, nil
}

type qaWorkflowEscalationBuilder struct {
	payloadTokens int
}

func (builder qaWorkflowEscalationBuilder) BuildWorkflowEscalation(
	_ context.Context,
	request agentapi.WorkflowEscalationBuildRequest,
) (agentapi.WorkflowEscalationBuildResult, error) {
	if builder.payloadTokens <= 0 {
		return agentapi.WorkflowEscalationBuildResult{}, fmt.Errorf(
			"workflow escalation payload budget is unavailable",
		)
	}
	if len(request.Request.FocusFacets) == 0 ||
		len(request.Request.FocusFacets) > 10 {
		return agentapi.WorkflowEscalationBuildResult{}, fmt.Errorf(
			"workflow escalation requires between 1 and 10 focus facets",
		)
	}
	source := workflowEscalationEvidenceSource(request.Capability.ID)
	if source == "" {
		return agentapi.WorkflowEscalationBuildResult{}, fmt.Errorf(
			"capability %q has no QA workflow evidence source",
			request.Capability.ID,
		)
	}
	goals := make(
		[]platformagent.EvidenceGoal,
		0,
		len(request.Request.FocusFacets),
	)
	for _, facet := range request.Request.FocusFacets {
		goals = append(goals, platformagent.EvidenceGoal{
			ID: facet, Facet: facet, Required: true,
			Sources:         []agentapi.EvidenceSource{source},
			Freshness:       request.Capability.Freshness,
			MinimumCoverage: 1,
			HighRisk: request.Request.Reason ==
				agentapi.EscalationHighRiskVerificationRequired,
		})
	}
	seedMaterial := escalationReportBlocks(request.Reports)
	if len(request.Evidence) > 0 {
		seedMaterial = append(seedMaterial, agentapi.ContextBlock{
			Source:      "qa.evidence",
			Title:       "Escalated parent evidence",
			Content:     "",
			Evidence:    cloneWorkflowEscalationEvidence(request.Evidence),
			Complete:    false,
			ContentHash: hashInvestigationContent(""),
		})
	}
	contract := platformagent.TaskContract{
		TaskID:        request.Parent.RunID,
		Question:      request.Parent.Question,
		Objective:     request.Request.Objective,
		Entities:      []platformagent.EntityRef{},
		EvidenceGoals: goals,
		Context: platformagent.TaskContext{
			SeedMaterial: seedMaterial,
		},
	}
	if request.Parent.Correlation.SessionID != "" {
		contract.Context.ConversationRefs = []platformagent.ConversationRef{{
			SessionID: request.Parent.Correlation.SessionID,
		}}
	}
	input, err := marshalInvestigationContract(contract, builder.payloadTokens)
	if err != nil {
		return agentapi.WorkflowEscalationBuildResult{}, err
	}
	return agentapi.WorkflowEscalationBuildResult{
		Input:        input,
		SeedEvidence: cloneWorkflowEscalationEvidence(request.Evidence),
	}, nil
}

func escalationReportBlocks(
	reports []agentapi.WorkflowEscalationReport,
) []agentapi.ContextBlock {
	if len(reports) == 0 {
		return nil
	}
	blocks := make([]agentapi.ContextBlock, 0, len(reports))
	for _, report := range reports {
		blocks = append(blocks, agentapi.ContextBlock{
			Source:  "delegation.report",
			Title:   "Delegation report " + report.Ref,
			Content: string(report.Payload),
			References: []agentapi.Reference{
				{
					Type: "delegation_report", Label: report.RunID,
					Target: report.Ref,
				},
				{
					Type: "sha256", Target: report.ContentHash,
				},
			},
			Complete:    true,
			ContentHash: report.ContentHash,
		})
	}
	return blocks
}

func cloneWorkflowEscalationEvidence(
	units []tool.EvidenceUnit,
) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	cloned := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		unit.Sections = append([]string(nil), unit.Sections...)
		unit.Facets = append([]string(nil), unit.Facets...)
		cloned[index] = unit
	}
	return cloned
}

func workflowEscalationEvidenceSource(
	capabilityID string,
) agentapi.EvidenceSource {
	switch capabilityID {
	case "knowledge.code.inspect", "knowledge.docs.verify":
		return agentapi.EvidenceSourceInternal
	case "knowledge.service.trace":
		return agentapi.EvidenceSourceRuntime
	default:
		return ""
	}
}

// buildDefaultWorkflowBindingRegistrations binds the built-in escalation
// capabilities to one exact Workflow using a prepared capability snapshot.
func buildDefaultWorkflowBindingRegistrations(
	capabilities workflow.CapabilityResolver,
	version int64,
	definition workflow.Definition,
	payloadTokens int,
) ([]workflow.WorkflowBindingRegistration, error) {
	if capabilities == nil {
		return nil, fmt.Errorf("workflow binding capability registry is required")
	}
	if payloadTokens <= 0 {
		return nil, fmt.Errorf("workflow binding payload budget must be positive")
	}
	builder := qaWorkflowEscalationBuilder{payloadTokens: payloadTokens}
	capabilityIDs := []string{
		"knowledge.code.inspect",
		"knowledge.service.trace",
		"knowledge.docs.verify",
	}
	registrations := make(
		[]workflow.WorkflowBindingRegistration,
		0,
		len(capabilityIDs),
	)
	for _, capabilityID := range capabilityIDs {
		capability, err := capabilities.Resolve(agentapi.CapabilityRef{
			ID: capabilityID, Version: version,
		})
		if err != nil {
			return nil, err
		}
		registrations = append(registrations, workflow.WorkflowBindingRegistration{
			Binding: agentapi.WorkflowBinding{
				ID:      "qa.escalation." + capabilityID,
				Version: version,
				Capability: agentapi.CapabilityRef{
					ID: capability.ID, Version: capability.Version,
				},
				CapabilityHash: capability.ContentHash,
				AllowedReasons: []agentapi.WorkflowEscalationReason{
					agentapi.EscalationStrongTaskDependencies,
					agentapi.EscalationDurableExecutionRequired,
					agentapi.EscalationHighRiskVerificationRequired,
					agentapi.EscalationChildLimitExceeded,
					agentapi.EscalationParentTimeBudgetInsufficient,
					agentapi.EscalationScenarioRequiresWorkflow,
				},
				Workflow: agentapi.WorkflowDefinitionRef{
					ID: definition.ID, Version: definition.Version,
					ContentHash: definition.ContentHash,
				},
				Scenario: workflow.FlowID,
				ScenarioPermissions: agentapi.PermissionPolicy{
					Scopes: []string{scope.KnowledgeRead},
				},
				InputSchema: definition.InputSchema,
				BuilderID:   qaWorkflowEscalationBuilderID,
			},
			Builder: builder,
		})
	}
	return registrations, nil
}
