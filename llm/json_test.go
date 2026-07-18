package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStripFences(t *testing.T) {
	cases := map[string]string{
		"```json block": "```json\n{\"a\":1}\n```",
		"plain fence":   "```\n{\"a\":1}\n```",
		"no fence":      `{"a":1}`,
	}
	for _, in := range cases {
		if StripFences(in) != `{"a":1}` {
			t.Fatalf("StripFences(%q) != %q", in, `{"a":1}`)
		}
	}
}

func TestParseJSONLoose(t *testing.T) {
	var got map[string]any
	if err := ParseJSONLoose("here you go:\n```json\n{\"a\":1}\n```", &got); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["a"] != float64(1) {
		t.Fatalf("got=%v", got)
	}

	var arr []any
	if err := ParseJSONLoose("wrap [1,2,3] done", &arr); err != nil {
		t.Fatalf("array err: %v", err)
	}
	if len(arr) != 3 {
		t.Fatalf("arr=%v", arr)
	}

	var nested map[string]any
	input := `prefix {"text":"literal } and [", "nested":{"ok":true}} suffix`
	if err := ParseJSONLoose(input, &nested); err != nil {
		t.Fatalf("string-aware extraction: %v", err)
	}
	if nested["text"] != "literal } and [" {
		t.Fatalf("nested = %#v", nested)
	}
}

func TestRepairJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  map[string]any
	}{
		{"trailing comma object", `{"a":1,"b":2,}`, map[string]any{"a": 1.0, "b": 2.0}},
		{"trailing comma array", `{"a":[1,2,]}`, map[string]any{"a": []any{1.0, 2.0}}},
		{"line comment", "{\n  \"a\": 1 // a comment\n}", map[string]any{"a": 1.0}},
		{"block comment", `{"a":1 /* c */}`, map[string]any{"a": 1.0}},
		{"unquoted key", `{a: "v"}`, map[string]any{"a": "v"}},
		{"unquoted key hyphen", `{service-name: "x"}`, map[string]any{"service-name": "x"}},
		{"bare newline in string", `{"a":"line1` + "\n" + `line2"}`, map[string]any{"a": "line1\nline2"}},
		{"truncated object", `{"a":1,"b":2`, map[string]any{"a": 1.0, "b": 2.0}},
		{"truncated nested", `{"a":[1,2`, map[string]any{"a": []any{1.0, 2.0}}},
		{"already valid", `{"a":1}`, map[string]any{"a": 1.0}},
		{"string preserves comma-brace", `{"a":"x,}"}`, map[string]any{"a": "x,}"}},
		{"string preserves slash-slash", `{"a":"http://x"}`, map[string]any{"a": "http://x"}},
		{"block comment keeps token boundary", `{"a": 1/* c */2}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repaired := RepairJSON(c.input)
			var got map[string]any
			err := json.Unmarshal([]byte(repaired), &got)
			if c.want == nil {
				if err == nil {
					t.Fatalf("unsafe repair became valid: %q", repaired)
				}
				return
			}
			if err != nil {
				t.Fatalf("repaired=%q unmarshal error: %v", repaired, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got=%v want=%v (repaired=%q)", got, c.want, repaired)
			}
		})
	}
}

// Documented boundaries: these defects are intentionally NOT repaired and must
// fall through to a model reprompt. The repaired output must remain invalid JSON
// rather than silently producing wrong data.
func TestRepairJSONBoundaries(t *testing.T) {
	concatenated := `{"a":1}{"b":2}`
	if err := json.Unmarshal([]byte(RepairJSON(concatenated)), &map[string]any{}); err == nil {
		t.Fatal("concatenated objects should remain invalid (fall to reprompt)")
	}
	barewordValue := `{a: ok}`
	if err := json.Unmarshal([]byte(RepairJSON(barewordValue)), &map[string]any{}); err == nil {
		t.Fatal("bareword value should remain invalid (fall to reprompt)")
	}
}
