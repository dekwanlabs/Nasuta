package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/platform"
)

func dashboardContractService(repo, name string) domain.ServiceRecord {
	return domain.ServiceRecord{
		ServiceKey: platform.UUIDFromString(repo + "\x00."), Repo: repo, ModulePath: ".", ServiceName: name,
		Layer: "hsds", Language: "java", Status: "active",
		Tags: []string{}, Docs: []string{}, SourceOfTruth: []string{},
		Entrypoints: []domain.Evidence{}, Ports: []int{}, Confidence: 1,
	}
}

func dashboardAPIData(t *testing.T, body []byte) any {
	t.Helper()
	var response domain.ApiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode response %s: %v", body, err)
	}
	return response.Data
}

func TestDashboardStructureAPIsCloseOverOneSnapshot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	caller := dashboardContractService("repo/caller", "caller")
	target := dashboardContractService("repo/target", "target")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{
			{Repo: caller.Repo, HeadSHA: "caller-sha", IndexedAt: time.Now().UnixMilli()},
			{Repo: target.Repo, HeadSHA: "target-sha", IndexedAt: time.Now().UnixMilli()},
		},
		Services: []domain.ServiceRecord{caller, target},
		Endpoints: []domain.EndpointRecord{
			{ServiceKey: caller.ServiceKey, ServiceName: caller.ServiceName, Repo: caller.Repo, Method: "GET", Path: "/caller", File: "repos/caller/src/main/Caller.java", Line: 10, Source: domain.SourceCodeScan, Confidence: 1},
			{ServiceKey: target.ServiceKey, ServiceName: target.ServiceName, Repo: target.Repo, Method: "GET", Path: "/target", File: "repos/target/src/main/Target.java", Line: 20, Source: domain.SourceCodeScan, Confidence: 1},
		},
		Dependencies: []domain.DependencyEdge{
			{CallerServiceKey: caller.ServiceKey, TargetKind: domain.DependencyTargetService, TargetServiceKey: target.ServiceKey, From: caller.ServiceName, To: target.ServiceName, Type: domain.EdgeFeign, Confidence: .9, Evidence: []domain.Evidence{{Path: "repos/caller/src/main/Caller.java", Line: 30, Kind: domain.SourceCodeScan}}},
			{CallerServiceKey: caller.ServiceKey, TargetKind: domain.DependencyTargetExternal, ExternalTarget: "api.example.com", From: caller.ServiceName, To: "api.example.com", Type: domain.EdgeHTTP, Confidence: .8, Evidence: []domain.Evidence{{Path: "repos/caller/src/main/Caller.java", Line: 40, Kind: domain.SourceCodeScan}}},
		},
	}
	if err := db.ReplaceStructure(context.Background(), "dashboard-contract", bundle); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}

	var summary map[string]any
	{
		recorder := httptest.NewRecorder()
		h.APISummary(recorder, httptest.NewRequest("GET", "/summary", nil))
		data, ok := dashboardAPIData(t, recorder.Body.Bytes()).(map[string]any)
		if !ok {
			t.Fatalf("summary data = %#v", dashboardAPIData(t, recorder.Body.Bytes()))
		}
		summary = data
	}
	services := callDashboardContract(t, h.APIServices, "/services")
	edges := callDashboardContract(t, h.APIEdges, "/edges")
	endpoints := callDashboardContract(t, h.APIEndpoints, "/endpoints?page=1&page_size=100000")

	if int(summary["services"].(float64)) != len(services.([]any)) {
		t.Fatalf("summary/services mismatch: %#v vs %#v", summary, services)
	}
	endpointPage, ok := endpoints.(map[string]any)
	if !ok {
		t.Fatalf("endpoints data = %#v", endpoints)
	}
	if int(summary["endpoints"].(float64)) != int(endpointPage["total"].(float64)) {
		t.Fatalf("summary/endpoints mismatch: %#v vs %#v", summary, endpointPage)
	}
	if int(summary["dependencies"].(float64)) != len(edges.([]any)) {
		t.Fatalf("summary/edges mismatch: %#v vs %#v", summary, edges)
	}

	known := map[string]bool{}
	for _, raw := range services.([]any) {
		known[raw.(map[string]any)["serviceName"].(string)] = true
	}
	for _, raw := range endpointPage["list"].([]any) {
		service := raw.(map[string]any)["serviceName"].(string)
		if !known[service] {
			t.Fatalf("endpoint references unknown service %q", service)
		}
	}
	for _, raw := range edges.([]any) {
		edge := raw.(map[string]any)
		if !known[edge["from"].(string)] {
			t.Fatalf("edge caller references unknown service: %#v", edge)
		}
		if edge["targetKind"] == "service" && !known[edge["to"].(string)] {
			t.Fatalf("internal edge target references unknown service: %#v", edge)
		}
	}
}

func callDashboardContract(t *testing.T, handler func(http.ResponseWriter, *http.Request), path string) any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest("GET", path, nil))
	return dashboardAPIData(t, recorder.Body.Bytes())
}
