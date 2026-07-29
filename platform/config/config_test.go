package config

import (
	"path/filepath"
	"testing"

	"github.com/dekwanlabs/nasuta/platform"
)

func TestLoadUsesNasutaEnvironment(t *testing.T) {
	t.Setenv("NASUTA_HTTP_ADDR", ":9100")
	t.Setenv("NASUTA_LOG_MAX_AGE", "45")
	t.Setenv("NASUTA_CODEX_BIN", "/opt/codex")
	t.Setenv("NASUTA_CLAUDE_BIN", "/opt/claude")
	t.Setenv("NASUTA_CODING_WORK_ROOT", "/var/lib/nasuta-coding")

	cfg := Load()

	if cfg.HTTPAddr != ":9100" {
		t.Fatalf("HTTPAddr = %q, want Nasuta value", cfg.HTTPAddr)
	}
	if cfg.Log.MaxAge != 45 {
		t.Fatalf("Log.MaxAge = %d, want Nasuta value", cfg.Log.MaxAge)
	}
	if cfg.CodexBin != "/opt/codex" || cfg.ClaudeBin != "/opt/claude" {
		t.Fatalf("coding binaries = %q, %q", cfg.CodexBin, cfg.ClaudeBin)
	}
	if cfg.CodingWorkRoot != "/var/lib/nasuta-coding" {
		t.Fatalf("CodingWorkRoot = %q", cfg.CodingWorkRoot)
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

func TestLoadDefaultsCodingWorkRootToWorkspaceMetadataDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NASUTA_WORKSPACE_ROOT", root)
	t.Setenv("NASUTA_CODING_WORK_ROOT", "")

	cfg := Load()
	want := filepath.Join(root, platform.WorkspaceMetadataDir, "coding")
	if cfg.CodingWorkRoot != want {
		t.Fatalf("CodingWorkRoot = %q, want %q", cfg.CodingWorkRoot, want)
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

func TestLoadOntologyDefaultsToSQLiteAndNormalizesNeo4jConfig(t *testing.T) {
	t.Setenv("NASUTA_ONTOLOGY_PROVIDER", "NEO4J")
	t.Setenv("NASUTA_ONTOLOGY_NEO4J_URI", "neo4j://graph.internal:7687")
	t.Setenv("NASUTA_ONTOLOGY_NEO4J_USERNAME", "reader")
	t.Setenv("NASUTA_ONTOLOGY_NEO4J_DATABASE", "knowledge")

	cfg := Load()
	if cfg.Ontology.Provider != "neo4j" || cfg.Ontology.Neo4j.URI != "neo4j://graph.internal:7687" || cfg.Ontology.Neo4j.Database != "knowledge" {
		t.Fatalf("ontology config = %+v", cfg.Ontology)
	}
}
