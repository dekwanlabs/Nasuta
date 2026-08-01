package docgen

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/log"
	"gopkg.in/yaml.v3"
)

// FlowTemplateEnglish is the canonical English flow template shown
// in the dashboard and used as the reformatting reference. Mirrors the Chinese
// template in docs/knowledge-base/flows/FLOW_TEMPLATE.md.
const FlowTemplateEnglish = `# Flow: <English Name> (<Chinese Name>)

## Format

Each flow document covers one complete **business chain**, from
trigger to completion.

### Frontmatter (required)

` + "```yaml" + `
---
id: flow-<domain>-<topic>            # unique id, kebab-case
scope: event-driven                     # fixed value
tags: [flow, <service>, <domain>, ...]  # at least 3 tags for semantic recall
---
` + "```" + `

### Title

` + "`# Flow: <English Name> (<Chinese Name>)`" + `

Example: ` + "`# Flow: Device Provisioning & SN Lifecycle`" + `

### Required Sections (in order)

1. **Overview** (1-2 paragraphs) — what this chain does, which services it
   touches, and the business value.
2. **Trigger Sources** (` + "`## Trigger Sources`" + `) — table: Event | Trigger
   Service | Trigger Condition.
3. **Full Chain** (` + "`## Full Chain`" + `) — ASCII diagram or nested list, from
   trigger to terminal state.
4. **Key Services** (` + "`## Key Services`" + `) — table: Service | Responsibility |
   Database | Kibana Index.
5. **Troubleshooting** (` + "`## Troubleshooting`" + `) — table: Common Problem →
   Resolution Steps.
6. **Dependencies** (` + "`## Dependencies`" + `) — external services this chain
   depends on + who depends on this chain.

### Optional Sections

- **Data Storage** (` + "`## Data Storage`" + `) — table groups when multiple DBs.
- **Middleware** (` + "`## Middleware`" + `) — Kafka/RabbitMQ/Redis etc.
- **Branch Flows** (` + "`## Branch Flows`" + `) — alternative handling branches.

### Notes

- All service names, API paths, and Feign interface names must come from real
  code — never invent them. Verify against codegraph before writing.
- Kibana index uses the ` + "`hs-iot-<service>-*`" + ` format, derived from existing
  indices.
- Prefer tables over prose. Troubleshooting must be operational ("query the Y
  index of service X, search keyword Z").
- Cross-reference other documents via their ` + "`flow-xxx.md`" + ` filename.
`

// FlowValidationResult describes why a flow doc failed validation.
type FlowValidationResult struct {
	Valid  bool
	Errors []string
}

// ValidateFlow checks that a markdown document conforms to the flow
// template structure: required frontmatter fields and the six required section
// headings. It is structural — it does not judge content quality.
func ValidateFlow(content string) FlowValidationResult {
	var res FlowValidationResult
	fm, body := splitFrontmatter(content)

	id := fm.scalar("id")
	if id == "" {
		res.Errors = append(res.Errors, "frontmatter field 'id' is missing")
	}
	scope := fm.scalar("scope")
	if scope != "event-driven" {
		res.Errors = append(res.Errors, fmt.Sprintf("frontmatter 'scope' must be 'event-driven', got %q", scope))
	}
	if len(fm.scalarArray("tags")) < 3 {
		res.Errors = append(res.Errors, "frontmatter 'tags' must list at least 3 entries")
	}

	required := []string{
		"## Trigger Sources",
		"## Full Chain",
		"## Key Services",
		"## Troubleshooting",
		"## Dependencies",
	}
	for _, sec := range required {
		if !strings.Contains(body, sec) {
			res.Errors = append(res.Errors, fmt.Sprintf("missing required section: %s", sec))
		}
	}
	res.Valid = len(res.Errors) == 0
	return res
}

// ReformatFlow asks the LLM to rewrite content into the flow template.
// It is used when validation fails and the template acts as the reference shape.
// If the LLM is unavailable, it returns the original content and logs the errors.
func (g *Generator) ReformatFlow(ctx context.Context, original string) (string, error) {
	if g.llm == nil {
		return original, fmt.Errorf("llm not configured")
	}
	prompt := fmt.Sprintf(`You are a technical documentation editor. Reformat the raw content below into the Nasuta flow template.

Rules:
- Output ONLY the final markdown document, no commentary.
- Start with the frontmatter block (id, scope: event-driven, tags with at least 3 entries).
- Use exactly these section headings in order: ## Trigger Sources, ## Full Chain, ## Key Services, ## Troubleshooting, ## Dependencies.
- Keep all real service names, API paths, and Feign interface names from the original — do not invent or rename them.
- Use tables for Trigger Sources, Key Services, and Troubleshooting.
- If the original lacks a section, infer it from context or leave a TODO placeholder rather than fabricating details.
- Write in the same language as the original content.

Reference template:
%s

Raw content to reformat:
---
%s
---`, FlowTemplateEnglish, original)

	llmCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	answer, err := g.llm.chat(llmCtx, prompt)
	if err != nil {
		log.Warnf("[docgen] reformat LLM failed: %v — returning original", err)
		return original, fmt.Errorf("llm reformat: %w", err)
	}
	return strings.TrimSpace(answer), nil
}

// ReformatFlowWithSettings builds a one-shot Generator from the current
// platform settings and rewrites the content. Used by the dashboard upload
// path, which has no long-lived Generator.
func ReformatFlowWithSettings(cfg config.Config, ps *config.PlatformSettings, docDB *store.DocStore, ctx context.Context, original string) (string, error) {
	if ps == nil {
		return original, fmt.Errorf("platform settings required")
	}
	g, err := New(cfg, ps, docDB)
	if err != nil {
		return original, err
	}
	return g.ReformatFlow(ctx, original)
}

// ---- frontmatter helpers (mirror the indexer's private copy) ----

type fmMap struct {
	data map[string]any
}

func splitFrontmatter(raw string) (fmMap, string) {
	const sep = "---"
	if !strings.HasPrefix(raw, sep) {
		return fmMap{data: map[string]any{}}, raw
	}
	// find the closing ---
	rest := raw[len(sep):]
	end := strings.Index(rest, "\n"+sep)
	if end < 0 {
		return fmMap{data: map[string]any{}}, raw
	}
	body := strings.TrimPrefix(rest[end+len(sep)+1:], "\n")
	data := map[string]any{}
	_ = yaml.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &data)
	if data == nil {
		data = map[string]any{}
	}
	return fmMap{data: data}, body
}

func (m fmMap) scalar(key string) string {
	if v, ok := m.data[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (m fmMap) scalarArray(key string) []string {
	v, ok := m.data[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	}
	return nil
}
