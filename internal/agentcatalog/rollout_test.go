package agentcatalog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func publishRolloutDefinitions(t *testing.T, catalog *Catalog) {
	t.Helper()
	if err := catalog.Publish([]agentapi.Definition{
		testDefinition(1, "candidate"),
		testDefinition(2, "default"),
	}); err != nil {
		t.Fatalf("publish definitions: %v", err)
	}
}

func TestCatalogResolveForUsesStableRolloutSelection(t *testing.T) {
	catalog := testCatalog(t)
	publishRolloutDefinitions(t, catalog)
	rule, err := catalog.SetRollout(
		context.Background(), "qa.answerer", 1, rolloutBucketCount,
		"rollout-2026-08", true, 7,
	)
	if err != nil {
		t.Fatalf("SetRollout: %v", err)
	}

	first, firstSelection, err := catalog.ResolveFor(
		agentapi.DefinitionRef{ID: "qa.answerer"},
		StableSelectionKey(42, "session-1"),
	)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	second, secondSelection, err := catalog.ResolveFor(
		agentapi.DefinitionRef{ID: "qa.answerer"},
		StableSelectionKey(42, "session-2"),
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
		agentapi.DefinitionRef{ID: "qa.answerer"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if defaultDefinition.Version != 2 {
		t.Fatalf("Resolve applied rollout and returned version %d", defaultDefinition.Version)
	}

	explicit, explicitSelection, err := catalog.ResolveFor(
		agentapi.DefinitionRef{ID: "qa.answerer", Version: 2},
		"",
	)
	if err != nil {
		t.Fatalf("ResolveFor explicit version: %v", err)
	}
	if explicit.Version != 2 || explicitSelection.Reason != "explicit_version" {
		t.Fatalf("explicit resolution = %d/%+v", explicit.Version, explicitSelection)
	}
}

func TestCatalogRolloutHonorsZeroPercentAndRequiresStableKey(t *testing.T) {
	catalog := testCatalog(t)
	publishRolloutDefinitions(t, catalog)
	if _, err := catalog.SetRollout(
		context.Background(), "qa.answerer", 1, 0, "zero-percent", true, 7,
	); err != nil {
		t.Fatalf("SetRollout: %v", err)
	}
	definition, selection, err := catalog.ResolveFor(
		agentapi.DefinitionRef{ID: "qa.answerer"}, "session:stable",
	)
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if definition.Version != 2 || selection.Reason != "rollout_default" ||
		selection.PercentageBasisPoints != 0 {
		t.Fatalf("zero-percent resolution = %d/%+v", definition.Version, selection)
	}

	_, _, err = catalog.ResolveFor(
		agentapi.DefinitionRef{ID: "qa.answerer"}, "",
	)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing stable key error = %v, want ErrInvalid", err)
	}
}

func TestCatalogRolloutRejectsUnavailableCandidate(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		catalog := testCatalog(t)
		publishRolloutDefinitions(t, catalog)
		if _, err := catalog.SetRollout(
			context.Background(), "qa.answerer", 1, rolloutBucketCount,
			"disabled-candidate", true, 7,
		); err != nil {
			t.Fatalf("SetRollout: %v", err)
		}
		if err := catalog.SetActive(
			context.Background(), "qa.answerer", 1, false, 8,
		); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		_, _, err := catalog.ResolveFor(
			agentapi.DefinitionRef{ID: "qa.answerer"}, "user:42",
		)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("disabled candidate error = %v, want ErrConflict", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		catalog := testCatalog(t)
		publishRolloutDefinitions(t, catalog)
		rule, err := prepareRolloutRule(RolloutRule{
			AgentID: "qa.answerer", RuleVersion: 1, CandidateVersion: 99,
			PercentageBPS: rolloutBucketCount, Salt: "missing-candidate", Active: true,
		})
		if err != nil {
			t.Fatalf("prepare rule: %v", err)
		}
		next := cloneState(catalog.state.Load())
		next.rollouts[rule.AgentID] = rule
		catalog.state.Store(next)

		_, _, err = catalog.ResolveFor(
			agentapi.DefinitionRef{ID: "qa.answerer"}, "user:42",
		)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("missing candidate error = %v, want ErrConflict", err)
		}
	})
}

func TestCatalogSetRolloutValidatesInputAndAppendsAudit(t *testing.T) {
	catalog := testCatalog(t)
	publishRolloutDefinitions(t, catalog)
	tests := []struct {
		name       string
		id         string
		candidate  int64
		percentage int
		salt       string
	}{
		{name: "id", candidate: 1, percentage: 1, salt: "salt"},
		{name: "candidate", id: "qa.answerer", percentage: 1, salt: "salt"},
		{name: "negative percentage", id: "qa.answerer", candidate: 1, percentage: -1, salt: "salt"},
		{name: "excess percentage", id: "qa.answerer", candidate: 1, percentage: rolloutBucketCount + 1, salt: "salt"},
		{name: "salt", id: "qa.answerer", candidate: 1, percentage: 1, salt: "  "},
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
		context.Background(), " qa.answerer ", 1, 2500, " first ", true, 7,
	)
	if err != nil {
		t.Fatalf("enable rollout: %v", err)
	}
	disabled, err := catalog.SetRollout(
		context.Background(), "qa.answerer", 1, 2500, "second", false, 8,
	)
	if err != nil {
		t.Fatalf("disable rollout: %v", err)
	}
	if enabled.AgentID != "qa.answerer" || enabled.Salt != "first" ||
		disabled.RuleVersion != enabled.RuleVersion+1 {
		t.Fatalf("rules = enabled %+v disabled %+v", enabled, disabled)
	}
	events, err := catalog.ListRolloutAudit(
		context.Background(), "qa.answerer", 0, 10,
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

func TestRolloutRuleHashIsDeterministicAndValidated(t *testing.T) {
	base := RolloutRule{
		AgentID: "qa.answerer", RuleVersion: 3, CandidateVersion: 2,
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

func TestStableSelectionKeyPrefersUserAndTrimsSession(t *testing.T) {
	if got := StableSelectionKey(42, "session-1"); got != "user:42" {
		t.Fatalf("user stable key = %q", got)
	}
	if got := StableSelectionKey(0, " session-1 "); got != "session:session-1" {
		t.Fatalf("session stable key = %q", got)
	}
	if got := StableSelectionKey(0, "   "); got != "" {
		t.Fatalf("empty stable key = %q", got)
	}
}
