-- Add monotonic fencing tokens for dynamic investigation workers.
-- Run during a maintenance window; the default keeps existing snapshots readable
-- until the application is deployed with token-aware writes.
ALTER TABLE investigation_runs
    ADD COLUMN fencing_token BIGINT NOT NULL DEFAULT 0;

ALTER TABLE investigation_leases
    ADD COLUMN fencing_token BIGINT NOT NULL DEFAULT 0;


-- Startup recovery scans snapshots by a stable keyset cursor.
ALTER TABLE investigation_runs
    ADD INDEX idx_investigation_runs_updated (updated_at, id);
