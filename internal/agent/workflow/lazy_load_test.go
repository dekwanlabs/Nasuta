package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestCatalogAttachStoreLoadsWorkingSetAndLazilyHydratesHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	schemas := testSchemaRegistry(t)
	createdAt := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	history := prepareWorkflowDefinition(t, schemas, 1, "history")
	defaultDefinition := prepareWorkflowDefinition(t, schemas, 2, "default")
	candidate := prepareWorkflowDefinition(t, schemas, 3, "candidate")
	defaultRaw, _ := json.Marshal(defaultDefinition)
	candidateRaw, _ := json.Marshal(candidate)
	historyRaw, _ := json.Marshal(history)
	rule, err := prepareRolloutRule(RolloutRule{
		WorkflowID: "delivery.review", RuleVersion: 1, CandidateVersion: 3,
		PercentageBPS: rolloutBucketCount, Salt: "all", Active: true,
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("prepare rollout: %v", err)
	}

	mock.ExpectQuery(`(?s)SELECT\s+definition_json,content_hash,active,is_default,created_by,created_at\s+FROM workflow_definitions WHERE is_default=1 ORDER BY id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"definition_json", "content_hash", "active", "is_default",
			"created_by", "created_at",
		}).AddRow(
			defaultRaw, defaultDefinition.ContentHash, true, true, int64(7), createdAt,
		))
	mock.ExpectQuery(`(?s)SELECT id,MAX\(version\)\s+FROM workflow_definitions GROUP BY id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "version",
		}).AddRow("delivery.review", int64(3)))
	mock.ExpectQuery(`(?s)SELECT\s+subject_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,\s+active,created_by,created_at\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow'\s+ORDER BY subject_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_id", "rule_version", "candidate_version", "percentage_bps",
			"salt", "rule_hash", "active", "created_by", "created_at",
		}).AddRow(
			rule.WorkflowID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(7), createdAt,
		))
	mock.ExpectQuery(`(?s)SELECT\s+definition_json,content_hash,active,is_default,created_by,created_at\s+FROM workflow_definitions WHERE id=\? AND version=\? LIMIT 1`).
		WithArgs("delivery.review", int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{
			"definition_json", "content_hash", "active", "is_default",
			"created_by", "created_at",
		}).AddRow(candidateRaw, candidate.ContentHash, true, false, int64(7), createdAt))

	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	if err := catalog.AttachStore(context.Background(), store); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	if catalog.MaxVersion() != 3 || len(catalog.List()) != 2 {
		t.Fatalf(
			"working set max_version=%d definitions=%d, want 3 and 2",
			catalog.MaxVersion(), len(catalog.List()),
		)
	}
	selected, _, err := catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review"},
		"stable",
	)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if selected.Version != 3 {
		t.Fatalf("rollout version = %d, want 3", selected.Version)
	}

	mock.ExpectQuery(`(?s)SELECT\s+definition_json,content_hash,active,is_default,created_by,created_at\s+FROM workflow_definitions WHERE id=\? AND version=\? LIMIT 1`).
		WithArgs("delivery.review", int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{
			"definition_json", "content_hash", "active", "is_default",
			"created_by", "created_at",
		}).AddRow(historyRaw, history.ContentHash, true, false, int64(7), createdAt))
	resolved, err := catalog.Resolve(DefinitionRef{
		ID: "delivery.review", Version: 1,
	})
	if err != nil {
		t.Fatalf("Resolve history: %v", err)
	}
	if resolved.Purpose != "history" || len(catalog.List()) != 3 {
		t.Fatalf("resolved history = %+v, cached=%d", resolved, len(catalog.List()))
	}
	if _, err := catalog.Resolve(DefinitionRef{
		ID: "delivery.review", Version: 1,
	}); err != nil {
		t.Fatalf("Resolve cached history: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreListsDefinitionsReferencedByUnfinishedRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mock.ExpectQuery(`(?s)SELECT DISTINCT\s+workflow_id,workflow_version\s+FROM workflow_runs\s+WHERE status IN \(\?,\?\)\s+ORDER BY workflow_id,workflow_version`).
		WithArgs(RunRunning, RunWaitingHuman).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_id", "workflow_version",
		}).
			AddRow("delivery.review", int64(2)).
			AddRow("investigation.default", int64(4)))

	refs, err := store.ListUnfinishedDefinitionRefs(context.Background())
	if err != nil {
		t.Fatalf("ListUnfinishedDefinitionRefs: %v", err)
	}
	if len(refs) != 2 ||
		refs[0] != (DefinitionRef{ID: "delivery.review", Version: 2}) ||
		refs[1] != (DefinitionRef{ID: "investigation.default", Version: 4}) {
		t.Fatalf("refs = %+v", refs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func prepareWorkflowDefinition(
	t *testing.T,
	schemas *agentapi.SchemaRegistry,
	version int64,
	purpose string,
) Definition {
	t.Helper()
	definition := testWorkflow()
	definition.Version = version
	definition.Purpose = purpose
	prepared, err := Prepare(definition, schemas)
	if err != nil {
		t.Fatalf("prepare workflow definition: %v", err)
	}
	return prepared
}
