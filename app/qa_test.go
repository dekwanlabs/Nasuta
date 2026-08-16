package app

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestSettingsAffectQARuntime(t *testing.T) {
	for _, test := range []struct {
		name string
		keys []string
		want bool
	}{
		{
			name: "platform integration settings",
			keys: []string{
				"vcs_url",
				"vcs_token",
				"vcs_groups",
				"vcs_webhook_secret",
				"vcs_clone_concurrency",
				"vcs_exclude_projects",
				"coding_enabled_providers",
				"coding_default_provider",
				"coding_codex_model",
				"coding_claude_model",
				"feature_generation_timeout",
				"coding_timeout",
				"coding_max_concurrency",
				"coding_allow_network",
				"coding_worktree_ttl",
			},
			want: false,
		},
		{
			name: "agent settings",
			keys: []string{"llm_model", "agent_timeout", "retrieval_router_max_tokens"},
			want: true,
		},
		{
			name: "retrieval settings",
			keys: []string{"context_budget", "rerank_enabled", "domain_knowledge"},
			want: true,
		},
		{
			name: "delegation settings",
			keys: []string{"delegation_enabled", "delegation_capabilities"},
			want: true,
		},
		{
			name: "unknown future setting",
			keys: []string{"future_runtime_setting"},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := settingsAffectQARuntime(test.keys); got != test.want {
				t.Fatalf("settingsAffectQARuntime(%v) = %t, want %t", test.keys, got, test.want)
			}
		})
	}
}

func TestConfigureIncidentsDoesNotRebuildQARuntime(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schemas := agentapiSchemaRegistry(t)
	agents := catalog.New(schemas)
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	sentinel := &dashboard.QARuntime{Settings: settings}
	platform := &Platform{
		cfg:      config.Config{WorkspaceRoot: t.TempDir()},
		settings: settings,
		registry: tool.NewRegistry(),
		agents: agentRuntime{
			schemas: schemas,
			catalog: agents,
			version: 7,
		},
		qa: qaState{current: *sentinel},
	}

	if err := platform.configureIncidentsWithDB(db, nil); err != nil {
		t.Fatal(err)
	}
	if platform.agents.version != 7 {
		t.Fatalf("agent catalog version = %d, want unchanged version 7", platform.agents.version)
	}
	if platform.qa.current.Settings != sentinel.Settings {
		t.Fatal("incident configuration replaced the QA runtime")
	}
	if !platform.qa.current.WriteAvailable {
		t.Fatal("incident configuration did not enable QA write availability")
	}
}

func agentapiSchemaRegistry(t *testing.T) *agentapi.SchemaRegistry {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	return schemas
}
