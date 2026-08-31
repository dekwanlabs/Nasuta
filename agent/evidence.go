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
