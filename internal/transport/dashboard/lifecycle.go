package dashboard

import (
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

// currentQAPorts returns the platform-owned QA boundary snapshot. The handler
// carries no QA aggregate; the app callback is authoritative and read per use.
func (handler *Handler) currentQAPorts() QAApplicationPorts {
	if handler.qaPortsFn == nil {
		return QAApplicationPorts{}
	}
	return handler.qaPortsFn()
}

// qaSessionStore returns the session store associated with the active runtime.
func (handler *Handler) qaSessionStore() QASessionStorePort {
	return handler.currentQAPorts().SessionStore
}

// qaRunStore returns the run store associated with the active runtime.
func (handler *Handler) qaRunStore() QARunStorePort {
	return handler.currentQAPorts().RunStore
}

// qaMemoryStore returns the memory store associated with the active runtime.
func (handler *Handler) qaMemoryStore() QAMemoryStorePort {
	return handler.currentQAPorts().MemoryStore
}

// qaRuntimeStatus returns the read-only status port for the active runtime.
func (handler *Handler) qaRuntimeStatus() QARuntimeStatusPort {
	return handler.currentQAPorts().RuntimeStatus
}

// qaApplication returns the active QA application used by ask requests.
func (handler *Handler) qaApplication() QAApplicationPort {
	return handler.currentQAPorts().Application
}

// platformSettings returns the active platform settings or empty defaults.
func (handler *Handler) platformSettings() *config.PlatformSettings {
	settings := handler.currentQAPorts().Settings
	if settings == nil {
		return &config.PlatformSettings{}
	}
	return settings
}

// writeAvailable reports whether write actions are currently authorized.
func (handler *Handler) writeAvailable() bool {
	return handler.currentQAPorts().WriteAvailable
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
