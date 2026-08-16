package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	exactAnswerContractPrefix      = "[NASUTA_EXACT_ANSWER_CONTRACT] "
	delegationAdoptionMarkerPrefix = "[NASUTA_DELEGATION_ADOPTION] "
	maxAnswerContractRetries       = 2
)

var ErrAnswerContractViolation = errors.New("final answer violated an exact-output contract")

type exactAnswerContract struct {
	required        []string
	seen            map[string]struct{}
	delegationOrder []string
	allowedReports  map[string]map[string]struct{}
	reportOrder     map[string][]string
	evaluated       []agentapi.DelegationAdoption
}

type delegationAdoptionEnvelope struct {
	Delegations []delegationAdoptionSelection `json:"delegations"`
}

type delegationAdoptionSelection struct {
	DelegationID     string   `json:"delegation_id"`
	AdoptedReportIDs []string `json:"adopted_report_ids"`
}

func withoutContractMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, exactAnswerContractPrefix) {
			continue
		}
		out = append(out, message)
	}
	return out
}

func (contract *exactAnswerContract) Active() bool {
	return contract != nil &&
		(len(contract.required) > 0 || len(contract.delegationOrder) > 0)
}

func (contract *exactAnswerContract) Add(candidate tool.AnswerContract) {
	if contract == nil {
		return
	}
	if len(candidate.RequiredLiterals) > 0 && contract.seen == nil {
		contract.seen = make(map[string]struct{}, len(candidate.RequiredLiterals))
	}
	for _, literal := range candidate.RequiredLiterals {
		if literal == "" {
			continue
		}
		if _, exists := contract.seen[literal]; exists {
			continue
		}
		contract.seen[literal] = struct{}{}
		contract.required = append(contract.required, literal)
	}
	for _, delegation := range candidate.Delegations {
		delegationID := strings.TrimSpace(delegation.DelegationID)
		if delegationID == "" {
			continue
		}
		if contract.allowedReports == nil {
			contract.allowedReports = make(
				map[string]map[string]struct{},
				len(candidate.Delegations),
			)
			contract.reportOrder = make(
				map[string][]string,
				len(candidate.Delegations),
			)
		}
		allowed, exists := contract.allowedReports[delegationID]
		if !exists {
			allowed = make(map[string]struct{}, len(delegation.ReportIDs))
			contract.allowedReports[delegationID] = allowed
			contract.delegationOrder = append(
				contract.delegationOrder,
				delegationID,
			)
		}
		for _, reportID := range delegation.ReportIDs {
			reportID = strings.TrimSpace(reportID)
			if reportID == "" {
				continue
			}
			if _, exists := allowed[reportID]; exists {
				continue
			}
			allowed[reportID] = struct{}{}
			contract.reportOrder[delegationID] = append(
				contract.reportOrder[delegationID],
				reportID,
			)
		}
	}
}

func (contract *exactAnswerContract) Missing(answer string) []string {
	if !contract.Active() {
		return nil
	}
	missing := make([]string, 0)
	for _, literal := range contract.required {
		if !strings.Contains(answer, literal) {
			missing = append(missing, literal)
		}
	}
	return missing
}

func (contract *exactAnswerContract) snapshot() tool.AnswerContract {
	if contract == nil {
		return tool.AnswerContract{}
	}
	snapshot := tool.AnswerContract{
		RequiredLiterals: append([]string(nil), contract.required...),
	}
	for _, delegationID := range contract.delegationOrder {
		snapshot.Delegations = append(
			snapshot.Delegations,
			tool.DelegationAdoptionContract{
				DelegationID: delegationID,
				ReportIDs: append(
					[]string(nil),
					contract.reportOrder[delegationID]...,
				),
			},
		)
	}
	return snapshot
}

func (contract *exactAnswerContract) Adoptions() []agentapi.DelegationAdoption {
	if contract == nil || len(contract.evaluated) == 0 {
		return nil
	}
	return cloneDelegationAdoptions(contract.evaluated)
}

func (contract *exactAnswerContract) Satisfied(answer string) bool {
	if !contract.Active() {
		return true
	}
	if len(contract.Missing(answer)) > 0 {
		return false
	}
	return len(contract.delegationOrder) == 0 ||
		len(contract.evaluated) == len(contract.delegationOrder)
}

func (contract *exactAnswerContract) UnknownAdoptions(
	reason string,
) []agentapi.DelegationAdoption {
	if contract == nil || len(contract.delegationOrder) == 0 {
		return nil
	}
	adoptions := make(
		[]agentapi.DelegationAdoption,
		0,
		len(contract.delegationOrder),
	)
	for _, delegationID := range contract.delegationOrder {
		adoptions = append(adoptions, agentapi.DelegationAdoption{
			DelegationID: delegationID,
			Status:       agentapi.DelegationUnknown,
			Reason:       reason,
		})
	}
	return adoptions
}

func (contract *exactAnswerContract) ValidateAndStrip(
	answer string,
) (string, []string) {
	if !contract.Active() {
		return answer, nil
	}
	contract.evaluated = nil
	violations := make([]string, 0)
	for _, literal := range contract.Missing(answer) {
		violations = append(
			violations,
			fmt.Sprintf("missing required literal %q", literal),
		)
	}
	if len(contract.delegationOrder) == 0 {
		return answer, violations
	}

	markerCount := strings.Count(answer, delegationAdoptionMarkerPrefix)
	if markerCount == 0 {
		return answer, append(
			violations,
			"delegation adoption marker is missing",
		)
	}
	if markerCount != 1 {
		return answer, append(
			violations,
			"delegation adoption marker must appear exactly once",
		)
	}
	markerIndex := strings.Index(answer, delegationAdoptionMarkerPrefix)
	if markerIndex > 0 && answer[markerIndex-1] != '\n' {
		violations = append(
			violations,
			"delegation adoption marker must start on its own final line",
		)
	}
	payload := strings.TrimSpace(
		answer[markerIndex+len(delegationAdoptionMarkerPrefix):],
	)
	var envelope delegationAdoptionEnvelope
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return answer, append(
			violations,
			fmt.Sprintf("delegation adoption metadata is invalid: %v", err),
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return answer, append(
			violations,
			"delegation adoption marker must be the final answer content",
		)
	}

	seenDelegations := make(
		map[string]struct{},
		len(envelope.Delegations),
	)
	evaluated := make(
		[]agentapi.DelegationAdoption,
		0,
		len(contract.delegationOrder),
	)
	for _, selection := range envelope.Delegations {
		delegationID := strings.TrimSpace(selection.DelegationID)
		allowed, known := contract.allowedReports[delegationID]
		if !known {
			violations = append(
				violations,
				fmt.Sprintf("unknown delegation %q", delegationID),
			)
			continue
		}
		if _, duplicate := seenDelegations[delegationID]; duplicate {
			violations = append(
				violations,
				fmt.Sprintf("delegation %q appears more than once", delegationID),
			)
			continue
		}
		seenDelegations[delegationID] = struct{}{}
		adopted := make([]string, 0, len(selection.AdoptedReportIDs))
		seenReports := make(
			map[string]struct{},
			len(selection.AdoptedReportIDs),
		)
		for _, reportID := range selection.AdoptedReportIDs {
			reportID = strings.TrimSpace(reportID)
			if _, duplicate := seenReports[reportID]; duplicate {
				violations = append(
					violations,
					fmt.Sprintf(
						"delegation %q repeats report %q",
						delegationID,
						reportID,
					),
				)
				continue
			}
			seenReports[reportID] = struct{}{}
			if _, permitted := allowed[reportID]; !permitted {
				violations = append(
					violations,
					fmt.Sprintf(
						"delegation %q selected unauthorized report %q",
						delegationID,
						reportID,
					),
				)
				continue
			}
			adopted = append(adopted, reportID)
		}
		status := agentapi.DelegationNotAdopted
		if len(adopted) > 0 {
			status = agentapi.DelegationAdopted
		}
		evaluated = append(evaluated, agentapi.DelegationAdoption{
			DelegationID:     delegationID,
			AdoptedReportIDs: adopted,
			Status:           status,
		})
	}
	for _, delegationID := range contract.delegationOrder {
		if _, exists := seenDelegations[delegationID]; !exists {
			violations = append(
				violations,
				fmt.Sprintf("delegation %q is missing", delegationID),
			)
		}
	}
	visible := strings.TrimRight(answer[:markerIndex], " \t\r\n")
	if strings.TrimSpace(visible) == "" {
		violations = append(
			violations,
			"visible final answer is missing before delegation adoption metadata",
		)
	}
	if len(violations) > 0 {
		return answer, violations
	}
	contract.evaluated = evaluated
	return visible, nil
}

func contractMessage(candidate tool.AnswerContract) (llm.Message, bool) {
	contract := &exactAnswerContract{}
	contract.Add(candidate)
	if !contract.Active() {
		return llm.Message{}, false
	}
	encoded, err := json.Marshal(contract.snapshot())
	if err != nil {
		return llm.Message{}, false
	}
	return llm.Message{
		Role: "system",
		Content: prompts.MustRender(prompts.AgentQAExactAnswerContract, struct {
			Prefix         string
			Contract       string
			AdoptionMarker string
		}{
			Prefix: exactAnswerContractPrefix, Contract: string(encoded),
			AdoptionMarker: delegationAdoptionMarkerPrefix,
		}),
	}, true
}

func repairInstruction(violations []string) string {
	encoded, _ := json.Marshal(violations)
	return prompts.MustRender(prompts.AgentQAAnswerRepair, struct {
		Violations     string
		AdoptionMarker string
	}{
		Violations:     string(encoded),
		AdoptionMarker: delegationAdoptionMarkerPrefix,
	})
}

func contractError(violations []string) error {
	return fmt.Errorf(
		"%w: %d validation errors",
		ErrAnswerContractViolation,
		len(violations),
	)
}

func (agent *Agent) enforceContract(ctx context.Context, messages []llm.Message, initial *llm.ChatStreamResult, contract *exactAnswerContract, maxTokens int, stream *StreamPipe) (*llm.ChatStreamResult, error) {
	if !contract.Active() || initial == nil {
		return initial, nil
	}
	candidate := initial
	clean, violations := contract.ValidateAndStrip(candidate.Content)
	for attempt := 1; len(violations) > 0 && attempt <= maxAnswerContractRetries; attempt++ {
		log.WarnfCtx(ctx, "[agent] exact-answer validation rejected candidate: violations=%d retry=%d/%d", len(violations), attempt, maxAnswerContractRetries)
		repairMessages := append(append([]llm.Message{}, messages...),
			llm.Message{Role: "assistant", Content: candidate.Content},
			llm.Message{Role: "user", Content: repairInstruction(violations)},
		)
		repaired, err := agent.generateWithContinue(ctx, repairMessages, maxTokens, stream)
		if err != nil {
			return repaired, fmt.Errorf("%w: retry %d failed: %v", ErrAnswerContractViolation, attempt, err)
		}
		candidate = repaired
		clean, violations = contract.ValidateAndStrip(candidate.Content)
	}
	if len(violations) > 0 {
		log.ErrorfCtx(ctx, "[agent] exact-answer validation failed after %d retries: violations=%d", maxAnswerContractRetries, len(violations))
		return candidate, contractError(violations)
	}
	candidate.Content = clean
	return candidate, nil
}

func cloneDelegationAdoptions(
	adoptions []agentapi.DelegationAdoption,
) []agentapi.DelegationAdoption {
	if len(adoptions) == 0 {
		return nil
	}
	cloned := make([]agentapi.DelegationAdoption, len(adoptions))
	for index, adoption := range adoptions {
		adoption.AdoptedReportIDs = append(
			[]string(nil),
			adoption.AdoptedReportIDs...,
		)
		cloned[index] = adoption
	}
	return cloned
}
