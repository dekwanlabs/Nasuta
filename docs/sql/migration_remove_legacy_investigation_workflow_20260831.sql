-- Remove the retired durable Investigation workflow (2026-08-31).
--
-- Stop Nasuta and CodeLoom before running this migration. The current QA path
-- runs a normal agent Run and uses delegate_investigation for bounded child
-- Runs; it no longer reads or writes the durable investigation_* tables.
-- This migration is idempotent and intentionally removes the obsolete
-- investigation settings rows as well as the tables.

DROP TABLE IF EXISTS investigation_leases;
DROP TABLE IF EXISTS investigation_events;
DROP TABLE IF EXISTS investigation_runs;
DROP TABLE IF EXISTS workflow_escalations;

-- Remove the superseded QA-to-Workflow escalation idempotency table too.
-- Feature Delivery continues to use the generic workflow tables directly; QA
-- no longer promotes a parent Run into a Workflow.

DELETE FROM settings
WHERE k IN (
    'investigation_max_input_tokens',
    'investigation_max_output_tokens',
    'investigation_max_total_tokens',
    'investigation_max_tool_calls',
    'investigation_max_duration',
    'investigation_max_rounds',
    'investigation_max_tasks',
    'investigation_max_parallelism',
    'investigation_max_cost_micros',
    'investigation_budget_profile',
    'investigation_enabled'
);
