package retrieval

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-ego/gse"
)

var (
	segOnce   sync.Once
	segmenter gse.Segmenter
	uuidRe    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`)
	hexRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`)
)

func tokenize(text string) []string {
	return uniqTokens(tokenizeDocument(text))
}

// TokenizeQuery exposes the same canonical terms used by sparse retrieval.
func TokenizeQuery(text string) []string {
	return tokenize(text)
}

// WarmTokenizer loads the process-wide tokenizer before the first user query.
func WarmTokenizer() {
	_ = TokenizeQuery("预热 tokenizer")
}

// tokenizeDocument preserves duplicates so document sparse vectors retain
// term-frequency information. Query tokenization deduplicates the same output.
func tokenizeDocument(text string) []string {
	if utf8.RuneCountInString(text) > 12000 {
		text = string([]rune(text)[:12000])
	}
	text = uuidRe.ReplaceAllString(text, " ")
	text = hexRe.ReplaceAllString(text, " ")
	segOnce.Do(func() {
		seg, _ := gse.New("zh", "jp")
		segmenter = seg
	})
	var tokens []string
	for _, seg := range splitCJK(text) {
		if seg.isCJK {
			for _, word := range segmenter.Cut(seg.text, true) {
				word = strings.TrimSpace(word)
				if acceptToken(word, true) {
					tokens = append(tokens, word)
				}
			}
			continue
		}
		for _, raw := range strings.FieldsFunc(seg.text, isWordBoundary) {
			tokens = append(tokens, splitCamel(raw)...)
		}
	}
	return tokens
}

func isWordBoundary(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func isCJK(r rune) bool { return unicode.Is(unicode.Han, r) }

type cjkSegment struct {
	text  string
	isCJK bool
}

func splitCJK(text string) []cjkSegment {
	var segs []cjkSegment
	var b strings.Builder
	cur := false
	flush := func() {
		if b.Len() > 0 {
			segs = append(segs, cjkSegment{text: b.String(), isCJK: cur})
			b.Reset()
		}
	}
	for i, r := range text {
		cjk := isCJK(r)
		if i == 0 {
			cur = cjk
		} else if cjk != cur {
			flush()
			cur = cjk
		}
		b.WriteRune(r)
	}
	flush()
	return segs
}

func splitCamel(s string) []string {
	var parts []string
	start := 0
	for i := 1; i < len(s); i++ {
		if unicode.IsUpper(rune(s[i])) && (unicode.IsLower(rune(s[i-1])) || (i+1 < len(s) && unicode.IsLower(rune(s[i+1])))) {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	out := parts[:0]
	for _, part := range parts {
		part = strings.ToLower(part)
		if acceptToken(part, false) {
			out = append(out, part)
		}
	}
	return out
}

func acceptToken(token string, cjk bool) bool {
	token = strings.TrimSpace(token)
	if token == "" || stopTokens[strings.ToLower(token)] {
		return false
	}
	runeLen := utf8.RuneCountInString(token)
	if runeLen > 48 || (!cjk && runeLen < 2) {
		return false
	}
	letters, digits := 0, 0
	allHex := true
	for _, r := range token {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		}
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			allHex = false
		}
	}
	if letters == 0 || (!cjk && runeLen == 2 && !shortTokenAllowlist[token]) {
		return false
	}
	if allHex && runeLen >= 12 {
		return false
	}
	if digits > 0 && float64(digits)/float64(runeLen) > 0.5 {
		return false
	}
	return true
}

func uniqTokens(tokens []string) []string {
	seen := map[string]bool{}
	out := tokens[:0]
	for _, token := range tokens {
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

var stopTokens = map[string]bool{
	"the": true, "and": true, "for": true, "this": true, "that": true,
	"with": true, "from": true, "boolean": true, "string": true, "int": true,
	"long": true, "double": true, "float": true, "void": true, "null": true,
	"true": true, "false": true, "public": true, "private": true, "protected": true,
	"static": true, "final": true, "class": true, "interface": true, "enum": true,
	"return": true, "new": true, "if": true, "else": true, "try": true, "catch": true,
	"throw": true, "throws": true, "import": true, "package": true, "extends": true,
	"implements": true, "override": true, "super": true, "default": true,
	"get": true, "set": true, "is": true, "of": true, "in": true, "to": true,
	"by": true, "or": true, "not": true, "as": true, "an": true, "be": true,
	"it": true, "on": true, "at": true, "com": true, "org": true,
	"xmlns": true, "xsi": true, "xsd": true, "w3": true, "schema": true,
	"nbsp": true, "quot": true, "amp": true, "lt": true, "gt": true,
	"author": true, "copyright": true, "param": true, "args": true,
	"的": true, "了": true, "和": true, "与": true, "或": true, "及": true,
	"是": true, "在": true, "为": true, "将": true, "中": true, "上": true,
	"下": true, "可": true, "不": true, "对": true, "进行": true, "使用": true,
}

var shortTokenAllowlist = map[string]bool{
	"ai": true, "api": true, "db": true, "go": true, "id": true, "io": true,
	"ip": true, "mq": true, "os": true, "ui": true, "ok": true,
}

type SparseVector map[uint32]float32

type BM25Builder struct {
	mu       sync.RWMutex
	vocab    map[string]uint32
	nextID   uint32
	docCount int
	k1       float64
}

const (
	VocabVersion     = 2
	TokenizerVersion = 1
)

var ErrLegacyVocabulary = errors.New("legacy BM25 vocabulary requires a full code embedding migration")

func NewBM25Builder() *BM25Builder {
	return &BM25Builder{
		vocab: map[string]uint32{},
		k1:    1.2,
	}
}

func (b *BM25Builder) AddDoc(text string) []string {
	tokens := tokenizeDocument(text)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addTokensLocked(tokens)
	b.docCount++
	return tokens
}

// ObserveDoc adds tokens without changing the document count. Incremental code
// indexing uses this because the existing corpus already owns unchanged docs;
// sparse coordinates are append-only while full corpus statistics are rebuilt
// by EmbedCodeChunks.
func (b *BM25Builder) ObserveDoc(text string) []string {
	tokens := tokenizeDocument(text)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addTokensLocked(tokens)
	return tokens
}

func (b *BM25Builder) addTokensLocked(tokens []string) {
	for _, token := range tokens {
		if _, ok := b.vocab[token]; ok {
			continue
		}
		id := b.nextID
		b.vocab[token] = id
		b.nextID++
	}
}

func (b *BM25Builder) TotalDocs() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.docCount
}

func (b *BM25Builder) BuildSparse(tokens []string) SparseVector {
	b.mu.RLock()
	defer b.mu.RUnlock()
	tf := map[uint32]int{}
	for _, token := range tokens {
		if id, ok := b.vocab[token]; ok {
			tf[id]++
		}
	}
	sv := make(SparseVector, len(tf))
	for id, f := range tf {
		// Qdrant applies collection-level IDF. Keeping b=0 makes the stored
		// document value independent of corpus-wide average document length.
		w := (float64(f) * (b.k1 + 1.0)) / (float64(f) + b.k1)
		if w > 0.01 {
			sv[id] = float32(w)
		}
	}
	if len(sv) == 0 {
		return nil
	}
	return sv
}

func (b *BM25Builder) QuerySparse(query string) SparseVector {
	tokens := TokenizeQuery(query)
	if len(tokens) == 0 {
		return nil
	}
	sv := make(SparseVector, len(tokens))
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, token := range tokens {
		if id, ok := b.vocab[token]; ok {
			sv[id] = 1.0
		}
	}
	if len(sv) == 0 {
		return nil
	}
	return sv
}

func (b *BM25Builder) VocabularySize() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.vocab)
}

func (b *BM25Builder) Clone() *BM25Builder {
	b.mu.RLock()
	defer b.mu.RUnlock()
	vocab := make(map[string]uint32, len(b.vocab))
	for token, id := range b.vocab {
		vocab[token] = id
	}
	return &BM25Builder{vocab: vocab, nextID: b.nextID, docCount: b.docCount, k1: b.k1}
}

func SparseToSorted(sv SparseVector) ([]uint32, []float32) {
	indices := make([]uint32, 0, len(sv))
	for k := range sv {
		indices = append(indices, k)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	values := make([]float32, len(indices))
	for i, idx := range indices {
		values[i] = sv[idx]
	}
	return indices, values
}

func (b *BM25Builder) SaveVocab(path string) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	vocab := make(map[string]uint32, len(b.vocab))
	for k, v := range b.vocab {
		vocab[k] = v
	}
	data, err := json.MarshalIndent(vocabFile{
		Version:          VocabVersion,
		TokenizerVersion: TokenizerVersion,
		NextID:           b.nextID,
		Tokens:           vocab,
	}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bm25_vocab-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func LoadVocab(path string) (*BM25Builder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return nil, err
	}
	if _, ok := raw["tokens"]; !ok {
		return nil, ErrLegacyVocabulary
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var vf vocabFile
	if err := json.Unmarshal(data, &vf); err != nil {
		return nil, err
	}
	if vf.Version != VocabVersion || vf.TokenizerVersion != TokenizerVersion {
		return nil, ErrLegacyVocabulary
	}
	nextID := vf.NextID
	for _, id := range vf.Tokens {
		if id >= nextID {
			nextID = id + 1
		}
	}
	return &BM25Builder{
		vocab:  vf.Tokens,
		nextID: nextID,
		k1:     1.2,
	}, nil
}

type vocabFile struct {
	Version          int               `json:"version"`
	TokenizerVersion int               `json:"tokenizer_version"`
	NextID           uint32            `json:"next_id"`
	Tokens           map[string]uint32 `json:"tokens"`
}
