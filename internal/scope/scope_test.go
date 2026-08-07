package scope

import (
	"strings"
	"testing"
)

func TestVocabularySeparatesRuntimeAndDomainScopes(t *testing.T) {
	read, ok := Lookup(KnowledgeRead)
	if !ok || !read.AgentRuntime || read.SideEffect || read.Owner != OwnerAgentRuntime {
		t.Fatalf("knowledge read metadata = %+v, found=%v", read, ok)
	}
	write, ok := Lookup(KnowledgeWrite)
	if !ok || !write.AgentRuntime || !write.SideEffect || write.Owner != OwnerAgentRuntime {
		t.Fatalf("knowledge write metadata = %+v, found=%v", write, ok)
	}
	delivery, ok := Lookup(FeatureDelivery)
	if !ok || delivery.AgentRuntime || !delivery.SideEffect ||
		delivery.Owner != OwnerFeatureDelivery {
		t.Fatalf("feature delivery metadata = %+v, found=%v", delivery, ok)
	}
}

func TestValidateRejectsUnknownAndDuplicateScopes(t *testing.T) {
	for _, test := range []struct {
		name   string
		scopes []string
		want   string
	}{
		{name: "unknown", scopes: []string{"approval.write"}, want: "not registered"},
		{name: "duplicate", scopes: []string{KnowledgeRead, KnowledgeRead}, want: "duplicated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(test.scopes); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateAgentRuntimeRejectsDomainScope(t *testing.T) {
	err := ValidateAgentRuntime([]string{FeatureDelivery})
	if err == nil || !strings.Contains(err.Error(), "not supported by the agent runtime") {
		t.Fatalf("ValidateAgentRuntime error = %v", err)
	}
}

func TestEnsureSubsetAndSideEffects(t *testing.T) {
	if err := EnsureSubset(
		[]string{KnowledgeRead},
		[]string{KnowledgeRead, KnowledgeWrite},
	); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSubset(
		[]string{KnowledgeRead, KnowledgeWrite},
		[]string{KnowledgeRead},
	); err == nil || !strings.Contains(err.Error(), KnowledgeWrite) {
		t.Fatalf("EnsureSubset error = %v", err)
	}
	if HasSideEffect([]string{KnowledgeRead}) {
		t.Fatal("read-only scope was classified as a side effect")
	}
	if !HasSideEffect([]string{FeatureDelivery}) || !HasSideEffect([]string{KnowledgeWrite}) {
		t.Fatal("write-capable scope was not classified as a side effect")
	}
}
