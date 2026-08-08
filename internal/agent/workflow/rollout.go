package workflow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const rolloutBucketCount = 10000

// DefinitionSelection records how a Workflow Definition was selected.
type DefinitionSelection = agentapi.DefinitionSelection

// RolloutRule selects one active candidate version for a stable population.
type RolloutRule struct {
	WorkflowID       string    `json:"workflow_id"`
	RuleVersion      int64     `json:"rule_version"`
	CandidateVersion int64     `json:"candidate_version"`
	PercentageBPS    int       `json:"percentage_bps"`
	Salt             string    `json:"salt"`
	RuleHash         string    `json:"rule_hash"`
	Active           bool      `json:"active"`
	CreatedBy        int64     `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
}

// RolloutAuditEvent records control-plane changes to one rollout rule.
type RolloutAuditEvent struct {
	Seq              int64     `json:"seq"`
	WorkflowID       string    `json:"workflow_id"`
	RuleVersion      int64     `json:"rule_version"`
	CandidateVersion int64     `json:"candidate_version"`
	PercentageBPS    int       `json:"percentage_bps"`
	RuleHash         string    `json:"rule_hash"`
	Action           string    `json:"action"`
	ActorUserID      int64     `json:"actor_user_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// StableSelectionKey keeps one actor and scenario in the same bucket.
func StableSelectionKey(actor agentapi.Actor, scenario string) string {
	scenario = strings.TrimSpace(scenario)
	actorKey := ""
	if actor.UserID > 0 {
		actorKey = fmt.Sprintf("user:%d", actor.UserID)
	} else if tenantID := strings.TrimSpace(actor.TenantID); tenantID != "" {
		actorKey = "tenant:" + tenantID
	}
	switch {
	case scenario != "" && actorKey != "":
		return "scenario:" + scenario + "\x00" + actorKey
	case scenario != "":
		return "scenario:" + scenario
	default:
		return actorKey
	}
}

func prepareRolloutRule(rule RolloutRule) (RolloutRule, error) {
	rule.WorkflowID = strings.TrimSpace(rule.WorkflowID)
	rule.Salt = strings.TrimSpace(rule.Salt)
	if rule.WorkflowID == "" {
		return RolloutRule{}, fmt.Errorf("workflow rollout workflow_id is required: %w", ErrInvalid)
	}
	if rule.RuleVersion <= 0 {
		return RolloutRule{}, fmt.Errorf("workflow rollout rule_version must be positive: %w", ErrInvalid)
	}
	if rule.CandidateVersion <= 0 {
		return RolloutRule{}, fmt.Errorf("workflow rollout candidate_version must be positive: %w", ErrInvalid)
	}
	if rule.PercentageBPS < 0 || rule.PercentageBPS > rolloutBucketCount {
		return RolloutRule{}, fmt.Errorf(
			"workflow rollout percentage_bps must be between 0 and %d: %w",
			rolloutBucketCount, ErrInvalid,
		)
	}
	if rule.Salt == "" {
		return RolloutRule{}, fmt.Errorf("workflow rollout salt is required: %w", ErrInvalid)
	}
	ruleHash, err := rolloutRuleHash(rule)
	if err != nil {
		return RolloutRule{}, err
	}
	if rule.RuleHash != "" && rule.RuleHash != ruleHash {
		return RolloutRule{}, fmt.Errorf("workflow rollout rule_hash does not match rule: %w", ErrInvalid)
	}
	rule.RuleHash = ruleHash
	return rule, nil
}

func rolloutRuleHash(rule RolloutRule) (string, error) {
	rule.RuleHash = ""
	rule.CreatedBy = 0
	rule.CreatedAt = time.Time{}
	payload, err := json.Marshal(rule)
	if err != nil {
		return "", fmt.Errorf("marshal workflow rollout rule %q: %w", rule.WorkflowID, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func selectRollout(
	rule RolloutRule,
	stableKey string,
) (DefinitionSelection, bool, error) {
	if strings.TrimSpace(stableKey) == "" {
		return DefinitionSelection{}, false, fmt.Errorf(
			"workflow rollout for %q requires a stable selection key: %w",
			rule.WorkflowID, ErrInvalid,
		)
	}
	stableKeyHash := sha256.Sum256([]byte(stableKey))
	input := rule.Salt + "\x00" + rule.WorkflowID + "\x00" + stableKey
	digest := sha256.Sum256([]byte(input))
	bucket := int(binary.BigEndian.Uint64(digest[:8]) % rolloutBucketCount)
	selection := DefinitionSelection{
		RuleVersion:           rule.RuleVersion,
		RuleHash:              rule.RuleHash,
		CandidateVersion:      rule.CandidateVersion,
		BucketBasisPoints:     bucket,
		PercentageBasisPoints: rule.PercentageBPS,
		StableKeyHash:         hex.EncodeToString(stableKeyHash[:]),
		Reason:                "rollout_default",
	}
	if bucket < rule.PercentageBPS {
		selection.Reason = "rollout_candidate"
		return selection, true, nil
	}
	return selection, false, nil
}
