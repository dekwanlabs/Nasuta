package domain

import (
	"reflect"
	"testing"
)

func TestCanonicalEntityIDsNormalizesAndDeduplicates(t *testing.T) {
	got := CanonicalEntityIDs([]string{
		" PaymentHandler.handle() ",
		"paymenthandler.handle",
		"HS-USER-SERVICE",
		"hs-user-service",
		"",
	})
	want := []string{"paymenthandler.handle", "hs-user-service"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entities = %v, want %v", got, want)
	}
}

func TestCanonicalQuestionEntitiesUsesCanonicalIdentity(t *testing.T) {
	got := CanonicalQuestionEntities(
		"Compare PaymentHandler.handle() with HS-USER-SERVICE and trace-123.",
	)
	want := []string{
		"paymenthandler.handle",
		"hs-user-service",
		"trace-123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entities = %v, want %v", got, want)
	}
}

func TestCanonicalEntityIDsAreBounded(t *testing.T) {
	got := CanonicalEntityIDs([]string{
		"A-1", "B-2", "C-3", "D-4", "E-5", "F-6", "G-7", "H-8", "I-9",
	})
	if len(got) != MaxCanonicalEntities {
		t.Fatalf("entities = %v, want %d entries", got, MaxCanonicalEntities)
	}
}
