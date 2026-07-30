ALTER TABLE agent_runs
    ADD COLUMN evidence_status VARCHAR(16) NOT NULL DEFAULT 'unavailable' AFTER peak_reserved_tokens,
    ADD COLUMN forced_conclusion BOOLEAN NOT NULL DEFAULT FALSE AFTER evidence_status,
    ADD COLUMN evidence_result_count INT NOT NULL DEFAULT 0 AFTER forced_conclusion,
    ADD COLUMN tool_call_count INT NOT NULL DEFAULT 0 AFTER evidence_result_count,
    ADD COLUMN tool_failure_count INT NOT NULL DEFAULT 0 AFTER tool_call_count,
    ADD COLUMN partial_result_count INT NOT NULL DEFAULT 0 AFTER tool_failure_count,
    ADD COLUMN omitted_evidence_count INT NOT NULL DEFAULT 0 AFTER partial_result_count;
