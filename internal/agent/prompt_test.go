package agent

import (
	"strings"
	"testing"
)

// resolveIdentity is the persona selector: an RBAC role prompt when present,
// else the built-in defaultIdentity fallback. These cases pin both branches so
// a role-less request never answers personaless.
func TestResolveIdentity(t *testing.T) {
	cases := []struct {
		name       string
		rolePrompt string
		want       string
	}{
		{"empty falls back to default", "", defaultIdentity},
		{"whitespace-only falls back to default", "  \n\t ", defaultIdentity},
		{"role prompt wins", "## Identity\n- Role: SRE", "## Identity\n- Role: SRE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveIdentity(c.rolePrompt); got != c.want {
				t.Fatalf("resolveIdentity(%q) = %q, want %q", c.rolePrompt, got, c.want)
			}
		})
	}
}

// The base systemPrompt must NOT carry the persona (that is now per-role), but
// must keep the role-neutral discipline — otherwise stripping Identity could
// silently drop the anti-fabrication rules that must never vary by role.
func TestSystemPromptShape(t *testing.T) {
	if strings.Contains(systemPrompt, "## Identity") {
		t.Error("systemPrompt still contains ## Identity — persona must be per-role, not baked into the base")
	}
	for _, must := range []string{"## Core Mission", "## Core Rules", "Ground every claim", "Match the user's language"} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing role-neutral discipline %q", must)
		}
	}
	if !strings.Contains(defaultIdentity, "## Identity") {
		t.Error("defaultIdentity must contain the ## Identity block")
	}
	if idx := strings.Index(systemPrompt, rolePromptPlaceholder); idx < 0 || idx > strings.Index(systemPrompt, "## Core Rules") {
		t.Fatal("systemPrompt must place the role slot before Core Rules")
	}
}

func TestSystemPromptRequiresEvidenceForPhysicalResourceNames(t *testing.T) {
	for _, required := range []string{"database tables", "search indices", "must appear verbatim", "does not prove where user data is stored"} {
		if !strings.Contains(systemPrompt, required) {
			t.Fatalf("systemPrompt missing physical-resource evidence rule %q", required)
		}
	}
}

func TestComposeSystemPromptReplacesRoleSlotBeforeRules(t *testing.T) {
	for name, template := range map[string]string{
		"core":   systemPrompt,
		"direct": directAgentSystemPrompt,
		"web":    webAgentSystemPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			got := composeSystemPrompt(template, "## Identity\n- Role: SRE")
			if strings.Contains(got, rolePromptPlaceholder) {
				t.Fatalf("role placeholder was not replaced: %s", got)
			}
			roleAt := strings.Index(got, "## Identity\n- Role: SRE")
			rulesAt := strings.Index(got, "Rules:")
			if rulesAt < 0 {
				rulesAt = strings.Index(got, "## Core Rules")
			}
			if roleAt < 0 || rulesAt < 0 || roleAt > rulesAt {
				t.Fatalf("role must precede rules: role=%d rules=%d", roleAt, rulesAt)
			}
		})
	}
	defaulted := composeSystemPrompt(webAgentSystemPrompt, "")
	if !strings.Contains(defaulted, defaultIdentity) {
		t.Fatal("empty role prompt must use default identity")
	}
}

func TestSystemPromptDoesNotAllowInferenceToCompleteRuntimeChains(t *testing.T) {
	for _, must := range []string{
		"Inference must never create a missing execution hop",
		"Keep distinct entry points and execution branches separate",
		"investigate only the missing fact",
		"verified nodes and edges only",
	} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing aligned chain rule %q", must)
		}
	}
	if strings.Contains(systemPrompt, "Run the DependencyChain for each found service") {
		t.Fatal("systemPrompt still requires broad dependency fan-out")
	}
}

func TestSystemPromptUsesClaimSpecificEvidence(t *testing.T) {
	for _, must := range []string{
		"Match evidence to the claim",
		"implementation evidence for executed behavior",
		"runtime evidence for observed events",
		"schema evidence for stored shape",
		"Do not apply one fixed evidence hierarchy",
		"Internal integration code",
	} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing general evidence rule %q", must)
		}
	}
	if strings.Contains(systemPrompt, " > ") {
		t.Fatal("systemPrompt contains a fixed global evidence ranking")
	}
}

func TestSystemPromptStaysCompact(t *testing.T) {
	const maxWords = 1500
	if words := len(strings.Fields(systemPrompt)); words > maxWords {
		t.Fatalf("systemPrompt has %d words, want at most %d", words, maxWords)
	}
}

func TestSystemPromptKeepsDiagramsReadable(t *testing.T) {
	for _, must := range []string{
		"at most 8 nodes",
		"Split larger or multi-layer views",
		"node labels to one short phrase",
		"explanations outside the nodes",
		"inline text for simple linear chains",
	} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing diagram readability rule %q", must)
		}
	}
}

func TestAgentToolPromptDoesNotRepeatCoreRoleOrEvidenceRules(t *testing.T) {
	for _, forbidden := range []string{
		"You are **Nasuta**",
		"## Core Rules",
		"Distinguish client entry points",
		"Propose the smallest fix",
		"Admit missing evidence",
	} {
		if strings.Contains(agentToolPrompt, forbidden) {
			t.Errorf("agentToolPrompt repeats core prompt content %q", forbidden)
		}
	}
	if !strings.HasPrefix(agentSystemPrompt, systemPrompt) {
		t.Fatal("agentSystemPrompt must compose the core prompt before tool policy")
	}
	if got := strings.Count(agentSystemPrompt, "## Core Rules"); got != 1 {
		t.Fatalf("agentSystemPrompt contains %d Core Rules sections, want 1", got)
	}
	for _, must := range []string{"## Agent Tool Policy", "Pick the tool that matches the intent", "Converge after a targeted lookup"} {
		if !strings.Contains(agentToolPrompt, must) {
			t.Errorf("agentToolPrompt missing tool policy %q", must)
		}
	}
}

func TestInternalAnswerInstructionsPreserveQuestionLanguage(t *testing.T) {
	for name, instruction := range map[string]string{
		"forced conclusion": forceConclusionInstruction,
		"continuation":      continuationInstruction,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(instruction, "same natural language as the original user question") {
				t.Fatalf("instruction can change the answer language: %q", instruction)
			}
		})
	}
}
