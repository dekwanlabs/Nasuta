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
		{group: GroupQARun, tables: []string{"agent_runs", "agent_steps"}},
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

func TestTimeMigrationRelaxesMemoryLastUsedBeforeNullNormalization(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_time_columns.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read time migration: %v", err)
	}
	script := string(raw)
	relax := strings.Index(script, "ALTER TABLE qa_memories MODIFY COLUMN last_used VARCHAR(40) NULL DEFAULT NULL")
	normalize := strings.Index(script, "UPDATE qa_memories")
	finalize := strings.Index(script, "MODIFY COLUMN last_used DATETIME NULL DEFAULT NULL")
	if relax < 0 || normalize < 0 || finalize < 0 {
		t.Fatalf("qa_memories migration is incomplete")
	}
	if !(relax < normalize && normalize < finalize) {
		t.Fatalf("qa_memories migration must relax last_used before normalization and finalize afterward")
	}
}

func TestMemoryLastUsedUsesDateTime(t *testing.T) {
	statements := mysqlSchema[GroupQAMemory]
	if len(statements) != 1 || !strings.Contains(statements[0], "last_used      DATETIME NULL DEFAULT NULL") {
		t.Fatalf("qa_memories.last_used must use nullable DATETIME")
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
