package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

func (runner *Runner) runCodex(ctx context.Context, request delivery.CodingRequest, sink delivery.EventSink) (delivery.CodingResult, error) {
	path, err := exec.LookPath(runner.codexBin)
	if err != nil {
		return delivery.CodingResult{}, fmt.Errorf("find codex binary: %w", err)
	}
	temp, err := os.MkdirTemp("", "nasuta-codex-*")
	if err != nil {
		return delivery.CodingResult{}, err
	}
	defer os.RemoveAll(temp)
	schemaPath := filepath.Join(temp, "result-schema.json")
	if err := os.WriteFile(schemaPath, finalResultSchema, 0o600); err != nil {
		return delivery.CodingResult{}, err
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

type commandDetail struct {
	ID       string `json:"id,omitempty"`
	Command  string `json:"command,omitempty"`
	Output   string `json:"output,omitempty"`
	Status   string `json:"status,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

type fileChangeDetail struct {
	Paths  []string `json:"paths"`
	Action string   `json:"action,omitempty"`
}

func parseCodexEvent(raw json.RawMessage) (parsedProviderEvent, error) {
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		return parsedProviderEvent{}, err
	}
	parsed := parsedProviderEvent{SessionID: stringValue(event, "thread_id", "session_id")}
	eventType := stringValue(event, "type")
	switch eventType {
	case "thread.started", "turn.started":
		parsed.Events = []delivery.ProviderEvent{{Kind: delivery.EventProviderMessage, Summary: eventType}}
	case "item.started", "item.completed":
		parsed.Events = codexItemEvents(eventType, event)
	case "turn.completed":
		parsed.Events = []delivery.ProviderEvent{{Kind: delivery.EventProviderMessage, Summary: eventType}}
	default:
		if eventType != "" {
			parsed.Events = []delivery.ProviderEvent{{Kind: delivery.EventProviderMessage, Summary: eventType}}
		}
	}
	if final := extractFinalResult(event); final != nil {
		parsed.Final = final
	}
	return parsed, nil
}

func codexItemEvents(eventType string, event map[string]any) []delivery.ProviderEvent {
	item, ok := event["item"].(map[string]any)
	if !ok {
		return nil
	}
	itemType := stringValue(item, "type")
	switch itemType {
	case "reasoning":
		return nil
	case "agent_message":
		if text := stringValue(item, "text"); text != "" {
			return []delivery.ProviderEvent{{Kind: delivery.EventProviderMessage, Summary: text}}
		}
	case "command_execution":
		kind := delivery.EventCommandStarted
		if eventType == "item.completed" {
			kind = delivery.EventCommandFinished
		}
		detail := commandDetail{
			ID: truncate(redact(stringValue(item, "id")), 255), Command: truncate(redact(stringValue(item, "command")), 2000),
			Output: truncate(redact(stringValue(item, "aggregated_output", "output")), 4000),
			Status: truncate(redact(stringValue(item, "status")), 64), ExitCode: intPointer(item["exit_code"]),
		}
		summary := detail.Command
		if summary == "" {
			summary = string(kind)
		}
		return []delivery.ProviderEvent{{Kind: kind, Summary: summary, Detail: eventDetail(detail)}}
	case "file_change":
		paths := codexChangedPaths(item)
		if len(paths) == 0 {
			return nil
		}
		return []delivery.ProviderEvent{{
			Kind: delivery.EventFileChanged, Summary: strings.Join(paths, ", "),
			Detail: eventDetail(fileChangeDetail{Paths: paths, Action: truncate(redact(stringValue(item, "status")), 64)}),
		}}
	default:
		if itemType != "" {
			return []delivery.ProviderEvent{{Kind: delivery.EventProviderMessage, Summary: eventType + ": " + itemType}}
		}
	}
	return nil
}

func codexChangedPaths(item map[string]any) []string {
	changes, _ := item["changes"].([]any)
	paths := make([]string, 0, min(len(changes), maxChangedPaths))
	for _, value := range changes {
		if len(paths) == maxChangedPaths {
			break
		}
		change, ok := value.(map[string]any)
		if !ok {
			continue
		}
		path := truncate(redact(stringValue(change, "path", "file_path")), 1024)
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func intPointer(value any) *int {
	switch number := value.(type) {
	case float64:
		result := int(number)
		return &result
	case json.Number:
		if result, err := number.Int64(); err == nil {
			converted := int(result)
			return &converted
		}
	}
	return nil
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
