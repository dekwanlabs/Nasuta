package indexer

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

func TestIsNoiseFile(t *testing.T) {
	noise := []string{
		// dir-based
		".nasuta/bm25_vocab.json",
		"iot__cloud__hsas__hsas-openapi/hsas-openapi-device/src/test/java/product_v2/kcm/云端chefmode_generated.json",
		"repo/src/test/java/com/foo/BarTest.java",
		"repo/tests/conftest.py",
		"repo/__tests__/x.spec.ts",
		// generated name patterns
		"repo/internal/foo/bar_generated.go",
		"repo/api/service.pb.go",
		"repo/web/dist/app.min.js",
		// low-value extensions
		"repo/src/main/resources/i18n/messages_ja.properties",
		"repo/src/test/resources/fixture.json",
		"repo/http/test-api.http",
		"repo/Cargo.toml",
		"repo/templates/email.vm",
		"repo/templates/page.ftl",
		"repo/web/index.html",
	}
	for _, p := range noise {
		if !isNoiseFile(p) {
			t.Errorf("expected noise: %s", p)
		}
	}
	keep := []string{
		"repo/src/main/java/com/foo/Bar.java",
		"repo/internal/app/app.go",
		"repo/docs/knowledge-base/runbooks/arch-01.md",
		"repo/hsds-test-data-provider/src/main/java/Svc.java",
		"repo/src/main/resources/application.yml",
		"repo/deploy/start.sh",
		"repo/api/user.proto",
		// build manifests carry dependency/version/framework info — keep
		"repo/pom.xml",
		"repo/build.gradle",
		"repo/build.gradle.kts",
		// XML that is NOT pom.xml (MyBatis mappers, Spring configs, etc.)
		"repo/src/main/resources/mapper/UserMapper.xml",
		"hsds-cookbook/src/main/resources/spring/applicationContext.xml",
	}
	for _, p := range keep {
		if isNoiseFile(p) {
			t.Errorf("should keep: %s", p)
		}
	}
}

func TestCanonicalDependenciesMergeEvidence(t *testing.T) {
	services := []domain.ServiceRecord{
		{ServiceName: "a", Repo: "team/a", ModulePath: ".", Layer: "server", Language: "go"},
		{ServiceName: "b", Repo: "team/b", ModulePath: ".", Layer: "server", Language: "go"},
	}
	edges := []domain.DependencyEdge{
		{From: "a", To: "b", Type: domain.EdgeHTTP, Confidence: 0.65, Evidence: []domain.Evidence{{Path: "repos/team/a/a.go", Line: 1, Kind: domain.SourceCodeScan}}},
		{From: "a", To: "b", Type: domain.EdgeHTTP, Confidence: 0.9, Evidence: []domain.Evidence{{Path: "repos/team/a/b.go", Line: 2, Kind: domain.SourceCodeScan}}},
	}
	bundle := CanonicalizeBundle(domain.IndexBundle{Services: services, Dependencies: edges})
	if len(bundle.Dependencies) != 1 {
		t.Fatalf("dependencies = %d, want one logical edge", len(bundle.Dependencies))
	}
	edge := bundle.Dependencies[0]
	if edge.TargetKind != domain.DependencyTargetService || len(edge.Evidence) != 2 || edge.Confidence != 0.9 {
		t.Fatalf("canonical dependency = %+v", edge)
	}
}

func TestCanonicalDependenciesNormalizeExternalTargetOnce(t *testing.T) {
	bundle := CanonicalizeBundle(domain.IndexBundle{
		Services: []domain.ServiceRecord{{
			ServiceName: "a", Repo: "team/a", ModulePath: ".", Layer: "server", Language: "go",
		}},
		Dependencies: []domain.DependencyEdge{{
			From: "a", To: " External.API ", Type: domain.EdgeHTTP,
		}},
	})
	if len(bundle.Dependencies) != 1 {
		t.Fatalf("dependencies = %d, want one", len(bundle.Dependencies))
	}
	edge := bundle.Dependencies[0]
	if edge.ExternalTarget != "external.api" || edge.To != "External.API" {
		t.Fatalf("external target = %q, display target = %q", edge.ExternalTarget, edge.To)
	}
}

func TestChunkByNodes(t *testing.T) {
	text := "package x\n\nfunc A() {\n  doA()\n}\n\nfunc B() {\n  doB()\n}\n"
	nodes := []codegraph.Node{
		{Kind: "function", Name: "A", QualifiedName: "x.A", StartLine: 3, EndLine: 5, Signature: "func A()"},
		{Kind: "function", Name: "B", QualifiedName: "x.B", StartLine: 7, EndLine: 9},
	}
	chunks := chunkByNodes("repo/x.go", "repo", "go", text, nodes)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].Text, "func A()") || !strings.Contains(chunks[0].Text, "x.A") {
		t.Errorf("chunk A missing body/header: %q", chunks[0].Text)
	}
	if chunks[0].StartLine != 3 || chunks[0].EndLine != 5 {
		t.Errorf("chunk A wrong range: %d-%d", chunks[0].StartLine, chunks[0].EndLine)
	}
}

func TestChunkMarkdownExcludesFrontmatter(t *testing.T) {
	chunks := ChunkMarkdown("doc-a", "Architecture", `---
id: legacy-architecture
scope: event-driven
tags: [flow, architecture, gateway]
---
# Architecture

Gateway routes traffic to application services.
`, DefaultDocChunkConfig())
	if len(chunks) == 0 {
		t.Fatal("ChunkMarkdown() returned no chunks")
	}
	if strings.Contains(chunks[0].Text, "legacy-architecture") || strings.Contains(chunks[0].Text, "scope: event-driven") {
		t.Fatalf("chunk contains frontmatter: %q", chunks[0].Text)
	}
	if !strings.Contains(chunks[0].Text, "Gateway routes traffic") {
		t.Fatalf("chunk missing markdown body: %q", chunks[0].Text)
	}
}
