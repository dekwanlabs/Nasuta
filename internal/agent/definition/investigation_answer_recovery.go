package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
)

const maxRecoveredInvestigationCitations = 50

type recoveredInvestigationBundle struct {
	SupportedClaims   []recoveredInvestigationClaim `json:"supported_claims"`
	PartialClaims     []recoveredInvestigationClaim `json:"partial_claims"`
	Limitations       []string                      `json:"limitations"`
	LimitationsDetail recoveredLimitationsDetail    `json:"limitations_detail"`
	Verification      struct {
		Decision   string `json:"decision"`
		StopReason string `json:"stop_reason"`
	} `json:"verification"`
	Completeness string `json:"completeness"`
}

type recoveredInvestigationClaim struct {
	ProducerNodeID string                           `json:"producer_node_id"`
	FindingIndex   int                              `json:"finding_index"`
	Claim          string                           `json:"claim"`
	Evidence       []recoveredInvestigationEvidence `json:"evidence"`
}

type recoveredInvestigationEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type recoveredLimitationsDetail struct {
	ArtifactID           string `json:"artifact_id"`
	TotalCount           int    `json:"total_count"`
	DisplayedCount       int    `json:"displayed_count"`
	OmittedCount         int    `json:"omitted_count"`
	NormalizationVersion string `json:"normalization_version"`
}

type recoveredInvestigationCitation struct {
	Claim    string                           `json:"claim"`
	Evidence []recoveredInvestigationEvidence `json:"evidence"`
}

type recoveredInvestigationAnswer struct {
	Answer            string                           `json:"answer"`
	Citations         []recoveredInvestigationCitation `json:"citations"`
	Limitations       []string                         `json:"limitations"`
	LimitationsDetail recoveredLimitationsDetail       `json:"limitations_detail"`
}

func canRecoverInvestigationAnswer(
	ref agentapi.SchemaRef,
	err error,
) bool {
	return ref == (agentapi.SchemaRef{
		ID: "investigation.answer", Version: 3,
	}) && (errors.Is(err, execution.ErrReasoningTruncated) ||
		errors.Is(err, execution.ErrEmptyModelResponse) ||
		errors.Is(err, execution.ErrAnswerTruncated))
}

func recoverInvestigationAnswer(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	context outputRecoveryContext,
) (json.RawMessage, error) {
	if schemas == nil {
		return nil, fmt.Errorf("investigation answer recovery schema registry is required")
	}
	if context.AgentID != "synthesizer" || len(context.Input) == 0 {
		return nil, fmt.Errorf("investigation answer recovery context is incomplete")
	}
	inputSchema := agentapi.SchemaRef{
		ID: "investigation.verified_bundle", Version: 2,
	}
	if err := schemas.Validate(inputSchema, context.Input); err != nil {
		return nil, fmt.Errorf("validate verified investigation bundle: %w", err)
	}

	var bundle recoveredInvestigationBundle
	if err := json.Unmarshal(context.Input, &bundle); err != nil {
		return nil, fmt.Errorf("decode verified investigation bundle: %w", err)
	}
	answer := recoveredAnswerText(
		bundle,
		synthesisObjective(context.Context),
	)
	citations := recoveredCitations(
		bundle.SupportedClaims,
		bundle.PartialClaims,
	)
	output, err := json.Marshal(recoveredInvestigationAnswer{
		Answer:            answer,
		Citations:         citations,
		Limitations:       append([]string(nil), bundle.Limitations...),
		LimitationsDetail: bundle.LimitationsDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("encode recovered investigation answer: %w", err)
	}
	if err := schemas.Validate(ref, output); err != nil {
		return nil, fmt.Errorf("validate recovered investigation answer: %w", err)
	}
	return output, nil
}

func recoveredAnswerText(
	bundle recoveredInvestigationBundle,
	objective string,
) string {
	claims := make(
		[]recoveredInvestigationClaim,
		0,
		min(
			len(bundle.SupportedClaims)+len(bundle.PartialClaims),
			maxRecoveredInvestigationCitations,
		),
	)
	claims = appendRecoveredClaims(claims, bundle.SupportedClaims)
	claims = appendRecoveredClaims(claims, bundle.PartialClaims)

	var answer strings.Builder
	for _, claim := range claims {
		if answer.Len() > 0 {
			answer.WriteByte('\n')
		}
		answer.WriteString("- ")
		answer.WriteString(strings.TrimSpace(claim.Claim))
	}
	if answer.Len() > 0 {
		return answer.String()
	}
	for _, limitation := range bundle.Limitations {
		limitation = strings.TrimSpace(limitation)
		if limitation == "" {
			continue
		}
		if answer.Len() > 0 {
			answer.WriteByte('\n')
		}
		answer.WriteString("- ")
		answer.WriteString(limitation)
	}
	if answer.Len() > 0 {
		return answer.String()
	}
	if objective = strings.TrimSpace(objective); objective != "" {
		answer.WriteString(objective)
		answer.WriteString("\n\n")
	}
	answer.WriteString("verification=")
	answer.WriteString(bundle.Verification.Decision)
	answer.WriteString(", stop_reason=")
	answer.WriteString(bundle.Verification.StopReason)
	return answer.String()
}

func appendRecoveredClaims(
	dst []recoveredInvestigationClaim,
	src []recoveredInvestigationClaim,
) []recoveredInvestigationClaim {
	for _, claim := range src {
		if len(dst) == maxRecoveredInvestigationCitations {
			break
		}
		if strings.TrimSpace(claim.Claim) == "" || len(claim.Evidence) == 0 {
			continue
		}
		dst = append(dst, claim)
	}
	return dst
}

func recoveredCitations(
	supported []recoveredInvestigationClaim,
	partial []recoveredInvestigationClaim,
) []recoveredInvestigationCitation {
	claims := make(
		[]recoveredInvestigationClaim,
		0,
		min(
			len(supported)+len(partial),
			maxRecoveredInvestigationCitations,
		),
	)
	claims = appendRecoveredClaims(claims, supported)
	claims = appendRecoveredClaims(claims, partial)
	citations := make([]recoveredInvestigationCitation, 0, len(claims))
	for _, claim := range claims {
		citations = append(citations, recoveredInvestigationCitation{
			Claim: strings.TrimSpace(claim.Claim),
			Evidence: append(
				[]recoveredInvestigationEvidence(nil),
				claim.Evidence...,
			),
		})
	}
	return citations
}

func synthesisObjective(blocks []agentapi.ContextBlock) string {
	for _, block := range blocks {
		if block.Source != "workflow.synthesis_objective" {
			continue
		}
		var objective struct {
			Objective string `json:"objective"`
		}
		if json.Unmarshal([]byte(block.Content), &objective) == nil {
			return objective.Objective
		}
	}
	return ""
}
