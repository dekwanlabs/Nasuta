package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MemoryKind controls how a memory participates in recall.
type MemoryKind string

const (
	KindPreference         MemoryKind = "preference"
	KindProfile            MemoryKind = "profile"
	KindWorkContext        MemoryKind = "work_context"
	KindEpisode            MemoryKind = "episode"
	KindAssistantInference MemoryKind = "assistant_inference"
)

// SourceType records who supplied the memory content.
type SourceType string

const (
	SourceExplicitUser       SourceType = "explicit_user"
	SourceUserStated         SourceType = "user_stated"
	SourceAssistantInference SourceType = "assistant_inference"
)

// MemoryStatus has only the current and historical states.
type MemoryStatus string

const (
	StatusActive     MemoryStatus = "active"
	StatusSuperseded MemoryStatus = "superseded"
)

const (
	AuthorityExplicitUser       = 100
	AuthorityUserStated         = 80
	AuthorityAssistantInference = 30
)

// MemoryRecord is one durable long-term memory.
type MemoryRecord struct {
	ID            string       `json:"id"`
	UserID        int64        `json:"user_id"`
	FactKey       string       `json:"fact_key"`
	Kind          MemoryKind   `json:"kind"`
	Content       string       `json:"content"`
	SourceType    SourceType   `json:"source_type"`
	Authority     int          `json:"authority"`
	Status        MemoryStatus `json:"status"`
	SupersededBy  string       `json:"superseded_by,omitempty"`
	SourceSession string       `json:"source_session,omitempty"`
	Confidence    float32      `json:"confidence"`
	ExpiresAt     *time.Time   `json:"expires_at,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	LastUsed      *time.Time   `json:"last_used,omitempty"`
	UseCount      int          `json:"use_count"`
}

type WriteOutcome string

const (
	WriteInserted   WriteOutcome = "inserted"
	WriteRefreshed  WriteOutcome = "refreshed"
	WriteSuperseded WriteOutcome = "superseded"
	WriteRejected   WriteOutcome = "rejected"
)

// WriteResult reports the single durable outcome of a memory write.
type WriteResult struct {
	ID               string
	Outcome          WriteOutcome
	SupersededRecord string
	VectorSynced     bool
}

var (
	factSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	sensitivePattern   = regexp.MustCompile(`(?i)(password|passwd|secret|api[_ -]?key|access[_ -]?token|refresh[_ -]?token|authorization:\s*bearer|-----begin [a-z ]*private key-----)`)
	jwtPattern         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
)

func authorityFor(source SourceType) (int, bool) {
	switch source {
	case SourceExplicitUser:
		return AuthorityExplicitUser, true
	case SourceUserStated:
		return AuthorityUserStated, true
	case SourceAssistantInference:
		return AuthorityAssistantInference, true
	default:
		return 0, false
	}
}

func validKind(kind MemoryKind) bool {
	switch kind {
	case KindPreference, KindProfile, KindWorkContext, KindEpisode, KindAssistantInference:
		return true
	default:
		return false
	}
}

func validFactKey(key string) bool {
	parts := strings.Split(key, ":")
	switch {
	case key == "user:response-language", key == "user:response-style", key == "user:current-focus":
		return true
	case len(parts) == 3 && parts[0] == "user" && parts[1] == "role":
		return factSegmentPattern.MatchString(parts[2])
	case len(parts) == 3 && parts[0] == "workspace":
		return factSegmentPattern.MatchString(parts[1]) && factSegmentPattern.MatchString(parts[2])
	default:
		return false
	}
}

func canonicalizeRecord(rec MemoryRecord, workContextTTL time.Duration, now time.Time) (MemoryRecord, error) {
	rec.FactKey = strings.ToLower(strings.TrimSpace(rec.FactKey))
	rec.Kind = MemoryKind(strings.ToLower(strings.TrimSpace(string(rec.Kind))))
	rec.Content = strings.TrimSpace(rec.Content)
	rec.SourceType = SourceType(strings.ToLower(strings.TrimSpace(string(rec.SourceType))))
	rec.SourceSession = strings.TrimSpace(rec.SourceSession)

	if rec.UserID < 0 {
		return MemoryRecord{}, fmt.Errorf("memory: user_id must not be negative")
	}
	if !validFactKey(rec.FactKey) {
		return MemoryRecord{}, fmt.Errorf("memory: invalid fact_key %q", rec.FactKey)
	}
	if !validKind(rec.Kind) {
		return MemoryRecord{}, fmt.Errorf("memory: invalid kind %q", rec.Kind)
	}
	if rec.Content == "" || strings.ContainsAny(rec.Content, "\r\n") {
		return MemoryRecord{}, fmt.Errorf("memory: content must be one non-empty line")
	}
	if len([]rune(rec.Content)) > 1000 {
		return MemoryRecord{}, fmt.Errorf("memory: content exceeds 1000 characters")
	}
	if sensitivePattern.MatchString(rec.Content) || jwtPattern.MatchString(rec.Content) {
		return MemoryRecord{}, fmt.Errorf("memory: sensitive content is not allowed")
	}

	authority, ok := authorityFor(rec.SourceType)
	if !ok {
		return MemoryRecord{}, fmt.Errorf("memory: invalid source_type %q", rec.SourceType)
	}
	if rec.SourceType == SourceAssistantInference && rec.Kind != KindAssistantInference {
		return MemoryRecord{}, fmt.Errorf("memory: assistant inference must use kind %q", KindAssistantInference)
	}
	if rec.SourceType != SourceAssistantInference && rec.Kind == KindAssistantInference {
		return MemoryRecord{}, fmt.Errorf("memory: user-sourced memory cannot use kind %q", KindAssistantInference)
	}

	rec.Authority = authority
	rec.Status = StatusActive
	rec.SupersededBy = ""
	if rec.Confidence <= 0 {
		rec.Confidence = 1
	}
	if rec.Confidence > 1 {
		rec.Confidence = 1
	}
	if rec.Kind == KindWorkContext && rec.ExpiresAt == nil {
		expiresAt := now.Add(workContextTTL)
		rec.ExpiresAt = &expiresAt
	}
	return rec, nil
}

func trustFor(source SourceType) string {
	switch source {
	case SourceExplicitUser:
		return "user_explicit"
	case SourceUserStated:
		return "user_stated"
	default:
		return "unverified_inference"
	}
}
