package domain

type Question struct {
	Text      string `json:"text"`
	SessionID string `json:"session_id,omitempty"`
}

type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Target string `json:"target"`
}

type RetrievedContext struct {
	Text             string      `json:"text"`
	References       []Reference `json:"references"`
	HitCount         int         `json:"hitCount"`
	OriginalQuestion string      `json:"-"`
}

type AskResult struct {
	RunID   string            `json:"run_id"`
	Answer  string            `json:"answer,omitempty"`
	Context *RetrievedContext `json:"context,omitempty"`
}
