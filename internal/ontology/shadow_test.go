package ontology

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestProjectionShadowsExistingDependencyAndEndpointRecords(t *testing.T) {
	bundle := projectionFixture()
	snapshot, err := Project(bundle)
	if err != nil {
		t.Fatal(err)
	}

	services := make(map[string]domain.ServiceRecord, len(bundle.Services))
	for _, service := range bundle.Services {
		services[service.ServiceKey] = service
	}
	endpointFacts := make(map[string]struct{})
	dependencyFacts := make(map[string]struct{})
	for _, fact := range snapshot.Facts {
		switch fact.Predicate {
		case PredicateExposes:
			endpointFacts[fact.SubjectID+"\x00"+fact.ObjectID] = struct{}{}
		case PredicateDependsOn:
			dependencyFacts[fact.SubjectID+"\x00"+fact.ObjectID+"\x00"+fact.Qualifiers["protocol"]] = struct{}{}
		}
	}

	for _, endpoint := range bundle.Endpoints {
		key := endpoint.ServiceKey + "\x00" + APIEndpointID(endpoint.ServiceKey, endpoint.Method, endpoint.Path)
		if _, ok := endpointFacts[key]; !ok {
			t.Errorf("missing ontology fact for endpoint %s %s", endpoint.Method, endpoint.Path)
		}
	}
	if len(endpointFacts) != len(bundle.Endpoints) {
		t.Fatalf("endpoint facts=%d records=%d", len(endpointFacts), len(bundle.Endpoints))
	}

	for _, dependency := range bundle.Dependencies {
		objectID := dependency.TargetServiceKey
		if dependency.TargetKind == domain.DependencyTargetExternal {
			objectID = ExternalSystemID(dependency.ExternalTarget)
		}
		key := dependency.CallerServiceKey + "\x00" + objectID + "\x00" + string(dependency.Type)
		if _, ok := dependencyFacts[key]; !ok {
			t.Errorf("missing ontology fact for dependency %s -> %s", dependency.From, dependency.To)
		}
		if dependency.TargetKind == domain.DependencyTargetService {
			if _, ok := services[dependency.TargetServiceKey]; !ok {
				t.Errorf("fixture dependency target is not a service: %s", dependency.TargetServiceKey)
			}
		}
	}
	if len(dependencyFacts) != len(bundle.Dependencies)-1 {
		// The fixture deliberately repeats one logical edge to verify evidence merging.
		t.Fatalf("dependency facts=%d logical records=%d", len(dependencyFacts), len(bundle.Dependencies)-1)
	}
}
