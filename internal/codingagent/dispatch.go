package codingagent

import "encoding/json"

var finalResultSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "summary":{"type":"string"},
    "tests":{"type":"string"}
  },
  "required":["summary","tests"]
}`)

type finalResult struct {
	Summary string `json:"summary"`
	Tests   string `json:"tests"`
}

func stringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func nestedString(values map[string]any, objectKey, valueKey string) string {
	nested, ok := values[objectKey].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := nested[valueKey].(string)
	return value
}
