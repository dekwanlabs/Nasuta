package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

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
	return service.RecoverWithObserver(ctx, startedBefore, pageSize, nil)
}

// RecoverWithObserver streams each recovery result to its owning domain.
func (service *Service) RecoverWithObserver(
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
			recoveryErr := service.recoverOne(ctx, run.ID, observer, &report)
			if recoveryErr != nil {
				if firstErr == nil {
					firstErr = recoveryErr
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

func (service *Service) recoverOne(
	ctx context.Context,
	runID string,
	observer RecoveryObserver,
	report *RecoveryReport,
) error {
	report.Scanned++
	result, resumeErr := service.Resume(ctx, runID)
	if result.Applied {
		report.Resumed++
	}
	recordRecoveryStatus(report, result.Status)
	var recoveryErr error
	if resumeErr != nil && !errors.Is(resumeErr, ErrHumanApprovalRequired) {
		recoveryErr = resumeErr
	}
	if observer != nil {
		recoveryErr = errors.Join(
			recoveryErr,
			observer(ctx, runID, result, resumeErr),
		)
	}
	if recoveryErr != nil {
		report.Errors++
		return fmt.Errorf("recover workflow run %q: %w", runID, recoveryErr)
	}
	return nil
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
	state, err = service.takeOverAttempts(runCtx, definition, state)
	if err != nil {
		return result, err
	}
	progress, err := progressFromState(definition, state)
	if err != nil {
		var terminal *checkpointTerminalError
		if !errors.As(err, &terminal) {
			return result, err
		}
		return service.finishResumedRun(runCtx, workflowRunID, Result{}, err)
	}
	observed, runErr := orchestrator.ResumeObserved(runCtx, definition, RunRequest{
		RunID: workflowRunID, ParentRunID: state.Run.ParentRunID,
		Round: state.Run.Round, BaseDepth: state.Run.BaseDepth,
		Actor: agentapi.Actor{
			UserID: state.Run.ActorUserID, TenantID: state.Run.ActorTenantID,
		},
		ActorPermissions:    state.Run.ActorPermissions,
		ScenarioPermissions: state.Run.ScenarioPermissions,
		StartedAt:           state.Run.StartedAt,
	}, progress, &storeRunObserver{store: service.store})
	return service.finishResumedRun(runCtx, workflowRunID, observed, runErr)
}

func (service *Service) takeOverAttempts(
	ctx context.Context,
	definition Definition,
	state *RunState,
) (*RunState, error) {
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
		persistCtx, cancel := persistenceContext(ctx)
		err := service.store.FailNode(
			persistCtx,
			state.Run.ID,
			nodeID,
			run.Attempt,
			run.AgentRunID,
			status,
			errorCode,
			interruptedUsage(node, run.Attempt),
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
	status, errorCode := resultStatus(runErr)
	stopReason := result.StopReason
	if runErr != nil {
		stopReason = errorStopReason(runErr)
	}
	resumed := ResumeResult{
		RunID:   workflowRunID,
		Applied: true,
		Status:  status,
	}
	var output *Handoff
	if runErr == nil {
		output = &result.Output
	}
	if runErr == nil || result.StopReason != "" {
		resumed.Result = &result
	}
	persistCtx, cancel := persistenceContext(ctx)
	finishErr := service.store.FinishRun(
		persistCtx,
		workflowRunID,
		status,
		errorCode,
		stopReason,
		output,
		time.Now().UTC(),
	)
	cancel()
	if finishErr != nil {
		finishErr = fmt.Errorf(
			"%w: finish resumed workflow run %q: %w",
			ErrRunPersistence,
			workflowRunID,
			finishErr,
		)
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
