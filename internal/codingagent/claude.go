package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

func (runner *Runner) runClaude(ctx context.Context, request featuredelivery.CodingRequest, sink featuredelivery.EventSink) (featuredelivery.CodingResult, error) {
	path, err := exec.LookPath(runner.claudeBin)
	if err != nil {
		return featuredelivery.CodingResult{}, fmt.Errorf("find claude binary: %w", err)
	}
	temp, err := os.MkdirTemp("", "nasuta-claude-*")
	if err != nil {
		return featuredelivery.CodingResult{}, err
	}
	defer os.RemoveAll(temp)
	settingsPath := filepath.Join(temp, "settings.json")
	allowedDomains := []string{}
	if request.NetworkEnabled {
		allowedDomains = []string{"*"}
	}
	settings := map[string]any{
		"permissions": map[string]any{"allow": []string{}, "deny": []string{}},
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			"credentials": map[string]any{
				"envVars": []map[string]string{{"name": "ANTHROPIC_API_KEY", "mode": "deny"}},
			},
			"network": map[string]any{
				"allowedDomains":  allowedDomains,
				"strictAllowlist": true,
			},
		},
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return featuredelivery.CodingResult{}, fmt.Errorf("marshal claude settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, settingsJSON, 0o600); err != nil {
		return featuredelivery.CodingResult{}, err
	}
	args := []string{
		"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence",
		"--setting-sources", "", "--settings", settingsPath,
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--permission-mode", "acceptEdits", "--tools", "Read,Edit,Write,Bash",
		"--json-schema", string(finalResultSchema),
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	environment := providerEnvironment("claude", temp)
	return runProvider(ctx, processRequest{
		Path: path, Args: args, Dir: request.WorktreePath, Env: environment,
		Stdin: request.TaskPackage, OutputLimit: maxProviderOutput,
	}, "claude", request, sink, newClaudeEventParser())
}

type claudeTool struct {
	name string
	path string
}

func newClaudeEventParser() eventParser {
	tools := make(map[string]claudeTool)
	return func(raw json.RawMessage) (parsedProviderEvent, error) {
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			return parsedProviderEvent{}, err
		}
		parsed := parsedProviderEvent{SessionID: stringValue(event, "session_id")}
		eventType := stringValue(event, "type")
		switch eventType {
		case "assistant", "user":
			parsed.Events = claudeContentEvents(event, tools)
		case "system":
			parsed.Events = []featuredelivery.ProviderEvent{{Kind: featuredelivery.EventProviderMessage, Summary: "system: " + stringValue(event, "subtype")}}
		case "result":
			parsed.Events = []featuredelivery.ProviderEvent{{Kind: featuredelivery.EventProviderMessage, Summary: "result"}}
		default:
			if eventType != "" {
				parsed.Events = []featuredelivery.ProviderEvent{{Kind: featuredelivery.EventProviderMessage, Summary: eventType}}
			}
		}
		if result, ok := event["structured_output"]; ok {
			encoded, _ := json.Marshal(result)
			var final finalResult
			if json.Unmarshal(encoded, &final) == nil {
				parsed.Final = &final
			}
		}
		if parsed.Final == nil && eventType == "result" {
			if result := stringValue(event, "result"); result != "" {
				var final finalResult
				if json.Unmarshal([]byte(result), &final) == nil {
					parsed.Final = &final
				} else {
					parsed.Events = []featuredelivery.ProviderEvent{{Kind: featuredelivery.EventProviderMessage, Summary: result}}
				}
			}
		}
		return parsed, nil
	}
}

func claudeContentEvents(event map[string]any, tools map[string]claudeTool) []featuredelivery.ProviderEvent {
	message, ok := event["message"].(map[string]any)
	if !ok {
		return nil
	}
	content, _ := message["content"].([]any)
	events := make([]featuredelivery.ProviderEvent, 0, min(len(content), maxPlatformEvents))
	for _, value := range content {
		if len(events) == maxPlatformEvents {
			break
		}
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(block, "type") {
		case "thinking":
			continue
		case "text":
			if text := stringValue(block, "text"); text != "" {
				events = append(events, featuredelivery.ProviderEvent{Kind: featuredelivery.EventProviderMessage, Summary: text})
			}
		case "tool_use":
			if toolEvent := claudeToolStart(block, tools); toolEvent != nil {
				events = append(events, *toolEvent)
			}
		case "tool_result":
			if toolEvent := claudeToolFinish(block, tools); toolEvent != nil {
				events = append(events, *toolEvent)
			}
		}
	}
	return events
}

func claudeToolStart(block map[string]any, tools map[string]claudeTool) *featuredelivery.ProviderEvent {
	id := truncate(redact(stringValue(block, "id")), 255)
	name := stringValue(block, "name")
	input, _ := block["input"].(map[string]any)
	tool := claudeTool{name: name, path: truncate(redact(stringValue(input, "file_path", "path")), 1024)}
	if id != "" {
		tools[id] = tool
	}
	if name != "Bash" {
		return nil
	}
	command := truncate(redact(stringValue(input, "command")), 2000)
	return &featuredelivery.ProviderEvent{
		Kind: featuredelivery.EventCommandStarted, Summary: command,
		Detail: eventDetail(commandDetail{ID: id, Command: command, Status: "started"}),
	}
}

func claudeToolFinish(block map[string]any, tools map[string]claudeTool) *featuredelivery.ProviderEvent {
	id := truncate(redact(stringValue(block, "tool_use_id")), 255)
	tool, ok := tools[id]
	if ok {
		delete(tools, id)
	}
	failed, _ := block["is_error"].(bool)
	if tool.name == "Edit" || tool.name == "Write" {
		if failed || tool.path == "" {
			return nil
		}
		return &featuredelivery.ProviderEvent{
			Kind: featuredelivery.EventFileChanged, Summary: tool.path,
			Detail: eventDetail(fileChangeDetail{Paths: []string{tool.path}, Action: strings.ToLower(tool.name)}),
		}
	}
	if tool.name != "Bash" {
		return nil
	}
	status := "completed"
	if failed {
		status = "failed"
	}
	output := truncate(redact(claudeContentText(block["content"])), 4000)
	return &featuredelivery.ProviderEvent{
		Kind: featuredelivery.EventCommandFinished, Summary: status,
		Detail: eventDetail(commandDetail{ID: id, Output: output, Status: status}),
	}
}

func claudeContentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, _ := value.([]any)
	var builder strings.Builder
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || stringValue(block, "type") != "text" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(stringValue(block, "text"))
	}
	return builder.String()
}
