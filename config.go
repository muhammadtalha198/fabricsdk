package fabricsdk

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// Default timeouts. Each one applies to a single phase of a call, and only when
// the context passed by the caller has no deadline of its own.
const (
	DefaultEvaluateTimeout     = 5 * time.Second
	DefaultEndorseTimeout      = 15 * time.Second
	DefaultSubmitTimeout       = 5 * time.Second
	DefaultCommitStatusTimeout = 60 * time.Second
)

// Config holds every parameter needed to connect to a Fabric Gateway peer and
// target a chaincode. Populate it from environment variables, YAML, flags or
// code — the SDK does not care how.
type Config struct {
	// ── Network ──────────────────────────────────────────────────────────────

	// PeerEndpoint is the gRPC address of the Gateway peer, e.g.
	// "peer0.org1.example.com:7051". A bare host:port is dialled with the
	// "dns:///" resolver; a value that already has a scheme is used as is.
	PeerEndpoint string

	// PeerHostOverride is the TLS server name to verify, used when PeerEndpoint
	// is an IP address, a load balancer or a port-forward whose name is not in
	// the peer certificate. Empty means "use the host part of PeerEndpoint".
	PeerHostOverride string

	// TLSCertPath is the PEM file holding the CA certificate(s) that issued the
	// peer's TLS certificate (the org's TLS CA, not the enrollment CA).
	TLSCertPath string

	// ClientTLSCertPath and ClientTLSKeyPath enable mutual TLS. Set both or
	// neither. When set, the certificate hash is also passed to the Gateway as
	// Fabric requires for mutual TLS.
	ClientTLSCertPath string
	ClientTLSKeyPath  string

	// KeepAlive, if set, configures gRPC client keepalive pings. Make sure the
	// peer's keepalive enforcement policy allows the interval you choose.
	KeepAlive *keepalive.ClientParameters

	// DialOptions are extra gRPC dial options appended after the SDK's own.
	DialOptions []grpc.DialOption

	// ── Default caller identity ──────────────────────────────────────────────

	// Identity is the default signing identity. If nil, the SDK loads one from
	// MSPID, CertPath and KeyPath.
	Identity *Identity

	// MSPID is the MSP ID of the caller's organization, e.g. "Org1MSP".
	MSPID string

	// CertPath is the signing certificate: a PEM file, or an MSP "signcerts"
	// directory that contains exactly one certificate file.
	CertPath string

	// KeyPath is the private key: a PEM file, or an MSP "keystore" directory
	// that contains exactly one key file.
	KeyPath string

	// ── Default chaincode target ─────────────────────────────────────────────

	ChannelName   string
	ChaincodeName string
	// ContractName selects a named contract inside the chaincode. Empty means
	// the chaincode's default contract.
	ContractName string

	// ── Timeouts (zero = package defaults) ───────────────────────────────────

	EvaluateTimeout     time.Duration
	EndorseTimeout      time.Duration
	SubmitTimeout       time.Duration
	CommitStatusTimeout time.Duration

	// ── Error classification ─────────────────────────────────────────────────

	// StrictErrorCodes disables the keyword fallback used to classify
	// chaincode error messages that do not start with a ccerr code prefix
	// (for example "... not found"). Turn it on once all your chaincode uses
	// the fabricsdk/ccerr package.
	StrictErrorCodes bool

	// ── Optional hooks ───────────────────────────────────────────────────────

	// Logger receives structured log lines. Nil disables logging. The SDK never
	// logs transaction arguments or transient data.
	Logger Logger

	// Observer receives one CallInfo per finished operation, for metrics.
	Observer Observer
}

func (c Config) withDefaults() Config {
	if c.EvaluateTimeout == 0 {
		c.EvaluateTimeout = DefaultEvaluateTimeout
	}
	if c.EndorseTimeout == 0 {
		c.EndorseTimeout = DefaultEndorseTimeout
	}
	if c.SubmitTimeout == 0 {
		c.SubmitTimeout = DefaultSubmitTimeout
	}
	if c.CommitStatusTimeout == 0 {
		c.CommitStatusTimeout = DefaultCommitStatusTimeout
	}
	if c.Logger == nil {
		c.Logger = noopLogger{}
	}
	if c.Observer == nil {
		c.Observer = noopObserver{}
	}
	return c
}

// validate checks the fields New needs. ownConn is false for
// NewWithConnection, where network fields are not used.
func (c Config) validate(ownConn bool) error {
	var problems []string
	if ownConn {
		if strings.TrimSpace(c.PeerEndpoint) == "" {
			problems = append(problems, "PeerEndpoint is required")
		}
		if strings.TrimSpace(c.TLSCertPath) == "" {
			problems = append(problems, "TLSCertPath is required")
		}
		if (c.ClientTLSCertPath == "") != (c.ClientTLSKeyPath == "") {
			problems = append(problems, "ClientTLSCertPath and ClientTLSKeyPath must be set together")
		}
	}
	if c.Identity == nil {
		if strings.TrimSpace(c.MSPID) == "" {
			problems = append(problems, "MSPID is required when Identity is nil")
		}
		if strings.TrimSpace(c.CertPath) == "" {
			problems = append(problems, "CertPath is required when Identity is nil")
		}
		if strings.TrimSpace(c.KeyPath) == "" {
			problems = append(problems, "KeyPath is required when Identity is nil")
		}
	}
	if strings.TrimSpace(c.ChannelName) == "" {
		problems = append(problems, "ChannelName is required")
	}
	if strings.TrimSpace(c.ChaincodeName) == "" {
		problems = append(problems, "ChaincodeName is required")
	}
	for _, d := range []time.Duration{c.EvaluateTimeout, c.EndorseTimeout, c.SubmitTimeout, c.CommitStatusTimeout} {
		if d < 0 {
			problems = append(problems, "timeouts must not be negative")
			break
		}
	}
	if len(problems) > 0 {
		return configError(errors.New(strings.Join(problems, "; ")))
	}
	return nil
}

// grpcTarget turns a host:port into a gRPC target with an explicit resolver.
func grpcTarget(endpoint string) string {
	if strings.Contains(endpoint, "://") || strings.HasPrefix(endpoint, "dns:") ||
		strings.HasPrefix(endpoint, "passthrough:") || strings.HasPrefix(endpoint, "unix:") {
		return endpoint
	}
	return "dns:///" + endpoint
}

// serverNameFor returns the TLS server name to verify.
func serverNameFor(cfg Config) string {
	if cfg.PeerHostOverride != "" {
		return cfg.PeerHostOverride
	}
	host := cfg.PeerEndpoint
	if i := strings.LastIndex(host, "/"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

func configError(err error) error {
	return &Error{Kind: KindConfig, Code: 500, Source: SourceSDK, Message: fmt.Sprintf("invalid configuration: %v", err), Cause: err}
}
