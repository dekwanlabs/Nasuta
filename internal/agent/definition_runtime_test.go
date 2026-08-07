package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
	"github.com/dekwanlabs/nasuta/internal/llm"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

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

func testQARequest(definition agentapi.Definition) agentapi.RunRequest {
	return agentapi.RunRequest{
		RunID: "qa-run-1",
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
		DefinitionHash: definition.ContentHash,
		Permissions:    agentapi.PermissionPolicy{Scopes: []string{knowledgeReadScope}},
		Input:          json.RawMessage(`{"question":"What causes a rainbow?"}`),
		Actor:          agentapi.Actor{UserID: 42, TenantID: "tenant-1"},
		Correlation:    agentapi.Correlation{SessionID: "session-1"},
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
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
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

func TestDefinitionRuntimeRejectsUnpinnedOrUnsupportedExecution(t *testing.T) {
	tests := []struct {
		name      string
		mutateDef func(*agentapi.Definition)
		mutateReq func(*agentapi.RunRequest)
		want      string
	}{
		{
			name: "definition hash",
			mutateReq: func(request *agentapi.RunRequest) {
				request.DefinitionHash = strings.Repeat("a", 64)
			},
			want: "definition hash does not match",
		},
		{
			name: "input schema",
			mutateReq: func(request *agentapi.RunRequest) {
				request.Input = json.RawMessage(`{"question":"wrong contract"}`)
			},
			want: `validate schema "review.request" version 1 payload`,
		},
		{
			name: "provider",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Model.Provider = "anthropic"
			},
			want: "does not match configured model",
		},
		{
			name: "model",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Model.Model = "other-model"
			},
			want: "does not match configured model",
		},
		{
			name: "unknown model parameter",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Model.Parameters = map[string]any{"unknown": 0}
			},
			want: `model parameter "unknown" is not supported`,
		},
		{
			name: "write tools exceed definition",
			mutateReq: func(request *agentapi.RunRequest) {
				request.ToolScope.AllowWrite = true
			},
			want: "definition does not permit write tools",
		},
		{
			name: "permission scope",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Permissions.Scopes = []string{"approval.write"}
			},
			want: "permission scope",
		},
		{
			name: "domain permission scope",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Permissions.Scopes = []string{platformscope.FeatureDelivery}
			},
			want: "not supported by the agent runtime",
		},
		{
			name: "missing read tool",
			mutateDef: func(definition *agentapi.Definition) {
				definition.Tools.VisibleToolIDs = []string{"search_code"}
			},
			want: `tool "search_code" is unavailable`,
		},
		{
			name: "context hash",
			mutateReq: func(request *agentapi.RunRequest) {
				request.Context[0].ContentHash = strings.Repeat("b", 64)
			},
			want: "content_hash does not match content",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := testReviewerDefinition(t, test.mutateDef)
			request := testDefinitionRequest(definition)
			if test.mutateReq != nil {
				test.mutateReq(&request)
			}
			runtime := newTestDefinitionRuntime(
				t, definition, tool.NewRegistry(), testRuntimeSettings("http://unused"), nil,
			)
			result, err := runtime.Run(t.Context(), request)
			if err != nil {
				t.Fatalf("Run returned persistence error: %v", err)
			}
			if result.Status != agentapi.RunFailed || result.Error == nil ||
				result.Error.Code != "invalid_request" ||
				!strings.Contains(result.Error.Message, test.want) {
				t.Fatalf("result = %+v, want invalid_request containing %q", result, test.want)
			}
		})
	}
}

func TestDefinitionRuntimePinsModelParameters(t *testing.T) {
	definition := testReviewerDefinition(t, func(definition *agentapi.Definition) {
		definition.Model.Parameters = map[string]any{
			"temperature":       0.2,
			"top_p":             0.8,
			"stop":              []any{"END", "DONE"},
			"frequency_penalty": -0.4,
			"presence_penalty":  0.6,
		}
	})
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings("http://unused"), nil,
	)

	execution, err := runtime.prepare(testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if execution.modelParameters.Temperature == nil ||
		*execution.modelParameters.Temperature != 0.2 {
		t.Fatalf("temperature = %+v", execution.modelParameters.Temperature)
	}
	want := map[string]any{
		"temperature":       0.2,
		"top_p":             0.8,
		"stop":              []string{"END", "DONE"},
		"frequency_penalty": -0.4,
		"presence_penalty":  0.6,
	}
	if !reflect.DeepEqual(execution.snapshot.ModelParameters, want) {
		t.Fatalf(
			"snapshot parameters = %#v, want %#v",
			execution.snapshot.ModelParameters, want,
		)
	}
	if execution.snapshot.ModelParameters["stop"].([]string)[0] != "END" {
		t.Fatal("snapshot stop sequences were not detached")
	}
	definition.Model.Parameters["temperature"] = 1
	if execution.snapshot.ModelParameters["temperature"] != 0.2 {
		t.Fatal("snapshot parameters changed after definition mutation")
	}
}

func TestDefinitionRuntimePinsToolSnapshotAcrossRegistryMutation(t *testing.T) {
	registry := tool.NewRegistry()
	publisher := tool.NewReadRegistry(registry)
	readTool := func(content string) tool.ReadTool {
		return tool.ReadTool{
			ID: "lookup", Description: "lookup evidence",
			InputSchema: tool.JSONSchema{
				"type": "object", "properties": map[string]any{},
			},
			Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
				return tool.Result{Content: content}, nil
			}),
		}
	}
	if err := publisher.Reconcile(tool.ReadToolSet{
		Owner: "test", Tools: []tool.ReadTool{readTool("first")},
	}); err != nil {
		t.Fatal(err)
	}
	definition := testReviewerDefinition(t, func(definition *agentapi.Definition) {
		definition.Tools.VisibleToolIDs = []string{"lookup"}
	})
	runtime := newTestDefinitionRuntime(t, definition, registry, testRuntimeSettings("http://unused"), nil)
	execution, err := runtime.prepare(testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := publisher.Reconcile(tool.ReadToolSet{
		Owner: "test", Tools: []tool.ReadTool{readTool("second")},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := NewToolExecutor(registry).ExecuteArguments(
		t.Context(), execution.toolSnapshot, "lookup", tool.Arguments{},
	)
	if err != nil {
		t.Fatalf("execute pinned tool: %v", err)
	}
	if result.Content != "first" {
		t.Fatalf("pinned tool content = %q, want first", result.Content)
	}
	if execution.snapshot.ToolSnapshotID == registry.Snapshot(execution.toolPolicy).ID() {
		t.Fatal("tool snapshot ID did not change after registry mutation")
	}
}

func TestDefinitionRuntimeNarrowsDefinitionToolsByRunPermission(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("read", ToolKindRead, noopTool),
		testAgentTool("write", ToolKindWrite, noopTool),
	)
	definition := testReviewerDefinition(t, func(definition *agentapi.Definition) {
		definition.Tools = agentapi.ToolPolicy{
			AllowWrite: true, VisibleToolIDs: []string{"read", "write"},
		}
		definition.Permissions.Scopes = []string{knowledgeReadScope, knowledgeWriteScope}
	})
	runtime := newTestDefinitionRuntime(
		t, definition, registry, testRuntimeSettings("http://unused"), nil,
	)

	request := testDefinitionRequest(definition)
	execution, err := runtime.prepare(request)
	if err != nil {
		t.Fatalf("prepare read-only run: %v", err)
	}
	if got := strings.Join(execution.snapshot.VisibleToolIDs, ","); got != "read" {
		t.Fatalf("read-only visible tools = %q, want read", got)
	}
	if _, ok := execution.toolSnapshot.Get("write"); ok {
		t.Fatal("write tool entered a read-only run snapshot")
	}

	request.ToolScope.AllowWrite = true
	request.Permissions.Scopes = append(request.Permissions.Scopes, knowledgeWriteScope)
	execution, err = runtime.prepare(request)
	if err != nil {
		t.Fatalf("prepare write-enabled run: %v", err)
	}
	if got := strings.Join(execution.snapshot.VisibleToolIDs, ","); got != "read,write" {
		t.Fatalf("write-enabled visible tools = %q, want read,write", got)
	}
}

func TestDefinitionRuntimeRejectsRequestToolOutsideDefinition(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("allowed", ToolKindRead, noopTool),
		testAgentTool("outside", ToolKindRead, noopTool),
	)
	definition := testReviewerDefinition(t, func(definition *agentapi.Definition) {
		definition.Tools.VisibleToolIDs = []string{"allowed"}
	})
	runtime := newTestDefinitionRuntime(
		t, definition, registry, testRuntimeSettings("http://unused"), nil,
	)
	request := testDefinitionRequest(definition)
	request.ToolScope = agentapi.ToolScope{
		RestrictVisible: true, VisibleToolIDs: []string{"outside"},
	}

	result, err := runtime.Run(t.Context(), request)
	if err != nil {
		t.Fatalf("Run returned persistence error: %v", err)
	}
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		!strings.Contains(result.Error.Message, `requested tool "outside" is outside the definition`) {
		t.Fatalf("result = %+v", result)
	}
}

func TestDefinitionRuntimePreservesExplicitZeroToolScope(t *testing.T) {
	registry := testRegistry(t, testAgentTool("read", ToolKindRead, noopTool))
	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, registry, testRuntimeSettings("http://unused"), nil,
	)
	request := testDefinitionRequest(definition)
	request.ToolScope = agentapi.ToolScope{RestrictVisible: true, VisibleToolIDs: []string{}}

	execution, err := runtime.prepare(request)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(execution.snapshot.VisibleToolIDs) != 0 || len(execution.toolSnapshot.Tools()) != 0 {
		t.Fatalf("explicit zero scope expanded to %+v", execution.snapshot.VisibleToolIDs)
	}
}

func TestDefinitionRuntimePreservesDefinitionWithZeroVisibleTools(t *testing.T) {
	registry := testRegistry(t, testAgentTool("read", ToolKindRead, noopTool))
	definition := testReviewerDefinition(t, func(definition *agentapi.Definition) {
		definition.Tools = agentapi.ToolPolicy{RestrictVisible: true}
	})
	runtime := newTestDefinitionRuntime(
		t, definition, registry, testRuntimeSettings("http://unused"), nil,
	)

	execution, err := runtime.prepare(testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(execution.snapshot.VisibleToolIDs) != 0 || len(execution.toolSnapshot.Tools()) != 0 {
		t.Fatalf("zero-tool definition expanded to %+v", execution.snapshot.VisibleToolIDs)
	}
}

func TestDefinitionRuntimePinsSelectionAcrossManagedRun(t *testing.T) {
	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings("http://unused"), nil,
	)
	request := testDefinitionRequest(definition)
	request.Selection = agentapi.DefinitionSelection{
		RuleVersion: 4, RuleHash: strings.Repeat("b", 64),
		CandidateVersion: definition.Version, BucketBasisPoints: 1200,
		PercentageBasisPoints: 2500, StableKeyHash: strings.Repeat("c", 64),
		Reason: "rollout_candidate",
	}
	managedRun, err := runtime.Begin(t.Context(), runStart(request))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	managed := managedRun.(*definitionManagedRun)
	if managed.execution.snapshot.Selection != request.Selection {
		t.Fatalf(
			"snapshot selection = %+v, want %+v",
			managed.execution.snapshot.Selection, request.Selection,
		)
	}

	request.Selection.Reason = "rollout_default"
	result, err := managed.Execute(t.Context(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		result.Error.Code != "invalid_request" ||
		!strings.Contains(result.Error.Message, "does not match the prepared run") {
		t.Fatalf("result = %+v, want selection mismatch failure", result)
	}
}

func TestDefinitionRuntimePreservesToolMessageRoundTrip(t *testing.T) {
	messages := []llm.Message{
		{
			Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "call-1", Type: "function",
				Function: llm.ToolFunction{Name: "lookup", Arguments: `{"query":"x"}`},
			}},
		},
		{Role: "tool", Content: "result", ToolCallID: "call-1", Name: "lookup"},
	}
	got := publicResultMessages(publicMessages(messages))
	if len(got) != 2 || len(got[0].ToolCalls) != 1 {
		t.Fatalf("round trip messages = %#v", got)
	}
	call := got[0].ToolCalls[0]
	if call.ID != "call-1" || call.Type != "function" ||
		call.Function.Name != "lookup" || call.Function.Arguments != `{"query":"x"}` {
		t.Fatalf("round trip tool call = %#v", call)
	}
	if got[1].ToolCallID != "call-1" || got[1].Name != "lookup" || got[1].Content != "result" {
		t.Fatalf("round trip tool result = %#v", got[1])
	}
}

func TestDefinitionRuntimeBroadcastsTerminalWhenRunCreationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec("INSERT INTO agent_runs").WillReturnError(errors.New("database unavailable"))

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings("http://unused"), &RunStore{db: db},
	)
	request := testDefinitionRequest(definition)
	request.RunID = "run-create-fail"
	terminalEvents := runtime.Hub().Subscribe(request.RunID)

	result, err := runtime.Run(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "create definition run") {
		t.Fatalf("result=%+v error=%v, want run creation failure", result, err)
	}
	terminal := waitForTerminal(t, terminalEvents)
	if terminal.Status != RunStatusFailed || terminal.Evidence.Status != EvidenceUnavailable ||
		!strings.Contains(terminal.Error, "database unavailable") {
		t.Fatalf("terminal = %+v", terminal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionRuntimeExecutesStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"coverage\\\":[],\\\"findings\\\":[],\\\"uncertainties\\\":[],\\\"summary\\\":\\\"pass\\\"}\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	result, err := runtime.Run(t.Context(), testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != agentapi.RunSucceeded || string(result.Output) != result.Text {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 7 ||
		result.Usage.ReasoningTokens != 2 || result.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestDefinitionRuntimeEncodesTextOutputAsJSONString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"A rainbow forms when light is refracted and reflected.\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	definition := testQADefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	result, err := runtime.Run(t.Context(), testQARequest(definition))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := `"A rainbow forms when light is refracted and reflected."`
	if result.Status != agentapi.RunSucceeded || string(result.Output) != want ||
		result.Text != "A rainbow forms when light is refracted and reflected." {
		t.Fatalf("result = %+v, want output %s", result, want)
	}
}

func TestDefinitionRuntimeRejectsOutputOutsideSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"summary\\\":\\\"missing required fields\\\"}\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	result, err := runtime.Run(t.Context(), testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		result.Error.Code != "invalid_output" || result.Output != nil {
		t.Fatalf("result = %+v, want invalid_output", result)
	}
}

func TestDefinitionManagedRunAccountsPreparationAndDefersTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var payload struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !payload.Stream {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"choices":[{"message":{"content":"prepared"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"coverage\\\":[],\\\"findings\\\":[],\\\"uncertainties\\\":[],\\\"summary\\\":\\\"pass\\\"}\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	request := testDefinitionRequest(definition)
	request.RunID = "managed-usage-run"
	events := runtime.Hub().Subscribe(request.RunID)
	managed, err := runtime.Begin(t.Context(), runStart(request))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ctx := managed.Context(t.Context())
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "review-model", 256, server.Client())
	if _, err := client.ChatMax(llm.WithUsagePhase(ctx, llm.PhaseRoute), "route", "input", 64); err != nil {
		t.Fatalf("prepare call: %v", err)
	}
	result, err := managed.Execute(ctx, request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Usage != (agentapi.Usage{
		InputTokens: 14, OutputTokens: 9, ReasoningTokens: 2, TotalTokens: 23,
	}) {
		t.Fatalf("usage = %+v", result.Usage)
	}

	var phases []string
	drain := true
	for drain {
		select {
		case event := <-events:
			if event.Type == EventRunFinished {
				t.Fatal("Execute published a terminal event before Finish")
			}
			if call, ok := event.Data.(llm.CallLifecycle); ok && call.Status == llm.CallLifecycleStarted {
				phases = append(phases, call.Phase)
			}
		default:
			drain = false
		}
	}
	if strings.Join(phases, ",") != llm.PhaseRoute+","+llm.PhaseAgentStep {
		t.Fatalf("LLM phases = %v, want route then agent_step", phases)
	}
	if err := managed.Finish(nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if terminal := waitForTerminal(t, events); terminal.Status != RunStatusDone {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestDefinitionManagedRunFailureFinishesOnce(t *testing.T) {
	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings("http://unused"), nil,
	)
	request := testDefinitionRequest(definition)
	request.RunID = "managed-failed-run"
	events := runtime.Hub().Subscribe(request.RunID)
	managed, err := runtime.Begin(t.Context(), runStart(request))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := managed.Finish(&agentapi.RunError{
		Code: "session_persistence_failed", Message: "session database unavailable",
	}); err != nil {
		t.Fatalf("Finish failure: %v", err)
	}
	terminal := waitForTerminal(t, events)
	if terminal.Status != RunStatusFailed || !strings.Contains(terminal.Error, "session database unavailable") {
		t.Fatalf("terminal = %+v", terminal)
	}
	if err := managed.Finish(nil); err == nil {
		t.Fatal("second Finish succeeded")
	}
	select {
	case event := <-events:
		if event.Type == EventRunFinished {
			t.Fatal("second Finish published another terminal event")
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestMapDefinitionResultPreservesRetryableFailure(t *testing.T) {
	result, outcome := mapDefinitionResult(
		"retryable-run",
		&RunResult{Err: retryableDefinitionError{}},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		nil,
		agentapi.SchemaRef{},
	)
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		result.Error.Code != "agent_failed" || !result.Error.Retryable {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != RunStatusFailed || !errors.Is(outcome.Err, retryableDefinitionError{}) {
		t.Fatalf("outcome = %+v", outcome)
	}
}

type retryableDefinitionError struct{}

func (retryableDefinitionError) Error() string {
	return "temporary model transport failure"
}

func (retryableDefinitionError) Retryable() bool {
	return true
}

func TestDefinitionRuntimeMapsCallerCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	result, err := runtime.Run(ctx, testDefinitionRequest(definition))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != agentapi.RunCancelled || result.Error == nil ||
		result.Error.Code != "cancelled" || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("result = %+v cause=%v", result, context.Cause(ctx))
	}
}

func TestDefinitionUsageRecorderPersistsAndAggregates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recorder := &definitionUsageRecorder{store: &RunStore{db: db}}
	call := llm.CallUsage{
		RunID: "run-usage", Phase: llm.PhaseAgentStep,
		Provider: "openai", Model: "review-model", MaxOutputTokens: 256,
		Duration: 20 * time.Millisecond, Status: llm.CallStatusSucceeded,
		Usage: llm.Usage{
			InputTokens: 11, CachedInputTokens: 3,
			OutputTokens: 7, ReasoningTokens: 2, TotalTokens: 18,
		},
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT llm_call_count FROM agent_runs").
		WithArgs(call.RunID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_call_count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO agent_llm_calls").
		WithArgs(
			call.RunID, 1, call.Phase, call.Provider, call.Model,
			11, 3, 7, 2, 18, 256, int64(20), call.Status,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE agent_runs SET").
		WithArgs(11, 3, 7, 2, 18, 1, 11, 267, call.RunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := recorder.RecordLLMCall(t.Context(), call); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
	if usage := recorder.Usage(); usage != (agentapi.Usage{
		InputTokens: 11, OutputTokens: 7, ReasoningTokens: 2, TotalTokens: 18,
	}) {
		t.Fatalf("usage = %+v", usage)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionUsageRecorderRoundsEachTokenClassUp(t *testing.T) {
	recorder := &definitionUsageRecorder{
		inputPriceMicrosPerMillionTokens:  1,
		outputPriceMicrosPerMillionTokens: 1,
	}
	err := recorder.RecordLLMCall(t.Context(), llm.CallUsage{
		Usage: llm.Usage{
			InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage := recorder.Usage(); usage.CostMicros != 2 {
		t.Fatalf("usage = %+v, want two rounded micro-cost units", usage)
	}
}

func TestDefinitionUsageRecorderRejectsCostOverflow(t *testing.T) {
	if _, err := tokenCostMicros(math.MaxInt64, 2); err == nil {
		t.Fatal("tokenCostMicros accepted multiplication overflow")
	}

	recorder := &definitionUsageRecorder{
		inputPriceMicrosPerMillionTokens: 1,
		usage: agentapi.Usage{
			CostMicros: math.MaxInt64,
		},
	}
	err := recorder.RecordLLMCall(t.Context(), llm.CallUsage{
		Usage: llm.Usage{InputTokens: 1, TotalTokens: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "accumulate model cost") {
		t.Fatalf("RecordLLMCall error = %v, want accumulated cost overflow", err)
	}
}
