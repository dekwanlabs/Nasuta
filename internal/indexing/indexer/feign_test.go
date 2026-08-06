package indexer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

type configResolverFunc func(
	context.Context,
	[]config.Ref,
) (map[config.Ref]config.Value, error)

func (fn configResolverFunc) ResolveConfig(
	ctx context.Context,
	refs []config.Ref,
) (map[config.Ref]config.Value, error) {
	return fn(ctx, refs)
}

func TestFeignDependenciesResolveExternalConfiguration(t *testing.T) {
	root := feignWorkspace(t, `api:
  url-online-service-name: wrong-online-service
  url-offline-service-name: wrong-offline-service
openai:
  url: https://wrong.example.com/v1
`)
	var requested []config.Ref
	resolver := configResolverFunc(func(
		_ context.Context,
		refs []config.Ref,
	) (map[config.Ref]config.Value, error) {
		requested = append(requested, refs...)
		return map[config.Ref]config.Value{
			{Application: "hsds-offline-cookbook", Key: "api.url-online-service-name"}: {
				Value: "hsds-online-service", Source: "config-center/na/application/hsds-offline-cookbook/api.url-online-service-name",
			},
			{Application: "hsds-offline-cookbook", Key: "api.url-offline-service-name"}: {
				Value: "hsds-offline-service", Source: "config-center/na/application/hsds-offline-cookbook/api.url-offline-service-name",
			},
			{Application: "hsds-offline-cookbook", Key: "openai.url"}: {
				Value: "https://api.openai.com/v1", Source: "config-center/na/application/hsds-offline-cookbook/openai.url",
			},
		}, nil
	})

	bundle, err := BuildBundleWithResolver(
		context.Background(), root, mustDiscoverScanDirs(t, root), nil, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requested) != 3 {
		t.Fatalf("requested config refs = %#v, want three unique refs", requested)
	}

	online := findDependency(bundle.Dependencies, "hsds-online-service")
	if online == nil || online.TargetKind != domain.DependencyTargetService {
		t.Fatalf("online dependency = %#v", online)
	}
	offline := findDependency(bundle.Dependencies, "hsds-offline-service")
	if offline == nil || offline.TargetKind != domain.DependencyTargetService {
		t.Fatalf("offline dependency = %#v", offline)
	}
	openAI := findDependency(bundle.Dependencies, "api.openai.com")
	if openAI == nil || openAI.TargetKind != domain.DependencyTargetExternal || openAI.ExternalTarget != "api.openai.com" {
		t.Fatalf("OpenAI dependency = %#v", openAI)
	}
	if !hasEvidenceKind(openAI.Evidence, domain.SourceConfig) {
		t.Fatalf("OpenAI evidence = %#v, want configuration evidence", openAI.Evidence)
	}
	for _, dependency := range bundle.Dependencies {
		if dependency.To == "openai-client" || dependency.To == "${openai.url}" ||
			dependency.To == "${api.url-online-service-name}" ||
			dependency.To == "${api.url-offline-service-name}" {
			t.Fatalf("unresolved Feign target leaked into dependency graph: %#v", dependency)
		}
	}
}

func TestFeignDependenciesIgnoreLocalSpringConfiguration(t *testing.T) {
	root := feignWorkspace(t, `api:
  url-online-service-name: hsds-online-service
  url-offline-service-name: hsds-offline-service
openai:
  url: https://api.openai.com/v1
`)
	bundle := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	if len(bundle.Dependencies) != 0 {
		t.Fatalf("dependencies = %#v, want local Spring configuration ignored", bundle.Dependencies)
	}
}

func TestFeignDependenciesDropUnresolvedConfiguration(t *testing.T) {
	root := feignWorkspace(t, "")
	bundle := BuildStructuralBundle(root, mustDiscoverScanDirs(t, root))
	if len(bundle.Dependencies) != 0 {
		t.Fatalf("dependencies = %#v, want unresolved Feign references omitted", bundle.Dependencies)
	}
}

func TestFeignDependenciesDegradeConfiguredResolverFailureWithWarning(t *testing.T) {
	root := feignWorkspace(t, "")
	writeFile(t, root, "repos/hsds/hsds-offline-cookbook/src/main/java/com/hesung/hsds/feign/StaticFeign.java", `package com.hesung.hsds.feign;
@FeignClient(name = "hsds-online-service")
public interface StaticFeign {}
`)
	writeFile(t, root, "repos/hsds/hsds-offline-cookbook/src/main/java/com/hesung/hsds/feign/DefaultFeign.java", `package com.hesung.hsds.feign;
@FeignClient(name = "${optional.service:hsds-offline-service}")
public interface DefaultFeign {}
`)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	called := 0
	resolver := configResolverFunc(func(
		context.Context,
		[]config.Ref,
	) (map[config.Ref]config.Value, error) {
		called++
		return nil, errors.New("config center unavailable")
	})
	bundle, err := BuildBundleWithResolver(
		context.Background(), root, mustDiscoverScanDirs(t, root), nil, resolver,
	)
	if err != nil {
		t.Fatalf("BuildBundleWithResolver: %v", err)
	}
	if called != 1 {
		t.Fatalf("resolver calls = %d, want 1", called)
	}
	if dependency := findDependencyFrom(bundle.Dependencies, "hsds-offline-cookbook", "hsds-online-service"); dependency == nil {
		t.Fatalf("static Feign dependency missing after resolver failure: %#v", bundle.Dependencies)
	}
	if dependency := findDependencyFrom(bundle.Dependencies, "hsds-offline-cookbook", "hsds-offline-service"); dependency == nil {
		t.Fatalf("defaulted Feign dependency missing after resolver failure: %#v", bundle.Dependencies)
	}
	for _, dependency := range bundle.Dependencies {
		if strings.Contains(dependency.To, "${") || dependency.To == "openai-client" {
			t.Fatalf("unresolved config dependency leaked into graph: %#v", dependency)
		}
	}
	got := logs.String()
	for _, want := range []string{
		"level=WARN",
		"Feign config resolver failed",
		"continuing with unresolved config-backed dependencies omitted",
		"config center unavailable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log does not contain %q:\n%s", want, got)
		}
	}
}

func TestFeignDependenciesPropagateResolverCancellation(t *testing.T) {
	root := feignWorkspace(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := configResolverFunc(func(
		ctx context.Context,
		_ []config.Ref,
	) (map[config.Ref]config.Value, error) {
		return nil, ctx.Err()
	})
	_, err := BuildBundleWithResolver(
		ctx, root, mustDiscoverScanDirs(t, root), nil, resolver,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildBundleWithResolver error = %v, want context cancellation", err)
	}
}

func TestFeignConfigExpansionSupportsNestedDefaults(t *testing.T) {
	ref := config.Ref{
		Application: "orders",
		Key:         "fallback.host",
	}
	resolver := feignConfigValueResolver{
		application: "orders",
		external: map[config.Ref]config.Value{
			ref: {Value: "payments.internal", Source: "config-center/na/application/orders/fallback.host"},
		},
		missing: make(map[config.Ref]struct{}),
	}
	value, evidence, ok := resolver.expand("${primary.host:${fallback.host}}", make(map[string]struct{}))
	if !ok || value != "payments.internal" {
		t.Fatalf("expanded value = %q ok=%v", value, ok)
	}
	if !hasEvidenceKind(evidence, domain.SourceConfig) {
		t.Fatalf("evidence = %#v, want configuration evidence", evidence)
	}
}

func TestFeignScannersIgnoreJVMComments(t *testing.T) {
	root := t.TempDir()
	source := "repos/hsds/hsds-upgrade"
	writeJavaService(t, root, source, "hsds-upgrade")
	writeFile(t, root, source+"/src/main/java/com/example/UpgradeFeign.java", `package com.example;
// @FeignClient(name = "ignored-line", url = "${ignored.line}")
@FeignClient(value = "hsds-upgrade-provider"/*, url = "${ignored.block}"*/)
public interface UpgradeFeign {}
`)
	writeFile(t, root, source+"/src/main/java/com/example/GatewayFeign.java", `package com.example;
@FeignClient(name = "gateway", url = "https://gateway.example.com/api")
public interface GatewayFeign {}
`)
	writeFile(t, root, source+"/src/main/kotlin/com/example/CatalogFeign.kt", `package com.example
// @FeignClient(name = "ignored-kotlin", url = "${ignored.kotlin.line}")
@FeignClient(name = "catalog", /* url = "${ignored.kotlin.block}" */)
interface CatalogFeign
`)

	dirs := mustDiscoverScanDirs(t, root)
	refs := append(scanFeignClients(root, dirs), scanKotlinFeigns(root, dirs)...)
	if len(refs) != 3 {
		t.Fatalf("Feign references = %#v, want three active annotations", refs)
	}
	got := make(map[string]string, len(refs))
	for _, ref := range refs {
		got[ref.ClientName] = ref.URL
	}
	want := map[string]string{
		"hsds-upgrade-provider": "",
		"gateway":               "https://gateway.example.com/api",
		"catalog":               "",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("Feign references = %#v, want %#v", got, want)
	}
}

func TestFeignDependenciesResolveAgainstRuntimeConsumers(t *testing.T) {
	root := t.TempDir()
	base := "repos/team/shared-feign"
	writeFile(t, root, base+"/pom.xml", `<project>
  <groupId>com.example</groupId>
  <artifactId>shared-feign</artifactId>
  <packaging>pom</packaging>
</project>`)
	writeFile(t, root, base+"/client-lib/pom.xml", `<project>
  <groupId>com.example</groupId>
  <artifactId>client-lib</artifactId>
</project>`)
	writeFile(t, root, base+"/client-lib/src/main/java/com/example/SharedFeign.java", `package com.example;
@FeignClient(name = "shared", url = "${shared.url}")
public interface SharedFeign {}
`)
	writeFile(t, root, base+"/middle-lib/pom.xml", `<project>
  <groupId>com.example</groupId>
  <artifactId>middle-lib</artifactId>
  <dependencies>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>client-lib</artifactId>
    </dependency>
  </dependencies>
</project>`)
	writeFile(t, root, base+"/service-a/pom.xml", `<project>
  <groupId>com.example</groupId>
  <artifactId>service-a</artifactId>
  <dependencies>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>middle-lib</artifactId>
    </dependency>
  </dependencies>
</project>`)
	writeFile(t, root, base+"/service-a/src/main/java/com/example/ServiceAApplication.java",
		"package com.example;\n@SpringBootApplication\npublic class ServiceAApplication {}\n")
	writeFile(t, root, base+"/service-b/pom.xml", `<project>
  <groupId>com.example</groupId>
  <artifactId>service-b</artifactId>
  <dependencies>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>client-lib</artifactId>
    </dependency>
  </dependencies>
</project>`)
	writeFile(t, root, base+"/service-b/src/main/java/com/example/ServiceBApplication.java",
		"package com.example;\n@SpringBootApplication\npublic class ServiceBApplication {}\n")

	var requested []config.Ref
	resolver := configResolverFunc(func(
		_ context.Context,
		refs []config.Ref,
	) (map[config.Ref]config.Value, error) {
		requested = append(requested, refs...)
		return map[config.Ref]config.Value{
			{Application: "service-a", Key: "shared.url"}: {
				Value: "https://a.example.com", Source: "config-center/na/application/service-a/shared.url",
			},
			{Application: "service-b", Key: "shared.url"}: {
				Value: "https://b.example.com", Source: "config-center/na/application/service-b/shared.url",
			},
		}, nil
	})

	bundle, err := BuildBundleWithResolver(
		context.Background(), root, mustDiscoverScanDirs(t, root), nil, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(requested, func(a, b config.Ref) int {
		if a.Application != b.Application {
			return strings.Compare(a.Application, b.Application)
		}
		return strings.Compare(a.Key, b.Key)
	})
	wantRequested := []config.Ref{
		{Application: "service-a", Key: "shared.url"},
		{Application: "service-b", Key: "shared.url"},
	}
	if !slices.Equal(requested, wantRequested) {
		t.Fatalf("requested config refs = %#v, want %#v", requested, wantRequested)
	}
	if dependency := findDependencyFrom(bundle.Dependencies, "service-a", "a.example.com"); dependency == nil {
		t.Fatalf("service-a dependency missing: %#v", bundle.Dependencies)
	}
	if dependency := findDependencyFrom(bundle.Dependencies, "service-b", "b.example.com"); dependency == nil {
		t.Fatalf("service-b dependency missing: %#v", bundle.Dependencies)
	}
}

func feignWorkspace(t *testing.T, applicationYAML string) string {
	t.Helper()
	root := t.TempDir()
	source := "repos/hsds/hsds-offline-cookbook"
	writeJavaService(t, root, source, "hsds-offline-cookbook")
	if applicationYAML != "" {
		writeFile(t, root, source+"/src/main/resources/application.yml", applicationYAML)
	}
	writeFile(t, root, source+"/src/main/java/com/hesung/hsds/feign/APIFeignOnline.java", `package com.hesung.hsds.feign;
@FeignClient(value = "${api.url-online-service-name}")
public interface APIFeignOnline {}
`)
	writeFile(t, root, source+"/src/main/java/com/hesung/hsds/feign/APIFeignOffline.java", `package com.hesung.hsds.feign;
@FeignClient(value = "${api.url-offline-service-name}")
public interface APIFeignOffline {}
`)
	writeFile(t, root, source+"/src/main/java/com/hesung/hsds/feign/OpenAiFeign.java", `package com.hesung.hsds.feign;
@FeignClient(name = "openai-client", url = "${openai.url}")
public interface OpenAiFeign {}
`)
	writeJavaService(t, root, "repos/hsds/hsds-online-service", "hsds-online-service")
	writeJavaService(t, root, "repos/hsds/hsds-offline-service", "hsds-offline-service")
	return root
}

func writeJavaService(t *testing.T, root, base, artifactID string) {
	t.Helper()
	writeFile(t, root, base+"/pom.xml", "<project><artifactId>"+artifactID+"</artifactId></project>")
	writeFile(t, root, base+"/src/main/java/com/example/Application.java",
		"package com.example;\n@SpringBootApplication\npublic class Application {}\n")
}

func findDependency(dependencies []domain.DependencyEdge, target string) *domain.DependencyEdge {
	for i := range dependencies {
		if dependencies[i].To == target {
			return &dependencies[i]
		}
	}
	return nil
}

func findDependencyFrom(dependencies []domain.DependencyEdge, from, target string) *domain.DependencyEdge {
	for i := range dependencies {
		if dependencies[i].From == from && dependencies[i].To == target {
			return &dependencies[i]
		}
	}
	return nil
}

func hasEvidenceKind(evidence []domain.Evidence, kind domain.SourceKind) bool {
	return slices.ContainsFunc(evidence, func(item domain.Evidence) bool {
		return item.Kind == kind
	})
}
