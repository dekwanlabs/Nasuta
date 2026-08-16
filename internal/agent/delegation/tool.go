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
	for _, capability := range capabilities {
		enum = append(enum, capability.ID)
	}
	maxTasks := executor.policy.MaxChildren
	return tool.ReadTool{
		ID: DelegateToolID,
		Description: "Delegate one to three independent, bounded, read-only investigations. " +
			"Use one batch call and keep each objective narrow.",
		MCPHidden: true,
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
								"items":       map[string]any{"type": "string"},
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

func delegationAdoptionContract(
	result agentapi.DelegationBatchResult,
) tool.AnswerContract {
	if result.DelegationID == "" {
		return tool.AnswerContract{}
	}
	reportIDs := make([]string, 0, len(result.Results))
	seen := make(map[string]struct{}, len(result.Results))
	for _, report := range result.Results {
		if report.ReportID == "" ||
			report.Status == agentapi.DelegationRejected {
			continue
		}
		if _, exists := seen[report.ReportID]; exists {
			continue
		}
		seen[report.ReportID] = struct{}{}
		reportIDs = append(reportIDs, report.ReportID)
	}
	return tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: result.DelegationID,
			ReportIDs:    reportIDs,
		}},
	}
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

func batchPartial(result agentapi.DelegationBatchResult) bool {
	for _, report := range result.Results {
		if report.Status != agentapi.DelegationCompleted {
			return true
		}
	}
	return false
}
