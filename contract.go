package fabricsdk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Contract is a lightweight handle binding an identity to a channel,
// chaincode and contract. Handles are cheap values: create them per request.
type Contract struct {
	fc        *FabricClient
	id        *Identity
	channel   string
	chaincode string
	contract  string
}

// WithIdentity returns a copy of the handle that signs with id.
func (c *Contract) WithIdentity(id *Identity) *Contract {
	cp := *c
	cp.id = id
	return &cp
}

// WithTarget returns a copy of the handle aimed at another channel, chaincode
// or contract. contractName may be empty for the chaincode's default contract.
func (c *Contract) WithTarget(channel, chaincode, contractName string) *Contract {
	cp := *c
	cp.channel, cp.chaincode, cp.contract = channel, chaincode, contractName
	return &cp
}

// Channel returns the target channel name.
func (c *Contract) Channel() string { return c.channel }

// Chaincode returns the target chaincode name.
func (c *Contract) Chaincode() string { return c.chaincode }

// ContractName returns the target contract name ("" for the default).
func (c *Contract) ContractName() string { return c.contract }

// Identity returns the signing identity.
func (c *Contract) Identity() *Identity { return c.id }

// gateway returns a Gateway for the handle's identity and a release func.
func (c *Contract) gateway() (*client.Gateway, func(), error) {
	if c.fc.closed.Load() {
		return nil, nil, newError(KindConfig, SourceSDK, "client is closed")
	}
	if c.id == nil {
		return nil, nil, configError(errors.New("identity is nil"))
	}
	if c.id == c.fc.id {
		return c.fc.gw, func() {}, nil
	}
	gw, err := c.fc.connect(c.id)
	if err != nil {
		return nil, nil, err
	}
	return gw, func() { _ = gw.Close() }, nil
}

func (c *Contract) target(gw *client.Gateway) *client.Contract {
	network := gw.GetNetwork(c.channel)
	if c.contract != "" {
		return network.GetContractWithName(c.chaincode, c.contract)
	}
	return network.GetContract(c.chaincode)
}

func (c *Contract) observe(ctx context.Context, op, fn, txID string, start time.Time, err error) {
	mspID := ""
	if c.id != nil {
		mspID = c.id.MSPID()
	}
	c.fc.cfg.Observer.ObserveCall(ctx, CallInfo{
		Operation: op, Channel: c.channel, Chaincode: c.chaincode, Contract: c.contract,
		Function: fn, MSPID: mspID, TxID: txID, Duration: time.Since(start), Err: err,
	})
	if err != nil {
		c.fc.cfg.Logger.Error("fabricsdk: "+op+" failed", "fn", fn, "txID", txID, "kind", string(KindOf(err)))
	}
}

// Evaluate runs a read-only query. Nothing is written to the ledger.
func (c *Contract) Evaluate(ctx context.Context, fn string, args ...string) ([]byte, error) {
	return c.EvaluateWith(ctx, fn, Args(args...))
}

// EvaluateWith runs a read-only query with call options, for example
// transient data for a private-data read.
func (c *Contract) EvaluateWith(ctx context.Context, fn string, opts ...CallOption) (result []byte, err error) {
	start := time.Now()
	txID := ""
	defer func() { c.observe(ctx, OpEvaluate, fn, txID, start, err) }()

	gw, release, err := c.gateway()
	if err != nil {
		return nil, err
	}
	defer release()

	proposal, err := c.target(gw).NewProposal(fn, proposalOptions(opts)...)
	if err != nil {
		return nil, configError(err)
	}
	txID = proposal.TransactionID()

	ectx, cancel := withDefaultTimeout(ctx, c.fc.cfg.EvaluateTimeout)
	defer cancel()
	result, err = proposal.EvaluateWithContext(ectx)
	if err != nil {
		return nil, classify(err, txID, phaseEvaluate, c.fc.cfg.StrictErrorCodes)
	}
	return result, nil
}

// TxResult describes a committed, valid transaction.
type TxResult struct {
	TxID        string
	BlockNumber uint64
	// Payload is the value returned by the transaction function.
	Payload []byte
}

// Submit endorses a transaction, sends it to the orderer and waits until it
// is committed. It returns an error unless the transaction committed as valid.
func (c *Contract) Submit(ctx context.Context, fn string, args ...string) (*TxResult, error) {
	return c.SubmitWith(ctx, fn, Args(args...))
}

// SubmitWith is Submit with call options.
func (c *Contract) SubmitWith(ctx context.Context, fn string, opts ...CallOption) (*TxResult, error) {
	pending, err := c.SubmitAsync(ctx, fn, opts...)
	if err != nil {
		return nil, err
	}
	return pending.Wait(ctx)
}

// PendingTx is a transaction accepted by the orderer whose commit has not yet
// been confirmed. Store TxID durably (for example in an outbox row) before
// calling Wait, so a crash cannot lose track of it.
type PendingTx struct {
	TxID string
	// Payload is the endorsed result of the transaction function. It only
	// becomes authoritative if the transaction commits as valid.
	Payload []byte

	c       *Contract
	fn      string
	commit  *client.Commit
	release func()
	once    sync.Once
	mu      sync.Mutex
}

// SubmitAsync endorses the transaction and sends it to the orderer, then
// returns without waiting for the commit. Call Wait on the result.
func (c *Contract) SubmitAsync(ctx context.Context, fn string, opts ...CallOption) (pending *PendingTx, err error) {
	gw, release, err := c.gateway()
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()

	proposal, err := c.target(gw).NewProposal(fn, proposalOptions(opts)...)
	if err != nil {
		return nil, configError(err)
	}
	txID := proposal.TransactionID()

	start := time.Now()
	ectx, cancel := withDefaultTimeout(ctx, c.fc.cfg.EndorseTimeout)
	tx, err := proposal.EndorseWithContext(ectx)
	cancel()
	if err != nil {
		err = classify(err, txID, phaseEndorse, c.fc.cfg.StrictErrorCodes)
		c.observe(ctx, OpEndorse, fn, txID, start, err)
		return nil, err
	}
	c.observe(ctx, OpEndorse, fn, txID, start, nil)

	start = time.Now()
	sctx, cancel := withDefaultTimeout(ctx, c.fc.cfg.SubmitTimeout)
	commit, err := tx.SubmitWithContext(sctx)
	cancel()
	if err != nil {
		err = classify(err, txID, phaseSubmit, c.fc.cfg.StrictErrorCodes)
		c.observe(ctx, OpSubmit, fn, txID, start, err)
		return nil, err
	}
	c.observe(ctx, OpSubmit, fn, txID, start, nil)

	keep = true
	return &PendingTx{TxID: txID, Payload: tx.Result(), c: c, fn: fn, commit: commit, release: release}, nil
}

// Wait blocks until the transaction is committed, ctx ends, or the commit
// status timeout passes. Errors other than a definite commit failure carry
// OutcomeUnknown and the TxID. Wait may be called again after a timeout.
func (p *PendingTx) Wait(ctx context.Context) (res *TxResult, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start := time.Now()
	defer func() { p.c.observe(ctx, OpCommit, p.fn, p.TxID, start, err) }()

	cctx, cancel := withDefaultTimeout(ctx, p.c.fc.cfg.CommitStatusTimeout)
	defer cancel()
	st, err := p.commit.StatusWithContext(cctx)
	if err != nil {
		return nil, classify(err, p.TxID, phaseCommitStatus, true)
	}
	p.once.Do(p.release)
	if !st.Successful {
		return nil, commitFailure(p.TxID, st.Code, nil)
	}
	p.c.fc.cfg.Logger.Info("fabricsdk: committed", "fn", p.fn, "txID", p.TxID, "block", st.BlockNumber)
	return &TxResult{TxID: p.TxID, BlockNumber: st.BlockNumber, Payload: p.Payload}, nil
}

// Release frees resources held by a PendingTx that will not be waited on.
// Wait releases them automatically once the status is known.
func (p *PendingTx) Release() { p.once.Do(p.release) }

// ── ledger queries via the qscc system chaincode ─────────────────────────────

// TxStatus is the ledger state of a transaction ID on one peer.
type TxStatus struct {
	TxID string
	// Found is false if the Gateway peer has no record of the transaction:
	// it was never ordered, or the peer has not committed that block yet.
	Found          bool
	ValidationCode peer.TxValidationCode
	// Valid is true if the transaction committed with code VALID.
	Valid bool
}

// noSuchTx is the ledger error text for an unknown transaction ID
// (Fabric v2.5 common/ledger/blkstorage/blockindex.go).
const noSuchTx = "no such transaction ID"

// TxStatus looks up txID with qscc GetTransactionByID on the handle's
// channel. Use it to resolve OutcomeUnknown errors. The caller's identity
// needs the qscc/GetTransactionByID ACL (channel Readers by default).
func (c *Contract) TxStatus(ctx context.Context, txID string) (ts *TxStatus, err error) {
	start := time.Now()
	defer func() { c.observe(ctx, OpTxStatus, "GetTransactionByID", txID, start, err) }()

	raw, err := c.qscc(ctx, "GetTransactionByID", c.channel, txID)
	if err != nil {
		if mentions(err, noSuchTx) {
			return &TxStatus{TxID: txID, Found: false}, nil
		}
		return nil, classify(err, "", phaseEvaluate, true)
	}
	var pt peer.ProcessedTransaction
	if err := proto.Unmarshal(raw, &pt); err != nil {
		return nil, &Error{Kind: KindInternal, Source: SourceSDK, Code: 500, Message: "decode ProcessedTransaction", Cause: err}
	}
	vc := peer.TxValidationCode(pt.GetValidationCode())
	return &TxStatus{TxID: txID, Found: true, ValidationCode: vc, Valid: vc == peer.TxValidationCode_VALID}, nil
}

// Height returns the block height of the handle's channel on the Gateway peer
// (qscc GetChainInfo). Handy for health checks and lag monitoring.
func (c *Contract) Height(ctx context.Context) (uint64, error) {
	raw, err := c.qscc(ctx, "GetChainInfo", c.channel)
	if err != nil {
		return 0, classify(err, "", phaseEvaluate, true)
	}
	var info common.BlockchainInfo
	if err := proto.Unmarshal(raw, &info); err != nil {
		return 0, &Error{Kind: KindInternal, Source: SourceSDK, Code: 500, Message: "decode BlockchainInfo", Cause: err}
	}
	return info.GetHeight(), nil
}

func (c *Contract) qscc(ctx context.Context, fn string, args ...string) ([]byte, error) {
	gw, release, err := c.gateway()
	if err != nil {
		return nil, err
	}
	defer release()
	proposal, err := gw.GetNetwork(c.channel).GetContract("qscc").NewProposal(fn, client.WithArguments(args...))
	if err != nil {
		return nil, configError(err)
	}
	ectx, cancel := withDefaultTimeout(ctx, c.fc.cfg.EvaluateTimeout)
	defer cancel()
	return proposal.EvaluateWithContext(ectx)
}

// mentions reports whether text appears in the error or its Gateway details.
func mentions(err error, text string) bool {
	st := status.Convert(err)
	if strings.Contains(st.Message(), text) {
		return true
	}
	for _, d := range gatewayDetails(st) {
		if strings.Contains(d.Message, text) {
			return true
		}
	}
	return strings.Contains(err.Error(), text)
}
