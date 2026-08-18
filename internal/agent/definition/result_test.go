package definition

import (
	"encoding/json"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

func TestCanonicalStructuredOutput(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		want    string
		wantErr bool
	}{
		{name: "raw", answer: ` {"answer":"ok"} `, want: `{"answer":"ok"}`},
		{name: "json fence", answer: "```json\n{\"answer\":\"ok\"}\n```", want: `{"answer":"ok"}`},
		{name: "plain fence", answer: "```\n{\"answer\":\"ok\"}\n```", want: `{"answer":"ok"}`},
		{name: "prose before fence", answer: "result:\n```json\n{}\n```", wantErr: true},
		{name: "prose after fence", answer: "```json\n{}\n```\nresult", wantErr: true},
		{name: "unsupported fence", answer: "```javascript\n{}\n```", wantErr: true},
		{name: "multiple fences", answer: "```json\n{}\n```\n```", wantErr: true},
		{name: "invalid json", answer: "```json\n{\"answer\":\n```", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalStructuredOutput(test.answer)
			if test.wantErr {
				if err == nil {
					t.Fatalf("canonicalStructuredOutput() = %s, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalStructuredOutput(): %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("canonicalStructuredOutput() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestValidatedOutputNormalizesSingleJSONFence(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish([]agentapi.SchemaDefinition{{
		ID: "test.output", Version: 1,
		Document: json.RawMessage(`{
			"type":"object",
			"properties":{"answer":{"type":"string"}},
			"required":["answer"],
			"additionalProperties":false
		}`),
	}}); err != nil {
		t.Fatalf("publish schema: %v", err)
	}
	got, err := validatedOutput(
		registry,
		agentapi.SchemaRef{ID: "test.output", Version: 1},
		"```json\n{\"answer\":\"ok\"}\n```",
	)
	if err != nil {
		t.Fatalf("validatedOutput(): %v", err)
	}
	if string(got) != `{"answer":"ok"}` {
		t.Fatalf("validatedOutput() = %s", got)
	}
}

func TestValidatedOutputEncodesPlainTextForStringSchema(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish([]agentapi.SchemaDefinition{{
		ID: "test.output", Version: 1,
		Document: json.RawMessage(`{"type":"string","minLength":1}`),
	}}); err != nil {
		t.Fatalf("publish schema: %v", err)
	}
	got, err := validatedOutput(
		registry,
		agentapi.SchemaRef{ID: "test.output", Version: 1},
		"A natural-language answer.",
	)
	if err != nil {
		t.Fatalf("validatedOutput(): %v", err)
	}
	if string(got) != `"A natural-language answer."` {
		t.Fatalf("validatedOutput() = %s", got)
	}
}

func TestValidatedOutputRejectsPlainTextForObjectSchema(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish([]agentapi.SchemaDefinition{{
		ID: "test.output", Version: 1,
		Document: json.RawMessage(`{
			"type":"object",
			"properties":{"answer":{"type":"string"}},
			"required":["answer"],
			"additionalProperties":false
		}`),
	}}); err != nil {
		t.Fatalf("publish schema: %v", err)
	}
	_, err := validatedOutput(
		registry,
		agentapi.SchemaRef{ID: "test.output", Version: 1},
		"A natural-language answer.",
	)
	if err == nil {
		t.Fatal("validatedOutput() succeeded for plain text and an object schema")
	}
}

func TestMapResultRecoversInvalidInvestigationReportAsUnavailable(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	input := json.RawMessage(`{
		"task_id":"task-1",
		"objective":"inspect code",
		"entities":[],
		"evidence_goals":[{"id":"goal-1","facet":"core_flow","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}],
		"context":{}
	}`)
	result, outcome := mapResult(
		"run-1",
		&execution.RunResult{Answer: `{"focus":"code","summary":"broken"`},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{AgentID: "investigator.code", Input: input},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != agentrun.StatusDone || outcome.ErrorCode != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	var report struct {
		Focus           string   `json:"focus"`
		CoveredGoals    []string `json:"covered_goals"`
		UnresolvedGoals []string `json:"unresolved_goals"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if report.Focus != "code" || len(report.CoveredGoals) != 0 || len(report.UnresolvedGoals) != 1 || report.UnresolvedGoals[0] != "core_flow" {
		t.Fatalf("recovered report = %+v", report)
	}
}
