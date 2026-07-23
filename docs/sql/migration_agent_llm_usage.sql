ALTER TABLE agent_runs
    ADD COLUMN input_tokens BIGINT NOT NULL DEFAULT 0 AFTER token_used,
    ADD COLUMN cached_input_tokens BIGINT NOT NULL DEFAULT 0 AFTER input_tokens,
    ADD COLUMN output_tokens BIGINT NOT NULL DEFAULT 0 AFTER cached_input_tokens,
    ADD COLUMN reasoning_tokens BIGINT NOT NULL DEFAULT 0 AFTER output_tokens,
    ADD COLUMN total_tokens BIGINT NOT NULL DEFAULT 0 AFTER reasoning_tokens,
    ADD COLUMN llm_call_count INT NOT NULL DEFAULT 0 AFTER total_tokens,
    ADD COLUMN peak_input_tokens INT NOT NULL DEFAULT 0 AFTER llm_call_count,
    ADD COLUMN peak_reserved_tokens INT NOT NULL DEFAULT 0 AFTER peak_input_tokens;

CREATE TABLE agent_llm_calls (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id              VARCHAR(64) NOT NULL,
    call_seq            INT NOT NULL,
    phase               VARCHAR(32) NOT NULL,
    provider            VARCHAR(32) NOT NULL,
    model               VARCHAR(128) NOT NULL,
    input_tokens        INT NOT NULL DEFAULT 0,
    cached_input_tokens INT NOT NULL DEFAULT 0,
    output_tokens       INT NOT NULL DEFAULT 0,
    reasoning_tokens    INT NOT NULL DEFAULT 0,
    total_tokens        INT NOT NULL DEFAULT 0,
    max_output_tokens   INT NOT NULL DEFAULT 0,
    duration_ms         INT NOT NULL DEFAULT 0,
    status              VARCHAR(16) NOT NULL,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_run_call (run_id, call_seq),
    KEY idx_run (run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
