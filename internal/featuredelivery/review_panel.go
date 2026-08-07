package featuredelivery

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	RiskFactArtifactContentBytes      = "artifact_content_bytes"
	RiskFactArtifactEvidenceCount     = "artifact_evidence_count"
	RiskFactArtifactEvidenceBytes     = "artifact_evidence_bytes"
	RiskFactFilesChanged              = "files_changed"
	RiskFactLinesAdded                = "lines_added"
	RiskFactLinesDeleted              = "lines_deleted"
	RiskFactPatchBytes                = "patch_bytes"
	RiskFactBinaryFiles               = "binary_files"
	RiskFactPlanDeviations            = "plan_deviations"
	RiskFactUnexplainedPlanDeviations = "unexplained_plan_deviations"
	RiskFactValidationFailures        = "validation_failures"
	RiskFactValidationTimeouts        = "validation_timeouts"
	RiskFactValidationOutputBytes     = "validation_output_bytes"
)

var canonicalRiskID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

var supportedReviewRiskFacts = map[string]struct{}{
	RiskFactArtifactContentBytes:      {},
	RiskFactArtifactEvidenceCount:     {},
	RiskFactArtifactEvidenceBytes:     {},
	RiskFactFilesChanged:              {},
	RiskFactLinesAdded:                {},
	RiskFactLinesDeleted:              {},
	RiskFactPatchBytes:                {},
	RiskFactBinaryFiles:               {},
	RiskFactPlanDeviations:            {},
	RiskFactUnexplainedPlanDeviations: {},
	RiskFactValidationFailures:        {},
	RiskFactValidationTimeouts:        {},
	RiskFactValidationOutputBytes:     {},
}

func prepareReviewRiskRules(
	policy *ReviewPolicy,
	reviewerIDs map[string]struct{},
) error {
	if len(policy.RiskRules) == 0 {
		if policy.RiskRuleVersion != "" {
			return fmt.Errorf("review risk rule version has no rules: %w", ErrInvalid)
		}
		return nil
	}
	if !canonicalRiskID.MatchString(policy.RiskRuleVersion) {
		return fmt.Errorf("review risk rule version %q is invalid: %w", policy.RiskRuleVersion, ErrInvalid)
	}
	if len(policy.RiskRules) > 32 {
		return fmt.Errorf("review policy exceeds 32 risk rules: %w", ErrInvalid)
	}
	required := make(map[string]struct{}, len(policy.Reviewers))
	optionalReferenced := make(map[string]struct{}, len(policy.Reviewers))
	for _, reviewer := range policy.Reviewers {
		if reviewer.Required {
			required[reviewer.ID] = struct{}{}
		}
	}
	ruleIDs := make(map[string]struct{}, len(policy.RiskRules))
	for ruleIndex := range policy.RiskRules {
		rule := &policy.RiskRules[ruleIndex]
		rule.ID = strings.ToLower(strings.TrimSpace(rule.ID))
		if !canonicalRiskID.MatchString(rule.ID) {
			return fmt.Errorf("review risk rule %d id is invalid: %w", ruleIndex, ErrInvalid)
		}
		if _, duplicate := ruleIDs[rule.ID]; duplicate {
			return fmt.Errorf("duplicate review risk rule %q: %w", rule.ID, ErrInvalid)
		}
		ruleIDs[rule.ID] = struct{}{}
		if len(rule.Conditions) == 0 || len(rule.Conditions) > len(supportedReviewRiskFacts) {
			return fmt.Errorf("review risk rule %q conditions are invalid: %w", rule.ID, ErrInvalid)
		}
		for conditionIndex := range rule.Conditions {
			condition := &rule.Conditions[conditionIndex]
			condition.Fact = strings.ToLower(strings.TrimSpace(condition.Fact))
			if _, ok := supportedReviewRiskFacts[condition.Fact]; !ok ||
				condition.Value < 0 ||
				!validReviewRiskOperator(condition.Operator) {
				return fmt.Errorf(
					"review risk rule %q condition %d is invalid: %w",
					rule.ID,
					conditionIndex,
					ErrInvalid,
				)
			}
		}
		sort.Slice(rule.Conditions, func(i, j int) bool {
			left := rule.Conditions[i]
			right := rule.Conditions[j]
			if left.Fact != right.Fact {
				return left.Fact < right.Fact
			}
			if left.Operator != right.Operator {
				return left.Operator < right.Operator
			}
			return left.Value < right.Value
		})
		rule.ReviewerIDs = canonicalStrings(rule.ReviewerIDs)
		if len(rule.ReviewerIDs) == 0 {
			return fmt.Errorf("review risk rule %q has no reviewers: %w", rule.ID, ErrInvalid)
		}
		for _, reviewerID := range rule.ReviewerIDs {
			if _, ok := reviewerIDs[reviewerID]; !ok {
				return fmt.Errorf(
					"review risk rule %q references unknown reviewer %q: %w",
					rule.ID,
					reviewerID,
					ErrInvalid,
				)
			}
			if _, isRequired := required[reviewerID]; isRequired {
				return fmt.Errorf(
					"review risk rule %q redundantly selects required reviewer %q: %w",
					rule.ID,
					reviewerID,
					ErrInvalid,
				)
			}
			optionalReferenced[reviewerID] = struct{}{}
		}
	}
	for _, reviewer := range policy.Reviewers {
		if reviewer.Required {
			continue
		}
		if _, ok := optionalReferenced[reviewer.ID]; !ok {
			return fmt.Errorf(
				"optional reviewer %q has no risk rule: %w",
				reviewer.ID,
				ErrInvalid,
			)
		}
	}
	sort.Slice(policy.RiskRules, func(i, j int) bool {
		return policy.RiskRules[i].ID < policy.RiskRules[j].ID
	})
	return nil
}

func validReviewRiskOperator(operator ReviewRiskOperator) bool {
	switch operator {
	case RiskEqual, RiskNotEqual, RiskGreaterThan, RiskGreaterThanOrEqual,
		RiskLessThan, RiskLessThanOrEqual:
		return true
	default:
		return false
	}
}

type reviewRiskValues struct {
	artifactContentBytes      int64
	artifactEvidenceCount     int64
	artifactEvidenceBytes     int64
	filesChanged              int64
	linesAdded                int64
	linesDeleted              int64
	patchBytes                int64
	binaryFiles               int64
	planDeviations            int64
	unexplainedPlanDeviations int64
	validationFailures        int64
	validationTimeouts        int64
	validationOutputBytes     int64
}

func BuildArtifactReviewRiskFacts(artifact Artifact) ([]ReviewRiskFact, error) {
	evidence, err := json.Marshal(artifact.Evidence)
	if err != nil {
		return nil, fmt.Errorf("marshal artifact risk evidence: %w", err)
	}
	return prepareReviewRiskFacts(reviewRiskValues{
		artifactContentBytes:  int64(len(artifact.DocumentJSON) + len(artifact.RenderedMarkdown)),
		artifactEvidenceCount: int64(len(artifact.Evidence)),
		artifactEvidenceBytes: int64(len(evidence)),
	})
}

func BuildImplementationReviewRiskFacts(
	kind SubjectKind,
	run ImplementationRun,
) ([]ReviewRiskFact, error) {
	if run.ChangeSet == nil {
		return nil, fmt.Errorf("implementation %q has no change set: %w", run.ID, ErrConflict)
	}
	values := reviewRiskValues{}
	switch kind {
	case SubjectChangeSet:
		addChangeRiskValues(&values, *run.ChangeSet)
	case SubjectValidationBundle:
		addValidationRiskValues(&values, run.ChangeSet.ValidationResults)
	case SubjectDeliveryBundle:
		addChangeRiskValues(&values, *run.ChangeSet)
		addValidationRiskValues(&values, run.ChangeSet.ValidationResults)
	default:
		return nil, fmt.Errorf("subject kind %q has no implementation risk facts: %w", kind, ErrInvalid)
	}
	return prepareReviewRiskFacts(values)
}

func PrepareReviewPanel(
	policy ReviewPolicy,
	facts []ReviewRiskFact,
) ([]ReviewRiskFact, string, []ReviewerSpec, string, error) {
	preparedFacts, err := canonicalReviewRiskFacts(facts)
	if err != nil {
		return nil, "", nil, "", err
	}
	riskHash, err := hashJSON(preparedFacts)
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("hash review risk facts: %w", err)
	}
	selected := make([]ReviewerSpec, 0, len(policy.Reviewers))
	if len(policy.RiskRules) == 0 {
		selected = cloneReviewerSpecs(policy.Reviewers)
	} else {
		values := make(map[string]int64, len(preparedFacts))
		for _, fact := range preparedFacts {
			values[fact.Key] = fact.Value
		}
		selectedIDs := make(map[string]struct{}, len(policy.Reviewers))
		for _, reviewer := range policy.Reviewers {
			if reviewer.Required {
				selectedIDs[reviewer.ID] = struct{}{}
			}
		}
		for _, rule := range policy.RiskRules {
			if !matchesReviewRiskRule(rule, values) {
				continue
			}
			for _, reviewerID := range rule.ReviewerIDs {
				selectedIDs[reviewerID] = struct{}{}
			}
		}
		for _, reviewer := range policy.Reviewers {
			if _, ok := selectedIDs[reviewer.ID]; ok {
				selected = append(selected, cloneReviewerSpec(reviewer))
			}
		}
	}
	if err := validateSelectedReviewers(policy, selected); err != nil {
		return nil, "", nil, "", err
	}
	panelHash, err := reviewPanelHash(policy.RiskRuleVersion, selected)
	if err != nil {
		return nil, "", nil, "", err
	}
	return preparedFacts, riskHash, selected, panelHash, nil
}

func ValidateReviewRoundSnapshot(policy ReviewPolicy, round ReviewRound) error {
	facts, err := canonicalReviewRiskFacts(round.RiskFacts)
	if err != nil {
		return fmt.Errorf("review round risk facts: %w", err)
	}
	riskHash, err := hashJSON(facts)
	if err != nil {
		return fmt.Errorf("hash review round risk facts: %w", err)
	}
	if riskHash != round.RiskHash || round.RuleVersion != policy.RiskRuleVersion {
		return fmt.Errorf("review round risk snapshot does not match policy: %w", ErrConflict)
	}
	if err := validateSelectedReviewers(policy, round.Reviewers); err != nil {
		return err
	}
	panelHash, err := reviewPanelHash(round.RuleVersion, round.Reviewers)
	if err != nil {
		return err
	}
	if panelHash != round.PanelHash {
		return fmt.Errorf("review round panel hash mismatch: %w", ErrConflict)
	}
	return nil
}

func canonicalReviewRiskFacts(facts []ReviewRiskFact) ([]ReviewRiskFact, error) {
	prepared := append([]ReviewRiskFact(nil), facts...)
	seen := make(map[string]struct{}, len(prepared))
	for index := range prepared {
		prepared[index].Key = strings.ToLower(strings.TrimSpace(prepared[index].Key))
		if _, ok := supportedReviewRiskFacts[prepared[index].Key]; !ok ||
			prepared[index].Value < 0 {
			return nil, fmt.Errorf("review risk fact %d is invalid: %w", index, ErrInvalid)
		}
		if _, duplicate := seen[prepared[index].Key]; duplicate {
			return nil, fmt.Errorf(
				"duplicate review risk fact %q: %w",
				prepared[index].Key,
				ErrInvalid,
			)
		}
		seen[prepared[index].Key] = struct{}{}
	}
	if len(seen) != len(supportedReviewRiskFacts) {
		return nil, fmt.Errorf("review risk facts are incomplete: %w", ErrInvalid)
	}
	sort.Slice(prepared, func(i, j int) bool {
		return prepared[i].Key < prepared[j].Key
	})
	return prepared, nil
}

func prepareReviewRiskFacts(values reviewRiskValues) ([]ReviewRiskFact, error) {
	return canonicalReviewRiskFacts([]ReviewRiskFact{
		{Key: RiskFactArtifactContentBytes, Value: values.artifactContentBytes},
		{Key: RiskFactArtifactEvidenceCount, Value: values.artifactEvidenceCount},
		{Key: RiskFactArtifactEvidenceBytes, Value: values.artifactEvidenceBytes},
		{Key: RiskFactFilesChanged, Value: values.filesChanged},
		{Key: RiskFactLinesAdded, Value: values.linesAdded},
		{Key: RiskFactLinesDeleted, Value: values.linesDeleted},
		{Key: RiskFactPatchBytes, Value: values.patchBytes},
		{Key: RiskFactBinaryFiles, Value: values.binaryFiles},
		{Key: RiskFactPlanDeviations, Value: values.planDeviations},
		{Key: RiskFactUnexplainedPlanDeviations, Value: values.unexplainedPlanDeviations},
		{Key: RiskFactValidationFailures, Value: values.validationFailures},
		{Key: RiskFactValidationTimeouts, Value: values.validationTimeouts},
		{Key: RiskFactValidationOutputBytes, Value: values.validationOutputBytes},
	})
}

func addChangeRiskValues(values *reviewRiskValues, changeSet ChangeSet) {
	values.filesChanged = int64(changeSet.FilesChanged)
	values.linesAdded = int64(changeSet.Additions)
	values.linesDeleted = int64(changeSet.Deletions)
	values.patchBytes = changeSet.PatchBytes
	values.planDeviations = int64(len(changeSet.PlanDeviations))
	for _, file := range changeSet.Files {
		if file.Binary {
			values.binaryFiles++
		}
	}
	for _, deviation := range changeSet.PlanDeviations {
		if !deviation.Explained {
			values.unexplainedPlanDeviations++
		}
	}
}

func addValidationRiskValues(values *reviewRiskValues, results []ValidationResult) {
	for _, result := range results {
		if result.Status == "failed" {
			values.validationFailures++
		}
		if result.TimedOut {
			values.validationTimeouts++
		}
		values.validationOutputBytes += result.OutputBytes
	}
}

func matchesReviewRiskRule(rule ReviewRiskRule, facts map[string]int64) bool {
	for _, condition := range rule.Conditions {
		actual := facts[condition.Fact]
		switch condition.Operator {
		case RiskEqual:
			if actual != condition.Value {
				return false
			}
		case RiskNotEqual:
			if actual == condition.Value {
				return false
			}
		case RiskGreaterThan:
			if actual <= condition.Value {
				return false
			}
		case RiskGreaterThanOrEqual:
			if actual < condition.Value {
				return false
			}
		case RiskLessThan:
			if actual >= condition.Value {
				return false
			}
		case RiskLessThanOrEqual:
			if actual > condition.Value {
				return false
			}
		}
	}
	return true
}

func validateSelectedReviewers(policy ReviewPolicy, selected []ReviewerSpec) error {
	if len(selected) < 2 || len(selected) > len(policy.Reviewers) {
		return fmt.Errorf("review round panel size is invalid: %w", ErrConflict)
	}
	available := make(map[string]ReviewerSpec, len(policy.Reviewers))
	for _, reviewer := range policy.Reviewers {
		available[reviewer.ID] = reviewer
	}
	seen := make(map[string]struct{}, len(selected))
	for _, reviewer := range selected {
		expected, ok := available[reviewer.ID]
		if !ok || !sameReviewerSpec(expected, reviewer) {
			return fmt.Errorf(
				"review round reviewer %q does not match policy: %w",
				reviewer.ID,
				ErrConflict,
			)
		}
		if _, duplicate := seen[reviewer.ID]; duplicate {
			return fmt.Errorf("duplicate review round reviewer %q: %w", reviewer.ID, ErrConflict)
		}
		seen[reviewer.ID] = struct{}{}
	}
	for _, reviewer := range policy.Reviewers {
		if reviewer.Required {
			if _, ok := seen[reviewer.ID]; !ok {
				return fmt.Errorf(
					"required reviewer %q is absent from round panel: %w",
					reviewer.ID,
					ErrConflict,
				)
			}
		}
	}
	return nil
}

func reviewPanelHash(ruleVersion string, reviewers []ReviewerSpec) (string, error) {
	hash, err := hashJSON(struct {
		RuleVersion string         `json:"rule_version"`
		Reviewers   []ReviewerSpec `json:"reviewers"`
	}{
		RuleVersion: ruleVersion,
		Reviewers:   reviewers,
	})
	if err != nil {
		return "", fmt.Errorf("hash review panel: %w", err)
	}
	return hash, nil
}

func sameReviewerSpec(left, right ReviewerSpec) bool {
	return left.ID == right.ID &&
		left.Agent == right.Agent &&
		left.DefinitionHash == right.DefinitionHash &&
		left.Required == right.Required &&
		left.ReadOnly == right.ReadOnly &&
		equalCanonicalStrings(left.Categories, right.Categories)
}

func cloneReviewerSpecs(reviewers []ReviewerSpec) []ReviewerSpec {
	cloned := make([]ReviewerSpec, 0, len(reviewers))
	for _, reviewer := range reviewers {
		cloned = append(cloned, cloneReviewerSpec(reviewer))
	}
	return cloned
}

func cloneReviewerSpec(reviewer ReviewerSpec) ReviewerSpec {
	reviewer.Categories = append([]string(nil), reviewer.Categories...)
	return reviewer
}
