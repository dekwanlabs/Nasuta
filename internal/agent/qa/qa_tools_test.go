package qa

import (
	"strings"
	"testing"
)

func TestPreferredToolsInstructionIsAdvisory(t *testing.T) {
	instruction := preferredToolsInstruction([]string{"runtime"})
	for _, want := range []string{"runtime", "advisory, not mandatory", "answer directly", "Other registered tools remain available"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, instruction)
		}
	}
	if strings.Contains(instruction, "must call") || strings.Contains(instruction, "required") {
		t.Fatalf("preference was expressed as a requirement: %s", instruction)
	}
}
