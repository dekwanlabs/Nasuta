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

// CodeSearchHit is one code chunk surfaced to scenario tools. It carries only
// the externally useful fields; internal scoring detail stays in domain types.
type CodeSearchHit struct {
	Path          string  `json:"path"`
	Lang          string  `json:"lang"`
	Repo          string  `json:"repo"`
	StartLine     int     `json:"startLine"`
	EndLine       int     `json:"endLine"`
	Preview       string  `json:"preview"`
	Score         float64 `json:"score"`
	ScoreKind     string  `json:"scoreKind"`
	EvidenceClass string  `json:"evidenceClass"`
	TrustTier     int     `json:"trustTier"`
}

// CodeSearchResult is the bounded code-search answer.
type CodeSearchResult struct {
	Matches  []CodeSearchHit
	Semantic bool
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
	Query string
	Limit int
}

// RunbookRecord is the runbook metadata exposed to scenario tools.
type RunbookRecord struct {
	ID    string   `json:"id"`
	Repo  string   `json:"repo,omitempty"`
	Title string   `json:"title"`
	Path  string   `json:"path"`
	Scope string   `json:"scope,omitempty"`
	Tags  []string `json:"tags"`
}

// RunbookSearchHit is one matched runbook chunk.
type RunbookSearchHit struct {
	Record        RunbookRecord `json:"record"`
	SectionHeader string        `json:"sectionHeader,omitempty"`
	ChunkText     string        `json:"chunkText,omitempty"`
	Score         float64       `json:"score"`
	EvidenceClass string        `json:"evidenceClass"`
	TrustTier     int           `json:"trustTier"`
}

// RunbookSearchResult is the bounded runbook-search answer.
type RunbookSearchResult struct {
	Matches  []RunbookSearchHit
	Semantic bool
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
	Matches  []ServiceRecord
	Semantic bool
}
