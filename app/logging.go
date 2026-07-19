package app

import (
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/log"
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
