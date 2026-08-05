package dashboard

import (
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

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

func (handler *Handler) qaService() *agent.QA {
	return handler.currentQARuntime().QA
}

func (handler *Handler) qaSessionStore() *memory.SessionStore {
	return handler.currentQARuntime().Sessions
}

func (handler *Handler) platformSettings() *config.PlatformSettings {
	settings := handler.currentQARuntime().Settings
	if settings == nil {
		return &config.PlatformSettings{}
	}
	return settings
}

func (handler *Handler) reloadQA(graph *codegraph.DB) error {
	if handler.reloadQAFn == nil {
		return nil
	}
	if err := handler.reloadQAFn(graph); err != nil {
		return fmt.Errorf("reload QA runtime: %w", err)
	}
	return nil
}
