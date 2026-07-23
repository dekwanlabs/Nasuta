package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	Knowledge     knowledge.API
	ReadTools     *tool.ReadRegistry
}

// APIRegistrar attaches one authenticated application endpoint.
type APIRegistrar = func(string, http.HandlerFunc)

// Extension is the application-owned surface mounted onto one platform host.
type Extension struct {
	RegisterRoutes   func(APIRegistrar)
	IncidentEvidence incident.EvidenceProvider
	Close            func() error
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
		if extension.Close != nil {
			defer func() {
				runErr = errors.Join(runErr, extension.Close())
			}()
		}
	}
	if err := platform.configureIncidents(extension.IncidentEvidence); err != nil {
		return err
	}

	mux := http.NewServeMux()
	platform.RegisterCommonRoutes(mux)
	if extension.RegisterRoutes != nil {
		extension.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	return platform.Serve(ctx, mux)
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
		Knowledge:     platform.Knowledge(),
		ReadTools:     platform.ReadTools(),
	}
}
