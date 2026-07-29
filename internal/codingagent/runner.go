package codingagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

const (
	maxProviderEventBytes = 1 << 20
	maxProviderOutput     = 8 << 20
	maxProviderEvents     = 5000
)

type Config struct {
	CodexBin         string
	ClaudeBin        string
	EnabledProviders []string
}

// Runner dispatches one explicitly selected coding provider.
type Runner struct {
	codexBin  string
	claudeBin string
	enabled   map[string]struct{}
}

func New(config Config) *Runner {
	enabled := make(map[string]struct{}, len(config.EnabledProviders))
	for _, provider := range config.EnabledProviders {
		enabled[strings.TrimSpace(provider)] = struct{}{}
	}
	return &Runner{codexBin: config.CodexBin, claudeBin: config.ClaudeBin, enabled: enabled}
}

func (runner *Runner) Run(ctx context.Context, request featuredelivery.CodingRequest, sink featuredelivery.EventSink) (featuredelivery.CodingResult, error) {
	if runner == nil {
		return featuredelivery.CodingResult{}, featuredelivery.ErrUnavailable
	}
	if _, ok := runner.enabled[request.Provider]; !ok {
		return featuredelivery.CodingResult{}, fmt.Errorf("coding provider %q is not enabled: %w", request.Provider, featuredelivery.ErrUnavailable)
	}
	status := runner.providerStatus(ctx, request.Provider)
	if !providerReady(status) {
		return featuredelivery.CodingResult{}, fmt.Errorf(
			"coding provider %q is unavailable (%s): %w",
			request.Provider, status.Reason, featuredelivery.ErrUnavailable,
		)
	}
	switch request.Provider {
	case "codex":
		return runner.runCodex(ctx, request, sink)
	case "claude":
		return runner.runClaude(ctx, request, sink)
	default:
		return featuredelivery.CodingResult{}, fmt.Errorf("unsupported coding provider %q", request.Provider)
	}
}

func (runner *Runner) ProviderStatus(ctx context.Context) map[string]featuredelivery.CodingProviderStatus {
	statuses := make(map[string]featuredelivery.CodingProviderStatus, 2)
	for _, provider := range []string{"codex", "claude"} {
		if _, ok := runner.enabled[provider]; !ok {
			statuses[provider] = featuredelivery.CodingProviderStatus{Reason: "not_configured"}
			continue
		}
		statuses[provider] = runner.providerStatus(ctx, provider)
	}
	return statuses
}

func (runner *Runner) providerStatus(ctx context.Context, provider string) featuredelivery.CodingProviderStatus {
	status := featuredelivery.CodingProviderStatus{Enabled: true}
	binary := runner.codexBin
	credential := "CODEX_API_KEY"
	if provider == "claude" {
		binary = runner.claudeBin
		credential = "ANTHROPIC_API_KEY"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		status.Reason = "binary_not_found"
		return status
	}
	status.BinaryFound = true
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	output, _, _, probeErr := runProcess(probeCtx, processRequest{
		Path: path, Args: []string{"--version"}, Env: baseEnvironment(), OutputLimit: 4096,
	}, nil)
	cancel()
	if probeErr != nil {
		status.Reason = "version_probe_failed"
		return status
	}
	status.BinaryVersion = strings.TrimSpace(string(output))
	status.ContractCompatible = provider != "claude" || versionAtLeast(status.BinaryVersion, 2, 1, 219)
	if !status.ContractCompatible {
		status.Reason = "incompatible_version"
		return status
	}
	status.CredentialIsolated = true
	if strings.TrimSpace(os.Getenv(credential)) == "" {
		status.Reason = "credential_missing"
	}
	return status
}

func providerReady(status featuredelivery.CodingProviderStatus) bool {
	return status.Enabled && status.BinaryFound && status.ContractCompatible &&
		status.CredentialIsolated && status.Reason == ""
}

func versionAtLeast(value string, wantMajor, wantMinor, wantPatch int) bool {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return false
	}
	field := strings.TrimPrefix(fields[0], "v")
	parts := strings.Split(field, ".")
	if len(parts) < 3 {
		return false
	}
	actual := [3]int{}
	for index := range actual {
		number, err := strconv.Atoi(parts[index])
		if err != nil {
			return false
		}
		actual[index] = number
	}
	want := [3]int{wantMajor, wantMinor, wantPatch}
	for index := range actual {
		if actual[index] != want[index] {
			return actual[index] > want[index]
		}
	}
	return true
}
