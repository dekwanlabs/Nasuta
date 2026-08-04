package indexing

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDrainStreamConsumesLongLine(t *testing.T) {
	input := bytes.Repeat([]byte("x"), 256*1024)
	var got bytes.Buffer
	chunks := 0

	err := drainStream(bytes.NewReader(input), func(chunk []byte) {
		chunks++
		_, _ = got.Write(chunk)
	})
	if err != nil {
		t.Fatalf("drainStream: %v", err)
	}
	if !bytes.Equal(got.Bytes(), input) {
		t.Fatalf("drained %d bytes, want %d", got.Len(), len(input))
	}
	if chunks < 2 {
		t.Fatalf("got %d chunk, want long line split into bounded chunks", chunks)
	}
}

func TestCodegraphIndexArgsForceFullRebuild(t *testing.T) {
	want := []string{"index", "--force", "--quiet", "/workspace"}
	if got := codegraphIndexArgs("/workspace"); !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestEnsureCodegraphConfigPreservesExistingSettings(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "codegraph.json")
	if err := os.WriteFile(path, []byte(`{"extensions":{".foo":"go"},"exclude":["custom/"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureCodegraphConfig(workspace); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCodegraphConfig(workspace); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second update changed an already canonical config")
	}

	var config struct {
		Extensions map[string]string `json:"extensions"`
		Exclude    []string          `json:"exclude"`
	}
	if err := json.Unmarshal(first, &config); err != nil {
		t.Fatal(err)
	}
	if config.Extensions[".foo"] != "go" {
		t.Fatalf("extensions = %v", config.Extensions)
	}
	want := append([]string{"custom/"}, codegraphRuntimeExcludes...)
	if !slices.Equal(config.Exclude, want) {
		t.Fatalf("exclude = %v, want %v", config.Exclude, want)
	}
}

func TestCodegraphRuntimeExcludesAgentMetadataDirectories(t *testing.T) {
	for _, pattern := range []string{".claude/", ".codex/"} {
		if !slices.Contains(codegraphRuntimeExcludes, pattern) {
			t.Fatalf("codegraph runtime excludes missing %q", pattern)
		}
	}
}

func TestEnsureCodegraphConfigRejectsInvalidExistingConfig(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "codegraph.json")
	if err := os.WriteFile(path, []byte(`{"exclude":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureCodegraphConfig(workspace); err == nil {
		t.Fatal("ensureCodegraphConfig accepted malformed JSON")
	}
}
