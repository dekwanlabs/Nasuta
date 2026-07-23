package ontology

import (
	"reflect"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestWorkspaceGenerationTracksContentAndIgnoresRefreshTimestamp(t *testing.T) {
	firstBundle := generationFixture()
	firstBundle.Repositories[0].IndexedAt = 100
	firstSnapshot, err := Project(firstBundle)
	if err != nil {
		t.Fatal(err)
	}
	first, err := (WorkspaceSnapshot{Structure: firstBundle, Ontology: firstSnapshot}).Generation()
	if err != nil {
		t.Fatal(err)
	}

	sameBundle := generationFixture()
	sameBundle.Repositories[0].IndexedAt = 200
	sameBundle.Repositories[0], sameBundle.Repositories[1] = sameBundle.Repositories[1], sameBundle.Repositories[0]
	sameBundle.Services[0], sameBundle.Services[1] = sameBundle.Services[1], sameBundle.Services[0]
	sameBundle.Dependencies[0], sameBundle.Dependencies[1] = sameBundle.Dependencies[1], sameBundle.Dependencies[0]
	sameSnapshot, err := Project(sameBundle)
	if err != nil {
		t.Fatal(err)
	}
	for i := range sameSnapshot.Entities {
		properties := make(map[string]string, len(sameSnapshot.Entities[i].Properties))
		keys := reflect.ValueOf(sameSnapshot.Entities[i].Properties).MapKeys()
		for j := len(keys) - 1; j >= 0; j-- {
			key := keys[j].String()
			properties[key] = sameSnapshot.Entities[i].Properties[key]
		}
		sameSnapshot.Entities[i].Properties = properties
	}
	same, err := (WorkspaceSnapshot{Structure: sameBundle, Ontology: sameSnapshot}).Generation()
	if err != nil {
		t.Fatal(err)
	}
	if same != first {
		t.Fatalf("generation changed for equivalent workspace: %q != %q", same, first)
	}

	changedBundle := generationFixture()
	changedBundle.Runbooks[0].Text = "updated recovery procedure"
	changedSnapshot, err := Project(changedBundle)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := (WorkspaceSnapshot{Structure: changedBundle, Ontology: changedSnapshot}).Generation()
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("generation did not change when runbook content changed")
	}
}

func generationFixture() domain.IndexBundle {
	bundle := projectionFixture()
	bundle.Dependencies[1].CallerServiceKey = "payments-key"
	bundle.Dependencies[1].TargetServiceKey = "orders-key"
	return bundle
}
