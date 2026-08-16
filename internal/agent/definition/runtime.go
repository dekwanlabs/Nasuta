package definition

import (
	"context"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	knowledgeReadScope  = scope.KnowledgeRead
	knowledgeWriteScope = scope.KnowledgeWrite
)

// Runtime executes immutable definitions through the shared loop.
type Runtime struct {
	definitions Resolver
	schemas     *agentapi.SchemaRegistry
	registry    *tool.Registry
	executor    *execution.ToolExecutor
	settings    runtimeSettings
	scenarios   *ScenarioRuntime
	usageStore  llm.UsageRecorder
	hub         *run.Hub
}

// ScenarioRuntime owns durable non-agent Run lifecycles without requiring model execution.
type ScenarioRuntime struct {
	runStore *run.Store
	hub      *run.Hub
}

// Resolver resolves one exact immutable definition version.
type Resolver interface {
	Resolve(agentapi.DefinitionRef) (agentapi.Definition, error)
}

type runtimeSettings struct {
	baseURL       string
	apiKey        string
	provider      string
	model         string
	answerReserve time.Duration
}

type preparedExecution struct {
	definition      agentapi.Definition
	modelParameters llm.ModelParameters
	snapshot        agentapi.RunSnapshot
	toolPolicy      tool.Policy
	toolSnapshot    tool.Snapshot
	offeredTools    map[tool.ToolID]struct{}
	pruneApplied    bool
}

type toolSelection struct {
	policy       tool.Policy
	snapshot     tool.Snapshot
	visibleIDs   []string
	offeredIDs   map[tool.ToolID]struct{}
	pruneApplied bool
}

type activeRun struct {
	runtime   *Runtime
	start     agentapi.RunStart
	execution preparedExecution
	recorder  *usageRecorder
	trace     *runtrace.Scope
	ownsTrace bool

	mu                   sync.Mutex
	executed             bool
	finished             bool
	preparationStepCount int
	preparationEvidence  run.EvidenceMetrics
	outcomeSet           bool
	outcome              run.Outcome
}

// ScenarioRunStart carries business-run identity without an agent snapshot.
type ScenarioRunStart struct {
	RunID         string
	ParentRunID   string
	WorkflowRunID string
	UserID        int64
	SessionID     string
	Question      string
	Mode          string
	Limits        agentapi.RunLimits
}

// ScenarioRun owns one non-agent parent lifecycle.
type ScenarioRun interface {
	Context(context.Context) context.Context
	RecordStep(context.Context, run.StepRecord) error
	Release()
}

// ScenarioLifecycle keeps parent persistence behind the Runtime boundary.
type ScenarioLifecycle interface {
	Start(context.Context, ScenarioRunStart) (ScenarioRun, error)
	Complete(context.Context, string, run.Outcome) error
}

type scenarioManagedRun struct {
	runtime   *ScenarioRuntime
	start     ScenarioRunStart
	trace     *runtrace.Scope
	ownsTrace bool

	mu                   sync.Mutex
	preparationStepCount int
	release              sync.Once
}

// NewRuntime pins one configured model endpoint for definition execution.
func NewRuntime(
	definitions Resolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *run.Store,
) (*Runtime, error) {
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
	var usageStore llm.UsageRecorder
	if runStore != nil {
		usageStore = runStore
	}
	scenarios := NewScenarioRuntime(runStore)
	return &Runtime{
		definitions: definitions,
		schemas:     schemas,
		registry:    registry,
		executor:    execution.NewToolExecutor(registry),
		settings: runtimeSettings{
			baseURL: settings.LLMBaseURL, apiKey: settings.LLMAPIKey,
			provider: settings.LLMProvider, model: settings.LLMModel,
			answerReserve: answerReserve,
		},
		scenarios:  scenarios,
		usageStore: usageStore,
		hub:        scenarios.Hub(),
	}, nil
}

// NewScenarioRuntime binds Parent persistence and terminal projection.
func NewScenarioRuntime(runStore *run.Store) *ScenarioRuntime {
	return &ScenarioRuntime{
		runStore: runStore,
		hub:      run.NewHub(runStore),
	}
}

// Hub exposes the Runtime-owned event and control boundary.
func (runtime *Runtime) Hub() *run.Hub {
	if runtime == nil {
		return nil
	}
	return runtime.hub
}

// Hub exposes Parent lifecycle events independently of model execution.
func (runtime *ScenarioRuntime) Hub() *run.Hub {
	if runtime == nil {
		return nil
	}
	return runtime.hub
}

// EmitPhase publishes QA preparation progress without exposing RunHub ownership.
func (runtime *Runtime) EmitPhase(runID, text string) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.EmitPhase(runID, text)
	}
}

func (runtime *Runtime) EmitSessionStatus(runID string, event run.SessionStatusEvent) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.EmitSessionStatus(runID, event)
	}
}

func (runtime *Runtime) EmitContextUsage(runID string, event run.ContextUsageEvent) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.OnContextUsage(context.Background(), runID, event)
	}
}

// EmitEvent publishes scenario progress through the shared QA stream.
func (runtime *Runtime) EmitEvent(
	eventType run.EventType,
	event run.ExecutionEvent,
) {
	if runtime != nil && runtime.hub != nil {
		runtime.hub.EmitEvent(eventType, event)
	}
}

// ScenarioToolSet pins tools used while a scenario prepares one RunRequest.
type ScenarioToolSet interface {
	Tools() []tool.Tool
	Get(tool.ToolID) (tool.Tool, bool)
	Execute(context.Context, tool.ToolID, tool.Arguments) (tool.Result, error)
}

// ScenarioToolSource exposes a narrow preparation boundary over Runtime-owned tools.
type ScenarioToolSource interface {
	ToolsFor(tool.Policy) ScenarioToolSet
}

type preparedScenarioTools struct {
	snapshot tool.Snapshot
	executor *execution.ToolExecutor
}

func (prepared preparedScenarioTools) Tools() []tool.Tool {
	return prepared.snapshot.Tools()
}

func (prepared preparedScenarioTools) Get(id tool.ToolID) (tool.Tool, bool) {
	return prepared.snapshot.Get(id)
}

func (prepared preparedScenarioTools) Execute(
	ctx context.Context,
	id tool.ToolID,
	arguments tool.Arguments,
) (tool.Result, error) {
	return prepared.executor.ExecuteArguments(ctx, prepared.snapshot, id, arguments)
}

// ToolsFor returns a pinned preparation view; execution Runs pin their own snapshot.
func (runtime *Runtime) ToolsFor(policy tool.Policy) ScenarioToolSet {
	return preparedScenarioTools{
		snapshot: runtime.registry.Snapshot(policy),
		executor: runtime.executor,
	}
}

type usageRecorder struct {
	mu                                sync.Mutex
	store                             llm.UsageRecorder
	inputPriceMicrosPerMillionTokens  int64
	outputPriceMicrosPerMillionTokens int64
	usage                             agentapi.Usage
	limits                            agentapi.RunLimits
}
