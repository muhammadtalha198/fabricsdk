package fabricsdk_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/gateway"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/muhammadtalha198/fabricsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ccEvent struct {
	block uint64
	tx    string
}

// eventLog serves chaincode events from a fixed list, honouring the start
// block and AfterTransactionId sent by the client, like a real peer. The
// first connection is cut after `cutAfter` events to simulate a peer restart.
type eventLog struct {
	events   []ccEvent
	cutAfter int

	mu       sync.Mutex
	requests []*gateway.ChaincodeEventsRequest
}

func (l *eventLog) serve(req *gateway.ChaincodeEventsRequest, send func(*gateway.ChaincodeEventsResponse) error) error {
	l.mu.Lock()
	l.requests = append(l.requests, req)
	first := len(l.requests) == 1
	l.mu.Unlock()

	var start uint64
	if s := req.GetStartPosition().GetSpecified(); s != nil {
		start = s.GetNumber()
	}
	skipping := req.GetAfterTransactionId() != ""
	sent := 0
	for _, e := range l.events {
		if e.block < start {
			continue
		}
		if skipping {
			if e.block == start && e.tx != req.GetAfterTransactionId() {
				continue
			}
			if e.block == start && e.tx == req.GetAfterTransactionId() {
				skipping = false
				continue
			}
			skipping = false
		}
		if first && sent == l.cutAfter {
			return status.Error(codes.Unavailable, "peer restarting")
		}
		err := send(&gateway.ChaincodeEventsResponse{
			BlockNumber: e.block,
			Events:      []*peer.ChaincodeEvent{{ChaincodeId: req.GetChaincodeId(), TxId: e.tx, EventName: "ChallanIssued", Payload: []byte(e.tx)}},
		})
		if err != nil {
			return err
		}
		sent++
	}
	time.Sleep(time.Hour) // keep the stream open like a live peer
	return nil
}

func TestListenChaincodeEventsResumesFromCheckpointWithoutGapsOrDuplicates(t *testing.T) {
	log := &eventLog{
		events:   []ccEvent{{5, "A"}, {5, "B"}, {5, "C"}, {6, "D"}},
		cutAfter: 2,
	}
	fake := &fakeGateway{events: log.serve}
	fc := newTestClient(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	cp := fabricsdk.NewMemoryCheckpointer()
	err := fc.ListenChaincodeEvents(ctx, cp, func(_ context.Context, ev fabricsdk.ChaincodeEvent) error {
		got = append(got, ev.TxID)
		if ev.TxID == "D" {
			cancel()
		}
		return nil
	}, fabricsdk.FromBlock(5), fabricsdk.RetryBackoff(10*time.Millisecond, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "B", "C", "D"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.requests) < 2 {
		t.Fatalf("expected a reconnect, saw %d requests", len(log.requests))
	}
	second := log.requests[1]
	if second.GetStartPosition().GetSpecified().GetNumber() != 5 || second.GetAfterTransactionId() != "B" {
		t.Fatalf("reconnect did not resume from checkpoint: %v", second)
	}
	if cp.BlockNumber() != 6 || cp.TransactionID() != "D" {
		t.Fatalf("checkpoint = %d/%s", cp.BlockNumber(), cp.TransactionID())
	}
}

func TestListenChaincodeEventsStopsOnHandlerErrorWithoutCheckpointing(t *testing.T) {
	log := &eventLog{events: []ccEvent{{1, "A"}, {1, "B"}}, cutAfter: 100}
	fc := newTestClient(t, &fakeGateway{events: log.serve})
	boom := errors.New("db down")
	cp := fabricsdk.NewMemoryCheckpointer()
	err := fc.ListenChaincodeEvents(context.Background(), cp, func(_ context.Context, ev fabricsdk.ChaincodeEvent) error {
		if ev.TxID == "B" {
			return boom
		}
		return nil
	}, fabricsdk.FromBlock(1))
	if !errors.Is(err, boom) {
		t.Fatalf("want handler error, got %v", err)
	}
	if cp.TransactionID() != "A" {
		t.Fatalf("checkpoint must stop at the last handled event, got %q", cp.TransactionID())
	}
}

func TestEventStreamReportsUnexpectedClose(t *testing.T) {
	fake := &fakeGateway{events: func(*gateway.ChaincodeEventsRequest, func(*gateway.ChaincodeEventsResponse) error) error {
		return status.Error(codes.Unavailable, "gone")
	}}
	fc := newTestClient(t, fake)
	s, err := fc.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range s.Events() {
	}
	if !errors.Is(s.Err(), fabricsdk.ErrStreamClosed) {
		t.Fatalf("Err() = %v", s.Err())
	}
}

func TestEventStreamCleanStopOnContextCancel(t *testing.T) {
	fc := newTestClient(t, &fakeGateway{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := fc.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	for range s.Events() {
	}
	if s.Err() != nil {
		t.Fatalf("Err() = %v, want nil after cancel", s.Err())
	}
}

func TestEventStreamDoesNotLeakWhenConsumerStopsReading(t *testing.T) {
	log := &eventLog{events: []ccEvent{{1, "A"}, {1, "B"}, {1, "C"}}, cutAfter: 100}
	fc := newTestClient(t, &fakeGateway{events: log.serve})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := fc.Events(ctx, fabricsdk.FromBlock(1))
	if err != nil {
		t.Fatal(err)
	}
	<-s.Events() // read one, then walk away
	cancel()
	done := make(chan struct{})
	go func() {
		for range s.Events() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event channel not closed after cancel")
	}
}

func TestListenBlocksCheckpointsAndReconnects(t *testing.T) {
	var conns atomic.Int32
	var starts sync.Map
	fake := &fakeGateway{blocks: func(start uint64, send func(*common.Block) error) error {
		n := conns.Add(1)
		starts.Store(n, start)
		for b := start; b < 4; b++ {
			if n == 1 && b == 2 {
				return status.Error(codes.Unavailable, "restart")
			}
			if err := send(&common.Block{Header: &common.BlockHeader{Number: b}}); err != nil {
				return err
			}
		}
		time.Sleep(time.Hour)
		return nil
	}}
	fc := newTestClient(t, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []uint64
	cp := fabricsdk.NewMemoryCheckpointer()
	err := fc.ListenBlocks(ctx, cp, func(_ context.Context, b *common.Block) error {
		got = append(got, b.GetHeader().GetNumber())
		if b.GetHeader().GetNumber() == 3 {
			cancel()
		}
		return nil
	}, fabricsdk.FromBlock(0), fabricsdk.RetryBackoff(10*time.Millisecond, 20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{0, 1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
	if s, _ := starts.Load(int32(2)); s != uint64(2) {
		t.Fatalf("reconnect started at %v, want 2", s)
	}
}

func TestFileCheckpointerPersists(t *testing.T) {
	path := t.TempDir() + "/cp.json"
	cp, err := fabricsdk.NewFileCheckpointer(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.CheckpointTransaction(9, "tx9"); err != nil {
		t.Fatal(err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := fabricsdk.NewFileCheckpointer(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var _ fabricsdk.Checkpointer = again
	if again.BlockNumber() != 9 || again.TransactionID() != "tx9" {
		t.Fatalf("reloaded %d/%s", again.BlockNumber(), again.TransactionID())
	}
}
