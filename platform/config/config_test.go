package config

import (
	"path/filepath"
	"testing"

	"github.com/dekwanlabs/nasuta/platform"
)

func TestLoadUsesNasutaEnvironment(t *testing.T) {
	t.Setenv("NASUTA_HTTP_ADDR", ":9100")
	t.Setenv("NASUTA_LOG_MAX_AGE", "45")

	cfg := Load()

	if cfg.HTTPAddr != ":9100" {
		t.Fatalf("HTTPAddr = %q, want Nasuta value", cfg.HTTPAddr)
	}
	if cfg.Log.MaxAge != 45 {
		t.Fatalf("Log.MaxAge = %d, want Nasuta value", cfg.Log.MaxAge)
	}
}

func TestLoadDefaultsSQLiteToWorkspaceMetadataDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NASUTA_WORKSPACE_ROOT", root)
	t.Setenv("NASUTA_SQLITE_PATH", "")

	cfg := Load()
	want := filepath.Join(root, platform.WorkspaceMetadataDir, "index.db")
	if cfg.SQLitePath != want {
		t.Fatalf("SQLitePath = %q, want %q", cfg.SQLitePath, want)
	}
}

func TestLoadKeepsQdrantConfigurationCompatible(t *testing.T) {
	t.Setenv("QDRANT_HOST", "qdrant.internal")
	t.Setenv("QDRANT_PORT", "7334")
	t.Setenv("QDRANT_COLLECTION", "company-knowledge")

	cfg := Load()

	if cfg.Semantic.Provider != "qdrant" || cfg.Semantic.Endpoint != "qdrant.internal:7334" || cfg.Semantic.Collection != "company-knowledge" {
		t.Fatalf("unexpected semantic config: provider=%q endpoint=%q collection=%q", cfg.Semantic.Provider, cfg.Semantic.Endpoint, cfg.Semantic.Collection)
	}
}

func TestLoadPrefersNasutaSemanticConfiguration(t *testing.T) {
	t.Setenv("NASUTA_SEMANTIC_PROVIDER", "milvus")
	t.Setenv("SEMANTIC_PROVIDER", "qdrant")
	t.Setenv("NASUTA_SEMANTIC_ENDPOINT", "milvus.internal:19530")
	t.Setenv("SEMANTIC_ENDPOINT", "localhost:6334")
	t.Setenv("NASUTA_SEMANTIC_COLLECTION", "company-knowledge")

	cfg := Load()

	if cfg.Semantic.Provider != "milvus" || cfg.Semantic.Endpoint != "milvus.internal:19530" || cfg.Semantic.Collection != "company-knowledge" {
		t.Fatalf("unexpected semantic precedence result: %+v", cfg.Semantic)
	}
}

func TestLoadLeavesSemanticProviderEmptyWithoutBackendAddress(t *testing.T) {
	for _, key := range []string{
		"NASUTA_SEMANTIC_PROVIDER", "SEMANTIC_PROVIDER",
		"NASUTA_SEMANTIC_ENDPOINT", "SEMANTIC_ENDPOINT",
		"NASUTA_QDRANT_HOST", "QDRANT_HOST",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()

	if cfg.Semantic.Provider != "" || cfg.Semantic.Endpoint != "" {
		t.Fatalf("semantic config = %+v, want provider and endpoint empty", cfg.Semantic)
	}
}

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
