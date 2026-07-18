package types

import "testing"

func TestEvidenceForRunbookScope(t *testing.T) {
	class, trust := EvidenceForRunbookScope(DocKindSchema)
	if class != EvidenceClassCuratedSchema || trust != TrustCuratedSchema {
		t.Fatalf("schema evidence = %s/%d", class, trust)
	}
	class, trust = EvidenceForRunbookScope(DocKindFlow)
	if class != EvidenceClassCuratedRunbook || trust != TrustCuratedRunbook {
		t.Fatalf("flow evidence = %s/%d", class, trust)
	}
}

func TestEvidenceForCodeChunkDistinguishesProjectDocs(t *testing.T) {
	class, trust := EvidenceForCodeChunk("markdown", "hsds/hsds-cookbook")
	if class != EvidenceClassRepoDoc || trust != TrustRepoDoc {
		t.Fatalf("project markdown evidence = %s/%d", class, trust)
	}

	class, trust = EvidenceForCodeChunk("markdown", "hsds/hsds-cookbook")
	if class != EvidenceClassRepoDoc || trust != TrustRepoDoc {
		t.Fatalf("project docs markdown evidence = %s/%d", class, trust)
	}

	class, trust = EvidenceForCodeChunk("markdown", "docs")
	if class != EvidenceClassCuratedRunbook || trust != TrustCuratedRunbook {
		t.Fatalf("docs markdown evidence = %s/%d", class, trust)
	}
}

func TestEvidenceForCodeChunkDDL(t *testing.T) {
	class, trust := EvidenceForCodeChunk("sql", "hsds/hsds-cookbook")
	if class != EvidenceClassRawDDL || trust != TrustRawDDL {
		t.Fatalf("sql evidence = %s/%d", class, trust)
	}
}
