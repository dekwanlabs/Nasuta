package indexer

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestProtocolDependencyScannersCoverJVMAndPython(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/orders/pom.xml", `<project><artifactId>orders</artifactId></project>`)
	writeFile(t, root, "repos/team/orders/src/Clients.java", `
class Clients {
  void call() {
    restTemplate.getForObject("http://payments/api/pay", String.class);
    ManagedChannelBuilder.forAddress("inventory:9090", 9090);
  }
  @DubboReference(interfaceClass = PricingService.class)
  PricingService pricing;
}`)
	writeFile(t, root, "repos/team/analytics/.env.example", "APP_NAME=analytics\n")
	writeFile(t, root, "repos/team/analytics/main.py", `
import grpc, requests
requests.get("http://profile/api")
grpc.insecure_channel("events:50051")
`)

	edges := scanJVMAndPythonDependencies(root, mustDiscoverScanDirs(t, root))
	want := map[string]struct{}{
		"orders\x00payments\x00http": {}, "orders\x00inventory\x00grpc": {},
		"orders\x00PricingService\x00rpc": {}, "analytics\x00profile\x00http": {},
		"analytics\x00events\x00grpc": {},
	}
	for _, edge := range edges {
		delete(want, edge.From+"\x00"+edge.To+"\x00"+string(edge.Type))
		if edge.CallerServiceKey == "" {
			t.Fatalf("protocol edge lost caller ownership: %+v", edge)
		}
		if len(edge.Evidence) != 1 || edge.Evidence[0].Line <= 0 {
			t.Fatalf("edge has no precise evidence: %+v", edge)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing protocol edges: %v; got %+v", want, edges)
	}
}

func TestKafkaScannerJoinsProducerToConsumer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/orders/pom.xml", `<project><artifactId>orders</artifactId></project>`)
	writeFile(t, root, "repos/team/orders/src/Publisher.java", `class Publisher { void run() { kafkaTemplate.send("order-created", event); } }`)
	writeFile(t, root, "repos/team/billing/pom.xml", `<project><artifactId>billing</artifactId></project>`)
	writeFile(t, root, "repos/team/billing/src/Listener.java", `class Listener { @KafkaListener(topics = "order-created") void consume() {} }`)

	edges := scanKafkaDependencies(root, mustDiscoverScanDirs(t, root))
	if len(edges) != 1 {
		t.Fatalf("edges=%+v", edges)
	}
	edge := edges[0]
	if edge.From != "orders" || edge.To != "billing" || edge.Type != domain.EdgeKafka || len(edge.Evidence) != 2 {
		t.Fatalf("joined kafka edge=%+v", edge)
	}
	if edge.CallerServiceKey == "" {
		t.Fatalf("Kafka edge lost producer ownership: %+v", edge)
	}
}

func TestProtocolDependencyScannersIgnoreTestSourcesAndFeign(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/orders/pom.xml", `<project><artifactId>orders</artifactId></project>`)
	writeFile(t, root, "repos/team/orders/src/test/java/com/example/ClientTest.java", `
class ClientTest { void call() { restTemplate.getForObject("http://test-only/api", String.class); } }
`)
	writeFile(t, root, "repos/team/orders/src/main/java/com/example/RemoteClient.java", `
@FeignClient(url = "http://payments/api")
interface RemoteClient {}
`)

	edges := scanJVMAndPythonDependencies(root, mustDiscoverScanDirs(t, root))
	if len(edges) != 0 {
		t.Fatalf("generic protocol scanner produced test or Feign edges: %+v", edges)
	}
}

func TestLiteralURLsDoNotCreateDependenciesWithoutClientCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/node/package.json", `{"name":"node-client"}`)
	writeFile(t, root, "repos/team/node/config.js", `const upstream = "https://config.example.com/api";`)
	writeFile(t, root, "repos/team/go/go.mod", "module example.com/go-client\n")
	writeFile(t, root, "repos/team/go/config.go", `package main
const upstream = "https://config.example.com/api"`)
	writeFile(t, root, "repos/team/dotnet/Client.csproj", `<Project></Project>`)
	writeFile(t, root, "repos/team/dotnet/Config.cs", `class Config { const string Upstream = "https://config.example.com/api"; }`)
	writeFile(t, root, "repos/team/java/pom.xml", `<project><artifactId>java-client</artifactId></project>`)
	writeFile(t, root, "repos/team/java/Config.java", `class Config { static final String UPSTREAM = "http://config.example.com/api"; }`)

	dirs := mustDiscoverScanDirs(t, root)
	for name, edges := range map[string][]domain.DependencyEdge{
		"node":   scanNodeJSDependencies(root, dirs),
		"go":     scanGoDependencies(root, dirs),
		"csharp": scanCSharpDependencies(root, dirs),
		"jvm":    scanJVMAndPythonDependencies(root, dirs),
	} {
		if len(edges) != 0 {
			t.Errorf("%s literal URL constants produced dependencies: %+v", name, edges)
		}
	}
}

func TestProtocolURLsRequireConcreteCallsAndSupportBoundVariables(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/java/pom.xml", `<project><artifactId>java-client</artifactId></project>`)
	writeFile(t, root, "repos/team/java/src/main/java/Clients.java", `class Clients {
	static final String UNUSED = "http://unused.example.com/api";
	void call() {
		String endpoint = "http://payments.example.com/api";
		restTemplate
			.getForObject(endpoint, String.class);
	}
}`)
	writeFile(t, root, "repos/team/go/go.mod", "module example.com/go-client\n")
	writeFile(t, root, "repos/team/go/client.go", `package main
import "net/http"
func call() {
  endpoint := "http://inventory.example.com/api"
  req, _ := http.NewRequest("GET", endpoint, nil)
  (&http.Client{}).Do(req)

}`)
	writeFile(t, root, "repos/team/node/package.json", `{"name":"node-client"}`)
	writeFile(t, root, "repos/team/node/client.js", `const endpoint = "https://profile.example.com/api";
fetch(
  endpoint,
  { method: "GET" },
);`)
	writeFile(t, root, "repos/team/dotnet/Client.csproj", `<Project></Project>`)
	writeFile(t, root, "repos/team/dotnet/Client.cs", `class Client {
  const string Unused = "https://unused.example.com/api";
  async Task Call(HttpClient client) {
    var endpoint = "https://billing.example.com/api";
    await client.GetAsync(endpoint);
  }
}`)

	dirs := mustDiscoverScanDirs(t, root)
	checks := []struct {
		name  string
		edges []domain.DependencyEdge
		want  string
	}{
		{"jvm", scanJVMAndPythonDependencies(root, dirs), "payments.example.com"},
		{"go", scanGoDependencies(root, dirs), "inventory.example.com"},
		{"node", scanNodeJSDependencies(root, dirs), "profile.example.com"},
		{"csharp", scanCSharpDependencies(root, dirs), "billing.example.com"},
	}
	for _, check := range checks {
		found := false
		for _, edge := range check.edges {
			if edge.To == check.want {
				found = true
			}
			if strings.Contains(edge.To, "unused.example.com") {
				t.Errorf("%s emitted an unused URL dependency: %+v", check.name, edge)
			}
		}
		if !found {
			t.Errorf("%s did not resolve the URL used by a concrete client call: %+v", check.name, check.edges)
		}
	}
}

func TestIOSURLsRequireConcreteRequestUsage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "repos/team/ios/Info.plist", `<plist><dict><key>CFBundleName</key><string>ios-client</string></dict></plist>`)
	writeFile(t, root, "repos/team/ios/Client.swift", `import Foundation
let unused = URL(string: "https://unused.example.com/api")
let endpoint = URL(string: "https://mobile.example.com/api")!
let request = URLRequest(url: endpoint)
URLSession.shared.dataTask(with: request) { _, _, _ in }.resume()`)
	writeFile(t, root, "repos/team/ios/Unused.swift", `import Foundation
let endpoint = URL(string: "https://not-called.example.com/api")`)

	edges := scanIOSDependencies(root, mustDiscoverScanDirs(t, root))
	found := false
	for _, edge := range edges {
		if edge.To == "mobile.example.com" {
			found = true
		}
		if strings.Contains(edge.To, "unused.example.com") || strings.Contains(edge.To, "not-called.example.com") {
			t.Fatalf("iOS URL declaration without a request was indexed: %+v", edge)
		}
	}
	if !found {
		t.Fatalf("iOS URL used by URLSession request was not indexed: %+v", edges)
	}
}
