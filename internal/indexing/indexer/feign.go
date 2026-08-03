package indexer

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/log"
)

const maxConfigResolutionRounds = 8

var configPlaceholderRe = regexp.MustCompile(`\$\{([^{}]+)\}`)

type feignReference struct {
	From       string
	ModulePath string
	ClientName string
	URL        string
	Evidence   []domain.Evidence
	Confidence float64
}

func resolveFeignDependencies(
	ctx context.Context,
	refs []feignReference,
	resolver config.Resolver,
) ([]domain.DependencyEdge, error) {
	external := make(map[config.Ref]config.Value)
	requested := make(map[config.Ref]struct{})

	for range maxConfigResolutionRounds {
		missing := collectMissingFeignConfigs(refs, external)
		pending := make([]config.Ref, 0, len(missing))
		for ref := range missing {
			if _, done := requested[ref]; !done {
				pending = append(pending, ref)
			}
		}
		if len(pending) == 0 || resolver == nil {
			break
		}
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].Application != pending[j].Application {
				return pending[i].Application < pending[j].Application
			}
			return pending[i].Key < pending[j].Key
		})
		resolved, err := resolver.ResolveConfig(ctx, pending)
		if err != nil {
			return nil, fmt.Errorf("resolve Feign configuration: %w", err)
		}
		for _, ref := range pending {
			requested[ref] = struct{}{}
			if value, ok := resolved[ref]; ok {
				external[ref] = value
			}
		}
	}

	edges := make([]domain.DependencyEdge, 0, len(refs))
	for _, ref := range refs {
		raw, fromURL := feignTargetExpression(ref)
		valueResolver := feignConfigValueResolver{
			application: ref.From,
			external:    external,
			missing:     make(map[config.Ref]struct{}),
		}
		target, configEvidence, ok := valueResolver.expand(raw, make(map[string]struct{}))
		if !ok {
			keys := make([]string, 0, len(valueResolver.missing))
			for missing := range valueResolver.missing {
				keys = append(keys, missing.Key)
			}
			sort.Strings(keys)
			path := ""
			if len(ref.Evidence) > 0 {
				path = ref.Evidence[0].Path
			}
			log.Warnf("[indexer] skip Feign dependency with unresolved config: app=%s keys=%v file=%s", ref.From, keys, path)
			continue
		}
		target = normalizeFeignTarget(target, fromURL)
		if target == "" {
			continue
		}
		edges = append(edges, domain.DependencyEdge{
			From:       ref.From,
			To:         target,
			Type:       domain.EdgeFeign,
			Evidence:   append(ref.Evidence, configEvidence...),
			Confidence: ref.Confidence,
		})
	}
	return edges, nil
}

func collectMissingFeignConfigs(
	refs []feignReference,
	external map[config.Ref]config.Value,
) map[config.Ref]struct{} {
	missing := make(map[config.Ref]struct{})
	for _, ref := range refs {
		raw, _ := feignTargetExpression(ref)
		resolver := feignConfigValueResolver{
			application: ref.From,
			external:    external,
			missing:     missing,
		}
		_, _, _ = resolver.expand(raw, make(map[string]struct{}))
	}
	return missing
}

func feignTargetExpression(ref feignReference) (string, bool) {
	if strings.TrimSpace(ref.URL) != "" {
		return ref.URL, true
	}
	return ref.ClientName, false
}

type feignConfigValueResolver struct {
	application string
	external    map[config.Ref]config.Value
	missing     map[config.Ref]struct{}
}

func (resolver feignConfigValueResolver) expand(value string, stack map[string]struct{}) (string, []domain.Evidence, bool) {
	return resolver.expandDepth(value, stack, 0)
}

func (resolver feignConfigValueResolver) expandDepth(
	value string,
	stack map[string]struct{},
	depth int,
) (string, []domain.Evidence, bool) {
	if depth >= maxConfigResolutionRounds {
		return "", nil, false
	}
	matches := configPlaceholderRe.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		if strings.Contains(value, "${") {
			return "", nil, false
		}
		return value, nil, true
	}
	var out strings.Builder
	out.Grow(len(value))
	evidence := make([]domain.Evidence, 0, len(matches))
	last := 0
	for _, match := range matches {
		out.WriteString(value[last:match[0]])
		expression := value[match[2]:match[3]]
		key, fallback, hasFallback := strings.Cut(expression, ":")
		key = strings.TrimSpace(key)
		if key == "" {
			return "", nil, false
		}
		resolved, source, ok := resolver.lookup(key)
		if !ok {
			if !hasFallback {
				resolver.missing[config.Ref{
					Application: resolver.application,
					Key:         key,
				}] = struct{}{}
				return "", nil, false
			}
			resolved = fallback
		} else if source.Path != "" {
			evidence = append(evidence, source)
		}
		if _, cyclic := stack[key]; cyclic {
			return "", nil, false
		}
		stack[key] = struct{}{}
		expanded, nestedEvidence, ok := resolver.expandDepth(resolved, stack, depth+1)
		delete(stack, key)
		if !ok {
			return "", nil, false
		}
		out.WriteString(expanded)
		evidence = append(evidence, nestedEvidence...)
		last = match[1]
	}
	out.WriteString(value[last:])
	expanded := out.String()
	if strings.Contains(expanded, "${") {
		var nestedEvidence []domain.Evidence
		var ok bool
		expanded, nestedEvidence, ok = resolver.expandDepth(expanded, stack, depth+1)
		if !ok {
			return "", nil, false
		}
		evidence = append(evidence, nestedEvidence...)
	}
	return expanded, evidence, true
}

func (resolver feignConfigValueResolver) lookup(key string) (string, domain.Evidence, bool) {
	ref := config.Ref{Application: resolver.application, Key: key}
	value, ok := resolver.external[ref]
	if !ok || strings.TrimSpace(value.Value) == "" {
		return "", domain.Evidence{}, false
	}
	evidence := domain.Evidence{Path: strings.TrimSpace(value.Source), Kind: domain.SourceConfig}
	return value.Value, evidence, true
}

func normalizeFeignTarget(value string, fromURL bool) string {
	target := strings.TrimSpace(value)
	if target == "" || !fromURL {
		return target
	}
	parsed, err := url.Parse(target)
	if err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	if parsed, err = url.Parse("//" + target); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	return target
}
