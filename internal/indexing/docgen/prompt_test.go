package docgen

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/prompts"
)

func TestGeneratePromptsEnforceBoundedProjectEvidence(t *testing.T) {
	for templateName := range docgenTemplateIDs {
		t.Run(templateName, func(t *testing.T) {
			prompt := buildGeneratePrompt("sample-project", "FILE TREE\nmain.go\n\nFILE CONTENTS\n", templateName)
			for _, required := range []string{
				"sample-project",
				"bounded snapshot",
				"Not established from the provided project files.",
				"never invent or estimate line numbers",
				"Scope every count to the supplied snapshot.",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("generate prompt missing evidence rule %q", required)
				}
			}
			for _, forbidden := range []string{
				"[file:line]",
				"Not provided in the skeleton.",
				"Not available in the provided files.",
				"certificate pinning [inferred]",
				"Certification status [inferred]",
				"Power supply requirements [inferred]",
				"Every section has substantive content",
				"You remember every project",
				"## Learning & Memory",
				"/api/devices/pair",
				"AT+SCAN",
				"AT+CFUN=1",
			} {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("generate prompt contains unsafe template rule %q", forbidden)
				}
			}
		})
	}
}

func TestClassifyPromptTreatsRTOSAsMCUFirmware(t *testing.T) {
	prompt := buildClassifyPrompt("FreeRTOSConfig.h\nmain.c\n")
	for _, required := range []string{
		"MCU includes",
		"bare-metal and RTOS microcontroller firmware",
		"embedded requires an embedded Linux",
		"Inspect only the supplied file paths and filenames",
		"do not claim to have",
		"read file contents or imports",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("classify prompt missing project-type boundary %q", required)
		}
	}
	for _, forbidden := range []string{"MCU has no OS", "Analyze file extensions, build files, framework imports, and README"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("classify prompt contains unsupported rule %q", forbidden)
		}
	}
}

func TestFlowPromptsDoNotInventEvidenceOrTranslatedTitles(t *testing.T) {
	flowTemplate := prompts.Text(prompts.DocgenFlowTemplate)
	if !strings.Contains(flowTemplate, "tags: [flow, event-driven, troubleshooting") {
		t.Fatalf("flow template does not provide evidence-neutral required tags")
	}
	for _, forbidden := range []string{
		"<Chinese Name>",
		"tags: [flow, <service>, <domain>",
		"hs-iot-",
		"Verify against codegraph",
	} {
		if strings.Contains(flowTemplate, forbidden) {
			t.Fatalf("flow template contains unsupported rule %q", forbidden)
		}
	}

	reformat := prompts.Text(prompts.DocgenReformat)
	for _, forbidden := range []string{
		"infer it from context",
		"TODO placeholder",
	} {
		if strings.Contains(reformat, forbidden) {
			t.Fatalf("flow reformat prompt contains unsupported rule %q", forbidden)
		}
	}
}
