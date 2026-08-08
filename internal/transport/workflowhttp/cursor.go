package workflowhttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
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

func decodeDefinitionCursor(value string) (workflow.DefinitionCursor, error) {
	var cursor workflow.DefinitionCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return workflow.DefinitionCursor{}, fmt.Errorf("invalid workflow cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if !canonicalCursorID.MatchString(cursor.ID) || cursor.Version <= 0 {
		return workflow.DefinitionCursor{}, fmt.Errorf("invalid workflow cursor")
	}
	return cursor, nil
}

func encodeDefinitionCursor(definition workflow.DefinitionRecord) string {
	return encodeCursor(workflow.DefinitionCursor{
		ID: definition.ID, Version: definition.Version,
	})
}

func decodeNodeCursor(value string) (workflow.NodeRunCursor, error) {
	var cursor workflow.NodeRunCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return workflow.NodeRunCursor{}, fmt.Errorf("invalid node cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if !canonicalCursorID.MatchString(cursor.NodeID) || cursor.Attempt <= 0 {
		return workflow.NodeRunCursor{}, fmt.Errorf("invalid node cursor")
	}
	return cursor, nil
}

func encodeNodeCursor(run workflow.NodeRunRecord) string {
	return encodeCursor(workflow.NodeRunCursor{
		NodeID: run.NodeID, Attempt: run.Attempt,
	})
}

func decodeHandoffCursor(value string) (workflow.HandoffCursor, error) {
	var cursor workflow.HandoffCursor
	if err := decodeCursor(value, &cursor); err != nil {
		return workflow.HandoffCursor{}, fmt.Errorf("invalid handoff cursor: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if cursor.CreatedAt.IsZero() || !canonicalCursorID.MatchString(cursor.ID) {
		return workflow.HandoffCursor{}, fmt.Errorf("invalid handoff cursor")
	}
	return cursor, nil
}

func encodeHandoffCursor(handoff workflow.Handoff) string {
	return encodeCursor(workflow.HandoffCursor{
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
