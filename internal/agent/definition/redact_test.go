package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestRedactDefinitionRequestRehashesContext(t *testing.T) {
	originalHash := strings.Repeat("a", 64)
	request := agentapi.RunRequest{
		Policy: agentapi.RunPolicy{RedactSensitive: true},
		Input:  json.RawMessage(`{"question":"Authorization: Bearer input-secret"}`),
		Messages: []agentapi.Message{{
			Role: "assistant", Content: "api_key=message-secret",
			ToolCalls: []agentapi.ToolCall{{
				ID: "call-1", Type: "function",
				Function: agentapi.ToolFunction{
					Name: "lookup", Arguments: `{"token":"argument-secret"}`,
				},
			}},
		}},
		Context: []agentapi.ContextBlock{{
			Source:  "mysql://app:source-secret@db/service",
			Title:   "password=title-secret",
			Content: `{"authorization":"Bearer context-secret","dsn":"postgres://app:database-secret@db/service"}`,
			References: []agentapi.Reference{{
				Label:  "access_token=label-secret",
				Target: "https://app:target-secret@example.com/path",
			}},
			ContentHash: originalHash,
		}},
	}

	redacted := redactRequest(request)

	assertSensitiveValuesAbsent(t, redacted, []string{
		"input-secret", "message-secret", "argument-secret", "source-secret",
		"title-secret", "context-secret", "database-secret", "label-secret", "target-secret",
	})
	if !json.Valid(redacted.Input) {
		t.Fatalf("redacted input is not JSON: %s", redacted.Input)
	}
	if got, want := redacted.Context[0].ContentHash, hashString(redacted.Context[0].Content); got != want {
		t.Fatalf("context hash = %q, want %q", got, want)
	}
	if redacted.Context[0].ContentHash == originalHash {
		t.Fatal("context hash was not recomputed after redaction")
	}
	if !strings.Contains(string(request.Input), "input-secret") ||
		!strings.Contains(request.Messages[0].ToolCalls[0].Function.Arguments, "argument-secret") ||
		!strings.Contains(request.Context[0].References[0].Target, "target-secret") {
		t.Fatal("redaction mutated the caller-owned request")
	}
}

func TestRedactDefinitionStepRehashesPersistedPayloads(t *testing.T) {
	step := StepRecord{
		Content:             "Authorization: Bearer content-secret",
		PromptContent:       "api_key=prompt-secret",
		AuthoritativeSHA256: strings.Repeat("a", 64),
		PromptSHA256:        strings.Repeat("b", 64),
		SizeBytes:           999,
		Args:                `{"token":"argument-secret"}`,
		ResultPreview:       "password=result-secret",
		DeliveryError:       "postgres://app:delivery-secret@db/service",
		AnswerContract: tool.AnswerContract{
			RequiredLiterals: []string{"client_secret=literal-secret"},
		},
	}

	redacted := redactStep(step)

	assertSensitiveValuesAbsent(t, redacted, []string{
		"content-secret", "prompt-secret", "argument-secret", "result-secret",
		"delivery-secret", "literal-secret",
	})
	if redacted.SizeBytes != int64(len(redacted.Content)) {
		t.Fatalf("content size = %d, want %d", redacted.SizeBytes, len(redacted.Content))
	}
	if redacted.AuthoritativeSHA256 != hashString(redacted.Content) ||
		redacted.PromptSHA256 != hashString(redacted.PromptContent) {
		t.Fatalf(
			"redacted hashes = content:%q prompt:%q",
			redacted.AuthoritativeSHA256,
			redacted.PromptSHA256,
		)
	}
	if strings.Contains(redacted.AuthoritativeSHA256, strings.Repeat("a", 64)) ||
		strings.Contains(redacted.PromptSHA256, strings.Repeat("b", 64)) {
		t.Fatal("step retained a pre-redaction hash")
	}
	if !strings.Contains(step.AnswerContract.RequiredLiterals[0], "literal-secret") {
		t.Fatal("redaction mutated the caller-owned answer contract")
	}
}

func TestRedactDefinitionResultAndOutcome(t *testing.T) {
	result := redactResult(agentapi.RunResult{
		Output: json.RawMessage(`{"summary":"Authorization: Bearer output-secret"}`),
		Text:   "access_token=text-secret",
		References: []agentapi.Reference{{
			Label: "password=label-secret", Target: "https://app:target-secret@example.com/path",
		}},
		Messages: []agentapi.Message{{
			Content: "api_key=message-secret",
			ToolCalls: []agentapi.ToolCall{{
				Function: agentapi.ToolFunction{Arguments: `{"token":"argument-secret"}`},
			}},
		}},
		Error: &agentapi.RunError{Message: "client_secret=error-secret"},
	})
	outcome := redactOutcome(RunOutcome{
		Answer: "Authorization: Bearer answer-secret",
		SessionMessages: []llm.Message{{
			Content: "password=session-secret",
			ToolCalls: []llm.ToolCall{{
				Function: llm.ToolFunction{Arguments: `{"token":"tool-secret"}`},
			}},
		}},
		References: []agentapi.Reference{{
			Label: "api_key=outcome-label-secret",
		}},
		Err: errors.New("postgres://app:outcome-error-secret@db/service"),
	})

	assertSensitiveValuesAbsent(t, result, []string{
		"output-secret", "text-secret", "label-secret", "target-secret",
		"message-secret", "argument-secret", "error-secret",
	})
	assertSensitiveValuesAbsent(t, outcome, []string{
		"answer-secret", "session-secret", "tool-secret",
		"outcome-label-secret", "outcome-error-secret",
	})
	if !json.Valid(result.Output) {
		t.Fatalf("redacted result output is not JSON: %s", result.Output)
	}
}

func TestDefinitionRuntimeRedactsRunInputBeforePersistence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	definition := testQADefinition(t, nil)
	request := testQARequest(definition)
	request.Policy.RedactSensitive = true
	request.Input = json.RawMessage(`{"question":"Authorization: Bearer persisted-input-secret"}`)
	expectedInput := platform.RedactSensitiveText(string(request.Input))
	mock.ExpectExec("INSERT INTO agent_runs").WithArgs(
		request.RunID,
		RunKindAgent,
		request.Actor.UserID,
		request.Correlation.SessionID,
		definition.ID,
		definition.Version,
		definition.ContentHash,
		[]byte(`{}`),
		sqlmock.AnyArg(),
		definition.InputSchema.Version,
		definition.OutputSchema.Version,
		"",
		"",
		int64(0),
		"",
		"",
		0,
		sqlmock.AnyArg(),
		uint64(0),
		"",
		"",
		expectedInput,
		RunStatusRunning,
		"",
		"single",
		definition.Budget.MaxSteps,
		0,
		0,
		sqlmock.AnyArg(),
	).WillReturnResult(sqlmock.NewResult(1, 1))
	runtime := newTestDefinitionRuntime(
		t,
		definition,
		tool.NewRegistry(),
		testRuntimeSettings("http://unused"),
		bindRunStore(db),
	)

	if _, err := runtime.Begin(t.Context(), runStart(request)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionRuntimeRedactsPublicAndTerminalResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		answer := `{"coverage":[],"findings":[],"uncertainties":[],"summary":"Authorization: Bearer result-secret"}`
		payload, err := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"delta":         map[string]any{"content": answer},
				"finish_reason": "stop",
			}},
		})
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\n", payload)
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(
		t, definition, tool.NewRegistry(), testRuntimeSettings(server.URL), nil,
	)
	request := testDefinitionRequest(definition)
	request.Policy.RedactSensitive = true
	events := runtime.Hub().Subscribe(request.RunID)

	result, err := runtime.Run(t.Context(), request)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != agentapi.RunSucceeded {
		t.Fatalf("result = %+v", result)
	}
	assertSensitiveValuesAbsent(t, result, []string{"result-secret"})
	terminal := waitForTerminal(t, events)
	assertSecretsAbsent(t, terminal, []string{"result-secret"})
}

func assertSensitiveValuesAbsent(t *testing.T, value any, secrets []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal redacted value: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret %q leaked: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), platform.RedactedValue) {
		t.Fatalf("redaction marker missing: %s", encoded)
	}
}

func assertSecretsAbsent(t *testing.T, value any, secrets []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret %q leaked: %s", secret, encoded)
		}
	}
}
