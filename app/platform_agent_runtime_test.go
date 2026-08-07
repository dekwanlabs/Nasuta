package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
)

func TestDefaultAgentDefinitionsShareOneSettingsVersion(t *testing.T) {
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 9)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{
		"qa.answerer",
		"review.architecture",
		"review.security",
		"review.reliability",
		"review.adjudicator",
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
		"synthesizer",
	}
	if len(definitions) != len(wantIDs) {
		t.Fatalf("definitions = %d, want %d", len(definitions), len(wantIDs))
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	catalog := agentcatalog.New(schemas)
	if err := catalog.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	if catalog.Revision() != 1 {
		t.Fatalf("catalog revision = %d, want one atomic publication", catalog.Revision())
	}
	for index, definition := range definitions {
		if definition.ID != wantIDs[index] || definition.Version != 9 ||
			definition.ContentHash == "" {
			t.Fatalf("definition %d = %+v", index, definition)
		}
		if index == 0 && !definition.Tools.AllowWrite {
			t.Fatalf("QA definition did not expose the platform write ceiling: %+v", definition)
		}
		if index > 0 && definition.Tools.AllowWrite {
			t.Fatalf("reviewer definition %q permits writes", definition.ID)
		}
		resolved, err := catalog.Resolve(agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.ContentHash != definition.ContentHash {
			t.Fatalf("resolved definition %q hash = %q", definition.ID, resolved.ContentHash)
		}
	}
}

func TestBuildQARuntimeDisablesDefinitionRuntimeWithoutLLM(t *testing.T) {
	platform := &Platform{}
	settings := &config.PlatformSettings{}
	settings.Apply(nil)

	qa, definitions, runtime, err := platform.buildQARuntime(settings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if qa.QA != nil || len(definitions) != 0 || runtime != nil {
		t.Fatalf("disabled runtime = (qa=%p definitions=%d runtime=%v)", qa.QA, len(definitions), runtime)
	}
}

func TestConfigureAgentWorkflowRuntimeTracksLLMAvailability(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := agentcatalog.New(schemas)
	workflowCatalog := agentworkflow.NewCatalog(schemas, agents)
	workflowStore, err := agentworkflow.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentworkflow.NewService(workflowCatalog, workflowStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		schemaRegistry: schemas, agentCatalog: agents,
		workflowCatalog: workflowCatalog, workflowStore: workflowStore,
		workflowService: service,
	}
	runtime := staticWorkflowAgentRuntime{}
	if err := platform.configureAgentWorkflowRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if platform.workflowNodes == nil || platform.workflowRunner == nil {
		t.Fatal("workflow runtime was not assembled")
	}
	if err := platform.configureAgentWorkflowRuntime(nil); err != nil {
		t.Fatal(err)
	}
	if platform.workflowNodes != nil || platform.workflowRunner != nil {
		t.Fatal("workflow runtime remained enabled without an LLM runtime")
	}
}

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
