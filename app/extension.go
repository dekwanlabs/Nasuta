package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

// ExtensionDeps exposes the stable construction inputs available to an upper-layer application.
type ExtensionDeps struct {
	Settings      config.PlatformSettings
	WorkspaceRoot string
	Database      *sql.DB
	Knowledge     knowledge.API
	ReadTools     *tool.ReadRegistry
}

// APIRegistrar attaches one authenticated application endpoint.
type APIRegistrar = func(string, http.HandlerFunc)

// AgentCatalogContribution adds application-owned agents and capabilities to one version.
type AgentCatalogContribution struct {
	Definitions      []agentapi.Definition
	Capabilities     []agentapi.Capability
	WorkflowBindings []WorkflowBindingContribution
}

// WorkflowBindingContribution binds one exact application capability to a
// durable Workflow and its server-owned input builder.
type WorkflowBindingContribution struct {
	Binding agentapi.WorkflowBinding
	Builder agentapi.WorkflowEscalationInputBuilder
}

// AgentCatalogProvider builds application-owned catalog entries from platform settings.
type AgentCatalogProvider interface {
	AgentCatalog(config.PlatformSettings, int64) (AgentCatalogContribution, error)
}

// AgentCatalogProviderFunc adapts a function to AgentCatalogProvider.
type AgentCatalogProviderFunc func(
	config.PlatformSettings,
	int64,
) (AgentCatalogContribution, error)

func (provide AgentCatalogProviderFunc) AgentCatalog(
	settings config.PlatformSettings,
	version int64,
) (AgentCatalogContribution, error) {
	return provide(settings, version)
}

// Extension is the application-owned surface mounted onto one platform host.
type Extension struct {
	RegisterRoutes       func(APIRegistrar)
	WebHandler           http.Handler
	IncidentEvidence     incident.EvidenceProvider
	ConfigResolver       config.Resolver
	AgentCatalogProvider AgentCatalogProvider
	Close                func() error
}

// ExtensionFactory constructs one application extension from stable platform ports.
type ExtensionFactory func(ExtensionDeps) (Extension, error)

// Run owns the fixed platform lifecycle around one explicit application extension.
func Run(ctx context.Context, factory ExtensionFactory) (runErr error) {
	platform, err := New()
	if err != nil {
		return fmt.Errorf("build platform: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, platform.Close())
	}()

	var extension Extension
	if factory != nil {
		extension, err = factory(platform.extensionDeps())
		if err != nil {
			return fmt.Errorf("build application extension: %w", err)
		}
		platform.index.SetConfigResolver(extension.ConfigResolver)
		if extension.Close != nil {
			defer func() {
				runErr = errors.Join(runErr, extension.Close())
			}()
		}
	}
	if err := platform.configureIncidents(extension.IncidentEvidence); err != nil {
		return err
	}
	if err := platform.configureAgentCatalogProvider(extension.AgentCatalogProvider); err != nil {
		return err
	}
	if err := platform.initializePlatformRuntime(); err != nil {
		return fmt.Errorf("initialize platform runtime: %w", err)
	}

	mux := http.NewServeMux()
	platform.RegisterCommonRoutes(mux)
	mountExtension(platform, mux, extension)
	return platform.Serve(ctx, mux)
}

// configureAgentCatalogProvider records an extension catalog provider before
// startup or rebuilds the active QA runtime after startup.
func (platform *Platform) configureAgentCatalogProvider(
	provider AgentCatalogProvider,
) error {
	platform.qa.reload.Lock()
	defer platform.qa.reload.Unlock()

	platform.qa.mu.RLock()
	initialized := platform.qa.current.Settings != nil
	settings := platform.settings
	graph := platform.graph
	platform.qa.mu.RUnlock()

	previous := platform.agents.provider
	platform.agents.provider = provider
	if !initialized {
		return nil
	}
	if err := platform.rebuildQARuntimeLocked(settings, graph); err != nil {
		platform.agents.provider = previous
		return fmt.Errorf("rebuild QA runtime with application agent catalog: %w", err)
	}
	return nil
}

func mountExtension(platform *Platform, mux *http.ServeMux, extension Extension) {
	if extension.RegisterRoutes != nil {
		extension.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	if extension.WebHandler != nil {
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			extension.WebHandler.ServeHTTP(w, r)
		}))
	}
}

// MustRun starts one extension host and terminates on construction or serving failure.
func MustRun(factory ExtensionFactory) {
	if err := Run(context.Background(), factory); err != nil {
		log.Fatalf("run application: %v", err)
	}
}

func (platform *Platform) extensionDeps() ExtensionDeps {
	return ExtensionDeps{
		Settings:      platform.Settings(),
		WorkspaceRoot: platform.WorkspaceRoot(),
		Database:      platform.db,
		Knowledge:     platform.Knowledge(),
		ReadTools:     platform.ReadTools(),
	}
}
