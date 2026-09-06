package qa

import "time"

// Tuning constants for the QA preparation pipeline. They live together so a
// behavior change is reviewable in one place instead of being scattered across
// per-concern files.
const (
	// helperTimeout bounds each pre-retrieval LLM helper. A stuck helper degrades
	// to its fallback (clean question / tech terms / original question) instead of
	// stalling retrieval until the request deadline. The parent ctx caps it lower.
	helperTimeout = 12 * time.Second

	// sessionArchiveTimeout bounds the asynchronous post-turn history archive.
	sessionArchiveTimeout = 2 * time.Minute

	// Active history selection and budget.
	activeHistoryTopK      = 4
	activeHistoryMaxTokens = 32_000
	routeHistoryTextTokens = 512

	// preloadedContextBudget caps prefetched/recalled context blocks before
	// retrieval is merged in.
	preloadedContextBudget = 16000
)
