package agent

// EvidenceSource identifies a planner-visible evidence boundary.
type EvidenceSource string

const (
	EvidenceSourceInternal EvidenceSource = "internal"
	EvidenceSourceMemory   EvidenceSource = "memory"
	EvidenceSourceWeb      EvidenceSource = "web"
	EvidenceSourceRuntime  EvidenceSource = "runtime"
)

// FreshnessPolicy declares how current evidence must be for one goal or capability.
type FreshnessPolicy string

const (
	FreshnessStable      FreshnessPolicy = "stable"
	FreshnessCurrent     FreshnessPolicy = "current"
	FreshnessBoundedLive FreshnessPolicy = "bounded_live"
)

// EvidenceRef points to evidence already admitted at the task boundary.
type EvidenceRef struct {
	SourceKind  string `json:"source_kind"`
	Target      string `json:"target"`
	Section     string `json:"section,omitempty"`
	Version     string `json:"version,omitempty"`
	TimeRange   string `json:"time_range,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// TaskBudget can only tighten the server-owned capability budget.
type TaskBudget struct {
	MaxInputTokens  int64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int64 `json:"max_total_tokens,omitempty"`
	MaxToolCalls    int64 `json:"max_tool_calls,omitempty"`
	MaxCostMicros   int64 `json:"max_cost_micros,omitempty"`
}

// TaskSpec requests one registered capability without selecting its implementation.
type TaskSpec struct {
	ID             string   `json:"id"`
	Purpose        string   `json:"purpose"`
	RequiredFacets []string `json:"required_facets,omitempty"`
	Capability     string   `json:"capability"`
	// InputRefs limits the task to evidence already admitted by the server.
	InputRefs     []EvidenceRef `json:"input_refs,omitempty"`
	OutputSchema  SchemaRef     `json:"output_schema"`
	ParallelGroup string        `json:"parallel_group,omitempty"`
	// Optional allows downstream scheduling to continue after terminal failure.
	Optional    bool       `json:"optional"`
	MaxAttempts int        `json:"max_attempts,omitempty"`
	Budget      TaskBudget `json:"budget"`
}

// TaskEdge declares that To may consume output from From.
type TaskEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Required blocks To when From cannot produce a valid handoff.
	Required bool `json:"required"`
}

// StopPolicy can only reduce server defaults; zero leaves the default unchanged.
type StopPolicy struct {
	MaxTasks          int     `json:"max_tasks,omitempty"`
	MaxParallelism    int     `json:"max_parallelism,omitempty"`
	MaxAttempts       int     `json:"max_attempts,omitempty"`
	MaxRounds         int     `json:"max_rounds,omitempty"`
	MaxDepth          int     `json:"max_depth,omitempty"`
	MaxDuplicateRatio float64 `json:"max_duplicate_ratio,omitempty"`
	MaxInputTokens    int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens   int64   `json:"max_output_tokens,omitempty"`
	MaxTotalTokens    int64   `json:"max_total_tokens,omitempty"`
	MaxToolCalls      int64   `json:"max_tool_calls,omitempty"`
	MaxCostMicros     int64   `json:"max_cost_micros,omitempty"`
	MaxRetries        int64   `json:"max_retries,omitempty"`
}

// TaskGraphProposal is planner output that requires server validation and compilation.
type TaskGraphProposal struct {
	Tasks []TaskSpec `json:"tasks"`
	Edges []TaskEdge `json:"edges,omitempty"`
	Stop  StopPolicy `json:"stop"`
}
