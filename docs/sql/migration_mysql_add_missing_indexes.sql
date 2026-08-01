-- Add indexes that match the current MySQL query paths.
-- Run this migration before migration_mysql_drop_redundant_indexes.sql.

ALTER TABLE documents
    ADD KEY idx_kind_updated_id (kind, updated_at DESC, id DESC),
    ADD KEY idx_updated_id (updated_at DESC, id DESC);

ALTER TABLE qa_sessions
    ADD KEY idx_user_updated_id (user_id, updated_at DESC, id DESC);

ALTER TABLE agent_runs
    ADD KEY idx_user_session_started_id (user_id, session_id, started_at DESC, id DESC),
    ADD KEY idx_status_started_id (status, started_at DESC, id DESC);

ALTER TABLE qa_session_history_index_outbox
    ADD KEY idx_operation_user_session (operation, user_id, session_id);

ALTER TABLE qa_memories
    ADD KEY idx_user_source_session (user_id, source_session);

ALTER TABLE incident_records
    ADD KEY idx_created_at_id (created_at DESC, id DESC);

ALTER TABLE pending_actions
    ADD KEY idx_status_created_id (status, created_at DESC, id DESC),
    ADD KEY idx_created_id (created_at DESC, id DESC);

ALTER TABLE feature_implementation_runs
    ADD KEY idx_cleanup_due (worktree_cleaned_at, retain_until, id);

-- The table is normally small; keep this index only if per-user key lists are frequent.
ALTER TABLE rbac_mcp_keys
    ADD KEY idx_user_created_id (user_id, created_at DESC, id DESC);
