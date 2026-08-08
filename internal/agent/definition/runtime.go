package definition

import (
	"context"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	knowledgeReadScope  = platformscope.KnowledgeRead
	knowledgeWriteScope = platformscope.KnowledgeWrite
)

// DefinitionRuntime executes immutable definitions through the shared loop.
type DefinitionRuntime struct {
	definitions DefinitionResolver
	schemas     *agentapi.SchemaRegistry
	registry    *tool.Registry
	executor    *agentexecution.ToolExecutor
	settings    definitionRuntimeSettings
	runStore    runCreator
	usageStore  llm.UsageRecorder
	hub         *agentrun.RunHub
}

// DefinitionResolver resolves one exact immutable definition version.
type DefinitionResolver interface {
	Resolve(agentapi.DefinitionRef) (agentapi.Definition, error)
}

type definitionRuntimeSettings struct {
	baseURL           string
	apiKey            string
	provider          string
	model             string
	answerReserve     time.Duration
	maxContinueRounds int
}

type definitionExecution struct {
	definition      agentapi.Definition
	modelParameters llm.ModelParameters
	snapshot        agentapi.RunSnapshot
	toolPolicy      tool.Policy
	toolSnapshot    tool.Snapshot
	offeredTools    map[tool.ToolID]struct{}
	pruneApplied    bool
}

type definitionToolSelection struct {
	policy       tool.Policy
	snapshot     tool.Snapshot
	visibleIDs   []string
	offeredIDs   map[tool.ToolID]struct{}
	pruneApplied bool
}

type definitionManagedRun struct {
	runtime   *DefinitionRuntime
	start     agentapi.RunStart
	execution definitionExecution
	recorder  *definitionUsageRecorder
	trace     *executiontrace.Scope
	ownsTrace bool

	mu       sync.Mutex
	executed bool
	finished bool
	outcome  agentrun.RunOutcome
}

// ScenarioRunStart carries business-run identity without an agent snapshot.
type ScenarioRunStart struct {
	RunID       string
	ParentRunID string
	UserID      int64
	SessionID   string
	Question    string
	Mode        string
}

// ScenarioRun owns one non-agent parent lifecycle.
type ScenarioRun interface {
	Context(context.Context) context.Context
	Finish(agentrun.RunOutcome) error
}

// ScenarioLifecycle keeps parent persistence behind the Runtime boundary.
type ScenarioLifecycle interface {
	BeginScenario(context.Context, ScenarioRunStart) (ScenarioRun, error)
}

type scenarioManagedRun struct {
	runtime   *DefinitionRuntime
	start     ScenarioRunStart
	trace     *executiontrace.Scope
	ownsTrace bool

	mu       sync.Mutex
	finished bool
}

// NewDefinitionRuntime pins one configured model endpoint for definition execution.
func NewDefinitionRuntime(
	definitions DefinitionResolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *agentrun.RunStore,
) (*DefinitionRuntime, error) {
	if definitions == nil {
		return nil, fmt.Errorf("definition runtime: definition resolver is required")
	}
	if schemas == nil {
		return nil, fmt.Errorf("definition runtime: schema registry is required")
	}
	if settings == nil {
		return nil, fmt.Errorf("definition runtime: platform settings are required")
	}
	switch settings.LLMProvider {
	case "openai", "anthropic":
	default:
		return nil, fmt.Errorf("definition runtime: unsupported LLM provider %q", settings.LLMProvider)
	}
	if !settings.LLMEnabled() {
		return nil, fmt.Errorf("definition runtime: LLM is unavailable")
	}
	answerReserve := time.Duration(settings.AgentAnswerReserve)
	if answerReserve <= 0 {
		return nil, fmt.Errorf("definition runtime: answer reserve must be positive")
	}
	if registry == nil {
		registry = tool.NewRegistry()
	}
	var creator runCreator
	var usageStore llm.UsageRecorder
	if runStore != nil {
		creator = runStore
		usageStore = runStore
	}
	return &DefinitionRuntime{
		definitions: definitions,
		schemas:     schemas,
		registry:    registry,
		executor:    agentexecution.NewToolExecutor(registry),
		settings: definitionRuntimeSettings{
			baseURL: settings.LLMBaseURL, apiKey: settings.LLMAPIKey,
			provider: settings.LLMProvider, model: settings.LLMModel,
			answerReserve: answerReserve, maxContinueRounds: settings.LLMMaxContinueRounds,
		},
		runStore:   creator,
		usageStore: usageStore,
		hub:        agentrun.NewRunHub(runStore),
	}, nil
}

// Hub exposes the Runtime-owned event and control boundary.
func (runtime *DefinitionRuntime) Hub() *agentrun.RunHub {
	if runtime == nil {
		return nil
	}
	return runtime.hub
}

// EmitPhase publishes QA preparation progress without exposing RunHub ownership.
func (runtime *DefinitionRuntime) EmitPhase(runID, text string) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.EmitPhase(runID, text)
	}
}

// EmitExecutionEvent publishes scenario progress through the shared QA stream.
func (runtime *DefinitionRuntime) EmitExecutionEvent(
	eventType agentrun.EventType,
	event agentrun.ExecutionEvent,
) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.EmitExecutionEvent(eventType, event)
	}
}

// ScenarioToolSet pins tools used while a scenario prepares one RunRequest.
type ScenarioToolSet interface {
	Tools() []tool.Tool
	Get(tool.ToolID) (tool.Tool, bool)
	ExecuteArguments(context.Context, tool.ToolID, tool.Arguments) (tool.Result, error)
}

// ScenarioToolSource exposes a narrow preparation boundary over Runtime-owned tools.
type ScenarioToolSource interface {
	PrepareTools(tool.Policy) ScenarioToolSet
}

type preparedScenarioTools struct {
	snapshot tool.Snapshot
	executor *agentexecution.ToolExecutor
}

func (prepared preparedScenarioTools) Tools() []tool.Tool {
	return prepared.snapshot.Tools()
}

func (prepared preparedScenarioTools) Get(id tool.ToolID) (tool.Tool, bool) {
	return prepared.snapshot.Get(id)
}

func (prepared preparedScenarioTools) ExecuteArguments(
	ctx context.Context,
	id tool.ToolID,
	arguments tool.Arguments,
) (tool.Result, error) {
	return prepared.executor.ExecuteArguments(ctx, prepared.snapshot, id, arguments)
}

// PrepareTools returns a pinned preparation view; execution Runs pin their own snapshot.
func (runtime *DefinitionRuntime) PrepareTools(policy tool.Policy) ScenarioToolSet {
	return preparedScenarioTools{
		snapshot: runtime.registry.Snapshot(policy),
		executor: runtime.executor,
	}
}

type runCreator interface {
	Create(agentrun.RunRecord) error
}

type definitionUsageRecorder struct {
	mu                                sync.Mutex
	store                             llm.UsageRecorder
	inputPriceMicrosPerMillionTokens  int64
	outputPriceMicrosPerMillionTokens int64
	usage                             agentapi.Usage
}
