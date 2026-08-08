package reviewworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

type reviewExecutionService interface {
	LoadReviewWorkflowSnapshot(context.Context, string, bool) (*delivery.ReviewWorkflowSnapshot, error)
	ExecuteReviewAssignmentAttempt(
		context.Context,
		string,
		string,
		string,
		string,
		int,
		agentapi.Actor,
		bool,
	) (*delivery.ReviewReport, error)
	EvaluateReviewWorkflow(context.Context, string, agentapi.Actor, bool) ([]delivery.ReviewReport, error)
	CompleteReviewWorkflow(context.Context, string, bool) (*delivery.ReviewGateResult, error)
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
	) (*delivery.ReviewReport, delivery.ReviewUsage, error)
	EvaluateReviewWorkflowWithUsage(
		context.Context,
		string,
		agentapi.Actor,
		bool,
	) ([]delivery.ReviewReport, delivery.ReviewUsage, error)
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
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	return executiontrace.Invoke(ctx, reviewStageTraceSpec, request, executor.execute)
}

var reviewStageTraceSpec = executiontrace.Spec[workflow.NodeRequest, workflow.NodeResult]{
	Operation: "feature_delivery.review_stage",
	Node:      "feature_delivery_stage",
	Input: func(request workflow.NodeRequest) map[string]any {
		return map[string]any{
			"node_id":      request.Node.ID,
			"transform_id": request.Node.TransformID,
			"attempt":      request.Attempt,
			"input_count":  len(request.Inputs),
		}
	},
	Output: func(_ workflow.NodeRequest, result workflow.NodeResult, err error) map[string]any {
		fields := map[string]any{
			"agent_run_id":  result.AgentRunID,
			"completeness":  result.Handoff.Completeness,
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.TotalTokens,
			"tool_calls":    result.Usage.ToolCalls,
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		return fields
	},
}

func (executor *Executor) execute(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	if executor == nil || executor.service == nil {
		return workflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q is unavailable: %w",
			request.Node.TransformID, delivery.ErrUnavailable,
		)
	}
	if err := platformscope.Validate(request.EffectivePermissions.Scopes); err != nil {
		return workflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q permissions: %w",
			request.Node.TransformID,
			err,
		)
	}
	if !platformscope.Has(
		request.EffectivePermissions.Scopes,
		platformscope.FeatureDelivery,
	) {
		return workflow.NodeResult{}, fmt.Errorf(
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
		return workflow.NodeResult{}, fmt.Errorf(
			"feature review transform %q is unsupported",
			request.Node.TransformID,
		)
	}
}

func (executor *Executor) assignment(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	input, err := onlyInput(request)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	reviewRequest, err := decodeRequest(input.Payload)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if reviewRequest.RoundID != roundID {
		return workflow.NodeResult{}, fmt.Errorf(
			"review request round %q does not match workflow run %q: %w",
			reviewRequest.RoundID, request.WorkflowRunID, delivery.ErrConflict,
		)
	}
	snapshot, err := executor.service.LoadReviewWorkflowSnapshot(ctx, roundID, true)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	reviewerID, err := reviewerIDForNode(snapshot.Round.Reviewers, request.Node.ID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	runID, err := agentRunID(request.WorkflowRunID, request.Node.ID, request.Attempt)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	childCtx := executiontrace.WithCorrelation(ctx, executiontrace.Correlation{
		RunID: runID, ParentRunID: request.WorkflowRunID,
		WorkflowRunID: request.WorkflowRunID, AgentRunID: runID,
		WorkflowNodeID: request.Node.ID,
	})
	child, runErr := executiontrace.Invoke(
		childCtx,
		reviewChildRunTraceSpec,
		reviewChildRunInput{
			roundID: roundID, reviewerID: reviewerID, runID: runID, request: request,
		},
		executor.executeAssignment,
	)
	nodeResult := workflow.NodeResult{
		AgentRunID: runID,
		Usage:      toWorkflowUsage(child.usage),
	}
	if runErr != nil {
		return nodeResult, runErr
	}
	payload, err := json.Marshal(child.report)
	if err != nil {
		return nodeResult, fmt.Errorf("marshal review report: %w", err)
	}
	nodeResult.Handoff = workflow.Handoff{
		Payload: payload, Completeness: workflow.Complete,
	}
	return nodeResult, nil
}

type reviewChildRunInput struct {
	roundID    string
	reviewerID string
	runID      string
	request    workflow.NodeRequest
}

type reviewChildRunOutput struct {
	report *delivery.ReviewReport
	usage  delivery.ReviewUsage
}

var reviewChildRunTraceSpec = executiontrace.Spec[reviewChildRunInput, reviewChildRunOutput]{
	Operation: "multi_agent.child_run",
	Node:      "multi_agent_child_run",
	Input: func(input reviewChildRunInput) map[string]any {
		return map[string]any{
			"agent_id":         input.request.Node.TransformID,
			"reviewer_id":      input.reviewerID,
			"workflow_node_id": input.request.Node.ID,
		}
	},
	Output: func(input reviewChildRunInput, output reviewChildRunOutput, err error) map[string]any {
		fields := map[string]any{
			"agent_run_id":  input.runID,
			"input_tokens":  output.usage.InputTokens,
			"output_tokens": output.usage.OutputTokens,
			"total_tokens":  output.usage.TotalTokens,
			"tool_calls":    output.usage.ToolCalls,
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		return fields
	},
}

func (executor *Executor) executeAssignment(
	ctx context.Context,
	input reviewChildRunInput,
) (reviewChildRunOutput, error) {
	if service, ok := executor.service.(reviewExecutionServiceWithUsage); ok {
		report, usage, err := service.ExecuteReviewAssignmentAttemptWithUsage(
			ctx,
			input.roundID,
			input.reviewerID,
			input.request.WorkflowRunID,
			input.runID,
			input.request.Attempt,
			input.request.Actor,
			true,
		)
		return reviewChildRunOutput{report: report, usage: usage}, err
	}
	report, err := executor.service.ExecuteReviewAssignmentAttempt(
		ctx,
		input.roundID,
		input.reviewerID,
		input.request.WorkflowRunID,
		input.runID,
		input.request.Attempt,
		input.request.Actor,
		true,
	)
	return reviewChildRunOutput{report: report}, err
}

func (executor *Executor) adjudicate(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	if _, err := onlyInput(request); err != nil {
		return workflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	var reports []delivery.ReviewReport
	var usage delivery.ReviewUsage
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
	nodeResult := workflow.NodeResult{Usage: toWorkflowUsage(usage)}
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
	nodeResult.Handoff = workflow.Handoff{
		Payload: payload, Completeness: workflow.Complete,
	}
	return nodeResult, nil
}

func (executor *Executor) gate(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	if _, err := onlyInput(request); err != nil {
		return workflow.NodeResult{}, err
	}
	roundID, err := roundIDFromRunID(request.WorkflowRunID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	result, err := executor.service.CompleteReviewWorkflow(ctx, roundID, true)
	if err != nil {
		return workflow.NodeResult{}, executor.handleFailure(ctx, request, roundID, err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return workflow.NodeResult{}, executor.handleFailure(
			ctx,
			request,
			roundID,
			fmt.Errorf("marshal review gate: %w", err),
		)
	}
	return workflow.NodeResult{
		Handoff: workflow.Handoff{
			Payload: payload, Completeness: workflow.Complete,
		},
	}, nil
}

func (executor *Executor) handleFailure(
	ctx context.Context,
	request workflow.NodeRequest,
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

func toWorkflowUsage(usage delivery.ReviewUsage) workflow.WorkflowUsage {
	return workflow.WorkflowUsage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens,
		TotalTokens:     usage.TotalTokens,
		ToolCalls:       usage.ToolCalls,
		CostMicros:      usage.CostMicros,
	}
}

func onlyInput(request workflow.NodeRequest) (workflow.Handoff, error) {
	if len(request.Inputs) != 1 {
		return workflow.Handoff{}, fmt.Errorf(
			"feature review node %q requires exactly one handoff, got %d",
			request.Node.ID, len(request.Inputs),
		)
	}
	return request.Inputs[0], nil
}

var _ workflow.NodeExecutor = (*Executor)(nil)
