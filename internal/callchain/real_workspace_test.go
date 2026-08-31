package callchain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

func realWorkspaceRoot(t *testing.T) string {
	t.Helper()
	if os.Getenv("NASUTA_RUN_REAL_WORKSPACE_TESTS") != "1" {
		t.Skip("real workspace tests disabled; set NASUTA_RUN_REAL_WORKSPACE_TESTS=1 to enable")
	}
	root := strings.TrimSpace(os.Getenv("NASUTA_REAL_WORKSPACE_ROOT"))
	if root == "" {
		t.Skip("real workspace tests require NASUTA_REAL_WORKSPACE_ROOT")
	}
	return filepath.Clean(root)
}

func openRealWorkspace(t *testing.T) (*store.SQLite, *codegraph.DB) {
	t.Helper()
	root := realWorkspaceRoot(t)
	databasePath := filepath.Join(root, ".nasuta", "index.db")
	if _, err := os.Stat(databasePath); err != nil {
		t.Skipf("real workspace index is unavailable: %v", err)
	}
	structure, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := codegraph.Open(root)
	if err != nil {
		structure.Close()
		t.Fatal(err)
	}
	if graph == nil {
		structure.Close()
		t.Skip("real workspace codegraph is unavailable")
	}
	t.Cleanup(func() {
		_ = graph.Close()
		_ = structure.Close()
	})
	return structure, graph
}

func TestRealWorkspaceFeignChainClosesAcrossModules(t *testing.T) {
	structure, graph := openRealWorkspace(t)
	service := New(structure, graph)
	result, err := service.Trace(context.Background(), Request{
		File:      "repos/hsas/hsas-dreo-app/hsas-share/src/main/java/com/hesung/hsas/share/controller/RoomDeviceController.java",
		Line:      63,
		Direction: "callees",
		MaxDepth:  5,
		MaxNodes:  80,
		MaxFanout: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Target == nil || result.Target.Name != "getDevices4Share" {
		t.Fatalf("target = %#v", result.Target)
	}
	var bridge bool
	var depths []int
	for _, hop := range result.Callees.Hops {
		depths = append(depths, hop.Depth)
		if hop.Bridge && hop.Target.ServiceName == "hsds-device-share-provider" &&
			strings.HasSuffix(hop.Target.FilePath, "/hsds-device-share-provider/src/main/java/com/hesung/hsds/device/share/controller/FamilyRoomDeviceController.java") {
			bridge = true
		}
	}
	if !bridge {
		t.Fatalf("no Feign service bridge in hops: %#v", result.Callees.Hops)
	}
	for _, want := range []int{1, 2, 3, 4} {
		found := false
		for _, depth := range depths {
			if depth == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("depth %d missing from multi-layer chain: %v", want, depths)
		}
	}
	if len(result.Callees.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved Feign chain items: %v", result.Callees.Unresolved)
	}
}

func TestRealWorkspaceHTTPWebClientChainClosesIntoPythonEndpoint(t *testing.T) {
	structure, graph := openRealWorkspace(t)
	service := New(structure, graph)
	result, err := service.Trace(context.Background(), Request{
		File:      "repos/hsas/hsas-aiot-application/src/main/java/com/hesung/hsas/aiot/application/controller/LightingEffectController.java",
		Line:      39,
		Direction: "callees",
		MaxDepth:  3,
		MaxNodes:  40,
		MaxFanout: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Callees.Hops) == 0 {
		t.Fatalf("HTTP client chain is empty: %#v", result.Callees)
	}
	found := false
	for _, hop := range result.Callees.Hops {
		if !hop.Bridge || hop.Edge.Provenance != string(domain.EdgeHTTP) {
			continue
		}
		if hop.Target.ServiceName == "hsds-aiot-service" && hop.Target.Name == "lighting_effect_router" &&
			strings.HasSuffix(hop.Target.FilePath, "/hsds-aiot-service/router/lighting_effect_router.py") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("HTTP bridge to Python endpoint missing: %#v", result.Callees.Hops)
	}
	if len(result.Callees.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved HTTP chain items: %v", result.Callees.Unresolved)
	}
}

func TestRealWorkspaceHTTPWebClientChainSupportsExactUpstreamRoute(t *testing.T) {
	structure, graph := openRealWorkspace(t)
	service := New(structure, graph)
	result, err := service.Trace(context.Background(), Request{
		File:      "repos/hsds/hsds-aiot-service/router/lighting_effect_router.py",
		Line:      21,
		Direction: "callers",
		MaxDepth:  2,
		MaxNodes:  40,
		MaxFanout: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hop := range result.Callers.Hops {
		if hop.Bridge && hop.Edge.Provenance == string(domain.EdgeHTTP) &&
			hop.Source.ServiceName == "hsas-aiot-application" && hop.Source.Name == "chat_stream" &&
			strings.HasSuffix(hop.Source.FilePath, "/hsas-aiot-application/src/main/java/com/hesung/hsas/aiot/application/controller/LightingEffectController.java") {
			found = true
		}
	}
	if !found {
		t.Fatalf("exact HTTP upstream bridge missing: %#v", result.Callers.Hops)
	}
	if len(result.Callers.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved HTTP callers: %v", result.Callers.Unresolved)
	}
}

func TestRealWorkspaceRestTemplateEvidenceRemainsExplicitWhenURLIsConfigured(t *testing.T) {
	structure, graph := openRealWorkspace(t)
	voiceTestFile := "repos/hsds/hsds-voice-manage/hsds-voice-manage-provider/src/main/java/com/hesung/hsds/voice/service/impl/VoiceTestServiceImpl.java"
	method, path, ok := graph.HTTPClientRouteAt(voiceTestFile, 80)
	if ok || method != "" || path != "" {
		t.Fatalf("configured RestTemplate URL was guessed as %s %s", method, path)
	}
	edges, err := structure.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, edge := range edges {
		if edge.Type != domain.EdgeHTTP || edge.ExternalTarget != "10.10.4.185" {
			continue
		}
		for _, evidence := range edge.Evidence {
			if evidence.Path == voiceTestFile && evidence.Line == 80 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("real RestTemplate evidence was not indexed")
	}
}
