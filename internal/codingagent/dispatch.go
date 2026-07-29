package codingagent

import (
	"encoding/json"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

var finalResultSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "summary":{"type":"string"},
    "tests":{"type":"string"},
    "deviations":{
      "type":"array",
      "maxItems":100,
      "items":{
        "type":"object",
        "additionalProperties":false,
        "properties":{
          "path":{"type":"string","maxLength":1024},
          "reason":{"type":"string","maxLength":2000}
        },
        "required":["path","reason"]
      }
    }
  },
  "required":["summary","tests","deviations"]
}`)

type finalResult struct {
	Summary    string                          `json:"summary"`
	Tests      string                          `json:"tests"`
	Deviations []featuredelivery.PlanDeviation `json:"deviations"`
}

func stringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
