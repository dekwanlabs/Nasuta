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

func (runner *Runner) runCodex(ctx context.Context, request featuredelivery.CodingRequest, sink featuredelivery.EventSink) (featuredelivery.CodingResult, error) {
	path, err := exec.LookPath(runner.codexBin)
	if err != nil {
		return featuredelivery.CodingResult{}, fmt.Errorf("find codex binary: %w", err)
	}
	temp, err := os.MkdirTemp("", "nasuta-codex-*")
	if err != nil {
		return featuredelivery.CodingResult{}, err
	}
	defer os.RemoveAll(temp)
	schemaPath := filepath.Join(temp, "result-schema.json")
	if err := os.WriteFile(schemaPath, finalResultSchema, 0o600); err != nil {
		return featuredelivery.CodingResult{}, err
	}
	args := []string{
		"-c", `shell_environment_policy.inherit="none"`,
		"-c", fmt.Sprintf("sandbox_workspace_write.network_access=%t", request.NetworkEnabled),
		"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--json", "--sandbox", "workspace-write", "--output-schema", schemaPath,
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	args = append(args, "-")
	environment := providerEnvironment("codex", temp)
	return runProvider(ctx, processRequest{
		Path: path, Args: args, Dir: request.WorktreePath, Env: environment,
		Stdin: request.TaskPackage, OutputLimit: maxProviderOutput,
	}, "codex", request, sink, parseCodexEvent)
}

func parseCodexEvent(raw json.RawMessage) (providerMessage, error) {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return providerMessage{}, err
	}
	message := providerMessage{Detail: raw}
	message.SessionID = stringValue(event, "thread_id", "session_id")
	eventType := stringValue(event, "type")
	switch eventType {
	case "thread.started", "turn.started":
		message.Summary = eventType
	case "item.completed", "turn.completed":
		message.Summary = nestedString(event, "item", "text")
		if message.Summary == "" {
			message.Summary = stringValue(event, "message", "content")
		}
	default:
		message.Summary = eventType
	}
	if final := extractFinalResult(event); final != nil {
		message.Final = final
	}
	return message, nil
}

func extractFinalResult(event map[string]any) *finalResult {
	for _, key := range []string{"result", "output", "structured_output"} {
		value, ok := event[key]
		if !ok {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		var final finalResult
		if json.Unmarshal(raw, &final) == nil && strings.TrimSpace(final.Summary) != "" {
			return &final
		}
	}
	return nil
}
