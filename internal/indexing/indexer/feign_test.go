package indexer

import (
	"context"
	"errors"
	"slices"
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

func TestFeignDependenciesSurfaceConfiguredResolverFailure(t *testing.T) {
	root := feignWorkspace(t, "")
	resolver := configResolverFunc(func(
		context.Context,
		[]config.Ref,
	) (map[config.Ref]config.Value, error) {
		return nil, errors.New("config center unavailable")
	})
	_, err := BuildBundleWithResolver(
		context.Background(), root, mustDiscoverScanDirs(t, root), nil, resolver,
	)
	if err == nil {
		t.Fatal("configured resolver failure was hidden")
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

func hasEvidenceKind(evidence []domain.Evidence, kind domain.SourceKind) bool {
	return slices.ContainsFunc(evidence, func(item domain.Evidence) bool {
		return item.Kind == kind
	})
}
