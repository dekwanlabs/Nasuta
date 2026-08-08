package delivery

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const reviewPolicyRolloutBucketCount = 10000

// StableReviewPolicySelectionKey keeps one user in one Policy bucket per subject kind.
func StableReviewPolicySelectionKey(userID int64, kind SubjectKind, subjectID string) string {
	kind = SubjectKind(strings.TrimSpace(string(kind)))
	subjectID = strings.TrimSpace(subjectID)
	if userID > 0 {
		return fmt.Sprintf("subject:%s\x00user:%d", kind, userID)
	}
	if subjectID != "" {
		return "subject:" + string(kind) + "\x00id:" + subjectID
	}
	return ""
}

func prepareReviewPolicyRolloutRule(rule ReviewPolicyRolloutRule) (ReviewPolicyRolloutRule, error) {
	rule.SubjectKind = SubjectKind(strings.TrimSpace(string(rule.SubjectKind)))
	rule.CandidatePolicyID = strings.TrimSpace(rule.CandidatePolicyID)
	rule.Salt = strings.TrimSpace(rule.Salt)
	if !validSubjectKind(rule.SubjectKind) {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout subject kind %q is invalid: %w",
			rule.SubjectKind, ErrInvalid,
		)
	}
	if rule.RuleVersion <= 0 {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout rule_version must be positive: %w", ErrInvalid,
		)
	}
	if rule.CandidatePolicyID == "" || rule.CandidatePolicyVersion <= 0 {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout candidate policy is required: %w", ErrInvalid,
		)
	}
	if rule.PercentageBPS < 0 || rule.PercentageBPS > reviewPolicyRolloutBucketCount {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout percentage_bps must be between 0 and %d: %w",
			reviewPolicyRolloutBucketCount, ErrInvalid,
		)
	}
	if rule.Salt == "" {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout salt is required: %w", ErrInvalid,
		)
	}
	hash, err := reviewPolicyRolloutRuleHash(rule)
	if err != nil {
		return ReviewPolicyRolloutRule{}, err
	}
	if rule.RuleHash != "" && rule.RuleHash != hash {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy rollout rule_hash does not match rule: %w", ErrInvalid,
		)
	}
	rule.RuleHash = hash
	return rule, nil
}

func reviewPolicyRolloutRuleHash(rule ReviewPolicyRolloutRule) (string, error) {
	rule.RuleHash = ""
	rule.CreatedBy = 0
	rule.CreatedAt = time.Time{}
	payload, err := json.Marshal(rule)
	if err != nil {
		return "", fmt.Errorf(
			"marshal review policy rollout for %q: %w",
			rule.SubjectKind, err,
		)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func selectReviewPolicyRollout(
	rule ReviewPolicyRolloutRule,
	stableKey string,
) (ReviewPolicySelection, bool, error) {
	if strings.TrimSpace(stableKey) == "" {
		return ReviewPolicySelection{}, false, fmt.Errorf(
			"review policy rollout for %q requires a stable selection key: %w",
			rule.SubjectKind, ErrInvalid,
		)
	}
	stableKeyHash := sha256.Sum256([]byte(stableKey))
	input := rule.Salt + "\x00" + string(rule.SubjectKind) + "\x00" + stableKey
	digest := sha256.Sum256([]byte(input))
	bucket := int(binary.BigEndian.Uint64(digest[:8]) % reviewPolicyRolloutBucketCount)
	selection := ReviewPolicySelection{
		RuleVersion:            rule.RuleVersion,
		RuleHash:               rule.RuleHash,
		CandidatePolicyID:      rule.CandidatePolicyID,
		CandidatePolicyVersion: rule.CandidatePolicyVersion,
		BucketBasisPoints:      bucket,
		PercentageBasisPoints:  rule.PercentageBPS,
		StableKeyHash:          hex.EncodeToString(stableKeyHash[:]),
		Reason:                 "rollout_default",
	}
	if bucket < rule.PercentageBPS {
		selection.Reason = "rollout_candidate"
		return selection, true, nil
	}
	return selection, false, nil
}
