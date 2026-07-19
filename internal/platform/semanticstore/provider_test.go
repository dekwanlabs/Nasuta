package semanticstore

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
)

func TestNewRejectsMissingProviderBeforeConnection(t *testing.T) {
	_, err := New(config.SemanticConfig{Endpoint: "localhost:6334", Collection: "knowledge"})
	if err == nil || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("error = %v, want missing provider error", err)
	}
}

func TestNewRejectsMissingEndpointBeforeConnection(t *testing.T) {
	_, err := New(config.SemanticConfig{Provider: "milvus", Collection: "knowledge"})
	if err == nil || !strings.Contains(err.Error(), `provider "milvus" requires`) {
		t.Fatalf("error = %v, want missing endpoint error", err)
	}
}

func TestNewRejectsUnsupportedProviderBeforeConnection(t *testing.T) {
	_, err := New(config.SemanticConfig{Provider: "elasticsearch", Endpoint: "localhost:9200", Collection: "knowledge"})
	if err == nil || !strings.Contains(err.Error(), `provider "elasticsearch" is unsupported`) {
		t.Fatalf("error = %v, want unsupported provider error", err)
	}
}
