package platform

import (
	"strings"
	"testing"
)

func TestRedactSensitiveText(t *testing.T) {
	text := `{"headers":{"authorization":"Bearer header-secret"},"response":"access_token=value-secret","nested":"{\"apiKey\":\"nested-secret\"}"}`
	redacted := RedactSensitiveText(text)
	for _, secret := range []string{"header-secret", "value-secret", "nested-secret"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret %q leaked: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, RedactedValue) {
		t.Fatalf("redaction marker missing: %s", redacted)
	}
}

func TestRedactSensitiveTextRemovesPlainAuthorizationHeader(t *testing.T) {
	redacted := RedactSensitiveText("Authorization: Bearer plain-secret\nAccept: application/json")
	if strings.Contains(redacted, "plain-secret") || !strings.Contains(redacted, RedactedValue) {
		t.Fatalf("authorization header leaked: %s", redacted)
	}
}

func TestRedactConfigValue(t *testing.T) {
	if got := RedactConfigValue("spring.datasource.password", "plain-secret"); got != RedactedValue {
		t.Fatalf("sensitive key = %q", got)
	}
	redacted := RedactConfigValue("application", "database:\n  password: |\n    yaml-secret\n    second-line\n  username: app")
	if strings.Contains(redacted, "yaml-secret") || strings.Contains(redacted, "second-line") {
		t.Fatalf("YAML secret leaked: %s", redacted)
	}
}

func TestRedactSensitiveURL(t *testing.T) {
	redacted := RedactSensitiveURL("https://user:db-secret@example.com/path?access_token=url-secret&timestamp=1")
	if strings.Contains(redacted, "db-secret") || strings.Contains(redacted, "url-secret") {
		t.Fatalf("URL secret leaked: %s", redacted)
	}
	if !strings.Contains(redacted, "timestamp=1") {
		t.Fatalf("non-sensitive query was removed: %s", redacted)
	}
}
