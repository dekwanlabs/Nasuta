package codingagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

func TestCodexEventParserDropsReasoningAndMapsCommands(t *testing.T) {
	parsed, err := parseCodexEvent(json.RawMessage(`{
		"type":"item.completed",
		"item":{"id":"reason-1","type":"reasoning","text":"private chain of thought"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Events) != 0 {
		t.Fatalf("reasoning produced persisted events: %+v", parsed.Events)
	}

	parsed, err = parseCodexEvent(json.RawMessage(`{
		"type":"item.completed",
		"item":{"id":"command-1","type":"command_execution","command":"go test ./...","aggregated_output":"ok","exit_code":0,"status":"completed","thinking":"must not persist"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Events) != 1 || parsed.Events[0].Kind != delivery.EventCommandFinished {
		t.Fatalf("command events = %+v", parsed.Events)
	}
	encoded, _ := json.Marshal(parsed.Events[0])
	if strings.Contains(string(encoded), "must not persist") {
		t.Fatalf("raw provider field leaked into event: %s", encoded)
	}
}

func TestClaudeEventParserDropsThinkingAndTracksToolResults(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	parser := newClaudeEventParser()
	parsed, err := parser(json.RawMessage(`{
		"type":"assistant","session_id":"session-1","message":{"content":[
			{"type":"thinking","thinking":"private chain of thought"},
			{"type":"text","text":"Checking the tests"},
			{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"TOKEN=anthropic-secret go test ./...","private":"must not persist"}}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SessionID != "session-1" || len(parsed.Events) != 2 ||
		parsed.Events[0].Kind != delivery.EventProviderMessage ||
		parsed.Events[1].Kind != delivery.EventCommandStarted {
		t.Fatalf("assistant events = %+v", parsed)
	}
	encoded, _ := json.Marshal(parsed.Events)
	if strings.Contains(string(encoded), "private chain of thought") ||
		strings.Contains(string(encoded), "must not persist") ||
		strings.Contains(string(encoded), "anthropic-secret") {
		t.Fatalf("unsafe Claude content persisted: %s", encoded)
	}

	parsed, err = parser(json.RawMessage(`{
		"type":"user","message":{"content":[
			{"type":"tool_result","tool_use_id":"tool-1","content":"tests passed","is_error":false}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Events) != 1 || parsed.Events[0].Kind != delivery.EventCommandFinished {
		t.Fatalf("tool result events = %+v", parsed.Events)
	}
}

func TestClaudeEventParserEmitsFileChangeAfterSuccessfulWrite(t *testing.T) {
	parser := newClaudeEventParser()
	start, err := parser(json.RawMessage(`{
		"type":"assistant","message":{"content":[
			{"type":"tool_use","id":"tool-2","name":"Edit","input":{"file_path":"internal/service.go","new_string":"untrusted source"}}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Events) != 0 {
		t.Fatalf("file change emitted before tool success: %+v", start.Events)
	}
	finish, err := parser(json.RawMessage(`{
		"type":"user","message":{"content":[
			{"type":"tool_result","tool_use_id":"tool-2","content":"updated","is_error":false}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(finish.Events) != 1 || finish.Events[0].Kind != delivery.EventFileChanged ||
		finish.Events[0].Summary != "internal/service.go" {
		t.Fatalf("file result events = %+v", finish.Events)
	}
}
