package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareModelParametersByProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		raw      map[string]any
		want     map[string]any
	}{
		{
			name:     "openai",
			provider: "openai",
			raw: map[string]any{
				"temperature":       0.2,
				"top_p":             0.8,
				"stop":              []any{"END"},
				"frequency_penalty": -0.4,
				"presence_penalty":  0.6,
			},
			want: map[string]any{
				"temperature":       0.2,
				"top_p":             0.8,
				"stop":              []string{"END"},
				"frequency_penalty": -0.4,
				"presence_penalty":  0.6,
			},
		},
		{
			name:     "anthropic",
			provider: "anthropic",
			raw: map[string]any{
				"temperature": 0.2,
				"top_p":       0.8,
				"stop":        "END",
				"top_k":       json.Number("32"),
			},
			want: map[string]any{
				"temperature": 0.2,
				"top_p":       0.8,
				"stop":        []string{"END"},
				"top_k":       32,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters, err := PrepareModelParameters(test.provider, test.raw)
			if err != nil {
				t.Fatalf("PrepareModelParameters: %v", err)
			}
			if got := parameters.Snapshot(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("snapshot = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestPrepareModelParametersRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		raw      map[string]any
		want     string
	}{
		{
			name:     "unknown",
			provider: "openai",
			raw:      map[string]any{"seed": 1},
			want:     `model parameter "seed" is not supported`,
		},
		{
			name:     "provider mismatch",
			provider: "anthropic",
			raw:      map[string]any{"frequency_penalty": 1},
			want:     `model parameter "frequency_penalty" is not supported`,
		},
		{
			name:     "wrong type",
			provider: "openai",
			raw:      map[string]any{"temperature": "warm"},
			want:     `model parameter "temperature" must be numeric`,
		},
		{
			name:     "temperature out of range",
			provider: "anthropic",
			raw:      map[string]any{"temperature": 1.1},
			want:     `model parameter "temperature" must be between 0 and 1`,
		},
		{
			name:     "stop empty",
			provider: "openai",
			raw:      map[string]any{"stop": []any{}},
			want:     `model parameter "stop" must contain between 1 and 4`,
		},
		{
			name:     "stop item empty",
			provider: "openai",
			raw:      map[string]any{"stop": []any{""}},
			want:     `model parameter "stop" item 0 must not be empty`,
		},
		{
			name:     "top k fractional",
			provider: "anthropic",
			raw:      map[string]any{"top_k": 1.5},
			want:     `model parameter "top_k" must be a non-negative integer`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareModelParameters(test.provider, test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPrepareModelParametersReasoningControls(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  map[string]any
		want ModelParameters
	}{
		{
			name: "enabled with effort",
			raw:  map[string]any{"reasoning_mode": " ENABLED ", "reasoning_effort": " HIGH "},
			want: ModelParameters{ReasoningMode: ReasoningEnabled, ReasoningEffort: "high"},
		},
		{
			name: "disabled",
			raw:  map[string]any{"reasoning_mode": "disabled", "reasoning_effort": "none"},
			want: ModelParameters{ReasoningMode: ReasoningDisabled, ReasoningEffort: "none"},
		},
		{
			name: "default",
			raw:  map[string]any{"reasoning_mode": "DEFAULT"},
			want: ModelParameters{ReasoningMode: ReasoningDefault},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := PrepareModelParameters("openai", test.raw)
			if err != nil {
				t.Fatalf("PrepareModelParameters: %v", err)
			}
			if got.ReasoningMode != test.want.ReasoningMode || got.ReasoningEffort != test.want.ReasoningEffort {
				t.Fatalf("parameters = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestPrepareModelParametersRejectsInvalidReasoningControls(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "mode type",
			raw:  map[string]any{"reasoning_mode": true},
			want: `model parameter "reasoning_mode" must be a string`,
		},
		{
			name: "mode value",
			raw:  map[string]any{"reasoning_mode": "auto"},
			want: `model parameter "reasoning_mode" must be one of default, enabled, disabled`,
		},
		{
			name: "effort type",
			raw:  map[string]any{"reasoning_effort": 1},
			want: `model parameter "reasoning_effort" must be a string`,
		},
		{
			name: "effort value",
			raw:  map[string]any{"reasoning_effort": "extreme"},
			want: `model parameter "reasoning_effort" must be one of none, low, medium, high`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareModelParameters("openai", test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestModelParametersReasoningSnapshotAndWithoutReasoning(t *testing.T) {
	parameters, err := PrepareModelParameters("openai", map[string]any{
		"reasoning_mode":   "enabled",
		"reasoning_effort": "medium",
	})
	if err != nil {
		t.Fatalf("PrepareModelParameters: %v", err)
	}
	if got, want := parameters.Snapshot(), map[string]any{
		"reasoning_mode":   "enabled",
		"reasoning_effort": "medium",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}

	without := parameters.WithoutReasoning()
	if parameters.ReasoningMode != ReasoningEnabled || parameters.ReasoningEffort != "medium" {
		t.Fatalf("WithoutReasoning mutated source = %+v", parameters)
	}
	if without.ReasoningMode != ReasoningDisabled || without.ReasoningEffort != "none" {
		t.Fatalf("without reasoning = %+v", without)
	}
}

func TestModelParametersCloneDetachesStop(t *testing.T) {
	parameters := ModelParameters{Stop: []string{"END", "DONE"}}
	clone := parameters.Clone()
	clone.Stop[0] = "CHANGED"
	if parameters.Stop[0] != "END" {
		t.Fatalf("Clone shared stop slice: source = %#v", parameters.Stop)
	}
}
