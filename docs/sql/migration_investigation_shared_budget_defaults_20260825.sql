-- Investigation Run shared-budget defaults (2026-08-25)
--
-- Update only values that still equal the previous built-in defaults. A value
-- changed by an operator is preserved and must be reviewed separately.
-- Run once after deploying the shared-ledger implementation.

UPDATE settings
SET v = '1000000'
WHERE k = 'llm_context_window' AND v = '128000';

UPDATE settings
SET v = '300000'
WHERE k = 'investigation_max_input_tokens' AND v = '20000';

UPDATE settings
SET v = '16000'
WHERE k = 'investigation_max_output_tokens' AND v = '8000';

UPDATE settings
SET v = '48'
WHERE k = 'investigation_max_tool_calls' AND v = '24';

UPDATE settings
SET v = '10m'
WHERE k = 'investigation_max_duration' AND v = '5m';

UPDATE settings
SET v = '6'
WHERE k = 'investigation_max_rounds' AND v = '4';

UPDATE settings
SET v = '32'
WHERE k = 'investigation_max_tasks' AND v = '24';

-- investigation_max_parallelism remains 4 and investigation_max_cost_micros
-- remains 0, so no row update is required for those settings.
