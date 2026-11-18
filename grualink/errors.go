package grualink

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every error returned by this package matches ErrGrualink and exactly one of the
// more specific kinds below with errors.Is. Use errors.As with *APIError to read the status, the
// code and the details of an answer of the API.
var (
	// ErrGrualink is the base of all: it matches every error returned by this package.
	ErrGrualink = errors.New("grualink: error")
	// ErrConfig means invalid configuration or argument; nothing was sent.
	ErrConfig = errors.New("grualink: invalid configuration or argument")
	// ErrConnection means there was no HTTP response (DNS, connection reset, TLS...). Retried automatically.
	ErrConnection = errors.New("grualink: no HTTP response")
	// ErrTimeout means there was no response within the timeout. Retried automatically.
	ErrTimeout = errors.New("grualink: no response within the timeout")
	// ErrAPI matches every error answered by the API (every *APIError).
	ErrAPI = errors.New("grualink: the API answered an error")
	// ErrInvalidRequest is 400, 405, 413 or 415: the request is malformed. Fix the request; do not retry.
	ErrInvalidRequest = errors.New("grualink: invalid request")
	// ErrAuthentication is 401 unauthorized: key missing, invalid, expired or revoked.
	ErrAuthentication = errors.New("grualink: authentication failed")
	// ErrPermission is 403: the key is not allowed to do this.
	ErrPermission = errors.New("grualink: permission denied")
	// ErrNotFound is 404 not_found: it does not exist or does not belong to your key.
	ErrNotFound = errors.New("grualink: not found")
	// ErrIdempotencyConflict is 409: the Idempotency-Key was already used with a different body.
	ErrIdempotencyConflict = errors.New("grualink: idempotency conflict")
	// ErrValidation is 422 validation_error: see APIError.Details for the invalid fields.
	ErrValidation = errors.New("grualink: validation failed")
	// ErrRateLimit is 429: rate_limited (retried automatically) or too_many_open_requests (not retried).
	ErrRateLimit = errors.New("grualink: rate limited")
	// ErrServer is 5xx. Retried automatically; safe because the same Idempotency-Key is reused.
	ErrServer = errors.New("grualink: server error")
)

// CodeUnexpectedResponse is the code of an answer whose body is not the documented shape.
const CodeUnexpectedResponse = "unexpected_response"

// ErrorDetail is one entry of error.details in a 400 or 422 response.
type ErrorDetail struct {
	// Field is the dotted path of the offending field, for example contact.phone.
	Field string `json:"field"`
	// Issue is one of: required, invalid_type, too_long, invalid_format, out_of_range, unknown_field,
	// invalid_value, duplicate.
	Issue string `json:"issue"`
}

// Error is returned when there is no answer of the API to report: Kind is ErrConfig, ErrConnection
// or ErrTimeout.
type Error struct {
	// Kind is the sentinel this error matches with errors.Is.
	Kind error
	// Message describes the failure. It never contains the API key.
	Message string
	// Attempts is the number of HTTP attempts made before giving up (0 when no request was sent).
	Attempts int

	cause error
}

func (e *Error) Error() string { return "grualink: " + e.Message }

// Is reports whether target is ErrGrualink or the kind of this error.
func (e *Error) Is(target error) bool { return target == ErrGrualink || target == e.Kind }

// Unwrap returns the context error (context.Canceled or context.DeadlineExceeded) when the context
// passed to the call ended, and nil otherwise.
func (e *Error) Unwrap() error { return e.cause }

// APIError is an error answered by the API. Code is the stable machine-readable code of
// {"error": {"code", "message", "details"}}; Message is written for people and may change. Branch on
// Code and Status, never on Message.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is the stable error code, or unexpected_response when the body is not the documented shape.
	Code string
	// Message is the text sent by the API (in Spanish). It never contains the API key.
	Message string
	// Details lists the invalid fields on validation_error and on some invalid_request.
	Details []ErrorDetail
	// RetryAfterSeconds is the value of the Retry-After header in seconds, or nil when absent.
	RetryAfterSeconds *int
	// Attempts is the number of HTTP attempts made before giving up.
	Attempts int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("grualink: API error %d %s: %s", e.Status, e.Code, e.Message)
}

// Kind maps the HTTP status to the sentinel that represents it. A status outside the documented
// table has the kind ErrAPI.
func (e *APIError) Kind() error {
	switch s := e.Status; {
	case s == 401:
		return ErrAuthentication
	case s == 403:
		return ErrPermission
	case s == 404:
		return ErrNotFound
	case s == 409:
		return ErrIdempotencyConflict
	case s == 422:
		return ErrValidation
	case s == 429:
		return ErrRateLimit
	case s >= 500:
		return ErrServer
	case s == 400 || s == 405 || s == 413 || s == 415:
		return ErrInvalidRequest
	}
	return ErrAPI
}

// Is reports whether target is ErrGrualink, ErrAPI or the kind of this error.
func (e *APIError) Is(target error) bool {
	return target == ErrGrualink || target == ErrAPI || target == e.Kind()
}

func configError(format string, args ...any) *Error {
	return &Error{Kind: ErrConfig, Message: fmt.Sprintf(format, args...)}
}
