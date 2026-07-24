ALTER TABLE qa_messages
    ADD COLUMN tool_calls_json MEDIUMTEXT NULL AFTER content,
    ADD COLUMN tool_call_id VARCHAR(128) NOT NULL DEFAULT '' AFTER tool_calls_json,
    ADD COLUMN tool_name VARCHAR(128) NOT NULL DEFAULT '' AFTER tool_call_id;
