package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const DelegateToolID tool.ToolID = "delegate_investigation"

// DelegationStatusToolID is the streaming backfill query. It is a fast,
// non-blocking read; the parent polls it between loop steps instead of waiting
// inside delegate_investigation.
const DelegationStatusToolID tool.ToolID = "delegation_status"

func (executor *Executor) Tool() tool.ReadTool {
	capabilities := executor.Capabilities()
	enum := make([]any, 0, len(capabilities))
	facetEnum := make([]any, 0)
	seenFacets := make(map[string]struct{})
	for _, capability := range capabilities {
		enum = append(enum, capability.ID)
		for _, facet := range capability.InputFacets {
			if _, exists := seenFacets[facet]; exists {
				continue
			}
			seenFacets[facet] = struct{}{}
			facetEnum = append(facetEnum, facet)
		}
	}
	maxTasks := executor.policy.MaxChildren
	return tool.ReadTool{
		ID: DelegateToolID,
		Description: fmt.Sprintf(
			"Required after any parent retrieval has isolated named subjects that still need a "+
				"deep investigation. Delegate one batch of at most %d independent, bounded, "+
				"read-only investigations. Put every isolated named subject that still needs a "+
				"deep-dive into this single batch; do not call this tool again for leftover "+
				"subjects. Keep each objective to one named subject and its primary missing "+
				"flow — not an exhaustive API inventory. Parent retrieval of any registered "+
				"tool is isolation-only: do not keep searching those subjects on the parent, "+
				"and do not write the deep-dive from parent results. Omit evidence_refs unless "+
				"they are ev_ handles from this run's manifests. Omit focus_facets unless they "+
				"are catalog IDs from the schema enum. Do not use this to inventory unnamed "+
				"businesses or to pre-split one question by capability.",
			maxTasks,
		),
		MCPHidden: true,
		Timeout:   15 * time.Second,
		InputSchema: tool.JSONSchema{
			"type":     "object",
			"required": []any{"tasks"},
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array", "minItems": 1, "maxItems": maxTasks,
					"items": map[string]any{
						"type":     "object",
						"required": []any{"capability", "objective"},
						"properties": map[string]any{
							"capability": map[string]any{
								"type": "string", "enum": enum,
							},
							"objective": map[string]any{
								"type": "string", "minLength": 1,
								"maxLength": maxObjectiveBytes,
							},
							"focus_facets": map[string]any{
								"type": "array", "maxItems": 10,
								"uniqueItems": true,
								"items":       facetItems(facetEnum),
							},
							"evidence_refs": map[string]any{
								"type": "array", "maxItems": maxEvidenceRefs,
								"uniqueItems": true,
								"items": map[string]any{
									"type": "string", "minLength": 1, "maxLength": 256,
								},
							},
						},
						"additionalProperties": false,
					},
				},
			},
			"additionalProperties": false,
		},
		Handler: tool.HandlerFunc(func(
			ctx context.Context,
			arguments tool.Arguments,
		) (tool.Result, error) {
			tasks, err := delegationTasks(arguments)
			if err != nil {
				return tool.Result{}, err
			}
			dispatch, err := executor.Dispatch(ctx, tasks)
			if err != nil {
				return tool.Result{}, err
			}
			content, err := json.Marshal(dispatch)
			if err != nil {
				return tool.Result{}, fmt.Errorf("encode delegation dispatch: %w", err)
			}
			anyRunning := dispatchHasRunning(dispatch)
			return tool.Result{
				Content: string(content),
				Coverage: tool.EvidenceCoverage{
					Complete: !anyRunning,
					Partial:  anyRunning,
					Included: len(dispatch.Tasks),
				},
				AnswerContract: delegationDispatchAdoptionContract(dispatch),
			}, nil
		}),
	}
}

const (
	verificationUnavailableWarning = "verification unavailable; do not state affected claims as verified; express them as inferred/unresolved and say what remains to confirm."
	verificationUnresolvedWarning  = "verification left one or more claims unresolved; preserve their unresolved status and say what remains to confirm."
)

func delegationWarnings(result agentapi.DelegationBatchResult) []string {
	if result.Validation.CitationCoverage <= 0 ||
		result.Validation.EvidenceBodyCoverage+1e-9 < result.Validation.CitationCoverage {
		return []string{verificationUnavailableWarning}
	}
	if !result.Validation.RequiresVerification {
		return nil
	}
	verification := result.Verification
	if verification == nil ||
		(verification.Status != agentapi.DelegationCompleted &&
			verification.Status != agentapi.DelegationPartial) ||
		len(verification.Verdicts) == 0 {
		return []string{verificationUnavailableWarning}
	}
	for _, verdict := range verification.Verdicts {
		if verdict.Decision == "unresolved" {
			return []string{verificationUnresolvedWarning}
		}
	}
	return nil
}

func delegationAdoptionContract(
	result agentapi.DelegationBatchResult,
) tool.AnswerContract {
	if result.DelegationID == "" {
		return tool.AnswerContract{}
	}
	reportIDs, claims := collectAdoptionContractItems(result)
	contract := tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: result.DelegationID,
			ReportIDs:    reportIDs,
		}},
	}
	if len(claims) > 0 {
		contract.Evidence = &tool.AnswerEvidenceContract{Claims: claims}
	}
	appendAdoptionContractEdges(&contract, result)
	return contract
}

func collectAdoptionContractItems(result agentapi.DelegationBatchResult) ([]string, []tool.AnswerEvidenceClaim) {
	reportIDs := make([]string, 0, len(result.Results))
	seenReports := make(map[string]struct{}, len(result.Results))
	claims := make([]tool.AnswerEvidenceClaim, 0)
	seenClaims := make(map[string]struct{})
	for _, report := range result.Results {
		if report.ReportID == "" ||
			report.Status == agentapi.DelegationRejected {
			continue
		}
		if _, exists := seenReports[report.ReportID]; !exists {
			seenReports[report.ReportID] = struct{}{}
			reportIDs = append(reportIDs, report.ReportID)
		}
		for _, finding := range report.Findings {
			claimID := report.ReportID + "/" + finding.ID
			if finding.ID == "" || finding.Statement == "" {
				continue
			}
			if _, exists := seenClaims[claimID]; exists {
				continue
			}
			seenClaims[claimID] = struct{}{}
			decision := delegationClaimDecision(result, claimID)
			// A supported natural-language claim must have an explicit citation
			// on the server-owned finding. The final-answer manifest can only
			// authorize a claim; it must not turn an uncited model statement into
			// evidence-backed fact.
			if len(canonicalStrings(finding.Citations)) == 0 {
				decision = "unresolved"
			}
			claims = append(claims, tool.AnswerEvidenceClaim{
				ClaimID: claimID, Decision: decision,
			})
		}
	}
	return reportIDs, claims
}

func appendAdoptionContractEdges(contract *tool.AnswerContract, result agentapi.DelegationBatchResult) {
	if merged, err := MergeFlowIRs(reportFlows(result.Results)); err == nil && merged != nil && len(merged.Edges) > 0 {
		contract.Evidence = ensureAnswerEvidenceContract(contract.Evidence)
		for _, edge := range merged.Edges {
			contract.Evidence.Edges = append(contract.Evidence.Edges, tool.AnswerEvidenceEdge{
				From: edge.From, To: edge.To, Protocol: edge.Protocol,
				SyncMode: edge.SyncMode, EvidenceState: edge.EvidenceState,
			})
		}
	}
}

func delegationClaimDecision(
	result agentapi.DelegationBatchResult,
	claimID string,
) string {
	if !result.Validation.RequiresVerification {
		return "supported"
	}
	if result.Verification == nil ||
		(result.Verification.Status != agentapi.DelegationCompleted &&
			result.Verification.Status != agentapi.DelegationPartial) {
		return "unresolved"
	}
	decision := "unresolved"
	for _, verdict := range result.Verification.Verdicts {
		for _, id := range verdict.ClaimIDs {
			if id != claimID {
				continue
			}
			decision = normalizeDelegationClaimDecision(verdict.Decision)
			if decision == "" {
				return "unresolved"
			}
		}
	}
	return decision
}

func normalizeDelegationClaimDecision(value string) string {
	switch value {
	case "supported", "contradicted", "distinct", "unresolved":
		return value
	default:
		return ""
	}
}

func reportFlows(reports []agentapi.DelegationReport) []agentapi.FlowIR {
	flows := make([]agentapi.FlowIR, 0, len(reports))
	for _, report := range reports {
		if report.Status == agentapi.DelegationRejected || report.Flow == nil {
			continue
		}
		flows = append(flows, *report.Flow)
	}
	return flows
}

func ensureAnswerEvidenceContract(
	contract *tool.AnswerEvidenceContract,
) *tool.AnswerEvidenceContract {
	if contract != nil {
		return contract
	}
	return &tool.AnswerEvidenceContract{}
}

func delegationTasks(arguments tool.Arguments) ([]agentapi.DelegationTask, error) {
	raw, ok := arguments["tasks"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("delegate_investigation tasks are required")
	}
	tasks := make([]agentapi.DelegationTask, 0, len(raw))
	for index, value := range raw {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("delegation task %d must be an object", index)
		}
		args := tool.Arguments(object)
		tasks = append(tasks, agentapi.DelegationTask{
			Capability:   args.String("capability"),
			Objective:    args.String("objective"),
			FocusFacets:  args.Strings("focus_facets"),
			EvidenceRefs: args.Strings("evidence_refs"),
		})
	}
	return tasks, nil
}

func facetItems(enum []any) map[string]any {
	items := map[string]any{"type": "string"}
	if len(enum) > 0 {
		items["enum"] = enum
	}
	return items
}

func batchPartial(result agentapi.DelegationBatchResult) bool {
	for _, report := range result.Results {
		if report.Status != agentapi.DelegationCompleted {
			return true
		}
	}
	return false
}

// StatusTool exposes a non-blocking poll of one dispatched delegation. It lets
// the parent read whichever children have already settled and backfill its
// draft answer between loop steps instead of blocking inside
// delegate_investigation.
func (executor *Executor) StatusTool() tool.ReadTool {
	return tool.ReadTool{
		ID:          DelegationStatusToolID,
		Description: "Poll the current state of a previously dispatched investigation batch. Returns the tasks that have already completed with their reports, and marks the rest as running. Call this after delegate_investigation to collect finished results; it never blocks.",
		MCPHidden:   true,
		NoDedup:     true,
		Timeout:     5 * time.Second,
		InputSchema: tool.JSONSchema{
			"type":     "object",
			"required": []any{"delegation_id"},
			"properties": map[string]any{
				"delegation_id": map[string]any{
					"type": "string", "minLength": 1, "maxLength": 128,
				},
			},
			"additionalProperties": false,
		},
		Handler: tool.HandlerFunc(func(
			ctx context.Context,
			arguments tool.Arguments,
		) (tool.Result, error) {
			delegationID := arguments.String("delegation_id")
			if delegationID == "" {
				return tool.Result{}, fmt.Errorf("delegation_id is required")
			}
			result, err := executor.Poll(ctx, delegationID)
			if err != nil {
				return tool.Result{}, err
			}
			content, err := json.Marshal(result)
			if err != nil {
				return tool.Result{}, fmt.Errorf("encode delegation status: %w", err)
			}
			anyRunning := dispatchHasRunning(result)
			return tool.Result{
				Content: string(content),
				Coverage: tool.EvidenceCoverage{
					Complete: !anyRunning,
					Partial:  anyRunning,
					Included: len(result.Tasks),
				},
				AnswerContract: delegationDispatchAdoptionContract(result),
			}, nil
		}),
	}
}

// dispatchHasRunning reports whether any admitted task has not yet settled.
func dispatchHasRunning(dispatch agentapi.DelegationDispatchResult) bool {
	for _, task := range dispatch.Tasks {
		switch task.Status {
		case agentapi.DelegationRunning:
			return true
		}
	}
	return false
}

// DelegationAdoptionContract adapts a streaming dispatch projection to the
// batch-shaped adoption contract used by the exact-answer contract. It is
// deliberately bounded to the reports already visible so adoption metadata is
// never emitted as "unknown". The execution loop calls it after an awaited
// settlement to re-register the backfilled reports as adoptable.
func DelegationAdoptionContract(result agentapi.DelegationDispatchResult) tool.AnswerContract {
	return delegationDispatchAdoptionContract(result)
}

// delegationDispatchAdoptionContract adapts a streaming dispatch projection to
// the batch-shaped adoption contract used by the exact-answer contract. It is
// deliberately bounded to the reports already visible so adoption metadata is
// never emitted as "unknown".
func delegationDispatchAdoptionContract(
	result agentapi.DelegationDispatchResult,
) tool.AnswerContract {
	batch := agentapi.DelegationBatchResult{
		DelegationID: result.DelegationID,
		Results:      make([]agentapi.DelegationReport, 0, len(result.Tasks)),
	}
	for _, task := range result.Tasks {
		if task.Report != nil {
			batch.Results = append(batch.Results, *task.Report)
		}
	}
	return delegationAdoptionContract(batch)
}
