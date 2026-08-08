package delivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
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
		evidence[i].Summary = truncateText(evidence[i].Summary, maxEvidenceText)
		if evidence[i].Summary == "" {
			return Artifact{}, fmt.Errorf("evidence %d summary is required: %w", i, ErrInvalid)
		}
	}
	if err := validateEvidenceReferences(document, len(evidence)); err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	rendered := renderDocument(kind, document)
	content := sha256.Sum256(append(append([]byte(nil), canonical...), []byte("\n"+rendered)...))
	id, err := NewID("art")
	if err != nil {
		return Artifact{}, err
	}
	evidenceSnapshot := make([]EvidenceRef, len(evidence))
	copy(evidenceSnapshot, evidence)
	return Artifact{
		ID:               id,
		RequestID:        requestID,
		Kind:             kind,
		ParentArtifactID: parentID,
		Origin:           origin,
		DocumentJSON:     canonical,
		RenderedMarkdown: rendered,
		Evidence:         evidenceSnapshot,
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
		if strings.TrimSpace(value.ProblemStatement) == "" || len(value.Goals) == 0 || len(value.FunctionalRequirements) == 0 || len(value.AcceptanceCriteria) == 0 {
			return fmt.Errorf("requirement analysis requires a problem statement, goals, functional requirements, and acceptance criteria")
		}
	case KindTechnicalProposal:
		value := document.(*TechnicalProposalDocument)
		if len(value.CurrentTechnicalBaseline) == 0 || len(value.ArchitectureDrivers) == 0 || len(value.CandidateArchitectures) < 2 ||
			strings.TrimSpace(value.TechnicalDecision.SelectedOption) == "" || strings.TrimSpace(value.TechnicalDecision.Rationale) == "" ||
			len(value.TechnicalDecision.AcceptedTradeoffs) == 0 {
			return fmt.Errorf("technical proposal requires a baseline, architecture drivers, at least two candidates, and a decision with trade-offs")
		}
		optionNames := make(map[string]struct{}, len(value.CandidateArchitectures))
		for i := range value.CandidateArchitectures {
			option := &value.CandidateArchitectures[i]
			option.Name = strings.TrimSpace(option.Name)
			if option.Name == "" || strings.TrimSpace(option.Summary) == "" ||
				strings.TrimSpace(option.ArchitecturePattern) == "" ||
				strings.TrimSpace(option.CommunicationPattern) == "" ||
				strings.TrimSpace(option.DataPattern) == "" ||
				strings.TrimSpace(option.DeploymentPattern) == "" ||
				strings.TrimSpace(option.ContractPattern) == "" ||
				strings.TrimSpace(option.MigrationPattern) == "" ||
				strings.TrimSpace(option.ReliabilityPattern) == "" ||
				strings.TrimSpace(option.ObservabilityPattern) == "" {
				return fmt.Errorf("candidate architecture %d requires name, summary, and every architecture pattern", i)
			}
			if _, ok := optionNames[option.Name]; ok {
				return fmt.Errorf("candidate architecture %q appears more than once", option.Name)
			}
			optionNames[option.Name] = struct{}{}
		}
		value.TechnicalDecision.SelectedOption = strings.TrimSpace(value.TechnicalDecision.SelectedOption)
		if _, ok := optionNames[value.TechnicalDecision.SelectedOption]; !ok {
			return fmt.Errorf("technical decision selects unknown candidate %q", value.TechnicalDecision.SelectedOption)
		}
		return validateClaims(value.CurrentTechnicalBaseline)
	case KindSystemDesign:
		value := document.(*SystemDesignDocument)
		record := value.ArchitectureDecisionRecord
		if strings.TrimSpace(record.Status) == "" || strings.TrimSpace(record.Context) == "" ||
			strings.TrimSpace(record.Decision) == "" || len(record.Consequences) == 0 {
			return fmt.Errorf("system design requires a complete architecture decision record")
		}
		if len(value.ArchitectureBoundaries) == 0 || len(value.Modules) == 0 || len(value.TestingStrategy) == 0 {
			return fmt.Errorf("system design requires architecture boundaries, modules, and testing strategy")
		}
		for i, module := range value.Modules {
			if strings.TrimSpace(module.Name) == "" || len(module.Responsibilities) == 0 || len(module.Invariants) == 0 {
				return fmt.Errorf("design module %d requires name, responsibilities, and invariants", i)
			}
		}
	case KindImplementationPlan:
		value := document.(*ImplementationPlanDocument)
		if strings.TrimSpace(value.DeliveryGoal) == "" || len(value.Repositories) == 0 || len(value.DefinitionOfDone) == 0 {
			return fmt.Errorf("implementation plan requires a delivery goal, at least one repository, and definition of done")
		}
		seen := make(map[string]struct{}, len(value.Repositories))
		for i := range value.Repositories {
			repository := &value.Repositories[i]
			if len(repository.Steps) == 0 {
				return fmt.Errorf("repository plan %d requires repository and steps", i)
			}
			canonical, err := NormalizeRepository(repository.Repository)
			if err != nil {
				return fmt.Errorf("repository plan %d: %w", i, err)
			}
			repository.Repository = canonical
			paths := repository.ExpectedPaths[:0]
			seenPaths := make(map[string]struct{}, len(repository.ExpectedPaths))
			for pathIndex, value := range repository.ExpectedPaths {
				canonicalPath, err := NormalizePlanPath(value)
				if err != nil {
					return fmt.Errorf("repository plan %d expected path %d: %w", i, pathIndex, err)
				}
				if _, ok := seenPaths[canonicalPath]; ok {
					continue
				}
				seenPaths[canonicalPath] = struct{}{}
				paths = append(paths, canonicalPath)
			}
			repository.ExpectedPaths = paths
			for stepIndex, step := range repository.Steps {
				if strings.TrimSpace(step.Description) == "" || len(step.DoneWhen) == 0 {
					return fmt.Errorf("repository plan %d step %d requires description and done_when", i, stepIndex)
				}
			}
			if _, ok := seen[canonical]; ok {
				return fmt.Errorf("repository %q appears more than once", canonical)
			}
			seen[canonical] = struct{}{}
		}
		for i, risk := range value.RisksAndMitigations {
			if strings.TrimSpace(risk.Description) == "" || strings.TrimSpace(risk.Mitigation) == "" {
				return fmt.Errorf("delivery risk %d requires description and mitigation", i)
			}
			if !validRiskLevel(risk.Likelihood) || !validRiskLevel(risk.Impact) {
				return fmt.Errorf("delivery risk %d requires low, medium, or high likelihood and impact", i)
			}
		}
	}
	return nil
}

func validRiskLevel(value string) bool {
	switch value {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

func NormalizePlanPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return "", fmt.Errorf("path must be a non-empty repository-relative path")
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return "", fmt.Errorf("path contains a control character")
		}
	}
	canonical := path.Clean(value)
	if canonical == "." || canonical == ".." || strings.HasPrefix(canonical, "../") {
		return "", fmt.Errorf("path escapes the repository")
	}
	return canonical, nil
}

func validateEvidenceReferences(document any, evidenceCount int) error {
	var claims []EvidenceClaim
	switch value := document.(type) {
	case *TechnicalProposalDocument:
		claims = value.CurrentTechnicalBaseline
	}
	for claimIndex, claim := range claims {
		seen := make(map[int]struct{}, len(claim.EvidenceIDs))
		for _, evidenceID := range claim.EvidenceIDs {
			if evidenceID < 0 || evidenceID >= evidenceCount {
				return fmt.Errorf("claim %d references evidence %d outside snapshot", claimIndex, evidenceID)
			}
			if _, ok := seen[evidenceID]; ok {
				return fmt.Errorf("claim %d references evidence %d more than once", claimIndex, evidenceID)
			}
			seen[evidenceID] = struct{}{}
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
		writeList(&builder, "Business Constraints", value.BusinessConstraints)
		writeList(&builder, "Attachments", value.Attachments)
		writeList(&builder, "Acceptance Criteria", value.AcceptanceCriteria)
	case KindRequirementAnalysis:
		value := document.(*RequirementAnalysisDocument)
		writeTitle(&builder, "Requirement Analysis")
		writeText(&builder, "Problem Statement", value.ProblemStatement)
		writeList(&builder, "Goals", value.Goals)
		writeList(&builder, "Success Metrics", value.SuccessMetrics)
		writeList(&builder, "Non-Goals", value.NonGoals)
		writeList(&builder, "Personas And Scenarios", value.PersonasAndScenarios)
		writeList(&builder, "User Stories", value.UserStories)
		writeList(&builder, "Functional Requirements", value.FunctionalRequirements)
		writeList(&builder, "Quality Expectations", value.QualityExpectations)
		writeList(&builder, "In Scope", value.InScope)
		writeList(&builder, "Business Constraints", value.BusinessConstraints)
		writeList(&builder, "Business Rules", value.BusinessRules)
		writeList(&builder, "Acceptance Criteria", value.AcceptanceCriteria)
		writeList(&builder, "Assumptions", value.Assumptions)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
		writeList(&builder, "Open Questions", value.OpenQuestions)
	case KindTechnicalProposal:
		value := document.(*TechnicalProposalDocument)
		writeTitle(&builder, "Technical Proposal")
		writeClaimsNamed(&builder, "Current Technical Baseline", value.CurrentTechnicalBaseline)
		writeList(&builder, "Architecture Drivers", value.ArchitectureDrivers)
		writeList(&builder, "Affected Capabilities", value.AffectedCapabilities)
		writeHeading(&builder, 2, "Candidate Architectures")
		for _, option := range value.CandidateArchitectures {
			writeHeading(&builder, 3, option.Name)
			writeTextLevel(&builder, 4, "Summary", option.Summary)
			writeTextLevel(&builder, 4, "Architecture Pattern", option.ArchitecturePattern)
			writeTextLevel(&builder, 4, "Communication Pattern", option.CommunicationPattern)
			writeTextLevel(&builder, 4, "Data Pattern", option.DataPattern)
			writeTextLevel(&builder, 4, "Deployment Pattern", option.DeploymentPattern)
			writeTextLevel(&builder, 4, "Contract Pattern", option.ContractPattern)
			writeTextLevel(&builder, 4, "Migration Pattern", option.MigrationPattern)
			writeTextLevel(&builder, 4, "Reliability Pattern", option.ReliabilityPattern)
			writeTextLevel(&builder, 4, "Observability Pattern", option.ObservabilityPattern)
			writeListLevel(&builder, 4, "Benefits", option.Benefits)
			writeListLevel(&builder, 4, "Costs", option.Costs)
			writeListLevel(&builder, 4, "Risks", option.Risks)
			writeListLevel(&builder, 4, "Reversibility", option.Reversibility)
		}
		writeHeading(&builder, 2, "Technical Decision")
		writeTextLevel(&builder, 3, "Selected Option", value.TechnicalDecision.SelectedOption)
		writeTextLevel(&builder, 3, "Rationale", value.TechnicalDecision.Rationale)
		writeListLevel(&builder, 3, "Accepted Tradeoffs", value.TechnicalDecision.AcceptedTradeoffs)
		writeList(&builder, "Compatibility Obligations", value.CompatibilityObligations)
		writeList(&builder, "Security Obligations", value.SecurityObligations)
		writeList(&builder, "Performance Obligations", value.PerformanceObligations)
		writeList(&builder, "Operational Obligations", value.OperationalObligations)
		writeList(&builder, "Delivery And Migration Strategy", value.DeliveryAndMigrationStrategy)
		writeList(&builder, "Open Decisions", value.OpenDecisions)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
	case KindSystemDesign:
		value := document.(*SystemDesignDocument)
		writeTitle(&builder, "System Design")
		writeHeading(&builder, 2, "Architecture Decision Record")
		writeTextLevel(&builder, 3, "Status", value.ArchitectureDecisionRecord.Status)
		writeTextLevel(&builder, 3, "Context", value.ArchitectureDecisionRecord.Context)
		writeTextLevel(&builder, 3, "Decision", value.ArchitectureDecisionRecord.Decision)
		writeListLevel(&builder, 3, "Consequences", value.ArchitectureDecisionRecord.Consequences)
		writeList(&builder, "Domain Model", value.DomainModel)
		writeList(&builder, "Architecture Boundaries", value.ArchitectureBoundaries)
		writeHeading(&builder, 2, "Modules")
		for _, module := range value.Modules {
			writeHeading(&builder, 3, module.Name)
			writeListLevel(&builder, 4, "Responsibilities", module.Responsibilities)
			writeListLevel(&builder, 4, "Dependencies", module.Dependencies)
			writeListLevel(&builder, 4, "Invariants", module.Invariants)
		}
		writeList(&builder, "Key Flows", value.KeyFlows)
		writeList(&builder, "Interface Contracts", value.InterfaceContracts)
		writeList(&builder, "Data Ownership And Model", value.DataOwnershipAndModel)
		writeList(&builder, "Consistency And Concurrency", value.ConsistencyAndConcurrency)
		writeList(&builder, "Scalability", value.Scalability)
		writeList(&builder, "Maintainability", value.Maintainability)
		writeList(&builder, "Reliability And Recovery", value.ReliabilityAndRecovery)
		writeList(&builder, "Security", value.Security)
		writeList(&builder, "Configuration", value.Configuration)
		writeList(&builder, "Observability", value.Observability)
		writeList(&builder, "Evolution And Migration", value.EvolutionAndMigration)
		writeList(&builder, "Testing Strategy", value.TestingStrategy)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
	case KindImplementationPlan:
		value := document.(*ImplementationPlanDocument)
		writeTitle(&builder, "Implementation Plan")
		writeText(&builder, "Delivery Goal", value.DeliveryGoal)
		writeHeading(&builder, 2, "Repositories")
		for _, repository := range value.Repositories {
			writeHeading(&builder, 3, repository.Repository)
			writeListLevel(&builder, 4, "Expected Paths", repository.ExpectedPaths)
			writeListLevel(&builder, 4, "Dependencies", repository.Dependencies)
			writeHeading(&builder, 4, "Steps")
			for i, step := range repository.Steps {
				fmt.Fprintf(&builder, "%d. %s\n", i+1, step.Description)
				for _, done := range step.DoneWhen {
					builder.WriteString("   - Done when: " + done + "\n")
				}
			}
			builder.WriteString("\n")
			writeCommandsLevel(&builder, 4, repository.ValidationCommands)
		}
		writeList(&builder, "Dependencies And Contracts", value.DependenciesAndContracts)
		writeList(&builder, "Migration Work", value.MigrationWork)
		writeList(&builder, "Definition Of Done", value.DefinitionOfDone)
		if len(value.RisksAndMitigations) > 0 {
			writeHeading(&builder, 2, "Risks And Mitigations")
			for i, risk := range value.RisksAndMitigations {
				fmt.Fprintf(&builder, "### Risk %d\n\n", i+1)
				writeTextLevel(&builder, 4, "Description", risk.Description)
				writeTextLevel(&builder, 4, "Likelihood", risk.Likelihood)
				writeTextLevel(&builder, 4, "Impact", risk.Impact)
				writeTextLevel(&builder, 4, "Mitigation", risk.Mitigation)
			}
		}
		writeList(&builder, "Do Not Modify", value.DoNotModify)
		writeList(&builder, "Blocking Questions", value.BlockingQuestions)
	}
	return strings.TrimSpace(builder.String()) + "\n"
}

func writeTitle(builder *strings.Builder, title string) {
	builder.WriteString("# " + title + "\n\n")
}

func writeText(builder *strings.Builder, title, value string) {
	writeTextLevel(builder, 2, title, value)
}

func writeTextLevel(builder *strings.Builder, level int, title, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	writeHeading(builder, level, title)
	builder.WriteString(value + "\n\n")
}

func writeList(builder *strings.Builder, title string, values []string) {
	writeListLevel(builder, 2, title, values)
}

func writeListLevel(builder *strings.Builder, level int, title string, values []string) {
	if len(values) == 0 {
		return
	}
	writeHeading(builder, level, title)
	for _, value := range values {
		builder.WriteString("- " + value + "\n")
	}
	builder.WriteString("\n")
}

func writeHeading(builder *strings.Builder, level int, title string) {
	builder.WriteString(strings.Repeat("#", level) + " " + title + "\n\n")
}

func writeClaimsNamed(builder *strings.Builder, title string, claims []EvidenceClaim) {
	if len(claims) == 0 {
		return
	}
	writeHeading(builder, 2, title)
	for _, claim := range claims {
		fmt.Fprintf(builder, "- [%s] %s", claim.Classification, claim.Statement)
		if len(claim.EvidenceIDs) > 0 {
			fmt.Fprintf(builder, " (evidence: %v)", claim.EvidenceIDs)
		}
		builder.WriteString("\n")
	}
	builder.WriteString("\n")
}

func writeCommandsLevel(builder *strings.Builder, level int, commands [][]string) {
	if len(commands) == 0 {
		return
	}
	writeHeading(builder, level, "Validation Commands")
	for _, command := range commands {
		builder.WriteString("- `" + strings.Join(command, " ") + "`\n")
	}
	builder.WriteString("\n")
}
