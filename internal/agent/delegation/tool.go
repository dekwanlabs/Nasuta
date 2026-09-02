package delegation

import (
	"context"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const DelegateToolID tool.ToolID = "delegate_investigation"

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
		Timeout:   tool.InheritCallerDeadline,
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
			result, evidence, err := executor.Execute(ctx, tasks)
			if err != nil {
				return tool.Result{}, err
			}
			result.Warnings = delegationWarnings(result)
			content, err := json.Marshal(result)
			if err != nil {
				return tool.Result{}, fmt.Errorf("encode delegation result: %w", err)
			}
			return tool.Result{
				Content: string(content), EvidenceUnits: evidence,
				Coverage: tool.EvidenceCoverage{
					Complete: !batchPartial(result),
					Partial:  batchPartial(result),
					Included: len(result.Results),
				},
				AnswerContract: delegationAdoptionContract(result),
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
	contract := tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: result.DelegationID,
			ReportIDs:    reportIDs,
		}},
	}
	if len(claims) > 0 {
		contract.Evidence = &tool.AnswerEvidenceContract{Claims: claims}
	}
	if merged, err := MergeFlowIRs(reportFlows(result.Results)); err == nil && merged != nil && len(merged.Edges) > 0 {
		contract.Evidence = ensureAnswerEvidenceContract(contract.Evidence)
		for _, edge := range merged.Edges {
			contract.Evidence.Edges = append(contract.Evidence.Edges, tool.AnswerEvidenceEdge{
				From: edge.From, To: edge.To, Protocol: edge.Protocol,
				SyncMode: edge.SyncMode, EvidenceState: edge.EvidenceState,
			})
		}
	}
	return contract
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
