package execution

import (
	"strings"

	"github.com/dekwanlabs/nasuta/internal/prompts"
)

const (
	rolePromptPlaceholder = "{{ROLE_PROMPT}}"
	rolePromptAction      = "{{ .RolePrompt }}"
)

// These compatibility variables keep the established agent prompt surface while
// the source text lives in the central embedded catalog.
var (
	userVisibleAnswerPrompt = prompts.Text(prompts.AgentQAUserVisibleAnswer)
	systemPrompt            = withUserVisibleAnswer(withRolePlaceholder(prompts.AgentQACore))
	defaultIdentity         = prompts.Text(prompts.AgentQADefaultIdentity)
)

func withUserVisibleAnswer(prompt string) string {
	return prompt + "\n\n" + userVisibleAnswerPrompt
}

func withRolePlaceholder(id prompts.ID) string {
	content := prompts.Text(id)
	if !strings.Contains(content, rolePromptAction) {
		panic("agent: role-aware prompt is missing its role template action")
	}
	return strings.Replace(content, rolePromptAction, rolePromptPlaceholder, 1)
}

// resolveIdentity picks the persona system message for a request: the asking
// user's already-combined RBAC role prompt when present, else defaultIdentity so
// a role-less or RBAC-disabled user still answers in character.
func resolveIdentity(rolePrompt string) string {
	if rp := strings.TrimSpace(rolePrompt); rp != "" {
		return rp
	}
	return defaultIdentity
}

// composeSystemPrompt replaces the fixed identity slot in a prompt template.
// Keeping the slot in the template makes role placement explicit and prevents
// dynamic identity text from being appended after rules or tool instructions.
func composeSystemPrompt(template, rolePrompt string) string {
	return strings.Replace(template, rolePromptPlaceholder, resolveIdentity(rolePrompt), 1)
}
