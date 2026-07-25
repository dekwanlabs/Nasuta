package dashboard

import (
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/log"
)

func (handler *Handler) reloadQA() {
	ps := loadPlatformSettings(handler.authDB)
	handler.platform = ps
	handler.rebuildQA(ps)
	log.Infof("[settings] QA service reloaded (model=%s, timeout=%s, max_steps=%d)",
		handler.platform.LLMModel, time.Duration(handler.platform.AgentTimeout), handler.platform.AgentMaxSteps)
}

func (handler *Handler) rebuildQA(ps *config.PlatformSettings) {
	handler.qa = agent.NewQA(agent.QADeps{Tools: handler.tools, Semantic: handler.semantic, Embedder: handler.embedder, WriteAvailable: handler.writeAvailable, Cfg: handler.cfg, Platform: ps, Registry: handler.registry, CodeGraphDB: handler.codegraphDB, DB: handler.platformDB, RunStore: handler.persistentRunStore, History: handler.history})
	handler.syncPlatform(ps)
}

func (handler *Handler) syncPlatform(ps *config.PlatformSettings) {
	if handler.idx != nil {
		handler.idx.SetPlatform(ps)
	}
}
