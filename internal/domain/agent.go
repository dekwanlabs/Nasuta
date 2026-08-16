package domain

import "time"

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type AgentConfig struct {
	MaxSteps      int           `json:"max_steps"`
	HistoryLimit  int           `json:"history_limit"`
	Timeout       time.Duration `json:"timeout"`
	AnswerReserve time.Duration `json:"answer_reserve"`
}
