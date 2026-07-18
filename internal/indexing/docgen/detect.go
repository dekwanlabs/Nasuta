package docgen

import (
	"encoding/json"
	"fmt"
	"strings"
)

// validTypes is the set of known project types.
var validTypes = map[string]bool{
	"backend": true, "frontend": true, "mobile": true,
	"embedded": true, "mcu": true, "host": true, "module": true, "generic": true,
}

// parseClassifyJSON parses the LLM classification response.
func parseClassifyJSON(raw string) (*classifyResult, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var result classifyResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	if result.ProjectType == "" {
		return nil, fmt.Errorf("empty project_type")
	}
	return &result, nil
}
