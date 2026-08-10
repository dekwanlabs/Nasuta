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
		{group: GroupRBAC, tables: []string{"rbac_roles", "rbac_user_roles", "rbac_menus", "rbac_role_menus", "rbac_mcp_keys"}},
		{group: GroupDocuments, tables: []string{"documents"}},
		{group: GroupQASession, tables: []string{"qa_sessions", "qa_messages", "qa_turns", "qa_session_history_terms", "qa_session_history_index_outbox"}},
		{group: GroupCatalogControl, tables: []string{"catalog_rollouts", "catalog_audit"}},
		{group: GroupQARun, tables: []string{
			"agent_definitions", "agent_runs", "agent_steps", "agent_llm_calls",
		}},
		{group: GroupWorkflow, tables: []string{
			"workflow_definitions", "workflow_runs", "workflow_node_runs", "handoff_artifacts",
		}},
		{group: GroupRuntimeEvents, tables: []string{"runtime_events"}},
		{group: GroupQAMemory, tables: []string{"qa_memories"}},
		{group: GroupIncident, tables: []string{"incident_records"}},
		{group: GroupApproval, tables: []string{"pending_actions"}},
		{group: GroupFeatureDelivery, tables: []string{
			"feature_user_workspaces", "feature_requests", "feature_artifacts",
			"feature_generation_runs",
			"feature_implementation_runs", "review_policies", "review_rounds",
			"review_assignments", "review_reports",
			"review_findings", "review_adjudications",
			"finding_resolutions",
			"review_evaluation_labels",
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

func TestManagedSchemaContainsFortyOneTables(t *testing.T) {
	total := 0
	for _, statements := range mysqlSchema {
		total += len(statements)
	}
	if total != 41 {
		t.Fatalf("managed schema table count=%d want=41", total)
	}
}

func TestAgentRolloutSchemaStoresSelection(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupCatalogControl], "\n")
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS catalog_rollouts",
		"CREATE TABLE IF NOT EXISTS catalog_audit",
		"catalog_kind      VARCHAR(32) NOT NULL",
		"KEY idx_catalog_audit_stream (catalog_kind, event_kind, subject_id, seq)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("agent rollout schema missing %q", required)
		}
	}
}

func TestWorkflowRolloutSchemaStoresSelection(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupCatalogControl], "\n")
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS catalog_rollouts",
		"CREATE TABLE IF NOT EXISTS catalog_audit",
		"PRIMARY KEY (catalog_kind, subject_id)",
		"UNIQUE KEY uniq_catalog_rollout_hash (catalog_kind, rule_hash)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("workflow rollout schema missing %q", required)
		}
	}
}

func TestReviewPolicySchemaStoresRolloutMetadata(t *testing.T) {
	policyStatements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	controlStatements := strings.Join(mysqlSchema[GroupCatalogControl], "\n")
	for _, required := range []string{
		"active          TINYINT(1) NOT NULL DEFAULT 1",
		"is_default      TINYINT(1) NOT NULL DEFAULT 0",
		"default_key     VARCHAR(48) GENERATED ALWAYS",
	} {
		if !strings.Contains(policyStatements, required) {
			t.Fatalf("review policy schema missing %q", required)
		}
	}
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS catalog_rollouts",
		"CREATE TABLE IF NOT EXISTS catalog_audit",
		"candidate_id      VARCHAR(128) NOT NULL DEFAULT ''",
	} {
		if !strings.Contains(controlStatements, required) {
			t.Fatalf("catalog control schema missing %q", required)
		}
	}
}

func TestReviewEvaluationSchemaStoresImmutableLabels(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS review_evaluation_labels",
		"UNIQUE KEY uniq_review_evaluation_target (round_id, target_hash)",
		"KEY idx_review_evaluation_policy (policy_id, policy_version, created_at, seq)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("review evaluation schema missing %q", required)
		}
	}
}

func TestWorkflowSchemaStoresApprovalSnapshots(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupWorkflow], "\n")
	for _, required := range []string{
		"actor_permissions_json JSON NOT NULL",
		"scenario_permissions_json JSON NOT NULL",
		"approval_decision   VARCHAR(16) NULL",
		"approver_user_id    BIGINT NULL",
		"approver_tenant_id  VARCHAR(128) NULL",
		"approval_decided_at TIMESTAMP NULL",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("workflow schema missing %q", required)
		}
	}
}

func TestFeatureDeliverySchemaStoresPlanDeviations(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	if !strings.Contains(statements, "plan_deviations_json   JSON NULL") {
		t.Fatal("feature delivery schema does not require plan deviation metadata")
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
	for _, required := range []string{
		"archived_summary_tokens BIGINT NOT NULL",
		"context_detail_json JSON NULL",
		"context_summary_tokens INT NULL",
		"UNIQUE KEY uniq_qa_turn_context_ref (context_ref)",
	} {
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

func TestLLMUsageSchemaStoresDetailAndRunAggregates(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupQARun], "\n")
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS agent_llm_calls",
		"input_tokens", "output_tokens", "total_tokens", "peak_reserved_tokens",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("LLM usage schema missing %q", required)
		}
	}
}

func TestAgentEvidenceCoverageSchemaStoresRunAggregates(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupQARun], "\n")
	for _, required := range []string{
		"evidence_status VARCHAR(16) NOT NULL DEFAULT 'unavailable'", "forced_conclusion",
		"evidence_result_count", "tool_call_count",
		"tool_failure_count", "partial_result_count",
		"omitted_evidence_count",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("agent evidence coverage schema missing %q", required)
		}
	}
}

func TestAgentDefinitionSnapshotSchemaPinsRunIdentity(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupQARun], "\n")
	for _, required := range []string{
		"agent_id", "definition_version", "definition_hash", "tool_snapshot_id",
		"input_schema_version", "output_schema_version", "parent_run_id",
		"workflow_run_id", "workflow_node_id", "error_code",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("agent run schema missing %q", required)
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
	if len(statements) != 5 {
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

func TestFeatureSubjectReviewRemovalMigrationCopiesBothSubjectKinds(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_remove_feature_subject_reviews.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read feature subject review removal migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ALTER TABLE feature_artifacts",
		"ALTER TABLE feature_implementation_runs",
		"IF old_table_exists = 0 THEN",
		"LEAVE migration",
		"subject_kind NOT IN ('artifact', 'change_set')",
		"orphan_artifact_count",
		"orphan_change_set_count",
		"artifact_conflict_count",
		"change_set_conflict_count",
		"UPDATE feature_artifacts artifact",
		"UPDATE feature_implementation_runs implementation",
		"old_artifact_count <> migrated_artifact_count",
		"old_change_set_count <> migrated_change_set_count",
		"SIGNAL SQLSTATE '45000'",
		"DROP TABLE feature_subject_reviews",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("feature subject review removal migration missing %q", required)
		}
	}
	for _, column := range []string{
		"ADD COLUMN review_subject_hash CHAR(64) NULL",
		"ADD COLUMN review_round_id VARCHAR(64) NULL",
		"ADD COLUMN review_gate_result_id VARCHAR(64) NULL",
		"ADD COLUMN review_decision VARCHAR(16) NULL",
		"ADD COLUMN review_comment TEXT NULL",
		"ADD COLUMN review_reviewer BIGINT NULL",
		"ADD COLUMN review_created_at TIMESTAMP NULL",
	} {
		if count := strings.Count(script, column); count != 2 {
			t.Fatalf("feature subject review removal migration has %d copies of %q, want 2", count, column)
		}
	}
	artifactValidation := strings.Index(script, "old_artifact_count <> migrated_artifact_count")
	changeSetValidation := strings.Index(script, "old_change_set_count <> migrated_change_set_count")
	commit := strings.Index(script, "COMMIT;")
	drop := strings.Index(script, "DROP TABLE feature_subject_reviews")
	if artifactValidation < 0 || changeSetValidation < 0 ||
		commit < artifactValidation || commit < changeSetValidation || drop < commit {
		t.Fatal("feature subject review removal migration drops source data before validation and commit")
	}
}

func TestReviewGateMigrationMergesIntoRound(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	for _, required := range []string{
		"gate_result_id VARCHAR(64) NULL",
		"gate_result_json JSON NULL",
		"UNIQUE KEY uniq_review_gate_result_id (gate_result_id)",
		"KEY idx_review_gate_subject (subject_hash, gate_created_at, gate_result_id)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("review round schema missing %q", required)
		}
	}
	if strings.Contains(statements, "CREATE TABLE IF NOT EXISTS review_gate_results") {
		t.Fatal("feature schema still creates review_gate_results")
	}
}

func TestFeatureChangeSetMigrationMergesIntoImplementationRun(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	for _, required := range []string{
		"worktree_head          VARCHAR(64) NULL",
		"plan_deviations_json   JSON NULL",
		"change_set_created_at  TIMESTAMP NULL",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("feature implementation schema missing %q", required)
		}
	}
	if strings.Contains(statements, "CREATE TABLE IF NOT EXISTS feature_change_sets") {
		t.Fatal("feature schema still creates feature_change_sets")
	}
}

func TestCompactSchemaAlignmentMigrationPreservesLegacySources(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "sql", "migration_align_schema_20260810.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read compact schema alignment migration: %v", err)
	}
	script := string(raw)
	for _, required := range []string{
		"ADD COLUMN selection_json JSON NULL",
		"SET selection_json = JSON_OBJECT()",
		"agent_tool_result_artifacts",
		"UPDATE agent_steps target",
		"feature_artifact_reviews",
		"UPDATE feature_artifacts target",
		"feature_change_sets",
		"feature_change_reviews",
		"UPDATE feature_implementation_runs target",
		"feature_run_events",
		"INSERT IGNORE INTO runtime_events",
		"qa_turn_contexts",
		"UPDATE qa_turns target",
		"CREATE UNIQUE INDEX uniq_workflow_node_attempt",
		"SIGNAL SQLSTATE '45000'",
		"legacy source tables retained",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("compact schema alignment migration missing %q", required)
		}
	}
	if strings.Contains(script, "DROP TABLE ") {
		t.Fatal("compact schema alignment migration drops a legacy source table")
	}

	artifactValidation := strings.Index(script, "artifact migration did not copy every legacy artifact")
	artifactIndex := strings.Index(script, "CREATE UNIQUE INDEX uniq_agent_step_artifact")
	qaValidation := strings.Index(script, "QA context migration did not copy every context")
	qaIndex := strings.Index(script, "CREATE UNIQUE INDEX uniq_session_run")
	if artifactValidation < 0 || artifactIndex < artifactValidation {
		t.Fatal("artifact uniqueness is applied before legacy data validation")
	}
	if qaValidation < 0 || qaIndex < qaValidation {
		t.Fatal("QA uniqueness is applied before legacy context validation")
	}
}

func TestReviewReportReuseMigrationMergesIntoReport(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupFeatureDelivery], "\n")
	for _, required := range []string{
		"reuse_id       VARCHAR(64) NULL",
		"reuse_source_report_id VARCHAR(64) NULL",
		"UNIQUE KEY uniq_review_report_reuse_id (reuse_id)",
		"KEY idx_review_report_reuse_source (reuse_source_report_id, reuse_created_at, reuse_id)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("review report schema missing %q", required)
		}
	}
	if strings.Contains(statements, "CREATE TABLE IF NOT EXISTS review_report_reuses") {
		t.Fatal("feature schema still creates review_report_reuses")
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

func TestAgentToolResultTraceSchemaPreservesAuthoritativeResults(t *testing.T) {
	statements := strings.Join(mysqlSchema[GroupQARun], "\n")
	for _, required := range []string{
		"prompt_content       MEDIUMTEXT",
		"authoritative_sha256 CHAR(64) NOT NULL",
		"artifact_content     LONGBLOB NULL",
		"artifact_content_type VARCHAR(128) NULL",
		"UNIQUE KEY uniq_agent_step_artifact (artifact_id_key)",
	} {
		if !strings.Contains(statements, required) {
			t.Fatalf("managed QA run schema missing %q", required)
		}
	}
	if strings.Contains(statements, "result_summary") {
		t.Fatal("managed QA run schema still persists a result preview")
	}
}
