// Package ccerr gives chaincode a tiny, dependency-free way to return errors
// that client applications can classify reliably.
//
// Fabric delivers a chaincode error to the client only as a text message, and
// the Fabric Gateway reports it with the same gRPC status code it uses for
// peer-side failures. Guessing the meaning from words like "not found" is
// fragile. Instead, chaincode returns an error from this package, whose message
// starts with a fixed code:
//
//	return ccerr.New(ccerr.NotFound, "challan %s does not exist", id)
//	// message: "NOT_FOUND: challan CH-1 does not exist"
//
// The fabricsdk client parses the prefix with Parse and exposes it as a typed
// error (fabricsdk.IsNotFound and friends).
//
// The package imports only the standard library, so vendoring it into
// chaincode adds nothing else to the chaincode build.
package ccerr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable error category.
type Code string

const (
	// InvalidArgument: the request is malformed or fails validation.
	InvalidArgument Code = "INVALID_ARGUMENT"
	// NotFound: the referenced record does not exist.
	NotFound Code = "NOT_FOUND"
	// AlreadyExists: a record with this key already exists. For idempotent
	// writes the caller can usually treat this as success.
	AlreadyExists Code = "ALREADY_EXISTS"
	// Conflict: the record changed since the caller read it (application-level
	// version check). Re-read and retry.
	Conflict Code = "CONFLICT"
	// Unauthorized: the caller's identity or attributes are not allowed to do this.
	Unauthorized Code = "UNAUTHORIZED"
	// FailedPrecondition: the record is in a state that does not allow this
	// operation (for example, paying a challan that is already cancelled).
	FailedPrecondition Code = "FAILED_PRECONDITION"
)

var known = map[Code]struct{}{
	InvalidArgument:    {},
	NotFound:           {},
	AlreadyExists:      {},
	Conflict:           {},
	Unauthorized:       {},
	FailedPrecondition: {},
}

// Error is a chaincode error carrying a Code.
type Error struct {
	Code    Code
	Message string
}

// Error renders "CODE: message", the wire format Parse understands.
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// New returns an *Error with a formatted message.
func New(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Is reports whether err (or anything it wraps) is a ccerr error with code.
func Is(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Parse extracts the code from a message produced by Error. It accepts and
// strips the "chaincode response NNN, " prefix that the Fabric Gateway adds to
// non-500 chaincode responses. ok is false when no known code is present.
func Parse(message string) (code Code, text string, ok bool) {
	m := strings.TrimSpace(message)
	if strings.HasPrefix(m, "chaincode response ") {
		if i := strings.Index(m, ", "); i >= 0 {
			m = m[i+2:]
		}
	}
	head, rest, found := strings.Cut(m, ":")
	if !found {
		return "", message, false
	}
	c := Code(strings.TrimSpace(head))
	if _, isKnown := known[c]; !isKnown {
		return "", message, false
	}
	return c, strings.TrimSpace(rest), true
}
