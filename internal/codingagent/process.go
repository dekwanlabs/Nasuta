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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/platform"
)

type processRequest struct {
	Path        string
	Args        []string
	Dir         string
	Env         []string
	Stdin       string
	OutputLimit int
}

type parsedProviderEvent struct {
	SessionID string
	Events    []delivery.ProviderEvent
	Final     *finalResult
}

type eventParser func(json.RawMessage) (parsedProviderEvent, error)

func runProvider(ctx context.Context, process processRequest, provider string, request delivery.CodingRequest, sink delivery.EventSink, parser eventParser) (delivery.CodingResult, error) {
	version := probeVersion(ctx, process.Path)
	result := delivery.CodingResult{ProviderVersion: version}
	providerEventCount := 0
	final, exitCode, _, err := runProcess(ctx, process, func(raw json.RawMessage) error {
		if providerEventCount >= maxProviderEvents {
			return fmt.Errorf("provider events exceed %d", maxProviderEvents)
		}
		providerEventCount++
		parsed, parseErr := parser(raw)
		if parseErr != nil {
			return fmt.Errorf("parse %s event: %w", provider, parseErr)
		}
		if parsed.SessionID != "" {
			result.ProviderSessionID = truncate(redact(parsed.SessionID), 255)
		}
		if parsed.Final != nil {
			result.Summary = truncate(redact(parsed.Final.Summary), 8000)
			result.TestSummary = truncate(redact(parsed.Final.Tests), 8000)
			result.Deviations = sanitizeDeviations(parsed.Final.Deviations)
		}
		if len(parsed.Events) > maxPlatformEvents {
			return fmt.Errorf("%s event expands to more than %d platform events", provider, maxPlatformEvents)
		}
		if len(parsed.Events) > maxProviderEvents-result.EventCount {
			return fmt.Errorf("platform events exceed %d", maxProviderEvents)
		}
		result.EventCount += len(parsed.Events)
		for _, event := range parsed.Events {
			if sink != nil {
				event.Summary = truncate(redact(event.Summary), 4000)
				if err := sink(ctx, event); err != nil {
					return err
				}
			}
		}
		return nil
	})
	result.ExitCode = exitCode
	if err != nil {
		return result, err
	}
	if result.Summary == "" {
		result.Summary = truncate(redact(string(final)), 8000)
	}
	return result, nil
}

func sanitizeDeviations(values []delivery.PlanDeviation) []delivery.PlanDeviation {
	if len(values) > 100 {
		values = values[:100]
	}
	out := make([]delivery.PlanDeviation, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		path, err := delivery.NormalizePlanPath(redact(value.Path))
		if err != nil {
			continue
		}
		reason := strings.TrimSpace(truncate(redact(value.Reason), 2000))
		if reason == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, delivery.PlanDeviation{Path: path, Reason: reason, Explained: true})
	}
	return out
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

	budget := newOutputBudget(request.OutputLimit)
	stdoutBuffer := limitedBuffer{budget: budget}
	streamResults := make(chan streamReadResult, 2)
	go func() {
		var readErr error
		if handle == nil {
			_, readErr = io.Copy(&stdoutBuffer, stdout)
		} else {
			readErr = readJSONLines(stdout, &stdoutBuffer, handle)
		}
		streamResults <- streamReadResult{stdout: true, err: readErr}
	}()

	stderrBuffer := limitedBuffer{budget: budget}
	go func() {
		_, readErr := io.Copy(&stderrBuffer, stderr)
		streamResults <- streamReadResult{err: readErr}
	}()

	var stdoutErr, stderrErr error
	for range 2 {
		readResult := <-streamResults
		if readResult.stdout {
			stdoutErr = readResult.err
		} else {
			stderrErr = readResult.err
		}
		if readResult.err != nil {
			select {
			case abort <- struct{}{}:
			default:
			}
		}
	}
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
	if stdoutErr != nil {
		return stdoutBuffer.Bytes(), exitCode, cancelled.Load(), normalizeOutputError(stdoutErr, request.OutputLimit)
	}
	if stderrErr != nil {
		return stdoutBuffer.Bytes(), exitCode, cancelled.Load(), normalizeOutputError(stderrErr, request.OutputLimit)
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
	return nil
}

var errOutputLimit = errors.New("process output limit exceeded")

type streamReadResult struct {
	stdout bool
	err    error
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int
}

func newOutputBudget(limit int) *outputBudget {
	return &outputBudget{remaining: max(limit, 0)}
}

func (budget *outputBudget) take(size int) int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if size <= budget.remaining {
		budget.remaining -= size
		return size
	}
	allowed := budget.remaining
	budget.remaining = 0
	return allowed
}

type limitedBuffer struct {
	data   bytes.Buffer
	budget *outputBudget
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	allowed := buffer.budget.take(len(data))
	if allowed > 0 {
		_, _ = buffer.data.Write(data[:allowed])
	}
	if allowed != len(data) {
		return allowed, errOutputLimit
	}
	return allowed, nil
}

func (buffer *limitedBuffer) Bytes() []byte {
	return buffer.data.Bytes()
}

func (buffer *limitedBuffer) String() string {
	return buffer.data.String()
}

func normalizeOutputError(err error, limit int) error {
	if errors.Is(err, errOutputLimit) {
		return fmt.Errorf("provider output exceeds shared %d byte limit", limit)
	}
	return err
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
		for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
			if value := os.Getenv(key); value != "" {
				environment = append(environment, key+"="+value)
			}
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
	value = platform.RedactSensitiveText(value)
	for _, key := range []string{"CODEX_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if secret := os.Getenv(key); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func eventDetail(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
