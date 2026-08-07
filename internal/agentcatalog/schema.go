package agentcatalog

import (
	"encoding/json"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// DefaultSchemas returns the versioned contracts shipped with the standard Agent definitions.
func DefaultSchemas() []agentapi.SchemaDefinition {
	return []agentapi.SchemaDefinition{
		{
			ID: "qa.request", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["question"],
				"properties":{"question":{"type":"string","minLength":1}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "qa.answer", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"string",
				"minLength":1
			}`),
		},
		{
			ID: "investigation.request", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["question"],
				"properties":{"question":{"type":"string","minLength":1,"maxLength":8000}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "investigation.report", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["focus","summary","findings","gaps"],
				"properties":{
					"focus":{"enum":["code","runtime","docs"]},
					"summary":{"type":"string","minLength":1},
					"findings":{"type":"array","maxItems":50,"items":{"$ref":"#/$defs/finding"}},
					"gaps":{"type":"array","maxItems":20,"items":{"type":"string","minLength":1}}
				},
				"additionalProperties":false,
				"$defs":{
					"evidence":{
						"type":"object",
						"required":["kind","reference","summary"],
						"properties":{
							"kind":{"type":"string","minLength":1},
							"reference":{"type":"string","minLength":1},
							"summary":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					},
					"finding":{
						"type":"object",
						"required":["claim","evidence","confidence"],
						"properties":{
							"claim":{"type":"string","minLength":1},
							"evidence":{"type":"array","minItems":1,"maxItems":20,"items":{"$ref":"#/$defs/evidence"}},
							"confidence":{"type":"number","minimum":0,"maximum":1}
						},
						"additionalProperties":false
					}
				}
			}`),
		},
		{
			ID: "investigation.bundle", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"array",
				"minItems":3,
				"maxItems":3,
				"prefixItems":[
					{"allOf":[{"$ref":"#/$defs/report"},{"properties":{"focus":{"const":"code"}}}]},
					{"allOf":[{"$ref":"#/$defs/report"},{"properties":{"focus":{"const":"docs"}}}]},
					{"allOf":[{"$ref":"#/$defs/report"},{"properties":{"focus":{"const":"runtime"}}}]}
				],
				"items":false,
				"$defs":{
					"report":{
					"type":"object",
					"required":["focus","summary","findings","gaps"],
					"properties":{
						"focus":{"enum":["code","runtime","docs"]},
						"summary":{"type":"string","minLength":1},
						"findings":{"type":"array","maxItems":50,"items":{"$ref":"#/$defs/finding"}},
						"gaps":{"type":"array","maxItems":20,"items":{"type":"string","minLength":1}}
					},
					"additionalProperties":false
					},
					"evidence":{
						"type":"object",
						"required":["kind","reference","summary"],
						"properties":{
							"kind":{"type":"string","minLength":1},
							"reference":{"type":"string","minLength":1},
							"summary":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					},
					"finding":{
						"type":"object",
						"required":["claim","evidence","confidence"],
						"properties":{
							"claim":{"type":"string","minLength":1},
							"evidence":{"type":"array","minItems":1,"maxItems":20,"items":{"$ref":"#/$defs/evidence"}},
							"confidence":{"type":"number","minimum":0,"maximum":1}
						},
						"additionalProperties":false
					}
				}
			}`),
		},
		{
			ID: "investigation.answer", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["answer","citations","limitations"],
				"properties":{
					"answer":{"type":"string","minLength":1},
					"citations":{"type":"array","maxItems":50,"items":{
						"type":"object",
						"required":["claim","evidence"],
						"properties":{
							"claim":{"type":"string","minLength":1},
							"evidence":{"type":"array","minItems":1,"maxItems":20,"items":{
								"type":"object",
								"required":["kind","reference","summary"],
								"properties":{
									"kind":{"type":"string","minLength":1},
									"reference":{"type":"string","minLength":1},
									"summary":{"type":"string","minLength":1}
								},
								"additionalProperties":false
							}}
						},
						"additionalProperties":false
					}},
					"limitations":{"type":"array","maxItems":20,"items":{"type":"string","minLength":1}}
				},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.request", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["subject","categories","policy_hash"],
				"properties":{
					"subject":{"type":"object"},
					"categories":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},
					"policy_hash":{"type":"string","minLength":1}
				},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.report", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["coverage","findings","uncertainties","summary"],
				"properties":{
					"coverage":{"type":"array","items":{"$ref":"#/$defs/coverage"}},
					"findings":{"type":"array","maxItems":100,"items":{"$ref":"#/$defs/finding"}},
					"uncertainties":{"type":"array","items":{"$ref":"#/$defs/uncertainty"}},
					"summary":{"type":"string","minLength":1}
				},
				"additionalProperties":false,
				"$defs":{
					"coverage":{
						"type":"object",
						"required":["category","covered","summary"],
						"properties":{
							"category":{"type":"string","minLength":1},
							"covered":{"type":"boolean"},
							"summary":{"type":"string"}
						},
						"additionalProperties":false
					},
					"uncertainty":{
						"type":"object",
						"required":["category","summary"],
						"properties":{
							"category":{"type":"string","minLength":1},
							"summary":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					},
					"evidence":{
						"type":"object",
						"required":["kind","ref","hash","summary"],
						"properties":{
							"kind":{"type":"string","minLength":1},
							"ref":{"type":"string","minLength":1},
							"hash":{"type":"string","minLength":1},
							"summary":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					},
					"location":{
						"type":"object",
						"properties":{
							"path":{"type":"string"},
							"field":{"type":"string"},
							"start_line":{"type":"integer","minimum":0},
							"end_line":{"type":"integer","minimum":0}
						},
						"additionalProperties":false
					},
					"finding":{
						"type":"object",
						"required":["category","severity","claim","impact","evidence","recommendation","confidence"],
						"properties":{
							"category":{"type":"string","minLength":1},
							"severity":{"enum":["critical","high","medium","low","info"]},
							"claim":{"type":"string","minLength":1},
							"impact":{"type":"string","minLength":1},
							"evidence":{"type":"array","minItems":1,"maxItems":20,"items":{"$ref":"#/$defs/evidence"}},
							"location":{"$ref":"#/$defs/location"},
							"recommendation":{"type":"string","minLength":1},
							"confidence":{"type":"number","minimum":0,"maximum":1}
						},
						"additionalProperties":false
					}
				}
			}`),
		},
		{
			ID: "review.adjudication.request", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["subject","policy_hash","fingerprint","findings"],
				"properties":{
					"subject":{"type":"object"},
					"policy_hash":{"type":"string","minLength":1},
					"fingerprint":{"type":"string","minLength":1},
					"findings":{"type":"array","minItems":2,"items":{"type":"object"}}
				},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.adjudication", Version: 1,
			Document: json.RawMessage(`{
				"$schema":"https://json-schema.org/draft/2020-12/schema",
				"type":"object",
				"required":["decision","rationale"],
				"properties":{
					"decision":{"enum":["confirmed","not_supported","distinct_findings","needs_human"]},
					"rationale":{"type":"string","minLength":1}
				},
				"additionalProperties":false
			}`),
		},
	}
}
