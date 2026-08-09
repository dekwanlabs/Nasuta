package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	agentdefinition "github.com/dekwanlabs/nasuta/internal/agent/definition"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	agenttools "github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
)

// Test-only type aliases into the agent subpackages. These are needed by the
// migrated fixtures but not by production QA code, so they live in the test
// helper file instead of polluting dependencies.go.
type DefinitionRuntime = agentdefinition.DefinitionRuntime
type ScenarioRun = agentdefinition.ScenarioRun
type AgentConfig = agentexecution.AgentConfig
type ToolExecutor = agentexecution.ToolExecutor
type Observer = agentexecution.Observer
type Controller = agentexecution.Controller
type Registry = tool.Registry
type RunStore = agentrun.RunStore
type RunStatus = agentrun.RunStatus
type RunRecord = agentrun.RunRecord
type RunUsageSummary = agentrun.RunUsageSummary
type RunTerminal = agentrun.RunTerminal
type SSEEvent = agentrun.SSEEvent

const (
	RunStatusRunning = agentrun.RunStatusRunning
	RunStatusPaused  = agentrun.RunStatusPaused
	RunKindAgent     = agentrun.RunKindAgent
	ToolKindRead     = tool.KindRead
	ToolKindWrite    = tool.KindWrite
	EvidenceNotRequired = agentrun.EvidenceNotRequired
	EventRunFinished     = agentrun.EventRunFinished
)

var ErrRunNotActive = agentrun.ErrRunNotActive

func NewAgent(client *llm.LLMClient, executor *ToolExecutor, config AgentConfig, observer Observer, controller Controller) *Agent {
	return agentexecution.NewAgent(client, executor, config, observer, controller)
}

func NewToolExecutor(registry *Registry) *ToolExecutor {
	return agentexecution.NewToolExecutor(registry)
}

func NewDefinitionRuntime(
	definitions DefinitionResolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *agentrun.RunStore,
) (*DefinitionRuntime, error) {
	return agentdefinition.NewDefinitionRuntime(definitions, schemas, registry, settings, runStore)
}

func TerminalFromEvent(event SSEEvent) *RunTerminal {
	return agentrun.TerminalFromEvent(event)
}

func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	return agenttools.NewRegistry(svc, cfg, sessions, history)
}

// ---- definition runtime fixtures (mirror of definition/runtime_test.go) ----

type definitionResolverFunc func(agentapi.DefinitionRef) (agentapi.Definition, error)

func (resolve definitionResolverFunc) Resolve(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
	return resolve(ref)
}

func testRuntimeSettings(baseURL string) *config.PlatformSettings {
	return &config.PlatformSettings{
		LLMBaseURL: baseURL, LLMAPIKey: "key",
		LLMProvider: "openai", LLMModel: "review-model",
		AgentAnswerReserve: config.Duration(100 * time.Millisecond),
	}
}

func testReviewerDefinition(t *testing.T, mutate func(*agentapi.Definition)) agentapi.Definition {
	t.Helper()
	definition := agentapi.Definition{
		ID: "review.architecture", Version: 3,
		Prompt: agentapi.PromptSpec{
			System:  "Review only the supplied subject and return structured JSON.",
			Version: "review-architecture-v1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "review.request", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: "openai", Model: "review-model", MaxOutputTokens: 256,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 2, ContextTokens: 4096,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{knowledgeReadScope}},
	}
	if mutate != nil {
		mutate(&definition)
	}
	prepared, err := agentapi.Prepare(definition)
	if err != nil {
		t.Fatalf("prepare definition: %v", err)
	}
	return prepared
}

func testQADefinition(t *testing.T, mutate func(*agentapi.Definition)) agentapi.Definition {
	t.Helper()
	definition := agentapi.Definition{
		ID: "qa.answerer", Version: 3,
		Prompt: agentapi.PromptSpec{
			System:  "Answer the supplied question.",
			Version: "qa-v1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "qa.answer", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: "openai", Model: "review-model", MaxOutputTokens: 256,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 2, ContextTokens: 4096,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{knowledgeReadScope}},
	}
	if mutate != nil {
		mutate(&definition)
	}
	prepared, err := agentapi.Prepare(definition)
	if err != nil {
		t.Fatalf("prepare definition: %v", err)
	}
	return prepared
}

func testDefinitionRequest(definition agentapi.Definition) agentapi.RunRequest {
	content := "immutable review material"
	return agentapi.RunRequest{
		RunID: "review-run-1",
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
		DefinitionHash: definition.ContentHash,
		Permissions:    agentapi.PermissionPolicy{Scopes: []string{knowledgeReadScope}},
		Input: json.RawMessage(
			`{"subject":{"kind":"technical_proposal"},"categories":["architecture"],"policy_hash":"policy-1"}`,
		),
		Context: []agentapi.ContextBlock{{
			Source: "feature_artifact", Title: "Technical Proposal",
			Content: content, Complete: true, ContentHash: hashString(content),
		}},
		Actor: agentapi.Actor{UserID: 42, TenantID: "tenant-1"},
		Correlation: agentapi.Correlation{
			SessionID: "session-1", WorkflowRunID: "round-1", NodeID: "architecture",
		},
	}
}

func newTestDefinitionRuntime(
	t *testing.T,
	definition agentapi.Definition,
	registry *Registry,
	settings *config.PlatformSettings,
	store *RunStore,
) *DefinitionRuntime {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	runtime, err := NewDefinitionRuntime(
		definitionResolverFunc(func(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
			if ref.ID != definition.ID || ref.Version != definition.Version {
				return agentapi.Definition{}, fmt.Errorf("definition not found")
			}
			return definition, nil
		}),
		schemas,
		registry,
		settings,
		store,
	)
	if err != nil {
		t.Fatalf("NewDefinitionRuntime: %v", err)
	}
	return runtime
}

// ---- tool fixtures (mirror of tool_test_helpers_test.go / tool_prune_test.go) ----

type toolTraceRecorder struct {
	events []domain.EvaluationTrace
}

func (recorder *toolTraceRecorder) RecordTrace(event domain.EvaluationTrace) {
	recorder.events = append(recorder.events, event)
}

func stringHandler(run func(context.Context, tool.Arguments) (string, error)) tool.Handler {
	return tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
		content, err := run(ctx, args)
		return tool.Result{Content: content}, err
	})
}

func objectSchema(properties map[string]any, required []string) tool.JSONSchema {
	schema := tool.JSONSchema{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func propString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func propInt(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func noopTool(context.Context, tool.Arguments) (string, error) {
	return "ok", nil
}

func testRegistry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	registry := tool.NewRegistry()
	if err := registry.RegisterAll(tools); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testAgentTool(
	id tool.ToolID,
	kind tool.Kind,
	run func(context.Context, tool.Arguments) (string, error),
) Tool {
	return Tool{
		ID:          id,
		Description: "test tool",
		Kind:        kind,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler:     stringHandler(run),
	}
}

// scenarioTool builds a read tool that the router is allowed to select.
func scenarioTool(id tool.ToolID) Tool {
	candidate := testAgentTool(id, ToolKindRead, noopTool)
	candidate.Routing = &tool.RoutingSpec{Intent: "current runtime evidence"}
	return candidate
}

func traceEventByNode(t *testing.T, events []domain.EvaluationTrace, node string) domain.EvaluationTrace {
	t.Helper()
	for _, event := range events {
		if event.Node == node {
			return event
		}
	}
	t.Fatalf("no trace event for node %q", node)
	return domain.EvaluationTrace{}
}

// preparedScenarioTools is the definition runtime's tool set projection.
type preparedScenarioTools struct {
	snapshot tool.Snapshot
	executor *ToolExecutor
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
