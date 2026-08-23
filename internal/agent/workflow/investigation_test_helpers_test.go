package workflow

import (
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/platform/config"
)

// investigationCatalogs provides the catalog fixtures shared by generic
// workflow verifier tests; it is deliberately test-only and is not a runtime
// investigation workflow compatibility layer.
func investigationCatalogs(t *testing.T, version int64) (*agentapi.SchemaRegistry, *catalog.Catalog) {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "test-model", LLMAnswerMaxTokens: 1024,
		LLMContextWindow: 16000, AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 3,
	}
	definitions, err := catalog.DefaultInvestigators(settings, version)
	if err != nil {
		t.Fatalf("prepare agents: %v", err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatalf("publish agents: %v", err)
	}
	return schemas, agents
}
