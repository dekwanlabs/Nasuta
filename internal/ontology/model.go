package ontology

type Class string

const (
	ClassRepository     Class = "repository"
	ClassService        Class = "service"
	ClassAPIEndpoint    Class = "api_endpoint"
	ClassCodeSymbol     Class = "code_symbol"
	ClassExternalSystem Class = "external_system"
	ClassRunbook        Class = "runbook"
)

type Predicate string

const (
	PredicateContains      Predicate = "contains"
	PredicateExposes       Predicate = "exposes"
	PredicateImplementedBy Predicate = "implemented_by"
	PredicateDependsOn     Predicate = "depends_on"
	PredicateDocumentedBy  Predicate = "documented_by"
)

type EvidenceSource string

const (
	EvidenceSourceDoc      EvidenceSource = "doc"
	EvidenceSourceCodeScan EvidenceSource = "code-scan"
)

type Entity struct {
	ID         string            `json:"id"`
	Class      Class             `json:"class"`
	Key        string            `json:"key"`
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
	Aliases    []string          `json:"aliases"`
	Confidence float64           `json:"confidence"`
}

type Evidence struct {
	Path   string         `json:"path"`
	Line   int            `json:"line,omitempty"`
	Symbol string         `json:"symbol,omitempty"`
	Source EvidenceSource `json:"source"`
}

type Fact struct {
	ID         string            `json:"id"`
	SubjectID  string            `json:"subject_id"`
	Predicate  Predicate         `json:"predicate"`
	ObjectID   string            `json:"object_id"`
	Qualifiers map[string]string `json:"qualifiers"`
	Confidence float64           `json:"confidence"`
	Evidence   []Evidence        `json:"evidence"`
}

type Snapshot struct {
	SchemaVersion int      `json:"schema_version"`
	Entities      []Entity `json:"entities"`
	Facts         []Fact   `json:"facts"`
}

const CurrentSchemaVersion = 1
