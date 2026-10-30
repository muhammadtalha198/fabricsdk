// Package fabricsdk is a small, ethers.js-style Go client for Hyperledger
// Fabric v2.4+ built on the official Fabric Gateway client
// (github.com/hyperledger/fabric-gateway).
//
// Call Evaluate for reads and Submit for writes; the SDK handles TLS, gRPC,
// identity, endorsement, ordering and commit waiting:
//
//	fc, err := fabricsdk.New(cfg)
//	if err != nil { ... }
//	defer fc.Close()
//
//	raw, err := fc.Evaluate(ctx, "ReadAsset", "asset1")
//	res, err := fc.Submit(ctx, "CreateAsset", "asset2", "blue")
//	fmt.Println(res.TxID, res.BlockNumber)
//
// Everything honours the context: a deadline or cancellation on ctx bounds the
// call. When ctx has no deadline, each phase uses its Config timeout.
//
// For private data, pass inputs as transient data and, when needed, choose the
// endorsing organizations:
//
//	res, err := fc.SubmitWith(ctx, "CreateChallan",
//	    fabricsdk.Args(challanID),
//	    fabricsdk.Transient(map[string][]byte{"owner": ownerJSON}),
//	)
//
// To act as another user (for example one Fabric identity per officer), load
// the Identity once and reuse it; every call shares the client's single gRPC
// connection:
//
//	officer, err := fabricsdk.LoadIdentity("PoliceMSP", certFile, keyFile)
//	res, err := fc.WithIdentity(officer).Submit(ctx, "IssueChallan", id)
//
// Errors are *Error values with a Kind. A write that fails after reaching the
// orderer is reported with OutcomeUnknown and its TxID; resolve it with
// TxStatus instead of resubmitting.
package fabricsdk

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/hash"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// FabricClient owns one gRPC connection to a Gateway peer and a default
// identity and chaincode target. It is safe for concurrent use; create one
// per peer endpoint and share it.
type FabricClient struct {
	cfg       Config
	conn      grpc.ClientConnInterface
	ownedConn *grpc.ClientConn
	id        *Identity
	ownsID    bool
	gw        *client.Gateway
	tlsHash   []byte
	closed    atomic.Bool
	closeOnce sync.Once
}

// New dials the peer described by cfg and returns a ready client. The gRPC
// connection is established lazily, on the first call.
func New(cfg Config) (*FabricClient, error) {
	if err := cfg.validate(true); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	creds, tlsHash, err := transportCredentials(cfg)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if cfg.KeepAlive != nil {
		opts = append(opts, grpc.WithKeepaliveParams(*cfg.KeepAlive))
	}
	opts = append(opts, cfg.DialOptions...)

	conn, err := grpc.NewClient(grpcTarget(cfg.PeerEndpoint), opts...)
	if err != nil {
		return nil, configError(fmt.Errorf("create gRPC client: %w", err))
	}
	fc, err := build(cfg, conn, tlsHash)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	fc.ownedConn = conn
	cfg.Logger.Info("fabricsdk: client ready", "peer", cfg.PeerEndpoint, "mspID", fc.id.MSPID(),
		"channel", cfg.ChannelName, "chaincode", cfg.ChaincodeName)
	return fc, nil
}

// NewWithConnection builds a client on an existing gRPC connection, for
// example one shared with other code or an in-process test server. The
// connection is not closed by Close. Network fields in cfg are ignored.
func NewWithConnection(conn grpc.ClientConnInterface, cfg Config) (*FabricClient, error) {
	if conn == nil {
		return nil, configError(errors.New("connection is nil"))
	}
	if err := cfg.validate(false); err != nil {
		return nil, err
	}
	return build(cfg.withDefaults(), conn, nil)
}

func build(cfg Config, conn grpc.ClientConnInterface, tlsHash []byte) (*FabricClient, error) {
	fc := &FabricClient{cfg: cfg, conn: conn, tlsHash: tlsHash}
	fc.id = cfg.Identity
	if fc.id == nil {
		id, err := LoadIdentity(cfg.MSPID, cfg.CertPath, cfg.KeyPath)
		if err != nil {
			return nil, err
		}
		fc.id, fc.ownsID = id, true
	}
	gw, err := fc.connect(fc.id)
	if err != nil {
		if fc.ownsID {
			_ = fc.id.Close()
		}
		return nil, err
	}
	fc.gw = gw
	return fc, nil
}

func transportCredentials(cfg Config) (credentials.TransportCredentials, []byte, error) {
	caPEM, err := os.ReadFile(cfg.TLSCertPath)
	if err != nil {
		return nil, nil, configError(fmt.Errorf("read TLS CA certificate: %w", err))
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, configError(fmt.Errorf("no PEM certificates found in %s", cfg.TLSCertPath))
	}
	tlsCfg := &tls.Config{
		RootCAs:    pool,
		ServerName: serverNameFor(cfg),
		MinVersion: tls.VersionTLS12,
	}
	var tlsHash []byte
	if cfg.ClientTLSCertPath != "" {
		pair, err := tls.LoadX509KeyPair(cfg.ClientTLSCertPath, cfg.ClientTLSKeyPath)
		if err != nil {
			return nil, nil, configError(fmt.Errorf("load client TLS key pair: %w", err))
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
		sum := sha256.Sum256(pair.Certificate[0])
		tlsHash = sum[:]
	}
	return credentials.NewTLS(tlsCfg), tlsHash, nil
}

// connect creates a Gateway for id on the shared connection. This is cheap:
// no network round trip happens until a call is made.
func (fc *FabricClient) connect(id *Identity) (*client.Gateway, error) {
	opts := []client.ConnectOption{
		client.WithSign(id.sign),
		client.WithHash(hash.SHA256),
		client.WithClientConnection(fc.conn),
		client.WithEvaluateTimeout(fc.cfg.EvaluateTimeout),
		client.WithEndorseTimeout(fc.cfg.EndorseTimeout),
		client.WithSubmitTimeout(fc.cfg.SubmitTimeout),
		client.WithCommitStatusTimeout(fc.cfg.CommitStatusTimeout),
	}
	if len(fc.tlsHash) > 0 {
		opts = append(opts, client.WithTLSClientCertificateHash(fc.tlsHash))
	}
	gw, err := client.Connect(id.x509, opts...)
	if err != nil {
		return nil, configError(fmt.Errorf("connect gateway: %w", err))
	}
	return gw, nil
}

// Close releases the default Gateway, the gRPC connection (if New created it)
// and the default identity (if the client loaded it). Safe to call twice.
func (fc *FabricClient) Close() error {
	var errs []error
	fc.closeOnce.Do(func() {
		fc.closed.Store(true)
		if fc.gw != nil {
			errs = append(errs, fc.gw.Close())
		}
		if fc.ownedConn != nil {
			errs = append(errs, fc.ownedConn.Close())
		}
		if fc.ownsID {
			errs = append(errs, fc.id.Close())
		}
	})
	return errors.Join(errs...)
}

// Identity returns the client's default identity.
func (fc *FabricClient) Identity() *Identity { return fc.id }

// Contract returns a handle for the default identity and target.
func (fc *FabricClient) Contract() *Contract {
	return &Contract{fc: fc, id: fc.id, channel: fc.cfg.ChannelName, chaincode: fc.cfg.ChaincodeName, contract: fc.cfg.ContractName}
}

// Target returns a handle for another channel, chaincode or contract, using
// the default identity. contractName may be empty for the default contract.
func (fc *FabricClient) Target(channel, chaincode, contractName string) *Contract {
	return fc.Contract().WithTarget(channel, chaincode, contractName)
}

// WithIdentity returns a handle that signs with id on the default target.
func (fc *FabricClient) WithIdentity(id *Identity) *Contract {
	return fc.Contract().WithIdentity(id)
}

// Evaluate runs a read-only query with the default identity and target.
func (fc *FabricClient) Evaluate(ctx context.Context, fn string, args ...string) ([]byte, error) {
	return fc.Contract().Evaluate(ctx, fn, args...)
}

// EvaluateWith runs a read-only query with call options.
func (fc *FabricClient) EvaluateWith(ctx context.Context, fn string, opts ...CallOption) ([]byte, error) {
	return fc.Contract().EvaluateWith(ctx, fn, opts...)
}

// Submit endorses, orders and waits for the commit of a transaction.
func (fc *FabricClient) Submit(ctx context.Context, fn string, args ...string) (*TxResult, error) {
	return fc.Contract().Submit(ctx, fn, args...)
}

// SubmitWith is Submit with call options (transient data, endorsing orgs).
func (fc *FabricClient) SubmitWith(ctx context.Context, fn string, opts ...CallOption) (*TxResult, error) {
	return fc.Contract().SubmitWith(ctx, fn, opts...)
}

// SubmitAsync endorses and orders a transaction and returns without waiting
// for the commit.
func (fc *FabricClient) SubmitAsync(ctx context.Context, fn string, opts ...CallOption) (*PendingTx, error) {
	return fc.Contract().SubmitAsync(ctx, fn, opts...)
}

// TxStatus looks up a transaction on the default channel.
func (fc *FabricClient) TxStatus(ctx context.Context, txID string) (*TxStatus, error) {
	return fc.Contract().TxStatus(ctx, txID)
}

// Height returns the block height of the default channel on the Gateway peer.
func (fc *FabricClient) Height(ctx context.Context) (uint64, error) {
	return fc.Contract().Height(ctx)
}

// ── call options ─────────────────────────────────────────────────────────────

// CallOption customises one Evaluate or Submit call.
type CallOption func(*callOptions)

type callOptions struct {
	args      [][]byte
	transient map[string][]byte
	orgs      []string
}

// Args appends string arguments.
func Args(args ...string) CallOption {
	return func(o *callOptions) {
		for _, a := range args {
			o.args = append(o.args, []byte(a))
		}
	}
}

// BytesArgs appends raw byte arguments.
func BytesArgs(args ...[]byte) CallOption {
	return func(o *callOptions) { o.args = append(o.args, args...) }
}

// Transient sets transient data: inputs that reach the endorsing peers but
// are not written to the block. Use it for private data and personal data.
func Transient(data map[string][]byte) CallOption {
	return func(o *callOptions) { o.transient = data }
}

// EndorsingOrgs restricts endorsement to the given MSP IDs. Only needed when
// the Gateway's own org cannot endorse a proposal carrying transient data, or
// for blind writes to a private collection. Otherwise let the Gateway choose.
func EndorsingOrgs(mspIDs ...string) CallOption {
	return func(o *callOptions) { o.orgs = append(o.orgs, mspIDs...) }
}

func proposalOptions(opts []CallOption) []client.ProposalOption {
	var o callOptions
	for _, opt := range opts {
		opt(&o)
	}
	out := []client.ProposalOption{client.WithBytesArguments(o.args...)}
	if o.transient != nil {
		out = append(out, client.WithTransient(o.transient))
	}
	if len(o.orgs) > 0 {
		out = append(out, client.WithEndorsingOrganizations(o.orgs...))
	}
	return out
}

// withDefaultTimeout bounds ctx by d unless ctx already has a deadline.
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok || d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
