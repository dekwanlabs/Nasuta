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
		{group: GroupQASession, tables: []string{"qa_sessions", "qa_messages", "qa_turns", "qa_turn_contexts", "qa_session_history_terms", "qa_session_history_index_outbox"}},
		{group: GroupQARun, tables: []string{"agent_runs", "agent_steps", "agent_llm_calls"}},
		{group: GroupQAMemory, tables: []string{"qa_memories"}},
		{group: GroupIncident, tables: []string{"incident_records"}},
		{group: GroupApproval, tables: []string{"pending_actions"}},
		{group: GroupFeatureDelivery, tables: []string{
			"feature_user_workspaces", "feature_requests", "feature_artifacts",
			"feature_artifact_reviews", "feature_generation_runs",
			"feature_implementation_runs", "feature_run_events",
			"feature_change_sets", "feature_change_reviews",
		}},
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

func TestFeatureDeliverySchemaStoresPlanDeviations(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	if !strings.Contains(statements, "plan_deviations_json    JSON NOT NULL") {
		t.Fatal("feature delivery schema does not require plan deviation metadata")
	}

	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_feature_change_set_deviations.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read feature change set migration: %v", err)
	}
	for _, required := range []string{
		"ADD COLUMN plan_deviations_json JSON NULL",
		"SET plan_deviations_json = JSON_ARRAY()",
		"MODIFY COLUMN plan_deviations_json JSON NOT NULL",
	} {
		if !strings.Contains(string(raw), required) {
			t.Fatalf("feature change set migration missing %q", required)
		}
	}
}

func TestQASessionHistoryRetrievalMigrationReplacesRollingSummary(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_session_history_retrieval.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA session history migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"DELETE FROM qa_turn_contexts", "DROP COLUMN summary",
		"archived_summary_tokens", "summary_tokens", "qa_session_history_terms",
		"qa_session_history_index_outbox", "compacted_through_turn = 0",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA session history migration missing %q", required)
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

func TestQASessionSchemaUsesJSONCompactionPayloads(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupQASession], "\n")
	for _, required := range []string{"archived_summary_tokens BIGINT NOT NULL", "detail_json   JSON NOT NULL", "summary_tokens INT NOT NULL"} {
		if !strings.Contains(statements, required) {
			t.Fatalf("QA session schema missing %q", required)
		}
	}
	if strings.Contains(statements, "session_state") {
		t.Fatal("QA session schema still declares session state columns")
	}
	if strings.Contains(statements, "text          MEDIUMTEXT NOT NULL") {
		t.Fatal("QA turn contexts still use legacy text detail storage")
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

func TestAgentEvidenceCoverageMigrationAddsRunAggregates(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_agent_evidence_coverage.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent evidence coverage migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ADD COLUMN evidence_status VARCHAR(16) NOT NULL DEFAULT 'unavailable'", "ADD COLUMN forced_conclusion",
		"ADD COLUMN evidence_result_count", "ADD COLUMN tool_call_count",
		"ADD COLUMN tool_failure_count", "ADD COLUMN partial_result_count",
		"ADD COLUMN omitted_evidence_count",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("agent evidence coverage migration missing %q", required)
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
	if len(statements) != 6 {
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

func TestQASessionTurnCompactionMigrationBackfillsBoundaries(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_session_turn_compaction.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA session compaction migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ADD COLUMN turn_no", "CREATE TABLE IF NOT EXISTS qa_turns",
		"CREATE TABLE IF NOT EXISTS qa_turn_contexts", "compacted_through_turn",
		"OVER (PARTITION BY session_id ORDER BY seq",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA session compaction migration missing %q", required)
		}
	}
}

func TestQASessionCompactionJSONMigrationResetsLegacySnapshots(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_session_compaction_json.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA session compaction JSON migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"DELETE FROM qa_turn_contexts", "SET summary = NULL", "MODIFY COLUMN summary JSON NULL",
		"CHANGE COLUMN text detail_json JSON NOT NULL", "DROP TABLE IF EXISTS qa_session_compactions",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA session compaction JSON migration missing %q", required)
		}
	}
}

func TestQATurnRunIDMigrationAddsMissingColumn(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_turns_run_id.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA turn run id migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ALTER TABLE qa_turns",
		"ADD COLUMN run_id",
		"VARCHAR(64) NOT NULL DEFAULT ''",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA turn run id migration missing %q", required)
		}
	}
}

func TestQAContextPollutionMigrationAddsCanonicalTurnMetadata(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_qa_context_pollution_control.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read QA context pollution migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ADD COLUMN question_text", "ADD COLUMN topic_key", "ADD COLUMN entities_json",
		"ADD COLUMN question_terms_json", "ADD COLUMN evidence_manifest_json",
		"'status', 'manifest_unavailable'", "MODIFY COLUMN entities_json JSON NOT NULL",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("QA context pollution migration missing %q", required)
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
