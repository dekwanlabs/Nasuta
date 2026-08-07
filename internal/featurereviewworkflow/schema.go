package featurereviewworkflow

import (
	"encoding/json"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const reportProperties = `
	"required":["id","round_id","assignment_id","reviewer_id","subject_hash","coverage","findings","uncertainties","summary","report_hash","content_hash","completed_at"],
	"properties":{
		"id":{"type":"string","minLength":1},
		"round_id":{"type":"string","minLength":1},
		"assignment_id":{"type":"string","minLength":1},
		"reviewer_id":{"type":"string","minLength":1},
		"subject_hash":{"type":"string","minLength":1},
		"coverage":{"type":"array","items":{"$ref":"#/$defs/coverage"}},
		"findings":{"type":"array","maxItems":100,"items":{"$ref":"#/$defs/finding"}},
		"uncertainties":{"type":"array","items":{"$ref":"#/$defs/uncertainty"}},
		"summary":{"type":"string"},
		"report_hash":{"type":"string","minLength":1},
		"content_hash":{"type":"string","minLength":1},
		"reuse":{"$ref":"#/$defs/reuse"},
		"completed_at":{"type":"string","format":"date-time"}
	},
	"additionalProperties":false`

const reportDefinitions = `
	"$defs":{
		"reuse":{
			"type":"object",
			"required":["source_report_id","source_round_id","source_assignment_id","reason"],
			"properties":{
				"source_report_id":{"type":"string","minLength":1},
				"source_round_id":{"type":"string","minLength":1},
				"source_assignment_id":{"type":"string","minLength":1},
				"reason":{"type":"string","minLength":1}
			},
			"additionalProperties":false
		},
		"coverage":{
			"type":"object",
			"required":["category","covered"],
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
			"required":["id","report_id","category","severity","claim","impact","evidence","recommendation","confidence","fingerprint","content_hash"],
			"properties":{
				"id":{"type":"string","minLength":1},
				"report_id":{"type":"string","minLength":1},
				"category":{"type":"string","minLength":1},
				"severity":{"enum":["critical","high","medium","low","info"]},
				"claim":{"type":"string","minLength":1},
				"impact":{"type":"string","minLength":1},
				"evidence":{"type":"array","maxItems":20,"items":{"$ref":"#/$defs/evidence"}},
				"location":{"$ref":"#/$defs/location"},
				"recommendation":{"type":"string","minLength":1},
				"confidence":{"type":"number","minimum":0,"maximum":1},
				"fingerprint":{"type":"string","minLength":1},
				"content_hash":{"type":"string","minLength":1}
			},
			"additionalProperties":false
		}
	}`

// Schemas returns the strict contracts used by Review Workflow handoffs.
func Schemas() []agentapi.SchemaDefinition {
	reportDocument := json.RawMessage(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		` + reportProperties + `,
		` + reportDefinitions + `
	}`)
	reportListDocument := json.RawMessage(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"array",
		"maxItems":16,
		"items":{
			"type":"object",
			` + reportProperties + `
		},
		` + reportDefinitions + `
	}`)
	return []agentapi.SchemaDefinition{
		{ID: RequestSchemaID, Version: 1, Document: json.RawMessage(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"type":"object",
			"required":["round_id"],
			"properties":{"round_id":{"type":"string","minLength":1,"maxLength":128}},
			"additionalProperties":false
		}`)},
		{ID: ReportSchemaID, Version: 1, Document: reportDocument},
		{ID: ReportListSchemaID, Version: 1, Document: reportListDocument},
		{ID: GateSchemaID, Version: 1, Document: json.RawMessage(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"type":"object",
			"required":["id","round_id","subject_hash","decision","reason_codes","blocking_ids","conflict_ids","coverage_gaps","policy_hash","report_hashes","adjudication_hashes","content_hash","created_at"],
			"properties":{
				"id":{"type":"string","minLength":1},
				"round_id":{"type":"string","minLength":1},
				"subject_hash":{"type":"string","minLength":1},
				"decision":{"enum":["pass","revise","human_required","incomplete","failed"]},
				"reason_codes":{"type":"array","items":{"type":"string"}},
				"blocking_ids":{"type":"array","items":{"type":"string"}},
				"conflict_ids":{"type":"array","items":{"type":"string"}},
				"coverage_gaps":{"type":"array","items":{"type":"string"}},
				"policy_hash":{"type":"string","minLength":1},
				"report_hashes":{"type":"array","items":{"type":"string"}},
				"adjudication_hashes":{"type":"array","items":{"type":"string"}},
				"content_hash":{"type":"string","minLength":1},
				"created_at":{"type":"string","format":"date-time"}
			},
			"additionalProperties":false
		}`)},
	}
}
