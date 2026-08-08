package run

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func (rs *RunStore) AddStep(st StepRow) error {
	if st.CreatedAt == "" {
		st.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	coverageJSON, err := json.Marshal(st.Coverage)
	if err != nil {
		return fmt.Errorf("marshal tool result coverage: %w", err)
	}
	contractJSON, err := json.Marshal(st.AnswerContract)
	if err != nil {
		return fmt.Errorf("marshal tool result answer contract: %w", err)
	}
	tx, err := rs.db.Begin()
	if err != nil {
		return fmt.Errorf("begin agent step: %w", err)
	}
	defer tx.Rollback()

	content := any(st.Content)
	var artifactContent any
	var artifactContentType any
	if st.ArtifactID != "" {
		contentType := "text/plain; charset=utf-8"
		if json.Valid([]byte(st.Content)) {
			contentType = "application/json"
		}
		artifactContent = []byte(st.Content)
		artifactContentType = contentType
		content = nil
	}
	_, err = tx.Exec(
		`INSERT INTO agent_steps(
			run_id,step_no,kind,trace_id,artifact_id,tool_call_id,tool,args,content,prompt_content,
			authoritative_sha256,prompt_sha256,content_bytes,coverage_json,answer_contract_json,failed,
			delivery_error,token_delta,reasoning_tokens,duration_ms,artifact_content,
			artifact_content_type,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.RunID, st.StepNo, st.Kind, st.TraceID, st.ArtifactID, st.ToolCallID, st.Tool, st.Args,
		content, st.PromptContent, st.AuthoritativeSHA256, st.PromptSHA256, st.SizeBytes,
		coverageJSON, contractJSON, st.Failed, st.DeliveryError, st.TokenDelta, st.ReasoningTokens,
		st.DurationMs, artifactContent, artifactContentType, store.DatabaseTime(st.CreatedAt))
	if err != nil {
		return fmt.Errorf("persist agent step %d: %w", st.StepNo, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent step %d: %w", st.StepNo, err)
	}
	return nil
}

// GetToolResultArtifact reads one bounded slice after enforcing user/session ownership.
func (rs *RunStore) GetToolResultArtifact(userID int64, sessionID, artifactID string, offset int64, limit int) (*ToolResultArtifactChunk, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact id is required")
	}
	if offset < 0 {
		return nil, fmt.Errorf("artifact offset must be non-negative")
	}
	limit = min(max(limit, utf8.UTFMax), maxToolResultArtifactChunkBytes)
	var artifact ToolResultArtifactChunk
	var content []byte
	var coverageRaw string
	var createdAt sql.NullTime
	err := rs.db.QueryRow(
		`SELECT s.artifact_id,r.session_id,s.run_id,s.tool_call_id,
			SUBSTRING(s.artifact_content,?,?),s.artifact_content_type,
			s.authoritative_sha256,s.content_bytes,CAST(s.coverage_json AS CHAR),s.created_at
		 FROM agent_steps s
		 JOIN agent_runs r ON r.id=s.run_id
		 WHERE s.artifact_id=? AND r.user_id=? AND (?='' OR r.session_id=?) LIMIT 1`,
		offset+1, limit, artifactID, userID, sessionID, sessionID,
	).Scan(
		&artifact.ID, &artifact.SessionID, &artifact.RunID, &artifact.ToolCallID, &content,
		&artifact.ContentType, &artifact.SHA256, &artifact.SizeBytes, &coverageRaw, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if coverageRaw != "" {
		if err := json.Unmarshal([]byte(coverageRaw), &artifact.Coverage); err != nil {
			return nil, fmt.Errorf("decode artifact %q coverage: %w", artifactID, err)
		}
	}
	content, err = validArtifactTextPrefix(content, offset)
	if err != nil {
		return nil, err
	}
	artifact.Content = string(content)
	artifact.Offset = offset
	artifact.NextOffset = offset + int64(len(content))
	artifact.HasMore = artifact.NextOffset < artifact.SizeBytes
	artifact.CreatedAt = store.FormatDatabaseTime(createdAt)
	return &artifact, nil
}

func validArtifactTextPrefix(content []byte, offset int64) ([]byte, error) {
	if utf8.Valid(content) {
		return content, nil
	}
	for trim := 1; trim < utf8.UTFMax && trim < len(content); trim++ {
		candidate := content[:len(content)-trim]
		if utf8.Valid(candidate) {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("artifact content is not valid UTF-8 at byte offset %d", offset)
}
