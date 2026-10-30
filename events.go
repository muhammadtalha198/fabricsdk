package fabricsdk

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
)

// ChaincodeEvent is an event emitted with SetEvent by a committed, valid
// transaction.
type ChaincodeEvent struct {
	BlockNumber   uint64
	TxID          string
	ChaincodeName string
	EventName     string
	Payload       []byte
}

// ── checkpointing ────────────────────────────────────────────────────────────

// Checkpointer records how far event processing has got, so a listener can
// resume after a restart without gaps. Implement it on your database (store
// the position in the same transaction as the data you derive from events)
// for exactly-once processing; the built-in ones give at-least-once.
type Checkpointer interface {
	// BlockNumber is the block in which the next event is expected.
	BlockNumber() uint64
	// TransactionID is the last processed transaction within that block, or "".
	TransactionID() string
	// CheckpointBlock records that every event in blockNumber was processed.
	CheckpointBlock(blockNumber uint64) error
	// CheckpointTransaction records that transactionID in blockNumber was processed.
	CheckpointTransaction(blockNumber uint64, transactionID string) error
}

type memoryCheckpointer struct{ cp client.InMemoryCheckpointer }

func (m *memoryCheckpointer) BlockNumber() uint64   { return m.cp.BlockNumber() }
func (m *memoryCheckpointer) TransactionID() string { return m.cp.TransactionID() }
func (m *memoryCheckpointer) CheckpointBlock(n uint64) error {
	m.cp.CheckpointBlock(n)
	return nil
}
func (m *memoryCheckpointer) CheckpointTransaction(n uint64, txID string) error {
	m.cp.CheckpointTransaction(n, txID)
	return nil
}

// NewMemoryCheckpointer returns a Checkpointer that lives only in memory. It
// lets a listener resume after a stream failure, but not after a restart.
func NewMemoryCheckpointer() Checkpointer { return &memoryCheckpointer{} }

// FileCheckpointer persists the position to a JSON file. Close it when done.
type FileCheckpointer struct{ *client.FileCheckpointer }

// NewFileCheckpointer opens (or creates) a checkpoint file.
func NewFileCheckpointer(path string) (*FileCheckpointer, error) {
	cp, err := client.NewFileCheckpointer(path)
	if err != nil {
		return nil, configError(err)
	}
	return &FileCheckpointer{cp}, nil
}

// ── options ──────────────────────────────────────────────────────────────────

// EventOption configures an event stream or listener.
type EventOption func(*eventOptions)

type eventOptions struct {
	startBlock *uint64
	checkpoint Checkpointer
	chaincode  string
	minBackoff time.Duration
	maxBackoff time.Duration
}

// FromBlock starts at blockNumber when no checkpoint position is set. Without
// it (and without a checkpoint) a stream starts at the next committed block.
// FromBlock(0) replays from the start of the ledger.
func FromBlock(blockNumber uint64) EventOption {
	return func(o *eventOptions) { o.startBlock = &blockNumber }
}

// WithCheckpointer resumes from, and is used by listeners to record, the
// processing position.
func WithCheckpointer(cp Checkpointer) EventOption {
	return func(o *eventOptions) { o.checkpoint = cp }
}

// ForChaincode listens to another chaincode on the handle's channel.
func ForChaincode(name string) EventOption {
	return func(o *eventOptions) { o.chaincode = name }
}

// RetryBackoff sets the reconnect delay range used by listeners. Defaults: 1s to 30s.
func RetryBackoff(min, max time.Duration) EventOption {
	return func(o *eventOptions) { o.minBackoff, o.maxBackoff = min, max }
}

func (c *Contract) eventOptions(opts []EventOption) eventOptions {
	o := eventOptions{chaincode: c.chaincode, minBackoff: time.Second, maxBackoff: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	if o.minBackoff <= 0 {
		o.minBackoff = time.Second
	}
	if o.maxBackoff < o.minBackoff {
		o.maxBackoff = o.minBackoff
	}
	return o
}

// ── single stream ────────────────────────────────────────────────────────────

// ErrStreamClosed is the cause reported by EventStream.Err when the Gateway
// ended the stream while the caller's context was still active (for example
// because the peer restarted). Open a new stream from a checkpoint.
var ErrStreamClosed = errors.New("event stream closed by the gateway")

// EventStream is one chaincode event subscription.
type EventStream struct {
	ch   chan ChaincodeEvent
	mu   sync.Mutex
	err  error
	stop context.CancelFunc
}

// Events returns the event channel. It is closed when the stream ends.
func (s *EventStream) Events() <-chan ChaincodeEvent { return s.ch }

// Err reports why the stream ended: nil if the caller's context ended, an
// *Error wrapping ErrStreamClosed otherwise. Valid after Events is closed.
func (s *EventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close stops the stream.
func (s *EventStream) Close() { s.stop() }

// ChaincodeEvents opens one subscription to the handle's chaincode. It does
// not reconnect; use ListenChaincodeEvents for a long-running consumer.
func (c *Contract) ChaincodeEvents(ctx context.Context, opts ...EventOption) (*EventStream, error) {
	o := c.eventOptions(opts)
	gw, release, err := c.gateway()
	if err != nil {
		return nil, err
	}
	sctx, stop := context.WithCancel(ctx)

	var gopts []client.ChaincodeEventsOption
	if o.startBlock != nil {
		gopts = append(gopts, client.WithStartBlock(*o.startBlock))
	}
	if o.checkpoint != nil {
		gopts = append(gopts, client.WithCheckpoint(o.checkpoint))
	}
	upstream, err := gw.GetNetwork(c.channel).ChaincodeEvents(sctx, o.chaincode, gopts...)
	if err != nil {
		stop()
		release()
		return nil, classify(err, "", phaseEvaluate, true)
	}

	s := &EventStream{ch: make(chan ChaincodeEvent), stop: stop}
	go func() {
		defer release()
		defer close(s.ch)
		for ev := range upstream {
			out := ChaincodeEvent{
				BlockNumber: ev.BlockNumber, TxID: ev.TransactionID,
				ChaincodeName: ev.ChaincodeName, EventName: ev.EventName, Payload: ev.Payload,
			}
			select {
			case s.ch <- out:
			case <-sctx.Done():
				return
			}
		}
		if ctx.Err() == nil && sctx.Err() == nil {
			s.mu.Lock()
			s.err = &Error{Kind: KindUnavailable, Source: SourceNetwork, Code: 503, Message: ErrStreamClosed.Error(), Cause: ErrStreamClosed}
			s.mu.Unlock()
		}
		stop()
	}()
	return s, nil
}

// Events opens a chaincode event stream for the default target.
func (fc *FabricClient) Events(ctx context.Context, opts ...EventOption) (*EventStream, error) {
	return fc.Contract().ChaincodeEvents(ctx, opts...)
}

// ── resilient listeners ──────────────────────────────────────────────────────

// EventHandler processes one chaincode event. Returning an error stops the
// listener without checkpointing that event.
type EventHandler func(ctx context.Context, ev ChaincodeEvent) error

// ListenChaincodeEvents delivers events to handle until ctx ends or handle
// fails. It checkpoints after each handled event and reconnects with backoff
// from the checkpoint when the stream drops, so no event is skipped. An event
// may be delivered again after a crash between handle and the checkpoint, so
// handle must be idempotent unless cp commits with your data.
//
// Returns nil when ctx ends, the handler's error, or a non-retryable error.
func (c *Contract) ListenChaincodeEvents(ctx context.Context, cp Checkpointer, handle EventHandler, opts ...EventOption) error {
	if cp == nil || handle == nil {
		return configError(errors.New("ListenChaincodeEvents needs a checkpointer and a handler"))
	}
	o := c.eventOptions(opts)
	opts = append(opts, WithCheckpointer(cp))
	backoff := o.minBackoff
	for {
		stream, err := c.ChaincodeEvents(ctx, opts...)
		if err == nil {
			for ev := range stream.Events() {
				if herr := handle(ctx, ev); herr != nil {
					stream.Close()
					return herr
				}
				if cerr := cp.CheckpointTransaction(ev.BlockNumber, ev.TxID); cerr != nil {
					stream.Close()
					return cerr
				}
				backoff = o.minBackoff
			}
			err = stream.Err()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !reconnectable(err) {
			return err
		}
		c.fc.cfg.Logger.Error("fabricsdk: chaincode event stream ended; reconnecting",
			"chaincode", o.chaincode, "block", cp.BlockNumber(), "retryIn", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, o.maxBackoff)
	}
}

// ListenChaincodeEvents listens on the default target. See Contract.ListenChaincodeEvents.
func (fc *FabricClient) ListenChaincodeEvents(ctx context.Context, cp Checkpointer, handle EventHandler, opts ...EventOption) error {
	return fc.Contract().ListenChaincodeEvents(ctx, cp, handle, opts...)
}

// BlockHandler processes one committed block. Returning an error stops the
// listener without checkpointing that block.
type BlockHandler func(ctx context.Context, block *common.Block) error

// ListenBlocks delivers every committed block on the handle's channel to
// handle, checkpointing after each block and reconnecting like
// ListenChaincodeEvents. Blocks contain all transactions, including invalid
// ones; check the TRANSACTIONS_FILTER metadata. Private data values are not
// included. Needs the channel's Deliver permission (Readers by default).
func (c *Contract) ListenBlocks(ctx context.Context, cp Checkpointer, handle BlockHandler, opts ...EventOption) error {
	if cp == nil || handle == nil {
		return configError(errors.New("ListenBlocks needs a checkpointer and a handler"))
	}
	o := c.eventOptions(opts)
	backoff := o.minBackoff
	for {
		err := c.blockSession(ctx, cp, handle, o, &backoff)
		if ctx.Err() != nil {
			return nil
		}
		var herr handlerError
		if errors.As(err, &herr) {
			return herr.err
		}
		if err != nil && !reconnectable(err) {
			return err
		}
		c.fc.cfg.Logger.Error("fabricsdk: block stream ended; reconnecting", "block", cp.BlockNumber(), "retryIn", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, o.maxBackoff)
	}
}

// ListenBlocks listens on the default channel. See Contract.ListenBlocks.
func (fc *FabricClient) ListenBlocks(ctx context.Context, cp Checkpointer, handle BlockHandler, opts ...EventOption) error {
	return fc.Contract().ListenBlocks(ctx, cp, handle, opts...)
}

type handlerError struct{ err error }

func (h handlerError) Error() string { return h.err.Error() }

func (c *Contract) blockSession(ctx context.Context, cp Checkpointer, handle BlockHandler, o eventOptions, backoff *time.Duration) error {
	gw, release, err := c.gateway()
	if err != nil {
		return err
	}
	defer release()
	sctx, stop := context.WithCancel(ctx)
	defer stop()

	var gopts []client.BlockEventsOption
	if o.startBlock != nil {
		gopts = append(gopts, client.WithStartBlock(*o.startBlock))
	}
	gopts = append(gopts, client.WithCheckpoint(cp))
	blocks, err := gw.GetNetwork(c.channel).BlockEvents(sctx, gopts...)
	if err != nil {
		return classify(err, "", phaseEvaluate, true)
	}
	for block := range blocks {
		if herr := handle(ctx, block); herr != nil {
			return handlerError{herr}
		}
		if cerr := cp.CheckpointBlock(block.GetHeader().GetNumber()); cerr != nil {
			return handlerError{cerr}
		}
		*backoff = o.minBackoff
	}
	return nil
}

// reconnectable reports whether a stream error is worth retrying.
func reconnectable(err error) bool {
	switch KindOf(err) {
	case KindConfig, KindUnauthorized, KindInvalidArgument, KindNotFound:
		return false
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
