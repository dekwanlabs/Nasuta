package retrieval

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func has(tokens []string, want string) bool { return slices.Contains(tokens, want) }

func TestTokenize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string // tokens that MUST be present
		absent  []string // tokens that must NOT be present
		exactly []string // if non-nil, the full expected set (order-insensitive)
	}{
		{
			name:   "pure chinese words via jieba",
			in:     "电池管理超时",
			want:   []string{"电池", "管理", "超时"},
			absent: []string{"池管", "理超"},
		},
		{
			name: "chinese mixed with camelCase identifier",
			in:   "订单orderService超时",
			want: []string{"订单", "order", "service", "超时"},
		},
		{
			name:   "english camelCase is split",
			in:     "OrderServiceImpl",
			want:   []string{"order", "service", "impl"},
			absent: []string{"orderserviceimpl"},
		},
		{
			name:    "snake case is split",
			in:      "create_payment_order",
			exactly: []string{"create", "payment", "order"},
		},
		{
			name:    "numeric and encoded noise is filtered",
			in:      "payment 60000 038088ef7778444b9867b366c44beaeb 550e8400-e29b-41d4-a716-446655440000",
			exactly: []string{"payment"},
		},
		{
			name:    "technical short words use allowlist",
			in:      "db mq id io go xy",
			exactly: []string{"db", "mq", "id", "io", "go"},
		},
		{
			name:    "single chinese char degrades to itself",
			in:      "锁",
			exactly: []string{"锁"},
		},
		{
			name:    "stop tokens filtered",
			in:      "public void",
			exactly: []string{},
		},
		{
			name:   "bigram does not cross non-cjk boundary",
			in:     "中a文",
			absent: []string{"中文"}, // 中 and 文 are separated by "a"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.in)
			for _, w := range tt.want {
				if !has(got, w) {
					t.Errorf("tokenize(%q) missing %q; got %v", tt.in, w, got)
				}
			}
			for _, a := range tt.absent {
				if has(got, a) {
					t.Errorf("tokenize(%q) should not contain %q; got %v", tt.in, a, got)
				}
			}
			if tt.exactly != nil {
				if len(got) != len(tt.exactly) {
					t.Fatalf("tokenize(%q) = %v; want exactly %v", tt.in, got, tt.exactly)
				}
				for _, w := range tt.exactly {
					if !has(got, w) {
						t.Errorf("tokenize(%q) = %v; want exactly %v", tt.in, got, tt.exactly)
					}
				}
			}
		})
	}
}

func TestDocumentTokensPreserveTermFrequency(t *testing.T) {
	b := NewBM25Builder()
	tokens := b.AddDoc("payment payment order")
	sv := b.BuildSparse(tokens)
	paymentID := b.vocab["payment"]
	orderID := b.vocab["order"]
	if sv[paymentID] <= sv[orderID] {
		t.Fatalf("repeated payment weight=%v, order weight=%v; repetition should increase TF weight", sv[paymentID], sv[orderID])
	}
}

func TestVocabV2AppendOnlyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25_vocab.json")
	b := NewBM25Builder()
	b.AddDoc("order payment")
	orderID := b.vocab["order"]
	if err := b.SaveVocab(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadVocab(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.AddDoc("settlement payment")
	if loaded.vocab["order"] != orderID {
		t.Fatalf("order ID changed from %d to %d", orderID, loaded.vocab["order"])
	}
	if loaded.vocab["settlement"] <= loaded.vocab["payment"] {
		t.Fatalf("new token ID %d was not appended after existing IDs", loaded.vocab["settlement"])
	}
	if err := loaded.SaveVocab(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadVocab(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.vocab["order"] != orderID || reloaded.vocab["settlement"] != loaded.vocab["settlement"] {
		t.Fatalf("vocabulary IDs changed after round trip: %#v", reloaded.vocab)
	}
}

func TestLoadVocabRejectsLegacyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bm25_vocab.json")
	if err := os.WriteFile(path, []byte(`{"order":0,"payment":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadVocab(path)
	if !errors.Is(err, ErrLegacyVocabulary) {
		t.Fatalf("LoadVocab legacy error = %v; want %v", err, ErrLegacyVocabulary)
	}
}

// TestChineseQueryAlignsWithIndex verifies that a Chinese query produces a
// non-empty sparse vector that overlaps the indexed document — i.e. AddDoc and
// QuerySparse share the same tokenization pipeline, so Chinese is now matchable.
func TestChineseQueryAlignsWithIndex(t *testing.T) {
	b := NewBM25Builder()
	b.AddDoc("电池管理服务在高并发下超时")
	b.AddDoc("用户登录鉴权流程")

	sv := b.QuerySparse("电池管理超时")
	if len(sv) == 0 {
		t.Fatalf("Chinese query produced empty sparse vector; bigrams not aligned with index")
	}

	// Before the fix the whole Chinese string was one token, so a partial query
	// like this would miss entirely. Now bigrams overlap.
	idx, vals := SparseToSorted(sv)
	if len(idx) != len(vals) || len(idx) == 0 {
		t.Fatalf("SparseToSorted returned %d indices / %d values", len(idx), len(vals))
	}
}

func TestSplitCJK(t *testing.T) {
	segs := splitCJK("电池timeout超时")
	want := []cjkSegment{
		{"电池", true},
		{"timeout", false},
		{"超时", true},
	}
	if !slices.Equal(segs, want) {
		t.Errorf("splitCJK = %v; want %v", segs, want)
	}
}
