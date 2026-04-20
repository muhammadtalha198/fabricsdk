package fabricsdk

import (
	"fmt"
	"strings"

	"github.com/hyperledger/fabric-protos-go-apiv2/gateway"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Error type
// ─────────────────────────────────────────────────────────────────────────────

// ErrorSource identifies whether the error came from the SDK layer
// (network, TLS, marshalling) or from inside the chaincode itself.
type ErrorSource string

const (
	// ErrorSourceSDK means the error occurred in the SDK or transport layer
	// before or after the chaincode ran (e.g. connection failure, commit timeout).
	ErrorSourceSDK ErrorSource = "sdk"

	// ErrorSourceChaincode means the chaincode itself returned an error
	// (e.g. asset not found, version conflict, business rule violation).
	ErrorSourceChaincode ErrorSource = "chaincode"
)

// Error is the structured error returned by all FabricClient methods.
// It carries a standard HTTP-style status code so API handlers and callers
// can map it to a response code without string parsing.
//
//	txID, err := fc.Submit("CreateAsset", string(jsonBytes))
//	var fabErr *fabricsdk.Error
//	if errors.As(err, &fabErr) {
//	    http.Error(w, fabErr.Message, fabErr.Code)
//	}
type Error struct {
	// Source is either ErrorSourceSDK or ErrorSourceChaincode.
	Source ErrorSource

	// Code is an HTTP-style status code derived from the error message:
	//   404 — asset/key not found
	//   409 — version conflict (optimistic concurrency) or duplicate key
	//   403 — unauthorized
	//   424 — failed dependency (e.g. cross-chaincode call failed)
	//   400 — other chaincode business logic error
	//   500 — SDK / transport / commit failure
	Code int

	// Message is a clean, human-readable description of the error.
	// For chaincode errors this is the exact message the chaincode returned.
	// For SDK errors it describes what operation failed.
	Message string

	// Cause is the underlying error, available for logging or further unwrapping.
	// May be nil for errors constructed from chaincode messages directly.
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s %d] %s: %v", e.Source, e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s %d] %s", e.Source, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// ─────────────────────────────────────────────────────────────────────────────
// Convenience predicates
// ─────────────────────────────────────────────────────────────────────────────

// IsNotFound returns true when the chaincode reported that the requested
// asset or key does not exist on the ledger (HTTP 404).
func IsNotFound(err error) bool { return codeOf(err) == 404 }

// IsConflict returns true on an optimistic concurrency conflict —
// the record was modified by another transaction between your read and write
// (HTTP 409). Retry with a fresh GetAsset to get the latest version.
func IsConflict(err error) bool { return codeOf(err) == 409 }

// IsUnauthorized returns true when the chaincode rejected the call due to
// insufficient permissions (HTTP 403).
func IsUnauthorized(err error) bool { return codeOf(err) == 403 }

// IsChaincode returns true when the error originated inside chaincode logic,
// as opposed to a transport or SDK failure.
func IsChaincode(err error) bool {
	var e *Error
	// errors.As not imported here intentionally to keep errors.go dependency-free.
	if fe, ok := err.(*Error); ok {
		e = fe
	}
	return e != nil && e.Source == ErrorSourceChaincode
}

func codeOf(err error) int {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal constructors
// ─────────────────────────────────────────────────────────────────────────────

func newSDKError(code int, msg string) *Error {
	return &Error{Source: ErrorSourceSDK, Code: code, Message: msg}
}

func wrapSDKError(code int, msg string, cause error) *Error {
	return &Error{Source: ErrorSourceSDK, Code: code, Message: msg, Cause: cause}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error classification
// ─────────────────────────────────────────────────────────────────────────────

// classifyError converts a raw Fabric Gateway gRPC error into a *Error.
//
// The Fabric Gateway wraps chaincode errors inside EndorseError/SubmitError
// proto messages. This function digs into the gRPC status Details to find
// the gateway.ErrorDetail proto, which carries the actual chaincode message
// (e.g. "not found: asset-1") rather than the generic gRPC wrapper text.
func classifyError(err error) error {
	msg := extractChaincodeMessage(err)

	// If we found a chaincode message that differs from the raw gRPC error text,
	// it came from chaincode logic — classify it by content.
	if msg != "" && msg != err.Error() {
		return &Error{
			Source:  ErrorSourceChaincode,
			Code:    codeFromMessage(msg),
			Message: strings.TrimSpace(msg),
		}
	}

	// Otherwise it is an SDK/transport error.
	return wrapSDKError(500, "fabric transaction failed", err)
}

// extractChaincodeMessage digs into the gRPC status details to find the
// gateway.ErrorDetail proto that wraps the real chaincode error message.
func extractChaincodeMessage(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	for _, detail := range st.Details() {
		if d, ok := detail.(*gateway.ErrorDetail); ok && d.GetMessage() != "" {
			return d.GetMessage()
		}
	}
	return st.Message()
}

// codeFromMessage maps chaincode error text to an HTTP-style status code.
// These keywords are conventional in Hyperledger Fabric chaincode error messages.
// Applications can add their own mapping on top by inspecting Error.Message.
func codeFromMessage(msg string) int {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "not found"):
		return 404
	case strings.Contains(m, "version conflict"):
		return 409
	case strings.Contains(m, "already exists"):
		return 409
	case strings.Contains(m, "unauthorized"):
		return 403
	case strings.Contains(m, "failed dependency"):
		return 424
	default:
		return 400
	}
}
