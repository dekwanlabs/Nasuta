package application

import (
	"github.com/dekwanlabs/astris/config"
	"github.com/dekwanlabs/astris/log"
)

// InitLogging installs the process-wide platform log sink before other capabilities start.
func InitLogging(cfg config.LogConfig) {
	if err := log.Init(log.Options{
		File:       cfg.File,
		Stdout:     cfg.Stdout,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   cfg.Compress,
	}); err != nil {
		// log.Init already falls back to console and reports the failure.
		_ = err
	}
}
