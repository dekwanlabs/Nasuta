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

func TestCanonicalEntitySpecsUseOpaqueIDsForNonCanonicalNames(t *testing.T) {
	got := CanonicalEntitySpecs([]EntitySpec{
		{
			ID: "本系统ai集成", Label: "我们AI", Role: "first_party_agent",
			Aliases: []string{"本系统 AI"},
		},
		{ID: "后端知识图谱项目", Label: "后端知识图谱", Role: "knowledge_backend"},
	})
	want := []EntitySpec{
		{
			ID:    "entity_75cbe4e1e8cee1d5879f90a9f477396b94d02f27a61407c4230618ddd2d16869",
			Label: "我们AI", Role: "first_party_agent",
			Aliases: []string{"本系统 AI", "本系统ai集成"},
		},
		{
			ID:    "entity_e51cb9b830126daecfeea0f171173af5b740ea94179f8ed074214c7e3631a646",
			Label: "后端知识图谱", Role: "knowledge_backend",
			Aliases: []string{"后端知识图谱项目"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entity specs = %#v, want %#v", got, want)
	}
	if got[0].ID != CanonicalEntityIDs([]string{"本系统ai集成"})[0] {
		t.Fatalf("entity ID mapping drifted: %#v", got[0])
	}
}

func TestIsSynthesizedEntityID(t *testing.T) {
	cases := map[string]bool{
		"entity_9a6a4c8c07579004fe1867dc472bb5688214542305113aab7020435df7246d47": true,
		"entity_18b7db54924fbd9017ceb18d492eb412363626b1c3ed5e6aea64a10777108e75": true,
		"tts":                   false,
		"rgb灯效":                 false,
		"paymenthandler.handle": false,
	}
	for candidate, want := range cases {
		if got := IsSynthesizedEntityID(candidate); got != want {
			t.Fatalf("IsSynthesizedEntityID(%q) = %v, want %v", candidate, got, want)
		}
	}
}
