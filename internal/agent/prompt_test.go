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
	for _, must := range []string{"## Core Mission", "## Core Rules", "Only retrieved context", "Match the user's language"} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing role-neutral discipline %q", must)
		}
	}
	if !strings.Contains(defaultIdentity, "## Identity") {
		t.Error("defaultIdentity must contain the ## Identity block")
	}
}

func TestSystemPromptDoesNotAllowInferenceToCompleteRuntimeChains(t *testing.T) {
	for _, must := range []string{
		"must never fill a missing runtime hop",
		"investigate that hop before the final answer",
		"Keep client entry points and alternate execution branches separate",
		"do not fan out over every weak service hit",
		"runtime paths stop at the last verified hop",
	} {
		if !strings.Contains(systemPrompt, must) {
			t.Errorf("systemPrompt missing aligned chain rule %q", must)
		}
	}
	if strings.Contains(systemPrompt, "Run the DependencyChain for each found service") {
		t.Fatal("systemPrompt still requires broad dependency fan-out")
	}
}

func TestAgentToolPromptDoesNotRepeatCoreRoleOrEvidenceRules(t *testing.T) {
	for _, forbidden := range []string{
		"You are **Astris**",
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
