package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/log"
)

// collectServices formats service candidates without implying endpoint relevance.
func (retrieve *Retriever) collectServices(ctx context.Context, services []string, svcMatches map[string]serviceMatch, addPart func(partial)) {
	if len(services) == 0 {
		return
	}
	var sb strings.Builder
	var refs []Reference
	sb.WriteString("## Relevant Services\n")
	for _, name := range services {
		m := svcMatches[name]
		fmt.Fprintf(&sb, "- **%s** (layer: %s, lang: %s)", name, m.layer, m.lang)
		if m.summary != "" {
			sb.WriteString(": " + m.summary)
		}
		sb.WriteString("\n")
		refs = append(refs, Reference{Type: "service", Label: name, Target: name})
	}
	log.InfofCtx(ctx, "[qa] collect services: %d services, %d refs", len(services), len(refs))
	addPart(partial{text: sb.String(), refs: refs, priority: partialPriorityService})
}

// collectCode converts code search hits into the code pool.
func (retrieve *Retriever) collectCode(ctx context.Context, codeHits []codeHit, addCode func(codeDoc)) {
	var dropped []string
	for _, h := range codeHits {
		if h.path == "" {
			continue
		}
		if h.scoreKind != "rrf" && !codeHitPassesFloor(h.denseScore, retrieve.platform.CodeMinScore) {
			dropped = append(dropped, retrieve.shortPath(ctx, h.path))
			continue
		}
		evidenceClass, trustTier := h.evidenceClass, h.trustTier
		if evidenceClass == "" || trustTier <= 0 {
			evidenceClass, trustTier = domain.EvidenceForCodeChunk(h.lang, h.repo)
		}
		addCode(codeDoc{
			source:        "code",
			service:       retrieve.serviceForRepo(ctx, h.repo, h.path),
			layer:         h.layer,
			filePath:      h.path,
			text:          h.snippet,
			chars:         len(h.snippet),
			recallScore:   h.recallScore,
			denseScore:    h.denseScore,
			scoreKind:     h.scoreKind,
			evidenceClass: evidenceClass,
			trustTier:     trustTier,
			startLine:     h.startLine,
			endLine:       h.endLine,
		})
	}
	if len(dropped) > 0 {
		log.InfofCtx(ctx, "[qa] code dropped below score %.2f: %v", retrieve.platform.CodeMinScore, dropped)
	}
}

// collectRunbooks turns the anchor's runbook hits into code-pool docs. The hits
// were fetched in the discovery hop, so an empty anchor.runbooks means expand
// pulls no doc bodies at all (hit-gated expansion — no search fired here).
//
// Multiple matched chunks from the same document are merged within a hard
// online-read bound. Full document bodies belong to explicit full-read flows.
func (retrieve *Retriever) collectRunbooks(ctx context.Context, runbookHits []domain.RunbookSearchHit, addCode func(codeDoc)) {
	if len(runbookHits) == 0 {
		return
	}
	type chunk struct {
		title       string
		section     string
		text        string
		scope       string
		score       float64
		evidenceCls string
		trust       int
	}
	byTitle := map[string][]chunk{}
	seenText := map[string]map[string]struct{}{}
	seenTitle := map[string]struct{}{}
	var titleOrder []string
	dropped := map[string]struct{}{}
	const maxChunksPerRunbook = 3
	for _, hit := range runbookHits {
		rb := hit.Record
		title := rb.Title
		if title == "" {
			continue
		}
		if hit.Score < retrieve.platform.RunbookMinScore {
			dropped[title] = struct{}{}
			continue
		}
		text := hit.ChunkText
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		if len(byTitle[title]) >= maxChunksPerRunbook {
			continue
		}
		if seenText[title] == nil {
			seenText[title] = map[string]struct{}{}
		}
		if _, duplicate := seenText[title][text]; duplicate {
			continue
		}
		seenText[title][text] = struct{}{}
		if _, ok := seenTitle[title]; !ok {
			seenTitle[title] = struct{}{}
			titleOrder = append(titleOrder, title)
		}
		byTitle[title] = append(byTitle[title], chunk{
			title:       title,
			section:     hit.SectionHeader,
			text:        text,
			scope:       rb.Scope,
			score:       hit.SemanticScore,
			evidenceCls: hit.EvidenceClass,
			trust:       hit.TrustTier,
		})
	}
	if len(dropped) > 0 {
		titles := make([]string, 0, len(dropped))
		for title := range dropped {
			titles = append(titles, title)
		}
		sort.Strings(titles)
		log.InfofCtx(ctx, "[qa] runbooks dropped below score %.2f: %v", retrieve.platform.RunbookMinScore, titles)
	}
	if len(titleOrder) == 0 {
		return
	}
	titles := make([]string, 0, len(titleOrder))
	for _, title := range titleOrder {
		chunks := byTitle[title]
		if len(chunks) == 0 {
			continue
		}
		var merged strings.Builder
		bestScore := 0.0
		for i, c := range chunks {
			if c.score > bestScore {
				bestScore = c.score
			}
			if i > 0 {
				merged.WriteString("\n")
			}
			merged.WriteString(c.text)
		}
		text := merged.String()
		const maxRunbookRunes = 4000
		if runes := []rune(text); len(runes) > maxRunbookRunes {
			text = string(runes[:maxRunbookRunes]) + "\n...(truncated)"
		}
		label := title
		if len(chunks) == 1 && chunks[0].section != "" {
			label = title + " › " + chunks[0].section
		}

		titles = append(titles, title)
		addCode(codeDoc{
			source:        "runbook",
			layer:         "docs",
			filePath:      title,
			funcName:      label,
			kind:          chunks[0].scope,
			text:          text,
			chars:         len(text),
			denseScore:    bestScore,
			evidenceClass: chunks[0].evidenceCls,
			trustTier:     chunks[0].trust,
		})
	}
	log.InfofCtx(ctx, "[qa] runbooks selected (merged %d unique titles from %d chunks):", len(titles), len(runbookHits))
	for i, t := range titles {
		cs := byTitle[t]
		best := 0.0
		for _, c := range cs {
			if c.score > best {
				best = c.score
			}
		}
		log.InfofCtx(ctx, "  [%d] %s trust=%d score=%.3f semantic=%.3f chunks=%d", i, t, cs[0].trust, best, best, len(cs))
	}
	log.InfofCtx(ctx, "[qa] runbooks matched: %d %v", len(titles), titles)
}

// collectDeps queries the dependency chain for each anchored service and formats
// the first one that has edges.
func (retrieve *Retriever) collectDeps(ctx context.Context, services []string, addPart func(partial)) {
	if len(services) == 0 {
		return
	}
	for _, c := range services {
		res, err := retrieve.tools.TraceDeps(ctx, c, "both", 2)
		if err != nil {
			log.WarnfCtx(ctx, "[qa] collect deps for %s: %v", c, err)
			continue
		}
		if len(res.Upstream) > 0 || len(res.Downstream) > 0 {
			var sb strings.Builder
			sb.WriteString("## Dependency Chain\n")
			const maxDependencyEdges = 30
			written := 0
			for _, e := range res.Upstream {
				if written >= maxDependencyEdges {
					break
				}
				fmt.Fprintf(&sb, "- %s → %s (upstream)\n", e.From, e.To)
				written++
			}
			for _, e := range res.Downstream {
				if written >= maxDependencyEdges {
					break
				}
				fmt.Fprintf(&sb, "- %s → %s (downstream)\n", e.From, e.To)
				written++
			}
			if written < len(res.Upstream)+len(res.Downstream) {
				sb.WriteString("- ...(additional edges omitted)\n")
			}
			log.InfofCtx(ctx, "[qa] collect deps: %s up=%d down=%d", c, len(res.Upstream), len(res.Downstream))
			addPart(partial{
				text:     sb.String(),
				refs:     []Reference{{Type: "service", Label: c, Target: c}},
				priority: partialPriorityDependency,
			})
			return
		}
	}
}

// collectCodeGraph performs one scoped FTS query, then fetches selected bodies.
func (retrieve *Retriever) collectCodeGraph(ctx context.Context, keywords, services []string, terms QueryTerms, addCode func(codeDoc)) {
	traceEnabled := domain.TraceEnabled(ctx)
	started := time.Now()
	allHits := retrieve.codeGraphQuery(ctx, keywords, services, 20)
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "codegraph_search", DurationMS: time.Since(started).Milliseconds(),
			Input:  map[string]any{"keywords": keywords, "services": services},
			Output: map[string]any{"hits": len(allHits)},
		})
	}
	log.InfofCtx(ctx, "[qa] codegraph query: %d keywords → %d hits", len(keywords), len(allHits))
	if len(allHits) == 0 {
		return
	}

	kindPri := map[string]int{"method": 0, "function": 0, "class": 1, "interface": 1, "route": 2, "field": 3, "import": 4, "namespace": 4, "constant": 4}
	techTermsLower := map[string]bool{}
	for _, t := range terms.allTerms() {
		techTermsLower[strings.ToLower(t)] = true
	}
	foundSvcLower := make([]string, len(services))
	for i, s := range services {
		foundSvcLower[i] = strings.ToLower(s)
	}
	inTechTerm := func(f, funcName string) bool {
		lf, ln := strings.ToLower(f), strings.ToLower(funcName)
		for t := range techTermsLower {
			if strings.Contains(ln, t) || strings.Contains(lf, t) {
				return true
			}
		}
		return false
	}
	inFoundSvc := func(f string) bool {
		lf := strings.ToLower(f)
		for _, s := range foundSvcLower {
			if strings.Contains(lf, s) {
				return true
			}
		}
		return false
	}

	type rankedHit struct {
		codegraph.Node
		tech    bool
		svc     bool
		pri     int
		service string
	}
	ranked := make([]rankedHit, len(allHits))
	for i, h := range allHits {
		pri, ok := kindPri[h.Kind]
		if !ok {
			pri = 5
		}
		ranked[i] = rankedHit{h, inTechTerm(h.FilePath, h.Name), inFoundSvc(h.FilePath), pri, retrieve.serviceForPath(ctx, h.FilePath)}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].tech != ranked[j].tech {
			return ranked[i].tech
		}
		if ranked[i].svc != ranked[j].svc {
			return ranked[i].svc
		}
		return ranked[i].pri < ranked[j].pri
	})

	limit := min(10, len(ranked))
	taken := map[int]bool{}
	done := 0

	fetchNode := func(r rankedHit) bool {
		switch r.Kind {
		case "field", "import", "namespace", "file", "constant":
			return false
		}
		nodeOut := retrieve.codeGraphNode(ctx, r.Name, r.FilePath, r.StartLine)
		if nodeOut == "" {
			return false
		}
		log.InfofCtx(ctx, "[qa] codegraph node: %s [%s] (%s:%d) → %d chars", r.Name, r.Kind, retrieve.shortPath(ctx, r.FilePath), r.StartLine, len(nodeOut))
		synScore := 0.05
		if r.tech {
			synScore = 0.25
		} else if r.svc {
			synScore = 0.15
		}
		addCode(codeDoc{
			source:        "codegraph",
			service:       r.service,
			layer:         retrieve.layerForPath(ctx, r.FilePath),
			filePath:      r.FilePath,
			methodSig:     r.Name,
			funcName:      r.Name,
			kind:          r.Kind,
			text:          nodeOut,
			chars:         len(nodeOut),
			refs:          countRefsFromNode(nodeOut),
			denseScore:    synScore,
			evidenceClass: domain.EvidenceClassCodeRuntime,
			trustTier:     domain.TrustCodeRuntime,
		})
		return true
	}

	// Two-pass: one node per anchored service first (spread coverage across
	// services), then backfill the rest by rank.
	seenSvc := map[string]bool{}
	for pass := 0; pass < 2 && done < limit; pass++ {
		for i, r := range ranked {
			if done >= limit {
				break
			}
			if taken[i] {
				continue
			}
			if pass == 0 && (r.service == "" || seenSvc[r.service]) {
				continue
			}
			if fetchNode(r) {
				taken[i] = true
				if pass == 0 {
					seenSvc[r.service] = true
				}
				done++
			}
		}
	}

	if done > 0 {
		svcCount := map[string]int{}
		for i := range taken {
			if svc := ranked[i].service; svc != "" {
				svcCount[svc]++
			}
		}
		var parts []string
		for _, s := range services {
			if c := svcCount[s]; c > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", s, c))
			}
		}
		log.InfofCtx(ctx, "[qa] codegraph per-service: %v", parts)
	}
}

// countRefsFromNode estimates call breadth from node output.
func countRefsFromNode(nodeOut string) int {
	n := 0
	for _, line := range strings.Split(nodeOut, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "• ") {
			n++
		}
	}
	return n
}
