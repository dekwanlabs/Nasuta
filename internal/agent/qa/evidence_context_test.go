package qa

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestAssembleAdmittedEvidenceSharesRetrievedProseAndMemory(t *testing.T) {
	rc := &retrieval.RetrievedContext{
		Text:     "Tech Stack | Java | Spring Boot",
		HitCount: 2,
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "runbook", Target: "doc-2015a2bba8c6e812",
			Sections: []string{"chunk:2"},
		}},
	}
	admitted := assembleAdmittedEvidence(&preparedEvidence{
		retrieved: rc,
		recalled: []memory.MemoryRecord{{
			Content: "User prefers architecture overviews.",
		}},
	})
	if admitted.Retrieved != rc {
		t.Fatal("retrieved context was not shared")
	}
	if len(admitted.Material) != 2 {
		t.Fatalf("material = %d, want retrieved + memory", len(admitted.Material))
	}
	if admitted.Material[0].Source != "qa.evidence" ||
		admitted.Material[0].Content != rc.Text ||
		admitted.Material[0].ContentHash != hashString(rc.Text) {
		t.Fatalf("retrieved block = %+v", admitted.Material[0])
	}
	if admitted.Material[1].Source != "qa.memory" {
		t.Fatalf("memory block = %+v", admitted.Material[1])
	}
	if len(admitted.Units) != 1 || admitted.Units[0].Target != "doc-2015a2bba8c6e812" {
		t.Fatalf("units = %+v", admitted.Units)
	}
}

func TestAssembleAdmittedEvidenceEmptyWhenNoText(t *testing.T) {
	admitted := assembleAdmittedEvidence(&preparedEvidence{
		retrieved: &retrieval.RetrievedContext{HitCount: 0},
	})
	if len(admitted.Material) != 0 || len(admitted.Units) != 0 {
		t.Fatalf("empty retrieval produced material=%+v units=%+v", admitted.Material, admitted.Units)
	}
}
