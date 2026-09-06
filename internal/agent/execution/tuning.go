package execution

// Tuning constants for the agent loop and tool delivery. They live together so
// a behavior change is reviewable in one place instead of being scattered
// across per-concern files.
const (
	// Evidence observation caps for the worker output projection.
	maxEvidenceObservationTokens = 256
	maxEvidenceObservations      = 128

	// toolArgumentLimit bounds a model tool-call argument payload persisted to
	// session history; larger payloads are replaced with an omission marker.
	toolArgumentLimit = 8_000
)
