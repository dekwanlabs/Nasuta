package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDelegationAdoptionContractValidateAndStrip(t *testing.T) {
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{
			{DelegationID: "del-1", ReportIDs: []string{"report-1", "report-2"}},
			{DelegationID: "del-2"},
		},
	})
	answer := "Visible answer.\n" + delegationAdoptionMarkerPrefix +
		`{"delegations":[` +
		`{"delegation_id":"del-1","adopted_report_ids":["report-2"]},` +
		`{"delegation_id":"del-2","adopted_report_ids":[]}` +
		`]}`

	clean, violations := contract.ValidateAndStrip(answer)
	if len(violations) != 0 {
		t.Fatalf("violations = %#v", violations)
	}
	if clean != "Visible answer." {
		t.Fatalf("clean answer = %q", clean)
	}
	want := []agentapi.DelegationAdoption{
		{
			DelegationID:     "del-1",
			AdoptedReportIDs: []string{"report-2"},
			Status:           agentapi.DelegationAdopted,
		},
		{
			DelegationID: "del-2",
			Status:       agentapi.DelegationNotAdopted,
		},
	}
	if got := contract.Adoptions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("adoptions = %#v, want %#v", got, want)
	}
}

func TestDelegationAdoptionContractRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name: "missing marker",
			want: "marker is missing",
		},
		{
			name: "unknown delegation",
			payload: `{"delegations":[` +
				`{"delegation_id":"del-unknown","adopted_report_ids":[]}` +
				`]}`,
			want: "unknown delegation",
		},
		{
			name: "duplicate delegation",
			payload: `{"delegations":[` +
				`{"delegation_id":"del-1","adopted_report_ids":[]},` +
				`{"delegation_id":"del-1","adopted_report_ids":[]}` +
				`]}`,
			want: "appears more than once",
		},
		{
			name: "unauthorized report",
			payload: `{"delegations":[` +
				`{"delegation_id":"del-1","adopted_report_ids":["report-other"]}` +
				`]}`,
			want: "unauthorized report",
		},
		{
			name: "duplicate report",
			payload: `{"delegations":[` +
				`{"delegation_id":"del-1","adopted_report_ids":["report-1","report-1"]}` +
				`]}`,
			want: "repeats report",
		},
		{
			name:    "missing delegation",
			payload: `{"delegations":[]}`,
			want:    "is missing",
		},
		{
			name: "unknown field",
			payload: `{"delegations":[` +
				`{"delegation_id":"del-1","adopted_report_ids":[]}` +
				`],"extra":true}`,
			want: "unknown field",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := &exactAnswerContract{}
			contract.Add(tool.AnswerContract{
				Delegations: []tool.DelegationAdoptionContract{{
					DelegationID: "del-1",
					ReportIDs:    []string{"report-1"},
				}},
			})
			answer := "Visible answer."
			if test.payload != "" {
				answer += "\n" + delegationAdoptionMarkerPrefix + test.payload
			}
			_, violations := contract.ValidateAndStrip(answer)
			if !strings.Contains(strings.Join(violations, "\n"), test.want) {
				t.Fatalf("violations = %#v, want %q", violations, test.want)
			}
			if contract.Adoptions() != nil {
				t.Fatalf("invalid metadata produced adoptions: %#v", contract.Adoptions())
			}
		})
	}
}

func TestRunStripsDelegationAdoptionMetadataEverywhere(t *testing.T) {
	const (
		delegationID = "del-1"
		reportID     = "report-1"
		visible      = "The delegated evidence supports the conclusion."
	)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-delegate","type":"function","function":{"name":"delegate","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		content := visible + "\n" + delegationAdoptionMarkerPrefix +
			`{"delegations":[{"delegation_id":"del-1","adopted_report_ids":["report-1"]}]}`
		encoded, _ := json.Marshal(streamChunkJS{
			Choices: []streamChoiceJS{{
				Delta:        streamDeltaJS{Content: content},
				FinishReason: "stop",
			}},
		})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID:          "delegate",
		Description: "return delegated evidence",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{
				Content: `{"summary":"delegated evidence"}`,
				AnswerContract: tool.AnswerContract{
					Delegations: []tool.DelegationAdoptionContract{{
						DelegationID: delegationID,
						ReportIDs:    []string{reportID},
					}},
				},
			}, nil
		}),
	})
	observer := &captureObserver{}
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		NewToolExecutor(registry),
		Config{
			MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0,
			Timeout: 5 * time.Second, AnswerReserve: time.Second,
		},
		observer,
		nil,
	)

	result, err := agent.RunWithPlan(
		t.Context(),
		"run-delegation-adoption",
		"Use delegated evidence.",
		nil,
		nil,
		domain.EvidencePlan{Sources: domain.Internal},
		false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan: %v", err)
	}
	if result.Err != nil || result.Answer != visible {
		t.Fatalf("result = %#v", result)
	}
	want := []agentapi.DelegationAdoption{{
		DelegationID:     delegationID,
		AdoptedReportIDs: []string{reportID},
		Status:           agentapi.DelegationAdopted,
	}}
	if !reflect.DeepEqual(result.DelegationAdoptions, want) {
		t.Fatalf("adoptions = %#v, want %#v", result.DelegationAdoptions, want)
	}
	if got := strings.Join(observer.tokens, ""); got != visible {
		t.Fatalf("visible stream = %q", got)
	}
	for _, message := range result.SessionMessages {
		if strings.Contains(message.Content, delegationAdoptionMarkerPrefix) {
			t.Fatalf("session message leaked adoption marker: %#v", message)
		}
	}
	var answerSteps []StepRecord
	for _, step := range observer.steps {
		if step.Kind == StepKindAnswer {
			answerSteps = append(answerSteps, step)
		}
	}
	if len(answerSteps) != 1 ||
		answerSteps[0].Content != visible ||
		!reflect.DeepEqual(answerSteps[0].DelegationAdoptions, want) {
		t.Fatalf("answer steps = %#v", answerSteps)
	}
}

func TestForceConclusionStripsDelegationAdoptionMetadata(t *testing.T) {
	const visible = "Forced visible answer."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		content := visible + "\n" + delegationAdoptionMarkerPrefix +
			`{"delegations":[{"delegation_id":"del-1","adopted_report_ids":[]}]}`
		encoded, _ := json.Marshal(streamChunkJS{
			Choices: []streamChoiceJS{{
				Delta:        streamDeltaJS{Content: content},
				FinishReason: "stop",
			}},
		})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	observer := &captureObserver{}
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		nil,
		Config{ConclusionMaxTokens: 100, MaxContinueRounds: 0},
		observer,
		nil,
	)
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: "del-1",
			ReportIDs:    []string{"report-1"},
		}},
	})
	seq := 0
	res, err := agent.forceConclusion(
		t.Context(),
		"run-force-adoption",
		nil,
		contract,
		&seq,
		time.Now(),
	)
	if err != nil || res == nil || res.Content != visible {
		t.Fatalf("res=%#v err=%v", res, err)
	}
	if got := strings.Join(observer.tokens, ""); got != visible {
		t.Fatalf("visible stream = %q", got)
	}
	if got := contract.Adoptions(); len(got) != 1 ||
		got[0].Status != agentapi.DelegationNotAdopted {
		t.Fatalf("adoptions = %#v", got)
	}
}

func TestFinalizeLoopMarksUnevaluatedDelegationsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		result *RunResult
		reason string
	}{
		{
			name:   "cancelled",
			result: &RunResult{Aborted: true},
			reason: "parent_cancelled",
		},
		{
			name:   "failed",
			result: &RunResult{Err: errors.New("failed")},
			reason: "parent_run_failed",
		},
		{
			name:   "no final answer",
			result: &RunResult{},
			reason: "final_answer_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := &exactAnswerContract{}
			contract.Add(tool.AnswerContract{
				Delegations: []tool.DelegationAdoptionContract{{
					DelegationID: "del-1",
					ReportIDs:    []string{"report-1"},
				}},
			})
			state := &compiledLoop{
				ctx:            t.Context(),
				input:          Input{Direct: true},
				answerContract: contract,
				result:         test.result,
				evidenceLedger: newRunEvidenceLedger(nil, nil),
			}
			NewAgent(nil, nil, Config{}, nil, nil).finalizeLoop(state)
			got := state.result.DelegationAdoptions
			if len(got) != 1 ||
				got[0].DelegationID != "del-1" ||
				got[0].Status != agentapi.DelegationUnknown ||
				got[0].Reason != test.reason {
				t.Fatalf("adoptions = %#v", got)
			}
		})
	}
}
