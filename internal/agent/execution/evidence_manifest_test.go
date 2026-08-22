package execution

import (
	"encoding/json"
	"strings"
	"testing"

	canonicalevidence "github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestModelFacingToolContentPublishesCanonicalEvidenceHandles(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "repos/voice/Controller.java",
		Sections: []string{"L10-L20"},
	}
	content, artifact := modelFacingToolContent("run-1", "call-1", `{"matches":[]}`, tool.AnswerContract{}, []tool.EvidenceUnit{unit}, 0)
	if artifact != "" {
		t.Fatalf("artifact = %q", artifact)
	}
	if !strings.Contains(content, `"_nasuta_evidence_manifest"`) {
		t.Fatalf("content has no evidence manifest: %s", content)
	}
	handle, ok := canonicalevidence.UnitHandle(unit)
	if !ok || !strings.Contains(content, handle) {
		t.Fatalf("content does not contain handle %q: %s", handle, content)
	}
}

func TestModelFacingToolContentKeepsManifestWithinPromptBudget(t *testing.T) {
	units := make([]tool.EvidenceUnit, 20)
	for index := range units {
		units[index] = tool.EvidenceUnit{
			SourceKind: "code", Target: "repos/voice/Controller.java",
			Sections: []string{"L" + string(rune('1'+index)) + "-L20"},
		}
	}
	const budget = 1200
	content, _ := modelFacingToolContent("run-1", "call-1", strings.Repeat("payload ", 500), tool.AnswerContract{}, units, budget)
	if len(content) > budget {
		t.Fatalf("prompt bytes = %d, budget = %d", len(content), budget)
	}
	manifestStart := strings.Index(content, `{"_nasuta_evidence_manifest"`)
	if manifestStart < 0 {
		t.Fatalf("manifest missing: %s", content)
	}
	var manifest struct {
		Nasuta struct {
			Items   []map[string]any `json:"items"`
			Omitted int              `json:"omitted"`
		} `json:"_nasuta_evidence_manifest"`
	}
	if err := json.Unmarshal([]byte(content[manifestStart:]), &manifest); err != nil {
		t.Fatalf("manifest JSON: %v; content=%s", err, content)
	}
	if len(manifest.Nasuta.Items) == 0 || manifest.Nasuta.Omitted == 0 {
		t.Fatalf("manifest compaction = %+v", manifest.Nasuta)
	}
}
