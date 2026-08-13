package catalog

import (
	"encoding/json"
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
				"question":"Why is checkout failing?",
				"objective":"Trace the checkout failure",
				"entities":[{"id":"Checkout.Place"}],
				"evidence_goals":[{"id":"core_flow","facet":"core_flow","required":true}],
				"context":{
					"conversation_refs":[{"session_id":"session-1","run_id":"qa_0"},{"session_id":"session-1","turn":2}],
					"time_range":{"from":"2026-08-11T00:00:00Z","to":"2026-08-12T00:00:00Z","to_exclusive":true,"raw":"yesterday"},
					"seed_evidence":[{
						"source_kind":"code",
						"target":"Checkout.Place",
						"sections":["implementation"],
						"content_hash":"sha256:source",
						"coverage":{"complete":true,"included":1},
						"facets":["core_flow"],
						"trust_tier":2,
						"evidence_class":"source",
						"token_cost":20,
						"version":"abc123",
						"time_range":""
					}]
				}
			}`,
			valid: true,
		},
		{
			name: "minimal canonical contract",
			payload: `{
				"task_id":"qa_1",
				"question":"Where is checkout implemented?",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"evidence_goals":[{"id":"entrypoint","facet":"entrypoint","required":true}],
				"context":{}
			}`,
			valid: true,
		},
		{
			name:    "legacy request",
			payload: `{"question":"Where is checkout implemented?"}`,
		},
		{
			name: "no structured evidence goals",
			payload: `{
				"task_id":"qa_1",
				"question":"Where is checkout implemented?",
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
				"question":"Where is checkout implemented?",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"context":{}
			}`,
		},
		{
			name: "copied conversation body",
			payload: `{
				"task_id":"qa_1",
				"question":"Where is checkout implemented?",
				"objective":"Locate the checkout implementation",
				"entities":[],
				"evidence_goals":[{"id":"entrypoint","facet":"entrypoint","required":true}],
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
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"code report","findings":[],"gaps":[]},"completeness":"complete"},
					{"producer_node_id":"investigate.runtime","schema":{"id":"investigation.report","version":1},"payload":{"focus":"runtime","summary":"runtime report","findings":[],"gaps":[]},"completeness":"complete"},
					{"producer_node_id":"investigate.docs","schema":{"id":"investigation.report","version":1},"payload":{"focus":"docs","summary":"docs report","findings":[],"gaps":[]},"completeness":"complete"}
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
					{"producer_node_id":"investigate.docs","schema":{"id":"investigation.report","version":1},"payload":{"focus":"docs","summary":"docs report","findings":[],"gaps":[]},"completeness":"partial"}
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
			name:    "no reports",
			payload: `{"handoffs":[],"evidence_units":[],"evidence_conflicts":[],"completeness":"unavailable"}`,
		},
		{
			name: "invalid report",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"unknown","summary":"report","findings":[],"gaps":[]},"completeness":"complete"}
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
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"report","findings":[],"gaps":[]},"completeness":"complete"}
				],
				"evidence_units":[],
				"completeness":"complete"
			}`,
		},
		{
			name: "invalid unavailable task",
			payload: `{
				"handoffs":[
					{"producer_node_id":"investigate.code","schema":{"id":"investigation.report","version":1},"payload":{"focus":"code","summary":"report","findings":[],"gaps":[]},"completeness":"complete"}
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
