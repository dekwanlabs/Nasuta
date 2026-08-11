package agent

import (
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
)

type Registry = tool.Registry

// NewRegistry registers every built-in tool through the public batch API.
func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	return tools.NewRegistry(svc, cfg, sessions, history)
}
