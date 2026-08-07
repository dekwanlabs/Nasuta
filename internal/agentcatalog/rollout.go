package agentcatalog

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

// RolloutRule selects one active candidate version for a stable population.
type RolloutRule struct {
	AgentID          string    `json:"agent_id"`
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
	AgentID          string    `json:"agent_id"`
	RuleVersion      int64     `json:"rule_version"`
	CandidateVersion int64     `json:"candidate_version"`
	PercentageBPS    int       `json:"percentage_bps"`
	RuleHash         string    `json:"rule_hash"`
	Action           string    `json:"action"`
	ActorUserID      int64     `json:"actor_user_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// StableSelectionKey keeps a user in one bucket across runs and sessions.
func StableSelectionKey(userID int64, sessionID string) string {
	if userID > 0 {
		return fmt.Sprintf("user:%d", userID)
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		return "session:" + sessionID
	}
	return ""
}

func prepareRolloutRule(rule RolloutRule) (RolloutRule, error) {
	rule.AgentID = strings.TrimSpace(rule.AgentID)
	rule.Salt = strings.TrimSpace(rule.Salt)
	if rule.AgentID == "" {
		return RolloutRule{}, fmt.Errorf("agent rollout agent_id is required: %w", ErrInvalid)
	}
	if rule.RuleVersion <= 0 {
		return RolloutRule{}, fmt.Errorf("agent rollout rule_version must be positive: %w", ErrInvalid)
	}
	if rule.CandidateVersion <= 0 {
		return RolloutRule{}, fmt.Errorf("agent rollout candidate_version must be positive: %w", ErrInvalid)
	}
	if rule.PercentageBPS < 0 || rule.PercentageBPS > rolloutBucketCount {
		return RolloutRule{}, fmt.Errorf(
			"agent rollout percentage_bps must be between 0 and %d: %w",
			rolloutBucketCount, ErrInvalid,
		)
	}
	if rule.Salt == "" {
		return RolloutRule{}, fmt.Errorf("agent rollout salt is required: %w", ErrInvalid)
	}
	ruleHash, err := rolloutRuleHash(rule)
	if err != nil {
		return RolloutRule{}, err
	}
	if rule.RuleHash != "" && rule.RuleHash != ruleHash {
		return RolloutRule{}, fmt.Errorf("agent rollout rule_hash does not match rule: %w", ErrInvalid)
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
		return "", fmt.Errorf("marshal agent rollout rule %q: %w", rule.AgentID, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func selectRollout(rule RolloutRule, stableKey string) (agentapi.DefinitionSelection, bool, error) {
	if strings.TrimSpace(stableKey) == "" {
		return agentapi.DefinitionSelection{}, false, fmt.Errorf(
			"agent rollout for %q requires a stable selection key: %w",
			rule.AgentID, ErrInvalid,
		)
	}
	stableKeyHash := sha256.Sum256([]byte(stableKey))
	input := rule.Salt + "\x00" + rule.AgentID + "\x00" + stableKey
	digest := sha256.Sum256([]byte(input))
	bucket := int(binary.BigEndian.Uint64(digest[:8]) % rolloutBucketCount)
	selection := agentapi.DefinitionSelection{
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
