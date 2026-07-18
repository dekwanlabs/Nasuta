package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Duration is a config-friendly time.Duration that parses from env strings
// ("30s", "2m"). Zero value means "use the default".
type Duration time.Duration

// LogConfig controls file logging + daily rotation, loaded from CODELOOM_LOG_*
// env vars. See internal/log.Options for the sink behavior.
type LogConfig struct {
	File       string
	Stdout     bool
	MaxBackups int
	MaxAge     int
	Compress   bool
}

// Config holds runtime configuration sourced from environment variables.
// Platform-settable settings (VCS, LLM, Agent) live in PlatformSettings
// and are populated from the MySQL settings table.
type Config struct {
	WorkspaceRoot string
	ScanDirs      []string
	IndexCode     bool
	SQLitePath    string

	MemoryWorkContextTTL time.Duration

	HTTPAddr  string
	AuthToken string

	// WebSearchEnabled gates the web_search / web_fetch tools.
	// When true (default), the QA agent can search the web.
	WebSearchEnabled    bool
	WebSearchMCPEnabled bool   // expose web tools to MCP clients when also enabled
	WebSearchEngine     string // duckduckgo (default) | brave | bing | searxng
	WebSearchAPIKey     string // API key for brave / tavily / etc.

	QdrantHost       string
	QdrantPort       int
	QdrantAPIKey     string
	QdrantCollection string

	EmbeddingProvider    string
	EmbeddingAPIKey      string
	EmbeddingModel       string
	EmbeddingBaseURL     string
	EmbeddingDim         int
	EmbeddingBatch       int
	EmbeddingConcurrency int

	CodeGraphContainer string // docker container running the codegraph CLI (rebuild indexes via `docker exec`)

	FeishuAppID       string
	FeishuAppSecret   string
	FeishuRedirectURI string
	WebBaseURL        string

	Log LogConfig
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return def
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return def
	}
	return duration
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ParseExcludeList parses exclude patterns: one per line, # comments, commas.
func ParseExcludeList(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, tok := range strings.Split(line, ",") {
			if t := strings.TrimSpace(tok); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func Load() Config {
	// Auto-load .env from cwd (graceful miss — no error when file absent).
	// Also try parent directories — IDE run configs often set the working dir to a
	// subdirectory (e.g. scenario/cmd/codeloom-server) rather than the project root.
	_ = godotenv.Load()
	if _, err := os.Stat(".env"); os.IsNotExist(err) {
		_ = godotenv.Load("../.env")
		_ = godotenv.Load("../../.env")
	}

	root := env("CODELOOM_WORKSPACE_ROOT", cwdOrDot())
	root, _ = filepath.Abs(root)

	return Config{
		WorkspaceRoot:        root,
		ScanDirs:             splitList(env("CODELOOM_SCAN_DIRS", "")),
		IndexCode:            envBool("CODELOOM_INDEX_CODE", true),
		MemoryWorkContextTTL: envDuration("CODELOOM_MEMORY_WORK_CONTEXT_TTL", 30*24*time.Hour),
		WebSearchEnabled:     envBool("CODELOOM_WEB_SEARCH_ENABLED", true),
		WebSearchMCPEnabled:  envBool("CODELOOM_WEB_SEARCH_MCP_ENABLED", false),
		WebSearchEngine:      env("CODELOOM_WEB_SEARCH_ENGINE", "duckduckgo"),
		WebSearchAPIKey:      env("CODELOOM_WEB_SEARCH_API_KEY", ""),
		SQLitePath:           env("CODELOOM_SQLITE_PATH", filepath.Join(root, ".mcp-index", "index.db")),
		HTTPAddr:             env("CODELOOM_HTTP_ADDR", ":8201"),
		AuthToken:            env("CODELOOM_AUTH_TOKEN", ""),

		QdrantHost:           env("QDRANT_HOST", ""),
		QdrantPort:           envInt("QDRANT_PORT", 6334),
		QdrantAPIKey:         env("QDRANT_API_KEY", ""),
		QdrantCollection:     env("QDRANT_COLLECTION", "knowledge"),
		EmbeddingProvider:    env("EMBEDDING_PROVIDER", "openai"),
		EmbeddingAPIKey:      env("EMBEDDING_API_KEY", ""),
		EmbeddingModel:       env("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingBaseURL:     env("EMBEDDING_BASE_URL", ""),
		EmbeddingDim:         envInt("EMBEDDING_DIM", 1536),
		EmbeddingBatch:       envInt("EMBEDDING_BATCH", 16),
		EmbeddingConcurrency: envInt("EMBEDDING_CONCURRENCY", 4),

		CodeGraphContainer: env("CODEGRAPH_CONTAINER", ""),

		FeishuAppID:       env("FEISHU_APP_ID", ""),
		FeishuAppSecret:   env("FEISHU_APP_SECRET", ""),
		FeishuRedirectURI: env("FEISHU_REDIRECT_URI", ""),
		WebBaseURL:        env("WEB_BASE_URL", "http://localhost:5173"),

		Log: LogConfig{
			File:       env("CODELOOM_LOG_FILE", filepath.Join(root, "logs", "all.log")),
			Stdout:     envBool("CODELOOM_LOG_STDOUT", true),
			MaxBackups: envInt("CODELOOM_LOG_MAX_BACKUPS", 7),
			MaxAge:     envInt("CODELOOM_LOG_MAX_AGE", 30),
			Compress:   envBool("CODELOOM_LOG_COMPRESS", false),
		},
	}
}

func cwdOrDot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (c Config) FeishuConfigured() bool { return c.FeishuAppID != "" && c.FeishuAppSecret != "" }
