package knowledge

import "context"

// API is the stable read-only facade available to built-in and scenario tools.
// Implementations live in internal/ and convert their typed results into the
// self-contained structs below, so this contract never leaks internal types.
type API interface {
	SearchCode(context.Context, CodeSearchQuery) (CodeSearchResult, error)
	TraceDependencies(context.Context, DependencyQuery) (DependencyResult, error)
	SearchRunbooks(context.Context, RunbookQuery) (RunbookSearchResult, error)
	SearchServices(context.Context, ServiceSearchQuery) (ServiceSearchResult, error)
}

// CodeSearchQuery selects code chunks by free-text query and optional language.
type CodeSearchQuery struct {
	Query string
	Lang  string
	Limit int
}

// CodeSearchHit is one code chunk surfaced to scenario tools.
// ScoreKind keeps dense similarity distinct from rank-fusion scores.
type CodeSearchHit struct {
	Path          string   `json:"path"`
	Lang          string   `json:"lang"`
	Repo          string   `json:"repo"`
	Layer         string   `json:"layer"`
	StartLine     int      `json:"startLine"`
	EndLine       int      `json:"endLine"`
	Text          string   `json:"text"`
	Preview       string   `json:"preview"`
	Score         float64  `json:"score"`
	ScoreKind     string   `json:"scoreKind"`
	FusionScore   *float64 `json:"fusionScore,omitempty"`
	SemanticScore *float64 `json:"semanticScore,omitempty"`
	EvidenceClass string   `json:"evidenceClass"`
	TrustTier     int      `json:"trustTier"`
}

// CodeSearchResult is the bounded code-search answer.
type CodeSearchResult struct {
	Matches  []CodeSearchHit `json:"matches"`
	Semantic bool            `json:"semantic"`
}

// DependencyQuery traces one service's upstream or downstream edges.
type DependencyQuery struct {
	Service   string
	Direction string
	Depth     int
}

// DependencyEdge is one hop in a traced dependency chain.
type DependencyEdge struct {
	From           string  `json:"from"`
	To             string  `json:"to"`
	Type           string  `json:"type"`
	ExternalTarget string  `json:"externalTarget,omitempty"`
	Confidence     float64 `json:"confidence"`
}

// DependencyResult is the upstream/downstream answer for one trace.
type DependencyResult struct {
	Service    string           `json:"service,omitempty"`
	Candidates []string         `json:"candidates,omitempty"`
	Upstream   []DependencyEdge `json:"upstream"`
	Downstream []DependencyEdge `json:"downstream"`
	Truncated  bool             `json:"truncated"`
}

// RunbookQuery searches the runbook corpus by free-text query.
type RunbookQuery struct {
	Query string `json:"query"`
	DocID string `json:"docId,omitempty"`
	Limit int    `json:"limit"`
}

// RunbookChunk is one independently traceable document fragment.
type RunbookChunk struct {
	ChunkIndex    int     `json:"chunkIndex"`
	SectionHeader string  `json:"sectionHeader,omitempty"`
	ChunkText     string  `json:"chunkText"`
	SemanticScore float64 `json:"semanticScore"`
}

// RunbookSearchHit groups matched chunks under their source document.
type RunbookSearchHit struct {
	DocID         string         `json:"docId"`
	Title         string         `json:"title"`
	Path          string         `json:"path"`
	DocKind       string         `json:"docKind"`
	EvidenceClass string         `json:"evidenceClass"`
	TrustTier     int            `json:"trustTier"`
	Chunks        []RunbookChunk `json:"chunks"`
}

// RunbookSearchResult is the bounded runbook-search answer.
type RunbookSearchResult struct {
	Matches   []RunbookSearchHit `json:"matches"`
	Semantic  bool               `json:"semantic"`
	DocScoped bool               `json:"docScoped"`
	Truncated bool               `json:"truncated"`
}

// ServiceSearchQuery locates service modules by free-text query.
type ServiceSearchQuery struct {
	Query string
	Limit int
}

// ServiceRecord is the service card exposed to scenario tools. It carries only
// the externally useful identity and metadata, not internal evidence payloads.
type ServiceRecord struct {
	ServiceName string   `json:"serviceName"`
	Repo        string   `json:"repo,omitempty"`
	Layer       string   `json:"layer,omitempty"`
	Language    string   `json:"language,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Status      string   `json:"status,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Tags        []string `json:"tags"`
	Docs        []string `json:"docs"`
	Confidence  float64  `json:"confidence"`
}

// ServiceSearchResult is the bounded service-search answer.
type ServiceSearchResult struct {
	Matches  []ServiceRecord `json:"matches"`
	Semantic bool            `json:"semantic"`
}
