package retrieval

import (
	"context"
	"testing"

	"github.com/dekwanlabs/astris/config"
	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/internal/platform/graph"
)

type servicePathFakeTools struct {
	services []types.ServiceRecord
}

func (f servicePathFakeTools) AllServices(context.Context) ([]types.ServiceRecord, error) {
	return f.services, nil
}

func (f servicePathFakeTools) FindServices(context.Context, string, int) (types.SearchResult[types.ServiceRecord], error) {
	return types.SearchResult[types.ServiceRecord]{}, nil
}

func (f servicePathFakeTools) FindCode(context.Context, string, string, int) (types.SearchResult[types.CodeSearchHit], error) {
	return types.SearchResult[types.CodeSearchHit]{}, nil
}

func (f servicePathFakeTools) FindAPIs(context.Context, string, string, int) ([]types.EndpointRecord, error) {
	return nil, nil
}

func (f servicePathFakeTools) FindRunbooks(context.Context, string, int, bool, string) (types.SearchResult[types.RunbookSearchHit], error) {
	return types.SearchResult[types.RunbookSearchHit]{}, nil
}

func (f servicePathFakeTools) TraceDeps(string, string, int) graph.Result {
	return graph.Result{}
}

func (f servicePathFakeTools) ServiceModules(_ context.Context, repos []string) ([]types.ServiceRecord, error) {
	if len(repos) == 0 {
		return f.services, nil
	}
	wanted := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		wanted[repo] = struct{}{}
	}
	modules := make([]types.ServiceRecord, 0, len(f.services))
	for _, svc := range f.services {
		if _, ok := wanted[svc.Repo]; ok {
			modules = append(modules, svc)
		}
	}
	return modules, nil
}

func TestServiceForPathUsesModulePathBeforeRepo(t *testing.T) {
	r := New(servicePathFakeTools{services: []types.ServiceRecord{
		{
			ServiceName: "hsas-cookbook",
			Repo:        "hsas/hsas-dreo-app",
			ModulePath:  "hsas-cookbook",
		},
		{
			ServiceName: "hsas-upgrade",
			Repo:        "hsas/hsas-dreo-app",
			ModulePath:  "hsas-upgrade",
		},
	}}, config.Config{})

	cookbookPath := "repos/hsas/hsas-dreo-app/hsas-cookbook/src/main/java/com/hesung/hsas/cook/controller/customized/H5RecipeController.java"
	if got := r.serviceForPath(context.Background(), cookbookPath); got != "hsas-cookbook" {
		t.Fatalf("cookbook path resolved to %q", got)
	}
	if got := r.serviceForPath(context.Background(), "repos/hsas/hsas-dreo-app/hsas-upgrade/src/main/java/UpgradeController.java"); got != "hsas-upgrade" {
		t.Fatalf("upgrade path resolved to %q", got)
	}
	if got := r.serviceForPath(context.Background(), "repos/hsas/hsas-dreo-app/README.md"); got != "" {
		t.Fatalf("ambiguous repo root should not resolve to a service, got %q", got)
	}
}
