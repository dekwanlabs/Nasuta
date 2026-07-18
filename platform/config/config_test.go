package config

import "testing"

func TestLoadCodeGraphContainerDefaultsToLocalCLI(t *testing.T) {
	t.Setenv("CODEGRAPH_CONTAINER", "")

	cfg := Load()

	if cfg.CodeGraphContainer != "" {
		t.Fatalf("CodeGraphContainer = %q, want local CLI", cfg.CodeGraphContainer)
	}
}

func TestLoadCodeGraphContainerUsesExplicitValue(t *testing.T) {
	t.Setenv("CODEGRAPH_CONTAINER", "codegraph-test")

	cfg := Load()

	if cfg.CodeGraphContainer != "codegraph-test" {
		t.Fatalf("CodeGraphContainer = %q, want %q", cfg.CodeGraphContainer, "codegraph-test")
	}
}
