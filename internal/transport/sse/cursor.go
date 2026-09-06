package sse

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Cursor reads a non-negative event sequence from the query string and, when
// enabled, the Last-Event-ID header.
func Cursor(r *http.Request, allowLastEventID bool) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if value == "" && allowLastEventID {
		value = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return seq, nil
}
