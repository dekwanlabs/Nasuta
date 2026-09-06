package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/tools"
)

func TestCodeSearchBackendFailureReturnsHTTPError(t *testing.T) {
	handler := &Handler{tools: tools.New(tools.Deps{})}
	request := httptest.NewRequest(http.MethodGet, "/api/tools/code-search?query=orders", nil)
	response := httptest.NewRecorder()

	handler.CodeSearch(response, request)

	if response.Code < http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
