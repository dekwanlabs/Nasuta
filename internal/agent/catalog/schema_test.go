package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestTaskContractRequiresCanonicalInvestigationContext(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SchemaRef{ID: "task.contract", Version: 1}
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{
			name: "full contract",
			payload: `{
				"task_id":"qa_1",
				"objective":"Trace the checkout failure",
				"entities":[{"id":"Checkout.Place"}],
				"investigation_goals":[
					{"id":"failure_path","objective":"Trace the failure path.","independently_useful":true,"depends_on":[]},
					{"id":"runtime_impact","objective":"Assess the runtime impact.","independently_useful":true,"depends_on":[]}
				],
				"evidence_goals":[{"id":"core_flow","facet":"core_flow","required":true,"sources":["internal","web"],"freshness":"bounded_live","minimum_coverage":1}],
				"context":{
					"conversation_refs":[{"session_id":"session-1","run_id":"qa_0"},{"session_id":"session-1","turn":2}],
					"time_range":{"from":"2026-08-11T00:00:00Z","to":"2026-08-12T00:00:00Z","to_exclusive":true,"raw":"yesterday"},
					"seed_material":[{
						"source":"qa.evidence",
						"title":"QA Evidence",
						"content":"bounded evidence",
						"evidence":[],
						"evidence_conflicts":[],
						"complete":false,
						"content_hash":"context-v1"
					}]
				}
			}`,
			valid: true,
		},
		{
			name: "invalid investigation goal id",
			payload: `{
				"task_id":"qa_1",
				"objective":"Trace the checkout failure",
				"entities":[],
				"investigation_goals":[
					{"id":"Invalid Goal","objective":"Trace the failure path.","independently_useful":true,"depends_on":[]}
				],
				"evidence_goals":[],
				"context":{}
			}`,
		},
		{
			name: "minimal canonical contract",
			payload: `{
				"task_id":"qa_1",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"evidence_goals":[{"id":"entrypoint","facet":"entrypoint","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}],
				"context":{}
			}`,
			valid: true,
		},
		{
			name:    "legacy request",
			payload: `{"question":"Where is checkout implemented?"}`,
		},
		{
			name: "narrow delegation task",
			payload: `{
				"capability":"knowledge.code.inspect",
				"objective":"Confirm the reachable failure path",
				"parent_question_summary":"Why is checkout failing?",
				"focus_facets":["core_flow","data_and_state"],
				"evidence_refs":["ev-code-1"],
				"delegation_id":"del-1",
				"parent_run_id":"run-parent",
				"task_index":0
			}`,
			valid: true,
		},
		{
			name: "incomplete delegation task",
			payload: `{
				"capability":"knowledge.code.inspect",
				"objective":"Confirm the reachable failure path",
				"parent_question_summary":"Why is checkout failing?",
				"focus_facets":[],
				"evidence_refs":[],
				"delegation_id":"del-1",
				"parent_run_id":"run-parent"
			}`,
		},
		{
			name: "no structured evidence goals",
			payload: `{
				"task_id":"qa_1",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"evidence_goals":[],
				"context":{}
			}`,
			valid: true,
		},
		{
			name: "missing evidence goals field",
			payload: `{
				"task_id":"qa_1",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"context":{}
			}`,
		},
		{
			name: "copied conversation body",
			payload: `{
				"task_id":"qa_1",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"evidence_goals":[{"id":"entrypoint","facet":"entrypoint","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}],
				"context":{"conversation_refs":[{"session_id":"session-1","content":"copied dialogue"}]}
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := registry.Validate(ref, json.RawMessage(test.payload))
			if test.valid && err != nil {
				t.Fatalf("valid task contract rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid task contract accepted")
			}
		})
	}
}

func TestDelegationReportSchema(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SchemaRef{ID: "delegation.report", Version: 1}
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{
			name: "complete report",
			payload: `{
				"run_id":"run_child_1",
				"report_id":"report_1",
				"capability":"knowledge.code.inspect",
				"status":"completed",
				"completeness":"complete",
				"summary":"A reachable nil path exists.",
				"findings":[{
					"id":"claim_1",
					"statement":"UpdateStatus dereferences a nil order.",
					"structured_claim":{
						"schema":"knowledge.code.assertion.v1",
						"subject":"symbol:UpdateStatus",
						"predicate":"reachable_null_dereference",
						"value":true,
						"scope":{"revision":"commit_abc"}
					},
					"confidence":"high",
					"citations":["ev_1"],
					"facets":["core_flow"],
					"critical":true
				}],
				"conflicts":[],
				"uncertainties":[],
				"usage":{
					"tool_calls":2,
					"input_tokens":300,
					"output_tokens":100,
					"reasoning_tokens":20,
					"total_tokens":420,
					"cost_micros":7
				}
			}`,
			valid: true,
		},
		{
			name: "failed report",
			payload: `{
				"capability":"knowledge.docs.verify",
				"status":"failed",
				"completeness":"partial",
				"usage":{"tool_calls":0,"input_tokens":0,"output_tokens":0,"total_tokens":0},
				"error":{"code":"child_execution_failed","message":"provider failed","retryable":false}
			}`,
			valid: true,
		},
		{
			name: "completed partial",
			payload: `{
				"capability":"knowledge.code.inspect",
				"status":"completed",
				"completeness":"partial",
				"usage":{"tool_calls":0,"input_tokens":0,"output_tokens":0,"total_tokens":0}
			}`,
		},
		{
			name: "finding without citation",
			payload: `{
				"capability":"knowledge.code.inspect",
				"status":"partial",
				"completeness":"partial",
				"findings":[{
					"id":"claim_1",
					"statement":"Unsupported claim.",
					"confidence":"medium",
					"citations":[]
				}],
				"usage":{"tool_calls":0,"input_tokens":0,"output_tokens":0,"total_tokens":0}
			}`,
		},
		{
			name: "oversized capability",
			payload: `{
				"capability":"` + strings.Repeat(
				"a",
				agentapi.MaxCapabilityIDBytes+1,
			) + `",
				"status":"failed",
				"completeness":"partial",
				"usage":{"tool_calls":0,"input_tokens":0,"output_tokens":0,"total_tokens":0}
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := registry.Validate(ref, json.RawMessage(test.payload))
			if test.valid && err != nil {
				t.Fatalf("valid delegation report rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid delegation report accepted")
			}
		})
	}
}

func TestInvestigationBundleAcceptsAvailableReports(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{
			name: "multiple reports",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"code report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"},
					{"producer_node_id":"investigate.runtime","schema":{"id":"investigation.report","version":1},"payload":{"focus":"runtime","summary":"runtime report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"},
					{"producer_node_id":"investigate.docs","schema":{"id":"investigation.report","version":1},"payload":{"focus":"docs","summary":"docs report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"}
				],
				"evidence_units":[],
				"evidence_conflicts":[],
				"completeness":"complete"
			}`,
			valid: true,
		},
		{
			name: "one available report",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.docs","schema":{"id":"investigation.report","version":1},"payload":{"focus":"docs","summary":"docs report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"partial"}
				],
				"unavailable_tasks":[
					{"producer_node_id":"investigate.code"},
					{"producer_node_id":"investigate.runtime"}
				],
				"evidence_units":[],
				"evidence_conflicts":[],
				"completeness":"partial"
			}`,
			valid: true,
		},
		{
			name: "evidence insufficient unavailable task",
			payload: `{
				"handoffs":[],
				"unavailable_tasks":[
					{"producer_node_id":"investigate.web","stop_reason":"evidence_insufficient"}
				],
				"evidence_units":[],
				"evidence_conflicts":[],
				"completeness":"unavailable"
			}`,
			valid: true,
		},
		{
			name: "report with canonical evidence identity",
			payload: `{
				"handoffs":[{
					"producer_node_id":"investigate.code",
					"schema":{"id":"investigation.report","version":1},
					"payload":{
						"focus":"code",
						"summary":"code report",
						"findings":[{
							"claim":"Checkout validates the request.",
							"goal_ids":["core_flow"],
							"evidence":[{
								"kind":"code",
								"reference":"Checkout.PlaceOrder",
								"summary":"validation branch",
								"identity":{
									"source_kind":"code",
									"target":"Checkout.PlaceOrder",
									"section":"validation",
									"version":"commit-123"
								}
							}],
							"confidence":0.9
						}],
						"gaps":[],
						"covered_goals":["core_flow"],
						"unresolved_goals":[]
					},
					"completeness":"complete"
				}],
				"evidence_units":[{
					"source_kind":"code",
					"target":"Checkout.PlaceOrder",
					"sections":["validation"],
					"coverage":{"complete":true},
					"version":"commit-123"
				}],
				"evidence_conflicts":[],
				"completeness":"complete"
			}`,
			valid: true,
		},
		{
			name: "report with evidence handle and entity IDs",
			payload: `{
				"handoffs":[{
					"producer_node_id":"investigate.code",
					"schema":{"id":"investigation.report","version":1},
					"payload":{
						"focus":"code",
						"summary":"code report",
						"findings":[{
							"claim":"Checkout validates the request.",
							"entity_ids":["Checkout.Place"],
							"goal_ids":["core_flow"],
							"evidence":[{
								"kind":"code",
								"reference":"Checkout.PlaceOrder",
								"summary":"validation branch",
								"evidence_id":"ev_0000000000000000000000000000000000000000000000000000000000000000"
							}],
							"confidence":0.9
						}],
						"gaps":[],
						"covered_goals":["core_flow"],
						"unresolved_goals":[]
					},
					"completeness":"complete"
				}],
				"evidence_units":[{
					"source_kind":"code",
					"target":"Checkout.PlaceOrder",
					"sections":["validation"],
					"coverage":{"complete":true}
				}],
				"evidence_conflicts":[],
				"completeness":"complete"
			}`,
			valid: true,
		},
		{
			name:    "no reports",
			payload: `{"handoffs":[],"evidence_units":[],"evidence_conflicts":[],"completeness":"unavailable"}`,
			valid:   true,
		},
		{
			name: "invalid report",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"unknown","summary":"report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"}
				],
				"evidence_units":[],
				"evidence_conflicts":[],
				"completeness":"complete"
			}`,
		},
		{
			name: "missing ledger field",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"}
				],
				"evidence_units":[],
				"completeness":"complete"
			}`,
		},
		{
			name: "invalid unavailable task",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"report","findings":[],"gaps":[],"covered_goals":[],"unresolved_goals":[]},"completeness":"complete"}
				],
				"unavailable_tasks":[
					{"producer_node_id":"investigate.runtime","reason":"timeout"}
				],
				"evidence_units":[],
				"evidence_conflicts":[],
				"completeness":"partial"
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := registry.Validate(ref, json.RawMessage(test.payload))
			if test.valid && err != nil {
				t.Fatalf("valid bundle rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}

func TestVerifiedBundleRequiresCanonicalEvidenceIdentities(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SchemaRef{
		ID: "investigation.verified_bundle", Version: 2,
	}
	tests := []struct {
		name  string
		claim string
		valid bool
	}{
		{
			name: "canonical claim support",
			claim: `{
				"producer_node_id":"investigate.code",
				"finding_index":0,
				"claim":"Checkout validates the request.",
				"goal_ids":["core_flow"],
				"evidence":[{
					"kind":"code",
					"reference":"Checkout.PlaceOrder",
					"summary":"validation branch",
					"identity":{
						"source_kind":"code",
						"target":"Checkout.PlaceOrder",
						"section":"validation",
						"version":"commit-123"
					}
				}],
				"evidence_identities":[{
					"source_kind":"code",
					"target":"Checkout.PlaceOrder",
					"section":"validation",
					"version":"commit-123"
				}],
				"confidence":0.9,
				"support":"supported",
				"high_risk":false
			}`,
			valid: true,
		},
		{
			name: "missing canonical identities",
			claim: `{
				"producer_node_id":"investigate.code",
				"finding_index":0,
				"claim":"Checkout validates the request.",
				"goal_ids":["core_flow"],
				"evidence":[{
					"kind":"code",
					"reference":"Checkout.PlaceOrder",
					"summary":"validation branch"
				}],
				"confidence":0.9,
				"support":"supported",
				"high_risk":false
			}`,
		},
		{
			name: "empty canonical identities",
			claim: `{
				"producer_node_id":"investigate.code",
				"finding_index":0,
				"claim":"Checkout validates the request.",
				"goal_ids":["core_flow"],
				"evidence":[{
					"kind":"code",
					"reference":"Checkout.PlaceOrder",
					"summary":"validation branch"
				}],
				"evidence_identities":[],
				"confidence":0.9,
				"support":"supported",
				"high_risk":false
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := json.RawMessage(`{
				"supported_claims":[` + test.claim + `],
				"partial_claims":[],
				"unsupported_claims":[],
				"partial_goals":[],
				"unresolved_goals":[],
				"limitations":[],
				"evidence_units":[{
					"source_kind":"code",
					"target":"Checkout.PlaceOrder",
					"sections":["validation"],
					"coverage":{"complete":true},
					"version":"commit-123"
				}],
				"evidence_conflicts":[],
				"omissions":{
					"claims":0,
					"goals":0,
					"limitations":0,
					"evidence_units":0,
					"evidence_conflicts":0
				},
				"verification":{
					"decision":"complete",
					"stop_reason":"required_goals_covered"
				},
				"completeness":"complete",
				"limitations_detail":{
					"artifact_id":"art_00000000-0000-0000-0000-000000000000",
					"total_count":0,"displayed_count":0,"omitted_count":0,
					"normalization_version":"limitations-v1"
				}
			}`)
			err := registry.Validate(ref, payload)
			if test.valid && err != nil {
				t.Fatalf("valid verified bundle rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid verified bundle accepted")
			}
		})
	}
}

func TestInvestigationAnswerV2AcceptsAllVerifiedLimitations(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	limitations := make([]string, 28)
	for index := range limitations {
		limitations[index] = "verified limitation"
	}
	payload, err := json.Marshal(map[string]any{
		"answer":      "The evidence is partial.",
		"citations":   []any{},
		"limitations": limitations,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(
		agentapi.SchemaRef{ID: "investigation.answer", Version: 2},
		payload,
	); err != nil {
		t.Fatalf("v2 rejected 28 verified limitations: %v", err)
	}
	if err := registry.Validate(
		agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		payload,
	); err == nil {
		t.Fatal("v1 accepted a payload beyond its immutable 20-item limit")
	}
}

func TestVerifiedBundleSubjectCoverageSchema(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SchemaRef{ID: "investigation.verified_bundle", Version: 2}
	base := func(subject string) json.RawMessage {
		return json.RawMessage(`{
			"supported_claims":[],
			"partial_claims":[],
			"unsupported_claims":[],
			"partial_goals":[],
			"unresolved_goals":[],
			"limitations":["One comparison subject is missing internal evidence."],
			"evidence_units":[],
			"evidence_conflicts":[],
			"subject_coverage":[` + subject + `],
			"omissions":{"claims":0,"goals":0,"limitations":0,"evidence_units":0,"evidence_conflicts":0},
			"verification":{"decision":"partial","stop_reason":"evidence_insufficient"},
			"completeness":"partial",
			"limitations_detail":{
				"artifact_id":"art_00000000-0000-0000-0000-000000000000",
				"total_count":1,"displayed_count":1,"omitted_count":0,
				"normalization_version":"limitations-v1"
			}
		}`)
	}
	valid := base(`{
		"entity_id":"our_agent",
		"covered_facets":["core_flow"],
		"missing_facets":[],
		"sources":["internal"],
		"complete":true
	}`)
	if err := registry.Validate(ref, valid); err != nil {
		t.Fatalf("valid subject coverage rejected: %v", err)
	}
	invalid := base(`{
		"entity_id":"our_agent",
		"covered_facets":["core_flow"],
		"missing_facets":[],
		"complete":true
	}`)
	if err := registry.Validate(ref, invalid); err == nil {
		t.Fatal("subject coverage without sources was accepted")
	}
}
