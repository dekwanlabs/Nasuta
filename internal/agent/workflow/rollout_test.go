package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func publishWorkflowRolloutDefinitions(t *testing.T, catalog *Catalog) {
	t.Helper()
	version1 := testWorkflow()
	version1.Purpose = "Run the candidate review workflow."
	version2 := testWorkflow()
	version2.Version = 2
	version2.Purpose = "Run the default review workflow."
	if err := catalog.Publish([]WorkflowDefinition{version1, version2}); err != nil {
		t.Fatalf("publish workflow definitions: %v", err)
	}
}

func TestWorkflowCatalogResolveForUsesStableRolloutSelection(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	publishWorkflowRolloutDefinitions(t, catalog)
	rule, err := catalog.SetRollout(
		context.Background(), "delivery.review", 1, rolloutBucketCount,
		"rollout-2026-08", true, 7,
	)
	if err != nil {
		t.Fatalf("SetRollout: %v", err)
	}
	stableKey := StableSelectionKey(
		agentapi.Actor{UserID: 42, TenantID: "tenant-a"},
		"delivery.review",
	)
	first, firstSelection, err := catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review"}, stableKey,
	)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	second, secondSelection, err := catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review"}, stableKey,
	)
	if err != nil {
		t.Fatalf("ResolveFor again: %v", err)
	}
	if first.Version != 1 || second.Version != first.Version ||
		firstSelection != secondSelection {
		t.Fatalf(
			"unstable rollout: first=%d/%+v second=%d/%+v",
			first.Version, firstSelection, second.Version, secondSelection,
		)
	}
	if firstSelection.RuleVersion != rule.RuleVersion ||
		firstSelection.RuleHash != rule.RuleHash ||
		firstSelection.CandidateVersion != 1 ||
		firstSelection.PercentageBasisPoints != rolloutBucketCount ||
		firstSelection.Reason != "rollout_candidate" ||
		firstSelection.StableKeyHash == "" ||
		strings.Contains(firstSelection.StableKeyHash, "user:42") {
		t.Fatalf("selection = %+v", firstSelection)
	}

	defaultDefinition, err := catalog.Resolve(
		DefinitionRef{ID: "delivery.review"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if defaultDefinition.Version != 2 {
		t.Fatalf("Resolve applied rollout and returned version %d", defaultDefinition.Version)
	}

	explicit, explicitSelection, err := catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review", Version: 2}, "",
	)
	if err != nil {
		t.Fatalf("ResolveFor explicit version: %v", err)
	}
	if explicit.Version != 2 || explicitSelection.Reason != "explicit_version" {
		t.Fatalf("explicit resolution = %d/%+v", explicit.Version, explicitSelection)
	}
}

func TestWorkflowCatalogRolloutHonorsZeroPercentAndRequiresStableKey(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	publishWorkflowRolloutDefinitions(t, catalog)
	if _, err := catalog.SetRollout(
		context.Background(), "delivery.review", 1, 0,
		"zero-percent", true, 7,
	); err != nil {
		t.Fatalf("SetRollout: %v", err)
	}
	definition, selection, err := catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review"}, "scenario:stable",
	)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if definition.Version != 2 || selection.Reason != "rollout_default" ||
		selection.PercentageBasisPoints != 0 {
		t.Fatalf("zero-percent resolution = %d/%+v", definition.Version, selection)
	}

	_, _, err = catalog.ResolveFor(
		DefinitionRef{ID: "delivery.review"}, "",
	)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing stable key error = %v, want ErrInvalid", err)
	}
}

func TestWorkflowCatalogRolloutRejectsUnavailableVersion(t *testing.T) {
	t.Run("disabled candidate", func(t *testing.T) {
		catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
		publishWorkflowRolloutDefinitions(t, catalog)
		if _, err := catalog.SetRollout(
			context.Background(), "delivery.review", 1, rolloutBucketCount,
			"disabled-candidate", true, 7,
		); err != nil {
			t.Fatalf("SetRollout: %v", err)
		}
		if err := catalog.SetActive(
			context.Background(), "delivery.review", 1, false, 8,
		); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		_, _, err := catalog.ResolveFor(
			DefinitionRef{ID: "delivery.review"}, "user:42",
		)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("disabled candidate error = %v, want ErrConflict", err)
		}
	})

	t.Run("missing candidate", func(t *testing.T) {
		catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
		publishWorkflowRolloutDefinitions(t, catalog)
		rule, err := prepareRolloutRule(RolloutRule{
			WorkflowID: "delivery.review", RuleVersion: 1, CandidateVersion: 99,
			PercentageBPS: rolloutBucketCount, Salt: "missing-candidate", Active: true,
		})
		if err != nil {
			t.Fatalf("prepare rule: %v", err)
		}
		next := cloneCatalogState(catalog.state.Load())
		next.rollouts[rule.WorkflowID] = rule
		catalog.state.Store(next)

		_, _, err = catalog.ResolveFor(
			DefinitionRef{ID: "delivery.review"}, "user:42",
		)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("missing candidate error = %v, want ErrConflict", err)
		}
	})

	t.Run("disabled default", func(t *testing.T) {
		catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
		publishWorkflowRolloutDefinitions(t, catalog)
		if _, err := catalog.SetRollout(
			context.Background(), "delivery.review", 1, 0,
			"disabled-default", true, 7,
		); err != nil {
			t.Fatalf("SetRollout: %v", err)
		}
		next := cloneCatalogState(catalog.state.Load())
		key := definitionKey{id: "delivery.review", version: 2}
		record := next.records[key]
		record.Active = false
		next.records[key] = record
		catalog.state.Store(next)

		_, _, err := catalog.ResolveFor(
			DefinitionRef{ID: "delivery.review"}, "user:42",
		)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("disabled default error = %v, want ErrConflict", err)
		}
	})
}

func TestWorkflowCatalogSetRolloutValidatesInputAndAppendsAudit(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	publishWorkflowRolloutDefinitions(t, catalog)
	tests := []struct {
		name       string
		id         string
		candidate  int64
		percentage int
		salt       string
	}{
		{name: "id", candidate: 1, percentage: 1, salt: "salt"},
		{name: "candidate", id: "delivery.review", percentage: 1, salt: "salt"},
		{name: "negative percentage", id: "delivery.review", candidate: 1, percentage: -1, salt: "salt"},
		{name: "excess percentage", id: "delivery.review", candidate: 1, percentage: rolloutBucketCount + 1, salt: "salt"},
		{name: "salt", id: "delivery.review", candidate: 1, percentage: 1, salt: "  "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := catalog.SetRollout(
				context.Background(), test.id, test.candidate,
				test.percentage, test.salt, true, 7,
			)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("SetRollout error = %v, want ErrInvalid", err)
			}
		})
	}

	enabled, err := catalog.SetRollout(
		context.Background(), " delivery.review ", 1, 2500, " first ", true, 7,
	)
	if err != nil {
		t.Fatalf("enable rollout: %v", err)
	}
	disabled, err := catalog.SetRollout(
		context.Background(), "delivery.review", 1, 2500, "second", false, 8,
	)
	if err != nil {
		t.Fatalf("disable rollout: %v", err)
	}
	if enabled.WorkflowID != "delivery.review" || enabled.Salt != "first" ||
		disabled.RuleVersion != enabled.RuleVersion+1 {
		t.Fatalf("rules = enabled %+v disabled %+v", enabled, disabled)
	}
	events, err := catalog.ListRolloutAudit(
		context.Background(), "delivery.review", 0, 10,
	)
	if err != nil {
		t.Fatalf("ListRolloutAudit: %v", err)
	}
	if len(events) != 2 ||
		events[0].Action != "rollout_enabled" ||
		events[0].ActorUserID != 7 ||
		events[1].Action != "rollout_disabled" ||
		events[1].ActorUserID != 8 ||
		events[1].Seq <= events[0].Seq {
		t.Fatalf("rollout audit = %+v", events)
	}
}

func TestWorkflowRolloutRuleHashIsDeterministicAndValidated(t *testing.T) {
	base := RolloutRule{
		WorkflowID: "delivery.review", RuleVersion: 3, CandidateVersion: 2,
		PercentageBPS: 2500, Salt: "stable-hash", Active: true,
		CreatedBy: 7, CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	}
	first, err := prepareRolloutRule(base)
	if err != nil {
		t.Fatalf("prepare first rule: %v", err)
	}
	base.CreatedBy = 99
	base.CreatedAt = base.CreatedAt.Add(time.Hour)
	second, err := prepareRolloutRule(base)
	if err != nil {
		t.Fatalf("prepare second rule: %v", err)
	}
	if first.RuleHash == "" || first.RuleHash != second.RuleHash {
		t.Fatalf("rule hashes = %q and %q", first.RuleHash, second.RuleHash)
	}

	second.PercentageBPS++
	if _, err := prepareRolloutRule(second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mutated rule hash error = %v, want ErrInvalid", err)
	}
}

func TestWorkflowStableSelectionKeyPrefersUserAndIncludesScenario(t *testing.T) {
	actor := agentapi.Actor{UserID: 42, TenantID: "tenant-a"}
	if got := StableSelectionKey(actor, " delivery.review "); got !=
		"scenario:delivery.review\x00user:42" {
		t.Fatalf("user stable key = %q", got)
	}
	if got := StableSelectionKey(
		agentapi.Actor{TenantID: " tenant-a "}, "delivery.review",
	); got != "scenario:delivery.review\x00tenant:tenant-a" {
		t.Fatalf("tenant stable key = %q", got)
	}
	if got := StableSelectionKey(agentapi.Actor{}, " delivery.review "); got !=
		"scenario:delivery.review" {
		t.Fatalf("scenario stable key = %q", got)
	}
	if got := StableSelectionKey(agentapi.Actor{}, "   "); got != "" {
		t.Fatalf("empty stable key = %q", got)
	}
}
