package agentworkflow

import (
	"sync"
	"testing"
)

func TestCatalogRetainsPublishedVersionsDuringConcurrentReads(t *testing.T) {
	catalog := NewCatalog()
	version1 := testWorkflow()
	if err := catalog.Publish([]WorkflowDefinition{version1}); err != nil {
		t.Fatal(err)
	}
	version2 := testWorkflow()
	version2.Version = 2
	version2.Purpose = "Run a revised independent review panel."

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				definition, err := catalog.Resolve(DefinitionRef{ID: version1.ID, Version: 1})
				if err != nil || definition.Version != 1 {
					t.Errorf("resolve version 1: definition=%#v err=%v", definition, err)
					return
				}
			}
		}()
	}
	if err := catalog.Publish([]WorkflowDefinition{version2}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	latest, err := catalog.Resolve(DefinitionRef{ID: version1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 || catalog.Revision() != 2 {
		t.Fatalf("latest=%d revision=%d", latest.Version, catalog.Revision())
	}
}
