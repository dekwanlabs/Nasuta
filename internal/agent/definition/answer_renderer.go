package definition

import (
	"encoding/json"
)

// RenderPublicAnswer is the schema-independent public answer renderer. It only
// extracts a top-level string or answer field after the output has already been
// validated against its schema; it never guesses through structured handoff data.
func RenderPublicAnswer(output json.RawMessage) string {
	var text string
	if err := json.Unmarshal(output, &text); err == nil {
		return text
	}
	var object struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(output, &object); err != nil {
		return ""
	}
	return object.Answer
}
