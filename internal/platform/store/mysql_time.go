package store

import (
	"database/sql"
	"time"
)

// DatabaseTime converts the API's RFC3339 representation into a native SQL
// time value. Keeping this conversion at the MySQL boundary lets JSON-facing
// records remain backward-compatible while database columns stay temporal.
func DatabaseTime(value string) any {
	if value == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return value
}

// FormatDatabaseTime converts a nullable MySQL timestamp into the stable
// RFC3339 JSON representation used by existing dashboard APIs.
func FormatDatabaseTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
}
