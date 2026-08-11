package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/log"
)

type preparedRun struct {
	definition WorkflowDefinition
	record     WorkflowRunRecord
	input      Handoff
}

func (service *Service) Execute(ctx context.Context, request ExecuteRequest) (Result, error) {
	orchestrator, err := service.executionCapability()
	if err != nil {
		return Result{}, err
	}
	definition, selection, err := service.resolveDefinitionFor(
		request.Workflow,
		request.Actor,
		request.Scenario,
	)
	if err != nil {
		return Result{}, err
	}
	prepared, err := prepareWorkflowRun(orchestrator, definition, selection, request)
	if err != nil {
		return Result{}, err
	}
	runCtx, release, err := service.registerActive(ctx, prepared.record.ID, false)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := service.store.StartWorkflow(runCtx, prepared.record, prepared.input); err != nil {
		return Result{}, err
	}
	return service.executePrepared(runCtx, orchestrator, prepared)
}

// Start persists a Run before executing it independently of the request lifetime.
func (service *Service) Start(
	ctx context.Context,
	request StartRequest,
) (*WorkflowRunRecord, error) {
	if request.Actor.UserID <= 0 {
		return nil, fmt.Errorf("workflow actor identity is required: %w", ErrInvalid)
	}
	orchestrator, err := service.executionCapability()
	if err != nil {
		return nil, err
	}
	const scenario = "workflow.api"
	definition, selection, err := service.resolveDefinitionFor(
		request.Workflow,
		request.Actor,
		scenario,
	)
	if err != nil {
		return nil, err
	}
	if !request.Admin && !definitionIsKnowledgeReadOnly(definition) {
		return nil, ErrForbidden
	}
	permissions := clonePermissionPolicy(definition.Permissions)
	prepared, err := prepareWorkflowRun(orchestrator, definition, selection, ExecuteRequest{
		RunID:               request.RunID,
		Workflow:            request.Workflow,
		Input:               request.Input,
		Actor:               request.Actor,
		ActorPermissions:    permissions,
		Scenario:            scenario,
		ScenarioPermissions: permissions,
	})
	if err != nil {
		return nil, err
	}
	runCtx, release, err := service.registerActive(ctx, prepared.record.ID, true)
	if err != nil {
		return nil, err
	}
	if err := service.store.StartWorkflow(ctx, prepared.record, prepared.input); err != nil {
		release()
		return nil, err
	}
	go func() {
		defer release()
		if _, runErr := service.executePrepared(runCtx, orchestrator, prepared); runErr != nil &&
			!errors.Is(runErr, context.Canceled) &&
			!errors.Is(runErr, context.DeadlineExceeded) &&
			!errors.Is(runErr, ErrHumanApprovalRequired) {
			log.Warnf(
				"[workflow] background run %s failed: %v",
				prepared.record.ID,
				runErr,
			)
		}
	}()
	run := detachedWorkflowRunRecord(prepared.record)
	return &run, nil
}

func (service *Service) executePrepared(
	ctx context.Context,
	orchestrator *Orchestrator,
	prepared preparedRun,
) (Result, error) {
	observer := &storeRunObserver{store: service.store}
	result, runErr := orchestrator.RunObserved(ctx, prepared.definition, RunRequest{
		RunID: prepared.record.ID, ParentRunID: prepared.record.ParentRunID,
		Input: prepared.input.Payload,
		Actor: agentapi.Actor{
			UserID:   prepared.record.ActorUserID,
			TenantID: prepared.record.ActorTenantID,
		},
		ActorPermissions:    prepared.record.ActorPermissions,
		ScenarioPermissions: prepared.record.ScenarioPermissions,
		StartedAt:           prepared.record.StartedAt,
	}, observer)
	status, errorCode := workflowResultStatus(runErr)
	var output *Handoff
	if runErr == nil {
		output = &result.Output
	}
	persistCtx, cancel := workflowPersistenceContext(ctx)
	finishErr := service.store.FinishWorkflow(
		persistCtx,
		prepared.record.ID,
		status,
		errorCode,
		output,
		time.Now().UTC(),
	)
	cancel()
	if finishErr != nil {
		if runErr != nil {
			return Result{}, errors.Join(runErr, finishErr)
		}
		return Result{}, finishErr
	}
	if runErr != nil {
		return Result{}, runErr
	}
	return result, nil
}

func randomWorkflowRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate workflow run id: %w", err)
	}
	return "workflow_" + hex.EncodeToString(id[:]), nil
}

func (service *Service) resolveDefinitionFor(
	ref DefinitionRef,
	actor agentapi.Actor,
	scenario string,
) (WorkflowDefinition, DefinitionSelection, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if !canonicalID.MatchString(ref.ID) || ref.Version < 0 {
		return WorkflowDefinition{}, DefinitionSelection{}, fmt.Errorf(
			"workflow reference is invalid: %w",
			ErrInvalid,
		)
	}
	definition, selection, err := service.catalog.ResolveFor(
		ref,
		StableSelectionKey(actor, scenario),
	)
	if err != nil {
		return WorkflowDefinition{}, DefinitionSelection{}, err
	}
	return definition, selection, nil
}

func prepareWorkflowRun(
	orchestrator *Orchestrator,
	definition WorkflowDefinition,
	selection DefinitionSelection,
	request ExecuteRequest,
) (preparedRun, error) {
	if orchestrator == nil || orchestrator.schemas == nil {
		return preparedRun{}, ErrUnavailable
	}
	if err := scope.Validate(request.ActorPermissions.Scopes); err != nil {
		return preparedRun{}, fmt.Errorf("workflow actor permissions: %v: %w", err, ErrInvalid)
	}
	if err := scope.Validate(request.ScenarioPermissions.Scopes); err != nil {
		return preparedRun{}, fmt.Errorf("workflow scenario permissions: %v: %w", err, ErrInvalid)
	}
	if err := scope.EnsureSubset(
		definition.Permissions.Scopes,
		request.ActorPermissions.Scopes,
	); err != nil {
		return preparedRun{}, fmt.Errorf(
			"workflow %q permissions exceed actor permissions: %v: %w",
			definition.ID,
			err,
			ErrForbidden,
		)
	}
	if err := scope.EnsureSubset(
		definition.Permissions.Scopes,
		request.ScenarioPermissions.Scopes,
	); err != nil {
		return preparedRun{}, fmt.Errorf(
			"workflow %q permissions exceed scenario permissions: %v: %w",
			definition.ID,
			err,
			ErrForbidden,
		)
	}
	runID := strings.TrimSpace(request.RunID)
	if runID == "" {
		var err error
		runID, err = randomWorkflowRunID()
		if err != nil {
			return preparedRun{}, err
		}
	} else if !canonicalID.MatchString(runID) {
		return preparedRun{}, fmt.Errorf("workflow run id %q is invalid: %w", runID, ErrInvalid)
	}
	parentRunID := strings.TrimSpace(request.ParentRunID)
	if parentRunID != "" && !canonicalID.MatchString(parentRunID) {
		return preparedRun{}, fmt.Errorf("workflow parent run id %q is invalid: %w", parentRunID, ErrInvalid)
	}
	startedAt := time.Now().UTC()
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID:  runID,
		ProducerNodeID: "workflow.input",
		Schema:         definition.InputSchema,
		Payload:        request.Input,
		Completeness:   Complete,
		CreatedAt:      startedAt,
	}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
	if err != nil {
		return preparedRun{}, fmt.Errorf(
			"workflow %q input: %v: %w",
			definition.ID,
			err,
			ErrInvalid,
		)
	}
	record := WorkflowRunRecord{
		ID:                  runID,
		ParentRunID:         parentRunID,
		WorkflowID:          definition.ID,
		WorkflowVersion:     definition.Version,
		WorkflowHash:        definition.ContentHash,
		Selection:           selection,
		InputHash:           input.ContentHash,
		ActorUserID:         request.Actor.UserID,
		ActorTenantID:       strings.TrimSpace(request.Actor.TenantID),
		ActorPermissions:    clonePermissionPolicy(request.ActorPermissions),
		Scenario:            strings.TrimSpace(request.Scenario),
		ScenarioPermissions: clonePermissionPolicy(request.ScenarioPermissions),
		Status:              RunRunning,
		Budget:              definition.Budget,
		StartedAt:           startedAt,
	}
	return preparedRun{definition: definition, record: record, input: input}, nil
}

func definitionIsKnowledgeReadOnly(definition WorkflowDefinition) bool {
	if !permissionIsKnowledgeReadOnly(definition.Permissions) {
		return false
	}
	for _, node := range definition.Nodes {
		if !permissionIsKnowledgeReadOnly(node.Permissions) {
			return false
		}
	}
	return true
}

func permissionIsKnowledgeReadOnly(policy agentapi.PermissionPolicy) bool {
	return len(policy.Scopes) > 0 &&
		!scope.HasSideEffect(policy.Scopes) &&
		scope.Has(policy.Scopes, scope.KnowledgeRead)
}

func clonePermissionPolicy(policy agentapi.PermissionPolicy) agentapi.PermissionPolicy {
	policy.Scopes = append([]string(nil), policy.Scopes...)
	return policy
}

func detachedWorkflowRunRecord(run WorkflowRunRecord) WorkflowRunRecord {
	run.ActorPermissions = clonePermissionPolicy(run.ActorPermissions)
	run.ScenarioPermissions = clonePermissionPolicy(run.ScenarioPermissions)
	if run.EndedAt != nil {
		endedAt := *run.EndedAt
		run.EndedAt = &endedAt
	}
	return run
}
