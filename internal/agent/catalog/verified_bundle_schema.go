package catalog

import "encoding/json"

// verifiedBundleSchema defines the single compact handoff consumed by the synthesizer.
func verifiedBundleSchema() json.RawMessage {
	return json.RawMessage(`{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "required":["supported_claims","partial_claims","unsupported_claims","partial_evidence_goals","unresolved_evidence_goals","limitations","omissions","verification","completeness","limitations_detail"],
  "properties":{
    "supported_claims":{"type":"array","maxItems":200,"items":{"$ref":"#/$defs/supported_claim"}},
    "partial_claims":{"type":"array","maxItems":200,"items":{"$ref":"#/$defs/supported_claim"}},
    "unsupported_claims":{"type":"array","maxItems":200,"items":{"$ref":"#/$defs/unsupported_claim"}},
    "partial_evidence_goals":{"type":"array","maxItems":50,"uniqueItems":true,"items":{"type":"string","minLength":1}},
    "unresolved_evidence_goals":{"type":"array","maxItems":50,"uniqueItems":true,"items":{"type":"string","minLength":1}},
    "limitations":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1}},
    "limitations_detail":{"$ref":"#/$defs/limitations_detail"},
    "evidence_units":{"type":"array","maxItems":200,"items":{"$ref":"#/$defs/evidence_unit"}},
    "evidence_conflicts":{"type":"array","maxItems":100,"items":{"$ref":"#/$defs/evidence_conflict"}},
    "subject_coverage":{"type":"array","maxItems":50,"items":{"$ref":"#/$defs/subject_coverage"}},
    "verification":{"$ref":"#/$defs/verification"},
    "completeness":{"enum":["complete","partial","unavailable"]},
    "omissions":{"$ref":"#/$defs/omissions"},
    "evidence_lookup":{"type":"object","propertyNames":{"type":"string","minLength":1},"additionalProperties":{"$ref":"#/$defs/evidence_summary"}},
    "evidence_context":{"$ref":"#/$defs/evidence_context"},
    "evidence_omissions":{"type":"array","maxItems":200,"items":{"$ref":"#/$defs/evidence_omission"}}
  },
  "additionalProperties":false,
  "$defs":{
    "evidence_ref":{"type":"object","required":["evidence_id"],"properties":{"evidence_id":{"type":"string","minLength":1}},"additionalProperties":false},
    "evidence_summary":{"type":"object","required":["kind","reference","summary"],"properties":{"kind":{"type":"string","minLength":1},"reference":{"type":"string","minLength":1},"summary":{"type":"string","minLength":1},"evidence_id":{"type":"string","minLength":1},"content_hash":{"type":"string"},"identity":{"$ref":"#/$defs/evidence_identity"}},"additionalProperties":false},
    "supported_claim":{"type":"object","required":["producer_node_id","finding_index","claim","evidence_goal_ids","evidence","confidence","support","high_risk"],"properties":{"producer_node_id":{"type":"string","minLength":1},"finding_index":{"type":"integer","minimum":0},"claim":{"type":"string","minLength":1},"evidence_goal_ids":{"type":"array","minItems":1,"maxItems":50,"uniqueItems":true,"items":{"type":"string","minLength":1}},"evidence":{"type":"array","minItems":1,"maxItems":20,"items":{"$ref":"#/$defs/evidence_ref"}},"evidence_identities":{"type":"array","maxItems":20,"items":{"$ref":"#/$defs/evidence_identity"}},"confidence":{"type":"number","minimum":0,"maximum":1},"support":{"enum":["supported","partial"]},"high_risk":{"type":"boolean"}},"additionalProperties":false},
    "unsupported_claim":{"type":"object","required":["producer_node_id","finding_index","evidence_goal_ids","support","high_risk","reason_code"],"properties":{"producer_node_id":{"type":"string","minLength":1},"finding_index":{"type":"integer","minimum":0},"evidence_goal_ids":{"type":"array","minItems":1,"maxItems":50,"uniqueItems":true,"items":{"type":"string","minLength":1}},"support":{"const":"unsupported"},"high_risk":{"type":"boolean"},"reason_code":{"const":"canonical_evidence_unbound"}},"additionalProperties":false},
    "evidence_identity":{"type":"object","required":["source_kind","target"],"properties":{"source_kind":{"type":"string","minLength":1},"target":{"type":"string","minLength":1},"section":{"type":"string"},"version":{"type":"string"},"time_range":{"type":"string"}},"additionalProperties":false},
    "coverage":{"type":"object","properties":{"complete":{"type":"boolean"},"partial":{"type":"boolean"},"included":{"type":"integer","minimum":0},"omitted_items":{"type":"integer","minimum":0},"next_cursor":{"type":"string"}},"additionalProperties":false},
    "evidence_unit":{"type":"object","required":["source_kind","target","coverage"],"properties":{"source_kind":{"type":"string","minLength":1},"target":{"type":"string","minLength":1},"sections":{"type":"array","items":{"type":"string","minLength":1}},"content_hash":{"type":"string"},"coverage":{"$ref":"#/$defs/coverage"},"facets":{"type":"array","items":{"type":"string","minLength":1}},"trust_tier":{"type":"integer","minimum":0},"evidence_class":{"type":"string"},"token_cost":{"type":"integer","minimum":0},"version":{"type":"string"},"time_range":{"type":"string"}},"additionalProperties":false},
    "evidence_conflict":{"type":"object","required":["identity","current","incoming"],"properties":{"identity":{"$ref":"#/$defs/evidence_identity"},"current":{"$ref":"#/$defs/evidence_unit"},"incoming":{"$ref":"#/$defs/evidence_unit"},"current_origin":{"type":"string"},"incoming_origin":{"type":"string"}},"additionalProperties":false},
    "subject_coverage":{"type":"object","required":["entity_id","covered_facets","missing_facets","sources","complete"],"properties":{"entity_id":{"type":"string","minLength":1},"covered_facets":{"type":"array","uniqueItems":true,"items":{"type":"string","minLength":1}},"missing_facets":{"type":"array","uniqueItems":true,"items":{"type":"string","minLength":1}},"sources":{"type":"array","uniqueItems":true,"items":{"type":"string","minLength":1}},"complete":{"type":"boolean"}},"additionalProperties":false},
    "verification":{"type":"object","required":["decision","stop_reason"],"properties":{"decision":{"enum":["complete","partial","unavailable"]},"stop_reason":{"enum":["required_goals_covered","no_new_evidence","no_affordable_task","duplicate_evidence_limit","verification_failed","deadline_exceeded","budget_exhausted","capability_unavailable","evidence_insufficient","needs_clarification"]}},"additionalProperties":false},
    "omissions":{"type":"object","required":["claims","goals","limitations","evidence_units","evidence_conflicts"],"properties":{"claims":{"type":"integer","minimum":0},"goals":{"type":"integer","minimum":0},"limitations":{"type":"integer","minimum":0},"evidence_units":{"type":"integer","minimum":0},"evidence_conflicts":{"type":"integer","minimum":0}},"additionalProperties":false},
    "evidence_context":{"type":"object","required":["budget_tokens","used_tokens"],"properties":{"budget_tokens":{"type":"integer","minimum":0},"used_tokens":{"type":"integer","minimum":0},"omitted_tokens":{"type":"integer","minimum":0}},"additionalProperties":false},
    "evidence_omission":{"type":"object","required":["evidence_id","reason"],"properties":{"evidence_id":{"type":"string","minLength":1},"reason":{"type":"string","minLength":1}},"additionalProperties":false},
    "limitations_detail":{"type":"object","required":["artifact_id","total_count","displayed_count","omitted_count","normalization_version"],"properties":{"artifact_id":{"type":"string","pattern":"^art_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},"total_count":{"type":"integer","minimum":0},"displayed_count":{"type":"integer","minimum":0},"omitted_count":{"type":"integer","minimum":0},"normalization_version":{"type":"string","minLength":1}},"additionalProperties":false}
  }
}`)
}
