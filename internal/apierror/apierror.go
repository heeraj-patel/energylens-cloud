// Package apierror implements the normative Kernel error envelope
// (plan §4.2): { "error": { code, message, retryable } }.
package apierror

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error is the wire-shape of a single API error.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Envelope wraps an Error under the top-level "error" key.
type Envelope struct {
	Error Error `json:"error"`
}

// APIError couples an HTTP status with the envelope payload. It implements
// the error interface so handlers can return it directly.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Retryable signals whether the caller may safely retry the same request.
	Retryable bool
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// New constructs an APIError.
func New(status int, code, message string, retryable bool) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Retryable: retryable}
}

// Common constructors for frequently used conditions.

func BadRequest(code, msg string) *APIError {
	return New(http.StatusBadRequest, code, msg, false)
}

func Unauthorized(code, msg string) *APIError {
	return New(http.StatusUnauthorized, code, msg, false)
}

func Forbidden(code, msg string) *APIError {
	return New(http.StatusForbidden, code, msg, false)
}

func NotFound(code, msg string) *APIError {
	return New(http.StatusNotFound, code, msg, false)
}

func Conflict(code, msg string) *APIError {
	return New(http.StatusConflict, code, msg, false)
}

// Internal errors are retryable by default: the client may back off and retry.
func Internal(msg string) *APIError {
	return New(http.StatusInternalServerError, "internal_error", msg, true)
}

func Unavailable(msg string) *APIError {
	return New(http.StatusServiceUnavailable, "unavailable", msg, true)
}

// Write serialises an APIError (or any error) as the envelope. Non-APIError
// values are rendered as a retryable internal_error to avoid leaking detail.
func Write(w http.ResponseWriter, err error) {
	ae, ok := err.(*APIError)
	if !ok {
		slog.Error("apierror: non-APIError passed to Write, returning generic internal_error", "err", err)
		ae = Internal("unexpected error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(Envelope{Error: Error{
		Code:      ae.Code,
		Message:   ae.Message,
		Retryable: ae.Retryable,
	}})
}
