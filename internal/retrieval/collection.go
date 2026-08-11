package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type dependencyEdge struct {
	from, to, direction string
}

type dependencyCollection struct {
	edges           []dependencyEdge
	queried         int
	queriedServices []string
	unqueried       int
	omittedEdges    int
}

var dependencyCollectSpec = runtrace.Spec[[]string, dependencyCollection]{
	Operation: "retrieval.dependency_collect",
	Node:      "dependency_collect",
	Output: func(_ []string, result dependencyCollection, _ error) map[string]any {
		return map[string]any{
			"queried_services": result.queried, "unqueried_services": result.unqueried,
			"selected_edges": len(result.edges), "omitted_edges": result.omittedEdges,
		}
	},
	Record: func(result dependencyCollection, _ error) bool {
		return len(result.edges) > 0
	},
}

type codeGraphSearchInput struct {
	keywords []string
	services []string
}

var codeGraphSearchSpec = runtrace.Spec[codeGraphSearchInput, []codegraph.Node]{
	Operation: "retrieval.codegraph_search",
	Node:      "codegraph_search",
	Input: func(input codeGraphSearchInput) map[string]any {
		return map[string]any{"keywords": input.keywords, "services": input.services}
	},
	Output: func(_ codeGraphSearchInput, result []codegraph.Node, _ error) map[string]any {
		return map[string]any{"hits": len(result)}
	},
}

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
	text := sb.String()
	units := make([]tool.EvidenceUnit, 0, len(services))
	for _, service := range services {
		units = append(units, evidenceUnitForPart("service", service, text, tool.EvidenceCoverage{
			Complete: true, Included: 1,
		}))
	}
	addPart(partial{text: text, refs: refs, units: units, priority: partialPriorityService})
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
		docID       string
		title       string
		index       int
		section     string
		text        string
		scope       string
		score       float64
		evidenceCls string
		trust       int
	}
	byDocID := map[string][]chunk{}
	seenText := map[string]map[string]struct{}{}
	seenDoc := map[string]struct{}{}
	var docOrder []string
	dropped := map[string]struct{}{}
	const maxChunksPerRunbook = 3
	for _, hit := range runbookHits {
		title := hit.Title
		docID := hit.DocID
		if title == "" || docID == "" {
			continue
		}
		for _, matched := range hit.Chunks {
			if matched.SemanticScore < retrieve.platform.RunbookMinScore {
				dropped[title] = struct{}{}
				continue
			}
			text := strings.TrimSpace(matched.ChunkText)
			if text == "" || len(byDocID[docID]) >= maxChunksPerRunbook {
				continue
			}
			if seenText[docID] == nil {
				seenText[docID] = map[string]struct{}{}
			}
			if _, duplicate := seenText[docID][text]; duplicate {
				continue
			}
			seenText[docID][text] = struct{}{}
			if _, ok := seenDoc[docID]; !ok {
				seenDoc[docID] = struct{}{}
				docOrder = append(docOrder, docID)
			}
			byDocID[docID] = append(byDocID[docID], chunk{
				docID: docID, title: title, index: matched.ChunkIndex,
				section: matched.SectionHeader, text: text, scope: hit.DocKind,
				score: matched.SemanticScore, evidenceCls: hit.EvidenceClass, trust: hit.TrustTier,
			})
		}
	}
	if len(dropped) > 0 {
		titles := make([]string, 0, len(dropped))
		for title := range dropped {
			titles = append(titles, title)
		}
		sort.Strings(titles)
		log.InfofCtx(ctx, "[qa] runbooks dropped below score %.2f: %v", retrieve.platform.RunbookMinScore, titles)
	}
	if len(docOrder) == 0 {
		return
	}
	titles := make([]string, 0, len(docOrder))
	for _, docID := range docOrder {
		chunks := byDocID[docID]
		if len(chunks) == 0 {
			continue
		}
		title := chunks[0].title
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
		sections := make([]string, 0, len(chunks))
		seenSections := make(map[string]struct{}, len(chunks))
		for _, chunk := range chunks {
			section := chunk.section
			if section == "" {
				section = fmt.Sprintf("chunk:%d", chunk.index)
			}
			if _, duplicate := seenSections[section]; duplicate {
				continue
			}
			seenSections[section] = struct{}{}
			sections = append(sections, section)
		}

		titles = append(titles, title)
		addCode(codeDoc{
			source:        "runbook",
			layer:         "docs",
			filePath:      title,
			funcName:      label,
			docID:         chunks[0].docID,
			kind:          chunks[0].scope,
			sections:      sections,
			coverage:      tool.EvidenceCoverage{Partial: true, Included: len(chunks)},
			text:          text,
			chars:         len(text),
			denseScore:    bestScore,
			evidenceClass: chunks[0].evidenceCls,
			trustTier:     chunks[0].trust,
		})
	}
	log.InfofCtx(ctx, "[qa] runbooks selected (merged %d documents from %d hits):", len(titles), len(runbookHits))
	for i, docID := range docOrder {
		cs := byDocID[docID]
		if len(cs) == 0 {
			continue
		}
		best := 0.0
		for _, c := range cs {
			if c.score > best {
				best = c.score
			}
		}
		log.InfofCtx(ctx, "  [%d] %s (%s) trust=%d score=%.3f semantic=%.3f chunks=%d",
			i, cs[0].title, docID, cs[0].trust, best, best, len(cs))
	}
	log.InfofCtx(ctx, "[qa] runbooks matched: %d %v", len(titles), titles)
}

// collectDeps collects unique dependency edges across anchored services.
func (retrieve *Retriever) collectDeps(ctx context.Context, services []string, addPart func(partial)) {
	if len(services) == 0 {
		return
	}
	result, _ := runtrace.Invoke(ctx, dependencyCollectSpec, services,
		func(ctx context.Context, services []string) (dependencyCollection, error) {
			return retrieve.collectDependencyEdges(ctx, services), nil
		})
	if len(result.edges) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("## Dependency Chain\n")
	for _, edge := range result.edges {
		fmt.Fprintf(&sb, "- %s → %s (%s)\n", edge.from, edge.to, edge.direction)
	}
	if result.omittedEdges > 0 || result.unqueried > 0 {
		sb.WriteString("- ...(additional edges omitted)\n")
	}
	refs := make([]Reference, 0, len(result.queriedServices))
	for _, service := range result.queriedServices {
		refs = append(refs, Reference{Type: "service", Label: service, Target: service})
	}
	log.InfofCtx(ctx, "[qa] collect deps: services=%d/%d edges=%d omitted_edges=%d",
		result.queried, len(services), len(result.edges), result.omittedEdges)
	text := sb.String()
	units := make([]tool.EvidenceUnit, 0, len(result.queriedServices))
	for _, service := range result.queriedServices {
		units = append(units, evidenceUnitForPart("dependency", service, text, tool.EvidenceCoverage{
			Complete: result.omittedEdges == 0 && result.unqueried == 0,
			Partial:  result.omittedEdges > 0 || result.unqueried > 0,
			Included: len(result.edges), OmittedItems: result.omittedEdges,
		}))
	}
	addPart(partial{text: text, refs: refs, units: units, priority: partialPriorityDependency})
}

func (retrieve *Retriever) collectDependencyEdges(ctx context.Context, services []string) dependencyCollection {
	const maxDependencyEdges = 30
	const maxDependencyServices = 3
	const dependencyBudget = 500 * time.Millisecond
	totalServices := len(services)
	if len(services) > maxDependencyServices {
		services = services[:maxDependencyServices]
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyBudget)
	defer cancel()

	type traceResult struct {
		service string
		trace   domain.DependencyTrace
		err     error
	}
	results := make(chan traceResult, len(services))
	for _, service := range services {
		go func(service string) {
			trace, err := retrieve.tools.TraceDeps(ctx, service, "both", 2)
			results <- traceResult{service: service, trace: trace, err: err}
		}(service)
	}

	edges := make([]dependencyEdge, 0, maxDependencyEdges)
	seen := make(map[string]struct{}, maxDependencyEdges)
	omittedEdges := 0
	traces := make(map[string]traceResult, len(services))
	completed := 0
	for completed < len(services) {
		select {
		case result := <-results:
			completed++
			traces[result.service] = result
		case <-ctx.Done():
			completed = len(services)
		}
	}
	queriedServices := make([]string, 0, len(traces))
	for _, service := range services {
		if _, ok := traces[service]; ok {
			queriedServices = append(queriedServices, service)
		}
	}
	for _, service := range services {
		result, ok := traces[service]
		if !ok {
			continue
		}
		if result.err != nil {
			log.WarnfCtx(ctx, "[qa] collect deps for %s: %v", service, result.err)
			continue
		}
		appendEdges := func(direction string, candidates []domain.DependencyEdge) {
			for _, edge := range candidates {
				key := direction + "\x00" + edge.From + "\x00" + edge.To
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				if len(edges) >= maxDependencyEdges {
					omittedEdges++
					continue
				}
				seen[key] = struct{}{}
				edges = append(edges, dependencyEdge{from: edge.From, to: edge.To, direction: direction})
			}
		}
		appendEdges("upstream", result.trace.Upstream)
		appendEdges("downstream", result.trace.Downstream)
	}
	return dependencyCollection{
		edges: edges, queried: len(queriedServices), queriedServices: queriedServices,
		unqueried: totalServices - len(queriedServices), omittedEdges: omittedEdges,
	}
}

// collectCodeGraph performs one scoped FTS query, then fetches selected bodies.
func (retrieve *Retriever) collectCodeGraph(ctx context.Context, keywords, services []string, terms QueryTerms, addCode func(codeDoc)) {
	allHits, _ := runtrace.Invoke(ctx, codeGraphSearchSpec, codeGraphSearchInput{
		keywords: keywords, services: services,
	}, func(ctx context.Context, input codeGraphSearchInput) ([]codegraph.Node, error) {
		return retrieve.codeGraphQuery(ctx, input.keywords, input.services, 20), nil
	})
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
