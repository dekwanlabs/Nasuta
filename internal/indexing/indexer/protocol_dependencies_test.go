package indexer

import (
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
}
