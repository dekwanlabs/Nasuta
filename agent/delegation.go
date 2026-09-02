package agent

import (
	"context"
	"encoding/json"
	"time"
)

// DelegationPolicy is a server-owned ceiling for dynamic child Runs.
type DelegationPolicy struct {
	MaxDepth             int           `json:"max_depth"`
	MaxChildren          int           `json:"max_children"`
	MaxConcurrent        int           `json:"max_concurrent"`
	MaxChildTurns        int           `json:"max_child_turns"`
	MaxChildToolCalls    int64         `json:"max_child_tool_calls"`
	MaxChildInputTokens  int64         `json:"max_child_input_tokens"`
	MaxChildOutputTokens int64         `json:"max_child_output_tokens"`
	MaxReportTokens      int64         `json:"max_report_tokens"`
	MaxTotalTokens       int64         `json:"max_total_tokens"`
	MaxTotalCostMicros   int64         `json:"max_total_cost_micros"`
	ParentAnswerReserve  int64         `json:"parent_answer_reserve"`
	ChildTimeout         time.Duration `json:"child_timeout"`
}

// DelegationTask is the complete model-visible child request. Definition,
// tools, permissions, provider, credentials, and limits are server-owned.
type DelegationTask struct {
	Capability   string   `json:"capability"`
	Objective    string   `json:"objective"`
	FocusFacets  []string `json:"focus_facets,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// DelegationStatus is projected from admission facts and the child Run outcome.
type DelegationStatus string

const (
	DelegationCompleted   DelegationStatus = "completed"
	DelegationPartial     DelegationStatus = "partial"
	DelegationFailed      DelegationStatus = "failed"
	DelegationTimeout     DelegationStatus = "timeout"
	DelegationCancelled   DelegationStatus = "cancelled"
	DelegationRejected    DelegationStatus = "rejected"
	DelegationInterrupted DelegationStatus = "interrupted"
)

type DelegationCompleteness string

const (
	DelegationComplete   DelegationCompleteness = "complete"
	DelegationIncomplete DelegationCompleteness = "partial"
)

type DelegationConfidence string

const (
	DelegationConfidenceLow    DelegationConfidence = "low"
	DelegationConfidenceMedium DelegationConfidence = "medium"
	DelegationConfidenceHigh   DelegationConfidence = "high"
)

// StructuredClaim is comparable only when a registered ClaimPolicy covers its schema.
type StructuredClaim struct {
	Schema    string         `json:"schema"`
	Subject   string         `json:"subject"`
	Predicate string         `json:"predicate"`
	Value     any            `json:"value"`
	Scope     map[string]any `json:"scope,omitempty"`
}

type DelegationFinding struct {
	ID              string               `json:"id"`
	Statement       string               `json:"statement"`
	StructuredClaim *StructuredClaim     `json:"structured_claim,omitempty"`
	Confidence      DelegationConfidence `json:"confidence"`
	Citations       []string             `json:"citations"`
	Facets          []string             `json:"facets,omitempty"`
	Critical        bool                 `json:"critical,omitempty"`
}

// FlowNode is one bounded responsibility or system node in a delegated flow.
type FlowNode struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Kind         string   `json:"kind"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// FlowEdge is one bounded hop between two FlowNode IDs. EvidenceState keeps
// the parent from presenting an inferred or unresolved hop as verified.
type FlowEdge struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	Protocol      string   `json:"protocol,omitempty"`
	SyncMode      string   `json:"sync_mode,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	EvidenceState string   `json:"evidence_state"`
}

// FlowIR is the compact, machine-readable flow handoff used when a child
// investigates a process or architecture question. It is optional so older
// investigator reports remain valid.
type FlowIR struct {
	Subject       string     `json:"subject"`
	Status        string     `json:"status"`
	Nodes         []FlowNode `json:"nodes,omitempty"`
	Edges         []FlowEdge `json:"edges,omitempty"`
	OpenHops      []string   `json:"open_hops,omitempty"`
	Uncertainties []string   `json:"uncertainties,omitempty"`
	Confidence    string     `json:"confidence"`
}

type DelegationConflict struct {
	ID       string   `json:"id,omitempty"`
	Kind     string   `json:"kind"`
	ClaimIDs []string `json:"claim_ids"`
	Critical bool     `json:"critical,omitempty"`
}

type DelegationUsage struct {
	ToolCalls       int64 `json:"tool_calls"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens     int64 `json:"total_tokens"`
	CostMicros      int64 `json:"cost_micros,omitempty"`
}

// DelegationReport is the bounded projection returned to the parent.
type DelegationReport struct {
	RunID         string                 `json:"run_id,omitempty"`
	ReportID      string                 `json:"report_id,omitempty"`
	Capability    string                 `json:"capability"`
	Status        DelegationStatus       `json:"status"`
	Completeness  DelegationCompleteness `json:"completeness"`
	Summary       string                 `json:"summary,omitempty"`
	Findings      []DelegationFinding    `json:"findings,omitempty"`
	Flow          *FlowIR                `json:"flow,omitempty"`
	Conflicts     []DelegationConflict   `json:"conflicts,omitempty"`
	Uncertainties []string               `json:"uncertainties,omitempty"`
	Usage         DelegationUsage        `json:"usage"`
	Error         *RunError              `json:"error,omitempty"`
}

type DelegationValidationConflict struct {
	Kind     string   `json:"kind"`
	ClaimKey string   `json:"claim_key,omitempty"`
	ClaimIDs []string `json:"claim_ids"`
	Critical bool     `json:"critical,omitempty"`
}

// DelegationValidation contains only deterministic contract and comparator facts.
type DelegationValidation struct {
	ReportIDs                 []string                       `json:"report_ids,omitempty"`
	CitationCoverage          float64                        `json:"citation_coverage"`
	EvidenceBodyCoverage      float64                        `json:"evidence_body_coverage"`
	StructuredClaimCoverage   float64                        `json:"structured_claim_coverage"`
	Conflicts                 []DelegationValidationConflict `json:"conflicts,omitempty"`
	HasConflicts              bool                           `json:"has_conflicts"`
	UnverifiedSemanticOverlap bool                           `json:"unverified_semantic_overlap"`
	RequiresVerification      bool                           `json:"requires_verification"`
	VerificationReasons       []string                       `json:"verification_reasons,omitempty"`
}

type DelegationVerificationClaim struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Critical  bool     `json:"critical,omitempty"`
	Citations []string `json:"citations"`
}

// DelegationEvidenceLookup gives a semantic verifier bounded authoritative
// context for an evidence reference. Body is present only when the server has
// the corresponding admitted content; it is never synthesized from metadata.
type DelegationEvidenceLookup struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
	Body      string `json:"body,omitempty"`
}

// DelegationVerificationRequest is the bounded semantic-verifier input. It
// deliberately excludes complete reports, child traces, and tool transcripts.
type DelegationVerificationRequest struct {
	Question         string                              `json:"question"`
	DecisionQuestion string                              `json:"decision_question"`
	Claims           []DelegationVerificationClaim       `json:"claims"`
	Conflicts        []DelegationValidationConflict      `json:"conflicts"`
	EvidenceRefs     []string                            `json:"evidence_refs"`
	EvidenceLookup   map[string]DelegationEvidenceLookup `json:"evidence_lookup,omitempty"`
	Reasons          []string                            `json:"reasons"`
}

type DelegationVerificationVerdict struct {
	ClaimIDs     []string `json:"claim_ids"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// DelegationVerification is the durable typed outcome of the optional semantic
// verifier child.
type DelegationVerification struct {
	RunID          string                          `json:"run_id,omitempty"`
	VerificationID string                          `json:"verification_id,omitempty"`
	Status         DelegationStatus                `json:"status"`
	Summary        string                          `json:"summary,omitempty"`
	Verdicts       []DelegationVerificationVerdict `json:"verdicts,omitempty"`
	Uncertainties  []string                        `json:"uncertainties,omitempty"`
	Usage          DelegationUsage                 `json:"usage"`
	Error          *RunError                       `json:"error,omitempty"`
}

type DelegationBatchResult struct {
	DelegationID string                  `json:"delegation_id"`
	Results      []DelegationReport      `json:"results"`
	Validation   DelegationValidation    `json:"validation"`
	Verification *DelegationVerification `json:"verification,omitempty"`
	Warnings     []string                `json:"warnings,omitempty"`
}

// DelegationAdoptionStatus records whether a parent final answer used any
// report from one completed delegation.
type DelegationAdoptionStatus string

const (
	DelegationAdopted    DelegationAdoptionStatus = "adopted"
	DelegationNotAdopted DelegationAdoptionStatus = "not_adopted"
	DelegationUnknown    DelegationAdoptionStatus = "unknown"
)

// DelegationAdoption is explicit final-answer metadata. Verifier output is not
// a report and cannot appear in AdoptedReportIDs.
type DelegationAdoption struct {
	DelegationID     string                   `json:"delegation_id"`
	AdoptedReportIDs []string                 `json:"adopted_report_ids,omitempty"`
	Status           DelegationAdoptionStatus `json:"status"`
	Reason           string                   `json:"reason,omitempty"`
}

// ClaimPolicy selects a deterministic comparator for one structured claim schema.
type ClaimPolicy struct {
	Schema       SchemaRef `json:"schema"`
	ComparatorID string    `json:"comparator_id"`
	KeyFields    []string  `json:"key_fields"`
	ScopeFields  []string  `json:"scope_fields"`
}

// ClaimComparator compares canonical structured values. It must not perform
// semantic inference over free text.
type ClaimComparator interface {
	Conflicts(context.Context, json.RawMessage, json.RawMessage) (bool, error)
}
