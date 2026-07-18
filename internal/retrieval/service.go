package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/platform"
)

// Service is the outward-facing retrieval capability.
type Service = Retriever

// blankDash returns "-" for blank strings, otherwise the trimmed string.
func blankDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}

// truncateInline collapses whitespace and truncates to one line.
func truncateInline(s string, max int) string {
	return platform.TruncateForLog(strings.Join(strings.Fields(s), " "), max)
}

func runbookTitles(matches []types.RunbookSearchHit) []string {
	titles := make([]string, 0, len(matches))
	for _, match := range matches {
		if match.Record.Title != "" {
			titles = append(titles, match.Record.Title)
		}
	}
	return titles
}

func uniqueRepoStrings(matches []types.CodeSearchHit) []string {
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		repo := strings.ToLower(strings.TrimSpace(match.Repo))
		if repo != "" {
			if _, ok := seen[repo]; ok {
				continue
			}
			seen[repo] = struct{}{}
			out = append(out, repo)
		}
	}
	return out
}

func codeMatchSummary(matches []types.CodeSearchHit, label string) string {
	if len(matches) == 0 {
		return "  (" + label + ": empty)"
	}
	var sb strings.Builder
	for i, match := range matches {
		fmt.Fprintf(&sb, "  [%d] %s trust=%d score=%.3f dense=%.3f fusion=%.3f kind=%s\n",
			i,
			shortLogPath(match.Path),
			match.TrustTier,
			match.Score,
			match.SemanticScore,
			match.FusionScore,
			match.ScoreKind,
		)
	}
	return sb.String()
}

func shortLogPath(path string) string {
	path = normalizeServicePath(path)
	parts := strings.Split(path, "/")
	if len(parts) <= 3 {
		return platform.TruncateForLog(path, 100)
	}
	short := ".../" + strings.Join(parts[len(parts)-3:], "/")
	return platform.TruncateForLog(short, 100)
}

// cleanWorkspacePaths rewrites workspace repo paths into service-facing names.
func (retrieve *Retriever) cleanWorkspacePaths(ctx context.Context, text string) string {
	type entry struct{ repo, svc string }
	modules := retrieve.allServiceModules(ctx)
	entries := make([]entry, 0, len(modules))
	for _, module := range modules {
		entries = append(entries, entry{modulePrefix(module), module.ServiceName})
	}
	sort.Slice(entries, func(i, j int) bool { return len(entries[i].repo) > len(entries[j].repo) })

	result := text
	for _, e := range entries {
		result = strings.ReplaceAll(result, "repos/"+e.repo+"/", e.svc+"/")
		result = strings.ReplaceAll(result, e.repo+"/", e.svc+"/")
	}

	return result
}

func normalizeServicePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "workspace/")
	p = strings.TrimPrefix(p, "repos/")
	p = strings.Trim(p, "/")
	return p
}

func pathHasSegmentPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func (retrieve *Retriever) serviceForPath(ctx context.Context, path string) string {
	svc, _, _ := serviceForPathPrefix(retrieve.allServiceModules(ctx), path)
	return svc
}

func (retrieve *Retriever) resolveServiceModules(ctx context.Context, repos []string) []types.ServiceRecord {
	if len(repos) == 0 || retrieve.tools == nil {
		return nil
	}
	modules, err := retrieve.tools.ServiceModules(ctx, repos)
	if err != nil {
		return nil
	}
	return modules
}

func (retrieve *Retriever) serviceForRepoMapped(ctx context.Context, modules []types.ServiceRecord, repo, path string) string {
	if service, _, ok := serviceForPathPrefix(modules, path); ok {
		return service
	}
	if service, ok := soleServiceForRepo(modules, repo); ok {
		return service
	}
	return retrieve.serviceForPath(ctx, path)
}

func (retrieve *Retriever) serviceForRepo(ctx context.Context, repo, path string) string {
	modules := retrieve.allServiceModules(ctx)
	if service, _, ok := serviceForPathPrefix(modules, path); ok {
		return service
	}
	service, _ := soleServiceForRepo(modules, repo)
	return service
}

func (retrieve *Retriever) allServiceModules(ctx context.Context) []types.ServiceRecord {
	if modules, ok := retrieve.serviceModules.Load().([]types.ServiceRecord); ok {
		return modules
	}
	if retrieve.tools == nil {
		return nil
	}
	modules, err := retrieve.tools.ServiceModules(ctx, nil)
	if err != nil {
		return nil
	}
	retrieve.serviceModules.Store(modules)
	return modules
}

func modulePrefix(service types.ServiceRecord) string {
	prefix := normalizeServicePath(service.Repo)
	module := normalizeServicePath(service.ModulePath)
	if module != "" && module != "." {
		prefix += "/" + module
	}
	return prefix
}

func soleServiceForRepo(modules []types.ServiceRecord, repo string) (string, bool) {
	repo = normalizeServicePath(repo)
	service := ""
	for _, module := range modules {
		if normalizeServicePath(module.Repo) != repo {
			continue
		}
		if service != "" {
			return "", false
		}
		service = module.ServiceName
	}
	return service, service != ""
}

func serviceForPathPrefix(modules []types.ServiceRecord, path string) (string, string, bool) {
	path = normalizeServicePath(path)
	bestPrefix := ""
	bestService := ""
	for _, module := range modules {
		prefix := modulePrefix(module)
		if prefix == "" || !pathHasSegmentPrefix(path, prefix) {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			bestService = module.ServiceName
		}
	}
	return bestService, bestPrefix, bestService != ""
}

func (retrieve *Retriever) layerForPath(ctx context.Context, path string) string {
	_, prefix, ok := serviceForPathPrefix(retrieve.allServiceModules(ctx), path)
	if !ok {
		return ""
	}
	for _, module := range retrieve.allServiceModules(ctx) {
		if modulePrefix(module) == prefix {
			return module.Layer
		}
	}
	return ""
}

// shortPath collapses a workspace file path to a service-facing, abbreviated form.
func (retrieve *Retriever) shortPath(ctx context.Context, p string) string {

	p, _ = strings.CutPrefix(p, "repos/")
	if svc, prefix, ok := serviceForPathPrefix(retrieve.allServiceModules(ctx), p); ok && prefix != "" {
		suffix := strings.TrimPrefix(normalizeServicePath(p), prefix)
		suffix = strings.TrimPrefix(suffix, "/")
		if suffix == "" {
			p = svc
		} else {
			p = svc + "/" + suffix
		}
	}
	parts := strings.Split(p, "/")

	if len(parts) > 4 {
		keep := parts
		for i, seg := range parts {
			if seg == "src" && i+2 < len(parts) && parts[i+1] == "main" && parts[i+2] == "java" {
				keep = parts[i+3:]
				break
			}
		}
		if len(keep) > 2 {
			return parts[0] + "/…/" + strings.Join(keep[len(keep)-2:], "/")
		}
		return parts[0] + "/…/" + strings.Join(keep, "/")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, "/")
}
