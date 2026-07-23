package ontology

import "testing"

func TestValidateSnapshotRejectsInvalidRelationDomain(t *testing.T) {
	repository := Entity{ID: RepositoryID("team/repo"), Class: ClassRepository, Key: "team/repo", Name: "team/repo", Confidence: 1}
	endpointKey := "service-key\x00GET\x00/orders"
	endpoint := Entity{ID: APIEndpointID("service-key", "GET", "/orders"), Class: ClassAPIEndpoint, Key: endpointKey, Name: "GET /orders", Confidence: 1}
	fact := Fact{SubjectID: repository.ID, Predicate: PredicateExposes, ObjectID: endpoint.ID, Confidence: 1}
	fact.ID = FactID(fact.SubjectID, fact.Predicate, fact.ObjectID, nil)

	err := ValidateSnapshot(Snapshot{SchemaVersion: CurrentSchemaVersion, Entities: []Entity{repository, endpoint}, Facts: []Fact{fact}})
	if err == nil {
		t.Fatal("repository exposes endpoint unexpectedly validated")
	}
}

func TestValidateSnapshotAcceptsStableServiceDependency(t *testing.T) {
	service := Entity{ID: "service-key", Class: ClassService, Key: "service-key", Name: "orders", Aliases: []string{"orders"}, Confidence: 0.9}
	target := Entity{ID: ExternalSystemID("mysql"), Class: ClassExternalSystem, Key: "mysql", Name: "mysql", Confidence: 0.8}
	fact := Fact{
		SubjectID: service.ID, Predicate: PredicateDependsOn, ObjectID: target.ID,
		Qualifiers: map[string]string{"protocol": "http"}, Confidence: 0.8,
		Evidence: []Evidence{{Path: "repos/team/orders/config.yml", Line: 3, Source: EvidenceSourceCodeScan}},
	}
	fact.ID = FactID(fact.SubjectID, fact.Predicate, fact.ObjectID, fact.Qualifiers)

	if err := ValidateSnapshot(Snapshot{SchemaVersion: CurrentSchemaVersion, Entities: []Entity{service, target}, Facts: []Fact{fact}}); err != nil {
		t.Fatal(err)
	}
}

func TestFactIDIgnoresQualifierMapOrder(t *testing.T) {
	left := FactID("a", PredicateDependsOn, "b", map[string]string{"protocol": "http", "scope": "x"})
	right := FactID("a", PredicateDependsOn, "b", map[string]string{"scope": "x", "protocol": "http"})
	if left != right {
		t.Fatalf("fact IDs differ: %q != %q", left, right)
	}
}
