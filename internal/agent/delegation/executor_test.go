package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
		records:   make(map[string]agentrun.DelegationTaskRecord),
		artifacts: make(map[string]agentrun.DelegationArtifact),
		evidence:  make(map[string][]tool.EvidenceUnit),
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

func executorTaskKey(parentRunID, delegationID string, taskIndex int) string {
	return fmt.Sprintf("%s/%s/%d", parentRunID, delegationID, taskIndex)
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

func TestDelegateToolInheritsCallerDeadline(t *testing.T) {
	executor := newExecutorFixture(t, executorRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return agentapi.RunResult{}, nil
	}), newExecutorPersistence(), nil)
	if executor.Tool().Timeout != tool.InheritCallerDeadline {
		t.Fatalf("timeout = %s, want inherit caller deadline", executor.Tool().Timeout)
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

func TestChildLimitsClampsToolCallsToDefinitionBudget(t *testing.T) {
	executor := &Executor{policy: agentapi.DelegationPolicy{
		MaxChildTurns: 3, MaxChildToolCalls: 4,
		MaxChildInputTokens: 256, MaxChildOutputTokens: 128,
		ChildTimeout: 200 * time.Millisecond,
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
		MaxChildInputTokens: 256, MaxChildOutputTokens: 128,
		MaxReportTokens: 512, MaxTotalTokens: 4096,
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
