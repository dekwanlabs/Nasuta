package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

const defaultPollInterval = 250 * time.Millisecond

// Executor implements only the transforms owned by Feature Delivery.
type Executor struct {
	service      *delivery.Service
	pollInterval time.Duration
}

func NewExecutor(service *delivery.Service) *Executor {
	return &Executor{service: service, pollInterval: defaultPollInterval}
}

func (executor *Executor) SetPollInterval(interval time.Duration) {
	if executor == nil || interval <= 0 {
		return
	}
	executor.pollInterval = interval
}

func (executor *Executor) Execute(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	return executiontrace.Invoke(ctx, featureStageTraceSpec, request, executor.execute)
}

var featureStageTraceSpec = executiontrace.Spec[workflow.NodeRequest, workflow.NodeResult]{
	Operation: "feature_delivery.stage",
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
			"completeness":  result.Handoff.Completeness,
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.TotalTokens,
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
			"feature pipeline transform %q is unavailable: %w",
			request.Node.TransformID, delivery.ErrUnavailable,
		)
	}
	if err := platformscope.Validate(request.EffectivePermissions.Scopes); err != nil {
		return workflow.NodeResult{}, fmt.Errorf(
			"feature pipeline transform %q permissions: %w",
			request.Node.TransformID,
			err,
		)
	}
	if !platformscope.Has(
		request.EffectivePermissions.Scopes,
		platformscope.FeatureDelivery,
	) {
		return workflow.NodeResult{}, fmt.Errorf(
			"feature pipeline transform %q requires %q permission",
			request.Node.TransformID,
			platformscope.FeatureDelivery,
		)
	}
	switch request.Node.TransformID {
	case TransformRequirementAnalysis, TransformTechnicalProposal,
		TransformSystemDesign, TransformImplementationPlan:
		return executor.generate(ctx, request)
	case TransformCoding:
		return executor.code(ctx, request)
	case TransformValidation:
		return executor.validate(ctx, request)
	default:
		return workflow.NodeResult{}, fmt.Errorf(
			"feature pipeline transform %q is unsupported",
			request.Node.TransformID,
		)
	}
}

func (executor *Executor) generate(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	input, err := onlyInput(request)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	kind, err := stageKind(request.Node.TransformID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	var (
		featureID string
		state     State
		options   RequestOptions
	)
	if kind == delivery.KindRequirementAnalysis {
		var pipelineRequest Request
		if err := json.Unmarshal(input.Payload, &pipelineRequest); err != nil {
			return workflow.NodeResult{}, fmt.Errorf("decode pipeline request: %w", err)
		}
		pipelineRequest, err = normalizeRequest(pipelineRequest)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		featureID = pipelineRequest.FeatureID
		options = optionsFromRequest(pipelineRequest)
	} else {
		state, err = decodeState(input.Payload)
		if err != nil {
			return workflow.NodeResult{}, err
		}
		featureID = state.FeatureID
		options = state.Options
		if _, ok := findArtifact(state.Artifacts, expectedParentKind(kind)); !ok ||
			state.CurrentArtifact == nil {
			return workflow.NodeResult{}, fmt.Errorf(
				"pipeline stage %q has no current approved predecessor: %w",
				kind, delivery.ErrConflict,
			)
		}
		expectedParent, ok := findArtifact(state.Artifacts, expectedParentKind(kind))
		if !ok || expectedParent.ID != state.CurrentArtifact.ID {
			return workflow.NodeResult{}, fmt.Errorf(
				"pipeline stage %q predecessor is inconsistent: %w",
				kind, delivery.ErrConflict,
			)
		}
	}
	artifact, run, err := executor.service.GenerateArtifactForWorkflow(
		ctx,
		featureID,
		kind,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		request.Actor.UserID,
		true,
	)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("generate %s: %w", kind, err)
	}
	if state.FeatureID == "" {
		state.FeatureID = featureID
	}
	if state.FeatureID != featureID {
		return workflow.NodeResult{}, fmt.Errorf("pipeline feature id changed: %w", delivery.ErrConflict)
	}
	if state.CurrentArtifact != nil && artifact.ParentArtifactID != state.CurrentArtifact.ID {
		return workflow.NodeResult{}, fmt.Errorf(
			"generated artifact %q parent %q does not match handoff artifact %q: %w",
			artifact.ID, artifact.ParentArtifactID, state.CurrentArtifact.ID, delivery.ErrConflict,
		)
	}
	summary := summarizeArtifact(*artifact)
	state.Options = options
	state.Artifacts = appendArtifact(state.Artifacts, summary)
	state.CurrentArtifact = &summary
	payload, err := state.payload()
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{
		Handoff: workflow.Handoff{Payload: payload, Completeness: workflow.Complete},
		Usage: workflow.WorkflowUsage{
			InputTokens:  run.InputTokens,
			OutputTokens: run.OutputTokens,
			TotalTokens:  run.InputTokens + run.OutputTokens,
		},
	}, nil
}

func (executor *Executor) code(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	input, err := onlyInput(request)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	state, err := decodeState(input.Payload)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if state.CurrentArtifact == nil || state.CurrentArtifact.Kind != delivery.KindImplementationPlan {
		return workflow.NodeResult{}, fmt.Errorf(
			"coding requires the implementation plan as current artifact: %w",
			delivery.ErrConflict,
		)
	}
	design, ok := findArtifact(state.Artifacts, delivery.KindSystemDesign)
	if !ok {
		return workflow.NodeResult{}, fmt.Errorf(
			"coding requires a system design: %w", delivery.ErrConflict,
		)
	}
	options := state.Options
	options.DesignArtifactID = design.ID
	options.PlanArtifactID = state.CurrentArtifact.ID
	run, _, err := executor.service.CreateImplementation(
		ctx,
		state.FeatureID,
		options.implementationOptions(),
		request.Actor.UserID,
		true,
	)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("create implementation: %w", err)
	}
	state.Options = options
	state.Implementation = &ImplementationSummary{
		ID: run.ID, ClientRequestID: run.ClientRequestID, Status: run.Status,
	}
	payload, err := state.payload()
	if err != nil {
		return workflow.NodeResult{}, err
	}
	return workflow.NodeResult{
		Handoff: workflow.Handoff{Payload: payload, Completeness: workflow.Complete},
	}, nil
}

func (executor *Executor) validate(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	input, err := onlyInput(request)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	state, err := decodeState(input.Payload)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if state.Implementation == nil {
		return workflow.NodeResult{}, fmt.Errorf(
			"validation requires an implementation run: %w",
			delivery.ErrConflict,
		)
	}
	run, err := executor.waitForImplementation(ctx, state.Implementation.ID)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	if run.RequestID != state.FeatureID ||
		run.ClientRequestID != state.Implementation.ClientRequestID {
		return workflow.NodeResult{}, fmt.Errorf(
			"implementation %q does not match pipeline state: %w",
			run.ID, delivery.ErrConflict,
		)
	}
	if run.Status != delivery.RunSucceeded || run.ChangeSet == nil {
		return workflow.NodeResult{}, fmt.Errorf(
			"implementation %q finished as %q without a change set: %w",
			run.ID, run.Status, delivery.ErrConflict,
		)
	}
	validation := summarizeValidationResults(run.ChangeSet.ValidationResults)
	result := Result{
		FeatureID:         state.FeatureID,
		Artifacts:         cloneArtifacts(state.Artifacts),
		FinalArtifact:     *state.CurrentArtifact,
		Implementation:    *state.Implementation,
		ChangeSet:         summarizeChangeSet(*run.ChangeSet),
		ValidationResults: validation,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return workflow.NodeResult{}, fmt.Errorf("marshal pipeline result: %w", err)
	}
	return workflow.NodeResult{
		Handoff: workflow.Handoff{Payload: payload, Completeness: workflow.Complete},
	}, nil
}

func (executor *Executor) waitForImplementation(
	ctx context.Context,
	runID string,
) (*delivery.ImplementationRun, error) {
	for {
		run, err := executor.service.GetImplementation(ctx, runID, 0, true)
		if err != nil {
			return nil, fmt.Errorf("load implementation %q: %w", runID, err)
		}
		switch run.Status {
		case delivery.RunSucceeded:
			return run, nil
		case delivery.RunFailed, delivery.RunCancelled, delivery.RunInterrupted:
			return nil, fmt.Errorf(
				"implementation %q ended as %q: %w",
				run.ID, run.Status, delivery.ErrConflict,
			)
		case delivery.RunQueued, delivery.RunPreparing,
			delivery.RunRunning, delivery.RunValidating:
			timer := time.NewTimer(executor.pollInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		default:
			return nil, fmt.Errorf(
				"implementation %q has unknown status %q: %w",
				run.ID, run.Status, delivery.ErrConflict,
			)
		}
	}
}

func onlyInput(request workflow.NodeRequest) (workflow.Handoff, error) {
	if len(request.Inputs) != 1 {
		return workflow.Handoff{}, fmt.Errorf(
			"pipeline node %q requires exactly one input handoff",
			request.Node.ID,
		)
	}
	return request.Inputs[0], nil
}

func decodeState(payload json.RawMessage) (State, error) {
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		return State{}, fmt.Errorf("decode pipeline state: %w", err)
	}
	if state.FeatureID == "" || state.CurrentArtifact == nil {
		return State{}, fmt.Errorf("pipeline state is incomplete: %w", delivery.ErrConflict)
	}
	return state, nil
}

func expectedParentKind(kind delivery.ArtifactKind) delivery.ArtifactKind {
	parent, _ := delivery.ParentKind(kind)
	return parent
}

func findArtifact(artifacts []ArtifactSummary, kind delivery.ArtifactKind) (ArtifactSummary, bool) {
	for _, artifact := range artifacts {
		if artifact.Kind == kind {
			return artifact, true
		}
	}
	return ArtifactSummary{}, false
}

func appendArtifact(artifacts []ArtifactSummary, artifact ArtifactSummary) []ArtifactSummary {
	out := make([]ArtifactSummary, 0, len(artifacts)+1)
	for _, existing := range artifacts {
		if existing.Kind != artifact.Kind {
			out = append(out, existing)
		}
	}
	return append(out, artifact)
}

func cloneArtifacts(artifacts []ArtifactSummary) []ArtifactSummary {
	return append([]ArtifactSummary(nil), artifacts...)
}

func summarizeArtifact(artifact delivery.Artifact) ArtifactSummary {
	return ArtifactSummary{
		ID: artifact.ID, ParentArtifactID: artifact.ParentArtifactID,
		Kind: artifact.Kind, Version: artifact.Version, ContentHash: artifact.ContentHash,
	}
}

func summarizeChangeSet(changeSet delivery.ChangeSet) ChangeSetSummary {
	files := make([]ChangedFileSummary, 0, len(changeSet.Files))
	for _, file := range changeSet.Files {
		files = append(files, ChangedFileSummary{
			Path: file.Path, Status: file.Status, Additions: file.Additions,
			Deletions: file.Deletions, Binary: file.Binary,
		})
	}
	return ChangeSetSummary{
		RunID: changeSet.RunID, WorktreeHead: changeSet.WorktreeHead,
		PatchRelPath: changeSet.PatchRelPath, PatchSHA256: changeSet.PatchSHA256,
		PatchBytes: changeSet.PatchBytes, FilesChanged: changeSet.FilesChanged,
		Additions: changeSet.Additions, Deletions: changeSet.Deletions,
		Files: files, PlanDeviations: append([]delivery.PlanDeviation(nil), changeSet.PlanDeviations...),
		ValidationResults: summarizeValidationResults(changeSet.ValidationResults),
		ProviderSummary:   changeSet.ProviderSummary, CreatedAt: changeSet.CreatedAt,
	}
}

func summarizeValidationResults(results []delivery.ValidationResult) []ValidationSummary {
	out := make([]ValidationSummary, 0, len(results))
	for _, result := range results {
		out = append(out, ValidationSummary{
			Sequence: result.Sequence, Status: result.Status, ExitCode: result.ExitCode,
			DurationMS: result.DurationMS, OutputSummary: result.OutputSummary,
			OutputRelPath: result.OutputRelPath, OutputSHA256: result.OutputSHA256,
			OutputBytes: result.OutputBytes, TimedOut: result.TimedOut,
		})
	}
	return out
}

var _ workflow.NodeExecutor = (*Executor)(nil)
