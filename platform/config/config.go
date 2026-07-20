package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/platform"
	"github.com/joho/godotenv"
)

// Duration is a config-friendly time.Duration that parses from env strings
// ("30s", "2m"). Zero value means "use the default".
type Duration time.Duration

// LogConfig controls file logging and daily rotation.
type LogConfig struct {
	File       string
	Stdout     bool
	MaxBackups int
	MaxAge     int
	Compress   bool
}

// SemanticConfig is the normalized connection contract shared by semantic providers.
type SemanticConfig struct {
	Provider   string
	Endpoint   string
	Collection string
	Namespace  string
	Auth       SemanticAuth
	TLS        SemanticTLS
}

type SemanticAuth struct {
	APIKey   string
	Username string
	Password string
}

type SemanticTLS struct {
	Enabled    bool
	CAFile     string
	ServerName string
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

	HTTPAddr      string
	AuthToken     string
	DailySyncTime string

	// WebSearchEnabled gates the web_search / web_fetch tools.
	// When true (default), the QA agent can search the web.
	WebSearchEnabled    bool
	WebSearchMCPEnabled bool   // expose web tools to MCP clients when also enabled
	WebSearchEngine     string // duckduckgo (default) | brave | bing
	WebSearchAPIKey     string // API key for brave / tavily / etc.

	Semantic SemanticConfig

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

	AlertWebhookSecret  string
	NotifyFeishuWebhook string
	NotifyWecomWebhook  string
	NotifyHTTPWebhook   string
	FixDefaultAssignee  string
	FixBranchPrefix     string

	Log LogConfig
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envFirst(def string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return def
}

func nasutaEnv(name, def string) string {
	return env("NASUTA_"+name, def)
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func nasutaEnvInt(name string, def int) int {
	value := strings.TrimSpace(os.Getenv("NASUTA_" + name))
	if value == "" {
		return def
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return def
	}
	return number
}

func nasutaEnvBool(name string, def bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("NASUTA_" + name)))
	switch value {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envBool(key string, def bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func nasutaEnvDuration(name string, def time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv("NASUTA_" + name))
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

	root := nasutaEnv("WORKSPACE_ROOT", cwdOrDot())
	root, _ = filepath.Abs(root)

	return Config{
		WorkspaceRoot:        root,
		ScanDirs:             splitList(nasutaEnv("SCAN_DIRS", "")),
		IndexCode:            nasutaEnvBool("INDEX_CODE", true),
		MemoryWorkContextTTL: nasutaEnvDuration("MEMORY_WORK_CONTEXT_TTL", 30*24*time.Hour),
		WebSearchEnabled:     nasutaEnvBool("WEB_SEARCH_ENABLED", true),
		WebSearchMCPEnabled:  nasutaEnvBool("WEB_SEARCH_MCP_ENABLED", false),
		WebSearchEngine:      nasutaEnv("WEB_SEARCH_ENGINE", "duckduckgo"),
		WebSearchAPIKey:      nasutaEnv("WEB_SEARCH_API_KEY", ""),
		SQLitePath:           nasutaEnv("SQLITE_PATH", filepath.Join(root, platform.WorkspaceMetadataDir, "index.db")),
		HTTPAddr:             nasutaEnv("HTTP_ADDR", ":8201"),
		AuthToken:            nasutaEnv("AUTH_TOKEN", ""),
		DailySyncTime:        nasutaEnv("DAILY_SYNC_TIME", "02:07"),

		Semantic:             loadSemanticConfig(),
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

		AlertWebhookSecret:  env("ALERT_WEBHOOK_SECRET", ""),
		NotifyFeishuWebhook: env("NOTIFY_FEISHU_WEBHOOK", ""),
		NotifyWecomWebhook:  env("NOTIFY_WECOM_WEBHOOK", ""),
		NotifyHTTPWebhook:   env("NOTIFY_HTTP_WEBHOOK", ""),
		FixDefaultAssignee:  env("FIX_DEFAULT_ASSIGNEE", ""),
		FixBranchPrefix:     env("FIX_BRANCH_PREFIX", "hotfix"),

		Log: LogConfig{
			File:       nasutaEnv("LOG_FILE", filepath.Join(root, "logs", "all.log")),
			Stdout:     nasutaEnvBool("LOG_STDOUT", true),
			MaxBackups: nasutaEnvInt("LOG_MAX_BACKUPS", 7),
			MaxAge:     nasutaEnvInt("LOG_MAX_AGE", 30),
			Compress:   nasutaEnvBool("LOG_COMPRESS", false),
		},
	}
}

func loadSemanticConfig() SemanticConfig {
	provider := strings.ToLower(semanticEnv("PROVIDER", ""))
	endpoint := semanticEnv("ENDPOINT", "")
	if endpoint == "" {
		if host := envFirst("", "NASUTA_QDRANT_HOST", "QDRANT_HOST"); host != "" {
			port := 6334
			if raw := envFirst("", "NASUTA_QDRANT_PORT", "QDRANT_PORT"); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil {
					port = parsed
				}
			}
			endpoint = qdrantEndpoint(host, port)
		}
	}
	if provider == "" && endpoint != "" {
		provider = "qdrant"
	}
	return SemanticConfig{
		Provider:   provider,
		Endpoint:   endpoint,
		Collection: semanticEnv("COLLECTION", envFirst("knowledge", "NASUTA_QDRANT_COLLECTION", "QDRANT_COLLECTION")),
		Namespace:  semanticEnv("NAMESPACE", "default"),
		Auth: SemanticAuth{
			APIKey:   semanticEnv("API_KEY", envFirst("", "NASUTA_QDRANT_API_KEY", "QDRANT_API_KEY")),
			Username: semanticEnv("USERNAME", ""),
			Password: semanticEnv("PASSWORD", ""),
		},
		TLS: SemanticTLS{
			Enabled:    semanticEnvBool("TLS_ENABLED", false),
			CAFile:     semanticEnv("TLS_CA_FILE", ""),
			ServerName: semanticEnv("TLS_SERVER_NAME", ""),
		},
	}
}

func semanticEnv(name, def string) string {
	return envFirst(def, "NASUTA_SEMANTIC_"+name, "SEMANTIC_"+name)
}

func semanticEnvBool(name string, def bool) bool {
	value := strings.ToLower(semanticEnv(name, ""))
	switch value {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func qdrantEndpoint(host string, port int) string {
	host = strings.TrimSpace(strings.TrimSuffix(host, "/"))
	if strings.Contains(host, "://") {
		return host
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func cwdOrDot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (c Config) FeishuConfigured() bool { return c.FeishuAppID != "" && c.FeishuAppSecret != "" }
