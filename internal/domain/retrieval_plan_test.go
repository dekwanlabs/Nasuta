package types

import "testing"

func TestEvidencePlanBitmask(t *testing.T) {
	plan := EvidencePlan{Sources: Internal | Web}
	if plan.Direct() {
		t.Fatal("combined sources must not be direct")
	}
	if !plan.Has(Internal) || !plan.Has(Web) || plan.Has(Memory) {
		t.Fatalf("plan = %08b, want internal and web", plan.Sources)
	}
	if got := plan.String(); got != "internal+web" {
		t.Fatalf("String() = %q, want internal+web", got)
	}
}

func TestEvidencePlanDirectAndValidity(t *testing.T) {
	if plan := DirectPlan(); !plan.Direct() || !plan.Valid() || plan.String() != "direct" {
		t.Fatalf("zero plan = %+v, want valid direct plan", plan)
	}
	if plan := (EvidencePlan{Sources: 1 << 7}); plan.Valid() {
		t.Fatalf("unknown source bit %08b must be invalid", plan.Sources)
	}
}

func TestParseEvidencePlan(t *testing.T) {
	tests := []struct {
		value string
		want  EvidenceSources
	}{
		{value: "direct", want: 0},
		{value: "memory", want: Memory},
		{value: "internal", want: Internal},
		{value: "web", want: Web},
		{value: "all", want: AllEvidence},
	}
	for _, tt := range tests {
		plan, err := ParseEvidencePlan(tt.value)
		if err != nil || plan.Sources != tt.want {
			t.Errorf("ParseEvidencePlan(%q) = %+v, %v; want %08b", tt.value, plan, err, tt.want)
		}
	}
	if _, err := ParseEvidencePlan("auto"); err == nil {
		t.Fatal("auto is a planning request, not an evidence plan")
	}
}
