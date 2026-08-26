package agent

const (
	InvestigationReportSchemaID                    = "investigation.report"
	InvestigationReportSchemaVersion         int64 = 1
	InvestigationBundleSchemaID                    = "investigation.bundle"
	InvestigationBundleSchemaVersion         int64 = 1
	InvestigationVerifiedBundleSchemaID            = "investigation.verified_bundle"
	InvestigationVerifiedBundleSchemaVersion int64 = 1
	InvestigationAnswerSchemaID                    = "investigation.answer"
	InvestigationAnswerSchemaVersion         int64 = 1
)

func InvestigationReportSchemaRef() SchemaRef {
	return SchemaRef{ID: InvestigationReportSchemaID, Version: InvestigationReportSchemaVersion}
}

func InvestigationBundleSchemaRef() SchemaRef {
	return SchemaRef{ID: InvestigationBundleSchemaID, Version: InvestigationBundleSchemaVersion}
}

func InvestigationVerifiedBundleSchemaRef() SchemaRef {
	return SchemaRef{ID: InvestigationVerifiedBundleSchemaID, Version: InvestigationVerifiedBundleSchemaVersion}
}

func InvestigationAnswerSchemaRef() SchemaRef {
	return SchemaRef{ID: InvestigationAnswerSchemaID, Version: InvestigationAnswerSchemaVersion}
}
