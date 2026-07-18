package docgen

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed prompts/docgen_*.md
var promptFS embed.FS

// templateMap caches loaded prompt templates keyed by template name.
var templateMap map[string]string

func init() {
	templateMap = make(map[string]string)
	entries, err := promptFS.ReadDir("prompts")
	if err != nil {
		panic(fmt.Sprintf("docgen: cannot read prompts dir: %v", err))
	}
	for _, e := range entries {
		data, err := promptFS.ReadFile("prompts/" + e.Name())
		if err != nil {
			panic(fmt.Sprintf("docgen: cannot read %s: %v", e.Name(), err))
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		templateMap[name] = string(data)
	}
}

// buildClassifyPrompt builds a lightweight prompt for project type classification.
func buildClassifyPrompt(filesCtx string) string {
	var sb strings.Builder
	sb.WriteString("Classify this project into exactly one type: backend, frontend, mobile, mcu, embedded, host, module, generic.\n\n")
	sb.WriteString("Definitions:\n")
	sb.WriteString("- backend: server-side API/service (Spring Boot, Gin, FastAPI, Express, gRPC)\n")
	sb.WriteString("- frontend: browser UI (React, Vue, Angular, Next.js)\n")
	sb.WriteString("- mobile: iOS/Android/Flutter/React Native app\n")
	sb.WriteString("- mcu: bare-metal / RTOS microcontroller firmware\n")
	sb.WriteString("- embedded: embedded Linux, Buildroot/Yocto, kernel modules\n")
	sb.WriteString("- host: desktop app (Qt, WPF, Electron desktop, PyQt) for device communication\n")
	sb.WriteString("- module: communication module firmware (WiFi/BLE/4G/NB-IoT modem, AT commands)\n")
	sb.WriteString("- generic: none of the above\n\n")
	sb.WriteString("Rules: prefer specific types. Mobile apps use mobile platform SDKs. MCU has no OS.\n")
	sb.WriteString("Analyze file extensions, build files, framework imports, and README.\n\n")
	sb.WriteString(filesCtx)
	sb.WriteString("\nReturn ONLY valid JSON (no fences):\n")
	sb.WriteString(`{"project_type":"<type>","confidence":"high|medium|low"}`)
	return sb.String()
}

// buildGeneratePrompt builds a targeted generation prompt with a single template.
func buildGeneratePrompt(filesCtx, templateName string) string {
	tmpl, ok := templateMap[templateName]
	if !ok {
		tmpl = templateMap["docgen_generic"]
	}
	var sb strings.Builder
	sb.WriteString("Generate a comprehensive technical document following the template below.\n")
	sb.WriteString("Base EVERY conclusion on the provided code. Mark inferences with [inferred]. Use English.\n\n")
	sb.WriteString("--- TEMPLATE ---\n")
	sb.WriteString(tmpl)
	sb.WriteString("\n\n--- PROJECT FILES ---\n")
	sb.WriteString(filesCtx)
	return sb.String()
}

// classifyResult is the JSON from the classification call.
type classifyResult struct {
	ProjectType string `json:"project_type"`
	Confidence  string `json:"confidence"`
}
