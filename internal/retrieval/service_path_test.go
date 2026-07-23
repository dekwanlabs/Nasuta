package retrieval

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

type servicePathFakeTools struct {
	services []domain.ServiceRecord
}

func (f servicePathFakeTools) AllServices(context.Context) ([]domain.ServiceRecord, error) {
	return f.services, nil
}

func (f servicePathFakeTools) FindServices(context.Context, string, int) (domain.SearchResult[domain.ServiceRecord], error) {
	return domain.SearchResult[domain.ServiceRecord]{}, nil
}

func (f servicePathFakeTools) FindCode(context.Context, string, string, int) (domain.SearchResult[domain.CodeSearchHit], error) {
	return domain.SearchResult[domain.CodeSearchHit]{}, nil
}

func (f servicePathFakeTools) FindAPIs(context.Context, string, string, int) ([]domain.EndpointRecord, error) {
	return nil, nil
}

func (f servicePathFakeTools) FindRunbooks(context.Context, string, int, bool, string) (domain.SearchResult[domain.RunbookSearchHit], error) {
	return domain.SearchResult[domain.RunbookSearchHit]{}, nil
}

func (f servicePathFakeTools) TraceDeps(context.Context, string, string, int) (domain.DependencyTrace, error) {
	return domain.DependencyTrace{}, nil
}

func (f servicePathFakeTools) ServiceModules(_ context.Context, repos []string) ([]domain.ServiceRecord, error) {
	if len(repos) == 0 {
		return f.services, nil
	}
	wanted := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		wanted[repo] = struct{}{}
	}
	modules := make([]domain.ServiceRecord, 0, len(f.services))
	for _, svc := range f.services {
		if _, ok := wanted[svc.Repo]; ok {
			modules = append(modules, svc)
		}
	}
	return modules, nil
}

func TestServiceForPathUsesModulePathBeforeRepo(t *testing.T) {
	r := New(servicePathFakeTools{services: []domain.ServiceRecord{
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
