package execution

import (
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

// NoopObserver returns a run observer that discards every step and token event.
func NoopObserver() run.Observer {
	return run.NoopObserver()
}

// ToolPolicyForRun derives the fixed tool policy for a single agent run.
func ToolPolicyForRun(allowWrite bool) tool.Policy {
	return tool.Policy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}
