ALTER TABLE feature_artifact_reviews
    ADD COLUMN subject_hash CHAR(64) NULL AFTER artifact_id,
    ADD COLUMN review_round_id VARCHAR(64) NULL AFTER subject_hash,
    ADD COLUMN gate_result_id VARCHAR(64) NULL AFTER review_round_id;

ALTER TABLE feature_change_reviews
    ADD COLUMN subject_hash CHAR(64) NULL AFTER run_id,
    ADD COLUMN review_round_id VARCHAR(64) NULL AFTER subject_hash,
    ADD COLUMN gate_result_id VARCHAR(64) NULL AFTER review_round_id;

-- Existing approvals predate Agent review binding and remain historical only.
UPDATE feature_artifact_reviews
SET subject_hash='', review_round_id='', gate_result_id=''
WHERE subject_hash IS NULL OR review_round_id IS NULL OR gate_result_id IS NULL;

UPDATE feature_change_reviews
SET subject_hash='', review_round_id='', gate_result_id=''
WHERE subject_hash IS NULL OR review_round_id IS NULL OR gate_result_id IS NULL;

ALTER TABLE feature_artifact_reviews
    MODIFY COLUMN subject_hash CHAR(64) NOT NULL,
    MODIFY COLUMN review_round_id VARCHAR(64) NOT NULL,
    MODIFY COLUMN gate_result_id VARCHAR(64) NOT NULL;

ALTER TABLE feature_change_reviews
    MODIFY COLUMN subject_hash CHAR(64) NOT NULL,
    MODIFY COLUMN review_round_id VARCHAR(64) NOT NULL,
    MODIFY COLUMN gate_result_id VARCHAR(64) NOT NULL;

CREATE TABLE IF NOT EXISTS review_policies (
    id              VARCHAR(128) NOT NULL,
    version         BIGINT NOT NULL,
    subject_kind    VARCHAR(48) NOT NULL,
    definition_json JSON NOT NULL,
    content_hash    CHAR(64) NOT NULL,
    active          TINYINT(1) NOT NULL DEFAULT 1,
    is_default      TINYINT(1) NOT NULL DEFAULT 0,
    created_by      BIGINT NOT NULL DEFAULT 0,
    default_key     VARCHAR(48) GENERATED ALWAYS AS (IF(is_default=1, subject_kind, NULL)) STORED,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id, version),
    UNIQUE KEY uniq_review_policy_hash (content_hash),
    UNIQUE KEY uniq_review_policy_default (default_key),
    KEY idx_review_policy_rollout (subject_kind, active, is_default, id, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_policy_audit (
    seq           BIGINT NOT NULL AUTO_INCREMENT,
    policy_id     VARCHAR(128) NOT NULL,
    version       BIGINT NOT NULL,
    action        VARCHAR(32) NOT NULL,
    actor_user_id BIGINT NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (seq),
    KEY idx_review_policy_audit (policy_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

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

CREATE TABLE IF NOT EXISTS review_rounds (
    id              VARCHAR(64) PRIMARY KEY,
    workflow_run_id VARCHAR(64) NOT NULL DEFAULT '',
    subject_kind    VARCHAR(48) NOT NULL,
    subject_id      VARCHAR(64) NOT NULL,
    subject_version INT NOT NULL,
    subject_hash    CHAR(64) NOT NULL,
    subject_json    JSON NOT NULL,
    policy_id       VARCHAR(128) NOT NULL,
    policy_version  BIGINT NOT NULL,
    policy_hash     CHAR(64) NOT NULL,
    policy_selection_json JSON NOT NULL,
    risk_facts_json JSON NOT NULL,
    risk_hash       CHAR(64) NOT NULL,
    selection_rule_version VARCHAR(64) NOT NULL DEFAULT '',
    selected_reviewers_json JSON NOT NULL,
    panel_hash      CHAR(64) NOT NULL,
    status          VARCHAR(16) NOT NULL,
    created_by      BIGINT NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    TIMESTAMP NULL,
    KEY idx_review_subject (subject_kind, subject_id, created_at, id),
    KEY idx_review_status (status, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_assignments (
    id              VARCHAR(64) PRIMARY KEY,
    round_id        VARCHAR(64) NOT NULL,
    reviewer_id     VARCHAR(128) NOT NULL,
    agent_id        VARCHAR(128) NOT NULL,
    agent_version   BIGINT NOT NULL,
    definition_hash CHAR(64) NOT NULL,
    categories_json JSON NOT NULL,
    required_review TINYINT(1) NOT NULL,
    status          VARCHAR(16) NOT NULL,
    attempt         INT NOT NULL,
    agent_run_id    VARCHAR(64) NOT NULL DEFAULT '',
    error_code      VARCHAR(64) NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at      TIMESTAMP NULL,
    completed_at    TIMESTAMP NULL,
    UNIQUE KEY uniq_round_reviewer_attempt (round_id, reviewer_id, attempt),
    KEY idx_assignment_round (round_id, created_at, id),
    KEY idx_assignment_status (status, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_round_events (
    round_id    VARCHAR(64) NOT NULL,
    seq         BIGINT NOT NULL,
    kind        VARCHAR(32) NOT NULL,
    summary     TEXT NOT NULL,
    detail_json JSON NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (round_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_reports (
    id            VARCHAR(64) PRIMARY KEY,
    round_id      VARCHAR(64) NOT NULL,
    assignment_id VARCHAR(64) NOT NULL,
    reviewer_id   VARCHAR(128) NOT NULL,
    subject_hash  CHAR(64) NOT NULL,
    report_json   JSON NOT NULL,
    report_hash   CHAR(64) NOT NULL,
    content_hash  CHAR(64) NOT NULL,
    completed_at  TIMESTAMP NOT NULL,
    UNIQUE KEY uniq_review_assignment_report (assignment_id),
    KEY idx_report_round (round_id, completed_at, id),
    KEY idx_report_reuse (report_hash, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_report_reuses (
    id                   VARCHAR(64) PRIMARY KEY,
    round_id             VARCHAR(64) NOT NULL,
    assignment_id        VARCHAR(64) NOT NULL,
    report_id            VARCHAR(64) NOT NULL,
    reviewer_id          VARCHAR(128) NOT NULL,
    source_round_id      VARCHAR(64) NOT NULL,
    source_assignment_id VARCHAR(64) NOT NULL,
    source_report_id     VARCHAR(64) NOT NULL,
    subject_hash         CHAR(64) NOT NULL,
    policy_hash          CHAR(64) NOT NULL,
    definition_hash      CHAR(64) NOT NULL,
    report_hash          CHAR(64) NOT NULL,
    reason               TEXT NOT NULL,
    actor_id             BIGINT NOT NULL,
    created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_review_report_reuse_assignment (assignment_id),
    UNIQUE KEY uniq_review_report_reuse_report (report_id),
    KEY idx_review_report_reuse_source (source_report_id, created_at, id),
    KEY idx_review_report_reuse_round (round_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_findings (
    id             VARCHAR(64) PRIMARY KEY,
    report_id      VARCHAR(64) NOT NULL,
    round_id       VARCHAR(64) NOT NULL,
    category       VARCHAR(128) NOT NULL,
    severity       VARCHAR(16) NOT NULL,
    claim          TEXT NOT NULL,
    impact         TEXT NOT NULL,
    recommendation TEXT NOT NULL,
    confidence     DOUBLE NOT NULL,
    fingerprint    CHAR(64) NOT NULL,
    location_json  JSON NULL,
    content_hash   CHAR(64) NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_finding_round (round_id, id),
    KEY idx_finding_severity (round_id, severity, id),
    KEY idx_finding_fingerprint (fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_finding_evidence (
    finding_id  VARCHAR(64) NOT NULL,
    sequence    INT NOT NULL,
    kind        VARCHAR(64) NOT NULL,
    ref_value   VARCHAR(1024) NOT NULL,
    source_hash VARCHAR(128) NOT NULL,
    summary     TEXT NOT NULL,
    PRIMARY KEY (finding_id, sequence)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_adjudications (
    id                VARCHAR(64) PRIMARY KEY,
    round_id          VARCHAR(64) NOT NULL,
    subject_hash      CHAR(64) NOT NULL,
    policy_hash       CHAR(64) NOT NULL,
    fingerprint       CHAR(64) NOT NULL,
    agent_id          VARCHAR(128) NOT NULL,
    agent_version     BIGINT NOT NULL,
    definition_hash   CHAR(64) NOT NULL,
    decision          VARCHAR(24) NOT NULL,
    error_code        VARCHAR(64) NOT NULL DEFAULT '',
    adjudication_json JSON NOT NULL,
    content_hash      CHAR(64) NOT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_round_adjudication (round_id, fingerprint),
    KEY idx_adjudication_round (round_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS review_gate_results (
    id           VARCHAR(64) PRIMARY KEY,
    round_id     VARCHAR(64) NOT NULL,
    subject_hash CHAR(64) NOT NULL,
    decision     VARCHAR(24) NOT NULL,
    result_json  JSON NOT NULL,
    policy_hash  CHAR(64) NOT NULL,
    content_hash CHAR(64) NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_review_round_gate (round_id),
    KEY idx_gate_subject (subject_hash, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS finding_resolutions (
    id               VARCHAR(64) PRIMARY KEY,
    finding_id       VARCHAR(64) NOT NULL,
    resolution       VARCHAR(24) NOT NULL,
    subject_hash     CHAR(64) NOT NULL,
    replacement_hash CHAR(64) NOT NULL DEFAULT '',
    rationale        TEXT NOT NULL,
    actor_id         BIGINT NOT NULL,
    expires_at       TIMESTAMP NULL,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_resolution_finding (finding_id, subject_hash, created_at, id),
    KEY idx_resolution_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
