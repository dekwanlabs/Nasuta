package docgen

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
)

func TestGenerateDocsWithoutDocStoreIsNoop(t *testing.T) {
	(&Generator{}).GenerateDocs(context.Background(), []string{"/does/not/matter"})
	if (&Generator{}).GenerateDocsChanged(context.Background(), []string{"/does/not/matter"}) {
		t.Fatal("GenerateDocsChanged reported a change without a document store")
	}
}

func TestNewRejectsAnthropicProvider(t *testing.T) {
	generator, err := New(config.Config{}, &config.PlatformSettings{LLMProvider: "anthropic"}, nil)
	if err == nil || generator != nil {
		t.Fatalf("New() = (%v, %v), want nil generator and provider error", generator, err)
	}
}
