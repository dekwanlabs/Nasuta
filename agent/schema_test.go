package agent

import (
	"encoding/json"
	"sync"
	"testing"
)

func testSchema(id string, version int64, document string, compatible ...SchemaRef) SchemaDefinition {
	return SchemaDefinition{
		ID: id, Version: version, Document: json.RawMessage(document), CompatibleFrom: compatible,
	}
}

func TestSchemaRegistryValidatesExactVersions(t *testing.T) {
	registry := NewSchemaRegistry()
	if err := registry.Publish([]SchemaDefinition{
		testSchema("qa.request", 1, `{
			"type":"object",
			"required":["question"],
			"properties":{"question":{"type":"string","minLength":1}},
			"additionalProperties":false
		}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(SchemaRef{ID: "qa.request", Version: 1}, json.RawMessage(`{"question":"where?"}`)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if err := registry.Validate(SchemaRef{ID: "qa.request", Version: 1}, json.RawMessage(`{"question":"","extra":true}`)); err == nil {
		t.Fatal("invalid payload accepted")
	}
	if err := registry.Validate(SchemaRef{ID: "qa.request", Version: 2}, json.RawMessage(`{"question":"where?"}`)); err == nil {
		t.Fatal("unknown schema version accepted")
	}
}

func TestSchemaRegistryPublishesAtomicallyAndRejectsMutation(t *testing.T) {
	registry := NewSchemaRegistry()
	first := testSchema("review.report", 1, `{"type":"object"}`)
	if err := registry.Publish([]SchemaDefinition{first}); err != nil {
		t.Fatal(err)
	}
	published, err := registry.Resolve(SchemaRef{ID: "review.report", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	published.Document[0] = '['
	resolved, err := registry.Resolve(SchemaRef{ID: "review.report", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(resolved.Document) {
		t.Fatal("resolved document mutated registry state")
	}
	if err := registry.Publish([]SchemaDefinition{
		testSchema("review.report", 2, `{"type":"object"}`),
		testSchema("review.report.list", 1, `{"type":"array"}`, SchemaRef{ID: "missing.report", Version: 1}),
	}); err == nil {
		t.Fatal("publication accepted an unresolved compatibility reference")
	}
	if _, err := registry.Resolve(SchemaRef{ID: "review.report", Version: 2}); err == nil {
		t.Fatal("failed batch became partially visible")
	}
	if err := registry.Publish([]SchemaDefinition{
		testSchema("review.report", 1, `{"type":"array"}`),
	}); err == nil {
		t.Fatal("published version mutation was accepted")
	}
}

func TestSchemaRegistryUsesExplicitConsumerCompatibility(t *testing.T) {
	registry := NewSchemaRegistry()
	producerV1 := SchemaRef{ID: "review.report", Version: 1}
	consumerV2 := SchemaRef{ID: "review.report", Version: 2}
	if err := registry.Publish([]SchemaDefinition{
		testSchema(producerV1.ID, producerV1.Version, `{"type":"object"}`),
		testSchema(consumerV2.ID, consumerV2.Version, `{"type":"object"}`, producerV1),
		testSchema("review.other", 1, `{"type":"object"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateCompatibility(producerV1, consumerV2); err != nil {
		t.Fatalf("declared compatibility rejected: %v", err)
	}
	if err := registry.ValidateCompatibility(consumerV2, producerV1); err == nil {
		t.Fatal("compatibility was treated as symmetric")
	}
	if err := registry.ValidateCompatibility(
		SchemaRef{ID: "review.other", Version: 1}, consumerV2,
	); err == nil {
		t.Fatal("undeclared compatibility accepted")
	}
}

func TestSchemaRegistryReadersObserveWholeSnapshots(t *testing.T) {
	registry := NewSchemaRegistry()
	if err := registry.Publish([]SchemaDefinition{
		testSchema("contract.input", 1, `{"type":"string"}`),
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if err := registry.Validate(
					SchemaRef{ID: "contract.input", Version: 1}, json.RawMessage(`"value"`),
				); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	if err := registry.Publish([]SchemaDefinition{
		testSchema("contract.input", 2, `{"type":"string","minLength":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
