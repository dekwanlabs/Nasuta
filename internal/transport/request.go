package transport

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

// AuthenticatedUser returns the user attached to the request context. It
// writes the common unauthorized response when authentication is missing.
func AuthenticatedUser(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		httputil.WriteUnauthorized(w, "authentication required")
		return nil, false
	}
	return user, true
}

// PathVersion parses the positive integer version path parameter used by
// catalog control endpoints.
func PathVersion(r *http.Request) (int64, error) {
	version, err := strconv.ParseInt(
		strings.TrimSpace(r.PathValue("version")), 10, 64,
	)
	if err != nil || version <= 0 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}
