package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"text/template"
)

// ID identifies one immutable, built-in prompt template.
type ID string

const (
	AgentQACore                   ID = "agent.qa.core"
	AgentQADirect                 ID = "agent.qa.direct"
	AgentQAToolPolicy             ID = "agent.qa.tool_policy"
	AgentQAWeb                    ID = "agent.qa.web"
	AgentQAUserVisibleAnswer      ID = "agent.qa.user_visible_answer"
	AgentQADefaultIdentity        ID = "agent.qa.default_identity"
	AgentQAQueryKind              ID = "agent.qa.query_kind"
	AgentQAForceConclusion        ID = "agent.qa.force_conclusion"
	AgentQAForceConclusionNoThink ID = "agent.qa.force_conclusion_no_think"
	AgentQAProtocolRepair         ID = "agent.qa.protocol_repair"
	AgentQAContinuation           ID = "agent.qa.continuation"
	AgentQAEvidencePlan           ID = "agent.qa.evidence_plan"
	AgentQARetrievedHistory       ID = "agent.qa.retrieved_history"
	AgentQAHistoricalContext      ID = "agent.qa.historical_context"
	AgentQARecentDialogue         ID = "agent.qa.recent_dialogue"
	AgentQAPreRetrievedEvidence   ID = "agent.qa.pre_retrieved_evidence"
	AgentQAMidRunAddition         ID = "agent.qa.mid_run_addition"
	AgentQAToolDeliveryNotice     ID = "agent.qa.tool_delivery_notice"
	AgentQAExactAnswerContract    ID = "agent.qa.exact_answer_contract"
	AgentQAAnswerRepair           ID = "agent.qa.answer_repair"
	AgentQATurnSummary            ID = "agent.qa.turn_summary"
	AgentQAWebConvergence         ID = "agent.qa.web_convergence"
	AgentRuntimeContextBlock      ID = "agent.runtime.context_block"
	AgentRuntimeExecuteInput      ID = "agent.runtime.execute_input"
	AgentPreferredTool            ID = "agent.runtime.preferred_tool"

	AgentCatalogFallbackQA          ID = "agent.catalog.fallback_qa"
	AgentCatalogInvestigator        ID = "agent.catalog.investigator"
	AgentCatalogInvestigationReport ID = "agent.catalog.investigation_report"
	AgentCatalogDelegationVerifier  ID = "agent.catalog.delegation_verifier"
	AgentCatalogSynthesizer         ID = "agent.catalog.synthesizer"
	AgentCatalogReviewer            ID = "agent.catalog.reviewer"
	AgentCatalogAdjudicator         ID = "agent.catalog.adjudicator"

	RetrievalPlanner        ID = "retrieval.planner"
	RetrievalRouting        ID = "retrieval.routing"
	RetrievalToolRouting    ID = "retrieval.tool_routing"
	RetrievalQueryTerms     ID = "retrieval.query_terms"
	RetrievalQuerySemantics ID = "retrieval.query_semantics"
	RetrievalTime           ID = "retrieval.time"
	RetrievalHistory        ID = "retrieval.history"
	RetrievalExecution      ID = "retrieval.execution"

	MemoryProbe         ID = "memory.probe"
	MemoryExtract       ID = "memory.extract"
	MemoryRecallWrapper ID = "memory.recall_wrapper"
	LLMJSONRepair       ID = "llm.json_repair"
	IncidentSystem      ID = "incident.system"

	DocgenClassify         ID = "indexing.docgen.classify"
	DocgenGenerate         ID = "indexing.docgen.generate"
	DocgenFlowTemplate     ID = "indexing.docgen.flow_template"
	DocgenReformat         ID = "indexing.docgen.reformat"
	DocgenTemplateBackend  ID = "indexing.docgen.template.backend"
	DocgenTemplateEmbedded ID = "indexing.docgen.template.embedded"
	DocgenTemplateFrontend ID = "indexing.docgen.template.frontend"
	DocgenTemplateGeneric  ID = "indexing.docgen.template.generic"
	DocgenTemplateHost     ID = "indexing.docgen.template.host"
	DocgenTemplateMCU      ID = "indexing.docgen.template.mcu"
	DocgenTemplateMobile   ID = "indexing.docgen.template.mobile"
	DocgenTemplateModule   ID = "indexing.docgen.template.module"

	FeatureDeliveryCodingTask          ID = "feature.delivery.en.coding_task"
	FeatureDeliveryGenerationRequest   ID = "feature.delivery.en.generation_request"
	FeatureDeliveryImplementationPlan  ID = "feature.delivery.en.implementation_plan"
	FeatureDeliveryRequirementAnalysis ID = "feature.delivery.en.requirement_analysis"
	FeatureDeliverySystemDesign        ID = "feature.delivery.en.system_design"
	FeatureDeliveryTechnicalProposal   ID = "feature.delivery.en.technical_proposal"

	FeatureDeliveryZHCNCodingTask          ID = "feature.delivery.zh-CN.coding_task"
	FeatureDeliveryZHCNGenerationRequest   ID = "feature.delivery.zh-CN.generation_request"
	FeatureDeliveryZHCNImplementationPlan  ID = "feature.delivery.zh-CN.implementation_plan"
	FeatureDeliveryZHCNRequirementAnalysis ID = "feature.delivery.zh-CN.requirement_analysis"
	FeatureDeliveryZHCNSystemDesign        ID = "feature.delivery.zh-CN.system_design"
	FeatureDeliveryZHCNTechnicalProposal   ID = "feature.delivery.zh-CN.technical_proposal"
)

//go:embed text
var promptFS embed.FS

type promptEntry struct {
	file string
	text string
	tmpl *template.Template
}

var entries map[ID]promptEntry

var idFiles = map[ID]string{
	AgentQACore:                            "agent/qa/core.txt",
	AgentQADirect:                          "agent/qa/direct.txt",
	AgentQAToolPolicy:                      "agent/qa/tool_policy.txt",
	AgentQAWeb:                             "agent/qa/web.txt",
	AgentQAUserVisibleAnswer:               "agent/qa/user_visible_answer.txt",
	AgentQADefaultIdentity:                 "agent/qa/default_identity.txt",
	AgentQAQueryKind:                       "agent/qa/query_kind.txt",
	AgentQAForceConclusion:                 "agent/qa/force_conclusion.txt",
	AgentQAForceConclusionNoThink:          "agent/qa/force_conclusion_no_think.txt",
	AgentQAProtocolRepair:                  "agent/qa/protocol_repair.txt",
	AgentQAContinuation:                    "agent/qa/continuation.txt",
	AgentQAEvidencePlan:                    "agent/qa/evidence_plan.txt",
	AgentQARetrievedHistory:                "agent/qa/retrieved_history.txt",
	AgentQAHistoricalContext:               "agent/qa/historical_context.txt",
	AgentQARecentDialogue:                  "agent/qa/recent_dialogue.txt",
	AgentQAPreRetrievedEvidence:            "agent/qa/pre_retrieved_evidence.txt",
	AgentQAMidRunAddition:                  "agent/qa/mid_run_addition.txt",
	AgentQAToolDeliveryNotice:              "agent/qa/tool_delivery_notice.txt",
	AgentQAExactAnswerContract:             "agent/qa/exact_answer_contract.txt",
	AgentQAAnswerRepair:                    "agent/qa/answer_repair.txt",
	AgentQATurnSummary:                     "agent/qa/turn_summary.txt",
	AgentQAWebConvergence:                  "agent/qa/web_convergence.txt",
	AgentRuntimeContextBlock:               "agent/runtime/context_block.txt",
	AgentRuntimeExecuteInput:               "agent/runtime/execute_input.txt",
	AgentPreferredTool:                     "agent/runtime/preferred_tool.txt",
	AgentCatalogFallbackQA:                 "agent/catalog/fallback_qa.txt",
	AgentCatalogInvestigator:               "agent/catalog/investigator.txt",
	AgentCatalogInvestigationReport:        "agent/catalog/investigation_report.txt",
	AgentCatalogDelegationVerifier:         "agent/catalog/delegation_verifier.txt",
	AgentCatalogSynthesizer:                "agent/catalog/synthesizer.txt",
	AgentCatalogReviewer:                   "agent/catalog/reviewer.txt",
	AgentCatalogAdjudicator:                "agent/catalog/adjudicator.txt",
	RetrievalPlanner:                       "retrieval/planner.txt",
	RetrievalRouting:                       "retrieval/routing.txt",
	RetrievalToolRouting:                   "retrieval/tool_routing.txt",
	RetrievalQueryTerms:                    "retrieval/query_terms.txt",
	RetrievalQuerySemantics:                "retrieval/query_semantics.txt",
	RetrievalTime:                          "retrieval/time.txt",
	RetrievalHistory:                       "retrieval/history.txt",
	RetrievalExecution:                     "retrieval/execution.txt",
	MemoryProbe:                            "memory/probe.txt",
	MemoryExtract:                          "memory/extract.txt",
	MemoryRecallWrapper:                    "memory/recall_wrapper.txt",
	LLMJSONRepair:                          "llm/json_repair.txt",
	IncidentSystem:                         "incident/system.txt",
	DocgenClassify:                         "indexing/docgen/classify.txt",
	DocgenGenerate:                         "indexing/docgen/generate.txt",
	DocgenFlowTemplate:                     "indexing/docgen/flow_template.txt",
	DocgenReformat:                         "indexing/docgen/reformat.txt",
	DocgenTemplateBackend:                  "indexing/docgen/docgen_backend.txt",
	DocgenTemplateEmbedded:                 "indexing/docgen/docgen_embedded.txt",
	DocgenTemplateFrontend:                 "indexing/docgen/docgen_frontend.txt",
	DocgenTemplateGeneric:                  "indexing/docgen/docgen_generic.txt",
	DocgenTemplateHost:                     "indexing/docgen/docgen_host.txt",
	DocgenTemplateMCU:                      "indexing/docgen/docgen_mcu.txt",
	DocgenTemplateMobile:                   "indexing/docgen/docgen_mobile.txt",
	DocgenTemplateModule:                   "indexing/docgen/docgen_module.txt",
	FeatureDeliveryCodingTask:              "feature/delivery/en/coding_task.txt",
	FeatureDeliveryGenerationRequest:       "feature/delivery/en/generation_request.txt",
	FeatureDeliveryImplementationPlan:      "feature/delivery/en/implementation_plan.txt",
	FeatureDeliveryRequirementAnalysis:     "feature/delivery/en/requirement_analysis.txt",
	FeatureDeliverySystemDesign:            "feature/delivery/en/system_design.txt",
	FeatureDeliveryTechnicalProposal:       "feature/delivery/en/technical_proposal.txt",
	FeatureDeliveryZHCNCodingTask:          "feature/delivery/zh-CN/coding_task.txt",
	FeatureDeliveryZHCNGenerationRequest:   "feature/delivery/zh-CN/generation_request.txt",
	FeatureDeliveryZHCNImplementationPlan:  "feature/delivery/zh-CN/implementation_plan.txt",
	FeatureDeliveryZHCNRequirementAnalysis: "feature/delivery/zh-CN/requirement_analysis.txt",
	FeatureDeliveryZHCNSystemDesign:        "feature/delivery/zh-CN/system_design.txt",
	FeatureDeliveryZHCNTechnicalProposal:   "feature/delivery/zh-CN/technical_proposal.txt",
}

func init() {
	entries = make(map[ID]promptEntry, len(idFiles))
	for id, file := range idFiles {
		raw, err := fs.ReadFile(promptFS, path.Join("text", file))
		if err != nil {
			panic(fmt.Sprintf("prompts: read %q for %s: %v", file, id, err))
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			panic(fmt.Sprintf("prompts: %s is empty", file))
		}
		tmpl, err := template.New(string(id)).Option("missingkey=error").Funcs(template.FuncMap{
			"addOne": func(index int) int { return index + 1 },
		}).Parse(content)
		if err != nil {
			panic(fmt.Sprintf("prompts: parse %s: %v", file, err))
		}
		entries[id] = promptEntry{file: file, text: content, tmpl: tmpl}
	}
	if err := validatePromptFiles(); err != nil {
		panic(err)
	}
}

// Text returns a built-in prompt without rendering template actions.
func Text(id ID) string {
	entry, ok := entries[id]
	if !ok {
		panic(fmt.Sprintf("prompts: unknown prompt %q", id))
	}
	return entry.text
}

// Render executes a built-in prompt template with strict missing-key checks.
func Render(id ID, data any) (string, error) {
	entry, ok := entries[id]
	if !ok {
		return "", fmt.Errorf("prompts: unknown prompt %q", id)
	}
	var output bytes.Buffer
	if err := entry.tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("prompts: render %q: %w", id, err)
	}
	return strings.TrimSpace(output.String()), nil
}

// MustRender executes a built-in prompt and panics when its data is invalid.
func MustRender(id ID, data any) string {
	rendered, err := Render(id, data)
	if err != nil {
		panic(err)
	}
	return rendered
}

func validatePromptFiles() error {
	declared := make(map[string]struct{}, len(idFiles))
	for id, file := range idFiles {
		if _, duplicate := declared[file]; duplicate {
			return fmt.Errorf("prompts: duplicate file declaration %q for %s", file, id)
		}
		declared[file] = struct{}{}
	}
	return fs.WalkDir(promptFS, "text", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(file, ".txt") {
			return nil
		}
		relative := strings.TrimPrefix(file, "text/")
		if _, ok := declared[relative]; !ok {
			return fmt.Errorf("prompts: undeclared prompt file %q", relative)
		}
		return nil
	})
}
