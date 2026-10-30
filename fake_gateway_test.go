package fabricsdk_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/gateway"
	"github.com/hyperledger/fabric-protos-go-apiv2/msp"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/muhammadtalha198/fabricsdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

// call is what the fake Gateway saw in a proposal.
type call struct {
	Channel   string
	Chaincode string
	Fn        string
	Args      []string
	Transient map[string][]byte
	Orgs      []string
	TxID      string
	MSPID     string
}

// fakeGateway implements the Fabric Gateway and Deliver gRPC services with
// pluggable behaviour, so tests exercise the real fabric-gateway client.
type fakeGateway struct {
	gateway.UnimplementedGatewayServer
	peer.UnimplementedDeliverServer

	mu    sync.Mutex
	calls []call

	evaluate     func(ctx context.Context, c call) ([]byte, error)
	endorse      func(ctx context.Context, c call) ([]byte, error)
	submit       func(ctx context.Context, txID string) error
	commitStatus func(ctx context.Context, txID string) (peer.TxValidationCode, uint64, error)
	events       func(req *gateway.ChaincodeEventsRequest, send func(*gateway.ChaincodeEventsResponse) error) error
	blocks       func(start uint64, send func(*common.Block) error) error
}

func (f *fakeGateway) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeGateway) lastCall(t *testing.T) call {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no calls recorded")
	}
	return f.calls[len(f.calls)-1]
}

func mustUnmarshal(m proto.Message, b []byte) {
	if err := proto.Unmarshal(b, m); err != nil {
		panic(err)
	}
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func decodeProposal(sp *peer.SignedProposal, orgs []string) call {
	prop := &peer.Proposal{}
	mustUnmarshal(prop, sp.GetProposalBytes())
	hdr := &common.Header{}
	mustUnmarshal(hdr, prop.GetHeader())
	ch := &common.ChannelHeader{}
	mustUnmarshal(ch, hdr.GetChannelHeader())
	sh := &common.SignatureHeader{}
	mustUnmarshal(sh, hdr.GetSignatureHeader())
	creator := &msp.SerializedIdentity{}
	mustUnmarshal(creator, sh.GetCreator())
	cpp := &peer.ChaincodeProposalPayload{}
	mustUnmarshal(cpp, prop.GetPayload())
	cis := &peer.ChaincodeInvocationSpec{}
	mustUnmarshal(cis, cpp.GetInput())

	args := cis.GetChaincodeSpec().GetInput().GetArgs()
	c := call{
		Channel: ch.GetChannelId(), TxID: ch.GetTxId(), MSPID: creator.GetMspid(),
		Chaincode: cis.GetChaincodeSpec().GetChaincodeId().GetName(),
		Transient: cpp.GetTransientMap(), Orgs: orgs,
	}
	if len(args) > 0 {
		c.Fn = string(args[0])
		for _, a := range args[1:] {
			c.Args = append(c.Args, string(a))
		}
	}
	return c
}

func (f *fakeGateway) Evaluate(ctx context.Context, req *gateway.EvaluateRequest) (*gateway.EvaluateResponse, error) {
	c := decodeProposal(req.GetProposedTransaction(), req.GetTargetOrganizations())
	f.record(c)
	if f.evaluate == nil {
		return &gateway.EvaluateResponse{Result: &peer.Response{Status: 200}}, nil
	}
	payload, err := f.evaluate(ctx, c)
	if err != nil {
		return nil, err
	}
	return &gateway.EvaluateResponse{Result: &peer.Response{Status: 200, Payload: payload}}, nil
}

func (f *fakeGateway) Endorse(ctx context.Context, req *gateway.EndorseRequest) (*gateway.EndorseResponse, error) {
	c := decodeProposal(req.GetProposedTransaction(), req.GetEndorsingOrganizations())
	f.record(c)
	var result []byte
	if f.endorse != nil {
		var err error
		if result, err = f.endorse(ctx, c); err != nil {
			return nil, err
		}
	}
	action := &peer.ChaincodeAction{Response: &peer.Response{Status: 200, Payload: result}}
	prp := &peer.ProposalResponsePayload{Extension: mustMarshal(action)}
	cap := &peer.ChaincodeActionPayload{Action: &peer.ChaincodeEndorsedAction{ProposalResponsePayload: mustMarshal(prp)}}
	tx := &peer.Transaction{Actions: []*peer.TransactionAction{{Payload: mustMarshal(cap)}}}
	chdr := &common.ChannelHeader{ChannelId: c.Channel, TxId: c.TxID, Type: int32(common.HeaderType_ENDORSER_TRANSACTION)}
	payload := &common.Payload{Header: &common.Header{ChannelHeader: mustMarshal(chdr)}, Data: mustMarshal(tx)}
	return &gateway.EndorseResponse{PreparedTransaction: &common.Envelope{Payload: mustMarshal(payload)}}, nil
}

func (f *fakeGateway) Submit(ctx context.Context, req *gateway.SubmitRequest) (*gateway.SubmitResponse, error) {
	if f.submit != nil {
		if err := f.submit(ctx, req.GetTransactionId()); err != nil {
			return nil, err
		}
	}
	return &gateway.SubmitResponse{}, nil
}

func (f *fakeGateway) CommitStatus(ctx context.Context, req *gateway.SignedCommitStatusRequest) (*gateway.CommitStatusResponse, error) {
	r := &gateway.CommitStatusRequest{}
	mustUnmarshal(r, req.GetRequest())
	if f.commitStatus == nil {
		return &gateway.CommitStatusResponse{Result: peer.TxValidationCode_VALID, BlockNumber: 7}, nil
	}
	code, block, err := f.commitStatus(ctx, r.GetTransactionId())
	if err != nil {
		return nil, err
	}
	return &gateway.CommitStatusResponse{Result: code, BlockNumber: block}, nil
}

func (f *fakeGateway) ChaincodeEvents(req *gateway.SignedChaincodeEventsRequest, stream gateway.Gateway_ChaincodeEventsServer) error {
	r := &gateway.ChaincodeEventsRequest{}
	mustUnmarshal(r, req.GetRequest())
	if f.events == nil {
		<-stream.Context().Done()
		return nil
	}
	return f.events(r, stream.Send)
}

func (f *fakeGateway) Deliver(stream peer.Deliver_DeliverServer) error {
	env, err := stream.Recv()
	if err != nil {
		return err
	}
	p := &common.Payload{}
	mustUnmarshal(p, env.GetPayload())
	seek := &orderer.SeekInfo{}
	mustUnmarshal(seek, p.GetData())
	var start uint64
	if s := seek.GetStart().GetSpecified(); s != nil {
		start = s.GetNumber()
	}
	if f.blocks == nil {
		<-stream.Context().Done()
		return nil
	}
	return f.blocks(start, func(b *common.Block) error {
		return stream.Send(&peer.DeliverResponse{Type: &peer.DeliverResponse_Block{Block: b}})
	})
}

// gatewayError builds a Gateway-style error with per-peer details.
func gatewayError(code codes.Code, msg string, details ...string) error {
	st := status.New(code, msg)
	var ds []protoadapt.MessageV1
	for i, d := range details {
		ds = append(ds, protoadapt.MessageV1Of(&gateway.ErrorDetail{Address: "peer" + string(rune('0'+i)) + ":7051", MspId: "Org1MSP", Message: d}))
	}
	if len(ds) > 0 {
		st, _ = st.WithDetails(ds...)
	}
	return st.Err()
}

// ── test fixtures ────────────────────────────────────────────────────────────

func newTestIdentityPEM(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

func newTestIdentity(t *testing.T, mspID, cn string) *fabricsdk.Identity {
	t.Helper()
	certPEM, keyPEM := newTestIdentityPEM(t, cn)
	id, err := fabricsdk.IdentityFromPEM(mspID, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newTestClient starts the fake on an in-memory connection and returns a client.
func newTestClient(t *testing.T, fake *fakeGateway, mutate ...func(*fabricsdk.Config)) *fabricsdk.FabricClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	gateway.RegisterGatewayServer(srv, fake)
	peer.RegisterDeliverServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cfg := fabricsdk.Config{
		Identity:      newTestIdentity(t, "Org1MSP", "user1"),
		ChannelName:   "mychannel",
		ChaincodeName: "basic",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	fc, err := fabricsdk.NewWithConnection(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	return fc
}
