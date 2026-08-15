package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

// Starter is the narrow authenticated entry point for the fixed pipeline.
type Starter struct {
	workflow *workflow.Service
}

func NewStarter(workflow *workflow.Service) *Starter {
	return &Starter{workflow: workflow}
}

func (starter *Starter) Start(
	ctx context.Context,
	request Request,
	actor agentapi.Actor,
) (*workflow.RunRecord, error) {
	if starter == nil || starter.workflow == nil {
		return nil, fmt.Errorf("feature pipeline workflow is unavailable: %w", delivery.ErrUnavailable)
	}
	request, err := normalizeRequest(request)
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal feature pipeline request: %w", err)
	}
	return starter.workflow.Start(ctx, workflow.StartRequest{
		Workflow: workflow.DefinitionRef{ID: WorkflowID, Version: WorkflowVersion},
		Input:    input,
		Actor:    actor,
		Admin:    true,
	})
}

var _ interface {
	Start(context.Context, Request, agentapi.Actor) (*workflow.RunRecord, error)
} = (*Starter)(nil)
