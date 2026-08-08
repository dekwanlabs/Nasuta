package catalog

import (
	"encoding/json"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestInvestigationBundleRequiresOneReportPerStableFocus(t *testing.T) {
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
			name: "code docs runtime",
			payload: `[
				{"focus":"code","summary":"code report","findings":[],"gaps":[]},
				{"focus":"docs","summary":"docs report","findings":[],"gaps":[]},
				{"focus":"runtime","summary":"runtime report","findings":[],"gaps":[]}
			]`,
			valid: true,
		},
		{
			name: "missing runtime",
			payload: `[
				{"focus":"code","summary":"code report","findings":[],"gaps":[]},
				{"focus":"docs","summary":"docs report","findings":[],"gaps":[]}
			]`,
		},
		{
			name: "duplicate docs",
			payload: `[
				{"focus":"code","summary":"code report","findings":[],"gaps":[]},
				{"focus":"docs","summary":"docs report","findings":[],"gaps":[]},
				{"focus":"docs","summary":"duplicate report","findings":[],"gaps":[]}
			]`,
		},
		{
			name: "unstable order",
			payload: `[
				{"focus":"runtime","summary":"runtime report","findings":[],"gaps":[]},
				{"focus":"docs","summary":"docs report","findings":[],"gaps":[]},
				{"focus":"code","summary":"code report","findings":[],"gaps":[]}
			]`,
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
