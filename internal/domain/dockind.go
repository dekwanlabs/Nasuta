package types

type DocKind = string

const (
	DocKindDocument DocKind = "document"
	DocKindFlow     DocKind = "flow"
	DocKindSchema   DocKind = "schema"
	DocKindModule   DocKind = "module"
)

var RunbookKinds = []DocKind{DocKindFlow, DocKindSchema, DocKindModule, DocKindDocument}

var UploadableDocKinds = []DocKind{DocKindDocument, DocKindFlow, DocKindSchema}

var KnowledgeDocKinds = []DocKind{DocKindFlow, DocKindSchema}

var GeneratedDocKinds = []DocKind{DocKindDocument, DocKindModule}

func kindSet(kinds []DocKind) map[DocKind]struct{} {
	m := make(map[DocKind]struct{}, len(kinds))
	for _, k := range kinds {
		m[k] = struct{}{}
	}
	return m
}

var UploadableDocKindSet = kindSet(UploadableDocKinds)

var KnowledgeDocKindSet = kindSet(KnowledgeDocKinds)
