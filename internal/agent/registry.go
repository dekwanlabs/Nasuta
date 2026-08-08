package agent

import (
	"github.com/dekwanlabs/nasuta/config"
	agenttools "github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
)

type Tool = tool.Tool
type Registry = tool.Registry
type ToolPolicy = tool.Policy

const (
	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite
)

// ToolPolicyForRun fixes the tool permission set for one run.
func ToolPolicyForRun(allowWrite bool) ToolPolicy {
	return ToolPolicy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

// NewRegistry registers every built-in tool through the public batch API.
func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	return agenttools.NewRegistry(svc, cfg, sessions, history)
}
