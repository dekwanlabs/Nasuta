package investigation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type DeterministicRenderer struct{}

func (DeterministicRenderer) Render(report InvestigationReport) string {
	var lines []string
	if len(report.Claims) > 0 {
		lines = append(lines, "Verified findings:")
		for _, claim := range report.Claims {
			prefix := "- "
			if claim.Status != ClaimSupported {
				prefix = "- [limited] "
			}
			lines = append(lines, prefix+claim.Text)
		}
	}
	if len(report.Gaps) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "Investigation limits:")
		for _, gap := range report.Gaps {
			reason := strings.TrimSpace(gap.Reason)
			if reason == "" {
				reason = "the goal has no verified coverage"
			}
			lines = append(lines, fmt.Sprintf("- %s: %s", gap.GoalID, reason))
		}
	}
	if len(lines) == 0 && len(report.Evidence) > 0 {
		return "Evidence was collected, but no claim was verified. A reliable conclusion cannot be formed yet."
	}
	if len(lines) == 0 {
		return "The investigation produced no admissible evidence, so a reliable conclusion cannot be given."
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
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
	status := deliveryStatus(contract, report)
	var compositionFailure *RunFailure
	text := ""
	if composer != nil && len(report.Claims) > 0 {
		draft, err := composer.Compose(ctx, contract, report)
		if err == nil {
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
	for _, goal := range contract.Goals {
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
