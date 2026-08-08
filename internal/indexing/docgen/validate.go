package docgen

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/log"
	"gopkg.in/yaml.v3"
)

// FlowTemplateEnglish is the canonical English flow template shown
// in the dashboard and used as the reformatting reference. Mirrors the Chinese
// template in docs/knowledge-base/flows/FLOW_TEMPLATE.md.
var FlowTemplateEnglish = prompts.Text(prompts.DocgenFlowTemplate)

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

	if _, exists := fm.data["id"]; exists {
		res.Errors = append(res.Errors, "frontmatter field 'id' is platform-owned and must be omitted")
	}
	if _, exists := fm.data["doc_id"]; exists {
		res.Errors = append(res.Errors, "frontmatter field 'doc_id' is platform-owned and must be omitted")
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
	prompt, err := prompts.Render(prompts.DocgenReformat, struct {
		Template string
		Original string
	}{
		Template: FlowTemplateEnglish,
		Original: original,
	})
	if err != nil {
		return original, fmt.Errorf("render reformat prompt: %w", err)
	}

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
