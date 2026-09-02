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
	required         []string
	seen             map[string]struct{}
	delegationOrder  []string
	allowedReports   map[string]map[string]struct{}
	reportOrder      map[string][]string
	evidenceRequired bool
	allowedClaims    map[string]string
	claimOrder       []string
	allowedEdges     map[string]string
	edgeOrder        []string
	evaluated        []agentapi.DelegationAdoption
}

type delegationAdoptionEnvelope struct {
	Delegations []delegationAdoptionSelection   `json:"delegations"`
	Claims      *[]answerEvidenceClaimSelection `json:"claims,omitempty"`
	Edges       *[]answerEvidenceEdgeSelection  `json:"edges,omitempty"`
}

type delegationAdoptionSelection struct {
	DelegationID     string   `json:"delegation_id"`
	AdoptedReportIDs []string `json:"adopted_report_ids"`
}

type answerEvidenceClaimSelection struct {
	ClaimID  string `json:"claim_id"`
	Decision string `json:"decision"`
}

type answerEvidenceEdgeSelection struct {
	From          string `json:"from"`
	To            string `json:"to"`
	Protocol      string `json:"protocol,omitempty"`
	SyncMode      string `json:"sync_mode,omitempty"`
	EvidenceState string `json:"evidence_state"`
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
		(len(contract.required) > 0 || len(contract.delegationOrder) > 0 || contract.evidenceRequired)
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
	if candidate.Evidence != nil {
		if len(candidate.Evidence.Claims) > 0 || len(candidate.Evidence.Edges) > 0 {
			contract.evidenceRequired = true
		}
		if contract.allowedClaims == nil {
			contract.allowedClaims = make(map[string]string, len(candidate.Evidence.Claims))
		}
		for _, claim := range candidate.Evidence.Claims {
			id := strings.TrimSpace(claim.ClaimID)
			decision := normalizeAnswerEvidenceDecision(claim.Decision)
			if id == "" || decision == "" {
				continue
			}
			if previous, exists := contract.allowedClaims[id]; exists {
				contract.allowedClaims[id] = mergeAnswerEvidenceDecision(previous, decision)
				continue
			}
			contract.allowedClaims[id] = decision
			contract.claimOrder = append(contract.claimOrder, id)
		}
		if contract.allowedEdges == nil {
			contract.allowedEdges = make(map[string]string, len(candidate.Evidence.Edges))
		}
		for _, edge := range candidate.Evidence.Edges {
			key := answerEvidenceEdgeKey(edge.From, edge.To, edge.Protocol, edge.SyncMode)
			state := normalizeAnswerEvidenceDecision(edge.EvidenceState)
			if key == "" || state == "" {
				continue
			}
			if previous, exists := contract.allowedEdges[key]; exists {
				contract.allowedEdges[key] = mergeAnswerEvidenceDecision(previous, state)
				continue
			}
			contract.allowedEdges[key] = state
			contract.edgeOrder = append(contract.edgeOrder, key)
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
	if contract.evidenceRequired {
		snapshot.Evidence = &tool.AnswerEvidenceContract{}
		for _, claimID := range contract.claimOrder {
			snapshot.Evidence.Claims = append(snapshot.Evidence.Claims, tool.AnswerEvidenceClaim{
				ClaimID: claimID, Decision: contract.allowedClaims[claimID],
			})
		}
		for _, edgeKey := range contract.edgeOrder {
			from, to, protocol, syncMode := splitAnswerEvidenceEdgeKey(edgeKey)
			snapshot.Evidence.Edges = append(snapshot.Evidence.Edges, tool.AnswerEvidenceEdge{
				From: from, To: to, Protocol: protocol, SyncMode: syncMode,
				EvidenceState: contract.allowedEdges[edgeKey],
			})
		}
	}
	return snapshot
}

func (contract *exactAnswerContract) restoreEvaluated(adoptions []agentapi.DelegationAdoption) {
	if contract == nil {
		return
	}
	contract.evaluated = cloneDelegationAdoptions(adoptions)
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

func (contract *exactAnswerContract) appendConservativeFallbackMetadata(answer string) (string, error) {
	if contract == nil || !contract.Active() {
		return answer, nil
	}
	answer = strings.TrimRight(answer, " \t\r\n")
	if answer == "" {
		return "", fmt.Errorf("fallback answer is empty")
	}
	var builder strings.Builder
	builder.WriteString(answer)
	if len(contract.required) > 0 {
		builder.WriteString("\n\n补充：最终模型调用未完成，以下契约要求内容来自已收集证据；其关系仍需继续核对：")
		for _, literal := range contract.required {
			builder.WriteString("\n- ")
			builder.WriteString(literal)
		}
	}
	if len(contract.delegationOrder) == 0 && !contract.evidenceRequired {
		return builder.String(), nil
	}
	builder.WriteString("\n")

	envelope := delegationAdoptionEnvelope{}
	if len(contract.delegationOrder) > 0 {
		envelope.Delegations = make([]delegationAdoptionSelection, 0, len(contract.delegationOrder))
		for _, delegationID := range contract.delegationOrder {
			envelope.Delegations = append(envelope.Delegations, delegationAdoptionSelection{
				DelegationID:     delegationID,
				AdoptedReportIDs: []string{},
			})
		}
	}
	if contract.evidenceRequired {
		if contractHasClaims(contract) {
			claims := make([]answerEvidenceClaimSelection, 0, len(contract.claimOrder))
			for _, claimID := range contract.claimOrder {
				claims = append(claims, answerEvidenceClaimSelection{
					ClaimID: claimID, Decision: contract.allowedClaims[claimID],
				})
			}
			envelope.Claims = &claims
		}
		if contractHasEdges(contract) {
			edges := make([]answerEvidenceEdgeSelection, 0, len(contract.edgeOrder))
			for _, edgeKey := range contract.edgeOrder {
				from, to, protocol, syncMode := splitAnswerEvidenceEdgeKey(edgeKey)
				edges = append(edges, answerEvidenceEdgeSelection{
					From: from, To: to, Protocol: protocol, SyncMode: syncMode,
					EvidenceState: contract.allowedEdges[edgeKey],
				})
			}
			envelope.Edges = &edges
		}
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode fallback answer contract: %w", err)
	}
	builder.WriteString(delegationAdoptionMarkerPrefix)
	builder.Write(encoded)
	return builder.String(), nil
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
	if len(contract.delegationOrder) == 0 && !contract.evidenceRequired {
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
	if contract.evidenceRequired {
		if contractHasClaims(contract) && envelope.Claims == nil {
			violations = append(violations, "claims evidence manifest is missing")
		}
		if contractHasEdges(contract) && envelope.Edges == nil {
			violations = append(violations, "edges evidence manifest is missing")
		}
		seenClaims := make(map[string]struct{})
		if envelope.Claims != nil {
			seenClaims = make(map[string]struct{}, len(*envelope.Claims))
			for _, selection := range *envelope.Claims {
				claimID := strings.TrimSpace(selection.ClaimID)
				if claimID == "" {
					violations = append(violations, "claim_id must not be empty")
					continue
				}
				if _, duplicate := seenClaims[claimID]; duplicate {
					violations = append(violations, fmt.Sprintf("claim %q appears more than once", claimID))
					continue
				}
				seenClaims[claimID] = struct{}{}
				expected, known := contract.allowedClaims[claimID]
				if !known {
					violations = append(violations, fmt.Sprintf("unknown claim %q", claimID))
					continue
				}
				decision := normalizeAnswerEvidenceDecision(selection.Decision)
				if decision == "" || !isAnswerEvidenceClaimDecision(decision) {
					violations = append(violations, fmt.Sprintf("claim %q has invalid decision %q", claimID, selection.Decision))
					continue
				}
				if decision != expected {
					violations = append(violations, fmt.Sprintf("claim %q decision %q does not match server state %q", claimID, decision, expected))
				}
			}
		}
		seenEdges := make(map[string]struct{})
		if envelope.Edges != nil {
			seenEdges = make(map[string]struct{}, len(*envelope.Edges))
			for _, selection := range *envelope.Edges {
				key := answerEvidenceEdgeKey(selection.From, selection.To, selection.Protocol, selection.SyncMode)
				if key == "" {
					violations = append(violations, "edge from and to must not be empty")
					continue
				}
				if _, duplicate := seenEdges[key]; duplicate {
					violations = append(violations, fmt.Sprintf("edge %q appears more than once", key))
					continue
				}
				seenEdges[key] = struct{}{}
				expected, known := contract.allowedEdges[key]
				if !known {
					violations = append(violations, fmt.Sprintf("unknown edge %q", key))
					continue
				}
				state := normalizeAnswerEvidenceDecision(selection.EvidenceState)
				if state == "" || !isAnswerEvidenceEdgeState(state) {
					violations = append(violations, fmt.Sprintf("edge %q has invalid evidence_state %q", key, selection.EvidenceState))
					continue
				}
				if state != expected {
					violations = append(violations, fmt.Sprintf("edge %q evidence_state %q does not match server state %q", key, state, expected))
				}
			}
		}
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

func contractHasClaims(contract *exactAnswerContract) bool {
	return contract != nil && len(contract.allowedClaims) > 0
}

func contractHasEdges(contract *exactAnswerContract) bool {
	return contract != nil && len(contract.allowedEdges) > 0
}

func normalizeAnswerEvidenceDecision(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "supported", "contradicted", "distinct", "unresolved", "verified", "inferred":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func isAnswerEvidenceClaimDecision(value string) bool {
	switch value {
	case "supported", "contradicted", "distinct", "unresolved":
		return true
	default:
		return false
	}
}

func isAnswerEvidenceEdgeState(value string) bool {
	switch value {
	case "verified", "inferred", "unresolved":
		return true
	default:
		return false
	}
}

func mergeAnswerEvidenceDecision(left, right string) string {
	left = normalizeAnswerEvidenceDecision(left)
	right = normalizeAnswerEvidenceDecision(right)
	if left == "" {
		return right
	}
	if right == "" || left == right {
		return left
	}
	// A disagreement must never widen what the parent may claim. For both
	// claim decisions and flow states, unresolved is the conservative join.
	return "unresolved"
}

func answerEvidenceEdgeKey(from, to, protocol, syncMode string) string {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return ""
	}
	return strings.Join([]string{
		from,
		to,
		strings.TrimSpace(protocol),
		strings.TrimSpace(syncMode),
	}, "\x00")
}

func splitAnswerEvidenceEdgeKey(key string) (from, to, protocol, syncMode string) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 4 {
		return "", "", "", ""
	}
	return parts[0], parts[1], parts[2], parts[3]
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
