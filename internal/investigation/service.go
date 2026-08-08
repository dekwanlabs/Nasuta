// Package investigation exposes the fixed delegated investigation scenario.
package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

const Scenario = "delegated.investigation"

var ErrUnavailable = errors.New("delegated investigation is unavailable")

type WorkflowExecutor interface {
	Execute(context.Context, workflow.ExecuteRequest) (workflow.Result, error)
}

type Service struct {
	executor WorkflowExecutor
}

type Evidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type Citation struct {
	Claim    string     `json:"claim"`
	Evidence []Evidence `json:"evidence"`
}

type Answer struct {
	Answer      string     `json:"answer"`
	Citations   []Citation `json:"citations"`
	Limitations []string   `json:"limitations"`
}

type Result struct {
	RunID  string `json:"run_id"`
	Answer Answer `json:"answer"`
}

func New(executor WorkflowExecutor) *Service {
	return &Service{executor: executor}
}

// Run maps one authenticated request to the immutable read-only workflow.
func (service *Service) Run(ctx context.Context, question string, actor agentapi.Actor) (Result, error) {
	if service == nil || service.executor == nil {
		return Result{}, ErrUnavailable
	}
	input, err := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: question})
	if err != nil {
		return Result{}, fmt.Errorf("marshal investigation request: %w", err)
	}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	workflowResult, err := service.executor.Execute(ctx, workflow.ExecuteRequest{
		Workflow: workflow.DefinitionRef{ID: workflow.DelegatedInvestigationID},
		Input:    input, Actor: actor, ActorPermissions: readOnly,
		Scenario: Scenario, ScenarioPermissions: readOnly,
	})
	if err != nil {
		return Result{}, fmt.Errorf("run delegated investigation: %w", err)
	}
	var answer Answer
	if err := json.Unmarshal(workflowResult.Output.Payload, &answer); err != nil {
		return Result{}, fmt.Errorf("decode delegated investigation answer: %w", err)
	}
	return Result{RunID: workflowResult.RunID, Answer: answer}, nil
}
