package dashboard

import (
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

// currentQARuntime returns the platform-owned QA snapshot or the handler's
// legacy fallback dependencies when no platform callback is configured.
func (handler *Handler) currentQARuntime() QARuntime {
	if handler.qaRuntimeFn != nil {
		return handler.qaRuntimeFn()
	}
	return QARuntime{
		QA: handler.qa, RunStore: handler.persistentRunStore,
		Sessions: handler.qaSessions, History: handler.history,
		Settings: handler.platform, WriteAvailable: handler.writeAvailable,
	}
}

// qaService returns the active QA service used by dashboard requests.
func (handler *Handler) qaService() *agent.QA {
	return handler.currentQARuntime().QA
}

// qaSessionStore returns the session store associated with the active runtime.
func (handler *Handler) qaSessionStore() *memory.SessionStore {
	return handler.currentQARuntime().Sessions
}

// platformSettings returns the active platform settings or empty defaults.
func (handler *Handler) platformSettings() *config.PlatformSettings {
	settings := handler.currentQARuntime().Settings
	if settings == nil {
		return &config.PlatformSettings{}
	}
	return settings
}

// applySettings forwards persisted setting changes to the platform lifecycle
// callback, preserving the set of changed keys for rebuild classification.
func (handler *Handler) applySettings(changedKeys []string) error {
	if handler.settingsChangedFn == nil {
		return nil
	}
	if err := handler.settingsChangedFn(changedKeys); err != nil {
		return fmt.Errorf("apply platform settings: %w", err)
	}
	return nil
}

// replaceCodeGraph forwards a rebuilt CodeGraph to the platform lifecycle
// callback so QA can refresh graph-bound retrievers.
func (handler *Handler) replaceCodeGraph(graph *codegraph.DB) error {
	if handler.codeGraphChangedFn == nil {
		return nil
	}
	if err := handler.codeGraphChangedFn(graph); err != nil {
		return fmt.Errorf("replace QA codegraph: %w", err)
	}
	return nil
}
