package app

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/investigation"
)

type platformInvestigationExecutor struct {
	platform *Platform
}

func (executor platformInvestigationExecutor) Execute(
	ctx context.Context,
	request agentworkflow.ExecuteRequest,
) (agentworkflow.Result, error) {
	if executor.platform == nil {
		return agentworkflow.Result{}, investigation.ErrUnavailable
	}
	platform := executor.platform
	platform.qaReloadMu.RLock()
	defer platform.qaReloadMu.RUnlock()
	if platform.workflowService == nil || platform.definitionRuntime == nil ||
		platform.agentDefinitionVer <= 0 {
		return agentworkflow.Result{}, investigation.ErrUnavailable
	}
	request.Workflow.Version = platform.agentDefinitionVer
	result, err := platform.workflowService.Execute(ctx, request)
	if err != nil {
		return agentworkflow.Result{}, fmt.Errorf("execute active workflow version %d: %w", request.Workflow.Version, err)
	}
	return result, nil
}
