package fabricsdk

import (
	"context"
	"log/slog"
	"time"
)

// Logger is an optional structured logging hook. Adapters for zap, logrus or
// slog are one-liners; NewSlogLogger wraps a *slog.Logger.
//
// The SDK logs function names, transaction IDs and error kinds only. It never
// logs arguments or transient data, which may contain personal data.
type Logger interface {
	Info(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Info(msg string, kv ...any)  { s.l.Info(msg, kv...) }
func (s slogLogger) Error(msg string, kv ...any) { s.l.Error(msg, kv...) }

// NewSlogLogger adapts a *slog.Logger to the Logger interface.
func NewSlogLogger(l *slog.Logger) Logger {
	if l == nil {
		return noopLogger{}
	}
	return slogLogger{l: l}
}

// Operation names reported in CallInfo.
const (
	OpEvaluate = "evaluate"
	OpEndorse  = "endorse"
	OpSubmit   = "submit"
	OpCommit   = "commit"
	OpTxStatus = "tx_status"
)

// CallInfo describes one finished operation. It is passed to Observer.
type CallInfo struct {
	Operation string
	Channel   string
	Chaincode string
	Contract  string
	Function  string
	MSPID     string
	TxID      string
	Duration  time.Duration
	// Err is nil on success. Use KindOf(Err) as a low-cardinality metric label.
	Err error
}

// Observer receives a CallInfo for every finished operation. Use it to feed
// Prometheus or OpenTelemetry without the SDK depending on either. It is
// called synchronously, so keep it fast.
type Observer interface {
	ObserveCall(ctx context.Context, info CallInfo)
}

// ObserverFunc adapts a function to the Observer interface.
type ObserverFunc func(ctx context.Context, info CallInfo)

// ObserveCall calls f.
func (f ObserverFunc) ObserveCall(ctx context.Context, info CallInfo) { f(ctx, info) }

type noopObserver struct{}

func (noopObserver) ObserveCall(context.Context, CallInfo) {}
