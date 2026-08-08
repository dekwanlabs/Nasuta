package featurehttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
)

const (
	maxRequirementListItems   = 100
	maxRequirementItemBytes   = 16 << 10
	maxReviewPolicyReviewers  = 16
	maxReviewPolicyItems      = 100
	maxReviewRiskRules        = 32
	maxReviewReuseReasonBytes = 2048
	maxReviewPolicyRolloutBPS = 10000
)

type reviewPolicyInput struct {
	ID                     string                    `json:"id"`
	Version                int64                     `json:"version"`
	SubjectKind            string                    `json:"subject_kind"`
	Reviewers              []delivery.ReviewerSpec   `json:"reviewers"`
	Adjudicator            *delivery.AdjudicatorSpec `json:"adjudicator,omitempty"`
	BlockingSeverities     []delivery.Severity       `json:"blocking_severities"`
	RequiredCategories     []string                  `json:"required_categories"`
	MaxParallelism         int                       `json:"max_parallelism"`
	MaxInputTokens         int64                     `json:"max_input_tokens"`
	MaxOutputTokens        int64                     `json:"max_output_tokens"`
	MaxTotalTokens         int64                     `json:"max_total_tokens"`
	MaxToolCalls           int64                     `json:"max_tool_calls"`
	MaxCostMicros          int64                     `json:"max_cost_micros"`
	MaxRetries             int64                     `json:"max_retries"`
	Timeout                time.Duration             `json:"timeout"`
	OptionalReviewerAction string                    `json:"optional_reviewer_action"`
	RiskRuleVersion        string                    `json:"risk_rule_version,omitempty"`
	RiskRules              []delivery.ReviewRiskRule `json:"risk_rules,omitempty"`
}

type pipelineInput struct {
	ClientRequestID string `json:"client_request_id"`
	Repository      string `json:"repository"`
	BaseRef         string `json:"base_ref"`
	Provider        string `json:"provider"`
	Model           string `json:"model,omitempty"`
	NetworkEnabled  bool   `json:"network_enabled"`
}

type reviewPolicyRolloutInput struct {
	CandidatePolicyID      string `json:"candidate_policy_id"`
	CandidatePolicyVersion int64  `json:"candidate_policy_version"`
	PercentageBPS          int    `json:"percentage_bps"`
	Salt                   string `json:"salt"`
	Active                 bool   `json:"active"`
}

func normalizeReviewPolicyRolloutInput(
	input reviewPolicyRolloutInput,
) (string, int64, int, string, bool, error) {
	candidateID := strings.ToLower(strings.TrimSpace(input.CandidatePolicyID))
	salt := strings.TrimSpace(input.Salt)
	if candidateID == "" || input.CandidatePolicyVersion <= 0 {
		return "", 0, 0, "", false, fmt.Errorf(
			"candidate_policy_id and positive candidate_policy_version are required",
		)
	}
	if input.PercentageBPS < 0 || input.PercentageBPS > maxReviewPolicyRolloutBPS {
		return "", 0, 0, "", false, fmt.Errorf(
			"percentage_bps must be between 0 and %d", maxReviewPolicyRolloutBPS,
		)
	}
	if salt == "" || len(salt) > 255 {
		return "", 0, 0, "", false, fmt.Errorf("salt is required and must not exceed 255 bytes")
	}
	return candidateID, input.CandidatePolicyVersion, input.PercentageBPS, salt, input.Active, nil
}

func normalizePipelineInput(featureID string, input pipelineInput) (pipeline.Request, error) {
	request := pipeline.Request{
		FeatureID:       strings.TrimSpace(featureID),
		ClientRequestID: strings.TrimSpace(input.ClientRequestID),
		Repository:      input.Repository,
		BaseRef:         strings.TrimSpace(input.BaseRef),
		Provider:        strings.ToLower(strings.TrimSpace(input.Provider)),
		Model:           strings.TrimSpace(input.Model),
		NetworkEnabled:  input.NetworkEnabled,
	}
	if request.FeatureID == "" {
		return pipeline.Request{}, fmt.Errorf("feature id is required")
	}
	if request.ClientRequestID == "" {
		return pipeline.Request{}, fmt.Errorf("client_request_id is required")
	}
	if len(request.ClientRequestID) > 128 {
		return pipeline.Request{}, fmt.Errorf("client_request_id exceeds 128 bytes")
	}
	repository, err := delivery.NormalizeRepository(input.Repository)
	if err != nil {
		return pipeline.Request{}, err
	}
	request.Repository = repository
	if request.BaseRef == "" {
		request.BaseRef = "HEAD"
	}
	if request.Provider == "" {
		return pipeline.Request{}, fmt.Errorf("provider is required")
	}
	return request, nil
}

func normalizeReviewPolicyInput(input reviewPolicyInput) (delivery.ReviewPolicy, error) {
	subjectKind, err := delivery.ParseSubjectKind(input.SubjectKind)
	if err != nil {
		return delivery.ReviewPolicy{}, err
	}
	if len(input.Reviewers) > maxReviewPolicyReviewers {
		return delivery.ReviewPolicy{}, fmt.Errorf(
			"review policy reviewers exceed %d items", maxReviewPolicyReviewers,
		)
	}
	if len(input.BlockingSeverities) > maxReviewPolicyItems ||
		len(input.RequiredCategories) > maxReviewPolicyItems {
		return delivery.ReviewPolicy{}, fmt.Errorf("review policy list exceeds %d items", maxReviewPolicyItems)
	}
	if len(input.RiskRules) > maxReviewRiskRules {
		return delivery.ReviewPolicy{}, fmt.Errorf(
			"review policy risk rules exceed %d items", maxReviewRiskRules,
		)
	}
	reviewers := append([]delivery.ReviewerSpec(nil), input.Reviewers...)
	for index := range reviewers {
		reviewer := &reviewers[index]
		reviewer.ID = strings.ToLower(strings.TrimSpace(reviewer.ID))
		reviewer.Agent.ID = strings.ToLower(strings.TrimSpace(reviewer.Agent.ID))
		reviewer.DefinitionHash = strings.ToLower(strings.TrimSpace(reviewer.DefinitionHash))
		reviewer.Categories, err = normalizeReviewCategories(reviewer.Categories)
		if err != nil {
			return delivery.ReviewPolicy{}, fmt.Errorf("reviewer %q categories: %w", reviewer.ID, err)
		}
	}
	var adjudicator *delivery.AdjudicatorSpec
	if input.Adjudicator != nil {
		prepared := *input.Adjudicator
		prepared.Agent.ID = strings.ToLower(strings.TrimSpace(prepared.Agent.ID))
		prepared.DefinitionHash = strings.ToLower(strings.TrimSpace(prepared.DefinitionHash))
		adjudicator = &prepared
	}
	blocking := make([]delivery.Severity, 0, len(input.BlockingSeverities))
	seenSeverities := make(map[delivery.Severity]struct{}, len(input.BlockingSeverities))
	for _, value := range input.BlockingSeverities {
		severity, parseErr := delivery.ParseSeverity(string(value))
		if parseErr != nil || severity == "" {
			if parseErr != nil {
				return delivery.ReviewPolicy{}, parseErr
			}
			return delivery.ReviewPolicy{}, fmt.Errorf("blocking severity is required")
		}
		if _, duplicate := seenSeverities[severity]; duplicate {
			continue
		}
		seenSeverities[severity] = struct{}{}
		blocking = append(blocking, severity)
	}
	requiredCategories, err := normalizeReviewCategories(input.RequiredCategories)
	if err != nil {
		return delivery.ReviewPolicy{}, fmt.Errorf("required categories: %w", err)
	}
	riskRules := make([]delivery.ReviewRiskRule, len(input.RiskRules))
	for index, rule := range input.RiskRules {
		if len(rule.Conditions) > maxReviewPolicyItems ||
			len(rule.ReviewerIDs) > maxReviewPolicyReviewers {
			return delivery.ReviewPolicy{}, fmt.Errorf(
				"review risk rule %d exceeds list limits", index,
			)
		}
		riskRules[index] = rule
		riskRules[index].ID = strings.ToLower(strings.TrimSpace(rule.ID))
		riskRules[index].Conditions = append(
			[]delivery.ReviewRiskCondition(nil),
			rule.Conditions...,
		)
		for conditionIndex := range riskRules[index].Conditions {
			condition := &riskRules[index].Conditions[conditionIndex]
			condition.Fact = strings.ToLower(strings.TrimSpace(condition.Fact))
			condition.Operator = delivery.ReviewRiskOperator(
				strings.ToLower(strings.TrimSpace(string(condition.Operator))),
			)
		}
		riskRules[index].ReviewerIDs, err = normalizeReviewCategories(rule.ReviewerIDs)
		if err != nil {
			return delivery.ReviewPolicy{}, fmt.Errorf(
				"review risk rule %q reviewers: %w", riskRules[index].ID, err,
			)
		}
	}
	optionalAction := delivery.OptionalReviewerContinue
	if strings.TrimSpace(input.OptionalReviewerAction) != "" {
		optionalAction, err = delivery.ParseOptionalReviewerAction(input.OptionalReviewerAction)
		if err != nil {
			return delivery.ReviewPolicy{}, err
		}
	}
	return delivery.ReviewPolicy{
		ID:                     strings.ToLower(strings.TrimSpace(input.ID)),
		Version:                input.Version,
		SubjectKind:            subjectKind,
		Reviewers:              reviewers,
		Adjudicator:            adjudicator,
		BlockingSeverities:     blocking,
		RequiredCategories:     requiredCategories,
		MaxParallelism:         input.MaxParallelism,
		MaxInputTokens:         input.MaxInputTokens,
		MaxOutputTokens:        input.MaxOutputTokens,
		MaxTotalTokens:         input.MaxTotalTokens,
		MaxToolCalls:           input.MaxToolCalls,
		MaxCostMicros:          input.MaxCostMicros,
		MaxRetries:             input.MaxRetries,
		Timeout:                input.Timeout,
		OptionalReviewerAction: optionalAction,
		RiskRuleVersion:        strings.ToLower(strings.TrimSpace(input.RiskRuleVersion)),
		RiskRules:              riskRules,
	}, nil
}

func normalizeReviewCategories(values []string) ([]string, error) {
	if len(values) > maxReviewPolicyItems {
		return nil, fmt.Errorf("list exceeds %d items", maxReviewPolicyItems)
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func normalizeReviewReportReuseRequests(
	requests []delivery.ReviewReportReuseRequest,
) ([]delivery.ReviewReportReuseRequest, error) {
	if len(requests) > maxReviewPolicyReviewers {
		return nil, fmt.Errorf(
			"reuse_reports exceeds %d items",
			maxReviewPolicyReviewers,
		)
	}
	prepared := make([]delivery.ReviewReportReuseRequest, len(requests))
	for index, request := range requests {
		request.ReviewerID = strings.ToLower(strings.TrimSpace(request.ReviewerID))
		request.SourceReportID = strings.ToLower(strings.TrimSpace(request.SourceReportID))
		request.ReportHash = strings.ToLower(strings.TrimSpace(request.ReportHash))
		request.Reason = strings.TrimSpace(request.Reason)
		if len(request.Reason) > maxReviewReuseReasonBytes {
			return nil, fmt.Errorf(
				"reuse_reports[%d].reason exceeds %d bytes",
				index,
				maxReviewReuseReasonBytes,
			)
		}
		prepared[index] = request
	}
	return prepared, nil
}

func normalizeFeatureInput(title string, requirement delivery.RequirementDocument) (string, delivery.RequirementDocument, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", requirement, fmt.Errorf("title is required")
	}
	normalized, err := normalizeRequirement(requirement)
	return title, normalized, err
}

func normalizeRequirement(value delivery.RequirementDocument) (delivery.RequirementDocument, error) {
	value.Description = strings.TrimSpace(value.Description)
	if value.Description == "" {
		return value, fmt.Errorf("requirement description is required")
	}
	var err error
	for _, target := range []struct {
		name   string
		values *[]string
	}{
		{"business_constraints", &value.BusinessConstraints},
		{"attachments", &value.Attachments},
		{"acceptance_criteria", &value.AcceptanceCriteria},
	} {
		*target.values, err = normalizeTextList(target.name, *target.values)
		if err != nil {
			return value, err
		}
	}
	return value, nil
}

func normalizeTextList(name string, values []string) ([]string, error) {
	if len(values) > maxRequirementListItems {
		return nil, fmt.Errorf("%s exceeds %d items", name, maxRequirementListItems)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > maxRequirementItemBytes {
			return nil, fmt.Errorf("%s item exceeds %d bytes", name, maxRequirementItemBytes)
		}
		out = append(out, value)
	}
	return out, nil
}

func normalizeImplementationOptions(options *delivery.ImplementationOptions) error {
	options.ClientRequestID = strings.TrimSpace(options.ClientRequestID)
	options.DesignArtifactID = strings.TrimSpace(options.DesignArtifactID)
	options.PlanArtifactID = strings.TrimSpace(options.PlanArtifactID)
	options.ParentRunID = strings.TrimSpace(options.ParentRunID)
	options.BaseRef = strings.TrimSpace(options.BaseRef)
	if options.BaseRef == "" {
		options.BaseRef = "HEAD"
	}
	options.Provider = strings.ToLower(strings.TrimSpace(options.Provider))
	options.Model = strings.TrimSpace(options.Model)
	repository, err := delivery.NormalizeRepository(options.Repository)
	if err != nil {
		return err
	}
	options.Repository = repository
	return nil
}

func requestLimit(r *http.Request, defaultLimit, maxLimit int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	return limit, nil
}

func reviewEventAfterSeq(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return seq, nil
}

type featureCursorPayload struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

type runCursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type artifactCursorPayload struct {
	Kind    delivery.ArtifactKind `json:"kind"`
	Version int                   `json:"version"`
}

type findingCursorPayload struct {
	ID string `json:"id"`
}

type findingResolutionCursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

type reviewAdjudicationCursorPayload struct {
	Fingerprint string `json:"fingerprint"`
	ID          string `json:"id"`
}

type reviewPolicyCursorPayload struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type reviewRoundCursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func decodeFeatureCursor(value string) (delivery.FeatureCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.FeatureCursor{}, nil
	}
	var payload featureCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.UpdatedAt.IsZero() || payload.ID == "" {
		return delivery.FeatureCursor{}, fmt.Errorf("invalid feature cursor")
	}
	return delivery.FeatureCursor{UpdatedAt: payload.UpdatedAt, ID: payload.ID}, nil
}

func decodeRunCursor(value string) (delivery.RunCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.RunCursor{}, nil
	}
	var payload runCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.CreatedAt.IsZero() || payload.ID == "" {
		return delivery.RunCursor{}, fmt.Errorf("invalid implementation cursor")
	}
	return delivery.RunCursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}

func decodeArtifactCursor(value string) (delivery.ArtifactCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.ArtifactCursor{}, nil
	}
	var payload artifactCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.Version < 1 {
		return delivery.ArtifactCursor{}, fmt.Errorf("invalid artifact cursor")
	}
	if _, err := delivery.ParseArtifactKind(string(payload.Kind)); err != nil {
		return delivery.ArtifactCursor{}, fmt.Errorf("invalid artifact cursor")
	}
	return delivery.ArtifactCursor{Kind: payload.Kind, Version: payload.Version}, nil
}

func decodeGenerationCursor(value string) (delivery.GenerationCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.GenerationCursor{}, nil
	}
	var payload runCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.CreatedAt.IsZero() || payload.ID == "" {
		return delivery.GenerationCursor{}, fmt.Errorf("invalid generation cursor")
	}
	return delivery.GenerationCursor{StartedAt: payload.CreatedAt, ID: payload.ID}, nil
}

func decodeReviewAssignmentCursor(value string) (delivery.ReviewAssignmentCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.ReviewAssignmentCursor{}, nil
	}
	var payload runCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.CreatedAt.IsZero() || payload.ID == "" {
		return delivery.ReviewAssignmentCursor{}, fmt.Errorf("invalid review assignment cursor")
	}
	return delivery.ReviewAssignmentCursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}

func decodeFindingCursor(value string) (delivery.FindingCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.FindingCursor{}, nil
	}
	var payload findingCursorPayload
	if err := decodeCursor(value, &payload); err != nil || payload.ID == "" {
		return delivery.FindingCursor{}, fmt.Errorf("invalid review finding cursor")
	}
	return delivery.FindingCursor{ID: payload.ID}, nil
}

func decodeFindingResolutionCursor(value string) (delivery.FindingResolutionCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.FindingResolutionCursor{}, nil
	}
	var payload findingResolutionCursorPayload
	if err := decodeCursor(value, &payload); err != nil ||
		payload.CreatedAt.IsZero() || payload.ID == "" {
		return delivery.FindingResolutionCursor{}, fmt.Errorf("invalid finding resolution cursor")
	}
	return delivery.FindingResolutionCursor{
		CreatedAt: payload.CreatedAt,
		ID:        payload.ID,
	}, nil
}

func decodeReviewAdjudicationCursor(value string) (delivery.ReviewAdjudicationCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.ReviewAdjudicationCursor{}, nil
	}
	var payload reviewAdjudicationCursorPayload
	if err := decodeCursor(value, &payload); err != nil ||
		payload.Fingerprint == "" || payload.ID == "" {
		return delivery.ReviewAdjudicationCursor{}, fmt.Errorf("invalid review adjudication cursor")
	}
	return delivery.ReviewAdjudicationCursor{
		Fingerprint: payload.Fingerprint,
		ID:          payload.ID,
	}, nil
}

func decodeReviewPolicyCursor(value string) (delivery.ReviewPolicyCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.ReviewPolicyCursor{}, nil
	}
	var payload reviewPolicyCursorPayload
	if err := decodeCursor(value, &payload); err != nil ||
		payload.ID == "" || payload.Version <= 0 {
		return delivery.ReviewPolicyCursor{}, fmt.Errorf("invalid review policy cursor")
	}
	return delivery.ReviewPolicyCursor{ID: payload.ID, Version: payload.Version}, nil
}

func decodeReviewRoundCursor(value string) (delivery.ReviewRoundCursor, error) {
	if strings.TrimSpace(value) == "" {
		return delivery.ReviewRoundCursor{}, nil
	}
	var payload reviewRoundCursorPayload
	if err := decodeCursor(value, &payload); err != nil ||
		payload.CreatedAt.IsZero() || payload.ID == "" {
		return delivery.ReviewRoundCursor{}, fmt.Errorf("invalid review round cursor")
	}
	return delivery.ReviewRoundCursor{
		CreatedAt: payload.CreatedAt, ID: payload.ID,
	}, nil
}

func decodeCursor(value string, payload any) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, payload)
}

func nextFeatureCursor(items []delivery.FeatureRequest, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(featureCursorPayload{UpdatedAt: last.UpdatedAt, ID: last.ID})
}

func nextRunCursor(items []delivery.ImplementationRun, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(runCursorPayload{CreatedAt: last.CreatedAt, ID: last.ID})
}

func nextArtifactCursor(items []delivery.ArtifactSummary, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(artifactCursorPayload{Kind: last.Kind, Version: last.Version})
}

func nextGenerationCursor(items []delivery.GenerationRun, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(runCursorPayload{CreatedAt: last.StartedAt, ID: last.ID})
}

func nextReviewAssignmentCursor(items []delivery.ReviewAssignment, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(runCursorPayload{CreatedAt: last.CreatedAt, ID: last.ID})
}

func nextFindingCursor(items []delivery.FindingSummary, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	return encodeCursor(findingCursorPayload{ID: items[len(items)-1].ID})
}

func nextFindingResolutionCursor(items []delivery.FindingResolution, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(findingResolutionCursorPayload{
		CreatedAt: last.CreatedAt,
		ID:        last.ID,
	})
}

func nextReviewAdjudicationCursor(items []delivery.ReviewAdjudication, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(reviewAdjudicationCursorPayload{
		Fingerprint: last.Fingerprint,
		ID:          last.ID,
	})
}

func nextReviewPolicyCursor(items []delivery.ReviewPolicyRecord, limit int) string {
	if len(items) != limit || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(reviewPolicyCursorPayload{ID: last.ID, Version: last.Version})
}

func nextReviewRoundCursor(items []delivery.ReviewRoundSummary, hasMore bool) string {
	if !hasMore || len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return encodeCursor(reviewRoundCursorPayload{CreatedAt: last.CreatedAt, ID: last.ID})
}

func encodeCursor(payload any) string {
	raw, _ := json.Marshal(payload)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func reviewPolicyPath(r *http.Request) (string, int64, error) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("policy_id")))
	version, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("version")), 10, 64)
	if id == "" || err != nil || version <= 0 {
		return "", 0, fmt.Errorf("policy_id and positive version are required")
	}
	return id, version, nil
}

func parseAfterSequence(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return sequence, nil
}

func validReviewRoundStatus(status delivery.ReviewRoundStatus) bool {
	switch status {
	case delivery.RoundCreated, delivery.RoundRunning,
		delivery.RoundEvaluating, delivery.RoundCompleted,
		delivery.RoundFailed, delivery.RoundCancelled:
		return true
	default:
		return false
	}
}
