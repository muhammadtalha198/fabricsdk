package fabricsdk

import (
	"context"
	"fmt"

	"github.com/hyperledger/fabric-gateway/pkg/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// DynamicClient — call any channel / chaincode / function
// ─────────────────────────────────────────────────────────────────────────────

// DynamicClient makes calls to any channel, chaincode, and function without
// being bound to the channel/chaincode configured in Config. Use it when:
//   - You need to call multiple chaincodes from one connection
//   - You are writing admin scripts or debug tooling
//   - You want to call a function before building a typed wrapper for it
//
// Note: DynamicClient opens a new Gateway per call. This is intentional —
// it prioritises flexibility over connection reuse. For high-throughput paths,
// use FabricClient.Evaluate / FabricClient.Submit which reuse one Gateway.
//
// Obtain a DynamicClient from an existing FabricClient:
//
//	dyn := fc.Dynamic()
//
//	ctx := context.Background()
//	raw,  err := dyn.Evaluate(ctx, "channelA", "chaincodeA", "ContractA", "GetAsset", "id-1")
//	txID, err := dyn.Submit(ctx, "channelB", "chaincodeB", "ContractB", "CreateAsset", string(jsonBytes))
//
//	// With identity override
//	txID, err = dyn.WithIdentity(cert, key).Submit(ctx, "ch", "cc", "C", "AdminFn", arg)
type DynamicClient struct {
	fc *FabricClient
}

// Dynamic returns a DynamicClient backed by this FabricClient's connection.
// The gRPC connection is shared — no new handshake.
func (fc *FabricClient) Dynamic() *DynamicClient {
	return &DynamicClient{fc: fc}
}

// Evaluate calls a read-only chaincode function on any channel/chaincode/contract.
// contractName may be empty to target the default contract.
// The context controls timeout and cancellation.
// Returns raw bytes from the chaincode response.
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	raw, err := dyn.Evaluate(ctx, "mychannel", "mychaincode", "MyContract", "GetAsset", "id-1")
func (d *DynamicClient) Evaluate(ctx context.Context, channel, chaincode, contractName, fn string, args ...string) ([]byte, error) {
	gw, err := newGateway(d.fc.cfg, d.fc.conn, d.fc.cfg.CertPath, d.fc.cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	defer gw.Close()

	result, err := resolveContract(gw, channel, chaincode, contractName).EvaluateTransaction(fn, args...)
	if err != nil {
		return nil, classifyError(err)
	}
	return result, nil
}

// Submit calls a write chaincode function on any channel/chaincode/contract.
// The context controls timeout and cancellation for the entire lifecycle.
// Blocks until the transaction is committed. Returns the transaction ID.
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	txID, err := dyn.Submit(ctx, "mychannel", "mychaincode", "MyContract", "CreateAsset", string(jsonBytes))
func (d *DynamicClient) Submit(ctx context.Context, channel, chaincode, contractName, fn string, args ...string) (string, error) {
	gw, err := newGateway(d.fc.cfg, d.fc.conn, d.fc.cfg.CertPath, d.fc.cfg.KeyPath)
	if err != nil {
		return "", err
	}
	defer gw.Close()

	return submitOnContract(ctx, resolveContract(gw, channel, chaincode, contractName), fn, args...)
}

// ─────────────────────────────────────────────────────────────────────────────
// DynamicCallerClient — identity override on DynamicClient
// ─────────────────────────────────────────────────────────────────────────────

// DynamicCallerClient is a DynamicClient view bound to a different caller identity.
// Obtain one via DynamicClient.WithIdentity.
type DynamicCallerClient struct {
	parent   *DynamicClient
	certPath string
	keyPath  string
}

// WithIdentity returns a DynamicCallerClient that signs with the provided
// cert/key instead of the default identity. The gRPC connection is reused.
func (d *DynamicClient) WithIdentity(certPath, keyPath string) *DynamicCallerClient {
	return &DynamicCallerClient{parent: d, certPath: certPath, keyPath: keyPath}
}

// Evaluate runs a read-only query signed by the overridden identity.
func (dc *DynamicCallerClient) Evaluate(ctx context.Context, channel, chaincode, contractName, fn string, args ...string) ([]byte, error) {
	var result []byte
	err := dc.scoped(channel, chaincode, contractName, func(c *client.Contract) error {
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
func (dc *DynamicCallerClient) Submit(ctx context.Context, channel, chaincode, contractName, fn string, args ...string) (string, error) {
	var txID string
	err := dc.scoped(channel, chaincode, contractName, func(c *client.Contract) error {
		var submitErr error
		txID, submitErr = submitOnContract(ctx, c, fn, args...)
		return submitErr
	})
	return txID, err
}

// scoped opens a short-lived gateway with the override identity,
// calls fn, then closes the gateway — guaranteed via defer.
func (dc *DynamicCallerClient) scoped(channel, chaincode, contractName string, fn func(*client.Contract) error) error {
	gw, err := newGateway(dc.parent.fc.cfg, dc.parent.fc.conn, dc.certPath, dc.keyPath)
	if err != nil {
		return fmt.Errorf("fabricsdk: dynamic identity override: %w", err)
	}
	defer gw.Close()
	return fn(resolveContract(gw, channel, chaincode, contractName))
}
