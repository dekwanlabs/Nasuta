package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestParseQAAskRequestSourceMode(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMode    string
		wantSources domain.EvidenceSources
		wantPlan    bool
		wantErr     bool
	}{
		{name: "default auto", body: `{"question":" q "}`, wantMode: "auto"},
		{name: "explicit web", body: `{"question":"q","source_mode":" WEB "}`, wantMode: "web", wantSources: domain.Web, wantPlan: true},
		{name: "explicit direct", body: `{"question":"q","source_mode":"direct"}`, wantMode: "direct", wantPlan: true},
		{name: "invalid", body: `{"question":"q","source_mode":"database"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parseQAAskRequest(httptest.NewRequest("POST", "/api/qa/ask", strings.NewReader(tt.body)))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr=%t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if req.SourceMode != tt.wantMode || (req.EvidencePlan != nil) != tt.wantPlan {
				t.Fatalf("request = %+v", req)
			}
			if req.EvidencePlan != nil && req.EvidencePlan.Sources != tt.wantSources {
				t.Fatalf("sources = %08b, want %08b", req.EvidencePlan.Sources, tt.wantSources)
			}
		})
	}
}

func TestQARuntimeStatusFormatting(t *testing.T) {
	if got := qaEndpointDomain("https://api.example.com:8443/v1"); got != "api.example.com" {
		t.Fatalf("endpoint domain = %q", got)
	}
	if got := cachePercent(90, 100); got != 90 {
		t.Fatalf("cache percent = %d, want 90", got)
	}
	if got := cachePercent(0, 0); got != 0 {
		t.Fatalf("empty cache percent = %d, want 0", got)
	}
}
