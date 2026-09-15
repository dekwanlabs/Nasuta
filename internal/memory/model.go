package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
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

// fixedFactKeys are the single-slot keys: one active record per user. Variable
// topics are not allowed on these so a paraphrase cannot fork the slot.
var fixedFactKeys = map[string]struct{}{
	"user:response-language": {},
	"user:response-style":    {},
	"user:current-focus":     {},
	"user:health":            {},
	"user:culture":           {},
	"user:environment":       {},
	"user:profile-inference": {},
}

// topicFactKeys are the keys that carry one kebab-case topic segment. They allow
// multiple records per user (one per topic) while keeping the namespace closed:
// user:role:<domain> for each distinct role, user:preference:<topic> for a life
// or tool preference, user:correction:<topic> for a past correction.
var topicFactKeys = map[string]struct{}{
	"user:role":       {},
	"user:preference": {},
	"user:correction": {},
}

// validFactKey restricts keys to a controlled vocabulary so the same fact always
// lands on the same slot and the single-active constraint can hold. System and
// codebase facts (the old workspace:* namespace) are not user memories and are
// rejected here.
func validFactKey(key string) bool {
	if _, ok := fixedFactKeys[key]; ok {
		return true
	}
	parts := strings.Split(key, ":")
	if len(parts) == 3 {
		if _, ok := topicFactKeys[parts[0]+":"+parts[1]]; ok {
			return factSegmentPattern.MatchString(parts[2])
		}
	}
	return false
}

// sensitiveFactKeys are the keys whose content touches personal health or custom.
// They are only recorded from an explicit first-person statement.
var sensitiveFactKeys = map[string]struct{}{
	"user:health":  {},
	"user:culture": {},
}

func isSensitiveFactKey(key string) bool {
	_, ok := sensitiveFactKeys[key]
	return ok
}

// isSelfContainedContent rejects fragments, mojibake, and content-free lines so a
// stored memory reads as a complete statement without surrounding context.
func isSelfContainedContent(content string) bool {
	runes := []rune(content)
	if len(runes) < 8 {
		return false
	}
	if strings.Contains(content, "??") || strings.ContainsRune(content, '�') {
		return false
	}
	// Require a minimum share of letters (any script) so pure-symbol or
	// punctuation-only lines are rejected.
	letters := 0
	for _, r := range runes {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters*2 >= len(runes)
}

// hasFirstPersonMarker reports whether content is stated in the user's own voice,
// required before recording sensitive health/custom facts.
func hasFirstPersonMarker(content string) bool {
	if strings.Contains(content, "我") {
		return true
	}
	lower := strings.ToLower(content)
	for _, marker := range []string{"i ", "i'm ", "i am ", "my ", "user "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.HasPrefix(lower, "i ") || strings.HasPrefix(lower, "my ")
}

func canonicalizeRecord(rec MemoryRecord) (MemoryRecord, error) {
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
	if !isSelfContainedContent(rec.Content) {
		return MemoryRecord{}, fmt.Errorf("memory: content must be a self-contained statement")
	}
	if isSensitiveFactKey(rec.FactKey) && !hasFirstPersonMarker(rec.Content) {
		return MemoryRecord{}, fmt.Errorf("memory: sensitive fact %q requires a first-person statement", rec.FactKey)
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
