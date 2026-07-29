package codingagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

type processRequest struct {
	Path        string
	Args        []string
	Dir         string
	Env         []string
	Stdin       string
	OutputLimit int
}

type providerMessage struct {
	SessionID string
	Summary   string
	Detail    json.RawMessage
	Final     *finalResult
}

type eventParser func(json.RawMessage) (providerMessage, error)

func runProvider(ctx context.Context, process processRequest, provider string, request featuredelivery.CodingRequest, sink featuredelivery.EventSink, parser eventParser) (featuredelivery.CodingResult, error) {
	version := probeVersion(ctx, process.Path)
	result := featuredelivery.CodingResult{ProviderVersion: version}
	final, exitCode, _, err := runProcess(ctx, process, func(raw json.RawMessage) error {
		if result.EventCount >= maxProviderEvents {
			return fmt.Errorf("provider events exceed %d", maxProviderEvents)
		}
		message, parseErr := parser(raw)
		if parseErr != nil {
			return fmt.Errorf("parse %s event: %w", provider, parseErr)
		}
		result.EventCount++
		if message.SessionID != "" {
			result.ProviderSessionID = message.SessionID
		}
		if message.Final != nil {
			result.Summary = message.Final.Summary
			result.TestSummary = message.Final.Tests
		}
		if sink != nil {
			return sink(ctx, featuredelivery.ProviderEvent{
				Kind: featuredelivery.EventProviderMessage, Summary: redact(message.Summary), Detail: redactJSON(message.Detail),
			})
		}
		return nil
	})
	result.ExitCode = exitCode
	if err != nil {
		return result, err
	}
	if result.Summary == "" {
		result.Summary = truncate(string(final), 8000)
	}
	return result, nil
}

func runProcess(ctx context.Context, request processRequest, handle func(json.RawMessage) error) ([]byte, int, bool, error) {
	command := exec.Command(request.Path, request.Args...)
	command.Dir = request.Dir
	command.Env = request.Env
	command.Stdin = strings.NewReader(request.Stdin)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, -1, false, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, -1, false, err
	}
	if err := command.Start(); err != nil {
		return nil, -1, false, err
	}
	processDone := make(chan struct{})
	var cancelled atomic.Bool
	abort := make(chan struct{}, 1)
	stopCancellation := make(chan struct{})
	cancellationDone := make(chan struct{})
	go func() {
		defer close(cancellationDone)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
		case <-abort:
		case <-processDone:
			return
		case <-stopCancellation:
			return
		}
		terminateProcessGroup(command.Process.Pid, processDone)
	}()

	var stdoutBuffer limitedBuffer
	stdoutBuffer.limit = request.OutputLimit
	stdoutResult := make(chan error, 1)
	go func() {
		var readErr error
		if handle == nil {
			_, readErr = io.Copy(&stdoutBuffer, stdout)
			if readErr == nil && stdoutBuffer.exceeded {
				readErr = fmt.Errorf("process output exceeds %d bytes", stdoutBuffer.limit)
			}
		} else {
			readErr = readJSONLines(stdout, &stdoutBuffer, handle)
		}
		stdoutResult <- readErr
	}()

	var stderrBuffer limitedBuffer
	stderrBuffer.limit = request.OutputLimit
	stderrResult := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(&stderrBuffer, stderr)
		stderrResult <- readErr
	}()

	readErr := <-stdoutResult
	if readErr != nil {
		select {
		case abort <- struct{}{}:
		default:
		}
	}
	stderrErr := <-stderrResult
	waitErr := command.Wait()
	close(processDone)
	close(stopCancellation)
	<-cancellationDone

	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	if readErr != nil {
		return stdoutBuffer.Bytes(), exitCode, cancelled.Load(), readErr
	}
	if stderrErr != nil {
		return stdoutBuffer.Bytes(), exitCode, cancelled.Load(), stderrErr
	}
	if cancelled.Load() {
		return stdoutBuffer.Bytes(), exitCode, true, ctx.Err()
	}
	if waitErr != nil {
		return stdoutBuffer.Bytes(), exitCode, false, fmt.Errorf("provider process failed: %w: %s", waitErr, truncate(stderrBuffer.String(), 2000))
	}
	return stdoutBuffer.Bytes(), exitCode, false, nil
}

func terminateProcessGroup(pid int, done <-chan struct{}) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func readJSONLines(reader io.Reader, captured *limitedBuffer, handle func(json.RawMessage) error) error {
	scanner := bufio.NewScanner(io.TeeReader(reader, captured))
	scanner.Buffer(make([]byte, 64<<10), maxProviderEventBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return fmt.Errorf("provider emitted non-JSON output")
		}
		if handle != nil {
			if err := handle(append(json.RawMessage(nil), line...)); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read provider stream: %w", err)
	}
	if captured.exceeded {
		return fmt.Errorf("provider output exceeds %d bytes", captured.limit)
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.Buffer.Write(data)
	return original, nil
}

func providerEnvironment(provider, tempHome string) []string {
	environment := baseEnvironment()
	environment = append(environment, "HOME="+tempHome)
	switch provider {
	case "codex":
		environment = append(environment, "CODEX_HOME="+tempHome)
		if value := os.Getenv("CODEX_API_KEY"); value != "" {
			environment = append(environment, "CODEX_API_KEY="+value)
		}
	case "claude":
		if value := os.Getenv("ANTHROPIC_API_KEY"); value != "" {
			environment = append(environment, "ANTHROPIC_API_KEY="+value)
		}
	}
	return environment
}

func baseEnvironment() []string {
	environment := make([]string, 0, 6)
	for _, key := range []string{"PATH", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func probeVersion(ctx context.Context, path string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, _, _, err := runProcess(probeCtx, processRequest{
		Path: path, Args: []string{"--version"}, Env: baseEnvironment(), OutputLimit: 4096,
	}, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func redact(value string) string {
	for _, key := range []string{"CODEX_API_KEY", "ANTHROPIC_API_KEY"} {
		if secret := os.Getenv(key); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func redactJSON(value json.RawMessage) json.RawMessage {
	redacted := string(value)
	for _, key := range []string{"CODEX_API_KEY", "ANTHROPIC_API_KEY"} {
		if secret := os.Getenv(key); secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	return json.RawMessage(redacted)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
