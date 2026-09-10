package delegation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
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
			Subjects: subjects, MaxHops: 6,
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

func TestDefaultSeedContextInjectsFacetMatchedEvidence(t *testing.T) {
	content := "pre-retrieved evidence"
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"qa.evidence": {
				Source:      "qa.evidence",
				Title:       "QA Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{
					{
						SourceKind: "code", Target: "core.go",
						ContentHash: validEvidenceHash("core"),
						Facets:      []string{"core_flow"},
					},
					{
						SourceKind: "runbook", Target: "ops.md",
						ContentHash: validEvidenceHash("ops"),
						Facets:      []string{"runtime_and_operations"},
					},
				},
			},
			"qa.memory": {
				Source:      "qa.memory",
				Title:       "Recalled Memory",
				Content:     "memory content",
				ContentHash: hashBytes([]byte("memory content")),
			},
		},
	}

	capability := agentapi.Capability{
		InputFacets: []string{"core_flow", "data_and_state"},
	}
	blocks := defaultSeedContext(parent, capability, agentapi.DelegationTask{FocusFacets: []string{"core_flow"}}, 4096)
	if len(blocks) != 1 {
		t.Fatalf("seed blocks = %#v, want one qa.evidence block", blocks)
	}
	if len(blocks[0].Evidence) != 1 ||
		blocks[0].Evidence[0].SourceKind != "code" ||
		blocks[0].Evidence[0].Target != "core.go" {
		t.Fatalf("seeded evidence = %#v, want only facet-matched unit", blocks[0].Evidence)
	}
}

func TestDefaultSeedContextSkipsNonSeedAndUnmatchedBlocks(t *testing.T) {
	content := "pre-retrieved evidence"
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"other.tool": {
				Source:      "other.tool",
				Title:       "Tool Result",
				Content:     "tool content",
				ContentHash: hashBytes([]byte("tool content")),
				Evidence: []tool.EvidenceUnit{{
					SourceKind: "code", Target: "x.go",
					ContentHash: validEvidenceHash("x"),
					Facets:      []string{"core_flow"},
				}},
			},
			"qa.evidence": {
				Source:      "qa.evidence",
				Title:       "QA Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{{
					SourceKind: "runtime", Target: "svc-a",
					ContentHash: validEvidenceHash("runtime"),
					Facets:      []string{"runtime_and_operations"},
				}},
			},
		},
	}

	capability := agentapi.Capability{InputFacets: []string{"core_flow"}}
	blocks := defaultSeedContext(parent, capability, agentapi.DelegationTask{FocusFacets: []string{"core_flow"}}, 4096)
	if len(blocks) != 0 {
		t.Fatalf("seed blocks = %#v, want none (facet mismatch and non-seed source)", blocks)
	}
}

func TestDefaultSeedContextFallsBackToCapabilityFacets(t *testing.T) {
	content := "pre-retrieved evidence"
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"qa.evidence": {
				Source:      "qa.evidence",
				Title:       "QA Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{{
					SourceKind: "code", Target: "core.go",
					ContentHash: validEvidenceHash("core"),
					Facets:      []string{"core_flow"},
				}},
			},
		},
	}

	capability := agentapi.Capability{InputFacets: []string{"core_flow"}}
	blocks := defaultSeedContext(parent, capability, agentapi.DelegationTask{}, 4096)
	if len(blocks) != 1 || len(blocks[0].Evidence) != 1 {
		t.Fatalf("seed blocks = %#v, want one block with one matched unit", blocks)
	}
}

func TestDefaultSeedContextEmptyWhenNoParentContext(t *testing.T) {
	blocks := defaultSeedContext(ParentContext{}, agentapi.Capability{}, agentapi.DelegationTask{}, 4096)
	if len(blocks) != 0 {
		t.Fatalf("seed blocks = %#v, want none", blocks)
	}
}

// CJK evidence is the case a bytes-per-token heuristic gets wrong: a non-ASCII
// rune costs six times an ASCII rune, so trimming by bytes admitted a seed that
// overflowed the child window before its first provider call.
func TestDefaultSeedContextBoundsSeedByTokensNotBytes(t *testing.T) {
	content := strings.Repeat("灯效下发链路与设备影子写入时序，", 4000)
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"qa.evidence": {
				Source:      "qa.evidence",
				Title:       "QA Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{
					{
						SourceKind: "code", Target: "scene.java",
						ContentHash: validEvidenceHash("scene"),
						Facets:      []string{"core_flow"},
					},
				},
			},
		},
	}

	const maxTokens = 512
	blocks := defaultSeedContext(
		parent,
		agentapi.Capability{InputFacets: []string{"core_flow"}},
		agentapi.DelegationTask{FocusFacets: []string{"core_flow"}},
		maxTokens,
	)
	if len(blocks) != 1 {
		t.Fatalf("seed blocks = %d, want 1", len(blocks))
	}
	if got := tooloutput.EstimateTokens(blocks[0].Content); got > maxTokens {
		t.Fatalf("seeded content = %d tokens, want <= %d", got, maxTokens)
	}
}

// A multi-block seed must respect the budget in aggregate, not per block.
func TestDefaultSeedContextBoundsTotalAcrossBlocks(t *testing.T) {
	parent := ParentContext{Context: map[string]agentapi.ContextBlock{}}
	for i := range 4 {
		content := strings.Repeat("消息中心推送开关与红点状态，", 2000)
		parent.Context[fmt.Sprintf("qa.evidence.%d", i)] = agentapi.ContextBlock{
			Source:      "qa.evidence",
			Title:       fmt.Sprintf("QA Evidence %d", i),
			Content:     content,
			ContentHash: hashBytes([]byte(fmt.Sprintf("%s-%d", content, i))),
			Evidence: []tool.EvidenceUnit{
				{
					SourceKind: "code", Target: fmt.Sprintf("center-%d.java", i),
					ContentHash: validEvidenceHash(fmt.Sprintf("center-%d", i)),
					Facets:      []string{"core_flow"},
				},
			},
		}
	}

	const maxTokens = 1024
	blocks := defaultSeedContext(
		parent,
		agentapi.Capability{InputFacets: []string{"core_flow"}},
		agentapi.DelegationTask{FocusFacets: []string{"core_flow"}},
		maxTokens,
	)
	total := 0
	for _, block := range blocks {
		total += tooloutput.EstimateTokens(block.Content)
	}
	if total > maxTokens {
		t.Fatalf("seeded total = %d tokens across %d blocks, want <= %d", total, len(blocks), maxTokens)
	}
}

// The seed budget plus everything else the child's request carries must fit the
// single-request window, or the child fails at step 0 with no answer at all.
func TestSeedContextTokensLeavesRoomForReserveSafetyAndPrompt(t *testing.T) {
	const window, outputReserve = 51200, 18000
	seed := seedContextTokens(window, outputReserve)
	if seed <= 0 {
		t.Fatalf("seed budget = %d, want positive", seed)
	}
	safety := int64(agentrun.ContextSafetyTokens(window))
	total := seed + outputReserve + safety + childPromptOverheadTokens
	if total > window {
		t.Fatalf("seed %d + reserve %d + safety %d + prompt %d = %d, want <= %d",
			seed, outputReserve, safety, childPromptOverheadTokens, total, window)
	}
}

func TestSeedContextTokensZeroWhenWindowCannotFitOverhead(t *testing.T) {
	if got := seedContextTokens(childPromptOverheadTokens, 0); got != 0 {
		t.Fatalf("seed budget = %d, want 0 when window only covers overhead", got)
	}
}

func TestSelectContextBoundsSeedByTokens(t *testing.T) {
	content := strings.Repeat("菜谱检索与烹饪下发链路，", 4000)
	parent := ParentContext{
		Context: map[string]agentapi.ContextBlock{
			"ev_cookbook": {
				Source:      "qa.evidence",
				Title:       "Cookbook Evidence",
				Content:     content,
				ContentHash: hashBytes([]byte(content)),
				Evidence: []tool.EvidenceUnit{
					{
						SourceKind: "code", Target: "recipe.py",
						ContentHash: validEvidenceHash("recipe"),
						Facets:      []string{"core_flow"},
					},
				},
			},
		},
	}

	const maxTokens = 512
	blocks := selectContext(parent, []string{"ev_cookbook"}, []string{"core_flow"}, maxTokens)
	if len(blocks) != 1 {
		t.Fatalf("selected blocks = %d, want 1", len(blocks))
	}
	if got := tooloutput.EstimateTokens(blocks[0].Content); got > maxTokens {
		t.Fatalf("selected content = %d tokens, want <= %d", got, maxTokens)
	}
}

// The declared entity label is the binding key. Run 2026-09-11T00:07 dispatched
// four tasks whose objectives never contained a canonical label, so every task
// stayed unbound, entity partitioning was skipped, and three of four children
// overflowed their window before step 1 holding all subjects' evidence.
func TestAssignTaskEntitiesBindsDeclaredLabelWhenObjectiveDoesNotMatch(t *testing.T) {
	parent := ParentContext{Entities: []domain.EntitySpec{
		{ID: "message_center", Label: "消息中心"},
		{ID: "rgb_lighting", Label: "RGB 灯效", Aliases: []string{"rgb灯效"}},
	}}
	tasks := []agentapi.DelegationTask{
		{Objective: "梳理推送触发与投递的主链路", Entity: "消息中心"},
		{Objective: "梳理灯板下发与影子写入", Entity: "rgb灯效"},
	}
	assignTaskEntities(parent, tasks)
	if tasks[0].EntityID != "message_center" {
		t.Fatalf("task 0 entity = %q, want message_center", tasks[0].EntityID)
	}
	if tasks[1].EntityID != "rgb_lighting" {
		t.Fatalf("task 1 entity = %q, want rgb_lighting", tasks[1].EntityID)
	}
}

// Two tasks naming one subject must not both claim its partition.
func TestAssignTaskEntitiesClaimsEachEntityOnce(t *testing.T) {
	parent := ParentContext{Entities: []domain.EntitySpec{
		{ID: "message_center", Label: "消息中心"},
	}}
	tasks := []agentapi.DelegationTask{
		{Objective: "trigger path", Entity: "消息中心"},
		{Objective: "delivery path", Entity: "消息中心"},
	}
	assignTaskEntities(parent, tasks)
	if tasks[0].EntityID != "message_center" {
		t.Fatalf("task 0 entity = %q, want message_center", tasks[0].EntityID)
	}
	if tasks[1].EntityID != "" {
		t.Fatalf("task 1 entity = %q, want empty (already claimed)", tasks[1].EntityID)
	}
}

// A declared label must outrank another task's objective substring, so the task
// that names a subject wins it.
func TestAssignTaskEntitiesPrefersDeclaredOverObjectiveSubstring(t *testing.T) {
	parent := ParentContext{Entities: []domain.EntitySpec{
		{ID: "tts", Label: "tts"},
	}}
	tasks := []agentapi.DelegationTask{
		{Objective: "compare tts against the recipe path"},
		{Objective: "synthesis pipeline", Entity: "tts"},
	}
	assignTaskEntities(parent, tasks)
	if tasks[1].EntityID != "tts" {
		t.Fatalf("declared task entity = %q, want tts", tasks[1].EntityID)
	}
	if tasks[0].EntityID != "" {
		t.Fatalf("substring task entity = %q, want empty", tasks[0].EntityID)
	}
}

// Objective matching stays as a fallback for parents that omit the field.
func TestAssignTaskEntitiesFallsBackToObjectiveMatch(t *testing.T) {
	parent := ParentContext{Entities: []domain.EntitySpec{
		{ID: "recipe", Label: "菜谱"},
	}}
	tasks := []agentapi.DelegationTask{{Objective: "梳理菜谱详情与收藏链路"}}
	assignTaskEntities(parent, tasks)
	if tasks[0].EntityID != "recipe" {
		t.Fatalf("entity = %q, want recipe from objective fallback", tasks[0].EntityID)
	}
}

// An unbound task must not silently inherit a sibling's partition.
func TestDefaultSeedContextSkipsPartitionedBlocksForUnboundTask(t *testing.T) {
	content := strings.Repeat("消息中心链路证据，", 200)
	parent := ParentContext{Context: map[string]agentapi.ContextBlock{
		"qa.evidence": {
			Source: "qa.evidence", Title: "QA Evidence", EntityID: "message_center",
			Content: content, ContentHash: hashBytes([]byte(content)),
			Evidence: []tool.EvidenceUnit{{
				SourceKind: "code", Target: "center.java",
				ContentHash: validEvidenceHash("center"), Facets: []string{"core_flow"},
			}},
		},
	}}
	blocks := defaultSeedContext(
		parent,
		agentapi.Capability{InputFacets: []string{"core_flow"}},
		agentapi.DelegationTask{FocusFacets: []string{"core_flow"}},
		4096,
	)
	if len(blocks) != 0 {
		t.Fatalf("unbound task received %d partitioned blocks, want 0", len(blocks))
	}
}
