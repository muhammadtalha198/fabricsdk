package fabricsdk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-protos-go-apiv2/gateway"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/muhammadtalha198/fabricsdk/ccerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Kind is the category of an Error. Switch on it, or use the Is* helpers.
type Kind string

const (
	// Errors returned by chaincode logic.
	KindInvalidArgument    Kind = "invalid_argument"    // 400
	KindNotFound           Kind = "not_found"           // 404
	KindAlreadyExists      Kind = "already_exists"      // 409, do not retry
	KindConflict           Kind = "conflict"            // 409, re-read and retry
	KindUnauthorized       Kind = "unauthorized"        // 403
	KindFailedPrecondition Kind = "failed_precondition" // 422
	KindChaincode          Kind = "chaincode"           // 400, unclassified chaincode error

	// Errors from the network, the peers or the orderers.
	KindEndorsement    Kind = "endorsement"     // 502, endorsement failed for a non-chaincode reason
	KindSubmit         Kind = "submit"          // 502, the orderer did not accept the transaction
	KindCommitFailed   Kind = "commit_failed"   // 500, committed as invalid (not a read conflict)
	KindOutcomeUnknown Kind = "outcome_unknown" // 504, the transaction may or may not be on the ledger
	KindTimeout        Kind = "timeout"         // 504, timed out before reaching the orderer
	KindUnavailable    Kind = "unavailable"     // 503
	KindCanceled       Kind = "canceled"        // 499
	KindConfig         Kind = "config"          // 500, bad SDK configuration or identity
	KindInternal       Kind = "internal"        // 500
)

// ErrorSource says which layer produced an error.
type ErrorSource string

const (
	SourceSDK       ErrorSource = "sdk"       // configuration, identity, local failures
	SourceChaincode ErrorSource = "chaincode" // the chaincode returned an error
	SourceNetwork   ErrorSource = "network"   // gateway, peers, orderers, transport
)

// ErrorDetail is one peer's or orderer's contribution to a Gateway error.
type ErrorDetail struct {
	Address string
	MSPID   string
	Message string
}

// Error is the structured error returned by every SDK call. Inspect it with
// errors.As, or with the Is* helpers, which also see through wrapping.
type Error struct {
	Kind   Kind
	Source ErrorSource
	// Code is an HTTP-style status code suggestion for API handlers.
	Code int
	// Message is a clean description. For chaincode errors it is the
	// chaincode's message without the ccerr code prefix.
	Message string
	// TxID is set whenever a transaction ID existed at the time of failure.
	// Keep it: it is the key for TxStatus and for reconciliation.
	TxID string
	// ValidationCode is set for KindConflict and KindCommitFailed when the
	// transaction was committed as invalid.
	ValidationCode peer.TxValidationCode
	// GRPCCode is the gRPC status code reported by the Gateway, if any.
	GRPCCode codes.Code
	// Details holds per-node messages reported by the Gateway.
	Details []ErrorDetail
	// OutcomeUnknown is true when the transaction may already be ordered. Do
	// not resubmit blindly: call TxStatus with TxID, or let a reconciler do it.
	OutcomeUnknown bool
	// Cause is the underlying error (often a fabric-gateway typed error).
	Cause error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fabricsdk: %s", e.Kind)
	if e.TxID != "" {
		fmt.Fprintf(&b, " (tx %s)", e.TxID)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// Retryable reports whether sending the same request again is safe and may
// succeed. It is never true when OutcomeUnknown is true.
func (e *Error) Retryable() bool {
	if e.OutcomeUnknown {
		return false
	}
	switch e.Kind {
	case KindConflict, KindUnavailable, KindTimeout, KindEndorsement, KindSubmit:
		// KindSubmit is only produced for definite rejections before ordering.
		return true
	}
	return false
}

func asError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// KindOf returns the Kind of err, or "" if err is not an *Error.
func KindOf(err error) Kind {
	if e, ok := asError(err); ok {
		return e.Kind
	}
	return ""
}

// TxIDOf returns the transaction ID carried by err, or "".
func TxIDOf(err error) string {
	if e, ok := asError(err); ok {
		return e.TxID
	}
	return ""
}

func isKind(err error, k Kind) bool { return KindOf(err) == k }

// IsNotFound reports a chaincode NOT_FOUND error.
func IsNotFound(err error) bool { return isKind(err, KindNotFound) }

// IsAlreadyExists reports a chaincode ALREADY_EXISTS error. For idempotent
// writes keyed by a deterministic ID this usually means "already done".
func IsAlreadyExists(err error) bool { return isKind(err, KindAlreadyExists) }

// IsConflict reports an optimistic-concurrency conflict: either an MVCC or
// phantom read conflict at commit, or a chaincode CONFLICT error. Re-read the
// data and retry.
func IsConflict(err error) bool { return isKind(err, KindConflict) }

// IsUnauthorized reports a chaincode UNAUTHORIZED error or a Fabric
// permission-denied response.
func IsUnauthorized(err error) bool { return isKind(err, KindUnauthorized) }

// IsInvalidArgument reports a chaincode INVALID_ARGUMENT error or a request
// the Gateway rejected as malformed.
func IsInvalidArgument(err error) bool { return isKind(err, KindInvalidArgument) }

// IsTimeout reports a timeout that happened before the transaction could have
// reached the orderer. Timeouts after that point are IsOutcomeUnknown.
func IsTimeout(err error) bool { return isKind(err, KindTimeout) }

// IsOutcomeUnknown reports that the transaction may or may not be committed.
func IsOutcomeUnknown(err error) bool {
	e, ok := asError(err)
	return ok && e.OutcomeUnknown
}

// IsRetryable reports whether the same request can be safely sent again.
func IsRetryable(err error) bool {
	e, ok := asError(err)
	return ok && e.Retryable()
}

// IsChaincode reports that the error came from chaincode logic.
func IsChaincode(err error) bool {
	e, ok := asError(err)
	return ok && e.Source == SourceChaincode
}

// ── classification ───────────────────────────────────────────────────────────

type phase int

const (
	phaseEvaluate phase = iota
	phaseEndorse
	phaseSubmit
	phaseCommitStatus
)

var kindCode = map[Kind]int{
	KindInvalidArgument: 400, KindNotFound: 404, KindAlreadyExists: 409, KindConflict: 409,
	KindUnauthorized: 403, KindFailedPrecondition: 422, KindChaincode: 400,
	KindEndorsement: 502, KindSubmit: 502, KindCommitFailed: 500, KindOutcomeUnknown: 504,
	KindTimeout: 504, KindUnavailable: 503, KindCanceled: 499, KindConfig: 500, KindInternal: 500,
}

var ccKind = map[ccerr.Code]Kind{
	ccerr.InvalidArgument:    KindInvalidArgument,
	ccerr.NotFound:           KindNotFound,
	ccerr.AlreadyExists:      KindAlreadyExists,
	ccerr.Conflict:           KindConflict,
	ccerr.Unauthorized:       KindUnauthorized,
	ccerr.FailedPrecondition: KindFailedPrecondition,
}

// peerSidePhrases mark messages generated by the peer rather than by chaincode
// logic (wording taken from Fabric v2.5 core/endorser and core/chaincode). The
// keyword fallback must not classify these, e.g. "chaincode basic not found".
var peerSidePhrases = []string{
	"make sure the chaincode",
	"error in simulation",
	"could not launch chaincode",
	"failed to execute transaction",
	"transaction returned with failure",
	"channel '",
	"has not been initialized",
	"does not require initialization",
	"is already initialized",
	"access denied",
	"chaincode definition",
}

// legacyKind maps free-text chaincode messages (the v0.1 behaviour, made
// safer). It returns "" when nothing matches.
func legacyKind(msg string) Kind {
	m := strings.ToLower(msg)
	for _, p := range peerSidePhrases {
		if strings.Contains(m, p) {
			return ""
		}
	}
	switch {
	case strings.Contains(m, "version conflict"):
		return KindConflict
	case strings.Contains(m, "already exists"):
		return KindAlreadyExists
	case strings.Contains(m, "not found"), strings.Contains(m, "does not exist"):
		return KindNotFound
	case strings.Contains(m, "unauthorized"), strings.Contains(m, "forbidden"), strings.Contains(m, "permission denied"):
		return KindUnauthorized
	}
	return ""
}

func newError(kind Kind, source ErrorSource, msg string) *Error {
	return &Error{Kind: kind, Source: source, Code: kindCode[kind], Message: msg}
}

func gatewayDetails(st *status.Status) []ErrorDetail {
	var out []ErrorDetail
	for _, d := range st.Details() {
		if gd, ok := d.(*gateway.ErrorDetail); ok {
			out = append(out, ErrorDetail{Address: gd.GetAddress(), MSPID: gd.GetMspId(), Message: gd.GetMessage()})
		}
	}
	return out
}

// classify converts an error from fabric-gateway into an *Error.
func classify(err error, txID string, ph phase, strict bool) error {
	if err == nil {
		return nil
	}
	if e, ok := asError(err); ok {
		return e
	}

	var commitErr *client.CommitError
	if errors.As(err, &commitErr) {
		return commitFailure(commitErr.TransactionID, commitErr.Code, err)
	}

	// status.FromError understands the fabric-gateway typed errors and plain
	// gRPC errors; context errors raised before any RPC map to their codes.
	st := status.Convert(err)
	switch {
	case errors.Is(err, context.Canceled) && st.Code() == codes.Unknown:
		st = status.New(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded) && st.Code() == codes.Unknown:
		st = status.New(codes.DeadlineExceeded, err.Error())
	}
	code := st.Code()
	details := gatewayDetails(st)

	// Chaincode errors can only appear in evaluate and endorse responses.
	if ph == phaseEvaluate || ph == phaseEndorse {
		if e := chaincodeError(st, details, strict); e != nil {
			e.TxID, e.GRPCCode, e.Details, e.Cause = txID, code, details, err
			return e
		}
	}

	var e *Error
	switch ph {
	case phaseCommitStatus:
		// The orderer accepted the transaction; only its final status is unknown.
		e = newError(KindOutcomeUnknown, SourceNetwork, "could not confirm commit status: "+st.Message())
		e.OutcomeUnknown = true
	case phaseSubmit:
		switch code {
		case codes.InvalidArgument, codes.FailedPrecondition, codes.Aborted:
			// Rejected before ordering: a malformed request, or an explicit
			// unsuccessful response from the orderer. Nothing was ordered.
			e = newError(KindSubmit, SourceNetwork, st.Message())
		default:
			// Timeouts, cancellation and transport failures during broadcast
			// leave the outcome open.
			e = newError(KindOutcomeUnknown, SourceNetwork, "submit to orderer did not complete: "+st.Message())
			e.OutcomeUnknown = true
		}
	default:
		e = newError(kindFromCode(code, ph), SourceNetwork, st.Message())
	}
	e.TxID, e.GRPCCode, e.Details, e.Cause = txID, code, details, err
	return e
}

func kindFromCode(code codes.Code, ph phase) Kind {
	switch code {
	case codes.InvalidArgument:
		return KindInvalidArgument
	case codes.PermissionDenied, codes.Unauthenticated:
		return KindUnauthorized
	case codes.NotFound:
		return KindNotFound
	case codes.DeadlineExceeded:
		return KindTimeout
	case codes.Canceled:
		return KindCanceled
	case codes.Unavailable:
		return KindUnavailable
	case codes.FailedPrecondition:
		return KindFailedPrecondition
	case codes.Aborted, codes.Unknown:
		if ph == phaseEndorse || ph == phaseEvaluate {
			return KindEndorsement
		}
	}
	return KindInternal
}

// chaincodeError looks for a chaincode-generated message in the Gateway error
// details (or, for evaluate, in the status message). It returns nil when the
// error does not look like it came from chaincode logic.
func chaincodeError(st *status.Status, details []ErrorDetail, strict bool) *Error {
	if st.Code() != codes.Aborted && st.Code() != codes.Unknown {
		return nil
	}
	candidates := make([]string, 0, len(details)+1)
	for _, d := range details {
		candidates = append(candidates, d.Message)
	}
	if msg := st.Message(); msg != "" {
		candidates = append(candidates, strings.TrimPrefix(msg, "evaluate call to endorser returned error: "))
	}

	// 1. A ccerr code prefix is authoritative.
	for _, m := range candidates {
		if code, text, ok := ccerr.Parse(m); ok {
			return newError(ccKind[code], SourceChaincode, text)
		}
	}
	// 2. "chaincode response NNN, msg" is always chaincode-generated.
	for _, m := range candidates {
		if strings.HasPrefix(m, "chaincode response ") {
			_, text, _ := strings.Cut(m, ", ")
			if !strict {
				if k := legacyKind(text); k != "" {
					return newError(k, SourceChaincode, text)
				}
			}
			return newError(KindChaincode, SourceChaincode, text)
		}
	}
	if strict || len(details) == 0 {
		return nil
	}
	// 3. Fabric v2.5 returns status-500 chaincode errors without that prefix,
	// indistinguishable from peer errors. Fall back to keywords, skipping
	// known peer-side wording.
	for _, d := range details {
		if k := legacyKind(d.Message); k != "" {
			return newError(k, SourceChaincode, d.Message)
		}
	}
	return nil
}

func commitFailure(txID string, vc peer.TxValidationCode, cause error) *Error {
	name := peer.TxValidationCode_name[int32(vc)]
	var e *Error
	switch vc {
	case peer.TxValidationCode_MVCC_READ_CONFLICT, peer.TxValidationCode_PHANTOM_READ_CONFLICT:
		e = newError(KindConflict, SourceNetwork, fmt.Sprintf("transaction invalidated at commit by %s; re-read and retry", name))
	default:
		e = newError(KindCommitFailed, SourceNetwork, fmt.Sprintf("transaction committed as invalid: %s (%d)", name, int32(vc)))
	}
	e.TxID, e.ValidationCode, e.Cause = txID, vc, cause
	return e
}
