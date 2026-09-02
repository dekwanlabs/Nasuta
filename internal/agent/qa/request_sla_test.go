package qa

import (
	"context"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestInitializePreparationUsesRequestEntryDeadline(t *testing.T) {
	definition := testQADefinition(t, func(definition *agentapi.Definition) {
		definition.Budget.Timeout = 2 * time.Second
	})
	startedAt := time.Now().Add(-700 * time.Millisecond)
	svc := &Service{
		agentRef: agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		definitions: definitionResolverFunc(func(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
			return definition, nil
		}),
		runtimeTools: requestSLAToolSource{},
	}

	prepared, err := svc.initializePreparation(context.Background(), Request{
		Question: "question", RunID: "run-sla",
	}, startedAt)
	if err != nil {
		t.Fatalf("initializePreparation: %v", err)
	}
	defer prepared.closeTrace()

	wantDeadline := startedAt.Add(definition.Budget.Timeout)
	if delta := prepared.requestDeadline.Sub(wantDeadline); delta > 20*time.Millisecond || delta < -20*time.Millisecond {
		t.Fatalf("request deadline = %s, want close to %s", prepared.requestDeadline, wantDeadline)
	}
	limits := svc.parentRunLimits(prepared, definition)
	if !limits.Deadline.Equal(prepared.requestDeadline) {
		t.Fatalf("parent deadline = %s, want request deadline %s", limits.Deadline, prepared.requestDeadline)
	}
}

func TestInitializePreparationDoesNotExtendEarlierCallerDeadline(t *testing.T) {
	definition := testQADefinition(t, func(definition *agentapi.Definition) {
		definition.Budget.Timeout = 10 * time.Second
	})
	callerDeadline := time.Now().Add(150 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	svc := &Service{
		agentRef: agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		definitions: definitionResolverFunc(func(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
			return definition, nil
		}),
		runtimeTools: requestSLAToolSource{},
	}

	prepared, err := svc.initializePreparation(ctx, Request{
		Question: "question", RunID: "run-sla-caller",
	}, time.Now())
	if err != nil {
		t.Fatalf("initializePreparation: %v", err)
	}
	defer prepared.closeTrace()

	deadline, ok := prepared.ctx.Deadline()
	if !ok {
		t.Fatal("prepared context has no deadline")
	}
	if deadline.After(callerDeadline.Add(20 * time.Millisecond)) {
		t.Fatalf("prepared deadline = %s extends caller deadline %s", deadline, callerDeadline)
	}
	if !prepared.requestDeadline.Equal(deadline) {
		t.Fatalf("request deadline = %s, context deadline = %s", prepared.requestDeadline, deadline)
	}
}

func TestParentRunLimitsFallbackDeadlineIsOnlyForDirectCallers(t *testing.T) {
	definition := testQADefinition(t, nil)
	before := time.Now()
	prepared := &preparation{}
	limits := (&Service{}).parentRunLimits(prepared, definition)
	after := time.Now().Add(definition.Budget.Timeout)
	if limits.Deadline.Before(before.Add(definition.Budget.Timeout-20*time.Millisecond)) ||
		limits.Deadline.After(after.Add(20*time.Millisecond)) {
		t.Fatalf("fallback deadline = %s outside expected range [%s,%s]", limits.Deadline, before.Add(definition.Budget.Timeout-20*time.Millisecond), after.Add(20*time.Millisecond))
	}
}

type requestSLAToolSource struct{}

func (requestSLAToolSource) ToolsFor(ToolPolicy) ScenarioToolSet {
	return requestSLAToolSet{}
}

type requestSLAToolSet struct{}

func (requestSLAToolSet) Tools() []Tool { return nil }

func (requestSLAToolSet) Get(toolID tool.ToolID) (tool.Tool, bool) {
	return tool.Tool{}, false
}

func (requestSLAToolSet) Execute(context.Context, tool.ToolID, tool.Arguments) (tool.Result, error) {
	return tool.Result{}, nil
}
