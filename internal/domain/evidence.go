package types

import "strings"

const (
	EvidenceClassCodeRuntime    = "code_runtime"
	EvidenceClassCuratedSchema  = "curated_schema"
	EvidenceClassCuratedRunbook = "curated_runbook"
	EvidenceClassGeneratedDoc   = "generated_doc"
	EvidenceClassUserDocument   = "user_document"
	EvidenceClassRawDDL         = "raw_ddl"
	EvidenceClassRepoDoc        = "repo_doc"
	EvidenceClassConfig         = "config"
	EvidenceClassServiceMeta    = "service_meta"
	EvidenceClassUnknown        = "unknown"
)

const (
	TrustCodeRuntime    = 100
	TrustCuratedSchema  = 90
	TrustCuratedRunbook = 85
	TrustGeneratedDoc   = 75
	TrustUserDocument   = 60
	TrustRawDDL         = 60
	TrustConfig         = 55
	TrustRepoDoc        = 35
	TrustServiceMeta    = 45
	TrustUnknown        = 30
)

func EvidenceForRunbookScope(scope string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case DocKindSchema:
		return EvidenceClassCuratedSchema, TrustCuratedSchema
	case DocKindFlow:
		return EvidenceClassCuratedRunbook, TrustCuratedRunbook
	case DocKindModule:
		return EvidenceClassGeneratedDoc, TrustGeneratedDoc
	case DocKindDocument:
		return EvidenceClassUserDocument, TrustUserDocument
	default:
		return EvidenceClassUnknown, TrustUnknown
	}
}

func EvidenceForCodeChunk(lang, repo string) (string, int) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	repo = strings.ToLower(strings.TrimSpace(repo))

	switch lang {
	case "sql":
		return EvidenceClassRawDDL, TrustRawDDL
	case "md", "mdx", "markdown":
		if repo == "docs" {
			return EvidenceClassCuratedRunbook, TrustCuratedRunbook
		}
		return EvidenceClassRepoDoc, TrustRepoDoc
	case "yaml", "yml", "json", "toml", "properties", "xml":
		return EvidenceClassConfig, TrustConfig
	case "java", "kt", "kotlin", "go", "py", "python", "js", "jsx", "ts", "tsx", "cs", "csharp", "swift", "dart", "rs", "rust", "c", "cpp", "cc", "h", "hpp":
		return EvidenceClassCodeRuntime, TrustCodeRuntime
	default:
		return EvidenceClassUnknown, TrustUnknown
	}
}

func TrustBand(trustTier int) int {
	switch {
	case trustTier >= 95: // code_runtime
		return 4
	case trustTier >= 70: // curated_schema / curated_runbook / generated_doc
		return 3
	case trustTier >= 50: // user_document / raw_ddl / config
		return 2
	case trustTier >= 40: // service_meta
		return 1
	default: // repo_doc / unknown
		return 0
	}
}

func TrustAdjustedScore(semanticScore float64, trustTier int) float64 {
	if semanticScore < 0 {
		semanticScore = 0
	}
	if semanticScore > 1 {
		semanticScore = 1
	}
	if trustTier < 0 {
		trustTier = 0
	}
	if trustTier > 100 {
		trustTier = 100
	}
	return semanticScore*0.8 + (float64(trustTier)/100.0)*0.2
}
