package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
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
printf '%s\n' '{"type":"turn.completed","structured_output":{"summary":"done","tests":"ok"}}'
`
			codex := writeFakeCLI(t, temp, "codex", "", script)
			t.Setenv("CODEX_API_KEY", "codex-secret")
			t.Setenv("ANTHROPIC_API_KEY", "must-not-be-forwarded")
			runner := New(Config{CodexBin: codex, EnabledProviders: []string{"codex"}})

			result, err := runner.Run(context.Background(), featuredelivery.CodingRequest{
				Provider: "codex", WorktreePath: temp, TaskPackage: "task", NetworkEnabled: networkEnabled,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "done" || result.ProviderSessionID != "session-1" {
				t.Fatalf("result = %+v", result)
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
			script := `
if [ "$1" = "--version" ]; then
  printf '%s\n' '2.1.219 (Claude Code)'
  exit 0
fi
previous=''
for argument in "$@"; do
  if [ "$previous" = "--settings" ]; then
    cp "$argument" ` + shellQuote(capture) + `
  fi
  previous="$argument"
done
printf '%s\n' '{"type":"result","session_id":"session-2","structured_output":{"summary":"done","tests":"ok"}}'
`
			claude := writeFakeCLI(t, temp, "claude", "", script)
			t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
			runner := New(Config{ClaudeBin: claude, EnabledProviders: []string{"claude"}})

			if _, err := runner.Run(context.Background(), featuredelivery.CodingRequest{
				Provider: "claude", WorktreePath: temp, TaskPackage: "task", NetworkEnabled: networkEnabled,
			}, nil); err != nil {
				t.Fatal(err)
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
			sandbox := settings.Sandbox
			if !sandbox.Enabled || !sandbox.FailIfUnavailable || sandbox.AllowUnsandboxedCommands {
				t.Fatalf("Claude sandbox policy = %+v", sandbox)
			}
			if len(sandbox.Credentials.EnvVars) != 1 ||
				sandbox.Credentials.EnvVars[0]["name"] != "ANTHROPIC_API_KEY" ||
				sandbox.Credentials.EnvVars[0]["mode"] != "deny" {
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

func TestRunnerDoesNotSubstituteProvider(t *testing.T) {
	temp := t.TempDir()
	marker := filepath.Join(temp, "claude-called")
	claude := writeFakeCLI(t, temp, "claude", "", `touch `+shellQuote(marker))
	t.Setenv("CODEX_API_KEY", "codex-secret")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	runner := New(Config{
		CodexBin: filepath.Join(temp, "missing-codex"), ClaudeBin: claude,
		EnabledProviders: []string{"codex", "claude"},
	})

	_, err := runner.Run(context.Background(), featuredelivery.CodingRequest{Provider: "codex"}, nil)
	if !errors.Is(err, featuredelivery.ErrUnavailable) {
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
