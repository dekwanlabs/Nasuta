package indexing

import (
	"bytes"
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
