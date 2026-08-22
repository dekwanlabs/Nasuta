package domain

// SearchResult is the typed result shared by internal search consumers.
type SearchResult[T any] struct {
	Matches  []T
	Semantic bool
}

// CodeSearchHit is one code chunk selected by hybrid or dense retrieval.
type CodeSearchHit struct {
	Path          string  `json:"path"`
	Lang          string  `json:"lang"`
	Repo          string  `json:"repo"`
	Layer         string  `json:"layer"`
	StartLine     int     `json:"startLine"`
	EndLine       int     `json:"endLine"`
	Text          string  `json:"text"`
	Score         float64 `json:"score"`
	ScoreKind     string  `json:"scoreKind"`
	FusionScore   float64 `json:"fusionScore,omitempty"`
	SemanticScore float64 `json:"semanticScore,omitempty"`
	HasDenseScore bool    `json:"-"`
	EvidenceClass string  `json:"evidenceClass"`
	TrustTier     int     `json:"trustTier"`
}

// RunbookChunk is one independently traceable document fragment.
type RunbookChunk struct {
	ChunkIndex    int     `json:"chunkIndex"`
	SectionHeader string  `json:"sectionHeader,omitempty"`
	ChunkText     string  `json:"chunkText"`
	SemanticScore float64 `json:"semanticScore"`
}

// RunbookSearchHit groups matched chunks under their source document.
type RunbookSearchHit struct {
	DocID         string         `json:"docId"`
	Title         string         `json:"title"`
	Path          string         `json:"path"`
	DocKind       string         `json:"docKind"`
	EvidenceClass string         `json:"evidenceClass"`
	TrustTier     int            `json:"trustTier"`
	Chunks        []RunbookChunk `json:"chunks"`
}

// RunbookSearchResult records retrieval mode and bounded omissions.
type RunbookSearchResult struct {
	Matches   []RunbookSearchHit `json:"matches"`
	Semantic  bool               `json:"semantic"`
	DocScoped bool               `json:"docScoped"`
	Truncated bool               `json:"truncated"`
}
