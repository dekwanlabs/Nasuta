// Package httputil provides shared HTTP response helpers for all transport handlers.
package httputil

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

type errorRecorder interface {
	RecordHTTPError(error)
}

// WriteJSON writes a success response wrapped in ApiResponse.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(domain.ApiResponse{Code: 0, Message: "", Data: v})
}

// WriteErr writes an error response (HTTP 500) wrapped in ApiResponse.
func WriteErr(w http.ResponseWriter, err error) {
	WriteErrStatus(w, http.StatusInternalServerError, err)
}

// WriteErrStatus writes an error response with a custom HTTP status code.
func WriteErrStatus(w http.ResponseWriter, status int, err error) {
	if recorder, ok := w.(errorRecorder); ok {
		recorder.RecordHTTPError(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	code := status
	if code < 400 {
		code = 500
	}
	json.NewEncoder(w).Encode(domain.ApiResponse{Code: code, Message: err.Error()})
}

// WriteBadRequest writes a 400 response.
func WriteBadRequest(w http.ResponseWriter, msg string) {
	WriteErrStatus(w, http.StatusBadRequest, fmt.Errorf("%s", msg))
}

// WriteServiceUnavailable writes a 503 response.
func WriteServiceUnavailable(w http.ResponseWriter, msg string) {
	WriteErrStatus(w, http.StatusServiceUnavailable, fmt.Errorf("%s", msg))
}

// WriteUnauthorized writes a 401 response.
func WriteUnauthorized(w http.ResponseWriter, msg string) {
	WriteErrStatus(w, http.StatusUnauthorized, fmt.Errorf("%s", msg))
}

// WriteMethodNotAllowed writes a 405 response.
func WriteMethodNotAllowed(w http.ResponseWriter, msg string) {
	WriteErrStatus(w, http.StatusMethodNotAllowed, fmt.Errorf("%s", msg))
}
