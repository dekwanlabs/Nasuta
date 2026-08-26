package investigation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type DeterministicRenderer struct{}

func (DeterministicRenderer) Render(report InvestigationReport) string {
	claims := readableClaims(report.Claims)
	if len(claims) == 0 {
		if len(report.Evidence) > 0 {
			return "已完成资料检索，但现有证据还没有形成可验证的结论，暂时不能可靠回答这个问题。"
		}
		return "当前没有足够的可验证资料，暂时不能可靠回答这个问题。"
	}

	lines := make([]string, 0, len(claims)+3)
	if len(report.Gaps) > 0 {
		lines = append(lines, "以下是基于已验证证据得到的部分结论：")
	} else {
		lines = append(lines, "基于已验证证据：")
	}
	for _, claim := range claims {
		prefix := "- "
		if claim.Status != ClaimSupported {
			prefix = "- （有限结论）"
		}
		lines = append(lines, prefix+claim.Text)
	}
	if len(report.Gaps) > 0 {
		lines = append(lines, "部分问题仍缺少足够证据，以上内容不应视为完整结论。")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func readableClaims(claims []VerifiedClaim) []VerifiedClaim {
	readable := make([]VerifiedClaim, 0, len(claims))
	for _, claim := range claims {
		if claim.Status == ClaimRejected || !isUserReadableClaimText(claim.Text) {
			continue
		}
		readable = append(readable, claim)
	}
	return readable
}

type DeliveryGate struct {
	Renderer DeterministicRenderer
}

func (gate DeliveryGate) Deliver(
	ctx context.Context,
	contract InvestigationContract,
	report InvestigationReport,
	composer Composer,
) DeliveryResult {
	report.Claims = readableClaims(report.Claims)
	status := deliveryStatus(contract, report)
	var compositionFailure *RunFailure
	text := ""
	var compositionUsage BudgetVector
	if composer != nil && len(report.Claims) > 0 {
		draft, err := composer.Compose(ctx, contract, report)
		if err == nil {
			compositionUsage = draft.Usage
			if draftErr := validateAnswerDraft(draft, status, report); draftErr != nil {
				compositionFailure = &RunFailure{
					Code:      FailureComposer,
					Message:   draftErr.Error(),
					Stage:     string(StageComposition),
					Retryable: false,
				}
			} else {
				text = strings.TrimSpace(draft.Text)
			}
		} else {
			compositionUsage = draft.Usage
			compositionFailure = &RunFailure{
				Code:      FailureComposer,
				Message:   err.Error(),
				Stage:     string(StageComposition),
				Retryable: false,
			}
		}
	}
	if text == "" {
		text = gate.Renderer.Render(report)
	}
	result := DeliveryResult{
		Status:    status,
		Text:      text,
		Usage:     compositionUsage,
		Report:    report,
		Failure:   compositionFailure,
		CreatedAt: time.Now().UTC(),
	}
	if err := ValidateDelivery(result); err != nil {
		result.Status = DeliveryFailed
		result.Failure = &RunFailure{
			Code:      FailureEmptyOutput,
			Message:   err.Error(),
			Stage:     string(StageComposition),
			Retryable: false,
		}
		result.Text = "The investigation could not produce a user-readable result."
	}
	return result
}

func validateAnswerDraft(draft AnswerDraft, expected DeliveryStatus, report InvestigationReport) error {
	if strings.TrimSpace(draft.Text) == "" {
		return ErrEmptyDelivery
	}
	if containsOpaqueIdentifier(draft.Text) {
		return fmt.Errorf("composer returned an opaque internal identifier")
	}
	if draft.Status != "" && draft.Status != expected {
		return fmt.Errorf("composer status %q does not match report status %q", draft.Status, expected)
	}
	known := make(map[string]struct{}, len(report.Claims))
	for _, claim := range report.Claims {
		known[claim.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(draft.ClaimIDs))
	for _, id := range draft.ClaimIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("composer returned an empty claim id")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("composer returned duplicate claim %q", id)
		}
		seen[id] = struct{}{}
		if _, exists := known[id]; !exists {
			return fmt.Errorf("composer referenced unknown claim %q", id)
		}
	}
	return nil
}

func ValidateDelivery(result DeliveryResult) error {
	if strings.TrimSpace(result.Text) == "" {
		return ErrEmptyDelivery
	}
	switch result.Status {
	case DeliverySucceeded, DeliveryPartial, DeliveryEvidenceInsufficient, DeliveryFailed:
		return nil
	default:
		return fmt.Errorf("unknown delivery status %q", result.Status)
	}
}

func deliveryStatus(contract InvestigationContract, report InvestigationReport) DeliveryStatus {
	required := make(map[string]struct{})
	for _, goal := range contract.EvidenceGoals {
		if goal.Required {
			required[goal.ID] = struct{}{}
		}
	}
	coveredRequired := make(map[string]struct{}, len(required))
	partial := false
	for _, coverage := range report.Coverage {
		if coverage.Status == GoalCovered {
			if _, isRequired := required[coverage.GoalID]; isRequired {
				coveredRequired[coverage.GoalID] = struct{}{}
			}
		}
		if coverage.Status == GoalPartial {
			partial = true
		}
	}
	hasSupported := false
	hasPartial := false
	for _, claim := range report.Claims {
		if claim.Status == ClaimSupported {
			hasSupported = true
		}
		if claim.Status != ClaimSupported {
			hasPartial = true
		}
	}
	if hasSupported && !partial && len(coveredRequired) == len(required) {
		return DeliverySucceeded
	}
	if hasSupported || hasPartial || partial {
		return DeliveryPartial
	}
	return DeliveryEvidenceInsufficient
}
