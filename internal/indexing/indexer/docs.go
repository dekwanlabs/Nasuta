package indexer

import (
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/log"
	"gopkg.in/yaml.v3"
)

type frontmatter struct {
	data    map[string]any
	content string
}

var fmRe = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

func parseFrontmatter(raw string) frontmatter {
	m := fmRe.FindStringSubmatch(raw)
	if m == nil {
		return frontmatter{data: map[string]any{}, content: raw}
	}
	data := map[string]any{}
	_ = yaml.Unmarshal([]byte(m[1]), &data)
	if data == nil {
		data = map[string]any{}
	}
	return frontmatter{data: data, content: m[2]}
}

func fmString(data map[string]any, key string) string {
	if v, ok := data[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func fmStringArray(data map[string]any, key string) []string {
	v, ok := data[key]
	if !ok {
		return []string{}
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
		if strings.TrimSpace(t) != "" {
			return []string{strings.TrimSpace(t)}
		}
	}
	return []string{}
}

var titleRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)

func extractTitle(content string) string {
	if m := titleRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// IndexKnowledgeBaseFromDocStore reads flow and schema docs from MySQL.
// The DocStore is the sole runbook source.
// It returns nil on missing docStore or error so retrieval can degrade gracefully.
func IndexKnowledgeBaseFromDocStore(docStore *store.DocStore) []domain.RunbookRecord {
	if docStore == nil {
		return nil
	}
	docs, err := docStore.ListDocsByKinds(domain.KnowledgeDocKinds)
	if err != nil {
		log.Warnf("[indexer] failed to read KB docs from DocStore: %v", err)
		return nil
	}
	if len(docs) == 0 {
		return nil
	}
	var records []domain.RunbookRecord
	for _, d := range docs {
		fm := parseFrontmatter(d.Content)
		id := fmString(fm.data, "id")
		if id == "" {
			id = d.ID
		}
		title := extractTitle(fm.content)
		if title == "" {
			title = d.Title
		}
		// scope mirrors DocStore kind verbatim.
		// The markdown frontmatter "scope" field is ignored as a legacy alias.
		// kind is now the single source of truth.
		scope := d.Kind
		records = append(records, domain.RunbookRecord{
			ID:         id,
			Repo:       "docs",
			Title:      title,
			Path:       d.Filename,
			Scope:      scope,
			Tags:       fmStringArray(fm.data, "tags"),
			Text:       fm.content,
			Confidence: 1,
		})
	}
	return records
}

// RunbookEdgesFromDocStore extracts service dependency edges declared in runbook
// frontmatter. A runbook names its subject via `service:`, then `depends_on:`
// lists targets (subject → target) and `called_by:` lists callers (caller →
// subject). Runbook-declared edges fill gaps the code scanners miss (App→Server,
// cross-protocol, name mismatches). No subject → no edges (nothing to anchor).
func RunbookEdgesFromDocStore(docStore *store.DocStore) []domain.DependencyEdge {
	if docStore == nil {
		return nil
	}
	docs, err := docStore.ListDocsByKinds(domain.KnowledgeDocKinds)
	if err != nil {
		return nil
	}
	var edges []domain.DependencyEdge
	for _, d := range docs {
		fm := parseFrontmatter(d.Content)
		subject := fmString(fm.data, "service")
		if subject == "" {
			continue
		}
		ev := []domain.Evidence{{Path: d.Filename, Kind: domain.SourceDoc}}
		for _, target := range fmStringArray(fm.data, "depends_on") {
			edges = append(edges, domain.DependencyEdge{
				From: subject, To: target, Type: domain.EdgeRunbook, Evidence: ev, Confidence: 0.9,
			})
		}
		for _, caller := range fmStringArray(fm.data, "called_by") {
			edges = append(edges, domain.DependencyEdge{
				From: caller, To: subject, Type: domain.EdgeRunbook, Evidence: ev, Confidence: 0.9,
			})
		}
	}
	return edges
}
