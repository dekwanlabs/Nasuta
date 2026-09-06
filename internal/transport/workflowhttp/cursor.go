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
	"github.com/dekwanlabs/nasuta/internal/transport/sse"
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
	return decodeValidatedCursor(value, "workflow", func(cursor workflow.DefinitionCursor) bool {
		return canonicalCursorID.MatchString(cursor.ID) && cursor.Version > 0
	})
}

func decodeValidatedCursor[T any](value, kind string, valid func(T) bool) (T, error) {
	var cursor T
	if err := decodeCursor(value, &cursor); err != nil {
		return cursor, fmt.Errorf("invalid %s cursor: %w", kind, err)
	}
	if strings.TrimSpace(value) == "" {
		return cursor, nil
	}
	if !valid(cursor) {
		var zero T
		return zero, fmt.Errorf("invalid %s cursor", kind)
	}
	return cursor, nil
}

func encodeDefinitionCursor(definition workflow.DefinitionRecord) string {
	return encodeCursor(workflow.DefinitionCursor{
		ID: definition.ID, Version: definition.Version,
	})
}

func decodeNodeCursor(value string) (workflow.NodeRunCursor, error) {
	return decodeValidatedCursor(value, "node", func(cursor workflow.NodeRunCursor) bool {
		return canonicalCursorID.MatchString(cursor.NodeID) && cursor.Attempt > 0
	})
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
	return sse.Cursor(r, allowLastEventID)
}
