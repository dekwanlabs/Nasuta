package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestOrchestratorSharesCapabilityLimitAcrossRuns(t *testing.T) {
	executor := &capabilityBlockingExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), executor, nil)
	definition := capabilityWorkflow(1, 1)
	errors := make(chan error, 2)

	go func() {
		_, err := orchestrator.Run(t.Context(), definition, capabilityRunRequest("workflow_run_1"))
		errors <- err
	}()
	if started := awaitCapabilityStart(t, executor.started); started != "workflow_run_1" {
		t.Fatalf("first started run = %q", started)
	}
	go func() {
		_, err := orchestrator.Run(t.Context(), definition, capabilityRunRequest("workflow_run_2"))
		errors <- err
	}()
	select {
	case started := <-executor.started:
		t.Fatalf("run %q bypassed the shared capability limit", started)
	case <-time.After(100 * time.Millisecond):
	}

	close(executor.release)
	if started := awaitCapabilityStart(t, executor.started); started != "workflow_run_2" {
		t.Fatalf("second started run = %q", started)
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrchestratorSeparatesCapabilityVersions(t *testing.T) {
	executor := &capabilityBlockingExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), executor, nil)
	errors := make(chan error, 2)
	for version := int64(1); version <= 2; version++ {
		version := version
		go func() {
			runID := fmt.Sprintf("workflow_run_%d", version)
			_, err := orchestrator.Run(
				t.Context(),
				capabilityWorkflow(version, 1),
				capabilityRunRequest(runID),
			)
			errors <- err
		}()
	}
	started := map[string]struct{}{
		awaitCapabilityStart(t, executor.started): {},
		awaitCapabilityStart(t, executor.started): {},
	}
	if len(started) != 2 {
		t.Fatalf("started runs = %v", started)
	}
	close(executor.release)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrchestratorCapabilityWaitDoesNotOccupWorkflowSlot(t *testing.T) {
	executor := &selectiveCapabilityBlockingExecutor{
		started: make(chan string, 3),
		release: make(chan struct{}),
	}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), executor, nil)
	blocking := capabilityWorkflow(1, 1)
	blocking.Budget.MaxParallelism = 1
	errors := make(chan error, 2)

	go func() {
		_, err := orchestrator.Run(
			t.Context(),
			blocking,
			capabilityRunRequest("blocking_run"),
		)
		errors <- err
	}()
	if started := awaitCapabilityStart(t, executor.started); started != "blocking_run:review.a" {
		t.Fatalf("first started node = %q", started)
	}

	wave := parallelCapabilityWorkflow()
	go func() {
		_, err := orchestrator.Run(
			t.Context(),
			wave,
			capabilityRunRequest("wave_run"),
		)
		errors <- err
	}()
	if started := awaitCapabilityStart(t, executor.started); started != "wave_run:review.b" {
		t.Fatalf("node started while capability was occupied = %q, want free node", started)
	}

	close(executor.release)
	if started := awaitCapabilityStart(t, executor.started); started != "wave_run:review.a" {
		t.Fatalf("node started after capability release = %q, want limited node", started)
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrchestratorRejectsCapabilityLimitChange(t *testing.T) {
	orchestrator := NewOrchestrator(testSchemaRegistry(t), staticOutputExecutor{}, nil)
	if _, err := orchestrator.Run(
		t.Context(),
		capabilityWorkflow(1, 1),
		capabilityRunRequest("workflow_run_1"),
	); err != nil {
		t.Fatal(err)
	}
	_, err := orchestrator.Run(
		t.Context(),
		capabilityWorkflow(1, 2),
		capabilityRunRequest("workflow_run_2"),
	)
	if err == nil || !strings.Contains(
		err.Error(),
		`capability "knowledge.code.inspect" version 1 concurrency limit changed from 1 to 2`,
	) {
		t.Fatalf("Run error = %v, want capability limit conflict", err)
	}
}

type capabilityBlockingExecutor struct {
	started chan string
	release chan struct{}
}

func (executor *capabilityBlockingExecutor) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.started <- request.WorkflowRunID
	select {
	case <-executor.release:
	case <-ctx.Done():
		return NodeResult{}, ctx.Err()
	}
	return staticOutputExecutor{}.Execute(ctx, request)
}

type selectiveCapabilityBlockingExecutor struct {
	started chan string
	release chan struct{}
}

func (executor *selectiveCapabilityBlockingExecutor) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.started <- request.WorkflowRunID + ":" + request.Node.ID
	if request.WorkflowRunID == "blocking_run" {
		select {
		case <-executor.release:
		case <-ctx.Done():
			return NodeResult{}, ctx.Err()
		}
	}
	return staticOutputExecutor{}.Execute(ctx, request)
}

func capabilityWorkflow(
	capabilityVersion int64,
	maxConcurrency int,
) Definition {
	definition := singleNodeWorkflow()
	definition.Budget.Timeout = 2 * time.Second
	definition.Nodes[0].Capability = agentapi.CapabilityRef{
		ID:      "knowledge.code.inspect",
		Version: capabilityVersion,
	}
	definition.Nodes[0].CapabilityMaxConcurrency = maxConcurrency
	definition.Nodes[0].RestrictVisibleTools = true
	return definition
}

func parallelCapabilityWorkflow() Definition {
	definition := testWorkflow()
	definition.ID = "delivery.review.parallel"
	definition.Budget.MaxParallelism = 1
	limited := &definition.Nodes[1]
	limited.Capability = agentapi.CapabilityRef{
		ID:      "knowledge.code.inspect",
		Version: 1,
	}
	limited.CapabilityMaxConcurrency = 1
	limited.RestrictVisibleTools = true
	return definition
}

func capabilityRunRequest(runID string) RunRequest {
	return RunRequest{
		RunID: runID,
		Input: json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	}
}

func awaitCapabilityStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case runID := <-started:
		return runID
	case <-time.After(time.Second):
		t.Fatal("capability execution did not start")
		return ""
	}
}
