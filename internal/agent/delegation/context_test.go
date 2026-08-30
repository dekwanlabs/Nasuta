package delegation

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestEvidenceAliasesIncludeManifestHandle(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "runbook", Target: "device.md", ContentHash: "hash-1",
	}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("unit handle")
	}
	for _, alias := range evidenceAliases(unit) {
		if alias == handle {
			return
		}
	}
	t.Fatalf("aliases = %v, want %s", evidenceAliases(unit), handle)
}

func TestWithLiveEvidenceAuthorizesManifestHandle(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "runbook", Target: "device.md", ContentHash: "hash-1",
	}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("unit handle")
	}
	ctx := WithParentContext(context.Background(), ParentContext{RunID: "parent-1"})
	ctx = WithLiveEvidence(ctx, []tool.EvidenceUnit{unit})
	parent, ok := ParentContextFrom(ctx)
	if !ok {
		t.Fatal("parent context")
	}
	if _, authorized := parent.Evidence[handle]; !authorized {
		t.Fatalf("live evidence keys = %v, want %s", keysOf(parent.Evidence), handle)
	}
}

func keysOf(ledger map[string]tool.EvidenceUnit) []string {
	keys := make([]string, 0, len(ledger))
	for key := range ledger {
		keys = append(keys, key)
	}
	return keys
}
