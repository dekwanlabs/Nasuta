package featurereviewworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

type reviewExecutionService interface {
	LoadReviewWorkflowSnapshot(context.Context, string, bool) (*featuredelivery.ReviewWorkflowSnapshot, error)
	ExecuteReviewAssignmentAttempt(
		context.Context,
		string,
		string,
		string,
		string,
		int,
		agentapi.Actor,
		bool,
	) (*featuredelivery.ReviewReport, error)
	EvaluateReviewWorkflow(context.Context, string, agentapi.Actor, bool) ([]featuredelivery.ReviewReport, error)
	CompleteReviewWorkflow(context.Context, string, bool) (*featuredelivery.ReviewGateResult, error)
	FailReviewWorkflow(context.Context, string, error, bool) error
}

type reviewExecutionServiceWithUsage interface {
	ExecuteReviewAssignmentAttemptWithUsage(
		context.Context,
		string,
		string,
		string,
		string,
		int,
		agentapi.Actor,
		bool,
	) (*featuredelivery.ReviewReport, featuredelivery.ReviewUsage, error)
	EvaluateReviewWorkflowWithUsage(
		context.Context,
		string,
		agentapi.Actor,
		bool,
	) ([]featuredelivery.ReviewReport, featuredelivery.ReviewUsage, error)
}

// Executor owns the Feature Review transforms used by the generic Workflow runtime.
type Executor struct {
	service reviewExecutionService
}

func NewExecutor(service reviewExecutionService) *Executor {
	return &Executor{service: service}
}

func (executor *Executor) Execute(
	ctx context.Context,
	request agentworkflow.NodeRequest,
) (agentworkflow.NodeResult, error) {
	if executor == nil || executor.service == nil {
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q is unavailable: %w",
			request.Node.TransformID, featuredelivery.ErrUnavailable,
		)
	}
	if err := platformscope.Validate(request.EffectivePermissions.Scopes); err != nil {
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q permissions: %w",
			request.Node.TransformID,
			err,
		)
	}
	if !platformscope.Has(
		request.EffectivePermissions.Scopes,
		platformscope.FeatureDelivery,
	) {
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q requires %q permission",
			request.Node.TransformID,
			platformscope.FeatureDelivery,
		)
	}
	switch request.Node.TransformID {
	case TransformAssignment:
		return executor.assignment(ctx, request)
	case TransformAdjudication:
		return executor.adjudicate(ctx, request)
	case TransformGate:
		return executor.gate(ctx, request)
	default:
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q is unsupported",
			request.Node.TransformID,
		)
	}
}

func (executor *Executor) assignment(
	ctx context.Context,
	request agentworkflow.NodeRequest,
) (agentworkflow.NodeResult, error) {
	input, err := onlyInput(request)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	reviewRequest, err := decodeRequest(input.Payload)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	if reviewRequest.RoundID != roundID {
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"review request round %q does not match workflow run %q: %w",
			reviewRequest.RoundID, request.WorkflowRunID, featuredelivery.ErrConflict,
		)
	}
	snapshot, err := executor.service.LoadReviewWorkflowSnapshot(ctx, roundID, true)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	reviewerID, err := reviewerIDForNode(snapshot.Round.Reviewers, request.Node.ID)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	runID, err := agentRunID(request.WorkflowRunID, request.Node.ID, request.Attempt)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	var report *featuredelivery.ReviewReport
	var usage featuredelivery.ReviewUsage
	var runErr error
	if service, ok := executor.service.(reviewExecutionServiceWithUsage); ok {
		report, usage, runErr = service.ExecuteReviewAssignmentAttemptWithUsage(
			ctx,
			roundID,
			reviewerID,
			request.WorkflowRunID,
			runID,
			request.Attempt,
			request.Actor,
			true,
		)
	} else {
		report, runErr = executor.service.ExecuteReviewAssignmentAttempt(
			ctx,
			roundID,
			reviewerID,
			request.WorkflowRunID,
			runID,
			request.Attempt,
			request.Actor,
			true,
		)
	}
	nodeResult := agentworkflow.NodeResult{
		AgentRunID: runID,
		Usage:      toWorkflowUsage(usage),
	}
	if runErr != nil {
		return nodeResult, runErr
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nodeResult, fmt.Errorf("marshal review report: %w", err)
	}
	nodeResult.Handoff = agentworkflow.Handoff{
		Payload: payload, Completeness: agentworkflow.Complete,
	}
	return nodeResult, nil
}

func (executor *Executor) adjudicate(
	ctx context.Context,
	request agentworkflow.NodeRequest,
) (agentworkflow.NodeResult, error) {
	if _, err := onlyInput(request); err != nil {
		return agentworkflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	var reports []featuredelivery.ReviewReport
	var usage featuredelivery.ReviewUsage
	var runErr error
	if service, ok := executor.service.(reviewExecutionServiceWithUsage); ok {
		reports, usage, runErr = service.EvaluateReviewWorkflowWithUsage(
			ctx, roundID, request.Actor, true,
		)
	} else {
		reports, runErr = executor.service.EvaluateReviewWorkflow(
			ctx, roundID, request.Actor, true,
		)
	}
	nodeResult := agentworkflow.NodeResult{Usage: toWorkflowUsage(usage)}
	if runErr != nil {
		return nodeResult, executor.handleFailure(ctx, request, roundID, runErr)
	}
	payload, err := json.Marshal(reports)
	if err != nil {
		return nodeResult, executor.handleFailure(
			ctx,
			request,
			roundID,
			fmt.Errorf("marshal adjudicated review reports: %w", err),
		)
	}
	nodeResult.Handoff = agentworkflow.Handoff{
		Payload: payload, Completeness: agentworkflow.Complete,
	}
	return nodeResult, nil
}

func (executor *Executor) gate(
	ctx context.Context,
	request agentworkflow.NodeRequest,
) (agentworkflow.NodeResult, error) {
	if _, err := onlyInput(request); err != nil {
		return agentworkflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return agentworkflow.NodeResult{}, err
	}
	result, err := executor.service.CompleteReviewWorkflow(ctx, roundID, true)
	if err != nil {
		return agentworkflow.NodeResult{}, executor.handleFailure(ctx, request, roundID, err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return agentworkflow.NodeResult{}, executor.handleFailure(
			ctx,
			request,
			roundID,
			fmt.Errorf("marshal review gate: %w", err),
		)
	}
	return agentworkflow.NodeResult{
		Handoff: agentworkflow.Handoff{
			Payload: payload, Completeness: agentworkflow.Complete,
		},
	}, nil
}

func (executor *Executor) handleFailure(
	ctx context.Context,
	request agentworkflow.NodeRequest,
	roundID string,
	cause error,
) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	var classified interface{ Retryable() bool }
	if request.Attempt < request.Node.Retry.MaxAttempts &&
		errors.As(cause, &classified) &&
		classified.Retryable() {
		return cause
	}
	return executor.failRound(ctx, roundID, cause)
}

func (executor *Executor) failRound(
	ctx context.Context,
	roundID string,
	cause error,
) error {
	if err := executor.service.FailReviewWorkflow(
		context.WithoutCancel(ctx), roundID, cause, true,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func toWorkflowUsage(usage featuredelivery.ReviewUsage) agentworkflow.WorkflowUsage {
	return agentworkflow.WorkflowUsage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens,
		TotalTokens:     usage.TotalTokens,
		ToolCalls:       usage.ToolCalls,
		CostMicros:      usage.CostMicros,
	}
}

func onlyInput(request agentworkflow.NodeRequest) (agentworkflow.Handoff, error) {
	if len(request.Inputs) != 1 {
		return agentworkflow.Handoff{}, fmt.Errorf(
			"feature review node %q requires exactly one handoff, got %d",
			request.Node.ID, len(request.Inputs),
		)
	}
	return request.Inputs[0], nil
}

var _ agentworkflow.NodeExecutor = (*Executor)(nil)
