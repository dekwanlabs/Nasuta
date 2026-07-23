package ontology

type ClassDef struct {
	Properties map[string]struct{}
}

type RelationDef struct {
	SubjectClasses map[Class]struct{}
	ObjectClasses  map[Class]struct{}
	Qualifiers     map[string]struct{}
}

var classSchema = map[Class]ClassDef{
	ClassRepository:     {Properties: stringSet("repo", "head_sha")},
	ClassService:        {Properties: stringSet("repo", "module_path", "language", "owner", "runtime")},
	ClassAPIEndpoint:    {Properties: stringSet("method", "path", "file", "handler")},
	ClassCodeSymbol:     {Properties: stringSet("repo", "file", "qualified_name", "language")},
	ClassExternalSystem: {Properties: stringSet("target", "protocol_hint")},
	ClassRunbook:        {Properties: stringSet("repo", "title", "path", "scope", "tags")},
}

var relationSchema = map[Predicate]RelationDef{
	PredicateContains: {
		SubjectClasses: classSet(ClassRepository),
		ObjectClasses:  classSet(ClassService),
	},
	PredicateExposes: {
		SubjectClasses: classSet(ClassService),
		ObjectClasses:  classSet(ClassAPIEndpoint),
	},
	PredicateImplementedBy: {
		SubjectClasses: classSet(ClassAPIEndpoint),
		ObjectClasses:  classSet(ClassCodeSymbol),
	},
	PredicateDependsOn: {
		SubjectClasses: classSet(ClassService),
		ObjectClasses:  classSet(ClassService, ClassExternalSystem),
		Qualifiers:     stringSet("protocol"),
	},
	PredicateDocumentedBy: {
		SubjectClasses: classSet(ClassService),
		ObjectClasses:  classSet(ClassRunbook),
		Qualifiers:     stringSet("scope"),
	},
}

func classSet(values ...Class) map[Class]struct{} {
	set := make(map[Class]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func stringSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
