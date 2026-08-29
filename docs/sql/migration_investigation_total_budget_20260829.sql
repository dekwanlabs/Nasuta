-- Investigation Run token-budget allocation defaults (2026-08-29)
--
-- Run budget:
--   input  = 300000
--   output = 128000
--   total  = 512000
-- The interactive/deep default profile allocates 10% each to Verification and
-- Composition, so one role pool receives 30000 input, 12800 output, and 51200
-- total tokens. Existing operator-customized values are preserved.

UPDATE settings
SET v = '300000', updated_at = CURRENT_TIMESTAMP
WHERE k = 'investigation_max_input_tokens' AND v = '20000';

UPDATE settings
SET v = '128000', updated_at = CURRENT_TIMESTAMP
WHERE k = 'investigation_max_output_tokens' AND v IN ('8000', '16000');

INSERT INTO settings (k, v, updated_at)
SELECT 'investigation_max_input_tokens', '300000', CURRENT_TIMESTAMP
WHERE NOT EXISTS (
    SELECT 1 FROM settings WHERE k = 'investigation_max_input_tokens'
);

INSERT INTO settings (k, v, updated_at)
SELECT 'investigation_max_output_tokens', '128000', CURRENT_TIMESTAMP
WHERE NOT EXISTS (
    SELECT 1 FROM settings WHERE k = 'investigation_max_output_tokens'
);

INSERT INTO settings (k, v, updated_at)
SELECT 'investigation_max_total_tokens', '512000', CURRENT_TIMESTAMP
WHERE NOT EXISTS (
    SELECT 1 FROM settings WHERE k = 'investigation_max_total_tokens'
);
