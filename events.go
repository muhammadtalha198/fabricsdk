package fabricsdk

import (
	"context"
	"fmt"

	"github.com/hyperledger/fabric-gateway/pkg/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// Event listening
// ─────────────────────────────────────────────────────────────────────────────

// ChaincodeEvent represents a single event emitted by a chaincode transaction.
type ChaincodeEvent struct {
	// TxID is the transaction ID that emitted this event.
	TxID string

	// EventName is the name passed to stub.SetEvent() in the chaincode.
	EventName string

	// Payload is the raw bytes passed to stub.SetEvent() in the chaincode.
	Payload []byte

	// BlockNumber is the ledger block that committed the transaction.
	BlockNumber uint64
}

// EventsOptions configures how the event stream is started.
type EventsOptions struct {
	// StartBlock, if non-zero, replays events from this block number forward.
	// Useful for catching up after a restart without missing events.
	// If zero, only new events (from the current block) are delivered.
	StartBlock uint64
}

// Events returns a channel of chaincode events emitted by the configured chaincode.
// The channel is closed when ctx is cancelled. The function does not block —
// events arrive asynchronously on the returned channel.
//
// Basic usage — listen to new events only:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//
//	events, err := fc.Events(ctx)
//	if err != nil { ... }
//
//	for ev := range events {
//	    fmt.Printf("event=%s txID=%s payload=%s\n", ev.EventName, ev.TxID, ev.Payload)
//	}
//
// Replay from a known block (useful on restart):
//
//	events, err := fc.EventsFrom(ctx, 42)
func (fc *FabricClient) Events(ctx context.Context) (<-chan ChaincodeEvent, error) {
	return fc.EventsFrom(ctx, 0)
}

// EventsFrom is like Events but starts delivering events from startBlock onward.
// Pass 0 to receive only new events.
func (fc *FabricClient) EventsFrom(ctx context.Context, startBlock uint64) (<-chan ChaincodeEvent, error) {
	network := fc.gw.GetNetwork(fc.cfg.ChannelName)

	var rawEvents <-chan *client.ChaincodeEvent
	var err error

	if startBlock > 0 {
		rawEvents, err = network.ChaincodeEvents(ctx, fc.cfg.ChaincodeName,
			client.WithStartBlock(startBlock))
	} else {
		rawEvents, err = network.ChaincodeEvents(ctx, fc.cfg.ChaincodeName)
	}

	if err != nil {
		return nil, fmt.Errorf("fabricsdk: start event stream: %w", err)
	}

	out := make(chan ChaincodeEvent, 16)

	go func() {
		defer close(out)
		for ev := range rawEvents {
			out <- ChaincodeEvent{
				TxID:        ev.TransactionID,
				EventName:   ev.EventName,
				Payload:     ev.Payload,
				BlockNumber: ev.BlockNumber,
			}
		}
	}()

	return out, nil
}
