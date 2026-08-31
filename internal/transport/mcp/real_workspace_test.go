package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/ontologystore"
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

func realMCPClient(t *testing.T) *client.Client {
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
		_ = structure.Close()
		t.Fatal(err)
	}
	if graph == nil {
		_ = structure.Close()
		t.Skip("real workspace codegraph is unavailable")
	}
	t.Cleanup(func() {
		_ = graph.Close()
		_ = structure.Close()
	})
	backend, err := ontologystore.New(config.OntologyConfig{Provider: "sqlite"}, structure)
	if err != nil {
		t.Fatal(err)
	}
	chain := callchain.New(structure, graph)
	service := tools.New(tools.Deps{
		DB: structure, WorkspaceRoot: root,
		CallChain: chain, Ontology: ontology.NewService(backend),
	})
	registry := tools.NewRegistry(service, config.Config{}, memory.NewSessionStore(nil), nil)
	mcpServer, err := BuildMCP(registry)
	if err != nil {
		t.Fatal(err)
	}
	mcpClient, err := client.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mcpClient.Close() })
	if err := mcpClient.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "nasuta-real-chain-test", Version: "1"}
	if _, err := mcpClient.Initialize(t.Context(), initialize); err != nil {
		t.Fatal(err)
	}
	return mcpClient
}

func callMCPJSON(t *testing.T, mcpClient *client.Client, name string, arguments map[string]any) map[string]any {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = arguments
	result, err := mcpClient.CallTool(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("%s content = %#v", name, result.Content)
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("%s content type = %T", name, result.Content[0])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content.Text), &payload); err != nil {
		t.Fatalf("%s payload = %s: %v", name, content.Text, err)
	}
	return payload
}

func TestRealWorkspaceMCPTraceCallsClosesHTTPChain(t *testing.T) {
	mcpClient := realMCPClient(t)
	payload := callMCPJSON(t, mcpClient, "trace_calls", map[string]any{
		"file":       "repos/hsas/hsas-aiot-application/src/main/java/com/hesung/hsas/aiot/application/controller/LightingEffectController.java",
		"line":       39,
		"direction":  "callees",
		"max_depth":  3,
		"max_nodes":  40,
		"max_fanout": 20,
	})
	callees, ok := payload["callees"].(map[string]any)
	if !ok {
		t.Fatalf("callees = %#v", payload["callees"])
	}
	if unresolved, _ := callees["unresolved"].([]any); len(unresolved) != 0 {
		t.Fatalf("MCP trace_calls unresolved = %#v", unresolved)
	}
	nodes, ok := callees["nodes"].([]any)
	if !ok || len(nodes) == 0 {
		t.Fatalf("MCP trace_calls nodes = %#v", callees["nodes"])
	}
	found := false
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if node["service"] == "hsds-aiot-service" && node["function"] == "lighting_effect_router" &&
			strings.HasSuffix(node["file"].(string), "/hsds-aiot-service/router/lighting_effect_router.py") && node["bridge"] == true {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP trace_calls did not expose HTTP bridge: %#v", nodes)
	}
}

func TestRealWorkspaceMCPTraceDepsKeepsServiceLevelContract(t *testing.T) {
	mcpClient := realMCPClient(t)
	payload := callMCPJSON(t, mcpClient, "trace_deps", map[string]any{
		"service":   "hsas-aiot-application",
		"direction": "downstream",
		"depth":     2,
	})
	if payload["service"] != "hsas-aiot-application" {
		t.Fatalf("trace_deps service = %#v", payload["service"])
	}
	downstream, ok := payload["downstream"].([]any)
	if !ok || len(downstream) == 0 {
		t.Fatalf("trace_deps downstream = %#v", payload["downstream"])
	}
	foundHTTP := false
	for _, raw := range downstream {
		edge, _ := raw.(map[string]any)
		if edge["to"] == "hsds-aiot-service" && edge["type"] == "http" {
			foundHTTP = true
		}
	}
	if !foundHTTP {
		t.Fatalf("trace_deps did not expose indexed HTTP service edge: %#v", downstream)
	}
}

func TestRealWorkspaceMCPToolCatalogAndTraceFlagAreComplete(t *testing.T) {
	mcpClient := realMCPClient(t)
	result, err := mcpClient.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{
		"get_service": false, "trace_deps": false, "list_apis": false,
		"search_code": false, "get_symbol": false, "trace_calls": false,
		"search_runbooks": false, "check_docs": false, "trace_relations": false,
	}
	for _, candidate := range result.Tools {
		if _, ok := expected[candidate.Name]; !ok {
			continue
		}
		expected[candidate.Name] = true
		if _, ok := candidate.InputSchema.Properties["_trace"]; !ok {
			t.Errorf("%s is missing the shared _trace opt-in", candidate.Name)
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("MCP tool %q is not registered", name)
		}
	}
}

func TestRealWorkspaceMCPTraceDepsKeepsRestTemplateExternalEvidence(t *testing.T) {
	mcpClient := realMCPClient(t)
	payload := callMCPJSON(t, mcpClient, "trace_deps", map[string]any{
		"service":   "hsds-voice-manage-provider",
		"direction": "downstream",
		"depth":     1,
	})
	downstream, ok := payload["downstream"].([]any)
	if !ok || len(downstream) == 0 {
		t.Fatalf("trace_deps downstream = %#v", payload["downstream"])
	}
	found := false
	for _, raw := range downstream {
		edge, _ := raw.(map[string]any)
		if edge["type"] == "http" && edge["externalTarget"] == "10.10.4.185" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("MCP trace_deps lost RestTemplate external evidence: %#v", downstream)
	}
}

func TestRealWorkspaceMCPListApisResolvesDeviceShareProvider(t *testing.T) {
	mcpClient := realMCPClient(t)
	payload := callMCPJSON(t, mcpClient, "list_apis", map[string]any{
		"service": "hsds-device-share-provider",
		"keyword": "/family/me/room/devices",
		"limit":   10,
	})
	matches, ok := payload["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatalf("list_apis matches = %#v", payload["matches"])
	}
	found := false
	for _, raw := range matches {
		match, _ := raw.(map[string]any)
		if match["method"] == "GET" && match["path"] == "/family/me/room/devices" &&
			strings.HasSuffix(match["file"].(string), "/hsds-device-share-provider/src/main/java/com/hesung/hsds/device/share/controller/FamilyRoomDeviceController.java") {
			found = true
		}
	}
	if !found {
		t.Fatalf("list_apis returned the wrong device-share route: %#v", matches)
	}
}
