package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	Definitions  []agentapi.Definition
	Capabilities []agentapi.Capability
}

// AgentCatalogProvider builds application-owned catalog entries from platform settings.
type AgentCatalogProvider interface {
	AgentCatalog(config.PlatformSettings, int64) (AgentCatalogContribution, error)
}

// InvestigationTemplate is the public application-owned template shape. It is
// converted to the internal investigation catalog after platform startup so
// upper-layer applications do not depend on internal packages.
type InvestigationTemplate struct {
	ID             string
	Version        int64
	GoalKinds      []string
	RequiredInputs []string
	Provides       []string
	ToolGrant      []tool.ToolID
	InputSchema    agentapi.SchemaRef
	OutputSchema   agentapi.SchemaRef
	Executor       InvestigationExecutorType
	ToolCalls      []InvestigationToolCallSpec
	CostProfile    InvestigationBudgetVector
	MaxAttempts    int
	Enabled        bool
}

type InvestigationExecutorType string

const (
	InvestigationExecutorDirectTool   InvestigationExecutorType = "direct_tool"
	InvestigationExecutorToolPipeline InvestigationExecutorType = "tool_pipeline"
	InvestigationExecutorInvestigator InvestigationExecutorType = "investigator"
	InvestigationExecutorVerifier     InvestigationExecutorType = "verifier"
	InvestigationExecutorComposer     InvestigationExecutorType = "composer"
)

type InvestigationToolCallSpec struct {
	ToolID tool.ToolID
	Args   tool.Arguments
}

type InvestigationBudgetVector struct {
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int
	Duration     time.Duration
	CostMicros   int64
}

// InvestigationTemplateProvider contributes application-owned investigation
// task templates. The platform still owns template validation and planning.
type InvestigationTemplateProvider interface {
	InvestigationTemplates() ([]InvestigationTemplate, error)
}

// InvestigationTemplateProviderFunc adapts a function to
// InvestigationTemplateProvider.
type InvestigationTemplateProviderFunc func() ([]InvestigationTemplate, error)

func (provide InvestigationTemplateProviderFunc) InvestigationTemplates() ([]InvestigationTemplate, error) {
	return provide()
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
	RegisterRoutes                func(APIRegistrar)
	WebHandler                    http.Handler
	IncidentEvidence              incident.EvidenceProvider
	ConfigResolver                config.Resolver
	AgentCatalogProvider          AgentCatalogProvider
	InvestigationTemplateProvider InvestigationTemplateProvider
	Close                         func() error
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
	if err := platform.configureInvestigationTemplateProvider(extension.InvestigationTemplateProvider); err != nil {
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

// configureInvestigationTemplateProvider records application-owned
// investigation templates before runtime initialization.
func (platform *Platform) configureInvestigationTemplateProvider(
	provider InvestigationTemplateProvider,
) error {
	if platform == nil {
		return fmt.Errorf("platform is required")
	}
	platform.investigationTemplateProvider = provider
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
	// Keep the process lifecycle tied to the host signals used by terminals,
	// IDEs, and debuggers. Run can then shut down the HTTP server and release
	// the configured port instead of leaving a stale service behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, factory); err != nil {
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
