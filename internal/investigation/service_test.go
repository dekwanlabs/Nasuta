package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
)

func TestServiceMapsFixedWorkflowRequestAndStructuredResult(t *testing.T) {
	executor := &recordingExecutor{result: agentworkflow.Result{
		RunID: "workflow_1",
		Output: agentworkflow.Handoff{Payload: json.RawMessage(`{
			"answer":"Use the indexed call path.",
			"citations":[{"claim":"checkout calls inventory","evidence":[{"kind":"call","reference":"Checkout.Place","summary":"calls inventory"}]}],
			"limitations":["live runtime evidence unavailable"]
		}`)},
	}}
	service := New(executor)
	actor := agentapi.Actor{UserID: 17, TenantID: "tenant-a"}
	result, err := service.Run(t.Context(), "Why does checkout fail?", actor)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != "workflow_1" || result.Answer.Answer != "Use the indexed call path." ||
		len(result.Answer.Citations) != 1 || len(result.Answer.Limitations) != 1 {
		t.Fatalf("result = %+v", result)
	}
	request := executor.request
	if request.Workflow.ID != agentworkflow.DelegatedInvestigationID || request.Workflow.Version != 0 ||
		request.Actor != actor || request.Scenario != Scenario ||
		!reflect.DeepEqual(request.ActorPermissions.Scopes, []string{"knowledge.read"}) ||
		!reflect.DeepEqual(request.ScenarioPermissions.Scopes, []string{"knowledge.read"}) ||
		string(request.Input) != `{"question":"Why does checkout fail?"}` {
		t.Fatalf("workflow request = %+v input=%s", request, request.Input)
	}
}

func TestServiceReportsUnavailableAndExecutionFailures(t *testing.T) {
	if _, err := (*Service)(nil).Run(t.Context(), "question", agentapi.Actor{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil service error = %v", err)
	}
	want := errors.New("workflow failed")
	service := New(&recordingExecutor{err: want})
	_, err := service.Run(t.Context(), "question", agentapi.Actor{})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "run delegated investigation") {
		t.Fatalf("execution error = %v", err)
	}
}

type recordingExecutor struct {
	request agentworkflow.ExecuteRequest
	result  agentworkflow.Result
	err     error
}

func (executor *recordingExecutor) Execute(
	_ context.Context,
	request agentworkflow.ExecuteRequest,
) (agentworkflow.Result, error) {
	executor.request = request
	return executor.result, executor.err
}
