package featuredelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	maxDocumentBytes = 512 << 10
	maxEvidenceRefs  = 100
	maxEvidenceText  = 1200
)

func BuildArtifact(kind ArtifactKind, requestID, parentID string, origin ArtifactOrigin, documentJSON []byte, evidence []EvidenceRef, createdBy int64) (Artifact, error) {
	document, canonical, err := decodeDocument(kind, documentJSON)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(evidence) > maxEvidenceRefs {
		return Artifact{}, fmt.Errorf("evidence exceeds %d entries: %w", maxEvidenceRefs, ErrInvalid)
	}
	for i := range evidence {
		evidence[i].Summary = strings.TrimSpace(evidence[i].Summary)
		if len(evidence[i].Summary) > maxEvidenceText {
			return Artifact{}, fmt.Errorf("evidence %d summary exceeds %d bytes: %w", i, maxEvidenceText, ErrInvalid)
		}
	}
	rendered := renderDocument(kind, document)
	content := sha256.Sum256(append(append([]byte(nil), canonical...), []byte("\n"+rendered)...))
	id, err := NewID("art")
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		ID:               id,
		RequestID:        requestID,
		Kind:             kind,
		ParentArtifactID: parentID,
		Origin:           origin,
		DocumentJSON:     canonical,
		RenderedMarkdown: rendered,
		Evidence:         append([]EvidenceRef(nil), evidence...),
		ContentHash:      hex.EncodeToString(content[:]),
		CreatedBy:        createdBy,
	}, nil
}

func decodeDocument(kind ArtifactKind, raw []byte) (any, json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maxDocumentBytes {
		return nil, nil, fmt.Errorf("document must be between 1 and %d bytes", maxDocumentBytes)
	}
	var document any
	switch kind {
	case KindRequirement:
		document = &RequirementDocument{}
	case KindRequirementAnalysis:
		document = &RequirementAnalysisDocument{}
	case KindTechnicalProposal:
		document = &TechnicalProposalDocument{}
	case KindSystemDesign:
		document = &SystemDesignDocument{}
	case KindImplementationPlan:
		document = &ImplementationPlanDocument{}
	default:
		return nil, nil, fmt.Errorf("unsupported artifact kind %q", kind)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(document); err != nil {
		return nil, nil, fmt.Errorf("decode %s document: %w", kind, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, fmt.Errorf("decode %s document: multiple JSON values", kind)
	}
	if err := validateDocument(kind, document); err != nil {
		return nil, nil, err
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal %s document: %w", kind, err)
	}
	return document, canonical, nil
}

func validateDocument(kind ArtifactKind, document any) error {
	switch kind {
	case KindRequirement:
		value := document.(*RequirementDocument)
		if strings.TrimSpace(value.Description) == "" {
			return fmt.Errorf("requirement description is required")
		}
	case KindRequirementAnalysis:
		value := document.(*RequirementAnalysisDocument)
		if strings.TrimSpace(value.Background) == "" || len(value.Goals) == 0 || len(value.FunctionalRequirements) == 0 || len(value.AcceptanceCriteria) == 0 {
			return fmt.Errorf("requirement analysis requires background, goals, functional requirements, and acceptance criteria")
		}
		return validateClaims(value.Claims)
	case KindTechnicalProposal:
		value := document.(*TechnicalProposalDocument)
		if len(value.CurrentFacts) == 0 || len(value.Options) < 2 || strings.TrimSpace(value.Recommendation) == "" || strings.TrimSpace(value.RecommendationReason) == "" {
			return fmt.Errorf("technical proposal requires facts, at least two options, and a recommendation")
		}
		return validateClaims(value.CurrentFacts)
	case KindSystemDesign:
		value := document.(*SystemDesignDocument)
		if len(value.ArchitectureBoundaries) == 0 || len(value.Modules) == 0 || len(value.Testing) == 0 {
			return fmt.Errorf("system design requires architecture boundaries, modules, and testing strategy")
		}
		return validateClaims(value.Claims)
	case KindImplementationPlan:
		value := document.(*ImplementationPlanDocument)
		if len(value.Repositories) == 0 {
			return fmt.Errorf("implementation plan requires at least one repository")
		}
		seen := make(map[string]struct{}, len(value.Repositories))
		for i, repository := range value.Repositories {
			if strings.TrimSpace(repository.Repository) == "" || len(repository.Steps) == 0 {
				return fmt.Errorf("repository plan %d requires repository and steps", i)
			}
			if _, ok := seen[repository.Repository]; ok {
				return fmt.Errorf("repository %q appears more than once", repository.Repository)
			}
			seen[repository.Repository] = struct{}{}
		}
	}
	return nil
}

func validateClaims(claims []EvidenceClaim) error {
	for i, claim := range claims {
		if strings.TrimSpace(claim.Statement) == "" {
			return fmt.Errorf("claim %d statement is required", i)
		}
		switch claim.Classification {
		case "fact":
			if len(claim.EvidenceIDs) == 0 {
				return fmt.Errorf("fact claim %d requires evidence IDs", i)
			}
		case "inference", "decision", "unknown":
		default:
			return fmt.Errorf("claim %d has invalid classification %q", i, claim.Classification)
		}
	}
	return nil
}

func BlockingQuestions(artifact Artifact) ([]string, error) {
	document, _, err := decodeDocument(artifact.Kind, artifact.DocumentJSON)
	if err != nil {
		return nil, err
	}
	switch value := document.(type) {
	case *RequirementAnalysisDocument:
		return value.BlockingQuestions, nil
	case *TechnicalProposalDocument:
		return value.BlockingQuestions, nil
	case *SystemDesignDocument:
		return value.BlockingQuestions, nil
	case *ImplementationPlanDocument:
		return value.BlockingQuestions, nil
	default:
		return nil, nil
	}
}

func renderDocument(kind ArtifactKind, document any) string {
	var builder strings.Builder
	switch kind {
	case KindRequirement:
		value := document.(*RequirementDocument)
		writeTitle(&builder, "Product Requirement")
		writeText(&builder, "Description", value.Description)
		writeList(&builder, "Target Repositories", value.TargetRepositories)
		writeList(&builder, "Business Constraints", value.BusinessConstraints)
		writeList(&builder, "Attachments", value.Attachments)
		writeList(&builder, "Acceptance Criteria", value.AcceptanceCriteria)
	case KindRequirementAnalysis:
		value := document.(*RequirementAnalysisDocument)
		writeTitle(&builder, "Requirement Analysis")
		writeText(&builder, "Background", value.Background)
		writeList(&builder, "Goals", value.Goals)
		writeList(&builder, "Users And Scenarios", value.UsersAndScenarios)
		writeList(&builder, "Functional Requirements", value.FunctionalRequirements)
		writeList(&builder, "Non-Functional Requirements", value.NonFunctionalRequirements)
		writeList(&builder, "In Scope", value.InScope)
		writeList(&builder, "Out Of Scope", value.OutOfScope)
		writeList(&builder, "Business Rules", value.BusinessRules)
		writeList(&builder, "Acceptance Criteria", value.AcceptanceCriteria)
		writeList(&builder, "Assumptions", value.Assumptions)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
		writeList(&builder, "Open Questions", value.OpenQuestions)
		writeList(&builder, "Initial Impact", value.InitialImpact)
		writeClaims(&builder, value.Claims)
	case KindTechnicalProposal:
		value := document.(*TechnicalProposalDocument)
		writeTitle(&builder, "Technical Proposal")
		writeClaimsNamed(&builder, "Current Facts", value.CurrentFacts)
		writeList(&builder, "Affected Areas", value.AffectedAreas)
		builder.WriteString("## Options\n\n")
		for _, option := range value.Options {
			builder.WriteString("### " + option.Name + "\n\n")
			builder.WriteString(option.Summary + "\n\n")
			writeList(&builder, "Benefits", option.Benefits)
			writeList(&builder, "Costs", option.Costs)
			writeList(&builder, "Risks", option.Risks)
		}
		writeText(&builder, "Recommendation", value.Recommendation)
		writeText(&builder, "Recommendation Reason", value.RecommendationReason)
		writeList(&builder, "Data And API Impact", value.DataAndAPIImpact)
		writeList(&builder, "Compatibility Risks", value.CompatibilityRisks)
		writeList(&builder, "Rollout", value.Rollout)
		writeList(&builder, "Rollback", value.Rollback)
		writeList(&builder, "Open Decisions", value.OpenDecisions)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
	case KindSystemDesign:
		value := document.(*SystemDesignDocument)
		writeTitle(&builder, "System Design")
		writeList(&builder, "Architecture Boundaries", value.ArchitectureBoundaries)
		builder.WriteString("## Modules\n\n")
		for _, module := range value.Modules {
			builder.WriteString("### " + module.Name + "\n\n")
			writeList(&builder, "Responsibilities", module.Responsibilities)
			writeList(&builder, "Dependencies", module.Dependencies)
		}
		writeList(&builder, "Key Flows", value.KeyFlows)
		writeList(&builder, "API Contracts", value.APIContracts)
		writeList(&builder, "Data Model", value.DataModel)
		writeList(&builder, "Consistency", value.Consistency)
		writeList(&builder, "Security", value.Security)
		writeList(&builder, "Configuration", value.Configuration)
		writeList(&builder, "Errors And Degradation", value.ErrorsAndDegradation)
		writeList(&builder, "Observability", value.Observability)
		writeList(&builder, "Testing", value.Testing)
		writeList(&builder, "Rollout And Rollback", value.RolloutAndRollback)
		writeList(&builder, "Rejected Alternatives", value.RejectedAlternatives)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
		writeClaims(&builder, value.Claims)
	case KindImplementationPlan:
		value := document.(*ImplementationPlanDocument)
		writeTitle(&builder, "Implementation Plan")
		for _, repository := range value.Repositories {
			builder.WriteString("## Repository: " + repository.Repository + "\n\n")
			writeList(&builder, "Expected Paths", repository.ExpectedPaths)
			builder.WriteString("### Steps\n\n")
			for i, step := range repository.Steps {
				fmt.Fprintf(&builder, "%d. %s\n", i+1, step.Description)
				for _, done := range step.DoneWhen {
					builder.WriteString("   - Done when: " + done + "\n")
				}
			}
			builder.WriteString("\n")
			writeCommands(&builder, repository.ValidationCommands)
		}
		writeList(&builder, "Contracts", value.Contracts)
		writeList(&builder, "Migrations", value.Migrations)
		writeList(&builder, "Risks", value.Risks)
		writeList(&builder, "Do Not Modify", value.DoNotModify)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
	}
	return strings.TrimSpace(builder.String()) + "\n"
}

func writeTitle(builder *strings.Builder, title string) {
	builder.WriteString("# " + title + "\n\n")
}

func writeText(builder *strings.Builder, title, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	builder.WriteString("## " + title + "\n\n" + value + "\n\n")
}

func writeList(builder *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	builder.WriteString("## " + title + "\n\n")
	for _, value := range values {
		builder.WriteString("- " + value + "\n")
	}
	builder.WriteString("\n")
}

func writeClaims(builder *strings.Builder, claims []EvidenceClaim) {
	writeClaimsNamed(builder, "Claims", claims)
}

func writeClaimsNamed(builder *strings.Builder, title string, claims []EvidenceClaim) {
	if len(claims) == 0 {
		return
	}
	builder.WriteString("## " + title + "\n\n")
	for _, claim := range claims {
		fmt.Fprintf(builder, "- [%s] %s", claim.Classification, claim.Statement)
		if len(claim.EvidenceIDs) > 0 {
			fmt.Fprintf(builder, " (evidence: %v)", claim.EvidenceIDs)
		}
		builder.WriteString("\n")
	}
	builder.WriteString("\n")
}

func writeCommands(builder *strings.Builder, commands [][]string) {
	if len(commands) == 0 {
		return
	}
	builder.WriteString("### Validation Commands\n\n")
	for _, command := range commands {
		builder.WriteString("- `" + strings.Join(command, " ") + "`\n")
	}
	builder.WriteString("\n")
}
