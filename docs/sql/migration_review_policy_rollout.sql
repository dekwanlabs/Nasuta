ALTER TABLE review_rounds
    ADD COLUMN policy_selection_json JSON NULL AFTER policy_hash;

UPDATE review_rounds
SET policy_selection_json = JSON_OBJECT()
WHERE policy_selection_json IS NULL;

ALTER TABLE review_rounds
    MODIFY COLUMN policy_selection_json JSON NOT NULL;

CREATE TABLE IF NOT EXISTS review_policy_rollouts (
    subject_kind              VARCHAR(48) NOT NULL,
    rule_version              BIGINT NOT NULL,
    candidate_policy_id       VARCHAR(128) NOT NULL,
    candidate_policy_version  BIGINT NOT NULL,
    percentage_bps            INT NOT NULL,
    salt                      VARCHAR(255) NOT NULL,
    rule_hash                 CHAR(64) NOT NULL,
    active                    TINYINT(1) NOT NULL DEFAULT 1,
    created_by                BIGINT NOT NULL DEFAULT 0,
    created_at                TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subject_kind),
    UNIQUE KEY uniq_review_policy_rollout_hash (rule_hash),
    KEY idx_review_policy_rollout_active (subject_kind, active, rule_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_policy_rollout_audit (
    seq                       BIGINT NOT NULL AUTO_INCREMENT,
    subject_kind              VARCHAR(48) NOT NULL,
    rule_version              BIGINT NOT NULL,
    candidate_policy_id       VARCHAR(128) NOT NULL,
    candidate_policy_version  BIGINT NOT NULL,
    percentage_bps            INT NOT NULL,
    rule_hash                 CHAR(64) NOT NULL,
    action                    VARCHAR(32) NOT NULL,
    actor_user_id             BIGINT NOT NULL DEFAULT 0,
    created_at                TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (seq),
    KEY idx_review_policy_rollout_audit (subject_kind, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
