package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAICompatibleRequestUsesExactlyOneCompletionLimitField(t *testing.T) {
	tests := []struct {
		name            string
		client          *LLMClient
		parameters      ModelParameters
		wantLimit       string
		wantReasoning   string
		forbidReasoning bool
	}{
		{
			name:       "conservative deepseek profile",
			client:     nil,
			parameters: ModelParameters{},
			wantLimit:  "max_tokens",
		},
		{
			name: "openai reasoning profile",
			parameters: ModelParameters{
				ReasoningMode:   ReasoningEnabled,
				ReasoningEffort: "low",
			},
			wantLimit:     "max_completion_tokens",
			wantReasoning: "low",
		},
		{
			name: "reasoning disabled",
			parameters: ModelParameters{
				ReasoningMode:   ReasoningDisabled,
				ReasoningEffort: "high",
			},
			wantLimit:       "max_completion_tokens",
			forbidReasoning: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			client := test.client
			if client == nil {
				if test.name == "conservative deepseek profile" {
					client = NewLLMClientWithHTTPAndProvider(server.URL, "key", "deepseek-v4-flash", "openai", 100, server.Client())
				} else {
					client = NewLLMClientWithHTTPAndCapability(server.URL, "key", "o3-mini", "openai", 100, OpenAIReasoningCapability("o3-mini"), server.Client())
				}
			}
			if _, err := client.ChatWithToolsMaxWithParameters(
				t.Context(), []Message{{Role: "user", Content: "q"}}, nil, nil, 100, test.parameters,
			); err != nil {
				t.Fatalf("ChatWithToolsMaxWithParameters: %v", err)
			}

			for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
				_, present := body[field]
				if field == test.wantLimit {
					if !present {
						t.Fatalf("request missing %s: %#v", field, body)
					}
				} else if present {
					t.Fatalf("request must not contain %s: %#v", field, body)
				}
			}
			if test.wantReasoning != "" {
				var got string
				if err := json.Unmarshal(body["reasoning_effort"], &got); err != nil {
					t.Fatalf("decode reasoning_effort: %v", err)
				}
				if got != test.wantReasoning {
					t.Fatalf("reasoning_effort = %q, want %q", got, test.wantReasoning)
				}
			} else if test.forbidReasoning {
				if _, present := body["reasoning_effort"]; present {
					t.Fatalf("request must not contain reasoning_effort: %#v", body)
				}
			}
		})
	}
}

func TestOpenAICompatibleRequestCanUseMaxOutputTokensCapability(t *testing.T) {
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	capability := DefaultModelCapability("openai", "responses-gateway")
	capability.CompletionLimitField = CompletionLimitMaxOutputTokens
	client := NewLLMClientWithHTTPAndCapability(server.URL, "key", "responses-gateway", "openai", 100, capability, server.Client())
	if _, err := client.ChatWithToolsMax(t.Context(), []Message{{Role: "user", Content: "q"}}, nil, nil, 37); err != nil {
		t.Fatalf("ChatWithToolsMax: %v", err)
	}
	if _, ok := body["max_output_tokens"]; !ok {
		t.Fatalf("request = %#v, want max_output_tokens", body)
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := body[field]; ok {
			t.Fatalf("request must not contain %s: %#v", field, body)
		}
	}
}
