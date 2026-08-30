package agent

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

type DefinitionRuntime = definition.Runtime
type DefinitionResolver = definition.Resolver
type ScenarioToolSet = definition.ScenarioToolSet
type ScenarioToolSource = definition.ScenarioToolSource

// NewDefinitionRuntime preserves the internal agent facade used by application wiring.
func NewDefinitionRuntime(
	definitions DefinitionResolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *run.Store,
) (*DefinitionRuntime, error) {
	return definition.NewRuntime(
		definitions,
		schemas,
		registry,
		settings,
		runStore,
	)
}
