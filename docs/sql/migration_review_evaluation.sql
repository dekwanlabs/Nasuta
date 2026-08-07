CREATE TABLE IF NOT EXISTS review_evaluation_labels (
    seq            BIGINT NOT NULL AUTO_INCREMENT,
    id             VARCHAR(64) NOT NULL,
    round_id       VARCHAR(64) NOT NULL,
    policy_id      VARCHAR(128) NOT NULL,
    policy_version BIGINT NOT NULL,
    subject_hash   CHAR(64) NOT NULL,
    finding_id     VARCHAR(64) NOT NULL DEFAULT '',
    target_hash    CHAR(64) NOT NULL,
    category       VARCHAR(128) NOT NULL,
    label          VARCHAR(24) NOT NULL,
    created_by     BIGINT NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (seq),
    UNIQUE KEY uniq_review_evaluation_id (id),
    UNIQUE KEY uniq_review_evaluation_target (round_id, target_hash),
    KEY idx_review_evaluation_policy (policy_id, policy_version, created_at, seq),
    KEY idx_review_evaluation_round (round_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
