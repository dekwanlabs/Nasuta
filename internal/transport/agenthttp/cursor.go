package agenthttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
)

var canonicalAgentID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

func decodeDefinitionCursor(value string) (agentcatalog.DefinitionCursor, error) {
	var cursor agentcatalog.DefinitionCursor
	value = strings.TrimSpace(value)
	if value == "" {
		return cursor, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, fmt.Errorf("invalid agent cursor: %w", err)
	}
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return cursor, fmt.Errorf("invalid agent cursor: %w", err)
	}
	if !canonicalAgentID.MatchString(cursor.ID) || cursor.Version <= 0 {
		return agentcatalog.DefinitionCursor{}, fmt.Errorf("invalid agent cursor")
	}
	return cursor, nil
}

func encodeDefinitionCursor(record agentcatalog.DefinitionRecord) string {
	raw, _ := json.Marshal(agentcatalog.DefinitionCursor{
		ID: record.ID, Version: record.Version,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}
