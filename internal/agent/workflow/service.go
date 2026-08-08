package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/log"
)

const (
	workflowPersistenceTimeout      = 5 * time.Second
	nodeRetryableErrorCode          = "node_retryable"
	nodeRestartedErrorCode          = "workflow_restarted"
	nodeRestartedRetryableErrorCode = "workflow_restarted_retryable"
)

type ExecuteRequest struct {
	RunID               string
	ParentRunID         string
	Workflow            DefinitionRef
	Input               json.RawMessage
	Actor               agentapi.Actor
	ActorPermissions    agentapi.PermissionPolicy
	Scenario            string
	ScenarioPermissions agentapi.PermissionPolicy
}

type StartRequest struct {
	RunID    string
	Workflow DefinitionRef
	Input    json.RawMessage
	Actor    agentapi.Actor
	Admin    bool
}

type ApprovalRequest struct {
	WorkflowRunID string
	NodeID        string
	Decision      ApprovalDecision
	Approver      agentapi.Actor
	Admin         bool
	Comment       string
}

type ApprovalResult struct {
	Approval WorkflowApproval
	Applied  bool
	Status   RunStatus
	Result   *Result
}

type ResumeResult struct {
	RunID   string
	Applied bool
	Status  RunStatus
	Result  *Result
}

type RecoveryReport struct {
	Scanned      int
	Resumed      int
	Succeeded    int
	WaitingHuman int
	Failed       int
	Cancelled    int
	TimedOut     int
	Errors       int
}

// RecoveryObserver reconciles one resumed Run with its owning domain.
type RecoveryObserver func(context.Context, string, ResumeResult, error) error

type workflowPersistence interface {
	StartWorkflow(context.Context, WorkflowRunRecord, Handoff) error
	StartNode(context.Context, NodeRunRecord) error
	SucceedNode(context.Context, string, string, int, string, Handoff, *GateDecision, WorkflowUsage, time.Time) error
	FailNode(context.Context, string, string, int, string, RunStatus, string, WorkflowUsage, time.Time) error
	FinishWorkflow(context.Context, string, RunStatus, string, *Handoff, time.Time) error
	LoadFullRunState(context.Context, string) (*WorkflowRunState, error)
	GetRun(context.Context, string) (*WorkflowRunRecord, error)
	ListNodeRuns(context.Context, string, NodeRunCursor, int) ([]NodeRunRecord, error)
	ListEvents(context.Context, string, int64, int) ([]Event, error)
	ListHandoffs(context.Context, string, HandoffCursor, int) ([]Handoff, error)
	ListActiveRuns(context.Context, time.Time, ActiveRunCursor, int) ([]ActiveRunRef, error)
	DecideHumanApproval(context.Context, WorkflowApproval, *Handoff) (ApprovalTransition, error)
	CancelWorkflow(context.Context, string, time.Time) (CancelTransition, error)
	SubscribeEvents(string) (<-chan Event, func())
}

type resumeCall struct {
	done   chan struct{}
	result ResumeResult
	err    error
}

type preparedRun struct {
	definition WorkflowDefinition
	record     WorkflowRunRecord
	input      Handoff
}

// RunEventReader scopes repeated event reads to one authorized Run.
type RunEventReader struct {
	store workflowPersistence
	runID string
}

func (reader *RunEventReader) List(
	ctx context.Context,
	afterSeq int64,
	limit int,
) ([]Event, error) {
	if reader == nil || reader.store == nil {
		return nil, ErrUnavailable
	}
	return reader.store.ListEvents(ctx, reader.runID, afterSeq, limit)
}

// Service fixes a Catalog snapshot and closes each execution transition in Store.
type Service struct {
	catalog *Catalog
	store   workflowPersistence

	mu           sync.RWMutex
	orchestrator *Orchestrator

	resumeMu sync.Mutex
	resumes  map[string]*resumeCall

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
	activeWG sync.WaitGroup
	closed   bool
}

func NewService(
	catalog *Catalog,
	store workflowPersistence,
	orchestrator *Orchestrator,
) (*Service, error) {
	if catalog == nil {
		return nil, fmt.Errorf("workflow service: catalog is required")
	}
	if store == nil {
		return nil, fmt.Errorf("workflow service: store is required")
	}
	return &Service{
		catalog: catalog, store: store, orchestrator: orchestrator,
		resumes: make(map[string]*resumeCall),
		active:  make(map[string]context.CancelFunc),
	}, nil
}

// SetOrchestrator atomically replaces the execution capability for future Runs.
func (service *Service) SetOrchestrator(orchestrator *Orchestrator) {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.orchestrator = orchestrator
	service.mu.Unlock()
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

func (service *Service) PublishDefinitions(
	definitions []WorkflowDefinition,
	admin bool,
) error {
	return service.PublishDefinitionsAs(
		context.Background(), definitions, 0, admin,
	)
}

func (service *Service) PublishDefinitionsAs(
	ctx context.Context,
	definitions []WorkflowDefinition,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	if len(definitions) == 0 {
		return fmt.Errorf("workflow definitions are required: %w", ErrInvalid)
	}
	if err := service.catalog.PublishAs(ctx, definitions, actorUserID); err != nil {
		return err
	}
	return nil
}

func (service *Service) ListDefinitions() ([]WorkflowDefinition, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	return service.catalog.List(), nil
}

func (service *Service) ListDefinitionRecords(
	ctx context.Context,
	cursor DefinitionCursor,
	limit int,
) ([]DefinitionRecord, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	return service.catalog.ListRecords(ctx, cursor, limit)
}

func (service *Service) SetDefinitionDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	return service.catalog.SetDefault(ctx, id, version, actorUserID)
}

func (service *Service) SetDefinitionActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
	admin bool,
) error {
	if service == nil || service.catalog == nil {
		return ErrUnavailable
	}
	if !admin {
		return ErrForbidden
	}
	return service.catalog.SetActive(ctx, id, version, active, actorUserID)
}

func (service *Service) ListDefinitionAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]DefinitionAuditEvent, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	if !admin {
		return nil, ErrForbidden
	}
	return service.catalog.ListAudit(ctx, id, afterSeq, limit)
}

func (service *Service) GetDefinitionRollout(
	id string,
) (RolloutRule, bool, error) {
	if service == nil || service.catalog == nil {
		return RolloutRule{}, false, ErrUnavailable
	}
	rule, ok := service.catalog.GetRollout(strings.TrimSpace(id))
	return rule, ok, nil
}

func (service *Service) SetDefinitionRollout(
	ctx context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
	admin bool,
) (RolloutRule, error) {
	if service == nil || service.catalog == nil {
		return RolloutRule{}, ErrUnavailable
	}
	if !admin {
		return RolloutRule{}, ErrForbidden
	}
	return service.catalog.SetRollout(
		ctx, id, candidateVersion, percentageBPS, salt, active, actorUserID,
	)
}

func (service *Service) ListDefinitionRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]RolloutAuditEvent, error) {
	if service == nil || service.catalog == nil {
		return nil, ErrUnavailable
	}
	if !admin {
		return nil, ErrForbidden
	}
	return service.catalog.ListRolloutAudit(ctx, id, afterSeq, limit)
}

func (service *Service) GetRun(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*WorkflowRunRecord, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	run, err := service.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !admin && run.ActorUserID != userID {
		return nil, ErrForbidden
	}
	return run, nil
}

func (service *Service) ListNodeRuns(
	ctx context.Context,
	runID string,
	cursor NodeRunCursor,
	limit int,
	userID int64,
	admin bool,
) ([]NodeRunRecord, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListNodeRuns(ctx, runID, cursor, limit)
}

func (service *Service) ListEvents(
	ctx context.Context,
	runID string,
	afterSeq int64,
	limit int,
	userID int64,
	admin bool,
) ([]Event, error) {
	_, reader, err := service.OpenRunEvents(ctx, runID, userID, admin)
	if err != nil {
		return nil, err
	}
	return reader.List(ctx, afterSeq, limit)
}

func (service *Service) OpenRunEvents(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*WorkflowRunRecord, *RunEventReader, error) {
	run, err := service.GetRun(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	return run, &RunEventReader{store: service.store, runID: runID}, nil
}

func (service *Service) SubscribeEvents(
	runID string,
) (<-chan Event, func(), error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	if err := validateRunID(runID); err != nil {
		return nil, nil, err
	}
	events, unsubscribe := service.store.SubscribeEvents(runID)
	return events, unsubscribe, nil
}

func (service *Service) ListHandoffs(
	ctx context.Context,
	runID string,
	cursor HandoffCursor,
	limit int,
	userID int64,
	admin bool,
) ([]Handoff, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListHandoffs(ctx, runID, cursor, limit)
}

func (service *Service) Cancel(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (CancelTransition, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return CancelTransition{}, err
	}
	transition, err := service.store.CancelWorkflow(ctx, runID, time.Now().UTC())
	if err != nil {
		return CancelTransition{}, err
	}
	if transition.Applied {
		service.cancelActive(runID)
	}
	return transition, nil
}

// DecideHumanApproval resumes only from durable facts fixed by the original Run.
func (service *Service) DecideHumanApproval(
	ctx context.Context,
	request ApprovalRequest,
) (ApprovalResult, error) {
	if service == nil || service.catalog == nil || service.store == nil {
		return ApprovalResult{}, ErrUnavailable
	}
	service.mu.RLock()
	orchestrator := service.orchestrator
	service.mu.RUnlock()
	if orchestrator == nil {
		return ApprovalResult{}, ErrUnavailable
	}
	prepared, err := prepareApprovalRequest(request)
	if err != nil {
		return ApprovalResult{}, err
	}
	state, err := service.store.LoadFullRunState(ctx, prepared.WorkflowRunID)
	if err != nil {
		return ApprovalResult{}, err
	}
	if !prepared.Admin {
		if prepared.Approver.TenantID != state.Run.ActorTenantID {
			return ApprovalResult{}, fmt.Errorf(
				"workflow approval tenant %q does not match run tenant %q: %w",
				prepared.Approver.TenantID, state.Run.ActorTenantID, ErrForbidden,
			)
		}
		if prepared.Approver.UserID != state.Run.ActorUserID {
			return ApprovalResult{}, ErrForbidden
		}
	}
	definition, err := service.catalog.Resolve(DefinitionRef{
		ID: state.Run.WorkflowID, Version: state.Run.WorkflowVersion,
	})
	if err != nil {
		return ApprovalResult{}, err
	}
	if definition.ContentHash != state.Run.WorkflowHash {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q definition hash mismatch",
			state.Run.ID,
		)
	}
	metadata, err := graph(definition, orchestrator.schemas)
	if err != nil {
		return ApprovalResult{}, err
	}
	node, ok := metadata.nodes[prepared.NodeID]
	if !ok {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q node %q not found: %w",
			state.Run.ID, prepared.NodeID, ErrNotFound,
		)
	}
	if node.Kind != NodeHumanApproval {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q node %q does not require human approval: %w",
			state.Run.ID, node.ID, ErrConflict,
		)
	}
	if _, decided := state.Approvals[node.ID]; !decided {
		if state.Run.Status != RunWaitingHuman {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q is %q, expected %q: %w",
				state.Run.ID, state.Run.Status, RunWaitingHuman, ErrConflict,
			)
		}
		nodeRun, exists := state.Nodes[node.ID]
		if !exists {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q has not started: %w",
				state.Run.ID, node.ID, ErrNotFound,
			)
		}
		if nodeRun.Kind != NodeHumanApproval {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q persisted kind %q does not require human approval: %w",
				state.Run.ID, node.ID, nodeRun.Kind, ErrConflict,
			)
		}
		if nodeRun.Status != RunWaitingHuman {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q is %q, expected %q: %w",
				state.Run.ID, node.ID, nodeRun.Status, RunWaitingHuman, ErrConflict,
			)
		}
	}
	approval := WorkflowApproval{
		WorkflowRunID:    state.Run.ID,
		NodeID:           node.ID,
		Decision:         prepared.Decision,
		ApproverUserID:   prepared.Approver.UserID,
		ApproverTenantID: prepared.Approver.TenantID,
		Comment:          prepared.Comment,
		DecidedAt:        time.Now().UTC(),
	}
	var approvedHandoff *Handoff
	if approval.Decision == ApprovalApproved {
		handoff, err := orchestrator.approvalHandoff(
			definition,
			state.Run.ID,
			node,
			predecessorHandoffs(
				node.ID,
				metadata.predecessors,
				state.NodeOutputs,
				state.Input,
			),
			approval.DecidedAt,
		)
		if err != nil {
			return ApprovalResult{}, fmt.Errorf(
				"approve workflow node %q/%q: %w",
				state.Run.ID, node.ID, err,
			)
		}
		approvedHandoff = &handoff
	}
	transition, err := service.store.DecideHumanApproval(ctx, approval, approvedHandoff)
	if err != nil {
		return ApprovalResult{}, err
	}
	approvalResult := ApprovalResult{
		Approval: transition.Approval,
		Applied:  transition.Applied,
		Status:   transition.RunStatus,
	}
	if transition.RunStatus != RunRunning {
		return approvalResult, nil
	}
	resumed, resumeErr := service.Resume(ctx, state.Run.ID)
	if resumed.Status != "" {
		approvalResult.Status = resumed.Status
	}
	approvalResult.Result = resumed.Result
	if resumeErr != nil && !errors.Is(resumeErr, ErrHumanApprovalRequired) {
		return approvalResult, resumeErr
	}
	return approvalResult, nil
}

// Resume continues one Run from its latest durable checkpoint.
func (service *Service) Resume(ctx context.Context, workflowRunID string) (ResumeResult, error) {
	if service == nil || service.catalog == nil || service.store == nil {
		return ResumeResult{}, fmt.Errorf("workflow service is unavailable")
	}
	runID := strings.TrimSpace(workflowRunID)
	if !canonicalID.MatchString(runID) {
		return ResumeResult{}, fmt.Errorf("workflow run id %q is not canonical", workflowRunID)
	}

	service.resumeMu.Lock()
	if call := service.resumes[runID]; call != nil {
		service.resumeMu.Unlock()
		select {
		case <-ctx.Done():
			return ResumeResult{}, ctx.Err()
		case <-call.done:
			return call.result, call.err
		}
	}
	call := &resumeCall{done: make(chan struct{})}
	service.resumes[runID] = call
	service.resumeMu.Unlock()

	result, err := service.resumeRun(ctx, runID)
	service.resumeMu.Lock()
	call.result = result
	call.err = err
	delete(service.resumes, runID)
	close(call.done)
	service.resumeMu.Unlock()
	return result, err
}

// RecoverActive resumes startup-era Runs in bounded keyset pages.
func (service *Service) RecoverActive(
	ctx context.Context,
	startedBefore time.Time,
	pageSize int,
) (RecoveryReport, error) {
	return service.RecoverActiveWithObserver(ctx, startedBefore, pageSize, nil)
}

// RecoverActiveWithObserver streams each recovery result to its owning domain.
func (service *Service) RecoverActiveWithObserver(
	ctx context.Context,
	startedBefore time.Time,
	pageSize int,
	observer RecoveryObserver,
) (RecoveryReport, error) {
	if service == nil || service.store == nil {
		return RecoveryReport{}, fmt.Errorf("workflow service is unavailable")
	}
	if startedBefore.IsZero() {
		startedBefore = time.Now().UTC()
	}
	pageSize = boundedLimit(pageSize)
	var (
		report   RecoveryReport
		cursor   ActiveRunCursor
		firstErr error
	)
	for {
		runs, err := service.store.ListActiveRuns(ctx, startedBefore, cursor, pageSize)
		if err != nil {
			return report, err
		}
		for _, run := range runs {
			report.Scanned++
			result, resumeErr := service.Resume(ctx, run.ID)
			if result.Applied {
				report.Resumed++
			}
			recordRecoveryStatus(&report, result.Status)
			var recoveryErr error
			if resumeErr != nil && !errors.Is(resumeErr, ErrHumanApprovalRequired) {
				recoveryErr = resumeErr
			}
			if observer != nil {
				recoveryErr = errors.Join(
					recoveryErr,
					observer(ctx, run.ID, result, resumeErr),
				)
			}
			if recoveryErr != nil {
				report.Errors++
				if firstErr == nil {
					firstErr = fmt.Errorf("recover workflow run %q: %w", run.ID, recoveryErr)
				}
			}
			if err := ctx.Err(); err != nil {
				return report, err
			}
		}
		if len(runs) < pageSize {
			break
		}
		last := runs[len(runs)-1]
		cursor = ActiveRunCursor{StartedAt: last.StartedAt, ID: last.ID}
	}
	if report.Errors > 0 {
		return report, fmt.Errorf(
			"%d workflow recoveries failed; first failure: %w",
			report.Errors, firstErr,
		)
	}
	return report, nil
}

func (service *Service) resumeRun(
	ctx context.Context,
	workflowRunID string,
) (ResumeResult, error) {
	orchestrator, err := service.executionCapability()
	if err != nil {
		return ResumeResult{}, err
	}
	runCtx, release, err := service.registerActive(ctx, workflowRunID, false)
	if err != nil {
		return ResumeResult{}, err
	}
	defer release()
	state, err := service.store.LoadFullRunState(runCtx, workflowRunID)
	if err != nil {
		return ResumeResult{}, err
	}
	result := ResumeResult{
		RunID:  workflowRunID,
		Status: state.Run.Status,
	}
	if state.Run.Status != RunRunning {
		return result, nil
	}
	definition, err := service.catalog.Resolve(DefinitionRef{
		ID: state.Run.WorkflowID, Version: state.Run.WorkflowVersion,
	})
	if err != nil {
		return result, err
	}
	if definition.ContentHash != state.Run.WorkflowHash {
		return result, fmt.Errorf(
			"workflow run %q definition hash mismatch",
			state.Run.ID,
		)
	}
	state, err = service.takeOverRunningAttempts(runCtx, definition, state)
	if err != nil {
		return result, err
	}
	progress, err := workflowProgressFromState(definition, state)
	if err != nil {
		var terminal *checkpointTerminalError
		if !errors.As(err, &terminal) {
			return result, err
		}
		return service.finishResumedRun(runCtx, workflowRunID, Result{}, err)
	}
	observed, runErr := orchestrator.ResumeObserved(runCtx, definition, RunRequest{
		RunID: workflowRunID, ParentRunID: state.Run.ParentRunID,
		Actor: agentapi.Actor{
			UserID: state.Run.ActorUserID, TenantID: state.Run.ActorTenantID,
		},
		ActorPermissions:    state.Run.ActorPermissions,
		ScenarioPermissions: state.Run.ScenarioPermissions,
		StartedAt:           state.Run.StartedAt,
	}, progress, &storeRunObserver{store: service.store})
	return service.finishResumedRun(runCtx, workflowRunID, observed, runErr)
}

func (service *Service) takeOverRunningAttempts(
	ctx context.Context,
	definition WorkflowDefinition,
	state *WorkflowRunState,
) (*WorkflowRunState, error) {
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	running := make([]string, 0)
	for nodeID, run := range state.Nodes {
		if run.Status == RunRunning {
			running = append(running, nodeID)
		}
	}
	if len(running) == 0 {
		return state, nil
	}
	sort.Strings(running)
	for _, nodeID := range running {
		run := state.Nodes[nodeID]
		node, ok := nodes[nodeID]
		if !ok {
			return nil, fmt.Errorf(
				"workflow run %q checkpoint contains unknown node %q",
				state.Run.ID, nodeID,
			)
		}
		if run.Kind != node.Kind {
			return nil, fmt.Errorf(
				"workflow run %q node %q kind changed from %q to %q",
				state.Run.ID, nodeID, run.Kind, node.Kind,
			)
		}
	}
	for _, nodeID := range running {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run := state.Nodes[nodeID]
		node := nodes[nodeID]
		status := RunFailed
		errorCode := nodeRestartedErrorCode
		if node.Kind == NodeHumanApproval {
			status = RunWaitingHuman
			errorCode = "human_approval_required"
		} else if recoveryRetryAllowed(definition, state.Run, node, run.Attempt) {
			errorCode = nodeRestartedRetryableErrorCode
		}
		persistCtx, cancel := workflowPersistenceContext(ctx)
		err := service.store.FailNode(
			persistCtx,
			state.Run.ID,
			nodeID,
			run.Attempt,
			run.AgentRunID,
			status,
			errorCode,
			interruptedAttemptUsage(node, run.Attempt),
			time.Now().UTC(),
		)
		cancel()
		if err != nil {
			return nil, fmt.Errorf(
				"take over workflow node %q/%q attempt %d: %w",
				state.Run.ID, nodeID, run.Attempt, err,
			)
		}
	}
	return service.store.LoadFullRunState(ctx, state.Run.ID)
}

func (service *Service) finishResumedRun(
	ctx context.Context,
	workflowRunID string,
	result Result,
	runErr error,
) (ResumeResult, error) {
	status, errorCode := workflowResultStatus(runErr)
	resumed := ResumeResult{
		RunID:   workflowRunID,
		Applied: true,
		Status:  status,
	}
	var output *Handoff
	if runErr == nil {
		output = &result.Output
		resumed.Result = &result
	}
	persistCtx, cancel := workflowPersistenceContext(ctx)
	finishErr := service.store.FinishWorkflow(
		persistCtx,
		workflowRunID,
		status,
		errorCode,
		output,
		time.Now().UTC(),
	)
	cancel()
	if finishErr != nil {
		if runErr != nil {
			return resumed, errors.Join(runErr, finishErr)
		}
		return resumed, finishErr
	}
	return resumed, runErr
}

func recordRecoveryStatus(report *RecoveryReport, status RunStatus) {
	switch status {
	case RunSucceeded:
		report.Succeeded++
	case RunWaitingHuman:
		report.WaitingHuman++
	case RunFailed:
		report.Failed++
	case RunCancelled:
		report.Cancelled++
	case RunTimedOut:
		report.TimedOut++
	}
}

type storeRunObserver struct {
	store workflowPersistence
}

func (observer *storeRunObserver) NodeStarted(ctx context.Context, request NodeRequest) error {
	inputIDs := make([]string, 0, len(request.Inputs))
	for _, input := range request.Inputs {
		inputIDs = append(inputIDs, input.ID)
	}
	persistCtx, cancel := workflowPersistenceContext(ctx)
	defer cancel()
	return observer.store.StartNode(persistCtx, NodeRunRecord{
		WorkflowRunID: request.WorkflowRunID, NodeID: request.Node.ID, Attempt: request.Attempt,
		Kind: request.Node.Kind, InputHandoffIDs: inputIDs, Status: RunRunning,
		StartedAt: time.Now().UTC(),
	})
}

func (observer *storeRunObserver) NodeSucceeded(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	decision *GateDecision,
) error {
	persistCtx, cancel := workflowPersistenceContext(ctx)
	err := observer.store.SucceedNode(
		persistCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		result.Handoff,
		decision,
		result.Usage,
		time.Now().UTC(),
	)
	cancel()
	if err == nil {
		return nil
	}
	failCtx, failCancel := workflowPersistenceContext(ctx)
	failErr := observer.store.FailNode(
		failCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		RunFailed,
		"node_persistence_failed",
		result.Usage,
		time.Now().UTC(),
	)
	failCancel()
	if failErr != nil {
		return errors.Join(err, fmt.Errorf("close node after success persistence failure: %w", failErr))
	}
	return err
}

func (observer *storeRunObserver) NodeFailed(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	runErr error,
) error {
	status, errorCode := nodeResultStatus(request, runErr)
	persistCtx, cancel := workflowPersistenceContext(ctx)
	defer cancel()
	return observer.store.FailNode(
		persistCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		status,
		errorCode,
		result.Usage,
		time.Now().UTC(),
	)
}

func workflowPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), workflowPersistenceTimeout)
}

func workflowResultStatus(runErr error) (RunStatus, string) {
	if runErr == nil {
		return RunSucceeded, ""
	}
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrWorkflowBudgetExhausted) {
		return RunFailed, "workflow_budget_exhausted"
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return RunTimedOut, "workflow_timeout"
	}
	if errors.Is(runErr, context.Canceled) {
		return RunCancelled, "workflow_cancelled"
	}
	return RunFailed, "workflow_failed"
}

func nodeResultStatus(request NodeRequest, runErr error) (RunStatus, string) {
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrWorkflowBudgetExhausted) {
		return RunFailed, "workflow_budget_exhausted"
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return RunTimedOut, "node_timeout"
	}
	if errors.Is(runErr, context.Canceled) {
		return RunCancelled, "workflow_cancelled"
	}
	if retryableNodeFailure(request, runErr) {
		return RunFailed, nodeRetryableErrorCode
	}
	return RunFailed, "node_failed"
}

func randomWorkflowRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate workflow run id: %w", err)
	}
	return "workflow_" + hex.EncodeToString(id[:]), nil
}

func prepareApprovalRequest(request ApprovalRequest) (ApprovalRequest, error) {
	prepared := request
	prepared.WorkflowRunID = strings.TrimSpace(prepared.WorkflowRunID)
	prepared.NodeID = strings.TrimSpace(prepared.NodeID)
	prepared.Comment = strings.TrimSpace(prepared.Comment)
	prepared.Approver.TenantID = strings.TrimSpace(prepared.Approver.TenantID)
	prepared.Decision = ApprovalDecision(strings.TrimSpace(string(prepared.Decision)))
	if !canonicalID.MatchString(prepared.WorkflowRunID) {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow run id %q is not canonical: %w",
			request.WorkflowRunID, ErrInvalid,
		)
	}
	if !canonicalID.MatchString(prepared.NodeID) {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow node id %q is not canonical: %w",
			request.NodeID, ErrInvalid,
		)
	}
	if prepared.Approver.UserID <= 0 {
		return ApprovalRequest{}, fmt.Errorf("workflow approver identity is required: %w", ErrInvalid)
	}
	if prepared.Decision != ApprovalApproved && prepared.Decision != ApprovalRejected {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow approval decision %q is invalid: %w",
			prepared.Decision, ErrInvalid,
		)
	}
	return prepared, nil
}

func (service *Service) executionCapability() (*Orchestrator, error) {
	if service == nil || service.catalog == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	service.activeMu.Lock()
	closed := service.closed
	service.activeMu.Unlock()
	if closed {
		return nil, ErrUnavailable
	}
	service.mu.RLock()
	orchestrator := service.orchestrator
	service.mu.RUnlock()
	if orchestrator == nil {
		return nil, ErrUnavailable
	}
	return orchestrator, nil
}

func (service *Service) resolveDefinition(ref DefinitionRef) (WorkflowDefinition, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if !canonicalID.MatchString(ref.ID) || ref.Version < 0 {
		return WorkflowDefinition{}, fmt.Errorf("workflow reference is invalid: %w", ErrInvalid)
	}
	definition, err := service.catalog.Resolve(ref)
	if err != nil {
		return WorkflowDefinition{}, fmt.Errorf("%v: %w", err, ErrNotFound)
	}
	return definition, nil
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
	if err := platformscope.Validate(request.ActorPermissions.Scopes); err != nil {
		return preparedRun{}, fmt.Errorf("workflow actor permissions: %v: %w", err, ErrInvalid)
	}
	if err := platformscope.Validate(request.ScenarioPermissions.Scopes); err != nil {
		return preparedRun{}, fmt.Errorf("workflow scenario permissions: %v: %w", err, ErrInvalid)
	}
	if err := platformscope.EnsureSubset(
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
	if err := platformscope.EnsureSubset(
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
		!platformscope.HasSideEffect(policy.Scopes) &&
		platformscope.Has(policy.Scopes, platformscope.KnowledgeRead)
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

func validateRunID(runID string) error {
	if !canonicalID.MatchString(runID) {
		return fmt.Errorf("workflow run id %q is not canonical: %w", runID, ErrInvalid)
	}
	return nil
}

func (service *Service) registerActive(
	ctx context.Context,
	runID string,
	detached bool,
) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if detached {
		ctx = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithCancel(ctx)
	service.activeMu.Lock()
	if service.closed {
		service.activeMu.Unlock()
		cancel()
		return nil, nil, ErrUnavailable
	}
	if _, exists := service.active[runID]; exists {
		service.activeMu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf(
			"workflow run %q is already active: %w",
			runID,
			ErrConflict,
		)
	}
	service.active[runID] = cancel
	service.activeWG.Add(1)
	service.activeMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			service.activeMu.Lock()
			delete(service.active, runID)
			service.activeMu.Unlock()
			service.activeWG.Done()
		})
	}
	return runCtx, release, nil
}

func (service *Service) cancelActive(runID string) {
	service.activeMu.Lock()
	cancel := service.active[runID]
	service.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close prevents new Runs, cancels active execution, and waits for persistence cleanup.
func (service *Service) Close() {
	if service == nil {
		return
	}
	service.activeMu.Lock()
	if service.closed {
		service.activeMu.Unlock()
		service.activeWG.Wait()
		return
	}
	service.closed = true
	cancels := make([]context.CancelFunc, 0, len(service.active))
	for _, cancel := range service.active {
		cancels = append(cancels, cancel)
	}
	service.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	service.activeWG.Wait()
}

func workflowProgressFromState(
	definition WorkflowDefinition,
	state *WorkflowRunState,
) (WorkflowProgress, error) {
	if state == nil {
		return WorkflowProgress{}, fmt.Errorf("workflow checkpoint is required")
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	progress := WorkflowProgress{
		StartedAt:      state.Run.StartedAt,
		Input:          state.Input,
		NodeOutputs:    state.NodeOutputs,
		Gates:          state.Gates,
		FailedOptional: make(map[string]struct{}),
		WaitingHuman:   make(map[string]struct{}),
		NodeAttempts:   make(map[string]NodeAttemptProgress),
		Usage:          state.Run.Usage,
	}
	nodeIDs := make([]string, 0, len(state.Nodes))
	for nodeID := range state.Nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		run := state.Nodes[nodeID]
		node, ok := nodes[nodeID]
		if !ok {
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q checkpoint contains unknown node %q",
				state.Run.ID, nodeID,
			)
		}
		if run.Kind != node.Kind {
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q node %q kind changed from %q to %q",
				state.Run.ID, nodeID, run.Kind, node.Kind,
			)
		}
		switch run.Status {
		case RunSucceeded:
		case RunWaitingHuman:
			if node.Kind != NodeHumanApproval {
				return WorkflowProgress{}, fmt.Errorf(
					"workflow run %q non-human node %q is waiting for approval",
					state.Run.ID, nodeID,
				)
			}
			progress.WaitingHuman[nodeID] = struct{}{}
		case RunFailed, RunCancelled, RunTimedOut:
			if run.Status == RunFailed &&
				retryableCheckpointFailure(run.ErrorCode) &&
				recoveryRetryAllowed(definition, state.Run, node, run.Attempt) {
				if run.EndedAt == nil || run.FirstStartedAt.IsZero() {
					return WorkflowProgress{}, fmt.Errorf(
						"workflow run %q node %q retry timing is incomplete",
						state.Run.ID, nodeID,
					)
				}
				progress.NodeAttempts[nodeID] = NodeAttemptProgress{
					NextAttempt:    run.Attempt + 1,
					FirstStartedAt: run.FirstStartedAt,
					NotBefore:      run.EndedAt.Add(node.Retry.Backoff),
				}
				continue
			}
			if node.Optional && definition.FailurePolicy.Mode == CollectAvailable {
				progress.FailedOptional[nodeID] = struct{}{}
				continue
			}
			return WorkflowProgress{}, &checkpointTerminalError{
				workflowRunID: state.Run.ID,
				nodeID:        nodeID,
				status:        run.Status,
			}
		default:
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q node %q checkpoint status %q cannot be resumed",
				state.Run.ID, nodeID, run.Status,
			)
		}
	}
	return progress, nil
}

func interruptedAttemptUsage(node NodeDefinition, attempt int) WorkflowUsage {
	totalTokens := node.Budget.MaxTotalTokens
	if totalTokens == 0 {
		totalTokens = node.Budget.MaxInputTokens + node.Budget.MaxOutputTokens
	}
	usage := WorkflowUsage{
		InputTokens:  node.Budget.MaxInputTokens,
		OutputTokens: node.Budget.MaxOutputTokens,
		TotalTokens:  totalTokens,
		ToolCalls:    node.Budget.MaxToolCalls,
		CostMicros:   node.Budget.MaxCostMicros,
	}
	if attempt > 1 {
		usage.Retries = 1
	}
	return usage
}

type checkpointTerminalError struct {
	workflowRunID string
	nodeID        string
	status        RunStatus
}

func (err *checkpointTerminalError) Error() string {
	return fmt.Sprintf(
		"workflow run %q required node %q is %q",
		err.workflowRunID, err.nodeID, err.status,
	)
}

func (err *checkpointTerminalError) Unwrap() error {
	switch err.status {
	case RunCancelled:
		return context.Canceled
	case RunTimedOut:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func retryableCheckpointFailure(errorCode string) bool {
	return errorCode == nodeRetryableErrorCode ||
		errorCode == nodeRestartedRetryableErrorCode
}

func recoveryRetryAllowed(
	definition WorkflowDefinition,
	run WorkflowRunRecord,
	node NodeDefinition,
	attempt int,
) bool {
	if attempt <= 0 || attempt >= node.Retry.MaxAttempts ||
		(node.Kind != NodeAgent &&
			node.Kind != NodeJoin &&
			!(node.Kind == NodeTransform && node.RetrySafe)) {
		return false
	}
	effective := IntersectPermissions(
		run.ActorPermissions,
		run.ScenarioPermissions,
		definition.Permissions,
		node.Permissions,
	)
	return !platformscope.HasSideEffect(effective.Scopes)
}
