package agent

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	agentdefinition "github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/tool"
)

type DefinitionRuntime = agentdefinition.DefinitionRuntime
type DefinitionResolver = agentdefinition.DefinitionResolver
type ScenarioRunStart = agentdefinition.ScenarioRunStart
type ScenarioRun = agentdefinition.ScenarioRun
type ScenarioLifecycle = agentdefinition.ScenarioLifecycle
type ScenarioToolSet = agentdefinition.ScenarioToolSet
type ScenarioToolSource = agentdefinition.ScenarioToolSource

// NewDefinitionRuntime preserves the internal agent facade used by application wiring.
func NewDefinitionRuntime(
	definitions DefinitionResolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *RunStore,
) (*DefinitionRuntime, error) {
	return agentdefinition.NewDefinitionRuntime(
		definitions,
		schemas,
		registry,
		settings,
		runStore,
	)
}
