package workflowhttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
)

var canonicalCursorID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

func requestLimit(r *http.Request) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return defaultPageSize, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxPageSize {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxPageSize)
	}
	return limit, nil
}

func decodeDefinitionCursor(value string) (agentworkflow.DefinitionCursor, error) {
	var cursor agentworkflow.DefinitionCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return agentworkflow.DefinitionCursor{}, fmt.Errorf("invalid workflow cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if !canonicalCursorID.MatchString(cursor.ID) || cursor.Version <= 0 {
		return agentworkflow.DefinitionCursor{}, fmt.Errorf("invalid workflow cursor")
	}
	return cursor, nil
}

func encodeDefinitionCursor(definition agentworkflow.DefinitionRecord) string {
	return encodeCursor(agentworkflow.DefinitionCursor{
		ID: definition.ID, Version: definition.Version,
	})
}

func decodeNodeCursor(value string) (agentworkflow.NodeRunCursor, error) {
	var cursor agentworkflow.NodeRunCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return agentworkflow.NodeRunCursor{}, fmt.Errorf("invalid node cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if !canonicalCursorID.MatchString(cursor.NodeID) || cursor.Attempt <= 0 {
		return agentworkflow.NodeRunCursor{}, fmt.Errorf("invalid node cursor")
	}
	return cursor, nil
}

func encodeNodeCursor(run agentworkflow.NodeRunRecord) string {
	return encodeCursor(agentworkflow.NodeRunCursor{
		NodeID: run.NodeID, Attempt: run.Attempt,
	})
}

func decodeHandoffCursor(value string) (agentworkflow.HandoffCursor, error) {
	var cursor agentworkflow.HandoffCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return agentworkflow.HandoffCursor{}, fmt.Errorf("invalid handoff cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if cursor.CreatedAt.IsZero() || !canonicalCursorID.MatchString(cursor.ID) {
		return agentworkflow.HandoffCursor{}, fmt.Errorf("invalid handoff cursor")
	}
	return cursor, nil
}

func encodeHandoffCursor(handoff agentworkflow.Handoff) string {
	return encodeCursor(agentworkflow.HandoffCursor{
		CreatedAt: handoff.CreatedAt, ID: handoff.ID,
	})
}

func decodeCursor(value string, target any) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return nil
}

func encodeCursor(value any) string {
	raw, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func eventCursor(r *http.Request, allowLastEventID bool) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if value == "" && allowLastEventID {
		value = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return seq, nil
}
