package domain

import (
	"fmt"
	"strings"
)

// EvidenceSources is the set of evidence capabilities selected for one run.
type EvidenceSources uint8

const (
	Memory EvidenceSources = 1 << iota
	Internal
	Web
)

const AllEvidence = Memory | Internal | Web

// EvidencePlan is the sole execution policy for evidence access and tools.
type EvidencePlan struct {
	Sources EvidenceSources
}

func DirectPlan() EvidencePlan { return EvidencePlan{} }

func (plan EvidencePlan) Direct() bool { return plan.Sources == 0 }

func (plan EvidencePlan) Has(source EvidenceSources) bool {
	return plan.Sources&source != 0
}

func (plan EvidencePlan) Valid() bool {
	return plan.Sources&^AllEvidence == 0
}

func (plan EvidencePlan) SourceNames() []string {
	names := make([]string, 0, 3)
	if plan.Has(Memory) {
		names = append(names, "memory")
	}
	if plan.Has(Internal) {
		names = append(names, "internal")
	}
	if plan.Has(Web) {
		names = append(names, "web")
	}
	return names
}

func (plan EvidencePlan) String() string {
	if plan.Direct() {
		return "direct"
	}
	return strings.Join(plan.SourceNames(), "+")
}

// ParseEvidencePlan parses an explicit API/UI source selection.
func ParseEvidencePlan(value string) (EvidencePlan, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "direct":
		return DirectPlan(), nil
	case "memory":
		return EvidencePlan{Sources: Memory}, nil
	case "internal":
		return EvidencePlan{Sources: Internal}, nil
	case "web":
		return EvidencePlan{Sources: Web}, nil
	case "all":
		return EvidencePlan{Sources: AllEvidence}, nil
	default:
		return EvidencePlan{}, fmt.Errorf("unknown evidence source mode %q", value)
	}
}

type DecisionOrigin string

const (
	Model    DecisionOrigin = "model"
	Rule     DecisionOrigin = "rule"
	Explicit DecisionOrigin = "explicit"
	Fallback DecisionOrigin = "fallback"
)

// PlanDecision keeps planning diagnostics out of the executable plan.
type PlanDecision struct {
	Plan       EvidencePlan
	Confidence float64
	Origin     DecisionOrigin
}

// InternalFallbackDecision conservatively enables workspace evidence.
func InternalFallbackDecision() PlanDecision {
	return PlanDecision{
		Plan:       EvidencePlan{Sources: Internal},
		Confidence: 0,
		Origin:     Fallback,
	}
}
