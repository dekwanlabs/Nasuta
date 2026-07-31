ALTER TABLE agent_steps
    DROP COLUMN result_summary,
    ADD COLUMN trace_id VARCHAR(64) NOT NULL DEFAULT '' AFTER kind,
    ADD COLUMN artifact_id VARCHAR(64) NOT NULL DEFAULT '' AFTER trace_id,
    ADD COLUMN tool_call_id VARCHAR(128) NOT NULL DEFAULT '' AFTER artifact_id,
    ADD COLUMN prompt_content MEDIUMTEXT NULL AFTER content,
    ADD COLUMN authoritative_sha256 CHAR(64) NOT NULL DEFAULT '' AFTER prompt_content,
    ADD COLUMN prompt_sha256 CHAR(64) NOT NULL DEFAULT '' AFTER authoritative_sha256,
    ADD COLUMN content_bytes BIGINT NOT NULL DEFAULT 0 AFTER prompt_sha256,
    ADD COLUMN coverage_json JSON NULL AFTER content_bytes,
    ADD COLUMN answer_contract_json JSON NULL AFTER coverage_json,
    ADD COLUMN failed BOOLEAN NOT NULL DEFAULT FALSE AFTER answer_contract_json,
    ADD COLUMN delivery_error VARCHAR(128) NOT NULL DEFAULT '' AFTER failed,
    ADD KEY idx_trace (trace_id),
    ADD KEY idx_artifact (artifact_id);

UPDATE agent_steps
SET prompt_content = COALESCE(content, ''),
    authoritative_sha256 = SHA2(COALESCE(content, ''), 256),
    prompt_sha256 = SHA2(COALESCE(content, ''), 256),
    content_bytes = OCTET_LENGTH(COALESCE(content, '')),
    coverage_json = JSON_OBJECT(),
    answer_contract_json = JSON_OBJECT()
WHERE trace_id = '';

ALTER TABLE agent_steps
    MODIFY COLUMN coverage_json JSON NOT NULL,
    MODIFY COLUMN answer_contract_json JSON NOT NULL;

CREATE TABLE agent_tool_result_artifacts (
    id            VARCHAR(64) PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    session_id    VARCHAR(64) NOT NULL,
    run_id        VARCHAR(64) NOT NULL,
    tool_call_id  VARCHAR(128) NOT NULL,
    content       LONGBLOB NOT NULL,
    content_type  VARCHAR(128) NOT NULL,
    sha256        CHAR(64) NOT NULL,
    size_bytes    BIGINT NOT NULL,
    coverage_json JSON NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_run_tool_call (run_id, tool_call_id),
    KEY idx_user_session (user_id, session_id),
    KEY idx_run (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
