package incidenthttp

import (
	"net/http"
	"testing"
)

func TestIncidentAndApprovalRoutesAreRegistered(t *testing.T) {
	registered := map[string]bool{}
	(&Handler{}).RegisterRoutes(func(pattern string, _ http.HandlerFunc) {
		registered[pattern] = true
	})
	for _, pattern := range []string{
		"POST /api/alert/webhook",
		"GET /api/incidents",
		"POST /api/incidents/{id}/fix",
		"GET /api/qa/actions",
		"POST /api/qa/actions/{id}",
	} {
		if !registered[pattern] {
			t.Fatalf("platform incident route %q is not registered", pattern)
		}
	}
}
