package app

import (
	"testing"

	"github.com/dekwanlabs/nasuta/config"
)

func TestSettingsReturnsDetachedCodingProviders(t *testing.T) {
	platform := &Platform{settings: &config.PlatformSettings{
		CodingEnabledProviders: []string{"codex", "claude"},
	}}
	copy := platform.Settings()
	copy.CodingEnabledProviders[0] = "changed"
	if platform.settings.CodingEnabledProviders[0] != "codex" {
		t.Fatal("Settings returned the platform coding provider slice")
	}
}
