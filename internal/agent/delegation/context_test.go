package delegation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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

func TestParentContextClonesOutputContractSubjects(t *testing.T) {
	subjects := []string{"订单创建", "消息发送"}
	ctx := WithParentContext(context.Background(), ParentContext{
		RunID: "parent-1",
		OutputContract: agentapi.RunOutputContract{
			Kind: "flow", RequireMermaid: true, Subjects: subjects, MaxHops: 6,
		},
	})
	subjects[0] = "mutated"
	parent, ok := ParentContextFrom(ctx)
	if !ok {
		t.Fatal("parent context")
	}
	parent.OutputContract.Subjects[0] = "changed in returned copy"
	again, ok := ParentContextFrom(ctx)
	if !ok {
		t.Fatal("parent context second read")
	}
	if again.OutputContract.Subjects[0] != "订单创建" {
		t.Fatalf("output contract subjects alias context state: %#v", again.OutputContract)
	}
}

func TestIndexContextFiltersMalformedEvidenceBeforeChildAdmission(t *testing.T) {
	content := "authoritative context"
	block := agentapi.ContextBlock{
		Source:      "qa.evidence",
		Title:       "QA Evidence",
		Content:     content,
		ContentHash: hashBytes([]byte(content)),
		References:  []agentapi.Reference{{Type: "service", Target: "svc-a"}},
		Evidence: []tool.EvidenceUnit{
			{SourceKind: "", Target: "missing-source", ContentHash: validEvidenceHash("bad")},
			{SourceKind: " code ", Target: " file.go ", ContentHash: validEvidenceHash("good")},
		},
	}

	ledger, contexts := IndexContext([]agentapi.ContextBlock{block})
	if _, ok := ledger["bad"]; ok {
		t.Fatalf("malformed evidence was indexed: %#v", ledger)
	}
	unit, ok := ledger["code:file.go"]
	if !ok {
		t.Fatalf("canonical evidence was not indexed: %#v", ledger)
	}
	if unit.SourceKind != "code" || unit.Target != "file.go" {
		t.Fatalf("evidence identity was not canonicalized: %#v", unit)
	}
	selected, ok := contexts["svc-a"]
	if !ok {
		t.Fatalf("context reference was not indexed: %#v", contexts)
	}
	if len(selected.Evidence) != 1 ||
		selected.Evidence[0].SourceKind != unit.SourceKind ||
		selected.Evidence[0].Target != unit.Target ||
		selected.Evidence[0].ContentHash != unit.ContentHash {
		t.Fatalf("selected context evidence = %#v, want only canonical unit", selected.Evidence)
	}
}

func TestCloneContextBlockFiltersMalformedConflicts(t *testing.T) {
	content := "conflict context"
	block := cloneContextBlock(agentapi.ContextBlock{
		Source:      "qa.evidence",
		Title:       "QA Evidence",
		Content:     content,
		ContentHash: hashBytes([]byte(content)),
		EvidenceConflicts: []agentapi.EvidenceConflict{{
			Identity: agentapi.EvidenceIdentity{SourceKind: "code", Target: "file.go"},
			Current: tool.EvidenceUnit{
				SourceKind: "code", Target: "file.go", ContentHash: validEvidenceHash("current"),
			},
			Incoming: tool.EvidenceUnit{
				SourceKind: "", Target: "file.go", ContentHash: validEvidenceHash("incoming"),
			},
		}},
	})
	if len(block.EvidenceConflicts) != 0 {
		t.Fatalf("malformed conflict survived context cloning: %#v", block.EvidenceConflicts)
	}
}

func validEvidenceHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func TestSelectContextFiltersMalformedEvidenceAtChildBoundary(t *testing.T) {
	content := "selected context"
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"ev_selected": {
				Source:      "qa.evidence",
				Title:       "QA Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{
					{SourceKind: "", Target: "missing-source", ContentHash: validEvidenceHash("bad")},
					{SourceKind: "code", Target: "file.go", ContentHash: validEvidenceHash("good")},
				},
			},
		},
	}

	blocks := selectContext(parent, []string{"ev_selected"}, nil, 4096)
	if len(blocks) != 1 {
		t.Fatalf("selected blocks = %#v, want one block", blocks)
	}
	if len(blocks[0].Evidence) != 1 ||
		blocks[0].Evidence[0].SourceKind != "code" ||
		blocks[0].Evidence[0].Target != "file.go" {
		t.Fatalf("selected evidence = %#v, want only canonical evidence", blocks[0].Evidence)
	}
}
