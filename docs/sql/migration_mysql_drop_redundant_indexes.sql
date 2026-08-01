-- Drop indexes covered by an existing unique/primary index or by the
-- replacement composite indexes added in migration_mysql_add_missing_indexes.sql.

-- Covered by uniq_session_seq(session_id, seq).
ALTER TABLE qa_messages
    DROP INDEX idx_session;

-- Covered by PRIMARY KEY(session_id, turn_no), including reverse scans.
ALTER TABLE qa_turns
    DROP INDEX idx_session_last;

-- Covered by idx_user_updated_id(user_id, updated_at, id).
ALTER TABLE qa_sessions
    DROP INDEX idx_user;

-- idx_user and idx_status are covered by the new composite indexes.
ALTER TABLE agent_runs
    DROP INDEX idx_user,
    DROP INDEX idx_status;

-- Covered by uniq_run_step(run_id, step_no).
ALTER TABLE agent_steps
    DROP INDEX idx_run;

-- Covered by uniq_run_tool_call(run_id, tool_call_id).
ALTER TABLE agent_tool_result_artifacts
    DROP INDEX idx_run;

-- Covered by uniq_run_call(run_id, call_seq).
ALTER TABLE agent_llm_calls
    DROP INDEX idx_run;

-- Covered by idx_status_created_id(status, created_at, id).
ALTER TABLE pending_actions
    DROP INDEX idx_status;
