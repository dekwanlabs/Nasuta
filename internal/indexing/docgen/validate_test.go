package docgen

import "testing"

func TestValidateFlowDoesNotRequireFrontmatterID(t *testing.T) {
	content := `---
scope: event-driven
tags: [flow, gateway, auth]
---
# Flow: Authentication

## Trigger Sources

## Full Chain

## Key Services

## Troubleshooting

## Dependencies
`

	result := ValidateFlow(content)
	if !result.Valid {
		t.Fatalf("ValidateFlow() errors = %v, want valid without frontmatter id", result.Errors)
	}
}

func TestValidateFlowRejectsDocumentIDFrontmatter(t *testing.T) {
	for _, field := range []string{"id", "doc_id"} {
		content := "---\n" + field + ": flow-auth\nscope: event-driven\ntags: [flow, gateway, auth]\n---\n" +
			"# Flow: Authentication\n\n## Trigger Sources\n\n## Full Chain\n\n## Key Services\n\n## Troubleshooting\n\n## Dependencies\n"
		result := ValidateFlow(content)
		if result.Valid {
			t.Fatalf("ValidateFlow() accepted platform-owned frontmatter field %q", field)
		}
	}
}
