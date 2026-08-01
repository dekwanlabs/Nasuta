ALTER TABLE qa_turns
  ADD COLUMN run_id VARCHAR(64) NOT NULL DEFAULT '' AFTER turn_no;

ALTER TABLE qa_turns
  ADD COLUMN run_id_key VARCHAR(64)
    GENERATED ALWAYS AS (NULLIF(run_id, '')) STORED,
  ADD UNIQUE KEY uniq_session_run (session_id, run_id_key);
