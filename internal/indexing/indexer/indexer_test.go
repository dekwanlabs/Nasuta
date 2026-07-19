package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// writeFile creates path (with parents) under root and writes content.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func miniWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	base := "repos/hsas/hsas-demo"

	writeFile(t, root, base+"/pom.xml", `<project>
  <parent><artifactId>hsas-parent</artifactId></parent>
  <artifactId>hsas-demo</artifactId>
  <dependencies></dependencies>
</project>`)
	writeFile(t, root, base+"/src/main/resources/bootstrap.yml", "server:\n  port: ${port:3009}\n")
	writeFile(t, root, base+"/src/main/java/com/demo/DemoApplication.java",
		"package com.demo;\n@SpringBootApplication\npublic class DemoApplication {}\n")
	writeFile(t, root, base+"/src/main/java/com/demo/DemoController.java", `package com.demo;
@RestController
@RequestMapping("/demo")
public class DemoController {
    @GetMapping("/ping")
    public String ping() { return "ok"; }

    @PostMapping("/items/create")
    public Long create() { return 1L; }
}`)
	writeFile(t, root, base+"/src/main/java/com/demo/UserFeign.java", `package com.demo;
@FeignClient(value = "hsds-user-provider", path = "/user")
public interface UserFeign {
    @GetMapping("/info")
    Object info();
}`)

	return root
}

func TestBuildBundleEndToEnd(t *testing.T) {
	root := miniWorkspace(t)
	b := BuildBundle(root, DiscoverScanDirs(root), nil)

	// service candidates are merged and keyed before they reach the store.
	svc := findService(b.Services, "hsas-demo")
	if svc == nil {
		t.Fatalf("hsas-demo service not found; got %d services", len(b.Services))
	}
	if svc.Language != "java" || svc.Runtime != "spring-boot" {
		t.Errorf("lang/runtime = %q/%q", svc.Language, svc.Runtime)
	}
	if !containsInt(svc.Ports, 3009) {
		t.Errorf("ports = %v, want contains 3009", svc.Ports)
	}
	if svc.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", svc.Confidence)
	}

	// endpoints: class prefix joined, handler method captured
	if got := findEndpoint(b.Endpoints, "GET", "/demo/ping"); got == nil {
		t.Errorf("endpoint GET /demo/ping not found; got %v", endpointPaths(b.Endpoints))
	} else if got.HandlerMethod != "ping" {
		t.Errorf("handlerMethod = %q, want ping", got.HandlerMethod)
	}
	if got := findEndpoint(b.Endpoints, "POST", "/demo/items/create"); got == nil {
		t.Errorf("endpoint POST /demo/items/create not found")
	}

	// Feign is represented by the generic dependency model with symbol evidence.
	if len(b.Dependencies) != 1 {
		t.Errorf("dependencies = %d, want 1", len(b.Dependencies))
	} else {
		dependency := b.Dependencies[0]
		if dependency.From != "hsas-demo" || dependency.To != "hsds-user-provider" || dependency.Type != domain.EdgeFeign {
			t.Errorf("dependency = %+v", dependency)
		}
		if dependency.CallerServiceKey != svc.ServiceKey || dependency.TargetKind != domain.DependencyTargetExternal {
			t.Errorf("dependency identity = %+v", dependency)
		}
		if len(dependency.Evidence) != 1 || dependency.Evidence[0].Symbol != "UserFeign" {
			t.Errorf("dependency evidence = %+v", dependency.Evidence)
		}
	}

	// runbooks come solely from the platform DocStore; with a nil DocStore
	// (as passed here) BuildBundle produces none — no disk fallback.
	if len(b.Runbooks) != 0 {
		t.Errorf("runbooks = %v, want none (nil DocStore)", b.Runbooks)
	}
}

func TestScanRepoNormalizesRepositoryKey(t *testing.T) {
	root := miniWorkspace(t)
	b := ScanRepo(root, "repos/hsas/hsas-demo")
	if b.Repo != "hsas/hsas-demo" {
		t.Fatalf("repo = %q, want hsas/hsas-demo", b.Repo)
	}
	if len(b.Dependencies) != 1 || b.Dependencies[0].CallerServiceKey == "" {
		t.Fatalf("canonical dependency = %+v", b.Dependencies)
	}
}

func TestMergeServicesOrderIndependent(t *testing.T) {
	metadata := domain.ServiceRecord{ServiceName: "svc", Repo: "hsas/svc", ModulePath: ".", Owner: "team-x",
		Summary: "does things", Tags: []string{"card"}, Docs: []string{"d.md"},
		Language: "unknown", Confidence: 0.85}
	code := domain.ServiceRecord{ServiceName: "svc", Repo: "hsas/svc", ModulePath: ".",
		Language: "java", Runtime: "spring-boot", Tags: []string{"code-scan"},
		Ports: []int{3001}, Confidence: 0.9}

	for _, order := range [][]domain.ServiceRecord{{metadata, code}, {code, metadata}} {
		m := CanonicalizeBundle(domain.IndexBundle{Services: order}).Services
		if len(m) != 1 {
			t.Fatalf("merge -> %d records, want 1", len(m))
		}
		s := m[0]
		if s.Owner != "team-x" || s.Summary == "" {
			t.Errorf("lost doc semantics: owner=%q summary=%q", s.Owner, s.Summary)
		}
		if s.Language != "java" || s.ModulePath != "." {
			t.Errorf("lost code evidence: lang=%q module=%q", s.Language, s.ModulePath)
		}
		if !containsInt(s.Ports, 3001) {
			t.Errorf("lost ports: %v", s.Ports)
		}
		if s.Confidence < 0.95 {
			t.Errorf("merged confidence = %v, want >= 0.95", s.Confidence)
		}
	}
}

func TestCanonicalizeBundleKeepsNestedModuleOwnership(t *testing.T) {
	bundle := domain.IndexBundle{Services: []domain.ServiceRecord{{
		ServiceName: "Siri",
		Repo:        "airone/dreo",
		ModulePath:  "repos/airone/dreo/ios/Siri",
		Language:    "swift",
		Layer:       "app",
		Confidence:  0.9,
	}}}

	first := CanonicalizeBundle(bundle)
	second := CanonicalizeBundle(first)
	if len(second.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(second.Services))
	}
	service := second.Services[0]
	if service.Repo != "airone/dreo" || service.ModulePath != "ios/Siri" {
		t.Fatalf("ownership = %q/%q, want airone/dreo + ios/Siri", service.Repo, service.ModulePath)
	}
	if service.ServiceKey != first.Services[0].ServiceKey {
		t.Fatalf("service key changed across canonicalization: %q -> %q", first.Services[0].ServiceKey, service.ServiceKey)
	}
}

func TestSensitiveFilesSkipped(t *testing.T) {
	if !sensitiveFile("/x/config/.env") {
		t.Error(".env should be sensitive")
	}
	if !sensitiveFile("/x/secret.key") {
		t.Error("*.key should be sensitive")
	}
	if sensitiveFile("/x/.env.example") {
		t.Error(".env.example should NOT be sensitive")
	}
}

func findService(list []domain.ServiceRecord, name string) *domain.ServiceRecord {
	for i := range list {
		if list[i].ServiceName == name {
			return &list[i]
		}
	}
	return nil
}

func findEndpoint(list []domain.EndpointRecord, method, path string) *domain.EndpointRecord {
	for i := range list {
		if list[i].Method == method && list[i].Path == path {
			return &list[i]
		}
	}
	return nil
}

func endpointPaths(list []domain.EndpointRecord) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Method+" "+e.Path)
	}
	return out
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
