package agent

const (
	// TaskContractSchemaID identifies the shared investigator input contract.
	TaskContractSchemaID = "task.contract"
	// TaskContractSchemaVersion identifies the current task contract.
	TaskContractSchemaVersion int64 = 1
)

// TaskContractSchemaRef returns the current task contract identity.
func TaskContractSchemaRef() SchemaRef {
	return SchemaRef{ID: TaskContractSchemaID, Version: TaskContractSchemaVersion}
}
