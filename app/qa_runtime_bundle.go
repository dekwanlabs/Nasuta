package app

import (
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/platform/config"
)

// qaRuntimeBundle is the app-owned assembly produced during a QA reload. The
// dashboard transport never sees this concrete aggregate: it is converted into
// narrow QAApplicationPorts at the composition boundary.
type qaRuntimeBundle struct {
	QA             *agentqa.Service
	Hub            *run.Hub
	CompactionLLM  *llm.LLMClient
	RunStore       *run.Store
	Sessions       *memory.SessionStore
	History        session.History
	Settings       *config.PlatformSettings
	WriteAvailable bool
}
