package app

import (
	"encoding/json"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestMarshalInvestigationContractBoundsSeedMaterial(t *testing.T) {
	block := agentapi.ContextBlock{
		Source: "qa.evidence", Title: "QA Evidence",
		Content:  strings.Repeat("bounded evidence payload\n", 4000),
		Complete: false, ContentHash: "original-hash",
	}
	for index := 0; index < 12; index++ {
		target := "target-" + string(rune('a'+index))
		unit := tool.EvidenceUnit{
			SourceKind: "code", Target: target,
			Sections: []string{"implementation"}, ContentHash: "version-1",
			Coverage: tool.EvidenceCoverage{Complete: true},
		}
		block.References = append(block.References, agentapi.Reference{
			Type: "symbol", Label: target, Target: target,
		})
		block.Evidence = append(block.Evidence, unit)
		block.EvidenceConflicts = append(
			block.EvidenceConflicts,
			agentapi.EvidenceConflict{
				Identity: agentapi.EvidenceIdentity{
					SourceKind: unit.SourceKind, Target: unit.Target,
					Section: "implementation",
				},
				Current: unit, Incoming: unit,
			},
		)
	}
	contract := platformagent.TaskContract{
		TaskID: "qa_1", Question: "Why is checkout failing?",
		Objective: "Trace the checkout failure.",
		Entities:  []platformagent.EntityRef{{ID: "Checkout.Place"}},
		EvidenceGoals: []platformagent.EvidenceGoal{{
			ID: "core_flow", Facet: "core_flow", Required: true,
			Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
		}},
		Context: platformagent.TaskContext{
			SeedMaterial: []agentapi.ContextBlock{block},
		},
	}

	const budget = 2500
	input, err := marshalInvestigationContract(contract, budget)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := tooloutput.EstimateTokens(string(input)); tokens > budget {
		t.Fatalf("payload tokens = %d, budget = %d", tokens, budget)
	}

	var prepared platformagent.TaskContract
	if err := json.Unmarshal(input, &prepared); err != nil {
		t.Fatal(err)
	}
	if len(prepared.Context.SeedMaterial) != 1 {
		t.Fatalf("seed blocks = %d", len(prepared.Context.SeedMaterial))
	}
	got := prepared.Context.SeedMaterial[0]
	if got.Content == block.Content || got.Content == "" {
		t.Fatalf("seed content was not usefully truncated: %d bytes", len(got.Content))
	}
	if got.ContentHash != hashInvestigationContent(got.Content) ||
		got.ContentHash == block.ContentHash {
		t.Fatalf("content hash = %q", got.ContentHash)
	}
	if len(got.References) != investigationSeedReferenceLimit ||
		len(got.Evidence) != investigationSeedEvidenceLimit ||
		len(got.EvidenceConflicts) != investigationSeedConflictLimit {
		t.Fatalf(
			"bounded metadata = references:%d evidence:%d conflicts:%d",
			len(got.References),
			len(got.Evidence),
			len(got.EvidenceConflicts),
		)
	}
	if contract.Context.SeedMaterial[0].Content != block.Content ||
		contract.Context.SeedMaterial[0].ContentHash != block.ContentHash ||
		len(contract.Context.SeedMaterial[0].References) != 12 ||
		len(contract.Context.SeedMaterial[0].Evidence) != 12 ||
		len(contract.Context.SeedMaterial[0].EvidenceConflicts) != 12 {
		t.Fatal("original task contract was modified")
	}

	again, err := marshalInvestigationContract(contract, budget)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(input) {
		t.Fatal("bounded task contract is not deterministic")
	}
}

func TestMarshalInvestigationContractRejectsOversizedBase(t *testing.T) {
	contract := platformagent.TaskContract{
		TaskID: "qa_1", Question: strings.Repeat("question ", 1000),
		Objective: "Investigate the request.",
	}
	_, err := marshalInvestigationContract(contract, 32)
	if err == nil || !strings.Contains(err.Error(), "base=") {
		t.Fatalf("error = %v", err)
	}
}
