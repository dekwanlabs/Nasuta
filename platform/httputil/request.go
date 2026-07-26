package httputil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// QueryParams provides typed access to URL query parameters.
// It accumulates the first parse or validation error so handlers can check once via Err().
// Values are trimmed before parsing.
type QueryParams struct {
	v   url.Values
	err error
}

// Query builds a QueryParams from the request URL.
func Query(r *http.Request) *QueryParams {
	return &QueryParams{v: r.URL.Query()}
}

// Err returns the first accumulated error, or nil.
func (q *QueryParams) Err() error { return q.err }

func (q *QueryParams) fail(err error) {
	if q.err == nil {
		q.err = err
	}
}

// Str returns the trimmed value for key (empty string if absent).
func (q *QueryParams) Str(key string) string {
	return strings.TrimSpace(q.v.Get(key))
}

// StrDefault returns the trimmed value for key, or def when absent/empty.
func (q *QueryParams) StrDefault(key, def string) string {
	if s := q.Str(key); s != "" {
		return s
	}
	return def
}

// Required returns the trimmed value for key, recording an error when empty.
func (q *QueryParams) Required(key string) string {
	s := q.Str(key)
	if s == "" {
		q.fail(fmt.Errorf("missing required query param %q", key))
	}
	return s
}

// Int returns the integer value for key. An absent/empty value yields def; a
// present-but-non-numeric value records an error and returns def.
func (q *QueryParams) Int(key string, def int) int {
	s := q.Str(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		q.fail(fmt.Errorf("invalid integer query param %q: %q", key, s))
		return def
	}
	return n
}

// Page returns canonical pagination values and accepts the legacy pageSize alias.
func (q *QueryParams) Page(defaultSize, maxSize int) (int, int) {
	if defaultSize <= 0 || defaultSize > maxSize {
		defaultSize = maxSize
	}
	page := q.Int("page", 1)
	if page < 1 {
		page = 1
	}
	size := defaultSize
	if q.Str("page_size") != "" {
		size = q.Int("page_size", defaultSize)
	} else if q.Str("pageSize") != "" {
		size = q.Int("pageSize", defaultSize)
	}
	if size <= 0 {
		size = defaultSize
	} else if size > maxSize {
		size = maxSize
	}
	return page, size
}

// Bool reports whether key is a truthy flag (1/true/yes, case-insensitive).
func (q *QueryParams) Bool(key string) bool {
	switch strings.ToLower(q.Str(key)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// DecodeJSON decodes the request body into dst, wrapping parse errors so
// handlers can surface a consistent 400.
func DecodeJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// DecodeStrictJSON rejects fields outside the endpoint contract.
func DecodeStrictJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
