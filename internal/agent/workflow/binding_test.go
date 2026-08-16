package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestWorkflowBindingRegistryPublishesImmutableExactBindings(t *testing.T) {
	schemas, capabilities, workflows, capability, definition := escalationCatalogs(t, true)
	registry := NewWorkflowBindingRegistry(capabilities, workflows)
	registration := testWorkflowBindingRegistration(capability, definition)
	if err := registry.Publish([]WorkflowBindingRegistration{registration}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	resolved, resolvedCapability, resolvedWorkflow, err := registry.Resolve(
		registration.Binding.Capability,
		registration.Binding.CapabilityHash,
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Binding.ContentHash == "" ||
		resolved.Binding.Workflow.ContentHash != definition.ContentHash ||
		resolvedCapability.ContentHash != capability.ContentHash ||
		resolvedWorkflow.ContentHash != definition.ContentHash {
		t.Fatalf(
			"resolved binding=%+v capability=%+v workflow=%+v",
			resolved.Binding,
			resolvedCapability,
			resolvedWorkflow,
		)
	}
	resolved.Binding.AllowedReasons[0] = agentapi.EscalationHumanApprovalRequired
	again, _, _, err := registry.Resolve(
		registration.Binding.Capability,
		registration.Binding.CapabilityHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.Binding.AllowedReasons[0] ==
		agentapi.EscalationHumanApprovalRequired {
		t.Fatal("Resolve returned mutable binding storage")
	}

	conflict := registration
	conflict.Binding.Scenario = "qa.changed"
	conflict.Binding.ContentHash = ""
	if err := registry.Publish([]WorkflowBindingRegistration{conflict}); err == nil ||
		!strings.Contains(err.Error(), "already published") {
		t.Fatalf("immutable duplicate error = %v", err)
	}

	wrongHash := registration
	wrongHash.Binding.ID = "binding.review.other"
	wrongHash.Binding.Version = 2
	wrongHash.Binding.CapabilityHash = strings.Repeat("0", 64)
	wrongHash.Binding.ContentHash = ""
	if err := registry.Publish([]WorkflowBindingRegistration{wrongHash}); err == nil ||
		!strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("capability hash mismatch error = %v", err)
	}
	if _, err := schemas.Resolve(definition.InputSchema); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowBindingRegistryRejectsUnknownReasonAndWorkflowHash(t *testing.T) {
	_, capabilities, workflows, capability, definition := escalationCatalogs(t, true)
	registry := NewWorkflowBindingRegistry(capabilities, workflows)

	unknownReason := testWorkflowBindingRegistration(capability, definition)
	unknownReason.Binding.AllowedReasons = []agentapi.WorkflowEscalationReason{"provider_failed"}
	if err := registry.Publish([]WorkflowBindingRegistration{unknownReason}); err == nil ||
		!strings.Contains(err.Error(), "reason") {
		t.Fatalf("unknown reason error = %v", err)
	}

	wrongWorkflow := testWorkflowBindingRegistration(capability, definition)
	wrongWorkflow.Binding.Workflow.ContentHash = strings.Repeat("0", 64)
	if err := registry.Publish([]WorkflowBindingRegistration{wrongWorkflow}); err == nil ||
		!strings.Contains(err.Error(), "workflow content hash mismatch") {
		t.Fatalf("workflow hash error = %v", err)
	}
}

func TestWorkflowEscalatorIsIdempotentAndPinsStartRequest(t *testing.T) {
	escalator, starter, _, request := testWorkflowEscalator(t, true)
	first, err := escalator.Escalate(t.Context(), request)
	if err != nil {
		t.Fatalf("Escalate first: %v", err)
	}
	second, err := escalator.Escalate(t.Context(), request)
	if err != nil {
		t.Fatalf("Escalate duplicate: %v", err)
	}
	if first.Status != agentapi.EscalationAccepted ||
		second.Status != agentapi.EscalationAlreadyStarted ||
		first.WorkflowRunID != second.WorkflowRunID ||
		first.WorkflowRunID != StableWorkflowEscalationRunID(
			request.ParentRunID,
			request.RequestID,
		) {
		t.Fatalf("receipts first=%+v second=%+v", first, second)
	}
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if starter.calls != 1 {
		t.Fatalf("Start calls = %d want 1", starter.calls)
	}
	if starter.last.RunID != first.WorkflowRunID ||
		starter.last.ParentRunID != request.ParentRunID ||
		starter.last.Workflow.ID != "delivery.review" ||
		starter.last.Workflow.Version != 1 ||
		starter.last.Scenario != "qa.investigation" {
		t.Fatalf("Start request = %+v", starter.last)
	}
	if string(starter.last.Input) != `{"subject":"bounded"}` {
		t.Fatalf("Start input = %s", starter.last.Input)
	}
}

func TestWorkflowEscalatorRejectsMissingBindingReasonPermissionAndStartFailure(
	t *testing.T,
) {
	t.Run("missing binding", func(t *testing.T) {
		escalator, starter, _, request := testWorkflowEscalator(t, true)
		request.Capability.ID = "knowledge.unbound"
		request.Capability.Version = 1
		request.CapabilityHash = strings.Repeat("1", 64)
		receipt, err := escalator.Escalate(t.Context(), request)
		if WorkflowEscalationErrorCode(err) != agentapi.WorkflowUnavailable ||
			receipt.ErrorCode != agentapi.WorkflowUnavailable ||
			starter.calls != 0 {
			t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, starter.calls)
		}
		if !errors.Is(err, ErrWorkflowBindingNotFound) {
			t.Fatalf("missing binding cause = %v", err)
		}
	})

	t.Run("reason", func(t *testing.T) {
		escalator, starter, _, request := testWorkflowEscalator(t, true)
		request.Reason = agentapi.EscalationHumanApprovalRequired
		receipt, err := escalator.Escalate(t.Context(), request)
		if WorkflowEscalationErrorCode(err) != agentapi.WorkflowReasonNotAllowed ||
			receipt.ErrorCode != agentapi.WorkflowReasonNotAllowed ||
			starter.calls != 0 {
			t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, starter.calls)
		}
	})

	t.Run("permission", func(t *testing.T) {
		escalator, starter, parent, request := testWorkflowEscalator(t, false)
		parent.parent.Permissions = agentapi.PermissionPolicy{}
		receipt, err := escalator.Escalate(t.Context(), request)
		if WorkflowEscalationErrorCode(err) != agentapi.WorkflowPermissionDenied ||
			receipt.ErrorCode != agentapi.WorkflowPermissionDenied ||
			starter.calls != 0 {
			t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, starter.calls)
		}
	})

	t.Run("start failure", func(t *testing.T) {
		escalator, starter, _, request := testWorkflowEscalator(t, true)
		starter.err = errors.New("provider unavailable")
		receipt, err := escalator.Escalate(t.Context(), request)
		if WorkflowEscalationErrorCode(err) != agentapi.WorkflowStartFailed ||
			receipt.ErrorCode != agentapi.WorkflowStartFailed ||
			starter.calls != 1 {
			t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, starter.calls)
		}
		again, duplicateErr := escalator.Escalate(t.Context(), request)
		if WorkflowEscalationErrorCode(duplicateErr) != agentapi.WorkflowStartFailed ||
			again.Status != agentapi.EscalationRejected ||
			starter.calls != 1 {
			t.Fatalf(
				"duplicate receipt=%+v err=%v calls=%d",
				again,
				duplicateErr,
				starter.calls,
			)
		}
	})
}

func TestWorkflowEscalatorValidatesBoundedOwnedHandoff(t *testing.T) {
	escalator, starter, _, request := testWorkflowEscalator(t, true)
	request.ReportRefs = []string{"report_1"}
	escalator.handoffs = workflowEscalationHandoffResolverFunc(func(
		context.Context,
		agentapi.WorkflowEscalationParent,
		agentapi.WorkflowEscalationRequest,
	) (WorkflowEscalationHandoff, error) {
		return WorkflowEscalationHandoff{
			Reports: []agentapi.WorkflowEscalationReport{{
				Ref: "report_1", RunID: "child_1",
				Schema:      agentapi.SchemaRef{ID: "review.report", Version: 1},
				ContentHash: strings.Repeat("0", 64),
				Payload:     json.RawMessage(`{"node":"child"}`),
			}},
		}, nil
	})
	receipt, err := escalator.Escalate(t.Context(), request)
	if WorkflowEscalationErrorCode(err) != agentapi.WorkflowInvalidHandoff ||
		receipt.ErrorCode != agentapi.WorkflowInvalidHandoff ||
		starter.calls != 0 {
		t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, starter.calls)
	}
}

func escalationCatalogs(
	t *testing.T,
	withPermission bool,
) (
	*agentapi.SchemaRegistry,
	*agentapi.CapabilityRegistry,
	*Catalog,
	agentapi.Capability,
	Definition,
) {
	t.Helper()
	schemas := testSchemaRegistry(t)
	agents := testAgentDefinitions(t)
	workflows := NewCatalog(schemas, agents)
	definition := testWorkflow()
	if err := workflows.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	definition, err := workflows.Resolve(DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	permissions := []string(nil)
	if withPermission {
		permissions = []string{"knowledge.read"}
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	if err := capabilities.Publish([]agentapi.Capability{{
		ID: "knowledge.review.inspect", Version: 1,
		Role: agentapi.RoleInvestigator, Purpose: "Inspect review evidence.",
		InputSchema: definition.InputSchema, OutputSchema: agentapi.SchemaRef{
			ID: "review.report", Version: 1,
		},
		PermissionScope: permissions,
		Freshness:       agentapi.FreshnessStable,
		SideEffects:     agentapi.SideEffectNone,
		RetrySafe:       true,
		MaxConcurrency:  1,
		Enabled:         true,
		Agent:           agentapi.DefinitionRef{ID: "review.correctness", Version: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	capability, err := capabilities.Resolve(agentapi.CapabilityRef{
		ID: "knowledge.review.inspect", Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return schemas, capabilities, workflows, capability, definition
}

func testWorkflowBindingRegistration(
	capability agentapi.Capability,
	definition Definition,
) WorkflowBindingRegistration {
	return WorkflowBindingRegistration{
		Binding: agentapi.WorkflowBinding{
			ID: "binding.review", Version: 1,
			Capability: agentapi.CapabilityRef{
				ID: capability.ID, Version: capability.Version,
			},
			CapabilityHash: capability.ContentHash,
			AllowedReasons: []agentapi.WorkflowEscalationReason{
				agentapi.EscalationHighRiskVerificationRequired,
				agentapi.EscalationStrongTaskDependencies,
			},
			Workflow: agentapi.WorkflowDefinitionRef{
				ID:          definition.ID,
				Version:     definition.Version,
				ContentHash: definition.ContentHash,
			},
			Scenario:            "qa.investigation",
			ScenarioPermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
			InputSchema:         definition.InputSchema,
			BuilderID:           "qa.review.input",
		},
		Builder: agentapi.WorkflowEscalationInputBuilderFunc(func(
			context.Context,
			agentapi.WorkflowEscalationBuildRequest,
		) (agentapi.WorkflowEscalationBuildResult, error) {
			return agentapi.WorkflowEscalationBuildResult{
				Input: json.RawMessage(`{"subject":"bounded"}`),
			}, nil
		}),
	}
}

func testWorkflowEscalator(
	t *testing.T,
	withPermission bool,
) (
	*ServerWorkflowEscalator,
	*recordingEscalationStarter,
	*staticEscalationParentLoader,
	agentapi.WorkflowEscalationRequest,
) {
	t.Helper()
	schemas, capabilities, workflows, capability, definition := escalationCatalogs(
		t,
		withPermission,
	)
	bindings := NewWorkflowBindingRegistry(capabilities, workflows)
	if err := bindings.Publish([]WorkflowBindingRegistration{
		testWorkflowBindingRegistration(capability, definition),
	}); err != nil {
		t.Fatal(err)
	}
	starter := &recordingEscalationStarter{
		workflowHash: definition.ContentHash,
	}
	parents := &staticEscalationParentLoader{
		parent: agentapi.WorkflowEscalationParent{
			RunID: "run_parent",
			Question: "Review this change.",
			Actor: agentapi.Actor{UserID: 42},
			Permissions: agentapi.PermissionPolicy{
				Scopes: []string{"knowledge.read"},
			},
			Correlation: agentapi.Correlation{SessionID: "session_1"},
		},
	}
	escalator, err := NewWorkflowEscalator(
		bindings,
		starter,
		newMemoryEscalationStore(),
		parents,
		nil,
		schemas,
		WorkflowEscalatorConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return escalator, starter, parents, agentapi.WorkflowEscalationRequest{
		RequestID: "request_1", ParentRunID: "run_parent",
		Capability: agentapi.CapabilityRef{
			ID: capability.ID, Version: capability.Version,
		},
		CapabilityHash: capability.ContentHash,
		Reason:         agentapi.EscalationHighRiskVerificationRequired,
		Objective:      "Review bounded evidence.",
		FocusFacets:    []string{"correctness"},
	}
}

type memoryEscalationStore struct {
	mu      sync.Mutex
	records map[string]WorkflowEscalationRecord
}

func newMemoryEscalationStore() *memoryEscalationStore {
	return &memoryEscalationStore{
		records: make(map[string]WorkflowEscalationRecord),
	}
}

func (store *memoryEscalationStore) LoadWorkflowEscalation(
	_ context.Context,
	parentRunID,
	requestID string,
) (WorkflowEscalationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[parentRunID+"\x00"+requestID]
	if !ok {
		return WorkflowEscalationRecord{}, ErrNotFound
	}
	return record, nil
}

func (store *memoryEscalationStore) ReserveWorkflowEscalation(
	_ context.Context,
	record WorkflowEscalationRecord,
) (WorkflowEscalationRecord, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := record.ParentRunID + "\x00" + record.RequestID
	if existing, ok := store.records[key]; ok {
		return existing, false, nil
	}
	store.records[key] = record
	return record, true, nil
}

func (store *memoryEscalationStore) FinishWorkflowEscalation(
	_ context.Context,
	record WorkflowEscalationRecord,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := record.ParentRunID + "\x00" + record.RequestID
	if _, ok := store.records[key]; !ok {
		return ErrNotFound
	}
	store.records[key] = record
	return nil
}

type staticEscalationParentLoader struct {
	parent agentapi.WorkflowEscalationParent
	err    error
}

func (loader *staticEscalationParentLoader) LoadWorkflowEscalationParent(
	context.Context,
	string,
) (agentapi.WorkflowEscalationParent, error) {
	return loader.parent, loader.err
}

type recordingEscalationStarter struct {
	mu           sync.Mutex
	calls        int
	last         StartRequest
	err          error
	workflowHash string
	run          *RunRecord
}

func (starter *recordingEscalationStarter) Start(
	_ context.Context,
	request StartRequest,
) (*RunRecord, error) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	starter.calls++
	starter.last = request
	if starter.err != nil {
		return nil, starter.err
	}
	starter.run = &RunRecord{
		ID: request.RunID, ParentRunID: request.ParentRunID,
		WorkflowID: request.Workflow.ID, WorkflowVersion: request.Workflow.Version,
		WorkflowHash: starter.workflowHash,
		ActorUserID: request.Actor.UserID, ActorTenantID: request.Actor.TenantID,
	}
	return starter.run, nil
}

func (starter *recordingEscalationStarter) GetRun(
	context.Context,
	string,
	int64,
	bool,
) (*RunRecord, error) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if starter.run == nil {
		return nil, ErrNotFound
	}
	copy := *starter.run
	return &copy, nil
}

type workflowEscalationHandoffResolverFunc func(
	context.Context,
	agentapi.WorkflowEscalationParent,
	agentapi.WorkflowEscalationRequest,
) (WorkflowEscalationHandoff, error)

func (resolve workflowEscalationHandoffResolverFunc) ResolveWorkflowEscalationHandoff(
	ctx context.Context,
	parent agentapi.WorkflowEscalationParent,
	request agentapi.WorkflowEscalationRequest,
) (WorkflowEscalationHandoff, error) {
	return resolve(ctx, parent, request)
}
