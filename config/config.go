package config

import platformconfig "github.com/dekwanlabs/nasuta/platform/config"

// Duration preserves the env-friendly duration contract at the public config boundary.
type Duration = platformconfig.Duration

// LogConfig controls the platform log sink.
type LogConfig = platformconfig.LogConfig

// Config holds environment-backed runtime configuration.
type Config = platformconfig.Config

type SemanticConfig = platformconfig.SemanticConfig

type SemanticAuth = platformconfig.SemanticAuth

type SemanticTLS = platformconfig.SemanticTLS

// PlatformSettings holds runtime settings managed by the platform UI.
type PlatformSettings = platformconfig.PlatformSettings

const (
	DefaultRetrievalRouterDirectConfidence = platformconfig.DefaultRetrievalRouterDirectConfidence
	DefaultRetrievalRouterMaxTokens        = platformconfig.DefaultRetrievalRouterMaxTokens
)

var (
	Load                      = platformconfig.Load
	LoadMySQLDSN              = platformconfig.LoadMySQLDSN
	ParseExcludeList          = platformconfig.ParseExcludeList
	IsPlatformSetting         = platformconfig.IsPlatformSetting
	MergeStoredPlatformValues = platformconfig.MergeStoredPlatformValues
	CanonicalPlatformSetting  = platformconfig.CanonicalPlatformSetting
)
