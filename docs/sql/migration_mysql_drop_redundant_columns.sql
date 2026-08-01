-- Preconditions:
-- 1. Deploy application code that no longer reads or writes created_unix/updated_unix.
-- 2. Incident lists and dedup lookups must order by created_at instead.
-- 3. Apply migration_mysql_add_missing_indexes.sql first so idx_created_at_id exists.

ALTER TABLE incident_records
    DROP INDEX idx_created,
    DROP COLUMN created_unix,
    DROP COLUMN updated_unix;
