package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const mermaidCLIVersion = "11.16.0"

// TestMermaidRendererSmoke uses a real Mermaid CLI/browser renderer. Installed
// mmdc is preferred. In hermetic CI or developer machines, setting
// NASUTA_MERMAID_NPX=1 enables the pinned npx fallback; it requires a local
// Chrome/Chromium binary and skips Puppeteer's browser download.
func TestMermaidRendererSmoke(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "flow.mmd")
	output := filepath.Join(dir, "flow.svg")
	if err := os.WriteFile(input, []byte("flowchart LR\n  api[API] --> worker[Worker]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := mermaidRenderCommand(t, dir, input, output)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mmdc render failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("mmdc output is not SVG: %q", string(body[:min(len(body), 200)]))
	}
}

func mermaidRenderCommand(t *testing.T, dir, input, output string) *exec.Cmd {
	t.Helper()
	if mmdc, err := exec.LookPath("mmdc"); err == nil {
		return exec.Command(mmdc, "-i", input, "-o", output, "--quiet")
	}
	if os.Getenv("NASUTA_MERMAID_NPX") != "1" {
		t.Skip("Mermaid CLI unavailable; set NASUTA_MERMAID_NPX=1 for pinned npx/browser integration")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx unavailable")
	}
	chrome := firstExistingPath(
		os.Getenv("NASUTA_CHROME_PATH"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	)
	if chrome == "" {
		t.Skip("Chrome/Chromium unavailable for Mermaid browser integration")
	}
	puppeteerConfig := filepath.Join(dir, "puppeteer.json")
	config := `{"executablePath":` + strconv.Quote(chrome) + `,"args":["--no-sandbox"]}`
	if err := os.WriteFile(puppeteerConfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(
		npx, "--yes", "@mermaid-js/mermaid-cli@"+mermaidCLIVersion,
		"-p", puppeteerConfig, "-i", input, "-o", output, "--quiet",
	)
	cmd.Env = append(os.Environ(), "PUPPETEER_SKIP_DOWNLOAD=true")
	return cmd
}

func firstExistingPath(paths ...string) string {
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}
