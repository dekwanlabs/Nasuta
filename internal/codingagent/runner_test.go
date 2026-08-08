package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

func TestProviderStatusRequiresCredentialAndCompatibleClaude(t *testing.T) {
	temp := t.TempDir()
	codex := writeFakeCLI(t, temp, "codex", `1.2.3`, "")
	claude := writeFakeCLI(t, temp, "claude", `2.1.6 (Claude Code)`, "")
	runner := New(Config{
		CodexBin: codex, ClaudeBin: claude,
		EnabledProviders: []string{"codex", "claude"},
	})
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")

	status := runner.ProviderStatus(context.Background())
	if status["codex"].Reason != "credential_missing" || !status["codex"].CredentialIsolated {
		t.Fatalf("codex status = %+v", status["codex"])
	}
	if status["claude"].Reason != "incompatible_version" || status["claude"].ContractCompatible {
		t.Fatalf("claude status = %+v", status["claude"])
	}
	if !versionAtLeast("2.1.219 (Claude Code)", 2, 1, 219) ||
		!versionAtLeast("2.2.0", 2, 1, 219) ||
		versionAtLeast("2.1.218", 2, 1, 219) {
		t.Fatal("Claude version comparison is incorrect")
	}
}

func TestCodexRunPinsEnvironmentAndNetworkPolicies(t *testing.T) {
	for _, networkEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "network_disabled", true: "network_enabled"}[networkEnabled], func(t *testing.T) {
			temp := t.TempDir()
			capture := filepath.Join(temp, "codex-args.txt")
			script := `
if [ "$1" = "--version" ]; then
  printf '%s\n' '1.2.3'
  exit 0
fi
printf '%s\n' "$@" > ` + shellQuote(capture) + `
printf '%s\n' '{"type":"thread.started","thread_id":"session-1"}'
printf '%s\n' '{"type":"turn.completed","structured_output":{"summary":"done codex-secret","tests":"ok codex-secret","deviations":[{"path":"internal/extra.go","reason":"needed codex-secret"},{"path":"internal/extra.go","reason":"duplicate"}]}}'
`
			codex := writeFakeCLI(t, temp, "codex", "", script)
			t.Setenv("CODEX_API_KEY", "codex-secret")
			t.Setenv("ANTHROPIC_API_KEY", "must-not-be-forwarded")
			runner := New(Config{CodexBin: codex, EnabledProviders: []string{"codex"}})

			result, err := runner.Run(context.Background(), delivery.CodingRequest{
				Provider: "codex", WorktreePath: temp, TaskPackage: "task", NetworkEnabled: networkEnabled,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "done [REDACTED]" || result.TestSummary != "ok [REDACTED]" || result.ProviderSessionID != "session-1" {
				t.Fatalf("result = %+v", result)
			}
			if len(result.Deviations) != 1 || result.Deviations[0].Path != "internal/extra.go" ||
				result.Deviations[0].Reason != "needed [REDACTED]" || !result.Deviations[0].Explained {
				t.Fatalf("deviations = %+v", result.Deviations)
			}
			args, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), `shell_environment_policy.inherit="none"`) {
				t.Fatalf("Codex args do not isolate command environment:\n%s", args)
			}
			wantNetwork := "sandbox_workspace_write.network_access=" + map[bool]string{false: "false", true: "true"}[networkEnabled]
			if !strings.Contains(string(args), wantNetwork) {
				t.Fatalf("Codex args missing %q:\n%s", wantNetwork, args)
			}
		})
	}
}

func TestClaudeRunWritesCredentialAndNetworkPolicies(t *testing.T) {
	for _, networkEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "network_disabled", true: "network_enabled"}[networkEnabled], func(t *testing.T) {
			temp := t.TempDir()
			capture := filepath.Join(temp, "claude-settings.json")
			argsCapture := filepath.Join(temp, "claude-args")
			envCapture := filepath.Join(temp, "claude-env")
			script := `
if [ "$1" = "--version" ]; then
  printf '%s\n' '2.1.219 (Claude Code)'
  exit 0
fi
printf '%s\n' "$@" > ` + shellQuote(argsCapture) + `
printf '%s|%s|%s\n' "${ANTHROPIC_API_KEY:-}" "${ANTHROPIC_AUTH_TOKEN:-}" "${ANTHROPIC_BASE_URL:-}" > ` + shellQuote(envCapture) + `
previous=''
for argument in "$@"; do
  if [ "$previous" = "--settings" ]; then
    cp "$argument" ` + shellQuote(capture) + `
  fi
  previous="$argument"
done
printf '%s\n' '{"type":"result","session_id":"session-2","structured_output":{"summary":"done anthropic-secret","tests":"ok"}}'
`
			claude := writeFakeCLI(t, temp, "claude", "", script)
			t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
			t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic-gateway.example")
			runner := New(Config{ClaudeBin: claude, EnabledProviders: []string{"claude"}})

			result, err := runner.Run(context.Background(), delivery.CodingRequest{
				Provider: "claude", WorktreePath: temp, TaskPackage: "task", NetworkEnabled: networkEnabled,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "done [REDACTED]" {
				t.Fatalf("summary = %q", result.Summary)
			}
			environment, err := os.ReadFile(envCapture)
			if err != nil {
				t.Fatal(err)
			}
			if string(environment) != "anthropic-secret||https://anthropic-gateway.example\n" {
				t.Fatalf("Claude environment = %q", environment)
			}
			settingsJSON, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Sandbox struct {
					Enabled                  bool `json:"enabled"`
					FailIfUnavailable        bool `json:"failIfUnavailable"`
					AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
					Credentials              struct {
						EnvVars []map[string]string `json:"envVars"`
					} `json:"credentials"`
					Network struct {
						AllowedDomains  []string `json:"allowedDomains"`
						StrictAllowlist bool     `json:"strictAllowlist"`
					} `json:"network"`
				} `json:"sandbox"`
			}
			if err := json.Unmarshal(settingsJSON, &settings); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(argsCapture)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(strings.Fields(string(args)), "--verbose") {
				t.Fatalf("Claude args missing --verbose:\n%s", args)
			}
			sandbox := settings.Sandbox
			if !sandbox.Enabled || !sandbox.FailIfUnavailable || sandbox.AllowUnsandboxedCommands {
				t.Fatalf("Claude sandbox policy = %+v", sandbox)
			}
			if len(sandbox.Credentials.EnvVars) != 2 ||
				sandbox.Credentials.EnvVars[0]["name"] != "ANTHROPIC_API_KEY" ||
				sandbox.Credentials.EnvVars[0]["mode"] != "deny" ||
				sandbox.Credentials.EnvVars[1]["name"] != "ANTHROPIC_AUTH_TOKEN" ||
				sandbox.Credentials.EnvVars[1]["mode"] != "deny" {
				t.Fatalf("Claude credential policy = %+v", sandbox.Credentials)
			}
			if !sandbox.Network.StrictAllowlist {
				t.Fatal("Claude network allowlist is not strict")
			}
			wantDomains := 0
			if networkEnabled {
				wantDomains = 1
			}
			if len(sandbox.Network.AllowedDomains) != wantDomains ||
				(networkEnabled && sandbox.Network.AllowedDomains[0] != "*") {
				t.Fatalf("Claude allowed domains = %v", sandbox.Network.AllowedDomains)
			}
		})
	}
}

func TestClaudeAuthTokenEnvironment(t *testing.T) {
	temp := t.TempDir()
	capture := filepath.Join(temp, "claude-env")
	claude := writeFakeCLI(t, temp, "claude", "", `
if [ "$1" = "--version" ]; then
  printf '%s\n' '2.1.219 (Claude Code)'
  exit 0
fi
printf '%s|%s|%s\n' "${ANTHROPIC_API_KEY:-}" "${ANTHROPIC_AUTH_TOKEN:-}" "${ANTHROPIC_BASE_URL:-}" > `+shellQuote(capture)+`
printf '%s\n' '{"type":"result","structured_output":{"summary":"done","tests":"ok"}}'
`)
	runner := New(Config{
		ClaudeBin: claude, EnabledProviders: []string{"claude"},
	})
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gateway-secret")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example")
	if _, err := runner.Run(context.Background(), delivery.CodingRequest{
		Provider: "claude", WorktreePath: temp, TaskPackage: "task",
	}, nil); err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(environment) != "|gateway-secret|https://gateway.example\n" {
		t.Fatalf("Claude environment = %q", environment)
	}
}

func TestRunnerDoesNotSubstituteProvider(t *testing.T) {
	temp := t.TempDir()
	marker := filepath.Join(temp, "claude-called")
	claude := writeFakeCLI(t, temp, "claude", "", `touch `+shellQuote(marker))
	t.Setenv("CODEX_API_KEY", "codex-secret")
	runner := New(Config{
		CodexBin: filepath.Join(temp, "missing-codex"), ClaudeBin: claude,
		EnabledProviders: []string{"codex", "claude"},
	})

	_, err := runner.Run(context.Background(), delivery.CodingRequest{Provider: "codex"}, nil)
	if !errors.Is(err, delivery.ErrUnavailable) {
		t.Fatalf("run error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Claude was invoked as a fallback")
	}
}

func TestRunProcessCancellationTerminatesProvider(t *testing.T) {
	temp := t.TempDir()
	script := writeFakeCLI(t, temp, "slow", "", `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, cancelled, err := runProcess(ctx, processRequest{
		Path: script, Env: baseEnvironment(), OutputLimit: 1024,
	}, nil)
	if !cancelled || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled=%t err=%v", cancelled, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("provider cancellation took too long")
	}
}

func TestRunProcessRejectsStderrOverSharedBudget(t *testing.T) {
	temp := t.TempDir()
	script := writeFakeCLI(t, temp, "stderr-heavy", "", `head -c 256 /dev/zero >&2`)
	_, _, _, err := runProcess(context.Background(), processRequest{
		Path: script, Env: baseEnvironment(), OutputLimit: 128,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "shared 128 byte limit") {
		t.Fatalf("output error = %v", err)
	}
}

func TestRunProcessSharesBudgetAcrossStdoutAndStderr(t *testing.T) {
	temp := t.TempDir()
	script := writeFakeCLI(t, temp, "combined-heavy", "", `
head -c 80 /dev/zero
head -c 80 /dev/zero >&2
`)
	_, _, _, err := runProcess(context.Background(), processRequest{
		Path: script, Env: baseEnvironment(), OutputLimit: 100,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "shared 100 byte limit") {
		t.Fatalf("output error = %v", err)
	}
}

func TestRunProviderLimitsExpandedPlatformEventTotal(t *testing.T) {
	temp := t.TempDir()
	script := writeFakeCLI(t, temp, "expanding-provider", "", `
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' '1.0.0'
  exit 0
fi
i=0
while [ "$i" -lt 79 ]; do
  printf '%s\n' '{"type":"event"}'
  i=$((i + 1))
done
`)
	parser := func(json.RawMessage) (parsedProviderEvent, error) {
		events := make([]delivery.ProviderEvent, maxPlatformEvents)
		for index := range events {
			events[index] = delivery.ProviderEvent{Kind: delivery.EventProviderMessage, Summary: "event"}
		}
		return parsedProviderEvent{Events: events}, nil
	}
	result, err := runProvider(
		context.Background(),
		processRequest{Path: script, Env: baseEnvironment(), OutputLimit: maxProviderOutput},
		"test", delivery.CodingRequest{}, nil, parser,
	)
	if err == nil || !strings.Contains(err.Error(), "platform events exceed 5000") {
		t.Fatalf("event limit error = %v", err)
	}
	if result.EventCount != 4992 {
		t.Fatalf("persistable event count = %d", result.EventCount)
	}
}

func writeFakeCLI(t *testing.T, dir, name, version, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if body == "" {
		body = `printf '%s\n' ` + shellQuote(version)
	}
	content := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
