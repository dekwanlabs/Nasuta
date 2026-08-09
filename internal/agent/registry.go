package agent

import (
	"github.com/dekwanlabs/nasuta/config"
	agenttools "github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
)

type Registry = tool.Registry

// NewRegistry registers every built-in tool through the public batch API.
func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	return agenttools.NewRegistry(svc, cfg, sessions, history)
}
