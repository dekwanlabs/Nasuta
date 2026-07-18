package types

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
	Preview       string  `json:"preview"`
	Score         float64 `json:"score"`
	ScoreKind     string  `json:"scoreKind"`
	FusionScore   float64 `json:"fusionScore,omitempty"`
	SemanticScore float64 `json:"semanticScore,omitempty"`
	EvidenceClass string  `json:"evidenceClass"`
	TrustTier     int     `json:"trustTier"`
}

// RunbookSearchHit combines runbook metadata with the matched chunk evidence.
type RunbookSearchHit struct {
	Record        RunbookRecord `json:"record"`
	ChunkText     string        `json:"chunkText,omitempty"`
	SectionHeader string        `json:"sectionHeader,omitempty"`
	Score         float64       `json:"score"`
	SemanticScore float64       `json:"semanticScore,omitempty"`
	EvidenceClass string        `json:"evidenceClass"`
	TrustTier     int           `json:"trustTier"`
}
