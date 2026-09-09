package delegation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/budget"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

type executorDefinitionResolver map[agentapi.DefinitionRef]agentapi.Definition

func (resolver executorDefinitionResolver) Resolve(
	ref agentapi.DefinitionRef,
) (agentapi.Definition, error) {
	definition, ok := resolver[ref]
	if !ok {
		return agentapi.Definition{}, fmt.Errorf("definition not found")
	}
	return definition, nil
}

type executorRuntimeFunc func(
	context.Context,
	agentapi.RunRequest,
) (agentapi.RunResult, error)

type recordedExecutorEvent struct {
	eventType agentrun.EventType
	event     agentrun.ExecutionEvent
}

type executorEventRecorder struct {
	mu     sync.Mutex
	events []recordedExecutorEvent
}

func (recorder *executorEventRecorder) EmitEvent(
	eventType agentrun.EventType,
	event agentrun.ExecutionEvent,
) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, recordedExecutorEvent{
		eventType: eventType,
		event:     event,
	})
}

func (recorder *executorEventRecorder) snapshot() []recordedExecutorEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]recordedExecutorEvent(nil), recorder.events...)
}

func (run executorRuntimeFunc) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return run(ctx, request)
}

type executorPersistence struct {
	mu sync.Mutex

	records     map[string]agentrun.DelegationTaskRecord
	artifacts   map[string]agentrun.DelegationArtifact
	evidence    map[string][]tool.EvidenceUnit
	linked      []int
	settlements []agentrun.DelegationSettlement
	rejections  []agentrun.DelegationRejection
	attempts    map[string]agentrun.DelegationAttemptRecord
	checkpoints map[string]agentrun.DelegationCheckpoint

	settleStarted chan struct{}
	settleRelease chan struct{}
	settleOnce    sync.Once
}

type verifierBudgetPersistence struct {
	*executorPersistence
}

func (persistence *verifierBudgetPersistence) ReserveDelegationBatch(
	_ context.Context,
	_ agentrun.DelegationAdmission,
) ([]agentrun.DelegationTaskRecord, error) {
	return nil, agentrun.ErrDelegationBudgetInsufficient
}

func newExecutorPersistence() *executorPersistence {
	return &executorPersistence{
		records:     make(map[string]agentrun.DelegationTaskRecord),
		artifacts:   make(map[string]agentrun.DelegationArtifact),
		evidence:    make(map[string][]tool.EvidenceUnit),
		attempts:    make(map[string]agentrun.DelegationAttemptRecord),
		checkpoints: make(map[string]agentrun.DelegationCheckpoint),
	}
}

func (persistence *executorPersistence) ReserveDelegationBatch(
	_ context.Context,
	admission agentrun.DelegationAdmission,
) ([]agentrun.DelegationTaskRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	records := make([]agentrun.DelegationTaskRecord, len(admission.Reservations))
	for index, reservation := range admission.Reservations {
		key := executorTaskKey(
			reservation.ParentRunID,
			reservation.DelegationID,
			reservation.TaskIndex,
		)
		if existing, ok := persistence.records[key]; ok {
			existing.Existing = true
			records[index] = existing
			continue
		}
		record := agentrun.DelegationTaskRecord{
			ParentRunID:    reservation.ParentRunID,
			DelegationID:   reservation.DelegationID,
			TaskIndex:      reservation.TaskIndex,
			ChildRunID:     reservation.ChildRunID,
			Capability:     reservation.Capability,
			CapabilityHash: reservation.CapabilityHash,
			ObjectiveHash:  reservation.ObjectiveHash,
			Admitted:       true,
			Reservation:    reservation,
		}
		persistence.records[key] = record
		records[index] = record
	}
	return records, nil
}

func (persistence *executorPersistence) RejectDelegationTask(
	_ context.Context,
	rejection agentrun.DelegationRejection,
) (agentrun.DelegationTaskRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.rejections = append(persistence.rejections, rejection)
	record := agentrun.DelegationTaskRecord{
		ParentRunID:    rejection.ParentRunID,
		DelegationID:   rejection.DelegationID,
		TaskIndex:      rejection.TaskIndex,
		Capability:     rejection.Capability,
		CapabilityHash: rejection.CapabilityHash,
		ObjectiveHash:  rejection.ObjectiveHash,
		RejectionCode:  rejection.Code,
	}
	persistence.records[executorTaskKey(
		rejection.ParentRunID,
		rejection.DelegationID,
		rejection.TaskIndex,
	)] = record
	return record, nil
}

func (persistence *executorPersistence) LinkDelegationChild(
	_ context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
	childRunID string,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorTaskKey(parentRunID, delegationID, taskIndex)
	record := persistence.records[key]
	record.ChildRunID = childRunID
	persistence.records[key] = record
	persistence.linked = append(persistence.linked, taskIndex)
	return nil
}

func (persistence *executorPersistence) SettleDelegationTask(
	_ context.Context,
	settlement agentrun.DelegationSettlement,
) (agentrun.DelegationTaskRecord, error) {
	if settlement.Artifact != nil && persistence.settleStarted != nil {
		persistence.settleOnce.Do(func() {
			close(persistence.settleStarted)
			<-persistence.settleRelease
		})
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorTaskKey(
		settlement.ParentRunID,
		settlement.DelegationID,
		settlement.TaskIndex,
	)
	record := persistence.records[key]
	usage := settlement.Usage
	record.SettledUsage = &usage
	if settlement.Artifact != nil {
		artifact := *settlement.Artifact
		artifact.Content = append([]byte(nil), settlement.Artifact.Content...)
		persistence.artifacts[artifact.ID] = artifact
		record.ReportArtifactID = artifact.ID
	}
	if settlement.EvidenceArtifact != nil {
		var units []tool.EvidenceUnit
		_ = json.Unmarshal(settlement.EvidenceArtifact.Content, &units)
		persistence.evidence[key] = cloneEvidenceUnits(units)
	}
	persistence.records[key] = record
	persistence.settlements = append(persistence.settlements, settlement)
	return record, nil
}

func (persistence *executorPersistence) GetDelegationTask(
	_ context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
) (agentrun.DelegationTaskRecord, *agentrun.DelegationArtifact, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	record, ok := persistence.records[executorTaskKey(
		parentRunID,
		delegationID,
		taskIndex,
	)]
	if !ok {
		return agentrun.DelegationTaskRecord{}, nil, fmt.Errorf("task not found")
	}
	var artifact *agentrun.DelegationArtifact
	if record.ReportArtifactID != "" {
		value := persistence.artifacts[record.ReportArtifactID]
		value.Content = append([]byte(nil), value.Content...)
		artifact = &value
	}
	return record, artifact, nil
}

func (persistence *executorPersistence) GetDelegationEvidence(
	_ context.Context,
	parentRunID,
	delegationID string,
	taskIndex int,
) ([]tool.EvidenceUnit, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	return cloneEvidenceUnits(persistence.evidence[executorTaskKey(
		parentRunID,
		delegationID,
		taskIndex,
	)]), nil
}

func (persistence *executorPersistence) ListDelegationTasks(
	_ context.Context,
	parentRunID,
	delegationID string,
) ([]agentrun.DelegationTaskRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	var tasks []agentrun.DelegationTaskRecord
	for _, record := range persistence.records {
		if record.ParentRunID == parentRunID &&
			record.DelegationID == delegationID {
			tasks = append(tasks, record)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].TaskIndex < tasks[j].TaskIndex
	})
	return tasks, nil
}

func executorTaskKey(parentRunID, delegationID string, taskIndex int) string {
	return fmt.Sprintf("%s/%s/%d", parentRunID, delegationID, taskIndex)
}

func executorAttemptKey(parentRunID, delegationID string, taskIndex, attemptNo int) string {
	return fmt.Sprintf("%s/%s/%d/%d", parentRunID, delegationID, taskIndex, attemptNo)
}

func (persistence *executorPersistence) StartDelegationAttempt(
	_ context.Context,
	start agentrun.DelegationAttemptStart,
) (agentrun.DelegationAttemptRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorAttemptKey(start.ParentRunID, start.DelegationID, start.TaskIndex, start.AttemptNo)
	if existing, ok := persistence.attempts[key]; ok {
		if existing.AttemptID != start.AttemptID || existing.ChildRunID != start.ChildRunID {
			return agentrun.DelegationAttemptRecord{}, agentrun.ErrDelegationTaskConflict
		}
		existing.Existing = existing.Status != agentrun.DelegationAttemptRunning
		return existing, nil
	}
	record := agentrun.DelegationAttemptRecord{
		ParentRunID: start.ParentRunID, DelegationID: start.DelegationID,
		TaskIndex: start.TaskIndex, AttemptNo: start.AttemptNo, AttemptID: start.AttemptID,
		ChildRunID: start.ChildRunID, Status: agentrun.DelegationAttemptRunning,
		StartedAt: start.StartedAt,
	}
	persistence.attempts[key] = record
	return record, nil
}

func (persistence *executorPersistence) FinishDelegationAttempt(
	_ context.Context,
	finish agentrun.DelegationAttemptFinish,
) (agentrun.DelegationAttemptRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorAttemptKey(finish.ParentRunID, finish.DelegationID, finish.TaskIndex, finish.AttemptNo)
	record, ok := persistence.attempts[key]
	if !ok {
		return agentrun.DelegationAttemptRecord{}, sql.ErrNoRows
	}
	if record.AttemptID != finish.AttemptID {
		return agentrun.DelegationAttemptRecord{}, agentrun.ErrDelegationTaskConflict
	}
	if record.Status != agentrun.DelegationAttemptRunning {
		if record.Status != finish.Status || record.Retryable != finish.Retryable ||
			record.ErrorCode != finish.ErrorCode || record.ErrorMessage != finish.ErrorMessage ||
			record.ReportArtifactID != finish.ReportArtifactID || !sameExecutorUsage(record.Usage, finish.Usage) {
			return agentrun.DelegationAttemptRecord{}, agentrun.ErrDelegationTaskConflict
		}
		return record, nil
	}
	record.Status = finish.Status
	record.Retryable = finish.Retryable
	record.ErrorCode = finish.ErrorCode
	record.ErrorMessage = finish.ErrorMessage
	record.EndedAt = finish.EndedAt
	record.NextAttemptAt = finish.NextAttemptAt
	record.ReportArtifactID = finish.ReportArtifactID
	if finish.Usage != nil {
		usage := *finish.Usage
		record.Usage = &usage
	} else {
		record.Usage = nil
	}
	persistence.attempts[key] = record
	return record, nil
}

func (persistence *executorPersistence) GetLatestDelegationAttempt(
	_ context.Context, parentRunID, delegationID string, taskIndex int,
) (agentrun.DelegationAttemptRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	var latest agentrun.DelegationAttemptRecord
	found := false
	for _, record := range persistence.attempts {
		if record.ParentRunID != parentRunID || record.DelegationID != delegationID || record.TaskIndex != taskIndex {
			continue
		}
		if !found || record.AttemptNo > latest.AttemptNo {
			latest, found = record, true
		}
	}
	if !found {
		return agentrun.DelegationAttemptRecord{}, sql.ErrNoRows
	}
	return latest, nil
}

func (persistence *executorPersistence) LinkDelegationAttemptChild(
	_ context.Context, parentRunID, delegationID string, taskIndex, attemptNo int, childRunID string,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorAttemptKey(parentRunID, delegationID, taskIndex, attemptNo)
	record, ok := persistence.attempts[key]
	if !ok {
		return sql.ErrNoRows
	}
	record.ChildRunID = childRunID
	persistence.attempts[key] = record
	taskKey := executorTaskKey(parentRunID, delegationID, taskIndex)
	task := persistence.records[taskKey]
	task.ChildRunID = childRunID
	persistence.records[taskKey] = task
	persistence.linked = append(persistence.linked, taskIndex)
	return nil
}

func (persistence *executorPersistence) UpsertDelegationCheckpoint(
	_ context.Context, checkpoint agentrun.DelegationCheckpoint,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	key := executorTaskKey(checkpoint.ParentRunID, checkpoint.DelegationID, checkpoint.TaskIndex)
	if existing, ok := persistence.checkpoints[key]; ok && existing.Status == agentrun.DelegationCheckpointCompleted && checkpoint.Status == agentrun.DelegationCheckpointPending {
		return nil
	}
	persistence.checkpoints[key] = checkpoint
	return nil
}

func (persistence *executorPersistence) GetDelegationCheckpoint(
	_ context.Context, parentRunID, delegationID string, taskIndex int,
) (agentrun.DelegationCheckpoint, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	checkpoint, ok := persistence.checkpoints[executorTaskKey(parentRunID, delegationID, taskIndex)]
	if !ok {
		return agentrun.DelegationCheckpoint{}, sql.ErrNoRows
	}
	return checkpoint, nil
}

func sameExecutorUsage(left *agentapi.Usage, right *agentapi.Usage) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	return left == nil || *left == *right
}

func TestExecutorRetriesTransientChildFailureAndSettlesOnlyFinalAttempt(t *testing.T) {
	persistence := newExecutorPersistence()
	var (
		mu       sync.Mutex
		calls    int
		childIDs []string
	)
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			mu.Lock()
			calls++
			childIDs = append(childIDs, request.RunID)
			call := calls
			mu.Unlock()
			if call == 1 {
				return agentapi.RunResult{
					RunID:  request.RunID,
					Status: agentapi.RunFailed,
					Error: &agentapi.RunError{
						Code:      "provider_unavailable",
						Message:   "temporary provider outage",
						Retryable: true,
					},
				}, nil
			}
			return successfulExecutorResult(request.RunID, "retry succeeded"), nil
		}),
		persistence,
		nil,
	)

	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "retry transient provider failure",
		}},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", result.Results)
	}

	mu.Lock()
	gotCalls := calls
	gotChildIDs := append([]string(nil), childIDs...)
	mu.Unlock()
	if gotCalls != 2 || len(gotChildIDs) != 2 || gotChildIDs[0] == gotChildIDs[1] {
		t.Fatalf("runtime calls=%d child ids=%v, want two distinct attempts", gotCalls, gotChildIDs)
	}

	delegationID := result.DelegationID
	key := executorTaskKey("parent-1", delegationID, 0)
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	record := persistence.records[key]
	if record.Reservation.ChildRunID != gotChildIDs[0] || record.ChildRunID != gotChildIDs[1] {
		t.Fatalf("record = %+v, want immutable reservation=%q and current child=%q", record, gotChildIDs[0], gotChildIDs[1])
	}
	if len(persistence.settlements) != 1 || persistence.settlements[0].ChildRunID != gotChildIDs[1] {
		t.Fatalf("settlements = %+v, want only final attempt", persistence.settlements)
	}
	first, ok := persistence.attempts[executorAttemptKey("parent-1", delegationID, 0, 1)]
	if !ok || first.Status != agentrun.DelegationAttemptFailed || !first.Retryable {
		t.Fatalf("first attempt = %+v", first)
	}
	second, ok := persistence.attempts[executorAttemptKey("parent-1", delegationID, 0, 2)]
	if !ok || second.Status != agentrun.DelegationAttemptSucceeded || second.Retryable {
		t.Fatalf("second attempt = %+v", second)
	}
	checkpoint := persistence.checkpoints[key]
	if checkpoint.Status != agentrun.DelegationCheckpointCompleted || checkpoint.ChildRunID != gotChildIDs[1] || checkpoint.ReportArtifactID == "" {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
}

func TestExecutorDoesNotRetryPermanentChildFailure(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			return agentapi.RunResult{
				RunID:  request.RunID,
				Status: agentapi.RunFailed,
				Error: &agentapi.RunError{
					Code:    "invalid_query",
					Message: "permanent child failure",
				},
			}, nil
		}),
		persistence,
		nil,
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "permanent failure"}},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runtime calls = %d, want 1", calls)
	}
	if len(result.Results) != 1 || result.Results[0].Error == nil || result.Results[0].Error.Code != "invalid_query" {
		t.Fatalf("result = %+v", result.Results)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", persistence.attempts)
	}
	for _, attempt := range persistence.attempts {
		if attempt.Status != agentrun.DelegationAttemptFailed || attempt.Retryable {
			t.Fatalf("attempt = %+v", attempt)
		}
	}
}

func TestExecutorParentCancellationDoesNotRetryTransientFailure(t *testing.T) {
	persistence := newExecutorPersistence()
	started := make(chan struct{})
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(ctx context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
			close(started)
			<-ctx.Done()
			return agentapi.RunResult{
				RunID:  request.RunID,
				Status: agentapi.RunFailed,
				Error:  &agentapi.RunError{Code: "provider_unavailable", Retryable: true},
			}, nil
		}),
		persistence,
		nil,
	)
	ctx, cancel := context.WithCancel(executorContext(t, context.Background(), 0))
	defer cancel()
	resultCh := make(chan struct {
		result agentapi.DelegationBatchResult
		err    error
	}, 1)
	go func() {
		result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect", Objective: "cancel retry",
		}})
		resultCh <- struct {
			result agentapi.DelegationBatchResult
			err    error
		}{result, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	execution := <-resultCh
	if execution.err != nil {
		t.Fatalf("Execute: %v", execution.err)
	}
	if len(execution.result.Results) != 1 || execution.result.Results[0].Error == nil || execution.result.Results[0].Error.Code != ErrorParentCancelled {
		t.Fatalf("result = %+v", execution.result.Results)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", persistence.attempts)
	}
	for _, attempt := range persistence.attempts {
		if attempt.Status != agentrun.DelegationAttemptCancelled || attempt.Retryable {
			t.Fatalf("attempt = %+v", attempt)
		}
	}
}

func TestExecutorChildDeadlinePreventsRetryBackoff(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			return agentapi.RunResult{
				RunID:  request.RunID,
				Status: agentapi.RunFailed,
				Error:  &agentapi.RunError{Code: "provider_unavailable", Retryable: true},
			}, nil
		}),
		persistence,
		nil,
	)
	ctx := executorContext(t, context.Background(), 0)
	parent, ok := ParentContextFrom(ctx)
	if !ok {
		t.Fatal("parent context unavailable")
	}
	parent.AnswerDeadline = time.Now().Add(childAnswerDeadlineSafety + 5*time.Millisecond)
	ctx = WithParentContext(ctx, parent)
	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect", Objective: "deadline prevents retry",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("runtime calls = %d, want 1", calls)
	}
	if len(result.Results) != 1 || result.Results[0].Error == nil || result.Results[0].Error.Code != "provider_unavailable" {
		t.Fatalf("result = %+v", result.Results)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", persistence.attempts)
	}
}

func TestExecutorRespectsInfrastructureRetryLimit(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			return agentapi.RunResult{
				RunID:  request.RunID,
				Status: agentapi.RunFailed,
				Error:  &agentapi.RunError{Code: "provider_unavailable", Retryable: true},
			}, nil
		}),
		persistence,
		nil,
	)
	definitions, ok := executor.definitions.(executorDefinitionResolver)
	if !ok {
		t.Fatalf("definitions = %T", executor.definitions)
	}
	for ref, definition := range definitions {
		definition.FailurePolicy.MaxInfrastructureRetries = 0
		definitions[ref] = definition
	}
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "retry limit"}},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 || len(result.Results) != 1 {
		t.Fatalf("calls=%d results=%+v", calls, result.Results)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", persistence.attempts)
	}
}

func TestExecutorReplayUsesFinalRetryAttemptWithoutRestartingChild(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			if calls == 1 {
				return agentapi.RunResult{
					RunID: request.RunID, Status: agentapi.RunFailed,
					Error: &agentapi.RunError{Code: "provider_unavailable", Retryable: true},
				}, nil
			}
			return successfulExecutorResult(request.RunID, "replay final attempt"), nil
		}),
		persistence,
		nil,
	)
	ctx := executorContext(t, context.Background(), 0)
	first, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect", Objective: "replay final attempt",
	}})
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("first runtime calls = %d, want 2", calls)
	}
	second, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect", Objective: "replay final attempt",
	}})
	if err != nil {
		t.Fatalf("replay Execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("replay restarted child: runtime calls = %d", calls)
	}
	if len(first.Results) != 1 || len(second.Results) != 1 ||
		first.Results[0].RunID != second.Results[0].RunID ||
		first.Results[0].Status != agentapi.DelegationCompleted ||
		second.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("first=%+v second=%+v", first.Results, second.Results)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.settlements) != 1 {
		t.Fatalf("settlements = %+v, want one", persistence.settlements)
	}
}

func TestExecutorCheckpointStatusesAreDurableAndBounded(t *testing.T) {
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context, request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return successfulExecutorResult(request.RunID, "checkpoint"), nil
	}), persistence, nil)
	parent, ok := ParentContextFrom(executorContext(t, context.Background(), 0))
	if !ok {
		t.Fatal("parent context unavailable")
	}
	task := preparedTask{index: 0, childRunID: "child-checkpoint", objectiveHash: "objective-hash"}
	delegationID := "delegation-checkpoint"
	for _, status := range []agentrun.DelegationCheckpointStatus{
		agentrun.DelegationCheckpointPending,
		agentrun.DelegationCheckpointCompleted,
		agentrun.DelegationCheckpointUnavailable,
		agentrun.DelegationCheckpointInterrupted,
	} {
		executor.markCheckpoint(context.Background(), parent, delegationID, task, string(status), "error", "artifact")
		checkpoint, err := persistence.GetDelegationCheckpoint(context.Background(), parent.RunID, delegationID, task.index)
		if err != nil {
			t.Fatalf("GetDelegationCheckpoint(%s): %v", status, err)
		}
		if checkpoint.Status != status || checkpoint.ChildRunID != task.childRunID || checkpoint.RequestHash != task.objectiveHash {
			t.Fatalf("checkpoint(%s) = %+v", status, checkpoint)
		}
	}
}

func TestExecutorChildUsesTaskBudgetAndSettlesIntoRoot(t *testing.T) {
	root := budget.NewRoot(agentapi.RunLimits{
		MaxTotalTokens:      1000,
		ParentAnswerReserve: 100,
	})
	var (
		mu                 sync.Mutex
		seenTaskGate       bool
		seenRootTaskGate   bool
		settledChildUsage  agentapi.Usage
		remainingTaskGrant agentapi.Usage
	)
	runtime := executorRuntimeFunc(func(ctx context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
		gate := agentapi.RunBudgetGateFromContext(ctx)
		usageGate := agentapi.RunBudgetUsageGateFromContext(ctx)
		if gate == nil || usageGate == nil {
			return agentapi.RunResult{}, fmt.Errorf("child budget gate was not attached")
		}
		mu.Lock()
		seenTaskGate = true
		seenRootTaskGate = agentapi.RunBudgetTaskGateFromContext(ctx) != nil
		mu.Unlock()

		actual := agentapi.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
		reservation, err := usageGate.ReserveCall(agentapi.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		})
		if err != nil {
			return agentapi.RunResult{}, fmt.Errorf("reserve child call: %w", err)
		}
		if err := reservation.Settle(actual); err != nil {
			return agentapi.RunResult{}, fmt.Errorf("settle child call: %w", err)
		}
		if availability, ok := usageGate.(agentapi.RunBudgetAvailability); ok {
			mu.Lock()
			settledChildUsage = actual
			remainingTaskGrant = availability.Available()
			mu.Unlock()
		}
		return successfulExecutorResult(request.RunID, executorObjective(t, request)), nil
	})
	executor := newExecutorFixture(t, runtime, newExecutorPersistence(), nil)
	ctx := agentapi.WithRunBudgetGate(
		executorContext(t, context.Background(), 0),
		root,
	)
	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "settle one child call",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seenTaskGate || seenRootTaskGate {
		t.Fatalf("child gate task=%v root_task_gate=%v", seenTaskGate, seenRootTaskGate)
	}
	if settledChildUsage.TotalTokens != 15 {
		t.Fatalf("settled child usage = %+v", settledChildUsage)
	}
	// The child grant is output-only (128 output, no cumulative input/total).
	// After settling 5 output tokens its output availability is 123; input and
	// total are unbounded per-child and charged to the shared batch ledger.
	if remainingTaskGrant.OutputTokens != 123 || remainingTaskGrant.TotalTokens != math.MaxInt64 {
		t.Fatalf("remaining child grant = %+v, want output 123 and unbounded total", remainingTaskGrant)
	}
	if got := root.Used().TotalTokens; got != 15 {
		t.Fatalf("root used total = %d, want 15", got)
	}
	if got := root.Available().TotalTokens; got != 885 {
		t.Fatalf("root default availability = %d, want 885", got)
	}
	if got := root.AvailableForPhase(agentapi.RunBudgetPhaseAnswer).TotalTokens; got != 985 {
		t.Fatalf("root answer availability = %d, want 985", got)
	}
}

func TestExecutorRejectsChildWhenParentRootHasNoAdmissionCapacity(t *testing.T) {
	root := budget.NewRoot(agentapi.RunLimits{
		MaxTotalTokens:      300,
		ParentAnswerReserve: 100,
	})
	parentCall, err := root.ReserveCall(agentapi.Usage{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := parentCall.Settle(agentapi.Usage{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
	}); err != nil {
		t.Fatal(err)
	}

	runtimeCalls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error) {
			runtimeCalls++
			return agentapi.RunResult{}, nil
		}),
		newExecutorPersistence(),
		nil,
	)
	ctx := agentapi.WithRunBudgetGate(
		executorContext(t, context.Background(), 0),
		root,
	)
	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "must not start after root admission is exhausted",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeCalls != 0 {
		t.Fatalf("runtime calls = %d, want 0", runtimeCalls)
	}
	if len(result.Results) != 1 ||
		result.Results[0].Status != agentapi.DelegationRejected ||
		result.Results[0].Error == nil ||
		result.Results[0].Error.Code != ErrorBudgetInsufficient {
		t.Fatalf("result = %+v", result.Results)
	}
	if got := root.Used().TotalTokens; got != 150 {
		t.Fatalf("root used total = %d, want 150", got)
	}
	// Default calls must still leave the 100-token parent answer reserve intact.
	if got := root.Available().TotalTokens; got != 50 {
		t.Fatalf("root default availability = %d, want 50", got)
	}
}

func TestExecutorReleasesInMemoryChildGrantWhenDurableAdmissionFails(t *testing.T) {
	root := budget.NewRoot(agentapi.RunLimits{MaxTotalTokens: 1000})
	persistence := &verifierBudgetPersistence{executorPersistence: newExecutorPersistence()}
	runtimeCalls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error) {
			runtimeCalls++
			return agentapi.RunResult{}, nil
		}),
		persistence,
		nil,
	)
	ctx := agentapi.WithRunBudgetGate(
		executorContext(t, context.Background(), 0),
		root,
	)
	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "durable admission failure must release grant",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeCalls != 0 {
		t.Fatalf("runtime calls = %d, want 0", runtimeCalls)
	}
	if len(result.Results) != 1 ||
		result.Results[0].Status != agentapi.DelegationRejected ||
		result.Results[0].Error == nil ||
		result.Results[0].Error.Code != ErrorBudgetInsufficient {
		t.Fatalf("result = %+v", result.Results)
	}
	if got := root.Used().TotalTokens; got != 0 {
		t.Fatalf("root used total = %d, want 0", got)
	}
	if got := root.Available().TotalTokens; got != 1000 {
		t.Fatalf("root availability = %d, want 1000 after release", got)
	}
}

func TestSemanticVerifierRespectsParentRootBudget(t *testing.T) {
	root := budget.NewRoot(agentapi.RunLimits{MaxTotalTokens: 140})
	var (
		mu          sync.Mutex
		runtimeRuns []string
	)
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(ctx context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
			mu.Lock()
			runtimeRuns = append(runtimeRuns, request.Agent.ID)
			mu.Unlock()
			gate := agentapi.RunBudgetUsageGateFromContext(ctx)
			if gate == nil {
				return agentapi.RunResult{}, fmt.Errorf("budget gate missing for %s", request.Agent.ID)
			}
			reservation, err := gate.ReserveCall(agentapi.Usage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			})
			if err != nil {
				return agentapi.RunResult{}, err
			}
			if err := reservation.Settle(agentapi.Usage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			}); err != nil {
				return agentapi.RunResult{}, err
			}
			if request.Agent.ID == "delegation.verifier" {
				return successfulVerificationResult(t, request), nil
			}
			return semanticInvestigationResult(
				request.RunID,
				"report for "+executorObjective(t, request),
				"claim for "+executorObjective(t, request),
				"evidence-"+executorObjective(t, request),
			), nil
		}),
		newExecutorPersistence(),
	)
	parentCtx := agentapi.WithRunBudgetGate(
		executorContext(t, context.Background(), 0),
		root,
	)
	parent, ok := ParentContextFrom(parentCtx)
	if !ok {
		t.Fatal("parent context is unavailable")
	}
	parent.HighRisk = true
	parentCtx = WithParentContext(parentCtx, parent)

	result, _, err := executor.Execute(parentCtx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "high-risk claim",
	}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotRuntimeRuns := append([]string(nil), runtimeRuns...)
	mu.Unlock()
	if len(gotRuntimeRuns) != 1 || gotRuntimeRuns[0] != "delegation.investigator" {
		t.Fatalf("runtime runs = %v, want investigator only", gotRuntimeRuns)
	}
	if result.Verification == nil ||
		result.Verification.Status != agentapi.DelegationRejected ||
		result.Verification.Error == nil ||
		result.Verification.Error.Code != ErrorBudgetInsufficient {
		t.Fatalf("verification = %+v", result.Verification)
	}
	if got := root.Used().TotalTokens; got != 15 {
		t.Fatalf("root used total = %d, want 15", got)
	}
	if got := root.Available().TotalTokens; got != 125 {
		t.Fatalf("root availability = %d, want 125 after child", got)
	}
}

func TestExecutorBoundsConcurrencyPreservesOrderAndNarrowsChild(t *testing.T) {
	var (
		mu        sync.Mutex
		active    int
		maxActive int
		requests  []agentapi.RunRequest
	)
	runtime := executorRuntimeFunc(func(
		ctx context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		objective := executorObjective(t, request)
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		requests = append(requests, request)
		mu.Unlock()
		if objective == "first" {
			time.Sleep(30 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		active--
		mu.Unlock()
		return successfulExecutorResult(request.RunID, objective), nil
	})
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(t, runtime, persistence, func(policy *agentapi.DelegationPolicy) {
		policy.MaxConcurrent = 2
	})
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{
			{Capability: "knowledge.code.inspect", Objective: "first"},
			{Capability: "knowledge.code.inspect", Objective: "second"},
			{Capability: "knowledge.code.inspect", Objective: "third"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 3 ||
		result.Results[0].Summary != "first" ||
		result.Results[1].Summary != "second" ||
		result.Results[2].Summary != "third" {
		t.Fatalf("results = %+v", result.Results)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 2 {
		t.Fatalf("max active children = %d, want 2", maxActive)
	}
	for _, request := range requests {
		if len(request.Permissions.Scopes) != 1 ||
			request.Permissions.Scopes[0] != "knowledge.read" {
			t.Fatalf("child permissions = %v", request.Permissions.Scopes)
		}
		if !request.ToolScope.RestrictVisible ||
			len(request.ToolScope.VisibleToolIDs) != 1 ||
			request.ToolScope.VisibleToolIDs[0] != "search_code" {
			t.Fatalf("child tool scope = %+v", request.ToolScope)
		}
		if request.Delegation.Depth != 1 ||
			request.Correlation.ParentRunID != "parent-1" {
			t.Fatalf("child delegation = %+v correlation=%+v",
				request.Delegation, request.Correlation)
		}
	}
	if len(persistence.settlements) != 3 {
		t.Fatalf("settlements = %d, want 3", len(persistence.settlements))
	}
	for _, settlement := range persistence.settlements {
		if settlement.Artifact == nil {
			t.Fatal("successful child settled without report artifact")
		}
		if settlement.EvidenceArtifact == nil {
			t.Fatal("successful child settled without evidence artifact")
		}
	}
}

func TestExecutorProjectsAgentIdentityOnDelegationEvents(t *testing.T) {
	events := &executorEventRecorder{}
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			return successfulExecutorResult(request.RunID, executorObjective(t, request)), nil
		}),
		newExecutorPersistence(),
		nil,
	)
	executor.events = events
	if _, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect checkout",
		}},
	); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, event := range events.snapshot() {
		if event.eventType != agentrun.EventDelegationCreated &&
			event.eventType != agentrun.EventDelegationStarted &&
			event.eventType != agentrun.EventDelegationDone {
			continue
		}
		if event.event.AgentID != "delegation.investigator" ||
			event.event.AgentName != "Code Investigator" {
			t.Fatalf("%s agent = %q %q", event.eventType, event.event.AgentID, event.event.AgentName)
		}
		seen++
	}
	if seen < 2 {
		t.Fatalf("agent identity events = %d, want created/started/completed", seen)
	}
}

func TestExecutorProjectsChildToolEventsOntoParent(t *testing.T) {
	runtime := &projectingExecutorRuntime{
		run: func(_ context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
			return successfulExecutorResult(request.RunID, executorObjective(t, request)), nil
		},
	}
	executor := newExecutorFixture(t, runtime, newExecutorPersistence(), nil)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect checkout",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].RunID == "" {
		t.Fatalf("result = %+v", result.Results)
	}
	if runtime.childRunID != result.Results[0].RunID ||
		runtime.parentRunID != "parent-1" ||
		runtime.nodeID != result.Results[0].RunID {
		t.Fatalf("projection = child=%q parent=%q node=%q",
			runtime.childRunID, runtime.parentRunID, runtime.nodeID)
	}
	if !runtime.stopped {
		t.Fatal("child tool projection was not stopped")
	}
}

type projectingExecutorRuntime struct {
	run         executorRuntimeFunc
	childRunID  string
	parentRunID string
	nodeID      string
	stopped     bool
}

func (runtime *projectingExecutorRuntime) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return runtime.run(ctx, request)
}

func (runtime *projectingExecutorRuntime) ProjectToolEvents(
	childRunID string,
	parentRunID string,
	_ string,
	nodeID string,
) func() {
	runtime.childRunID = childRunID
	runtime.parentRunID = parentRunID
	runtime.nodeID = nodeID
	return func() { runtime.stopped = true }
}

func TestExecutorDropsUnauthorizedHintsWithoutRejectingTask(t *testing.T) {
	var captured agentapi.RunRequest
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		captured = request
		return successfulExecutorResult(request.RunID, "inspect"), nil
	}), persistence, nil)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability:   "knowledge.code.inspect",
			Objective:    "inspect",
			FocusFacets:  []string{"core_flow", "设备激活与配网"},
			EvidenceRefs: []string{"owned-evidence", "not-owned"},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 ||
		result.Results[0].Status != agentapi.DelegationCompleted ||
		captured.RunID == "" {
		t.Fatalf("result = %+v captured=%+v", result, captured)
	}
	facets, refs := executorTaskHints(t, captured)
	if len(facets) != 1 || facets[0] != "core_flow" {
		t.Fatalf("focus_facets = %v", facets)
	}
	if len(refs) != 1 || refs[0] != "owned-evidence" {
		t.Fatalf("evidence_refs = %v", refs)
	}
	if len(persistence.rejections) != 0 || len(persistence.linked) != 1 {
		t.Fatalf("rejections=%d linked=%v",
			len(persistence.rejections), persistence.linked)
	}
}

func TestExecutorAcceptsLiveManifestEvidenceHandle(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "runbook", Target: "device.md", ContentHash: "hash-1",
	}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("unit handle")
	}
	var captured agentapi.RunRequest
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		captured = request
		return successfulExecutorResult(request.RunID, "inspect"), nil
	}), newExecutorPersistence(), nil)
	ctx := executorContext(t, context.Background(), 0)
	ctx = WithLiveEvidence(ctx, []tool.EvidenceUnit{unit})
	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability:   "knowledge.code.inspect",
		Objective:    "inspect",
		EvidenceRefs: []string{handle},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 ||
		result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", result)
	}
	_, refs := executorTaskHints(t, captured)
	if len(refs) != 1 || refs[0] != handle {
		t.Fatalf("evidence_refs = %v, want %s", refs, handle)
	}
}

func TestExecutorEmptyHintsEncodeAsArrays(t *testing.T) {
	var captured agentapi.RunRequest
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		captured = request
		return successfulExecutorResult(request.RunID, "inspect"), nil
	}), newExecutorPersistence(), nil)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability:   "knowledge.code.inspect",
			Objective:    "inspect",
			FocusFacets:  []string{"设备激活与配网"},
			EvidenceRefs: []string{"not-owned"},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 ||
		result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", result)
	}
	if !bytes.Contains(captured.Input, []byte(`"focus_facets":[]`)) ||
		!bytes.Contains(captured.Input, []byte(`"evidence_refs":[]`)) {
		t.Fatalf("input = %s", captured.Input)
	}
}

func TestDelegateToolDoesNotInheritCallerDeadline(t *testing.T) {
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return agentapi.RunResult{}, nil
	}), newExecutorPersistence(), nil)
	if executor.Tool().Timeout == tool.InheritCallerDeadline || executor.Tool().Timeout <= 0 {
		t.Fatalf("timeout = %s, want a bounded short timeout", executor.Tool().Timeout)
	}
	if executor.StatusTool().Timeout == tool.InheritCallerDeadline || executor.StatusTool().Timeout <= 0 {
		t.Fatalf("status timeout = %s, want a bounded short timeout", executor.StatusTool().Timeout)
	}
}

func TestExecutorChildTimeoutDoesNotCancelSibling(t *testing.T) {
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			ctx context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			objective := executorObjective(t, request)
			if objective == "timeout" {
				<-ctx.Done()
				return agentapi.RunResult{}, ctx.Err()
			}
			time.Sleep(5 * time.Millisecond)
			return successfulExecutorResult(request.RunID, objective), nil
		}),
		newExecutorPersistence(),
		func(policy *agentapi.DelegationPolicy) {
			policy.ChildTimeout = 40 * time.Millisecond
			policy.MaxConcurrent = 2
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{
			{Capability: "knowledge.code.inspect", Objective: "timeout"},
			{Capability: "knowledge.code.inspect", Objective: "sibling"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Results[0].Status != agentapi.DelegationTimeout ||
		result.Results[0].Error == nil ||
		result.Results[0].Error.Code != ErrorChildTimeout {
		t.Fatalf("timed out result = %+v", result.Results[0])
	}
	if result.Results[1].Status != agentapi.DelegationCompleted ||
		result.Results[1].Summary != "sibling" {
		t.Fatalf("sibling result = %+v", result.Results[1])
	}
}

func TestExecutorParentCancellationStopsQueuedChildren(t *testing.T) {
	started := make(chan struct{})
	var (
		mu           sync.Mutex
		runtimeCalls int
	)
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			ctx context.Context,
			_ agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			mu.Lock()
			runtimeCalls++
			mu.Unlock()
			select {
			case <-started:
			default:
				close(started)
			}
			<-ctx.Done()
			return agentapi.RunResult{}, ctx.Err()
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxConcurrent = 1
		},
	)
	ctx, cancel := context.WithCancel(
		executorContext(t, context.Background(), 0),
	)
	type executionResult struct {
		result agentapi.DelegationBatchResult
		err    error
	}
	done := make(chan executionResult, 1)
	go func() {
		result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{
			{Capability: "knowledge.code.inspect", Objective: "first"},
			{Capability: "knowledge.code.inspect", Objective: "queued-1"},
			{Capability: "knowledge.code.inspect", Objective: "queued-2"},
		})
		done <- executionResult{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first child did not start")
	}
	cancel()
	execution := <-done
	if execution.err != nil {
		t.Fatal(execution.err)
	}
	mu.Lock()
	if runtimeCalls != 1 {
		t.Fatalf("runtime calls = %d, want 1", runtimeCalls)
	}
	mu.Unlock()
	persistence.mu.Lock()
	if len(persistence.linked) != 1 {
		t.Fatalf("linked children = %v, want only the active child", persistence.linked)
	}
	persistence.mu.Unlock()
	for index, report := range execution.result.Results {
		if report.Status != agentapi.DelegationCancelled ||
			report.Error == nil ||
			report.Error.Code != ErrorParentCancelled {
			t.Fatalf("result %d = %+v", index, report)
		}
	}
}

func TestExecutorReturnsOnlyAfterReportSettlement(t *testing.T) {
	persistence := newExecutorPersistence()
	persistence.settleStarted = make(chan struct{})
	persistence.settleRelease = make(chan struct{})
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			return successfulExecutorResult(request.RunID, "settled"), nil
		}),
		persistence,
		nil,
	)
	done := make(chan error, 1)
	go func() {
		_, _, err := executor.Execute(
			executorContext(t, context.Background(), 0),
			[]agentapi.DelegationTask{{
				Capability: "knowledge.code.inspect",
				Objective:  "settled",
			}},
		)
		done <- err
	}()
	select {
	case <-persistence.settleStarted:
	case <-time.After(time.Second):
		t.Fatal("report settlement did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("Execute returned before report settlement completed: %v", err)
	default:
	}
	close(persistence.settleRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecutorParentCancellationDuringCompletedChildWaitsForSettlement(t *testing.T) {
	persistence := newExecutorPersistence()
	persistence.settleStarted = make(chan struct{})
	persistence.settleRelease = make(chan struct{})
	events := &executorEventRecorder{}
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			return successfulExecutorResult(request.RunID, "completed before cancel"), nil
		}),
		persistence,
		nil,
	)
	executor.events = events
	ctx, cancel := context.WithCancel(executorContext(t, context.Background(), 0))
	defer cancel()
	type executionResult struct {
		result agentapi.DelegationBatchResult
		err    error
	}
	done := make(chan executionResult, 1)
	go func() {
		result, _, err := executor.Execute(
			ctx,
			[]agentapi.DelegationTask{{
				Capability: "knowledge.code.inspect",
				Objective:  "completed before cancel",
			}},
		)
		done <- executionResult{result: result, err: err}
	}()
	select {
	case <-persistence.settleStarted:
	case <-time.After(time.Second):
		t.Fatal("report settlement did not start")
	}
	cancel()
	select {
	case execution := <-done:
		t.Fatalf(
			"Execute returned before cancelled parent settlement completed: %+v",
			execution,
		)
	default:
	}
	close(persistence.settleRelease)
	execution := <-done
	if execution.err != nil {
		t.Fatal(execution.err)
	}
	if len(execution.result.Results) != 1 ||
		execution.result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", execution.result.Results)
	}
	persistence.mu.Lock()
	if len(persistence.settlements) != 1 ||
		persistence.settlements[0].Artifact == nil {
		t.Fatalf("settlements = %+v", persistence.settlements)
	}
	persistence.mu.Unlock()
	terminalEvents := 0
	validationEvents := 0
	for _, event := range events.snapshot() {
		switch event.eventType {
		case agentrun.EventDelegationDone,
			agentrun.EventDelegationFailed,
			agentrun.EventDelegationCancelled:
			terminalEvents++
			if event.eventType != agentrun.EventDelegationDone ||
				event.event.Status != string(agentapi.DelegationCompleted) ||
				event.event.ToolCalls != 1 ||
				event.event.ReportBytes <= 0 ||
				event.event.Completeness != string(agentapi.DelegationComplete) {
				t.Fatalf("terminal event = %+v", event)
			}
		case agentrun.EventDelegationValidated:
			validationEvents++
			if event.event.Status != "completed" ||
				event.event.CitationCoverage != 1 ||
				event.event.ConflictCount != 0 ||
				event.event.RequiresVerification {
				t.Fatalf("validation event = %+v", event)
			}
		}
	}
	if terminalEvents != 1 {
		t.Fatalf("terminal events = %d, want 1", terminalEvents)
	}
	if validationEvents != 1 {
		t.Fatalf("validation events = %d, want 1", validationEvents)
	}
}

func TestExecutorInterruptedReportEmitsFailedEventWithInterruptedStatus(t *testing.T) {
	events := &executorEventRecorder{}
	executor := &Executor{events: events}
	task := preparedTask{
		childRunID: "child-1",
		request: agentapi.DelegationTask{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect interrupted work",
		},
	}
	executor.emitTerminal(
		ParentContext{RunID: "parent-1"},
		"del-1",
		task,
		agentapi.DelegationReport{
			RunID:        "child-1",
			ReportID:     "report-1",
			Status:       agentapi.DelegationInterrupted,
			Completeness: agentapi.DelegationIncomplete,
			Error: &agentapi.RunError{
				Code: ErrorInterrupted,
			},
		},
		time.Time{},
	)
	recorded := events.snapshot()
	if len(recorded) != 1 ||
		recorded[0].eventType != agentrun.EventDelegationFailed ||
		recorded[0].event.Status != string(agentapi.DelegationInterrupted) ||
		recorded[0].event.ErrorCode != ErrorInterrupted {
		t.Fatalf("event = %+v", recorded)
	}
}

func TestExecutorReplayRestoresPersistedEvidence(t *testing.T) {
	var runtimeCalls int
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			runtimeCalls++
			return successfulExecutorResult(request.RunID, "replayed"), nil
		}),
		persistence,
		nil,
	)
	task := []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "replayed",
	}}
	ctx := executorContext(t, context.Background(), 0)
	first, firstEvidence, err := executor.Execute(ctx, task)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	second, secondEvidence, err := executor.Execute(ctx, task)
	if err != nil {
		t.Fatalf("replayed Execute: %v", err)
	}
	if runtimeCalls != 1 {
		t.Fatalf("runtime calls = %d, want 1", runtimeCalls)
	}
	if len(first.Results) != 1 || len(second.Results) != 1 ||
		first.Results[0].ReportID != second.Results[0].ReportID {
		t.Fatalf("first=%+v second=%+v", first.Results, second.Results)
	}
	if len(firstEvidence) != 1 || len(secondEvidence) != 1 ||
		firstEvidence[0].ContentHash != secondEvidence[0].ContentHash {
		t.Fatalf(
			"first evidence=%+v replayed evidence=%+v",
			firstEvidence,
			secondEvidence,
		)
	}
}

func TestExecutorReplayRejectsMismatchedReportIdentityWithoutRestartingChild(
	t *testing.T,
) {
	tests := []struct {
		name   string
		mutate func(*agentrun.DelegationArtifact)
	}{
		{
			name: "artifact run",
			mutate: func(artifact *agentrun.DelegationArtifact) {
				artifact.RunID = "run_wrong"
			},
		},
		{
			name: "schema",
			mutate: func(artifact *agentrun.DelegationArtifact) {
				artifact.Schema.Version = 2
			},
		},
		{
			name: "report capability",
			mutate: func(artifact *agentrun.DelegationArtifact) {
				var report agentapi.DelegationReport
				if err := json.Unmarshal(artifact.Content, &report); err != nil {
					panic(err)
				}
				report.Capability = "knowledge.other"
				raw, err := json.Marshal(report)
				if err != nil {
					panic(err)
				}
				artifact.Content = raw
				artifact.ContentHash = hashBytes(raw)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var runtimeCalls int
			persistence := newExecutorPersistence()
			executor := newExecutorFixture(
				t,
				executorRuntimeFunc(func(
					_ context.Context,
					request agentapi.RunRequest,
				) (agentapi.RunResult, error) {
					runtimeCalls++
					return successfulExecutorResult(
						request.RunID,
						"replayed",
					), nil
				}),
				persistence,
				nil,
			)
			task := []agentapi.DelegationTask{{
				Capability: "knowledge.code.inspect",
				Objective:  "replayed",
			}}
			ctx := executorContext(t, context.Background(), 0)
			if _, _, err := executor.Execute(ctx, task); err != nil {
				t.Fatalf("first Execute: %v", err)
			}
			persistence.mu.Lock()
			for id, artifact := range persistence.artifacts {
				test.mutate(&artifact)
				persistence.artifacts[id] = artifact
			}
			persistence.mu.Unlock()

			result, _, err := executor.Execute(ctx, task)
			if err != nil {
				t.Fatalf("replayed Execute: %v", err)
			}
			if runtimeCalls != 1 {
				t.Fatalf("runtime calls = %d, want 1", runtimeCalls)
			}
			if len(result.Results) != 1 ||
				result.Results[0].Status != agentapi.DelegationFailed ||
				result.Results[0].Error == nil ||
				result.Results[0].Error.Code != ErrorReportPersistenceFailed {
				t.Fatalf("result = %+v", result.Results)
			}
		})
	}
}

func TestExecutorHighRiskRunsBoundedSemanticVerifier(t *testing.T) {
	var (
		runtimeCalls      int
		verificationInput json.RawMessage
		verificationRun   agentapi.RunRequest
	)
	persistence := newExecutorPersistence()
	events := &executorEventRecorder{}
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			runtimeCalls++
			if request.Agent.ID != "delegation.verifier" {
				return semanticInvestigationResult(
					request.RunID,
					"complete report material that must stay out of verifier input",
					"the bounded claim",
					"evidence-semantic",
				), nil
			}
			verificationRun = request
			verificationInput = append(
				json.RawMessage(nil),
				request.Input...,
			)
			var input agentapi.DelegationVerificationRequest
			if err := json.Unmarshal(request.Input, &input); err != nil {
				t.Fatal(err)
			}
			output, err := json.Marshal(verificationOutput{
				Summary: "the cited claim is supported",
				Verdicts: []agentapi.DelegationVerificationVerdict{{
					ClaimIDs:     []string{input.Claims[0].ID},
					Decision:     "supported",
					Rationale:    "the selected evidence supports the claim",
					EvidenceRefs: []string{"evidence-semantic"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			return agentapi.RunResult{
				RunID: request.RunID, Status: agentapi.RunSucceeded,
				Output: output,
				Usage: agentapi.Usage{
					InputTokens: 20, OutputTokens: 10, TotalTokens: 30,
				},
			}, nil
		}),
		persistence,
	)
	executor.events = events
	ctx := executorContext(t, context.Background(), 0)
	parent, _ := ParentContextFrom(ctx)
	parent.HighRisk = true
	ctx = WithParentContext(ctx, parent)

	result, _, err := executor.Execute(ctx, []agentapi.DelegationTask{{
		Capability: "knowledge.code.inspect",
		Objective:  "inspect the high-risk claim",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeCalls != 2 ||
		!result.Validation.RequiresVerification ||
		len(result.Validation.VerificationReasons) != 1 ||
		result.Validation.VerificationReasons[0] != ReasonHighRiskPolicy ||
		result.Verification == nil ||
		result.Verification.Status != agentapi.DelegationCompleted {
		t.Fatalf("result=%+v runtime calls=%d", result, runtimeCalls)
	}
	if !verificationRun.ToolScope.RestrictVisible ||
		len(verificationRun.ToolScope.VisibleToolIDs) != 0 ||
		verificationRun.Policy.MaxToolCalls != 0 ||
		verificationRun.Limits.MaxToolCalls != 0 {
		t.Fatalf("verifier tool scope = %+v policy=%+v limits=%+v",
			verificationRun.ToolScope, verificationRun.Policy, verificationRun.Limits)
	}
	var inputFields map[string]json.RawMessage
	if err := json.Unmarshal(verificationInput, &inputFields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"reports", "report", "summary", "trace", "traces", "usage",
		"tool_calls", "messages",
	} {
		if _, ok := inputFields[forbidden]; ok {
			t.Fatalf("verifier input contains forbidden field %q: %s",
				forbidden, verificationInput)
		}
	}
	if strings.Contains(
		string(verificationInput),
		"complete report material that must stay out of verifier input",
	) {
		t.Fatalf("verifier input leaked the complete report: %s", verificationInput)
	}
	assertVerificationLifecycle(t, events.snapshot())
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	var verificationArtifact *agentrun.DelegationArtifact
	for _, settlement := range persistence.settlements {
		if settlement.Artifact != nil &&
			settlement.Artifact.Kind ==
				agentrun.DelegationVerificationArtifactKind {
			verificationArtifact = settlement.Artifact
			break
		}
	}
	if verificationArtifact == nil {
		t.Fatal("semantic verification artifact was not settled")
	}
}

func TestFlowChildBudgetMatchesOrdinaryDelegation(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns:        4,
		MaxChildToolCalls:    16,
		MaxChildOutputTokens: 16000,
		MaxReportTokens:      4000,
		ChildTimeout:         200 * time.Millisecond,
	}}
	parent := ParentContext{
		OutputContract: agentapi.RunOutputContract{
			Subjects: []string{"订单创建"},
			MaxHops:  6,
		},
		Limits: agentapi.RunLimits{Deadline: time.Now().UTC().Add(time.Minute)},
	}
	budget := executor.childBudget(parent)
	if budget.turns != executor.policy.MaxChildTurns ||
		budget.toolCalls != executor.policy.MaxChildToolCalls ||
		budget.outputTokens != executor.policy.MaxChildOutputTokens ||
		budget.reportTokens != executor.policy.MaxReportTokens {
		t.Fatalf("flow child budget was narrowed unexpectedly = %#v", budget)
	}
	limits, err := executor.childLimits(parent, agentapi.Definition{
		Model: agentapi.ModelPolicy{
			InputPriceMicrosPerMillionTokens:  1,
			OutputPriceMicrosPerMillionTokens: 1,
		},
		Budget: agentapi.BudgetPolicy{Timeout: time.Second, MaxSteps: 8, MaxToolCalls: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxSteps != executor.policy.MaxChildTurns ||
		limits.MaxToolCalls != executor.policy.MaxChildToolCalls ||
		limits.MaxTotalTokens != 0 {
		t.Fatalf("flow child limits = %#v", limits)
	}
}

func TestChildInputCarriesParentFlowShapeWithoutForcingMermaidOnChild(t *testing.T) {
	parent := ParentContext{
		RunID: "parent-1", QuestionSummary: "订单创建流程",
		OutputContract: agentapi.RunOutputContract{
			MaxHops: 6,
		},
	}
	task := preparedTask{
		capability: agentapi.Capability{ID: "knowledge.code.inspect"},
		request:    agentapi.DelegationTask{Objective: "调查订单创建主路径"},
	}
	raw, err := childInput(parent, "del-1", task)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if int(payload["max_hops"].(float64)) != 6 {
		t.Fatalf("flow shape missing from child input: %#v", payload)
	}
	if _, has := payload["output_kind"]; has {
		t.Fatalf("child input should not carry a classification kind: %#v", payload)
	}
	request := (&Executor{}).runRequest(parent, "del-1", task)
	if request.Policy.OutputContract.MaxHops != 0 || len(request.Policy.OutputContract.Subjects) != 0 {
		t.Fatalf("child inherited user-facing flow contract: %#v", request.Policy.OutputContract)
	}
}

func TestChildLimitsClampsOutputTokensToDefinitionModel(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns: 3, MaxChildToolCalls: 4,
		MaxChildOutputTokens: 128,
		ChildTimeout:         200 * time.Millisecond,
	}}
	parent := ParentContext{Limits: agentapi.RunLimits{Deadline: time.Now().UTC().Add(time.Minute)}}
	limits, err := executor.childLimits(parent, agentapi.Definition{
		Model:  agentapi.ModelPolicy{MaxOutputTokens: 64},
		Budget: agentapi.BudgetPolicy{Timeout: time.Second, MaxSteps: 8, MaxToolCalls: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxOutputTokens != 64 || limits.MaxTotalTokens != 0 || limits.MaxInputTokens != 0 {
		t.Fatalf("child limits = %+v, want output=64 with no cumulative input/total quota", limits)
	}
}

func TestChildLimitsForContextUsesCallerDeadline(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns: 3, MaxChildToolCalls: 4,
		MaxChildOutputTokens: 128,
		ChildTimeout:         10 * time.Second,
	}}
	parent := ParentContext{Limits: agentapi.RunLimits{Deadline: time.Now().UTC().Add(time.Minute)}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	contextDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context deadline was not installed")
	}

	limits, err := executor.childLimitsForContext(ctx, parent, agentapi.Definition{
		Model:  agentapi.ModelPolicy{MaxOutputTokens: 256},
		Budget: agentapi.BudgetPolicy{Timeout: time.Minute, MaxSteps: 8, MaxToolCalls: 24},
	})
	if err != nil {
		t.Fatalf("childLimitsForContext: %v", err)
	}
	if limits.Deadline.After(contextDeadline) {
		t.Fatalf("child deadline %s exceeds caller deadline %s", limits.Deadline, contextDeadline)
	}
}

func TestChildLimitsClampsToolCallsToDefinitionBudget(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns: 3, MaxChildToolCalls: 4,
		MaxChildOutputTokens: 128,
		ChildTimeout:         200 * time.Millisecond,
	}}
	parent := ParentContext{
		Limits: agentapi.RunLimits{Deadline: time.Now().UTC().Add(time.Minute)},
	}

	observe, err := executor.childLimits(parent, agentapi.Definition{
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 8, MaxToolCalls: 24,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observe.MaxToolCalls != 4 {
		t.Fatalf("observe max tool calls = %d, want policy cap 4", observe.MaxToolCalls)
	}

	toolless, err := executor.childLimits(parent, agentapi.Definition{
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 8, MaxToolCalls: 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolless.MaxToolCalls != 0 {
		t.Fatalf("tool-less max tool calls = %d, want 0", toolless.MaxToolCalls)
	}

	tight, err := executor.childLimits(parent, agentapi.Definition{
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 2, MaxToolCalls: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tight.MaxToolCalls != 2 {
		t.Fatalf("tight budget max tool calls = %d, want 2", tight.MaxToolCalls)
	}
}

func TestExecutorMultipleReportsRunSemanticVerifier(t *testing.T) {
	var (
		mu            sync.Mutex
		verifierCalls int
	)
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			if request.Agent.ID == "delegation.verifier" {
				mu.Lock()
				verifierCalls++
				mu.Unlock()
				output, err := json.Marshal(verificationOutput{
					Summary: "the reports require semantic reconciliation",
				})
				if err != nil {
					t.Fatal(err)
				}
				return agentapi.RunResult{
					RunID: request.RunID, Status: agentapi.RunSucceeded,
					Output: output,
				}, nil
			}
			objective := executorObjective(t, request)
			return semanticInvestigationResult(
				request.RunID,
				"report for "+objective,
				"claim for "+objective,
				"evidence-"+objective,
			), nil
		}),
		newExecutorPersistence(),
	)

	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{
			{
				Capability: "knowledge.code.inspect",
				Objective:  "implementation",
			},
			{
				Capability: "knowledge.code.inspect",
				Objective:  "runtime",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotVerifierCalls := verifierCalls
	mu.Unlock()
	if gotVerifierCalls != 1 ||
		!result.Validation.UnverifiedSemanticOverlap ||
		!result.Validation.RequiresVerification ||
		len(result.Validation.VerificationReasons) != 1 ||
		result.Validation.VerificationReasons[0] !=
			ReasonUnstructuredCrossReportMerge ||
		result.Verification == nil ||
		result.Verification.Status != agentapi.DelegationCompleted {
		t.Fatalf("result=%+v verifier calls=%d",
			result, gotVerifierCalls)
	}
}

func TestSemanticVerifierReplayDoesNotRestartRuntime(t *testing.T) {
	var runtimeCalls int
	persistence := newExecutorPersistence()
	events := &executorEventRecorder{}
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			runtimeCalls++
			return successfulVerificationResult(t, request), nil
		}),
		persistence,
	)
	executor.events = events
	parent := semanticVerifierParent(t)
	reports, validation, evidence := semanticVerificationFacts()

	first := executor.executeVerification(
		t.Context(), parent, "del-verifier", reports, validation, evidence,
	)
	second := executor.executeVerification(
		t.Context(), parent, "del-verifier", reports, validation, evidence,
	)

	if runtimeCalls != 1 ||
		first.Status != agentapi.DelegationCompleted ||
		second.Status != agentapi.DelegationCompleted ||
		first.VerificationID != second.VerificationID ||
		first.Summary != second.Summary {
		t.Fatalf("first=%+v second=%+v runtime calls=%d",
			first, second, runtimeCalls)
	}
	assertVerificationLifecycle(t, events.snapshot())
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.settlements) != 1 ||
		persistence.settlements[0].Artifact == nil ||
		persistence.settlements[0].Artifact.Kind !=
			agentrun.DelegationVerificationArtifactKind {
		t.Fatalf("settlements = %+v", persistence.settlements)
	}
}

func TestSemanticVerifierBudgetRejectionIsTyped(t *testing.T) {
	var runtimeCalls int
	persistence := &verifierBudgetPersistence{
		executorPersistence: newExecutorPersistence(),
	}
	events := &executorEventRecorder{}
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(
			context.Context,
			agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			runtimeCalls++
			return agentapi.RunResult{}, nil
		}),
		persistence,
	)
	executor.events = events
	reports, validation, evidence := semanticVerificationFacts()

	verification := executor.executeVerification(
		t.Context(),
		semanticVerifierParent(t),
		"del-budget",
		reports,
		validation,
		evidence,
	)

	if runtimeCalls != 0 ||
		verification.Status != agentapi.DelegationRejected ||
		verification.Error == nil ||
		verification.Error.Code != ErrorBudgetInsufficient {
		t.Fatalf("verification=%+v runtime calls=%d",
			verification, runtimeCalls)
	}
	recorded := events.snapshot()
	if len(recorded) != 1 ||
		recorded[0].eventType !=
			agentrun.EventDelegationVerificationRejected ||
		recorded[0].event.ErrorCode != ErrorBudgetInsufficient {
		t.Fatalf("events = %+v", recorded)
	}
}

func TestSemanticVerifierRecoversUnsettledAdmission(t *testing.T) {
	var runtimeCalls int
	persistence := newExecutorPersistence()
	executor := newSemanticVerifierExecutor(
		t,
		executorRuntimeFunc(func(
			context.Context,
			agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			runtimeCalls++
			return agentapi.RunResult{}, nil
		}),
		persistence,
	)
	parent := semanticVerifierParent(t)
	reports, validation, evidence := semanticVerificationFacts()
	task, code, err := executor.prepareVerification(
		parent,
		"del-recovery",
		len(reports),
		reports,
		validation,
		evidence,
	)
	if err != nil {
		t.Fatalf("prepare verification (%s): %v", code, err)
	}
	key := executorTaskKey(parent.RunID, "del-recovery", task.index)
	persistence.records[key] = agentrun.DelegationTaskRecord{
		ParentRunID:  parent.RunID,
		DelegationID: "del-recovery",
		TaskIndex:    task.index,
		ChildRunID:   task.childRunID,
		Capability: agentapi.CapabilityRef{
			ID: task.capability.ID, Version: task.capability.Version,
		},
		CapabilityHash: task.capability.ContentHash,
		ObjectiveHash:  task.objectiveHash,
		Admitted:       true,
	}

	verification := executor.executeVerification(
		t.Context(),
		parent,
		"del-recovery",
		reports,
		validation,
		evidence,
	)

	if runtimeCalls != 0 ||
		verification.Status != agentapi.DelegationFailed ||
		verification.Error == nil ||
		verification.Error.Code != ErrorInterrupted {
		t.Fatalf("verification=%+v runtime calls=%d",
			verification, runtimeCalls)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.settlements) != 1 ||
		persistence.settlements[0].Artifact == nil ||
		persistence.settlements[0].Artifact.Kind !=
			agentrun.DelegationVerificationArtifactKind {
		t.Fatalf("settlements = %+v", persistence.settlements)
	}
}

func newExecutorFixture(
	t *testing.T,
	runtime agentapi.Runtime,
	persistence Persistence,
	mutatePolicy func(*agentapi.DelegationPolicy),
) *Executor {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{
			ID: "delegation.input", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
		{
			ID: "delegation.output", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
		{
			ID: "delegation.verification.request", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
		{
			ID: "delegation.verification.result", Version: 1,
			Document: []byte(`{"type":"object"}`),
		},
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "delegation.investigator", Version: 1,
		DisplayName: "Code Investigator",
		Prompt: agentapi.PromptSpec{
			System: "Inspect the delegated objective.", Version: "1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "delegation.input", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "delegation.output", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: "test", Model: "test", MaxOutputTokens: 128,
		},
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs:  []string{"delegate_investigation", "search_code"},
			RestrictVisible: true,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: 2 * time.Second, MaxSteps: 4, ContextTokens: 2048,
		},
		Permissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read", "tenant.read"},
		},
		FailurePolicy: agentapi.FailurePolicy{MaxInfrastructureRetries: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := executorDefinitionResolver{
		{ID: definition.ID, Version: definition.Version}: definition,
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, definitions)
	if err := capabilities.Publish([]agentapi.Capability{{
		ID: "knowledge.code.inspect", Version: 1,
		Role:            agentapi.RoleInvestigator,
		Purpose:         "Inspect source code.",
		InputFacets:     []string{"core_flow", "implementation"},
		InputSchema:     agentapi.SchemaRef{ID: "delegation.input", Version: 1},
		OutputSchema:    agentapi.SchemaRef{ID: "delegation.output", Version: 1},
		ToolIDs:         []string{"delegate_investigation", "search_code"},
		PermissionScope: []string{"knowledge.read"},
		Freshness:       agentapi.FreshnessStable,
		SideEffects:     agentapi.SideEffectNone,
		MaxConcurrency:  2,
		Enabled:         true,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	policy := agentapi.DelegationPolicy{
		MaxDepth: 1, MaxChildren: 4, MaxConcurrent: 2,
		MaxChildTurns: 3, MaxChildToolCalls: 4,
		MaxChildOutputTokens: 128, MaxReportTokens: 512, MaxTotalTokens: 4096,
		MaxTotalCostMicros: 0, ParentAnswerReserve: 256,
		ChildTimeout: 200 * time.Millisecond,
	}
	if mutatePolicy != nil {
		mutatePolicy(&policy)
	}
	executor, err := NewExecutor(ExecutorConfig{
		Capabilities: capabilities,
		Definitions:  definitions,
		Runtime:      runtime,
		Persistence:  persistence,
		Policy:       policy,
		Allowlist:    []string{"knowledge.code.inspect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func newSemanticVerifierExecutor(
	t *testing.T,
	runtime agentapi.Runtime,
	persistence Persistence,
) *Executor {
	t.Helper()
	executor := newExecutorFixture(t, runtime, persistence, nil)
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "delegation.verifier", Version: 1,
		Prompt: agentapi.PromptSpec{
			System: "Verify only the bounded claims.", Version: "1",
		},
		InputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.request", Version: 1,
		},
		OutputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.result", Version: 1,
		},
		Model: agentapi.ModelPolicy{
			Provider: "test", Model: "test", MaxOutputTokens: 128,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: 2 * time.Second, MaxSteps: 2, ContextTokens: 2048,
		},
		Permissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions, ok := executor.definitions.(executorDefinitionResolver)
	if !ok {
		t.Fatalf("definitions = %T", executor.definitions)
	}
	definitions[agentapi.DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	}] = definition
	if err := executor.capabilities.Publish([]agentapi.Capability{{
		ID: SemanticVerifierCapabilityID, Version: 1,
		Role:            agentapi.RoleVerifier,
		Purpose:         "Verify bounded delegated claims.",
		InputSchema:     definition.InputSchema,
		OutputSchema:    definition.OutputSchema,
		PermissionScope: []string{"knowledge.read"},
		Freshness:       agentapi.FreshnessStable,
		SideEffects:     agentapi.SideEffectNone,
		MaxConcurrency:  1,
		Enabled:         true,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	return executor
}

func semanticVerifierParent(t *testing.T) ParentContext {
	t.Helper()
	parent, ok := ParentContextFrom(
		executorContext(t, context.Background(), 0),
	)
	if !ok {
		t.Fatal("parent context is unavailable")
	}
	return parent
}

func semanticVerificationFacts() (
	[]agentapi.DelegationReport,
	agentapi.DelegationValidation,
	map[string]tool.EvidenceUnit,
) {
	reports := []agentapi.DelegationReport{{
		RunID:        "child-semantic",
		ReportID:     "report-semantic",
		Capability:   "knowledge.code.inspect",
		Status:       agentapi.DelegationCompleted,
		Completeness: agentapi.DelegationComplete,
		Summary:      "complete report summary",
		Findings: []agentapi.DelegationFinding{{
			ID:         "claim-semantic",
			Statement:  "the bounded semantic claim",
			Confidence: agentapi.DelegationConfidenceHigh,
			Citations:  []string{"evidence-semantic"},
			Critical:   true,
		}},
	}}
	validation := agentapi.DelegationValidation{
		ReportIDs:            []string{"report-semantic"},
		RequiresVerification: true,
		VerificationReasons:  []string{ReasonHighRiskPolicy},
	}
	evidence := map[string]tool.EvidenceUnit{
		"evidence-semantic": {
			SourceKind: "code", Target: "semantic.go",
			ContentHash: "evidence-semantic",
		},
	}
	return reports, validation, evidence
}

func successfulVerificationResult(
	t *testing.T,
	request agentapi.RunRequest,
) agentapi.RunResult {
	t.Helper()
	var input agentapi.DelegationVerificationRequest
	if err := json.Unmarshal(request.Input, &input); err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(verificationOutput{
		Summary: "verification complete",
		Verdicts: []agentapi.DelegationVerificationVerdict{{
			ClaimIDs:     []string{input.Claims[0].ID},
			Decision:     "supported",
			Rationale:    "the cited evidence supports the claim",
			EvidenceRefs: []string{"evidence-semantic"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return agentapi.RunResult{
		RunID: request.RunID, Status: agentapi.RunSucceeded,
		Output: output,
		Usage: agentapi.Usage{
			InputTokens: 20, OutputTokens: 10, TotalTokens: 30,
		},
	}
}

func semanticInvestigationResult(
	runID,
	summary,
	claim,
	evidenceRef string,
) agentapi.RunResult {
	output, _ := json.Marshal(investigationOutput{
		Summary: summary,
		Findings: []investigationFinding{{
			Claim: claim, Confidence: 0.9,
			Evidence: []investigationEvidence{{Reference: evidenceRef}},
		}},
	})
	return agentapi.RunResult{
		RunID: runID, Status: agentapi.RunSucceeded, Output: output,
		Evidence: agentapi.EvidenceSummary{
			Status: "complete", ToolCallCount: 1, ResultCount: 1,
		},
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "code", Target: claim, ContentHash: evidenceRef,
		}},
		Usage: agentapi.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
	}
}

func assertVerificationLifecycle(
	t *testing.T,
	events []recordedExecutorEvent,
) {
	t.Helper()
	var started, done int
	for _, event := range events {
		switch event.eventType {
		case agentrun.EventDelegationVerificationStarted:
			started++
			if event.event.VerificationID == "" ||
				len(event.event.VerificationReasons) == 0 {
				t.Fatalf("started event = %+v", event)
			}
		case agentrun.EventDelegationVerificationDone:
			done++
			if event.event.VerificationID == "" ||
				event.event.Status !=
					string(agentapi.DelegationCompleted) ||
				event.event.Usage.TotalTokens != 30 {
				t.Fatalf("done event = %+v", event)
			}
		}
	}
	if started != 1 || done != 1 {
		t.Fatalf("verification lifecycle started=%d done=%d events=%+v",
			started, done, events)
	}
}

func executorContext(
	t *testing.T,
	ctx context.Context,
	depth int,
) context.Context {
	t.Helper()
	ctx = WithParentContext(ctx, ParentContext{
		RunID:           "parent-1",
		QuestionSummary: "How does the path work?",
		Actor:           agentapi.Actor{UserID: 42},
		Permissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read", "parent.only"},
		},
		Correlation: agentapi.Correlation{SessionID: "session-1"},
		Limits: agentapi.RunLimits{
			Deadline:       time.Now().Add(5 * time.Second),
			MaxTotalTokens: 8192,
		},
		Depth: depth,
		Evidence: map[string]tool.EvidenceUnit{
			"owned-evidence": {
				SourceKind: "code", Target: "owned.go", ContentHash: "owned-hash",
			},
		},
	})
	return tool.WithInvocationID(ctx, "tool-call-1")
}

func executorTaskHints(t *testing.T, request agentapi.RunRequest) ([]string, []string) {
	t.Helper()
	var payload struct {
		FocusFacets  []string `json:"focus_facets"`
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := json.Unmarshal(request.Input, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.FocusFacets, payload.EvidenceRefs
}

func executorObjective(t *testing.T, request agentapi.RunRequest) string {
	t.Helper()
	var payload struct {
		Objective string `json:"objective"`
	}
	if err := json.Unmarshal(request.Input, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Objective
}

func successfulExecutorResult(
	runID,
	summary string,
) agentapi.RunResult {
	evidenceRef := "evidence-" + summary
	output, _ := json.Marshal(investigationOutput{
		Summary: summary,
		Findings: []investigationFinding{{
			Claim: summary, Confidence: 0.9,
			Evidence: []investigationEvidence{{Reference: evidenceRef}},
		}},
	})
	return agentapi.RunResult{
		RunID: runID, Status: agentapi.RunSucceeded, Output: output,
		Evidence: agentapi.EvidenceSummary{
			Status: "complete", ToolCallCount: 1, ResultCount: 1,
		},
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "code", Target: summary, ContentHash: evidenceRef,
		}},
		Usage: agentapi.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
	}
}

type scriptedExecutorQueue struct {
	mu           sync.Mutex
	items        map[string]agentrun.WorkItem
	claimStarted chan struct{}
	claimOnce    sync.Once
	claimErr     error
}

func newScriptedExecutorQueue() *scriptedExecutorQueue {
	return &scriptedExecutorQueue{items: make(map[string]agentrun.WorkItem)}
}

func (queue *scriptedExecutorQueue) EnqueueWorkItem(_ context.Context, item agentrun.WorkItem) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.items[item.WorkID] = item
	return nil
}

func (queue *scriptedExecutorQueue) ClaimWorkItem(_ context.Context, owner string, now time.Time, ttl time.Duration) (agentrun.WorkItem, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for workID, item := range queue.items {
		if item.State != agentrun.WorkReady {
			continue
		}
		item.State = agentrun.WorkRunning
		item.LeaseOwner = owner
		item.LeaseFence++
		item.LeaseExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
		queue.items[workID] = item
		return item, nil
	}
	return agentrun.WorkItem{}, sql.ErrNoRows
}

func (queue *scriptedExecutorQueue) ClaimWorkItemByID(_ context.Context, workID, owner string, now time.Time, ttl time.Duration) (agentrun.WorkItem, error) {
	if queue.claimStarted != nil {
		queue.claimOnce.Do(func() { close(queue.claimStarted) })
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.claimErr != nil {
		return agentrun.WorkItem{}, queue.claimErr
	}
	item, ok := queue.items[workID]
	if !ok {
		return agentrun.WorkItem{}, sql.ErrNoRows
	}
	if item.State != agentrun.WorkReady {
		return agentrun.WorkItem{}, sql.ErrNoRows
	}
	item.State = agentrun.WorkRunning
	item.LeaseOwner = owner
	item.LeaseFence++
	item.LeaseExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
	queue.items[workID] = item
	return item, nil
}

func (queue *scriptedExecutorQueue) RenewWorkItem(_ context.Context, workID, owner string, fence int64, _ time.Time, _ time.Duration) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	item, ok := queue.items[workID]
	if !ok || item.State != agentrun.WorkRunning || item.LeaseOwner != owner || item.LeaseFence != fence {
		return fmt.Errorf("worker lease lost")
	}
	return nil
}

func (queue *scriptedExecutorQueue) CompleteWorkItem(_ context.Context, workID, owner string, fence int64, state, lastError string) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	item, ok := queue.items[workID]
	if !ok || item.State != agentrun.WorkRunning || item.LeaseOwner != owner || item.LeaseFence != fence {
		return fmt.Errorf("worker lease lost")
	}
	item.State = state
	item.LastError = lastError
	queue.items[workID] = item
	return nil
}

func queuedReportArtifact(childRunID, capability string) agentrun.DelegationArtifact {
	reportID := stableID("report", childRunID)
	report := agentapi.DelegationReport{
		RunID: childRunID, ReportID: reportID, Capability: capability,
		Status: agentapi.DelegationCompleted, Completeness: agentapi.DelegationComplete,
		Summary: "completed by durable worker",
	}
	raw, _ := json.Marshal(report)
	return agentrun.DelegationArtifact{
		ID: stableID("artifact", reportID), RunID: childRunID,
		Kind:        agentrun.DelegationReportArtifactKind,
		Schema:      agentapi.SchemaRef{ID: "delegation.report", Version: 1},
		ContentHash: hashBytes(raw), Content: raw,
	}
}

func TestExecutorQueuedWorkerSettlementIsReplayedByParent(t *testing.T) {
	persistence := newExecutorPersistence()
	queue := newScriptedExecutorQueue()
	queue.claimStarted = make(chan struct{})
	queue.claimErr = sql.ErrNoRows
	var runtimeCalls int
	executor := newExecutorFixture(t, executorRuntimeFunc(func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error) {
		runtimeCalls++
		return agentapi.RunResult{}, fmt.Errorf("parent must not execute a worker-owned child")
	}), persistence, nil)
	executor.queue = queue
	executor.workerOwner = "parent-dispatcher"
	executor.workerLeaseTTL = time.Second

	type executionResult struct {
		result agentapi.DelegationBatchResult
		err    error
	}
	done := make(chan executionResult, 1)
	go func() {
		result, _, err := executor.Execute(
			executorContext(t, context.Background(), 0),
			[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "queue-race"}},
		)
		done <- executionResult{result: result, err: err}
	}()
	select {
	case <-queue.claimStarted:
	case <-time.After(time.Second):
		t.Fatal("parent did not attempt the queue claim")
	}

	delegationID := stableID("del", "parent-1", "tool-call-1")
	var record agentrun.DelegationTaskRecord
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		persistence.mu.Lock()
		record = persistence.records[executorTaskKey("parent-1", delegationID, 0)]
		persistence.mu.Unlock()
		if record.ChildRunID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if record.ChildRunID == "" {
		t.Fatal("delegation admission was not persisted")
	}
	worker := newExecutorFixture(t, executorRuntimeFunc(func(_ context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
		return successfulExecutorResult(request.RunID, "queue-race-worker"), nil
	}), persistence, nil)
	worker.queue = queue
	worker.workerOwner = "worker-instance-1"
	worker.workerLeaseTTL = time.Second
	workerDone := make(chan error, 1)
	go func() {
		_, err := worker.RunOneQueuedWork(context.Background())
		workerDone <- err
	}()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not settle the queued child")
	}

	select {
	case execution := <-done:
		if execution.err != nil {
			t.Fatal(execution.err)
		}
		if execution.result.Results[0].Status != agentapi.DelegationCompleted {
			t.Fatalf("result = %+v error=%+v", execution.result.Results[0], execution.result.Results[0].Error)
		}
	case <-time.After(time.Second):
		t.Fatal("parent did not replay the worker settlement")
	}
	if runtimeCalls != 0 {
		t.Fatalf("runtime calls = %d, want 0", runtimeCalls)
	}
	persistence.mu.Lock()
	settlements := len(persistence.settlements)
	persistence.mu.Unlock()
	if settlements != 1 {
		t.Fatalf("settlements = %d, want one authoritative worker settlement", settlements)
	}
}

func TestExecutorQueuedParentCancellationDoesNotSettleWorkerOwnedTask(t *testing.T) {
	persistence := newExecutorPersistence()
	queue := newScriptedExecutorQueue()
	queue.claimStarted = make(chan struct{})
	queue.claimErr = sql.ErrNoRows
	executor := newExecutorFixture(t, executorRuntimeFunc(func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error) {
		return agentapi.RunResult{}, fmt.Errorf("child must remain queued")
	}), persistence, nil)
	executor.queue = queue
	executor.workerOwner = "parent-dispatcher"
	executor.workerLeaseTTL = time.Second

	ctx, cancel := context.WithCancel(executorContext(t, context.Background(), 0))
	done := make(chan error, 1)
	var result agentapi.DelegationBatchResult
	go func() {
		var err error
		result, _, err = executor.Execute(ctx, []agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect", Objective: "queue-cancel",
		}})
		done <- err
	}()
	select {
	case <-queue.claimStarted:
	case <-time.After(time.Second):
		t.Fatal("parent did not attempt the queue claim")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("parent did not stop waiting after cancellation")
	}
	if result.Results[0].Status != agentapi.DelegationCancelled ||
		result.Results[0].Error == nil || result.Results[0].Error.Code != ErrorParentCancelled {
		t.Fatalf("result = %+v error=%+v", result.Results[0], result.Results[0].Error)
	}
	persistence.mu.Lock()
	settlements := len(persistence.settlements)
	persistence.mu.Unlock()
	if settlements != 0 {
		t.Fatalf("settlements = %d, want 0 while worker-owned task is unresolved", settlements)
	}
}

func TestExecutorDurableContextBoundsEachOperationAndPreservesDeadline(t *testing.T) {
	executor := &Executor{durableIOTimeout: 80 * time.Millisecond}
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	durableCtx, stop := executor.durableContext(parent)
	defer stop()
	if err := durableCtx.Err(); err != nil {
		t.Fatalf("durable context inherited parent cancellation: %v", err)
	}
	deadline, ok := durableCtx.Deadline()
	if !ok {
		t.Fatal("durable context is missing I/O deadline")
	}
	if remaining := deadline.Sub(started); remaining <= 0 || remaining > 150*time.Millisecond {
		t.Fatalf("durable I/O deadline remaining = %v", remaining)
	}

	requestDeadline := time.Now().Add(20 * time.Millisecond)
	requestCtx, cancelRequest := context.WithDeadline(context.Background(), requestDeadline)
	defer cancelRequest()
	boundedCtx, stopBounded := executor.durableContext(requestCtx)
	defer stopBounded()
	boundedDeadline, ok := boundedCtx.Deadline()
	if !ok || !boundedDeadline.Equal(requestDeadline) {
		t.Fatalf("deadline = %v, want request deadline %v", boundedDeadline, requestDeadline)
	}
}

func TestExecutorCleanupContextAllowsBoundedTerminalWriteAfterDeadline(t *testing.T) {
	executor := &Executor{durableIOTimeout: 50 * time.Millisecond}
	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if !errors.Is(expiredCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("parent err = %v", expiredCtx.Err())
	}

	cleanupCtx, stop := executor.cleanupContext(expiredCtx)
	defer stop()
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context inherited expired request deadline: %v", err)
	}
	deadline, ok := cleanupCtx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 100*time.Millisecond {
		t.Fatalf("cleanup deadline = %v", deadline)
	}
}

type atomicScriptedExecutorQueue struct {
	*scriptedExecutorQueue
	atomicCalls    int
	enqueueCalls   int
	claimByIDCalls int
}

func newAtomicScriptedExecutorQueue() *atomicScriptedExecutorQueue {
	return &atomicScriptedExecutorQueue{scriptedExecutorQueue: newScriptedExecutorQueue()}
}

func (queue *atomicScriptedExecutorQueue) EnqueueWorkItem(ctx context.Context, item agentrun.WorkItem) error {
	queue.mu.Lock()
	queue.enqueueCalls++
	queue.mu.Unlock()
	return queue.scriptedExecutorQueue.EnqueueWorkItem(ctx, item)
}

func (queue *atomicScriptedExecutorQueue) EnqueueAndClaimWorkItem(
	_ context.Context,
	item agentrun.WorkItem,
	owner string,
	now time.Time,
	ttl time.Duration,
) (agentrun.WorkItem, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.atomicCalls++
	if existing, ok := queue.items[item.WorkID]; ok {
		if existing.RunID != item.RunID || existing.ParentRunID != item.ParentRunID ||
			existing.DelegationID != item.DelegationID || existing.TaskIndex != item.TaskIndex ||
			existing.AttemptNo != item.AttemptNo || existing.Kind != item.Kind ||
			!bytes.Equal(existing.Payload, item.Payload) {
			return agentrun.WorkItem{}, fmt.Errorf("work item conflict")
		}
		item = existing
	} else {
		queue.items[item.WorkID] = item
	}
	if item.State != agentrun.WorkReady {
		return agentrun.WorkItem{}, sql.ErrNoRows
	}
	item.State = agentrun.WorkRunning
	item.LeaseOwner = owner
	item.LeaseFence++
	item.LeaseExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
	item.AttemptCount++
	queue.items[item.WorkID] = item
	return item, nil
}

func (queue *atomicScriptedExecutorQueue) ClaimWorkItemByID(
	ctx context.Context,
	workID, owner string,
	now time.Time,
	ttl time.Duration,
) (agentrun.WorkItem, error) {
	queue.mu.Lock()
	queue.claimByIDCalls++
	queue.mu.Unlock()
	return queue.scriptedExecutorQueue.ClaimWorkItemByID(ctx, workID, owner, now, ttl)
}

func TestExecutorUsesAtomicQueueDispatchWithoutParentClaimPolling(t *testing.T) {
	persistence := newExecutorPersistence()
	queue := newAtomicScriptedExecutorQueue()
	var runtimeCalls int
	executor := newExecutorFixture(t, executorRuntimeFunc(func(_ context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
		runtimeCalls++
		return successfulExecutorResult(request.RunID, "atomic queue child"), nil
	}), persistence, nil)
	executor.queue = queue
	executor.workerOwner = "parent-dispatcher"
	executor.workerLeaseTTL = time.Second

	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "atomic queue"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("result = %+v", result.Results)
	}
	if runtimeCalls != 1 {
		t.Fatalf("runtime calls = %d, want 1", runtimeCalls)
	}

	queue.mu.Lock()
	atomicCalls := queue.atomicCalls
	enqueueCalls := queue.enqueueCalls
	claimByIDCalls := queue.claimByIDCalls
	var item agentrun.WorkItem
	for _, candidate := range queue.items {
		item = candidate
		break
	}
	queue.mu.Unlock()
	if atomicCalls != 1 || enqueueCalls != 0 || claimByIDCalls != 0 {
		t.Fatalf("queue calls: atomic=%d enqueue=%d claim_by_id=%d; want 1/0/0", atomicCalls, enqueueCalls, claimByIDCalls)
	}
	if item.State != agentrun.WorkSucceeded {
		t.Fatalf("queue item = %#v", item)
	}
	persistence.mu.Lock()
	settlements := len(persistence.settlements)
	persistence.mu.Unlock()
	if settlements != 1 {
		t.Fatalf("settlements = %d, want 1", settlements)
	}
}

func seedDelegationRecordForPoll(
	persistence *executorPersistence,
	parentRunID, delegationID string,
	taskIndex int,
	record agentrun.DelegationTaskRecord,
	artifact *agentrun.DelegationArtifact,
) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.records[executorTaskKey(parentRunID, delegationID, taskIndex)] = record
	if artifact != nil {
		persistence.artifacts[artifact.ID] = *artifact
	}
}

func TestExecutorDispatchWithoutQueueReturnsImmediately(t *testing.T) {
	persistence := newExecutorPersistence()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		ctx context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return successfulExecutorResult(request.RunID, "async-child"), nil
		case <-ctx.Done():
			return agentapi.RunResult{}, ctx.Err()
		}
	}), persistence, nil)

	dispatch, err := executor.Dispatch(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "async"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Tasks) != 1 || dispatch.Tasks[0].Status != agentapi.DelegationRunning {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	// Dispatch must not wait for the child. It returns while the runtime is
	// still blocked; if it blocked synchronously, this test would hang here.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("child runtime did not start on the background goroutine")
	}
	select {
	case <-release:
		t.Fatal("runtime was released before Dispatch returned; test misconfigured")
	default:
	}
	close(release)

	// Wait for settlement so the background goroutine finishes cleanly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		persistence.mu.Lock()
		record := persistence.records[executorTaskKey("parent-1", dispatch.DelegationID, 0)]
		settled := record.SettledUsage != nil
		persistence.mu.Unlock()
		if settled {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestExecutorDispatchWithQueueDoesNotExecuteInline(t *testing.T) {
	persistence := newExecutorPersistence()
	queue := newScriptedExecutorQueue()
	var runtimeCalls int
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		runtimeCalls++
		return successfulExecutorResult(request.RunID, "queued-child"), nil
	}), persistence, nil)
	executor.queue = queue
	executor.workerOwner = "parent-dispatcher"
	executor.workerLeaseTTL = time.Second

	dispatch, err := executor.Dispatch(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "queue-dispatch"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Tasks) != 1 || dispatch.Tasks[0].Status != agentapi.DelegationRunning {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	if runtimeCalls != 0 {
		t.Fatalf("runtime calls = %d, want 0 (a durable worker owns the child)", runtimeCalls)
	}
	queue.mu.Lock()
	items := len(queue.items)
	queue.mu.Unlock()
	if items != 1 {
		t.Fatalf("queue items = %d, want 1 enqueued work item", items)
	}
}

func TestExecutorPollProjectsRejectedRunningAndCompleted(t *testing.T) {
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		t.Fatal("Poll must never invoke the child runtime")
		return agentapi.RunResult{}, nil
	}), persistence, nil)

	const (
		parentRunID  = "parent-1"
		delegationID = "del-poll"
		capability   = "knowledge.code.inspect"
	)

	// running: admitted but no settled usage.
	seedDelegationRecordForPoll(persistence, parentRunID, delegationID, 0, agentrun.DelegationTaskRecord{
		ParentRunID: parentRunID, DelegationID: delegationID, TaskIndex: 0,
		ChildRunID: "child-running", Capability: agentapi.CapabilityRef{ID: capability},
		Admitted: true,
	}, nil)

	// rejected: not admitted with a rejection code.
	seedDelegationRecordForPoll(persistence, parentRunID, delegationID, 1, agentrun.DelegationTaskRecord{
		ParentRunID: parentRunID, DelegationID: delegationID, TaskIndex: 1,
		ChildRunID: "child-rejected", Capability: agentapi.CapabilityRef{ID: capability},
		RejectionCode: ErrorDepthExceeded,
	}, nil)

	// completed: settled with a durable report artifact.
	completedChild := "child-done"
	artifact := queuedReportArtifact(completedChild, capability)
	usage := agentapi.Usage{TotalTokens: 120}
	seedDelegationRecordForPoll(persistence, parentRunID, delegationID, 2, agentrun.DelegationTaskRecord{
		ParentRunID: parentRunID, DelegationID: delegationID, TaskIndex: 2,
		ChildRunID: completedChild, Capability: agentapi.CapabilityRef{ID: capability},
		Admitted: true, SettledUsage: &usage, ReportArtifactID: artifact.ID,
	}, &artifact)

	dispatch, err := executor.Poll(executorContext(t, context.Background(), 0), delegationID)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.DelegationID != delegationID {
		t.Fatalf("delegation id = %q, want %q", dispatch.DelegationID, delegationID)
	}
	if len(dispatch.Tasks) != 3 {
		t.Fatalf("tasks = %+v, want 3", dispatch.Tasks)
	}
	if dispatch.Tasks[0].Status != agentapi.DelegationRunning {
		t.Fatalf("task 0 = %+v, want running", dispatch.Tasks[0])
	}
	if dispatch.Tasks[1].Status != agentapi.DelegationRejected {
		t.Fatalf("task 1 = %+v, want rejected", dispatch.Tasks[1])
	}
	if dispatch.Tasks[2].Status != agentapi.DelegationCompleted ||
		dispatch.Tasks[2].Report == nil ||
		dispatch.Tasks[2].Report.Summary != "completed by durable worker" {
		t.Fatalf("task 2 = %+v, want completed report", dispatch.Tasks[2])
	}
}

func TestExecutorStatusToolReturnsBoundedResult(t *testing.T) {
	persistence := newExecutorPersistence()
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		t.Fatal("status tool must never invoke the child runtime")
		return agentapi.RunResult{}, nil
	}), persistence, nil)
	statusTool := executor.StatusTool()

	// Without a parent context the poll must fail cleanly, not panic or block.
	if _, err := statusTool.Handler.Execute(context.Background(), tool.Arguments{
		"delegation_id": "del-status",
	}); err == nil {
		t.Fatal("status tool did not reject a context without a delegation parent")
	}

	const (
		parentRunID  = "parent-1"
		delegationID = "del-status"
		capability   = "knowledge.code.inspect"
	)
	childID := "child-status"
	artifact := queuedReportArtifact(childID, capability)
	usage := agentapi.Usage{TotalTokens: 50}
	seedDelegationRecordForPoll(persistence, parentRunID, delegationID, 0, agentrun.DelegationTaskRecord{
		ParentRunID: parentRunID, DelegationID: delegationID, TaskIndex: 0,
		ChildRunID: childID, Capability: agentapi.CapabilityRef{ID: capability},
		Admitted: true, SettledUsage: &usage, ReportArtifactID: artifact.ID,
	}, &artifact)

	result, err := statusTool.Handler.Execute(executorContext(t, context.Background(), 0), tool.Arguments{
		"delegation_id": delegationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var dispatch agentapi.DelegationDispatchResult
	if err := json.Unmarshal([]byte(result.Content), &dispatch); err != nil {
		t.Fatalf("status content = %q: %v", result.Content, err)
	}
	if dispatch.DelegationID != delegationID {
		t.Fatalf("dispatch = %+v, want delegation id %q", dispatch, delegationID)
	}
	if len(dispatch.Tasks) != 1 || dispatch.Tasks[0].Status != agentapi.DelegationCompleted {
		t.Fatalf("dispatch = %+v, want one completed task", dispatch)
	}
	if len(result.AnswerContract.Delegations) != 1 ||
		result.AnswerContract.Delegations[0].DelegationID != delegationID {
		t.Fatalf("answer contract delegations = %#v, want the polled delegation id", result.AnswerContract.Delegations)
	}
}

func TestExecutorChildDeadlineCapsAtAnswerDeadlineNotRunDeadline(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns:        3,
		MaxChildToolCalls:    4,
		MaxChildOutputTokens: 128,
		ChildTimeout:         time.Minute,
	}}
	runDeadline := time.Now().UTC().Add(5 * time.Minute)
	answerDeadline := time.Now().UTC().Add(childAnswerDeadlineSafety + 2*time.Second)
	parent := ParentContext{
		Limits:         agentapi.RunLimits{Deadline: runDeadline},
		AnswerDeadline: answerDeadline,
	}
	limits, err := executor.childLimits(parent, agentapi.Definition{
		Model:  agentapi.ModelPolicy{MaxOutputTokens: 256},
		Budget: agentapi.BudgetPolicy{Timeout: time.Minute, MaxSteps: 8, MaxToolCalls: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !limits.Deadline.Before(answerDeadline) {
		t.Fatalf("child deadline %s must precede answer deadline %s", limits.Deadline, answerDeadline)
	}
	if !limits.Deadline.Before(runDeadline) {
		t.Fatalf("child deadline %s must not inherit the parent run deadline %s", limits.Deadline, runDeadline)
	}
	if remaining := time.Until(limits.Deadline); remaining > childAnswerDeadlineSafety+2*time.Second {
		t.Fatalf("child deadline %s is not capped to the answer window; remaining=%s", limits.Deadline, remaining)
	}
}

func TestExecutorDispatchWithAtomicQueueSettlesAndPollsReport(t *testing.T) {
	persistence := newExecutorPersistence()
	queue := newAtomicScriptedExecutorQueue()
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return successfulExecutorResult(request.RunID, "atomic-child"), nil
	}), persistence, nil)
	executor.queue = queue
	executor.workerOwner = "parent-dispatcher"
	executor.workerLeaseTTL = time.Second

	const childCount = 3
	tasks := make([]agentapi.DelegationTask, 0, childCount)
	for index := 0; index < childCount; index++ {
		tasks = append(tasks, agentapi.DelegationTask{
			Capability: "knowledge.code.inspect",
			Objective:  "atomic-dispatch-" + fmt.Sprintf("%d", index),
		})
	}
	dispatch, err := executor.Dispatch(executorContext(t, context.Background(), 0), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Tasks) != childCount {
		t.Fatalf("dispatch tasks = %d, want %d", len(dispatch.Tasks), childCount)
	}
	for _, task := range dispatch.Tasks {
		if task.Status != agentapi.DelegationRunning {
			t.Fatalf("dispatch task = %+v, want running on admission", task)
		}
	}

	// The atomic queue must actually claim and run each child. Wait until every
	// child has settled a durable report artifact.
	deadline := time.Now().Add(2 * time.Second)
	for {
		persistence.mu.Lock()
		settlements := len(persistence.settlements)
		persistence.mu.Unlock()
		if settlements == childCount {
			break
		}
		if time.Now().After(deadline) {
			persistence.mu.Lock()
			n := len(persistence.settlements)
			persistence.mu.Unlock()
			t.Fatalf("settlements = %d, want %d (children never settled)", n, childCount)
		}
		time.Sleep(5 * time.Millisecond)
	}

	polled, err := executor.Poll(executorContext(t, context.Background(), 0), dispatch.DelegationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(polled.Tasks) != childCount {
		t.Fatalf("polled tasks = %d, want %d", len(polled.Tasks), childCount)
	}
	for _, task := range polled.Tasks {
		if task.Status != agentapi.DelegationCompleted || task.Report == nil {
			t.Fatalf("polled task = %+v, want completed with report", task)
		}
	}
}

func TestNormalizePolicyDefaultsBatchTimeoutToChildTimeout(t *testing.T) {
	policy := agentapi.DelegationPolicy{
		MaxDepth: 1, MaxChildren: 2, MaxConcurrent: 1,
		MaxChildTurns: 2, MaxChildToolCalls: 2,
		MaxChildOutputTokens: 128,
		MaxReportTokens:      512, MaxTotalTokens: 1024,
		ParentAnswerReserve: 64, ChildTimeout: 3 * time.Second,
	}
	got, err := normalizePolicy(policy)
	if err != nil {
		t.Fatalf("normalizePolicy: %v", err)
	}
	if got.BatchTimeout != policy.ChildTimeout {
		t.Fatalf("batch timeout = %s, want child timeout %s", got.BatchTimeout, policy.ChildTimeout)
	}
}

func TestNormalizePolicyRejectsChildTimeoutLongerThanBatch(t *testing.T) {
	policy := agentapi.DelegationPolicy{
		MaxDepth: 1, MaxChildren: 2, MaxConcurrent: 1,
		MaxChildTurns: 2, MaxChildToolCalls: 2,
		MaxChildOutputTokens: 128,
		MaxReportTokens:      512, MaxTotalTokens: 1024,
		ParentAnswerReserve: 64, BatchTimeout: time.Second,
		ChildTimeout: 2 * time.Second,
	}
	if _, err := normalizePolicy(policy); err == nil || !strings.Contains(err.Error(), "policy limits") {
		t.Fatalf("normalizePolicy error = %v, want invalid policy limits", err)
	}
}

func TestBatchDeadlineUsesEarliestBatchAnswerAndContextDeadline(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{BatchTimeout: time.Minute, ChildTimeout: 20 * time.Second}}
	now := time.Now()
	parent := ParentContext{AnswerDeadline: now.Add(40 * time.Second)}
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(15*time.Second))
	defer cancel()

	got := executor.batchDeadline(ctx, parent)
	if got.After(now.Add(15 * time.Second)) {
		t.Fatalf("batch deadline %s exceeds context deadline", got)
	}
	if got.After(parent.AnswerDeadline.Add(-childAnswerDeadlineSafety)) {
		t.Fatalf("batch deadline %s exceeds answer reserve boundary", got)
	}
	if got.Before(now) {
		t.Fatalf("batch deadline %s is already expired", got)
	}
}

func TestChildDeadlineShortBatchWindowKeepsUsableSafetyMargin(t *testing.T) {
	now := time.Now().UTC()
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns: 2, MaxChildToolCalls: 2,
		MaxChildOutputTokens: 128,
		ChildTimeout:         time.Second, BatchTimeout: 200 * time.Millisecond,
	}}
	parent := ParentContext{BatchDeadline: now.Add(200 * time.Millisecond)}
	limits, err := executor.childLimitsAt(parent, agentapi.Definition{
		Model:  agentapi.ModelPolicy{MaxOutputTokens: 128},
		Budget: agentapi.BudgetPolicy{Timeout: time.Second, MaxSteps: 2, MaxToolCalls: 2},
	}, now, time.Time{})
	if err != nil {
		t.Fatalf("childLimitsAt: %v", err)
	}
	if !limits.Deadline.After(now) || !limits.Deadline.Before(parent.BatchDeadline) {
		t.Fatalf("child deadline = %s, want inside (%s, %s)", limits.Deadline, now, parent.BatchDeadline)
	}
}

// TestDispatchIgnoresToolInvocationTimeoutForBatchDeadline guards the fix for
// the case where the delegate_investigation tool's own 15s invocation timeout
// leaked into ctx.Deadline() and was then mistaken for the admitted batch
// deadline, leaving children with a 5s window that is shorter than the child
// answer reserve and rejecting every child at prepare time.
func TestDispatchIgnoresToolInvocationTimeoutForBatchDeadline(t *testing.T) {
	persistence := newExecutorPersistence()
	childDeadline := make(chan time.Time, 1)
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		ctx context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		select {
		case childDeadline <- request.Limits.Deadline:
		default:
		}
		return successfulExecutorResult(request.RunID, "child"), nil
	}), persistence, func(policy *agentapi.DelegationPolicy) {
		// The real child window is far larger than the tool's 15s timeout.
		policy.BatchTimeout = 60 * time.Second
		policy.ChildTimeout = 60 * time.Second
	})

	// Bump the child definition timeout above the 15s tool timeout so the
	// child window is governed by the batch deadline, not the tiny fixture
	// definition budget (2s), which would otherwise mask the regression.
	defs := executor.definitions.(executorDefinitionResolver)
	ref := agentapi.DefinitionRef{ID: "delegation.investigator", Version: 1}
	d := defs[ref]
	d.Budget.Timeout = 60 * time.Second
	defs[ref] = d

	// Simulate the tool executor wrapping the handler ctx with its 15s timeout.
	base := executorContext(t, context.Background(), 0)
	ctx, cancel := context.WithTimeout(base, 15*time.Second)
	defer cancel()

	dispatch, err := executor.Dispatch(
		ctx,
		[]agentapi.DelegationTask{{Capability: "knowledge.code.inspect", Objective: "async"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1", dispatch.Tasks)
	}

	select {
	case deadline := <-childDeadline:
		// The child deadline must reflect the batch window (60s minus safety),
		// not the 15s tool timeout minus safety (5s). Assert it stays well above
		// the 15s child answer reserve that would otherwise reject every child.
		if remaining := time.Until(deadline); remaining < 20*time.Second {
			t.Fatalf("child deadline = %s (remaining %s), want well above the 15s tool timeout", deadline, remaining)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child runtime was not invoked")
	}
}

func TestApplyAttemptTokenLimitsSoftOutputOverrun(t *testing.T) {
	result := agentapi.RunResult{
		Status: agentapi.RunSucceeded,
		Usage:  agentapi.Usage{InputTokens: 100, OutputTokens: 1001},
	}
	task := preparedTask{outputTokens: 1000}
	applyAttemptTokenLimits(&result, task)
	if result.Status != agentapi.RunSucceeded {
		t.Fatalf("status = %s, want succeeded for 1-token overrun", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorChildOutputSoftOverrun {
		t.Fatalf("error = %+v, want soft overrun code", result.Error)
	}
}

func TestApplyAttemptTokenLimitsHardOutputOverrun(t *testing.T) {
	result := agentapi.RunResult{
		Status: agentapi.RunSucceeded,
		Usage:  agentapi.Usage{InputTokens: 100, OutputTokens: 2000},
	}
	task := preparedTask{outputTokens: 1000}
	applyAttemptTokenLimits(&result, task)
	if result.Status != agentapi.RunFailed {
		t.Fatalf("status = %s, want failed for large overrun", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorChildOutputLimit {
		t.Fatalf("error = %+v, want hard output limit code", result.Error)
	}
}

func TestApplyAttemptTokenLimitsSoftOverrunDoesNotMaskFailure(t *testing.T) {
	result := agentapi.RunResult{
		Status: agentapi.RunFailed,
		Error:  &agentapi.RunError{Code: ErrorChildExecution, Message: "boom"},
		Usage:  agentapi.Usage{InputTokens: 100, OutputTokens: 1001},
	}
	task := preparedTask{outputTokens: 1000}
	applyAttemptTokenLimits(&result, task)
	if result.Status != agentapi.RunFailed {
		t.Fatalf("status = %s, want failed", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorChildExecution {
		t.Fatalf("error = %+v, want original execution error", result.Error)
	}
}

func partialInvestigationResult(
	runID string,
	unresolved []string,
	openHops []string,
) agentapi.RunResult {
	output, _ := json.Marshal(investigationOutput{
		Summary:         "partial evidence collected",
		UnresolvedGoals: unresolved,
	})
	if len(openHops) > 0 {
		output, _ = json.Marshal(investigationOutput{
			Summary:         "partial evidence collected",
			UnresolvedGoals: unresolved,
			Flow: &agentapi.FlowIR{
				Subject:    "test flow",
				Status:     "partial",
				Confidence: "low",
				OpenHops:   openHops,
			},
		})
	}
	return agentapi.RunResult{
		RunID:  runID,
		Status: agentapi.RunSucceeded,
		Output: output,
		Evidence: agentapi.EvidenceSummary{
			Status: "partial", ToolCallCount: 1, ResultCount: 1,
		},
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "code", Target: "partial.go", ContentHash: "partial-hash",
		}},
		Usage: agentapi.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
	}
}

func TestExecutorGapChaseDisabledByDefault(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			return partialInvestigationResult(
				request.RunID, []string{"core_flow"}, []string{"worker -> database"},
			), nil
		}),
		persistence,
		nil,
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect flow gaps",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runtime calls = %d, want 1 (gap chase disabled)", calls)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v", result.Results)
	}
	report := result.Results[0]
	if report.GapChase != agentapi.DelegationGapChaseUnavailable {
		t.Fatalf("gap chase = %s, want unavailable when disabled", report.GapChase)
	}
	if len(report.UnresolvedGoals) != 1 || report.UnresolvedGoals[0] != "core_flow" {
		t.Fatalf("unresolved goals = %v, want preserved core_flow", report.UnresolvedGoals)
	}
}

func TestExecutorGapChaseRetrievesResolvedGoal(t *testing.T) {
	persistence := newExecutorPersistence()
	var (
		mu    sync.Mutex
		calls int
	)
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()
			if call == 1 {
				return partialInvestigationResult(
					request.RunID,
					[]string{"core_flow", "data_and_state"},
					nil,
				), nil
			}
			// Bounded continuation resolves core_flow and leaves data_and_state.
			output, _ := json.Marshal(investigationOutput{
				Summary:         "gap chase continuation",
				CoveredGoals:    []string{"core_flow"},
				UnresolvedGoals: []string{"data_and_state"},
			})
			return agentapi.RunResult{
				RunID:  request.RunID,
				Status: agentapi.RunSucceeded,
				Output: output,
				Evidence: agentapi.EvidenceSummary{
					Status: "complete", ToolCallCount: 1, ResultCount: 1,
				},
				EvidenceUnits: []tool.EvidenceUnit{{
					SourceKind: "code", Target: "core_flow.go", ContentHash: "core-flow-hash",
				}},
				Usage: agentapi.Usage{
					InputTokens: 5, OutputTokens: 3, TotalTokens: 8,
				},
			}, nil
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxGapChaseRounds = 1
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect flow gaps",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("runtime calls = %d, want 2 (initial + chase)", gotCalls)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v", result.Results)
	}
	report := result.Results[0]
	if report.GapChase != agentapi.DelegationGapChaseRetrieved {
		t.Fatalf("gap chase = %s, want retrieved", report.GapChase)
	}
	if !containsString(report.CoveredGoals, "core_flow") {
		t.Fatalf("covered goals = %v, want core_flow moved in", report.CoveredGoals)
	}
	if containsString(report.UnresolvedGoals, "core_flow") {
		t.Fatalf("unresolved goals = %v, core_flow should be removed", report.UnresolvedGoals)
	}
	if !containsString(report.UnresolvedGoals, "data_and_state") {
		t.Fatalf("unresolved goals = %v, want data_and_state kept", report.UnresolvedGoals)
	}
}

func TestExecutorGapChaseUnavailableWhenChaseReturnsNoResolution(t *testing.T) {
	persistence := newExecutorPersistence()
	var (
		mu    sync.Mutex
		calls int
	)
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			// Both the initial and the chase return the same unresolved gap.
			return partialInvestigationResult(
				request.RunID, []string{"core_flow"}, nil,
			), nil
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxGapChaseRounds = 1
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect flow gaps",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("runtime calls = %d, want 2 (initial + one chase, no retry)", gotCalls)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v", result.Results)
	}
	if result.Results[0].GapChase != agentapi.DelegationGapChaseUnavailable {
		t.Fatalf("gap chase = %s, want unavailable", result.Results[0].GapChase)
	}
}

func TestExecutorGapChaseNoneWhenNoGaps(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			return partialInvestigationResult(request.RunID, nil, nil), nil
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxGapChaseRounds = 1
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect flow gaps",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runtime calls = %d, want 1 (no gaps to chase)", calls)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v", result.Results)
	}
	if result.Results[0].GapChase != agentapi.DelegationGapChaseNone {
		t.Fatalf("gap chase = %s, want none", result.Results[0].GapChase)
	}
}

func TestExecutorGapChaseValueGateSkipsOpenHopsOnly(t *testing.T) {
	persistence := newExecutorPersistence()
	calls := 0
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			calls++
			// Partial report with only an open hop, no unresolved evidence goals.
			return partialInvestigationResult(request.RunID, nil, []string{"worker -> database"}), nil
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxGapChaseRounds = 1
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{{
			Capability: "knowledge.code.inspect",
			Objective:  "inspect flow gaps",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runtime calls = %d, want 1 (open hop alone does not trigger chase)", calls)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v", result.Results)
	}
	if result.Results[0].GapChase != agentapi.DelegationGapChaseNone {
		t.Fatalf("gap chase = %s, want none (value gate rejects open-hop-only gaps)", result.Results[0].GapChase)
	}
}

func TestExecutorGapChaseQuotaGateLimitsOneChasePerBatch(t *testing.T) {
	persistence := newExecutorPersistence()
	var (
		mu    sync.Mutex
		calls int
	)
	executor := newExecutorFixture(
		t,
		executorRuntimeFunc(func(
			_ context.Context,
			request agentapi.RunRequest,
		) (agentapi.RunResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			// Both children return partial reports with an unresolved goal.
			return partialInvestigationResult(request.RunID, []string{"core_flow"}, nil), nil
		}),
		persistence,
		func(policy *agentapi.DelegationPolicy) {
			policy.MaxGapChaseRounds = 1
			policy.MaxGapChasePerBatch = 1
		},
	)
	result, _, err := executor.Execute(
		executorContext(t, context.Background(), 0),
		[]agentapi.DelegationTask{
			{Capability: "knowledge.code.inspect", Objective: "inspect flow gaps a"},
			{Capability: "knowledge.code.inspect", Objective: "inspect flow gaps b"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	// Two initial children plus exactly one admitted chase: the second partial
	// child must be denied by the per-batch quota gate.
	if gotCalls != 3 {
		t.Fatalf("runtime calls = %d, want 3 (two initial + one chase)", gotCalls)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %+v", result.Results)
	}
	var chased, denied int
	for _, report := range result.Results {
		switch report.GapChase {
		case agentapi.DelegationGapChaseTriggered, agentapi.DelegationGapChaseRetrieved, agentapi.DelegationGapChaseUnavailable:
			chased++
		case agentapi.DelegationGapChaseBudgetExhausted:
			denied++
		}
	}
	if chased != 1 {
		t.Fatalf("chased reports = %d, want exactly 1 (quota gate)", chased)
	}
	if denied != 1 {
		t.Fatalf("quota-denied reports = %d, want exactly 1", denied)
	}
}
