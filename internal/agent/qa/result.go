package qa

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/tool"
)

func outcomeFor(result *RunResult, preRetrieved []agentapi.Reference, runErr error) RunOutcome {
	return execution.OutcomeFor(result, preRetrieved, runErr)
}

// mergeOutcomeReferences keeps one canonical public reference set across sources.
func mergeOutcomeReferences(preRetrieved []agentapi.Reference, dynamic []tool.Reference) []agentapi.Reference {
	return execution.MergeOutcomeReferences(preRetrieved, dynamic)
}
