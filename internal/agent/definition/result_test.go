package definition

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestMapResultRecoversReasoningTruncatedSynthesizerFromVerifiedBundle(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	input := verifiedBundleForAnswerRecovery()
	result, outcome := mapResult(
		"run-synthesis-recovery",
		&execution.RunResult{
			Err:      execution.ErrReasoningTruncated,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidencePartial},
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
		outputRecoveryContext{
			AgentID: "synthesizer",
			Input:   input,
			Context: []agentapi.ContextBlock{{
				Source:  "workflow.synthesis_objective",
				Content: `{"objective":"Explain the system"}`,
			}},
		},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != agentrun.StatusDone || outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !result.Evidence.ForcedConclusion || !outcome.Evidence.ForcedConclusion {
		t.Fatalf("forced conclusion was not observable: result=%+v outcome=%+v", result.Evidence, outcome.Evidence)
	}
	var answer struct {
		Answer    string `json:"answer"`
		Citations []struct {
			Claim string `json:"claim"`
		} `json:"citations"`
		Limitations []string `json:"limitations"`
	}
	if err := json.Unmarshal(result.Output, &answer); err != nil {
		t.Fatalf("decode recovered answer: %v", err)
	}
	if !strings.Contains(answer.Answer, "The runtime uses a durable workflow.") {
		t.Fatalf("answer = %q", answer.Answer)
	}
	if len(answer.Citations) != 2 ||
		answer.Citations[0].Claim != "The runtime uses a durable workflow." ||
		len(answer.Limitations) != 1 ||
		answer.Limitations[0] != "Live logs were unavailable." {
		t.Fatalf("recovered answer = %+v", answer)
	}
}

func TestMapResultRecoversAnswerTruncatedSynthesizerFromVerifiedBundle(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	result, outcome := mapResult(
		"run-synthesis-answer-truncated",
		&execution.RunResult{
			Answer:   "{\"answer\":\"partial",
			Err:      execution.ErrAnswerTruncated,
			Evidence: agentrun.EvidenceMetrics{Status: agentrun.EvidencePartial},
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
		outputRecoveryContext{
			AgentID: "synthesizer",
			Input:   verifiedBundleForAnswerRecovery(),
		},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != agentrun.StatusDone || outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !result.Evidence.ForcedConclusion || !outcome.Evidence.ForcedConclusion {
		t.Fatalf("forced conclusion was not observable: result=%+v outcome=%+v", result.Evidence, outcome.Evidence)
	}
}

func TestMapResultDoesNotRecoverUnclassifiedSynthesizerFailure(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	result, outcome := mapResult(
		"run-synthesis-failed",
		&execution.RunResult{Err: errors.New("provider authentication failed")},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
		outputRecoveryContext{
			AgentID: "synthesizer",
			Input:   verifiedBundleForAnswerRecovery(),
		},
	)
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		result.Error.Code != "agent_failed" {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != agentrun.StatusFailed ||
		outcome.Err == nil ||
		outcome.Err.Error() != "provider authentication failed" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestMapResultRecoversValidTruncatedInvestigationReport(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	answer := `{
		"focus":"code",
		"summary":"code report",
		"findings":[{
			"claim":"The route reaches the handler.",
			"goal_ids":["core_flow"],
			"evidence":[{"kind":"code","reference":"router.go","summary":"Route registration"}],
			"confidence":0.9
		}],
		"gaps":[],
		"covered_goals":["core_flow"],
		"unresolved_goals":[]
	}`
	result, outcome := mapResult(
		"run-investigator-valid-truncated",
		&execution.RunResult{
			Answer: answer,
			Err:    execution.ErrAnswerTruncated,
			Evidence: agentrun.EvidenceMetrics{
				Status: agentrun.EvidencePartial,
			},
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{
			AgentID: "investigator.code",
			Input:   investigationReportRecoveryContract(),
		},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil ||
		outcome.Status != agentrun.StatusDone || outcome.Err != nil {
		t.Fatalf("result=%+v outcome=%+v", result, outcome)
	}
	if !result.Evidence.ForcedConclusion || !outcome.Evidence.ForcedConclusion {
		t.Fatalf("forced conclusion was not observable: result=%+v outcome=%+v", result.Evidence, outcome.Evidence)
	}
	var report struct {
		Findings []struct {
			Claim string `json:"claim"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if len(report.Findings) != 1 ||
		report.Findings[0].Claim != "The route reaches the handler." {
		t.Fatalf("recovered report = %+v", report)
	}
}

func TestMapResultRepairsTruncatedInvestigationJSON(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	answer := `{
		"focus":"code",
		"summary":"code report",
		"findings":[],
		"gaps":["No evidence-backed finding was completed."],
		"covered_goals":[],
		"unresolved_goals":["core_flow"]`
	result, outcome := mapResult(
		"run-investigator-repair-truncated",
		&execution.RunResult{
			Answer: answer,
			Err:    execution.ErrAnswerTruncated,
			Evidence: agentrun.EvidenceMetrics{
				Status: agentrun.EvidencePartial,
			},
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{
			AgentID: "investigator.code",
			Input:   investigationReportRecoveryContract(),
		},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil ||
		outcome.Status != agentrun.StatusDone || outcome.Err != nil {
		t.Fatalf("result=%+v outcome=%+v", result, outcome)
	}
	if err := registry.Validate(
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		result.Output,
	); err != nil {
		t.Fatalf("repaired output: %v", err)
	}
}

func TestMapResultRecoversEmptyTruncatedInvestigationReportWithEvidence(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	unit := tool.EvidenceUnit{
		SourceKind: "code",
		Target:     "router.go",
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}
	result, outcome := mapResult(
		"run-investigator-empty-truncated",
		&execution.RunResult{
			Err:           execution.ErrReasoningTruncated,
			Evidence:      agentrun.EvidenceMetrics{Status: agentrun.EvidencePartial},
			EvidenceUnits: []tool.EvidenceUnit{unit},
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{
			AgentID: "investigator.code",
			Input:   investigationReportRecoveryContract(),
		},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil ||
		outcome.Status != agentrun.StatusDone || outcome.Err != nil {
		t.Fatalf("result=%+v outcome=%+v", result, outcome)
	}
	if !result.Evidence.ForcedConclusion ||
		len(result.EvidenceUnits) != 1 ||
		result.EvidenceUnits[0].Target != unit.Target {
		t.Fatalf("recovered evidence = %+v units=%+v", result.Evidence, result.EvidenceUnits)
	}
	var report struct {
		Summary         string   `json:"summary"`
		Findings        []any    `json:"findings"`
		Gaps            []string `json:"gaps"`
		CoveredGoals    []string `json:"covered_goals"`
		UnresolvedGoals []string `json:"unresolved_goals"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if len(report.Findings) != 0 ||
		len(report.CoveredGoals) != 0 ||
		len(report.UnresolvedGoals) != 1 ||
		report.UnresolvedGoals[0] != "core_flow" ||
		len(report.Gaps) != 1 ||
		!strings.Contains(report.Summary, "Evidence collection completed") {
		t.Fatalf("recovered report = %+v", report)
	}
}

func TestMapResultDoesNotRecoverUnclassifiedInvestigationFailure(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	providerErr := errors.New("provider authentication failed")
	result, outcome := mapResult(
		"run-investigator-provider-failed",
		&execution.RunResult{
			Answer: `{"focus":"code"}`,
			Err:    providerErr,
		},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{
			AgentID: "investigator.code",
			Input:   investigationReportRecoveryContract(),
		},
	)
	if result.Status != agentapi.RunFailed || result.Error == nil ||
		result.Error.Code != "agent_failed" ||
		outcome.Status != agentrun.StatusFailed ||
		!errors.Is(outcome.Err, providerErr) {
		t.Fatalf("result=%+v outcome=%+v", result, outcome)
	}
}

func investigationReportRecoveryContract() json.RawMessage {
	return json.RawMessage(`{
		"task_id":"task-1",
		"objective":"inspect code",
		"entities":[],
		"evidence_goals":[{
			"id":"goal-1",
			"facet":"core_flow",
			"required":true,
			"sources":["internal"],
			"freshness":"stable",
			"minimum_coverage":1
		}],
		"context":{}
	}`)
}

func verifiedBundleForAnswerRecovery() json.RawMessage {
	return json.RawMessage(`{
		"supported_claims":[{
			"producer_node_id":"investigate.code",
			"finding_index":0,
			"claim":"The runtime uses a durable workflow.",
			"goal_ids":["core_flow"],
			"evidence":[{"kind":"code","reference":"runtime.go","summary":"Workflow execution entrypoint"}],
			"evidence_identities":[{"source_kind":"code","target":"runtime.go","section":"L1-L20"}],
			"confidence":0.95,
			"support":"supported",
			"high_risk":false
		}],
		"partial_claims":[{
			"producer_node_id":"investigate.runtime",
			"finding_index":0,
			"claim":"Live runtime behavior could not be fully confirmed.",
			"goal_ids":["runtime_and_operations"],
			"evidence":[{"kind":"runbook","reference":"operations","summary":"Documented runtime behavior"}],
			"evidence_identities":[{"source_kind":"runbook","target":"operations","section":"runtime"}],
			"confidence":0.6,
			"support":"partial",
			"high_risk":false
		}],
		"unsupported_claims":[],
		"partial_goals":["runtime_and_operations"],
		"unresolved_goals":[],
		"limitations":["Live logs were unavailable."],
		"evidence_units":[],
		"evidence_conflicts":[],
		"omissions":{"claims":0,"goals":0,"limitations":0,"evidence_units":0,"evidence_conflicts":0},
		"verification":{"decision":"partial","stop_reason":"capability_unavailable"},
		"completeness":"partial",
		"limitations_detail":{
			"artifact_id":"art_00000000-0000-0000-0000-000000000000",
			"total_count":1,
			"displayed_count":1,
			"omitted_count":0,
			"normalization_version":"limitations-v1"
		}
	}`)
}

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

func TestValidatedInvestigationReportNormalizesEvidenceMetadata(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	validEvidenceID := "ev_" + strings.Repeat("a", 64)
	report := map[string]any{
		"focus":   "code",
		"summary": "code report",
		"findings": []any{
			map[string]any{
				"claim":    "The request reaches the handler.",
				"goal_ids": []string{"core_flow"},
				"evidence": []any{
					map[string]any{
						"kind":        "code",
						"reference":   "handler",
						"summary":     "handler implementation",
						"evidence_id": validEvidenceID,
						"identity":    "service-uuid-must-not-be-an-identity",
					},
					map[string]any{
						"kind":      "code",
						"reference": "handler:validation",
						"summary":   "validation branch",
						"identity": map[string]any{
							"source_kind": "code",
							"target":      "handler:validation",
							"section":     "L10-L20",
						},
					},
					map[string]any{
						"kind":        "code",
						"reference":   "handler:legacy",
						"summary":     "legacy reference",
						"evidence_id": "service-uuid",
					},
				},
				"confidence": 0.9,
			},
		},
		"gaps":             []string{},
		"covered_goals":    []string{"core_flow"},
		"unresolved_goals": []string{},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	got, err := validatedOutput(
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		string(raw),
	)
	if err != nil {
		t.Fatalf("validatedOutput(): %v", err)
	}

	var normalized struct {
		Findings []struct {
			Evidence []map[string]any `json:"evidence"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(got, &normalized); err != nil {
		t.Fatalf("decode normalized report: %v", err)
	}
	evidence := normalized.Findings[0].Evidence
	if _, ok := evidence[0]["identity"]; ok {
		t.Fatalf("string identity was retained: %#v", evidence[0])
	}
	if evidence[0]["evidence_id"] != validEvidenceID {
		t.Fatalf("valid evidence handle was removed: %#v", evidence[0])
	}
	if _, ok := evidence[1]["identity"].(map[string]any); !ok {
		t.Fatalf("canonical identity was removed: %#v", evidence[1])
	}
	if _, ok := evidence[2]["evidence_id"]; ok {
		t.Fatalf("invalid evidence handle was retained: %#v", evidence[2])
	}
}

func TestPublicOutputTextExtractsOnlyTheTopLevelAnswer(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "structured answer",
			output: `{"answer":"grounded answer","supported_claims":[{"claim":"internal"}],"verification":{"decision":"partial"}}`,
			want:   "grounded answer",
		},
		{
			name:   "json string",
			output: `"plain answer"`,
			want:   "plain answer",
		},
		{
			name:   "structured output without answer",
			output: `{"supported_claims":[],"verification":{"decision":"partial"},"evidence_units":[]}`,
			want:   "",
		},
		{
			name:   "invalid output",
			output: `{"answer":`,
			want:   "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publicOutputText(json.RawMessage(test.output)); got != test.want {
				t.Fatalf("publicOutputText() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMapResultDoesNotExposeStructuredInvestigationFields(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish([]agentapi.SchemaDefinition{{
		ID: "test.structured-answer", Version: 1,
		Document: json.RawMessage(`{
			"type":"object",
			"required":["answer","supported_claims","partial_claims","unsupported_claims","verification","evidence_units","unavailable_tasks"],
			"properties":{
				"answer":{"type":"string"},
				"supported_claims":{"type":"array"},
				"partial_claims":{"type":"array"},
				"unsupported_claims":{"type":"array"},
				"verification":{"type":"object"},
				"evidence_units":{"type":"array"},
				"unavailable_tasks":{"type":"array"}
			},
			"additionalProperties":false
		}`),
	}}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	output := `{"answer":"natural-language answer","supported_claims":[],"partial_claims":[],"unsupported_claims":[],"verification":{"decision":"partial"},"evidence_units":[],"unavailable_tasks":[]}`
	result, outcome := mapResult(
		"run-structured-answer",
		&execution.RunResult{Answer: output},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "test.structured-answer", Version: 1},
	)
	if result.Status != agentapi.RunSucceeded || outcome.Status != agentrun.StatusDone {
		t.Fatalf("result=%+v outcome=%+v", result, outcome)
	}
	if result.Text != "natural-language answer" || outcome.Answer != result.Text {
		t.Fatalf("public answer = %q, outcome = %q", result.Text, outcome.Answer)
	}
	for _, internalField := range []string{
		"supported_claims", "partial_claims", "unsupported_claims",
		"verification", "evidence_units", "unavailable_tasks",
	} {
		if strings.Contains(result.Text, internalField) {
			t.Fatalf("public answer leaked %q: %q", internalField, result.Text)
		}
	}
	if string(result.Output) != output {
		t.Fatalf("structured output changed: %s", result.Output)
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

func TestMapResultPreservesInvestigationFindingsWhenGoalCoverageIsMissing(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	input := json.RawMessage(`{
		"task_id":"task-1",
		"objective":"inspect docs",
		"entities":[],
		"evidence_goals":[
			{"id":"goal-1","facet":"business_domain","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1},
			{"id":"goal-2","facet":"runtime_and_operations","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}
		],
		"context":{}
	}`)
	answer := `{
		"focus":"docs",
		"summary":"domain report",
		"findings":[{
			"claim":"The service belongs to the smart-home domain.",
			"goal_ids":["business_domain"],
			"evidence":[{"kind":"runbook","reference":"architecture","summary":"Domain overview"}],
			"confidence":0.9
		}],
		"gaps":[]
	}`
	result, outcome := mapResult(
		"run-preserve-findings",
		&execution.RunResult{Answer: answer},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{AgentID: "investigator.docs", Input: input},
	)
	if result.Status != agentapi.RunSucceeded || result.Error != nil {
		t.Fatalf("result = %+v", result)
	}
	if outcome.Status != agentrun.StatusDone || outcome.ErrorCode != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	var report struct {
		Findings        []map[string]any `json:"findings"`
		CoveredGoals    []string         `json:"covered_goals"`
		UnresolvedGoals []string         `json:"unresolved_goals"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings were not preserved: %+v", report)
	}
	if len(report.CoveredGoals) != 1 || report.CoveredGoals[0] != "business_domain" {
		t.Fatalf("covered goals = %v", report.CoveredGoals)
	}
	if len(report.UnresolvedGoals) != 1 || report.UnresolvedGoals[0] != "runtime_and_operations" {
		t.Fatalf("unresolved goals = %v", report.UnresolvedGoals)
	}
}

func TestMapResultRebuildsPartialInvestigationGoalCoverage(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	input := json.RawMessage(`{
		"task_id":"task-1",
		"objective":"inspect code",
		"entities":[],
		"evidence_goals":[
			{"id":"goal-1","facet":"entrypoint","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1},
			{"id":"goal-2","facet":"core_flow","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}
		],
		"context":{}
	}`)
	answer := `{
		"focus":"code",
		"summary":"code report",
		"findings":[{
			"claim":"The route reaches the handler.",
			"goal_ids":["entrypoint"],
			"evidence":[{"kind":"code","reference":"router.go","summary":"Route registration"}],
			"confidence":0.9
		}],
		"gaps":[],
		"covered_goals":["stale"]
	}`
	result, _ := mapResult(
		"run-rebuild-coverage",
		&execution.RunResult{Answer: answer},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{AgentID: "investigator.code", Input: input},
	)
	var report struct {
		Findings        []map[string]any `json:"findings"`
		CoveredGoals    []string         `json:"covered_goals"`
		UnresolvedGoals []string         `json:"unresolved_goals"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if len(report.Findings) != 1 ||
		len(report.CoveredGoals) != 1 || report.CoveredGoals[0] != "entrypoint" ||
		len(report.UnresolvedGoals) != 1 || report.UnresolvedGoals[0] != "core_flow" {
		t.Fatalf("recovered report = %+v", report)
	}
}

func TestMapResultDoesNotPreserveInvestigationFindingsWithUnknownGoals(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	input := json.RawMessage(`{
		"task_id":"task-1",
		"objective":"inspect docs",
		"entities":[],
		"evidence_goals":[{"id":"goal-1","facet":"business_domain","required":true,"sources":["internal"],"freshness":"stable","minimum_coverage":1}],
		"context":{}
	}`)
	answer := `{
		"focus":"docs",
		"summary":"domain report",
		"findings":[{
			"claim":"An unrelated claim.",
			"goal_ids":["unknown_goal"],
			"evidence":[{"kind":"runbook","reference":"architecture","summary":"Overview"}],
			"confidence":0.9
		}],
		"gaps":[]
	}`
	result, _ := mapResult(
		"run-unknown-goal",
		&execution.RunResult{Answer: answer},
		nil,
		nil,
		agentapi.Usage{},
		nil,
		registry,
		agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		outputRecoveryContext{AgentID: "investigator.docs", Input: input},
	)
	var report struct {
		Findings        []map[string]any `json:"findings"`
		CoveredGoals    []string         `json:"covered_goals"`
		UnresolvedGoals []string         `json:"unresolved_goals"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode recovered report: %v", err)
	}
	if len(report.Findings) != 0 || len(report.CoveredGoals) != 0 ||
		len(report.UnresolvedGoals) != 1 || report.UnresolvedGoals[0] != "business_domain" {
		t.Fatalf("unknown goal was accepted: %+v", report)
	}
}
