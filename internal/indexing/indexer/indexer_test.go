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
import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/demo")
public class DemoController {
    @GetMapping("/ping")
    public String ping() { return "ok"; }

    @PostMapping("/items/create")
    public Long create() { return 1L; }

    @PostMapping("/items/annotated")
    @ApiResponses({
        @ApiResponse(code = 1001, message = "first"),
        @ApiResponse(code = 1002, message = "second"),
        @ApiResponse(code = 1003, message = "third"),
        @ApiResponse(code = 1004, message = "fourth"),
        @ApiResponse(code = 1005, message = "fifth"),
        @ApiResponse(code = 1006, message = "sixth"),
        @ApiResponse(code = 1007, message = "seventh")
    })
    public Long annotated() { return 2L; }
}`)
	writeFile(t, root, base+"/src/main/java/com/demo/UserFeign.java", `package com.demo;
@FeignClient(value = "hsds-user-provider", path = "/user")
public interface UserFeign {
    @GetMapping("/info")
    Object info();
}`)
	writeFile(t, root, base+"/src/main/java/com/demo/UserService.java", `package com.demo;
public class UserService {
    private final UserFeign userFeign;
    public UserService(UserFeign userFeign) { this.userFeign = userFeign; }
    public Object user() { return userFeign.info(); }
}`)

	return root
}

func TestBuildBundleEndToEnd(t *testing.T) {
	root := miniWorkspace(t)
	b, err := BuildBundle(root, mustDiscoverScanDirs(t, root), nil)
	if err != nil {
		t.Fatal(err)
	}

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
	if got := findEndpoint(b.Endpoints, "POST", "/demo/items/annotated"); got == nil {
		t.Errorf("endpoint POST /demo/items/annotated not found")
	} else if got.HandlerMethod != "annotated" {
		t.Errorf("long-annotation handlerMethod = %q, want annotated", got.HandlerMethod)
	}

	// Feign is represented only after a typed client method is actually called.
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
		if len(dependency.Evidence) < 2 ||
			dependency.Evidence[0].Symbol != "userFeign.info" ||
			dependency.Evidence[1].Symbol != "UserFeign.info" {
			t.Errorf("dependency evidence = %+v", dependency.Evidence)
		}
	}

	// runbooks come solely from the platform DocStore; with a nil DocStore
	// (as passed here) BuildBundle produces none — no disk fallback.
	if len(b.Runbooks) != 0 {
		t.Errorf("runbooks = %v, want none (nil DocStore)", b.Runbooks)
	}
}

func multiModuleWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	base := "repos/hsas/hsas-multi"

	// Aggregator parent: packaging=pom must be skipped (no code, no entrypoint).
	writeFile(t, root, base+"/pom.xml", `<project>
  <artifactId>hsas-multi</artifactId>
  <packaging>pom</packaging>
  <modules>
    <module>app-module</module>
    <module>lib-module</module>
  </modules>
</project>`)

	writeFile(t, root, base+"/app-module/pom.xml", `<project>
  <parent><artifactId>hsas-multi</artifactId></parent>
  <artifactId>app-module</artifactId>
</project>`)
	writeFile(t, root, base+"/app-module/src/main/java/com/app/AppApplication.java",
		"package com.app;\n@SpringBootApplication\npublic class AppApplication {}\n")

	// Library module: no entrypoint, but exposes a controller whose endpoint
	// must resolve to this module instead of being dropped.
	writeFile(t, root, base+"/lib-module/pom.xml", `<project>
  <parent><artifactId>hsas-multi</artifactId></parent>
  <artifactId>lib-module</artifactId>
</project>`)
	writeFile(t, root, base+"/lib-module/src/main/java/com/lib/LibController.java", `package com.lib;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
public class LibController {
    @GetMapping("/items")
    public String items() { return "ok"; }
}`)
	writeFile(t, root, base+"/lib-module/src/test/java/com/lib/ManualMain.java", `package com.lib;
public class ManualMain {
    public static void main(String[] args) {}
}`)
	return root
}

// TestScanJavaServicesIgnoresTestSourceEntrypoints keeps test helpers from
// turning a library module into a Spring Boot runtime service.
func TestScanJavaServicesIgnoresTestSourceEntrypoints(t *testing.T) {
	root := multiModuleWorkspace(t)
	services := scanJavaServices(root, mustDiscoverScanDirs(t, root))
	library := findService(services, "lib-module")
	if library == nil {
		t.Fatalf("library module not registered; got %v", serviceNames(services))
	}
	if library.Runtime != "" {
		t.Fatalf("library runtime = %q, want empty when main is only under src/test", library.Runtime)
	}
}

func TestScanJavaServicesIgnoresRuntimeKeywordsOutsideCode(t *testing.T) {
	root := t.TempDir()
	base := "repos/team/shared-library"
	writeFile(t, root, base+"/pom.xml", "<project><artifactId>shared-library</artifactId></project>")
	writeFile(t, root, base+"/src/main/java/com/example/Example.java", `package com.example;
// @SpringBootApplication public static void main(String[] args) {}
public class Example {
    String example = "SpringApplication.run";
}`)

	services := scanJavaServices(root, mustDiscoverScanDirs(t, root))
	service := findService(services, "shared-library")
	if service == nil || service.Runtime != "" {
		t.Fatalf("runtime inferred from comments or strings: %+v", service)
	}
}

// TestScanJavaServicesRegistersLibraryModules guards the fix for silent
// endpoint/dependency drops: a Maven library module (no Spring Boot entrypoint)
// is still registered as a service so its controllers resolve instead of being
// discarded by canonicalEndpoints as "unresolved service".
func TestScanJavaServicesRegistersLibraryModules(t *testing.T) {
	root := multiModuleWorkspace(t)
	b, err := BuildBundle(root, mustDiscoverScanDirs(t, root), nil)
	if err != nil {
		t.Fatal(err)
	}

	if svc := findService(b.Services, "hsas-multi"); svc != nil {
		t.Errorf("aggregator parent registered as service: %+v", svc)
	}

	app := findService(b.Services, "app-module")
	if app == nil {
		t.Fatalf("app-module service not found; got %d services: %v", len(b.Services), serviceNames(b.Services))
	}
	if app.Runtime != "spring-boot" || app.Confidence != 0.9 {
		t.Errorf("app-module runtime/confidence = %q/%v, want spring-boot/0.9", app.Runtime, app.Confidence)
	}

	lib := findService(b.Services, "lib-module")
	if lib == nil {
		t.Fatalf("library module lib-module not registered; got %d services: %v", len(b.Services), serviceNames(b.Services))
	}
	if lib.Runtime != "" {
		t.Errorf("lib-module runtime = %q, want empty (library)", lib.Runtime)
	}

	ep := findEndpoint(b.Endpoints, "GET", "/items")
	if ep == nil {
		t.Fatalf("lib-module endpoint GET /items dropped; endpoints=%v", endpointPaths(b.Endpoints))
	}
	if ep.ServiceKey != lib.ServiceKey {
		t.Errorf("endpoint attributed to %q, want lib-module %q", ep.ServiceKey, lib.ServiceKey)
	}
}

func TestScanPythonRoutesResolveToProjectModule(t *testing.T) {
	root := t.TempDir()
	base := "repos/ai/catalog-api"
	writeFile(t, root, base+"/pyproject.toml", `[project]
name = "catalog-api"
`)
	writeFile(t, root, base+"/app/main.py", `from fastapi import FastAPI
app = FastAPI()

@app.get('/health')
async def health():
    return {"ok": True}
`)
	writeFile(t, root, base+"/app/api/routes/items.py", `from fastapi import APIRouter
router = APIRouter(prefix='/items')

@router.post('/create')
async def create_item():
    return {}
`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "catalog-api")
	if service == nil {
		t.Fatalf("catalog-api service not found; services=%v", serviceNames(b.Services))
	}
	for _, want := range []struct {
		method string
		path   string
	}{{"GET", "/health"}, {"POST", "/items/create"}} {
		endpoint := findEndpoint(b.Endpoints, want.method, want.path)
		if endpoint == nil {
			t.Fatalf("%s %s not indexed; endpoints=%v", want.method, want.path, endpointPaths(b.Endpoints))
		}
		if endpoint.ServiceKey != service.ServiceKey {
			t.Errorf("%s %s service key = %q, want %q", want.method, want.path, endpoint.ServiceKey, service.ServiceKey)
		}
	}
}

func TestScanPythonRoutesPreservesFastAPIDecorators(t *testing.T) {
	root := t.TempDir()
	base := "repos/ai/catalog-api"
	writeFile(t, root, base+"/pyproject.toml", `[project]
name = "catalog-api"
`)
	writeFile(t, root, base+"/routes.py", `from fastapi import APIRouter, FastAPI

router = APIRouter(
    prefix="/items",
    description="""Metadata with a closing parenthesis ).
    """
)
app = FastAPI()

@router.get()
@router.head(
    "",
)
async def list_root():
    return []

@router.post(
    "/create",
    response_model=build_model(
        "item",
    ),
    description="""
    A multiline description with ).
    @router.delete("/not-a-route")
    """,
)
async def create_item():
    return {}

@router.put(
    response_model=Result,
    path="/named",
)
async def update_item():
    return {}

@app.get("/health")
async def health():
    return {"ok": True}
`)

	bundle := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	wants := []struct {
		method  string
		path    string
		handler string
	}{
		{method: "GET", path: "/items", handler: "list_root"},
		{method: "HEAD", path: "/items", handler: "list_root"},
		{method: "POST", path: "/items/create", handler: "create_item"},
		{method: "PUT", path: "/items/named", handler: "update_item"},
		{method: "GET", path: "/health", handler: "health"},
	}
	for _, want := range wants {
		endpoint := findEndpoint(bundle.Endpoints, want.method, want.path)
		if endpoint == nil {
			t.Fatalf("%s %s not indexed; endpoints=%v", want.method, want.path, endpointPaths(bundle.Endpoints))
		}
		if endpoint.HandlerMethod != want.handler || endpoint.Line <= 0 {
			t.Errorf("%s %s handler/line = %q/%d, want %q/positive", want.method, want.path, endpoint.HandlerMethod, endpoint.Line, want.handler)
		}
	}
	if endpoint := findEndpoint(bundle.Endpoints, "DELETE", "/items/not-a-route"); endpoint != nil {
		t.Fatalf("decorator text inside a multiline string was indexed: %+v", endpoint)
	}
}

func TestScanJavaEndpointsPreservesSpringMappingSemantics(t *testing.T) {
	root := t.TempDir()
	base := "repos/hsas/hsas-routing"
	writeFile(t, root, base+"/pom.xml", `<project>
  <artifactId>hsas-routing</artifactId>
</project>`)
	writeFile(t, root, base+"/src/main/java/com/demo/RoutingApplication.java",
		"package com.demo;\n@SpringBootApplication\npublic class RoutingApplication {}\n")
	writeFile(t, root, base+"/src/main/java/com/demo/RoutingController.java", `package com.demo;
import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping(path = {"/v1", "/legacy"})
public class RoutingController {
    @GetMapping(path = {"/items", "/records"})
    public Object list() { return null; }

    @PostMapping(produces = "application/json")
    public Object create() { return null; }

    @RequestMapping(
        value = {"/bulk", "/batch"},
        method = {RequestMethod.GET, RequestMethod.POST}
    )
    public Object bulk() { return null; }

    @DeleteMapping("/gone")
    public Object delete() { return null; }

    @GetMapping({"/items/{id}"})
    public Object getById() { return null; }

    @PutMapping("/apps/{name}/status?value={status}")
    public Object updateStatus() { return null; }

    @GetMapping(("/wrapped"))
    public Object wrapped() { return null; }
}`)

	bundle := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	wants := []struct {
		method  string
		path    string
		handler string
	}{
		{method: "GET", path: "/v1/items", handler: "list"},
		{method: "GET", path: "/legacy/records", handler: "list"},
		{method: "POST", path: "/v1", handler: "create"},
		{method: "POST", path: "/legacy", handler: "create"},
		{method: "GET", path: "/v1/bulk", handler: "bulk"},
		{method: "POST", path: "/legacy/batch", handler: "bulk"},
		{method: "DELETE", path: "/v1/gone", handler: "delete"},
		{method: "GET", path: "/v1/items/{id}", handler: "getById"},
		{method: "PUT", path: "/legacy/apps/{name}/status?value={status}", handler: "updateStatus"},
		{method: "GET", path: "/v1/wrapped", handler: "wrapped"},
	}
	for _, want := range wants {
		endpoint := findEndpoint(bundle.Endpoints, want.method, want.path)
		if endpoint == nil {
			t.Fatalf("%s %s not indexed; endpoints=%v", want.method, want.path, endpointPaths(bundle.Endpoints))
		}
		if endpoint.HandlerMethod != want.handler {
			t.Errorf("%s %s handler = %q, want %q", want.method, want.path, endpoint.HandlerMethod, want.handler)
		}
	}
	if endpoint := findEndpoint(bundle.Endpoints, "POST", "/v1/application/json"); endpoint != nil {
		t.Fatalf("non-path annotation value was indexed as a route: %+v", endpoint)
	}
}

func TestScanKotlinServicesRegistersLibraryModules(t *testing.T) {
	root := t.TempDir()
	base := "repos/mobile/shared-api"
	writeFile(t, root, base+"/build.gradle.kts", `plugins { kotlin("jvm") version "2.0.0" }`)
	writeFile(t, root, base+"/src/main/kotlin/api/SharedController.kt", `package api
import org.springframework.web.bind.annotation.GetMapping
import org.springframework.web.bind.annotation.RestController
@RestController
class SharedController {
    @GetMapping("/shared")
    fun shared(): String = "ok"
}`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "shared-api")
	if service == nil {
		t.Fatalf("shared-api service not found; services=%v", serviceNames(b.Services))
	}
	if service.Runtime != "" {
		t.Errorf("library runtime = %q, want empty", service.Runtime)
	}
	endpoint := findEndpoint(b.Endpoints, "GET", "/shared")
	if endpoint == nil || endpoint.ServiceKey != service.ServiceKey {
		t.Fatalf("library endpoint ownership = %+v, service=%+v", endpoint, service)
	}
}

func TestScanNodeJSRoutesPreservesFrameworkSemantics(t *testing.T) {
	root := t.TempDir()
	base := "repos/web/orders-api"
	writeFile(t, root, base+"/package.json", `{"name":"@demo/orders-api"}`)
	writeFile(t, root, base+"/src/orders.controller.ts", `import { Controller, Get } from "@nestjs/common";

@Controller("orders")
export class OrdersController {
  @Get(":id")
  findOne() {}
}`)
	writeFile(t, root, base+"/src/hapi.ts", `import * as Hapi from "@hapi/hapi";
const server = Hapi.server({});
server.route({ path: "/health", method: "GET", handler })`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	for _, want := range []struct {
		method string
		path   string
	}{{"GET", "/orders/:id"}, {"GET", "/health"}} {
		if endpoint := findEndpoint(b.Endpoints, want.method, want.path); endpoint == nil {
			t.Fatalf("%s %s not indexed; endpoints=%v", want.method, want.path, endpointPaths(b.Endpoints))
		}
	}
	if endpoint := findEndpoint(b.Endpoints, "ANY", "/orders"); endpoint != nil {
		t.Fatalf("controller prefix incorrectly indexed as endpoint: %+v", endpoint)
	}
}

func TestNodeJSPackageWithoutRuntimeEvidenceIsNotAnApplication(t *testing.T) {
	root := t.TempDir()
	base := "repos/web/shared-types"
	writeFile(t, root, base+"/package.json", `{"name":"@demo/shared-types","main":"dist/index.js"}`)
	writeFile(t, root, base+"/src/index.ts", "export type User = { id: string };\n")

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "shared-types")
	if service == nil {
		t.Fatalf("shared package was not registered; services=%v", serviceNames(b.Services))
	}
	if service.Runtime != "" || len(service.Entrypoints) != 0 {
		t.Fatalf("shared package runtime evidence = %+v, want no runtime", service)
	}
}

func TestPythonServiceDoesNotInferBusinessTagFromRepositoryName(t *testing.T) {
	root := t.TempDir()
	base := "repos/ai/catalog-api"
	writeFile(t, root, base+"/pyproject.toml", "[project]\nname = \"catalog-api\"\n")
	writeFile(t, root, base+"/main.py", "from fastapi import FastAPI\napp = FastAPI()\n")

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "catalog-api")
	if service == nil {
		t.Fatalf("catalog-api service was not registered; services=%v", serviceNames(b.Services))
	}
	for _, tag := range service.Tags {
		if tag == "ai" {
			t.Fatalf("service received business tag from repository path: %+v", service)
		}
	}
}

func TestScanGoServicesDedupesByRepositoryModule(t *testing.T) {
	root := t.TempDir()
	for _, repo := range []string{"team-a", "team-b"} {
		base := "repos/" + repo + "/gateway"
		writeFile(t, root, base+"/go.mod", "module example.com/"+repo+"/gateway\n\ngo 1.22\n")
		writeFile(t, root, base+"/main.go", "package main\nfunc main() {}\n")
	}

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	count := 0
	for _, service := range b.Services {
		if service.ServiceName == "gateway" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("gateway services = %d, want 2; services=%+v", count, b.Services)
	}
}

func TestScanCSharpServicesRegistersControllerLibrary(t *testing.T) {
	root := t.TempDir()
	base := "repos/dotnet/shared-api"
	writeFile(t, root, base+"/Shared.Api.csproj", `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`)
	writeFile(t, root, base+"/Controllers/ItemsController.cs", `[ApiController]
using Microsoft.AspNetCore.Mvc;
[Route("items")]
public class ItemsController : ControllerBase {
    [HttpGet("list")]
    public string List() { return "ok"; }
}`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "Shared.Api")
	if service == nil {
		t.Fatalf("Shared.Api service not found; services=%v", serviceNames(b.Services))
	}
	if endpoint := findEndpoint(b.Endpoints, "GET", "/items/list"); endpoint == nil || endpoint.ServiceKey != service.ServiceKey {
		t.Fatalf("controller library endpoint ownership = %+v, service=%+v", endpoint, service)
	}
}

func TestAndroidModuleIsNotRegisteredAsKotlinServer(t *testing.T) {
	root := t.TempDir()
	base := "repos/mobile/client"
	writeFile(t, root, base+"/build.gradle.kts", `android { namespace = "com.example.client" }`)
	writeFile(t, root, base+"/src/main/AndroidManifest.xml", `<manifest package="com.example.client"></manifest>`)
	writeFile(t, root, base+"/src/main/kotlin/App.kt", `class App`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "com.example.client")
	if service == nil {
		t.Fatalf("Android service not found; services=%v", serviceNames(b.Services))
	}
	if service.Layer != "app" || service.Runtime != "android" {
		t.Fatalf("Android service layer/runtime = %q/%q, want app/android", service.Layer, service.Runtime)
	}
}

func TestScanIOSServicesFindsXcodeProjectDirectories(t *testing.T) {
	root := t.TempDir()
	base := "repos/mobile/ios-client"
	writeFile(t, root, base+"/Client.xcodeproj/project.pbxproj", `PRODUCT_NAME = ClientApp;`)
	writeFile(t, root, base+"/Sources/AppDelegate.swift", `final class AppDelegate {}`)

	b := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	service := findService(b.Services, "ClientApp")
	if service == nil {
		t.Fatalf("ClientApp service not found; services=%v", serviceNames(b.Services))
	}
	if service.Language != "swift" || service.Runtime != "ios" || service.Layer != "app" {
		t.Fatalf("iOS service language/runtime/layer = %q/%q/%q", service.Language, service.Runtime, service.Layer)
	}
}

func TestBuildStructuralBundleExcludesDocumentsOnlyForScannerCallers(t *testing.T) {
	root := miniWorkspace(t)
	bundle := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	if len(bundle.Runbooks) != 0 {
		t.Fatalf("structural scanner runbooks = %#v", bundle.Runbooks)
	}
	if len(bundle.Services) == 0 || len(bundle.Endpoints) == 0 {
		t.Fatalf("structural scanner lost code records: services=%d endpoints=%d", len(bundle.Services), len(bundle.Endpoints))
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

func serviceNames(list []domain.ServiceRecord) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ServiceName)
	}
	return out
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

func TestCSharpRefitRequiresActualMethodCall(t *testing.T) {
	root := t.TempDir()
	base := "repos/dotnet/client"
	writeFile(t, root, base+"/Client.csproj", `<Project Sdk="Microsoft.NET.Sdk"></Project>`)
	writeFile(t, root, base+"/IUsersApi.cs", `using Refit;
[BaseAddress("https://users.example.com")]
public interface IUsersApi {
    [Get("/users")] Task<string> GetUsers();
}`)
	writeFile(t, root, base+"/Consumer.cs", `public class Consumer {
    private readonly IUsersApi api;
    public Consumer(IUsersApi api) { this.api = api; }
}`)
	if edges := scanCSharpRefits(root, mustDiscoverScanDirs(t, root)); len(edges) != 0 {
		t.Fatalf("unused Refit client produced dependencies: %+v", edges)
	}
	writeFile(t, root, base+"/Consumer.cs", `public class Consumer {
    private readonly IUsersApi api;
    public Consumer(IUsersApi api) { this.api = api; }
    public Task<string> Run() { return api.GetUsers(); }
}`)
	edges := scanCSharpRefits(root, mustDiscoverScanDirs(t, root))
	if len(edges) != 1 || edges[0].To != "users.example.com" {
		t.Fatalf("actual Refit call = %+v, want one dependency", edges)
	}
	if len(edges[0].Evidence) < 2 || edges[0].Evidence[0].Line <= 0 {
		t.Fatalf("Refit evidence = %+v, want call and declaration evidence", edges[0].Evidence)
	}
}

func TestAndroidRetrofitRequiresActualMethodCall(t *testing.T) {
	root := t.TempDir()
	base := "repos/mobile/client"
	writeFile(t, root, base+"/build.gradle.kts", `android { namespace = "com.example.client" }`)
	writeFile(t, root, base+"/src/main/AndroidManifest.xml", `<manifest package="com.example.client"></manifest>`)
	writeFile(t, root, base+"/src/main/kotlin/UsersApi.kt", `import retrofit2.http.GET
interface UsersApi {
    @GET("https://users.example.com/users")
    suspend fun getUsers(): String
}`)
	writeFile(t, root, base+"/src/main/kotlin/Consumer.kt", `class Consumer(private val api: UsersApi)`)
	if edges := scanAndroidDependencies(root, mustDiscoverScanDirs(t, root)); len(edges) != 0 {
		t.Fatalf("unused Retrofit client produced dependencies: %+v", edges)
	}
	writeFile(t, root, base+"/src/main/kotlin/Consumer.kt", `class Consumer(private val api: UsersApi) {
    suspend fun run() = api.getUsers()
}`)
	edges := scanAndroidDependencies(root, mustDiscoverScanDirs(t, root))
	if len(edges) != 1 || edges[0].To != "users.example.com" {
		t.Fatalf("actual Retrofit call = %+v, want one dependency", edges)
	}
	if len(edges[0].Evidence) < 2 || edges[0].Evidence[0].Line <= 0 {
		t.Fatalf("Retrofit evidence = %+v, want call and declaration evidence", edges[0].Evidence)
	}
}
