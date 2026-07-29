package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
		"-p", "--output-format", "stream-json", "--no-session-persistence",
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
	}, "claude", request, sink, parseClaudeEvent)
}

func parseClaudeEvent(raw json.RawMessage) (providerMessage, error) {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return providerMessage{}, err
	}
	message := providerMessage{
		SessionID: stringValue(event, "session_id"),
		Summary:   stringValue(event, "type", "subtype"),
		Detail:    raw,
	}
	if result, ok := event["structured_output"]; ok {
		encoded, _ := json.Marshal(result)
		var final finalResult
		if json.Unmarshal(encoded, &final) == nil {
			message.Final = &final
		}
	}
	if message.Final == nil && stringValue(event, "type") == "result" {
		if result := stringValue(event, "result"); result != "" {
			var final finalResult
			if json.Unmarshal([]byte(result), &final) == nil {
				message.Final = &final
			} else {
				message.Summary = result
			}
		}
	}
	return message, nil
}
