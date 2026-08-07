package featurepipeline

import (
	"context"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

// Starter is the narrow authenticated entry point for the fixed pipeline.
type Starter struct {
	workflow *agentworkflow.Service
}

func NewStarter(workflow *agentworkflow.Service) *Starter {
	return &Starter{workflow: workflow}
}

func (starter *Starter) Start(
	ctx context.Context,
	request Request,
	actor agentapi.Actor,
) (*agentworkflow.WorkflowRunRecord, error) {
	if starter == nil || starter.workflow == nil {
		return nil, fmt.Errorf("feature pipeline workflow is unavailable: %w", featuredelivery.ErrUnavailable)
	}
	if actor.UserID <= 0 {
		return nil, fmt.Errorf("pipeline actor identity is required: %w", featuredelivery.ErrInvalid)
	}
	request, err := normalizeRequest(request)
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal feature pipeline request: %w", err)
	}
	return starter.workflow.Start(ctx, agentworkflow.StartRequest{
		Workflow: agentworkflow.DefinitionRef{ID: WorkflowID, Version: WorkflowVersion},
		Input:    input,
		Actor:    actor,
		Admin:    true,
	})
}

var _ interface {
	Start(context.Context, Request, agentapi.Actor) (*agentworkflow.WorkflowRunRecord, error)
} = (*Starter)(nil)
