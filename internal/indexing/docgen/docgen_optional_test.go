package docgen

import (
	"context"
	"testing"
)

func TestGenerateDocsWithoutDocStoreIsNoop(t *testing.T) {
	(&Generator{}).GenerateDocs(context.Background(), []string{"/does/not/matter"})
	if (&Generator{}).GenerateDocsChanged(context.Background(), []string{"/does/not/matter"}) {
		t.Fatal("GenerateDocsChanged reported a change without a document store")
	}
}
