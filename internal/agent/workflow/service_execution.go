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
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/log"
)

type preparedRun struct {
	definition Definition
	record     RunRecord
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
	prepared, err := prepareStart(orchestrator, definition, selection, request)
	if err != nil {
		return Result{}, err
	}
	runCtx, release, err := service.registerActive(ctx, prepared.record.ID, false)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := service.store.StartRun(runCtx, prepared.record, prepared.input); err != nil {
		return Result{}, err
	}
	return service.executePrepared(runCtx, orchestrator, prepared)
}

// Start persists a Run before executing it independently of the request lifetime.
func (service *Service) Start(
	ctx context.Context,
	request StartRequest,
) (*RunRecord, error) {
	if request.Actor.UserID <= 0 {
		return nil, fmt.Errorf("workflow actor identity is required: %w", ErrInvalid)
	}
	orchestrator, err := service.executionCapability()
	if err != nil {
		return nil, err
	}
	scenario, actorPermissions, scenarioPermissions, enforceReadOnly := startPolicy(request)
	definition, selection, err := service.resolveDefinitionFor(
		request.Workflow,
		request.Actor,
		scenario,
	)
	if err != nil {
		return nil, err
	}
	if enforceReadOnly && !request.Admin && !definitionReadOnly(definition) {
		return nil, ErrForbidden
	}
	if enforceReadOnly {
		actorPermissions = clonePermissionPolicy(definition.Permissions)
		scenarioPermissions = clonePermissionPolicy(definition.Permissions)
	}
	prepared, err := prepareStart(orchestrator, definition, selection, ExecuteRequest{
		RunID:               request.RunID,
		ParentRunID:         request.ParentRunID,
		Round:               request.Round,
		BaseDepth:           request.BaseDepth,
		Workflow:            request.Workflow,
		Input:               request.Input,
		SeedEvidence:        request.SeedEvidence,
		Actor:               request.Actor,
		ActorPermissions:    actorPermissions,
		Scenario:            scenario,
		ScenarioPermissions: scenarioPermissions,
	})
	if err != nil {
		return nil, err
	}
	runCtx, release, err := service.registerActive(ctx, prepared.record.ID, true)
	if err != nil {
		return nil, err
	}
	if err := service.store.StartRun(runCtx, prepared.record, prepared.input); err != nil {
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
	run := detachedRunRecord(prepared.record)
	return &run, nil
}

func startPolicy(
	request StartRequest,
) (string, agentapi.PermissionPolicy, agentapi.PermissionPolicy, bool) {
	if request.Scenario == "" &&
		len(request.ActorPermissions.Scopes) == 0 &&
		len(request.ScenarioPermissions.Scopes) == 0 {
		return "workflow.api", agentapi.PermissionPolicy{}, agentapi.PermissionPolicy{}, true
	}
	return request.Scenario, request.ActorPermissions, request.ScenarioPermissions, false
}

func (service *Service) executePrepared(
	ctx context.Context,
	orchestrator *Orchestrator,
	prepared preparedRun,
) (Result, error) {
	observer := &storeRunObserver{store: service.store}
	result, runErr := orchestrator.RunObserved(ctx, prepared.definition, RunRequest{
		RunID: prepared.record.ID, ParentRunID: prepared.record.ParentRunID,
		Round: prepared.record.Round, BaseDepth: prepared.record.BaseDepth,
		InputHandoff: &prepared.input,
		Actor: agentapi.Actor{
			UserID:   prepared.record.ActorUserID,
			TenantID: prepared.record.ActorTenantID,
		},
		ActorPermissions:    prepared.record.ActorPermissions,
		ScenarioPermissions: prepared.record.ScenarioPermissions,
		StartedAt:           prepared.record.StartedAt,
	}, observer)
	status, errorCode := resultStatus(runErr)
	stopReason := result.StopReason
	if runErr != nil {
		stopReason = errorStopReason(runErr)
	}
	var output *Handoff
	if runErr == nil {
		output = &result.Output
	}
	persistCtx, cancel := persistenceContext(ctx)
	finishErr := service.store.FinishRun(
		persistCtx,
		prepared.record.ID,
		status,
		errorCode,
		stopReason,
		output,
		time.Now().UTC(),
	)
	cancel()
	if finishErr != nil {
		finishErr = fmt.Errorf(
			"%w: finish workflow run %q: %w",
			ErrRunPersistence,
			prepared.record.ID,
			finishErr,
		)
		if runErr != nil {
			return result, errors.Join(runErr, finishErr)
		}
		return Result{}, finishErr
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func newRunID() (string, error) {
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
) (Definition, DefinitionSelection, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if !canonicalID.MatchString(ref.ID) || ref.Version < 0 {
		return Definition{}, DefinitionSelection{}, fmt.Errorf(
			"workflow reference is invalid: %w",
			ErrInvalid,
		)
	}
	definition, selection, err := service.catalog.ResolveFor(
		ref,
		StableSelectionKey(actor, scenario),
	)
	if err != nil {
		return Definition{}, DefinitionSelection{}, err
	}
	return definition, selection, nil
}

func prepareStart(
	orchestrator *Orchestrator,
	definition Definition,
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
		runID, err = newRunID()
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
	round, baseDepth := normalizePosition(request.Round, request.BaseDepth)
	if round <= 0 || baseDepth < 0 {
		return preparedRun{}, fmt.Errorf(
			"workflow execution position round=%d base_depth=%d is invalid: %w",
			round,
			baseDepth,
			ErrInvalid,
		)
	}
	startedAt := time.Now().UTC()
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID:  runID,
		ProducerNodeID: "workflow.input",
		Schema:         definition.InputSchema,
		Payload:        request.Input,
		EvidenceUnits:  evidence.CloneUnits(request.SeedEvidence),
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
	record := RunRecord{
		ID:                  runID,
		ParentRunID:         parentRunID,
		Round:               round,
		BaseDepth:           baseDepth,
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

func definitionReadOnly(definition Definition) bool {
	if !permissionReadOnly(definition.Permissions) {
		return false
	}
	for _, node := range definition.Nodes {
		if !permissionReadOnly(node.Permissions) {
			return false
		}
	}
	return true
}

func permissionReadOnly(policy agentapi.PermissionPolicy) bool {
	return len(policy.Scopes) > 0 &&
		!scope.HasSideEffect(policy.Scopes) &&
		scope.Has(policy.Scopes, scope.KnowledgeRead)
}

func clonePermissionPolicy(policy agentapi.PermissionPolicy) agentapi.PermissionPolicy {
	policy.Scopes = append([]string(nil), policy.Scopes...)
	return policy
}

func detachedRunRecord(run RunRecord) RunRecord {
	run.ActorPermissions = clonePermissionPolicy(run.ActorPermissions)
	run.ScenarioPermissions = clonePermissionPolicy(run.ScenarioPermissions)
	if run.EndedAt != nil {
		endedAt := *run.EndedAt
		run.EndedAt = &endedAt
	}
	return run
}
