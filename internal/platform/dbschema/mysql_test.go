package dbschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDedupeGroupsPreservesOrder(t *testing.T) {
	got := dedupeGroups([]MySQLGroup{
		GroupQARun,
		GroupAuth,
		GroupQARun,
		GroupQASession,
		GroupAuth,
	})
	want := []MySQLGroup{GroupQARun, GroupAuth, GroupQASession}
	if len(got) != len(want) {
		t.Fatalf("dedupeGroups len=%d want=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeGroups[%d]=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestSchemaGroupsContainCreateStatements(t *testing.T) {
	cases := []struct {
		group  MySQLGroup
		tables []string
	}{
		{group: GroupAuth, tables: []string{"users", "sessions", "settings"}},
		{group: GroupDocuments, tables: []string{"documents"}},
		{group: GroupQASession, tables: []string{"qa_sessions", "qa_messages"}},
		{group: GroupQARun, tables: []string{"agent_runs", "agent_steps", "agent_llm_calls"}},
		{group: GroupQAMemory, tables: []string{"qa_memories"}},
		{group: GroupIncident, tables: []string{"incident_records"}},
		{group: GroupApproval, tables: []string{"pending_actions"}},
	}
	for _, tc := range cases {
		stmts, ok := mysqlSchema[tc.group]
		if !ok {
			t.Fatalf("group %q missing", tc.group)
		}
		if len(stmts) != len(tc.tables) {
			t.Fatalf("group %q stmt count=%d want=%d", tc.group, len(stmts), len(tc.tables))
		}
		for _, table := range tc.tables {
			if !containsCreateTable(stmts, table) {
				t.Fatalf("group %q missing create statement for %s", tc.group, table)
			}
		}
	}
}

func TestManagedSchemaDoesNotStoreTimesAsStrings(t *testing.T) {
	for group, statements := range mysqlSchema {
		for _, statement := range statements {
			if strings.Contains(statement, "created_at VARCHAR(40)") ||
				strings.Contains(statement, "updated_at VARCHAR(40)") ||
				strings.Contains(statement, "started_at VARCHAR(40)") ||
				strings.Contains(statement, "ended_at   VARCHAR(40)") {
				t.Fatalf("group %q still declares a string time column: %s", group, statement)
			}
		}
	}
}

func TestPlatformSchemaExcludesObserveTables(t *testing.T) {
	for group, statements := range mysqlSchema {
		for _, statement := range statements {
			for _, table := range []string{"observe_history", "observe_sources"} {
				if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS "+table) {
					t.Fatalf("platform group %q owns scenario table %q", group, table)
				}
			}
		}
	}
}

func TestLLMUsageMigrationAddsDetailAndRunAggregates(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_agent_llm_usage.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read LLM usage migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"CREATE TABLE agent_llm_calls", "ADD COLUMN input_tokens", "ADD COLUMN output_tokens",
		"ADD COLUMN total_tokens", "ADD COLUMN peak_reserved_tokens",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("LLM usage migration missing %q", required)
		}
	}
}

func TestMemoryLastUsedUsesDateTime(t *testing.T) {
	statements := mysqlSchema[GroupQAMemory]
	if len(statements) != 1 || !strings.Contains(statements[0], "last_used      DATETIME NULL DEFAULT NULL") {
		t.Fatalf("qa_memories.last_used must use nullable DATETIME")
	}
}

func TestQAMessageSchemaStoresToolProtocol(t *testing.T) {
	statements := mysqlSchema[GroupQASession]
	if len(statements) != 2 {
		t.Fatalf("qa session schema statements = %d", len(statements))
	}
	for _, required := range []string{
		"tool_calls_json MEDIUMTEXT NULL",
		"tool_call_id VARCHAR(128) NOT NULL DEFAULT ''",
		"tool_name   VARCHAR(128) NOT NULL DEFAULT ''",
	} {
		if !strings.Contains(statements[1], required) {
			t.Fatalf("qa_messages schema missing %q", required)
		}
	}
}

func TestQAMessageToolMigrationAddsProtocolColumns(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_message_tools.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA message tool migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ADD COLUMN tool_calls_json",
		"ADD COLUMN tool_call_id",
		"ADD COLUMN tool_name",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA message tool migration missing %q", required)
		}
	}
}

func TestMemorySchemaEnforcesOneActiveFact(t *testing.T) {
	statements := mysqlSchema[GroupQAMemory]
	if len(statements) != 1 {
		t.Fatalf("qa memory schema statements = %d", len(statements))
	}
	schema := statements[0]
	for _, required := range []string{
		"fact_key       VARCHAR(255) NOT NULL",
		"source_type    VARCHAR(32) NOT NULL",
		"status         VARCHAR(16) NOT NULL DEFAULT 'active'",
		"GENERATED ALWAYS AS (CASE WHEN status = 'active' THEN fact_key ELSE NULL END) STORED",
		"UNIQUE KEY uniq_user_factkey_active (user_id, active_fact_key)",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("qa memory schema missing %q", required)
		}
	}
}

func containsCreateTable(stmts []string, table string) bool {
	needle := "CREATE TABLE IF NOT EXISTS " + table
	for _, stmt := range stmts {
		if strings.Contains(stmt, needle) {
			return true
		}
	}
	return false
}
