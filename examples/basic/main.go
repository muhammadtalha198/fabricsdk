// Command basic shows the core fabricsdk API against the fabric-samples
// asset-transfer-basic chaincode: read, write, and error handling.
//
// Configure with environment variables, then run `go run ./examples/basic`:
//
//	export PEER_ENDPOINT=localhost:7051
//	export PEER_HOST_OVERRIDE=peer0.org1.example.com
//	export TLS_CERT_PATH=.../peers/peer0.org1.example.com/tls/ca.crt
//	export CERT_PATH=.../users/User1@org1.example.com/msp/signcerts
//	export KEY_PATH=.../users/User1@org1.example.com/msp/keystore
//	export MSP_ID=Org1MSP
//	export CHANNEL=mychannel
//	export CHAINCODE=basic
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/muhammadtalha198/fabricsdk"
)

func main() {
	fc, err := fabricsdk.New(fabricsdk.Config{
		PeerEndpoint:     mustEnv("PEER_ENDPOINT"),
		PeerHostOverride: os.Getenv("PEER_HOST_OVERRIDE"),
		TLSCertPath:      mustEnv("TLS_CERT_PATH"),
		CertPath:         mustEnv("CERT_PATH"),
		KeyPath:          mustEnv("KEY_PATH"),
		MSPID:            mustEnv("MSP_ID"),
		ChannelName:      mustEnv("CHANNEL"),
		ChaincodeName:    mustEnv("CHAINCODE"),
	})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer fc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Write: endorse, order and wait for the commit.
	assetID := fmt.Sprintf("asset-%d", time.Now().UnixNano())
	res, err := fc.Submit(ctx, "CreateAsset", assetID, "blue", "5", "Tomoko", "300")
	if err != nil {
		var fe *fabricsdk.Error
		if errors.As(err, &fe) && fe.OutcomeUnknown {
			// The transaction may be on the ledger. Never resubmit blindly.
			st, serr := fc.TxStatus(ctx, fe.TxID)
			log.Fatalf("outcome unknown for %s: status=%+v err=%v", fe.TxID, st, serr)
		}
		log.Fatalf("CreateAsset: %v", err)
	}
	fmt.Printf("committed tx %s in block %d\n", res.TxID, res.BlockNumber)

	// Read: query one peer, nothing is written.
	raw, err := fc.Evaluate(ctx, "ReadAsset", assetID)
	if err != nil {
		log.Fatalf("ReadAsset: %v", err)
	}
	fmt.Printf("asset: %s\n", raw)

	// Errors are typed. asset-transfer-basic answers "the asset X does not
	// exist", which the default keyword fallback maps to NotFound. Chaincode
	// using the fabricsdk/ccerr package gets this without keyword guessing.
	_, err = fc.Evaluate(ctx, "ReadAsset", "does-not-exist")
	switch {
	case fabricsdk.IsNotFound(err):
		fmt.Println("missing asset reported as NotFound")
	case err != nil:
		fmt.Printf("unexpected error kind %s: %v\n", fabricsdk.KindOf(err), err)
	}

	height, err := fc.Height(ctx)
	if err == nil {
		fmt.Printf("channel height: %d\n", height)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
