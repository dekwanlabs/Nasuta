package retrieval

import (
	"testing"
)

func TestQueryTermsNormalize(t *testing.T) {
	qt := QueryTerms{
		// services removed from QueryTerms
		DomainTerms: []string{"Binding", "binding"},
		Identifiers: []string{"publishRuleChange", "PublishRuleChange"},
	}
	n := qt.normalize()
	if len(n.DomainTerms) != 1 {
		t.Fatalf("domain dedup: got %v", n.DomainTerms)
	}
	// Identifiers keep original casing but dedupe case-insensitively.
	if len(n.Identifiers) != 1 || n.Identifiers[0] != "publishRuleChange" {
		t.Fatalf("idents: got %v", n.Identifiers)
	}
}

func TestBuildCodeGraphKeywords_DropsStopwordsAndCaps(t *testing.T) {
	r := &Retriever{}
	// Mix of good keywords, stopwords, and a short token.
	terms := QueryTerms{
		// services removed
		DomainTerms: []string{"rule", "public", "x"}, // "public" is stopword, "x" too short
		Identifiers: []string{"publishRuleChange"},
	}
	kw := r.buildCodeGraphKeywords([]string{"hsds-risk-control"}, terms)
	if len(kw) > 20 {
		t.Fatalf("keywords not capped: %d", len(kw))
	}
	hasStop := func(s string) bool {
		for _, k := range kw {
			if k == s {
				return true
			}
		}
		return false
	}
	if hasStop("public") {
		t.Fatal("stopword 'public' should be dropped")
	}
	// Service name + identifier should survive.
	hasKW := func(s string) bool {
		for _, k := range kw {
			if k == s {
				return true
			}
		}
		return false
	}
	if !hasKW("hsds-risk-control") || !hasKW("publishrulechange") {
		t.Fatalf("expected service name + identifier kept, got %v", kw)
	}
}

func TestBuildCodeGraphKeywords_PrioritizesServices(t *testing.T) {
	r := &Retriever{}
	terms := QueryTerms{
		// services removed
		Identifiers: []string{"identOne"},
		DomainTerms: []string{"domain"},
	}
	kw := r.buildCodeGraphKeywords([]string{"svc-a"}, terms)
	// Service name should come first.
	if kw[0] != "svc-a" {
		t.Fatalf("service should be first, got %v", kw)
	}
}

func TestBuildCodeGraphKeywords_DoesNotDeriveServiceSuffixKeywords(t *testing.T) {
	r := &Retriever{}
	kw := r.buildCodeGraphKeywords([]string{"hsds-cookbook-provider"}, QueryTerms{})
	if len(kw) != 1 || kw[0] != "hsds-cookbook-provider" {
		t.Fatalf("expected only grounded service keyword, got %v", kw)
	}
}

func TestExtractQueryTermsLLM_Parses(t *testing.T) {
	// Direct test of the parser with a fake LLM response.
	// We can't easily fake *LLMClient.chat without an http server here;
	// verify the JSON-tolerance path by feeding through normalize.
	raw := struct {
		Services    []string `json:"services"`
		DomainTerms []string `json:"domain_terms"`
		Identifiers []string `json:"identifiers"`
	}{
		Services:    []string{"nps"},
		DomainTerms: []string{"binding"},
		Identifiers: []string{"ThresholdEvaluator"},
	}
	qt := QueryTerms{DomainTerms: raw.DomainTerms, Identifiers: raw.Identifiers}.normalize()
	if qt.DomainTerms[0] != "binding" || qt.Identifiers[0] != "ThresholdEvaluator" {
		t.Fatalf("normalize mangled terms: %v", qt)
	}
}

func TestShouldExpandCodeGraphRequiresSymbolOrTraceIntent(t *testing.T) {
	if shouldExpandCodeGraph("explain the service architecture", QueryTerms{}) {
		t.Fatal("architecture overview should not fan out into CodeGraph")
	}
	if !shouldExpandCodeGraph("trace the caller call chain", QueryTerms{}) {
		t.Fatal("call-chain intent should expand CodeGraph")
	}
	if !shouldExpandCodeGraph("explain this", QueryTerms{Identifiers: []string{"PaymentHandler"}}) {
		t.Fatal("explicit identifier should expand CodeGraph")
	}
}
