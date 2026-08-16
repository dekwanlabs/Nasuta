package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestExtensionDepsDetachSettingsAndExposeStablePorts(t *testing.T) {
	registry := tool.NewRegistry()
	readTools := tool.NewReadRegistry(registry)
	settings := &config.PlatformSettings{
		VCSGroups:          []string{"group-a"},
		VCSExcludeProjects: []string{"repo-a"},
	}
	platform := &Platform{
		cfg:      config.Config{WorkspaceRoot: "/workspace"},
		settings: settings,
		reads:    readTools,
	}

	deps := platform.extensionDeps()
	deps.Settings.VCSGroups[0] = "changed"
	deps.Settings.VCSExcludeProjects[0] = "changed"

	if settings.VCSGroups[0] != "group-a" || settings.VCSExcludeProjects[0] != "repo-a" {
		t.Fatalf("extension mutated platform settings: %#v", settings)
	}
	if deps.WorkspaceRoot != "/workspace" || deps.ReadTools != readTools {
		t.Fatalf("extension deps = %#v", deps)
	}
}

func TestConfigureAgentCatalogProviderBeforeInitializationOnlyRecordsProvider(t *testing.T) {
	calls := 0
	provider := AgentCatalogProviderFunc(func(
		config.PlatformSettings,
		int64,
	) (AgentCatalogContribution, error) {
		calls++
		return AgentCatalogContribution{}, nil
	})
	platform := &Platform{
		settings: &config.PlatformSettings{},
	}

	if err := platform.configureAgentCatalogProvider(provider); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("provider was invoked %d times before runtime initialization", calls)
	}
	if platform.agents.provider == nil {
		t.Fatal("provider was not recorded for runtime initialization")
	}
}

func TestMountExtensionRegistersAPIAndWebHandler(t *testing.T) {
	platform := &Platform{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mountExtension(platform, mux, Extension{
		RegisterRoutes: func(api APIRegistrar) {
			api("GET /api/extension", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
		},
		WebHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("web"))
		}),
	})

	apiResponse := httptest.NewRecorder()
	mux.ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/extension", nil))
	if apiResponse.Code != http.StatusNoContent {
		t.Fatalf("API status = %d; want %d", apiResponse.Code, http.StatusNoContent)
	}

	webResponse := httptest.NewRecorder()
	mux.ServeHTTP(webResponse, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if webResponse.Code != http.StatusOK || strings.TrimSpace(webResponse.Body.String()) != "web" {
		t.Fatalf("web response = (%d, %q)", webResponse.Code, webResponse.Body.String())
	}

	authResponse := httptest.NewRecorder()
	mux.ServeHTTP(authResponse, httptest.NewRequest(http.MethodGet, "/auth/callback", nil))
	if authResponse.Code != http.StatusAccepted {
		t.Fatalf("auth callback status = %d; want %d", authResponse.Code, http.StatusAccepted)
	}

	methodResponse := httptest.NewRecorder()
	mux.ServeHTTP(methodResponse, httptest.NewRequest(http.MethodPost, "/dashboard", nil))
	if methodResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST web status = %d; want %d", methodResponse.Code, http.StatusMethodNotAllowed)
	}
}
