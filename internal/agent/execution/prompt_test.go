package execution

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/prompts"
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

func TestSystemPromptPreservesEvidenceStatementBoundaries(t *testing.T) {
	for _, required := range []string{"Never broaden the subject, scope, or quantifier", "local list cannot establish a global total", "require explicit support for the same subject"} {
		if !strings.Contains(systemPrompt, required) {
			t.Fatalf("systemPrompt missing statement-boundary rule %q", required)
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

func TestAllAgentPromptsExplainPartialToolCoverage(t *testing.T) {
	for name, prompt := range map[string]string{
		"core":   systemPrompt,
		"direct": directAgentSystemPrompt,
		"web":    webAgentSystemPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{"delivery succeeded", "partial", "omitted", "unknown"} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("prompt missing partial-result rule %q", required)
				}
			}
		})
	}
}

func TestAllAgentPromptsShareUserVisibleAnswerContract(t *testing.T) {
	for name, prompt := range map[string]string{
		"core":   systemPrompt,
		"direct": directAgentSystemPrompt,
		"web":    webAgentSystemPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Count(prompt, "## User-Visible Answer Contract"); got != 1 {
				t.Fatalf("prompt contains %d user-visible answer contracts, want 1", got)
			}
			for _, required := range []string{
				"Do not name or cite internal knowledge documents",
				"Never expose capability names",
				"no more than two to four question-driven sections",
				"Do not repeat the same information",
				"concise impact and tradeoff first",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("prompt missing user-visible answer rule %q", required)
				}
			}
		})
	}
}

func TestCorePromptDoesNotInviteInternalSourceIdentifiers(t *testing.T) {
	if strings.Contains(systemPrompt, "concise source identifiers") {
		t.Fatal("systemPrompt still permits internal source identifiers")
	}
	for _, required := range []string{
		"internal evidence metadata",
		"rather than naming it as a source",
	} {
		if !strings.Contains(systemPrompt, required) {
			t.Fatalf("systemPrompt missing internal attribution boundary %q", required)
		}
	}
}

func TestEvidenceAndRepairPromptsKeepInternalMetadataPrivate(t *testing.T) {
	evidence := prompts.MustRender(prompts.AgentQAPreRetrievedEvidence, struct {
		HitCount int
		Evidence string
	}{HitCount: 3, Evidence: "internal evidence"})
	for _, required := range []string{
		"INTERNAL, NOT USER-FACING",
		"Do not expose this header",
		"document or runbook titles",
		"retrieval metadata",
	} {
		if !strings.Contains(evidence, required) {
			t.Fatalf("pre-retrieved evidence prompt missing %q", required)
		}
	}

	repair := repairInstruction([]string{"TRACE-1"})
	for _, required := range []string{
		"user-visible answer contract",
		"non-repetitive structure",
		"internal document attribution",
		"retrieval metadata",
	} {
		if !strings.Contains(repair, required) {
			t.Fatalf("answer repair prompt missing %q", required)
		}
	}

	for _, required := range []string{
		"capability or tool names",
		"internal document titles or paths",
		"Describe useful actions in natural language",
	} {
		if !strings.Contains(protocolRepairInstruction, required) {
			t.Fatalf("protocol repair prompt missing %q", required)
		}
	}
}

func TestAllAgentPromptsRequireEfficientImplementations(t *testing.T) {
	for name, prompt := range map[string]string{
		"core":   systemPrompt,
		"direct": directAgentSystemPrompt,
		"web":    webAgentSystemPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{"every user requirement", "least practical time complexity"} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("prompt missing efficient-generation rule %q", required)
				}
			}
		})
	}
}

func TestWorkspaceIdentifierAmbiguityUsesOneExactLookup(t *testing.T) {
	for name, prompt := range map[string]string{
		"direct": directAgentSystemPrompt,
		"tools":  agentToolPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"current evidence does not already contain one exact definition",
				"do not repeat symbol resolution",
				"first tool-calling turn MUST contain exactly one get_symbol call",
				"no parallel",
				"priority over API",
				`resolution "ambiguous"`,
				"list the returned file or qualified-name candidates",
				"do not call another tool or answer the original question",
				`result is "unique"`,
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("prompt missing result-driven ambiguity policy %q", required)
				}
			}
		})
	}
}

func TestAllToolCapableAgentPromptsRequireStepRationale(t *testing.T) {
	for name, prompt := range map[string]string{
		"direct": directAgentSystemPrompt,
		"web":    webAgentSystemPrompt,
		"tools":  agentToolPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"short sentence",
				"same natural language",
				"concrete target",
				"tool names",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("prompt missing tool rationale rule %q", required)
				}
			}
		})
	}
}

func TestBuildMessagesUsesResolvedQueryKind(t *testing.T) {
	messages := BuildMessages(
		"why did this request fail",
		domain.QueryPlan{Kind: domain.QueryComparison},
		ConversationContext{},
		nil,
		domain.EvidencePlan{},
		"",
		0,
	)
	if len(messages) == 0 || !strings.Contains(messages[0].Content, "[QUERY_KIND: comparison]") {
		t.Fatalf("system prompt did not use the prepared query kind: %#v", messages)
	}
	if strings.Contains(messages[0].Content, "[QUERY_KIND: runtime_diagnosis]") {
		t.Fatal("BuildMessages reclassified the raw question instead of using QueryPlan")
	}
}

func TestQueryKindOverrideStaysInternal(t *testing.T) {
	instruction := prompts.MustRender(prompts.AgentQAQueryKind, struct {
		Kind string
	}{Kind: "focused_fact"})
	for _, required := range []string{"Override silently", "never expose this internal hint"} {
		if !strings.Contains(instruction, required) {
			t.Fatalf("query kind instruction missing %q: %s", required, instruction)
		}
	}
	if strings.Contains(instruction, "brief justification") {
		t.Fatalf("query kind instruction asks the model to expose an internal override: %s", instruction)
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
		"self-contained SVG document",
		"Do not use scripts",
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
	for _, must := range []string{
		"## Agent Tool Policy",
		"Pick the tool that matches the intent",
		"Converge after a targeted lookup",
		"Name runtime result states precisely",
		"Resolve client-facing entries across tool boundaries",
		"Prefer structured runtime scope",
	} {
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
