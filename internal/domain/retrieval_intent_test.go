package domain

import "testing"

func TestResolveRetrievalIntentUnifiesFlowSignals(t *testing.T) {
	resolution := ResolveRetrievalIntent(
		"这个方法的调用链是什么",
		RetrievalIntentSignals{Identifiers: []string{"PaymentHandler.handle"}},
	)
	if resolution.Intent.Kind != RetrievalFlow {
		t.Fatalf("intent = %q, want %q", resolution.Intent.Kind, RetrievalFlow)
	}
	if len(resolution.Intent.TargetEntities) != 1 ||
		resolution.Intent.TargetEntities[0] != "paymenthandler.handle" {
		t.Fatalf("target entities = %v", resolution.Intent.TargetEntities)
	}
}

func TestResolveRetrievalIntentFallsBackLocally(t *testing.T) {
	resolution := ResolveRetrievalIntent("告诉我这个项目的情况", RetrievalIntentSignals{})
	if resolution.Intent.Kind != RetrievalFocusedFact || resolution.Origin != IntentOriginFallback {
		t.Fatalf("resolution = %+v", resolution)
	}
}

func TestResolveRetrievalIntentDoesNotTreatEveryIdentifierAsFlow(t *testing.T) {
	resolution := ResolveRetrievalIntent(
		"PaymentHandler 这个类负责什么",
		RetrievalIntentSignals{Identifiers: []string{"PaymentHandler"}},
	)
	if resolution.Intent.Kind != RetrievalFocusedFact {
		t.Fatalf("intent = %q, want %q", resolution.Intent.Kind, RetrievalFocusedFact)
	}
}

func TestResolveRetrievalIntentBoundsTargetEntities(t *testing.T) {
	resolution := ResolveRetrievalIntent(
		"这些符号分别做什么",
		RetrievalIntentSignals{Identifiers: []string{
			"A", "a", "B", "C", "D", "E", "F", "G", "H", "I", "J",
		}},
	)
	if len(resolution.Intent.TargetEntities) != MaxCanonicalEntities {
		t.Fatalf("target entities = %v, want 8 unique bounded entries", resolution.Intent.TargetEntities)
	}
}
