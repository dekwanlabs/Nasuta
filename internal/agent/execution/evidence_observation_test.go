package execution

import (
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestAppendEvidenceObservationsHashesBoundedSummary(t *testing.T) {
	sourceContent := strings.Repeat("architecture evidence ", 800)
	sourceHash := toolContentSHA256(sourceContent)
	var observations []agentapi.EvidenceObservation

	appendEvidenceObservations(&observations, ToolExecution{
		AuthoritativeContent: sourceContent,
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind:  "code",
			Target:      "service.go",
			ContentHash: sourceHash,
		}},
		Evidence: true,
	}, "search_code")

	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.Summary == sourceContent {
		t.Fatal("summary was not bounded")
	}
	if observation.ContentHash != toolContentSHA256(observation.Summary) {
		t.Fatalf("content hash = %q, want bounded summary hash", observation.ContentHash)
	}
	if observation.ContentHash == sourceHash {
		t.Fatal("bounded summary retained the full source content hash")
	}
}
