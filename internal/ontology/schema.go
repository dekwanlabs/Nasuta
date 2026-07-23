package ontology

type classDef struct {
	Properties map[string]struct{}
	ID         func(string) string
}

type relationDef struct {
	SubjectClasses map[Class]struct{}
	ObjectClasses  map[Class]struct{}
	Qualifiers     map[string]struct{}
}

var classSchema = map[Class]classDef{
	ClassRepository:     {Properties: stringSet("repo", "head_sha"), ID: RepositoryID},
	ClassService:        {Properties: stringSet("repo", "module_path", "language", "owner", "runtime"), ID: ServiceID},
	ClassAPIEndpoint:    {Properties: stringSet("method", "path", "file", "handler"), ID: compoundID3(APIEndpointID)},
	ClassCodeSymbol:     {Properties: stringSet("repo", "file", "qualified_name", "language"), ID: compoundID3(CodeSymbolID)},
	ClassExternalSystem: {Properties: stringSet("target"), ID: ExternalSystemID},
	ClassRunbook:        {Properties: stringSet("repo", "title", "path", "scope", "tags"), ID: compoundID2(RunbookID)},
}

var relationSchema = map[Predicate]relationDef{
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
