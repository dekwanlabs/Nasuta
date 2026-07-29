ALTER TABLE feature_artifacts
    ADD INDEX idx_request_kind_parent_version (request_id, kind, parent_artifact_id, version);
