package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestBuiltinToolDescriptionsKeepEvidenceBoundariesDistinct(t *testing.T) {
	descriptions := make(map[string]string)
	for _, candidate := range builtinTools(&Service{}, config.Config{}, nil) {
		descriptions[string(candidate.ID)] = candidate.Description
	}
	checks := map[string][]string{
		"get_service":     {"metadata", "does not establish dependencies"},
		"trace_deps":      {"service-level", "does not establish method-level"},
		"list_apis":       {"complete API routes", "class-level and method-level"},
		"search_code":     {"fallback", "not as proof", "complete API route"},
		"get_symbol":      {"exact definitions", "does not establish its callers"},
		"trace_calls":     {"method-level callers and callees", "not proof of complete service dependencies"},
		"search_runbooks": {"operational runbooks", "do not prove current runtime state"},
		"check_docs":      {"documentation coverage", "does not establish runtime"},
		"index_stats":     {"health and summary counts", "does not establish business behavior"},
	}
	for name, fragments := range checks {
		description := descriptions[name]
		if description == "" {
			t.Errorf("missing built-in tool %q", name)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s description missing %q: %q", name, fragment, description)
			}
		}
	}
}

func TestSessionTurnDetailsToolIsPrivateAndRequiresCurrentReference(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sessions := memory.NewSessionStore(db)
	candidate := sessionTurnDetailsTool(sessions)
	if !candidate.MCPHidden {
		t.Fatal("session detail tool must not enter MCP")
	}
	if _, err := candidate.Handler.Execute(context.Background(), tool.Arguments{"ref": "cmp-1"}); err == nil {
		t.Fatal("detail tool accepted a call without session scope")
	}
	ctx := withSessionToolScope(context.Background(), ConversationContext{
		SessionID: "session-1", CompactedThroughTurn: 1,
	}, 42)
	mock.ExpectQuery(`SELECT ref,session_id,user_id,run_id,text,turn_number,summary_text,source_tokens,retained_tokens.*FROM qa_turn_contexts`).
		WithArgs("cmp-current", "session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"ref", "session_id", "user_id", "run_id", "text", "turn_number", "summary_text", "source_tokens", "retained_tokens"}).
			AddRow("cmp-current", "session-1", 42, "run-1", "detail", 1, "summary", 50, 10))
	if _, err := candidate.Handler.Execute(ctx, tool.Arguments{"ref": "cmp-current"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWithoutToolRemovesSessionDetailsFromRunSnapshot(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewRegistry(&Service{}, config.Config{}, memory.NewSessionStore(db))
	snapshot := registry.Snapshot(tool.ReadPolicy())
	if _, ok := snapshot.Get("get_session_turn_details"); !ok {
		t.Fatal("registered detail tool missing")
	}
	filtered := withoutTool(snapshot, "get_session_turn_details")
	if _, ok := filtered.Get("get_session_turn_details"); ok {
		t.Fatal("detail tool remained visible without a compaction reference")
	}
	for _, published := range snapshot.MCPTools() {
		if published.ID == "get_session_turn_details" {
			t.Fatal("detail tool was published over MCP")
		}
	}
}

func TestQueryRelationsRegistersOnlyWhenOntologyIsAvailable(t *testing.T) {
	without := builtinTools(&Service{}, config.Config{}, nil)
	for _, candidate := range without {
		if candidate.ID == "query_relations" {
			t.Fatal("query_relations registered without ontology service")
		}
	}

	svc := &Service{ontology: ontology.NewService(staticOntologyRepository{})}
	tools := builtinTools(svc, config.Config{}, nil)
	var relation *Tool
	for i := range tools {
		if tools[i].ID == "query_relations" {
			relation = &tools[i]
			break
		}
	}
	if relation == nil {
		t.Fatal("query_relations was not registered")
	}
	result, err := relation.Handler.Execute(context.Background(), map[string]any{
		"entity": "orders", "relations": []any{"depends_on"}, "max_depth": 2, "max_nodes": 20, "max_fanout": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"name": "payments"`) || !strings.Contains(result.Content, `"depth": 1`) {
		t.Fatalf("tool output = %s", result.Content)
	}
}

func TestTraceDepsUsesOntologyFacts(t *testing.T) {
	svc := &Service{ontology: ontology.NewService(staticOntologyRepository{})}
	var trace *Tool
	for _, candidate := range builtinTools(svc, config.Config{}, nil) {
		if candidate.ID == "trace_deps" {
			trace = &candidate
			break
		}
	}
	if trace == nil {
		t.Fatal("trace_deps was not registered")
	}
	result, err := trace.Handler.Execute(context.Background(), map[string]any{
		"service": "orders", "direction": "both", "depth": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"service": "orders"`, `"to": "payments"`, `"type": "http"`, `"truncated": false`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("trace_deps output missing %s: %s", want, result.Content)
		}
	}
}

func TestBuiltinToolReturnsExecutionErrorWhenBackendIsUnavailable(t *testing.T) {
	var symbol *Tool
	for _, candidate := range builtinTools(&Service{}, config.Config{}, nil) {
		if candidate.ID == "get_symbol" {
			symbol = &candidate
			break
		}
	}
	if symbol == nil {
		t.Fatal("get_symbol was not registered")
	}
	result, err := symbol.Handler.Execute(context.Background(), map[string]any{"query": "OrderService"})
	if err == nil {
		t.Fatalf("expected backend error, got result %q", result.Content)
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("error = %v, want workspace root failure", err)
	}
}

type staticOntologyRepository struct{}

func (staticOntologyRepository) Resolve(context.Context, ontology.ResolveQuery) (ontology.ResolveResult, error) {
	return ontology.ResolveResult{
		Generation: "test",
		Entities:   []ontology.EntityRef{{ID: "orders", Class: ontology.ClassService, Name: "orders"}},
	}, nil
}

func (staticOntologyRepository) EntitiesByID(context.Context, ontology.EntityQuery) ([]ontology.EntityRef, error) {
	return []ontology.EntityRef{{ID: "payments", Class: ontology.ClassService, Name: "payments"}}, nil
}

func (staticOntologyRepository) Neighbors(context.Context, ontology.NeighborQuery) ([]ontology.Fact, bool, error) {
	return []ontology.Fact{{
		ID: "dependency", SubjectID: "orders", Predicate: ontology.PredicateDependsOn, ObjectID: "payments",
		Qualifiers: map[string]string{"protocol": "http"}, Confidence: 0.9,
		Evidence: []ontology.Evidence{{Path: "repos/team/orders/client.go", Line: 8, Source: ontology.EvidenceSourceCodeScan}},
	}}, false, nil
}

func (staticOntologyRepository) Stats(context.Context) (ontology.Stats, error) {
	return ontology.Stats{Generation: "test"}, nil
}
