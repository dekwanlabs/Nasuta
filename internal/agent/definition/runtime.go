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
	runStore    *run.Store
	usageStore  llm.UsageRecorder
	hub         *run.Hub

	// delegationAwaiter resolves finished child reports on the server side.
	// The application wires it after the delegation executor is built.
	delegationAwaiter execution.DelegationAwaiter

	recoveryMu         sync.Mutex
	recoveryCancel     context.CancelFunc
	recoveryGeneration uint64
}

// Resolver resolves one exact immutable definition version.
type Resolver interface {
	Resolve(agentapi.DefinitionRef) (agentapi.Definition, error)
}

type runtimeSettings struct {
	baseURL                     string
	apiKey                      string
	provider                    string
	model                       string
	answerReserve               time.Duration
	conclusionMaxTokens         int
	disableLegacyAnswerRecovery bool
}

type preparedExecution struct {
	definition       agentapi.Definition
	modelParameters  llm.ModelParameters
	snapshot         agentapi.RunSnapshot
	toolPolicy       tool.Policy
	toolSnapshot     tool.Snapshot
	offeredTools     map[tool.ToolID]struct{}
	pruneApplied     bool
	structuredOutput bool
	answerReserve    time.Duration
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
	budget    agentapi.RunBudgetGate

	mu                   sync.Mutex
	executed             bool
	finished             bool
	preparationStepCount int
	preparationEvidence  run.EvidenceMetrics
	outcomeSet           bool
	outcome              run.Outcome
}

// NewRuntime pins one configured model endpoint for definition execution.
func NewRuntime(
	definitions Resolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *run.Store,
	hub *run.Hub,
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
	if hub == nil {
		hub = run.NewHub(runStore)
	}
	return &Runtime{
		definitions: definitions,
		schemas:     schemas,
		registry:    registry,
		executor:    execution.NewToolExecutor(registry),
		settings: runtimeSettings{
			baseURL: settings.LLMBaseURL, apiKey: settings.LLMAPIKey,
			provider: settings.LLMProvider, model: settings.LLMModel,
			answerReserve:               answerReserve,
			conclusionMaxTokens:         settings.LLMConclusionMaxTokens,
			disableLegacyAnswerRecovery: settings.DisableLegacyAnswerRecovery,
		},
		runStore:   runStore,
		usageStore: usageStore,
		hub:        hub,
	}, nil
}

// SetDelegationAwaiter wires the server-side delegation settlement resolver
// into the runtime. The application calls it after building the delegation
// executor; before that the parent loop simply runs without the await.
func (runtime *Runtime) SetDelegationAwaiter(awaiter execution.DelegationAwaiter) {
	if runtime == nil {
		return
	}
	runtime.delegationAwaiter = awaiter
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
