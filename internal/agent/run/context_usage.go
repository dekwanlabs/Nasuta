package run

const ContextHighWaterPercent = 80

// ContextHighWaterTokens is the shared threshold for proactive compaction.
func ContextHighWaterTokens(window int) int {
	return max(0, window) * ContextHighWaterPercent / 100
}

// ContextSafetyTokens is reserved for provider-side tokenization differences
// and request-envelope overhead that local estimation cannot observe exactly.
func ContextSafetyTokens(window int) int {
	if window <= 0 {
		return 0
	}
	return max(window/20, 1024)
}

// ContextSafeLimitTokens is the largest projected input-plus-output reservation
// admitted before a provider call.
func ContextSafeLimitTokens(window int) int {
	return max(0, window-ContextSafetyTokens(window))
}
