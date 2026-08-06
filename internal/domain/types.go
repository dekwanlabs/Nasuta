package domain

// SourceKind marks where a piece of evidence came from.
type SourceKind string

const (
	SourceDoc      SourceKind = "doc"
	SourceCodeScan SourceKind = "code-scan"
	SourceConfig   SourceKind = "config"
)

// Evidence points at a file (and optionally a line) backing a record.
type Evidence struct {
	Path   string     `json:"path"`
	Line   int        `json:"line,omitempty"`
	Symbol string     `json:"symbol,omitempty"`
	Kind   SourceKind `json:"kind"`
}

// ServiceRecord describes one backend service, merged from docs and code scan.
type ServiceRecord struct {
	ServiceKey    string     `json:"serviceKey,omitempty"`
	ServiceName   string     `json:"serviceName"`
	Repo          string     `json:"repo,omitempty"`
	Layer         string     `json:"layer,omitempty"`
	Scope         string     `json:"scope,omitempty"`
	ModulePath    string     `json:"modulePath,omitempty"`
	Language      string     `json:"language,omitempty"` // java | python | unknown
	Runtime       string     `json:"runtime,omitempty"`
	Owner         string     `json:"owner,omitempty"`
	Status        string     `json:"status,omitempty"`
	Tags          []string   `json:"tags"`
	Docs          []string   `json:"docs"`
	SourceOfTruth []string   `json:"sourceOfTruth"`
	Entrypoints   []Evidence `json:"entrypoints"`
	Ports         []int      `json:"ports"`
	Summary       string     `json:"summary,omitempty"`
	Confidence    float64    `json:"confidence"`
}

// EndpointRecord is a resolved HTTP route backed by source evidence.
type EndpointRecord struct {
	ServiceKey    string     `json:"serviceKey,omitempty"`
	ServiceName   string     `json:"serviceName"`
	Repo          string     `json:"repo,omitempty"`
	Method        string     `json:"method"`
	Path          string     `json:"path"`
	Handler       string     `json:"handler,omitempty"`       // controller class / router module
	HandlerMethod string     `json:"handlerMethod,omitempty"` // ★ codegraph anchor: method signature
	File          string     `json:"file"`
	Line          int        `json:"line,omitempty"`
	Source        SourceKind `json:"source"`
	Confidence    float64    `json:"confidence"`
}

// EdgeType classifies how one service depends on another. Different call
// mechanisms (Feign, RestTemplate/HTTP, gRPC, RPC) are distinct types so the
// dependency graph can distinguish and surface them.
type EdgeType string

const (
	EdgeFeign   EdgeType = "feign"
	EdgeHTTP    EdgeType = "http"    // RestTemplate/WebClient/raw HTTP URL
	EdgeGRPC    EdgeType = "grpc"    // gRPC client
	EdgeRPC     EdgeType = "rpc"     // Dubbo or other RPC
	EdgeKafka   EdgeType = "kafka"   // Kafka producer-to-consumer topic flow
	EdgeRunbook EdgeType = "runbook" // declared in runbook frontmatter
)

// DependencyTargetKind distinguishes workspace services from external systems.
type DependencyTargetKind string

const (
	DependencyTargetService  DependencyTargetKind = "service"
	DependencyTargetExternal DependencyTargetKind = "external"
)

// DependencyEdge is a directed service-to-service dependency.
type DependencyEdge struct {
	CallerServiceKey string               `json:"callerServiceKey,omitempty"`
	TargetKind       DependencyTargetKind `json:"targetKind,omitempty"`
	TargetServiceKey string               `json:"targetServiceKey,omitempty"`
	ExternalTarget   string               `json:"externalTarget,omitempty"`
	From             string               `json:"from"`
	To               string               `json:"to"`
	Type             EdgeType             `json:"type"`
	Evidence         []Evidence           `json:"evidence"`
	Confidence       float64              `json:"confidence"`
}

// DependencyTrace is one bounded upstream/downstream ontology answer.
type DependencyTrace struct {
	Service    string           `json:"service,omitempty"`
	Candidates []string         `json:"candidates,omitempty"`
	Upstream   []DependencyEdge `json:"upstream"`
	Downstream []DependencyEdge `json:"downstream"`
	Truncated  bool             `json:"truncated"`
}

// RepositoryRecord identifies the source revision represented by a snapshot.
type RepositoryRecord struct {
	Repo      string `json:"repo"`
	HeadSHA   string `json:"headSha"`
	IndexedAt int64  `json:"indexedAt"`
}

// RunbookRecord is a runbook document (persisted in the platform DocStore/MySQL,
// body embedded into the configured semantic store for recall).
type RunbookRecord struct {
	ID          string   `json:"id"`
	Repo        string   `json:"repo,omitempty"`
	Title       string   `json:"title"`
	Path        string   `json:"path"`
	Scope       string   `json:"scope,omitempty"`
	ServiceName string   `json:"serviceName,omitempty"`
	Tags        []string `json:"tags"`
	Text        string   `json:"text,omitempty"`
	Confidence  float64  `json:"confidence"`
}

// CodeChunk is a language-agnostic slice of a source/config/SQL/doc file,
// embedded into the vector store for semantic code search.
type CodeChunk struct {
	Path      string `json:"path"`
	Repo      string `json:"repo,omitempty"`
	Lang      string `json:"lang"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Text      string `json:"text"`
}

// DocRecord is a markdown document stored in the platform DocStore (MySQL).
type DocRecord struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Filename   string `json:"filename"`
	Kind       string `json:"kind"`
	Content    string `json:"content,omitempty"`
	ChunkCount int    `json:"chunkCount"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// IndexBundle is the full set of records produced by an indexing pass.
type IndexBundle struct {
	Repo         string
	Repositories []RepositoryRecord
	Services     []ServiceRecord
	Endpoints    []EndpointRecord
	Dependencies []DependencyEdge
	Runbooks     []RunbookRecord
}

// Page is a generic paginated response payload.
// Page/PageSize are optional for backward-compatible list endpoints.
type Page[T any] struct {
	Total    int `json:"total"`
	Page     int `json:"page,omitempty"`
	PageSize int `json:"page_size,omitempty"`
	List     []T `json:"list"`
}

// ApiResponse is the unified JSON envelope for every HTTP response.
type ApiResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
