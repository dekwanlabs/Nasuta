package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

type staticWorkflowAgentRuntime struct{}

func (staticWorkflowAgentRuntime) Run(
	context.Context,
	agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return agentapi.RunResult{
		Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{"ok":true}`),
	}, nil
}

var _ agentapi.Runtime = staticWorkflowAgentRuntime{}

func qaRuntimeTestPlatform(t *testing.T) *Platform {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	workflowCatalog := workflow.NewCatalog(schemas, agents)
	workflowStore, err := workflow.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	workflowService, err := workflow.NewService(workflowCatalog, workflowStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Platform{
		db: db,
		agents: agentRuntime{
			schemas: schemas,
			catalog: agents,
		},
		qa: qaState{
			runs:     agentrun.Bind(db),
			sessions: memory.NewSessionStore(db),
			memory:   memory.NewMemoryStore(db, nil, nil, 0),
		},
		flow: workflowRuntime{
			catalog: workflowCatalog,
			service: workflowService,
		},
	}
}

func enabledAgentSettings() *config.PlatformSettings {
	settings := &config.PlatformSettings{
		LLMBaseURL: "http://llm.invalid", LLMAPIKey: "key",
		LLMProvider: "openai", LLMModel: "model",
		LLMMaxTokens: 2048, LLMAnswerMaxTokens: 2048,
		LLMConclusionMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(2 * time.Minute), AgentMaxSteps: 4,
		AgentAnswerReserve: config.Duration(30 * time.Second),
	}
	settings.Apply(nil)
	return settings
}
