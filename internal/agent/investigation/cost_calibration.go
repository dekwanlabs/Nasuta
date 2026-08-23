package investigation

// CalibrateTemplateCosts derives a conservative p95 task budget per template
// from completed runs. It is the input for future template cost profiles and
// never mutates the live catalog.
func CalibrateTemplateCosts(runs []InvestigationRun) map[string]BudgetVector {
	type dimension struct {
		input  []int64
		output []int64
		total  []int64
		tools  []int
		cost   []int64
	}
	byTemplate := make(map[string]*dimension)
	for _, run := range runs {
		for taskID, record := range run.Results {
			task, ok := run.Tasks[taskID]
			if !ok {
				continue
			}
			bucket := byTemplate[task.Template.ID]
			if bucket == nil {
				bucket = &dimension{}
				byTemplate[task.Template.ID] = bucket
			}
			bucket.input = append(bucket.input, record.Usage.InputTokens)
			bucket.output = append(bucket.output, record.Usage.OutputTokens)
			bucket.total = append(bucket.total, tokenTotal(record.Usage))
			bucket.tools = append(bucket.tools, record.Usage.ToolCalls)
			bucket.cost = append(bucket.cost, record.Usage.CostMicros)
		}
	}
	result := make(map[string]BudgetVector, len(byTemplate))
	for templateID, bucket := range byTemplate {
		result[templateID] = BudgetVector{
			InputTokens:  percentileInt64(bucket.input, 0.95),
			OutputTokens: percentileInt64(bucket.output, 0.95),
			TotalTokens:  percentileInt64(bucket.total, 0.95),
			ToolCalls:    percentileInt(bucket.tools, 0.95),
			CostMicros:   percentileInt64(bucket.cost, 0.95),
		}
	}
	return result
}
