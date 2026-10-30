package fabricsdk_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/muhammadtalha198/fabricsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func TestEvaluatePassesArgumentsAndReturnsResult(t *testing.T) {
	fake := &fakeGateway{evaluate: func(_ context.Context, c call) ([]byte, error) {
		return []byte(`{"id":"` + c.Args[0] + `"}`), nil
	}}
	fc := newTestClient(t, fake)

	got, err := fc.Evaluate(context.Background(), "ReadAsset", "asset1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"id":"asset1"}` {
		t.Fatalf("result = %s", got)
	}
	c := fake.lastCall(t)
	if c.Channel != "mychannel" || c.Chaincode != "basic" || c.Fn != "ReadAsset" || !reflect.DeepEqual(c.Args, []string{"asset1"}) {
		t.Fatalf("unexpected call %+v", c)
	}
}

func TestNamedContractPrefixesFunction(t *testing.T) {
	fake := &fakeGateway{}
	fc := newTestClient(t, fake, func(c *fabricsdk.Config) { c.ContractName = "ChallanContract" })
	if _, err := fc.Evaluate(context.Background(), "Get", "x"); err != nil {
		t.Fatal(err)
	}
	if c := fake.lastCall(t); c.Fn != "ChallanContract:Get" {
		t.Fatalf("fn = %q", c.Fn)
	}
}

func blockUntilDone(ctx context.Context, _ call) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEvaluateHonoursCallerDeadline(t *testing.T) {
	fake := &fakeGateway{evaluate: blockUntilDone}
	fc := newTestClient(t, fake, func(c *fabricsdk.Config) { c.EvaluateTimeout = time.Minute })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fc.Evaluate(ctx, "Slow")
	if !fabricsdk.IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("caller deadline was ignored")
	}
}

func TestEvaluateAppliesDefaultTimeoutWhenNoDeadline(t *testing.T) {
	fake := &fakeGateway{evaluate: blockUntilDone}
	fc := newTestClient(t, fake, func(c *fabricsdk.Config) { c.EvaluateTimeout = 50 * time.Millisecond })
	_, err := fc.Evaluate(context.Background(), "Slow")
	if !fabricsdk.IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
}

func TestEvaluateCancellation(t *testing.T) {
	fake := &fakeGateway{evaluate: blockUntilDone}
	fc := newTestClient(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := fc.Evaluate(ctx, "Slow")
	if fabricsdk.KindOf(err) != fabricsdk.KindCanceled {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestSubmitReturnsCommittedResult(t *testing.T) {
	var seenTx string
	fake := &fakeGateway{
		endorse: func(_ context.Context, c call) ([]byte, error) { return []byte("ok:" + c.Args[0]), nil },
		submit:  func(_ context.Context, txID string) error { seenTx = txID; return nil },
		commitStatus: func(_ context.Context, txID string) (peer.TxValidationCode, uint64, error) {
			return peer.TxValidationCode_VALID, 42, nil
		},
	}
	fc := newTestClient(t, fake)
	res, err := fc.Submit(context.Background(), "CreateAsset", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if res.TxID == "" || res.TxID != seenTx || res.BlockNumber != 42 || string(res.Payload) != "ok:a1" {
		t.Fatalf("unexpected result %+v (server saw tx %s)", res, seenTx)
	}
}

func TestSubmitWithTransientAndEndorsingOrgs(t *testing.T) {
	fake := &fakeGateway{}
	fc := newTestClient(t, fake)
	secret := map[string][]byte{"owner": []byte(`{"cnic":"00000-0000000-0"}`)}
	_, err := fc.SubmitWith(context.Background(), "CreateChallan",
		fabricsdk.Args("CH-1"),
		fabricsdk.BytesArgs([]byte("raw")),
		fabricsdk.Transient(secret),
		fabricsdk.EndorsingOrgs("PoliceMSP"),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := fake.lastCall(t)
	if !reflect.DeepEqual(c.Args, []string{"CH-1", "raw"}) {
		t.Fatalf("args = %v", c.Args)
	}
	if !reflect.DeepEqual(c.Transient, secret) {
		t.Fatalf("transient = %v", c.Transient)
	}
	if !reflect.DeepEqual(c.Orgs, []string{"PoliceMSP"}) {
		t.Fatalf("orgs = %v", c.Orgs)
	}
}

func TestSubmitCommitConflictIsRetryable(t *testing.T) {
	fake := &fakeGateway{commitStatus: func(context.Context, string) (peer.TxValidationCode, uint64, error) {
		return peer.TxValidationCode_MVCC_READ_CONFLICT, 9, nil
	}}
	fc := newTestClient(t, fake)
	_, err := fc.Submit(context.Background(), "Update", "k")
	var fe *fabricsdk.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want *Error, got %v", err)
	}
	if !fabricsdk.IsConflict(err) || !fabricsdk.IsRetryable(err) || fabricsdk.IsOutcomeUnknown(err) {
		t.Fatalf("want retryable conflict, got %+v", fe)
	}
	if fe.ValidationCode != peer.TxValidationCode_MVCC_READ_CONFLICT || fe.TxID == "" || fe.Code != 409 {
		t.Fatalf("unexpected fields %+v", fe)
	}
}

func TestSubmitCommitFailedIsNotRetryable(t *testing.T) {
	fake := &fakeGateway{commitStatus: func(context.Context, string) (peer.TxValidationCode, uint64, error) {
		return peer.TxValidationCode_ENDORSEMENT_POLICY_FAILURE, 9, nil
	}}
	fc := newTestClient(t, fake)
	_, err := fc.Submit(context.Background(), "Update", "k")
	if fabricsdk.KindOf(err) != fabricsdk.KindCommitFailed || fabricsdk.IsRetryable(err) {
		t.Fatalf("want non-retryable commit failure, got %v", err)
	}
}

func TestCommitStatusTimeoutIsOutcomeUnknownWithTxID(t *testing.T) {
	var ready atomic.Bool
	fake := &fakeGateway{commitStatus: func(ctx context.Context, _ string) (peer.TxValidationCode, uint64, error) {
		if !ready.Load() {
			<-ctx.Done()
			return 0, 0, ctx.Err()
		}
		return peer.TxValidationCode_VALID, 3, nil
	}}
	fc := newTestClient(t, fake, func(c *fabricsdk.Config) { c.CommitStatusTimeout = 50 * time.Millisecond })

	pending, err := fc.SubmitAsync(context.Background(), "Create", fabricsdk.Args("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = pending.Wait(context.Background())
	if !fabricsdk.IsOutcomeUnknown(err) || fabricsdk.IsRetryable(err) {
		t.Fatalf("want outcome unknown, got %v", err)
	}
	if fabricsdk.TxIDOf(err) != pending.TxID || pending.TxID == "" {
		t.Fatalf("error must carry the tx ID: %v", err)
	}

	ready.Store(true)
	res, err := pending.Wait(context.Background())
	if err != nil || res.BlockNumber != 3 {
		t.Fatalf("second Wait: %+v %v", res, err)
	}
}

func TestSubmitPhaseErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantKind  fabricsdk.Kind
		unknown   bool
		retryable bool
	}{
		{"orderer rejected", gatewayError(codes.Aborted, "received unsuccessful response from orderer: status=BAD_REQUEST"), fabricsdk.KindSubmit, false, true},
		{"broadcast timeout", gatewayError(codes.DeadlineExceeded, "submit timeout expired while broadcasting to ordering service"), fabricsdk.KindOutcomeUnknown, true, false},
		{"no orderers", gatewayError(codes.Unavailable, "no orderers could successfully process transaction"), fabricsdk.KindOutcomeUnknown, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeGateway{submit: func(context.Context, string) error { return tt.err }}
			fc := newTestClient(t, fake)
			_, err := fc.Submit(context.Background(), "Create", "x")
			if fabricsdk.KindOf(err) != tt.wantKind || fabricsdk.IsOutcomeUnknown(err) != tt.unknown || fabricsdk.IsRetryable(err) != tt.retryable {
				t.Fatalf("got %v (unknown=%v retryable=%v)", err, fabricsdk.IsOutcomeUnknown(err), fabricsdk.IsRetryable(err))
			}
			if fabricsdk.TxIDOf(err) == "" {
				t.Fatal("submit errors must carry the tx ID")
			}
		})
	}
}

func TestEndorseAndEvaluateErrorClassification(t *testing.T) {
	const endorseMsg = "failed to endorse transaction, see attached details for more info"
	tests := []struct {
		name     string
		strict   bool
		err      error
		wantKind fabricsdk.Kind
		wantMsg  string
		source   fabricsdk.ErrorSource
	}{
		{"ccerr prefix", false, gatewayError(codes.Aborted, endorseMsg, "NOT_FOUND: challan CH-9 does not exist"),
			fabricsdk.KindNotFound, "challan CH-9 does not exist", fabricsdk.SourceChaincode},
		{"ccerr prefix behind chaincode response", true, gatewayError(codes.Aborted, endorseMsg, "chaincode response 409, ALREADY_EXISTS: challan CH-1"),
			fabricsdk.KindAlreadyExists, "challan CH-1", fabricsdk.SourceChaincode},
		{"ccerr wins over other peers' text", false, gatewayError(codes.Aborted, endorseMsg, "connection refused", "CONFLICT: version 3 != 4"),
			fabricsdk.KindConflict, "version 3 != 4", fabricsdk.SourceChaincode},
		{"legacy keyword", false, gatewayError(codes.Aborted, endorseMsg, "the asset a9 does not exist"),
			fabricsdk.KindNotFound, "the asset a9 does not exist", fabricsdk.SourceChaincode},
		{"legacy keyword disabled in strict mode", true, gatewayError(codes.Aborted, endorseMsg, "the asset a9 does not exist"),
			fabricsdk.KindEndorsement, "", fabricsdk.SourceNetwork},
		{"peer-side 'not found' is not a chaincode NotFound", false, gatewayError(codes.Aborted, endorseMsg,
			"make sure the chaincode basic has been successfully defined on channel mychannel and try again: chaincode basic not found"),
			fabricsdk.KindEndorsement, "", fabricsdk.SourceNetwork},
		{"unclassified chaincode response", false, gatewayError(codes.Unknown, "evaluate call to endorser returned error: chaincode response 400, bad plate", "chaincode response 400, bad plate"),
			fabricsdk.KindChaincode, "bad plate", fabricsdk.SourceChaincode},
		{"unavailable", false, gatewayError(codes.Unavailable, "no peers available"), fabricsdk.KindUnavailable, "", fabricsdk.SourceNetwork},
		{"permission denied", false, gatewayError(codes.PermissionDenied, "access denied"), fabricsdk.KindUnauthorized, "", fabricsdk.SourceNetwork},
		{"invalid argument", false, gatewayError(codes.InvalidArgument, "bad request"), fabricsdk.KindInvalidArgument, "", fabricsdk.SourceNetwork},
	}
	for _, tt := range tests {
		for _, op := range []string{"evaluate", "submit"} {
			t.Run(tt.name+"/"+op, func(t *testing.T) {
				fail := func(context.Context, call) ([]byte, error) { return nil, tt.err }
				fake := &fakeGateway{evaluate: fail, endorse: fail}
				fc := newTestClient(t, fake, func(c *fabricsdk.Config) { c.StrictErrorCodes = tt.strict })
				var err error
				if op == "evaluate" {
					_, err = fc.Evaluate(context.Background(), "Fn")
				} else {
					_, err = fc.Submit(context.Background(), "Fn")
				}
				// Wrapping by the caller must not hide the classification.
				err = fmt.Errorf("handler: %w", err)
				var fe *fabricsdk.Error
				if !errors.As(err, &fe) {
					t.Fatalf("want *Error, got %v", err)
				}
				if fe.Kind != tt.wantKind || fe.Source != tt.source {
					t.Fatalf("kind/source = %s/%s, want %s/%s (%v)", fe.Kind, fe.Source, tt.wantKind, tt.source, fe)
				}
				if tt.wantMsg != "" && fe.Message != tt.wantMsg {
					t.Fatalf("message = %q, want %q", fe.Message, tt.wantMsg)
				}
				if fe.TxID == "" {
					t.Fatal("error must carry the tx ID")
				}
				if fe.OutcomeUnknown {
					t.Fatal("endorse/evaluate failures never leave the outcome unknown")
				}
			})
		}
	}
}

func TestWithIdentitySignsAsOtherUser(t *testing.T) {
	fake := &fakeGateway{}
	fc := newTestClient(t, fake)
	officer := newTestIdentity(t, "PoliceMSP", "officer-0001")

	if _, err := fc.WithIdentity(officer).Submit(context.Background(), "IssueChallan", "CH-1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.lastCall(t).MSPID; got != "PoliceMSP" {
		t.Fatalf("signed as %q", got)
	}
	if _, err := fc.Evaluate(context.Background(), "Read"); err != nil {
		t.Fatal(err)
	}
	if got := fake.lastCall(t).MSPID; got != "Org1MSP" {
		t.Fatalf("default identity changed to %q", got)
	}
}

func TestTargetOverridesChannelChaincodeAndContract(t *testing.T) {
	fake := &fakeGateway{}
	fc := newTestClient(t, fake)
	if _, err := fc.Target("trafficchannel", "requests", "RequestContract").Evaluate(context.Background(), "Get"); err != nil {
		t.Fatal(err)
	}
	c := fake.lastCall(t)
	if c.Channel != "trafficchannel" || c.Chaincode != "requests" || c.Fn != "RequestContract:Get" {
		t.Fatalf("unexpected call %+v", c)
	}
}

func TestTxStatus(t *testing.T) {
	fake := &fakeGateway{evaluate: func(_ context.Context, c call) ([]byte, error) {
		if c.Chaincode != "qscc" || c.Fn != "GetTransactionByID" || c.Args[0] != "mychannel" {
			return nil, fmt.Errorf("unexpected call %+v", c)
		}
		switch c.Args[1] {
		case "valid":
			return proto.Marshal(&peer.ProcessedTransaction{ValidationCode: int32(peer.TxValidationCode_VALID)})
		case "mvcc":
			return proto.Marshal(&peer.ProcessedTransaction{ValidationCode: int32(peer.TxValidationCode_MVCC_READ_CONFLICT)})
		default:
			return nil, gatewayError(codes.Aborted, "failed to evaluate transaction, see attached details for more info",
				"Failed to get transaction with id "+c.Args[1]+", error no such transaction ID ["+c.Args[1]+"] in index")
		}
	}}
	fc := newTestClient(t, fake)
	ctx := context.Background()

	if st, err := fc.TxStatus(ctx, "valid"); err != nil || !st.Found || !st.Valid {
		t.Fatalf("valid: %+v %v", st, err)
	}
	if st, err := fc.TxStatus(ctx, "mvcc"); err != nil || !st.Found || st.Valid || st.ValidationCode != peer.TxValidationCode_MVCC_READ_CONFLICT {
		t.Fatalf("mvcc: %+v %v", st, err)
	}
	if st, err := fc.TxStatus(ctx, "missing"); err != nil || st.Found {
		t.Fatalf("missing: %+v %v", st, err)
	}
}

func TestHeight(t *testing.T) {
	fake := &fakeGateway{evaluate: func(_ context.Context, c call) ([]byte, error) {
		if c.Chaincode != "qscc" || c.Fn != "GetChainInfo" {
			return nil, fmt.Errorf("unexpected call %+v", c)
		}
		return proto.Marshal(&common.BlockchainInfo{Height: 1234})
	}}
	fc := newTestClient(t, fake)
	h, err := fc.Height(context.Background())
	if err != nil || h != 1234 {
		t.Fatalf("height = %d, %v", h, err)
	}
}

func TestObserverSeesEveryPhase(t *testing.T) {
	var ops []string
	fake := &fakeGateway{}
	fc := newTestClient(t, fake, func(c *fabricsdk.Config) {
		c.Observer = fabricsdk.ObserverFunc(func(_ context.Context, info fabricsdk.CallInfo) {
			if info.TxID == "" || info.Function != "Create" || info.MSPID != "Org1MSP" {
				t.Errorf("incomplete call info %+v", info)
			}
			ops = append(ops, info.Operation)
		})
	})
	if _, err := fc.Submit(context.Background(), "Create", "x"); err != nil {
		t.Fatal(err)
	}
	want := []string{fabricsdk.OpEndorse, fabricsdk.OpSubmit, fabricsdk.OpCommit}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
}

func TestCloseIsIdempotentAndBlocksFurtherCalls(t *testing.T) {
	fc := newTestClient(t, &fakeGateway{})
	if err := fc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fc.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := fc.Evaluate(context.Background(), "Read")
	if fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error after close, got %v", err)
	}
}
