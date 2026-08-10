package docgen

import (
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

var docgenTemplateIDs = map[string]prompts.ID{
	"docgen_backend":  prompts.DocgenTemplateBackend,
	"docgen_embedded": prompts.DocgenTemplateEmbedded,
	"docgen_frontend": prompts.DocgenTemplateFrontend,
	"docgen_generic":  prompts.DocgenTemplateGeneric,
	"docgen_host":     prompts.DocgenTemplateHost,
	"docgen_mcu":      prompts.DocgenTemplateMCU,
	"docgen_mobile":   prompts.DocgenTemplateMobile,
	"docgen_module":   prompts.DocgenTemplateModule,
}

// buildClassifyPrompt builds a lightweight prompt for project type classification.
func buildClassifyPrompt(filesCtx string) string {
	return prompts.MustRender(prompts.DocgenClassify, struct {
		Files string
	}{Files: filesCtx})
}

// buildGeneratePrompt builds a targeted generation prompt with a single template.
func buildGeneratePrompt(projectName, filesCtx, templateName string) string {
	templateID, ok := docgenTemplateIDs[templateName]
	if !ok {
		templateID = prompts.DocgenTemplateGeneric
	}
	return prompts.MustRender(prompts.DocgenGenerate, struct {
		ProjectName string
		Template    string
		Files       string
	}{
		ProjectName: projectName,
		Template:    prompts.Text(templateID),
		Files:       filesCtx,
	})
}

// classifyResult is the JSON from the classification call.
type classifyResult struct {
	ProjectType string `json:"project_type"`
	Confidence  string `json:"confidence"`
}
