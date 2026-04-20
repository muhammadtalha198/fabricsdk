// Package fabricsdk provides a simple, ethers.js-style client for
// Hyperledger Fabric. It handles gRPC connections, TLS, x509 identity
// loading, transaction submission, and commit waiting — so application
// code only needs to call Evaluate (read) or Submit (write).
//
// # Quickstart
//
//	cfg := fabricsdk.Config{
//	    PeerEndpoint:     "localhost:7051",
//	    PeerHostOverride: "peer0.org1.example.com",
//	    TLSCertPath:      "/path/to/peer-tls-ca.pem",    // peer TLS CA cert file
//	    CertPath:         "/path/to/msp/signcerts",       // directory with signing cert
//	    KeyPath:          "/path/to/msp/keystore",        // directory with private key
//	    MSPID:            "Org1MSP",
//	    ChannelName:      "mychannel",
//	    ChaincodeName:    "mychaincode",
//	    ContractName:     "MyContract",  // leave empty for default contract
//	}
//
//	fc, err := fabricsdk.New(cfg)
//	if err != nil { ... }
//	defer fc.Close()
//
//	ctx := context.Background()
//
//	// Read — EvaluateTransaction, no ledger write
//	raw, err := fc.Evaluate(ctx, "GetAsset", "asset-1")
//
//	// Write — SubmitAsync + commit wait, returns txID
//	txID, err := fc.Submit(ctx, "CreateAsset", string(jsonBytes))
//
//	// Different caller identity for one call
//	txID, err = fc.WithIdentity("/admin/signcerts", "/admin/keystore").Submit(ctx, "AdminFn", arg)
package fabricsdk

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/hash"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ─────────────────────────────────────────────────────────────────────────────
// Logger interface — optional structured logging hook
// ─────────────────────────────────────────────────────────────────────────────

// Logger is an optional logging interface you can inject into the SDK.
// If nil, all logging is suppressed. Compatible with zap.SugaredLogger,
// logrus.Entry, slog, or any custom adapter.
//
//	type zapAdapter struct{ l *zap.SugaredLogger }
//	func (a *zapAdapter) Info(msg string, kv ...any)  { a.l.Infow(msg, kv...) }
//	func (a *zapAdapter) Error(msg string, kv ...any) { a.l.Errorw(msg, kv...) }
//
//	cfg.Logger = &zapAdapter{l: logger.Sugar()}
type Logger interface {
	Info(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

// noopLogger swallows all log calls when no Logger is configured.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

// Config holds every parameter needed to connect to a Fabric peer and target
// a chaincode. Construct it from environment variables, YAML, flags, or
// hardcoded values — the SDK does not care how you populate it.
type Config struct {
	// ── Network ──────────────────────────────────────────────────────────────
	// PeerEndpoint is the host:port of the peer gRPC endpoint.
	// Example: "localhost:7051" or "peer0.org1.example.com:7051"
	PeerEndpoint string

	// PeerHostOverride is the TLS SNI hostname sent during the TLS handshake.
	// Required when PeerEndpoint is an IP or differs from the cert's CN/SAN.
	// Example: "peer0.org1.example.com"
	PeerHostOverride string

	// TLSCertPath is the path to the peer's TLS CA certificate PEM file.
	// Used to verify the peer's TLS certificate.
	TLSCertPath string

	// ── Caller identity (default) ────────────────────────────────────────────
	// CertPath is a directory containing the x509 signing certificate PEM file.
	// Fabric MSP signcerts directories hold exactly one file — this SDK reads it.
	CertPath string

	// KeyPath is a directory containing the private key PEM file corresponding
	// to the signing certificate. Fabric MSP keystore directories hold one file.
	KeyPath string

	// MSPID is the Membership Service Provider identifier for the caller's org.
	// Example: "Org1MSP"
	MSPID string

	// ── Chaincode target ─────────────────────────────────────────────────────
	// ChannelName is the Fabric channel the chaincode is deployed on.
	ChannelName string

	// ChaincodeName is the name the chaincode was deployed with.
	ChaincodeName string

	// ContractName is the named contract inside the chaincode.
	// Leave empty to use the default (unnamed) contract.
	ContractName string

	// ── Timeouts (zero = library defaults) ───────────────────────────────────
	EvaluateTimeout     time.Duration // default: 5s
	EndorseTimeout      time.Duration // default: 15s
	SubmitTimeout       time.Duration // default: 5s
	CommitStatusTimeout time.Duration // default: 60s

	// ── Optional hooks ───────────────────────────────────────────────────────
	// Logger is an optional structured logger. If nil, all logging is suppressed.
	// See the Logger interface for how to wire in zap, logrus, slog, etc.
	Logger Logger
}

func (c Config) withDefaults() Config {
	if c.EvaluateTimeout == 0 {
		c.EvaluateTimeout = 5 * time.Second
	}
	if c.EndorseTimeout == 0 {
		c.EndorseTimeout = 15 * time.Second
	}
	if c.SubmitTimeout == 0 {
		c.SubmitTimeout = 5 * time.Second
	}
	if c.CommitStatusTimeout == 0 {
		c.CommitStatusTimeout = 60 * time.Second
	}
	if c.Logger == nil {
		c.Logger = noopLogger{}
	}
	return c
}

// ─────────────────────────────────────────────────────────────────────────────
// FabricClient
// ─────────────────────────────────────────────────────────────────────────────

// FabricClient is the top-level SDK handle. Create one with New, then call
// Evaluate for reads and Submit for writes. Safe to share across goroutines.
type FabricClient struct {
	cfg      Config
	conn     *grpc.ClientConn
	gw       *client.Gateway
	contract *client.Contract
	log      Logger
}

// New connects to the Fabric peer and returns a ready-to-use FabricClient.
// It opens one persistent gRPC connection and one Gateway session.
// Always call Close() when done — use defer:
//
//	fc, err := fabricsdk.New(cfg)
//	if err != nil { ... }
//	defer fc.Close()
func New(cfg Config) (*FabricClient, error) {
	cfg = cfg.withDefaults()

	cfg.Logger.Info("fabricsdk: connecting", "peer", cfg.PeerEndpoint, "mspID", cfg.MSPID)

	conn, err := newGrpcConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: gRPC connection failed: %w", err)
	}

	gw, err := newGateway(cfg, conn, cfg.CertPath, cfg.KeyPath)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	contract := resolveContract(gw, cfg.ChannelName, cfg.ChaincodeName, cfg.ContractName)

	cfg.Logger.Info("fabricsdk: connected", "channel", cfg.ChannelName, "chaincode", cfg.ChaincodeName)

	return &FabricClient{cfg: cfg, conn: conn, gw: gw, contract: contract, log: cfg.Logger}, nil
}

// Close releases the Gateway and the underlying gRPC connection.
// Always defer Close after a successful New call.
func (fc *FabricClient) Close() {
	if fc.gw != nil {
		fc.gw.Close()
	}
	if fc.conn != nil {
		_ = fc.conn.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Evaluate — read-only query
// ─────────────────────────────────────────────────────────────────────────────

// Evaluate calls a chaincode function as a read-only query.
// The context controls timeout and cancellation — pass context.Background()
// for no cancellation, or a context with deadline for automatic timeout.
// The transaction is sent to one peer for execution but is never submitted to
// the orderer and does not modify ledger state.
// Returns the raw bytes from the chaincode response.
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	raw, err := fc.Evaluate(ctx, "GetAsset", "asset-1")
//	if err != nil { ... }
//	var asset MyAsset
//	json.Unmarshal(raw, &asset)
func (fc *FabricClient) Evaluate(ctx context.Context, fn string, args ...string) ([]byte, error) {
	fc.log.Info("fabricsdk: evaluate", "fn", fn)
	result, err := fc.contract.EvaluateTransaction(fn, args...)
	if err != nil {
		classified := classifyError(err)
		fc.log.Error("fabricsdk: evaluate failed", "fn", fn, "err", classified)
		return nil, classified
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Submit — write transaction
// ─────────────────────────────────────────────────────────────────────────────

// Submit calls a chaincode function as a write transaction.
// The context controls timeout and cancellation for the entire lifecycle
// (endorse + submit + commit wait). Pass context.Background() for no timeout,
// or a context with deadline to cap the total wait.
// It endorses the proposal, submits to the orderer, and waits for the block
// to be committed before returning. This is equivalent to ethers.js
// contract.someWriteFn(...) followed by tx.wait().
// Returns the committed transaction ID on success.
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	txID, err := fc.Submit(ctx, "CreateAsset", string(jsonBytes))
func (fc *FabricClient) Submit(ctx context.Context, fn string, args ...string) (string, error) {
	fc.log.Info("fabricsdk: submit", "fn", fn)
	txID, err := submitOnContract(ctx, fc.contract, fn, args...)
	if err != nil {
		fc.log.Error("fabricsdk: submit failed", "fn", fn, "err", err)
		return "", err
	}
	fc.log.Info("fabricsdk: submit committed", "fn", fn, "txID", txID)
	return txID, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// WithIdentity — per-call identity override
// ─────────────────────────────────────────────────────────────────────────────

// WithIdentity returns a one-shot caller that signs transactions with the
// provided certificate and key instead of the default identity from Config.
// The underlying gRPC connection is reused — no new TCP/TLS handshake.
// A new Gateway is opened for the single call and closed immediately after.
//
// Use this when different org members need to invoke the same chaincode:
//
//	ctx := context.Background()
//	// Default user submits:
//	txID, err := fc.Submit(ctx, "CreateAsset", string(jsonBytes))
//
//	// Admin submits with their own identity:
//	txID, err = fc.WithIdentity("/admin/signcerts", "/admin/keystore").Submit(ctx, "DeleteAsset", id)
func (fc *FabricClient) WithIdentity(certPath, keyPath string) *CallerClient {
	return &CallerClient{parent: fc, certPath: certPath, keyPath: keyPath}
}

// CallerClient is a one-shot FabricClient view bound to a specific identity.
// Obtain one via FabricClient.WithIdentity — do not construct directly.
type CallerClient struct {
	parent   *FabricClient
	certPath string
	keyPath  string
}

// Evaluate runs a read-only query signed by the overridden identity.
func (cc *CallerClient) Evaluate(ctx context.Context, fn string, args ...string) ([]byte, error) {
	var result []byte
	err := cc.withContract(func(c *client.Contract) error {
		var txErr error
		result, txErr = c.EvaluateTransaction(fn, args...)
		if txErr != nil {
			return classifyError(txErr)
		}
		return nil
	})
	return result, err
}

// Submit sends a write transaction signed by the overridden identity.
func (cc *CallerClient) Submit(ctx context.Context, fn string, args ...string) (string, error) {
	var txID string
	err := cc.withContract(func(c *client.Contract) error {
		var submitErr error
		txID, submitErr = submitOnContract(ctx, c, fn, args...)
		return submitErr
	})
	return txID, err
}

// withContract opens a scoped Gateway for one operation and closes it after.
// defer gw.Close() guarantees cleanup even if fn panics.
func (cc *CallerClient) withContract(fn func(*client.Contract) error) error {
	gw, err := newGateway(cc.parent.cfg, cc.parent.conn, cc.certPath, cc.keyPath)
	if err != nil {
		return err
	}
	defer gw.Close()
	return fn(resolveContract(gw, cc.parent.cfg.ChannelName, cc.parent.cfg.ChaincodeName, cc.parent.cfg.ContractName))
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

func submitOnContract(ctx context.Context, c *client.Contract, fn string, args ...string) (string, error) {
	_, commit, err := c.SubmitAsync(fn, client.WithArguments(args...))
	if err != nil {
		return "", classifyError(err)
	}

	st, err := commit.Status()
	if err != nil {
		return "", wrapSDKError(500, "failed to get commit status", err)
	}
	if !st.Successful {
		return "", newSDKError(500, fmt.Sprintf(
			"transaction %s failed to commit with status code %d",
			st.TransactionID, int32(st.Code),
		))
	}
	return commit.TransactionID(), nil
}

func resolveContract(gw *client.Gateway, channel, chaincode, contractName string) *client.Contract {
	network := gw.GetNetwork(channel)
	if contractName != "" {
		return network.GetContractWithName(chaincode, contractName)
	}
	return network.GetContract(chaincode)
}

func newGrpcConnection(cfg Config) (*grpc.ClientConn, error) {
	certPEM, err := os.ReadFile(cfg.TLSCertPath)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: read TLS cert: %w", err)
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: parse TLS cert: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	conn, err := grpc.Dial(
		cfg.PeerEndpoint,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, cfg.PeerHostOverride)),
	)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: dial peer: %w", err)
	}
	return conn, nil
}

func newGateway(cfg Config, conn *grpc.ClientConn, certPath, keyPath string) (*client.Gateway, error) {
	id, err := loadIdentity(certPath, cfg.MSPID)
	if err != nil {
		return nil, err
	}
	sign, err := loadSigner(keyPath)
	if err != nil {
		return nil, err
	}
	gw, err := client.Connect(
		id,
		client.WithSign(sign),
		client.WithHash(hash.SHA256),
		client.WithClientConnection(conn),
		client.WithEvaluateTimeout(cfg.EvaluateTimeout),
		client.WithEndorseTimeout(cfg.EndorseTimeout),
		client.WithSubmitTimeout(cfg.SubmitTimeout),
		client.WithCommitStatusTimeout(cfg.CommitStatusTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: connect gateway: %w", err)
	}
	return gw, nil
}

func loadIdentity(certPath, mspID string) (*identity.X509Identity, error) {
	certPEM, err := readFirstFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: read signing cert: %w", err)
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: parse signing cert: %w", err)
	}
	id, err := identity.NewX509Identity(mspID, cert)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: create identity: %w", err)
	}
	return id, nil
}

func loadSigner(keyPath string) (identity.Sign, error) {
	keyPEM, err := readFirstFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: read private key: %w", err)
	}
	pk, err := identity.PrivateKeyFromPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: parse private key: %w", err)
	}
	sign, err := identity.NewPrivateKeySign(pk)
	if err != nil {
		return nil, fmt.Errorf("fabricsdk: create signer: %w", err)
	}
	return sign, nil
}

// readFirstFile reads the single file inside a Fabric MSP directory
// (signcerts or keystore). These directories conventionally hold one file.
func readFirstFile(dirPath string) ([]byte, error) {
	dir, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	names, err := dir.Readdirnames(1)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no files in directory: %s", dirPath)
	}
	return os.ReadFile(path.Join(dirPath, names[0]))
}
